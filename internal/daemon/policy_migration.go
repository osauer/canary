package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/osauer/canary/v2/internal/risk"
)

// PolicyMigrationApproval binds an operator-reviewed conversion to the exact
// input and proposed bytes. It never supplies values or paths to the converter.
type PolicyMigrationApproval struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
}

func policyFileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func previewPolicyFileMigration(k policyFileKind, data []byte, release string) ([]byte, []string, []string, error) {
	out, changes, notes, err := k.migrate(data, release)
	if err != nil {
		return nil, nil, notes, err
	}
	// Low-revision constitutions retain their historical semantics. A format
	// conversion that enabled features would require a separate policy decision.
	if k.name == PolicyFileConstitution && bytes.Equal(out, data) && len(notes) > 0 {
		return data, nil, notes, nil
	}
	kind := map[string]string{PolicyFileRulebook: risk.RulebookPolicyKind, PolicyFileProtection: protectionPolicyKind, PolicyFileOpportunity: opportunityPolicyKind, PolicyFileConstitution: risk.ConstitutionKind}[k.name]
	var envelope struct {
		Kind string `toml:"kind"`
	}
	if _, err := toml.Decode(string(out), &envelope); err != nil {
		return nil, nil, notes, err
	}
	if envelope.Kind != kind {
		doc := parseTOMLDoc(out)
		doc.set("", "kind", tomlLiteral(kind), nil)
		out = doc.bytes()
		changes = append(changes, "use canonical kind "+kind+" (legacy alias remains readable)")
	}
	beforeKey, err := effectivePolicyFileKey(k.name, data)
	if err != nil {
		return nil, nil, notes, err
	}
	afterKey, err := effectivePolicyFileKey(k.name, out)
	if err != nil || beforeKey == "" || beforeKey != afterKey {
		return nil, nil, notes, fmt.Errorf("conversion would change effective %s settings; nothing written: %v", k.name, err)
	}
	if len(changes) > 0 && !bytes.Equal(data, out) {
		out = append(out, []byte("\n# Format/comment migration by Canary "+sanitizeReleaseLabel(release)+"; effective settings verified unchanged.\n")...)
	}
	return out, changes, notes, nil
}

func effectivePolicyFileKey(name string, data []byte) (string, error) {
	switch name {
	case PolicyFileRulebook:
		read, err := parseRulebookPolicy(data)
		return read.policy.EffectiveFingerprintKey(), err
	case PolicyFileProtection:
		p, _, err := parseProtectionPolicy(data)
		return effectiveProtectionPolicy(p).Key, err
	case PolicyFileOpportunity:
		p, err := parseOpportunityPolicy(data)
		return effectiveOpportunityPolicy(p).Key, err
	case PolicyFileConstitution:
		if err := parseConstitutionPolicy(data); err != nil {
			return "", err
		}
		var c risk.Constitution
		_, err := toml.Decode(string(data), &c)
		return c.EffectiveFingerprintKey(), err
	default:
		return "", fmt.Errorf("unknown policy %q", name)
	}
}

func migrateConstitutionPolicyFile(data []byte, _ string) ([]byte, []string, []string, error) {
	var c risk.Constitution
	if _, err := toml.Decode(string(data), &c); err != nil {
		return nil, nil, nil, err
	}
	if !c.Semantics().ProcessReminders {
		return data, nil, []string{"legacy accounting/reminder semantics retained; conversion needs a separate policy decision"}, nil
	}
	if c.SchemaVersion == 2 {
		return data, nil, nil, nil
	}
	doc := parseTOMLDoc(data)
	doc.set("", "schema_version", "2", nil)
	return doc.bytes(), []string{"schema 2 separates format semantics from document revision"}, nil, nil
}

func migrateOpportunityPolicyFile(data []byte, _ string) ([]byte, []string, []string, error) {
	p, err := parseOpportunityPolicy(data)
	if err != nil {
		return nil, nil, nil, err
	}
	doc := parseTOMLDoc(data)
	var changes []string
	if p.SchemaVersion != 2 {
		doc.set("", "schema_version", "2", nil)
		changes = append(changes, "schema 2; materialise legacy effective values, including zero/false")
	}
	e := p.Buckets.OptionExercise
	for _, row := range []struct {
		key   string
		value any
	}{
		{"enabled", e.Enabled}, {"min_total_gain", e.MinTotalGain}, {"min_gain_pct_intrinsic", e.MinGainPctIntrinsic},
		{"require_rth", e.RequireRTH}, {"max_quote_age", e.MaxQuoteAge}, {"require_american_style", e.RequireAmericanStyle},
	} {
		doc.set("buckets.option_exercise", row.key, tomlValueText(row.value), nil)
	}
	for _, key := range []string{"profile", "authority.exercise_reduce_only", "authority.auto_submit", "buckets.option_exercise.allow_no_option_bid"} {
		i := strings.LastIndex(key, ".")
		table, leaf := "", key
		if i >= 0 {
			table, leaf = key[:i], key[i+1:]
		}
		if doc.commentOut(table, leaf, "retired: "+opportunityPolicyHelp[key]) {
			changes = append(changes, "comment out "+key+" (no execution authority)")
		}
	}
	out := doc.bytes()
	if !bytes.Equal(out, data) && len(changes) == 0 {
		changes = append(changes, "materialise effective exercise settings")
	}
	return out, changes, nil, nil
}

// policyFileDiff keeps a common prefix/suffix and shows the exact changed
// region. The preview stays local; it can contain the owner's private settings.
func policyFileDiff(path string, before, after []byte) string {
	a, b := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	start, endA, endB := 0, len(a), len(b)
	for start < endA && start < endB && a[start] == b[start] {
		start++
	}
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s (proposed)\n@@ -%d,%d +%d,%d @@\n", path, path, start+1, endA-start, start+1, endB-start)
	for _, line := range a[start:endA] {
		out.WriteString("-" + line + "\n")
	}
	for _, line := range b[start:endB] {
		out.WriteString("+" + line + "\n")
	}
	return out.String()
}
