package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// addLeg is one leg of a synthetic strategy add: ratio is signed as held.
type addLeg struct {
	conID  int
	right  string
	strike float64
	expiry string
	ratio  int
}

// strategyAddTestDraft is the order the strategy preview builds for an add
// of units to held units of a strategy with the given legs.
func strategyAddTestDraft(symbol, currency string, units, held int, limit float64, legs ...addLeg) rpc.OrderDraft {
	group := &rpc.StrategyOrderDraft{StrategyID: "strat-test-1", StrategyRevision: 1, Operation: rpc.StrategyOperationAdd,
		Units: units, UnitsBefore: held, UnitsAfter: held + units, GuaranteedCombo: true}
	for _, l := range legs {
		expiry := l.expiry
		if expiry == "" {
			expiry = "20270115"
		}
		action, quantity := rpc.OrderActionBuy, absOrderRatio(l.ratio)*units
		before := float64(l.ratio * held)
		after := before + float64(quantity)
		if l.ratio < 0 {
			action, after = rpc.OrderActionSell, before-float64(quantity)
		}
		group.Legs = append(group.Legs, rpc.StrategyOrderLeg{
			Contract: rpc.ContractParams{ConID: l.conID, Symbol: symbol, SecType: "OPT", Currency: currency, Right: l.right, Strike: l.strike, Expiry: expiry, Multiplier: 100, Exchange: "SMART"},
			Ratio:    l.ratio, Action: action, Quantity: quantity, Before: before, After: after})
	}
	return rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{Symbol: symbol, SecType: "BAG", Exchange: "SMART", Currency: currency},
		Quantity: units, OrderType: rpc.OrderTypeLMT, LimitPrice: limit, TIF: rpc.OrderTIFDay, Strategy: "group-add", OpenClose: "O", OrderRef: "ref-add", StrategyGroup: group}
}

func strategyAddImpact(held, units int) rpc.OrderPositionImpact {
	return rpc.OrderPositionImpact{Before: float64(held), After: float64(held + units), Effect: rpc.OrderPositionEffectIncrease}
}

// A combination is covered by the Rulebook's spread rule, the one that
// counts a held spread's premium at risk: every sold contract capped by a
// bought contract of the same kind, expiry and size nearer the money.
func TestStrategyAddCoveredFollowsTheSpreadRule(t *testing.T) {
	cases := []struct {
		name string
		legs []addLeg
		want bool
	}{
		{"bear put spread", []addLeg{{1, "P", 145, "", 1}, {2, "P", 135, "", -1}}, true},
		{"bull call spread", []addLeg{{1, "C", 100, "", 1}, {2, "C", 110, "", -1}}, true},
		{"call backspread", []addLeg{{1, "C", 100, "", 2}, {2, "C", 110, "", -1}}, true},
		{"bull put credit spread", []addLeg{{1, "P", 85, "", 1}, {2, "P", 95, "", -1}}, false},
		{"bear call credit spread", []addLeg{{1, "C", 110, "", 1}, {2, "C", 100, "", -1}}, false},
		{"ratio spread", []addLeg{{1, "P", 145, "", 1}, {2, "P", 135, "", -2}}, false},
		{"calendar", []addLeg{{1, "C", 100, "20270319", 1}, {2, "C", 100, "20270115", -1}}, false},
		{"diagonal", []addLeg{{1, "P", 145, "20270319", 1}, {2, "P", 135, "20270115", -1}}, false},
		{"long call butterfly", []addLeg{{1, "C", 90, "", 1}, {2, "C", 100, "", -2}, {3, "C", 110, "", 1}}, false},
		{"long strangle", []addLeg{{1, "P", 90, "", 1}, {2, "C", 110, "", 1}}, true},
		{"short strangle", []addLeg{{1, "P", 90, "", -1}, {2, "C", 110, "", -1}}, false},
		{"risk reversal", []addLeg{{1, "C", 110, "", 1}, {2, "P", 90, "", -1}}, false},
	}
	for _, tc := range cases {
		draft := strategyAddTestDraft("SYNA", "USD", 1, 2, 1, tc.legs...)
		if got := strategyAddCovered(draft.StrategyGroup); got != tc.want {
			t.Errorf("%s: covered = %v, want %v", tc.name, got, tc.want)
		}
	}
	if strategyAddCovered(nil) {
		t.Error("no group read as covered")
	}
}

// Owner, 2026-11-22: the strategy is added as one order and judged as one.
// A covered add needs no sell-to-open permission and its cap reads the net
// premium at the limit; an add that sells an option nothing in the order
// caps needs the permission; the per-leg contract cap holds for both.
func TestStrategyAddOrderLimitsJudgeTheWholeOrder(t *testing.T) {
	limits := testOrderLimits(10000)
	spread := []addLeg{{11, "P", 145, "", 1}, {12, "P", 135, "", -1}}
	covered := strategyAddTestDraft("SYNB", "EUR", 5, 5, 4, spread...)
	// 5 units at 4.00: 2,000 EUR can be paid, whatever the legs gross.
	if net := strategyAddNetPremium(covered); net != 2000 {
		t.Fatalf("net premium %v, want 2000", net)
	}
	if got := strategyAddCapNotional(covered, 12000); got != 2000 {
		t.Fatalf("a covered add's cap notional = %v, want its net premium 2000", got)
	}
	check := func(draft rpc.OrderDraft, held, units int, notional float64, l risk.OrderLimitsInForce) error {
		return validateOrderRiskAuthority(l, draft, strategyAddImpact(held, units), protectiveExitTestNotional(notional), "EUR", protectiveExitInventory{}, deltaReductionEvidence{})
	}
	if err := check(covered, 5, 5, 2000, limits); err != nil {
		t.Fatalf("a covered add without the sell-to-open permission: %v", err)
	}
	if err := check(covered, 5, 5, 1000, limits); err == nil || !strings.Contains(err.Error(), "below the premium its limit can pay") {
		t.Fatalf("a covered add signed with less than it can pay: %v", err)
	}
	if err := check(covered, 5, 5, 12000, limits); err == nil || !strings.Contains(err.Error(), "exceeds the order cap") {
		t.Fatalf("a notional over the order cap: %v", err)
	}
	// One unit more puts a leg past the 5-contract cap; nothing lifts it.
	if err := check(strategyAddTestDraft("SYNB", "EUR", 6, 5, 1, spread...), 5, 6, 600, limits); err == nil || !strings.Contains(err.Error(), "proportional option quantity limit") {
		t.Fatalf("six contracts a leg: %v", err)
	}

	// A credit spread sells an option nearer the money than the one bought:
	// nothing in the order caps it, so it is an option sold to open.
	credit := strategyAddTestDraft("SYNB", "EUR", 2, 2, -3, addLeg{21, "P", 85, "", 1}, addLeg{22, "P", 95, "", -1})
	if got := strategyAddCapNotional(credit, 1800); got != 1800 {
		t.Fatalf("a credit add's cap notional = %v, want the gross premium of its legs", got)
	}
	// An uncovered add that pays (a calendar) is capped on the larger of its
	// legs' gross premium and what its limit can pay.
	calendar := strategyAddTestDraft("SYNB", "EUR", 2, 2, 30, addLeg{31, "C", 100, "20270319", 1}, addLeg{32, "C", 100, "20270115", -1})
	if got := strategyAddCapNotional(calendar, 1800); got != 6000 {
		t.Fatalf("a calendar's cap notional = %v, want the 6000 its limit can pay", got)
	}
	if err := check(credit, 2, 2, 1800, limits); err == nil || !strings.Contains(err.Error(), "allow_option_sell_to_open") {
		t.Fatalf("a credit add without the permission: %v", err)
	}
	permitted := testOrderLimits(10000, func(o *risk.ConstitutionOrderLimits) { o.AllowOptionSellToOpen = new(true) })
	if err := check(credit, 2, 2, 1800, permitted); err != nil {
		t.Fatalf("a credit add with the permission: %v", err)
	}

	// The order is an add of the held strategy or nothing: a leg the book
	// does not hold in that direction, a sold parent, a held count that does
	// not match the preview.
	flipped := strategyAddTestDraft("SYNB", "EUR", 1, 2, 1, spread...)
	flipped.StrategyGroup.Legs[1].Before, flipped.StrategyGroup.Legs[1].After = 2, 1
	if err := check(flipped, 2, 1, 100, limits); err == nil {
		t.Fatal("an add accepted a leg held the other way")
	}
	sold := strategyAddTestDraft("SYNB", "EUR", 1, 2, 1, spread...)
	sold.Action = rpc.OrderActionSell
	if err := check(sold, 2, 1, 100, limits); err == nil {
		t.Fatal("an add accepted a sold combination")
	}
	if err := check(strategyAddTestDraft("SYNB", "EUR", 1, 2, 1, spread...), 3, 1, 100, limits); err == nil {
		t.Fatal("an add accepted a position other than the previewed one")
	}
}

// An add opens every leg at the broker and is bought; a close or reduction
// closes every leg and is sold.
func TestStrategyAddOpensEveryLeg(t *testing.T) {
	op, units, err := normalizeStrategyOperation(rpc.StrategyOperationAdd, 4, 3)
	if err != nil || op != rpc.StrategyOperationAdd || units != 4 {
		t.Fatalf("add = %q, %d, %v", op, units, err)
	}
	if _, _, err := normalizeStrategyOperation(rpc.StrategyOperationAdd, 0, 3); err == nil {
		t.Fatal("add accepted no units")
	}
	add := strategyAddTestDraft("SYNA", "USD", 1, 2, 1, addLeg{11, "P", 145, "", 1}, addLeg{12, "P", 135, "", -1})
	contract := previewIBKRStrategyContract(add)
	if len(contract.ComboLegs) != 2 || contract.ComboLegs[0].OpenClose != 1 || contract.ComboLegs[1].OpenClose != 1 ||
		contract.ComboLegs[0].Action != rpc.OrderActionBuy || contract.ComboLegs[1].Action != rpc.OrderActionSell {
		t.Fatalf("add legs %+v", contract.ComboLegs)
	}
	reduce := add
	group := *add.StrategyGroup
	group.Operation = rpc.StrategyOperationReduce
	reduce.StrategyGroup = &group
	for _, leg := range previewIBKRStrategyContract(reduce).ComboLegs {
		if leg.OpenClose != 2 {
			t.Fatalf("a reduction's leg opens: %+v", leg)
		}
	}
}

const (
	addHedgeLongPut  = 9101
	addHedgeShortPut = 9102
	addCreditShort   = 9103
	addCreditLong    = 9104
	addCallShort     = 9108
)

// strategyAddBook adds three held strategies to a stock fixture: a put
// debit spread on the Rulebook's hedge symbol, a put credit spread on SYNA
// and, where the fixture holds the SYNB 80 calls, the 90 calls sold against
// them.
func strategyAddBook(srv *Server, pos *rpc.PositionsResult) *rpc.PositionsResult {
	iv, fx := 0.3, 0.9
	hedge := srv.rulebookPolicy().HedgeSymbols[0]
	hmv := 2 * 100 * 30.0 * fx
	cmv := 2 * 100 * 2.0 * fx
	pos.Options = append(pos.Options,
		rpc.PositionView{Symbol: hedge, SecType: rpc.SecTypeOption, ConID: addHedgeLongPut, Currency: "USD", Quantity: 2, Multiplier: 100, Mark: 30, AvgCost: 3500, FXRate: &fx,
			MarketValue: 2 * 100 * 30, MarketValueBase: &hmv, Expiry: "20270115", Strike: 500, Right: "P", IV: &iv, Underlying: new(560.0),
			Delta: new(-0.3), Gamma: new(0.01), Theta: new(-0.1), Vega: new(0.5)},
		rpc.PositionView{Symbol: hedge, SecType: rpc.SecTypeOption, ConID: addHedgeShortPut, Currency: "USD", Quantity: -2, Multiplier: 100, Mark: 12, Expiry: "20270115", Strike: 450, Right: "P", IV: &iv, Underlying: new(560.0), FXRate: &fx},
		rpc.PositionView{Symbol: "SYNA", SecType: rpc.SecTypeOption, ConID: addCreditShort, Currency: "USD", Quantity: -2, Multiplier: 100, Mark: 5, Expiry: "20270115", Strike: 95, Right: "P", IV: &iv, Underlying: new(100.0), FXRate: &fx},
		rpc.PositionView{Symbol: "SYNA", SecType: rpc.SecTypeOption, ConID: addCreditLong, Currency: "USD", Quantity: 2, Multiplier: 100, Mark: 2, AvgCost: 250, FXRate: &fx,
			MarketValue: 2 * 100 * 2, MarketValueBase: &cmv, Expiry: "20270115", Strike: 85, Right: "P", IV: &iv, Underlying: new(100.0),
			Delta: new(-0.2), Gamma: new(0.01), Theta: new(-0.02), Vega: new(0.2)})
	for _, o := range pos.Options {
		if o.ConID == 9005 {
			pos.Options = append(pos.Options, rpc.PositionView{Symbol: "SYNB", SecType: rpc.SecTypeOption, ConID: addCallShort, Currency: "EUR", Quantity: -o.Quantity, Multiplier: 100, Mark: 15,
				Expiry: "20270115", Strike: 90, Right: "C", IV: &iv, Underlying: new(80.0)})
		}
	}
	return mandateTestGrouped(pos)
}

func strategyAddHedgeSpread(srv *Server, units int, limit float64) rpc.OrderDraft {
	return strategyAddTestDraft(srv.rulebookPolicy().HedgeSymbols[0], "USD", units, 2, limit, addLeg{addHedgeLongPut, "P", 500, "", 1}, addLeg{addHedgeShortPut, "P", 450, "", -1})
}

func strategyAddCreditSpread(units int) rpc.OrderDraft {
	return strategyAddTestDraft("SYNA", "USD", units, 2, -3, addLeg{addCreditLong, "P", 85, "", 1}, addLeg{addCreditShort, "P", 95, "", -1})
}

// The owner's case (2026-11-22 12:13 CET): "If the combined order (the
// strategy) adds protection, but by selling the put I reduce the price, it
// should not be blocked." On a book over its loss room, where every addition
// is refused, units added to a put spread on the hedge symbol stay open as
// the single put does; a credit spread added there is refused as any
// addition is; and a covered spread off the hedge list is judged by the
// premium limits on its net premium, with the verdict as its warning.
func TestStrategyAddIsJudgedAsOneOrder(t *testing.T) {
	c := testMandateConstitution()
	c.Mandate.GapRoomPct = new(100.0)
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return strategyAddBook(srv, mandateTestPositionsOverLoss()), nil
	}
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusReduce || res.Gate == nil || !res.Gate.RefusesAdditions {
		t.Fatalf("fixture: %+v gate %+v", res.Verdict, res.Gate)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil {
		t.Fatal("fixture: a stock buy passes on a book over its loss room")
	}

	hedge := strategyAddHedgeSpread(srv, 1, 18)
	if srv.riskAddingOrder(hedge, strategyAddImpact(2, 1)) {
		t.Fatal("a put spread added on the hedge symbol reads as adding risk")
	}
	if blocker, warning := srv.mandateBudgetJudgement(hedge, strategyAddImpact(2, 1)); blocker != nil || warning != nil {
		t.Fatalf("a put spread added on the hedge symbol: blocker %+v warning %+v", blocker, warning)
	}
	if brake, _ := srv.riskPolicyBrake(hedge, strategyAddImpact(2, 1)); brake != nil {
		t.Fatalf("the brake refuses a hedge entry: %+v", brake)
	}

	credit := strategyAddCreditSpread(1)
	if !srv.riskAddingOrder(credit, strategyAddImpact(2, 1)) {
		t.Fatal("a credit spread reads as a hedge entry")
	}
	if blocker := srv.mandateBudgetGate(credit, strategyAddImpact(2, 1)); blocker == nil || blocker.Code != mandateBudgetCode {
		t.Fatalf("a credit spread added over the loss room: %+v, want mandate_budget", blocker)
	}
	if err := srv.mandateBudgetGateError(credit, strategyAddImpact(2, 1)); err == nil {
		t.Fatal("admission passes a credit spread added over the loss room")
	}

	// SYNB call spread, off the hedge list: 100 EUR of net premium passes
	// with the verdict as its warning; 54,000 EUR is past the 45,000 EUR
	// limit for one name, whatever the name holds.
	calls := func(units int, limit float64) rpc.OrderDraft {
		return strategyAddTestDraft("SYNB", "EUR", units, 18, limit, addLeg{9005, "C", 80, "", 1}, addLeg{addCallShort, "C", 90, "", -1})
	}
	if !srv.riskAddingOrder(calls(1, 1), strategyAddImpact(18, 1)) {
		t.Fatal("a call spread reads as a hedge entry")
	}
	blocker, warning := srv.mandateBudgetJudgement(calls(1, 1), strategyAddImpact(18, 1))
	if blocker != nil || warning == nil || warning.Code != mandateBudgetCode {
		t.Fatalf("a covered call spread over the loss room: blocker %+v warning %+v, want passed with the verdict as its warning", blocker, warning)
	}
	blocker, _ = srv.mandateBudgetJudgement(calls(60, 9), strategyAddImpact(18, 60))
	if blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || !strings.Contains(blocker.Message, "premium at risk in SYNB") {
		t.Fatalf("54,000 EUR of net premium on one name: %+v, want mandate_premium", blocker)
	}
}

// An add the mandate cannot measure is refused, never passed: before the
// add existed a combination reached the gate only as a reduction, and one
// the evaluator could not price passed unmeasured.
func TestStrategyAddIsNeverPassedUnmeasured(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return strategyAddBook(srv, mandateTestUngrouped(100)), nil
	}
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("fixture: %+v", res.Verdict)
	}
	// Measured with both legs on the book, one unit of the credit spread
	// stays inside the mandate.
	if blocker := srv.mandateBudgetGate(strategyAddCreditSpread(1), strategyAddImpact(2, 1)); blocker != nil {
		t.Fatalf("a measured credit spread inside the mandate: %+v", blocker)
	}
	// A leg the kept book does not carry cannot be priced.
	unknown := strategyAddTestDraft("SYNA", "USD", 1, 2, -3, addLeg{addCreditLong, "P", 85, "", 1}, addLeg{9999, "P", 95, "", -1})
	blocker := srv.mandateBudgetGate(unknown, strategyAddImpact(2, 1))
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "cannot be measured") {
		t.Fatalf("an add with a leg the book cannot price: %+v, want mandate_budget", blocker)
	}
	_, input, _, _ := srv.currentMandateVerdict()
	legs, why := mandateStrategyAddLegs(strategyAddCreditSpread(3), input)
	if why != "" || len(legs) != 2 || legs[0].Quantity != 3 || legs[1].Quantity != -3 || legs[0].IV == nil || legs[1].IV == nil {
		t.Fatalf("legs %+v why %q", legs, why)
	}
}
