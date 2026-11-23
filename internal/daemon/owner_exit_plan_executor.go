package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

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
			w.TriggeredAt, w.TriggerPrice = now.UTC(), price
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
	position, err := s.capturePreviewOrderPositionAuthority(ctx, s.currentTradingStatus(), rec.Terms.Contract, rpc.OrderActionSell, 1)
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
// its window is read again: at most queuedTrailCheckEvery, sooner when a
// read at or below the line awaits its confirmation. ok is false when no
// trail is inside its window, so nothing spins.
func (e *proposalEngine) queuedTrailTick(now time.Time) (time.Duration, bool) {
	if e == nil || !e.queued.attached() {
		return 0, false
	}
	tick, ok := time.Duration(0), false
	for _, rec := range e.queued.list() {
		if !queuedPlanTrail(rec.Terms) || !rec.waiting() || now.Before(rec.Terms.NotBefore) || !now.Before(rec.Terms.NotAfter) {
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
