package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
)

// Policy files (owner decision 2026-09-26). Canary always materializes its
// policy files, so the file, never a compiled baseline, is what runs: a fresh
// install, and an upgrade that finds a file missing, write each one from the
// code template with a "Canary defaults, not yet reviewed" header. From then
// on the file is the source of truth and code defaults only generate the
// template. An existing file is left intact until the operator applies a
// reviewed, hash-bound migration plan. Conversion keeps effective settings,
// backs up the original and records provenance. Personal numbers
// and automation switches (constitution capital numbers, premium caps as a
// share of risk capital, pre-authorised buckets, automatic brake release) are
// written only as commented placeholders.

// Policy file names, as reported by the ensure step and the explain surface.
const (
	PolicyFileRulebook     = "rulebook"
	PolicyFileProtection   = "protection"
	PolicyFileOpportunity  = "opportunity"
	PolicyFileConstitution = "constitution"
)

// Ensure actions.
const (
	PolicyFileCreated      = "created"
	PolicyFileMigrated     = "migrated"
	PolicyFileUnchanged    = "unchanged"
	PolicyFileUnreadable   = "unreadable"
	PolicyFileFailed       = "failed"
	PolicyFileWouldCreate  = "would_create"
	PolicyFileWouldMigrate = "would_migrate"
)

// PolicyFileAction is what one ensure step did, or would do, to one file.
type PolicyFileAction struct {
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	Diff         string `json:"diff,omitempty"`
	Policy       string `json:"policy"`
	Path         string `json:"path"`
	Action       string `json:"action"`
	// Backup is the copy taken before an in-place migration.
	Backup string `json:"backup,omitempty"`
	// Changes lists each line-level change a migration made.
	Changes []string `json:"changes,omitempty"`
	// Notes carries recommendations the ensure step reports and never
	// applies ("Canary now recommends X; yours is Y") and why a file was left
	// alone.
	Notes []string `json:"notes,omitempty"`
	Error string   `json:"error,omitempty"`
}

// PolicyFileSet is the path of every policy file one ensure step covers,
// resolved exactly as the daemon's managers resolve them.
type PolicyFileSet struct {
	Rulebook     string
	Protection   string
	Opportunity  string
	Constitution string
	// OrderGateSource is config.toml [trading] as written: the source policy
	// ensure writes the constitution's [order_limits] from. It is never read
	// for a trading decision. OrderGateSourceRead is false when no config was
	// read, so only compiled values are known.
	OrderGateSource     config.Trading
	OrderGateSourceRead bool
}

// PolicyFileSetFor resolves the policy paths from a daemon config; nil means
// every default.
func PolicyFileSetFor(cfg *config.Resolved) PolicyFileSet {
	set := PolicyFileSet{
		Rulebook:     config.DefaultRulebookPolicyFile,
		Protection:   config.AutoTrade{}.WithDefaults().PolicyFile,
		Opportunity:  config.Opportunities{}.WithDefaults().PolicyFile,
		Constitution: riskPolicyDefaultPath,
	}
	if cfg != nil {
		set.Rulebook = cfg.Rulebook.PolicyFilePath()
		set.Protection = cfg.AutoTrade.WithDefaults().PolicyFile
		set.Opportunity = cfg.Opportunities.WithDefaults().PolicyFile
		set.OrderGateSource, set.OrderGateSourceRead = cfg.Trading, true
	}
	set.Rulebook = expandUserPath(strings.TrimSpace(set.Rulebook))
	set.Protection = expandUserPath(strings.TrimSpace(set.Protection))
	set.Opportunity = expandUserPath(strings.TrimSpace(set.Opportunity))
	set.Constitution = expandUserPath(strings.TrimSpace(set.Constitution))
	return set
}

// PolicyFileSetFromConfigFile loads the config the daemon would load and
// resolves the policy paths from it, reading them the way the daemon does: a
// part that cannot be read keeps its default path. Config whose account pins
// cannot be read falls back to the default paths, so the ensure step never
// blocks on it.
func PolicyFileSetFromConfigFile(path string) (PolicyFileSet, error) {
	cfg, _, err := config.LoadForDaemon(path)
	if err != nil {
		return PolicyFileSetFor(nil), err
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		return PolicyFileSetFor(nil), err
	}
	return PolicyFileSetFor(resolved), nil
}

// EnsureOptions tunes one ensure step.
type EnsureOptions struct {
	// Release names the Canary release in comments it writes.
	Release string
	Now     time.Time
	// DryRun reports what would change without writing anything.
	DryRun bool
	// Approved contains exact file digests from a reviewed dry-run plan.
	Approved map[string]PolicyMigrationApproval
	// ApplyMigrations applies each file's own migration, with a backup, as
	// daemon start does: an upgrade writes the defaults a release adds and
	// the daemon reads them from the file, so nobody runs a plan first
	// (owner instruction 2026-10-06 06:33 CEST).
	ApplyMigrations bool
}

// policyFileKind binds one policy file to its template, parser and migration.
type policyFileKind struct {
	name     string
	path     string
	template func(release string) []byte
	parse    func([]byte) error
	migrate  func(data []byte, release string) (out []byte, changes, notes []string, err error)
}

// EnsurePolicyFiles writes every missing policy file from its template and
// previews conversions of existing files; it applies one with exact reviewed
// hashes, or with ApplyMigrations at daemon start. It never replaces owner settings or touches a file it cannot read: an
// unreadable file keeps the policy in force and is reported, not replaced.
func EnsurePolicyFiles(set PolicyFileSet, opts EnsureOptions) []PolicyFileAction {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	release := strings.TrimSpace(opts.Release)
	if release == "" {
		release = "this release"
	}
	kinds := []policyFileKind{
		{PolicyFileRulebook, set.Rulebook, RulebookPolicyTemplate, func(b []byte) error { _, err := parseRulebookPolicy(b); return err }, migrateRulebookPolicyFile},
		{PolicyFileProtection, set.Protection, ProtectionPolicyTemplate, func(b []byte) error { _, _, err := parseProtectionPolicy(b); return err }, migrateProtectionPolicyFile},
		{PolicyFileOpportunity, set.Opportunity, OpportunityPolicyTemplate, func(b []byte) error { _, err := parseOpportunityPolicy(b); return err }, migrateOpportunityPolicyFile},
		{PolicyFileConstitution, set.Constitution, func(release string) []byte {
			return constitutionPolicyTemplateFrom(release, set.OrderGateSource, set.OrderGateSourceRead)
		}, parseConstitutionPolicy, migrateConstitutionPolicyFileFrom(set.OrderGateSource, set.OrderGateSourceRead)},
	}
	// Validate the whole reviewed set before any write. Applying a plan never
	// creates unrelated missing files, and an unknown/stale entry cannot cause
	// another entry to be applied first. Each file is checked again at write time.
	if len(opts.Approved) > 0 {
		selected := make([]policyFileKind, 0, len(opts.Approved))
		for name, approval := range opts.Approved {
			i := slices.IndexFunc(kinds, func(k policyFileKind) bool { return k.name == name })
			if i < 0 || kinds[i].path == "" || approval.Path != kinds[i].path {
				return []PolicyFileAction{{Policy: name, Path: approval.Path, Action: PolicyFileFailed, Error: "reviewed plan does not match the configured policy paths"}}
			}
			preview := ensurePolicyFile(kinds[i], release, EnsureOptions{DryRun: true})
			if preview.Action != PolicyFileWouldMigrate || approval.BeforeSHA256 != preview.BeforeSHA256 || approval.AfterSHA256 != preview.AfterSHA256 {
				return []PolicyFileAction{{Policy: name, Path: approval.Path, Action: PolicyFileFailed, Error: "file or proposed conversion changed since review; regenerate the plan"}}
			}
		}
		for _, k := range kinds {
			if _, ok := opts.Approved[k.name]; ok {
				selected = append(selected, k)
			}
		}
		kinds = selected
	}
	var out []PolicyFileAction
	for _, k := range kinds {
		if k.path == "" {
			continue
		}
		out = append(out, ensurePolicyFile(k, release, opts))
	}
	return out
}

func ensurePolicyFile(k policyFileKind, release string, opts EnsureOptions) PolicyFileAction {
	act := PolicyFileAction{Policy: k.name, Path: k.path}
	data, err := os.ReadFile(k.path)
	approval, approved := opts.Approved[k.name]
	if approved && (err != nil || approval.Path != k.path || approval.BeforeSHA256 != policyFileDigest(data)) {
		act.Action, act.Error = PolicyFileFailed, "file changed since review; nothing written"
		return act
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		if opts.DryRun {
			act.Action = PolicyFileWouldCreate
			return act
		}
		if err := writePrivateFileExclusive(k.path, k.template(release)); err != nil {
			if errors.Is(err, os.ErrExist) {
				// Another writer won the race; its file stands.
				act.Action = PolicyFileUnchanged
				return act
			}
			act.Action, act.Error = PolicyFileFailed, err.Error()
			return act
		}
		act.Action = PolicyFileCreated
		return act
	case err != nil:
		act.Action, act.Error = PolicyFileUnreadable, err.Error()
		act.Notes = append(act.Notes, "left untouched; the policy in force stays until the file reads again")
		return act
	}
	if err := k.parse(data); err != nil {
		act.Action, act.Error = PolicyFileUnreadable, err.Error()
		act.Notes = append(act.Notes, "left untouched; the policy in force stays until you fix the file")
		return act
	}
	if k.migrate == nil {
		act.Action = PolicyFileUnchanged
		return act
	}
	migrated, changes, notes, err := previewPolicyFileMigration(k, data, release)
	act.Notes = append(act.Notes, notes...)
	if err != nil {
		act.Action, act.Error = PolicyFileFailed, err.Error()
		act.Notes = append(act.Notes, "left untouched; migration refused")
		return act
	}
	if len(changes) == 0 || bytes.Equal(migrated, data) {
		act.Action = PolicyFileUnchanged
		return act
	}
	act.Changes = changes
	act.BeforeSHA256, act.AfterSHA256 = policyFileDigest(data), policyFileDigest(migrated)
	act.Diff = policyFileDiff(k.path, data, migrated)
	if !approved && opts.ApplyMigrations && !opts.DryRun {
		approval, approved = PolicyMigrationApproval{Path: k.path, BeforeSHA256: act.BeforeSHA256, AfterSHA256: act.AfterSHA256}, true
	}
	if opts.DryRun || !approved {
		act.Action = PolicyFileWouldMigrate
		act.Notes = append(act.Notes, "the daemon applies this at its next start, after backing the file up; policy ensure --apply-plan FILE applies a reviewed plan now")
		return act
	}

	if approval.Path != k.path || approval.BeforeSHA256 != act.BeforeSHA256 || approval.AfterSHA256 != act.AfterSHA256 {
		act.Action, act.Error = PolicyFileFailed, "file or proposed conversion changed since review; regenerate the plan"
		return act
	}
	current, err := os.ReadFile(k.path)
	if err != nil || !bytes.Equal(current, data) {
		act.Action, act.Error = PolicyFileFailed, "file changed during conversion; nothing written"
		return act
	}
	backup := fmt.Sprintf("%s.bak-%s-%s", k.path, sanitizeReleaseLabel(release), opts.Now.UTC().Format("20060102T150405Z"))
	if err := writePrivateFileExclusive(backup, data); err != nil {
		act.Action, act.Error = PolicyFileFailed, "backup: "+err.Error()
		return act
	}
	act.Backup = backup
	if err := writePrivateFile(k.path, migrated); err != nil {
		act.Action, act.Error = PolicyFileFailed, err.Error()
		return act
	}
	act.Action = PolicyFileMigrated
	return act
}

func sanitizeReleaseLabel(release string) string {
	var b strings.Builder
	for _, r := range release {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "canary"
	}
	return b.String()
}

// writePrivateFileExclusive creates path with owner-only permissions and
// fails with os.ErrExist when it already exists, so a template or backup can
// never replace a file.
func writePrivateFileExclusive(path string, data []byte) error {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, string(os.PathSeparator)); i > 0 {
		return path[:i]
	}
	return "."
}

// policyTemplateHeader is the opening block of every materialized file. The
// first line is the review marker; the owner deletes it once they have read
// the file. holdsValues says whether the file carries Canary's recommended
// values (every policy but the constitution, which holds placeholders only).
func policyTemplateHeader(b *strings.Builder, title, release string, holdsValues bool, body ...string) {
	b.WriteString(PolicyUnreviewedMarker + "\n")
	fmt.Fprintf(b, "# %s\n", title)
	who := "Canary"
	if release = strings.TrimSpace(release); release != "" {
		who += " " + release
	}
	if holdsValues {
		fmt.Fprintf(b, "# %s wrote this file from its defaults. The values are Canary's\n", who)
		b.WriteString("# recommendations until you review them: delete the first line of this file\n")
		b.WriteString("# once you have, and every surface reports the file as yours.\n")
	} else {
		fmt.Fprintf(b, "# %s wrote this skeleton. It holds no values: delete the first line of\n", who)
		b.WriteString("# this file once you have written yours, and every surface reports it as yours.\n")
	}
	for _, line := range body {
		if line == "" {
			b.WriteString("#\n")
			continue
		}
		b.WriteString("# " + line + "\n")
	}
	b.WriteString("# kind checks file type; schema_version selects format; policy_version records edits.\n")
	b.WriteString("# policy_id is stable identity. The review marker is a label, never trading approval.\n\n")
}

// tomlFloat renders a float as TOML keeps it a float: 30 is written 30.0.
func tomlFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func tomlValueText(v any) string {
	switch x := v.(type) {
	case float64:
		return tomlFloat(x)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case string:
		return strconv.Quote(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprint(x)
	}
}

// rulebookTemplateKey is one scalar key of the Rulebook template, in the
// order the file lists them.
type rulebookTemplateKey struct {
	key     string
	heading string
}

// rulebookTemplateKeys orders and explains every top-level Rulebook key. A
// test fails when a policy field has no entry here, so the template can
// never silently leave a limit to a compiled value.
var rulebookTemplateKeys = []rulebookTemplateKey{
	{"single_name_watch_pct", "Rule 1 — worst-case loss on one issuer"},
	{"single_name_act_pct", ""},
	{"takeover_gap_pct", ""},
	{"hedge_min_days", ""},
	{"exit_participation_pct", ""},
	{"illiquid_days_to_exit", ""},
	{"illiquid_watch_pct", ""},
	{"illiquid_act_pct", ""},
	{"delta_swing_watch_pct", "Rules 16–18 — concentration watches (never act, never trim)"},
	{"cluster_drop_pct", ""},
	{"cluster_watch_pct", ""},
	{"budget_watch_pct", ""},
	{"option_line_watch_pct", "Rule 2 — premium at risk in one option position"},
	{"option_line_act_pct", ""},
	{"hedge_line_watch_pct", ""},
	{"hedge_line_act_pct", ""},
	{"runway_watch_dte", "Rule 5 — options nearing expiry"},
	{"runway_act_dte", ""},
	{"runway_itm_delta_floor", ""},
	{"short_put_act_line_pct_nlv", "Rule 7 — short options through earnings"},
	{"short_put_act_name_pct_nlv", ""},
	{"earnings_freeze_sessions", "Rule 8 — position size near earnings"},
	{"red_on_green_name_drop_pct", "Rules 9–10 — intraday tape (off by default)"},
	{"red_on_green_spy_up_pct", ""},
	{"winner_trim_day_up_pct", ""},
	{"winner_trim_min_exposure_pct", ""},
	{"regime_stage_max_age_minutes", "Rules 3, 4, 12 and 15 — regime (their bands are in the [regime_*] tables below)"},
	{"overhedge_multiple", ""},
	{"exit_watch_loss_pct", "Rule 13 — long option loss limit"},
	{"exit_act_loss_pct", ""},
	{"fx_exposure_watch_pct", "Rule 14 — foreign-currency exposure"},
	{"margin_headroom_watch_pct", "Rule 19 — margin headroom (broker excess liquidity)"},
	{"margin_headroom_act_pct", ""},
	{"hedge_symbols", "Shared inputs"},
	{"greeks_gap_floor_pct_nlv", ""},
}

// rulebookRegimeKeys orders and reads every key of a [regime_*] table: rule 3's
// premium budget beside rule 4's time value budget, then rule 12's protection
// band and rule 15's net exposure bands. A test fails when a RegimeThresholds
// field has no entry here.
var rulebookRegimeKeys = []struct {
	key   string
	value func(risk.RegimeThresholds) float64
}{
	{"premium_budget_watch_pct", func(t risk.RegimeThresholds) float64 { return t.PremiumBudgetWatchPct }},
	{"premium_budget_act_pct", func(t risk.RegimeThresholds) float64 { return t.PremiumBudgetActPct }},
	{"extrinsic_watch_pct", func(t risk.RegimeThresholds) float64 { return t.ExtrinsicWatchPct }},
	{"extrinsic_act_pct", func(t risk.RegimeThresholds) float64 { return t.ExtrinsicActPct }},
	{"hedge_band_min_pct", func(t risk.RegimeThresholds) float64 { return t.HedgeBandMinPct }},
	{"hedge_band_max_pct", func(t risk.RegimeThresholds) float64 { return t.HedgeBandMaxPct }},
	{"net_exposure_watch_pct", func(t risk.RegimeThresholds) float64 { return t.NetExposureWatchPct }},
	{"net_exposure_act_pct", func(t risk.RegimeThresholds) float64 { return t.NetExposureActPct }},
}

// rulebookRegimeSets names the three [regime_*] tables with their values
// under p.
func rulebookRegimeSets(p risk.RulebookPolicy) []struct {
	name string
	t    risk.RegimeThresholds
} {
	return []struct {
		name string
		t    risk.RegimeThresholds
	}{{"regime_calm", p.RegimeCalm}, {"regime_early_warning", p.RegimeEarlyWarning}, {"regime_confirmed", p.RegimeConfirmed}}
}

// rulebookPolicyValues maps each toml key of the policy to its value.
func rulebookPolicyValues(p risk.RulebookPolicy) map[string]any {
	out := map[string]any{}
	v := reflect.ValueOf(p)
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		out[tag] = v.Field(i).Interface()
	}
	return out
}

// rulebookTemplateKeySet lists every key Canary's Rulebook template writes,
// as dotted names, in template order: the file is complete when it carries
// all of them.
func rulebookTemplateKeySet() []string {
	var out []string
	for _, k := range rulebookTemplateKeys {
		out = append(out, k.key)
	}
	for _, id := range risk.RuleIDs() {
		out = append(out, "modes."+id)
	}
	for _, set := range []string{"regime_calm", "regime_early_warning", "regime_confirmed"} {
		for _, k := range rulebookRegimeKeys {
			out = append(out, set+"."+k.key)
		}
	}
	return append(out, "issuer_groups", "clusters")
}

// RulebookPolicyTemplate renders Canary's Rulebook defaults as a complete,
// commented owner file.
func RulebookPolicyTemplate(release string) []byte {
	p := risk.DefaultRulebookPolicy()
	values := rulebookPolicyValues(p)
	var b strings.Builder
	policyTemplateHeader(&b, "Rulebook policy: the limits behind every `canary rules` verdict.", release, true,
		"",
		"Edits take effect with a higher policy_version (the daemon rereads the file",
		"every 30 seconds by default); `canary rules policy set KEY=VALUE` edits one key and",
		"raises it for you. An unreadable file never replaces the limits in force.",
		"Canary's current defaults: canary policy default rulebook",
		"NLV means account equity. Watch/act/freeze/trim describe findings or",
		"proposals, never permission to trade. Off hides a rule; it does not disable",
		"a separately enabled proposal bucket using that rule's thresholds.",
		"Rules 6 and 11 have no numerical knob here. Rules 9-10 use the US session.",
		"Regime premium, extrinsic and net exposure bands use % of NLV; hedge bands",
		"use % of gross long exposure. Watch/act edges are inclusive (at or above).",
		"Rule 12: inside edges passes, below the bottom or above the top watches (no",
		"protection at all is 0%), above its overhedge multiple acts. Rule 3 or 15 at",
		"watch or act reads sell-only. Rule 19 reads downward: it watches and acts",
		"strictly below its levels, and a reading exactly at a level passes it.")
	fmt.Fprintf(&b, "kind = %q\nschema_version = 1\npolicy_id = %q\npolicy_version = %d\n", risk.RulebookPolicyKind, p.ID, p.Version)
	for _, k := range rulebookTemplateKeys {
		if k.heading != "" {
			fmt.Fprintf(&b, "\n# %s\n", k.heading)
		}
		writePolicyComment(&b, rulebookPolicyHelp[k.key])
		fmt.Fprintf(&b, "%s = %s\n", k.key, tomlValueText(values[k.key]))
	}
	writePolicyComment(&b, rulebookPolicyHelp["modes"])
	b.WriteString("[modes]\n")
	for _, id := range risk.RuleIDs() {
		fmt.Fprintf(&b, "%s = %q\n", id, p.ModeFor(id))
	}
	notes := map[string]string{
		"regime_calm":          "Rule 3 premium budget, rule 4 time value budget, rule 12 protection band\n# and rule 15 net exposure, calm regime.",
		"regime_early_warning": "The same in an early-warning regime.",
		"regime_confirmed":     "The same in confirmed stress.",
	}
	for _, set := range rulebookRegimeSets(p) {
		fmt.Fprintf(&b, "\n# %s\n[%s]\n", notes[set.name], set.name)
		for _, field := range rulebookRegimeKeys {
			writePolicyComment(&b, rulebookPolicyHelp[set.name+"."+field.key])
			fmt.Fprintf(&b, "%s = %s\n", field.key, tomlFloat(field.value(set.t)))
		}
	}
	b.WriteString(rulebookGroupsTemplate)
	return []byte(b.String())
}

// rulebookGroupsTemplate is the issuer group and cluster tables: empty, with
// an example, because Canary has no issuer data to fill them.
const rulebookGroupsTemplate = `
# Issuer groups: share classes and ADR/ordinary lines that are one issuer.
# Canary has no issuer data, so every symbol is its own issuer until you list
# it here. Example:
#   GroupA = ["AAA", "AAB"]
[issuer_groups]

# Clusters for rule 17: related issuers tested falling together. Members are
# symbols or issuer group names. Example:
#   ClusterA = ["AAA", "BBB", "CCC"]
# Empty means rule 17 has no cluster to evaluate.
[clusters]
`

// rulebookRecommendations are the keys whose meaning or recommended value
// changed in a release. Migration reports each one the file sets and never
// applies the recommendation.
var rulebookRecommendations = []struct {
	key  string
	note func(yours any) string
}{
	{"single_name_watch_pct", func(yours any) string {
		return fmt.Sprintf("single_name_watch_pct now bounds the worst-case loss on one issuer with every leg netted (before amendment 15 it bounded stock-equivalent delta): Canary now recommends %s; yours is %s", tomlFloat(risk.DefaultRulebookPolicy().SingleNameWatchPct), tomlLiteral(yours))
	}},
	{"single_name_act_pct", func(yours any) string {
		return fmt.Sprintf("single_name_act_pct now caps the worst-case loss on one issuer with every leg netted (before amendment 15 it capped stock-equivalent delta): Canary now recommends %s; yours is %s", tomlFloat(risk.DefaultRulebookPolicy().SingleNameActPct), tomlLiteral(yours))
	}},
}

// migrateRulebookPolicyFile adds every key the file lacks at Canary's
// default, comments out retired keys, and reports recommendations. The
// effective policy is unchanged by construction (an absent key already read
// as its default, and a retired key was ignored); the migration verifies
// that, so no policy_version bump is needed or made.
func migrateRulebookPolicyFile(data []byte, release string) ([]byte, []string, []string, error) {
	before, err := parseRulebookPolicy(data)
	if err != nil {
		return data, nil, nil, err
	}
	var raw map[string]any
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return data, nil, nil, err
	}
	defined := map[string]bool{}
	for _, k := range md.Keys() {
		defined[k.String()] = true
	}
	flat := flattenTOMLMap(raw)
	doc := parseTOMLDoc(data)
	var changes, notes []string
	added := func(key string) {
		changes = append(changes, "added "+key+" at Canary's default")
	}
	p := risk.DefaultRulebookPolicy()
	values := rulebookPolicyValues(p)
	for _, k := range rulebookTemplateKeys {
		if defined[k.key] {
			continue
		}
		doc.insert("", []string{fmt.Sprintf("# Added by Canary %s at its default: %s", release, rulebookPolicyHelp[k.key]), k.key + " = " + tomlValueText(values[k.key])})
		added(k.key)
	}
	for _, id := range risk.RuleIDs() {
		if defined["modes."+id] {
			continue
		}
		if !defined["modes"] || doc.headerLine("modes") >= 0 {
			doc.insert("modes", []string{fmt.Sprintf("%s = %q  # added by Canary %s at its default", id, p.ModeFor(id), release)})
			added("modes." + id)
		} else {
			notes = append(notes, "modes."+id+" is not in the file and follows Canary's default; the file's modes table is not a [modes] section, so it was not edited")
		}
	}
	for _, set := range rulebookRegimeSets(p) {
		for _, kv := range rulebookRegimeKeys {
			full := set.name + "." + kv.key
			if defined[full] {
				continue
			}
			if defined[set.name] && doc.headerLine(set.name) < 0 {
				notes = append(notes, full+" is not in the file and follows Canary's default; the table is not a ["+set.name+"] section, so it was not edited")
				continue
			}
			doc.insert(set.name, []string{fmt.Sprintf("%s = %s  # added by Canary %s at its default", kv.key, tomlFloat(kv.value(set.t)), release)})
			added(full)
		}
	}
	for _, table := range []string{"issuer_groups", "clusters"} {
		if defined[table] {
			continue
		}
		block := strings.Split(strings.TrimPrefix(rulebookGroupsTemplate, "\n"), "\n")
		start := slices.IndexFunc(block, func(l string) bool {
			return strings.HasPrefix(l, "# ") && strings.Contains(l, map[string]string{"issuer_groups": "Issuer groups", "clusters": "Clusters for rule 17"}[table])
		})
		end := slices.Index(block, "["+table+"]")
		if start >= 0 && end > start {
			lines := append([]string{"", fmt.Sprintf("# Added by Canary %s (empty; fill it in).", release)}, block[start:end+1]...)
			doc.lines = append(doc.lines, lines...)
			added(table)
		}
	}
	for key := range flat {
		if !retiredRulebookKey(key) {
			continue
		}
		table, leaf := splitRulebookKey(key)
		if doc.commentOut(table, leaf, fmt.Sprintf("retired by Canary %s: no operational consumer; see the current policy reference", release)) {
			changes = append(changes, "commented out retired "+key)
			if note := retiredRulebookMigrationNote(key, flat[key]); note != "" {
				notes = append(notes, note)
			}
		}
	}
	if len(changes) == 0 {
		// Nothing to migrate: the file already carries this release's keys,
		// so its recommendations were reported when it was migrated.
		return data, nil, nil, nil
	}
	for _, r := range rulebookRecommendations {
		if yours, ok := flat[r.key]; ok {
			notes = append(notes, r.note(yours))
		}
	}
	out := doc.bytes()
	after, err := parseRulebookPolicy(out)
	if err != nil {
		return data, nil, notes, fmt.Errorf("migrated file does not parse: %w", err)
	}
	if before.policy.EffectiveFingerprintKey() != after.policy.EffectiveFingerprintKey() {
		return data, nil, notes, fmt.Errorf("migration would change the policy in force; nothing written")
	}
	slices.Sort(changes)
	return out, changes, notes, nil
}

// migrateProtectionPolicyFile comments out retired keys, naming where each
// concept went and the value the owner had. It adds no protection table: an
// absent protection table has its own meaning (an absent bucket table is
// disabled), so filling one in could switch a bucket on. It moves cash
// management to [cash] (owner decision 2026-10-06 06:08 CEST): the
// [buckets.cash_sweep] tables become [cash.sweep], and cash_sweep in
// [authority] pre_authorised becomes a [cash] pre_authorised entry, which
// changes no setting. Into an existing [cash.sweep] section it writes each
// missing sizing number at the owner-approved value (cashSweepWrittenDefaults)
// and raises policy_version, because the sweep reads those numbers from the
// file only; every value the file already writes is kept (owner decision
// 2026-10-05 18:35 CEST). A file without [cash] confirmation_window gets the
// owner's 10 minutes (owner decision 2026-10-06 15:31 CEST), with
// policy_version raised the same way.
func migrateProtectionPolicyFile(data []byte, release string) ([]byte, []string, []string, error) {
	before, _, err := parseProtectionPolicy(data)
	if err != nil {
		return data, nil, nil, err
	}
	var raw map[string]any
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return data, nil, nil, err
	}
	doc := parseTOMLDoc(data)
	var changes, notes []string
	moved, movedNotes := moveLegacyCashTables(doc, md, raw, release)
	changes, notes = append(changes, moved...), append(notes, movedNotes...)
	if len(moved) > 0 {
		// What follows writes at the new place: read the moved document.
		raw = nil
		if md, err = toml.Decode(string(doc.bytes()), &raw); err != nil {
			return data, nil, notes, fmt.Errorf("moved file does not parse: %w", err)
		}
	}
	flat := flattenTOMLMap(raw)
	materialised := false
	if md.IsDefined("cash", "sweep") {
		var missing []cashSweepWrittenDefault
		for _, d := range cashSweepWrittenDefaults {
			if !md.IsDefined("cash", "sweep", d.key) {
				missing = append(missing, d)
			}
		}
		switch {
		case len(missing) == 0:
		case doc.headerLine("cash.sweep") < 0:
			notes = append(notes, "[cash.sweep] is not a plain section, so its missing sizing numbers were not written; the sweep holds until you write them")
		default:
			for _, d := range missing {
				doc.insert("cash.sweep", []string{fmt.Sprintf("%s = %s  # written by Canary %s; the sweep reads it from this file only", d.key, d.value, release)})
				changes = append(changes, fmt.Sprintf("added cash.sweep.%s = %s", d.key, d.value))
			}
			materialised = true
		}
	}
	levelingChanges, levelingNotes := materialiseCurrencyLeveling(doc, md, release)
	changes, notes = append(changes, levelingChanges...), append(notes, levelingNotes...)
	windowChanges, windowNotes := materialiseCashConfirmationWindow(doc, md, release)
	changes, notes = append(changes, windowChanges...), append(notes, windowNotes...)
	if len(windowChanges) > 0 {
		levelingChanges = append(levelingChanges, windowChanges...)
	}
	if materialised || len(levelingChanges) > 0 {
		what := "the sweep sizing numbers above take effect"
		if len(levelingChanges) > 0 {
			what = "the keys above take effect"
		}
		doc.set("", "policy_version", strconv.Itoa(before.PolicyVersion+1), nil)
		changes = append(changes, fmt.Sprintf("raised policy_version %d to %d: %s", before.PolicyVersion, before.PolicyVersion+1, what))
		materialised = true
	}
	keys := make([]string, 0, len(retiredProtectionKeys))
	for k := range retiredProtectionKeys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		yours, ok := flat[key]
		if !ok {
			continue
		}
		i := strings.LastIndex(key, ".")
		table, leaf := key[:i], key[i+1:]
		if !doc.commentOut(table, leaf, fmt.Sprintf("retired by Canary %s: %s", release, retiredProtectionKeys[key])) {
			notes = append(notes, key+" is retired and ignored; it is not a plain line in a ["+table+"] section, so it was not edited")
			continue
		}
		changes = append(changes, "commented out retired "+key)
		if key == "buckets.risk_reduction.single_name_target_pct_nlv" {
			notes = append(notes, fmt.Sprintf("the risk-reduction trim now goes back to the Rulebook's issuer watch level: Canary now recommends %s (single_name_watch_pct, on worst-case loss); yours was %s (on market value) and is kept only as a comment",
				tomlFloat(risk.DefaultRulebookPolicy().SingleNameWatchPct), tomlLiteral(yours)))
		}
	}
	if len(changes) == 0 {
		return data, nil, notes, nil
	}
	out := doc.bytes()
	after, _, err := parseProtectionPolicy(out)
	if err != nil {
		return data, nil, notes, fmt.Errorf("migrated file does not parse: %w", err)
	}
	if !materialised && fingerprintProtectionPolicy(before).Key != fingerprintProtectionPolicy(after).Key {
		return data, nil, notes, fmt.Errorf("migration would change the policy in force; nothing written")
	}
	if materialised && !protectionMaterialisationPreserves(before, after) {
		return data, nil, notes, fmt.Errorf("migration would change more than the missing cash sweep and currency leveling numbers; nothing written")
	}
	return out, changes, notes, nil
}

// cashConfirmationWindowLines is [cash] confirmation_window as Canary writes
// it, with the owner's decision beside it (owner question 1 of the cash
// settings design, answered 2026-10-06 15:31 CEST).
func cashConfirmationWindowLines(release string) []string {
	return []string{
		"# How long, after you confirm a save from Desk's Settings on your passkey or",
		"# companion, a further save from the same Desk console may rely on that",
		"# confirmation. A save that lets more reach the broker always asks your",
		"# device, and \"0s\" asks it for every save. Owner decision 2026-10-06 15:31",
		"# CEST: \"Cache the decision for 5 or 10 minutes, if not serious concerns\".",
		"confirmation_window = " + strconv.Quote(cashConfirmationWindowWritten) + "  # written by Canary " + sanitizeReleaseLabel(release),
	}
}

// materialiseCashConfirmationWindow writes [cash] confirmation_window into a
// file that has none, so an upgraded install relies on the device's
// confirmation the way the owner decided without a step of its own. Until it
// is written, every save from Desk asks the device. A new [cash] table goes
// before the first [cash.*] table, with the comments above it.
func materialiseCashConfirmationWindow(doc *tomlDoc, md toml.MetaData, release string) (changes, notes []string) {
	if md.IsDefined("cash", "confirmation_window") {
		return nil, nil
	}
	if _, inline := doc.find("", "cash"); inline {
		return nil, []string{"[cash] is not a plain section, so confirmation_window was not written; every cash settings save from Desk asks your device until you write it"}
	}
	lines := cashConfirmationWindowLines(release)
	if doc.headerLine("cash") >= 0 {
		doc.insert("cash", lines)
		return []string{"added cash.confirmation_window = " + strconv.Quote(cashConfirmationWindowWritten)}, nil
	}
	at := -1
	for i, line := range doc.lines {
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], "cash.") {
			at = i
			break
		}
	}
	if at < 0 {
		doc.insert("cash", lines)
		return []string{"added [cash] with confirmation_window = " + strconv.Quote(cashConfirmationWindowWritten)}, nil
	}
	for at > 0 && strings.HasPrefix(strings.TrimSpace(doc.lines[at-1]), "#") {
		at--
	}
	block := append(append([]string{"[cash]"}, lines...), "")
	doc.lines = append(doc.lines[:at], append(block, doc.lines[at:]...)...)
	return []string{"added [cash] with confirmation_window = " + strconv.Quote(cashConfirmationWindowWritten)}, nil
}

// moveLegacyCashTables moves what a file written before cash management moved
// to [cash] still carries: the [buckets.cash_sweep] headers become
// [cash.sweep], keys and comments moving with them, and cash_sweep in
// [authority] pre_authorised becomes a [cash] pre_authorised entry. The
// policy in force is the same before and after (parsing reads the legacy
// places the same way), so policy_version stays. A legacy table written as
// dotted keys or an inline table is left in place with a note; it still
// reads.
func moveLegacyCashTables(doc *tomlDoc, md toml.MetaData, raw map[string]any, release string) (changes, notes []string) {
	if md.IsDefined("buckets", "cash_sweep") {
		if doc.headerLine("buckets.cash_sweep") >= 0 && doc.renameTable("buckets.cash_sweep", "cash.sweep") {
			changes = append(changes, "moved [buckets.cash_sweep] to [cash.sweep]: cash management has its own section")
		} else {
			notes = append(notes, "[buckets.cash_sweep] is not a plain section, so it was not moved to [cash.sweep]; it still reads, and you can move it by hand")
		}
	}
	authority, _ := raw["authority"].(map[string]any)
	listed, _ := authority["pre_authorised"].([]any)
	var keep []string
	found := false
	for _, v := range listed {
		name, _ := v.(string)
		if name == preAuthorisedBucketCashSweep {
			found = true
			continue
		}
		keep = append(keep, name)
	}
	if !found {
		return changes, notes
	}
	if _, ok := doc.find("authority", "pre_authorised"); !ok {
		notes = append(notes, "cash_sweep in [authority] pre_authorised is not a plain line, so it was not moved to [cash]; it still reads as the [cash] entry")
		return changes, notes
	}
	cash, _ := raw["cash"].(map[string]any)
	var cashList []string
	for _, v := range func() []any { l, _ := cash["pre_authorised"].([]any); return l }() {
		if name, ok := v.(string); ok {
			cashList = append(cashList, name)
		}
	}
	if keep == nil {
		keep = []string{}
	}
	doc.set("authority", "pre_authorised", tomlLiteral(keep), nil)
	if !slices.Contains(cashList, preAuthorisedBucketCashSweep) {
		cashList = append(cashList, preAuthorisedBucketCashSweep)
		comment := []string{"# The cash sweep's pre-authorisation, moved from [authority] by Canary " + release + "."}
		doc.set("cash", "pre_authorised", tomlLiteral(cashList), comment)
	}
	changes = append(changes, "moved cash_sweep from [authority] pre_authorised to [cash] pre_authorised: the cash sweep has its own authority")
	return changes, notes
}

// protectionMaterialisationKey is the effective protection key with the
// cash sweep's materialisable sizing numbers, the currency leveling table and
// [cash] confirmation_window cleared: a conversion that only writes missing
// numbers keeps it, and protectionMaterialisationPreserves checks those key by
// key.
func protectionMaterialisationKey(p protectionPolicy) string {
	p.Cash.Leveling, p.Cash.ConfirmationWindow = nil, ""
	if c := p.Cash.Sweep; c != nil {
		cleared := *c
		cleared.MaxOrderNotional, cleared.MaxOrderPctNLV, cleared.MinOrderNotional = 0, nil, nil
		cleared.ReserveFloorBase, cleared.ReservePctNLV, cleared.KeepCash, cleared.BillsExemptFromTradingMaxNotional = nil, nil, nil, nil
		cleared.NoBuyWhileBorrowed, cleared.OrderStepBase = nil, nil
		p.Cash.Sweep = &cleared
	}
	return effectiveProtectionPolicy(p).Key
}

// protectionMaterialisationPreserves reports whether after differs from
// before only by cash sweep sizing numbers before did not write, the
// currency leveling keys policy ensure writes and the confirmation_window
// Canary writes where there was none.
func protectionMaterialisationPreserves(before, after protectionPolicy) bool {
	if protectionMaterialisationKey(before) != protectionMaterialisationKey(after) ||
		!currencyLevelingMaterialisationPreserves(before.Cash.Leveling, after.Cash.Leveling) {
		return false
	}
	switch b, a := before.Cash.ConfirmationWindow, after.Cash.ConfirmationWindow; {
	case b != "" && a != b, b == "" && a != "" && a != cashConfirmationWindowWritten:
		return false
	}
	b, a := before.Cash.Sweep, after.Cash.Sweep
	if b == nil || a == nil {
		return b == a
	}
	if b.MaxOrderNotional != 0 && b.MaxOrderNotional != a.MaxOrderNotional {
		return false
	}
	for _, f := range [][2]*float64{{b.MaxOrderPctNLV, a.MaxOrderPctNLV}, {b.MinOrderNotional, a.MinOrderNotional}, {b.ReserveFloorBase, a.ReserveFloorBase}, {b.ReservePctNLV, a.ReservePctNLV}, {b.OrderStepBase, a.OrderStepBase}, {b.KeepCash, a.KeepCash}} {
		if f[0] != nil && (f[1] == nil || *f[0] != *f[1]) {
			return false
		}
	}
	for _, f := range [][2]*bool{{b.BillsExemptFromTradingMaxNotional, a.BillsExemptFromTradingMaxNotional}, {b.NoBuyWhileBorrowed, a.NoBuyWhileBorrowed}} {
		if f[0] != nil && (f[1] == nil || *f[0] != *f[1]) {
			return false
		}
	}
	return true
}

// ProtectionPolicyTemplate renders Canary's protection defaults as a complete
// owner file. Automatic submission (pre_authorised) and the premium budget
// governor's caps as a share of risk capital are commented placeholders: they
// are the owner's to decide, and each feature stays off until written.
func ProtectionPolicyTemplate(release string) []byte {
	p := defaultProtectionPolicy()
	t, se, o := p.Buckets.ThetaHygiene, p.Buckets.TrailingStop.StockETF, p.Buckets.TrailingStop.Options
	var b strings.Builder
	policyTemplateHeader(&b, "Protection policy: the reduce-only proposals Canary stages for you.", release, true,
		"",
		"Edits take effect with a higher policy_version (the daemon rereads the file",
		"every 30 seconds). A broken or drifted file never blocks an exit or a trim:",
		"the policy in force keeps proposing, and only automatic submission pauses.",
		"Canary's current defaults: canary policy default protection")
	fmt.Fprintf(&b, "kind = %q\nschema_version = 1\npolicy_id = %q\npolicy_version = %d\nprofile = %q\n\n", protectionPolicyKind, p.PolicyID, p.PolicyVersion, p.Profile)
	b.WriteString(`[authority]
# Proposals only reduce or close positions; both keys are fixed in this schema.
close_reduce_only = true
auto_submit = false
# Automatic submission is yours to decide, so Canary writes no value. List the
# protection buckets the daemon may place itself after a notice and a veto
# window, then raise policy_version. Choices: trailing_stop, option_loss_exit,
# option_profit_trail, budget_reduction. The cash sweep is pre-authorised in
# [cash] below; the veto window here serves both.
# pre_authorised = []   # for example ["trailing_stop"]
# veto_window = "30m"

`)
	fmt.Fprintf(&b, `# Close near-dated options whose time value is decaying.
[buckets.theta_hygiene]
enabled = %t
max_dte = %d
min_abs_theta_per_day = %s
min_extrinsic_pct_of_mark = %s
max_spread_pct_of_mid = %s

# Trim an issuer at the Rulebook's issuer cap (rule 1, single_name_act_pct in
# rulebook-policy.toml) back to its watch level (single_name_watch_pct), on
# worst-case loss with every leg netted. The levels live in the Rulebook.
[buckets.risk_reduction]
enabled = %t
max_order_notional = %s

# Broker-side trailing stops for stock and ETF positions.
[buckets.trailing_stop]
enabled = %t
# tif = "DAY"   # DAY (the default) or GTC to keep stops working overnight

[buckets.trailing_stop.stock_etf]
enabled = %t
order_type = %q
default_pct = %s
fallback_pct = %s
min_pct = %s
max_pct = %s
max_spread_pct_of_mid = %s

# Directional long-option loss exits and profit trails (off by default).
[buckets.trailing_stop.options]
enabled = %t
default_long_calls_directional = %t
default_index_puts_protection = %t
min_dte = %d
profit_arm_gain_pct = %s
locked_gain_pct = %s
tif = %q
order_type = %q
default_pct = %s
min_pct = %s
max_pct = %s
max_spread_pct_of_mid = %s
min_trail_abs = %s
spread_multiple = %s
allow_short_profit_trail = %t
# Required before option exits can be enabled: the limit offset you approve.
# limit_offset_abs = %s
# Exact contracts you approve for an independent exit, sorted by con_id:
# directional_intents = [
#   { con_id = 0, reason = "why", approved_at = 2026-01-01T00:00:00Z, expires_at = 2026-02-01T00:00:00Z, independent_exit = true },
# ]
`, t.Enabled, t.MaxDTE, tomlFloat(t.MinAbsThetaPerDay), tomlFloat(t.MinExtrinsicPctOfMark), tomlFloat(t.MaxSpreadPctOfMid),
		p.Buckets.RiskReduction.Enabled, tomlFloat(p.Buckets.RiskReduction.MaxOrderNotional),
		p.Buckets.TrailingStop.Enabled,
		se.Enabled, se.OrderType, tomlFloat(se.DefaultPct), tomlFloat(se.FallbackPct), tomlFloat(se.MinPct), tomlFloat(se.MaxPct), tomlFloat(se.MaxSpreadPctOfMid),
		o.Enabled, o.DefaultLongCallsDirectional, o.DefaultIndexPutsProtection, o.MinDTE, tomlFloat(o.ProfitArmGainPct), tomlFloat(o.LockedGainPct), o.TIF, o.OrderType,
		tomlFloat(o.DefaultPct), tomlFloat(o.MinPct), tomlFloat(o.MaxPct), tomlFloat(o.MaxSpreadPctOfMid), tomlFloat(o.MinTrailAbs), tomlFloat(o.SpreadMultiple), o.AllowShortProfitTrail,
		tomlFloat(o.LimitOffsetAbs))
	b.WriteString(`
# Premium budget governor: cuts long option premium back to a share of the
# constitution's declared risk capital while the drawdown brake is latched.
# The caps are your numbers, so Canary writes none; until you write them the
# governor stays off and reports that it needs your number.
# [buckets.budget_reduction]
# enabled = false
# mode = "shadow"   # shadow lists and journals; active stages the sells
# basis = "declared_risk_capital"   # or "rulebook": the Rulebook's premium budget and per-line limit
# premium_at_risk_pct_of_risk_capital = 0.0
# per_line_pct_of_risk_capital = 0.0
# max_order_notional = 0.0
`)
	b.WriteString(`
# Cash management: the cash sweep and currency leveling buy bills and convert
# currencies under this section's own authority, not as protection;
# protection above stays close-or-reduce only. List cash_sweep to let the
# daemon place the sweep's bill orders itself after the full veto window
# ([authority] veto_window), each held to the sweep's order cap in force, then raise
# policy_version. Currency leveling is always yours to approve.
[cash]
# pre_authorised = []   # for example ["cash_sweep"]
`)
	b.WriteString(strings.Join(cashConfirmationWindowLines(release), "\n") + "\n\n")
	writeCashSweepTemplate(&b)
	writeCurrencyLevelingTemplate(&b)
	return []byte(b.String())
}

// writeCashSweepTemplate appends the cash sweep as a commented placeholder:
// the sweep is the owner's to switch on, so the file Canary writes leaves it
// off. The sizing lines show the values the ensure step writes into an enabled
// table (cashSweepWrittenDefaults); the EUR block shows the compiled
// instrument defaults and the owner-approved example fallback ETF (O3).
func writeCashSweepTemplate(b *strings.Builder) {
	eur := defaultCashSweepCurrency("EUR")
	written := map[string]string{}
	for _, d := range cashSweepWrittenDefaults {
		written[d.key] = d.value
	}
	fmt.Fprintf(b, `
# Cash sweep: keeps a reserve as cash and puts the rest into bills of the same
# currency; it never converts. Every sizing number is read from this file
# only: a missing one holds the sweep at needs_your_number naming the key, and
# canary policy ensure writes the values shown below. In active
# mode a row is an ordinary proposal: its bill order (BILL or BOND, LMT DAY) is
# previewed under every gate and placed on your approval, or by the daemon
# after the full veto window when cash_sweep is in [cash] pre_authorised. USD bills
# come from TreasuryDirect's public list; EUR, GBP and CAD bills from the
# isins you list. A currency without its own table follows Canary's default:
# USD us_tbill; EUR de_bubill and fr_btf with an etf fallback; GBP uk_tbill;
# CAD ca_tbill; any other currency none. USD and EUR bills settle on Canary's
# maintained route (SMART; USD T+1, EUR T+2 and T+1 for trades from
# 2027-10-11); settlement_exchange and settlement_days in a currency table
# override it, and settlement_valid_through only ends an override on a date.
# [cash.sweep]
# enabled = false
# mode = "shadow"   # shadow lists and journals; active stages the orders
# currency_priority = "usd_first"   # usd_first | balanced | eur_first; existing native cash only
# reserve_cushion_eur = 10000.0   # ONE total cushion; funding-weighted EUR/USD split
# These opt into calibrated reserves. Missing funding/stress/exit evidence holds;
# configuration cannot commission the unfinished reserve observer or authorize FX.
# Reserve kept as cash, in base: the largest of reserve_floor_base,
# reserve_pct_nlv of net liquidation value and cash planned exercises or
# withdrawals need. It is held in the base currency; what base cash cannot
# hold is kept in the other currencies before they invest.
# reserve_floor_base = %s
# reserve_pct_nlv = %s   # percent of NLV
# One order, in base: at least min_order_notional (a redemption that restores
# cash is never held to it), at most the larger of max_order_notional and
# max_order_pct_nlv of NLV.
# min_order_notional = %s
# max_order_notional = %s
# max_order_pct_nlv = %s   # percent of NLV
# order_step_base = %s   # grid in base: buys round down, redemptions up
# keep_cash = %s   # settlement float in each currency's own unit; a currency table may override it
# Same-currency bill orders may pass the order cap in force ([order_limits])
# up to the sweep's
# own cap in force; anything else keeps the order cap. Absent means false.
# bills_exempt_from_trading_max_notional = %s
# No bill buys while any currency's cash is negative (a margin loan costs more
# than a bill earns) or a currency's cash is unknown; redemptions still sell.
# Canary never converts: convert or deposit to repay.
# no_buy_while_borrowed = %s
# min_net_gain = 25.0   # incremental purchase gain in base, including cash interest forgone
# tax_reviewed_at = 2026-01-01   # when you reviewed the tax on bill rolls; advisory, blocks nothing
#
# [cash.sweep.currency.EUR]
# instruments = [%q, %q]
# isins = []   # the EUR bills the sweep may buy, by ISIN (DE… de_bubill, FR… fr_btf); empty reads universe_unavailable
# fallback = %q   # used only after a contract search finds no bill line
# etf_symbol = "XEON"   # example: a euro money-market ETF on Xetra
# etf_exchange = "IBIS"   # Xetra
# keep_cash = %s   # overrides the bucket's keep_cash for EUR
# cash_interest_rate_upper = 0.0   # EXAMPLE ONLY: review an annual decimal upper bound; missing is unknown
# cash_interest_valid_through = 2026-01-01   # EXAMPLE ONLY: expires; does not attest today's rate
# settlement_exchange = "SMART"   # optional override; Canary's route needs no line here
# settlement_days = 2   # optional override of the route's lag in payment business days
# settlement_valid_through = 2027-06-30   # optional end date for this currency's route; omit it to follow Canary
# min_maturity_days = %d
# max_maturity_days = %d
# ladder_rungs = %d
`, written["reserve_floor_base"], written["reserve_pct_nlv"], written["min_order_notional"], written["max_order_notional"], written["max_order_pct_nlv"], written["order_step_base"], written["keep_cash"],
		written["bills_exempt_from_trading_max_notional"], written["no_buy_while_borrowed"], eur.Instruments[0], eur.Instruments[1], eur.Fallback, written["keep_cash"], eur.MinMaturityDays, eur.MaxMaturityDays, eur.LadderRungs)
}

// OpportunityPolicyTemplate renders Canary's option-exercise defaults as a
// complete owner file.
func OpportunityPolicyTemplate(release string) []byte {
	p := defaultOpportunityPolicy()
	e := p.Buckets.OptionExercise
	var b strings.Builder
	policyTemplateHeader(&b, "Opportunity policy: early option-exercise candidates.", release, true,
		"Material edits need a higher policy_version; reload is every 30 seconds by default.",
		"Candidates require a close/reduce effect and an option bid. No key here enables",
		"automatic exercise. Every submission still needs permission and broker checks.",
		"An invalid file retains the last good policy but pauses dependent exercise.",
		"Schema 2 requires each real setting when enabled. Values below are Canary defaults.")
	fmt.Fprintf(&b, "kind = %q\nschema_version = 2\npolicy_id = %q\npolicy_version = %d\n\n[buckets.option_exercise]\n", opportunityPolicyKind, p.PolicyID, p.PolicyVersion)
	for _, row := range []struct {
		key   string
		value any
	}{
		{"enabled", e.Enabled}, {"min_total_gain", e.MinTotalGain}, {"min_gain_pct_intrinsic", e.MinGainPctIntrinsic},
		{"require_rth", e.RequireRTH}, {"max_quote_age", e.MaxQuoteAge}, {"require_american_style", e.RequireAmericanStyle},
	} {
		writePolicyComment(&b, opportunityPolicyHelp["buckets.option_exercise."+row.key])
		fmt.Fprintf(&b, "%s = %s\n", row.key, tomlValueText(row.value))
	}
	return []byte(b.String())
}

// writePolicyComment wraps the same field explanation used by the reference.
func writePolicyComment(b *strings.Builder, text string) {
	line := "#"
	for word := range strings.FieldsSeq(text) {
		if len(line)+1+len(word) > 82 {
			b.WriteString(line + "\n")
			line = "#"
		}
		line += " " + word
	}
	b.WriteString(line + "\n")
}

// ConstitutionPolicyTemplate renders the risk constitution skeleton with the
// compiled order limits; ensure writes it from config.toml [trading] through
// constitutionPolicyTemplateFrom.
func ConstitutionPolicyTemplate(release string) []byte {
	return constitutionPolicyTemplateFrom(release, config.Trading{}, false)
}

// constitutionPolicyTemplateFrom renders the risk constitution skeleton. It
// keeps "no embedded default" for capital: every capital number and the
// automatic brake release are commented placeholders, so each control reads
// unapproved and its feature reports that it needs your number until you
// write one. [order_limits] carries values (owner decision 2026-10-05 19:56
// CEST): src, config.toml [trading] as written, else the compiled ones. It is
// written at the constitution schema that accepts every key it lists.
func constitutionPolicyTemplateFrom(release string, src config.Trading, srcRead bool) []byte {
	rb, pp, sp := risk.DefaultRulebookPolicy(), defaultProtectionPolicy(), risk.DefaultPolicy()
	var b strings.Builder
	policyTemplateHeader(&b, "Risk constitution: your capital numbers. Canary has no default for any of them.", release, false,
		"",
		"Every capital key below is a placeholder, not a value: until you write a",
		"number, the control that needs it reads \"unapproved\" and its feature says it",
		"needs your number. [order_limits] at the end carries values: the per-order",
		"gates every order preview is judged by. Raise policy_version with every edit.",
		"The 0 beside a placeholder marks the slot; it is not a recommendation. Most",
		"keys reject 0, so a key uncommented without your number fails the load and",
		"names itself; protected_floor and the two amount tolerances accept 0 as a",
		"real choice.")
	fmt.Fprintf(&b, "kind = %q\nschema_version = 2\npolicy_id = \"risk-constitution\"\npolicy_version = %d\n", risk.ConstitutionKind, constitutionTemplateVersion)
	b.WriteString(`
[capital]
# Must match the account's base currency; observations in any other currency
# are refused for capital math.
# base_currency = "EUR"
# Equity that is never risk capital (a policy boundary, not segregation).
# protected_floor = 0.0
# The money you deliberately authorize to be at risk. Effective risk capital =
# min(declared_risk_capital, equity - protected_floor). Deposits and profits
# never raise it; only a policy revision does.
# declared_risk_capital = 0.0
# Staleness horizons: beyond these the capital state is stale and says so.
# max_equity_age_minutes = 0
# max_unreconciled_days = 0

[drawdown]
# Both tiers are % of declared_risk_capital consumed from the cash-flow-adjusted
# equity peak. warn is advisory and clears itself; block latches.
# warn_consumed_pct = 0.0
# block_consumed_pct = 0.0
# shadow (default): journal what would block, gate nothing. advisory: warn
# loudly on surfaces and previews, gate nothing. Both formats reject "hard".
# block_enforcement = "shadow"
# How a latched brake clears: manual (default) only by a journaled human reset
# (canary policy reset-drawdown); automatic also when fresh, verified drawdown
# falls below the block threshold, keeping the peak and loss history.
# release = "manual"

[override]
# Longest lifetime of a one-shot exception (canary policy override).
# max_duration_hours = 0

[recon]
# What counts as a reconciliation exception when broker statement flows are
# matched against declared capital events.
# amount_tolerance_pct = 0.0
# amount_tolerance_min = 0.0
# date_window_business_days = 0
# max_report_age_days = 0
# Largest same-day statement-versus-runtime equity divergence that still lets a
# clean report extend the reconcile clock automatically.
# max_equity_divergence_pct = 0.0

# Reminder timing ([cadence.nudges], [cadence.monthly]) defaults in code; see
# the configuration reference to change it.

[inventory]
# Sibling pins record the policy versions you approved this constitution
# against; a changed sibling is disclosed, and blocks nothing unless you set
# require_signoff = true.
# require_signoff = false
`)
	for _, pin := range []struct{ name, id, version string }{
		{"rulebook", rb.ID, strconv.Itoa(rb.Version)},
		{"protection", pp.PolicyID, strconv.Itoa(pp.PolicyVersion)},
		{"stress", sp.PolicyProfile(), sp.PolicyVersion()},
	} {
		fmt.Fprintf(&b, "# [inventory.%s]\n# id = %q\n# version = %q\n", pin.name, pin.id, pin.version)
	}
	b.WriteString(`
# Optional stock opening/addition policy. No values are assigned on install.
# Uncomment the table and choose BOTH percentages before using canary add.
# This opts stock increases into cash, allocation and portfolio-risk checks.
# [position_add]
# max_stock_pct_nlv = 0.0
# max_underlying_stock_pct_nlv = 0.0
`)
	b.WriteString(orderLimitsTemplateBlock(src, srcRead))
	b.WriteString(`
# Your device, for saves from Desk (owner decision 2026-10-07 08:33 CEST).
# Desk's Settings can change the three order-cap keys above only with your
# passkey or the paired companion, and Canary verifies that confirmation
# itself against the key pinned here. Paste the line Desk shows for your
# credential, raise policy_version, and Canary verifies from the next reread;
# until then a save from Desk cannot change an order cap. One line per
# credential class: "<credential id>:<base64url of the 65-byte P-256 point>".
# [desk_device]
# companion = ""
# passkey = ""
`)
	return []byte(b.String())
}

// constitutionTemplateVersion is the initial document revision; format 2 defines its semantics.
const constitutionTemplateVersion = 1

// parseConstitutionPolicy validates a constitution file the way the risk
// policy manager does.
func parseConstitutionPolicy(data []byte) error {
	var c risk.Constitution
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return fmt.Errorf("unknown risk policy key(s): %s", strings.Join(keys, ", "))
	}
	return c.Validate()
}

// ensurePolicyFilesOnStart runs the ensure step once per daemon start, logs
// every action, and makes the file managers reread what it wrote. It writes
// missing files and applies each existing file's migration after a backup, so
// an upgrade needs no plan (owner instruction 2026-10-06 06:33 CEST). Start
// calls it after the SQLite authority is bound, because a reread journals
// status transitions. A failure is logged and never stops the daemon: an
// absent or broken file never blocks exits, trims or reads.
func (s *Server) ensurePolicyFilesOnStart() {
	if s == nil || !s.ensurePolicyFiles {
		return
	}
	for _, a := range EnsurePolicyFiles(PolicyFileSetFor(s.cfg), EnsureOptions{Release: s.version, ApplyMigrations: true}) {
		line := fmt.Sprintf("policy file %s: %s %s", a.Policy, a.Action, a.Path)
		if a.Backup != "" {
			line += " (backup " + a.Backup + ")"
		}
		if len(a.Changes) > 0 {
			line += "; " + strings.Join(a.Changes, "; ")
		}
		if a.Error != "" {
			line += "; error: " + a.Error
		}
		switch a.Action {
		case PolicyFileFailed, PolicyFileUnreadable:
			s.warnf("%s", line)
		case PolicyFileUnchanged:
			s.logger.Debugf("%s", line)
		default:
			s.logger.Infof("%s", line)
		}
		for _, n := range a.Notes {
			s.logger.Infof("policy file %s: %s", a.Policy, n)
		}
	}
	if s.protectionPolicies != nil {
		s.protectionPolicies.reload()
	}
	if s.rulebookPolicies != nil {
		s.rulebookPolicies.reload()
	}
	if s.opportunityPolicies != nil {
		s.opportunityPolicies.reload()
	}
	if s.riskPolicies != nil {
		s.riskPolicies.reload()
	}
}
