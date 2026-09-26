package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Owner decision 2026-09-26: every template Canary writes is its own code
// defaults, byte for byte in effect, and opens with the review marker.
func TestPolicyTemplatesAreCanaryDefaults(t *testing.T) {
	rb := RulebookPolicyTemplate("v9.9.9")
	read, err := parseRulebookPolicy(rb)
	if err != nil || read.policy.FingerprintKey() != risk.DefaultRulebookPolicy().FingerprintKey() || len(read.missing) != 0 || len(read.retired) != 0 {
		t.Fatalf("rulebook template: err %v missing %v retired %v", err, read.missing, read.retired)
	}
	pp := ProtectionPolicyTemplate("v9.9.9")
	prot, _, err := parseProtectionPolicy(pp)
	if err != nil || fingerprintProtectionPolicy(prot).Key != fingerprintProtectionPolicy(defaultProtectionPolicy()).Key {
		t.Fatalf("protection template parses to a different policy: %v\n%s", err, pp)
	}
	if len(prot.Authority.PreAuthorised) != 0 || prot.Buckets.BudgetReduction != nil || prot.Buckets.TrailingStop.Options.limitOffsetExplicit {
		t.Fatalf("protection template switches on an owner decision: %+v", prot.Authority)
	}
	op := OpportunityPolicyTemplate("v9.9.9")
	opp, err := parseOpportunityPolicy(op)
	if err != nil || !sameEffectivePolicy(effectiveOpportunityPolicy(opp), effectiveOpportunityPolicy(defaultOpportunityPolicy())) {
		t.Fatalf("opportunity template: %v\n%+v\n%+v", err, opp, defaultOpportunityPolicy())
	}
	cp := ConstitutionPolicyTemplate("v9.9.9")
	if err := parseConstitutionPolicy(cp); err != nil {
		t.Fatalf("constitution template does not load: %v", err)
	}
	var c risk.Constitution
	if _, err := toml.Decode(string(cp), &c); err != nil {
		t.Fatal(err)
	}
	empty := risk.Constitution{Kind: risk.ConstitutionKind, SchemaVersion: 2, PolicyVersion: constitutionTemplateVersion}
	if !slices.Equal(c.UnapprovedKeys(), empty.UnapprovedKeys()) || c.Drawdown.Release != "" || c.Inventory.RequireSignoff != nil {
		t.Fatalf("constitution template carries a number or decision: unapproved %v", c.UnapprovedKeys())
	}
	for name, data := range map[string][]byte{"rulebook": rb, "protection": pp, "opportunity": op, "constitution": cp} {
		if policyFileReview(data) != rpc.PolicyReviewUnreviewed || !strings.Contains(string(data), "Canary v9.9.9 wrote this") {
			t.Fatalf("%s template lacks the review header:\n%s", name, data)
		}
		printed, err := DefaultPolicyTOML(name)
		if err != nil || !bytes.Equal(printed, templateFor(name, rulebookEditRelease)) {
			t.Fatalf("canary policy default %s is not the template it writes: %v", name, err)
		}
	}
}

func templateFor(name, release string) []byte {
	switch name {
	case "rulebook":
		return RulebookPolicyTemplate(release)
	case "protection":
		return ProtectionPolicyTemplate(release)
	case "opportunity":
		return OpportunityPolicyTemplate(release)
	default:
		return ConstitutionPolicyTemplate(release)
	}
}

// Every Rulebook limit is written into the file, so no limit is left to a
// compiled value the owner cannot see; a new policy field without a template
// entry fails here.
func TestRulebookTemplateCoversEveryPolicyKey(t *testing.T) {
	listed := map[string]bool{}
	for _, k := range rulebookTemplateKeys {
		if listed[k.key] {
			t.Fatalf("%s is listed twice", k.key)
		}
		listed[k.key] = true
	}
	tables := map[string]bool{"modes": true, "regime_calm": true, "regime_early_warning": true, "regime_confirmed": true, "issuer_groups": true, "clusters": true}
	typ := reflect.TypeFor[risk.RulebookPolicy]()
	for field := range typ.Fields() {
		tag, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
		switch {
		case tag == "" || tag == "-" || tag == "kind" || tag == "schema_version" || tag == "policy_id" || tag == "policy_version":
		case retiredRulebookKey(tag):
		case tables[tag]:
			delete(tables, tag)
		case !listed[tag]:
			t.Fatalf("policy key %s is not in the template", tag)
		default:
			delete(listed, tag)
		}
	}
	if len(listed) != 0 || len(tables) != 0 {
		t.Fatalf("template lists keys the policy does not have: %v %v", listed, tables)
	}
	var raw map[string]any
	md, err := toml.Decode(string(RulebookPolicyTemplate("v9.9.9")), &raw)
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, k := range md.Keys() {
		defined[k.String()] = true
	}
	for _, key := range rulebookTemplateKeySet() {
		if !defined[key] {
			t.Fatalf("template does not write %s", key)
		}
	}
}

// Owner decision 2026-09-26: rule 18 (loss budget) alerts by default and
// rules 16 and 17 track; all three stay watch-only.
func TestRulebookTemplateModesFollowTheOwnersDecision(t *testing.T) {
	read, err := parseRulebookPolicy(RulebookPolicyTemplate("v9.9.9"))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{risk.RuleLossBudget: risk.RuleModeAlert, risk.RuleDeltaSwing: risk.RuleModeTrack, risk.RuleClusterStress: risk.RuleModeTrack} {
		if got := read.policy.ModeFor(id); got != want {
			t.Fatalf("%s mode in the template = %s, want %s", id, got, want)
		}
		if !risk.WatchOnlyRule(id) {
			t.Fatalf("%s is not watch-only", id)
		}
	}
	if !strings.Contains(string(RulebookPolicyTemplate("v9.9.9")), `loss_budget = "alert"`) {
		t.Fatal("the template does not write rule 18's mode")
	}
}

// The protection and constitution templates show every key the owner can
// set, live or as a commented placeholder, so nothing is discoverable only in
// code.
func TestProtectionAndConstitutionTemplatesShowEveryKey(t *testing.T) {
	for name, tc := range map[string]struct {
		typ  reflect.Type
		data []byte
		skip map[string]bool
	}{
		"protection":   {reflect.TypeFor[protectionPolicy](), ProtectionPolicyTemplate("v9.9.9"), nil},
		"constitution": {reflect.TypeFor[risk.Constitution](), ConstitutionPolicyTemplate("v9.9.9"), map[string]bool{"cadence": true}},
	} {
		text := string(tc.data)
		for _, leaf := range tomlLeafKeys(tc.typ, tc.skip) {
			if !strings.Contains(text, leaf+" = ") {
				t.Fatalf("%s template never shows %s", name, leaf)
			}
		}
	}
}

// tomlLeafKeys lists the leaf toml keys of a struct type, descending into
// nested structs, pointers and slices of structs.
func tomlLeafKeys(typ reflect.Type, skip map[string]bool) []string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	var out []string
	for f := range typ.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if tag == "" || tag == "-" || skip[tag] {
			continue
		}
		inner := f.Type
		for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Slice {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct && inner != reflect.TypeFor[time.Time]() {
			if f.Type.Kind() == reflect.Slice {
				out = append(out, tag)
			}
			out = append(out, tomlLeafKeys(inner, nil)...)
			continue
		}
		out = append(out, tag)
	}
	return out
}

func TestPolicyFileReviewMarkerOnlyCountsInTheHeader(t *testing.T) {
	for _, tc := range []struct {
		data string
		want string
	}{
		{PolicyUnreviewedMarker + "\nkind = \"x\"\n", rpc.PolicyReviewUnreviewed},
		{"\n# My notes\n" + PolicyUnreviewedMarker + "\nkind = \"x\"\n", rpc.PolicyReviewUnreviewed},
		{"# My notes\nkind = \"x\"\n" + PolicyUnreviewedMarker + "\n", ""},
		{"# Canary defaults, reviewed on a Sunday.\nkind = \"x\"\n", ""},
		{"kind = \"x\"\n", ""},
	} {
		if got := policyFileReview([]byte(tc.data)); got != tc.want {
			t.Fatalf("review(%q) = %q, want %q", tc.data, got, tc.want)
		}
	}
}

// policyTestSet places every policy file under one temporary directory.
func policyTestSet(t *testing.T) PolicyFileSet {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "policies")
	return PolicyFileSet{
		Rulebook:     filepath.Join(dir, "rulebook-policy.toml"),
		Protection:   filepath.Join(dir, "protection-policy.toml"),
		Opportunity:  filepath.Join(dir, "opportunity-policy.toml"),
		Constitution: filepath.Join(dir, "risk-policy.toml"),
	}
}

func writePolicyTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRulebookTestFile(t, path, body)
}

func ensureActions(actions []PolicyFileAction) map[string]PolicyFileAction {
	out := map[string]PolicyFileAction{}
	for _, a := range actions {
		out[a.Policy] = a
	}
	return out
}

func readPolicyTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A fresh install under a new HOME writes all four files at the documented
// paths, owner-only, each with the review header; a second start changes
// nothing.
func TestEnsurePolicyFilesFreshInstallUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	set := PolicyFileSetFor(nil)
	policies := filepath.Join(home, ".config", "ibkr", "policies")
	want := PolicyFileSet{
		Rulebook:     filepath.Join(policies, "rulebook-policy.toml"),
		Protection:   filepath.Join(policies, "protection-policy.toml"),
		Opportunity:  filepath.Join(policies, "opportunity-policy.toml"),
		Constitution: filepath.Join(policies, "risk-policy.toml"),
	}
	if set != want {
		t.Fatalf("default paths = %+v, want %+v", set, want)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	first := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9", Now: now}))
	for _, name := range []string{PolicyFileRulebook, PolicyFileProtection, PolicyFileOpportunity, PolicyFileConstitution} {
		a := first[name]
		if a.Action != PolicyFileCreated || a.Error != "" {
			t.Fatalf("%s: %+v", name, a)
		}
		info, err := os.Stat(a.Path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v err %v", name, info.Mode().Perm(), err)
		}
		if data := readPolicyTestFile(t, a.Path); policyFileReview(data) != rpc.PolicyReviewUnreviewed || !bytes.Equal(data, templateFor(name, "v9.9.9")) {
			t.Fatalf("%s is not the template:\n%s", name, data)
		}
	}
	if info, err := os.Stat(policies); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("policy directory mode %v err %v", info.Mode().Perm(), err)
	}
	before := map[string][]byte{}
	for name, a := range first {
		before[name] = readPolicyTestFile(t, a.Path)
	}
	for name, a := range ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.10", Now: now.Add(time.Hour)})) {
		if a.Action != PolicyFileUnchanged || len(a.Notes) != 0 || !bytes.Equal(readPolicyTestFile(t, a.Path), before[name]) {
			t.Fatalf("second start touched %s: %+v", name, a)
		}
	}
	if backups, _ := filepath.Glob(filepath.Join(policies, "*.bak-*")); len(backups) != 0 {
		t.Fatalf("an idempotent start left backups: %v", backups)
	}
	// The managers read what was written as the owner's file, unreviewed.
	m := newRulebookPolicyManager(set.Rulebook, time.Minute, time.Now)
	m.reload()
	if _, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || st.Source != rulebookPolicySourceFile || st.Review != rpc.PolicyReviewUnreviewed || len(st.Missing) != 0 {
		t.Fatalf("rulebook manager reads %+v", st)
	}
	pm := newProtectionPolicyManager(set.Protection, false, time.Minute, time.Now)
	pm.reload()
	if _, st := pm.Active(); st.Status != rpc.ProtectionPolicyStatusActive || st.Review != rpc.PolicyReviewUnreviewed || st.AutomationPaused {
		t.Fatalf("protection manager reads %+v", st)
	}
	rm := newRiskPolicyManager(set.Constitution, time.Minute, time.Now)
	rm.reload()
	if snap := rm.snapshot(); snap.status != rpc.RiskPolicyStatusActive || snap.review != rpc.PolicyReviewUnreviewed || snap.policy == nil || len(snap.policy.UnapprovedKeys()) == 0 {
		t.Fatalf("constitution manager reads %+v", snap)
	}
}

// ownerLikeProtection is a synthetic file shaped like a long-lived owner
// file: annotated, versioned past the default, GTC stops, a multi-line
// directional_intents array, and the key this release retires.
const ownerLikeProtection = `# Protection policy: my changes from the default.
#  - trailing stops are GTC.
kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = 6
profile = "theta-priority-mvp"

[authority]
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 21
# Dust floor only.
min_abs_theta_per_day = 3.0
min_extrinsic_pct_of_mark = 40.0
max_spread_pct_of_mid = 12.0

[buckets.risk_reduction]
enabled = true
single_name_target_pct_nlv = 22.0
max_order_notional = 12000.0

[buckets.trailing_stop]
enabled = true
tif = "GTC"

[buckets.trailing_stop.stock_etf]
enabled = true
order_type = "TRAIL"
default_pct = 9.0
min_pct = 4.0
max_pct = 16.0
max_spread_pct_of_mid = 3.0

[buckets.trailing_stop.options]
default_long_calls_directional = true
default_index_puts_protection = true
enabled = true
directional_intents = [
  { independent_exit = true, con_id = 101, reason = "Synthetic approval one", approved_at = 2026-09-01T10:00:00Z, expires_at = 2026-10-16T23:59:00Z },
  { independent_exit = true, con_id = 202, reason = "Synthetic approval two", approved_at = 2026-09-01T10:00:00Z, expires_at = 2026-11-20T23:59:00Z }
]
min_dte = 10
profit_arm_gain_pct = 50.0
locked_gain_pct = 25.0
tif = "DAY"
order_type = "TRAIL LIMIT"
default_pct = 12.0
min_pct = 8.0
max_pct = 30.0
max_spread_pct_of_mid = 15.0
min_trail_abs = 0.1
spread_multiple = 3.0
limit_offset_abs = 0.05
allow_short_profit_trail = false
`

// ownerLikeConstitution is a synthetic constitution with every capital number
// written and sibling pins that no longer match.
const ownerLikeConstitution = `kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 4

[capital]
base_currency = "EUR"
protected_floor = 10000.0
declared_risk_capital = 50000.0
max_equity_age_minutes = 30
max_unreconciled_days = 5

[drawdown]
warn_consumed_pct = 40.0
block_consumed_pct = 80.0
block_enforcement = "shadow"

[override]
max_duration_hours = 24

[recon]
amount_tolerance_pct = 0.5
amount_tolerance_min = 5.0
date_window_business_days = 3
max_report_age_days = 7
max_equity_divergence_pct = 2.0

[inventory.rulebook]
id = "rulebook-v2"
version = "2"
[inventory.protection]
id = "protection-mvp"
version = "3"
`

// An upgrade over an owner-like set: the retired protection key is commented
// out in place with a backup and a recommendation, every owner value and
// comment survives, the policy in force is unchanged (no policy_version bump,
// no drift), the missing Rulebook and opportunity files are written, and the
// constitution is never edited.
func TestEnsurePolicyFilesUpgradesAnOwnerLikeSet(t *testing.T) {
	set := policyTestSet(t)
	writePolicyTestFile(t, set.Protection, ownerLikeProtection)
	writePolicyTestFile(t, set.Constitution, ownerLikeConstitution)
	before, _, err := parseProtectionPolicy([]byte(ownerLikeProtection))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	startup := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9", Now: now}))
	if startup[PolicyFileProtection].Action != PolicyFileWouldMigrate || string(readPolicyTestFile(t, set.Protection)) != ownerLikeProtection || string(readPolicyTestFile(t, set.Constitution)) != ownerLikeConstitution {
		t.Fatal("startup changed an existing owner file")
	}
	if startup[PolicyFileRulebook].Action != PolicyFileCreated || startup[PolicyFileOpportunity].Action != PolicyFileCreated {
		t.Fatal("missing templates were not created")
	}
	got := ensureActions(applyReviewedTestConversions(t, set, EnsureOptions{Release: "v9.9.9", Now: now}))
	prot := got[PolicyFileProtection]
	if prot.Action != PolicyFileMigrated || prot.Backup == "" {
		t.Fatalf("protection: %+v", prot)
	}
	if string(readPolicyTestFile(t, prot.Backup)) != ownerLikeProtection {
		t.Fatal("backup did not preserve original")
	}
	migrated := readPolicyTestFile(t, set.Protection)
	after, _, err := parseProtectionPolicy(migrated)
	if err != nil || !sameEffectivePolicy(effectiveProtectionPolicy(before), effectiveProtectionPolicy(after)) || after.PolicyVersion != before.PolicyVersion {
		t.Fatal("conversion changed settings")
	}
	if !strings.Contains(string(migrated), "Format/comment migration by Canary v9.9.9") {
		t.Fatal("missing provenance")
	}
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9", Now: now.Add(time.Minute)}) {
		if a.Action != PolicyFileUnchanged {
			t.Fatalf("conversion not idempotent: %+v", a)
		}
	}
	if backups, _ := filepath.Glob(set.Protection + ".bak-*"); len(backups) != 1 {
		t.Fatalf("backups: %v", backups)
	}
}

// An older owner Rulebook file (written by `canary rules policy set` before
// files were complete) gains every missing key at Canary's default, keeps
// every owner value and comment, loses the retired key to a comment, and
// reports the recommendation for keys whose meaning changed, once.
func TestEnsurePolicyFilesMigratesAnOlderRulebookFileInPlace(t *testing.T) {
	set := policyTestSet(t)
	old := `# My Rulebook overrides.
kind = "ibkr.rulebook_policy"
schema_version = 1
policy_id = "rulebook-owner"
policy_version = 5
single_name_watch_pct = 25.0  # set after the spring drawdown
single_name_act_pct = 35.0
cash_reserve_min_pct = 60.0

[modes]
winner_trim = "track"

[regime_confirmed]
cash_sell_only_pct = 10.0
`
	writePolicyTestFile(t, set.Rulebook, old)
	before, err := parseRulebookPolicy([]byte(old))
	if err != nil || len(before.retired) != 1 {
		t.Fatalf("old file: %v retired %v", err, before.retired)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	a := ensureActions(applyReviewedTestConversions(t, set, EnsureOptions{Release: "v9.9.9", Now: now}))[PolicyFileRulebook]
	if a.Action != PolicyFileMigrated || a.Backup == "" || a.Error != "" {
		t.Fatalf("rulebook: %+v", a)
	}
	for _, want := range []string{
		"single_name_act_pct now caps the worst-case loss on one issuer with every leg netted (before amendment 15 it capped stock-equivalent delta): Canary now recommends 40.0; yours is 35.0",
		"single_name_watch_pct now bounds the worst-case loss on one issuer with every leg netted (before amendment 15 it bounded stock-equivalent delta): Canary now recommends 30.0; yours is 25.0",
	} {
		if !slices.Contains(a.Notes, want) {
			t.Fatalf("notes %q lack %q", a.Notes, want)
		}
	}
	if !slices.Contains(a.Changes, "added takeover_gap_pct at Canary's default") || !slices.Contains(a.Changes, "added modes.loss_budget at Canary's default") ||
		!slices.Contains(a.Changes, "commented out retired regime_confirmed.cash_sell_only_pct") || !slices.Contains(a.Changes, "added issuer_groups at Canary's default") {
		t.Fatalf("changes = %v", a.Changes)
	}
	data := readPolicyTestFile(t, set.Rulebook)
	after, err := parseRulebookPolicy(data)
	if err != nil || after.policy.EffectiveFingerprintKey() != before.policy.EffectiveFingerprintKey() || after.policy.Version != 5 || len(after.missing) != 0 || len(after.retired) != 0 {
		t.Fatalf("migrated file: err %v version %d missing %v retired %v", err, after.policy.Version, after.missing, after.retired)
	}
	if after.policy.SingleNameWatchPct != 25 || after.policy.SingleNameActPct != 35 || after.policy.ModeFor(risk.RuleLossBudget) != risk.RuleModeAlert {
		t.Fatalf("owner values moved: %+v", after.policy)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(old, "\n"), "\n") {
		if line == "cash_sell_only_pct = 10.0" || strings.HasPrefix(line, "kind =") {
			continue
		}
		if !strings.Contains(string(data), line) {
			t.Fatalf("migration lost the owner's line %q:\n%s", line, data)
		}
	}
	if policyFileReview(data) != "" {
		t.Fatal("migration marked an owner file unreviewed")
	}
	second := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9", Now: now.Add(time.Minute)}))[PolicyFileRulebook]
	if second.Action != PolicyFileUnchanged || len(second.Notes) != 0 {
		t.Fatalf("second start: %+v", second)
	}
}

// A file that does not parse is never replaced, migrated or backed up: the
// policy in force stays and the step says so. Other files are still written.
func TestEnsurePolicyFilesLeavesABrokenFileAlone(t *testing.T) {
	set := policyTestSet(t)
	broken := "kind = \"ibkr.protection_policy\"\npolicy_version = [oops\n"
	writePolicyTestFile(t, set.Protection, broken)
	writePolicyTestFile(t, set.Rulebook, "single_name_watch_pct = 55.0\nsingle_name_act_pct = 50.0\n")
	got := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9"}))
	for _, name := range []string{PolicyFileProtection, PolicyFileRulebook} {
		a := got[name]
		if a.Action != PolicyFileUnreadable || a.Error == "" || !slices.ContainsFunc(a.Notes, func(n string) bool { return strings.Contains(n, "left untouched") }) {
			t.Fatalf("%s: %+v", name, a)
		}
	}
	if string(readPolicyTestFile(t, set.Protection)) != broken {
		t.Fatal("a broken protection file was rewritten")
	}
	if backups, _ := filepath.Glob(filepath.Join(filepath.Dir(set.Protection), "*.bak-*")); len(backups) != 0 {
		t.Fatalf("a broken file was backed up as if migrated: %v", backups)
	}
	if got[PolicyFileOpportunity].Action != PolicyFileCreated || got[PolicyFileConstitution].Action != PolicyFileCreated {
		t.Fatalf("a broken sibling stopped the others: %+v", got)
	}
	// The protection manager keeps proposing on the embedded default and
	// pauses only automatic submission.
	pm := newProtectionPolicyManager(set.Protection, false, time.Minute, time.Now)
	pm.reload()
	if _, st := pm.Active(); st.Status != rpc.ProtectionPolicyStatusError || !st.AutomationPaused {
		t.Fatalf("broken file status %+v", st)
	}
}

// A file the owner deletes between restarts comes back as Canary's template
// at the next start; the others are untouched.
func TestEnsurePolicyFilesRecreatesAFileDeletedBetweenRestarts(t *testing.T) {
	set := policyTestSet(t)
	EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9"})
	if _, err := EditRulebookPolicy(set.Rulebook, []string{"cash_reserve_min_pct=70"}, nil, false); err != nil {
		t.Fatal(err)
	}
	protection := readPolicyTestFile(t, set.Protection)
	if err := os.Remove(set.Rulebook); err != nil {
		t.Fatal(err)
	}
	got := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.10"}))
	if got[PolicyFileRulebook].Action != PolicyFileCreated || !bytes.Equal(readPolicyTestFile(t, set.Rulebook), RulebookPolicyTemplate("v9.9.10")) {
		t.Fatalf("deleted rulebook: %+v", got[PolicyFileRulebook])
	}
	if got[PolicyFileProtection].Action != PolicyFileUnchanged || !bytes.Equal(readPolicyTestFile(t, set.Protection), protection) {
		t.Fatalf("protection touched: %+v", got[PolicyFileProtection])
	}
}

// The dry run behind `canary policy ensure --dry-run` and the explain surface
// reports what a start would do and writes nothing.
func TestEnsurePolicyFilesDryRunWritesNothing(t *testing.T) {
	set := policyTestSet(t)
	writePolicyTestFile(t, set.Protection, ownerLikeProtection)
	got := ensureActions(EnsurePolicyFiles(set, EnsureOptions{Release: "v9.9.9", DryRun: true}))
	if got[PolicyFileRulebook].Action != PolicyFileWouldCreate || got[PolicyFileProtection].Action != PolicyFileWouldMigrate || len(got[PolicyFileProtection].Changes) < 1 {
		t.Fatalf("dry run = %+v", got)
	}
	if _, err := os.Stat(set.Rulebook); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the rulebook")
	}
	if string(readPolicyTestFile(t, set.Protection)) != ownerLikeProtection {
		t.Fatal("dry run migrated the protection file")
	}
	if backups, _ := filepath.Glob(set.Protection + ".bak-*"); len(backups) != 0 {
		t.Fatal("dry run took a backup")
	}
}

// A retired key the line editor cannot reach (inside an inline table) is
// left in place, still ignored on load, and the step says so instead of
// rewriting the table.
func TestProtectionMigrationLeavesARetiredKeyItCannotEditAlone(t *testing.T) {
	block := "[buckets.risk_reduction]\nenabled = true\nsingle_name_target_pct_nlv = 22.0\nmax_order_notional = 12000.0\n\n"
	if !strings.Contains(ownerLikeProtection, block) {
		t.Fatal("fixture drifted")
	}
	inline := strings.Replace(strings.Replace(ownerLikeProtection, block, "", 1), "[buckets.theta_hygiene]",
		"[buckets]\nrisk_reduction = { enabled = true, single_name_target_pct_nlv = 22.0, max_order_notional = 12000.0 }\n\n[buckets.theta_hygiene]", 1)
	if _, _, err := parseProtectionPolicy([]byte(inline)); err != nil {
		t.Fatalf("inline retired key must load: %v", err)
	}
	out, changes, notes, err := migrateProtectionPolicyFile([]byte(inline), "v9.9.9")
	if err != nil || len(changes) != 0 || string(out) != inline || !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "not a plain line") }) {
		t.Fatalf("inline retired key: changes %v notes %v err %v", changes, notes, err)
	}
}

// The daemon runs the ensure step at start and its managers reread what it
// wrote: an absent file that ran on defaults becomes the owner's file.
func TestEnsurePolicyFilesOnStartReloadsTheManagers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	set := policyTestSet(t)
	cfg := &config.Resolved{
		Rulebook:      config.Rulebook{PolicyFile: set.Rulebook},
		AutoTrade:     config.AutoTrade{PolicyFile: set.Protection},
		Opportunities: config.Opportunities{PolicyFile: set.Opportunity},
	}
	var logs bytes.Buffer
	s := &Server{cfg: cfg, ensurePolicyFiles: true, version: "v9.9.9", logger: NewLogger(&logs, "debug"),
		protectionPolicies:  newProtectionPolicyManager(set.Protection, false, time.Minute, time.Now),
		rulebookPolicies:    newRulebookPolicyManager(set.Rulebook, time.Minute, time.Now),
		opportunityPolicies: newOpportunityPolicyManager(set.Opportunity, false, time.Minute, time.Now),
	}
	s.protectionPolicies.reload()
	s.rulebookPolicies.reload()
	if _, st := s.rulebookPolicies.Active(); st.Status != rpc.RulebookPolicyStatusDefault {
		t.Fatalf("before start: %+v", st)
	}
	s.ensurePolicyFilesOnStart()
	if _, st := s.rulebookPolicies.Active(); st.Status != rpc.RulebookPolicyStatusActive || st.Review != rpc.PolicyReviewUnreviewed {
		t.Fatalf("rulebook after start: %+v", st)
	}
	if _, st := s.protectionPolicies.Active(); st.Status != rpc.ProtectionPolicyStatusActive || st.Review != rpc.PolicyReviewUnreviewed {
		t.Fatalf("protection after start: %+v", st)
	}
	if _, st := s.opportunityPolicies.Active(); st.Review != rpc.PolicyReviewUnreviewed {
		t.Fatalf("opportunity after start: %+v", st)
	}
	constitution := filepath.Join(home, ".config", "ibkr", "policies", "risk-policy.toml")
	if data := readPolicyTestFile(t, constitution); policyFileReview(data) != rpc.PolicyReviewUnreviewed {
		t.Fatal("constitution template not written under HOME")
	}
	if !strings.Contains(logs.String(), "policy file rulebook: created "+set.Rulebook) {
		t.Fatalf("start did not log what it wrote:\n%s", logs.String())
	}
	// A server built without the option (every test server) never writes.
	quiet := t.TempDir()
	t.Setenv("HOME", quiet)
	(&Server{cfg: &config.Resolved{Rulebook: config.Rulebook{PolicyFile: filepath.Join(quiet, "rb.toml")}}, logger: NewLogger(&logs, "debug")}).ensurePolicyFilesOnStart()
	if entries, _ := os.ReadDir(quiet); len(entries) != 0 {
		t.Fatalf("a server without EnsurePolicyFiles wrote %v", entries)
	}
}

// `canary policy show --explain` lists every file with its review state and
// the features waiting for the owner's number.
func TestPolicyFileStatusesNameReviewAndMissingNumbers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	set := policyTestSet(t)
	cfg := &config.Resolved{
		Rulebook:      config.Rulebook{PolicyFile: set.Rulebook},
		AutoTrade:     config.AutoTrade{PolicyFile: set.Protection},
		Opportunities: config.Opportunities{PolicyFile: set.Opportunity},
	}
	s := &Server{cfg: cfg, version: "v9.9.9", logger: NewLogger(&bytes.Buffer{}, "error"),
		protectionPolicies:  newProtectionPolicyManager(set.Protection, false, time.Minute, time.Now),
		rulebookPolicies:    newRulebookPolicyManager(set.Rulebook, time.Minute, time.Now),
		opportunityPolicies: newOpportunityPolicyManager(set.Opportunity, false, time.Minute, time.Now),
	}
	EnsurePolicyFiles(PolicyFileSet{Rulebook: set.Rulebook, Protection: set.Protection, Opportunity: set.Opportunity, Constitution: filepath.Join(home, ".config", "ibkr", "policies", "risk-policy.toml")}, EnsureOptions{Release: "v9.9.9"})
	s.protectionPolicies.reload()
	s.rulebookPolicies.reload()
	s.opportunityPolicies.reload()
	rm := newRiskPolicyManager(riskPolicyDefaultPath, time.Minute, time.Now)
	rm.reload()
	rows := map[string]rpc.PolicyFileStatus{}
	for _, row := range s.policyFileStatuses(rm.snapshot()) {
		rows[row.Policy] = row
	}
	for _, name := range []string{PolicyFileRulebook, PolicyFileProtection, PolicyFileOpportunity, PolicyFileConstitution} {
		if rows[name].Review != rpc.PolicyReviewUnreviewed {
			t.Fatalf("%s row = %+v", name, rows[name])
		}
	}
	joined := strings.Join(rows[PolicyFileProtection].NeedsYourNumber, " | ")
	if !strings.Contains(joined, "automatic submission") || !strings.Contains(joined, "premium budget governor") {
		t.Fatalf("protection needs = %s", joined)
	}
	joined = strings.Join(rows[PolicyFileConstitution].NeedsYourNumber, " | ")
	if !strings.Contains(joined, "capital.declared_risk_capital") || !strings.Contains(joined, "automatic brake release") {
		t.Fatalf("constitution needs = %s", joined)
	}
	if !slices.Contains(rows[PolicyFileRulebook].Notes, "no issuer groups declared: every symbol is its own issuer") {
		t.Fatalf("rulebook notes = %v", rows[PolicyFileRulebook].Notes)
	}
}
