package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The index hedge (owner decisions 2026-10-28 13:17 CET): sell Micro E-mini
// S&P 500 futures until a 10% index fall leaves the book about even. It is
// sized on the mandate's index stress of the whole book, every line counted,
// locked hedges too: neutral is the book's own change in the fall, and a
// hedge the owner keeps pays in it. Whole contracts, rounded toward less
// hedge, so the book never ends short of the index. A plan only: no order
// path carries a future yet.

// hedgeRollWindow skips a contract in its expiry week: the hedge is sold in
// the next quarterly before the front one's last week begins.
const hedgeRollWindow = 7 * 24 * time.Hour

// hedgeFutureSymbol is the hedge's contract root on CME.
const hedgeFutureSymbol = "MES"

// reduceHedgeFuture reads the front Micro S&P future outside its expiry
// week and its quote.
func (s *Server) reduceHedgeFuture(ctx context.Context) (rpc.ContractParams, *rpc.Quote, error) {
	if s.reduceHedgeFutureForTest != nil {
		return s.reduceHedgeFutureForTest(ctx)
	}
	c := s.gatewayConnector()
	if c == nil {
		return rpc.ContractParams{}, nil, s.gatewayUnavailableError()
	}
	fut, err := c.FrontFuture(ctx, ibkrlib.Contract{Symbol: hedgeFutureSymbol, SecType: "FUT", Exchange: "CME", Currency: "USD"}, s.nowUTC().Add(hedgeRollWindow))
	if err != nil {
		return rpc.ContractParams{}, nil, err
	}
	contract := rpc.ContractParams{ConID: fut.ConID, Symbol: fut.Symbol, SecType: fut.SecType, Exchange: fut.Exchange, Currency: fut.Currency,
		Expiry: fut.Expiry, LocalSymbol: fut.LocalSymbol, TradingClass: fut.TradingClass, Multiplier: fut.Multiplier}
	raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: contract, TimeoutMs: reduceLegQuoteTimeoutMs})
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	return contract, q, err
}

// hedgeQuotePrice is the price the hedge is sized at: the quote's mark,
// else its selected price, else the mid, else the last trade.
func hedgeQuotePrice(q *rpc.Quote) float64 {
	if q == nil {
		return 0
	}
	for _, v := range []*float64{q.Mark, q.Price} {
		if v != nil && *v > 0 {
			return *v
		}
	}
	if q.Bid != nil && q.Ask != nil && *q.Bid > 0 && *q.Ask >= *q.Bid {
		return (*q.Bid + *q.Ask) / 2
	}
	if q.Last != nil && *q.Last > 0 {
		return *q.Last
	}
	return 0
}

// reduceHedgePlan sizes the index hedge and scores it as a sweep plan is
// scored: the index fall and rise, the loss measure and the trading cost,
// on the book now and with the hedge sold. Nothing to hedge, or less than
// one contract, is a blocker with the book's figures unchanged.
func (s *Server) reduceHedgePlan(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) *rpc.TradeProposalReducePortfolioResult {
	res := &rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeHedge, PlanOnly: p.PlanOnly, AsOf: s.reduceClock(), Legs: []rpc.TradeProposalReduceLeg{}}
	block := func(code, msg, action string) *rpc.TradeProposalReducePortfolioResult {
		res.Blockers = append(res.Blockers, rpc.TradingBlocker{Code: code, Message: msg, Action: action})
		return res
	}
	unknown := func(why string) {
		res.IndexLoss, res.PlanLossMeasure, res.TradeCostUnknown = indexLossUnknown(why), planLossMeasureUnknown(why), why
	}
	switch {
	case len(p.Sectors) > 0 || p.MinBeta != 0 || len(p.ProposalKeys) > 0 || p.Percent != 0 && p.Percent != 100:
		return block("bad_request", "an index hedge is sized to neutral on the whole book; percent, sector, beta and proposals do not apply", "Ask for the hedge alone.")
	}
	input, why := s.reduceMandateInput(ctx)
	if input == nil {
		unknown(why)
		return block("hedge_unsized", "the hedge cannot be sized: "+why, "Retry once the mandate has measured the book.")
	}
	res.BaseCurrency = strings.ToUpper(input.BaseCurrency)
	s.reduceHedgeFlags(ctx, res, input)
	now := mandate.Evaluate(*input)
	if now.Stresses == nil {
		why := "the mandate cannot measure the book"
		if len(now.Reasons) > 0 {
			why = now.Reasons[0]
		}
		unknown(why)
		return block("hedge_unsized", "the hedge cannot be sized: "+why, "Retry once the mandate has measured the book.")
	}
	unchanged := func() {
		res.IndexLoss, res.PlanLossMeasure, res.TradeCostBase = indexLossBetween(*input, *input), planLossMeasureBetween(*input, *input), new(0.0)
	}
	fall := now.Stresses.IndexLossBase
	if fall <= 0 {
		unchanged()
		return block("nothing_to_hedge", fmt.Sprintf("a %g%% S&P fall already leaves the book even or ahead; selling futures would add a short", mandate.IndexShockPct), "Keep the book as it is.")
	}
	contract, quote, err := s.reduceHedgeFuture(ctx)
	if err != nil {
		unchanged()
		return block("hedge_contract_unavailable", "the Micro S&P future could not be read: "+err.Error(), "Retry once the gateway quotes it.")
	}
	price := hedgeQuotePrice(quote)
	fx := 1.0
	if ccy := normCcy(contract.Currency); ccy != res.BaseCurrency {
		fx = input.FXNow[ccy]
	}
	mult := float64(contract.Multiplier)
	switch {
	case price <= 0:
		unchanged()
		return block("quote_unavailable", contract.LocalSymbol+" has no price to size the hedge", "Retry once the market quotes it.")
	case fx <= 0 || mult <= 0:
		unchanged()
		return block("hedge_unsized", contract.LocalSymbol+" has no multiplier or rate to the base currency", "Retry once the positions carry the rate.")
	}
	perContract := mandate.IndexShockPct / 100 * price * mult * fx
	n := int(math.Floor(fall/perContract + 1e-9))
	if n < 1 {
		unchanged()
		return block("below_one_contract", fmt.Sprintf("the book loses %s in a %g%% fall; one %s contract offsets %s, so even one would hedge past neutral",
			mandateMoney(fall, res.BaseCurrency), mandate.IndexShockPct, contract.LocalSymbol, mandateMoney(perContract, res.BaseCurrency)), "Keep the book as it is.")
	}
	id := mandateConLegID(contract.ConID)
	held := 0.0
	for _, l := range input.Legs {
		if l.ID == id {
			held += l.Quantity
		}
	}
	value := float64(n) * price * mult
	leg := rpc.TradeProposalReduceLeg{ConID: contract.ConID, Symbol: normSym(contract.Symbol), SecType: "FUT", Action: rpc.OrderActionSell, PositionQuantity: held,
		ReduceQuantity: n, DollarDelta: -value * fx, Notional: value, NotionalCurrency: normCcy(contract.Currency), NotionalBase: new(value * fx), Contract: &contract}
	res.Legs = append(res.Legs, leg)
	res.LegCount = 1
	hedge := mandate.Leg{ID: id, Symbol: contract.LocalSymbol, Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock, Quantity: -float64(n), Multiplier: mult,
		Currency: normCcy(contract.Currency), Spot: price}
	after := mandateInputWithLegs(*input, []mandate.Leg{hedge})
	res.IndexLoss, res.PlanLossMeasure = indexLossBetween(*input, after), planLossMeasureBetween(*input, after)
	if quote != nil && quote.Bid != nil && quote.Ask != nil && *quote.Bid > 0 && *quote.Ask >= *quote.Bid {
		res.TradeCostBase = new((*quote.Ask - *quote.Bid) / 2 * float64(n) * mult * fx)
	} else {
		res.TradeCostUnknown = contract.LocalSymbol + " has no two-sided quote"
	}
	res.TotalNotional = value * fx
	res.Message = fmt.Sprintf("sell %d %s to neutral", n, contract.LocalSymbol)
	if p.PlanOnly {
		return res
	}
	// A preview prices the leg through the one order path and hands back
	// its signed token, as a cut's leg does; the plan's figures stay.
	res.Legs[0] = s.reduceHedgePreviewLeg(ctx, p, leg, input)
	aggregateBasket(res, false)
	if len(res.Legs[0].Blockers) == 0 {
		res.Message = fmt.Sprintf("sell %d %s to neutral", n, contract.LocalSymbol)
	}
	return res
}

// hedgeRollDays is how long before a held contract's expiry the plan says
// a roll is due (owner decision 2026-10-28 20:07 CET).
const hedgeRollDays = 10

// reduceHedgeFlags writes the two words Desk shows beside a held hedge:
// roll_due from ten days before a held contract's expiry, and
// hedge_not_needed while the book without its held hedges reads within the
// mandate. A positions read that fails leaves roll_due unset; the plan
// does not depend on it.
func (s *Server) reduceHedgeFlags(ctx context.Context, res *rpc.TradeProposalReducePortfolioResult, input *mandate.Input) {
	held := false
	without := *input
	without.Legs = nil
	for _, l := range input.Legs {
		if mandate.IsIndexFuture(l) {
			held = held || l.Quantity < 0
			continue
		}
		without.Legs = append(without.Legs, l)
	}
	if !held {
		return
	}
	if v := mandate.Evaluate(without); v.Status == mandate.StatusWithin && v.Loss != nil {
		res.HedgeNotNeeded = true
		res.HedgeNotNeededMessage = fmt.Sprintf("The book without the hedge reads within the mandate: its loss measure %s is inside the loss room %s. Buy the hedge back when you choose.",
			mandateMoney(v.Loss.TotalBase, res.BaseCurrency), mandateMoney(v.Loss.RoomBase, res.BaseCurrency))
	}
	pos, err := s.reducePositions(ctx)
	if err != nil || pos == nil {
		return
	}
	today := s.reduceClock()
	var earliest time.Time
	for _, row := range pos.Stocks {
		if !heldIndexHedge(row) {
			continue
		}
		expiry, err := time.Parse("20060102", strings.TrimSpace(row.Expiry))
		if err != nil {
			continue
		}
		from := expiry.AddDate(0, 0, -hedgeRollDays)
		if today.Before(from) {
			continue
		}
		name := nonEmptyString(row.LocalSymbol, "MES "+row.Expiry)
		msg := fmt.Sprintf("%s expires %s: roll the hedge to the next contract (due from %s).", name, expiry.Format(time.DateOnly), from.Format(time.DateOnly))
		if !res.RollDue || expiry.Before(earliest) {
			res.RollDue, res.RollDueMessage, earliest = true, msg, expiry
		}
	}
}

// reduceHedgePreviewLeg previews the sized hedge sale: the table must be
// written and the book's short contracts with this sale within
// max_contracts, never resized to fit (a hard stop against a wrong size,
// not the target); then a limit at the bid, good for the day, in US
// regular hours, through previewOrder with the hedge's source.
func (s *Server) reduceHedgePreviewLeg(ctx context.Context, p rpc.TradeProposalReducePortfolioParams, leg rpc.TradeProposalReduceLeg, input *mandate.Input) rpc.TradeProposalReduceLeg {
	heldShort := 0.0
	for _, l := range input.Legs {
		if mandate.IsIndexFuture(l) && l.Quantity < 0 {
			heldShort -= l.Quantity
		}
	}
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: *leg.Contract, Quantity: leg.ReduceQuantity}
	if blockers := s.indexHedgeCapRefusal(draft, heldShort); len(blockers) > 0 {
		leg.Blockers = blockers
		return leg
	}
	legCtx, cancel := legContext(ctx, p.TimeoutMs)
	defer cancel()
	preview, err := s.previewOrder(legCtx, rpc.OrderPreviewParams{Action: rpc.OrderActionSell, Contract: *leg.Contract, Quantity: leg.ReduceQuantity,
		OrderType: rpc.OrderTypeLMT, Strategy: rpc.OrderStrategyAllowanceLimit, Allowance: &rpc.OrderAllowanceLimit{Pct: 0}, TIF: rpc.OrderTIFDay,
		TimeoutMs: p.TimeoutMs, Source: hedgeOrderSource})
	if err != nil {
		leg.Blockers = previewFailureBlockers(err)
		return leg
	}
	leg.Notional = preview.Notional
	leg.PreviewTokenID = preview.PreviewTokenID
	leg.SubmitEligible = preview.SubmitEligible
	leg.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
	leg.Terms = reduceTermsFor(preview.Draft, preview.Quote, preview.PreviewTokenID, preview.PreviewTokenExpiresAt, "")
	if !preview.SubmitEligible {
		leg.Blockers = previewNotSubmitEligibleBlockers(preview)
		return leg
	}
	leg.PreviewToken = preview.PreviewToken
	return leg
}
