package daemon

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The cash sweep's order path (internal-docs/design/cash-sweep.md, Phase B).
// An invest row buys its resolved bill in whole order units on the line's
// size grid; a redemption sells a held bill on the same grid. Both are BOND
// LMT DAY orders priced per 100 of face on the line's minimum tick, previewed
// through every gate an ordinary proposal meets (freeze, authority, session,
// quote freshness, WhatIf) and submitted only on the owner's approval, or,
// when the owner lists cash_sweep under [authority].pre_authorised, by the
// daemon after the full veto window.

// bondSessionMarket names a bond line's session in readiness; it is no
// exchange calendar.
const bondSessionMarket marketcal.Market = "bond"

// cashSweepAssumedSessionDays is how many calendar days of assumed windows a
// session carries from generation: a week covers any weekend.
const cashSweepAssumedSessionDays = 8

// cashSweepBondSession is when a line's DAY order can fill: its liquid (else
// trading) hours from contract details, else the instrument's assumed hours
// on weekdays.
func cashSweepBondSession(line *ibkrlib.BondContractDetails, instrument string, now time.Time) *rpc.BondSession {
	conv := cashSweepInstrumentConventions[instrument]
	if line != nil {
		if windows, source, ok := line.SessionWindows(); ok {
			s := &rpc.BondSession{Source: source, Label: nonEmptyString(conv.SessionLabel, "Bond line") + " (contract hours)", TimeZone: line.TimeZoneID}
			for _, w := range windows {
				s.Windows = append(s.Windows, rpc.BondSessionWindow{Open: w.Open, Close: w.Close})
			}
			return s
		}
	}
	return cashSweepAssumedSession(instrument, now)
}

// cashSweepAssumedSession is the instrument's assumed session (A5) for the
// days from now: one window a weekday, holidays not modelled.
func cashSweepAssumedSession(instrument string, now time.Time) *rpc.BondSession {
	conv, ok := cashSweepInstrumentConventions[instrument]
	if !ok || conv.TimeZone == "" {
		return nil
	}
	loc, err := time.LoadLocation(conv.TimeZone)
	if err != nil {
		return nil
	}
	s := &rpc.BondSession{Source: rpc.BondSessionSourceAssumed, Label: conv.SessionLabel + " (assumed hours)", TimeZone: conv.TimeZone}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for i := range cashSweepAssumedSessionDays {
		day := today.AddDate(0, 0, i)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		open := time.Date(day.Year(), day.Month(), day.Day(), conv.Open/100, conv.Open%100, 0, 0, loc)
		closeAt := time.Date(day.Year(), day.Month(), day.Day(), conv.Close/100, conv.Close%100, 0, 0, loc)
		s.Windows = append(s.Windows, rpc.BondSessionWindow{Open: open.UTC(), Close: closeAt.UTC()})
	}
	return s
}

// bondSessionAt reads a bond session at an instant the way readiness reads
// an exchange calendar: open inside a window, closed before the next one
// (with its open), unknown past the last window the session names.
func bondSessionAt(sess *rpc.BondSession, at time.Time) (marketcal.Session, bool) {
	if sess == nil || len(sess.Windows) == 0 || at.IsZero() {
		return marketcal.Session{}, false
	}
	windows := slices.Clone(sess.Windows)
	slices.SortFunc(windows, func(a, b rpc.BondSessionWindow) int { return a.Open.Compare(b.Open) })
	out := marketcal.Session{Market: bondSessionMarket, Label: nonEmptyString(sess.Label, "Bond line"), Timezone: sess.TimeZone, Source: sess.Source}
	for _, w := range windows {
		if !at.Before(w.Open) && at.Before(w.Close) {
			out.State, out.IsOpen, out.Open, out.Close = marketcal.StateRegular, true, w.Open, w.Close
			out.Windows = []marketcal.Window{{Open: w.Open, Close: w.Close}}
			return out, true
		}
		if at.Before(w.Open) {
			open, closeAt := w.Open, w.Close
			out.State, out.Reason, out.NextOpen, out.NextClose = marketcal.StateClosed, "outside the bill's session", &open, &closeAt
			return out, true
		}
	}
	return marketcal.Session{}, false
}

// proposalBondSession is the session a cash_sweep BOND row's order fills in,
// when the row carries one.
func proposalBondSession(prop rpc.TradeProposal) (*rpc.BondSession, bool) {
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || prop.CashSweep == nil || prop.CashSweep.Session == nil ||
		!strings.EqualFold(strings.TrimSpace(prop.Contract.SecType), "BOND") {
		return nil, false
	}
	return prop.CashSweep.Session, true
}

// proposalSessionAt is the session readiness and the pre-authorised
// scheduler judge a row by: a sweep bill's own session, else the contract's
// exchange calendar. hasMarket is false when neither applies.
func (e *proposalEngine) proposalSessionAt(prop rpc.TradeProposal, at time.Time, sessions readinessSessions) (market marketcal.Market, session marketcal.Session, hasMarket, known bool) {
	if !proposalHasContract(prop) {
		return "", marketcal.Session{}, false, false
	}
	if sess, ok := proposalBondSession(prop); ok {
		session, known = bondSessionAt(sess, at)
		return bondSessionMarket, session, true, known
	}
	market, hasMarket = quoteSessionMarketForContract(prop.Contract)
	if !hasMarket {
		return "", marketcal.Session{}, false, false
	}
	if sessions != nil {
		session, known = e.readinessSession(market, at, sessions)
	} else if e != nil && e.server != nil {
		session, known = e.server.previewSession(market, at)
	}
	return market, session, true, known
}

// cashSweepInvestUnits sizes an invest order in the bill's order unit: the
// planned cash amount at the higher of par and the quoted price, in whole
// units, rounded down onto the line's size grid. Pricing at par or above
// keeps both the face value and the cost inside the amount. Zero units come
// with the reason.
func cashSweepInvestUnits(amount float64, conv cashSweepInstrumentConvention, rules ibkrlib.BondOrderRules, price float64, ccy string) (int, string) {
	if conv.FacePerUnit <= 0 || !positiveFinite(price) || rules.Validate() != nil {
		return 0, "the bill cannot be sized: its unit, price or size rules are missing"
	}
	perUnit := conv.FacePerUnit * max(price, 100) / 100
	units := int(math.Floor(amount/perUnit + 1e-9))
	if step := rules.Step(); step > 0 {
		units = units / step * step
	}
	if units < rules.Minimum() || units < 1 {
		return 0, fmt.Sprintf("%s buys less than the bill's minimum order of %d × %s (%s of face)",
			formatBudgetMoney(amount, ccy), rules.Minimum(), conv.QuantityUnit, formatBudgetMoney(float64(rules.Minimum())*conv.FacePerUnit, ccy))
	}
	return units, ""
}

// cashSweepRedeemUnits rounds a sale up onto the line's size grid within
// limit, the units it may sell (the position, held to max_order_notional);
// when rounding up passes the limit it rounds down instead, and a sale of
// the whole limit must itself sit on the grid. Zero units come with the
// reason.
func cashSweepRedeemUnits(want, limit int, rules ibkrlib.BondOrderRules) (int, string) {
	if limit < 1 {
		return 0, "no whole unit of the bill can be sold"
	}
	step := max(rules.Step(), 1)
	units := max(want, rules.Minimum(), 1)
	if units%step != 0 {
		units = (units/step + 1) * step
	}
	if units > limit {
		units = limit
		if down := limit / step * step; down >= rules.Minimum() && down > 0 {
			units = down
		}
	}
	if err := rules.CheckQuantity(units); err != nil {
		return 0, fmt.Sprintf("a sale of %d (at most %d can be sold) cannot meet the bill's size rules: %v", units, limit, err)
	}
	return units, ""
}

// cashSweepOrderTerms are the bill conventions a sweep row's BOND preview
// carries (rpc.OrderPreviewParams.Bond); nil for anything else.
func cashSweepOrderTerms(prop rpc.TradeProposal) *rpc.OrderBondTerms {
	s := prop.CashSweep
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || !strings.EqualFold(strings.TrimSpace(prop.Contract.SecType), "BOND") || !cashSweepIsBill(s.Instrument) {
		return nil
	}
	conv := cashSweepInstrumentConventions[s.Instrument]
	return &rpc.OrderBondTerms{Instrument: s.Instrument, QuantityUnit: conv.QuantityUnit, FacePerUnit: conv.FacePerUnit, PriceConvention: conv.PriceConvention}
}

// closeReduceOnlyException is the one exception to authority.close_reduce_only
// (owner decision O1, 2026-09-30): a cash_sweep buy that opens or increases
// the row's own resolved bill, a vocabulary instrument of the row's currency,
// for no more whole units than the free cash it was planned against. It is
// typed so it cannot widen by accident: every other bucket, action,
// instrument, contract, currency or size stays close-or-reduce only.
type closeReduceOnlyException struct {
	Currency    string
	Instrument  string
	ConID       int
	MaxQuantity int
	FacePerUnit float64
	// MaxCost is the row's free cash in its currency. MaxBaseNotional is the
	// bucket's max_order_notional, compared in base at Rate, the ledger rate
	// the row was planned at (as the policy field says).
	MaxCost         float64
	MaxBaseNotional float64
	Rate            float64
}

// cashSweepOpenException returns the exception prop qualifies for.
func cashSweepOpenException(prop rpc.TradeProposal) (closeReduceOnlyException, bool) {
	s := prop.CashSweep
	none := closeReduceOnlyException{}
	switch {
	case prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || s.Side != rpc.CashSweepSideInvest:
		return none, false
	case !strings.EqualFold(strings.TrimSpace(prop.Action), rpc.OrderActionBuy):
		return none, false
	case prop.PositionEffect != rpc.OrderPositionEffectOpen && prop.PositionEffect != rpc.OrderPositionEffectIncrease:
		return none, false
	case !cashSweepIsBill(s.Instrument) || !cashSweepInstrumentAllowed(s.Instrument, s.Currency):
		return none, false
	case s.Currency == "" || normCcy(prop.Contract.Currency) != s.Currency:
		return none, false
	case s.Bill == nil || s.Bill.ConID <= 0 || s.Bill.ConID != prop.Contract.ConID || s.Bill.Instrument != s.Instrument ||
		!strings.EqualFold(strings.TrimSpace(prop.Contract.SecType), "BOND"):
		return none, false
	case s.MaxOrderNotionalBase <= 0 || !positiveFinite(s.ExchangeRate):
		return none, false
	}
	conv := cashSweepInstrumentConventions[s.Instrument]
	switch {
	case conv.FacePerUnit <= 0 || s.QuantityUnit != conv.QuantityUnit:
		return none, false
	case prop.Quantity < 1 || prop.MaxQuantity < prop.Quantity || float64(prop.MaxQuantity)*conv.FacePerUnit > s.Free+cashSweepMoneyEpsilon:
		return none, false
	}
	return closeReduceOnlyException{Currency: s.Currency, Instrument: s.Instrument, ConID: s.Bill.ConID, MaxQuantity: prop.MaxQuantity,
		FacePerUnit: conv.FacePerUnit, MaxCost: s.Free, MaxBaseNotional: s.MaxOrderNotionalBase, Rate: s.ExchangeRate}, true
}

// previewBlockers says why a preview the close_reduce_only gate would refuse
// is outside the exception; none means it is inside: the row's own bill in
// its currency, opening or increasing, within the planned units, costing no
// more than the free cash at its own limit and, at the row's ledger rate, no
// more than max_order_notional.
func (x closeReduceOnlyException) previewBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if preview == nil {
		return []rpc.TradingBlocker{{Code: "proposal_preview_missing", Message: "proposal preview result is unavailable"}}
	}
	d := preview.Draft
	effect := preview.Position.Effect
	if (effect != rpc.OrderPositionEffectOpen && effect != rpc.OrderPositionEffectIncrease) || d.Quantity < 1 || d.Quantity > x.MaxQuantity ||
		d.Contract.ConID != x.ConID || normCcy(d.Contract.Currency) != x.Currency || !strings.EqualFold(d.Contract.SecType, "BOND") ||
		d.Bond == nil || d.Bond.Instrument != x.Instrument || d.Bond.FacePerUnit != x.FacePerUnit || !positiveFinite(d.LimitPrice) {
		return []rpc.TradingBlocker{{Code: "preview_effect_not_close_reduce",
			Message: fmt.Sprintf("preview effect %q is not close/reduce and is not the sweep row's own bill buy within its planned units", effect),
			Action:  "Refresh proposals and positions and preview the row again."}}
	}
	var out []rpc.TradingBlocker
	cost := float64(d.Quantity) * x.FacePerUnit * d.LimitPrice / 100
	if cost > x.MaxCost+cashSweepMoneyEpsilon {
		out = append(out, rpc.TradingBlocker{Code: "cash_sweep_cost_above_free_cash",
			Message: fmt.Sprintf("the buy costs %s at its limit, above the free cash %s it was planned against", formatBudgetMoney(cost, x.Currency), formatBudgetMoney(x.MaxCost, x.Currency)),
			Action:  "Refresh proposals; the next cycle sizes the buy at the current price."})
	}
	if base := cost * x.Rate; base > x.MaxBaseNotional+cashSweepMoneyEpsilon {
		out = append(out, rpc.TradingBlocker{Code: "cash_sweep_above_max_order_notional",
			Message: fmt.Sprintf("the buy is %s in base at the ledger rate, above max_order_notional %s", formatBudgetMoney(base, "base"), formatBudgetMoney(x.MaxBaseNotional, "base")),
			Action:  "Refresh proposals; the next cycle sizes the buy at the current price."})
	}
	return out
}

// admits reports whether a preview is inside the exception.
func (x closeReduceOnlyException) admits(preview *rpc.OrderPreviewResult) bool {
	return preview != nil && len(x.previewBlockers(preview)) == 0
}

// cashSweepBondAdmitted reports whether a BOND preview belongs to a
// cash_sweep row's own bill: the invest row's resolved bill inside the typed
// exception, or a redemption's held bill sold to reduce or close it.
func cashSweepBondAdmitted(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) bool {
	s := prop.CashSweep
	if preview == nil || prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil ||
		!strings.EqualFold(strings.TrimSpace(prop.SecType), "BOND") || !strings.EqualFold(strings.TrimSpace(preview.Draft.Contract.SecType), "BOND") ||
		prop.Contract.ConID <= 0 || preview.Draft.Contract.ConID != prop.Contract.ConID || preview.Draft.Bond == nil ||
		preview.Draft.Bond.Instrument != s.Instrument || !cashSweepIsBill(s.Instrument) {
		return false
	}
	switch s.Side {
	case rpc.CashSweepSideInvest:
		_, ok := cashSweepOpenException(prop)
		return ok
	case rpc.CashSweepSideRedeem:
		return strings.EqualFold(prop.Action, rpc.OrderActionSell) && s.QuantityUnit == rpc.CashSweepQuantityPosition &&
			normCcy(prop.Contract.Currency) == s.Currency && cashSweepInstrumentAllowed(s.Instrument, s.Currency)
	}
	return false
}
