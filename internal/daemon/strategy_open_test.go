package daemon

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// strategyOpenTestDraft is the order the strategy preview builds to open
// units of a two-leg combination from flat, each leg at the given mark.
func strategyOpenTestDraft(symbol string, units int, limit float64, legs ...addLeg) rpc.OrderDraft {
	draft := strategyAddTestDraft(symbol, "USD", units, 0, limit, legs...)
	draft.Strategy = "group-open"
	draft.StrategyGroup.Operation, draft.StrategyGroup.StrategyRevision = rpc.StrategyOperationOpen, 0
	for i := range draft.StrategyGroup.Legs {
		draft.StrategyGroup.Legs[i].Contract.TradingClass = symbol
	}
	return draft
}

// usdTestNotional is an order notional in a US dollar base account.
func usdTestNotional(base float64) orderNotionalAuthority {
	n := protectiveExitTestNotional(base)
	n.ContractCurrency, n.BaseCurrency = "USD", "USD"
	return n
}

func strategyOpenImpact(units int) rpc.OrderPositionImpact {
	return rpc.OrderPositionImpact{Before: 0, After: float64(units), Effect: rpc.OrderPositionEffectOpen}
}

// Owner, 2026-11-24 07:19 CET: "Yes, spreads only". An open is a debit
// vertical and nothing wider: a credit spread, a ratio, a calendar or
// diagonal, mixed rights, a naked sale and a debit at or above the strike
// width are refused with a sentence the owner reads.
func TestStrategyOpenIsADebitVerticalOnly(t *testing.T) {
	cases := []struct {
		name  string
		limit float64
		legs  []addLeg
		want  string // "" passes
	}{
		{"bull call spread", 3.5, []addLeg{{1, "C", 100, "", 1}, {2, "C", 110, "", -1}}, ""},
		{"bear put spread", 4, []addLeg{{1, "P", 600, "", 1}, {2, "P", 570, "", -1}}, ""},
		{"bull put credit spread", 2, []addLeg{{1, "P", 85, "", 1}, {2, "P", 95, "", -1}}, "credit spread"},
		{"bear call credit spread", 2, []addLeg{{1, "C", 110, "", 1}, {2, "C", 100, "", -1}}, "credit spread"},
		{"credit limit", -1, []addLeg{{1, "C", 100, "", 1}, {2, "C", 110, "", -1}}, "credit spread"},
		{"debit at the width", 10, []addLeg{{1, "C", 100, "", 1}, {2, "C", 110, "", -1}}, "cannot make money"},
		{"debit above the width", 12, []addLeg{{1, "P", 600, "", 1}, {2, "P", 590, "", -1}}, "cannot make money"},
		{"ratio", 1, []addLeg{{1, "C", 100, "", 1}, {2, "C", 110, "", -2}}, "ratio"},
		{"calendar", 1, []addLeg{{1, "C", 100, "20270319", 1}, {2, "C", 100, "20270115", -1}}, "calendar"},
		{"diagonal", 1, []addLeg{{1, "C", 100, "20270319", 1}, {2, "C", 110, "20270115", -1}}, "calendar or diagonal"},
		{"risk reversal", 1, []addLeg{{1, "C", 110, "", 1}, {2, "P", 90, "", -1}}, "calls, or both puts"},
		{"long strangle", 1, []addLeg{{1, "P", 90, "", 1}, {2, "C", 110, "", 1}}, "ratio"},
	}
	for _, tc := range cases {
		draft := strategyOpenTestDraft("SYNA", 1, tc.limit, tc.legs...)
		err := validateStrategyOpenVertical(draft.StrategyGroup, draft.LimitPrice)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
	naked := strategyOpenTestDraft("SYNA", 1, 1, addLeg{2, "C", 110, "", -1})
	if err := validateStrategyOpenVertical(naked.StrategyGroup, 1); err == nil {
		t.Error("a naked short call opened as a spread")
	}
}

// The order limits judge an open as a covered add: the cap reads the debit
// at the limit, the sold leg needs no sell-to-open permission, each leg
// stays inside the option-contract cap, and a credit spread is refused even
// with the permission, because an open is a debit vertical or nothing.
func TestStrategyOpenOrderLimitsJudgeTheDebit(t *testing.T) {
	limits := testOrderLimits(10000)
	spread := []addLeg{{11, "C", 100, "", 1}, {12, "C", 110, "", -1}}
	open := strategyOpenTestDraft("SYNA", 4, 3.5, spread...)
	if net := strategyAddNetPremium(open); net != 1400 {
		t.Fatalf("net premium %v, want 1400", net)
	}
	if got := strategyAddCapNotional(open, 9000); got != 1400 {
		t.Fatalf("an open's cap notional = %v, want its debit 1400", got)
	}
	check := func(draft rpc.OrderDraft, units int, notional float64, l risk.OrderLimitsInForce) error {
		return validateOrderRiskAuthority(l, draft, strategyOpenImpact(units), usdTestNotional(notional), "USD", protectiveExitInventory{}, deltaReductionEvidence{})
	}
	if err := check(open, 4, 1400, limits); err != nil {
		t.Fatalf("a debit vertical without the sell-to-open permission: %v", err)
	}
	if err := check(open, 4, 1000, limits); err == nil || !strings.Contains(err.Error(), "below the debit") {
		t.Fatalf("an open signed with less than its debit: %v", err)
	}
	if err := check(strategyOpenTestDraft("SYNA", 4, 3.5, spread...), 4, 12000, limits); err == nil || !strings.Contains(err.Error(), "exceeds the order cap") {
		t.Fatalf("a debit over the order cap: %v", err)
	}
	if err := check(strategyOpenTestDraft("SYNA", 6, 1, spread...), 6, 600, limits); err == nil || !strings.Contains(err.Error(), "max_option_contracts") {
		t.Fatalf("six contracts a leg: %v", err)
	}
	permitted := testOrderLimits(10000, func(o *risk.ConstitutionOrderLimits) { o.AllowOptionSellToOpen = new(true) })
	credit := strategyOpenTestDraft("SYNA", 1, 2, addLeg{21, "P", 85, "", 1}, addLeg{22, "P", 95, "", -1})
	if err := check(credit, 1, 1000, permitted); err == nil || !strings.Contains(err.Error(), "credit spread") {
		t.Fatalf("a credit spread opened with the sell-to-open permission: %v", err)
	}
	// The bought contract may be held already (the extra stays a line of
	// its own); the sold one may not.
	overLong := strategyOpenTestDraft("SYNA", 1, 3.5, spread...)
	overLong.StrategyGroup.Legs[0].Before, overLong.StrategyGroup.Legs[0].After = 3, 4
	if err := check(overLong, 1, 350, limits); err != nil {
		t.Fatalf("an open over three held bought contracts: %v", err)
	}
	overShort := strategyOpenTestDraft("SYNA", 1, 3.5, spread...)
	overShort.StrategyGroup.Legs[1].Before, overShort.StrategyGroup.Legs[1].After = -1, -2
	if err := check(overShort, 1, 350, limits); err == nil || !strings.Contains(err.Error(), "sold option must not be held") {
		t.Fatalf("an open over a held sold contract: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, open, strategyAddImpact(1, 4), usdTestNotional(1400), "USD", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil {
		t.Fatal("an open accepted a position held before")
	}
}

// The open names a group the grouping recognises once held. Other options of
// the name may be held, the bought contract too; the sold one may not be,
// nor the bought one held short.
func TestStrategyOpenGroupIsTheHeldStrategy(t *testing.T) {
	pos := mandateTestUngrouped(100) // short two SYNA 110 calls (9004), no SYNB option
	if _, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open", Open: &rpc.StrategyOpenLegs{Long: rpc.ContractParams{ConID: 9201, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 100, Right: "C"}, Short: rpc.ContractParams{ConID: 9202, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 120, Right: "C"}}}); err != nil {
		t.Fatalf("an open beside another held SYNA option: %v", err)
	}
	if _, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open", Open: &rpc.StrategyOpenLegs{Long: rpc.ContractParams{ConID: 9004, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 110, Right: "C"}, Short: rpc.ContractParams{ConID: 9202, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 120, Right: "C"}}}); err == nil || !strings.Contains(err.Error(), "short the option this spread buys") {
		t.Fatalf("an open buying a contract held short: %v", err)
	}
	if _, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open", Open: &rpc.StrategyOpenLegs{Long: rpc.ContractParams{ConID: 9201, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 100, Right: "C"}, Short: rpc.ContractParams{ConID: 9004, Symbol: "SYNA", SecType: "OPT", Expiry: "20270115", Strike: 110, Right: "C"}}}); err == nil || !strings.Contains(err.Error(), "sells already") {
		t.Fatalf("an open selling a contract held: %v", err)
	}
	if _, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open", StrategyID: "strategy-x", ExpectedRevision: 1, Open: &rpc.StrategyOpenLegs{Long: rpc.ContractParams{ConID: 9201, Symbol: "SYNB", SecType: "OPT", Expiry: "20270115", Strike: 80, Right: "C"}, Short: rpc.ContractParams{ConID: 9202, Symbol: "SYNB", SecType: "OPT", Expiry: "20270115", Strike: 90, Right: "C"}}}); err == nil {
		t.Fatal("an open accepted a strategy id")
	}
	if _, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open"}); err == nil {
		t.Fatal("an open without its legs")
	}
	group, err := strategyOpenGroup(pos, rpc.StrategyPreviewParams{Operation: "open", Open: &rpc.StrategyOpenLegs{Long: rpc.ContractParams{ConID: 9202, Symbol: "SYNB", SecType: "OPT", Expiry: "20270115", Strike: 80, Right: "C"}, Short: rpc.ContractParams{ConID: 9201, Symbol: "SYNB", SecType: "OPT", Expiry: "20270115", Strike: 90, Right: "C"}}})
	if err != nil || group.Units != 0 || len(group.Legs) != 2 || group.Legs[0].Ratio != 1 || group.Legs[1].Ratio != -1 || group.Underlying != "SYNB" {
		t.Fatalf("group %+v, %v", group, err)
	}
	held := []rpc.PositionView{
		{Symbol: "SYNB", SecType: "OPT", ConID: 9202, Currency: "EUR", Quantity: 2, Multiplier: 100, Expiry: "20270115", Strike: 80, Right: "C"},
		{Symbol: "SYNB", SecType: "OPT", ConID: 9201, Currency: "EUR", Quantity: -2, Multiplier: 100, Expiry: "20270115", Strike: 90, Right: "C"},
	}
	inferred, _ := positionstrategy.InferStrategies(nil, held)
	if len(inferred) != 1 || inferred[0].ID != group.ID || inferred[0].Name != "bull call spread" {
		t.Fatalf("the filled spread groups as %+v, want ID %s", inferred, group.ID)
	}
	if op, units, err := normalizeStrategyOperation("open", 2, 0); err != nil || op != rpc.StrategyOperationOpen || units != 2 {
		t.Fatalf("open = %q, %d, %v", op, units, err)
	}
	if _, _, err := normalizeStrategyOperation("open", 0, 0); err == nil {
		t.Fatal("open accepted no units")
	}
}

// The mandate judges an open as an option buy on its debit: a bear put
// spread on a Rulebook hedge symbol is a hedge entry on a book holding none
// of its shares, which the drawdown brake and the mandate leave open; a bull
// call spread adds risk, priced leg by leg at the volatility its preview
// marks imply, the bought leg carrying the debit as its loss and the sold
// leg none.
func TestStrategyOpenIsJudgedAsAnOptionBuy(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return mandateTestPositions(100), nil
	}
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("fixture: %+v", res.Verdict)
	}
	if !srv.rulebookPolicy().IsHedgeSymbol("SPY") {
		t.Fatal("fixture: SPY is not a hedge symbol")
	}
	hedge := strategyOpenTestDraft("SPY", 1, 9, addLeg{9301, "P", 600, "", 1}, addLeg{9302, "P", 570, "", -1})
	if srv.riskAddingOrder(hedge, strategyOpenImpact(1)) || !srv.strategyAddHedgeEntry(hedge) {
		t.Fatal("a SPY bear put spread opened from flat is not a hedge entry")
	}
	if brake, _ := srv.riskPolicyBrake(hedge, strategyOpenImpact(1)); brake != nil {
		t.Fatalf("the brake refuses a hedge entry: %+v", brake)
	}
	if blocker, _ := srv.mandateBudgetJudgement(hedge, strategyOpenImpact(1)); blocker != nil {
		t.Fatalf("the mandate refuses a hedge entry: %+v", blocker)
	}

	calls := strategyOpenTestDraft("SYNA", 1, 4, addLeg{9201, "C", 100, "", 1}, addLeg{9202, "C", 110, "", -1})
	calls.StrategyGroup.Legs[0].Mark, calls.StrategyGroup.Legs[1].Mark = 9, 5
	if !srv.riskAddingOrder(calls, strategyOpenImpact(1)) {
		t.Fatal("a bull call spread reads as a hedge entry")
	}
	if blocker, warning := srv.mandateBudgetJudgement(calls, strategyOpenImpact(1)); blocker != nil || warning != nil && strings.Contains(warning.Message, "cannot be measured") {
		t.Fatalf("a bull call spread inside the mandate: blocker %+v warning %+v, want measured and passed", blocker, warning)
	}
	_, input, _, _ := srv.currentMandateVerdict()
	legs, why := mandateStrategyAddLegs(calls, input)
	fx := input.FXNow["USD"]
	if why != "" || len(legs) != 2 || legs[0].IV == nil || legs[1].IV == nil || legs[0].Quantity != 1 || legs[1].Quantity != -1 ||
		math.Abs(legs[0].LossPerUnitBase-400*fx) > 1e-6 || legs[1].LossPerUnitBase != 0 {
		t.Fatalf("legs %+v why %q fx %v", legs, why, fx)
	}
	unmarked := strategyOpenTestDraft("SYNA", 1, 4, addLeg{9201, "C", 100, "", 1}, addLeg{9202, "C", 110, "", -1})
	if _, why := mandateStrategyAddLegs(unmarked, input); why == "" {
		t.Fatal("an open without preview marks was priced")
	}
}

// The default sold strike follows the rule in force: of the strikes that
// take back enough premium beyond half the expected move, the delta nearest
// half the bought option's; with none qualifying there is no default and a
// reason, never a relaxed pick.
func TestSpreadDefaultShortStrike(t *testing.T) {
	q := func(bid, ask, delta float64) *rpc.SetupOptionQuote {
		return &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: &bid, Ask: &ask, Delta: &delta, IV: new(0.3)}
	}
	leg := func(strike, bid, ask, delta float64) rpc.AddCallCandidate {
		return rpc.AddCallCandidate{Contract: &rpc.ContractParams{Symbol: "SYNC", SecType: "OPT", Right: "C", Strike: strike, Multiplier: 100}, Quote: q(bid, ask, delta)}
	}
	build := func(rule rpc.StrategySpreadRule) *rpc.StrategySpreadsResult {
		out := &rpc.StrategySpreadsResult{Spot: new(100.0), DTE: 60, Rule: rule, Default: -1, Long: leg(100, 5.9, 6.1, 0.55)}
		priceSpreads(out, []rpc.AddCallCandidate{leg(105, 3.4, 3.6, 0.40), leg(110, 1.9, 2.1, 0.27), leg(115, 0.9, 1.1, 0.15), leg(120, 0.3, 0.5, 0.07)})
		return out
	}
	out := build(rpc.StrategySpreadRule{ShortDeltaRatio: 0.5, MinSaving: 0.25, CapExpectedMoves: 0.5})
	// One sigma at 30% over 60 days is 12.16; half of it puts the cap at 106.08 or beyond.
	if out.ExpectedMove == nil || math.Abs(*out.ExpectedMove-12.16) > 0.01 || out.Default < 0 {
		t.Fatalf("result %+v", out)
	}
	pick := out.Candidates[out.Default]
	if pick.Short.Contract.Strike != 110 || *pick.NetMid != 4 || *pick.MaxLossPerUnit != 400 || *pick.MaxGainPerUnit != 600 || *pick.Breakeven != 104 || *pick.NetBid != 3.8 || *pick.NetAsk != 4.2 {
		t.Fatalf("default %+v", pick)
	}
	if out.Candidates[0].Eligible {
		t.Fatal("a sold strike inside half the expected move is eligible")
	}
	none := build(rpc.StrategySpreadRule{ShortDeltaRatio: 0.5, MinSaving: 0.9, CapExpectedMoves: 0.5})
	if none.Default != -1 || !strings.Contains(none.DefaultReason, "90%") {
		t.Fatalf("no qualifying strike: default %d reason %q", none.Default, none.DefaultReason)
	}
}

// On a book over its loss room, where every addition is refused, a SPY bear
// put spread opened from flat stays open as the single hedge put does.
func TestStrategyOpenHedgeEntryOverTheLossRoom(t *testing.T) {
	srv, res := mandateTestOverLossServer(t)
	if res.Gate == nil || !res.Gate.RefusesAdditions {
		t.Fatalf("fixture: gate %+v", res.Gate)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil {
		t.Fatal("fixture: a stock buy passes over the loss room")
	}
	hedge := strategyOpenTestDraft("SPY", 1, 9, addLeg{9301, "P", 600, "", 1}, addLeg{9302, "P", 570, "", -1})
	if blocker, warning := srv.mandateBudgetJudgement(hedge, strategyOpenImpact(1)); blocker != nil || warning != nil {
		t.Fatalf("a SPY bear put spread over the loss room: blocker %+v warning %+v", blocker, warning)
	}
	if brake, _ := srv.riskPolicyBrake(hedge, strategyOpenImpact(1)); brake != nil {
		t.Fatalf("the brake refuses a hedge entry: %+v", brake)
	}
}

// The policy template writes the sold-strike rule, and a file carrying it
// is the rule in force; a file without it serves Canary's default, marked.
func TestAddSpreadRuleSource(t *testing.T) {
	data := OpportunityPolicyTemplate("v0.0.0-test")
	p, err := parseOpportunityPolicy(data)
	if err != nil || p.Add.Spread == nil || *p.Add.Spread != defaultAddSpreadPolicy() {
		t.Fatalf("template spread %+v, %v", p.Add.Spread, err)
	}
	if got := (*Server)(nil).addSpreadRule(); got.Source != rpc.StrategySpreadRuleDefault || got.ShortDeltaRatio != 0.5 {
		t.Fatalf("no policy: %+v", got)
	}
	if err := validateAddSpreadPolicy(addSpreadPolicy{ShortDeltaRatio: 1.2}); err == nil {
		t.Fatal("a ratio of 1.2 passed")
	}
}

// A spread a Canary open recorded is listed as that spread on a name that
// holds other options, and over a bought contract the book held already;
// what the book holds beyond it stays a line of its own, and a sold leg the
// bought one no longer covers is for review.
func TestOpenedSpreadIsGroupedBesideOtherOptions(t *testing.T) {
	row := func(conID int, qty, strike float64, expiry string) rpc.PositionView {
		return rpc.PositionView{Symbol: "SYNA", SecType: "OPT", ConID: conID, Currency: "USD", Exchange: "SMART", Quantity: qty, Multiplier: 100, Expiry: expiry, Strike: strike, Right: "C", TradingClass: "SYNA"}
	}
	open := strategyOpenTestDraft("SYNA", 1, 9, addLeg{465, "C", 465, "20270115", 1}, addLeg{500, "C", 500, "20270115", -1})
	open.StrategyGroup.StrategyID = positionstrategy.StrategyID("SYNA", 465, 500)
	srv := &Server{orderJournal: &orderJournalStore{}, strategyLineageLoaded: true,
		strategyLineage: map[string]rpc.StrategyOrderDraft{open.StrategyGroup.StrategyID: *open.StrategyGroup}}
	list := func(options []rpc.PositionView) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
		groups, issues := positionstrategy.InferStrategies(nil, options)
		return srv.reconcileJournaledStrategyGroups(nil, options, groups, issues)
	}
	spread := func(groups []rpc.PositionStrategy) *rpc.PositionStrategy {
		for i := range groups {
			if groups[i].ID == open.StrategyGroup.StrategyID {
				return &groups[i]
			}
		}
		return nil
	}

	// Beside a 450 call of another expiry: three legs the grouping alone
	// cannot read.
	beside := []rpc.PositionView{row(450, 1, 450, "20261218"), row(465, 1, 465, "20270115"), row(500, -1, 500, "20270115")}
	groups, issues := list(beside)
	if g := spread(groups); g == nil || g.Units != 1 || g.Name != "bull call spread" || !g.Actionable || g.Source != rpc.PositionStrategySourceCanary || g.Revision < 1 || len(issues) != 0 {
		t.Fatalf("beside another call: groups %+v issues %+v", groups, issues)
	}

	// Over three held 465 calls: the spread is one unit, the other three
	// calls are a line of their own.
	over := []rpc.PositionView{row(465, 4, 465, "20270115"), row(500, -1, 500, "20270115")}
	groups, issues = list(over)
	if g := spread(groups); g == nil || g.Units != 1 || g.Legs[0].Quantity != 1 || g.Legs[1].Quantity != -1 || len(groups) != 1 || len(issues) != 0 {
		t.Fatalf("over held calls: groups %+v issues %+v", groups, issues)
	}

	// The 465 calls sold down below the sold 500: the spread is for review.
	uncovered := []rpc.PositionView{row(500, -1, 500, "20270115")}
	groups, _ = list(uncovered)
	if g := spread(groups); g == nil || g.Status != rpc.PositionStrategyStatusReview || g.Actionable {
		t.Fatalf("a sold leg left uncovered: %+v", groups)
	}

	// Nothing sold any more: the spread is closed, the calls a line.
	groups, _ = list([]rpc.PositionView{row(465, 3, 465, "20270115")})
	if spread(groups) != nil {
		t.Fatalf("a closed spread is listed: %+v", groups)
	}
}

// A combination placed elsewhere reads as one, with each leg's side in the
// order: a sold combination sells the legs the broker states as bought.
func TestBrokerOrderCombinationSides(t *testing.T) {
	legs := []ibkr.ComboLeg{{ConID: 11, Ratio: 1, Action: "BUY", Exchange: "SMART"}, {ConID: 12, Ratio: 1, Action: "SELL", Exchange: "SMART"}}
	sold := brokerOrderCombination(ibkr.OrderLifecycleEvent{SecType: "BAG", Action: "SELL", ComboLegs: legs})
	if sold == nil || sold.Source != rpc.OrderCombinationBroker || len(sold.Legs) != 2 || sold.Legs[0].Action != "SELL" || sold.Legs[1].Action != "BUY" || sold.Legs[0].ConID != 11 {
		t.Fatalf("a sold combination: %+v", sold)
	}
	if bought := brokerOrderCombination(ibkr.OrderLifecycleEvent{SecType: "BAG", Action: "BUY", ComboLegs: legs}); bought.Legs[0].Action != "BUY" {
		t.Fatalf("a bought combination: %+v", bought)
	}
	if brokerOrderCombination(ibkr.OrderLifecycleEvent{SecType: "OPT"}) != nil {
		t.Fatal("a single option reads as a combination")
	}
	close := strategyOpenTestDraft("SYNC", 2, 1, addLeg{1, "C", 100, "", 1}, addLeg{2, "C", 110, "", -1})
	close.StrategyGroup.Operation = rpc.StrategyOperationClose
	close.StrategyGroup.Legs[0].Action, close.StrategyGroup.Legs[1].Action = "SELL", "BUY"
	if c := orderCombinationFromGroup(close.StrategyGroup); c == nil || c.Strategy != "bull_call_spread" || c.Operation != "close" || c.Units != 2 || c.Legs[0].Action != "SELL" || c.Legs[0].Ratio != 1 {
		t.Fatalf("a close of a bull call spread: %+v", c)
	}
}
