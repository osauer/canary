package daemon

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Delta-reducing exit exemption (owner decision 2026-10-07 08:13 CEST:
// "tighter order cap must not block exits if they reduce delta"; the two open
// questions decided 2026-10-07 09:51 CEST: "Both" and "Keep the cap").
//
// The rule, stated once here and in internal-docs/design/risk-policy.md: an
// order passes the notional cap and the option-contract cap of [order_limits]
// when it reduces delta: it only closes or shrinks an existing position (it
// never opens a position, never adds to one, never flips to the other side),
// and it lowers both the absolute net delta of its underlying and the
// absolute net delta of the whole book ("Both"), measured with the same
// position deltas the daemon's risk verdicts use (positionDollarDelta ×
// positionBaseRate: shares × mark for a stock, delta × contracts ×
// multiplier × model spot for an option, in base currency, as rule 15's net
// exposure and the portfolio reduce sweep read them). If any delta the test
// needs is unknown or stale, the order does not qualify and the cap applies
// (fail closed); bills, bonds and conversions carry no equity delta and
// count as zero, and so does a stock row Canary marks stale only because it
// is a zero-value row. An exit that would leave a short option uncovered
// keeps the cap ("Keep the cap", the principle of Canary's own option
// combos: never leave a short leg uncovered): after the order, the
// underlying's short calls need long shares or long calls and its short puts
// need short shares or long puts, and an exit that takes that cover away
// does not qualify. The order and every other working order in its
// direction on the same contract must together stay within the held line,
// read from the broker's complete open-order inventory; without that
// inventory the cap applies too, so two exits of one line cannot both pass
// and together flip it.
//
// Every other gate stays as it is: freeze, account and route pins, previews
// and WhatIf, the sell-as-short re-read, origin gating, owner approval, the
// drawdown brake, sell-only and the governor. The protective stock exit
// (protective_exit.go) and the sweep bill (cash_sweep_orders.go) keep their
// own exemptions beside this one: the protective exit also passes the short
// re-read and reads only the open-order inventory, not deltas, so it is not
// a special case of this rule.
//
// What still bounds cost and risk once a delta-reducing exit passes the caps:
// the order can only shrink a line the book already carries, and with every
// other working order in its direction it stays within that line, so its
// worst case is the whole line sent as one order instead of several. The
// preview prices it from a live quote inside the strategy's limit (LMT,
// bounded limit or trail limit; nothing goes out at market), the broker
// WhatIf must accept it, the exact position, this measurement and the
// inventory are read again at admission, and the wire guard re-checks the
// position. Who decides to send it is unchanged by this rule: a hand order,
// a proposal submit and a single reduce are approved per order (device
// confirmation or the human CLI); a portfolio reduce sweep is approved as one
// basket of up to 25 orders; a queued authorisation was signed per order by
// the owner ahead of time; a bucket listed in the protection policy's
// [authority] pre_authorised (trailing_stop, option_loss_exit,
// option_profit_trail, budget_reduction) is sent by the daemon after a notice
// and the veto window with no per-order approval, so for those buckets the
// exemption now admits an option exit above the caps on the standing policy
// alone; the protective stop guard only shrinks or cancels Canary's own stock
// stops. What remains is the price impact of one large exit against its
// limit, which the owner accepts by approving a preview that shows the whole
// quantity, or, for a pre-authorised bucket, by listing the bucket.

// deltaReductionLeg is one held line the order touches: the quantity the
// measurement saw and the base dollar delta one unit of it carries.
type deltaReductionLeg struct {
	Quantity float64
	UnitBase float64
}

// deltaCoverLeg is one option line of the underlying as the short-leg
// coverage rule sees it.
type deltaCoverLeg struct {
	ConID      int
	Right      string
	Quantity   float64
	Multiplier float64
}

// deltaCoverage is the underlying's shares and option lines for the
// short-leg coverage rule (owner decision 2026-10-07 09:51 CEST, "Keep the
// cap").
type deltaCoverage struct {
	// Shares is the underlying's net stock quantity, by ConID, so a sale of
	// one of several stock rows changes the right one.
	Shares  map[int]float64
	Options []deltaCoverLeg
}

// deltaReductionEvidence is one current measurement of the order's
// underlying and of the whole book for the exemption. The zero value is
// "not measured" and never exempts.
type deltaReductionEvidence struct {
	// Current is true only when every held equity and option leg of the
	// book was measured from a current, same-account positions read with no
	// stale row and no missing delta, spot or FX rate.
	Current bool
	// Unread is true when nothing was read because no cap bound the order
	// when the evidence was captured; a cap that binds later (the book's
	// NLV moved before the wire guard) asks for a new preview.
	Unread       bool
	Underlying   string
	BaseCurrency string
	// NetBefore is the signed net dollar delta of the underlying in base
	// currency before the order.
	NetBefore float64
	// BookBefore is the signed net dollar delta of the whole book in base
	// currency before the order, from the same per-row measure.
	BookBefore float64
	// Legs holds the measurement of each contract the order touches, by
	// ConID.
	Legs map[int]deltaReductionLeg
	// Cover is the underlying's shares and option lines for the coverage
	// rule.
	Cover deltaCoverage
	// Reason says, in plain words, why the measurement is not current.
	Reason string
}

// deltaChange is one contract's quantity change: a close or a reduction
// moves Before toward zero.
type deltaChange struct {
	ConID         int
	Contract      rpc.ContractParams
	Action        string
	Before, After float64
}

// shrinksPosition reports whether after is on the same side as before, or
// flat, and smaller in magnitude: a close or a reduction, never an open, an
// increase or a flip.
func shrinksPosition(before, after float64) bool {
	if math.IsNaN(before) || math.IsInf(before, 0) || math.IsNaN(after) || math.IsInf(after, 0) || before == 0 {
		return false
	}
	return before*after >= 0 && math.Abs(after) < math.Abs(before)
}

// deltaReductionCandidate is the shape test that needs no measurement: a
// stock, ETF or option order, or a guaranteed strategy combo of options,
// whose every leg closes or shrinks a held position and none opens, adds or
// flips. Bills, bonds and conversions carry no equity delta and never
// qualify.
func deltaReductionCandidate(draft rpc.OrderDraft, position rpc.OrderPositionImpact) bool {
	if draft.Quantity <= 0 {
		return false
	}
	if group := draft.StrategyGroup; group != nil {
		if !group.GuaranteedCombo || len(group.Legs) < 2 || group.Units <= 0 || group.UnitsAfter < 0 || group.UnitsAfter != group.UnitsBefore-group.Units {
			return false
		}
		for _, leg := range group.Legs {
			if !strings.EqualFold(leg.Contract.SecType, "OPT") || leg.Contract.ConID <= 0 || !shrinksPosition(leg.Before, leg.After) {
				return false
			}
		}
		return true
	}
	if draft.Contract.ConID <= 0 || (!isStockLikeRiskSecType(draft.Contract.SecType) && !strings.EqualFold(draft.Contract.SecType, "OPT")) {
		return false
	}
	return isRiskReducing(position.Effect) && shrinksPosition(position.Before, position.After)
}

// deltaReductionChanges lists the quantity changes a candidate order makes:
// its one contract, or every leg of a strategy combo.
func deltaReductionChanges(draft rpc.OrderDraft, position rpc.OrderPositionImpact) []deltaChange {
	if group := draft.StrategyGroup; group != nil {
		out := make([]deltaChange, 0, len(group.Legs))
		for _, leg := range group.Legs {
			out = append(out, deltaChange{ConID: leg.Contract.ConID, Contract: leg.Contract, Action: leg.Action, Before: leg.Before, After: leg.After})
		}
		return out
	}
	return []deltaChange{{ConID: draft.Contract.ConID, Contract: draft.Contract, Action: draft.Action, Before: position.Before, After: position.After}}
}

// deltaReductionCapsBind reports whether the notional cap or the option
// contract cap would refuse the order, so a measurement is worth reading.
func deltaReductionCapsBind(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, notional orderNotionalAuthority) bool {
	if !limits.Complete {
		return false
	}
	if notional.BaseNotional > limits.CapBase {
		return true
	}
	if strings.EqualFold(draft.Contract.SecType, "OPT") && draft.Quantity > limits.MaxOptionContracts {
		return true
	}
	if group := draft.StrategyGroup; group != nil {
		for _, leg := range group.Legs {
			if leg.Quantity > limits.MaxOptionContracts {
				return true
			}
		}
	}
	return false
}

// deltaContractDesc names a contract in plain words: "the SYNB stock" or,
// as the Rulebook describes an option leg, "the SYNB 20261218 C 75 option".
func deltaContractDesc(c rpc.ContractParams) string {
	symbol := strings.ToUpper(strings.TrimSpace(c.Symbol))
	if strings.EqualFold(c.SecType, "OPT") {
		return "the " + legDesc(rpc.PositionView{Symbol: symbol, Expiry: c.Expiry, Right: c.Right, Strike: c.Strike}) + " option"
	}
	return "the " + symbol + " stock"
}

// deltaReductionLegDesc names a held row in a refusal, an option leg as the
// Rulebook describes it (legDesc).
func deltaReductionLegDesc(row rpc.PositionView, isOption bool) string {
	if !isOption {
		return "the " + strings.ToUpper(strings.TrimSpace(row.Symbol)) + " stock line"
	}
	return "the " + legDesc(row) + " option line"
}

// deltaLegBase is the one measurement of a held equity or option row for the
// rule, shared by the trading gate and the policy check: the signed base
// dollar delta the daemon's risk verdicts use (positionDollarDelta at the
// row's own FX rate), or why the row cannot be measured. A stock row Canary
// marks stale only because it is a zero-value row (flagZeroValueStockPositions,
// "likely inactive or defunct") has a known value of zero and measures as
// zero, so a defunct line cannot make the book unmeasurable. There is no
// other fallback: a row without its rate is unmeasured for both.
func deltaLegBase(row rpc.PositionView, isOption bool, baseCcy string) (float64, string) {
	if !isOption && positionWarningHasCode(row.WarningDetails, zeroValueStockPositionCode) {
		return 0, ""
	}
	if row.Stale {
		return 0, "has a stale quote"
	}
	dd, ok := positionDollarDelta(row, isOption)
	if !ok {
		if isOption {
			return 0, "has no delta or no underlying price"
		}
		return 0, "has no price"
	}
	rate, ok := positionBaseRate(row, baseCcy)
	if !ok {
		return 0, "has no exchange rate to " + normCcy(baseCcy)
	}
	base := dd * rate
	if math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, "has no usable delta"
	}
	return base, ""
}

// measureDeltaReduction measures a candidate order's underlying and the whole
// book from one positions read: every held equity and option leg, in base
// currency, with the deltas the daemon's risk verdicts use, in one pass.
// Any line that cannot be measured leaves the evidence not current, saying
// which line.
func measureDeltaReduction(pos *rpc.PositionsResult, scope brokerStateScope, draft rpc.OrderDraft) deltaReductionEvidence {
	underlying := strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
	ev := deltaReductionEvidence{Underlying: underlying, Legs: map[int]deltaReductionLeg{}, Cover: deltaCoverage{Shares: map[int]float64{}}}
	changes := deltaReductionChanges(draft, rpc.OrderPositionImpact{})
	switch {
	case underlying == "" || len(changes) == 0 || changes[0].ConID <= 0:
		ev.Reason = "the order names no held position"
		return ev
	case pos == nil || !currentPortfolioAuthority(pos.Authority):
		ev.Reason = "Canary has no current positions; preview again"
		return ev
	case !strings.EqualFold(strings.TrimSpace(pos.Authority.Scope.AccountID), strings.TrimSpace(scope.Account)) ||
		!strings.EqualFold(strings.TrimSpace(pos.Authority.Scope.AccountMode), strings.TrimSpace(scope.Mode)):
		ev.Reason = "the positions Canary read are not this account's; preview again"
		return ev
	case pos.Portfolio == nil || normCcy(pos.Portfolio.BaseCurrency) == "":
		ev.Reason = "the account base currency is unknown"
		return ev
	}
	ev.BaseCurrency = normCcy(pos.Portfolio.BaseCurrency)
	wanted := make(map[int]struct{}, len(changes))
	for _, change := range changes {
		wanted[change.ConID] = struct{}{}
	}
	visit := func(row rpc.PositionView, isOption bool) string {
		if row.Quantity == 0 {
			return ""
		}
		base, why := deltaLegBase(row, isOption, ev.BaseCurrency)
		if why != "" {
			return deltaReductionLegDesc(row, isOption) + " " + why
		}
		ev.BookBefore += base
		if !strings.EqualFold(strings.TrimSpace(row.Symbol), underlying) {
			return ""
		}
		ev.NetBefore += base
		if isOption {
			ev.Cover.Options = append(ev.Cover.Options, deltaCoverLeg{ConID: row.ConID, Right: strings.ToUpper(strings.TrimSpace(row.Right)), Quantity: row.Quantity, Multiplier: float64(optionMultiplier(row))})
		} else {
			ev.Cover.Shares[row.ConID] += row.Quantity
		}
		if _, want := wanted[row.ConID]; want {
			if _, dup := ev.Legs[row.ConID]; dup {
				return deltaReductionLegDesc(row, isOption) + " appears twice in the positions"
			}
			ev.Legs[row.ConID] = deltaReductionLeg{Quantity: row.Quantity, UnitBase: base / row.Quantity}
		}
		return ""
	}
	for _, row := range pos.Stocks {
		if ibkrlib.IsBillOrBond(row.SecType) || strings.EqualFold(strings.TrimSpace(row.SecType), "CASH") {
			continue // bills, bonds and conversions carry no equity delta (owner decision 2026-10-07 09:51 CEST)
		}
		if !rpc.PositionQuotesAsStock(row) {
			// A future, index, CFD, fund or warrant row carries delta the
			// daemon does not measure: it keeps the cap and is named.
			if row.Quantity != 0 {
				ev.Reason = fmt.Sprintf("the %s %s line is not a stock, ETF or option, so Canary has no delta for it",
					strings.ToUpper(strings.TrimSpace(row.Symbol)), strings.ToUpper(strings.TrimSpace(row.SecType)))
				return ev
			}
			continue
		}
		if why := visit(row, false); why != "" {
			ev.Reason = why
			return ev
		}
	}
	for _, row := range pos.Options {
		if why := visit(row, true); why != "" {
			ev.Reason = why
			return ev
		}
	}
	for _, change := range changes {
		if _, ok := ev.Legs[change.ConID]; !ok {
			ev.Reason = deltaContractDesc(change.Contract) + " is not a held position; preview again"
			return ev
		}
	}
	ev.Current = true
	return ev
}

// netAfter applies the order's quantity changes to the measured net delta.
// It fails, naming the contract, when a changed contract was not measured or
// the measured quantity differs from the one the order was judged against.
func (ev deltaReductionEvidence) netAfter(changes []deltaChange) (after float64, failed *deltaChange) {
	after = ev.NetBefore
	for i := range changes {
		change := &changes[i]
		leg, ok := ev.Legs[change.ConID]
		if !ok || math.Abs(leg.Quantity-change.Before) > 1e-9 {
			return 0, change
		}
		after += (change.After - change.Before) * leg.UnitBase
	}
	return after, nil
}

// lowersAbsoluteDelta reports whether a net delta moving from before to
// after shrinks in absolute terms, with a tolerance for float noise. A net
// delta of zero cannot be lowered.
func lowersAbsoluteDelta(before, after float64) bool {
	if math.IsNaN(before) || math.IsInf(before, 0) || math.IsNaN(after) || math.IsInf(after, 0) {
		return false
	}
	return math.Abs(after) < math.Abs(before)-1e-9*max(1, math.Abs(before))
}

// uncovered is the underlying's short option exposure the coverage rule
// finds unprotected, in contracts of each right, after the given changes
// are applied: short calls beyond the long shares and long calls, short
// puts beyond the short shares and long puts (Canary's own pairing of
// opposite-signed legs of one right, internal/strategy). Share-equivalents
// are converted to contracts at the short legs' multiplier.
func (c deltaCoverage) uncovered(changes []deltaChange) (calls, puts float64) {
	shares := 0.0
	for _, q := range c.Shares {
		shares += q
	}
	quantity := func(leg deltaCoverLeg) float64 {
		for _, change := range changes {
			if change.ConID == leg.ConID {
				return change.After
			}
		}
		return leg.Quantity
	}
	for _, change := range changes {
		if _, stock := c.Shares[change.ConID]; stock {
			shares += change.After - change.Before
		}
	}
	var shortCalls, longCalls, shortPuts, longPuts, callMult, putMult float64
	for _, leg := range c.Options {
		q := quantity(leg)
		mult := max(leg.Multiplier, 1)
		switch {
		case strings.HasPrefix(leg.Right, "C") && q < 0:
			shortCalls += -q * mult
			callMult = mult
		case strings.HasPrefix(leg.Right, "C"):
			longCalls += q * mult
		case strings.HasPrefix(leg.Right, "P") && q < 0:
			shortPuts += -q * mult
			putMult = mult
		case strings.HasPrefix(leg.Right, "P"):
			longPuts += q * mult
		}
	}
	if callMult > 0 {
		calls = max(0, shortCalls-longCalls-max(shares, 0)) / callMult
	}
	if putMult > 0 {
		puts = max(0, shortPuts-longPuts-max(-shares, 0)) / putMult
	}
	return calls, puts
}

// judge applies the rule to a current measurement: the order's changes must
// lower the underlying's absolute delta and the book's, and must not take
// cover away from a short option. It returns the reason a refusal carries.
func (ev deltaReductionEvidence) judge(name string, changes []deltaChange) (bool, string) {
	after, failed := ev.netAfter(changes)
	if failed != nil {
		return false, fmt.Sprintf("; the %s position has changed since the order was previewed; preview again", strings.TrimPrefix(deltaContractDesc(failed.Contract), "the "))
	}
	if !lowersAbsoluteDelta(ev.NetBefore, after) {
		return false, fmt.Sprintf("; the exit does not lower the absolute delta of %s (%s before, %s after), so the cap applies",
			name, risk.FormatOrderMoney(math.Abs(ev.NetBefore), ev.BaseCurrency), risk.FormatOrderMoney(math.Abs(after), ev.BaseCurrency))
	}
	bookAfter := ev.BookBefore + (after - ev.NetBefore)
	if !lowersAbsoluteDelta(ev.BookBefore, bookAfter) {
		return false, fmt.Sprintf("; the exit does not lower the absolute delta of the whole book (%s before, %s after), so the cap applies",
			risk.FormatOrderMoney(math.Abs(ev.BookBefore), ev.BaseCurrency), risk.FormatOrderMoney(math.Abs(bookAfter), ev.BaseCurrency))
	}
	callsBefore, putsBefore := ev.Cover.uncovered(nil)
	callsAfter, putsAfter := ev.Cover.uncovered(changes)
	verb := "sale"
	if len(changes) == 1 && strings.EqualFold(strings.TrimSpace(changes[0].Action), rpc.OrderActionBuy) {
		verb = "buy-back"
	}
	if callsAfter > callsBefore+1e-9 {
		return false, fmt.Sprintf("; this %s would leave %s short calls on %s uncovered, so the order cap applies", verb, deltaContracts(callsAfter-callsBefore), name)
	}
	if putsAfter > putsBefore+1e-9 {
		return false, fmt.Sprintf("; this %s would leave %s short puts on %s uncovered, so the order cap applies", verb, deltaContracts(putsAfter-putsBefore), name)
	}
	return true, ""
}

// deltaContracts renders a contract count, whole when it is whole.
func deltaContracts(v float64) string {
	return strconv.FormatFloat(math.Ceil(v-1e-9), 'f', -1, 64)
}

// competingExitClause words the refusal when the other working orders in
// the order's direction and this order together exceed the held line:
// "another working order already sells 700 of the 1,000 SYNB shares you
// hold; with this one, more would be sold than you hold. Cancel it first".
// Counts carry thousands separators like the money beside them, and the
// clause never contains "); ", which Desk reads as a sentence break.
func competingExitClause(change deltaChange, other float64) string {
	present, past := "sells", "sold"
	if strings.EqualFold(strings.TrimSpace(change.Action), rpc.OrderActionBuy) {
		present, past = "buys back", "bought back"
	}
	hold := "hold"
	if change.Before < 0 {
		hold = "are short"
	}
	unit := "shares"
	if strings.EqualFold(change.Contract.SecType, "OPT") {
		unit = "contracts"
	}
	line := strings.TrimPrefix(deltaContractDesc(change.Contract), "the ")
	line = strings.TrimSuffix(strings.TrimSuffix(line, " stock"), " option")
	return fmt.Sprintf("another working order already %s %s of the %s %s %s you %s; with this one, more would be %s than you %s. Cancel it first",
		present, risk.FormatOrderMoney(other, ""), risk.FormatOrderMoney(math.Abs(change.Before), ""), line, unit, hold, past, hold)
}

// deltaReducingExit decides the exemption for one order from its evidence
// and the open-order inventory: whether the order passes the notional and
// option-contract caps and, for a close or reduction that does not, the
// reason the refusal carries. An order that is no close or reduction never
// qualifies and carries no reason.
func deltaReducingExit(draft rpc.OrderDraft, position rpc.OrderPositionImpact, ev deltaReductionEvidence, inv protectiveExitInventory) (bool, string) {
	if !deltaReductionCandidate(draft, position) {
		if bondSaleCandidate(draft, position) {
			// A bill or bond sale keeps the cap: it carries no equity delta,
			// so there is nothing for this rule to measure (the sweep bill
			// exemption, cash_sweep_orders.go, is the one that can apply).
			return false, "; a bill or bond carries no equity delta, so the exemption for delta-reducing exits cannot apply"
		}
		return false, ""
	}
	if ev.Unread {
		return false, "; the order cap did not apply when you previewed this order; preview it again"
	}
	name := strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
	if !ev.Current || !strings.EqualFold(ev.Underlying, name) || ev.Legs == nil {
		why := strings.TrimSpace(ev.Reason)
		if why == "" {
			why = "no current measurement"
		}
		return false, fmt.Sprintf("; a close or reduction passes the cap when it lowers the absolute delta of %s and of the whole book, but that delta cannot be measured (%s)", name, why)
	}
	changes := deltaReductionChanges(draft, position)
	if ok, why := ev.judge(name, changes); !ok {
		return false, why
	}
	// Two exits of one line, each judged against the same position, would
	// together flip it: this order and every other working order in its
	// direction must stay within the held line.
	if !inv.Current {
		return false, "; Canary cannot read the broker's open orders right now, so it cannot rule out another exit of this line; preview again"
	}
	for _, change := range changes {
		other := inv.otherWorkingSameSide(change.ConID)
		qty := math.Abs(change.After - change.Before)
		if math.IsNaN(other) || math.IsInf(other, 0) || other < 0 || other+qty > math.Abs(change.Before)+1e-9 {
			return false, "; " + competingExitClause(change, other)
		}
	}
	return true, ""
}

// captureDeltaReductionEvidence reads the positions for a close or reduction
// the caps would otherwise refuse and measures its underlying and the book.
// Any other order reads nothing: a non-candidate gets the zero value, a
// candidate the caps admit an Unread value, and neither exempts. A read that
// fails leaves the evidence not current: the cap applies. Preview and
// admission each read; the wire guard reuses the admission reading with the
// re-read position and issues no broker request.
func (s *Server) captureDeltaReductionEvidence(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft, position rpc.OrderPositionImpact, limits risk.OrderLimitsInForce, notional orderNotionalAuthority) deltaReductionEvidence {
	if s == nil || ctx == nil || !deltaReductionCandidate(draft, position) {
		return deltaReductionEvidence{}
	}
	underlying := strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
	if !deltaReductionCapsBind(limits, draft, notional) {
		return deltaReductionEvidence{Unread: true, Underlying: underlying}
	}
	readCtx, cancel := requestCtx(ctx, rpc.MethodPositionsList)
	defer cancel()
	var pos *rpc.PositionsResult
	var err error
	if s.orderDeltaPositionsForTest != nil {
		pos, err = s.orderDeltaPositionsForTest(readCtx)
	} else {
		pos, err = s.handlePositionsList(readCtx, &rpc.Request{})
	}
	if err != nil {
		return deltaReductionEvidence{Underlying: underlying, Reason: "Canary cannot read the positions right now; preview again"}
	}
	return measureDeltaReduction(pos, brokerStateScope{Account: status.Account, Mode: status.Mode}, draft)
}
