package daemon

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Adding units of a held strategy (owner decisions 2026-11-22: "When adding,
// we add the strategy", and 12:13 CET: "If the combined order (the strategy)
// adds protection, but by selling the put I reduce the price, it should not
// be blocked").
//
// The order is one guaranteed combination that opens every leg in the held
// ratio, and every gate judges it once, as that order, never leg by leg:
//
//   - A covered combination (strategyAddCovered) can lose what is paid for
//     it and no more. The order cap reads the net premium at the limit, the
//     sold legs need no sell-to-open permission, and the mandate judges it
//     as it judges an option buy, on the net premium.
//   - A covered put combination on a Rulebook hedge symbol, or one whose
//     bought puts protect shares the book holds, is a hedge entry: the
//     drawdown brake and the mandate leave it open as they leave the single
//     put open.
//   - Every other combination (a credit, a ratio, a sold leg nothing in the
//     order caps) adds risk like any addition: the order cap reads the gross
//     premium of its legs, the sold legs need
//     [order_limits].allow_option_sell_to_open, the brake applies, and the
//     mandate measures the book once with every leg added and refuses what
//     it cannot measure. Its bought contracts also meet the premium limits,
//     at the held contract's value (strategyAddBoughtPremiumBase): once held
//     they count in the premium at risk in full.
//
// What still bounds a covered add now that its sold legs pass without the
// permission and its cap reads the net premium: the loss is the premium
// paid, which the order cap bounds per order and, unless it is a hedge
// entry, the premium limits and the mandate's hard holds bound across the
// book; each leg stays inside [order_limits].max_option_contracts; the
// broker margins and checks the combination as one order; and it is sent
// only from a preview a person confirmed at the command line. No proposal,
// no pre-authorised bucket and no app route sends an add.
//
// A hedge entry meets the order cap and the contract cap only, each per
// order, as the single hedge put does: nothing sums the premium of repeated
// hedge entries at the gate. Rule 2's hedge tier and rule 12 report on the
// held hedge afterwards; they do not refuse an order.
//
// Opening a debit vertical from flat (operation open) is an add from zero
// units, judged by the same rule and nothing wider. Owner, 2026-11-24 07:00
// CET: buy an option as a defined-risk debit vertical in one order (a bull
// call spread, a bear put spread); 07:19 CET, asked "May Desk sell the short
// leg of a defined-risk spread, sent as one combination order?": "Yes,
// spreads only". So an open is exactly one bought and one sold option of
// one underlying, expiry, right, contract size and trading class, 1:1, the
// sold strike further out of the money, bought for a net debit above zero
// and below the strike width (validateStrategyOpenVertical). Its sold leg
// needs no sell-to-open permission because the order caps it; a credit
// spread, a ratio, a calendar or diagonal and a naked sale are refused
// whatever [order_limits].allow_option_sell_to_open says. What it can lose
// is the debit paid, bounded as a covered add's is above.

// strategyAdd returns the group of an order that adds units of a held
// strategy or opens a debit vertical from flat, or nil for every other
// order.
func strategyAdd(draft rpc.OrderDraft) *rpc.StrategyOrderDraft {
	if g := draft.StrategyGroup; g != nil && (g.Operation == rpc.StrategyOperationAdd || g.Operation == rpc.StrategyOperationOpen) {
		return g
	}
	return nil
}

// validateStrategyOpenVertical holds an open to a debit vertical: two
// distinct exact USD SMART options of one underlying, expiry, right,
// contract size and trading class, one bought and one sold in equal
// numbers, the sold strike above the bought for calls and below it for
// puts, bought at a limit above zero and below the strike width. The
// sentence it returns is the refusal the owner reads.
func validateStrategyOpenVertical(g *rpc.StrategyOrderDraft, limit float64) error {
	if g == nil || len(g.Legs) != 2 {
		return fmt.Errorf("a spread is opened as one bought and one sold option")
	}
	var long, short *rpc.StrategyOrderLeg
	for i := range g.Legs {
		switch g.Legs[i].Ratio {
		case 1:
			long = &g.Legs[i]
		case -1:
			short = &g.Legs[i]
		}
	}
	if long == nil || short == nil {
		return fmt.Errorf("a spread is opened one sold option for each one bought; Canary opens no ratio")
	}
	l, s := long.Contract, short.Contract
	right := strings.ToUpper(strings.TrimSpace(l.Right))
	for _, c := range []rpc.ContractParams{l, s} {
		if !strings.EqualFold(c.SecType, "OPT") || c.ConID <= 0 || !strings.EqualFold(c.Currency, "USD") || !strings.EqualFold(c.Exchange, "SMART") || contractMultiplier(c) != 100 || !(c.Strike > 0) {
			return fmt.Errorf("both legs of a spread must be exact US dollar options of 100 shares on SMART")
		}
	}
	if l.ConID == s.ConID {
		return fmt.Errorf("a spread buys and sells two different options")
	}
	if !strings.EqualFold(strings.TrimSpace(l.Symbol), strings.TrimSpace(s.Symbol)) || !strings.EqualFold(strings.TrimSpace(l.TradingClass), strings.TrimSpace(s.TradingClass)) {
		return fmt.Errorf("both legs of a spread must be options on the same underlying and trading class")
	}
	if strings.TrimSpace(l.Expiry) == "" || strings.TrimSpace(l.Expiry) != strings.TrimSpace(s.Expiry) {
		return fmt.Errorf("both legs of a spread must share one expiry; Canary opens no calendar or diagonal")
	}
	if (right != "C" && right != "P") || !strings.EqualFold(right, strings.TrimSpace(s.Right)) {
		return fmt.Errorf("both legs of a spread must be calls, or both puts")
	}
	if right == "C" && !(s.Strike > l.Strike) || right == "P" && !(s.Strike < l.Strike) {
		if right == "C" {
			return fmt.Errorf("a bull call spread sells the higher strike; selling the lower one is a credit spread, which Canary does not open")
		}
		return fmt.Errorf("a bear put spread sells the lower strike; selling the higher one is a credit spread, which Canary does not open")
	}
	if g.Units <= 0 || long.Quantity != g.Units || short.Quantity != g.Units || long.Action != rpc.OrderActionBuy || short.Action != rpc.OrderActionSell {
		return fmt.Errorf("a spread buys and sells the same number of contracts")
	}
	width := math.Abs(l.Strike - s.Strike)
	if !(limit > 0) {
		return fmt.Errorf("a spread is opened for a net debit above zero; one taken in for nothing or a credit is a credit spread, which Canary does not open")
	}
	if limit >= width-1e-9 {
		return fmt.Errorf("a debit of %.2f at or above the strike width of %.2f cannot make money; lower the limit or widen the spread", limit, width)
	}
	if !strategyAddCovered(g) {
		return rpc.Defect(fmt.Errorf("a validated debit vertical is not covered"))
	}
	return nil
}

// strategyAddCovered reports a combination whose every sold contract is
// capped by a bought contract of the same order, by the Rulebook's spread
// rule (risk.PremiumAtRiskLegs): the same kind, expiry day, contract size
// and trading class (two classes of one index settle on different prices),
// the sold strike further out of the money, each bought contract
// capping one sold contract. Such a combination can lose the premium paid
// and no more, and once held its premium at risk is counted at the same net.
// A calendar, a ratio's extra sold contract and a sold leg nearer the money
// are not covered: the rule may refuse a bounded shape (a butterfly's second
// sold contract), never pass an unbounded one.
func strategyAddCovered(g *rpc.StrategyOrderDraft) bool {
	if g == nil || len(g.Legs) < 2 {
		return false
	}
	// room is what each bought leg can still cap, per unit of the strategy.
	room := make([]int, len(g.Legs))
	bought := false
	for i, leg := range g.Legs {
		right := strings.ToUpper(strings.TrimSpace(leg.Contract.Right))
		if !strings.EqualFold(leg.Contract.SecType, "OPT") || (right != "P" && right != "C") || leg.Contract.Strike <= 0 || strings.TrimSpace(leg.Contract.Expiry) == "" || leg.Ratio == 0 {
			return false
		}
		if leg.Ratio > 0 {
			room[i], bought = leg.Ratio, true
		}
	}
	if !bought {
		return false
	}
	// The sold legs nearest the money are the hardest to cap, so they choose
	// first; any bought leg that caps one of them caps every sold leg
	// further out.
	sold := make([]int, 0, len(g.Legs))
	for i, leg := range g.Legs {
		if leg.Ratio < 0 {
			sold = append(sold, i)
		}
	}
	slices.SortFunc(sold, func(a, b int) int {
		la, lb := g.Legs[a].Contract, g.Legs[b].Contract
		if c := cmp.Compare(strings.ToUpper(la.Right), strings.ToUpper(lb.Right)); c != 0 {
			return c
		}
		if strings.EqualFold(la.Right, "P") {
			return cmp.Compare(lb.Strike, la.Strike)
		}
		return cmp.Compare(la.Strike, lb.Strike)
	})
	for _, j := range sold {
		s := g.Legs[j].Contract
		need := -g.Legs[j].Ratio
		for i, leg := range g.Legs {
			if need == 0 {
				break
			}
			l := leg.Contract
			if room[i] <= 0 || !strings.EqualFold(l.Right, s.Right) || strings.TrimSpace(l.Expiry) != strings.TrimSpace(s.Expiry) || contractMultiplier(l) != contractMultiplier(s) ||
				!strings.EqualFold(strings.TrimSpace(l.TradingClass), strings.TrimSpace(s.TradingClass)) {
				continue
			}
			if strings.EqualFold(s.Right, "P") && !(s.Strike < l.Strike) || strings.EqualFold(s.Right, "C") && !(s.Strike > l.Strike) {
				continue
			}
			take := min(room[i], need)
			room[i] -= take
			need -= take
		}
		if need > 0 {
			return false
		}
	}
	return true
}

// strategyAddNetPremium is what a strategy add pays at its limit, in the
// contract currency: units × limit × contract size, zero for a limit that
// pays nothing. The legs of a guaranteed combination share one contract
// size (guaranteedUSOptionComboLeg).
func strategyAddNetPremium(draft rpc.OrderDraft) float64 {
	g := strategyAdd(draft)
	if g == nil || len(g.Legs) == 0 || !(draft.LimitPrice > 0) {
		return 0
	}
	return float64(g.Units) * draft.LimitPrice * float64(contractMultiplier(g.Legs[0].Contract))
}

// strategyAddCapNotional is the notional the order cap reads for a strategy
// add: the net premium at the limit for a covered combination that pays
// one, which is all it can lose; else the gross premium of its legs, as for
// a close or reduction, or the premium its limit can pay when that is more.
func strategyAddCapNotional(draft rpc.OrderDraft, gross float64) float64 {
	net := strategyAddNetPremium(draft)
	if net > 0 && strategyAddCovered(strategyAdd(draft)) {
		return net
	}
	return max(gross, net)
}

// strategyAddBoughtPuts is the bought put contracts of a combination whose
// every leg is a put, and the first such leg; zero when any leg is a call.
func strategyAddBoughtPuts(g *rpc.StrategyOrderDraft) (rpc.ContractParams, int) {
	var first rpc.ContractParams
	quantity := 0
	for _, leg := range g.Legs {
		if !strings.EqualFold(strings.TrimSpace(leg.Contract.Right), "P") {
			return rpc.ContractParams{}, 0
		}
		if leg.Ratio > 0 {
			if quantity == 0 {
				first = leg.Contract
			}
			quantity += leg.Quantity
		}
	}
	return first, quantity
}

// strategyAddHedgeEntry reports a strategy add that is a hedge entry, as a
// single bought put is one (riskAddingOrder): a covered combination of puts
// on a Rulebook hedge symbol, or one whose bought puts cover no more shares
// than the book holds (protectivePut). The sold puts lower its price and
// its reach; what it can lose is still the premium paid.
func (s *Server) strategyAddHedgeEntry(draft rpc.OrderDraft) bool {
	g := strategyAdd(draft)
	if g == nil || !strategyAddCovered(g) {
		return false
	}
	put, quantity := strategyAddBoughtPuts(g)
	if quantity <= 0 {
		return false
	}
	if s.rulebookPolicy().IsHedgeSymbol(put.Symbol) {
		return true
	}
	return s.protectivePut(rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: put, Quantity: quantity, OrderRef: draft.OrderRef})
}

// validateStrategyAddDraft holds a strategy add to its shape: one guaranteed
// combination bought in whole units of a held strategy, every leg an option
// opening further in the held direction and in ratio, each inside the
// option-contract cap, which nothing lifts for an addition. A sold leg that
// no bought leg of the order caps needs the sell-to-open permission like any
// option sold to open, and a covered add's cap notional may not be less than
// the premium its limit can pay.
func validateStrategyAddDraft(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional orderNotionalAuthority) error {
	group := draft.StrategyGroup
	if group == nil || !group.GuaranteedCombo || len(group.Legs) < 2 || draft.Contract.SecType != "BAG" || draft.Action != rpc.OrderActionBuy || group.Units <= 0 || draft.Quantity != group.Units {
		return fmt.Errorf("strategy order is not one guaranteed proportional combo")
	}
	if group.Operation == rpc.StrategyOperationOpen {
		return validateStrategyOpenDraft(limits, draft, position, notional)
	}
	if group.UnitsBefore <= 0 || group.UnitsAfter != group.UnitsBefore+group.Units || position.Before != float64(group.UnitsBefore) || position.After != float64(group.UnitsAfter) {
		return fmt.Errorf("strategy order does not add whole units to the current group")
	}
	sells := false
	for _, leg := range group.Legs {
		wantQuantity := absOrderRatio(leg.Ratio) * group.Units
		if wantQuantity <= 0 || leg.Quantity != wantQuantity || leg.Quantity > limits.MaxOptionContracts || !strings.EqualFold(leg.Contract.SecType, "OPT") {
			return fmt.Errorf("strategy leg %d violates the proportional option quantity limit", leg.Contract.ConID)
		}
		wantAction, expectedAfter := rpc.OrderActionBuy, leg.Before+float64(leg.Quantity)
		if leg.Ratio < 0 {
			wantAction, expectedAfter = rpc.OrderActionSell, leg.Before-float64(leg.Quantity)
			sells = true
		}
		if leg.Action != wantAction || leg.Before == 0 || math.Signbit(leg.Before) != (leg.Ratio < 0) {
			return fmt.Errorf("strategy leg %d would not add to its held position", leg.Contract.ConID)
		}
		if math.Abs(expectedAfter-leg.After) > 1e-9 {
			return rpc.Defect(fmt.Errorf("strategy leg %d has inconsistent before/after quantities", leg.Contract.ConID))
		}
	}
	if strategyAddCovered(group) {
		if net := strategyAddNetPremium(draft); notional.QuoteNotional < net-max(1e-8, net*1e-9) {
			return fmt.Errorf("strategy add notional is below the premium its limit can pay")
		}
		return nil
	}
	if sells && !limits.AllowOptionSellToOpen {
		return fmt.Errorf("this strategy sells an option to open that no bought leg of the order caps; option sell-to-open requires [order_limits].allow_option_sell_to_open = true")
	}
	return nil
}

// validateStrategyOpenDraft holds an open to a debit vertical: no unit of it
// held before, the sold leg opening from none and the bought one from none
// or more held on its own, each inside the option-contract cap, and a cap notional no less than the debit its limit
// can pay. Nothing here reads the sell-to-open permission: only a covered
// debit vertical passes, and every other shape is refused outright.
func validateStrategyOpenDraft(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional orderNotionalAuthority) error {
	group := draft.StrategyGroup
	if group.UnitsBefore != 0 || group.UnitsAfter != group.Units || position.Before != 0 || position.After != float64(group.Units) {
		return fmt.Errorf("a spread is opened from flat")
	}
	if err := validateStrategyOpenVertical(group, draft.LimitPrice); err != nil {
		return err
	}
	for _, leg := range group.Legs {
		if leg.Quantity > limits.MaxOptionContracts {
			return fmt.Errorf("spread leg of %d contracts exceeds the option cap in force of %d contracts ([order_limits].max_option_contracts)", leg.Quantity, limits.MaxOptionContracts)
		}
		// The bought contract may be held already (more of it stays a line
		// of its own); the sold one is opened from none.
		after := leg.Before + float64(leg.Quantity)
		if leg.Ratio < 0 {
			after = leg.Before - float64(leg.Quantity)
		}
		if leg.Before < 0 || leg.Ratio < 0 && leg.Before != 0 || math.Abs(leg.After-after) > 1e-9 {
			return fmt.Errorf("spread leg %d is held in a way a new spread cannot open over: the sold option must not be held, the bought one not held short", leg.Contract.ConID)
		}
	}
	if net := strategyAddNetPremium(draft); notional.QuoteNotional < net-max(1e-8, net*1e-9) {
		return fmt.Errorf("spread notional is below the debit its limit can pay")
	}
	return nil
}

// mandateStrategyAddLegs prices a strategy add as legs of the kept book,
// every leg at the held contract's terms and volatility, so the book is
// measured once with the whole order. A covered add's bought contracts
// share the net premium at the limit as the loss they add; any other add's
// bought contracts keep the held line's value per contract. why names the
// first leg the book cannot price.
func mandateStrategyAddLegs(draft rpc.OrderDraft, in *mandate.Input) (legs []mandate.Leg, why string) {
	g := strategyAdd(draft)
	if g == nil {
		return nil, "the order is no strategy add"
	}
	covered := strategyAddCovered(g)
	boughtContracts := 0.0
	for _, leg := range g.Legs {
		if leg.Ratio > 0 {
			boughtContracts += float64(leg.Quantity)
		}
	}
	netBase := 0.0
	if covered {
		fx := 1.0
		if ccy := normCcy(draft.Contract.Currency); ccy != "" && ccy != normCcy(in.BaseCurrency) {
			fx = in.FXNow[ccy]
			if !(fx > 0) {
				return nil, "no " + in.BaseCurrency + "/" + ccy + " exchange rate for its premium"
			}
		}
		netBase = strategyAddNetPremium(draft) * fx
	}
	for _, leg := range g.Legs {
		id := mandateConLegID(leg.Contract.ConID)
		var held *mandate.Leg
		for i := range in.Legs {
			if in.Legs[i].ID == id && in.Legs[i].Kind == mandate.KindOption {
				held = &in.Legs[i]
			}
		}
		var add mandate.Leg
		switch {
		case held != nil && held.IV != nil && held.Quantity != 0:
			add = *held
		case g.Operation == rpc.StrategyOperationOpen && leg.Mark > 0:
			// A leg the book does not hold yet is priced as a single option
			// order is, at the volatility its preview midpoint implies.
			priced, ok := mandateOrderLeg(rpc.OrderDraft{Action: leg.Action, Contract: leg.Contract, Quantity: leg.Quantity, LimitPrice: leg.Mark}, in)
			if !ok {
				return nil, "no volatility can be implied for " + deltaContractDesc(leg.Contract)
			}
			add = priced
		default:
			return nil, "the book carries no volatility for " + deltaContractDesc(leg.Contract)
		}
		add.Quantity = float64(leg.Quantity)
		if leg.Ratio < 0 {
			add.Quantity = -add.Quantity
		}
		add.LossUnknown = ""
		if covered && leg.Ratio > 0 && boughtContracts > 0 {
			add.LossPerUnitBase = netBase / boughtContracts
		}
		legs = append(legs, add)
	}
	return legs, ""
}

// strategyAddBuys reports an add with a bought leg.
func strategyAddBuys(g *rpc.StrategyOrderDraft) bool {
	if g == nil {
		return false
	}
	for _, leg := range g.Legs {
		if leg.Ratio > 0 {
			return true
		}
	}
	return false
}

// strategyAddBoughtPremiumBase is the premium the bought contracts of an add
// put at risk, in base currency: each at the held contract's value today,
// the figure the kept book carries per contract. The premium limits read it
// for an add that is not covered, whose net limit bounds nothing. why names
// the first bought leg the book cannot value; such an order is refused, never
// passed unmeasured.
func strategyAddBoughtPremiumBase(g *rpc.StrategyOrderDraft, in *mandate.Input) (premium float64, why string) {
	if g == nil || in == nil {
		return 0, "the book carries no value for its legs"
	}
	for _, leg := range g.Legs {
		if leg.Ratio <= 0 {
			continue
		}
		id, value := mandateConLegID(leg.Contract.ConID), 0.0
		for i := range in.Legs {
			if in.Legs[i].ID == id && in.Legs[i].Kind == mandate.KindOption && in.Legs[i].Quantity > 0 {
				value = in.Legs[i].LossPerUnitBase
			}
		}
		if !positiveFinite(value) || leg.Quantity <= 0 {
			return 0, "the book carries no value for " + deltaContractDesc(leg.Contract)
		}
		premium += float64(leg.Quantity) * value
	}
	return premium, ""
}
