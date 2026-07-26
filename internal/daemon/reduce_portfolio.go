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
// to. A book whose sweep would touch more eligible positions than this
// returns a basket-level blocker and places nothing — a sanity backstop
// against a runaway one-tap action. Disclosure-only rows (delta_unavailable)
// never count toward this cap.
const maxBasketLegs = 25

// reduceBasketDedupeTTL is how long a submit RequestRef is remembered so a
// double-tap or client retry replays the prior result instead of placing again.
const reduceBasketDedupeTTL = 90 * time.Second

// minNetDeltaForSweepFraction is the materiality floor for net portfolio
// dollar-delta, expressed as a fraction of net liquidation value: below this,
// longs and shorts are considered to roughly offset and there is no dominant
// direction to trim. A fixed dollar figure would be material for a small
// account and noise for a large one, so this scales with NLV when available.
const minNetDeltaForSweepFraction = 0.001 // 0.1% of NLV

// minNetDeltaForSweepFloor is the absolute-dollar fallback materiality floor
// used only when net liquidation value is unavailable to scale against.
const minNetDeltaForSweepFloor = 100.0

type reduceBasketDedupeEntry struct {
	at     time.Time
	result *rpc.TradeProposalReducePortfolioResult
}

// reduceSweepCandidate is one position the sweep will act on. dollarDelta is
// its signed, base-currency delta-adjusted exposure (same sign as net
// portfolio delta, since opposite-sign rows never become candidates); qty is
// the already-sized reduce quantity; allocatedDollars is the (positive)
// dollar-delta this qty is expected to remove. blockers is set only for a
// disclosure-only row (e.g. delta_unavailable) that carries no qty and will
// never be placed.
type reduceSweepCandidate struct {
	row              rpc.PositionView
	dollarDelta      float64
	qty              int
	allocatedDollars float64
	blockers         []rpc.TradingBlocker
}

// netPortfolioDollarDelta sums every non-stale position's base-currency
// dollar-delta (positionDollarDelta converted via positionBaseRate). complete
// is false when one or more non-stale rows had no computable delta or no
// resolvable FX rate and were excluded from the sum. The net is still usable
// in that case — disclosed as a partial-book estimate — rather than nulling
// the whole computation the way an all-or-nothing aggregate would; a single
// stale-FX row should not block the entire sweep from having a direction.
func netPortfolioDollarDelta(pos *rpc.PositionsResult) (net float64, complete bool) {
	if pos == nil {
		return 0, false
	}
	baseCcy := ""
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
	}
	complete = true
	add := func(row rpc.PositionView, isOption bool) {
		if row.Stale {
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
		net += dd * rate
	}
	for _, o := range pos.Options {
		add(o, true)
	}
	for _, st := range pos.Stocks {
		add(st, false)
	}
	return net, complete
}

// reduceSweepMaterialityFloor scales the net-delta materiality floor off net
// liquidation value when available, falling back to a small fixed-dollar
// floor.
func reduceSweepMaterialityFloor(pos *rpc.PositionsResult) float64 {
	if pos != nil && pos.Portfolio != nil && pos.Portfolio.NetLiquidationBase != nil && *pos.Portfolio.NetLiquidationBase > 0 {
		return *pos.Portfolio.NetLiquidationBase * minNetDeltaForSweepFraction
	}
	return minNetDeltaForSweepFloor
}

// reduceSweepCandidates implements the delta-adjusted risk-contribution
// sweep. It computes net portfolio dollar-delta, derives a target dollar
// amount to remove from percent, then selects positions in reduceEligible
// scope whose dollar-delta shares the net's sign — same-direction
// contributors. Opposite-sign positions are protective hedges and are never
// selected: trimming them would increase net risk, not reduce it, so the
// sign-matched ranking structurally excludes them without any separate flag.
// Each selected candidate's reduce quantity is sized proportional to its own
// share of total contributing risk (pro-rata, not greedy-largest-first), so a
// tiny-delta long option is never force-trimmed to hit the aggregate target —
// its allocated share is naturally small, and a share that floors to less
// than one unit is omitted entirely rather than padded up.
func reduceSweepCandidates(pos *rpc.PositionsResult, percent int) (cands []reduceSweepCandidate, netDelta float64, netComplete bool, targetDollarDelta float64, blockers []rpc.TradingBlocker) {
	if pos == nil {
		return nil, 0, false, 0, []rpc.TradingBlocker{{Code: "positions_unavailable", Message: "current positions are unavailable", Action: "Retry once the daemon has refreshed positions."}}
	}
	netDelta, netComplete = netPortfolioDollarDelta(pos)
	if math.Abs(netDelta) < reduceSweepMaterialityFloor(pos) {
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
// including basis-blind position context: DollarDelta/RiskContributionCut
// describe the risk being cut, PositionUnrealizedPnL(Base) is annotation only
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
// path. It returns the leg (with disclosure + per-leg blockers) and, when the
// leg is submit-eligible, the raw preview so the caller can redeem its token.
// A nil preview means the leg is terminal (blocked/not eligible) — never
// placed.
func (s *Server) reduceLegPrepare(ctx context.Context, c reduceSweepCandidate, timeoutMs int) (rpc.TradeProposalReduceLeg, *rpc.OrderPreviewResult) {
	leg := reduceLegBase(c)
	if len(c.blockers) > 0 {
		leg.Blockers = c.blockers
		return leg, nil
	}
	prep, blockers := preparedReduceWithQty(c.row, c.qty, timeoutMs)
	if len(blockers) > 0 {
		leg.Blockers = blockers
		return leg, nil
	}
	leg.Action = prep.action
	leg.ReduceQuantity = prep.qty
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

func (s *Server) reduceLegSubmit(ctx context.Context, c reduceSweepCandidate, timeoutMs int, origin string) rpc.TradeProposalReduceLeg {
	leg, preview := s.reduceLegPrepare(ctx, c, timeoutMs)
	if preview == nil {
		return leg // terminal: blocked or not submit-eligible; nothing placed
	}
	legCtx, cancel := legContext(ctx, timeoutMs)
	defer cancel()
	place, err := s.proposalPlaceOrder(legCtx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: timeoutMs, Origin: origin})
	if err != nil {
		leg.Blockers = []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error(), Action: "Reconcile this leg before retrying the sweep."}}
		return leg
	}
	leg.Placed = place.Accepted
	leg.Place = place
	leg.OrderRef = place.OrderRef
	leg.Message = place.Message
	return leg
}

// aggregateBasket fills the counts, base-currency total, achieved risk
// removal, and Accepted verdict. submit=true counts placed legs as eligible;
// preview counts submit-eligible legs.
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
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, AsOf: now}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	pos, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return nil, err
	}
	cands, netDelta, netComplete, target, blockers := reduceSweepCandidates(pos, p.Percent)
	res.NetDollarDeltaBefore = netDelta
	res.NetDeltaIncomplete = !netComplete
	res.TargetDollarDelta = target
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
		leg, _ := s.reduceLegPrepare(ctx, c, p.TimeoutMs)
		res.Legs = append(res.Legs, leg)
	}
	aggregateBasket(res, false)
	return res, nil
}

func (s *Server) reducePortfolioSubmit(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, AsOf: now}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	// Write gate before the dedupe replay: a freeze engaged between the
	// original call and a RequestRef retry must surface as a blocker, not
	// replay a pre-freeze "ok" result. Blocked results are never cached, so
	// a retry after unfreezing re-attempts rather than replaying.
	if blockers := s.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	if cached, ok := s.reduceBasketReplay(p.RequestRef); ok {
		return cached, nil
	}
	pos, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return nil, err
	}
	cands, netDelta, netComplete, target, blockers := reduceSweepCandidates(pos, p.Percent)
	res.NetDollarDeltaBefore = netDelta
	res.NetDeltaIncomplete = !netComplete
	res.TargetDollarDelta = target
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
		res.Legs = append(res.Legs, s.reduceLegSubmit(ctx, c, p.TimeoutMs, p.Origin))
	}
	aggregateBasket(res, true)
	s.reduceBasketStore(p.RequestRef, res)
	return res, nil
}

// reduceSweepActionableCount counts candidates that will become a real
// broker order (qty > 0); disclosure-only delta_unavailable rows never count
// toward the basket leg cap.
func reduceSweepActionableCount(cands []reduceSweepCandidate) int {
	n := 0
	for _, c := range cands {
		if c.qty > 0 {
			n++
		}
	}
	return n
}

// reduceBasketReplay returns a prior submit result for a repeated RequestRef
// (marked Replayed), sweeping expired entries on the way.
func (s *Server) reduceBasketReplay(ref string) (*rpc.TradeProposalReducePortfolioResult, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, false
	}
	s.reduceBasketMu.Lock()
	defer s.reduceBasketMu.Unlock()
	now := s.reduceClock()
	for k, e := range s.reduceBasketDedupe {
		if now.Sub(e.at) > reduceBasketDedupeTTL {
			delete(s.reduceBasketDedupe, k)
		}
	}
	e, ok := s.reduceBasketDedupe[ref]
	if !ok {
		return nil, false
	}
	clone := *e.result
	clone.Replayed = true
	return &clone, true
}

func (s *Server) reduceBasketStore(ref string, res *rpc.TradeProposalReducePortfolioResult) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	s.reduceBasketMu.Lock()
	defer s.reduceBasketMu.Unlock()
	if s.reduceBasketDedupe == nil {
		s.reduceBasketDedupe = make(map[string]reduceBasketDedupeEntry)
	}
	s.reduceBasketDedupe[ref] = reduceBasketDedupeEntry{at: s.reduceClock(), result: res}
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
	// lock around all legs, matching handleTradeProposalsSubmit's discipline.
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.reducePortfolioSubmit(ctx, p)
}
