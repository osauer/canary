package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Amendment 2026-09-30 (budget governor): the advisory Rulebook review state
// (the review gate of 09:10 was removed at 12:35), the ranking by what a sale
// fixes, and the whole plan on the status and every row.

// budgetPlanLeg is a discretionary call (strike 100, 20 a share, 2,000 a
// contract) whose underlying price sets its time value: at 90 the premium is
// all time value, at 110 half, at 116 a fifth; nil leaves it unknown.
func budgetPlanLeg(symbol string, conID int, unrealized float64, underlying *float64) rpc.PositionView {
	row := budgetOptionLeg(symbol, conID, "C", 4, 2000, unrealized)
	row.Underlying = underlying
	return row
}

// budgetPlanBook holds four lines of 8,000 each, 32,000 at risk:
//
//	AAA  loss   500  50% time value  offends rules 2 and 13
//	BBB  loss   100 100% time value  offends rule 4
//	CCC  loss 3,000  20% time value  its issuer (GroupC) offends rule 1
//	DDD  loss 6,000  time value unknown, offends nothing open
//
// Largest loss first would sell DDD, then CCC; the amendment sells AAA
// (two open rules), then BBB (one rule, more time value than CCC).
func budgetPlanBook() *rpc.PositionsResult {
	return &rpc.PositionsResult{
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Options: []rpc.PositionView{
			budgetPlanLeg("DDD", 704, -6000, nil),
			budgetPlanLeg("CCC", 703, -3000, new(116.0)),
			budgetPlanLeg("BBB", 702, -100, new(90.0)),
			budgetPlanLeg("AAA", 701, -500, new(110.0)),
		},
	}
}

// budgetPlanRules is a synthetic Rulebook result. Only rows at watch or act
// among rules 1, 2, 4, 5, 13, 16 and 18 count, and only measured offenders:
// DDD appears on a passing row, an unknown row, rule 3, rule 17, and as an
// unmeasured offender of rule 13, none of which is relief.
func budgetPlanRules() *rpc.RulesResult {
	return &rpc.RulesResult{Rules: []risk.RuleRow{
		{ID: risk.RuleOptionLinePremium, Number: 2, Status: risk.RuleStatusWatch, Offenders: []risk.RuleOffender{
			{Symbol: "AAA", Leg: "AAA 20261218 C 100", Observed: 10, Status: risk.RuleStatusWatch}}},
		{ID: risk.RuleExitDiscipline, Number: 13, Status: risk.RuleStatusAct, Offenders: []risk.RuleOffender{
			{Symbol: "AAA", Leg: "AAA 20261218 C 100", Observed: 70, Status: risk.RuleStatusAct},
			{Symbol: "DDD", Leg: "DDD 20261218 C 100", Status: risk.RuleStatusUnknown}}},
		{ID: risk.RuleExtrinsicBudget, Number: 4, Status: risk.RuleStatusWatch, Offenders: []risk.RuleOffender{
			{Symbol: "BBB", Leg: "BBB 20261218 C 100", Observed: 10}}},
		{ID: risk.RuleSingleNameExposure, Number: 1, Status: risk.RuleStatusWatch, Offenders: []risk.RuleOffender{
			{Symbol: "GroupC", Observed: 12, Status: risk.RuleStatusWatch}}},
		{ID: risk.RuleExpiryRunway, Number: 5, Status: risk.RuleStatusPass, Offenders: []risk.RuleOffender{{Symbol: "DDD", Leg: "DDD 20261218 C 100"}}},
		{ID: risk.RuleDeltaSwing, Number: 16, Status: risk.RuleStatusUnknown, Offenders: []risk.RuleOffender{{Symbol: "DDD"}}},
		{ID: risk.RuleCashSellOnly, Number: 3, Status: risk.RuleStatusAct, Offenders: []risk.RuleOffender{{Symbol: "DDD", Leg: "DDD 20261218 C 100"}}},
		{ID: risk.RuleClusterStress, Number: 17, Status: risk.RuleStatusWatch, Offenders: []risk.RuleOffender{{Symbol: "DDD"}}},
	}}
}

// budgetPlanInput measures the book on the Rulebook basis: 32,000 at risk
// is 40% of an 80,000 NLV, over the calm set's 35% act level, so the total
// is cut back to 25% (20,000): 12,000, six contracts. No line is above the
// 10% line limit (8,000), so only the total pass sells.
func budgetPlanInput() budgetGovernorInput {
	rb := risk.DefaultRulebookPolicy()
	rb.IssuerGroups = map[string][]string{"GroupC": {"CCC", "CCD"}}
	return budgetGovernorInput{Rulebook: rb, RulebookStatus: reviewedRulebookStatus(), NLVBase: new(80000.0), AccountBaseCurrency: "EUR", Rules: budgetPlanRules()}
}

// reviewedRulebookStatus is an owner file in force, read cleanly, without
// Canary's review marker: the one review state that adds no advisory line.
func reviewedRulebookStatus() rpc.RulebookPolicyStatus {
	return rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: rulebookPolicySourceFile}
}

func budgetPlanPolicy(mode string, maxOrderNotional float64) protectionPolicy {
	p := budgetTestPolicy(mode, 0, 0)
	p.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	p.Buckets.BudgetReduction.MaxOrderNotional = maxOrderNotional
	return p
}

// budgetVetoSentence is the last detail line of every governor row.
const budgetVetoSentence = "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop"

// budgetRowsFollowTheMode checks that the configured mode alone decided the
// rows and that each carries the review state's advisory line (none when
// note is empty) just before the veto sentence.
func budgetRowsFollowTheMode(t *testing.T, st *rpc.TradeProposalBudgetStatus, rows []rpc.TradeProposal, mode, review, note string) {
	t.Helper()
	shadow := mode == rpc.BudgetReductionModeShadow
	if st == nil || st.Mode != mode || st.Shadow != shadow || st.ShadowReason != "" || st.RulebookReview != review || st.Rows != 2 || len(rows) != 2 {
		t.Fatalf("status = %+v rows %d; want mode %s shadow %v review %s", st, len(rows), mode, shadow, review)
	}
	for _, row := range rows {
		if slices.ContainsFunc(row.Blockers, func(b rpc.TradingBlocker) bool { return strings.HasPrefix(b.Code, "rulebook_") }) {
			t.Fatalf("a review state blocks the row: %+v", row.Blockers)
		}
		if row.Shadow != shadow || row.AutomaticEligible() == shadow {
			t.Fatalf("row shadow %v automatic %v, want the mode's %s: %+v", row.Shadow, row.AutomaticEligible(), mode, row)
		}
		if shadow {
			if len(row.Blockers) == 0 || row.Blockers[0].Code != "shadow_mode" || row.State != rpc.TradeProposalStateBlocked {
				t.Fatalf("shadow row blockers = %+v", row.Blockers)
			}
			if got := shadowProposalBlockers(row); len(got) != 1 || got[0].Code != "shadow_mode" {
				t.Fatalf("shadow refusal = %+v", got)
			}
		} else if len(row.Blockers) != 0 || shadowProposalBlockers(row) != nil || queuedQueueableBlockers(row) != nil {
			t.Fatalf("active row refused: blockers %+v, preview %+v, queue %+v", row.Blockers, shadowProposalBlockers(row), queuedQueueableBlockers(row))
		}
		n := len(row.Details)
		if n < 2 || row.Details[n-1] != budgetVetoSentence {
			t.Fatalf("details = %q", row.Details)
		}
		advisory := slices.IndexFunc(row.Details, func(d string) bool { return strings.HasPrefix(d, "Rulebook limits: ") })
		switch {
		case note == "" && advisory >= 0:
			t.Fatalf("a reviewed file still carries an advisory line: %q", row.Details)
		case note != "" && (advisory != n-2 || row.Details[advisory] != note):
			t.Fatalf("advisory line = %d in %q, want %q before the veto sentence", advisory, row.Details, note)
		}
	}
}

// Owner decision 2026-09-30 12:35 CEST ("no shadow, armed. Human need to
// approve anyway."): the review gate of 09:10 is gone. An unreviewed Rulebook
// policy file no longer holds an active rulebook basis in shadow: the rows
// are ordinary proposals that preview, the queue and the pre-authorisation
// scheduler accept, and each names the review state in one advisory line.
func TestBudgetRulebookReviewIsAdvisory(t *testing.T) {
	now := optionExitTestTime()
	input := budgetPlanInput()
	input.RulebookStatus.Review = rpc.PolicyReviewUnreviewed
	const unreviewed = "Rulebook limits: Canary's defaults, not yet reviewed"
	rows, st := (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, input, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	budgetRowsFollowTheMode(t, st, rows, rpc.BudgetReductionModeActive, rpc.BudgetRulebookUnreviewed, unreviewed)
	if counts := proposalCounts(rows, "EUR"); counts.Actionable != 2 || counts.BudgetReductionShadow != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	raw, err := json.Marshal(st)
	if err != nil || !strings.Contains(string(raw), `"rulebook_review":"unreviewed"`) || strings.Contains(string(raw), "shadow_reason") {
		t.Fatalf("status JSON = %s (err %v)", raw, err)
	}

	// A configured shadow mode is the only way the rows go to shadow, and
	// shadow_mode is then their only refusal.
	rows, st = (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeShadow, 1e9), rpc.ProtectionPolicyStatus{}, input, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	budgetRowsFollowTheMode(t, st, rows, rpc.BudgetReductionModeShadow, rpc.BudgetRulebookUnreviewed, unreviewed)

	// Reviewed: the same rows without the advisory line.
	input.RulebookStatus = reviewedRulebookStatus()
	rows, st = (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, input, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	budgetRowsFollowTheMode(t, st, rows, rpc.BudgetReductionModeActive, rpc.BudgetRulebookReviewed, "")

	// The declared-risk-capital basis sells against the owner's own caps:
	// it reads no Rulebook limits, so it names no review state.
	declared := budgetLatchedInput()
	declaredRows, st := (&proposalEngine{}).budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15), rpc.ProtectionPolicyStatus{}, declared, nil, budgetTestBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if st.Shadow || st.ShadowReason != "" || st.RulebookReview != "" || len(declaredRows) == 0 {
		t.Fatalf("declared basis status = %+v", st)
	}
	for _, row := range declaredRows {
		if slices.ContainsFunc(row.Details, func(d string) bool { return strings.HasPrefix(d, "Rulebook limits: ") }) {
			t.Fatalf("declared basis row names the Rulebook review: %q", row.Details)
		}
	}
}

// The mode is followed in every review state, read through the engine from a
// real policy manager: no file (the compiled baseline), Canary's template
// still marked unreviewed, a reviewed file, a file in drift, and a file in
// error after a good load and before any. Each state is named on the status
// and, unless reviewed, in one advisory line on every row.
func TestBudgetFollowsTheModeInEveryRulebookReviewState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := optionExitTestTime()
	check := func(t *testing.T, m *rulebookPolicyManager, review, note string) {
		t.Helper()
		gathered := (&proposalEngine{server: &Server{rulebookPolicies: m}}).budgetGovernorInput(nil, now)
		input := budgetPlanInput()
		input.RulebookStatus = gathered.RulebookStatus
		for _, mode := range []string{rpc.BudgetReductionModeActive, rpc.BudgetReductionModeShadow} {
			rows, st := (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(mode, 1e9), rpc.ProtectionPolicyStatus{}, input, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			budgetRowsFollowTheMode(t, st, rows, mode, review, note)
		}
	}

	// No file: Canary's compiled baseline is in force.
	m, path, _ := rulebookTestManager(t)
	m.reload()
	check(t, m, rpc.BudgetRulebookNoFile, "Rulebook limits: no Rulebook policy file; compiled defaults")
	// An engine without a policy manager reads the same way.
	if review, _ := budgetRulebookReview((&proposalEngine{}).budgetGovernorInput(nil, now).RulebookStatus); review != rpc.BudgetRulebookNoFile {
		t.Fatalf("bare engine review = %q", review)
	}
	// Canary's template, still marked unreviewed.
	writeRulebookTestFile(t, path, PolicyUnreviewedMarker+"\npolicy_id = \"synthetic\"\npolicy_version = 1\nfx_exposure_watch_pct = 70\n")
	m.reload()
	check(t, m, rpc.BudgetRulebookUnreviewed, "Rulebook limits: Canary's defaults, not yet reviewed")
	// Reviewed: the marker line deleted.
	writeRulebookTestFile(t, path, "policy_id = \"synthetic\"\npolicy_version = 1\nfx_exposure_watch_pct = 70\n")
	m.reload()
	check(t, m, rpc.BudgetRulebookReviewed, "")
	// Drift: an edit without a higher policy_version is not the file in force.
	writeRulebookTestFile(t, path, "policy_id = \"synthetic\"\npolicy_version = 1\nfx_exposure_watch_pct = 50\n")
	m.reload()
	check(t, m, rpc.BudgetRulebookDrift, "Rulebook limits: the file on disk is not the one in force")
	// Error: an unreadable file keeps the last good file in force.
	writeRulebookTestFile(t, path, "policy_id = \"synthetic\"\npolicy_version = 2\nnot_a_rulebook_key = 1\n")
	m.reload()
	check(t, m, rpc.BudgetRulebookError, "Rulebook limits: the file could not be read; the last good file applies")
	// Error before any good file: the compiled baseline applies.
	first, firstPath, _ := rulebookTestManager(t)
	writeRulebookTestFile(t, firstPath, "not_a_rulebook_key = 1\n")
	first.reload()
	check(t, first, rpc.BudgetRulebookError, "Rulebook limits: the file could not be read; Canary's compiled defaults apply")
}

// The engine reads the status of the file in force and the Rulebook result
// the daemon holds; deleting the marker line reads reviewed at the next
// reload.
func TestBudgetGovernorInputReadsTheReviewAndTheHeldRulebookResult(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m, path, _ := rulebookTestManager(t)
	writeRulebookTestFile(t, path, PolicyUnreviewedMarker+"\npolicy_id = \"synthetic\"\npolicy_version = 1\n")
	m.reload()
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	port := 7497
	srv := &Server{rulebookPolicies: m, cfg: &config.Resolved{Gateway: config.Gateway{Account: "DU1234567", Port: &port}}, now: func() time.Time { return now }}
	policy, _ := m.Active()
	fp := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: policy.FingerprintKey()}
	srv.lastRules, srv.lastRulesAt = &rpc.RulesResult{AsOf: now, PolicyFingerprint: &fp, Rules: budgetPlanRules().Rules}, now
	srv.lastRulesScope = brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}

	in := (&proposalEngine{server: srv}).budgetGovernorInput(nil, now)
	if in.RulebookStatus.Review != rpc.PolicyReviewUnreviewed || in.RulebookStatus.Path != path || in.Rules == nil || len(in.Rules.Rules) != len(budgetPlanRules().Rules) {
		t.Fatalf("input = status %+v rules %v", in.RulebookStatus, in.Rules)
	}
	writeRulebookTestFile(t, path, "policy_id = \"synthetic\"\npolicy_version = 1\n")
	m.reload()
	if in := (&proposalEngine{server: srv}).budgetGovernorInput(nil, now); in.RulebookStatus.Review != "" {
		t.Fatalf("reviewed file still reads unreviewed: %+v", in.RulebookStatus)
	} else if review, note := budgetRulebookReview(in.RulebookStatus); review != rpc.BudgetRulebookReviewed || note != "" {
		t.Fatalf("reviewed file review = %q %q", review, note)
	}
	// A result older than the preview window is not current: no relief.
	srv.lastRulesAt = now.Add(-rulesPreviewTTL - time.Second)
	if in := (&proposalEngine{server: srv}).budgetGovernorInput(nil, now); in.Rules != nil {
		t.Fatal("a stale Rulebook result was read")
	}
}

// G2: relief beats time value, time value beats loss, on both bases; a
// ranking without a Rulebook result falls back to time value, then loss, and
// says so.
func TestBudgetRankingReliefThenTimeValueThenLoss(t *testing.T) {
	now := optionExitTestTime()
	rows, st := (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, budgetPlanInput(), nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	assertBudgetRowsReduceOnly(t, rows, budgetPlanBook())
	byID := budgetRowsByConID(rows)
	if len(rows) != 2 || byID[701].Quantity != 4 || byID[701].Budget.Order != 1 || byID[702].Quantity != 2 || byID[702].Budget.Order != 2 || st.RankingWithoutRulebook {
		t.Fatalf("rows %+v status %+v", byID, st)
	}
	if !strings.Contains(byID[702].Reason, "2nd in the reduction order (most open Rulebook rules relieved first, then most time value, then largest loss)") {
		t.Fatalf("reason does not name the order: %q", byID[702].Reason)
	}
	var ranked []int
	for _, c := range st.Candidates {
		ranked = append(ranked, c.Contract.ConID)
	}
	if !slices.Equal(ranked, []int{701, 702, 703}) {
		t.Fatalf("candidates = %v", ranked)
	}
	if c := st.Candidates[2]; !slices.Equal(c.Relief, []string{risk.RuleSingleNameExposure}) {
		t.Fatalf("issuer-group relief = %+v", c)
	}

	// The declared-risk-capital basis runs the same algorithm: 32,000 against
	// a 40% cap of 50,000 is 12,000 over, and no line exceeds 20%.
	declared := budgetLatchedInput()
	declared.Rulebook, declared.Rules = budgetPlanInput().Rulebook, budgetPlanRules()
	rows, _ = (&proposalEngine{}).budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 20), rpc.ProtectionPolicyStatus{}, declared, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if byID := budgetRowsByConID(rows); len(rows) != 2 || byID[701].Quantity != 4 || byID[702].Quantity != 2 {
		t.Fatalf("declared basis rows = %+v", byID)
	}

	// No Rulebook result: nobody relieves anything, time value leads (BBB
	// 100%, AAA 50%), and the status and reason say so.
	input := budgetPlanInput()
	input.Rules = nil
	rows, st = (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, input, nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	byID = budgetRowsByConID(rows)
	if !st.RankingWithoutRulebook || byID[702].Quantity != 4 || byID[701].Quantity != 2 || len(rows) != 2 ||
		!strings.Contains(byID[701].Reason, "no current Rulebook result, so most time value first, then largest loss") ||
		st.Candidates[0].Why != "no Rulebook result; 100% time value; unrealised −100" {
		t.Fatalf("without rulebook: rows %+v status %+v", byID, st)
	}
}

// The tie-breaks: relief, then time value (unknown last), then loss, then
// value, then the lower contract id. An ignored line, or one that counts
// for nothing, is not ranked.
func TestBudgetRankTieBreaks(t *testing.T) {
	line := func(conID int, relief int, tv *float64, loss, value float64) budgetLine {
		l := budgetLine{row: rpc.PositionView{ConID: conID}, contracts: 1, unitBase: value, valueBase: value, lossBase: loss, timeValuePct: tv}
		for range relief {
			l.relief = append(l.relief, budgetRelief{id: risk.RuleExtrinsicBudget, number: 4})
		}
		return l
	}
	lines := []budgetLine{
		line(5, 0, nil, 900, 100),
		line(4, 0, new(30.0), 0, 50),
		line(3, 0, new(30.0), 20, 50),
		line(2, 0, new(30.0), 20, 80),
		line(1, 0, new(30.0), 20, 80),
		line(8, 1, nil, 0, 10),
		line(7, 0, new(99.0), 999, 0),
		line(6, 3, new(99.0), 999, 100),
	}
	lines[len(lines)-1].ignored = true
	order := budgetRank(lines, budgetLineUnitValue)
	var got []int
	for _, i := range order {
		got = append(got, lines[i].row.ConID)
		if lines[i].rank != len(got) {
			t.Fatalf("rank of %d = %d", lines[i].row.ConID, lines[i].rank)
		}
	}
	if !slices.Equal(got, []int{8, 1, 2, 3, 4, 5}) {
		t.Fatalf("order = %v", got)
	}
	if lines[6].rank != 0 || lines[7].rank != 0 {
		t.Fatal("an ignored or worthless line was ranked")
	}
}

// G3: the status carries at most three candidates and the whole plan; a plan
// that max_order_notional holds back runs into cycle 2, and every row names
// its place in the plan, what else it relieves and the next two candidates.
func TestBudgetPlanCandidatesCyclesAndDetails(t *testing.T) {
	now := optionExitTestTime()
	// 4,000 per order at 2,000 a contract: AAA's 4 go in two cycles.
	rows, st := (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 4000), rpc.ProtectionPolicyStatus{}, budgetPlanInput(), nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Candidates []map[string]any `json:"candidates"`
		Plan       []map[string]any `json:"plan"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Candidates) != 3 || len(wire.Plan) != 3 {
		t.Fatalf("status JSON = %s", raw)
	}
	first := wire.Candidates[0]
	if first["rank"] != 1.0 || first["contracts"] != 4.0 || first["unit_value_base"] != 2000.0 || first["time_value_pct"] != 50.0 ||
		first["unrealized_pnl_base"] != -500.0 || first["why"] != "offends 2 open rules; 50% time value; unrealised −500" ||
		!slices.Equal(anyStrings(first["relief"]), []string{risk.RuleOptionLinePremium, risk.RuleExitDiscipline}) ||
		first["contract"].(map[string]any)["con_id"] != 701.0 || first["contract"].(map[string]any)["right"] != "C" ||
		first["contract"].(map[string]any)["strike"] != 100.0 || first["contract"].(map[string]any)["expiry"] != "20261218" {
		t.Fatalf("first candidate = %v", first)
	}
	type order struct {
		conID, contracts, cycle, rank int
		raises                        float64
	}
	var plan []order
	for _, o := range st.Plan {
		plan = append(plan, order{o.Contract.ConID, o.Contracts, o.Cycle, o.Rank, o.RaisesBase})
	}
	if !slices.Equal(plan, []order{{701, 2, 1, 1, 4000}, {701, 2, 2, 1, 4000}, {702, 2, 1, 2, 4000}}) {
		t.Fatalf("plan = %+v", plan)
	}
	byID := budgetRowsByConID(rows)
	aaa, bbb := byID[701], byID[702]
	if aaa.Quantity != 2 || bbb.Quantity != 2 {
		t.Fatalf("rows AAA %d BBB %d", aaa.Quantity, bbb.Quantity)
	}
	for _, want := range []string{
		"order 1 of 3 in the plan; order 2 of 3 (2 contracts of the same line) follows after this fill",
		"also relieves rule 2 (line 10.0% of NLV), rule 13 (70% of premium lost)",
		"alternatives: 2nd BBB 20261218 C 100 (offends 1 open rule; 100% time value; unrealised −100); 3rd CCC 20261218 C 100 (offends 1 open rule; 20% time value; unrealised −3.0k)",
	} {
		if !slices.Contains(aaa.Details, want) {
			t.Fatalf("AAA details miss %q:\n%s", want, strings.Join(aaa.Details, "\n"))
		}
	}
	for _, want := range []string{
		"order 3 of 3 in the plan, the last",
		"also relieves rule 4",
		"alternatives: 3rd CCC 20261218 C 100 (offends 1 open rule; 20% time value; unrealised −3.0k); 4th DDD 20261218 C 100 (offends no open rule; time value unknown; unrealised −6.0k)",
	} {
		if !slices.Contains(bbb.Details, want) {
			t.Fatalf("BBB details miss %q:\n%s", want, strings.Join(bbb.Details, "\n"))
		}
	}
	// The veto sentence stays last.
	if last := aaa.Details[len(aaa.Details)-1]; !strings.HasPrefix(last, "waits the full veto window") {
		t.Fatalf("last detail = %q", last)
	}

	// Unheld, the next order is the next line.
	rows, st = (&proposalEngine{}).budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, budgetPlanInput(), nil, budgetPlanBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if aaa := budgetRowsByConID(rows)[701]; len(st.Plan) != 2 || st.Plan[1].Cycle != 1 ||
		!slices.Contains(aaa.Details, "order 1 of 2 in the plan; order 2 of 2 (2 contracts of BBB 20261218 C 100) is the next line") {
		t.Fatalf("unheld plan %+v details %q", st.Plan, aaa.Details)
	}
	// A snapshot copy owns the new lists.
	copied := cloneBudgetStatus(st)
	copied.Candidates[0].Relief[0], *copied.Candidates[0].TimeValuePct, copied.Plan[0].Contracts = "x", 0, 99
	if st.Candidates[0].Relief[0] != risk.RuleOptionLinePremium || *st.Candidates[0].TimeValuePct != 50 || st.Plan[0].Contracts != 4 {
		t.Fatal("the snapshot copy shares the candidates or the plan")
	}
}

// Ignoring a row takes its line out of the plan: the next refresh sells the
// next candidates instead, and the ignored line is no longer a candidate.
func TestBudgetIgnoredLineMovesThePlanToTheNextCandidate(t *testing.T) {
	book := budgetPlanBook()
	engine := &proposalEngine{ignored: map[string]struct{}{}}
	for _, row := range book.Options {
		if row.ConID == 701 {
			engine.ignored[scopedIgnoreKey(brokerStateScope{}, budgetRowKey(row))] = struct{}{}
		}
	}
	rows, st := engine.budgetReductionProposals(budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), rpc.ProtectionPolicyStatus{}, budgetPlanInput(), nil, book, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	byID := budgetRowsByConID(rows)
	if _, ok := byID[701]; ok || len(rows) != 2 || byID[702].Quantity != 4 || byID[703].Quantity != 2 || st.Rows != 2 {
		t.Fatalf("rows %+v", byID)
	}
	if st.Candidates[0].Contract.ConID != 702 || st.Candidates[0].Rank != 1 || slices.ContainsFunc(st.Candidates, func(c rpc.TradeProposalBudgetCandidate) bool { return c.Contract.ConID == 701 }) {
		t.Fatalf("candidates = %+v", st.Candidates)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}
