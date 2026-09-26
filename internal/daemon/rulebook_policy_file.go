package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// Differs lists the keys whose value in the file is not Canary's current
	// default: the file carries every key, and these are the ones moved from
	// Canary's recommendation (issuer groups and clusters always count).
	Differs []string
	// Review is rpc.PolicyReviewUnreviewed while the file still opens with
	// Canary's "not yet reviewed" header: an edit reviews the keys it names,
	// never the rest of the file, so the header stays until the owner removes it.
	Review string
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
// owner's Rulebook policy file in place. The file is the source of truth
// (owner decision 2026-09-26), so an edit changes only the lines it names
// and keeps every other value and comment: a missing file starts from
// Canary's template, a reset writes Canary's current default for the key,
// and reset --all rewrites the file from the template after a backup. An
// issuer group or cluster is set as issuer_groups.NAME=A,B and removed with
// reset. The result is validated exactly as the daemon will read it,
// policy_version is raised so the running daemon adopts it, and the file is
// replaced atomically with owner-only permissions. Nothing is written when
// any assignment is unknown or invalid. Retired keys are refused by set and
// removed by any edit.
func EditRulebookPolicy(path string, assignments, resets []string, resetAll bool) (RulebookPolicyEdit, error) {
	path = expandUserPath(strings.TrimSpace(path))
	if path == "" {
		path = DefaultRulebookPolicyPath()
	}
	baseline, err := rulebookPolicyTOMLMap(risk.DefaultRulebookPolicy())
	if err != nil {
		return RulebookPolicyEdit{}, err
	}
	known := flattenTOMLMap(baseline)
	template := RulebookPolicyTemplate(rulebookEditRelease)
	data, err := os.ReadFile(path)
	existed := err == nil
	switch {
	case errors.Is(err, os.ErrNotExist):
		data = template
	case err != nil:
		return RulebookPolicyEdit{}, fmt.Errorf("read %s: %w", path, err)
	}
	current := map[string]any{}
	if _, err := toml.Decode(string(data), &current); err != nil {
		return RulebookPolicyEdit{}, fmt.Errorf("read %s: %w", path, err)
	}
	currentVersion := 0
	if v, ok := current["policy_version"].(int64); ok {
		currentVersion = int(v)
	}
	currentID, _ := current["policy_id"].(string)
	for _, key := range []string{"kind", "schema_version", "policy_id", "policy_version"} {
		delete(current, key)
	}
	before := flattenTOMLMap(current)

	doc := parseTOMLDoc(data)
	if resetAll {
		doc = parseTOMLDoc(template)
	}
	for key := range before {
		if retiredRulebookKey(key) {
			table, leaf, _ := strings.Cut(key, ".")
			doc.remove(table, leaf)
		}
	}
	for _, key := range resets {
		key = strings.TrimSpace(key)
		table, leaf := splitRulebookKey(key)
		switch {
		case retiredRulebookKey(key):
			doc.remove(table, leaf)
		case table == "issuer_groups" || table == "clusters":
			doc.remove(table, leaf)
		default:
			want, ok := known[key]
			if !ok {
				return RulebookPolicyEdit{}, unknownRulebookKey(key, known)
			}
			doc.set(table, leaf, tomlLiteral(want), nil)
		}
	}
	for _, assignment := range assignments {
		key, raw, ok := strings.Cut(assignment, "=")
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		if !ok || key == "" || raw == "" {
			return RulebookPolicyEdit{}, fmt.Errorf("%q is not KEY=VALUE", assignment)
		}
		if retiredRulebookKey(key) {
			return RulebookPolicyEdit{}, fmt.Errorf("%s is retired: %s", key, retiredCashSellOnlyReason)
		}
		table, leaf := splitRulebookKey(key)
		if (table == "issuer_groups" || table == "clusters") && leaf != "" {
			value, err := parseRulebookValue(raw, []any{})
			if err != nil {
				return RulebookPolicyEdit{}, fmt.Errorf("%s: %w", key, err)
			}
			doc.set(table, leaf, tomlLiteral(value), nil)
			continue
		}
		want, ok := known[key]
		if !ok {
			return RulebookPolicyEdit{}, unknownRulebookKey(key, known)
		}
		value, err := parseRulebookValue(raw, want)
		if err != nil {
			return RulebookPolicyEdit{}, fmt.Errorf("%s: %w", key, err)
		}
		doc.set(table, leaf, tomlLiteral(value), nil)
	}
	version := max(currentVersion, risk.DefaultRulebookPolicy().Version) + 1
	doc.set("", "policy_version", strconv.Itoa(version), nil)
	if currentID == "" || currentID == risk.DefaultRulebookPolicy().ID {
		doc.set("", "policy_id", strconv.Quote(RulebookPolicyOwnerID), nil)
	}
	out := doc.bytes()
	read, err := parseRulebookPolicy(out)
	if err != nil {
		return RulebookPolicyEdit{}, err
	}
	if resetAll && existed {
		backup := fmt.Sprintf("%s.bak-reset-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := writePrivateFileExclusive(backup, data); err != nil {
			return RulebookPolicyEdit{}, fmt.Errorf("backup before reset: %w", err)
		}
	}
	if err := writePrivateFile(path, out); err != nil {
		return RulebookPolicyEdit{}, err
	}

	after := map[string]any{}
	if _, err := toml.Decode(string(out), &after); err != nil {
		return RulebookPolicyEdit{}, err
	}
	for _, key := range []string{"kind", "schema_version", "policy_id", "policy_version"} {
		delete(after, key)
	}
	afterFlat := flattenTOMLMap(after)
	var changes []RulebookPolicyChange
	for _, key := range unionKeys(before, afterFlat) {
		from, to := before[key], afterFlat[key]
		if !existed {
			from = known[key]
		}
		if tomlValuesEqual(from, to) {
			continue
		}
		change := RulebookPolicyChange{Key: key, From: tomlLiteral(from), To: tomlLiteral(to)}
		if retiredRulebookKey(key) {
			change.To = "removed (no rule reads it)"
		}
		changes = append(changes, change)
	}
	var differs []string
	for key, v := range afterFlat {
		if want, ok := known[key]; !ok || !tomlValuesEqual(want, v) {
			differs = append(differs, key)
		}
	}
	slices.Sort(differs)
	return RulebookPolicyEdit{Path: path, Version: read.policy.Version, Changes: changes, Differs: differs, Review: policyFileReview(out)}, nil
}

// rulebookEditRelease labels a template the edit command writes; the CLI
// sets it to its own version.
var rulebookEditRelease = ""

// SetRulebookEditRelease names the release in templates the edit command
// writes.
func SetRulebookEditRelease(release string) { rulebookEditRelease = release }

// splitRulebookKey splits a dotted key into its table and leaf; a top-level
// key has no table.
func splitRulebookKey(key string) (table, leaf string) {
	if before, after, ok := strings.Cut(key, "."); ok {
		return before, after
	}
	return "", key
}

// DefaultRulebookPolicyTOML renders Canary's Rulebook defaults as the
// complete, commented file Canary materializes.
func DefaultRulebookPolicyTOML() ([]byte, error) {
	return RulebookPolicyTemplate(rulebookEditRelease), nil
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
