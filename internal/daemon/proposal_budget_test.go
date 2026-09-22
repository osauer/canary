package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// budgetTestPolicy enables the governor with the caps the product manager set
// on 2026-09-21 (40% total, 15% per line) unless a test overrides them. The
// option-exit bucket stays at the embedded default (disabled, no standing
// classification), which is the state a desk that never enabled option exits
// runs in — the governor must still find protection legs.
func budgetTestPolicy(mode string, totalPct, perLinePct float64) protectionPolicy {
	p := defaultProtectionPolicy()
	p.PolicyVersion = 9
	p.Buckets.ThetaHygiene.Enabled, p.Buckets.RiskReduction.Enabled, p.Buckets.TrailingStop.Enabled = false, false, false
	p.Buckets.BudgetReduction = &protectionBudgetPolicy{Enabled: true, Mode: mode, PremiumAtRiskPctOfRiskCapital: totalPct, PerLinePctOfRiskCapital: perLinePct, MaxOrderNotional: 1e9}
	return p
}

// budgetLatchedInput is an approved constitution (50,000 EUR declared) with the
// drawdown block tier latched under advisory enforcement.
func budgetLatchedInput() budgetGovernorInput {
	return budgetGovernorInput{
		Constitution: approvedTestConstitution(),
		Capital:      rpc.CapitalStateReport{Tier: risk.CapitalTierBlock, BlockLatched: true, Enforcement: risk.EnforcementAdvisory, BaseCurrency: "EUR"},
	}
}

func budgetOptionLeg(symbol string, conID int, right string, qty float64, unitBase, unrealized float64) rpc.PositionView {
	mv := unitBase * qty
	pnl := unrealized
	return rpc.PositionView{
		Symbol: symbol, SecType: "OPTION", ConID: conID, Exchange: "SMART", Currency: "USD", Quantity: qty, Multiplier: 100,
		LocalSymbol: symbol + "  261218" + right + "00100000", TradingClass: symbol, Expiry: "20261218", Strike: 100, Right: right,
		Mark: unitBase / 100, MarketValue: mv, MarketValueBase: &mv, UnrealizedPnL: unrealized, UnrealizedPnLBase: &pnl,
		DataType: rpc.MarketDataLive, PriceAt: optionExitTestTime(),
	}
}

// budgetTestBook: two protection legs the governor may never touch (a
// hedge-listed SPY put and a put covering the long TEST stock) and three
// discretionary lines over a 50,000 EUR declared risk capital:
//
//	AAA  6 ct × 2,000 = 12,000 (24%)  loss 1,000
//	BBB  4 ct × 2,500 = 10,000 (20%)  loss 4,000
//	CCC  2 ct × 3,000 =  6,000 (12%)  gain   500
func budgetTestBook() *rpc.PositionsResult {
	return &rpc.PositionsResult{
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Stocks:    []rpc.PositionView{{Symbol: "TEST", SecType: "STK", ConID: 7, Quantity: 100, Multiplier: 1}},
		Options: []rpc.PositionView{
			budgetOptionLeg("SPY", 501, "P", 3, 2000, -300),
			budgetOptionLeg("TEST", 502, "P", 1, 1000, -100),
			budgetOptionLeg("AAA", 601, "C", 6, 2000, -1000),
			budgetOptionLeg("BBB", 602, "C", 4, 2500, -4000),
			budgetOptionLeg("CCC", 603, "C", 2, 3000, 500),
		},
	}
}

func budgetRowsByConID(rows []rpc.TradeProposal) map[int]rpc.TradeProposal {
	out := map[int]rpc.TradeProposal{}
	for _, row := range rows {
		out[row.Contract.ConID] = row
	}
	return out
}

func assertBudgetRowsReduceOnly(t *testing.T, rows []rpc.TradeProposal, pos *rpc.PositionsResult) {
	t.Helper()
	held := map[int]float64{}
	for _, row := range pos.Options {
		held[row.ConID] = row.Quantity
	}
	for _, row := range rows {
		if row.Bucket != rpc.TradeProposalBucketBudgetReduction || row.Action != rpc.OrderActionSell || !proposalCloseReduceEffect(row.PositionEffect) ||
			row.Quantity < 1 || float64(row.Quantity) > held[row.Contract.ConID] || row.SecType != "OPT" || !row.NeverSkipVeto {
			t.Fatalf("row would not reduce risk within the position: %+v", row)
		}
		if (row.PositionEffect == rpc.OrderPositionEffectClose) != (float64(row.Quantity) == held[row.Contract.ConID]) {
			t.Fatalf("position effect %s disagrees with quantity %d of %g: %s", row.PositionEffect, row.Quantity, held[row.Contract.ConID], row.Key)
		}
	}
}

func TestBudgetReductionPerLineThenTotalLargestLossFirst(t *testing.T) {
	// Total cap 20% = 10,000. Per line first: AAA keeps 3 (7,500/2,000) and
	// sells 3; BBB keeps 3 (7,500/2,500) and sells 1; CCC is within its cap.
	// Projected 6,000 + 7,500 + 6,000 = 19,500 leaves 9,500 over the total:
	// BBB (largest loss) sells its remaining 3 (7,500), then AAA sells 1
	// (2,000) and the excess is covered; CCC (a gain) is never reached.
	policy := budgetTestPolicy(rpc.BudgetReductionModeShadow, 20, 15)
	pos := budgetTestBook()
	engine := &proposalEngine{}
	rows, st := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	if st == nil || st.State != rpc.BudgetStateOverBudget || st.ProtectionLegs != 2 || st.IncludedLegs != 3 || st.ExcludedLegs != 0 || st.Rows != 2 ||
		st.DeclaredRiskCapitalBase == nil || *st.DeclaredRiskCapitalBase != 50000 || st.MeasuredPremiumBase == nil || *st.MeasuredPremiumBase != 28000 ||
		st.MeasuredPctOfRiskCapital == nil || math.Abs(*st.MeasuredPctOfRiskCapital-56) > 1e-9 || st.TotalExcessBase == nil || *st.TotalExcessBase != 18000 ||
		!st.Shadow || st.Mode != rpc.BudgetReductionModeShadow || st.BaseCurrency != "EUR" {
		t.Fatalf("status = %+v", st)
	}
	assertBudgetRowsReduceOnly(t, rows, pos)
	byID := budgetRowsByConID(rows)
	if _, ok := byID[501]; ok {
		t.Fatal("the hedge-listed SPY put was selected")
	}
	if _, ok := byID[502]; ok {
		t.Fatal("the put covering TEST stock was selected")
	}
	if _, ok := byID[603]; ok {
		t.Fatal("CCC (within its cap, a gain) was selected")
	}
	bbb, aaa := byID[602], byID[601]
	if bbb.Budget == nil || bbb.Budget.Cap != "per_line+total" || bbb.Budget.ContractsPerLine != 1 || bbb.Budget.ContractsTotal != 3 || bbb.Budget.Order != 1 ||
		bbb.Quantity != 4 || bbb.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("BBB row = qty %d effect %s budget %+v", bbb.Quantity, bbb.PositionEffect, bbb.Budget)
	}
	if aaa.Budget == nil || aaa.Budget.Cap != "per_line+total" || aaa.Budget.ContractsPerLine != 3 || aaa.Budget.ContractsTotal != 1 || aaa.Budget.Order != 2 ||
		aaa.Quantity != 4 || aaa.PositionEffect != rpc.OrderPositionEffectReduce {
		t.Fatalf("AAA row = qty %d effect %s budget %+v", aaa.Quantity, aaa.PositionEffect, aaa.Budget)
	}
	// Every row names the cap, the measured values, the excess and its order.
	if aaa.Budget.LineMarketValueBase != 12000 || math.Abs(aaa.Budget.LinePctOfRiskCapital-24) > 1e-9 || aaa.Budget.LineExcessBase == nil || *aaa.Budget.LineExcessBase != 4500 ||
		aaa.Budget.TotalMeasuredBase != 28000 || aaa.Budget.TotalExcessBase == nil || *aaa.Budget.TotalExcessBase != 18000 || aaa.Budget.DeclaredRiskCapitalBase != 50000 ||
		aaa.Budget.UnrealizedPnLBase == nil || *aaa.Budget.UnrealizedPnLBase != -1000 || aaa.Reason == "" || len(aaa.Details) < 3 {
		t.Fatalf("AAA arithmetic = %+v reason %q details %q", aaa.Budget, aaa.Reason, aaa.Details)
	}
	// Shadow: every row is flagged, blocked with shadow_mode in front, and
	// never automatically eligible; the counts keep them out of actionable.
	for _, row := range rows {
		if !row.Shadow || row.State != rpc.TradeProposalStateBlocked || len(row.Blockers) == 0 || row.Blockers[0].Code != "shadow_mode" || row.AutomaticEligible() {
			t.Fatalf("shadow row not marked: %+v", row)
		}
	}
	counts := proposalCounts(rows, "EUR")
	if counts.Total != 2 || counts.Actionable != 0 || counts.BudgetReduction != 2 || counts.BudgetReductionShadow != 2 {
		t.Fatalf("counts = %+v", counts)
	}

	// Active: the same rows, no blockers, automatically eligible, still never
	// skipping the veto window.
	active := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
	rows, st = engine.budgetReductionProposals(active, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	if st.Shadow || st.Mode != rpc.BudgetReductionModeActive || len(rows) != 2 {
		t.Fatalf("active status = %+v rows %d", st, len(rows))
	}
	for _, row := range rows {
		if row.Shadow || len(row.Blockers) != 0 || row.State != rpc.TradeProposalStateGenerated || !row.AutomaticEligible() || !row.NeverSkipVeto || row.Budget.Mode != rpc.BudgetReductionModeActive {
			t.Fatalf("active row = %+v", row)
		}
	}
	counts = proposalCounts(rows, "EUR")
	if counts.Actionable != 2 || counts.BudgetReduction != 2 || counts.BudgetReductionShadow != 0 {
		t.Fatalf("active counts = %+v", counts)
	}
}

func TestBudgetReductionCapArithmeticInWholeContracts(t *testing.T) {
	engine := &proposalEngine{}
	now := optionExitTestTime()
	// A single line worth more per contract than the per-line cap: the cap
	// rounds to zero contracts to keep, so the row is a full close.
	pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Options: []rpc.PositionView{budgetOptionLeg("DDD", 604, "C", 1, 9000, 0)}}
	rows, st := engine.budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(rows) != 1 || rows[0].Quantity != 1 || rows[0].PositionEffect != rpc.OrderPositionEffectClose || rows[0].Budget.Cap != "per_line" || rows[0].Budget.ContractsPerLine != 1 || st.State != rpc.BudgetStateOverBudget {
		t.Fatalf("zero-contract cap did not close: rows %+v status %+v", rows, st)
	}
	// The total cap alone, with one contract too many: the loss line sells
	// exactly one whole contract even though the excess is a fraction of it.
	pos = &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Options: []rpc.PositionView{
			budgetOptionLeg("EEE", 605, "C", 3, 2000, -50),
			budgetOptionLeg("FFF", 606, "C", 3, 2000, 10),
		}}
	// 12,000 measured against a 22% cap (11,000): 1,000 over, one contract of EEE.
	rows, st = engine.budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 22, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(rows) != 1 || rows[0].Contract.ConID != 605 || rows[0].Quantity != 1 || rows[0].PositionEffect != rpc.OrderPositionEffectReduce ||
		rows[0].Budget.Cap != "total" || rows[0].Budget.ContractsTotal != 1 || rows[0].Budget.Order != 1 || rows[0].Budget.LineExcessBase != nil || st.TotalExcessBase == nil || *st.TotalExcessBase != 1000 {
		t.Fatalf("fractional excess rows %+v status %+v", rows, st)
	}
	// Within both caps: measured, no rows, within_budget.
	rows, st = engine.budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(rows) != 0 || st.State != rpc.BudgetStateWithinBudget || st.MeasuredPremiumBase == nil || *st.MeasuredPremiumBase != 12000 || st.TotalExcessBase != nil {
		t.Fatalf("within budget: rows %d status %+v", len(rows), st)
	}
	// The order notional cap bounds one order like risk_reduction: DDD's one
	// contract is 9,000 USD at mark 90 × 100; a 2,500 cap still allows one.
	// AAA at 20 × 100 = 2,000 per contract: a 2,500 cap holds a 3-contract
	// cut to 1 and the row says so.
	pol := budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15)
	pol.Buckets.BudgetReduction.MaxOrderNotional = 2500
	pos = &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"}, Options: []rpc.PositionView{budgetOptionLeg("AAA", 601, "C", 6, 2000, -1000)}}
	rows, _ = engine.budgetReductionProposals(pol, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(rows) != 1 || rows[0].Quantity != 1 || rows[0].PositionEffect != rpc.OrderPositionEffectReduce || rows[0].Budget.ContractsPerLine != 3 {
		t.Fatalf("notional cap rows %+v", rows)
	}
}

func TestBudgetReductionGatesNameWhyNothingIsGenerated(t *testing.T) {
	engine := &proposalEngine{}
	pos := budgetTestBook()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
	now := optionExitTestTime()
	for name, tc := range map[string]struct {
		input func() budgetGovernorInput
		want  string
	}{
		"no constitution": {func() budgetGovernorInput { return budgetGovernorInput{} }, rpc.BudgetStateConstitutionUnapproved},
		"unapproved key": {func() budgetGovernorInput {
			in := budgetLatchedInput()
			in.Constitution.Capital.DeclaredRiskCapital = nil
			in.Unapproved = in.Constitution.UnapprovedKeys()
			return in
		}, rpc.BudgetStateConstitutionUnapproved},
		"unapproved tier": {func() budgetGovernorInput {
			in := budgetLatchedInput()
			in.Capital.Tier, in.Capital.BlockLatched = risk.CapitalTierUnapproved, false
			return in
		}, rpc.BudgetStateConstitutionUnapproved},
		"shadow enforcement": {func() budgetGovernorInput {
			in := budgetLatchedInput()
			in.Constitution.Drawdown.BlockEnforcement = ""
			return in
		}, rpc.BudgetStateEnforcementShadow},
		"not latched": {func() budgetGovernorInput {
			in := budgetLatchedInput()
			in.Capital.Tier, in.Capital.BlockLatched = risk.CapitalTierWarn, false
			return in
		}, rpc.BudgetStateNotLatched},
		"currency mismatch": {func() budgetGovernorInput {
			in := budgetLatchedInput()
			in.Constitution.Capital.BaseCurrency = "USD"
			return in
		}, rpc.BudgetStateCurrencyMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			rows, st := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, tc.input(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if len(rows) != 0 || st == nil || st.State != tc.want || st.Reason == "" || st.MeasuredPremiumBase != nil {
				t.Fatalf("rows %d status %+v, want state %s", len(rows), st, tc.want)
			}
		})
	}
	// A breached tier without a latch opens the gate too.
	breached := budgetLatchedInput()
	breached.Capital.BlockLatched = false
	if rows, st := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, breached, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); len(rows) == 0 || st.State != rpc.BudgetStateOverBudget {
		t.Fatalf("breached block tier generated nothing: %+v", st)
	}
	// A disabled or absent bucket is silent: no rows, no status.
	for _, disabled := range []*protectionBudgetPolicy{nil, {PremiumAtRiskPctOfRiskCapital: 40, PerLinePctOfRiskCapital: 15}} {
		off := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
		off.Buckets.BudgetReduction = disabled
		if rows, st := engine.budgetReductionProposals(off, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); rows != nil || st != nil {
			t.Fatalf("disabled bucket produced rows %v status %v", rows, st)
		}
	}
	// Legs without a base value are excluded and counted; a book of only such
	// legs is unmeasurable.
	blind := budgetTestBook()
	for i := range blind.Options {
		blind.Options[i].MarketValueBase = nil
	}
	if rows, st := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, blind, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); len(rows) != 0 || st.State != rpc.BudgetStateUnmeasurable || st.ExcludedLegs != 3 {
		t.Fatalf("blind book: rows %d status %+v", len(rows), st)
	}
}

func TestBudgetReductionBlockersAndBookIntegration(t *testing.T) {
	now := optionExitTestTime()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
	pos := budgetTestBook()
	// A stale mark blocks the order; a strategy leg routes to the strategy
	// workflow but is still measured.
	pos.Options[2].Stale = true
	pos.Strategies = []rpc.PositionStrategy{{Source: rpc.PositionStrategySourceCanary, Legs: []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{ConID: 602}, Quantity: 4}}}}
	engine := &proposalEngine{}
	rows, st := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if st.IncludedLegs != 3 || len(rows) != 2 {
		t.Fatalf("rows %d status %+v", len(rows), st)
	}
	byID := budgetRowsByConID(rows)
	if b := byID[601].Blockers; len(b) != 1 || b[0].Code != "fresh_option_quote_required" || b[0].Action == "" {
		t.Fatalf("stale line blockers = %+v", b)
	}
	if b := byID[602].Blockers; len(b) != 1 || b[0].Code != "strategy_workflow_required" || b[0].Action == "" {
		t.Fatalf("strategy leg blockers = %+v", b)
	}
	// Through the book: the governor's rows sit beside the other buckets'
	// output, the status reaches the caller, and a snapshot copy owns them.
	pos = budgetTestBook()
	engine.budgetInput = func(*rpc.AccountResult, time.Time) budgetGovernorInput { return budgetLatchedInput() }
	proposals, _, hedges, status := engine.generateBook(context.Background(), policy, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 2 || status == nil || status.Rows != 2 || len(hedges) != 0 {
		t.Fatalf("book: %d proposals, %d hedges, status %+v", len(proposals), len(hedges), status)
	}
	snap := rpc.TradeProposalSnapshot{Proposals: proposals, BudgetReduction: status}
	copied := cloneProposalSnapshot(snap)
	*copied.BudgetReduction.MeasuredPremiumBase = 0
	*copied.Proposals[0].Budget.TotalExcessBase = 0
	if *snap.BudgetReduction.MeasuredPremiumBase != 28000 || *snap.Proposals[0].Budget.TotalExcessBase != 18000 {
		t.Fatal("snapshot copy shared governor pointers with the original")
	}
	// An engine without the risk-policy manager reads as no constitution.
	if in := (&proposalEngine{server: &Server{}}).budgetGovernorInput(nil, now); in.Constitution != nil {
		t.Fatal("bare server produced a constitution")
	}
	// The journal records the shadow flag on generated events.
	shadow := proposals[0]
	shadow.Shadow = true
	if ev := proposalEventForProposal("generated", shadow, now, "", "", ""); !ev.Shadow || ev.Bucket != rpc.TradeProposalBucketBudgetReduction {
		t.Fatalf("event = %+v", ev)
	}
}

func TestBudgetReductionReevaluatesFromTheCurrentPosition(t *testing.T) {
	// AAA alone: 6 contracts at 2,000 against a 7,500 per-line cap sells 3.
	// After a partial fill leaves 4, the next cycle measures 8,000 and sells
	// 1; at 3 the line is within its cap and no row follows.
	engine := &proposalEngine{}
	now := optionExitTestTime()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15)
	for held, want := range map[float64]int{6: 3, 4: 1, 3: 0} {
		pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"}, Options: []rpc.PositionView{budgetOptionLeg("AAA", 601, "C", held, 2000, -1000)}}
		rows, _ := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
		got := 0
		if len(rows) == 1 {
			got = rows[0].Quantity
		}
		if got != want || len(rows) > 1 {
			t.Fatalf("held %g: rows %+v, want quantity %d", held, rows, want)
		}
		assertBudgetRowsReduceOnly(t, rows, pos)
	}
	// The key is stable across cycles, so an ignore or a journal line follows
	// the line, not the quantity.
	first := budgetOptionLeg("AAA", 601, "C", 6, 2000, -1000)
	second := first
	second.Quantity = 4
	if proposalKey(rpc.TradeProposalBucketBudgetReduction, proposalContractFromPosition(first, "OPT"), rpc.OrderActionSell) !=
		proposalKey(rpc.TradeProposalBucketBudgetReduction, proposalContractFromPosition(second, "OPT"), rpc.OrderActionSell) {
		t.Fatal("proposal key moved with the quantity")
	}
}
