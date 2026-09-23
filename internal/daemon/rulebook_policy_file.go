package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
)

// RulebookPolicyOwnerID names a policy written by the owner's edits.
const RulebookPolicyOwnerID = "rulebook-owner"

// RulebookPolicyChange is one key an edit moved: its value before and after,
// as TOML literals. An empty From or To means the compiled baseline.
type RulebookPolicyChange struct {
	Key  string
	From string
	To   string
}

// RulebookPolicyEdit is the result of an edit to the owner's policy file.
type RulebookPolicyEdit struct {
	Path    string
	Version int
	Changes []RulebookPolicyChange
	// Overrides are the keys the file now sets; everything else follows the
	// compiled baseline, including values later releases change.
	Overrides []string
}

// DefaultRulebookPolicyPath resolves the owner's Rulebook policy path from
// the loaded config, or the documented default when no config is present.
func DefaultRulebookPolicyPath() string {
	path := config.DefaultRulebookPolicyFile
	if cfg, err := config.Load(""); err == nil && cfg != nil {
		path = cfg.Rulebook.PolicyFilePath()
	}
	return expandUserPath(path)
}

// EditRulebookPolicy applies key=value assignments and key resets to the
// owner's Rulebook policy file. The file keeps only the keys that differ from
// the compiled baseline, so a later release's baseline still reaches every key
// the owner never set. The result is validated exactly as the daemon will read
// it, policy_version is raised so the running daemon adopts it, and the file is
// replaced atomically with owner-only permissions. Nothing is written when any
// assignment is unknown or invalid.
func EditRulebookPolicy(path string, assignments, resets []string, resetAll bool) (RulebookPolicyEdit, error) {
	path = expandUserPath(strings.TrimSpace(path))
	if path == "" {
		path = DefaultRulebookPolicyPath()
	}
	baseline, err := rulebookPolicyTOMLMap(risk.DefaultRulebookPolicy())
	if err != nil {
		return RulebookPolicyEdit{}, err
	}
	current := map[string]any{}
	currentVersion := 0
	if data, err := os.ReadFile(path); err == nil {
		if _, err := toml.Decode(string(data), &current); err != nil {
			return RulebookPolicyEdit{}, fmt.Errorf("read %s: %w", path, err)
		}
		if v, ok := current["policy_version"].(int64); ok {
			currentVersion = int(v)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return RulebookPolicyEdit{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, key := range []string{"kind", "schema_version", "policy_id", "policy_version"} {
		delete(current, key)
	}
	before := flattenTOMLMap(current)

	overrides := current
	if resetAll {
		overrides = map[string]any{}
	}
	known := flattenTOMLMap(baseline)
	for _, key := range resets {
		key = strings.TrimSpace(key)
		if _, ok := known[key]; !ok {
			return RulebookPolicyEdit{}, unknownRulebookKey(key, known)
		}
		deleteTOMLKey(overrides, key)
	}
	for _, assignment := range assignments {
		key, raw, ok := strings.Cut(assignment, "=")
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		if !ok || key == "" || raw == "" {
			return RulebookPolicyEdit{}, fmt.Errorf("%q is not KEY=VALUE", assignment)
		}
		want, ok := known[key]
		if !ok {
			return RulebookPolicyEdit{}, unknownRulebookKey(key, known)
		}
		value, err := parseRulebookValue(raw, want)
		if err != nil {
			return RulebookPolicyEdit{}, fmt.Errorf("%s: %w", key, err)
		}
		setTOMLKey(overrides, key, value)
	}
	// A value equal to the baseline is not an override: dropping it lets a
	// later baseline change reach the key.
	for key, value := range flattenTOMLMap(overrides) {
		if tomlValuesEqual(value, known[key]) {
			deleteTOMLKey(overrides, key)
		}
	}

	version := max(currentVersion, risk.DefaultRulebookPolicy().Version) + 1
	var buf bytes.Buffer
	buf.WriteString("# Owner Rulebook policy, written by `canary rules policy set`.\n")
	buf.WriteString("# Keys here override the compiled baseline; every other key follows it.\n")
	buf.WriteString("# Baseline and every key: canary policy default rulebook\n")
	fmt.Fprintf(&buf, "kind = %q\nschema_version = 1\npolicy_id = %q\npolicy_version = %d\n\n", risk.RulebookPolicyKind, RulebookPolicyOwnerID, version)
	if err := toml.NewEncoder(&buf).Encode(overrides); err != nil {
		return RulebookPolicyEdit{}, err
	}
	policy, keys, err := parseRulebookPolicy(buf.Bytes())
	if err != nil {
		return RulebookPolicyEdit{}, err
	}
	if err := writePrivateFile(path, buf.Bytes()); err != nil {
		return RulebookPolicyEdit{}, err
	}

	after := flattenTOMLMap(overrides)
	var changes []RulebookPolicyChange
	for _, key := range unionKeys(before, after) {
		from, to := before[key], after[key]
		if tomlValuesEqual(from, to) {
			continue
		}
		changes = append(changes, RulebookPolicyChange{Key: key, From: tomlLiteral(from), To: tomlLiteral(to)})
	}
	return RulebookPolicyEdit{Path: path, Version: policy.Version, Changes: changes, Overrides: keys}, nil
}

// DefaultRulebookPolicyTOML renders the compiled baseline as a complete,
// valid owner file.
func DefaultRulebookPolicyTOML() ([]byte, error) {
	p := risk.DefaultRulebookPolicy()
	p.Kind, p.SchemaVersion = risk.RulebookPolicyKind, 1
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rulebookPolicyTOMLMap(p risk.RulebookPolicy) (map[string]any, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(p); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if _, err := toml.Decode(buf.String(), &out); err != nil {
		return nil, err
	}
	for _, key := range []string{"kind", "schema_version", "policy_id", "policy_version"} {
		delete(out, key)
	}
	return out, nil
}

// parseRulebookValue reads one value as the type the baseline uses for the
// key: numbers stay numbers, mode names and symbols stay strings, and a list
// may be written comma-separated.
func parseRulebookValue(raw string, like any) (any, error) {
	switch like.(type) {
	case string:
		return strings.Trim(raw, `"'`), nil
	case []any:
		var out []any
		for part := range strings.SplitSeq(strings.Trim(raw, "[]"), ",") {
			if part = strings.Trim(strings.TrimSpace(part), `"'`); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	}
	var probe struct{ V any }
	if _, err := toml.Decode("V = "+raw, &probe); err != nil {
		return nil, fmt.Errorf("%q is not a number", raw)
	}
	switch v := probe.V.(type) {
	case int64:
		if _, isFloat := like.(float64); isFloat {
			return float64(v), nil
		}
		return v, nil
	case float64:
		if _, isInt := like.(int64); isInt {
			return nil, fmt.Errorf("%q must be a whole number", raw)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("%q is not a number", raw)
	}
}

func unknownRulebookKey(key string, known map[string]any) error {
	var similar []string
	for k := range known {
		if strings.Contains(k, key) || strings.Contains(key, k) {
			similar = append(similar, k)
		}
	}
	slices.Sort(similar)
	if len(similar) > 0 {
		return fmt.Errorf("unknown key %q (did you mean %s?); every key: canary policy default rulebook", key, strings.Join(similar, ", "))
	}
	return fmt.Errorf("unknown key %q; every key: canary policy default rulebook", key)
}

func flattenTOMLMap(in map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if nested, ok := v.(map[string]any); ok {
				walk(prefix+k+".", nested)
				continue
			}
			out[prefix+k] = v
		}
	}
	walk("", in)
	return out
}

func setTOMLKey(m map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = value
}

func deleteTOMLKey(m map[string]any, key string) {
	parts := strings.Split(key, ".")
	parents := []map[string]any{m}
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
		parents = append(parents, m)
	}
	delete(m, parts[len(parts)-1])
	for i := len(parents) - 1; i > 0; i-- {
		if len(parents[i]) == 0 {
			delete(parents[i-1], parts[i-1])
		}
	}
}

func tomlValuesEqual(a, b any) bool {
	fa, aNum := tomlNumber(a)
	fb, bNum := tomlNumber(b)
	if aNum || bNum {
		return aNum && bNum && fa == fb
	}
	return reflect.DeepEqual(a, b)
}

func tomlNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func tomlLiteral(v any) string {
	if v == nil {
		return ""
	}
	var probe struct{ V any }
	probe.V = v
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(probe); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(strings.TrimPrefix(buf.String(), "V = "))
}

func unionKeys(a, b map[string]any) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// writePrivateFile replaces path atomically with owner-only permissions,
// creating a private parent directory when it is missing.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rulebook-policy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
