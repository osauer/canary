package daemon

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The mandate's forced reduction (internal-docs/design/mandate-risk-budget.md,
// "Forced reduction"; owner decision 2026-10-14 06:26 CEST, "Protection that
// acts": "on a budget breach, one reduce-only batch sized back to budget,
// largest risk contributor first, pre-authorised with the 30-minute veto. The
// brake releases when risk is back in budget, not when P&L recovers.").
//
// While the risk constitution carries [mandate], two consecutive scheduled
// verdicts at least five minutes apart reading reduce start a breach
// episode: every generation then sizes one batch of mandate_reduction rows
// afresh, with the daemon's own evaluator on the book the verdict measured.
// Cut by cut, the book re-evaluated after each: the largest gap loser of a
// name over its gap cap, then the largest premium at risk left, name by name
// (spec TM-04.12, owner decision 2026-10-27 19:50 CET, replacing the
// largest Euler contributor of the volatility headline), each by the
// smallest whole step (contracts, or share lots) that takes the loss
// measure to 0.95 × the loss room with every gap cap met, or else by the
// step that takes the most off it. Short options are never
// bought back, and the shares that cover them are never sold: a covered
// position needs the owner's hand. Shares are never cut below what the long
// puts on them would deliver at exercise, and protection is never sold. An
// in-the-money long option close to expiry is cut first only where its
// exercise alone would push the planned book over budget. Every row is
// reduce-only, NeverSkipVeto, bounded by the order caps in force, and carries
// the verdict it was sized on and its episode, inside which a re-sized row
// keeps its veto clock. A verdict reading within ends the episode and
// withdraws the rows (orders at the broker are never cancelled); an unknown
// verdict, or one measured on another book than the one held, holds them,
// unsent, with the reason. Nothing here places an order: the rows go through
// the ordinary proposal path, and the daemon sends them only when the owner
// lists mandate_reduction under [authority].pre_authorised.

const (
	// mandateReductionTargetFactor sizes the batch to this share of the
	// loss room, so the next verdict reads within rather than over by a hair.
	mandateReductionTargetFactor = 0.95
	// mandateReductionExpirySessions is how close to expiry an in-the-money
	// long option is judged by its exercise.
	mandateReductionExpirySessions = 3
	// mandateReductionOverReadings is how many consecutive scheduled over or
	// reduce verdicts, gap-cap breaches included, start the batch.
	mandateReductionOverReadings = 2
	// mandateReductionMaxCuts bounds the sizing loop.
	mandateReductionMaxCuts = 64
	mandateReductionEpsilon = 1e-6
)

// mandateSizingHeldCode is the preview and submit refusal of a held row.
const mandateSizingHeldCode = "mandate_sizing_held"

// automaticHoldMandateVerdict is the hold the scheduler records on a due
// mandate record whose row is held: the verdict cannot confirm the sizing.
const automaticHoldMandateVerdict = "held: the mandate verdict this reduction was sized on cannot be confirmed right now; it is placed once a fresh verdict confirms the sizing, or withdrawn once the risk is back within budget, without a new veto window"

// mandateReductionInput is what the forced reduction reads: the kept verdict
// and the evaluator input it rests on, the order caps, the working orders,
// and the standing rows and episode of the last generation. The engine
// gathers it; tests build it directly.
type mandateReductionInput struct {
	// Active is true while the constitution carries [mandate].
	Active bool
	// Result and Input are the kept verdict and the evaluator input it was
	// measured on; EvaluatedAt when; OverStreak the consecutive scheduled
	// over or reduce readings (mandateVerdictCache).
	Result      *rpc.MandateRiskResult
	Input       *mandate.Input
	EvaluatedAt time.Time
	OverStreak  int
	// MaxOrderNotional is [buckets.risk_reduction].max_order_notional, the
	// reduction cap the issuer trim uses; OrderCapBase the [order_limits] cap
	// in force in base currency (0 when unknown). Both bound one order.
	MaxOrderNotional float64
	OrderCapBase     float64
	// Ignored reports a row key the owner ignored; its leg leaves the batch.
	Ignored func(key string) bool
	// WorkingOrder reports a same-side order working at the broker for a
	// held line, and whether it is conditional (a stop or a trail): netting
	// holds a reduction row behind any of them, and a conditional one may
	// stand for good, so its line leaves the plan. Nil reads none.
	WorkingOrder func(row rpc.PositionView, action string) (found, conditional bool)
	// Locked reports a line on the owner's keep list; its leg leaves the
	// batch. An unreadable keep list holds the batch through LocksUnknown.
	Locked func(conID int) bool
	// LocksUnknown is the reason the owner's keep list cannot be confirmed.
	LocksUnknown string
	// Previous holds the mandate_reduction rows of the last generation in
	// this scope, and Episode its breach episode.
	Previous []rpc.TradeProposal
	Episode  string
}

// resolveMandateInput honours the test seam, else reads the daemon's own
// verdict cache, order caps and working orders; the standing rows, the
// episode and the ignore check come from the engine unless the seam set them.
func (e *proposalEngine) resolveMandateInput(ctx context.Context, policy protectionPolicy, pos *rpc.PositionsResult, scope brokerStateScope, now time.Time) mandateReductionInput {
	var in mandateReductionInput
	if e != nil && e.mandateInput != nil {
		in = e.mandateInput(now)
	} else {
		in = e.mandateReductionInput(ctx, policy, pos, scope)
	}
	if e != nil {
		if in.Previous == nil && in.Episode == "" {
			in.Previous, in.Episode = e.standingMandateRows(scope)
		}
		if in.Ignored == nil {
			in.Ignored = func(key string) bool { return e.isIgnored(scope, key) }
		}
	}
	return in
}

// mandateReductionInput reads the kept verdict for the policy in force, the
// order caps and the broker's working orders. A daemon without the
// risk-policy manager reads inactive.
func (e *proposalEngine) mandateReductionInput(ctx context.Context, policy protectionPolicy, pos *rpc.PositionsResult, scope brokerStateScope) mandateReductionInput {
	in := mandateReductionInput{MaxOrderNotional: policy.Buckets.RiskReduction.MaxOrderNotional}
	if e == nil || e.server == nil || e.server.riskPolicies == nil {
		return in
	}
	s := e.server
	c := s.riskPolicies.snapshot().policy
	if !c.MandateActive() {
		return in
	}
	in.Active = true
	if kept := s.keptMandateVerdict(); kept.result != nil && kept.fingerprint == c.FingerprintKey() {
		in.Result, in.Input, in.EvaluatedAt, in.OverStreak = kept.result, kept.input, kept.evaluatedAt, kept.overStreak
	}
	if limits := s.orderLimitsInForce(protectionCoverageBaseCurrency(pos)); limits.Complete {
		in.OrderCapBase = limits.CapBase
	}
	in.LocksUnknown = "the locked holdings cannot be read without a request context"
	if ctx != nil {
		if locks, err := s.holdingLocks(ctx); err == nil {
			in.LocksUnknown = ""
			in.Locked = locks.Resolve(pos).Locked
		} else {
			in.LocksUnknown = "the locked holdings cannot be read; automatic reductions wait until the keep list is available"
			if s.logger != nil {
				s.logger.Warnf("mandate reduction held: locked holdings unreadable: %v", err)
			}
		}
		if snapshot, inventoryScope, err := s.brokerOpenOrderInventory(ctx, false); err == nil && sameBrokerScope(inventoryScope, scope) {
			in.WorkingOrder = func(row rpc.PositionView, action string) (bool, bool) {
				secType := positionWireSecType(row.SecType)
				probe := rpc.TradeProposal{Action: action, SecType: secType, Contract: proposalContractFromPosition(row, secType)}
				order, found, _ := sameContractWorkingOrder(snapshot, inventoryScope, probe)
				return found, found && mandateConditionalOrder(order.OrderType)
			}
		}
	}
	return in
}

// mandateConditionalOrder reports a stop or a trail: an order that may work
// at the broker for good without filling.
func mandateConditionalOrder(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT, "STP", "STP LMT", "STOP", "STOP LIMIT":
		return true
	}
	return false
}

// standingMandateRows returns the last generation's mandate rows and breach
// episode for scope.
func (e *proposalEngine) standingMandateRows(scope brokerStateScope) ([]rpc.TradeProposal, string) {
	e.mu.Lock()
	snap := e.snapshot
	e.mu.Unlock()
	if snap.Kind == "" || !sameBrokerScope(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}, scope) {
		return nil, ""
	}
	episode := ""
	if snap.MandateReduction != nil {
		episode = snap.MandateReduction.Episode
	}
	var out []rpc.TradeProposal
	for _, p := range snap.Proposals {
		if p.Bucket != rpc.TradeProposalBucketMandateReduction || p.Mandate == nil {
			continue
		}
		row := p
		row.Mandate = rpc.CloneProposalMandate(p.Mandate)
		row.Details = slices.Clone(p.Details)
		out = append(out, row)
	}
	return out, episode
}

// mandateCut is one leg of the batch: the evaluator leg, the position row
// the order reduces, and the cut in whole units (contracts or shares).
type mandateCut struct {
	id   string
	leg  mandate.Leg
	row  rpc.PositionView
	full int // the whole units held, the most the cut may reach
	step int // the lot the cut moves in: 1 contract, or 1, 10 or 100 shares
	// units is the cut; rank its place in the batch (1 = first); phase why;
	// sleeve the allocation the leg is placed in.
	units  int
	rank   int
	phase  string
	sleeve string
	// loss is the leg's part of the loss measure on the verdict and lossCut
	// what the cut takes off it in its place in the batch; contribution the
	// leg's Euler share of the variance and riskCut the volatility the cut
	// removes; gapLoss and gapCap a gap row's figures; sessions an expiry
	// row's sessions to expiry.
	loss         float64
	lossCut      float64
	contribution float64
	gapLoss      *float64
	gapCap       *float64
	sessions     *int
	riskCut      float64
	// prevUnits and prevQty are the last generation's cut and order on this
	// line, when its position has not moved since.
	prevUnits int
	prevQty   int
}

// mandateBatch is the sized batch: the loss measure before and after it,
// the loss room (budget) and the target it sizes to, and the volatility
// before and after it, reported beside.
type mandateBatch struct {
	cuts       []*mandateCut
	lossBefore float64
	lossAfter  float64
	riskBefore float64
	riskAfter  float64
	budget     float64
	target     float64
	reached    bool
	leftOut    []string
}

// mandateReductionProposals sizes the batch afresh from the kept verdict,
// holds the standing rows while no verdict can be trusted for the book as
// held, and withdraws them once the risk is back within budget. It returns
// no status while the constitution carries no [mandate]: nothing changes
// anywhere.
func (e *proposalEngine) mandateReductionProposals(policy protectionPolicy, status rpc.ProtectionPolicyStatus, in mandateReductionInput, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalMandateStatus) {
	if !in.Active || pos == nil {
		return nil, nil
	}
	st := &rpc.TradeProposalMandateStatus{BaseCurrency: protectionCoverageBaseCurrency(pos), Episode: in.Episode}
	standing := mandateStandingRows(in.Previous)
	hold := func(state, why string) ([]rpc.TradeProposal, *rpc.TradeProposalMandateStatus) {
		st.State, st.Reason = state, why
		return e.mandateHeldRows(policy, status, standing, acct, pos, sources, marketEvents, now, st, why)
	}
	if in.LocksUnknown != "" {
		return hold(rpc.MandateReductionUnknown, in.LocksUnknown)
	}
	res := in.Result
	if res == nil || res.Verdict == nil || in.Input == nil {
		return hold(rpc.MandateReductionNoVerdict, "no mandate verdict has been measured for the policy in force yet")
	}
	v := res.Verdict
	ccy := nonEmptyString(v.BaseCurrency, st.BaseCurrency)
	st.VerdictStatus, st.VerdictAt, st.VerdictFingerprint, st.ConsecutiveOver = v.Status, v.AsOf, mandateVerdictFingerprint(res), in.OverStreak
	// The owner sleeve's drawdown stop acts through this same batch: engaged
	// with risk to cut, it is a breach reading whatever the book reads
	// (mandate_sleeve_stop.go).
	stopOver := mandateStopOver(*v)
	if v.Capital.Known {
		st.BudgetAnnualSD = v.Capital.BudgetAnnualSD
	}
	if v.Risk != nil {
		st.RiskAnnualBase = v.Risk.SDAnnualBase
	}
	if v.Loss != nil {
		st.LossBase, st.LossRoomBase, st.LossTargetBase = v.Loss.TotalBase, v.Loss.RoomBase, mandateReductionTargetFactor*v.Loss.RoomBase
	}
	age := now.Sub(in.EvaluatedAt)
	switch {
	case v.Status == mandate.StatusUnknown:
		why := "the mandate verdict cannot be measured"
		if len(v.Reasons) > 0 {
			why += ": " + v.Reasons[0]
		}
		return hold(rpc.MandateReductionUnknown, why)
	case in.EvaluatedAt.IsZero() || age > mandateVerdictMaxAge:
		return hold(rpc.MandateReductionUnknown, fmt.Sprintf("the verdict is %s old; the batch trusts one up to %s", formatQuoteAge(age), mandateVerdictMaxAge))
	case v.Status == mandate.StatusWithin && !stopOver:
		// The brake releases when the loss measure is back in its room: the episode
		// ends and every standing row leaves the set, so a waiting automatic
		// record is superseded. Nothing is cancelled at the broker. An owner
		// sleeve stop still engaged keeps the episode open: the sleeve is at
		// its stop target, reduce-only until its drawdown is back under the
		// stop, and a rise in its risk is sized again inside the episode.
		st.State, st.Withdrawn = rpc.MandateReductionWithin, len(standing)
		st.Reason = fmt.Sprintf("the loss measure %s is within the mandate's loss room %s", mandateMoney(st.LossBase, ccy), mandateMoney(st.LossRoomBase, ccy))
		if stop := v.OwnerStop; stop != nil && stop.Engaged {
			st.Reason += fmt.Sprintf("; %s, and the sleeve's risk %s a year is at or under its stop target %s: the sleeve stays reduce-only %s",
				mandateOwnerStopSentence(stop, ccy), mandateMoney(stop.UseBase, ccy), mandateMoney(stop.TargetUseBase, ccy), mandateOwnerStopReleaseClause(stop))
		} else {
			st.Episode = ""
		}
		if st.Withdrawn > 0 {
			st.Reason += fmt.Sprintf("; %d standing %s withdrawn, orders already at the broker are untouched", st.Withdrawn, pluralNoun(st.Withdrawn, "row"))
		}
		return nil, st
	}
	// A verdict sizes only the book it measured: after a fill or a hand trade
	// it would cut what is already gone.
	if drift := mandateBookDrift(*in.Input, pos, now); drift != "" {
		return hold(rpc.MandateReductionBookChanged, fmt.Sprintf("the positions changed since the verdict of %s (%s); the batch waits for a verdict on the book as it is", mandateClock(v.AsOf), drift))
	}
	if in.Episode == "" && in.OverStreak < mandateReductionOverReadings {
		st.State = rpc.MandateReductionWatching
		st.Reason = fmt.Sprintf("%s; a confirming scheduled verdict at least %s later starts the batch", mandateBreachSentence(v, st.LossRoomBase, ccy), mandateStreakSpacing)
		return nil, st
	}
	if st.Episode == "" {
		st.Episode = "mandate-" + v.AsOf.UTC().Format("20060102T150405Z") + "-" + st.VerdictFingerprint
	}
	candidates, leftOut, covered := e.mandateCandidates(policy, in, pos, now)
	batch := mandateSizeBatch(*in.Input, candidates, standing, now)
	batch.leftOut = append(leftOut, batch.leftOut...)
	st.LeftOut = batch.leftOut
	st.LossRoomBase, st.LossTargetBase, st.LossBase, st.RiskAnnualBase = batch.budget, batch.target, batch.lossBefore, batch.riskBefore
	st.TargetReached = batch.reached
	if len(batch.cuts) == 0 {
		st.State = rpc.MandateReductionNothingReducible
		st.Reason = fmt.Sprintf("%s, but no held leg can be reduced", mandateBreachSentence(v, batch.budget, ccy))
		if len(batch.leftOut) > 0 {
			st.Reason += ": " + strings.Join(batch.leftOut, "; ")
		}
		if len(covered) > 0 {
			st.Reason += "; a covered position needs your hand: " + strings.Join(covered, "; ")
		}
		return nil, st
	}
	st.ProjectedLossBase, st.ProjectedRiskAnnualBase = new(batch.lossAfter), new(batch.riskAfter)
	var out []rpc.TradeProposal
	partialOrders := 0
	for _, c := range batch.cuts {
		qty, waits := mandateCycleQuantity(c, in, st.BaseCurrency)
		st.Plan = append(st.Plan, rpc.TradeProposalMandatePlanOrder{Rank: c.rank, Contract: budgetDisplayContract(c.row), Action: mandateAction(c.row),
			Units: c.units, Quantity: qty, Waits: waits, Phase: c.phase, LossBase: c.loss, LossCutBase: c.lossCut, ContributionPct: c.contribution, RiskCutAnnualBase: c.riskCut, Sleeve: c.sleeve})
		if qty <= 0 {
			continue
		}
		if qty < c.units {
			partialOrders++
		}
		p := e.mandateReductionRow(policy, status, sources, now, in, st, batch, c, qty, waits, ccy)
		if c.row.Stale && strings.EqualFold(positionWireSecType(c.row.SecType), "OPT") {
			budgetBlock(&p, rpc.TradingBlocker{Code: "fresh_option_quote_required", Message: "the position mark is stale, so the order cannot be priced from it", Action: "Refresh during the options session (09:30-16:00 ET) so the mark is current before acting."})
		}
		enrichProposalPositionContext(&p, c.row, acct)
		applyMarketEventFlagsToProposal(&p, marketEvents)
		out = append(out, p)
	}
	st.Rows = len(out)
	st.State = rpc.MandateReductionReducing
	st.Reason = fmt.Sprintf("%s; the full plan of %d %s is sized to take the loss measure to %s if all planned units fill", mandateBreachSentence(v, batch.budget, ccy), len(batch.cuts), pluralNoun(len(batch.cuts), "cut"), mandateMoney(batch.lossAfter, ccy))
	switch {
	case batch.reached && v.Status == mandate.StatusWithin && stopOver:
		st.Reason += fmt.Sprintf(", the sleeve's risk to its stop target %s", mandateMoney(v.OwnerStop.TargetUseBase, ccy))
	case batch.reached:
		st.Reason += fmt.Sprintf(", inside the %s target", mandateMoney(batch.target, ccy))
	default:
		st.Reason += fmt.Sprintf("; even the full plan does not reach the %s target, with no further eligible cut on this verdict", mandateMoney(batch.target, ccy))
		if len(covered) > 0 {
			st.Reason += "; a covered position needs your hand: " + strings.Join(covered, "; ")
		}
	}
	if partialOrders > 0 {
		st.Reason += fmt.Sprintf("; this cycle offers %d %s, including %d partial %s; after fills, a fresh verdict measures the remaining cuts", len(out), pluralNoun(len(out), "order"), partialOrders, pluralNoun(partialOrders, "installment"))
	}
	return out, st
}

// mandateBreachSentence says what the batch answers: the loss measure over
// its room, or, inside the room, the owner sleeve's stop with risk still to
// cut.
func mandateBreachSentence(v *mandate.Verdict, room float64, ccy string) string {
	if v.Status == mandate.StatusWithin && mandateStopOver(*v) {
		return fmt.Sprintf("the loss measure is within the mandate's maximum loss, but %s (risk %s a year, over its stop target %s)", mandateOwnerStopSentence(v.OwnerStop, ccy), mandateMoney(v.OwnerStop.UseBase, ccy), mandateMoney(v.OwnerStop.TargetUseBase, ccy))
	}
	return fmt.Sprintf("the verdict reads %s (the loss measure %s is %s of the loss room %s)", v.Status, mandateMoney(mandateLossOf(v), ccy), mandatePctOfRoom(v), mandateMoney(room, ccy))
}

// mandateAction is the reducing side of a held line.
func mandateAction(row rpc.PositionView) string {
	if row.Quantity < 0 {
		return rpc.OrderActionBuy
	}
	return rpc.OrderActionSell
}

// mandateClock is a verdict time as the rows state it.
func mandateClock(at time.Time) string {
	return at.Local().Format("15:04 MST")
}

// mandatePctOfRoom states the verdict's loss measure as a share of the
// loss room.
func mandatePctOfRoom(v *mandate.Verdict) string {
	if v == nil || v.Loss == nil || math.IsInf(v.Loss.RatioToRoom, 0) || v.Loss.RoomBase <= 0 {
		return "all"
	}
	return fmt.Sprintf("%.0f%%", v.Loss.RatioToRoom*100)
}

// mandateLossOf is a verdict's loss measure, zero when it carries none.
func mandateLossOf(v *mandate.Verdict) float64 {
	if v == nil || v.Loss == nil {
		return 0
	}
	return v.Loss.TotalBase
}

// mandateBookDrift compares the book the verdict measured with the positions
// held now, leg by leg; empty when they agree, else the first differences.
func mandateBookDrift(in mandate.Input, pos *rpc.PositionsResult, now time.Time) string {
	held, _, _, _ := mandateLegsFromPositions(pos, in.BaseCurrency, now)
	sum := func(legs []mandate.Leg) (map[string]float64, map[string]string) {
		qty, names := map[string]float64{}, map[string]string{}
		for _, l := range legs {
			qty[l.ID] += l.Quantity
			names[l.ID] = l.Symbol
		}
		return qty, names
	}
	was, wasNames := sum(in.Legs)
	is, isNames := sum(held)
	var diffs []string
	for id, q := range was {
		if math.Abs(q-is[id]) > 1e-9 {
			diffs = append(diffs, fmt.Sprintf("%s %g → %g", wasNames[id], q, is[id]))
		}
	}
	for id, q := range is {
		if _, ok := was[id]; !ok && math.Abs(q) > 1e-9 {
			diffs = append(diffs, fmt.Sprintf("%s 0 → %g", isNames[id], q))
		}
	}
	slices.Sort(diffs)
	if len(diffs) > 3 {
		diffs = append(diffs[:3], fmt.Sprintf("%d more", len(diffs)-3))
	}
	return strings.Join(diffs, "; ")
}

// mandateStandingRows keeps the standing batch's rows, in rank order.
func mandateStandingRows(previous []rpc.TradeProposal) []rpc.TradeProposal {
	var out []rpc.TradeProposal
	for _, p := range previous {
		if p.Bucket == rpc.TradeProposalBucketMandateReduction && p.Mandate != nil && p.Contract.ConID > 0 && p.Quantity > 0 {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b rpc.TradeProposal) int { return cmp.Compare(a.Mandate.Rank, b.Mandate.Rank) })
	return out
}

// mandateHeldRows carries the standing batch forward, unsent, while no
// verdict can confirm its sizing: every row keeps its key, quantity and
// effect (so its revision and its veto window stand) and says why it waits.
// A row whose position moved since it was sized is dropped; a fresh verdict
// sizes again. Without a standing batch nothing is proposed.
func (e *proposalEngine) mandateHeldRows(policy protectionPolicy, status rpc.ProtectionPolicyStatus, standing []rpc.TradeProposal, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, now time.Time, st *rpc.TradeProposalMandateStatus, why string) ([]rpc.TradeProposal, *rpc.TradeProposalMandateStatus) {
	if len(standing) == 0 {
		return nil, st
	}
	note := "held: " + why + "; not sent, by the daemon or by hand, until a fresh verdict confirms the sizing; the veto window keeps running"
	rows := mandatePositionRows(pos)
	var out []rpc.TradeProposal
	for _, prev := range standing {
		row, ok := rows[mandateLegID(rpc.PositionView{ConID: prev.Contract.ConID})]
		if !ok || row.Quantity != prev.PositionQuantity {
			continue
		}
		full := int(math.Floor(math.Abs(row.Quantity) + 1e-9))
		if prev.Quantity > full {
			continue
		}
		p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketMandateReduction, row, prev.Action, prev.Quantity, prev.PositionEffect, prev.Reason)
		p.Mandate = rpc.CloneProposalMandate(prev.Mandate)
		p.Mandate.Held, p.Mandate.HoldReason = true, why
		p.NeverSkipVeto = true
		p.Score = prev.Score
		for _, d := range prev.Details {
			if !strings.HasPrefix(d, "held: ") {
				p.Details = append(p.Details, d)
			}
		}
		p.Details = append(p.Details, note)
		enrichProposalPositionContext(&p, row, acct)
		applyMarketEventFlagsToProposal(&p, marketEvents)
		out = append(out, p)
		st.Plan = append(st.Plan, rpc.TradeProposalMandatePlanOrder{Rank: p.Mandate.Rank, Contract: budgetDisplayContract(row), Action: p.Action,
			Units: p.Mandate.Units, Quantity: p.Quantity, Phase: p.Mandate.Phase, LossBase: p.Mandate.LegLossBase, LossCutBase: p.Mandate.LossCutBase, ContributionPct: p.Mandate.ContributionPct, RiskCutAnnualBase: p.Mandate.RiskCutAnnualBase})
	}
	if len(out) == 0 {
		return nil, st
	}
	first := out[0].Mandate
	st.State, st.Held, st.HoldReason, st.Rows = rpc.MandateReductionHeld, true, why, len(out)
	st.VerdictStatus, st.VerdictAt, st.VerdictFingerprint = first.VerdictStatus, first.VerdictAt, first.VerdictFingerprint
	st.RiskAnnualBase, st.BudgetAnnualSD = first.RiskAnnualBase, first.BudgetAnnualSD
	st.LossBase, st.LossRoomBase, st.LossTargetBase = first.LossBase, first.LossRoomBase, first.LossTargetBase
	if st.Episode == "" {
		st.Episode = first.Episode
	}
	st.Reason = fmt.Sprintf("%d standing %s held: %s", len(out), pluralNoun(len(out), "row"), why)
	return out, st
}

// mandatePositionRows indexes the held stock and option rows by leg id.
func mandatePositionRows(pos *rpc.PositionsResult) map[string]rpc.PositionView {
	out := map[string]rpc.PositionView{}
	if pos == nil {
		return out
	}
	for _, row := range pos.Stocks {
		if positionQuotesAsStock(row) {
			if _, seen := out[mandateLegID(row)]; !seen {
				out[mandateLegID(row)] = row
			}
		}
	}
	for _, row := range pos.Options {
		if _, seen := out[mandateLegID(row)]; !seen {
			out[mandateLegID(row)] = row
		}
	}
	return out
}

// mandateMergedLegs sums the verdict's legs per contract, in their order. A
// line the owner and a register entry both hold is split in the verdict
// (strategy_placement.go); the reduction sizes it as one line, its row on
// the position the broker holds, so the entry's units are within its reach,
// and its fill takes the owner's units first, then the entry's (placement
// rule 2, strategy_ledger.go).
func mandateMergedLegs(legs []mandate.Leg) []mandate.Leg {
	index := map[string]int{}
	var out []mandate.Leg
	for _, l := range legs {
		if strings.HasPrefix(l.ID, "sleeve:") {
			if id := mandateLegConID(l.ID); id > 0 {
				l.ID = mandateConLegID(id)
			}
		}
		if i, ok := index[l.ID]; ok {
			out[i].Quantity += l.Quantity
			continue
		}
		index[l.ID] = len(out)
		out = append(out, l)
	}
	return out
}

// mandateCandidates lists the legs the batch may cut, each stock no further
// than the shares its own options leave free. Never cut: a short option
// (buying it back is the owner's: short options come from hand trades, and
// unwinding one with its shares takes a combination order Canary does not
// route), the shares short options are covered by, the shares long options
// would deliver or take at exercise (so an exercise never flips the line), a
// protection leg (a Rulebook-classified index put, a long option that protects its own
// stock, a leg the option policy derives as protection: the budget
// governor's test), a leg of a multi-leg unit (the strategy workflow reduces
// it), a row the owner ignored, and a line a working stop or trail holds
// (netting would hold the row for as long as it works). leftOut names what
// was left out and why; covered names each covered position the batch
// leaves to the owner.
func (e *proposalEngine) mandateCandidates(policy protectionPolicy, in mandateReductionInput, pos *rpc.PositionsResult, now time.Time) (map[string]*mandateCut, []string, []string) {
	rows := mandatePositionRows(pos)
	cfg := policy.Buckets.TrailingStop.Options
	unitLegs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(cfg), now)
	pol := e.rulebookPolicy()
	legs := mandateMergedLegs(in.Input.Legs)
	// What each underlying's options hold of its shares, in shares.
	type optionHold struct{ shortCalls, shortPuts, longCalls, longPuts float64 }
	holds := map[string]*optionHold{}
	for _, l := range legs {
		if l.Kind != mandate.KindOption || l.Quantity == 0 {
			continue
		}
		h := holds[l.Underlying]
		if h == nil {
			h = &optionHold{}
			holds[l.Underlying] = h
		}
		shares := math.Abs(l.Quantity) * l.Multiplier
		switch {
		case l.Quantity < 0 && l.Right == "C":
			h.shortCalls += shares
		case l.Quantity < 0 && l.Right == "P":
			h.shortPuts += shares
		case l.Right == "C":
			h.longCalls += shares
		case l.Right == "P":
			h.longPuts += shares
		}
	}
	out := map[string]*mandateCut{}
	var leftOut, covered []string
	for _, leg := range legs {
		row, held := rows[leg.ID]
		if !held || leg.Quantity == 0 || row.Quantity == 0 || (leg.Quantity > 0) != (row.Quantity > 0) {
			continue
		}
		name := legDesc(row)
		if positionWireSecType(row.SecType) == "STK" {
			name = normSym(row.Symbol)
		}
		action := mandateAction(row)
		option := leg.Kind == mandate.KindOption
		switch {
		case option && leg.Quantity < 0:
			leftOut = append(leftOut, name+" (a short option: buying it back is yours, the reduction never does)")
			continue
		case option && budgetLegRole(cfg, row, pos, unitLegs, ambiguous, pol, now) == risk.IndexPutRoleProtection:
			leftOut = append(leftOut, name+" (protection: the reduction never sells it)")
			continue
		case option && budgetLegRole(cfg, row, pos, unitLegs, ambiguous, pol, now) == risk.IndexPutRoleUnclassified:
			leftOut = append(leftOut, name+" (index-put classification unavailable; reduction waits for complete position data)")
			continue
		case option && (unitLegs[row.ConID] || ambiguous[normSym(row.Symbol)]):
			leftOut = append(leftOut, name+" (a leg of a multi-leg unit; reduce it through the strategy workflow)")
			continue
		case in.Locked != nil && in.Locked(row.ConID):
			leftOut = append(leftOut, name+" (locked: you keep it out of every reduction)")
			continue
		case in.Ignored != nil && in.Ignored(proposalKey(rpc.TradeProposalBucketMandateReduction, proposalContractFromPosition(row, positionWireSecType(row.SecType)), action)):
			leftOut = append(leftOut, name+" (its row is ignored)")
			continue
		}
		if in.WorkingOrder != nil {
			if found, conditional := in.WorkingOrder(row, action); found && conditional {
				leftOut = append(leftOut, name+" (a working stop or trail holds this line, so the reduction cannot "+strings.ToLower(action)+" beside it and the next line is cut instead)")
				continue
			}
		}
		full := int(math.Floor(math.Min(math.Abs(leg.Quantity), math.Abs(row.Quantity)) + 1e-9))
		if leg.Kind == mandate.KindStock {
			if h := holds[leg.Underlying]; h != nil {
				// A long line covers short calls and stays for the long puts'
				// exercise; a short sale covers short puts and stays for the
				// long calls'.
				cover, deliver, what := h.shortCalls, h.longPuts, "calls"
				if leg.Quantity < 0 {
					cover, deliver, what = h.shortPuts, h.longCalls, "puts"
				}
				shares := math.Abs(row.Quantity)
				if cover > 0 {
					covered = append(covered, fmt.Sprintf("%s: %.0f shares cover short %s", name, math.Min(shares, cover), what))
				}
				if keep := math.Max(cover, deliver); keep > 0 {
					full = min(full, int(math.Floor(shares-keep+1e-9)))
					switch {
					case full > 0:
					case cover > 0:
						leftOut = append(leftOut, fmt.Sprintf("%s (its shares cover short %s: a covered position needs your hand)", name, what))
					default:
						leftOut = append(leftOut, name+" (its shares stay for what the long options on it deliver at exercise)")
					}
				}
			}
		}
		if full <= 0 {
			continue
		}
		c := &mandateCut{id: leg.ID, leg: leg, row: row, full: full, step: 1}
		if leg.Kind == mandate.KindStock {
			c.step = mandateShareLot(full)
		}
		out[leg.ID] = c
	}
	return out, leftOut, covered
}

// mandateShareLot is the lot a stock cut moves in: a tenth of the line at
// most, in round numbers, so a cut is a whole number of shares a trader
// would type and small moves in the risk do not re-size it.
func mandateShareLot(shares int) int {
	switch {
	case shares >= 1000:
		return 100
	case shares >= 100:
		return 10
	default:
		return 1
	}
}

// mandateSizer sizes one batch on the verdict's book.
type mandateSizer struct {
	in         mandate.Input
	candidates map[string]*mandateCut
	ids        []string
	cuts       []*mandateCut
	// budget is the loss room and target the share of it the batch sizes to;
	// volBudget the volatility budget the expiry gate judges an exercise by.
	budget    float64
	target    float64
	volBudget float64
	leftOut   []string
	// expiry and sessions are the expiry gate's cuts, which lead every pass.
	expiry   map[string]int
	sessions map[string]int
}

// book is the evaluator input with every cut applied, over in place of a
// cut's own units.
func (z *mandateSizer) book(over map[string]int) mandate.Input {
	out := z.in
	out.Legs = slices.Clone(z.in.Legs)
	for i := range out.Legs {
		l := &out.Legs[i]
		units := 0
		if c, ok := z.candidates[l.ID]; ok {
			units = c.units
		}
		if u, ok := over[l.ID]; ok {
			units = u
		}
		if units <= 0 {
			continue
		}
		cut := math.Min(float64(units), math.Abs(l.Quantity))
		if l.Quantity > 0 {
			l.Quantity -= cut
		} else {
			l.Quantity += cut
		}
	}
	return out
}

// eval evaluates book(over).
func (z *mandateSizer) eval(over map[string]int) mandate.Verdict {
	return mandate.Evaluate(z.book(over))
}

// commit applies one leg's cut, the leg joining the batch in the given
// phase the first time.
func (z *mandateSizer) commit(c *mandateCut, units int, phase string) {
	if units <= c.units {
		return
	}
	c.units = units
	if !slices.Contains(z.cuts, c) {
		c.phase = phase
		z.cuts = append(z.cuts, c)
	}
}

// move sizes one leg's next cut against f to limit, on the leg's own grid.
// It returns the cut, and false when no step lowers f.
func (z *mandateSizer) move(c *mandateCut, f func(over map[string]int) float64, limit float64) (int, bool) {
	return mandateCutTo(c, c.units, func(u int) float64 { return f(map[string]int{c.id: u}) }, limit)
}

// mandateSizeBatch sizes the batch afresh on the verdict's book, the sleeve
// furthest over its allocation first. The allocations' order never stops short where
// the whole book's order (largest contributor first) reaches the target:
// the reduction protects the whole book first, so it then takes that one.
func mandateSizeBatch(verdictBook mandate.Input, candidates map[string]*mandateCut, standing []rpc.TradeProposal, now time.Time) mandateBatch {
	for id, c := range candidates {
		c.sleeve = verdictBook.Allocations.SleeveOf(id)
	}
	return mandateSizeBatchBy(verdictBook, candidates, standing, now)
}

// mandateSizeBatchBy sizes one batch on the verdict's book: cut by cut,
// re-evaluating the book after each, the gap breaches and then the largest
// premium at risk left, name by name, to 0.95 × the loss room; then the
// expiry gate, sizing again around any option it cuts; then the damping
// against the standing batch.
func mandateSizeBatchBy(verdictBook mandate.Input, candidates map[string]*mandateCut, standing []rpc.TradeProposal, now time.Time) mandateBatch {
	in := verdictBook
	in.Legs = mandateMergedLegs(verdictBook.Legs)
	z := &mandateSizer{in: in, candidates: candidates, expiry: map[string]int{}, sessions: map[string]int{}}
	base := mandate.Evaluate(in)
	z.budget = mandate.LossRoomBase(in.Policy, in.Capital)
	z.target = mandateReductionTargetFactor * z.budget
	z.volBudget = base.Capital.BudgetAnnualSD
	for _, rep := range base.Legs {
		if c, ok := candidates[rep.ID]; ok {
			c.contribution, c.loss = rep.ContributionPct, rep.LossBase
		}
	}
	for id := range candidates {
		z.ids = append(z.ids, id)
	}
	slices.Sort(z.ids)
	for pass := 0; ; pass++ {
		z.reset()
		z.sizeMain()
		if pass == 3 || !z.expiryGate(now) {
			break
		}
	}
	mandateKeepStanding(z, standing)
	batch := mandateBatch{budget: z.budget, target: z.target, leftOut: z.leftOut}
	if base.Risk != nil {
		batch.riskBefore = base.Risk.SDAnnualBase
	}
	if base.Loss != nil {
		batch.lossBefore = base.Loss.TotalBase
	}
	// Expiry cuts lead the batch; the rest keep the order they were cut in.
	batch.cuts = slices.DeleteFunc(slices.Clone(z.cuts), func(c *mandateCut) bool { return c.units <= 0 })
	slices.SortStableFunc(batch.cuts, func(a, b *mandateCut) int {
		return cmp.Compare(boolRank(a.phase != rpc.MandatePhaseExpiry), boolRank(b.phase != rpc.MandatePhaseExpiry))
	})
	riskBefore, lossBefore := batch.riskBefore, batch.lossBefore
	applied := map[string]int{}
	for id := range candidates {
		applied[id] = 0
	}
	for i, c := range batch.cuts {
		c.rank = i + 1
		applied[c.id] = c.units
		after := z.eval(applied)
		risk, loss := mandateRisk(after), mandateLoss(after)
		if math.IsInf(risk, 0) {
			risk = riskBefore
		}
		if math.IsInf(loss, 0) {
			loss = lossBefore
		}
		c.riskCut, c.lossCut = riskBefore-risk, lossBefore-loss
		riskBefore, lossBefore = risk, loss
	}
	batch.riskAfter, batch.lossAfter = riskBefore, lossBefore
	batch.reached = mandateTargetReached(z.eval(nil), batch.target)
	return batch
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mandateKeepStanding damps the batch against small moves of the risk: a
// line whose position has not moved keeps the last generation's cut when the
// fresh one is smaller by no more than one step (never kept larger than
// that, and never kept where the fresh batch drops the line), so a steady
// risk does not re-size, and re-revise, a row every verdict.
func mandateKeepStanding(z *mandateSizer, standing []rpc.TradeProposal) {
	for _, p := range standing {
		c, ok := z.candidates[mandateLegID(rpc.PositionView{ConID: p.Contract.ConID})]
		if !ok || p.PositionQuantity != c.row.Quantity {
			continue
		}
		c.prevUnits, c.prevQty = p.Mandate.Units, p.Quantity
		if c.units > 0 && c.units < p.Mandate.Units && p.Mandate.Units <= min(c.units+c.step, c.full) {
			c.units = p.Mandate.Units
		}
	}
}

// reset starts a sizing pass from the expiry gate's cuts alone.
func (z *mandateSizer) reset() {
	z.cuts, z.leftOut = nil, nil
	for _, c := range z.candidates {
		c.units, c.phase, c.gapLoss, c.gapCap, c.sessions = 0, "", nil, nil, nil
	}
	ids := make([]string, 0, len(z.expiry))
	for id := range z.expiry {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := z.candidates[id]
		s := z.sessions[id]
		c.units, c.phase, c.sessions = z.expiry[id], rpc.MandatePhaseExpiry, &s
		z.cuts = append(z.cuts, c)
	}
}

// sizeMain cuts gap breaches first, then the largest premium at risk left,
// name by name and within a name its largest leg, until the loss measure
// reaches the target or nothing reduces further (spec TM-04.12, owner
// decision 2026-10-27 19:50 CET: the reduction is ordered by premium at risk
// per name until the book fits the maximum loss). Inside the target only
// the owner sleeve's stop can be left, cut in its own sleeve alone by the
// sleeve's volatility, as before.
func (z *mandateSizer) sizeMain() {
	skipped := map[string]bool{}
	exhausted := map[string]bool{}
	for range mandateReductionMaxCuts {
		v := z.eval(nil)
		if math.IsInf(mandateRisk(v), 0) || math.IsInf(mandateLoss(v), 0) {
			return
		}
		if name := mandateFirstBreach(v, skipped); name != "" {
			if !z.gapStep(v, name) {
				skipped[name] = true
				z.leftOut = append(z.leftOut, name+" (its gap loss is over the cap, but no held leg of it can be reduced to bring it under)")
			}
			continue
		}
		// The book at its target: only the owner sleeve's stop can be left,
		// and it is cut in its own sleeve alone, nothing of the machine's.
		stopPhase := mandateLoss(v) <= z.target+mandateReductionEpsilon
		if stopPhase && !mandateStopOver(v) {
			return
		}
		var top *mandateCut
		if stopPhase {
			contribution := map[string]float64{}
			for _, rep := range v.Legs {
				contribution[rep.ID] = rep.ContributionPct
			}
			for _, id := range z.ids {
				c := z.candidates[id]
				if z.in.Allocations.SleeveOf(c.id) != v.OwnerStop.Sleeve || c.units >= c.full || exhausted[c.id] || contribution[c.id] <= mandateReductionEpsilon {
					continue
				}
				if top == nil || contribution[c.id] > contribution[top.id] {
					top = c
				}
			}
			if top == nil {
				return
			}
			// The stop's cut: the smallest step that takes the sleeve's use to
			// its stop target, half its risk at the stop (the owner's words
			// of 2026-10-18 21:41 CEST: "reduce-only to half the sleeve's
			// risk").
			units, ok := z.move(top, func(over map[string]int) float64 { return mandateSleeveUseOf(z.eval(over), v.OwnerStop.Sleeve) }, v.OwnerStop.TargetUseBase)
			if !ok {
				exhausted[top.id] = true
				continue
			}
			z.commit(top, units, rpc.MandatePhaseStop)
			continue
		}
		top = z.largestPremium(v, exhausted)
		if top == nil {
			return
		}
		units, ok := z.move(top, func(over map[string]int) float64 { return mandateLoss(z.eval(over)) }, z.target)
		if !ok {
			exhausted[top.id] = true
			continue
		}
		z.commit(top, units, rpc.MandatePhasePremium)
	}
}

// largestPremium picks the next premium cut: the name with the most of the
// loss measure left among the legs still reducible, and within it the leg
// with the most. Nil when no reducible leg carries any.
func (z *mandateSizer) largestPremium(v mandate.Verdict, exhausted map[string]bool) *mandateCut {
	legLoss := map[string]float64{}
	for _, rep := range v.Legs {
		legLoss[rep.ID] = rep.LossBase
	}
	nameLoss := map[string]float64{}
	for _, id := range z.ids {
		c := z.candidates[id]
		if c.units >= c.full || exhausted[c.id] || legLoss[c.id] <= mandateReductionEpsilon {
			continue
		}
		nameLoss[c.leg.Underlying] += legLoss[c.id]
	}
	var top *mandateCut
	for _, id := range z.ids {
		c := z.candidates[id]
		if c.units >= c.full || exhausted[c.id] || legLoss[c.id] <= mandateReductionEpsilon {
			continue
		}
		if top == nil || nameLoss[c.leg.Underlying] > nameLoss[top.leg.Underlying] ||
			nameLoss[c.leg.Underlying] == nameLoss[top.leg.Underlying] && legLoss[c.id] > legLoss[top.id] {
			top = c
		}
	}
	if top != nil {
		top.loss = legLoss[top.id]
	}
	return top
}

// mandateSleeveUseOf is one sleeve's use on a verdict: its allocation's use,
// or, with every leg in one sleeve, the book's risk; +Inf unmeasured.
func mandateSleeveUseOf(v mandate.Verdict, sleeve string) float64 {
	if risk := mandateRisk(v); math.IsInf(risk, 0) {
		return risk
	}
	if a, ok := v.Allocation(sleeve); ok {
		return a.UseBase
	}
	return v.Risk.SDAnnualBase
}

// gapStep cuts the largest gap loser of one breached name, the losers tried
// in order until one lowers the name's gap loss; false when none can. A gap
// cut never leaves the book riskier than before it.
func (z *mandateSizer) gapStep(v mandate.Verdict, name string) bool {
	loss, capBase := mandateGapLoss(v, name)
	var report mandate.UnderlyingReport
	for _, u := range v.Underlyings {
		if u.Underlying == name {
			report = u
		}
	}
	up := report.GapUpLossBase != nil && *report.GapUpLossBase > report.GapDownLossBase
	// The gap the name was stressed by: a broad index takes the index gap.
	shock := report.GapShockPct
	if shock <= 0 && v.Stresses != nil {
		shock = v.Stresses.GapShockPct
	}
	riskNow := mandateRisk(v)
	var losers []*mandateCut
	losses := map[string]float64{}
	for _, id := range z.ids {
		c := z.candidates[id]
		if c.units >= c.full || c.leg.Underlying != name {
			continue
		}
		if l := mandateLegGapLoss(z.in, c.leg, math.Abs(c.leg.Quantity)-float64(c.units), shock, up); l > mandateReductionEpsilon {
			losers = append(losers, c)
			losses[c.id] = l
		}
	}
	slices.SortStableFunc(losers, func(a, b *mandateCut) int { return cmp.Compare(losses[b.id], losses[a.id]) })
	for _, c := range losers {
		units, ok := z.move(c, func(over map[string]int) float64 {
			after := z.eval(over)
			if mandateRisk(after) > riskNow+mandateReductionEpsilon {
				return math.Inf(1)
			}
			l, _ := mandateGapLoss(after, name)
			return l
		}, capBase)
		if !ok {
			continue
		}
		z.commit(c, units, rpc.MandatePhaseGap)
		c.gapLoss, c.gapCap = new(loss), new(capBase)
		return true
	}
	return false
}

// expiryGate checks the planned book against the exercise of every
// in-the-money long option within three sessions of expiry: where the
// delivered shares alone would push the planned book over budget, adding
// risk, the option is cut, ahead of the batch, by the fewest contracts that
// keep the exercised book inside the target, or closed. It reports whether
// any expiry cut grew, so the batch is sized again around it.
func (z *mandateSizer) expiryGate(now time.Time) bool {
	held := z.eval(nil)
	if !mandateTargetReached(held, z.target) {
		return false // over anyway: the exercise is not what pushes it
	}
	grew := false
	for _, id := range z.ids {
		c := z.candidates[id]
		if c.units >= c.full || c.leg.Kind != mandate.KindOption || c.leg.Quantity <= 0 || !mandateInTheMoney(c.leg) {
			continue
		}
		sessions, ok := mandateSessionsToExpiry(c.row.Expiry, now)
		if !ok || sessions > mandateReductionExpirySessions {
			continue
		}
		exercised := func(units int) mandate.Verdict {
			return mandate.Evaluate(mandateExercise(z.book(map[string]int{c.id: units}), c.leg, c.full-units))
		}
		ex := exercised(c.units)
		if math.IsInf(mandateRisk(ex), 0) || !mandateExerciseOverBudget(ex, z.volBudget) || !mandateExerciseAddsRisk(ex, held) {
			continue
		}
		units := c.full
		steps := mandateCutSteps(c, c.units)
		if first := mandateFirst(steps, func(u int) bool { return mandateExerciseFits(exercised(u), z.volBudget) }); first >= 0 {
			units = steps[first]
		}
		if units > z.expiry[id] {
			z.expiry[id], z.sessions[id], grew = units, sessions, true
		}
	}
	return grew
}

// mandateExerciseOverBudget reports an exercised book whose volatility is
// over the volatility budget or that breaches a gap cap: the delivered
// shares carry no premium, so the exercise is judged by the sizing input
// the loss measure does not see.
func mandateExerciseOverBudget(v mandate.Verdict, volBudget float64) bool {
	return mandateRisk(v) > volBudget+mandateReductionEpsilon || v.Stresses != nil && len(v.Stresses.GapBreaches) > 0
}

// mandateExerciseFits reports an exercised book with its volatility at or
// under the sizing target and no gap breach.
func mandateExerciseFits(v mandate.Verdict, volBudget float64) bool {
	return mandateRisk(v) <= mandateReductionTargetFactor*volBudget+mandateReductionEpsilon && (v.Stresses == nil || len(v.Stresses.GapBreaches) == 0)
}

// mandateRisk is a verdict's annual risk; +Inf when it was not measured.
func mandateRisk(v mandate.Verdict) float64 {
	if v.Status == mandate.StatusUnknown || v.Risk == nil {
		return math.Inf(1)
	}
	return v.Risk.SDAnnualBase
}

// mandateLoss is a verdict's loss measure; +Inf when it was not measured.
func mandateLoss(v mandate.Verdict) float64 {
	if v.Status == mandate.StatusUnknown || v.Loss == nil {
		return math.Inf(1)
	}
	return v.Loss.TotalBase
}

// mandateTargetReached reports a verdict with the loss measure at or under
// the target, no gap breach and the owner sleeve's stop, when engaged, with
// nothing left to cut: the book the next verdict reads within and sizes
// nothing on.
func mandateTargetReached(v mandate.Verdict, target float64) bool {
	return mandateLoss(v) <= target+mandateReductionEpsilon && (v.Stresses == nil || len(v.Stresses.GapBreaches) == 0) && !mandateStopOver(v)
}

// mandateGapLoss is one name's gap loss on a verdict; zero when it holds
// nothing of the name any more.
func mandateGapLoss(v mandate.Verdict, name string) (loss, capBase float64) {
	for _, u := range v.Underlyings {
		if u.Underlying == name {
			return u.GapLossBase, u.GapCapBase
		}
	}
	if v.Stresses != nil {
		capBase = v.Stresses.GapCapBase
	}
	return 0, capBase
}

// mandateFirstBreach names the first gap breach not given up on.
func mandateFirstBreach(v mandate.Verdict, skipped map[string]bool) string {
	if v.Stresses == nil {
		return ""
	}
	for _, name := range v.Stresses.GapBreaches {
		if !skipped[name] {
			return name
		}
	}
	return ""
}

// mandateCutSteps lists the cuts a leg may take above from: each lot on its
// grid, up to the whole line.
func mandateCutSteps(c *mandateCut, from int) []int {
	var out []int
	for k := from/c.step + 1; ; k++ {
		u := min(k*c.step, c.full)
		if u > from {
			out = append(out, u)
		}
		if u >= c.full {
			return out
		}
	}
}

// mandateFirst is the index of the first cut for which ok holds, ok being
// false then true along cuts; -1 when it holds for none.
func mandateFirst(cuts []int, ok func(int) bool) int {
	lo, hi := 0, len(cuts)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if ok(cuts[mid]) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo == len(cuts) {
		return -1
	}
	return lo
}

// mandateArgMin is the index of the smallest value of f along cuts, the
// first on a tie. f is convex along one leg's cuts (risk is a norm of P&L
// linear in the cut, a gap loss the larger of two linear losses), so the
// first cut after which f stops falling is the minimum.
func mandateArgMin(cuts []int, f func(int) float64) int {
	lo, hi := 0, len(cuts)-1
	for lo < hi {
		mid := lo + (hi-lo)/2
		if f(cuts[mid+1]) >= f(cuts[mid])-mandateReductionEpsilon {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// mandateCutTo sizes one leg's cut above from against a convex f: the
// smallest step that brings f to limit or under, or, when no step does, the
// step at which f is least. improved is false when no step lowers f at all.
func mandateCutTo(c *mandateCut, from int, f func(int) float64, limit float64) (units int, improved bool) {
	cuts := append([]int{from}, mandateCutSteps(c, from)...)
	memo := map[int]float64{}
	g := func(u int) float64 {
		if v, ok := memo[u]; ok {
			return v
		}
		v := f(u)
		memo[u] = v
		return v
	}
	best := mandateArgMin(cuts, g)
	if best == 0 || g(cuts[best]) >= g(from)-mandateReductionEpsilon {
		return from, false
	}
	if g(cuts[best]) <= limit+mandateReductionEpsilon {
		first := mandateFirst(cuts[1:best+1], func(u int) bool { return g(u) <= limit+mandateReductionEpsilon })
		return cuts[1+first], true
	}
	return cuts[best], true
}

// mandateExerciseAddsRisk reports an exercise that leaves the book riskier
// than holding the option: more annual risk, or a gap breach the held book
// does not have.
func mandateExerciseAddsRisk(exercised, held mandate.Verdict) bool {
	if mandateRisk(exercised) > mandateRisk(held)+mandateReductionEpsilon {
		return true
	}
	if exercised.Stresses == nil {
		return false
	}
	for _, name := range exercised.Stresses.GapBreaches {
		if held.Stresses == nil || !slices.Contains(held.Stresses.GapBreaches, name) {
			return true
		}
	}
	return false
}

// mandateInTheMoney reports a long option that would be exercised: a call
// with the spot above the strike, a put below it.
func mandateInTheMoney(l mandate.Leg) bool {
	switch l.Right {
	case "C":
		return l.Spot > l.Strike
	case "P":
		return l.Spot < l.Strike
	}
	return false
}

// mandateExercise returns the book with keep contracts of the option leg
// exercised: the leg itself gone and the delivered shares in its place, long
// for a call and short for a put, at the spot.
func mandateExercise(book mandate.Input, leg mandate.Leg, keep int) mandate.Input {
	out := book
	out.Legs = slices.Clone(book.Legs)
	for i := range out.Legs {
		if out.Legs[i].ID == leg.ID {
			out.Legs[i].Quantity = 0
		}
	}
	shares := float64(keep) * leg.Multiplier
	if leg.Right == "P" {
		shares = -shares
	}
	out.Legs = append(out.Legs, mandate.Leg{ID: "exercise:" + leg.ID, Symbol: leg.Underlying, Underlying: leg.Underlying, Kind: mandate.KindStock,
		Quantity: shares, Multiplier: 1, Currency: leg.Currency, Spot: leg.Spot})
	return out
}

// mandateLegGapLoss is the loss, in base currency, of qty units of one leg
// in the one-name gap (down, or up for the breach that loses on the upside):
// positive when the leg loses, so a hedge never counts as a loser.
func mandateLegGapLoss(in mandate.Input, leg mandate.Leg, qty, gapShockPct float64, up bool) float64 {
	factor := 1 - gapShockPct/100
	if up {
		factor = 1 + gapShockPct/100
	}
	if leg.Quantity < 0 {
		qty = -qty
	}
	value := func(spot float64) float64 {
		if leg.Kind == mandate.KindOption {
			if leg.IV == nil {
				return 0
			}
			return qty * leg.Multiplier * mandate.OptionPrice(leg.Right == "C", spot, leg.Strike, leg.TimeToExpiryYears, *leg.IV)
		}
		return qty * leg.Multiplier * spot
	}
	fx := 1.0
	if ccy := strings.ToUpper(leg.Currency); ccy != "" && ccy != strings.ToUpper(in.BaseCurrency) {
		fx = in.FXNow[ccy]
	}
	return (value(leg.Spot) - value(leg.Spot*factor)) * fx
}

// mandateSessionsToExpiry counts the US option sessions after today up to
// and including the expiry day; zero on the expiry day itself. False when
// the expiry cannot be read.
func mandateSessionsToExpiry(expiryYMD string, now time.Time) (int, bool) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	day, err := time.ParseInLocation("20060102", strings.TrimSpace(expiryYMD), ny)
	if err != nil {
		return 0, false
	}
	local := now.In(ny)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, ny)
	if !day.After(today) {
		return 0, true
	}
	if day.Sub(today) > 14*24*time.Hour {
		return 99, true // far beyond the gate; the calendar is not consulted
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	sessions := 0
	for d := today.AddDate(0, 0, 1); !d.After(day); d = d.AddDate(0, 0, 1) {
		noon := time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, ny)
		s, err := cal.SessionAt(marketcal.MarketUSOptions, noon)
		if err != nil || s.State == marketcal.StateUnknown {
			// No calendar: a weekday counts.
			if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
				sessions++
			}
			continue
		}
		if s.State == marketcal.StateRegular || s.State == marketcal.StateEarlyClose {
			sessions++
		}
	}
	return sessions, true
}

// mandateOrderLimit is the most units one order may carry under the caps
// in force at the row's mark: max_order_notional as the issuer trim and the
// governor apply it, and the [order_limits] cap in base currency when the
// row's rate is known. Never below one, as the governor's: one unit above a
// cap is the order gate's to judge, which admits it only as a delta-reducing
// exit. Zero when nothing bounds it.
func mandateOrderLimit(row rpc.PositionView, base string, maxOrderNotional, capBase float64) int {
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark <= 0 || maxOrderNotional <= 0 && capBase <= 0 {
		return 0
	}
	unit := mark * float64(max(row.Multiplier, 1))
	limit := 0
	if maxOrderNotional > 0 {
		limit = int(math.Floor(maxOrderNotional / unit))
	}
	if capBase > 0 {
		if rate, ok := positionBaseRate(row, base); ok && rate > 0 {
			// A positive cap may admit zero whole units. Keep it binding;
			// only an absent cap yields to the other limit before the
			// existing one-unit exit exception is applied below.
			if byCap := int(math.Floor(capBase / (unit * rate))); maxOrderNotional <= 0 || byCap < limit {
				limit = byCap
			}
		}
	}
	return max(1, limit)
}

// mandateCycleQuantity is this cycle's order for a planned cut: no more
// shares than are free of cover with the short options as they are held now
// (a buyback goes first and must fill before the shares it frees are sold),
// held to the order caps; a kept cut keeps its order while the caps still
// admit it, so a moving mark does not re-size it. waits says why the order
// is below the cut, and names the order as it is kept, not the cap of the
// cycle that set it (2026-10-19: a kept order under a cap that no longer
// binds read "the batch asks for 150; " with nothing after, and one kept at
// 110 under a cap now at 111 named the 111).
func mandateCycleQuantity(c *mandateCut, in mandateReductionInput, base string) (int, string) {
	qty := c.units
	limit := mandateOrderLimit(c.row, base, in.MaxOrderNotional, in.OrderCapBase)
	capped := limit > 0 && qty > limit
	if capped {
		qty = limit
	}
	kept := c.units == c.prevUnits && c.prevQty > 0 && c.prevQty < qty
	if kept {
		qty = c.prevQty
	}
	qty = max(0, min(qty, c.full))
	waits := ""
	switch {
	case qty >= c.units:
	case kept && capped:
		waits = fmt.Sprintf("the order caps hold one order to %d; this order is kept at %d, the size the caps admitted last cycle, so a moving mark does not re-size it, and the next cycle measures the rest", limit, qty)
	case kept:
		waits = fmt.Sprintf("this order is kept at %d, the size the order caps admitted last cycle, so a moving mark does not re-size it; the next cycle measures the rest", qty)
	case capped:
		waits = fmt.Sprintf("the order caps hold one order to %d, and the next cycle measures the rest", limit)
	}
	return qty, waits
}

// mandateReductionRow builds one row of the batch: a reduce-only order of
// qty on the leg this cycle, with the arithmetic and the verdict it was
// sized on. Every row is NeverSkipVeto.
func (e *proposalEngine) mandateReductionRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, in mandateReductionInput, st *rpc.TradeProposalMandateStatus, batch mandateBatch, c *mandateCut, qty int, waits, ccy string) rpc.TradeProposal {
	row := c.row
	action := mandateAction(row)
	effect := rpc.OrderPositionEffectReduce
	if qty == c.full && c.full == int(math.Ceil(math.Abs(row.Quantity))) {
		effect = rpc.OrderPositionEffectClose
	}
	unit := "shares"
	if c.leg.Kind == mandate.KindOption {
		unit = pluralNoun(c.full, "contract")
	}
	verb := "sell"
	if action == rpc.OrderActionBuy {
		verb = "buy back"
	}
	v := in.Result.Verdict
	risk := fmt.Sprintf("the loss measure %s is %s of the mandate's loss room %s (%s)", mandateMoney(batch.lossBefore, ccy), mandatePctOfRoom(v), mandateMoney(batch.budget, ccy), v.Status)
	order := fmt.Sprintf("%s %d of %d %s", verb, qty, c.full, unit)
	if qty < c.units {
		order += fmt.Sprintf(" now (the full plan asks for %d; %s)", c.units, waits)
	}
	var why string
	switch {
	case c.phase == rpc.MandatePhaseExpiry:
		sessions := 0
		if c.sessions != nil {
			sessions = *c.sessions
		}
		why = fmt.Sprintf("in the money %d %s before expiry; exercised, the delivered shares would push the reduced book's volatility over its budget, so it goes first", sessions, pluralNoun(sessions, "session"))
	case c.phase == rpc.MandatePhaseGap:
		loss, capBase := 0.0, 0.0
		if c.gapLoss != nil {
			loss = *c.gapLoss
		}
		if c.gapCap != nil {
			capBase = *c.gapCap
		}
		why = fmt.Sprintf("a gap in %s would lose %s, over the cap %s", c.leg.Underlying, mandateMoney(loss, ccy), mandateMoney(capBase, ccy))
	case c.phase == rpc.MandatePhaseStop && v.OwnerStop != nil:
		why = fmt.Sprintf("%s: the largest risk contributor left in %s, cut to half the sleeve's risk at the stop (%s a year) (%.0f%% of the risk)",
			mandateOwnerStopSentence(v.OwnerStop, ccy), mandateSleeveName(c.sleeve), mandateMoney(v.OwnerStop.TargetUseBase, ccy), c.contribution)
	default:
		why = fmt.Sprintf("the largest premium at risk left, %s in %s (%s of the loss measure)", c.leg.Underlying, mandateSleeveName(c.sleeve), mandateMoney(c.loss, ccy))
	}
	reason := fmt.Sprintf("%s; %s: %s; the full plan projects the loss measure to %s if all planned units fill", risk, why, order, mandateMoney(batch.lossAfter, ccy))
	if batch.reached {
		reason += fmt.Sprintf(", inside the %s target", mandateMoney(batch.target, ccy))
	} else {
		reason += fmt.Sprintf(", short of the %s target", mandateMoney(batch.target, ccy))
	}
	details := []string{
		fmt.Sprintf("loss measure %s · loss room %s · target %s (%.0f%% of the room, the full plan's sizing objective)", mandateMoney(batch.lossBefore, ccy), mandateMoney(batch.budget, ccy), mandateMoney(batch.target, ccy), mandateReductionTargetFactor*100),
		fmt.Sprintf("planned cut %d of %d, acted on in this order; if all %d planned %s fill, this cut takes %s off the loss measure and %s a year off the volatility", c.rank, len(batch.cuts), c.units, unit, mandateMoney(c.lossCut, ccy), mandateMoney(c.riskCut, ccy)),
		fmt.Sprintf("volatility %s a year against the sizing budget %s, the 1-day swing beside the loss measure", mandateMoney(batch.riskBefore, ccy), mandateMoney(v.Capital.BudgetAnnualSD, ccy)),
	}
	if c.phase != rpc.MandatePhasePremium && c.phase != rpc.MandatePhaseStop {
		details = append(details, fmt.Sprintf("%.0f%% of the risk on the verdict", c.contribution))
	}
	if stop := v.OwnerStop; stop != nil && stop.Engaged {
		details = append(details, fmt.Sprintf("owner sleeve stop: %.0f%% below its high-water mark %s, stop %g%%, releases under %g%%; engaged since %s, reduce-only to %s a year, half its risk %s at the stop, the target pinned until the release",
			stop.DrawdownPct, mandateMoney(stop.HWMRiskBase, ccy), stop.StopPct, stop.ReleasePct, mandateClock(stop.EngagedAt), mandateMoney(stop.TargetUseBase, ccy), mandateMoney(stop.UseAtStopBase, ccy)))
	}
	details = append(details, fmt.Sprintf("sized on the %s verdict of %s (%s), afresh at every verdict", v.Status, mandateClock(v.AsOf), st.VerdictFingerprint))
	if qty < c.units {
		details = append(details, fmt.Sprintf("current order: %d of %d planned %s; %s; after it fills, a fresh verdict measures the remaining cut from the resulting holding", qty, c.units, unit, waits))
	}
	if effect == rpc.OrderPositionEffectClose {
		details = append(details, "full close: the target leaves nothing of this line to keep")
	}
	if !batch.reached {
		note := "even if the full plan fills, its projection does not reach the target; there is no further eligible cut on this verdict"
		if len(batch.leftOut) > 0 {
			note += " (left out: " + strings.Join(batch.leftOut, "; ") + ")"
		}
		details = append(details, note)
	}
	details = append(details, "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop; re-sized within this breach, it keeps its veto clock")

	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketMandateReduction, row, action, qty, effect, reason)
	p.Details = details
	p.Score = c.lossCut
	if p.Score <= 0 {
		p.Score = c.riskCut
	}
	p.NeverSkipVeto = true
	p.Mandate = &rpc.TradeProposalMandate{
		Rank: c.rank, BatchRows: len(batch.cuts), Phase: c.phase, ContributionPct: c.contribution, Units: c.units,
		GapLossBase: cloneFloat64Ptr(c.gapLoss), GapCapBase: cloneFloat64Ptr(c.gapCap),
		LossBase: batch.lossBefore, LossRoomBase: batch.budget, LossTargetBase: batch.target,
		LegLossBase: c.loss, LossCutBase: c.lossCut, LossAfterBase: batch.lossAfter,
		RiskAnnualBase: batch.riskBefore, BudgetAnnualSD: v.Capital.BudgetAnnualSD,
		RiskCutAnnualBase: c.riskCut, RiskAfterAnnualBase: batch.riskAfter,
		VerdictStatus: v.Status, VerdictAt: v.AsOf, VerdictFingerprint: st.VerdictFingerprint, Episode: st.Episode, BaseCurrency: ccy,
		Lot: c.step, Session: mandateSessionDate(v.AsOf), Sleeve: c.sleeve,
	}
	if c.sessions != nil {
		s := *c.sessions
		p.Mandate.SessionsToExpiry = &s
	}
	return p
}

// mandateHeldBlockers is the preview and submit refusal of a held row: the
// verdict it was sized on cannot be confirmed right now, so nobody sends it,
// the daemon or a hand. The row itself stays unblocked, so its automatic
// record and veto window stand; the scheduler holds it (submitDueAutomatic).
func mandateHeldBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	if p.Mandate == nil || !p.Mandate.Held {
		return nil
	}
	return []rpc.TradingBlocker{{Code: mandateSizingHeldCode,
		Message: "the mandate verdict this row was sized on cannot be confirmed right now: " + p.Mandate.HoldReason,
		Action:  "Wait for a fresh verdict (`canary mandate --refresh`); the row is sent, or withdrawn, once one reads."}}
}

// mandateHeldRefusal reports a submission refused only because its mandate
// row is held: the record keeps waiting instead of failing.
func mandateHeldRefusal(blockers []rpc.TradingBlocker) bool {
	return len(blockers) > 0 && !slices.ContainsFunc(blockers, func(b rpc.TradingBlocker) bool { return b.Code != mandateSizingHeldCode })
}

// prioritizeMandateRecords orders the mandate batch's records among their
// slots by the rows' rank, so one cycle dispatches them in batch order.
func prioritizeMandateRecords(records []automaticSubmissionRecord, proposals []rpc.TradeProposal) {
	ranks := map[string]int{}
	for _, p := range proposals {
		if p.Bucket == rpc.TradeProposalBucketMandateReduction && p.Mandate != nil && p.Mandate.Rank > 0 {
			ranks[automaticRecordKey(p.Key, p.Revision)] = p.Mandate.Rank
		}
	}
	var slots []int
	var batch []automaticSubmissionRecord
	for i, rec := range records {
		if rec.Bucket == preAuthorisedBucketMandateReduction && ranks[automaticRecordKey(rec.Key, rec.Revision)] > 0 {
			slots, batch = append(slots, i), append(batch, rec)
		}
	}
	slices.SortStableFunc(batch, func(a, b automaticSubmissionRecord) int {
		return cmp.Compare(ranks[automaticRecordKey(a.Key, a.Revision)], ranks[automaticRecordKey(b.Key, b.Revision)])
	})
	for i, slot := range slots {
		records[slot] = batch[i]
	}
}

// mandateReductionView is the forced reduction in brief for the mandate
// surface: the served batch, how many rows the daemon will place and until
// when the veto runs.
func (s *Server) mandateReductionView() *rpc.MandateReductionView {
	if s == nil || s.tradeProposals == nil {
		return nil
	}
	snap := s.tradeProposals.snapshotForRead(false, false)
	st := snap.MandateReduction
	if st == nil {
		return nil
	}
	view := &rpc.MandateReductionView{State: st.State, Reason: st.Reason, Rows: st.Rows, Withdrawn: st.Withdrawn, Held: st.Held, HoldReason: st.HoldReason}
	if policy, ok := s.tradeProposals.automaticPolicy(); ok {
		view.PreAuthorised = policy.preAuthorised(preAuthorisedBucketMandateReduction)
	}
	for _, p := range snap.Proposals {
		if p.Bucket != rpc.TradeProposalBucketMandateReduction || p.Mandate == nil {
			continue
		}
		order := rpc.MandateReductionOrder{Rank: p.Mandate.Rank, Key: p.Key, Contract: budgetDisplayContract(rpc.PositionView{ConID: p.Contract.ConID, Symbol: p.Contract.Symbol,
			SecType: p.Contract.SecType, Expiry: p.Contract.Expiry, Strike: p.Contract.Strike, Right: p.Contract.Right}),
			Action: p.Action, Quantity: p.Quantity, Phase: p.Mandate.Phase, ContributionPct: p.Mandate.ContributionPct, Held: p.Mandate.Held}
		if len(p.Blockers) > 0 {
			order.Blocked = p.Blockers[0].Code
		}
		if a := p.Automatic; a != nil {
			order.Automatic = a.State
			order.NoticeID = a.NoticeID
			switch a.State {
			case rpc.TradeProposalAutomaticPending, rpc.TradeProposalAutomaticDeferred:
				order.SubmitAt = a.SubmitAt
				view.Pending++
				if !a.SubmitAt.IsZero() && (view.VetoUntil.IsZero() || a.SubmitAt.Before(view.VetoUntil)) {
					view.VetoUntil = a.SubmitAt
				}
			case rpc.TradeProposalAutomaticSubmitting:
				// The elapsed window remains visible on this row, but an
				// in-flight order can no longer be vetoed or announced as pending.
				order.SubmitAt = a.SubmitAt
			}
		}
		view.Orders = append(view.Orders, order)
	}
	slices.SortStableFunc(view.Orders, func(a, b rpc.MandateReductionOrder) int { return cmp.Compare(a.Rank, b.Rank) })
	return view
}
