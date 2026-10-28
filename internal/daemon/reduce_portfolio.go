package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// maxBasketLegs bounds how many real orders one portfolio sweep can fan out
// never count toward this cap.
const maxBasketLegs = 25

// minNetDeltaForSweepFraction is the materiality floor for net portfolio
// account and noise for a large one, so this scales with NLV when available.
const minNetDeltaForSweepFraction = 0.001 // 0.1% of NLV

// minNetDeltaForSweepFloor is the absolute-dollar fallback materiality floor
// used only when net liquidation value is unavailable to scale against.
const minNetDeltaForSweepFloor = 100.0

// reduceSweepCandidate is one position the sweep will act on. dollarDelta is
// portfolio delta, since opposite-sign rows never become candidates); qty is
// dollar-delta this qty is expected to remove. blockers is set only for a
// never be placed.
type reduceSweepCandidate struct {
	row              rpc.PositionView
	dollarDelta      float64
	qty              int
	allocatedDollars float64
	blockers         []rpc.TradingBlocker
	// sector and beta are set by a scoped sweep: the facts it selected on.
	sector string
	beta   *float64
}

// netPortfolioDollarDelta measures the whole signed book in base currency.
// Any unmeasured holding makes the net direction unusable for a sweep.
func netPortfolioDollarDelta(pos *rpc.PositionsResult) (net float64, complete bool) {
	if pos == nil || !currentPortfolioAuthority(pos.Authority) {
		return 0, false
	}
	baseCcy := ""
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
	}
	complete = true
	add := func(row rpc.PositionView, isOption bool) {
		if !isOption && defunctHolding(row) {
			return // no market and no value: no exposure to measure
		}
		if row.Stale {
			complete = false
			return
		}
		dd, ok := positionDollarDelta(row, isOption)
		if !ok {
			complete = false
			return
		}
		rate, ok := positionBaseRate(row, baseCcy)
		if !ok {
			complete = false
			return
		}
		next := net + dd*rate
		if math.IsNaN(next) || math.IsInf(next, 0) {
			complete = false
			return
		}
		net = next
	}
	for _, o := range pos.Options {
		add(o, true)
	}
	for _, st := range pos.Stocks {
		add(st, false)
	}
	return net, complete
}

// defunctHolding is a stock the broker reports as having no market
// (terminal non-reporting) and values at zero: it carries no exposure, so it
// neither blocks the book's reduction nor is reduced (review 2026-10-22,
// owner instruction 17:45 CEST). A zero mark without that verdict is still
// unknown exposure and blocks.
func defunctHolding(row rpc.PositionView) bool {
	return positionWireSecType(row.SecType) != "OPT" && row.QuoteExpectation == rpc.QuoteExpectationNone &&
		row.QuoteExpectationReason == rpc.QuoteExpectationReasonTerminal && row.MarketValue == 0 && (row.MarketValueBase == nil || *row.MarketValueBase == 0)
}

// reduceSweepMaterialityFloor scales the net-delta materiality floor off net
func reduceSweepMaterialityFloor(pos *rpc.PositionsResult) float64 {
	if pos != nil && pos.Portfolio != nil && pos.Portfolio.NetLiquidationBase != nil && *pos.Portfolio.NetLiquidationBase > 0 {
		return *pos.Portfolio.NetLiquidationBase * minNetDeltaForSweepFraction
	}
	return minNetDeltaForSweepFloor
}

// reduceSweepCandidates is the delta-adjusted net sweep. The exposure book's
// net dollar delta (reduceExposureBook) sets the direction and, times
// percent, the target. Only pos's lines are selected: those whose delta
// shares the net's sign, each sized pro rata to its share of the
// contributing risk. An opposite-sign line is protection and never
// selected, and a share that rounds below one unit is left alone, never
// force-trimmed to hit the target.
func reduceSweepCandidates(pos, exposure *rpc.PositionsResult, percent int) (cands []reduceSweepCandidate, netDelta float64, netComplete bool, targetDollarDelta float64, blockers []rpc.TradingBlocker) {
	if pos == nil {
		return nil, 0, false, 0, []rpc.TradingBlocker{{Code: "positions_unavailable", Message: "current positions are unavailable", Action: "Retry once the daemon has refreshed positions."}}
	}
	netDelta, netComplete = netPortfolioDollarDelta(exposure)
	if !netComplete {
		return nil, netDelta, false, 0, []rpc.TradingBlocker{{Code: "net_delta_incomplete", Message: "the full portfolio's net delta is unavailable; a partial book cannot establish which holdings reduce risk", Action: "Refresh positions and wait for current prices, Greeks, and FX before reducing the portfolio."}}
	}
	if math.Abs(netDelta) < reduceSweepMaterialityFloor(exposure) {
		return nil, netDelta, netComplete, 0, []rpc.TradingBlocker{{Code: "net_delta_immaterial", Message: "longs and shorts roughly offset; there is no dominant net delta direction to trim", Action: "Trim individual holdings instead, or review hedges manually."}}
	}
	netSign := 1.0
	if netDelta < 0 {
		netSign = -1.0
	}
	targetDollarDelta = math.Abs(netDelta) * float64(percent) / 100.0

	baseCcy := ""
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
	}
	rows := make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options))
	rows = append(rows, pos.Stocks...)
	rows = append(rows, pos.Options...)

	type rawCandidate struct {
		row         rpc.PositionView
		dollarDelta float64 // base currency, signed, matches netSign
	}
	var raw []rawCandidate
	for _, row := range rows {
		if defunctHolding(row) {
			cands = append(cands, reduceSweepCandidate{row: row, blockers: []rpc.TradingBlocker{{Code: "defunct_holding", Message: fmt.Sprintf("%s has no market and the broker values it at zero, so it carries no exposure to reduce", strings.ToUpper(strings.TrimSpace(row.Symbol))), Action: "Nothing to reduce; reconcile the holding with the broker's records."}}})
			continue
		}
		if row.Stale || !reduceEligible(row) {
			continue
		}
		isOption := positionWireSecType(row.SecType) == "OPT"
		dd, ok := positionDollarDelta(row, isOption)
		if !ok {
			cands = append(cands, reduceSweepCandidate{row: row, blockers: []rpc.TradingBlocker{{Code: "delta_unavailable", Message: fmt.Sprintf("%s has no computable delta/spot to size a risk-based trim", strings.ToUpper(strings.TrimSpace(row.Symbol))), Action: "Refresh during market hours so Greeks and the underlying tick are present."}}})
			continue
		}
		rate, ok := positionBaseRate(row, baseCcy)
		if !ok {
			cands = append(cands, reduceSweepCandidate{row: row, blockers: []rpc.TradingBlocker{{Code: "delta_unavailable", Message: fmt.Sprintf("%s has no resolvable FX rate to size a risk-based trim", strings.ToUpper(strings.TrimSpace(row.Symbol))), Action: "Refresh positions; FX data may be temporarily unavailable."}}})
			continue
		}
		ddBase := dd * rate
		sign := 1.0
		if ddBase < 0 {
			sign = -1.0
		}
		if sign != netSign {
			continue // opposite-sign: a protective hedge, structurally excluded
		}
		raw = append(raw, rawCandidate{row: row, dollarDelta: ddBase})
	}

	var total float64
	for _, c := range raw {
		total += math.Abs(c.dollarDelta)
	}
	if total <= 0 {
		return cands, netDelta, netComplete, targetDollarDelta, nil
	}
	for _, c := range raw {
		allocated := targetDollarDelta * (math.Abs(c.dollarDelta) / total)
		allocated = min(allocated, math.Abs(c.dollarDelta)) // shortfall cap: never ask for more than this leg carries
		perUnit := math.Abs(c.dollarDelta) / math.Abs(c.row.Quantity)
		if perUnit <= 0 {
			continue
		}
		qty := int(math.Floor(allocated/perUnit + 1e-9))
		if qty < 1 {
			continue // rounds to nothing; never force-trim to hit the target
		}
		heldAbs, _ := closeReduceQuantity(c.row.Quantity)
		qty = min(qty, heldAbs)
		cands = append(cands, reduceSweepCandidate{row: c.row, dollarDelta: c.dollarDelta, qty: qty, allocatedDollars: float64(qty) * perUnit})
	}
	return cands, netDelta, netComplete, targetDollarDelta, nil
}

// reduceLegBase seeds a leg's disclosure fields from a sweep candidate,
// and is never read by reduceSweepCandidates' selection or sizing.
func reduceLegBase(c reduceSweepCandidate) rpc.TradeProposalReduceLeg {
	row := c.row
	leg := rpc.TradeProposalReduceLeg{
		ConID:                 row.ConID,
		Symbol:                strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:               positionWireSecType(row.SecType),
		PositionQuantity:      row.Quantity,
		DollarDelta:           c.dollarDelta,
		RiskContributionCut:   c.allocatedDollars,
		PositionUnrealizedPnL: row.UnrealizedPnL,
		Sector:                c.sector,
		Beta:                  c.beta,
	}
	if row.UnrealizedPnLBase != nil {
		v := *row.UnrealizedPnLBase
		leg.PositionUnrealizedPnLBase = &v
	}
	return leg
}

// legContext bounds one leg's quote/WhatIf/place so a single stuck leg cannot
// drain the basket budget; it never exceeds the parent deadline.
func legContext(parent context.Context, timeoutMs int) (context.Context, context.CancelFunc) {
	budget := max(time.Duration(timeoutMs)*time.Millisecond, 15*time.Second)
	budget = min(budget, 30*time.Second)
	if dl, ok := parent.Deadline(); ok {
		budget = min(budget, time.Until(dl)-500*time.Millisecond)
	}
	if budget <= 0 {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	return context.WithTimeout(parent, budget)
}

// reduceLegPrepare sizes and previews one candidate through the gated order
// leg is submit-eligible, the raw preview so the caller can redeem its token.
func (s *Server) reduceLegPrepare(ctx context.Context, c reduceSweepCandidate, timeoutMs int, adaptive string) (rpc.TradeProposalReduceLeg, *rpc.OrderPreviewResult) {
	leg := reduceLegBase(c)
	if len(c.blockers) > 0 {
		leg.Blockers = c.blockers
		return leg, nil
	}
	// A leg never sells beyond what Canary's own working orders leave of the
	// line (review 2026-10-22, finding 2).
	qty := c.qty
	if reduceEligible(c.row) {
		action := rpc.OrderActionSell
		if c.row.Quantity < 0 {
			action = rpc.OrderActionBuy
		}
		committed, blockers := s.reduceCommittedSales(proposalContractFromPosition(c.row, positionWireSecType(c.row.SecType)), action)
		var free float64
		if len(blockers) == 0 {
			free, blockers = reduceFreeQuantity(c.row.Quantity, committed, c.row.Symbol)
		}
		if len(blockers) > 0 {
			leg.Blockers = blockers
			return leg, nil
		}
		freeAbs, _ := closeReduceQuantity(free)
		qty = min(qty, freeAbs)
		leg.CommittedQuantity = committed
	}
	prep, blockers := preparedReduceWithQty(c.row, qty, timeoutMs, adaptive, s.reduceOrderLimits())
	if len(blockers) > 0 {
		leg.Blockers = blockers
		return leg, nil
	}
	leg.Action = prep.action
	leg.ReduceQuantity = prep.qty
	// Working orders can leave less capacity than the selected allocation.
	// Report only the relief of the quantity this preview will actually trade.
	if c.qty > 0 {
		leg.RiskContributionCut *= float64(prep.qty) / float64(c.qty)
	}
	legCtx, cancel := legContext(ctx, timeoutMs)
	defer cancel()
	preview, err := s.previewOrder(legCtx, prep.params)
	if err != nil {
		leg.Blockers = []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error(), Action: "Refresh positions and preview again."}}
		return leg, nil
	}
	leg.Notional = preview.Notional
	leg.NotionalCurrency = preview.Draft.Contract.Currency
	if c.row.FXRate != nil && *c.row.FXRate > 0 {
		base := preview.Notional * *c.row.FXRate
		leg.NotionalBase = &base
	}
	leg.PreviewTokenID = preview.PreviewTokenID
	leg.SubmitEligible = preview.SubmitEligible
	leg.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
	leg.Terms = reduceTermsFor(preview.Draft, preview.Quote, preview.PreviewTokenID, preview.PreviewTokenExpiresAt, prep.algoNote)
	if b := reduceCloseReduceBlockers(preview); len(b) > 0 {
		leg.Blockers = b
		return leg, nil
	}
	if !preview.SubmitEligible {
		leg.Blockers = previewNotSubmitEligibleBlockers(preview)
		return leg, nil
	}
	return leg, preview
}

func (s *Server) reduceLegSubmit(ctx context.Context, c reduceSweepCandidate, timeoutMs int, origin, adaptive string) rpc.TradeProposalReduceLeg {
	leg, preview := s.reduceLegPrepare(ctx, c, timeoutMs, adaptive)
	if preview == nil {
		return leg // terminal: blocked or not submit-eligible; nothing placed
	}
	legCtx, cancel := legContext(ctx, timeoutMs)
	defer cancel()
	place, err := s.proposalPlaceOrder(legCtx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: timeoutMs, Origin: origin})
	if err != nil {
		leg.Blockers = []rpc.TradingBlocker{reducePlaceBlocker(err)}
		return leg
	}
	leg.Placed = place.Accepted
	leg.Place = place
	leg.OrderRef = place.OrderRef
	leg.Message = place.Message
	return leg
}

// aggregateBasket fills the counts, base-currency total, achieved risk
func aggregateBasket(res *rpc.TradeProposalReducePortfolioResult, submit bool) {
	res.LegCount = len(res.Legs)
	eligible, blocked := 0, 0
	var total, achieved float64
	fxMissing := false
	for _, leg := range res.Legs {
		ok := leg.SubmitEligible
		if submit {
			ok = leg.Placed
		}
		if ok && len(leg.Blockers) == 0 {
			eligible++
			achieved += leg.RiskContributionCut
			if leg.NotionalBase != nil {
				total += *leg.NotionalBase
			} else {
				fxMissing = true
			}
		} else {
			blocked++
		}
	}
	res.EligibleCount = eligible
	res.BlockedCount = blocked
	res.AchievedDollarDelta = achieved
	if res.TargetDollarDelta > 0 {
		pct := achieved / res.TargetDollarDelta * 100
		res.AchievedPctOfTarget = &pct
	}
	if eligible > 0 {
		if res.BaseCurrency != "" && !fxMissing {
			res.TotalNotional = total
		} else {
			res.FXIncomplete = true
			if res.BaseCurrency != "" {
				res.TotalNotional = total
			}
		}
	}
	res.Accepted = eligible > 0 && blocked == 0
	verb := "eligible"
	if submit {
		verb = "placed"
	}
	msg := fmt.Sprintf("%d %s · %d blocked of %d legs", eligible, verb, blocked, res.LegCount)
	if res.TargetDollarDelta > 0 {
		ccy := res.BaseCurrency
		if ccy == "" {
			ccy = "ccy"
		}
		pctStr := ""
		if res.AchievedPctOfTarget != nil {
			pctStr = fmt.Sprintf(" (%.0f%%)", *res.AchievedPctOfTarget)
		}
		msg += fmt.Sprintf(" · removing ~%.0f of %.0f %s targeted net delta%s", res.AchievedDollarDelta, res.TargetDollarDelta, ccy, pctStr)
	}
	res.Message = msg
}

func (s *Server) reducePortfolioPreview(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	if len(p.ProposalKeys) > 0 || p.Mode == rpc.ReduceModeProposals {
		return s.reduceProposalsPlan(ctx, p), nil
	}
	if p.Mode == rpc.ReduceModeHedge {
		return s.reduceHedgePlan(ctx, p), nil
	}
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, AsOf: now}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	adaptive, blockers := reduceAdaptivePriority(p.AdaptivePriority)
	if len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	pos, err := s.reducePositions(ctx)
	if err != nil {
		return nil, err
	}
	cands, blockers := s.reducePortfolioSelect(ctx, p, pos, res)
	if len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	if actionable := reduceSweepActionableCount(cands); actionable > maxBasketLegs {
		res.Blockers = []rpc.TradingBlocker{{Code: "too_many_legs", Message: fmt.Sprintf("portfolio sweep would place %d orders, above the %d-leg cap", actionable, maxBasketLegs), Action: "Trim individual holdings instead."}}
		return res, nil
	}
	if pos.Portfolio != nil {
		res.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	for _, c := range cands {
		if p.PlanOnly {
			res.Legs = append(res.Legs, reducePlanLeg(c))
			continue
		}
		leg, preview := s.reduceLegPrepare(ctx, c, p.TimeoutMs, adaptive)
		if preview != nil {
			// The signed token leaves with the preview so the exact leg can
			// be submitted later; a leg that is not submit-eligible has none.
			leg.PreviewToken = preview.PreviewToken
		}
		res.Legs = append(res.Legs, leg)
	}
	s.reduceMandateScores(ctx, res, reduceTradeChanges(res.Legs))
	res.TradeCostBase, res.TradeCostUnknown = s.reduceTradeCost(ctx, res.Legs, pos)
	aggregateBasket(res, false)
	if res.Mode == rpc.ReduceModeNet && !p.PlanOnly {
		projectReducePortfolioReview(res, pos, cands)
	}
	if p.PlanOnly {
		// A plan is never a basket to submit: its legs are planned or
		// blocked, and its total is the planned legs' marks.
		res.PlanOnly, res.Accepted, res.EligibleCount, res.BlockedCount, res.TotalNotional = true, false, 0, 0, 0
		for i, leg := range res.Legs {
			res.Legs[i].SubmitEligible = false
			if len(leg.Blockers) > 0 {
				res.BlockedCount++
			} else if leg.NotionalBase != nil {
				res.TotalNotional += *leg.NotionalBase
			}
		}
		res.Message = fmt.Sprintf("%d planned · %d blocked of %d legs", res.LegCount-res.BlockedCount, res.BlockedCount, res.LegCount)
	}
	return res, nil
}

// reducePortfolioSelect reads the keep list, takes the locked lines out of
// the book, and selects the legs for the requested mode. An unreadable keep
// list blocks the sweep: it never runs as if nothing were locked.
func (s *Server) reducePortfolioSelect(ctx context.Context, p rpc.TradeProposalReducePortfolioParams, pos *rpc.PositionsResult, res *rpc.TradeProposalReducePortfolioResult) ([]reduceSweepCandidate, []rpc.TradingBlocker) {
	locks, err := s.holdingLocks(ctx)
	if err != nil {
		return nil, []rpc.TradingBlocker{{Code: "locks_unavailable", Message: "the locked holdings could not be read: " + err.Error(), Action: "Retry; nothing is reduced while the keep list is unknown."}}
	}
	book, kept := reduceUnlockedBook(pos, locks)
	res.Mode = p.Mode
	switch p.Mode {
	case "", rpc.ReduceModeNet:
		res.Mode = rpc.ReduceModeNet
		if len(p.Sectors) > 0 || p.MinBeta > 0 {
			return nil, []rpc.TradingBlocker{{Code: "bad_request", Message: "sectors and min_beta narrow a longs or all sweep, not a net one", Action: "Choose mode longs or all."}}
		}
		cands, netDelta, netComplete, target, blockers := reduceSweepCandidates(book, reduceExposureBook(pos, locks), p.Percent)
		res.NetDollarDeltaBefore, res.NetDeltaIncomplete, res.TargetDollarDelta, res.Kept = netDelta, !netComplete, target, kept
		return cands, blockers
	case rpc.ReduceModeLongs, rpc.ReduceModeAll:
		if p.MinBeta < 0 {
			return nil, []rpc.TradingBlocker{{Code: "bad_request", Message: "min_beta cannot be negative", Action: "Choose a beta of zero or more."}}
		}
		cands, scoped := reduceScopedCandidates(book, p, s.reduceScopeFacts(ctx, book, p))
		res.Kept = append(kept, scoped...)
		return cands, nil
	default:
		return nil, []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("unknown reduce mode %q", p.Mode), Action: "Choose net, longs or all."}}
	}
}

func (s *Server) reducePortfolioSubmit(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, AsOf: now}
	// Write gate before the dedupe replay: a freeze engaged between the
	// original call and a RequestRef retry must surface as a blocker, not
	// replay a pre-freeze "ok" result.
	if blockers := s.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	if e, ok := s.reduceReplay(p.RequestRef); ok && e.basket != nil {
		clone := *e.basket
		clone.Replayed = true
		return &clone, nil
	}
	if p.PlanOnly || len(p.ProposalKeys) > 0 || p.Mode == rpc.ReduceModeHedge && len(p.PreviewTokens) == 0 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: "a plan is preview-only, and a hedge is placed from its previewed token", Action: "Preview the basket, then submit its tokens; nothing was sent."}}
		return res, nil
	}
	if len(p.PreviewTokens) > 0 {
		// The tokens carry the legs' terms; the percent is a preview input.
		return s.reducePortfolioSubmitTokens(ctx, p, res)
	}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	adaptive, blockers := reduceAdaptivePriority(p.AdaptivePriority)
	if len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	pos, err := s.reducePositions(ctx)
	if err != nil {
		return nil, err
	}
	cands, blockers := s.reducePortfolioSelect(ctx, p, pos, res)
	if len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	if actionable := reduceSweepActionableCount(cands); actionable > maxBasketLegs {
		res.Blockers = []rpc.TradingBlocker{{Code: "too_many_legs", Message: fmt.Sprintf("portfolio sweep would place %d orders, above the %d-leg cap", actionable, maxBasketLegs), Action: "Trim individual holdings instead."}}
		return res, nil
	}
	if pos.Portfolio != nil {
		res.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	for _, c := range cands {
		res.Legs = append(res.Legs, s.reduceLegSubmit(ctx, c, p.TimeoutMs, p.Origin, adaptive))
	}
	aggregateBasket(res, true)
	res.RequestRef = strings.TrimSpace(p.RequestRef)
	s.reduceRemember(p.RequestRef, reduceDedupeEntry{basket: res})
	return res, nil
}

// reducePortfolioSubmitTokens places the legs of a reviewed basket at exactly
// their reviewed terms: each token is taken through the reduce submit checks
// and placed on its own, so one refused leg never holds another, and the
// whole basket is remembered under its RequestRef. The write gate and the
// replay have already run.
func (s *Server) reducePortfolioSubmitTokens(ctx context.Context, p rpc.TradeProposalReducePortfolioParams, res *rpc.TradeProposalReducePortfolioResult) (*rpc.TradeProposalReducePortfolioResult, error) {
	res.RequestRef = strings.TrimSpace(p.RequestRef)
	if len(p.PreviewTokens) > maxBasketLegs {
		res.Blockers = []rpc.TradingBlocker{{Code: "too_many_legs", Message: fmt.Sprintf("the basket names %d legs, above the %d-leg cap", len(p.PreviewTokens), maxBasketLegs), Action: "Submit fewer legs; nothing was sent."}}
		return res, nil
	}
	pos, err := s.reducePositions(ctx)
	if err != nil {
		res.Blockers = []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error(), Action: "Retry once the daemon has refreshed positions; nothing was sent."}}
		return res, nil
	}
	if pos != nil && pos.Portfolio != nil {
		res.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	locks, err := s.holdingLocks(ctx)
	if err != nil {
		res.Blockers = []rpc.TradingBlocker{{Code: "locks_unavailable", Message: "the locked holdings could not be read: " + err.Error(), Action: "Retry; nothing was sent."}}
		return res, nil
	}
	seen := make(map[string]struct{}, len(p.PreviewTokens))
	for _, token := range p.PreviewTokens {
		token = strings.TrimSpace(token)
		if _, dup := seen[token]; dup {
			res.Legs = append(res.Legs, rpc.TradeProposalReduceLeg{Blockers: []rpc.TradingBlocker{{Code: "token_duplicate", Message: "this leg's token appears twice in the basket", Action: "Submit each leg once; the repeat was not sent."}}})
			continue
		}
		seen[token] = struct{}{}
		res.Legs = append(res.Legs, reduceLegFromTokenOutcome(s.reduceTokenLeg(ctx, token, pos, p.TimeoutMs, p.Origin, locks.Resolve(pos).Locked)))
	}
	aggregateBasket(res, true)
	s.reduceRemember(p.RequestRef, reduceDedupeEntry{basket: res})
	return res, nil
}

// reduceLegFromTokenOutcome writes a token outcome into a basket leg: the
// reviewed terms, the position the leg was checked against, and the place
// outcome or the blockers. The risk-contribution fields stay zero: a
// tokened basket reports what was sent, not a fresh delta allocation.
func reduceLegFromTokenOutcome(out reduceTokenOutcome) rpc.TradeProposalReduceLeg {
	var leg rpc.TradeProposalReduceLeg
	if out.verified {
		draft := out.payload.Draft
		leg.ConID = draft.Contract.ConID
		leg.Symbol = strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
		leg.SecType = draft.Contract.SecType
		leg.Action = draft.Action
		leg.ReduceQuantity = draft.Quantity
		leg.PreviewTokenID = out.payload.TokenID
		leg.SubmitEligible = true
		leg.Terms = reduceTermsFor(draft, out.payload.Quote, out.payload.TokenID, out.payload.ExpiresAt, reduceAlgoNoteFor(draft))
		leg.Notional = float64(draft.Quantity) * draft.LimitPrice * float64(contractMultiplier(draft.Contract))
		leg.NotionalCurrency = draft.Contract.Currency
	}
	if out.rowFound {
		leg.PositionQuantity = out.row.Quantity
		leg.PositionUnrealizedPnL = out.row.UnrealizedPnL
		if out.row.UnrealizedPnLBase != nil {
			v := *out.row.UnrealizedPnLBase
			leg.PositionUnrealizedPnLBase = &v
		}
		if out.verified && out.row.FXRate != nil && *out.row.FXRate > 0 {
			base := leg.Notional * *out.row.FXRate
			leg.NotionalBase = &base
		}
	}
	if len(out.blockers) > 0 {
		leg.Blockers = out.blockers
		return leg
	}
	leg.Placed = out.place.Accepted
	leg.Place = out.place
	leg.OrderRef = out.place.OrderRef
	leg.Message = out.place.Message
	return leg
}

// reduceSweepActionableCount counts candidates that will become a real
// broker order (qty > 0); disclosure-only delta_unavailable rows never count
func reduceSweepActionableCount(cands []reduceSweepCandidate) int {
	n := 0
	for _, c := range cands {
		if c.qty > 0 {
			n++
		}
	}
	return n
}

func (s *Server) handleTradeProposalsReducePortfolioPreview(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReducePortfolioResult, error) {
	var p rpc.TradeProposalReducePortfolioParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.reducePortfolioPreview(ctx, p)
}

func (s *Server) handleTradeProposalsReducePortfolioSubmit(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReducePortfolioResult, error) {
	var p rpc.TradeProposalReducePortfolioParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	// Serialize the whole basket against every other broker writer (R1): one
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.reducePortfolioSubmit(ctx, p)
}
