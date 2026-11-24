package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The exit plan's half of the queued executor (owner_exit_plan.go): the
// trail watches the underlying stock inside its window and latches its
// trigger; the first sale never sends once the trail has fired; and a
// triggered trail sells only after its plan's first sale can no longer
// work beside it.

// queuedTrailLive is what this process last read for one trail. It is never
// persisted: after a restart a trail is unwatched until its first usable read.
type queuedTrailLive struct {
	// price and usableAt are the last usable stock midpoint and when it was
	// quoted.
	price    float64
	usableAt time.Time
	// code and reason say why the latest read was unusable; empty when it
	// was usable.
	code, reason string
	// breachAt is when, by the executor's clock, the first read at or below
	// the line in the current run of such reads was taken; zero when the
	// latest read was above it or unusable. The trail fires on a later read,
	// itself usable and at or below the line, at least
	// queuedTrailConfirmAfter after it. The quote's own time decides only
	// whether a read is fresh, so a stock resting under the line with an
	// unchanged quote still confirms.
	breachAt time.Time
}

func (e *proposalEngine) trailLiveFor(id string) (queuedTrailLive, bool) {
	e.trailMu.Lock()
	defer e.trailMu.Unlock()
	live, ok := e.trailLive[id]
	return live, ok
}

func (e *proposalEngine) setTrailLive(id string, live queuedTrailLive) {
	e.trailMu.Lock()
	defer e.trailMu.Unlock()
	if e.trailLive == nil {
		e.trailLive = map[string]queuedTrailLive{}
	}
	e.trailLive[id] = live
}

// trailSession is where a trail stands against the regular sessions of its
// option and its stock. in reports that it may watch or sell now; open is
// the session's open, its identity; next is when it watches again (zero when
// unknown or past its validity); code and reason say why it does not now.
type trailSession struct {
	in           bool
	open, next   time.Time
	code, reason string
}

// trailSessionAt reads both markets' official sessions at now. A trail
// watches and sells only while both are open: on the plan's first session
// from not_before, on a later one (version 4) from the later of the two
// opens plus the signed resume wait, so the opening minutes' wide option
// spreads are past; and it never watches between sessions, so a pre-market
// print never latches it. When the calendar cannot say, a version-3 trail
// keeps its one-session window, as before; a carry-over trail does not watch.
func (e *proposalEngine) trailSessionAt(rec queuedAuthRecord, now time.Time) trailSession {
	t := rec.Terms
	s := e.server
	carry := t.Version == queuedPlanSessionsTermsVersion
	if s == nil || t.OwnerOrder == nil || t.OwnerOrder.StockTrail == nil {
		return trailSession{in: !carry}
	}
	stockMarket, known := quoteSessionMarketForContract(t.OwnerOrder.StockTrail.Underlying)
	opt, ok := s.previewSession(marketcal.Market(t.Market), now)
	stk, sok := s.previewSession(stockMarket, now)
	if !known || !ok || !sok || opt.State == marketcal.StateUnknown || stk.State == marketcal.StateUnknown {
		if !carry {
			return trailSession{in: true}
		}
		return trailSession{code: "queue_session_unknown", reason: "the trading calendar cannot say whether the regular session is open; the plan does not watch until it can"}
	}
	resume := time.Duration(0)
	if plan := queuedPlanOf(t); carry && plan != nil {
		resume = time.Duration(plan.ResumeWaitMinutes) * time.Minute
	}
	// from is when the trail watches in the session that opens at open,
	// dated date in the option market's calendar: the plan's own session
	// from not_before, a later one from its open plus the resume wait.
	from := func(open time.Time, date string) time.Time {
		if date == t.SessionDate {
			return t.NotBefore
		}
		return open.Add(resume)
	}
	if opt.IsOpen && stk.IsOpen {
		open := opt.Open
		if stk.Open.After(open) {
			open = stk.Open
		}
		if start := from(open, opt.Date); now.Before(start) {
			return trailSession{open: open, next: start, code: rpc.ReadinessOpeningWindow,
				reason: fmt.Sprintf("the session opened at %s; the plan watches from %s, %d minutes after the open, once the opening spreads have settled",
					open.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339), int(start.Sub(open)/time.Minute))}
		}
		return trailSession{in: true, open: open}
	}
	nextOpen := func(x marketcal.Session) time.Time {
		switch {
		case x.IsOpen:
			return x.Open
		case x.NextOpen != nil:
			return *x.NextOpen
		}
		return time.Time{}
	}
	next := time.Time{}
	if a, b := nextOpen(opt), nextOpen(stk); !a.IsZero() && !b.IsZero() {
		if b.After(a) {
			a = b
		}
		if day, ok := s.previewSession(marketcal.Market(t.Market), a); ok {
			next = from(a, day.Date)
		}
	}
	reason := "the regular session is closed; the plan's validity ends before the next session"
	switch {
	case next.IsZero() || !next.Before(t.NotAfter):
		next = time.Time{}
	default:
		reason = "the regular session is closed; the plan watches again from " + next.UTC().Format(time.RFC3339)
		if !rec.Watch.TriggeredAt.IsZero() {
			reason = "the regular session is closed; the exit line was crossed, so the sale resumes from " + next.UTC().Format(time.RFC3339)
		}
	}
	return trailSession{next: next, code: rpc.ReadinessMarketClosed, reason: reason}
}

// enterTrailSession records a carry-over trail entering a new regular
// session. A fired trail whose sale is not at the broker starts its price
// steps again at the mid: yesterday's step is no price for today's quote.
func (e *proposalEngine) enterTrailSession(ctx context.Context, rec queuedAuthRecord, open time.Time) queuedAuthRecord {
	if rec.Terms.Version != queuedPlanSessionsTermsVersion || open.IsZero() || rec.Watch.SessionOpen.Equal(open) {
		return rec
	}
	now := e.clock()
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || !r.waiting() || r.Watch.SessionOpen.Equal(open) {
			return nil
		}
		r.Watch.SessionOpen = open.UTC()
		ev := r.note(queuedEventSessionStarted, r.State, now)
		ev.ExecutorTickAt, ev.Reason = now, "a new regular session opened inside the plan's validity; the trail watches again"
		if !r.Watch.TriggeredAt.IsZero() && r.OrderRef == "" {
			r.Watch.SaleStep, r.Watch.LastLimit, r.Watch.LastBid, r.Watch.LastAsk = 0, 0, 0, 0
			ev.Reason = "a new regular session opened inside the plan's validity; the exit line was crossed, so the sale starts again at the mid"
		}
		return []queuedAuthEvent{ev}
	})
	if err != nil {
		e.server.warnf("exit plan trail %s: record session: %v", rec.Terms.QueueID, err)
		return rec
	}
	if current, ok := e.queued.get(rec.Terms.QueueID); ok {
		return current
	}
	return rec
}

// queuedEventSaleSettled journals a carry-over trail settling a sale left
// from an earlier session.
const queuedEventSaleSettled = "queued_auth.sale_settled"

// settleCarriedTrailSales settles a fired carry-over trail whose last sale
// was sent in an earlier session and is still recorded as at the broker
// (sent, or held because its outcome was unclear or its end unconfirmed,
// as after a daemon that was down at the close). The guard that never
// resends such an attempt protects one session: it is narrowed here, and
// only here, to an attempt from a session that has since closed.
//
// What still bounds it: the attempt was a DAY order, which the broker ends
// at its session's close, so it cannot be working today; the settlement
// runs only inside today's session after the resume wait, with a fresh and
// complete open-order read showing no working sell, and no order of unknown
// contract that could be one, on this option (any working sell keeps the
// record as it is); and the next sale is what the broker shows held now,
// never more than the line less what the trail is known to have sold, so a
// fill the daemon never saw has already reduced it. Worst case: an earlier
// attempt that filled while Canary could not see it leaves the record
// counting less sold than was, and the trail sells what is still held, which
// is what the owner signed. A line that is gone ends the plan sold. Within
// the session an attempt was sent in, nothing changes: it is never resent.
func (e *proposalEngine) settleCarriedTrailSales(ctx context.Context) {
	s := e.server
	if s == nil {
		return
	}
	now := e.clock()
	for _, rec := range e.queued.list() {
		if rec.Terms.Version != queuedPlanSessionsTermsVersion || !queuedPlanTrail(rec.Terms) || rec.Watch.TriggeredAt.IsZero() || rec.OrderRef == "" {
			continue
		}
		carried := rec.State == rpc.QueuedAuthSent ||
			rec.State == rpc.QueuedAuthHeld && (rec.HoldCode == queuedHoldTrailSaleUnconfirmed || rec.HoldCode == queuedReasonSendUnclear)
		// The attempt was sent in the session the trail last entered
		// (enterTrailSession runs before every send, and a record at the
		// broker enters no other): a different open now means that session
		// has closed.
		sess := e.trailSessionAt(rec, now)
		if !carried || !sess.in || rec.Watch.SessionOpen.IsZero() || rec.Watch.SessionOpen.Equal(sess.open) {
			continue
		}
		scope := e.currentScope()
		if !brokerScopeConcrete(scope) || !sameBrokerScope(scope, rec.scope()) {
			continue
		}
		snapshot, inventoryScope, err := s.brokerOpenOrderInventory(ctx, true)
		if err != nil || !snapshot.Complete || !sameBrokerScope(inventoryScope, scope) {
			continue
		}
		probe := rpc.TradeProposal{Contract: rec.Terms.Contract, SecType: rec.Terms.Contract.SecType, Action: rpc.OrderActionSell}
		if _, found, unknown := sameContractWorkingOrder(snapshot, scope, probe); found || unknown {
			continue
		}
		position, err := s.captureOptionLinePosition(ctx, rec.Terms.Contract, rpc.OrderActionSell, 1)
		if err != nil {
			continue
		}
		held := max(position.Impact.Before, 0)
		// What the ended attempt sold, when the journal recorded its end.
		sold := 0
		if views, _, err := s.loadOrderViews(); err == nil {
			for _, v := range views {
				if v.OrderRef == rec.OrderRef && v.ConID == rec.Terms.Contract.ConID {
					sold = int(math.Round(max(v.Filled, 0)))
				}
			}
		}
		at := e.clock()
		var done *queuedAuthRecord
		err = e.queued.update(ctx, at, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
			r := records[rec.Terms.QueueID]
			if r == nil || r.OrderRef != rec.OrderRef || r.State != rec.State {
				return nil
			}
			ref := r.OrderRef
			r.Watch.SaleFilled += sold
			r.SendingAt, r.PreviewTokenID, r.OrderRef, r.QuantitySent, r.LimitPrice, r.SendQuote, r.Late, r.SentAt = time.Time{}, "", "", 0, 0, nil, false, time.Time{}
			r.FilledQuantity, r.AvgFillPrice, r.PermID = 0, 0, 0
			r.Watch.SaleStep, r.Watch.LastLimit, r.Watch.LastBid, r.Watch.LastAsk = 0, 0, 0, 0
			if held <= 0 {
				ev := r.resolve(queuedEventFilled, rpc.QueuedAuthFilled, "", "the line is closed; the sale sent in an earlier session sold it", at)
				ev.OrderRef, ev.ExecutorTickAt = ref, at
				c := *r
				done = &c
				return []queuedAuthEvent{ev}
			}
			r.HeldAt, r.HoldCode = at.UTC(), queuedHoldTrailSaleEnded
			r.HoldReason = boundedQueuedReason(fmt.Sprintf("the sale sent in an earlier session is no longer working at the broker (a DAY order ends at its close); %g is held now and is sold from the mid", held))
			events := []queuedAuthEvent{}
			if r.State != rpc.QueuedAuthHeld {
				ev := r.transition(queuedEventHeld, rpc.QueuedAuthHeld, at)
				ev.ReasonCode, ev.Reason = r.HoldCode, r.HoldReason
				events = append(events, ev)
			}
			ev := r.note(queuedEventSaleSettled, r.State, at)
			ev.OrderRef, ev.FillQty, ev.ExecutorTickAt, ev.Reason = ref, float64(sold), at, r.HoldReason
			return append(events, ev)
		})
		if err != nil {
			s.warnf("exit plan trail %s: settle an earlier session's sale: %v", rec.Terms.QueueID, err)
			continue
		}
		s.infof("exit plan trail %s: settled its sale %s from an earlier session; %g held", rec.Terms.QueueID, rec.OrderRef, held)
		if done != nil {
			e.recordQueuedOutcome(*done, done.State, done.ReasonCode, done.Reason)
		}
	}
}

// planEndReason is why a trail ends when its window does: the option's last
// trading day closed, or its validity ended; crossed or not.
func (e *proposalEngine) planEndReason(r queuedAuthRecord) (code, reason string) {
	code, what := queuedReasonPlanValidityEnded, "the plan's validity ended"
	if e.planEndsWithContract(r.Terms) {
		code, what = queuedReasonPlanContractExpired, "the option's last trading day closed, so the plan ends with its contract"
	}
	if r.Watch.TriggeredAt.IsZero() {
		return code, what + " and the exit line was not crossed; nothing was sold"
	}
	return code, fmt.Sprintf("%s after the exit line was crossed, with %d of %d sold", what, r.Watch.SaleFilled, trailLineCap(r))
}

// planEndsWithContract reports a trail whose window ends at the close of the
// option's last trading day: the next session of its market is past expiry.
func (e *proposalEngine) planEndsWithContract(t rpc.QueuedAuthTerms) bool {
	last := optionLastTradingDate(t.Contract)
	if last == "" || e.server == nil || t.NotAfter.IsZero() {
		return false
	}
	market := marketcal.Market(t.Market)
	at, ok := e.server.previewSession(market, t.NotAfter)
	if ok && at.IsOpen && !at.Close.IsZero() {
		// The stock closed first (US options trade to 16:15 New York).
		at, ok = e.server.previewSession(market, at.Close)
	}
	if !ok || at.NextOpen == nil {
		return false
	}
	next, ok := e.server.previewSession(market, *at.NextOpen)
	return ok && next.State != marketcal.StateUnknown && next.Date > last
}

// planSibling is the other record of rec's plan with role, if any.
func (e *proposalEngine) planSibling(rec queuedAuthRecord, role string) (queuedAuthRecord, bool) {
	for _, other := range e.queued.list() {
		if other.Terms.QueueID != rec.Terms.QueueID && queuedSamePlan(other, rec) && queuedPlanOf(other.Terms).Role == role {
			return other, true
		}
	}
	return queuedAuthRecord{}, false
}

// advanceExitPlan runs a plan record's own step before the ordinary send
// path: the trail watches the stock until it fires, then waits for its first
// sale to be out of the way; the first sale stands aside once the trail has
// fired, and for the cycle while a reading at or below the line awaits its
// confirmation. It reports whether the ordinary send path may continue, with
// the record as it now stands.
func (e *proposalEngine) advanceExitPlan(ctx context.Context, rec queuedAuthRecord) (queuedAuthRecord, bool) {
	switch {
	case queuedPlanFirstSale(rec.Terms):
		trail, ok := e.planSibling(rec, rpc.QueuedPlanRoleTrail)
		if !ok {
			return rec, true
		}
		if !trail.Watch.TriggeredAt.IsZero() {
			e.cancelQueued(ctx, rec, queuedReasonPlanStopTriggered, "the plan's trail fired first and sells what is held; this sale was withdrawn unsent")
			return rec, false
		}
		if live, seen := e.trailLiveFor(trail.Terms.QueueID); seen && !live.breachAt.IsZero() && trail.waiting() {
			return rec, false
		}
		return rec, true
	case queuedPlanTrail(rec.Terms):
		// Outside the regular session of the option or the stock, or inside
		// a later session's opening wait, the trail neither watches nor
		// sells; a read awaiting its confirmation does not carry over.
		sess := e.trailSessionAt(rec, e.clock())
		if !sess.in {
			if live, seen := e.trailLiveFor(rec.Terms.QueueID); seen && !live.breachAt.IsZero() {
				live.breachAt = time.Time{}
				e.setTrailLive(rec.Terms.QueueID, live)
			}
			e.holdQueued(ctx, rec, sess.code, sess.reason)
			return rec, false
		}
		rec = e.enterTrailSession(ctx, rec, sess.open)
		if rec.Watch.TriggeredAt.IsZero() {
			if !e.watchQueuedTrail(ctx, rec) {
				return rec, false
			}
			current, ok := e.queued.get(rec.Terms.QueueID)
			if !ok || !current.waiting() {
				return rec, false
			}
			rec = current
		}
		if e.finishSoldTrail(ctx, rec) {
			return rec, false
		}
		return rec, e.planFirstSaleOutOfTheWay(ctx, rec)
	}
	return rec, true
}

// watchQueuedTrail reads the stock once and folds it into the trail. It
// reports whether the trail fired on this read: two reads at or below the
// line at least five seconds apart. An unusable read holds the record as
// stock_quote_unusable and never fires; a usable one resumes it.
func (e *proposalEngine) watchQueuedTrail(ctx context.Context, rec queuedAuthRecord) bool {
	id := rec.Terms.QueueID
	if e.endFlatPlan(ctx, rec) {
		return false
	}
	// Before it fires, a trail keeps the queue's rule: a protection policy
	// changed since signing holds it. Once fired, it sells on its signed terms.
	if s := e.server; s.protectionPolicies != nil {
		if _, status := s.protectionPolicies.Active(); status.EffectiveFingerprint != rec.Terms.PolicyFingerprint {
			e.holdQueued(ctx, rec, queuedHoldPolicyChanged, "the protection policy changed after the owner signed; the trail is held and cannot fire until the plan is signed again")
			return false
		}
	}
	price, at, code, reason := e.readTrailStock(ctx, rec)
	live, _ := e.trailLiveFor(id)
	if code != "" {
		live.code, live.reason, live.breachAt = code, reason, time.Time{}
		e.setTrailLive(id, live)
		e.holdQueued(ctx, rec, queuedHoldStockQuote, reason)
		return false
	}
	st := *rec.Terms.OwnerOrder.StockTrail
	now := e.clock()
	live.price, live.usableAt, live.code, live.reason = price, at, "", ""
	next, _ := trailAdvance(rec.Watch, st, price, at)
	fire := false
	if price <= next.StopPrice {
		switch {
		case live.breachAt.IsZero():
			live.breachAt = now
		case now.Sub(live.breachAt) >= queuedTrailConfirmAfter:
			fire = true
		}
	} else {
		live.breachAt = time.Time{}
	}
	e.setTrailLive(id, live)
	// The line held when the exit line is crossed is what a whole-line trail
	// sells; read before the trigger is latched.
	line := 0
	if fire && st.WholeLine {
		if position, err := e.server.captureOptionLinePosition(ctx, rec.Terms.Contract, rpc.OrderActionSell, 1); err == nil && position.Impact.Before > 0 {
			line = int(math.Trunc(position.Impact.Before))
		}
	}
	var withdrawn []queuedAuthRecord
	fired := false
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[id]
		if r == nil || !r.waiting() || !r.Watch.TriggeredAt.IsZero() {
			return nil
		}
		var events []queuedAuthEvent
		if r.State == rpc.QueuedAuthHeld {
			r.HeldAt, r.HoldCode, r.HoldReason = time.Time{}, "", ""
			ev := r.transition(queuedEventResumed, rpc.QueuedAuthArmed, now)
			ev.ExecutorTickAt = now
			events = append(events, ev)
		}
		// The stored watch decides, so a stop is never lowered.
		w, raised := trailAdvance(r.Watch, st, price, at)
		if raised {
			r.Watch = w
			ev := r.note(queuedEventStopRaised, r.State, now)
			ev.StockPrice, ev.HighWater, ev.StopPrice, ev.ExecutorTickAt = price, w.HighWater, w.StopPrice, now
			events = append(events, ev)
		}
		if fire && price <= w.StopPrice {
			w.TriggeredAt, w.TriggerPrice, w.LineAtTrigger = now.UTC(), price, line
			r.Watch = w
			ev := r.note(queuedEventTriggered, r.State, now)
			ev.StockPrice, ev.HighWater, ev.StopPrice, ev.ExecutorTickAt = price, w.HighWater, w.StopPrice, now
			ev.Reason = boundedQueuedReason(fmt.Sprintf("the stock's midpoint %g is at or below the exit line %g on two reads at least %d seconds apart; the trail sells what is held", price, w.StopPrice, int(queuedTrailConfirmAfter/time.Second)))
			events = append(events, ev)
			fired = true
			for _, other := range records {
				if other != r && queuedSamePlan(*other, *r) && queuedPlanFirstSale(other.Terms) && other.waiting() {
					oev := other.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonPlanStopTriggered, "the plan's trail fired before this sale was sent; it was withdrawn unsent and the trail sells what is held", now)
					oev.ExecutorTickAt = now
					events = append(events, oev)
					withdrawn = append(withdrawn, *other)
				}
			}
		}
		return events
	})
	if err != nil {
		e.server.warnf("exit plan trail %s: record watch: %v", id, err)
		return false
	}
	for _, w := range withdrawn {
		e.server.infof("queued authorisation %s withdrawn: its plan's trail fired", w.Terms.QueueID)
		e.recordQueuedOutcome(w, decisionCancelled, w.ReasonCode, w.Reason)
	}
	if fired {
		e.server.warnf("exit plan trail %s fired: stock %g at or below its line", id, price)
	}
	return fired
}

// endFlatPlan ends a trail, and its unsent first sale, once the broker shows
// the option line closed: there is nothing left to sell. A position that
// cannot be read ends nothing.
func (e *proposalEngine) endFlatPlan(ctx context.Context, rec queuedAuthRecord) bool {
	s := e.server
	position, err := s.captureOptionLinePosition(ctx, rec.Terms.Contract, rpc.OrderActionSell, 1)
	if err != nil || position.Impact.Before > 0 {
		return false
	}
	if sib, ok := e.planSibling(rec, rpc.QueuedPlanRoleFirstSale); ok && sib.waiting() {
		e.cancelQueued(ctx, sib, queuedReasonPositionChanged, "the option position closed; the exit plan ends")
	}
	e.cancelQueued(ctx, rec, queuedReasonPositionChanged, "the option position closed; the exit plan ends")
	return true
}

// readTrailStock is one read of the trail's stock: the midpoint of a live,
// fresh, two-sided quote whose spread is within the protection policy's
// stock spread limit, and the quote's time. Anything else returns the
// stock_quote_unusable code with a plain reason, and no price.
func (e *proposalEngine) readTrailStock(ctx context.Context, rec queuedAuthRecord) (price float64, at time.Time, code, reason string) {
	s := e.server
	unusable := func(why string) (float64, time.Time, string, string) {
		return 0, time.Time{}, queuedHoldStockQuote, why
	}
	if !s.tradingGatewayReady() {
		return unusable("the broker link is not ready, so the stock cannot be read; nothing can fire until it is")
	}
	authority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return unusable("broker identity could not be verified, so the stock cannot be read; nothing can fire until it is")
	}
	underlying := rec.Terms.OwnerOrder.StockTrail.Underlying
	q, err := s.fetchPreviewSidesQuote(ctx, underlying, queuedTrailQuoteTimeout, authority)
	if err != nil {
		return unusable("no stock quote arrived; nothing can fire until one does")
	}
	limit := 2.0
	if s.protectionPolicies != nil {
		policy, _ := s.protectionPolicies.Active()
		limit = queuedSpreadLimit(policy, rpc.TradeProposal{Contract: underlying})
	}
	bid, ask, err := liveTwoSidedQuote(q, "the stock trail")
	if err != nil {
		return unusable("the stock quote is not usable (" + err.Error() + "); nothing can fire on it")
	}
	mid := (bid + ask) / 2
	if spread := (ask - bid) / mid * 100; spread > limit {
		return unusable(fmt.Sprintf("the stock's spread is %.2f%% of its midpoint, above the %.2f%% limit; nothing can fire on it", spread, limit))
	}
	// A halt or a limit-up-limit-down pause can leave the last quote
	// standing; it is never a price to sell on.
	if why := stockHaltReason(s.marketEventsForSymbols(ctx, []string{underlying.Symbol}), underlying.Symbol); why != "" {
		return unusable(why)
	}
	at = q.PriceAt
	if at.IsZero() {
		at = q.AsOf
	}
	if at.IsZero() {
		at = e.clock()
	}
	return roundPrice(mid), at.UTC(), "", ""
}

// planFirstSaleOutOfTheWay reports whether a triggered trail may sell: its
// plan's first sale is unsent and withdrawn, or has ended at the broker. A
// first sale being placed holds the trail; one still working at the broker
// is cancelled through the daemon's cancel path first, and the trail holds as
// plan_first_sale_cancelling until the order journal shows it ended.
func (e *proposalEngine) planFirstSaleOutOfTheWay(ctx context.Context, rec queuedAuthRecord) bool {
	sib, ok := e.planSibling(rec, rpc.QueuedPlanRoleFirstSale)
	switch {
	case !ok || sib.final() || sib.State == rpc.QueuedAuthPrepared:
		return true
	case sib.waiting():
		e.cancelQueued(ctx, sib, queuedReasonPlanStopTriggered, "the plan's trail fired before this sale was sent; it was withdrawn unsent and the trail sells what is held")
		current, ok := e.queued.get(sib.Terms.QueueID)
		return ok && current.final()
	case sib.State == rpc.QueuedAuthSending:
		e.holdQueued(ctx, rec, queuedHoldFirstSaleCancelling, "the plan's first sale is being placed now; the trail sells what is held once it is cancelled or has ended")
		return false
	}
	// Sent: still working at the broker until the journal shows it ended.
	if sib.OrderRef != "" && e.clock().Sub(rec.Watch.FirstSaleCancelAt) >= queuedPlanCancelRetry {
		e.cancelPlanFirstSale(ctx, rec, sib)
	}
	e.holdQueued(ctx, rec, queuedHoldFirstSaleCancelling, "the trail fired while the plan's first sale was working at the broker; it is being cancelled, and the trail sells what is held once it has ended")
	return false
}

// cancelPlanFirstSale asks the broker to cancel the plan's first sale under
// brokerWriteMu and a grant that names this trail and that order alone, and
// records when it asked.
func (e *proposalEngine) cancelPlanFirstSale(ctx context.Context, rec, sib queuedAuthRecord) {
	s := e.server
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	current, ok := e.queued.get(rec.Terms.QueueID)
	first, sok := e.queued.get(sib.Terms.QueueID)
	if !ok || !current.waiting() || current.Watch.TriggeredAt.IsZero() || !sok || first.State != rpc.QueuedAuthSent || first.OrderRef == "" {
		return
	}
	s.queuedGrant.Store(&queuedWriteGrant{QueueID: current.Terms.QueueID, Key: current.Terms.Key, Bucket: current.Terms.Bucket, CancelOrderRef: first.OrderRef})
	err := s.proposalCancelOrder(ctx, first.OrderRef)
	s.queuedGrant.Store(nil)
	now := e.clock()
	if err != nil {
		s.warnf("exit plan trail %s: cancel of its first sale %s: %v", current.Terms.QueueID, first.OrderRef, err)
	} else {
		s.infof("exit plan trail %s: asked the broker to cancel its first sale %s", current.Terms.QueueID, first.OrderRef)
	}
	if uerr := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[current.Terms.QueueID]
		if r == nil || !r.waiting() {
			return nil
		}
		r.Watch.FirstSaleCancelAt = now.UTC()
		ev := r.note(queuedEventFirstSaleCancel, r.State, now)
		ev.OrderRef, ev.ExecutorTickAt = first.OrderRef, now
		if err != nil {
			ev.Reason = boundedQueuedReason("the broker cancel was refused: " + err.Error())
		}
		return []queuedAuthEvent{ev}
	}); uerr != nil {
		s.warnf("exit plan trail %s: record first-sale cancel: %v", current.Terms.QueueID, uerr)
	}
}

// queuedEventFirstSaleCancel journals a trail's broker cancel of its plan's
// first sale.
const queuedEventFirstSaleCancel = "queued_auth.first_sale_cancel"

// queuedGrantCoversCancel is the cancel half of the daemon-owner-queued
// origin: such a cancel is accepted only for the one order the executor's
// grant names (a triggered trail's own plan first sale).
func (s *Server) queuedGrantCoversCancel(origin string, view rpc.OrderView) error {
	if origin != rpc.OrderOriginDaemonOwnerQueued {
		return nil
	}
	grant := s.queuedGrant.Load()
	if grant == nil || grant.CancelOrderRef == "" || grant.CancelOrderRef != view.OrderRef {
		return errors.New("a daemon-owner-queued cancel is accepted only for the order its grant names")
	}
	return nil
}

// queuedTrailTick is how long the run loop may wait before a trail inside
// its window is read again (or its working sale followed): at most queuedTrailCheckEvery, sooner when a
// read at or below the line awaits its confirmation. ok is false when no
// trail is inside its window, so nothing spins.
func (e *proposalEngine) queuedTrailTick(now time.Time) (time.Duration, bool) {
	if e == nil || !e.queued.attached() {
		return 0, false
	}
	tick, ok := time.Duration(0), false
	for _, rec := range e.queued.list() {
		// A fired trail's working sale is followed and re-priced on the same
		// tick.
		if !queuedPlanTrail(rec.Terms) || !rec.waiting() && rec.State != rpc.QueuedAuthSent || now.Before(rec.Terms.NotBefore) || !now.Before(rec.Terms.NotAfter) ||
			!e.trailSessionAt(rec, now).in {
			continue
		}
		next := queuedTrailCheckEvery
		if live, seen := e.trailLiveFor(rec.Terms.QueueID); seen && !live.breachAt.IsZero() && rec.Watch.TriggeredAt.IsZero() {
			next = min(next, max(queuedTrailConfirmAfter-now.Sub(live.breachAt), queuedMinWake))
		}
		if !ok || next < tick {
			tick, ok = next, true
		}
	}
	return tick, ok
}

// stockHaltReason names an active trading halt or limit-up-limit-down pause
// of symbol in the market-event flags, or "".
func stockHaltReason(events rpc.MarketEventsResult, symbol string) string {
	for _, flag := range events.BySymbol[strings.ToUpper(strings.TrimSpace(symbol))] {
		if flag.Status == rpc.MarketEventStatusActive && (flag.ID == rpc.MarketEventHaltRegulatoryOrNews || flag.ID == rpc.MarketEventLULDRecent) {
			return "the stock is halted or paused (" + nonEmptyString(flag.Label, flag.ID) + "); its last quote is no price to fire on"
		}
	}
	return ""
}

// trailSendQuantity is what a trail sells now: what is held, never more than
// its signed maximum. ok is false while the held quantity is not whole.
func trailSendQuantity(held float64, maxQuantity int) (int, bool) {
	if held <= 0 || math.Trunc(held) != held || held > 1000000 {
		return 0, false
	}
	return min(int(held), maxQuantity), true
}

// queuedTrailSaleWideSpread is a fired trail's view reason while its sale
// works on a quote wider than the signed spread limit.
const queuedTrailSaleWideSpread = "resting_at_mid_wide_spread"

// trailWideFloor is the lowest price a fired trail sells at on a quote wider
// than its spread limit: the bid of a quote exactly that wide around the same
// mid, mid × (1 − limit/200).
func trailWideFloor(mid, maxSpreadPct float64) float64 {
	return mid * (1 - maxSpreadPct/200)
}

// trailSaleBound is the bounded limit of a fired trail's attempt at step.
//
// Once the exit line is crossed the sale always works an order (owner,
// 2026-11-24 10:01 CET, asked whether a crossed line meeting a quote wider
// than 25% should still rest at the mid: "Yes, rest at mid"). Inside the
// signed spread limit the steps are the mid, halfway to the bid, the bid.
// On a wider quote the first attempt rests at the mid, and the steps go down
// no further than trailWideFloor: mid × (1 − limit/200), the bid a quote
// exactly at the limit would show around the same mid (12.5% under the mid
// at 25%). The re-price at the floor follows the current mid every 30
// seconds; when the quote narrows inside the limit the ordinary steps price
// it again. With no two-sided quote there is no mid and nothing is sent.
//
// Inside what was signed: the terms sign no minimum price (stock_trail, no
// worst_price) and max_spread_pct_of_mid, which bounds what a sale gives up
// against the mid. Every price this sends is one the signed terms already
// send on an acceptable quote: the mid, or at most the concession of a sale
// at the bid of a quote at the limit. A wide quote changes when the order
// rests, not how much it may give away, so version-3 and version-4 plans
// both take it; the first sale keeps refusing wide quotes.
func trailSaleBound(step int, maxSpreadPct float64) *rpc.OrderBoundedLimit {
	return &rpc.OrderBoundedLimit{Concession: trailSaleSteps[trailStep(step)].concession, NoWorstPrice: true, MaxSpreadPctOfMid: maxSpreadPct, WideFloorPctOfMid: maxSpreadPct / 2}
}

// queuedFollowsHeldAttempt reports records whose attempt, once its send
// outcome is unclear, is held and followed in the order journal rather than
// failed: a full-position stop, and an exit plan's trail. Neither is resent
// while that attempt is unresolved.
func queuedFollowsHeldAttempt(t rpc.QueuedAuthTerms) bool {
	return ownerQueueProtection(t) || queuedPlanTrail(t)
}

// trailSaleProgress folds the broker's view of a fired trail's current order
// into the record. While it works: an attempt held unclear that the broker
// shows becomes sent, and fill progress is noted. Once it has ended: what it
// sold is added up; the record ends filled once the line is sold, ends with
// the window, and otherwise waits to send again at the bid (an owner cancel
// meanwhile ends it). An order the broker reconciled as gone without a
// recorded end is held for reconciliation and never resent.
// planEnd names the end once the window is over (planEndReason).
func trailSaleProgress(r *queuedAuthRecord, v rpc.OrderView, now time.Time, planEnd func(queuedAuthRecord) (string, string)) (queuedAuthEvent, bool) {
	if !orderLifecycleStatusIsTerminal(v.LifecycleStatus) {
		if v.PermID != 0 {
			r.PermID = v.PermID
		}
		if r.State == rpc.QueuedAuthHeld {
			r.HeldAt, r.HoldCode, r.HoldReason, r.SentAt = time.Time{}, "", "", now.UTC()
			ev := r.transition(queuedEventSent, rpc.QueuedAuthSent, now)
			ev.Reason = "the broker shows the attempt whose outcome was unclear; it is followed as sent"
			return ev, true
		}
		if v.Filled == r.FilledQuantity {
			return queuedAuthEvent{}, false
		}
		r.FilledQuantity, r.AvgFillPrice = v.Filled, v.AvgFillPrice
		ev := r.note(queuedEventPartiallyFilled, r.State, now)
		ev.PermID, ev.FillQty, ev.AvgPrice = r.PermID, r.FilledQuantity, r.AvgFillPrice
		return ev, true
	}
	if v.LifecycleStatus == rpc.OrderLifecycleClosedReconciled {
		if r.HoldCode == queuedHoldTrailSaleUnconfirmed {
			return queuedAuthEvent{}, false
		}
		r.HeldAt, r.HoldCode = now.UTC(), queuedHoldTrailSaleUnconfirmed
		r.HoldReason = "the sale's order closed at the broker without a recorded end, so what it sold is not known; it is never resent until that is reconciled"
		ev := r.transition(queuedEventHeld, rpc.QueuedAuthHeld, now)
		ev.ReasonCode, ev.Reason = r.HoldCode, r.HoldReason
		return ev, true
	}
	sold := int(math.Round(max(v.Filled, 0)))
	r.Watch.SaleFilled += sold
	r.FilledQuantity, r.AvgFillPrice = v.Filled, v.AvgFillPrice
	total, signed := r.Watch.SaleFilled, trailLineCap(*r)
	end := queuedOrderEnd(v)
	var ev queuedAuthEvent
	switch {
	case total >= signed:
		ev = r.resolve(queuedEventFilled, rpc.QueuedAuthFilled, "", fmt.Sprintf("the trail sold the line: %d of %d", total, signed), now)
	case r.CancelRequested:
		ev = r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonOwnerCancelled, fmt.Sprintf("cancelled by the owner; the trail had sold %d of %d", total, signed), now)
	case !now.Before(r.Terms.NotAfter):
		code, reason := planEnd(*r)
		state, eventType := rpc.QueuedAuthExpiredUnfilled, queuedEventExpiredUnfilled
		if total > 0 {
			state, eventType = rpc.QueuedAuthPartiallyFilled, queuedEventPartiallyFilled
		}
		ev = r.resolve(eventType, state, code, boundedQueuedReason(reason+"; the last order "+end), now)
	default:
		// The next attempt starts one step nearer the bid; the ended one's
		// limit and quote decide whether that step would change the price.
		r.Watch.LastLimit, r.Watch.LastBid, r.Watch.LastAsk = r.LimitPrice, 0, 0
		if q := r.SendQuote; q != nil && q.Bid != nil && q.Ask != nil {
			r.Watch.LastBid, r.Watch.LastAsk = *q.Bid, *q.Ask
		}
		r.Watch.SaleStep = trailStep(r.Watch.SaleStep + 1)
		r.SendingAt, r.PreviewTokenID, r.OrderRef, r.QuantitySent, r.LimitPrice, r.SendQuote, r.Late, r.SentAt = time.Time{}, "", "", 0, 0, nil, false, time.Time{}
		r.FilledQuantity, r.AvgFillPrice, r.PermID = 0, 0, 0
		r.HeldAt, r.HoldCode = now.UTC(), queuedHoldTrailSaleEnded
		r.HoldReason = boundedQueuedReason(fmt.Sprintf("the sale's order ended at the broker (%s) with %d of %d sold; the rest is sent again a step nearer the bid, or from the mid at the next session if this one has closed", end, total, signed))
		ev = r.transition(queuedEventHeld, rpc.QueuedAuthHeld, now)
		ev.ReasonCode, ev.Reason = r.HoldCode, r.HoldReason
	}
	ev.FillQty, ev.AvgPrice = v.Filled, v.AvgFillPrice
	return ev, true
}

// repriceTrailSales cancels a fired trail's own sale that has worked
// unfilled for queuedTrailRepriceAfter since it was sent or last cancelled,
// so it is sent again at the fresh bid once the journal shows it ended. It
// stops at the window's end; at most one cancel per interval.
func (e *proposalEngine) repriceTrailSales(ctx context.Context) {
	now := e.clock()
	for _, rec := range e.queued.list() {
		if !queuedPlanTrail(rec.Terms) || rec.State != rpc.QueuedAuthSent || rec.OrderRef == "" || rec.CancelRequested || !now.Before(rec.Terms.NotAfter) ||
			!e.trailSessionAt(rec, now).in {
			continue
		}
		last := rec.SentAt
		if rec.Watch.SaleCancelAt.After(last) {
			last = rec.Watch.SaleCancelAt
		}
		if now.Sub(last) < trailStepWorks(rec.Watch.SaleStep) {
			continue
		}
		e.cancelTrailSale(ctx, rec)
	}
}

// cancelTrailSale cancels the trail's working sale under brokerWriteMu and a
// grant that names that order alone, and journals the attempt with its limit
// and the quote it was priced from.
func (e *proposalEngine) cancelTrailSale(ctx context.Context, rec queuedAuthRecord) {
	s := e.server
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	current, ok := e.queued.get(rec.Terms.QueueID)
	if !ok || current.State != rpc.QueuedAuthSent || current.OrderRef == "" || current.OrderRef != rec.OrderRef || current.CancelRequested {
		return
	}
	s.queuedGrant.Store(&queuedWriteGrant{QueueID: current.Terms.QueueID, Key: current.Terms.Key, Bucket: current.Terms.Bucket, CancelOrderRef: current.OrderRef})
	err := s.proposalCancelOrder(ctx, current.OrderRef)
	s.queuedGrant.Store(nil)
	now := e.clock()
	if err != nil {
		s.warnf("exit plan trail %s: re-price cancel of %s: %v", current.Terms.QueueID, current.OrderRef, err)
	} else {
		s.infof("exit plan trail %s: cancelled its unfilled sale %s at %g to send again at the bid", current.Terms.QueueID, current.OrderRef, current.LimitPrice)
	}
	if uerr := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[current.Terms.QueueID]
		if r == nil || r.State != rpc.QueuedAuthSent || r.OrderRef != current.OrderRef {
			return nil
		}
		r.Watch.SaleCancelAt = now.UTC()
		ev := r.note(queuedEventSaleCancel, r.State, now)
		ev.QtySent, ev.Limit, ev.Quote, ev.FillQty, ev.ExecutorTickAt = r.QuantitySent, r.LimitPrice, r.SendQuote, r.FilledQuantity, now
		if err != nil {
			ev.Reason = boundedQueuedReason("the broker cancel was refused: " + err.Error())
		}
		return []queuedAuthEvent{ev}
	}); uerr != nil {
		s.warnf("exit plan trail %s: record re-price cancel: %v", current.Terms.QueueID, uerr)
	}
}

// finishSoldTrail ends a fired trail that has sold and whose line the
// broker now shows closed: filled, with what it sold.
func (e *proposalEngine) finishSoldTrail(ctx context.Context, rec queuedAuthRecord) bool {
	if rec.Watch.SaleFilled <= 0 {
		return false
	}
	s := e.server
	position, err := s.captureOptionLinePosition(ctx, rec.Terms.Contract, rpc.OrderActionSell, 1)
	if err != nil || position.Impact.Before > 0 {
		return false
	}
	now := e.clock()
	_ = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || !r.waiting() || r.OrderRef != "" {
			return nil
		}
		return []queuedAuthEvent{r.resolve(queuedEventFilled, rpc.QueuedAuthFilled, "", fmt.Sprintf("the line is closed; the trail sold %d", r.Watch.SaleFilled), now)}
	})
	return true
}

// trailStepRepeats reports a fired trail's attempt whose price step, below
// the bid, would send the ended attempt's limit again on an unchanged quote:
// on a market one or two ticks wide a step can land where the last did.
func trailStepRepeats(rec queuedAuthRecord, preview *rpc.OrderPreviewResult) bool {
	w := rec.Watch
	if !queuedPlanTrail(rec.Terms) || preview == nil || trailStep(w.SaleStep) >= len(trailSaleSteps)-1 || !positiveFinite(w.LastLimit) {
		return false
	}
	q := preview.Quote
	return q.Bid != nil && q.Ask != nil && *q.Bid == w.LastBid && *q.Ask == w.LastAsk && preview.Draft.LimitPrice == w.LastLimit
}

// skipTrailStep moves a fired trail to its next price step at once, for a
// step that would not have changed the price. It reports whether the step
// moved, so the caller sends again in the same cycle.
func (e *proposalEngine) skipTrailStep(ctx context.Context, rec queuedAuthRecord) bool {
	now := e.clock()
	moved := false
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || !r.waiting() || r.OrderRef != "" || trailStep(r.Watch.SaleStep) >= len(trailSaleSteps)-1 {
			return nil
		}
		from := trailSaleSteps[trailStep(r.Watch.SaleStep)].name
		r.Watch.SaleStep = trailStep(r.Watch.SaleStep + 1)
		ev := r.note(queuedEventSaleStepSkipped, r.State, now)
		ev.SaleStep, ev.Limit, ev.ExecutorTickAt = trailSaleSteps[r.Watch.SaleStep].name, r.Watch.LastLimit, now
		ev.Reason = "the " + from + " step would send the ended attempt's limit again on the same quote; the next step is sent at once"
		moved = true
		return []queuedAuthEvent{ev}
	})
	if err != nil {
		e.server.warnf("exit plan trail %s: record step skip: %v", rec.Terms.QueueID, err)
		return false
	}
	return moved
}

// queuedEventSaleStepSkipped journals a price step a fired trail passed over.
const queuedEventSaleStepSkipped = "queued_auth.sale_step_skipped"
