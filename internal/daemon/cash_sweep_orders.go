package daemon

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The cash sweep's order path (internal-docs/design/cash-sweep.md, Phase B).
// An invest row buys its resolved bill in whole order units on the line's
// size grid; a redemption sells a held bill on the same grid. Both are BILL
// or BOND (the line's own type) LMT DAY orders priced per 100 of face on the line's minimum tick, previewed
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
		!ibkrlib.IsBillOrBond(prop.Contract.SecType) {
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
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || !ibkrlib.IsBillOrBond(prop.Contract.SecType) || !cashSweepIsBill(s.Instrument) {
		return nil
	}
	conv := cashSweepInstrumentConventions[s.Instrument]
	terms := &rpc.OrderBondTerms{Instrument: s.Instrument, QuantityUnit: conv.QuantityUnit, FacePerUnit: conv.FacePerUnit, PriceConvention: conv.PriceConvention,
		Maturity: s.MaturityDate, MaturitySource: s.MaturitySource, CUSIP: s.CUSIP, ISIN: s.ISIN, ResolutionSource: s.ResolutionSource}
	if b := s.Bill; b != nil {
		terms.Maturity, terms.MaturitySource, terms.CUSIP, terms.ISIN = b.Maturity, b.MaturitySource, b.CUSIP, b.ISIN
		terms.ResolutionSource = b.ResolutionSource
	}
	return terms
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
		!ibkrlib.IsBillOrBond(prop.Contract.SecType):
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
// more than the free cash at its own limit including the broker's upper fee
// envelope and, at the row's ledger rate, principal no more than max_order_notional.
func (x closeReduceOnlyException) previewBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if preview == nil {
		return []rpc.TradingBlocker{{Code: "proposal_preview_missing", Message: "proposal preview result is unavailable"}}
	}
	d := preview.Draft
	effect := preview.Position.Effect
	if (effect != rpc.OrderPositionEffectOpen && effect != rpc.OrderPositionEffectIncrease) || d.Quantity < 1 || d.Quantity > x.MaxQuantity ||
		d.Contract.ConID != x.ConID || normCcy(d.Contract.Currency) != x.Currency || !ibkrlib.IsBillOrBond(d.Contract.SecType) ||
		d.Bond == nil || d.Bond.Instrument != x.Instrument || d.Bond.FacePerUnit != x.FacePerUnit || !positiveFinite(d.LimitPrice) {
		return []rpc.TradingBlocker{{Code: "preview_effect_not_close_reduce",
			Message: fmt.Sprintf("preview effect %q or order terms do not match the sweep row's planned bill buy within its units", effect),
			Action:  "Refresh proposals and positions and preview the row again."}}
	}
	var out []rpc.TradingBlocker
	cost := float64(d.Quantity) * x.FacePerUnit * d.LimitPrice / 100
	if !positiveFinite(cost) || !positiveFinite(x.MaxCost) || cost > x.MaxCost+cashSweepMoneyEpsilon {
		out = append(out, rpc.TradingBlocker{Code: "cash_sweep_cost_above_free_cash",
			Message: fmt.Sprintf("the buy costs %s at its limit, above the free cash %s it was planned against", formatBudgetMoney(cost, x.Currency), formatBudgetMoney(x.MaxCost, x.Currency)),
			Action:  "Refresh proposals; the next cycle sizes the buy at the current price."})
	} else {
		out = append(out, cashSweepFeeReserveBlockers(preview, cost, x.MaxCost, x.Currency)...)
	}
	out = append(out, cashSweepNotionalBlockers(cost, x.Rate, x.MaxBaseNotional)...)
	return out
}

// cashSweepPreviewNotionalBlockers applies the bucket's one-order cap to
// the actual reviewed draft on every proposal path. A redemption's current
// limit can differ from the mark used to size it; its position quantity is
// not permission to sell beyond the planned tranche. Both sides use the
// bill's pinned unit convention and the proposal's ledger FX rate.
func cashSweepPreviewNotionalBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if prop.Bucket != rpc.TradeProposalBucketCashSweep {
		return nil
	}
	s := prop.CashSweep
	if s == nil || preview == nil || preview.Draft.Bond == nil ||
		!cashSweepIsBill(s.Instrument) || !ibkrlib.IsBillOrBond(preview.Draft.Contract.SecType) {
		return cashSweepNotionalBlockers(0, 0, 0)
	}
	d := preview.Draft
	conv := cashSweepInstrumentConventions[s.Instrument]
	if d.Quantity < 1 || !positiveFinite(d.LimitPrice) || d.Bond.Instrument != s.Instrument ||
		d.Bond.FacePerUnit != conv.FacePerUnit || d.Bond.QuantityUnit != conv.QuantityUnit ||
		normCcy(d.Contract.Currency) != s.Currency {
		return cashSweepNotionalBlockers(0, s.ExchangeRate, s.MaxOrderNotionalBase)
	}
	return cashSweepNotionalBlockers(bondOrderNotional(d.Quantity, d.Bond, d.LimitPrice), s.ExchangeRate, s.MaxOrderNotionalBase)
}

func cashSweepNotionalBlockers(notional, rate, capBase float64) []rpc.TradingBlocker {
	base := notional * rate
	if !positiveFinite(notional) || !positiveFinite(rate) || !positiveFinite(capBase) || !positiveFinite(base) {
		return []rpc.TradingBlocker{{Code: "cash_sweep_notional_unknown",
			Message: "the sweep order's value, ledger exchange rate or max_order_notional is unavailable or nonfinite",
			Action:  "Refresh proposals and preview again with complete bill terms and a finite order cap."}}
	}
	if base > capBase+cashSweepMoneyEpsilon {
		return []rpc.TradingBlocker{{Code: "cash_sweep_above_max_order_notional",
			Message: fmt.Sprintf("the sweep order is %s in base at the ledger rate, above max_order_notional %s", formatBudgetMoney(base, "base"), formatBudgetMoney(capBase, "base")),
			Action:  "Refresh proposals; the next cycle sizes the order at the current price."}}
	}
	return nil
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
		!ibkrlib.IsBillOrBond(prop.SecType) || !ibkrlib.IsBillOrBond(preview.Draft.Contract.SecType) ||
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

// The band the WhatIf's initial-margin change ÷ an invest order's expected
// value must lie in before the assumed quantity unit is accepted (reviewer
// decisions 2026-09-30 15:25 and 15:45 CEST; assumption A9). The account is
// a margin account: a bill margined at one percent reads about 0.01, a cash
// account about 1.0, and a 1,000-fold unit error near 10 or near 0.00001.
const (
	cashSweepUnitRatioMin = 0.005
	cashSweepUnitRatioMax = 1.2
)

// cashSweepBillUnitCheck compares an invest bill preview's accepted WhatIf
// with the order's expected value at the assumed unit (face × limit / 100).
// IBKR's WhatIf sends no order cost for a bond, so the figure is its
// initial-margin change (after − before), read in the margin currency: the
// account base (the order's base notional) or the contract currency (its
// notional). checked is false when there is nothing to check (another row,
// a redemption, whose quantity is the broker's own position count, or a
// WhatIf that was not accepted, which the submit-eligibility gate refuses
// anyway); mismatch is true when their ratio falls outside the band or the
// figure cannot be read, and the blocker says which.
func cashSweepBillUnitCheck(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) (blocker rpc.TradingBlocker, mismatch, checked bool) {
	s := prop.CashSweep
	if preview == nil || prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || s.Side != rpc.CashSweepSideInvest ||
		!ibkrlib.IsBillOrBond(preview.Draft.Contract.SecType) || preview.Draft.Bond == nil ||
		preview.WhatIf.Status != rpc.OrderWhatIfStatusAccepted {
		return rpc.TradingBlocker{}, false, false
	}
	terms := preview.Draft.Bond
	unit := fmt.Sprintf("%s (%s of face per unit)", terms.QuantityUnit, formatBudgetMoney(terms.FacePerUnit, preview.Draft.Contract.Currency))
	block := func(message string) (rpc.TradingBlocker, bool, bool) {
		return rpc.TradingBlocker{Code: rpc.CashSweepBlockerBillUnitMismatch, Message: message,
			Action: "Do not submit: check the bill's quantity unit against the broker (post-install proof steps 6–7). The row stays blocked until a preview checks clean."}, true, true
	}
	m := preview.WhatIf.Margin
	if m == nil || m.InitialMarginBefore == nil || m.InitialMarginAfter == nil ||
		math.IsNaN(*m.InitialMarginBefore) || math.IsNaN(*m.InitialMarginAfter) || math.IsInf(*m.InitialMarginBefore, 0) || math.IsInf(*m.InitialMarginAfter, 0) {
		return block(fmt.Sprintf("the broker's WhatIf carried no initial-margin change, so the assumed unit %s cannot be checked against the broker's own figures", unit))
	}
	broker := math.Abs(*m.InitialMarginAfter - *m.InitialMarginBefore)
	marginCcy := normCcy(m.Currency)
	var expected float64
	var ccy string
	switch {
	case marginCcy == "" || marginCcy == normCcy(preview.BaseCurrency):
		expected, ccy = preview.NotionalBase, nonEmptyString(normCcy(preview.BaseCurrency), marginCcy)
	case marginCcy == normCcy(nonEmptyString(preview.NotionalCurrency, preview.Draft.Contract.Currency)):
		expected, ccy = preview.Notional, marginCcy
	default:
		return block(fmt.Sprintf("the broker's WhatIf reports its initial-margin change in %s, neither the account base nor the bill's currency, so the assumed unit %s cannot be checked", marginCcy, unit))
	}
	if ratio := broker / expected; !positiveFinite(expected) || !positiveFinite(broker) || ratio < cashSweepUnitRatioMin || ratio > cashSweepUnitRatioMax {
		return block(fmt.Sprintf("the broker's WhatIf initial-margin change %s is %s of the order's expected value %s at the assumed unit %s, outside the band %s to %s; the unit may be wrong",
			formatBudgetMoney(broker, ccy), strconv.FormatFloat(ratio, 'g', 3, 64), formatBudgetMoney(expected, ccy), unit,
			strconv.FormatFloat(cashSweepUnitRatioMin, 'f', -1, 64), strconv.FormatFloat(cashSweepUnitRatioMax, 'f', -1, 64)))
	}
	return rpc.TradingBlocker{}, false, true
}

// cashSweepBillUnitKey names the unit assumption a check speaks for: one
// instrument in one currency.
func cashSweepBillUnitKey(ccy, instrument string) string {
	return normCcy(ccy) + "|" + instrument
}

// cashSweepBillUnitLatch remembers, per currency and instrument, the last
// invest preview whose WhatIf disagreed with the assumed unit. Rows of that
// instrument carry bill_unit_mismatch until a preview of it checks clean, so
// no submit (by hand, prepared or pre-authorised) can pass meanwhile. It is
// the daemon's memory: a restart forgets it, and every submit previews and
// checks again anyway.
type cashSweepBillUnitLatch struct {
	mu      sync.Mutex
	blocked map[string]rpc.TradingBlocker
}

func (l *cashSweepBillUnitLatch) note(key string, blocker rpc.TradingBlocker, mismatch bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !mismatch {
		delete(l.blocked, key)
		return
	}
	if l.blocked == nil {
		l.blocked = map[string]rpc.TradingBlocker{}
	}
	l.blocked[key] = blocker
}

func (l *cashSweepBillUnitLatch) get(key string) (rpc.TradingBlocker, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.blocked[key]
	return b, ok
}

// noteBillUnitCheck records a fresh preview's unit check in the latch.
func (e *proposalEngine) noteBillUnitCheck(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) {
	if e == nil {
		return
	}
	if b, mismatch, checked := cashSweepBillUnitCheck(prop, preview); checked {
		e.billUnits.note(cashSweepBillUnitKey(prop.CashSweep.Currency, prop.CashSweep.Instrument), b, mismatch)
	}
}

// applyBillUnitLatch blocks an invest row whose instrument a preview found
// off its assumed unit.
func (e *proposalEngine) applyBillUnitLatch(p *rpc.TradeProposal) {
	if e == nil || p.CashSweep == nil || p.CashSweep.Side != rpc.CashSweepSideInvest {
		return
	}
	if b, ok := e.billUnits.get(cashSweepBillUnitKey(p.CashSweep.Currency, p.CashSweep.Instrument)); ok {
		cashSweepBlock(p, b)
	}
}

// withoutBillUnitLatch drops the latch's blocker from the blockers a preview
// resolves with: only a preview can clear it, so it never refuses one.
// Submits keep it.
func withoutBillUnitLatch(blockers []rpc.TradingBlocker) []rpc.TradingBlocker {
	return slices.DeleteFunc(slices.Clone(blockers), func(b rpc.TradingBlocker) bool {
		return b.Code == rpc.CashSweepBlockerBillUnitMismatch
	})
}
