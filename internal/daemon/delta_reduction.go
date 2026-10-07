package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Delta-reducing exit exemption (owner decision 2026-10-07 08:13 CEST:
// "tighter order cap must not block exits if they reduce delta").
//
// The rule, stated once here and in internal-docs/design/risk-policy.md: an
// order passes the notional cap and the option-contract cap of [order_limits]
// when it reduces delta: it only closes or shrinks an existing position (it
// never opens a position, never adds to one, never flips to the other side),
// and it lowers the absolute net delta of its underlying, measured with the
// same position deltas the daemon's risk verdicts use (positionDollarDelta:
// shares × mark for a stock, delta × contracts × multiplier × model spot for
// an option, in base currency, as rule 15's net exposure and the portfolio
// reduce sweep read them). If any delta the test needs is unknown or stale,
// the order does not qualify and the cap applies (fail closed).
//
// Every other gate stays as it is: freeze, account and route pins, previews
// and WhatIf, the sell-as-short re-read, origin gating, owner approval, the
// drawdown brake, sell-only and the governor. The protective stock exit
// (protective_exit.go) and the sweep bill (cash_sweep_orders.go) keep their
// own exemptions beside this one: the protective exit also passes the short
// re-read and reads the open-order inventory, not deltas, so it is not a
// special case of this rule. Bills, bonds and conversions carry no equity
// delta, so they never qualify here.
//
// What still bounds cost and risk once a delta-reducing exit passes the caps:
// the order can only shrink a line the book already carries, so its worst
// case is that whole line sent as one order instead of several. The preview
// prices it from a live quote inside the strategy's limit (LMT, bounded
// limit or trail limit; nothing goes out at market), the broker WhatIf must
// accept it, the exact position and this measurement are read again at
// admission, the wire guard re-checks the position, and the owner approves
// each order. What remains is the price impact of one large exit against its
// limit, which the owner accepts by approving a preview that shows the whole
// quantity.

// deltaReductionLeg is one held line the order touches: the quantity the
// measurement saw and the base dollar delta one unit of it carries.
type deltaReductionLeg struct {
	Quantity float64
	UnitBase float64
}

// deltaReductionEvidence is one current measurement of the order's
// underlying for the exemption. The zero value is "not measured" and never
// exempts.
type deltaReductionEvidence struct {
	// Current is true only when every held equity and option leg of the
	// underlying was measured from a current, same-account positions read
	// with no stale row and no missing delta, spot or FX rate.
	Current      bool
	Underlying   string
	BaseCurrency string
	// NetBefore is the signed net dollar delta of the underlying in base
	// currency before the order.
	NetBefore float64
	// Legs holds the measurement of each contract the order touches, by
	// ConID.
	Legs map[int]deltaReductionLeg
	// Reason says, in plain words, why the measurement is not current.
	Reason string
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

// deltaReductionConIDs names the contracts whose held lines the order
// changes.
func deltaReductionConIDs(draft rpc.OrderDraft) map[int]struct{} {
	out := map[int]struct{}{}
	if group := draft.StrategyGroup; group != nil {
		for _, leg := range group.Legs {
			if leg.Contract.ConID > 0 {
				out[leg.Contract.ConID] = struct{}{}
			}
		}
		return out
	}
	if draft.Contract.ConID > 0 {
		out[draft.Contract.ConID] = struct{}{}
	}
	return out
}

// deltaReductionLegDesc names a held line in a refusal, an option leg as the
// Rulebook describes it (legDesc).
func deltaReductionLegDesc(row rpc.PositionView, isOption bool) string {
	if !isOption {
		return "the " + strings.ToUpper(strings.TrimSpace(row.Symbol)) + " stock line"
	}
	return "the " + legDesc(row) + " option line"
}

// measureDeltaReduction measures a candidate order's underlying from one
// positions read: every held equity and option leg with that symbol, in base
// currency, with the deltas the daemon's risk verdicts use. Anything that
// cannot be measured leaves the evidence not current, saying why.
func measureDeltaReduction(pos *rpc.PositionsResult, scope brokerStateScope, draft rpc.OrderDraft) deltaReductionEvidence {
	underlying := strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
	ev := deltaReductionEvidence{Underlying: underlying, Legs: map[int]deltaReductionLeg{}}
	wanted := deltaReductionConIDs(draft)
	switch {
	case underlying == "" || len(wanted) == 0:
		ev.Reason = "the order names no held contract"
		return ev
	case pos == nil || !currentPortfolioAuthority(pos.Authority):
		ev.Reason = "current positions are unavailable"
		return ev
	case !strings.EqualFold(strings.TrimSpace(pos.Authority.Scope.AccountID), strings.TrimSpace(scope.Account)) ||
		!strings.EqualFold(strings.TrimSpace(pos.Authority.Scope.AccountMode), strings.TrimSpace(scope.Mode)):
		ev.Reason = "the positions read belongs to another account session"
		return ev
	case pos.Portfolio == nil || normCcy(pos.Portfolio.BaseCurrency) == "":
		ev.Reason = "the account base currency is unknown"
		return ev
	}
	ev.BaseCurrency = normCcy(pos.Portfolio.BaseCurrency)
	visit := func(row rpc.PositionView, isOption bool) string {
		if !strings.EqualFold(strings.TrimSpace(row.Symbol), underlying) || row.Quantity == 0 {
			return ""
		}
		desc := deltaReductionLegDesc(row, isOption)
		if row.Stale {
			return desc + " has a stale quote"
		}
		dd, ok := positionDollarDelta(row, isOption)
		if !ok {
			if isOption {
				return desc + " has no delta or underlying spot"
			}
			return desc + " has no mark"
		}
		rate, ok := positionBaseRate(row, ev.BaseCurrency)
		if !ok {
			return desc + " has no FX rate to " + ev.BaseCurrency
		}
		base := dd * rate
		if math.IsNaN(base) || math.IsInf(base, 0) {
			return desc + " has no finite delta"
		}
		ev.NetBefore += base
		if _, want := wanted[row.ConID]; want {
			if _, dup := ev.Legs[row.ConID]; dup {
				return desc + " appears in duplicate rows"
			}
			ev.Legs[row.ConID] = deltaReductionLeg{Quantity: row.Quantity, UnitBase: base / row.Quantity}
		}
		return ""
	}
	for _, row := range pos.Stocks {
		if !rpc.PositionQuotesAsStock(row) {
			continue // bills, bonds and conversions carry no equity delta
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
	for conID := range wanted {
		if _, ok := ev.Legs[conID]; !ok {
			ev.Reason = fmt.Sprintf("contract %d is not a held line of %s", conID, underlying)
			return ev
		}
	}
	ev.Current = true
	return ev
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

// deltaReducingExit decides the exemption for one order from its evidence:
// whether the order passes the notional and option-contract caps and, for a
// close or reduction that does not, the reason the refusal carries. An order
// that is no close or reduction never qualifies and carries no reason.
func deltaReducingExit(draft rpc.OrderDraft, position rpc.OrderPositionImpact, ev deltaReductionEvidence) (bool, string) {
	if !deltaReductionCandidate(draft, position) {
		if draft.StrategyGroup == nil && ibkrlib.IsBillOrBond(draft.Contract.SecType) && isRiskReducing(position.Effect) {
			// A bill or bond sale keeps the cap: it carries no equity delta,
			// so there is nothing for this rule to measure (the sweep bill
			// exemption, cash_sweep_orders.go, is the one that can apply).
			return false, "; a bill or bond carries no equity delta, so the exemption for delta-reducing exits cannot apply"
		}
		return false, ""
	}
	name := strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
	if !ev.Current || !strings.EqualFold(ev.Underlying, name) || ev.Legs == nil {
		why := strings.TrimSpace(ev.Reason)
		if why == "" {
			why = "no current measurement"
		}
		return false, fmt.Sprintf("; a close or reduction passes the cap when it lowers the absolute delta of %s, but that delta cannot be measured (%s)", name, why)
	}
	after := ev.NetBefore
	apply := func(conID int, before, afterQty float64) bool {
		leg, ok := ev.Legs[conID]
		if !ok || math.Abs(leg.Quantity-before) > 1e-9 {
			return false
		}
		after += (afterQty - before) * leg.UnitBase
		return true
	}
	if group := draft.StrategyGroup; group != nil {
		for _, leg := range group.Legs {
			if !apply(leg.Contract.ConID, leg.Before, leg.After) {
				return false, fmt.Sprintf("; the measured position of %s leg %d differs from the order's; preview again", name, leg.Contract.ConID)
			}
		}
	} else if !apply(draft.Contract.ConID, position.Before, position.After) {
		return false, fmt.Sprintf("; the measured %s position differs from the order's; preview again", name)
	}
	if lowersAbsoluteDelta(ev.NetBefore, after) {
		return true, ""
	}
	return false, fmt.Sprintf("; the exit does not lower the absolute delta of %s (%s before, %s after), so the cap applies",
		name, risk.FormatOrderMoney(math.Abs(ev.NetBefore), ev.BaseCurrency), risk.FormatOrderMoney(math.Abs(after), ev.BaseCurrency))
}

// captureDeltaReductionEvidence reads the positions for a close or reduction
// the caps would otherwise refuse and measures its underlying. Any other
// order, or one the caps admit, reads nothing and gets the zero value, which
// never exempts. A read that fails leaves the evidence not current: the cap
// applies. Preview and admission each read; the wire guard reuses the
// admission reading with the re-read position and issues no broker request.
func (s *Server) captureDeltaReductionEvidence(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft, position rpc.OrderPositionImpact, limits risk.OrderLimitsInForce, notional orderNotionalAuthority) deltaReductionEvidence {
	if s == nil || ctx == nil || !deltaReductionCandidate(draft, position) || !deltaReductionCapsBind(limits, draft, notional) {
		return deltaReductionEvidence{}
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
		return deltaReductionEvidence{Underlying: strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol)), Reason: "the positions read failed: " + err.Error()}
	}
	return measureDeltaReduction(pos, brokerStateScope{Account: status.Account, Mode: status.Mode}, draft)
}
