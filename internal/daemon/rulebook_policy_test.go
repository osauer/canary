package daemon

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func rulebookTestManager(t *testing.T) (*rulebookPolicyManager, string, *[]rpc.RulebookPolicyStatus) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rulebook-policy.toml")
	m := newRulebookPolicyManager(path, time.Minute, func() time.Time { return time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC) })
	var journal []rpc.RulebookPolicyStatus
	m.onTransition = func(_, next rpc.RulebookPolicyStatus) { journal = append(journal, next) }
	return m, path, &journal
}

func writeRulebookTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A first install has no file: the compiled baseline runs and says so, and
// nothing a user relied on changes.
func TestRulebookPolicyAbsentFileRunsTheBaseline(t *testing.T) {
	m, _, journal := rulebookTestManager(t)
	m.reload()
	p, st := m.Active()
	if st.Status != rpc.RulebookPolicyStatusDefault || st.Source != rulebookPolicySourceDefault || len(st.Overrides) != 0 ||
		p.FingerprintKey() != risk.DefaultRulebookPolicy().FingerprintKey() || len(*journal) != 1 {
		t.Fatalf("absent file: %+v", st)
	}
}

// A partial file overrides only its keys; the first file is adopted whatever
// its version, and every later edit needs a higher version.
func TestRulebookPolicyFileOverridesOnlyItsKeysAndVersionGatesEdits(t *testing.T) {
	m, path, journal := rulebookTestManager(t)
	m.reload()
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 1\nfx_exposure_watch_pct = 70\n[modes]\nnet_exposure = \"alert\"\n")
	m.reload()
	p, st := m.Active()
	if st.Status != rpc.RulebookPolicyStatusActive || p.FXExposureWatchPct != 70 || p.ModeFor(risk.RuleNetExposure) != risk.RuleModeAlert ||
		p.SingleNameActPct != risk.DefaultRulebookPolicy().SingleNameActPct || !slices.Equal(st.Overrides, []string{"fx_exposure_watch_pct", "modes.net_exposure"}) {
		t.Fatalf("partial file: %+v %+v", st, p)
	}
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 1\nfx_exposure_watch_pct = 50\n")
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusDrift || p.FXExposureWatchPct != 70 || !strings.Contains(st.Message, "higher policy_version") {
		t.Fatalf("an edit without a version bump took effect: %+v", st)
	}
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 2\nfx_exposure_watch_pct = 50\n")
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.FXExposureWatchPct != 50 || p.ModeFor(risk.RuleNetExposure) != risk.RuleModeTrack {
		t.Fatalf("a bumped edit was not adopted: %+v", st)
	}
	if len(*journal) < 4 {
		t.Fatalf("transitions were not journaled: %d", len(*journal))
	}
}

// An unreadable or invalid file never replaces the limits in force, and a
// removed file does not silently return to the baseline.
func TestRulebookPolicyBadOrRemovedFileKeepsThePolicyInForce(t *testing.T) {
	m, path, _ := rulebookTestManager(t)
	writeRulebookTestFile(t, path, "policy_version = 5\nfx_exposure_watch_pct = 65\n")
	m.reload()
	for name, body := range map[string]string{
		"unknown key": "policy_version = 6\nfx_exposure_watch = 10\n",
		"invalid":     "policy_version = 6\noption_line_watch_pct = 20\noption_line_act_pct = 10\n",
		"syntax":      "policy_version = \n",
	} {
		writeRulebookTestFile(t, path, body)
		m.reload()
		if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusError || p.FXExposureWatchPct != 65 || st.Message == "" {
			t.Fatalf("%s: %+v", name, st)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusDrift || p.FXExposureWatchPct != 65 || !strings.Contains(st.Message, "restart") {
		t.Fatalf("a removed file changed the limits in force: %+v", st)
	}
}

// The CLI edit writes only the keys that differ from the baseline, raises the
// version, keeps the file private and writes nothing when a value is invalid.
// A first edit starts from Canary's complete template (owner decision
// 2026-09-26: the file, never a compiled baseline, is what runs), changes only
// the keys it names, and refuses anything the daemon would refuse.
func TestEditRulebookPolicyStartsFromTheTemplateAndRefusesInvalidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies", "rulebook-policy.toml")
	edit, err := EditRulebookPolicy(path, []string{"fx_exposure_watch_pct=70", "modes.winner_trim=track", "single_name_act_pct=40"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if edit.Version != risk.DefaultRulebookPolicy().Version+1 || !slices.Equal(edit.Differs, []string{"fx_exposure_watch_pct", "modes.winner_trim"}) || len(edit.Changes) != 2 {
		t.Fatalf("edit = %+v", edit)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v err %v", info.Mode().Perm(), err)
	}
	before, _ := os.ReadFile(path)
	read, err := parseRulebookPolicy(before)
	if err != nil || len(read.missing) != 0 || policyFileReview(before) != "unreviewed" ||
		!strings.Contains(string(before), "# Rule 1 — worst-case loss on one issuer") || !strings.Contains(string(before), "fx_exposure_watch_pct = 70") {
		t.Fatalf("first edit did not start from the template: missing %v err %v\n%s", read.missing, err, before)
	}
	for _, bad := range [][]string{{"fx_exposure_watch_pct=abc"}, {"no_such_key=1"}, {"option_line_act_pct=1"}, {"modes.fx_exposure=loud"}, {"runway_act_dte=2.5"}, {"regime_calm.premium_budget_watch_pct=40"}} {
		if _, err := EditRulebookPolicy(path, bad, nil, false); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused edit changed the file")
	}
	m := newRulebookPolicyManager(path, time.Minute, time.Now)
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.FXExposureWatchPct != 70 || p.ModeFor(risk.RuleWinnerTrim) != risk.RuleModeTrack {
		t.Fatalf("the daemon reads the edit differently: %+v", st)
	}
	edit, err = EditRulebookPolicy(path, nil, nil, true)
	if err != nil || len(edit.Differs) != 0 || edit.Version != risk.DefaultRulebookPolicy().Version+2 {
		t.Fatalf("reset --all = %+v %v", edit, err)
	}
	backups, _ := filepath.Glob(path + ".bak-reset-*")
	if len(backups) != 1 {
		t.Fatalf("reset --all kept no backup: %v", backups)
	}
	if saved, _ := os.ReadFile(backups[0]); string(saved) != string(before) {
		t.Fatal("the reset backup is not the file as it was")
	}
}

// An edit changes only the lines it names: every comment, every other value
// and a trailing note on the edited line stay byte for byte (owner decision
// 2026-09-26: Canary edits the file the owner reads and annotates in place).
func TestEditRulebookPolicyEditsInPlaceKeepingComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rulebook-policy.toml")
	original := `# My limits, reviewed with my adviser.
kind = "ibkr.rulebook_policy"
schema_version = 1
policy_id = "rulebook-owner"
policy_version = 7

# Concentration: I keep it tight.
single_name_watch_pct = 25.0  # tighter than Canary's
single_name_act_pct = 35.0
fx_exposure_watch_pct = 50.0

[modes]
winner_trim = "track"  # I like the reminder

[issuer_groups]
GroupA = ["AAA", "AAB"]
`
	writeRulebookTestFile(t, path, original)
	edit, err := EditRulebookPolicy(path, []string{"single_name_watch_pct=28", "issuer_groups.GroupB=BBB,BBC"}, []string{"fx_exposure_watch_pct"}, false)
	if err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"policy_version = 7", "policy_version = 8",
		"single_name_watch_pct = 25.0  # tighter than Canary's", "single_name_watch_pct = 28.0  # tighter than Canary's",
		"fx_exposure_watch_pct = 50.0", "fx_exposure_watch_pct = "+tomlFloat(risk.DefaultRulebookPolicy().FXExposureWatchPct),
		`GroupA = ["AAA", "AAB"]`, `GroupA = ["AAA", "AAB"]`+"\n"+`GroupB = ["BBB", "BBC"]`,
	).Replace(original)
	if string(written) != want {
		t.Fatalf("edit touched more than its lines:\n--- got\n%s\n--- want\n%s", written, want)
	}
	if edit.Version != 8 || len(edit.Changes) != 3 {
		t.Fatalf("edit = %+v", edit)
	}
	read, err := parseRulebookPolicy(written)
	if err != nil || read.policy.SingleNameWatchPct != 28 || read.policy.IssuerOf("BBC") != "GroupB" || read.policy.IssuerOf("AAB") != "GroupA" {
		t.Fatalf("the daemon reads the edit differently: %+v %v", read.policy, err)
	}
	if _, err := EditRulebookPolicy(path, nil, []string{"issuer_groups.GroupB"}, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); strings.Contains(string(again), "GroupB") || !strings.Contains(string(again), "# I like the reminder") {
		t.Fatalf("reset of an issuer group:\n%s", again)
	}
}

// With basis = rulebook the governor holds the book to the Rulebook's own
// limits as shares of NLV: the per-line act level, and rule 3's premium
// budget of the regime set in force, which triggers at its act level and cuts
// back to its watch level (amendment 17, rule 1's trim convention). It needs
// no constitution, waits for no brake, and never sells protection.
func TestBudgetRulebookBasisCutsThePremiumBudgetBackToWatch(t *testing.T) {
	policy := budgetTestPolicy(rpc.BudgetReductionModeShadow, 0, 0)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	if err := validateProtectionPolicy(policy); err != nil {
		t.Fatal(err)
	}
	pos := budgetTestBook() // AAA 6×2,000 (loss 1,000), BBB 4×2,500 (loss 4,000), CCC 2×3,000 (gain); 28,000 at risk
	input := budgetGovernorInput{Rulebook: risk.DefaultRulebookPolicy(), NLVBase: new(80000.0), AccountBaseCurrency: "EUR"}
	rows, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	// 28,000 is 35% of 80,000: exactly the calm act level, so the total pass
	// runs. Line limit 10% = 8,000: AAA sells 2, BBB 1 (6,500 at risk). Back
	// to the 25% budget is 8,000; BBB (largest loss) sells 1 more.
	if st == nil || st.Basis != rpc.BudgetBasisRulebook || st.State != rpc.BudgetStateOverBudget || st.PremiumExcessBase == nil ||
		math.Abs(*st.PremiumExcessBase-8000) > 1e-6 || st.PremiumPctOfNLV == nil || math.Abs(*st.PremiumPctOfNLV-35) > 1e-9 ||
		st.PremiumBudgetWatchPct != 25 || st.PremiumBudgetActPct != 35 || st.PremiumBudgetSet != "calm" ||
		st.ProtectionLegs != 2 || st.Rows != 2 || !st.Shadow || st.AvailableFundsBase != nil {
		t.Fatalf("status = %+v", st)
	}
	assertBudgetRowsReduceOnly(t, rows, pos)
	byID := budgetRowsByConID(rows)
	if aaa := byID[601]; aaa.Quantity != 2 || aaa.Budget == nil || aaa.Budget.Cap != "per_line" || aaa.Budget.Basis != rpc.BudgetBasisRulebook {
		t.Fatalf("AAA = %+v", aaa.Budget)
	}
	bbb := byID[602]
	if bbb.Quantity != 2 || bbb.Budget.Cap != "per_line+total" || bbb.Budget.ContractsTotal != 1 || bbb.Budget.PremiumBudgetActPct != 35 ||
		bbb.Budget.PremiumExcessBase == nil || !strings.Contains(bbb.Reason, "at or above the Rulebook's 35% premium budget act level (calm set)") ||
		!strings.Contains(bbb.Reason, "cut back to its 25% budget") || strings.Contains(bbb.Reason, "reserve") || strings.Contains(bbb.Reason, "available funds") {
		t.Fatalf("BBB = %+v %s", bbb.Budget, bbb.Reason)
	}
	if !slices.ContainsFunc(bbb.Details, func(d string) bool {
		return strings.HasPrefix(d, "premium budget (calm set): 28000 EUR at risk (35.0% of NLV)")
	}) {
		t.Fatalf("BBB details do not name the premium budget: %v", bbb.Details)
	}
	if _, ok := byID[603]; ok {
		t.Fatal("a gaining line was sold before the losers covered the excess")
	}

	// Above the budget but under its act level: the total pass waits, only
	// the line limit sells.
	input.NLVBase = new(80500.0)
	rows, st = (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	if st.State != rpc.BudgetStateOverBudget || st.PremiumExcessBase != nil {
		t.Fatalf("under the act level: %+v", st)
	}
	for _, row := range rows {
		if row.Budget.ContractsTotal != 0 {
			t.Fatalf("the total pass ran under the act level: %+v", row.Budget)
		}
	}

	// The regime set in force moves the levels: confirmed stress is 15/25.
	input.NLVBase, input.RegimeStage = new(100000.0), risk.RegimeBucketConfirmed
	rows, st = (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	byID = budgetRowsByConID(rows)
	if st.PremiumBudgetActPct != 25 || st.PremiumBudgetSet != "confirmed-stress" || st.PremiumExcessBase == nil || math.Abs(*st.PremiumExcessBase-13000) > 1e-6 ||
		byID[602].Quantity != 4 || byID[601].Quantity != 2 {
		t.Fatalf("confirmed set: %+v rows %+v", st, byID)
	}

	input.NLVBase = nil
	if _, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime()); st.State != rpc.BudgetStateAccountUnavailable {
		t.Fatalf("no NLV: %+v", st)
	}
}

// The total pass counts a losing line at its price paid, as rule 3 does: a
// contract sold removes its premium at risk, not today's lower value.
func TestBudgetRulebookBasisCountsALosingLineAtItsPricePaid(t *testing.T) {
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	leg := budgetOptionLeg("ZZZ", 701, "C", 10, 1000, -16000)
	leg.Currency, leg.AvgCost = "EUR", 2600
	pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"}, Options: []rpc.PositionView{leg}}
	rb := risk.DefaultRulebookPolicy()
	rb.OptionLineActPct = 50
	input := budgetGovernorInput{Rulebook: rb, NLVBase: new(100000.0), AccountBaseCurrency: "EUR", RegimeStage: risk.RegimeBucketConfirmed}
	rows, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	// 26,000 at risk (price paid, over a 10,000 value) is 26% ≥ 25: back to
	// 15% is 11,000, which 5 contracts at 2,600 cover; at value it would be
	// the whole line.
	if len(rows) != 1 || rows[0].Quantity != 5 || st.MeasuredPremiumBase == nil || *st.MeasuredPremiumBase != 26000 {
		t.Fatalf("rows %+v status %+v", rows, st)
	}
}

func TestBudgetBasisRulebookRejectsDeclaredCaps(t *testing.T) {
	policy := budgetTestPolicy(rpc.BudgetReductionModeShadow, 20, 15)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	if err := validateProtectionPolicy(policy); err == nil {
		t.Fatal("rulebook basis accepted declared-capital caps")
	}
	policy.Buckets.BudgetReduction.Basis = "equity"
	if err := validateProtectionPolicy(policy); err == nil {
		t.Fatal("unknown basis accepted")
	}
}

// A cached verdict computed under a superseded policy is not served: the
// owner's edit shows on the next read, not after the cache ages out.
func TestRulebookCacheRefusesAVerdictFromASupersededPolicy(t *testing.T) {
	m, path, _ := rulebookTestManager(t)
	m.reload()
	s := &Server{rulebookPolicies: m}
	scope := brokerStateScope{Account: "U_SYNTHETIC", Mode: rpc.AccountModeLive}
	now := time.Now().UTC()
	fp := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: risk.DefaultRulebookPolicy().FingerprintKey()}
	s.lastRules, s.lastRulesAt, s.lastRulesScope = &rpc.RulesResult{AsOf: now, PolicyFingerprint: &fp}, now, scope
	if _, ok := s.cachedRulebookResult(rulebookCacheBinding{scope: scope}, time.Minute, now); !ok {
		t.Fatal("a verdict under the policy in force was not served")
	}
	writeRulebookTestFile(t, path, "policy_version = 4\nfx_exposure_watch_pct = 70\n")
	m.reload()
	if _, ok := s.cachedRulebookResult(rulebookCacheBinding{scope: scope}, time.Minute, now); ok {
		t.Fatal("a verdict from a superseded policy was served")
	}
}

// Retired keys never void the limits the owner set beside them: until
// v3.11.1 canary policy default rulebook wrote cash_sell_only_pct, and since
// amendment 17 (2026-09-30) cash_reserve_min_pct and the top-level
// net_exposure_*_pct are retired too. The file loads, each key is ignored,
// and the status says which and why; the regime tables' net_exposure_*_pct
// are live limits.
func TestRulebookPolicyFileWithARetiredKeyKeepsTheOwnersLimits(t *testing.T) {
	m, path, _ := rulebookTestManager(t)
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\nfx_exposure_watch_pct = 70\ncash_reserve_min_pct = 70\nnet_exposure_watch_pct = 90\n\n[regime_calm]\ncash_sell_only_pct = -25\nnet_exposure_act_pct = 140\n")
	m.reload()
	p, st := m.Active()
	if st.Status != rpc.RulebookPolicyStatusActive || p.FXExposureWatchPct != 70 || p.RegimeCalm.NetExposureActPct != 140 || p.RegimeCalm.NetExposureWatchPct != 100 ||
		!slices.Equal(st.Overrides, []string{"fx_exposure_watch_pct", "regime_calm.net_exposure_act_pct"}) {
		t.Fatalf("retired keys: %+v (policy %+v)", st, p)
	}
	for _, key := range []string{"regime_calm.cash_sell_only_pct", "cash_reserve_min_pct", "net_exposure_watch_pct"} {
		if !strings.Contains(st.Message, key) {
			t.Fatalf("the status does not name retired %s: %q", key, st.Message)
		}
	}
	features := map[string]string{}
	for _, d := range st.Diagnostics {
		features[d.Key] = d.Feature
	}
	if features["cash_reserve_min_pct"] != "premium_budget" || features["net_exposure_watch_pct"] != "net_exposure" || !strings.Contains(st.Message, "premium budget") {
		t.Fatalf("diagnostics = %+v", st.Diagnostics)
	}
	m.reload()
	if _, st := m.Active(); !strings.Contains(st.Message, "cash_sell_only_pct") || !strings.Contains(st.Message, "cash_reserve_min_pct") {
		t.Fatalf("the note vanished on a steady reload: %+v", st)
	}
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\nfx_exposure_watch_pct = 70\n\n[regime_calm]\ncash_sell_only_pcx = -25\n")
	m.reload()
	if _, st := m.Active(); st.Status != rpc.RulebookPolicyStatusError || !strings.Contains(st.Message, "cash_sell_only_pcx") {
		t.Fatalf("a misspelt key was tolerated like a retired one: %+v", st)
	}
}

// set refuses a retired key, reset removes it, and any other edit drops it,
// so a key that changes nothing never reads as a limit in force.
func TestEditRulebookPolicyRefusesAndRemovesTheRetiredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rulebook-policy.toml")
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\nfx_exposure_watch_pct = 70\ncash_reserve_min_pct = 70\nnet_exposure_act_pct = 140\n\n[regime_calm]\ncash_sell_only_pct = -25\n\n[regime_confirmed]\ncash_sell_only_pct = 10\n")
	before, _ := os.ReadFile(path)
	for assignment, why := range map[string]string{
		"regime_confirmed.cash_sell_only_pct=5": "premium_budget_watch_pct",
		"cash_reserve_min_pct=70":               "rule 3 is the premium budget",
		"net_exposure_watch_pct=90":             "[regime_*] table",
	} {
		_, err := EditRulebookPolicy(path, []string{assignment}, nil, false)
		if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), why) {
			t.Fatalf("set %s: %v", assignment, err)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused set changed the file")
	}
	edit, err := EditRulebookPolicy(path, []string{"overhedge_multiple=1.5", "regime_confirmed.net_exposure_watch_pct=70"}, []string{"regime_calm.cash_sell_only_pct"}, false)
	if err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)
	if strings.Contains(string(written), "cash_sell_only_pct") || strings.Contains(string(written), "cash_reserve_min_pct") ||
		strings.Contains(string(written), "\nnet_exposure_act_pct") || !strings.Contains(string(written), "[regime_confirmed]\nnet_exposure_watch_pct = 70.0") ||
		!slices.Equal(edit.Differs, []string{"fx_exposure_watch_pct", "overhedge_multiple", "regime_confirmed.net_exposure_watch_pct"}) {
		t.Fatalf("edit kept a retired key or lost a limit: %+v\n%s", edit, written)
	}
	removed := 0
	for _, c := range edit.Changes {
		if retiredRulebookKey(c.Key) && strings.HasPrefix(c.To, "removed") {
			removed++
		}
	}
	if removed != 4 {
		t.Fatalf("changes do not report every retired key removed: %+v", edit.Changes)
	}
	if _, err := EditRulebookPolicy(path, []string{"overhedge_multiple=0.5"}, nil, false); err == nil {
		t.Fatal("an over-hedge multiple below 1 was accepted")
	}
	m := newRulebookPolicyManager(path, time.Minute, time.Now)
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.OverhedgeMultiple != 1.5 || p.RegimeConfirmed.NetExposureWatchPct != 70 || st.Message != "" {
		t.Fatalf("the daemon reads the cleaned file differently: %+v (multiple %v)", st, p.OverhedgeMultiple)
	}
}

// The complete file canary policy default rulebook prints no longer carries
// the retired key, and it loads as the baseline.
func TestDefaultRulebookPolicyTOMLCarriesNoRetiredKey(t *testing.T) {
	data, err := DefaultRulebookPolicyTOML()
	if err != nil {
		t.Fatal(err)
	}
	read, err := parseRulebookPolicy(data)
	if err != nil || strings.Contains(string(data), "cash_sell_only_pct") || strings.Contains(string(data), "cash_reserve_min_pct") || len(read.retired) != 0 ||
		read.policy.FingerprintKey() != risk.DefaultRulebookPolicy().FingerprintKey() || !strings.Contains(string(data), "overhedge_multiple = 2.0") {
		t.Fatalf("default file: err %v retired %v\n%s", err, read.retired, data)
	}
	// The premium budget sits beside rule 4's time value budget in every
	// regime table, and rule 15's bands follow the protection band.
	for _, want := range []string{
		"[regime_calm]\n", "premium_budget_watch_pct = 25.0\n", "premium_budget_act_pct = 35.0\n", "net_exposure_watch_pct = 100.0\n", "net_exposure_act_pct = 150.0\n",
		"premium_budget_watch_pct = 20.0\n", "net_exposure_act_pct = 130.0\n",
		"premium_budget_watch_pct = 15.0\n", "premium_budget_act_pct = 25.0\n", "net_exposure_watch_pct = 75.0\n", "net_exposure_act_pct = 100.0\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("default file lacks %q:\n%s", want, data)
		}
	}
	calm := string(data)[strings.Index(string(data), "[regime_calm]"):]
	if i, j := strings.Index(calm, "premium_budget_act_pct"), strings.Index(calm, "extrinsic_watch_pct"); i < 0 || j < 0 || i > j {
		t.Fatalf("the premium budget is not written beside extrinsic_*:\n%s", calm)
	}
}
