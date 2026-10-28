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
	now := s.nowUTC()
	fut, err := c.FrontFutureAfter(ctx, ibkrlib.Contract{Symbol: hedgeFutureSymbol, SecType: "FUT", Exchange: "CME", Currency: "USD"}, now, now.Add(hedgeRollWindow))
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
	res := &rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeHedge, PlanOnly: true, AsOf: s.reduceClock(), Legs: []rpc.TradeProposalReduceLeg{}}
	block := func(code, msg, action string) *rpc.TradeProposalReducePortfolioResult {
		res.Blockers = append(res.Blockers, rpc.TradingBlocker{Code: code, Message: msg, Action: action})
		return res
	}
	unknown := func(why string) {
		res.IndexLoss, res.PlanLossMeasure, res.TradeCostUnknown = indexLossUnknown(why), planLossMeasureUnknown(why), why
	}
	switch {
	case !p.PlanOnly:
		return block("hedge_plan_only", "an index hedge is priced by a plan only; no order path places a future yet", "Read the plan.")
	case len(p.Sectors) > 0 || p.MinBeta != 0 || len(p.ProposalKeys) > 0 || p.Percent != 0 && p.Percent != 100:
		return block("bad_request", "an index hedge is sized to neutral on the whole book; percent, sector, beta and proposals do not apply", "Ask for the hedge alone.")
	}
	input, why := s.reduceMandateInput(ctx)
	if input == nil {
		unknown(why)
		return block("hedge_unsized", "the hedge cannot be sized: "+why, "Retry once the mandate has measured the book.")
	}
	res.BaseCurrency = strings.ToUpper(input.BaseCurrency)
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
	id := mandateConLegID(contract.ConID)
	hedge := mandate.Leg{ID: id, Symbol: contract.LocalSymbol, Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock, Quantity: -1, Multiplier: mult,
		Currency: normCcy(contract.Currency), Spot: price}
	// A held copy of this contract keeps its mandate valuation when quantities
	// merge. Measure one unit on that same book, rather than mix its held mark
	// with the new quote and size past the model's neutral point.
	one := mandate.Evaluate(mandateInputWithLegs(*input, []mandate.Leg{hedge}))
	if one.Stresses == nil {
		unchanged()
		return block("hedge_unsized", "the mandate cannot measure one hedge contract", "Retry once the mandate has measured the book.")
	}
	perContract := fall - one.Stresses.IndexLossBase
	if perContract <= 0 || math.IsNaN(perContract) || math.IsInf(perContract, 0) {
		unchanged()
		return block("hedge_unsized", "the future has no measurable offset to the index fall", "Retry once the mandate has measured the book.")
	}
	n := int(math.Floor(fall / perContract))
	if n < 1 {
		unchanged()
		return block("below_one_contract", fmt.Sprintf("the book loses %s in a %g%% fall; one %s contract offsets %s, so even one would hedge past neutral",
			mandateMoney(fall, res.BaseCurrency), mandate.IndexShockPct, contract.LocalSymbol, mandateMoney(perContract, res.BaseCurrency)), "Keep the book as it is.")
	}
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
	hedge.Quantity = -float64(n)
	after := mandateInputWithLegs(*input, []mandate.Leg{hedge})
	res.IndexLoss, res.PlanLossMeasure = indexLossBetween(*input, after), planLossMeasureBetween(*input, after)
	if quote != nil && quote.Bid != nil && quote.Ask != nil && *quote.Bid > 0 && *quote.Ask >= *quote.Bid {
		res.TradeCostBase = new((*quote.Ask - *quote.Bid) / 2 * float64(n) * mult * fx)
	} else {
		res.TradeCostUnknown = contract.LocalSymbol + " has no two-sided quote"
	}
	res.TotalNotional = value * fx
	res.Message = fmt.Sprintf("sell %d %s to neutral", n, contract.LocalSymbol)
	return res
}
