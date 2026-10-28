package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Kept reasons for TradeProposalReduceKept.
const (
	reduceKeptLocked     = "locked"
	reduceKeptShort      = "short"
	reduceKeptOutOfScope = "out_of_scope"
)

// reduceUnlockedBook is the book a sweep may act on: every locked line is
// taken out and disclosed as kept. A lock names an underlying, so the keep
// list is resolved against this book first: the shares and every option on
// them go together (owner, 2026-10-28 06:41 CET). What a net sweep counts is
// reduceExposureBook's.
func reduceUnlockedBook(pos *rpc.PositionsResult, locks rpc.HoldingLocks) (*rpc.PositionsResult, []rpc.TradeProposalReduceKept) {
	if pos == nil || len(locks.Locks) == 0 {
		return pos, nil
	}
	var kept []rpc.TradeProposalReduceKept
	resolved := locks.Resolve(pos)
	keep := func(rows []rpc.PositionView) []rpc.PositionView {
		out := make([]rpc.PositionView, 0, len(rows))
		for _, row := range rows {
			if resolved.Locked(row.ConID) {
				kept = append(kept, rpc.TradeProposalReduceKept{ConID: row.ConID, Symbol: normSym(row.Symbol), Reason: reduceKeptLocked, Role: resolved.Role(row.ConID)})
				continue
			}
			out = append(out, row)
		}
		return out
	}
	book := *pos
	book.Stocks = keep(pos.Stocks)
	book.Options = keep(pos.Options)
	return &book, kept
}

// reduceExposureBook is the book whose net delta sets a net sweep's
// direction and size: every line but a locked hedge. A hedge the owner
// locked never sets the direction (owner, 2026-10-22 19:50 CEST: a hedge is
// a hedge because the owner says so). A locked core line is exposure the
// owner keeps, so it counts though it is never traded, and the unlocked
// protection over it is not read as a bare short (owner, 2026-10-28 12:04
// CET: a sweep that left the core out sold the core's puts).
func reduceExposureBook(pos *rpc.PositionsResult, locks rpc.HoldingLocks) *rpc.PositionsResult {
	if pos == nil || len(locks.Locks) == 0 {
		return pos
	}
	resolved := locks.Resolve(pos)
	counted := func(rows []rpc.PositionView) []rpc.PositionView {
		return slices.DeleteFunc(slices.Clone(rows), func(row rpc.PositionView) bool {
			return resolved.Locked(row.ConID) && resolved.Role(row.ConID) == rpc.HoldingLockRoleHedge
		})
	}
	book := *pos
	book.Stocks, book.Options = counted(pos.Stocks), counted(pos.Options)
	return &book
}

// reduceScopeFacts are the per-underlying facts a scoped sweep selects on:
// beta to the index from the kept mandate verdict, and the GICS sector.
type reduceScopeFacts struct {
	betas   map[string]float64
	sectors map[string]string
}

func (s *Server) reduceScopeFacts(ctx context.Context, pos *rpc.PositionsResult, p rpc.TradeProposalReducePortfolioParams) reduceScopeFacts {
	facts := reduceScopeFacts{betas: map[string]float64{}, sectors: map[string]string{}}
	if p.MinBeta > 0 {
		if result, _, _, _ := s.currentMandateVerdict(); result != nil && result.Verdict != nil {
			for _, u := range result.Verdict.Underlyings {
				facts.betas[normSym(u.Underlying)] = u.Beta
			}
		}
	}
	if len(p.Sectors) > 0 && pos != nil {
		var resolve classificationResolver
		if c := s.gatewayConnector(); c != nil {
			if binding, ok := c.CaptureSession(); ok {
				resolve = s.brokerClassificationResolver(c, binding)
			}
		}
		for key, class := range resolveUnderlyingClassifications(ctx, pos, resolve) {
			facts.sectors[normSym(key)] = class.Sector
		}
	}
	return facts
}

// reduceScopedCandidates selects and sizes a "longs" or "all" sweep: the
// same share of every selected line, never more than the line, in whole
// units. Unlike the net sweep it does not allocate a delta target.
func reduceScopedCandidates(pos *rpc.PositionsResult, p rpc.TradeProposalReducePortfolioParams, facts reduceScopeFacts) (cands []reduceSweepCandidate, kept []rpc.TradeProposalReduceKept) {
	if pos == nil {
		return nil, nil
	}
	baseCcy := ""
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
	}
	sectors := map[string]bool{}
	for _, sector := range p.Sectors {
		sectors[strings.ToLower(strings.TrimSpace(sector))] = true
	}
	rows := make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options))
	rows = append(rows, pos.Stocks...)
	rows = append(rows, pos.Options...)
	for _, row := range rows {
		symbol := normSym(row.Symbol)
		if defunctHolding(row) || !reduceEligible(row) {
			continue
		}
		block := func(code, msg, action string) {
			cands = append(cands, reduceSweepCandidate{row: row, blockers: []rpc.TradingBlocker{{Code: code, Message: msg, Action: action}}})
		}
		isOption := positionWireSecType(row.SecType) == "OPT"
		var ddBase float64
		if row.Stale {
			block("position_stale", fmt.Sprintf("%s has no current price to size a trim", symbol), "Refresh positions once the market quotes it.")
			continue
		}
		dd, ok := positionDollarDelta(row, isOption)
		rate, rateOK := positionBaseRate(row, baseCcy)
		if !ok || !rateOK {
			block("delta_unavailable", fmt.Sprintf("%s has no computable delta, spot or FX rate to size a trim", symbol), "Refresh during market hours so Greeks, the underlying tick and FX are present.")
			continue
		}
		ddBase = dd * rate
		if p.Mode == rpc.ReduceModeLongs && ddBase <= 0 {
			kept = append(kept, rpc.TradeProposalReduceKept{ConID: row.ConID, Symbol: symbol, Reason: reduceKeptShort})
			continue
		}
		var beta *float64
		if p.MinBeta > 0 {
			b, ok := facts.betas[symbol]
			if !ok {
				block("beta_unavailable", fmt.Sprintf("%s has no measured beta to the index", symbol), "Wait for the mandate's next evaluation, or reduce this line on its own.")
				continue
			}
			beta = &b
			if b < p.MinBeta {
				kept = append(kept, rpc.TradeProposalReduceKept{ConID: row.ConID, Symbol: symbol, Reason: reduceKeptOutOfScope})
				continue
			}
		}
		sector := facts.sectors[symbol]
		if len(sectors) > 0 && !sectors[strings.ToLower(sector)] {
			kept = append(kept, rpc.TradeProposalReduceKept{ConID: row.ConID, Symbol: symbol, Reason: reduceKeptOutOfScope})
			continue
		}
		heldAbs, _ := closeReduceQuantity(row.Quantity)
		qty := heldAbs
		if p.Percent < 100 {
			qty = min(int(math.Floor(math.Abs(row.Quantity)*float64(p.Percent)/100+1e-9)), heldAbs)
		}
		if qty < 1 {
			block("percent_too_small", fmt.Sprintf("%d%% of %s rounds to nothing", p.Percent, symbol), "Choose a larger share, or reduce this line on its own.")
			continue
		}
		perUnit := math.Abs(ddBase) / math.Abs(row.Quantity)
		c := reduceSweepCandidate{row: row, dollarDelta: ddBase, qty: qty, allocatedDollars: float64(qty) * perUnit, sector: sector, beta: beta}
		cands = append(cands, c)
	}
	return cands, kept
}

// reduceMandateInput is the last evaluation's input for a plan's scores.
// With no evaluation since the daemon started, or one under another policy,
// a read's evaluation (it never counts toward the forced reduction's
// streak) measures the book now.
func (s *Server) reduceMandateInput(ctx context.Context) (*mandate.Input, string) {
	input, _, why := s.mandateCurrentInput()
	if input == nil && s.riskPolicies != nil {
		s.evaluateMandate(ctx, true)
		input, _, why = s.mandateCurrentInput()
	}
	return input, why
}

// reduceMandateScores scores a plan on the mandate's measures, on the book
// now and with every planned leg filled: the index stress, fall and rise,
// and the loss measure against the loss room (owner, 2026-10-28 13:13 CET:
// a plan is scored on both sides against the loss limit). trades maps a
// contract to its signed quantity change.
func (s *Server) reduceMandateScores(ctx context.Context, res *rpc.TradeProposalReducePortfolioResult, trades map[int]float64) {
	input, why := s.reduceMandateInput(ctx)
	if input == nil {
		res.IndexLoss, res.PlanLossMeasure = indexLossUnknown(why), planLossMeasureUnknown(why)
		return
	}
	after := mandateInputWithTrades(*input, trades)
	res.IndexLoss, res.PlanLossMeasure = indexLossBetween(*input, after), planLossMeasureBetween(*input, after)
}

// reduceTradeCost is what the planned legs give up to the market: half the
// quoted bid-ask spread of each leg's line, times its units and multiplier,
// in base currency. It reads the legs the scores fill. A line's quote is
// its position row's when the row carries both sides; the broker's
// portfolio stream carries a mark for an option and never a bid or ask, so
// a row without one is quoted by its exact contract, as the hedge's future
// is. The cost is unknown, naming the first line without a two-sided quote
// or base rate, when any lacks one: a partial cost would read as the plan's.
func (s *Server) reduceTradeCost(ctx context.Context, legs []rpc.TradeProposalReduceLeg, pos *rpc.PositionsResult) (*float64, string) {
	if pos == nil {
		return nil, "the positions could not be read"
	}
	rows := map[int]rpc.PositionView{}
	for _, row := range slices.Concat(pos.Stocks, pos.Options) {
		rows[row.ConID] = row
	}
	baseCcy := ""
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
	}
	total := 0.0
	for _, leg := range legs {
		if leg.ReduceQuantity <= 0 || len(leg.Blockers) > 0 {
			continue
		}
		row, ok := rows[leg.ConID]
		if !ok || leg.ConID <= 0 {
			return nil, leg.Symbol + " has no two-sided quote"
		}
		bid, ask := row.Bid, row.Ask
		if !twoSided(bid, ask) {
			q, err := s.reduceLegQuote(ctx, row)
			if err != nil || q == nil {
				return nil, leg.Symbol + " has no two-sided quote"
			}
			bid, ask = q.Bid, q.Ask
		}
		if !twoSided(bid, ask) {
			return nil, leg.Symbol + " has no two-sided quote"
		}
		mult := 1.0
		if positionWireSecType(row.SecType) == "OPT" {
			mult = float64(optionMultiplier(row))
		}
		rate, rateOK := positionBaseRate(row, baseCcy)
		if !rateOK {
			return nil, leg.Symbol + " has no rate to the base currency"
		}
		total += (*ask - *bid) / 2 * float64(leg.ReduceQuantity) * mult * rate
	}
	return &total, ""
}

// twoSided reports a quote with a bid above zero and an ask at or above it.
func twoSided(bid, ask *float64) bool {
	return bid != nil && ask != nil && *bid > 0 && *ask >= *bid
}

// reduceLegQuote reads a planned leg's line by its exact contract, within
// the budget the hedge's future is read in.
func (s *Server) reduceLegQuote(ctx context.Context, row rpc.PositionView) (*rpc.Quote, error) {
	contract := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	if s.reduceLegQuoteForTest != nil {
		return s.reduceLegQuoteForTest(ctx, contract)
	}
	raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: contract, TimeoutMs: reduceLegQuoteTimeoutMs})
	return s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
}

// reduceLegQuoteTimeoutMs bounds one leg's exact-contract quote; the
// hedge's future is read within the same budget (reduceHedgeFuture).
const reduceLegQuoteTimeoutMs = 2000

// mandateInputWithTrades is the input with each traded contract's quantity
// moved by its change. Two legs of one contract are summed by the
// simulation, so the change lands on the first.
func mandateInputWithTrades(in mandate.Input, trades map[int]float64) mandate.Input {
	in.Legs = slices.Clone(in.Legs)
	for conID, change := range trades {
		id := mandateConLegID(conID)
		if i := slices.IndexFunc(in.Legs, func(l mandate.Leg) bool { return l.ID == id }); i >= 0 {
			in.Legs[i].Quantity += change
		}
	}
	return in
}

// reducePlanLeg is a leg sized without a broker preview: its notional is the
// line's own mark, and nothing about it is submit-eligible.
func reducePlanLeg(c reduceSweepCandidate) rpc.TradeProposalReduceLeg {
	leg := reduceLegBase(c)
	if len(c.blockers) > 0 {
		leg.Blockers = c.blockers
		return leg
	}
	leg.Action = rpc.OrderActionSell
	if c.row.Quantity < 0 {
		leg.Action = rpc.OrderActionBuy
	}
	leg.ReduceQuantity = c.qty
	share := float64(c.qty) / math.Abs(c.row.Quantity)
	leg.Notional = math.Abs(c.row.MarketValue) * share
	leg.NotionalCurrency = c.row.Currency
	if c.row.MarketValueBase != nil {
		base := math.Abs(*c.row.MarketValueBase) * share
		leg.NotionalBase = &base
	}
	return leg
}

// reduceTradeChanges is the signed quantity each planned leg moves its line by.
func reduceTradeChanges(legs []rpc.TradeProposalReduceLeg) map[int]float64 {
	out := map[int]float64{}
	for _, leg := range legs {
		if leg.ConID <= 0 || leg.ReduceQuantity <= 0 || len(leg.Blockers) > 0 {
			continue
		}
		change := -float64(leg.ReduceQuantity)
		if leg.Action == rpc.OrderActionBuy {
			change = float64(leg.ReduceQuantity)
		}
		out[leg.ConID] += change
	}
	return out
}
