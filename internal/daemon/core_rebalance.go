package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The core sleeve's monthly rows (internal-docs/design/core-sleeve.md, phase
// 2, option A; owner decision 2026-10-14: the owner confirms each row on a
// device first, automatic submission comes later). While [mandate.core] is
// live in the strategy register, each month's decision becomes at most one
// core_rebalance proposal row: the rule's buy, sell or sale to zero of the
// core's contract, sized by core_target.go (the frozen target lowered to
// today's capital, the band, the minimum and the mandate's headroom), a
// plain DAY limit at the live mid (D2), served for the first sessions after
// the decision that the US also trades. The row goes through the
// prepared-proposal path and every gate like any other proposal: the owner
// confirms it on a device, Canary re-checks it at preview, prepare and
// submit, and the daemon never sends it by itself. In shadow the same
// decision reaches only the shadow record, with the same order terms from
// the one builder here (coreRowTerms).
//
// Every other live register entry ([mandate.strategies.<id>]) is served the
// same way by the same code (strategy-register.md, section 2; Desk
// docs/strategy-engine.md, build 2): its month's decision becomes at most
// one strategy_rebalance row on its own line, with the same terms, window,
// due time, readiness, netting and typed exception, keyed by the entry's id
// and sleeve. The input names the entry; the core's input leaves it empty.

// Blocker codes of a core row that is served but may not be sent.
const (
	coreBuyBesideReductionCode = "core_buy_beside_reduction"
	coreLimitDistanceCode      = "core_limit_far_from_decision_close"
)

// coreLimitRule says how the row's limit is priced at preview (D2).
const coreLimitRule = "the live mid, snapped to the tick grid, never crossing the mid"

// coreRowInput is what the core's rows read: the core's verdict of this
// refresh, its contract, its register state and the mandate's state. The
// engine gathers it; tests build it directly.
type coreRowInput struct {
	// Active is true while the constitution carries [mandate.core].
	Active   bool
	Result   *rpc.CoreTargetResult
	Contract *coreContract
	State    register.State
	ScalePct float64
	Scope    brokerStateScope
	// ReductionStands says a mandate_reduction row of the core's contract
	// stands this generation: no core buy beside it. (An open breach
	// episode held buys too until 2026-10-19; a sleeve inside its
	// allocation now adds while the reduction runs, as the gate admits.)
	ReductionStands bool
	// Entry is the register entry the row is for; its zero value (and the
	// core's own entry) is the core.
	Entry register.Resolved
}

// isCore reports whether the input is the core's.
func (in coreRowInput) isCore() bool {
	return in.Entry.Entry.Form == "" || in.Entry.Entry.Form == risk.StrategyFormCore
}

// bucket is the proposal bucket of the input's rows.
func (in coreRowInput) bucket() string {
	if in.isCore() {
		return rpc.TradeProposalBucketCoreRebalance
	}
	return rpc.TradeProposalBucketStrategyRebalance
}

// name is how the input's rows name their strategy.
func (in coreRowInput) name() string {
	if in.isCore() {
		return "the core"
	}
	return strategyName(in.Entry.Entry)
}

// resolveCoreInput honours the test seam, else reads the daemon's own core
// verdict; the mandate's state comes from this generation's rows.
func (e *proposalEngine) resolveCoreInput(ctx context.Context, scope brokerStateScope, mandateStatus *rpc.TradeProposalMandateStatus, rows []rpc.TradeProposal, now time.Time) coreRowInput {
	var in coreRowInput
	switch {
	case e != nil && e.coreInput != nil:
		in = e.coreInput(now)
	case e != nil && e.server != nil:
		in = e.server.coreRowInput(ctx, now)
	}
	in.Scope = scope
	if in.Contract != nil {
		in.ReductionStands = slices.ContainsFunc(rows, func(p rpc.TradeProposal) bool {
			return p.Bucket == rpc.TradeProposalBucketMandateReduction && p.Contract.ConID == in.Contract.ConID
		})
	}
	return in
}

// coreRowInput evaluates the core on the kept mandate verdict, as
// core.target does, and resolves its register entry.
func (s *Server) coreRowInput(ctx context.Context, now time.Time) coreRowInput {
	if s == nil || s.riskPolicies == nil {
		return coreRowInput{}
	}
	c := s.riskPolicies.snapshot().policy
	if c == nil || c.Mandate == nil || c.Mandate.Core == nil {
		return coreRowInput{}
	}
	in := coreRowInput{Active: true, Result: s.evaluateCoreTarget(ctx, now)}
	if reg, ok := resolvedCore(s.resolvedRegister(c)); ok {
		in.State, in.ScalePct = reg.State, reg.ScalePct
	}
	in.Contract, _, _, _ = s.corePinState(c.Mandate.Core)
	return in
}

// resolveStrategyInputs is every other register entry's input for this
// generation, by id: the entry's verdict on the kept mandate verdict, its
// line and its register state, with the mandate's state from this
// generation's rows. The engine's test seam answers for the server.
func (e *proposalEngine) resolveStrategyInputs(ctx context.Context, scope brokerStateScope, mandateStatus *rpc.TradeProposalMandateStatus, rows []rpc.TradeProposal, now time.Time) []coreRowInput {
	var ins []coreRowInput
	switch {
	case e != nil && e.strategyInputs != nil:
		ins = e.strategyInputs(now)
	case e != nil && e.server != nil:
		ins = e.server.strategyRowInputs(ctx, now)
	}
	for i := range ins {
		in := &ins[i]
		in.Scope = scope
		if in.Contract != nil {
			in.ReductionStands = slices.ContainsFunc(rows, func(p rpc.TradeProposal) bool {
				return p.Bucket == rpc.TradeProposalBucketMandateReduction && p.Contract.ConID == in.Contract.ConID
			})
		}
	}
	return ins
}

// strategyRowInputs evaluates every register entry but the core, as
// strategy.target does, each with its resolved line.
func (s *Server) strategyRowInputs(ctx context.Context, now time.Time) []coreRowInput {
	if s == nil || s.riskPolicies == nil {
		return nil
	}
	c := s.riskPolicies.snapshot().policy
	var out []coreRowInput
	for _, r := range s.resolvedRegister(c) {
		if r.Entry.Form == risk.StrategyFormCore {
			continue
		}
		in := coreRowInput{Active: true, Entry: r, State: r.State, ScalePct: r.ScalePct, Result: s.evaluateStrategyTarget(ctx, now, r)}
		if r.Instrument.ISIN != "" {
			in.Contract, _, _, _ = s.instrumentPinState(s.requestedInstrument(r))
		}
		out = append(out, in)
	}
	return out
}

// coreWindow lists the sessions a decision's row is served for: the first
// core.WindowSessions sessions on the line's calendar after the decision
// day that the US also trades (review S8). On the London line an early
// close ends before the US open and is skipped like a US holiday; on Xetra
// an early close stays a session. Ends early past the calendars' coverage.
func coreWindow(decision string, market marketcal.Market, now time.Time) []string {
	day, err := time.Parse(time.DateOnly, decision)
	if err != nil {
		return nil
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	var out []string
	for range 45 {
		day = day.AddDate(0, 0, 1)
		d := day.Format(time.DateOnly)
		line, err := cal.Query(marketcal.Query{Market: market, Date: d, Days: 1})
		if err != nil || line.Session.State == marketcal.StateUnknown {
			return out
		}
		us, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: d, Days: 1})
		if err != nil || us.Session.State == marketcal.StateUnknown {
			return out
		}
		lineOpen := line.Session.State == marketcal.StateRegular || line.Session.State == marketcal.StateEarlyClose && market != marketcal.MarketUKLSE
		usOpen := us.Session.State == marketcal.StateRegular || us.Session.State == marketcal.StateEarlyClose
		if lineOpen && usOpen {
			out = append(out, d)
			if len(out) == core.WindowSessions {
				return out
			}
		}
	}
	return out
}

// coreSessionClose is the scheduled close of a session on the line's
// calendar; false when the calendar does not cover it.
func coreSessionClose(date string, market marketcal.Market, now time.Time) (time.Time, bool) {
	cal := marketcal.NewWithClock(func() time.Time { return now })
	res, err := cal.Query(marketcal.Query{Market: market, Date: date, Days: 1})
	if err != nil || res.Session.Close.IsZero() {
		return time.Time{}, false
	}
	return res.Session.Close, true
}

// coreWindowSession is the window session the row is for at now: the first
// whose close is still ahead. ok is false once the window has ended.
func coreWindowSession(window []string, market marketcal.Market, now time.Time) (session string, ok bool) {
	for _, d := range window {
		if closeAt, known := coreSessionClose(d, market, now); known && now.Before(closeAt) {
			return d, true
		}
	}
	return "", false
}

// coreOpenSession is the session a sale to zero continues in past the
// window: today's session on the line's calendar while it has not closed,
// else the next one the US also trades.
func coreOpenSession(market marketcal.Market, now time.Time) string {
	cal := marketcal.NewWithClock(func() time.Time { return now })
	probe, err := cal.Query(marketcal.Query{Market: market, At: now, Days: 1})
	if err != nil {
		return ""
	}
	loc, err := time.LoadLocation(probe.Session.Timezone)
	if err != nil {
		return ""
	}
	day := now.In(loc).AddDate(0, 0, -1)
	for range 45 {
		day = day.AddDate(0, 0, 1)
		d := day.Format(time.DateOnly)
		line, err := cal.Query(marketcal.Query{Market: market, Date: d, Days: 1})
		if err != nil || line.Session.State == marketcal.StateUnknown {
			return ""
		}
		if line.Session.State != marketcal.StateRegular && line.Session.State != marketcal.StateEarlyClose || !now.Before(line.Session.Close) {
			continue
		}
		us, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: d, Days: 1})
		if err != nil || us.Session.State == marketcal.StateUnknown {
			return ""
		}
		if us.Session.State == marketcal.StateRegular || us.Session.State == marketcal.StateEarlyClose {
			return d
		}
	}
	return ""
}

// coreEpisode keys a month's rows: core:<account>:<mode>:<YYYY-MM>.
func coreEpisode(scope brokerStateScope, decision string) string {
	month := decision
	if len(decision) >= 7 {
		month = decision[:7]
	}
	return fmt.Sprintf("core:%s:%s:%s", strings.TrimSpace(scope.keyAccount()), scope.Mode, month)
}

// strategyEpisode keys a month's rows of the input's entry: the core's
// episode, or strategy:<id>:<account>:<mode>:<YYYY-MM> for another entry,
// so two entries' rows of one month never share an episode.
func strategyEpisode(in coreRowInput, scope brokerStateScope, decision string) string {
	if in.isCore() {
		return coreEpisode(scope, decision)
	}
	return "strategy:" + in.Entry.Entry.ID + ":" + strings.TrimPrefix(coreEpisode(scope, decision), "core:")
}

// coreRowTerms builds the order terms of a decision's row: the one builder
// the live row and the shadow intent share, so a shadow intent carries the
// terms its live row would (strategy-register.md, section 3).
func coreRowTerms(k *coreContract, decision string, scope brokerStateScope, now time.Time) rpc.CoreRowTerms {
	return strategyRowTerms(coreRowInput{}, k, decision, scope, now)
}

// strategyRowTerms is coreRowTerms for the input's entry: the same terms,
// under the entry's episode.
func strategyRowTerms(in coreRowInput, k *coreContract, decision string, scope brokerStateScope, now time.Time) rpc.CoreRowTerms {
	terms := rpc.CoreRowTerms{OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Limit: coreLimitRule, Exchange: "SMART", Episode: strategyEpisode(in, scope, decision)}
	if k != nil {
		terms.PrimaryExchange = k.PrimaryExch
		terms.Window = coreWindow(decision, k.Market, now)
	}
	return terms
}

// coreContractParams is the core's contract as its row orders it: by
// contract id, routed SMART with the line's venue as IBKR names it.
func coreContractParams(k *coreContract) rpc.ContractParams {
	return rpc.ContractParams{ConID: k.ConID, Symbol: k.Symbol, SecType: "STK", Market: coreContractMarket(k.Market), Exchange: "SMART", PrimaryExch: k.PrimaryExch,
		Currency: k.Currency, LocalSymbol: k.LocalSymbol, TradingClass: k.TradingClass, Multiplier: 1}
}

// coreContractMarket names the line's market the way quote and readiness
// code read it from a contract.
func coreContractMarket(market marketcal.Market) string {
	switch market {
	case marketcal.MarketDEXetra:
		return "de"
	case marketcal.MarketUKLSE:
		return "uk"
	default:
		return ""
	}
}

// corePlan is the row builder's decision for one generation: the action
// and why, the window, the session the row is for, and why no row is
// served when none is.
type corePlan struct {
	Kind     string
	Quantity int
	Why      string
	Window   []string
	Session  string
	// Hold says the decision holds and only the sale to zero the frozen
	// month wants is served, for review (review S6).
	Hold    bool
	Reasons []string
}

// corePlanFor applies the register, the window and the rule's action to a
// generation: a row only while the core is live, the month is decided, the
// rule wants a buy, a sell or a sale to zero, and the window stands (a sale
// to zero continues past it until sold).
func corePlanFor(in coreRowInput, now time.Time) corePlan {
	res := in.Result
	var plan corePlan
	if !in.Active || res == nil || !res.Active {
		plan.Reasons = append(plan.Reasons, "the core sleeve is inactive")
		return plan
	}
	if !in.isCore() {
		return strategyPlanGate(in, now)
	}
	if in.State != register.StateLive {
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("the core runs in %s and serves no row; its decision reaches the shadow record only", nonEmptyString(string(in.State), "no stage")))
		return plan
	}
	if in.Contract == nil {
		plan.Reasons = append(plan.Reasons, "the core's contract is not resolved")
		return plan
	}
	return corePlanDecided(in, now)
}

// strategyPlanGate is corePlanFor's register and contract test for another
// entry, in the entry's own words: a row only while it is live, never in
// another stage (a hold names its reasons, suspended serves none).
func strategyPlanGate(in coreRowInput, now time.Time) corePlan {
	var plan corePlan
	name := in.name()
	switch in.State {
	case register.StateLive:
	case register.StateHold:
		plan.Reasons = append(plan.Reasons, name+" holds and serves no row: "+strings.Join(in.Entry.Reasons, "; "))
		return plan
	default:
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("%s is %s and serves no row", name, nonEmptyString(string(in.State), "unwritten")))
		return plan
	}
	if in.Contract == nil {
		plan.Reasons = append(plan.Reasons, name+"'s contract is not resolved")
		return plan
	}
	return corePlanDecided(in, now)
}

// corePlanDecided is corePlanFor past the register: the month's action, the
// window and the session.
func corePlanDecided(in coreRowInput, now time.Time) corePlan {
	res := in.Result
	var plan corePlan
	d, a := res.Decision, res.Action
	if d == nil || d.Decision == "" || a == nil {
		plan.Reasons = append(plan.Reasons, "no month is decided")
		plan.Reasons = append(plan.Reasons, res.Reasons...)
		return plan
	}
	held := 0
	if res.Holding != nil {
		held = res.Holding.Shares
	}
	plan.Window = coreWindow(d.Decision, in.Contract.Market, now)
	switch {
	case res.Status == rpc.CoreStatusReady && (a.Kind == core.ActionBuy || a.Kind == core.ActionSell || a.Kind == core.ActionSellAll) && a.Quantity > 0:
		plan.Kind, plan.Quantity, plan.Why = a.Kind, a.Quantity, a.Why
	case res.Status == rpc.CoreStatusHold && d.Status == core.StatusOK && !d.TrendOn && !d.Drift && held > 0:
		// Review S6: a data hold stops buys, and the sale to zero the frozen
		// month wants goes to the owner for review, with the hold named.
		plan.Kind, plan.Quantity, plan.Hold = core.ActionSellAll, held, true
		plan.Why = "the trend is off and " + in.name() + " holds: the sale to zero goes to you for review (" + strings.Join(res.Reasons, "; ") + ")"
	default:
		plan.Kind, plan.Why = a.Kind, a.Why
		plan.Reasons = append(plan.Reasons, a.Why)
		if res.Status == rpc.CoreStatusHold {
			plan.Reasons = append(plan.Reasons, res.Reasons...)
		}
		return plan
	}
	session, open := coreWindowSession(plan.Window, in.Contract.Market, now)
	switch {
	case open:
		plan.Session = session
	case plan.Kind == core.ActionSellAll:
		plan.Session = coreOpenSession(in.Contract.Market, now)
		if plan.Session == "" {
			plan.Reasons = append(plan.Reasons, "no session the US also trades is within the calendar's coverage")
		}
	case len(plan.Window) == 0:
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("no window session after %s is within the calendars' coverage", d.Decision))
	default:
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("the %d-session window after %s ended with %s; what is left waits for the next decision", core.WindowSessions, d.Decision, plan.Window[len(plan.Window)-1]))
	}
	return plan
}

// coreRebalanceProposals serves the core's row for this generation, or
// none, with the core's status. Nil status while the constitution carries no
// [mandate.core]. A buy is served blocked while a reduction of the line
// stands, and any row while an order for the contract works, so nobody
// sends it; the refusal is on the row.
func (e *proposalEngine) coreRebalanceProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, in coreRowInput, sources rpc.TradeProposalSourceFingerprints, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalCoreStatus) {
	if !in.Active {
		return nil, nil
	}
	plan := corePlanFor(in, now)
	st := &rpc.TradeProposalCoreStatus{State: nonEmptyString(string(in.State), "inactive"), Kind: plan.Kind, Quantity: plan.Quantity, Why: plan.Why, Window: plan.Window, Reasons: plan.Reasons}
	if !in.isCore() {
		st.Strategy, st.Sleeve = in.Entry.Entry.ID, in.Entry.Entry.Sleeve
	}
	if in.Result != nil && in.Result.Decision != nil {
		st.Decision = in.Result.Decision.Decision
	}
	if plan.Session == "" || plan.Quantity <= 0 {
		return nil, st
	}
	p := coreRow(policy, status, sources, now, in, plan)
	if e != nil && e.isIgnored(in.Scope, p.Key) {
		st.Reasons = append(st.Reasons, "the row is ignored by you ("+p.Key+"); unignore it to see it again")
		return nil, st
	}
	if plan.Kind == core.ActionBuy && in.ReductionStands {
		// A buy beside a reduction of its own line never goes. An open
		// breach episode no longer blocks it (until 2026-10-19 it did): a
		// sleeve inside its allocation adds while the forced reduction
		// cuts the sleeve furthest over its own (owner, 2026-10-18 15:31
		// CEST), and the decision step judges the buy by its sleeve as the
		// gate does.
		what, line := "core buy", "the core's line"
		if !in.isCore() {
			what, line = in.Entry.Entry.ID+" buy", in.name()+"'s line"
		}
		proposalBlock(&p, coreBuyBesideReductionCode, "a mandate reduction of "+line+" stands this generation: no "+what+" beside it")
	}
	for _, b := range e.coreWorkingOrderBlockers(ctx, p) {
		proposalBlock(&p, b.Code, b.Message)
	}
	st.Served, st.RowKey = true, p.Key
	for _, b := range p.Blockers {
		st.Reasons = append(st.Reasons, b.Message)
	}
	return []rpc.TradeProposal{p}, st
}

// coreWorkingOrderBlockers nets the row against every order working for the
// core's contract on either side, the owner's included, and against an
// order Canary sent that the broker has not confirmed: while one works no
// core row may be sent (core-sleeve.md, "Rebalance and no-trade band").
// Missing inventory fails closed, as a reduction's netting does.
func (e *proposalEngine) coreWorkingOrderBlockers(ctx context.Context, p rpc.TradeProposal) []rpc.TradingBlocker {
	if e == nil || e.server == nil {
		return nil
	}
	var out []rpc.TradingBlocker
	for _, action := range []string{rpc.OrderActionBuy, rpc.OrderActionSell} {
		probe := p
		probe.Action = action
		for _, b := range e.reductionOrderBlockers(ctx, probe, false) {
			out = appendTradingBlockerOnce(out, b)
		}
	}
	return out
}

// coreRow builds the month's row: the order the rule wants on the confirmed
// holding, with the decision's figures and the terms, for the owner's
// review.
func coreRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, in coreRowInput, plan corePlan) rpc.TradeProposal {
	res, k := in.Result, in.Contract
	contract := coreContractParams(k)
	held, price := 0, 0.0
	if res.Holding != nil {
		held, price = res.Holding.Shares, res.Holding.Price
	}
	target := 0
	if res.Sizing != nil {
		target = res.Sizing.Target
	}
	action, effect, verb := rpc.OrderActionBuy, rpc.OrderPositionEffectIncrease, "buy"
	switch {
	case plan.Kind == core.ActionBuy && held == 0:
		effect = rpc.OrderPositionEffectOpen
	case plan.Kind == core.ActionSellAll:
		action, effect, verb = rpc.OrderActionSell, rpc.OrderPositionEffectClose, "sell every one of"
	case plan.Kind == core.ActionSell:
		action, effect, verb = rpc.OrderActionSell, rpc.OrderPositionEffectReduce, "sell"
	}
	d := res.Decision
	terms := strategyRowTerms(in, k, d.Decision, in.Scope, now)
	trend := "trend on"
	if !d.TrendOn {
		trend = "trend off"
	}
	head, confirm := "core sleeve", "you confirm this row on your device, or Desk's controller sends it under the standing mandate you confirmed there; nothing else sends it, and it is never pre-authorised"
	if !in.isCore() {
		head = fmt.Sprintf("%s strategy (sleeve %s)", in.Entry.Entry.ID, in.Entry.Entry.Sleeve)
	}
	if plan.Hold {
		// For your review means your review: the standing mandate never
		// sends a row served while the entry holds (owner, 2026-10-19).
		confirm = "this row goes to you for review: you confirm it on your device, and the standing mandate never sends it; nothing else sends it, and it is never pre-authorised"
	}
	reason := fmt.Sprintf("%s, decision %s (%s, target %s, %s held): %s %s for the %s session, a DAY limit at the live mid; %s",
		head, d.Decision, trend, pluralNoun(target, "share"), pluralNoun(held, "share"), verb, pluralNoun(plan.Quantity, "share"), plan.Session, plan.Why)
	details := []string{
		confirm,
		fmt.Sprintf("the limit is %s; the mandate gate, funding and every other gate re-check the order at preview and submit", coreLimitRule),
	}
	if len(plan.Window) > 0 {
		window := fmt.Sprintf("window %s to %s (%d %s the US also trades)", plan.Window[0], plan.Window[len(plan.Window)-1], len(plan.Window), pluralNoun(len(plan.Window), "session"))
		if plan.Kind == core.ActionSellAll {
			window += "; a sale to zero continues in the next sessions until the holding is zero"
		} else {
			window += "; what is left after it waits for the next decision"
		}
		details = append(details, window)
	}
	if res.Sizing != nil {
		on := res.Sizing.CurrentOn
		details = append(details, fmt.Sprintf("target %s: %s%% of %s × room %s ÷ %s volatility, at most %s%%, at scale %s%%; frozen %s, lowered to today's capital",
			pluralNoun(target, "share"), fmtPct(on.AllocationPct), mandateMoney(on.BudgetBase, k.Currency), fmtPct(on.Room*100), fmtPct(d.Sigma*100), fmtPct(on.MaxValuePct), fmtPct(on.ScalePct), d.FrozenAt.UTC().Format(time.DateTime)))
	}
	if res.Mandate != nil {
		details = append(details, fmt.Sprintf("sized on the %s mandate verdict of %s, afresh at every generation", res.Mandate.Status, mandateClock(res.Mandate.EvaluatedAt)))
	}
	if res.Action != nil && res.Action.HeadroomCut {
		details = append(details, fmt.Sprintf("cut to the mandate's headroom: the book at most at %.0f%% of the budget and of each gap cap, %s inside its allocation", core.HeadroomFactor*100, in.name()))
	}
	if plan.Hold {
		details = append(details, in.name()+" holds: "+strings.Join(res.Reasons, "; "))
	}
	bucket := in.bucket()
	p := rpc.TradeProposal{Key: proposalKey(bucket, contract, action), State: rpc.TradeProposalStateGenerated, Bucket: bucket,
		Symbol: contract.Symbol, SecType: "STK", Action: action, Quantity: plan.Quantity, MaxQuantity: plan.Quantity, PositionQuantity: float64(held), PositionEffect: effect,
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, Details: details,
		PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now}
	if price > 0 {
		v := price
		p.LimitPrice = &v
		p.Notional = price * float64(plan.Quantity)
	}
	p.Core = &rpc.TradeProposalCore{Decision: d.Decision, TrendOn: d.TrendOn, Kind: plan.Kind, Target: target, Held: held, ScalePct: in.ScalePct,
		ConID: k.ConID, Currency: k.Currency, DecisionClose: d.LineClose(), Session: plan.Session, Sleeve: risk.CoreStrategyID, Stage: string(in.State), Terms: terms, OwnerReview: plan.Hold}
	if !in.isCore() {
		p.Core.Sleeve, p.Core.Strategy = in.Entry.Entry.Sleeve, in.Entry.Entry.ID
		if in.Entry.Rule != nil {
			p.Core.Rule = in.Entry.Rule.ID
		}
	}
	if a := res.Action; a != nil && !plan.Hold {
		p.Core.Diff, p.Core.BandShares, p.Core.HeadroomCut = a.Diff, a.BandShares, a.HeadroomCut
	}
	if res.Mandate != nil {
		p.Core.VerdictStatus, p.Core.VerdictAt = res.Mandate.Status, res.Mandate.EvaluatedAt
	}
	return p
}

// fmtPct prints a percentage number without a trailing zero fraction.
func fmtPct(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// coreOpenException is the core's typed exception to
// authority.close_reduce_only (core-sleeve.md, "The one typed exception";
// review S4): a core_rebalance buy of the core's own contract, in its
// currency, for no more shares than the row sized and never more than the
// frozen target leaves to buy. Every other bucket, action, contract or size
// stays close-or-reduce only; a core sale is an ordinary reduction.
type coreOpenException struct {
	// Strategy is the register entry of a strategy_rebalance row; empty for
	// the core's.
	Strategy      string
	ConID         int
	Currency      string
	MaxQuantity   int
	DecisionClose float64
}

// coreOpenExceptionFor returns the exception prop qualifies for.
func coreOpenExceptionFor(prop rpc.TradeProposal) (coreOpenException, bool) {
	c := prop.Core
	none := coreOpenException{}
	switch {
	case !rpc.StrategyRowBucket(prop.Bucket) || c == nil || c.Kind != core.ActionBuy:
		return none, false
	case prop.Bucket == rpc.TradeProposalBucketStrategyRebalance && strings.TrimSpace(c.Strategy) == "":
		return none, false
	case !strings.EqualFold(strings.TrimSpace(prop.Action), rpc.OrderActionBuy):
		return none, false
	case prop.PositionEffect != rpc.OrderPositionEffectOpen && prop.PositionEffect != rpc.OrderPositionEffectIncrease:
		return none, false
	case c.ConID <= 0 || prop.Contract.ConID != c.ConID || !strings.EqualFold(strings.TrimSpace(prop.Contract.SecType), "STK") || normCcy(prop.Contract.Currency) != normCcy(c.Currency):
		return none, false
	case prop.Quantity < 1 || prop.MaxQuantity != prop.Quantity || prop.Quantity > c.Target-c.Held:
		return none, false
	}
	x := coreOpenException{ConID: c.ConID, Currency: normCcy(c.Currency), MaxQuantity: prop.Quantity, DecisionClose: c.DecisionClose}
	if prop.Bucket == rpc.TradeProposalBucketStrategyRebalance {
		x.Strategy = c.Strategy
	}
	return x, true
}

// previewBlockers says why a preview the close_reduce_only gate would refuse
// is outside the exception; none means it is inside: a regular-session DAY
// limit buy of the core's contract in its currency, opening or increasing,
// within the sized quantity, with a limit near the decision close (a limit
// further away is a probable data or contract error and holds the row).
func (x coreOpenException) previewBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if preview == nil {
		return []rpc.TradingBlocker{{Code: "proposal_preview_missing", Message: "proposal preview result is unavailable"}}
	}
	d := preview.Draft
	effect := preview.Position.Effect
	if (effect != rpc.OrderPositionEffectOpen && effect != rpc.OrderPositionEffectIncrease) || d.Quantity < 1 || d.Quantity > x.MaxQuantity ||
		d.Contract.ConID != x.ConID || normCcy(d.Contract.Currency) != x.Currency || !strings.EqualFold(strings.TrimSpace(d.Contract.SecType), "STK") ||
		!strings.EqualFold(strings.TrimSpace(d.OrderType), rpc.OrderTypeLMT) || !strings.EqualFold(strings.TrimSpace(d.TIF), rpc.OrderTIFDay) || d.OutsideRTH || !positiveFinite(d.LimitPrice) {
		return []rpc.TradingBlocker{{Code: "preview_effect_not_close_reduce",
			Message: fmt.Sprintf("preview effect %q or order terms do not match the %s row's DAY limit buy of its own contract within its sized quantity", effect, nonEmptyString(x.Strategy, "core")),
			Action:  "Refresh proposals and positions and preview the row again."}}
	}
	if x.DecisionClose > 0 {
		if distance := math.Abs(d.LimitPrice/x.DecisionClose-1) * 100; distance > core.LimitDistancePct {
			return []rpc.TradingBlocker{{Code: coreLimitDistanceCode,
				Message: fmt.Sprintf("the limit %.2f is %.1f%% from the decision close %.2f, beyond %.0f%%: a probable data or contract error, so the row holds for review", d.LimitPrice, distance, x.DecisionClose, core.LimitDistancePct),
				Action:  "Check the line's quote and its series; the next generation sizes the row again."}}
		}
	}
	return nil
}

// coreDecisionRow is the record of a month's decision while the core was
// live: what the rule decided on the holding then, the window, the row's
// key. Written once per frozen month at the first live refresh after the
// decision; the row itself is sized afresh every generation.
type coreDecisionRow struct {
	DecidedAt time.Time `json:"decided_at"`
	Stage     string    `json:"stage"`
	Kind      string    `json:"kind"`
	Quantity  int       `json:"quantity,omitempty"`
	Why       string    `json:"why"`
	Target    int       `json:"target"`
	Held      int       `json:"held"`
	Price     float64   `json:"price,omitempty"`
	Window    []string  `json:"window,omitempty"`
	Session   string    `json:"session,omitempty"`
	RowKey    string    `json:"row_key,omitempty"`
	Served    bool      `json:"served"`
}

// recordStrategyDecision keeps a live entry's month's decision with its
// frozen target the first time the month reads ready (or holds with a sale
// to zero to review): the action, its reason (a refused buy names the gate's
// reading) and the window; the core's as since phase 2, every other entry's
// beside its own frozen month.
func (s *Server) recordStrategyDecision(ctx context.Context, res *rpc.CoreTargetResult, reg register.Resolved, now time.Time) {
	if s == nil || s.riskPolicies == nil || res == nil || !res.Active || res.Decision == nil || res.Decision.Decision == "" {
		return
	}
	if reg.State != register.StateLive {
		return
	}
	contract, _, _, _ := s.instrumentPinState(s.requestedInstrument(reg))
	if contract == nil {
		return
	}
	scope := s.currentBrokerStateScope()
	frozenScope := frozenScopeFor(reg, scope, contract.ConID)
	f := s.coreFrozen(ctx, frozenScope)
	if f == nil || f.Decision != res.Decision.Decision {
		return
	}
	in := coreRowInput{Active: true, Result: res, Contract: contract, State: reg.State, ScalePct: reg.ScalePct, Scope: scope}
	if reg.Entry.Form != risk.StrategyFormCore {
		in.Entry = reg
	}
	plan := corePlanFor(in, now)
	if res.Status != rpc.CoreStatusReady && !plan.Hold {
		return // the month may still clear; nothing is recorded for a hold
	}
	row := &coreDecisionRow{DecidedAt: now, Stage: string(reg.State), Kind: plan.Kind, Quantity: plan.Quantity, Why: plan.Why, Window: plan.Window, Session: plan.Session}
	if res.Sizing != nil {
		row.Target = res.Sizing.Target
	}
	if res.Holding != nil {
		row.Held, row.Price = res.Holding.Shares, res.Holding.Price
	}
	if plan.Session != "" && plan.Quantity > 0 {
		action := rpc.OrderActionBuy
		if plan.Kind != core.ActionBuy {
			action = rpc.OrderActionSell
		}
		row.Served, row.RowKey = true, proposalKey(in.bucket(), coreContractParams(contract), action)
	}
	// The record follows the row the window actually serves: a month first
	// recorded as "no buy" (the verdict read reduce, a breach stood) is
	// re-recorded when a later refresh inside the window serves a row, so
	// `canary core` reads that row's outcome. A served row's record is
	// final once the row was submitted, and stays when the row is later
	// withdrawn or its window ends, with the journal's outcome on it. The
	// sized quantity alone, which every generation re-sizes, changes no
	// record.
	if f.Row != nil && (!coreDecisionChanged(f.Row, row) || f.Row.Served && s.coreDecisionSubmitted(f, frozenScope)) {
		return
	}
	// A copy replaces the kept month under the store's lock, so no reader
	// of the frozen month sees a record half-written.
	recorded := *f
	recorded.Row = row
	if err := s.freezeCore(ctx, &recorded); err != nil {
		s.warnf("core decision %s: recorded in memory, the database save failed: %v", f.Decision, err)
	}
}

// coreDecisionChanged reports whether a later refresh serves a row the
// record does not have: a row where none was served, or another action's
// row; the sized quantity alone does not count, and a record keeps its
// served row when a later refresh serves none.
func coreDecisionChanged(recorded, next *coreDecisionRow) bool {
	return next.Served && (!recorded.Served || recorded.Kind != next.Kind || recorded.RowKey != next.RowKey)
}

// coreDecisionSubmitted reports whether the recorded row was submitted, read
// from the proposal journal since the month was frozen: a re-recorded row
// carries the time it was re-recorded, and a send between the engine serving
// the row and that re-record (the controller sends at the first ready read,
// the record follows on the strategy cadence a minute later) must still
// count (quality loop, 2026-10-18).
func (s *Server) coreDecisionSubmitted(f *coreFrozenTarget, scope string) bool {
	e := s.tradeProposals
	if e == nil || e.store == nil || f == nil || f.Row == nil || f.Row.RowKey == "" {
		return false
	}
	r := f.Row
	for _, ev := range e.store.eventsSince(f.FrozenAt) {
		if ev.Key == r.RowKey && ev.Type == "submitted" && coreDecisionEventMatchesScope(ev, scope) {
			return true
		}
	}
	return false
}

// coreFinish attaches a live entry's row and last decision to its verdict:
// the pending row as the proposal engine serves it, and the recorded
// decision with what became of its row in the proposal journal. Suspended
// or on hold the verdict stays read-only.
func (s *Server) coreFinish(ctx context.Context, res *rpc.CoreTargetResult, contract *coreContract, reg register.Resolved) *rpc.CoreTargetResult {
	res.ReadOnly = reg.State != register.StateLive
	if res.ReadOnly || contract == nil {
		return res
	}
	bucket := rpc.TradeProposalBucketCoreRebalance
	if reg.Entry.Form != risk.StrategyFormCore {
		bucket = rpc.TradeProposalBucketStrategyRebalance
	}
	res.Row = s.strategyPendingRow(bucket, reg.Entry.ID, contract)
	res.LastDecision = s.coreLastDecision(ctx, frozenScopeFor(reg, s.currentBrokerStateScope(), contract.ConID))
	return res
}

// strategyPendingRow is an entry's row in the served snapshot: its bucket,
// its line and, for another entry, its id.
func (s *Server) strategyPendingRow(bucket, id string, contract *coreContract) *rpc.CoreRowView {
	e := s.tradeProposals
	if e == nil || contract == nil {
		return nil
	}
	snap := e.snapshotForRead(false, false)
	for _, p := range snap.Proposals {
		if p.Bucket != bucket || p.Contract.ConID != contract.ConID || p.Core == nil {
			continue
		}
		if bucket == rpc.TradeProposalBucketStrategyRebalance && p.Core.Strategy != id {
			continue
		}
		v := &rpc.CoreRowView{Key: p.Key, Revision: p.Revision, Action: p.Action, Quantity: p.Quantity, Session: p.Core.Session, State: p.State, Blockers: slices.Clone(p.Blockers), Terms: p.Core.Terms}
		if p.LimitPrice != nil {
			v.ReferencePrice = *p.LimitPrice
		}
		if p.Readiness != nil {
			v.Readiness = p.Readiness.Code
		}
		return v
	}
	return nil
}

// coreLastDecision is the recorded decision of the frozen month with its
// row's fate from the proposal journal: submitted (the owner confirmed it)
// with the order reference, or the latest refusal recorded on it.
func (s *Server) coreLastDecision(ctx context.Context, scope string) *rpc.CoreDecisionRecord {
	f := s.coreFrozen(ctx, scope)
	if f == nil || f.Row == nil {
		return nil
	}
	r := f.Row
	out := &rpc.CoreDecisionRecord{Decision: f.Decision, DecidedAt: r.DecidedAt, Stage: r.Stage, Kind: r.Kind, Quantity: r.Quantity, Why: r.Why, Target: r.Target, Held: r.Held, Price: r.Price,
		Window: slices.Clone(r.Window), RowKey: r.RowKey, Outcome: rpc.CoreDecisionOutcome{Served: r.Served}}
	// The journal is read since the month was frozen, never since the row's
	// last record, so a send between the row's serve and its re-record is
	// read (coreDecisionSubmitted).
	if e := s.tradeProposals; e != nil && e.store != nil && r.RowKey != "" {
		for _, ev := range e.store.eventsSince(f.FrozenAt) {
			if ev.Key != r.RowKey || !coreDecisionEventMatchesScope(ev, scope) {
				continue
			}
			switch ev.Type {
			case "submitted":
				out.Outcome.Submitted, out.Outcome.SubmittedAt, out.Outcome.OrderRef = true, ev.At, ev.OrderRef
			case "blocked":
				out.Outcome.Refused, out.Outcome.RefusedAt = ev.Message, ev.At
			}
		}
	}
	return out
}

func coreDecisionEventMatchesScope(ev proposalEvent, scope string) bool {
	// Another entry's frozen scope is strategy|<id>|<account>|<mode>|<con>
	// (frozenScopeFor); the broker scope is the same after its prefix.
	if rest, ok := strings.CutPrefix(scope, "strategy|"); ok {
		if _, after, found := strings.Cut(rest, "|"); found {
			scope = after
		}
	}
	parts := strings.SplitN(scope, "|", 3)
	return len(parts) == 3 && brokerScopeAccountConcrete(scopeFromKeyAccount(parts[0], parts[1]).Account) &&
		sameBrokerScope(scopeFromKeyAccount(parts[0], parts[1]), recordScope(ev.Broker, ev.AccountID, ev.AccountMode))
}
