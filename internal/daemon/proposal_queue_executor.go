package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The queued executor sends what the owner armed (proposal_queue.go). It runs
// in the proposal loop after every refresh, and the loop also wakes at the
// next send time. Nothing happens before not_before; a record expires at
// not_after. Inside the window every send revalidates like a manual submit,
// by key rather than revision, and a record the gates refuse is held while
// the refusal is one the send can wait out, else cancelled.

// Hold reasons of the settling rule for a queued record. Neither names an
// order, symbol or account.
const (
	queuedHoldHandOrderReason   = "an order placed outside Canary's gate after the owner signed, or modified since, is still working at the broker; this sends once it fills or is cancelled, inside the window"
	queuedHoldUnverifiedReason  = "the broker's open-order inventory is unavailable, so a new or modified hand order cannot be ruled out; this sends once it can, inside the window"
	queuedHoldBrokerLinkReason  = "the broker link to IBKR Gateway/TWS is not ready"
	queuedHoldUnscopedReason    = "the broker session has no concrete account and paper/live mode yet"
	queuedHoldSessionReason     = "the regular session is not open"
	queuedMinWake               = time.Second
	queuedIntentNotStagedReason = "the send intent could not be persisted; nothing was sent"
	// queuedSendingStale is how long a record may stay sending in this
	// process before its outcome write is taken as lost and the order journal
	// decides, as after a restart. The executor sends synchronously, so
	// nothing is in flight between cycles.
	queuedSendingStale = queuedSubmitTimeout + time.Minute
	// queuedHoldConfigPaused holds sends while config.toml runs parts of the
	// automation sections on Canary's defaults (owner decision #42).
	queuedHoldConfigPaused = "config_automation_paused"
)

// queuedWriteGrant is the request-scoped fact the broker-write gate checks
// for the daemon-owner-queued origin: which record the executor is sending
// right now. It exists only while the executor holds brokerWriteMu.
type queuedWriteGrant struct {
	QueueID string
	Key     string
	Bucket  string
}

// Refusal codes a send can wait out, beyond the readiness groups: transient
// failures of the preview's own broker stages and of local evidence.
var (
	queuedTransientCodes = []string{previewContractUnresolvedCode, previewPositionUnavailableCode, previewNotionalUnavailableCode,
		previewWhatIfFailedCode, previewTokenUnavailableCode, previewGenericFailureCode, "order_journal_unavailable",
		"journal_authority_unavailable", "submit_intent_not_persisted", "proposal_refresh_failed", reductionOrderUnavailableCode,
		"protective_order_evidence_unavailable"}
	queuedHandOrderCodes = []string{reductionOrderExistingCode, reductionOrderUnknownCode}
)

// queuedDisposition says what a refusal means for a waiting record: hold it
// under a hold code, or cancel it under a reason code. A refusal the send
// cannot wait out wins over one it can.
func queuedDisposition(blockers []rpc.TradingBlocker) (hold bool, code, reason string) {
	holdCode, holdReason := "", ""
	for _, b := range blockers {
		c := strings.TrimSpace(b.Code)
		if c == "" {
			continue
		}
		var group string
		switch {
		case slices.Contains(readinessFrozenCodes, c):
			group = rpc.ReadinessTradingFrozen
		case slices.Contains(readinessBrokerCodes, c), slices.Contains(queuedTransientCodes, c):
			group = rpc.ReadinessBrokerUnavailable
		case slices.Contains(readinessHaltCodes, c):
			group = rpc.ReadinessHalted
		case slices.Contains(readinessSessionCodes, c):
			group = rpc.ReadinessMarketClosed
		case slices.Contains(readinessSpreadCodes, c):
			group = rpc.ReadinessSpreadTooWide
		case slices.Contains(readinessQuoteCodes, c):
			group = rpc.ReadinessQuoteUnusable
		case slices.Contains(queuedHandOrderCodes, c):
			group = queuedHoldHandOrder
		case c == previewBoundedWorstCode:
			group = queuedHoldWorstPrice
		case c == "preview_not_submit_eligible":
			return false, queuedReasonWhatIfRefused, nonEmptyString(b.Message, "broker WhatIf did not accept the order")
		default:
			return false, c, nonEmptyString(b.Message, c)
		}
		if holdCode == "" {
			holdCode, holdReason = group, nonEmptyString(b.Message, c)
		}
	}
	if holdCode == "" {
		return false, "", ""
	}
	return true, holdCode, holdReason
}

// runQueuedCycle is one pass of the executor: resolve sends a previous
// process left in flight, expire what can no longer send, follow sent orders
// to their outcome, then send or hold what is due.
func (e *proposalEngine) runQueuedCycle(ctx context.Context) {
	if e == nil || !e.queued.attached() || ctx == nil || ctx.Err() != nil {
		return
	}
	e.endUnreadableQueued(ctx)
	e.recoverQueuedAfterRestart(ctx)
	e.expireQueued(ctx)
	e.followQueuedSent(ctx)
	now := e.clock()
	for _, rec := range e.queued.list() {
		if !rec.waiting() || now.Before(rec.Terms.NotBefore) || !now.Before(rec.Terms.NotAfter) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		e.executeQueued(ctx, rec)
	}
}

// queuedWake is how long Run may wait before the executor next has something
// to do: a send window opening, an arm deadline or a window end. ok is false
// when nothing waits.
func (e *proposalEngine) queuedWake(now time.Time) (time.Duration, bool) {
	if e == nil || !e.queued.attached() {
		return 0, false
	}
	var next time.Time
	consider := func(at time.Time) {
		if at.After(now) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for _, rec := range e.queued.list() {
		switch {
		case rec.State == rpc.QueuedAuthPrepared:
			consider(rec.Terms.ArmDeadline)
		case rec.waiting():
			consider(rec.Terms.NotBefore)
			consider(rec.Terms.NotAfter)
		}
	}
	if next.IsZero() {
		return 0, false
	}
	return max(next.Sub(now), queuedMinWake), true
}

// expireQueued ends the records that can no longer send: prepared and never
// armed within ten minutes, or waiting when the window ends.
func (e *proposalEngine) expireQueued(ctx context.Context) {
	now := e.clock()
	var ended []queuedAuthRecord
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, r := range records {
			switch {
			case r.State == rpc.QueuedAuthPrepared && !now.Before(r.Terms.ArmDeadline):
				events = append(events, r.resolve(queuedEventExpired, rpc.QueuedAuthExpired, queuedReasonNotArmed, "the owner's signature did not arrive within ten minutes", now))
			case r.waiting() && !now.Before(r.Terms.NotAfter):
				reason := "the send window ended before every gate passed"
				if r.HoldReason != "" {
					reason += "; it was held: " + r.HoldReason
				}
				ev := r.resolve(queuedEventExpired, rpc.QueuedAuthExpired, queuedReasonWindowEnded, reason, now)
				ev.ExecutorTickAt = now
				events = append(events, ev)
				ended = append(ended, *r)
			}
		}
		return events
	})
	if err != nil {
		e.server.warnf("queued authorisations: record expiry: %v", err)
		return
	}
	for _, r := range ended {
		e.server.warnf("queued authorisation %s for %s expired unsent: %s", r.Terms.QueueID, r.Terms.Key, r.Reason)
		e.recordQueuedOutcome(r, decisionExpired, r.ReasonCode, r.Reason)
	}
}

// followQueuedSent follows each sent order in the order journal to its
// outcome: fill progress while it works, then filled, partially filled or
// expired unfilled once it is done.
func (e *proposalEngine) followQueuedSent(ctx context.Context) {
	var sent []queuedAuthRecord
	for _, rec := range e.queued.list() {
		if rec.State == rpc.QueuedAuthSent && rec.OrderRef != "" {
			sent = append(sent, rec)
		}
	}
	if len(sent) == 0 || e.server == nil {
		return
	}
	views, _, err := e.server.loadOrderViews()
	if err != nil {
		return
	}
	byRef := make(map[string]rpc.OrderView, len(views))
	for _, v := range views {
		if v.OrderRef != "" {
			byRef[v.OrderRef] = v
		}
	}
	now := e.clock()
	var done []queuedAuthRecord
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, s := range sent {
			r := records[s.Terms.QueueID]
			if r == nil || r.State != rpc.QueuedAuthSent {
				continue
			}
			v, ok := byRef[r.OrderRef]
			if !ok || !orderViewMatchesBrokerScope(v, r.scope()) {
				continue
			}
			progressed := v.Filled != r.FilledQuantity || v.PermID != r.PermID && v.PermID != 0
			r.FilledQuantity, r.AvgFillPrice = v.Filled, v.AvgFillPrice
			if v.PermID != 0 {
				r.PermID = v.PermID
			}
			var ev queuedAuthEvent
			switch {
			case orderLifecycleStatusIsTerminal(v.LifecycleStatus) && r.QuantitySent > 0 && r.FilledQuantity+1e-9 >= float64(r.QuantitySent):
				ev = r.resolve(queuedEventFilled, rpc.QueuedAuthFilled, "", "filled at the broker", now)
			case orderLifecycleStatusIsTerminal(v.LifecycleStatus) && r.FilledQuantity > 0:
				ev = r.resolve(queuedEventPartiallyFilled, rpc.QueuedAuthPartiallyFilled, v.LifecycleStatus, "the order ended partly filled: "+queuedOrderEnd(v), now)
			case orderLifecycleStatusIsTerminal(v.LifecycleStatus):
				ev = r.resolve(queuedEventExpiredUnfilled, rpc.QueuedAuthExpiredUnfilled, v.LifecycleStatus, "the order ended unfilled: "+queuedOrderEnd(v), now)
			case progressed && r.FilledQuantity > 0:
				ev = r.note(queuedEventPartiallyFilled, r.State, now)
			default:
				continue
			}
			ev.PermID, ev.FillQty, ev.AvgPrice = r.PermID, r.FilledQuantity, r.AvgFillPrice
			events = append(events, ev)
			if r.final() {
				done = append(done, *r)
			}
		}
		return events
	})
	if err != nil {
		e.server.warnf("queued authorisations: record order outcome: %v", err)
		return
	}
	for _, r := range done {
		e.server.infof("queued authorisation %s for %s %s (%g of %d)", r.Terms.QueueID, r.Terms.Key, r.State, r.FilledQuantity, r.QuantitySent)
		e.recordQueuedOutcome(r, r.State, r.ReasonCode, r.Reason)
	}
}

func queuedOrderEnd(v rpc.OrderView) string {
	if msg := strings.TrimSpace(v.LastMessage); msg != "" {
		return v.LifecycleStatus + " (" + truncateBlockerCause(msg) + ")"
	}
	return v.LifecycleStatus
}

// endUnreadableQueued ends the records this build cannot fully read
// (queuedRecordProblem) before anything else runs: an unsent one is
// cancelled, and one caught sending, sent or in an unknown state fails for
// the owner to confirm against the order journal. None is ever sent.
func (e *proposalEngine) endUnreadableQueued(ctx context.Context) {
	now := e.clock()
	var ended []queuedAuthRecord
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, r := range records {
			problem := queuedRecordProblem(*r)
			if problem == "" || r.final() && slices.Contains(queuedKnownStates, r.State) {
				continue
			}
			r.problem = ""
			var ev queuedAuthEvent
			switch r.State {
			case rpc.QueuedAuthPrepared, rpc.QueuedAuthArmed, rpc.QueuedAuthHeld:
				ev = r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonRecordUnreadable, problem+"; nothing was sent", now)
			case rpc.QueuedAuthSent:
				ev = r.resolve(queuedEventFailed, rpc.QueuedAuthFailed, queuedReasonRecordUnreadable, problem+"; the order reached the broker, so follow it in the order journal", now)
			default:
				ev = r.resolve(queuedEventFailed, rpc.QueuedAuthFailed, queuedReasonRecordUnreadable, problem+"; confirm against the order journal and broker statements; not resent", now)
			}
			ev.ExecutorTickAt = now
			events = append(events, ev)
			ended = append(ended, *r)
		}
		return events
	})
	if err != nil {
		e.server.warnf("queued authorisations: ending unreadable records: %v", err)
		return
	}
	for _, r := range ended {
		e.server.warnf("queued authorisation %s for %s ended unreadable: %s", r.Terms.QueueID, r.Terms.Key, r.Reason)
		e.recordQueuedOutcome(r, r.State, r.ReasonCode, r.Reason)
	}
}

// recoverQueuedAfterRestart resolves records a previous process left in
// sending. The intent was persisted before the broker call and the order
// journal stages its send attempt before the first frame, so the journal
// decides: a journaled send is sent, a staged attempt without an outcome is
// unclear and never resent, and no trace at all proves nothing went out, so
// the record waits again for a send inside its window, or ends cancelled if
// the owner cancelled it meanwhile. A record this process left sending
// because its outcome write failed is resolved the same way once stale.
func (e *proposalEngine) recoverQueuedAfterRestart(ctx context.Context) {
	var stuck []queuedAuthRecord
	now := e.clock()
	for _, rec := range e.queued.list() {
		if rec.State == rpc.QueuedAuthSending && (rec.SendingAt.Before(e.startedAt) || now.Sub(rec.SendingAt) > queuedSendingStale) {
			stuck = append(stuck, rec)
		}
	}
	if len(stuck) == 0 || e.server == nil || e.server.orderJournal == nil {
		return
	}
	journal, err := e.server.orderJournal.LoadEvents(0)
	if err != nil {
		e.server.warnf("queued authorisations: restart recovery cannot read the order journal: %v", err)
		return
	}
	var recovered []queuedAuthRecord
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, s := range stuck {
			r := records[s.Terms.QueueID]
			if r == nil || r.State != rpc.QueuedAuthSending {
				continue
			}
			outcome, orderRef := orderJournalSendOutcome(journal, r.PreviewTokenID)
			if orderRef != "" {
				r.OrderRef = orderRef
			}
			across := "across the restart"
			if !r.SendingAt.Before(e.startedAt) {
				across = "because its outcome was not recorded"
			}
			var ev queuedAuthEvent
			switch {
			case outcome == orderSendReached:
				r.SentAt = now
				ev = r.transition(queuedEventRecovered, rpc.QueuedAuthSent, now)
				ev.Reason = "the order reached the broker; confirmed from the order journal " + across
			case outcome == orderSendUnclear:
				ev = r.resolve(queuedEventRecovered, rpc.QueuedAuthFailed, queuedReasonSendUnclear, "the broker send outcome is unknown "+across+"; confirm against the order journal and broker statements; not resent", now)
			case r.CancelRequested:
				ev = r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonOwnerCancelled, "cancelled by the owner while the order was being placed; the order journal shows the attempt never reached the broker, so nothing was sent", now)
			default:
				r.SendingAt, r.PreviewTokenID, r.OrderRef, r.QuantitySent, r.LimitPrice, r.SendQuote, r.Late = time.Time{}, "", "", 0, 0, nil, false
				ev = r.transition(queuedEventRecovered, rpc.QueuedAuthArmed, now)
				ev.ReasonCode, ev.Reason = queuedReasonRestartBeforeSend, "the send stopped "+across+" before it reached the broker; nothing was sent, and the send is retried inside the window after full revalidation"
			}
			ev.DaemonStartedAt, ev.ExecutorTickAt = e.startedAt, now
			events = append(events, ev)
			recovered = append(recovered, *r)
		}
		return events
	})
	if err != nil {
		e.server.warnf("queued authorisations: restart recovery: %v", err)
		return
	}
	for _, r := range recovered {
		e.server.warnf("queued authorisation %s for %s recovered after restart: %s", r.Terms.QueueID, r.Terms.Key, r.State)
	}
}

// executeQueued revalidates one waiting record inside its window and sends
// it, or holds or cancels it.
func (e *proposalEngine) executeQueued(ctx context.Context, rec queuedAuthRecord) {
	s := e.server
	if s == nil {
		return
	}
	now := e.clock()
	if problem := queuedRecordProblem(rec); problem != "" {
		e.cancelQueued(ctx, rec, queuedReasonRecordUnreadable, problem)
		return
	}
	scope := e.currentScope()
	switch {
	case !brokerScopeConcrete(scope):
		e.holdQueued(ctx, rec, rpc.ReadinessBrokerUnavailable, queuedHoldUnscopedReason)
		return
	case !sameBrokerScope(scope, rec.scope()):
		e.cancelQueued(ctx, rec, queuedReasonAccountChanged, "the connected account or paper/live mode changed after the owner signed")
		return
	case s.tradingFrozen():
		e.holdQueued(ctx, rec, rpc.ReadinessTradingFrozen, tradingFrozenBlockerMessage)
		return
	}
	if paused, sections := s.configPausesAutomation(); paused {
		e.holdQueued(ctx, rec, queuedHoldConfigPaused, fmt.Sprintf("config.toml [%s] runs on Canary's defaults; the send waits inside its window until the file is fixed and Canary restarted", strings.Join(sections, "], [")))
		return
	}
	if !s.tradingGatewayReady() {
		e.holdQueued(ctx, rec, rpc.ReadinessBrokerUnavailable, queuedHoldBrokerLinkReason)
		return
	}
	if session, ok := s.previewSession(marketcal.Market(rec.Terms.Market), now); ok && session.State != marketcal.StateUnknown && !session.IsOpen {
		e.holdQueued(ctx, rec, rpc.ReadinessMarketClosed, nonEmptyString(sessionClosedPhrase(session), queuedHoldSessionReason))
		return
	}
	if !s.cfg.AutoTrade.WithDefaults().FastPathEnabledResolved() {
		e.cancelQueued(ctx, rec, "fast_path_disabled", "proposal submit was disabled by [auto_trade].fast_path_enabled after the owner signed")
		return
	}
	prop, blockers, code, reason := e.queuedResolve(ctx, rec)
	if code != "" {
		e.cancelQueued(ctx, rec, code, reason)
		return
	}
	if len(blockers) > 0 {
		e.settleQueuedRefusal(ctx, rec, blockers)
		return
	}
	book := e.automaticSettlingBook(ctx, scope, true)
	switch book.holdSince(rec.ArmedAt, rec.SettlingBaseline, rec.SettlingBaselineKnown) {
	case "":
	case automaticHoldUnavailable:
		e.holdQueued(ctx, rec, rpc.ReadinessBrokerUnavailable, queuedHoldUnverifiedReason)
		return
	default:
		e.holdQueued(ctx, rec, queuedHoldHandOrder, queuedHoldHandOrderReason)
		return
	}
	e.sendQueued(ctx, rec, prop)
}

// queuedResolve revalidates a record's row by key, not revision: the row
// must still exist with the signed row terms, position and policy. Revision
// churn never cancels. It returns the current row and the blockers it
// carries, or the reason code and reason to cancel with.
func (e *proposalEngine) queuedResolve(ctx context.Context, rec queuedAuthRecord) (rpc.TradeProposal, []rpc.TradingBlocker, string, string) {
	var snap rpc.TradeProposalSnapshot
	var err error
	if e.queuedRefreshForTest != nil {
		snap, err = e.queuedRefreshForTest(ctx)
	} else {
		snap, err = e.Refresh(ctx, false)
	}
	if err != nil {
		// A failed refresh may leave an older snapshot; the send waits for a
		// current one rather than revalidating against it.
		blockers := snap.Blockers
		if len(blockers) == 0 || len(snap.Proposals) > 0 {
			blockers = []rpc.TradingBlocker{{Code: "proposal_refresh_failed", Message: err.Error()}}
		}
		return rpc.TradeProposal{}, blockers, "", ""
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, "", ""
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, "", ""
	}
	if !sameBrokerScope(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}, rec.scope()) {
		return rpc.TradeProposal{}, nil, queuedReasonAccountChanged, "the proposals now belong to a different account or paper/live mode"
	}
	i := slices.IndexFunc(snap.Proposals, func(p rpc.TradeProposal) bool { return p.Key == rec.Terms.Key })
	if i < 0 {
		return rpc.TradeProposal{}, nil, queuedReasonRowGone, "the rule no longer proposes this reduction"
	}
	prop := snap.Proposals[i]
	switch {
	case queuedRowTermsDigest(prop) != rec.Terms.RowTermsDigest:
		return prop, nil, queuedReasonRowTermsChanged, "the proposal's contract, side or effect changed after the owner signed"
	case snap.EffectivePolicyFingerprint != rec.Terms.PolicyFingerprint:
		return prop, nil, queuedReasonPolicyChanged, "the protection policy changed after the owner signed"
	case prop.SourceFingerprints.EffectiveRulebook != rec.Terms.RulebookFingerprint:
		return prop, nil, queuedReasonPolicyChanged, "the Rulebook changed after the owner signed"
	case math.Abs(prop.PositionQuantity-rec.Terms.PositionQuantity) > 1e-9:
		return prop, nil, queuedReasonPositionChanged, fmt.Sprintf("the position changed from %g to %g after the owner signed", rec.Terms.PositionQuantity, prop.PositionQuantity)
	}
	return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), "", ""
}

// settleQueuedRefusal holds or cancels a record on a refusal that sent
// nothing.
func (e *proposalEngine) settleQueuedRefusal(ctx context.Context, rec queuedAuthRecord, blockers []rpc.TradingBlocker) {
	hold, code, reason := queuedDisposition(blockers)
	switch {
	case hold:
		e.holdQueued(ctx, rec, code, reason)
	case code != "":
		e.cancelQueued(ctx, rec, code, reason)
	}
}

// holdQueued records what a waiting record waits for, once per hold code.
func (e *proposalEngine) holdQueued(ctx context.Context, rec queuedAuthRecord, code, reason string) {
	now := e.clock()
	reason = boundedQueuedReason(reason)
	changed := false
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || !r.waiting() || r.State == rpc.QueuedAuthHeld && r.HoldCode == code {
			return nil
		}
		if r.HeldAt.IsZero() {
			r.HeldAt = now.UTC()
		}
		r.HoldCode, r.HoldReason = code, reason
		ev := r.transition(queuedEventHeld, rpc.QueuedAuthHeld, now)
		ev.ReasonCode, ev.Reason, ev.ExecutorTickAt = code, reason, now
		changed = true
		return []queuedAuthEvent{ev}
	})
	if err != nil {
		e.server.warnf("queued authorisation %s: record hold: %v", rec.Terms.QueueID, err)
		return
	}
	if changed {
		e.server.infof("queued authorisation %s for %s held (%s): %s", rec.Terms.QueueID, rec.Terms.Key, code, reason)
		e.recordQueuedOutcome(rec, decisionHeld, code, reason)
	}
}

// cancelQueued ends a waiting record the gates refused for good.
func (e *proposalEngine) cancelQueued(ctx context.Context, rec queuedAuthRecord, code, reason string) {
	now := e.clock()
	changed := false
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || !r.waiting() {
			return nil
		}
		ev := r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, code, reason, now)
		ev.ExecutorTickAt = now
		changed = true
		return []queuedAuthEvent{ev}
	})
	if err != nil {
		e.server.warnf("queued authorisation %s: record cancel: %v", rec.Terms.QueueID, err)
		return
	}
	if changed {
		e.server.warnf("queued authorisation %s for %s cancelled unsent (%s): %s", rec.Terms.QueueID, rec.Terms.Key, code, reason)
		e.recordQueuedOutcome(rec, decisionCancelled, code, reason)
	}
}

// sendQueued performs one send under brokerWriteMu and the record's grant.
// The intent (state sending, preview token id, price and quote) is persisted
// after the bounded preview passes every gate and before the broker call, so
// a crash in between is resolved from the order journal and never produces a
// second order.
func (e *proposalEngine) sendQueued(ctx context.Context, rec queuedAuthRecord, prop rpc.TradeProposal) {
	s := e.server
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	id := rec.Terms.QueueID
	current, ok := e.queued.get(id)
	if !ok || !current.waiting() {
		// A cancel landed between the list and the lock.
		return
	}
	s.queuedGrant.Store(&queuedWriteGrant{QueueID: id, Key: current.Terms.Key, Bucket: current.Terms.Bucket})
	defer s.queuedGrant.Store(nil)
	terms := current.Terms
	params := rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: prop.Revision, Quantity: min(terms.MaxQuantity, prop.Quantity), FastPath: true,
		Origin: rpc.OrderOriginDaemonOwnerQueued, TimeoutMs: int(queuedSubmitTimeout.Milliseconds())}
	staged, stagedToken := false, ""
	// mismatch and problem end the record: the draft cannot sit inside the
	// signed terms, or the record cannot be read as signed. outside means the
	// window ended before staging; expiry ends the record.
	mismatch, problem, outside := "", "", false
	var placeErr error
	res, err := e.submit(ctx, params, proposalSubmitOptions{
		queued:       &current,
		resolved:     &prop,
		bounded:      &rpc.OrderBoundedLimit{Concession: terms.Concession, WorstPrice: terms.WorstPrice, MaxSpreadPctOfMid: terms.MaxSpreadPctOfMid},
		placeRefused: func(err error) { placeErr = err },
		beforePlace: func(preview *rpc.OrderPreviewResult) error {
			// The row and the preview are both reduce-only (the submit
			// path's safety gate); the draft must also sit inside the signed
			// contract, side, quantity and price.
			switch {
			case preview.Draft.Contract.ConID != terms.Contract.ConID || !strings.EqualFold(preview.Draft.Action, terms.Action):
				mismatch = "the preview does not describe the signed contract and side"
			case preview.Draft.Quantity <= 0 || preview.Draft.Quantity > terms.MaxQuantity:
				mismatch = "the preview quantity is outside the signed maximum"
			case !queuedLimitInside(terms.Action, preview.Draft.LimitPrice, terms.WorstPrice):
				mismatch = "the preview limit is beyond the signed worst price"
			}
			if mismatch != "" {
				return errors.New(mismatch)
			}
			at := e.clock()
			stageErr := e.queued.update(ctx, at, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
				r := records[id]
				if r == nil || !r.waiting() {
					return nil
				}
				// The stored terms are read again as signed, and the send stays
				// inside the window, at the moment the intent is staged.
				if problem = queuedRecordProblem(*r); problem != "" {
					return nil
				}
				if at.Before(r.Terms.NotBefore) || !at.Before(r.Terms.NotAfter) {
					outside = true
					return nil
				}
				var events []queuedAuthEvent
				if r.State == rpc.QueuedAuthHeld {
					r.HeldAt, r.HoldCode, r.HoldReason = time.Time{}, "", ""
					events = append(events, r.transition(queuedEventResumed, rpc.QueuedAuthArmed, at))
				}
				r.SendingAt, r.PreviewTokenID, r.OrderRef = at.UTC(), preview.PreviewTokenID, preview.Draft.OrderRef
				r.QuantitySent, r.LimitPrice, r.SendQuote = preview.Draft.Quantity, preview.Draft.LimitPrice, queuedSendQuote(preview.Quote)
				r.Late = at.Sub(r.Terms.NotBefore) > queuedLateAfter
				ev := r.transition(queuedEventSending, rpc.QueuedAuthSending, at)
				ev.QtySent, ev.Limit, ev.Quote, ev.Late, ev.Origin = r.QuantitySent, r.LimitPrice, r.SendQuote, r.Late, rpc.OrderOriginDaemonOwnerQueued
				ev.DaemonStartedAt, ev.ExecutorTickAt = e.startedAt, at
				return append(events, ev)
			})
			if stageErr != nil {
				return fmt.Errorf("persist queued send intent: %w", stageErr)
			}
			// Only the attempt whose token the intent names may send.
			if got, ok := e.queued.get(id); !ok || got.State != rpc.QueuedAuthSending || got.PreviewTokenID != preview.PreviewTokenID {
				return errors.New("the queued authorisation was cancelled or taken before the broker call")
			}
			staged, stagedToken = true, preview.PreviewTokenID
			return nil
		},
	})
	finish := e.clock()
	if !staged {
		switch {
		case mismatch != "":
			e.cancelQueued(ctx, current, queuedReasonSendRefused, mismatch+"; nothing was sent")
			return
		case problem != "":
			e.cancelQueued(ctx, current, queuedReasonRecordUnreadable, problem)
			return
		case outside:
			return
		}
		if err == nil && len(res.Blockers) == 0 {
			return
		}
		blockers := slices.Clone(res.Blockers)
		if len(blockers) == 0 {
			blockers = previewFailureBlockers(err)
		}
		// WhatIf that the broker never answered is not a refusal: only a
		// rejected WhatIf cancels the record.
		if res.Preview != nil && res.Preview.WhatIf.Status != rpc.OrderWhatIfStatusRejected {
			for i := range blockers {
				if blockers[i].Code == "preview_not_submit_eligible" {
					blockers[i].Code = previewWhatIfFailedCode
				}
			}
		}
		e.settleQueuedRefusal(ctx, current, blockers)
		return
	}
	outcome, journalRef := orderSendReached, ""
	if err != nil || !res.Accepted {
		// A staged send that did not return accepted may still have reached
		// the broker; the order journal says what went out. The freeze's
		// typed refusal proves by itself that nothing did.
		outcome = orderSendUnclear
		if automaticFrozenRefusal(res, err, placeErr) {
			outcome = orderSendNone
		} else if s.orderJournal != nil {
			if journal, loadErr := s.orderJournal.LoadEvents(0); loadErr == nil {
				outcome, journalRef = orderJournalSendOutcome(journal, stagedToken)
			}
		}
	}
	var final queuedAuthRecord
	outcomeErr := e.queued.update(ctx, finish, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[id]
		if r == nil || r.State != rpc.QueuedAuthSending {
			return nil
		}
		var ev queuedAuthEvent
		switch outcome {
		case orderSendReached:
			r.SentAt, r.OrderRef = finish.UTC(), nonEmptyString(res.OrderRef, nonEmptyString(journalRef, r.OrderRef))
			ev = r.transition(queuedEventSent, rpc.QueuedAuthSent, finish)
			ev.QtySent, ev.Limit, ev.Late, ev.Origin = r.QuantitySent, r.LimitPrice, r.Late, rpc.OrderOriginDaemonOwnerQueued
		case orderSendNone, orderSendFailedUnsent:
			// Nothing reached the broker: the freeze or a gate refused the
			// place before its first frame, or before it staged anything.
			// The record waits again with its spent token proven unsent; the
			// next attempt revalidates and previews afresh inside the window.
			// An owner cancel that arrived meanwhile ends it instead.
			if r.CancelRequested {
				ev = r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonOwnerCancelled, "cancelled by the owner while the order was being placed; the attempt never reached the broker, so nothing was sent", finish)
				break
			}
			code, reason := rpc.ReadinessBrokerUnavailable, "the send was refused before it reached the broker ("+automaticOutcomeReason(res, err)+"); it is retried inside the window"
			if automaticFrozenRefusal(res, err, placeErr) {
				code, reason = rpc.ReadinessTradingFrozen, tradingFrozenBlockerMessage
			}
			r.SendingAt, r.PreviewTokenID, r.OrderRef, r.QuantitySent, r.LimitPrice, r.SendQuote, r.Late = time.Time{}, "", "", 0, 0, nil, false
			r.HeldAt, r.HoldCode, r.HoldReason = finish.UTC(), code, boundedQueuedReason(reason)
			ev = r.transition(queuedEventHeld, rpc.QueuedAuthHeld, finish)
			ev.ReasonCode, ev.Reason = code, r.HoldReason
		default:
			ev = r.resolve(queuedEventFailed, rpc.QueuedAuthFailed, queuedReasonSendUnclear,
				"the send did not return a confirmed result: "+automaticOutcomeReason(res, err)+"; confirm against the order journal and broker statements; not resent", finish)
		}
		ev.ExecutorTickAt = finish
		final = *r
		return []queuedAuthEvent{ev}
	})
	if outcomeErr != nil {
		s.warnf("queued authorisation %s: record send outcome: %v", id, outcomeErr)
		return
	}
	switch final.State {
	case rpc.QueuedAuthSent:
		s.infof("queued authorisation %s: sent %s %d %s at %g", id, strings.ToLower(terms.Action), final.QuantitySent, terms.Key, final.LimitPrice)
		e.recordQueuedOutcome(final, decisionSent, "", "")
	case rpc.QueuedAuthCancelled:
		s.infof("queued authorisation %s cancelled while it was being placed; nothing was sent", id)
		e.recordQueuedOutcome(final, decisionCancelled, final.ReasonCode, final.Reason)
	case rpc.QueuedAuthHeld:
		s.infof("queued authorisation %s held after a refused send (%s): %s", id, final.HoldCode, final.HoldReason)
		e.recordQueuedOutcome(final, decisionHeld, final.HoldCode, final.HoldReason)
	default:
		s.warnf("queued authorisation %s for %s failed (%s): %s", id, terms.Key, final.ReasonCode, final.Reason)
		e.recordQueuedOutcome(final, decisionFailed, final.ReasonCode, final.Reason)
	}
}

// queuedSubmitBlockers is the proposal-side half of the origin gate: the
// row must be the one the executor holds a grant for, the record must still
// be waiting, and proposal submit must still be enabled.
func (e *proposalEngine) queuedSubmitBlockers(prop rpc.TradeProposal, rec *queuedAuthRecord) []rpc.TradingBlocker {
	unauthorised := []rpc.TradingBlocker{{Code: "daemon_origin_unauthorised", Message: "queued submission requires the executor's grant for this record",
		Action: "Queue the proposal through Desk, or submit it by hand."}}
	if e == nil || e.server == nil || rec == nil {
		return unauthorised
	}
	grant := e.server.queuedGrant.Load()
	current, ok := e.queued.get(rec.Terms.QueueID)
	if grant == nil || grant.QueueID != rec.Terms.QueueID || grant.Key != prop.Key || grant.Bucket != prop.Bucket || !ok || !current.waiting() {
		return unauthorised
	}
	if !e.server.cfg.AutoTrade.WithDefaults().FastPathEnabledResolved() {
		return []rpc.TradingBlocker{{Code: "fast_path_disabled", Message: "proposal submit is disabled by [auto_trade].fast_path_enabled"}}
	}
	return nil
}

// daemonOwnerQueuedOriginBlockers is the origin-specific policy for the
// executor's writes: accepted only while it holds a grant for one record that
// is still an authorised intent (waiting, or sending once its intent is
// persisted). Every other gate is evaluated by the same authorization as a
// human write.
func (s *Server) daemonOwnerQueuedOriginBlockers() []rpc.TradingBlocker {
	unauthorised := []rpc.TradingBlocker{{Code: "daemon_origin_unauthorised",
		Message: "daemon-owner-queued writes are issued only by the daemon's queued executor for an armed, owner-signed record",
		Action:  "Queue the proposal through Desk, or submit it by hand."}}
	if s == nil || s.tradeProposals == nil {
		return unauthorised
	}
	grant := s.queuedGrant.Load()
	if grant == nil {
		return unauthorised
	}
	rec, ok := s.tradeProposals.queued.get(grant.QueueID)
	if !ok || !rec.liveIntent() || rec.Terms.Key != grant.Key {
		return unauthorised
	}
	return nil
}

// queuedLiveIntentFor reports the key of an armed, held or sending record for
// one contract and side in the current broker scope.
func (e *proposalEngine) queuedLiveIntentFor(contractSide string) (string, bool) {
	rec, ok := e.queuedLiveRecordFor(contractSide)
	return rec.Terms.Key, ok
}

// queuedLiveRecordFor is the armed, held or sending record for one contract
// and side in the current broker scope.
func (e *proposalEngine) queuedLiveRecordFor(contractSide string) (queuedAuthRecord, bool) {
	if e == nil || contractSide == "" || !e.queued.attached() {
		return queuedAuthRecord{}, false
	}
	scope := e.currentScope()
	for _, rec := range e.queued.list() {
		if rec.liveIntent() && rec.ContractSide == contractSide && sameBrokerScope(rec.scope(), scope) {
			return rec, true
		}
	}
	return queuedAuthRecord{}, false
}

// queuedIntentGateBlockers refuses a preview, a prepared submit or a submit
// for a row whose exact contract and side a live queued authorisation covers,
// the row's own key included. Row generation still exempts the own key, so
// the executor's revalidation finds its row; the row's queued marker keeps it
// out of approvals. Only the executor's send of that very record passes,
// under its grant.
func (e *proposalEngine) queuedIntentGateBlockers(p rpc.TradeProposal, own *queuedAuthRecord) []rpc.TradingBlocker {
	rec, ok := e.queuedLiveRecordFor(sameContractSide(p))
	if !ok {
		return nil
	}
	if own != nil && own.Terms.QueueID == rec.Terms.QueueID && e.server != nil {
		if grant := e.server.queuedGrant.Load(); grant != nil && grant.QueueID == rec.Terms.QueueID {
			return nil
		}
	}
	return []rpc.TradingBlocker{{Code: queuedIntentExistsCode,
		Message: fmt.Sprintf("a queued authorisation (%s) for this contract and side is armed for the open; nothing else is sent beside it", rec.Terms.QueueID),
		Action:  "Cancel the queued authorisation first to act on this row now."}}
}

// recordQueuedOutcome writes one decision line for an executor outcome.
func (e *proposalEngine) recordQueuedOutcome(rec queuedAuthRecord, outcome, code, reason string) {
	e.recordDecision(proposalDecision{event: "queue_execute", prop: rpc.TradeProposal{Key: rec.Terms.Key, Bucket: rec.Terms.Bucket},
		rev: rec.Terms.RevisionAtQueue, accepted: true, accept: outcome, queue: rec.Terms.QueueID, code: code, note: reason,
		tokenID: rec.PreviewTokenID, orderRef: rec.OrderRef, mode: rec.Terms.AccountMode})
}
