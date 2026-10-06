package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Currency leveling's order path (internal-docs/design/currency-leveling.md;
// owner decision 2026-10-05 22:02 CEST: real orders, no shadow). A
// conversion is a CASH LMT DAY order on IDEALPRO, on the pair's exact
// contract, for a currency_leveling row only: the proposal engine attaches
// the row's terms (rpc.OrderPreviewParams.FX, which no RPC caller can set),
// the preview reads a live two-sided quote on its own broker session and
// bounds the limit to max_slippage_bp from the mid, and the second typed
// exception to authority.close_reduce_only admits the order only while it
// brings the borrowed currency back no further than its share of the target
// and spends no more of the funding currency than its allotment.
// Every other gate stays: trading mode and freeze, account pins, the order
// cap in force of [order_limits], WhatIf, the token and the journal, and the
// owner's approval of each conversion, or of a loan's conversions as one
// bundle. Leveling is never pre-authorised.

// idealproSessionMarket names IDEALPRO's FX session in readiness; it is no
// exchange calendar.
const idealproSessionMarket marketcal.Market = "idealpro"

// idealproSessionAt reads IBKR's IDEALPRO FX session at an instant: open from
// Sunday 17:15 to Friday 17:00 New York time, closed daily from 17:00 to
// 17:15 (idealproTrading). Holidays are not modelled; the preview's
// live-quote requirement refuses instead.
func idealproSessionAt(at time.Time) (marketcal.Session, bool) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil || at.IsZero() {
		return marketcal.Session{}, false
	}
	out := marketcal.Session{Market: idealproSessionMarket, Label: "IDEALPRO FX", Timezone: "America/New_York", Source: "assumed"}
	local := at.In(loc)
	for i := -1; i <= 7; i++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+i, 0, 0, 0, 0, loc)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		// Each weekday's window opens at 17:15 the evening before.
		closeAt := time.Date(day.Year(), day.Month(), day.Day(), 17, 0, 0, 0, loc)
		openAt := time.Date(day.Year(), day.Month(), day.Day()-1, 17, 15, 0, 0, loc)
		switch {
		case at.Before(openAt):
			open, closeUTC := openAt.UTC(), closeAt.UTC()
			out.State, out.Reason, out.NextOpen, out.NextClose = marketcal.StateClosed, "outside IDEALPRO's hours", &open, &closeUTC
			return out, true
		case at.Before(closeAt):
			out.State, out.IsOpen, out.Open, out.Close = marketcal.StateRegular, true, openAt.UTC(), closeAt.UTC()
			out.Windows = []marketcal.Window{{Open: openAt.UTC(), Close: closeAt.UTC()}}
			return out, true
		}
	}
	return marketcal.Session{}, false
}

// currencyLevelingSessionRefusal refuses a conversion preview, before any
// quote is requested, while IDEALPRO is closed.
func currencyLevelingSessionRefusal(now time.Time) error {
	sess, ok := idealproSessionAt(now)
	if !ok {
		return refusePreview(errBadRequest("IDEALPRO's session cannot be read"), rpc.TradingBlocker{Code: "session_unknown", Message: "IDEALPRO's session cannot be read, so the conversion cannot be priced.", Action: "Preview again shortly."})
	}
	if sess.IsOpen {
		return nil
	}
	message := "a conversion needs IDEALPRO open: " + sessionClosedPhrase(sess)
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{Code: previewMarketClosedCode, Message: message,
		Action: "Preview again once IDEALPRO is open (Sunday 17:15 to Friday 17:00 New York time); no quote was requested."})
}

// normalizePreviewFXContract keeps a conversion's identity: CASH on
// IDEALPRO, a conventional major pair (EUR.USD, never USD.EUR), no option or
// multiplier fields.
func normalizePreviewFXContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	out := rpc.ContractParams{ConID: in.ConID, Symbol: normCcy(in.Symbol), SecType: "CASH", Exchange: strings.ToUpper(strings.TrimSpace(in.Exchange)), Currency: normCcy(in.Currency)}
	if out.Exchange == "" {
		out.Exchange = rpc.CurrencyLevelingExchange
	}
	pair, _, err := canonicalOrderFXContract(out.Symbol, out.Currency)
	switch {
	case out.Exchange != rpc.CurrencyLevelingExchange:
		return rpc.ContractParams{}, errBadRequest("a CASH preview is a conversion on IDEALPRO only")
	case err != nil || pair.Symbol != out.Symbol || pair.Currency != out.Currency:
		return rpc.ContractParams{}, errBadRequest("a CASH preview needs a conventional major pair, such as EUR.USD")
	case in.Expiry != "" || in.Right != "" || in.Strike != 0 || in.Multiplier > 1:
		return rpc.ContractParams{}, errBadRequest("a CASH preview carries no expiry, right, strike or multiplier")
	}
	out.LocalSymbol = out.Symbol + "." + out.Currency
	return out, nil
}

// validatePreviewFXParams admits a CASH preview only in the one shape a
// currency_leveling row asks for: the engine's conversion terms, a new LMT
// DAY order priced inside the slippage bound.
func validatePreviewFXParams(p rpc.OrderPreviewParams, replace bool) error {
	switch {
	case p.FX == nil:
		return errBadRequest("order preview admits CASH only for a currency_leveling proposal row; preview it with `canary proposals preview`")
	case replace:
		return errBadRequest("a working conversion cannot be modified through preview; cancel it and preview the row again")
	case !positiveFinite(p.FX.MaxSlippageBP) || p.FX.MaxSlippageBP > currencyLevelingMaxSlippageBP:
		return errBadRequest("the conversion's slippage bound is missing or out of range")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.OrderType), rpc.OrderTypeLMT), rpc.OrderTypeLMT):
		return errBadRequest("a conversion is a limit order")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.TIF), rpc.OrderTIFDay), rpc.OrderTIFDay):
		return errBadRequest("a conversion is a DAY order")
	case p.OutsideRTH || p.Trail != nil || p.LimitPrice != nil || p.Bounded != nil || p.ResolvedStrategy != nil || p.Bond != nil:
		return errBadRequest("a conversion's limit comes from the live quote and its bound: no limit, trail, bound, bond terms or outside_rth")
	}
	return nil
}

// fetchPreviewFXQuote reads the pair's live bid and ask on the preview's own
// broker session; both sides must arrive after the request.
func (s *Server) fetchPreviewFXQuote(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	if s.orderPreviewQuote != nil {
		return s.orderPreviewQuote(ctx, contract, timeout)
	}
	if authority == nil || authority.connector == nil || contract.ConID <= 0 || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: exact broker FX quote authority is unavailable", ErrTradingDisabled)
	}
	return s.previewExactSessionContractQuote(ctx, authority, contract, timeout, true)
}

// fxBoundedLimitPrice prices a conversion from a live two-sided quote read
// during this preview: the mid moved max_slippage_bp against the order
// (a sell below it, a buy above it), snapped to the pair's tick toward the
// mid. The order is marketable only while the touch lies inside the bound;
// a wider market refuses, so no fill can be worse than the bound.
func fxBoundedLimitPrice(action string, tick float64, quote rpc.OrderQuoteSnapshot, slipBP float64) (float64, error) {
	switch {
	case quote.DataType != rpc.MarketDataLive:
		return 0, refusePreviewCode(previewQuoteNotLiveCode, errBadRequest("a conversion needs a live IDEALPRO bid and ask"))
	case quote.PriceAt.IsZero() || quote.Stale:
		return 0, refusePreviewCode(previewQuoteStaleCode, errBadRequest("a conversion needs a bid and ask received during this preview"))
	case quote.Bid == nil || quote.Ask == nil || !positiveFinite(*quote.Bid) || !positiveFinite(*quote.Ask) || *quote.Ask <= *quote.Bid:
		return 0, refusePreviewCode(previewQuoteNotTwoSidedCode, errBadRequest("a conversion needs a positive two-sided bid and ask"))
	case !positiveFinite(slipBP):
		return 0, errBadRequest("the conversion's slippage bound is missing")
	}
	if !positiveFinite(tick) {
		tick = currencyLevelingFallbackTick
	}
	bid, ask := *quote.Bid, *quote.Ask
	mid := (bid + ask) / 2
	slip := slipBP / 10000
	var limit float64
	switch action {
	case rpc.OrderActionSell:
		limit = ibkrlib.BondTickCeil(mid*(1-slip), tick)
		if limit > bid+1e-12 {
			return 0, currencyLevelingWideQuote(bid, ask, slipBP)
		}
	case rpc.OrderActionBuy:
		limit = ibkrlib.BondTickFloor(mid*(1+slip), tick)
		if limit < ask-1e-12 {
			return 0, currencyLevelingWideQuote(bid, ask, slipBP)
		}
	default:
		return 0, errBadRequest("action must be buy or sell")
	}
	return limit, nil
}

// currencyLevelingFallbackTick is the price grid used when contract details
// carry no minimum tick: one hundred-thousandth, finer than any IDEALPRO
// major pair's own tick, so rounding only ever moves the limit toward the mid.
const currencyLevelingFallbackTick = 0.00001

// currencyLevelingWideQuoteCode refuses a conversion whose market is wider
// than the slippage bound allows.
const currencyLevelingWideQuoteCode = "fx_spread_beyond_bound"

func currencyLevelingWideQuote(bid, ask, slipBP float64) error {
	message := fmt.Sprintf("the IDEALPRO quote %s/%s is wider than max_slippage_bp %s from its mid, so a limit inside the bound would not fill",
		strconv.FormatFloat(bid, 'f', -1, 64), strconv.FormatFloat(ask, 'f', -1, 64), strconv.FormatFloat(slipBP, 'f', -1, 64))
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{Code: currencyLevelingWideQuoteCode, Message: message,
		Action: "Preview again once the market narrows; a wide quote is common around 17:00 New York time and at weekends."})
}

// currencyLevelingOrderTerms are the conversion terms a currency_leveling
// row's CASH preview carries; nil for anything else.
func currencyLevelingOrderTerms(prop rpc.TradeProposal) *rpc.OrderFXTerms {
	b := prop.CurrencyLeveling
	if prop.Bucket != rpc.TradeProposalBucketCurrencyLeveling || b == nil || !strings.EqualFold(prop.Contract.SecType, "CASH") {
		return nil
	}
	return &rpc.OrderFXTerms{Currency: b.Currency, FundingCurrency: b.FundingCurrency, Cash: b.Cash, Target: b.Target,
		FundingCash: b.Allotment, MaxSlippageBP: b.MaxSlippageBP}
}

// currencyLevelingSingleApprovalBlockers refuses to prepare or send, on its
// own, a conversion that is one of several repaying one loan: the bundle is
// approved and sent as a whole (prepare_bundle, submit_bundle), so a part
// never lands without the rest it was planned beside.
func currencyLevelingSingleApprovalBlockers(prop rpc.TradeProposal) []rpc.TradingBlocker {
	b := prop.CurrencyLeveling
	if prop.Bucket != rpc.TradeProposalBucketCurrencyLeveling || b == nil || b.Legs <= 1 {
		return nil
	}
	return []rpc.TradingBlocker{{Code: rpc.CurrencyLevelingBlockerBundle,
		Message: fmt.Sprintf("this conversion is %d of %d that repay the %s loan together; they are approved and sent as one", b.Leg, b.Legs, b.Currency),
		Action:  "Approve the repayment as a whole (prepare_bundle, then submit_bundle)."}}
}

// currencyLevelingException is the second exception to
// authority.close_reduce_only (owner decision 2026-10-05 22:02 CEST): a
// currency_leveling conversion that only reduces a negative currency
// balance. The order's position effect on the pair says nothing about a
// currency balance, so the exception judges the conversion against the
// ledger terms the row was planned from: the borrowed currency's trade-date
// cash and this conversion's share of the target, and its allotment of the
// funding currency. It is typed so it cannot widen by accident: every other
// bucket, instrument, venue, side or size stays close-or-reduce only.
type currencyLevelingException struct {
	Currency, Funding, Symbol, PairCurrency, Action string
	// ConID is the pair's exact contract, the one the owner reviews.
	ConID, MaxQuantity int
	// Cash is the borrowed currency's trade-date cash (negative) and Target
	// the most this conversion may bring it to, in its own unit; Available is
	// the conversion's allotment of the funding currency. The order cap in
	// force is the preview's own gate, so the exception carries none.
	Cash, Target, Available, SlipBP float64
}

// currencyLevelingReduceException returns the exception prop qualifies for.
func currencyLevelingReduceException(prop rpc.TradeProposal) (currencyLevelingException, bool) {
	b := prop.CurrencyLeveling
	none := currencyLevelingException{}
	if prop.Bucket != rpc.TradeProposalBucketCurrencyLeveling || b == nil || prop.Shadow {
		return none, false
	}
	c := prop.Contract
	pair, _, err := canonicalOrderFXContract(b.Currency, b.FundingCurrency)
	wantAction := rpc.OrderActionBuy
	if b.Currency == pair.Currency {
		wantAction = rpc.OrderActionSell
	}
	switch {
	case err != nil || !strings.EqualFold(c.SecType, "CASH") || !strings.EqualFold(prop.SecType, "CASH") || !strings.EqualFold(c.Exchange, rpc.CurrencyLevelingExchange) || c.ConID <= 0:
		return none, false
	case normCcy(c.Symbol) != pair.Symbol || normCcy(c.Currency) != pair.Currency || b.PairSymbol != pair.Symbol || b.PairCurrency != pair.Currency:
		return none, false
	case !strings.EqualFold(strings.TrimSpace(prop.Action), wantAction) || prop.PositionEffect != rpc.OrderPositionEffectReduce:
		return none, false
	case prop.Quantity < 1 || prop.MaxQuantity < prop.Quantity:
		return none, false
	case !finiteProtectionOptionPolicyValue(b.Cash) || b.Cash >= 0 || !finiteProtectionOptionPolicyValue(b.Target) || b.Target <= b.Cash:
		// A conversion repays a negative balance and must bring something
		// in; one of several may stop short of zero, the bundle does not.
		return none, false
	case !positiveFinite(b.ExchangeRate) || !positiveFinite(b.MaxSlippageBP) || b.MaxSlippageBP > currencyLevelingMaxSlippageBP:
		return none, false
	case b.Target > b.CushionBase/b.ExchangeRate+cashSweepMoneyEpsilon:
		// A share of the target never reaches past the policy cushion.
		return none, false
	case b.Legs < 1 || b.Leg < 1 || b.Leg > b.Legs:
		return none, false
	}
	// The allotment is the most the conversion may spend: positive, and
	// never more than the funding currency's cash less what working and
	// armed buys hold of it.
	available := b.Allotment
	if !positiveFinite(available) || available > b.FundingCash-b.FundingCommitted+cashSweepMoneyEpsilon {
		return none, false
	}
	return currencyLevelingException{Currency: b.Currency, Funding: b.FundingCurrency, Symbol: pair.Symbol, PairCurrency: pair.Currency, Action: wantAction,
		ConID: c.ConID, MaxQuantity: prop.MaxQuantity, Cash: b.Cash, Target: b.Target, Available: available, SlipBP: b.MaxSlippageBP}, true
}

// previewBlockers says why a conversion preview the close_reduce_only gate
// would refuse is outside the exception; none means it is inside: the row's
// own pair, side and venue, a LMT DAY order within the planned quantity,
// its limit inside the slippage bound of the quote it was priced from, and,
// at that quote's far side, no more of the borrowed currency than its share
// of the target and no more of the funding currency than its allotment.
func (x currencyLevelingException) previewBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if preview == nil {
		return []rpc.TradingBlocker{{Code: "proposal_preview_missing", Message: "proposal preview result is unavailable"}}
	}
	d := preview.Draft
	drift := func(what string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: rpc.CurrencyLevelingBlockerOrderTerms,
			Message: "the previewed order is not the row's conversion: " + what,
			Action:  "Refresh proposals and preview the row again."}}
	}
	switch {
	case !strings.EqualFold(d.Contract.SecType, "CASH") || !strings.EqualFold(d.Contract.Exchange, rpc.CurrencyLevelingExchange) ||
		normCcy(d.Contract.Symbol) != x.Symbol || normCcy(d.Contract.Currency) != x.PairCurrency || d.Contract.ConID != x.ConID:
		return drift("contract or venue")
	case !strings.EqualFold(d.Action, x.Action):
		return drift("side")
	case d.Quantity < 1 || d.Quantity > x.MaxQuantity:
		return drift(fmt.Sprintf("quantity %d outside 1 to %d", d.Quantity, x.MaxQuantity))
	case !strings.EqualFold(d.OrderType, rpc.OrderTypeLMT) || !strings.EqualFold(d.TIF, rpc.OrderTIFDay) || d.OutsideRTH || d.Trail != nil:
		return drift("order type or time in force")
	case d.FX == nil || d.FX.Bid == nil || d.FX.Ask == nil || !positiveFinite(*d.FX.Bid) || !positiveFinite(*d.FX.Ask) || *d.FX.Ask <= *d.FX.Bid || !positiveFinite(d.LimitPrice):
		return drift("no live two-sided quote or limit")
	}
	bid, ask, limit := *d.FX.Bid, *d.FX.Ask, d.LimitPrice
	mid := (bid + ask) / 2
	if math.Abs(limit-mid) > mid*x.SlipBP/10000+currencyLevelingFallbackTick {
		return drift(fmt.Sprintf("limit %s is further than %s bp from the mid %s", strconv.FormatFloat(limit, 'f', -1, 64), strconv.FormatFloat(x.SlipBP, 'f', -1, 64), strconv.FormatFloat(mid, 'f', -1, 64)))
	}
	q := float64(d.Quantity)
	// The most the conversion can bring in and spend: a sell fills at its
	// limit or better, but not above the ask it saw; a buy brings in exactly
	// its quantity and pays at most its limit.
	received, spent := q, q*limit
	if x.Action == rpc.OrderActionSell {
		received, spent = q*max(ask, limit), q
	}
	var out []rpc.TradingBlocker
	if x.Cash+received > x.Target+cashSweepMoneyEpsilon {
		out = append(out, rpc.TradingBlocker{Code: rpc.CurrencyLevelingBlockerBeyondTarget,
			Message: fmt.Sprintf("at the quote's far side the conversion brings in up to %s, taking %s to %s, above its share of the target, %s",
				currencyLevelingMoney(received, x.Currency, false), x.Currency, currencyLevelingMoney(x.Cash+received, x.Currency, true), currencyLevelingMoney(x.Target, x.Currency, true)),
			Action: "Refresh proposals; the next cycle sizes the conversion at the current rate."})
	}
	if spent > x.Available+cashSweepMoneyEpsilon {
		out = append(out, rpc.TradingBlocker{Code: rpc.CurrencyLevelingBlockerFundingShort,
			Message: fmt.Sprintf("the conversion spends up to %s, more than the %s allotted to it",
				currencyLevelingMoney(spent, x.Funding, false), currencyLevelingMoney(x.Available, x.Funding, false)),
			Action: "Refresh proposals; leveling never borrows one currency to repay another."})
	}
	return out
}

// currencyLevelingAdmitted reports whether a CASH preview passes the
// security-type check: only a currency_leveling row's own conversion inside
// its exception.
func currencyLevelingAdmitted(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) bool {
	x, ok := currencyLevelingReduceException(prop)
	return ok && preview != nil && strings.EqualFold(preview.Draft.Contract.SecType, "CASH") && len(x.previewBlockers(preview)) == 0
}

// currencyLevelingOrderBlockers holds a conversion's preview or submit while
// another conversion in either of its currencies is working at the broker,
// from any client, or was sent by Canary and is not yet acknowledged: a
// conversion is never sent twice, and the ledger it was planned from may
// not show the first one yet.
func (e *proposalEngine) currencyLevelingOrderBlockers(ctx context.Context, p rpc.TradeProposal, forceCurrent bool) []rpc.TradingBlocker {
	b := p.CurrencyLeveling
	if b == nil {
		return []rpc.TradingBlocker{{Code: rpc.CurrencyLevelingBlockerOrderTerms, Message: "the row carries no conversion terms", Action: "Refresh proposals."}}
	}
	unavailable := func(message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: reductionOrderUnavailableCode, Message: message, Action: "Refresh and retry once complete, current broker open-order evidence is available."}}
	}
	snapshot, scope, err := e.server.brokerOpenOrderInventory(ctx, forceCurrent)
	if err != nil {
		return unavailable("complete, current open-order inventory from every client is unavailable, so a conversion already working cannot be ruled out")
	}
	working := currencyLevelingWorkingFrom(snapshot.Orders, scope)
	if working[b.Currency] || working[b.FundingCurrency] {
		return []rpc.TradingBlocker{{Code: rpc.CurrencyLevelingBlockerWorking,
			Message: fmt.Sprintf("a conversion in %s or %s is already working at the broker; this row waits until it fills or is cancelled, then recomputes from the new balances", b.Currency, b.FundingCurrency),
			Action:  "Keep the working conversion, or cancel it at the broker first; this row then recomputes."}}
	}
	views, _, err := e.server.loadOrderViews()
	switch {
	case errors.Is(err, ErrTradingDisabled):
		return nil
	case err != nil:
		return unavailable("Canary's own order journal is unreadable, so a conversion it sent a moment ago cannot be ruled out")
	}
	for _, v := range views {
		if !v.Open || v.SendState != orderSendStateSendAttempted && v.SendState != orderSendStateUncertainSend {
			continue
		}
		if strings.EqualFold(v.SecType, "CASH") && orderViewRemainingQuantity(v) > 0 && orderViewMatchesBrokerScope(v, scope) {
			return []rpc.TradingBlocker{{Code: rpc.CurrencyLevelingBlockerWorking,
				Message: "Canary sent a conversion the broker has not confirmed yet; this row waits until it fills or is cancelled, then recomputes",
				Action:  "Wait for the broker's answer on that order; this row then recomputes."}}
		}
	}
	return nil
}
