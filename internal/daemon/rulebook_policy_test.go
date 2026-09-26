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
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 1\ncash_reserve_min_pct = 70\n[modes]\nnet_exposure = \"alert\"\n")
	m.reload()
	p, st := m.Active()
	if st.Status != rpc.RulebookPolicyStatusActive || p.CashReserveMinPct != 70 || p.ModeFor(risk.RuleNetExposure) != risk.RuleModeAlert ||
		p.SingleNameActPct != risk.DefaultRulebookPolicy().SingleNameActPct || !slices.Equal(st.Overrides, []string{"cash_reserve_min_pct", "modes.net_exposure"}) {
		t.Fatalf("partial file: %+v %+v", st, p)
	}
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 1\ncash_reserve_min_pct = 50\n")
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusDrift || p.CashReserveMinPct != 70 || !strings.Contains(st.Message, "higher policy_version") {
		t.Fatalf("an edit without a version bump took effect: %+v", st)
	}
	writeRulebookTestFile(t, path, "policy_id = \"mine\"\npolicy_version = 2\ncash_reserve_min_pct = 50\n")
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.CashReserveMinPct != 50 || p.ModeFor(risk.RuleNetExposure) != risk.RuleModeTrack {
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
	writeRulebookTestFile(t, path, "policy_version = 5\ncash_reserve_min_pct = 60\n")
	m.reload()
	for name, body := range map[string]string{
		"unknown key": "policy_version = 6\ncash_reserve_min = 10\n",
		"invalid":     "policy_version = 6\noption_line_watch_pct = 20\noption_line_act_pct = 10\n",
		"syntax":      "policy_version = \n",
	} {
		writeRulebookTestFile(t, path, body)
		m.reload()
		if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusError || p.CashReserveMinPct != 60 || st.Message == "" {
			t.Fatalf("%s: %+v", name, st)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusDrift || p.CashReserveMinPct != 60 || !strings.Contains(st.Message, "restart") {
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
	edit, err := EditRulebookPolicy(path, []string{"cash_reserve_min_pct=70", "modes.winner_trim=track", "single_name_act_pct=40"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if edit.Version != risk.DefaultRulebookPolicy().Version+1 || !slices.Equal(edit.Differs, []string{"cash_reserve_min_pct", "modes.winner_trim"}) || len(edit.Changes) != 2 {
		t.Fatalf("edit = %+v", edit)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v err %v", info.Mode().Perm(), err)
	}
	before, _ := os.ReadFile(path)
	read, err := parseRulebookPolicy(before)
	if err != nil || len(read.missing) != 0 || policyFileReview(before) != "unreviewed" ||
		!strings.Contains(string(before), "# Rule 1 — worst-case loss on one issuer") || !strings.Contains(string(before), "cash_reserve_min_pct = 70") {
		t.Fatalf("first edit did not start from the template: missing %v err %v\n%s", read.missing, err, before)
	}
	for _, bad := range [][]string{{"cash_reserve_min_pct=abc"}, {"no_such_key=1"}, {"option_line_act_pct=1"}, {"modes.fx_exposure=loud"}, {"runway_act_dte=2.5"}} {
		if _, err := EditRulebookPolicy(path, bad, nil, false); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused edit changed the file")
	}
	m := newRulebookPolicyManager(path, time.Minute, time.Now)
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.CashReserveMinPct != 70 || p.ModeFor(risk.RuleWinnerTrim) != risk.RuleModeTrack {
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
cash_reserve_min_pct = 60.0

[modes]
winner_trim = "track"  # I like the reminder

[issuer_groups]
GroupA = ["AAA", "AAB"]
`
	writeRulebookTestFile(t, path, original)
	edit, err := EditRulebookPolicy(path, []string{"single_name_watch_pct=28", "issuer_groups.GroupB=BBB,BBC"}, []string{"cash_reserve_min_pct"}, false)
	if err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"policy_version = 7", "policy_version = 8",
		"single_name_watch_pct = 25.0  # tighter than Canary's", "single_name_watch_pct = 28.0  # tighter than Canary's",
		"cash_reserve_min_pct = 60.0", "cash_reserve_min_pct = "+tomlFloat(risk.DefaultRulebookPolicy().CashReserveMinPct),
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

// With basis = rulebook the governor restores the Rulebook's cash reserve and
// per-line limit as shares of NLV. It needs no constitution and waits for no
// brake, and protection legs are never sold.
func TestBudgetRulebookBasisRestoresTheCashReserveWithoutABrake(t *testing.T) {
	policy := budgetTestPolicy(rpc.BudgetReductionModeShadow, 0, 0)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	if err := validateProtectionPolicy(policy); err != nil {
		t.Fatal(err)
	}
	pos := budgetTestBook() // AAA 6×2,000 (loss 1,000), BBB 4×2,500 (loss 4,000), CCC 2×3,000 (gain)
	input := budgetGovernorInput{Rulebook: risk.DefaultRulebookPolicy(), NLVBase: new(100000.0), AvailableFundsBase: new(60000.0), AccountBaseCurrency: "EUR"}
	rows, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	// Line limit 10% = 10,000: AAA sells 1 (raises 2,000). Reserve 75% = 75,000
	// against 60,000 available: 15,000 short, 13,000 after AAA. BBB (largest
	// loss) sells all 4 (10,000), then AAA 2 more (4,000).
	if st == nil || st.Basis != rpc.BudgetBasisRulebook || st.State != rpc.BudgetStateOverBudget || st.CashShortfallBase == nil ||
		math.Abs(*st.CashShortfallBase-15000) > 1e-6 || st.ProtectionLegs != 2 || st.Rows != 2 || !st.Shadow {
		t.Fatalf("status = %+v", st)
	}
	assertBudgetRowsReduceOnly(t, rows, pos)
	byID := budgetRowsByConID(rows)
	if aaa := byID[601]; aaa.Quantity != 3 || aaa.Budget == nil || aaa.Budget.ContractsPerLine != 1 || aaa.Budget.ContractsTotal != 2 || aaa.Budget.Basis != rpc.BudgetBasisRulebook {
		t.Fatalf("AAA = %+v", aaa.Budget)
	}
	if bbb := byID[602]; bbb.Quantity != 4 || bbb.Budget.Cap != "total" || !strings.Contains(bbb.Reason, "cash reserve") {
		t.Fatalf("BBB = %+v %s", bbb.Budget, bbb.Reason)
	}
	if _, ok := byID[603]; ok {
		t.Fatal("a gaining line was sold before the losers covered the shortfall")
	}
	input.AvailableFundsBase = new(90000.0)
	pos.Options = pos.Options[:4] // drop CCC; AAA still above the line limit
	if _, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime()); st.State != rpc.BudgetStateOverBudget || st.CashShortfallBase != nil {
		t.Fatalf("line limit alone: %+v", st)
	}
	input.NLVBase = nil
	if _, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime()); st.State != rpc.BudgetStateAccountUnavailable {
		t.Fatalf("no NLV: %+v", st)
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
	writeRulebookTestFile(t, path, "policy_version = 4\ncash_reserve_min_pct = 70\n")
	m.reload()
	if _, ok := s.cachedRulebookResult(rulebookCacheBinding{scope: scope}, time.Minute, now); ok {
		t.Fatal("a verdict from a superseded policy was served")
	}
}

// Until v3.11.1 canary policy default rulebook wrote cash_sell_only_pct, which
// no rule reads. A file carrying it must not void the limits the owner set
// beside it: it loads, the key is ignored, and the status says which and why.
func TestRulebookPolicyFileWithARetiredKeyKeepsTheOwnersLimits(t *testing.T) {
	m, path, _ := rulebookTestManager(t)
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\ncash_reserve_min_pct = 70\n\n[regime_calm]\ncash_sell_only_pct = -25\n")
	m.reload()
	p, st := m.Active()
	if st.Status != rpc.RulebookPolicyStatusActive || p.CashReserveMinPct != 70 || !slices.Equal(st.Overrides, []string{"cash_reserve_min_pct"}) ||
		!strings.Contains(st.Message, "regime_calm.cash_sell_only_pct") || !strings.Contains(st.Message, "cash_reserve_min_pct") {
		t.Fatalf("retired key: %+v (cash reserve %v)", st, p.CashReserveMinPct)
	}
	m.reload()
	if _, st := m.Active(); !strings.Contains(st.Message, "cash_sell_only_pct") {
		t.Fatalf("the note vanished on a steady reload: %+v", st)
	}
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\ncash_reserve_min_pct = 70\n\n[regime_calm]\ncash_sell_only_pcx = -25\n")
	m.reload()
	if _, st := m.Active(); st.Status != rpc.RulebookPolicyStatusError || !strings.Contains(st.Message, "cash_sell_only_pcx") {
		t.Fatalf("a misspelt key was tolerated like a retired one: %+v", st)
	}
}

// set refuses the retired key, reset removes it, and any other edit drops it,
// so a key that changes nothing never reads as a limit in force.
func TestEditRulebookPolicyRefusesAndRemovesTheRetiredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rulebook-policy.toml")
	writeRulebookTestFile(t, path, "policy_id = \"rulebook-owner\"\npolicy_version = 4\ncash_reserve_min_pct = 70\n\n[regime_calm]\ncash_sell_only_pct = -25\n\n[regime_confirmed]\ncash_sell_only_pct = 10\n")
	before, _ := os.ReadFile(path)
	_, err := EditRulebookPolicy(path, []string{"regime_confirmed.cash_sell_only_pct=5"}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "cash_reserve_min_pct") {
		t.Fatalf("set accepted the retired key: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused set changed the file")
	}
	edit, err := EditRulebookPolicy(path, []string{"overhedge_multiple=1.5"}, []string{"regime_calm.cash_sell_only_pct"}, false)
	if err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)
	if strings.Contains(string(written), "cash_sell_only_pct") || !slices.Equal(edit.Differs, []string{"cash_reserve_min_pct", "overhedge_multiple"}) {
		t.Fatalf("edit kept a retired key or lost a limit: %+v\n%s", edit, written)
	}
	removed := 0
	for _, c := range edit.Changes {
		if strings.HasSuffix(c.Key, ".cash_sell_only_pct") && strings.HasPrefix(c.To, "removed") {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("changes do not report both retired keys removed: %+v", edit.Changes)
	}
	if _, err := EditRulebookPolicy(path, []string{"overhedge_multiple=0.5"}, nil, false); err == nil {
		t.Fatal("an over-hedge multiple below 1 was accepted")
	}
	m := newRulebookPolicyManager(path, time.Minute, time.Now)
	m.reload()
	if p, st := m.Active(); st.Status != rpc.RulebookPolicyStatusActive || p.OverhedgeMultiple != 1.5 || st.Message != "" {
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
	if err != nil || strings.Contains(string(data), "cash_sell_only_pct") || len(read.retired) != 0 ||
		read.policy.FingerprintKey() != risk.DefaultRulebookPolicy().FingerprintKey() || !strings.Contains(string(data), "overhedge_multiple = 2.0") {
		t.Fatalf("default file: err %v retired %v\n%s", err, read.retired, data)
	}
}
