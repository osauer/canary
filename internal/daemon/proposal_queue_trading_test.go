//go:build trading

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// queueWindowOpen is 13:35Z on the fixture day: the US open plus a stock's
// five-minute offset, when a queued trim may first send.
var queueWindowOpen = time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC)

// newQueueTradingRig is the trading-build rig at the 15:19 CEST fixture: real
// preview tokens signed on the rig clock, a real order journal in daemon.db,
// the embedded calendar and a fake broker socket. The previewed position is
// 40 shares, reduced by a sale.
func newQueueTradingRig(t *testing.T) (*automaticTestRig, *brokerCallLog) {
	t.Helper()
	rig := newAutomaticTradingRig(t, "")
	rig.now = queuedFixtureNow
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "queue-preview-key"), func() time.Time { return rig.now })
	if err != nil {
		t.Fatal(err)
	}
	head, err := rig.core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.bindAuthority(head.AuthorityEpoch, head.SignerGeneration); err != nil {
		t.Fatal(err)
	}
	rig.server.orderTokens = signer
	useQueueCalendar(rig.server)
	rig.server.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, qty int) (rpc.OrderPositionImpact, error) {
		after := 40 - float64(qty)
		effect := rpc.OrderPositionEffectReduce
		if after == 0 {
			effect = rpc.OrderPositionEffectClose
		}
		return rpc.OrderPositionImpact{Before: 40, After: after, Effect: effect}, nil
	}
	broker := &brokerCallLog{}
	broker.install(rig.server)
	return rig, broker
}

// queueArmedTrim prepares and arms a trim of qty at the fixture time.
func queueArmedTrim(t *testing.T, rig *automaticTestRig, qty int) (rpc.TradeProposal, string) {
	t.Helper()
	prop := rig.trimProposal(qty)
	return prop, rig.queueAndArm(prop, rig.install(prop))
}

func sentOrigins(t *testing.T, rig *automaticTestRig, tokenID string) []string {
	t.Helper()
	var out []string
	for _, ev := range journalEventsForToken(t, rig.server, tokenID) {
		if ev.Type == orderJournalEventSendAttempted {
			out = append(out, ev.Origin)
		}
	}
	return out
}

// The witness: an armed trim makes no broker frame before its window, then
// exactly one DAY limit halfway from the mid to the bid, journaled under the
// daemon-owner-queued origin, and nothing more however often the executor
// runs or restarts.
func TestQueuedSendsOnceInsideItsWindow(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	prop, id := queueArmedTrim(t, rig, 10)
	for _, at := range []time.Duration{0, 5 * time.Minute, 15*time.Minute + 59*time.Second} {
		rig.now = queuedFixtureNow.Add(at)
		rig.queueCycle()
	}
	if broker.count() != 0 || rig.queuedRefreshes != 0 {
		t.Fatalf("before the window: %d broker calls, %d revalidations", broker.count(), rig.queuedRefreshes)
	}
	rig.now = queueWindowOpen
	rig.queueCycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls in the window = %d, want 1; record = %+v", broker.count(), rig.queued(id))
	}
	order := broker.orders[0]
	if !strings.EqualFold(order.Action, rpc.OrderActionSell) || order.TotalQty != 10 || order.OrderType != rpc.OrderTypeLMT || order.LmtPrice != 24.95 || order.TIF != rpc.OrderTIFDay {
		t.Fatalf("broker order = %+v, want SELL 10 LMT 24.95 DAY", order)
	}
	rec := rig.queued(id)
	if rec.State != rpc.QueuedAuthSent || rec.OrderRef == "" || rec.PreviewTokenID == "" || rec.QuantitySent != 10 || rec.LimitPrice != 24.95 || rec.Late ||
		rec.SendQuote == nil || rec.SendQuote.SpreadPct == nil || rec.SentAt.IsZero() {
		t.Fatalf("sent record = %+v", rec)
	}
	if got := sentOrigins(t, rig, rec.PreviewTokenID); len(got) != 1 || got[0] != rpc.OrderOriginDaemonOwnerQueued {
		t.Fatalf("journaled send origins = %v", got)
	}
	if got := strings.Join(queuedEventTypes(rig.queuedEvents(id)), ","); got != strings.Join([]string{queuedEventPrepared, queuedEventArmed, queuedEventSending, queuedEventSent}, ",") {
		t.Fatalf("events = %s", got)
	}
	if rig.server.queuedGrant.Load() != nil {
		t.Fatal("the executor left its grant installed")
	}
	rig.install(prop)
	for range 3 {
		rig.advance(time.Minute)
		rig.queueCycle()
	}
	rig.restart()
	rig.advance(time.Minute)
	rig.queueCycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after more cycles and a restart = %d, want still 1", broker.count())
	}
}

// An unarmed preparation carries no authority: no broker frame inside its
// open window, however close its arm deadline, then expiry.
func TestQueuedUnarmedRecordSendsNothing(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	// Prepared in the opening window: the send window opens at 13:35Z while
	// the arm deadline is still 13:43Z away.
	rig.now = time.Date(2026, 9, 28, 13, 33, 0, 0, time.UTC)
	prop := rig.trimProposal(10)
	p := rig.queuePrepare(prop, rig.install(prop))
	if p.Readiness.Code != rpc.ReadinessOpeningWindow || !p.Queue.Terms.NotBefore.Equal(queueWindowOpen) {
		t.Fatalf("opening-window prepare: readiness %+v, window from %s", p.Readiness, p.Queue.Terms.NotBefore)
	}
	rig.now = queueWindowOpen.Add(time.Minute)
	rig.queueCycle()
	if rec := rig.queued(p.Queue.Terms.QueueID); broker.count() != 0 || rig.queuedRefreshes != 0 || rec.State != rpc.QueuedAuthPrepared {
		t.Fatalf("unarmed record in its window: %d broker calls, %d revalidations, state %s", broker.count(), rig.queuedRefreshes, rec.State)
	}
	rig.now = p.Queue.Terms.ArmDeadline
	rig.queueCycle()
	if broker.count() != 0 {
		t.Fatalf("unarmed record: %d broker calls", broker.count())
	}
	if rec := rig.queued(p.Queue.Terms.QueueID); rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonNotArmed {
		t.Fatalf("unarmed record = %+v", rec)
	}
}

// Revision churn at the open never cancels: the send revalidates by key and
// row terms, and sends at most the signed maximum, never more than the row
// asks for now.
func TestQueuedRevisionChurnStillSendsWithinTheSignedMaximum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		rowNow int
		want   int
	}{
		{"the row asks for less", 8, 8},
		{"the row asks for more", 14, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, broker := newQueueTradingRig(t)
			prop, id := queueArmedTrim(t, rig, 10)
			before := rig.engine.Snapshot(false).Revision
			now := rig.trimProposal(tc.rowNow)
			extra := rig.reductionRow(rpc.TradeProposalBucketRiskReduction, queueTestOptionRow(), 1)
			if after := rig.install(now, extra); after == before || now.Key != prop.Key {
				t.Fatalf("the snapshot did not churn: %s", after)
			}
			rig.now = queueWindowOpen
			rig.queueCycle()
			if broker.count() != 1 || broker.orders[0].TotalQty != tc.want {
				t.Fatalf("broker orders = %+v, want one of %v; record = %+v", broker.orders, tc.want, rig.queued(id))
			}
		})
	}
}

// Concurrent executor ticks place exactly one order.
func TestQueuedConcurrentTicksSendExactlyOnce(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	rig.now = queueWindowOpen
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { rig.engine.runQueuedCycle(context.Background()) })
	}
	wg.Wait()
	if broker.count() != 1 || rig.queued(id).State != rpc.QueuedAuthSent {
		t.Fatalf("broker calls = %d, record %s", broker.count(), rig.queued(id).State)
	}
}

// A lost broker reply is never resent: the attempt is staged in the journal,
// so the outcome is unclear and the record fails.
func TestQueuedLostBrokerReplyIsNeverResent(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	calls := 0
	rig.server.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		calls++
		return ibkrlib.WithSendDisposition(errors.New("socket reset after write"), ibkrlib.SendDispositionMayHaveWritten)
	}
	_, id := queueArmedTrim(t, rig, 10)
	rig.now = queueWindowOpen
	rig.queueCycle()
	rec := rig.queued(id)
	if rec.State != rpc.QueuedAuthFailed || rec.ReasonCode != queuedReasonSendUnclear || calls != 1 {
		t.Fatalf("record after a lost reply = %+v, %d calls", rec, calls)
	}
	rig.advance(5 * time.Minute)
	rig.queueCycle()
	if calls != 1 || broker.count() != 0 {
		t.Fatalf("broker calls after the unclear send = %d", calls)
	}
}

// A freeze that lands between admission and the wire refuses before the
// first frame: the record holds, then sends once with a fresh token.
func TestQueuedFreezeAtTheWireHoldsThenSendsOnce(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	armedFreeze := true
	rig.server.orderWriteBeforeBrokerSend = func() {
		if armedFreeze {
			armedFreeze = false
			rig.server.platformSettings = frozenPlatformSettings()
		}
	}
	rig.now = queueWindowOpen
	rig.queueCycle()
	rec := rig.queued(id)
	if broker.count() != 0 || rec.State != rpc.QueuedAuthHeld || rec.HoldCode != rpc.ReadinessTradingFrozen || rec.PreviewTokenID != "" {
		t.Fatalf("after a freeze at the wire: %d calls, record %+v", broker.count(), rec)
	}
	rig.queueCycle()
	if broker.count() != 0 {
		t.Fatal("sent while frozen")
	}
	rig.server.platformSettings = nil
	rig.advance(time.Minute)
	rig.queueCycle()
	rec = rig.queued(id)
	if broker.count() != 1 || rec.State != rpc.QueuedAuthSent || rec.PreviewTokenID == "" {
		t.Fatalf("after the freeze lifted: %d calls, record %+v", broker.count(), rec)
	}
	types := queuedEventTypes(rig.queuedEvents(id))
	if strings.Join(types, ",") != strings.Join([]string{queuedEventPrepared, queuedEventArmed, queuedEventSending, queuedEventHeld, queuedEventResumed, queuedEventSending, queuedEventSent}, ",") {
		t.Fatalf("events = %v", types)
	}
}

// Restart at the sending state: the order journal decides, and nothing is
// ever sent twice.
func TestQueuedRestartInSendingIsResolvedFromTheJournal(t *testing.T) {
	t.Parallel()
	t.Run("the order reached the broker", func(t *testing.T) {
		t.Parallel()
		rig, broker := newQueueTradingRig(t)
		_, id := queueArmedTrim(t, rig, 10)
		rig.now = queueWindowOpen
		rig.queueCycle()
		sent := rig.queued(id)
		// The outcome write was lost: the record still says sending.
		rewindQueuedToSending(t, rig, id)
		rig.advance(time.Minute)
		rig.restart()
		rig.queueCycle()
		rec := rig.queued(id)
		if rec.State != rpc.QueuedAuthSent || rec.OrderRef != sent.OrderRef || broker.count() != 1 {
			t.Fatalf("recovered record = %+v, %d calls", rec, broker.count())
		}
	})
	t.Run("the attempt was staged but never answered", func(t *testing.T) {
		t.Parallel()
		rig, _ := newQueueTradingRig(t)
		release, reached := make(chan struct{}), make(chan struct{})
		var mu sync.Mutex
		calls := 0
		rig.server.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				close(reached)
				<-release
			}
			return nil
		}
		_, id := queueArmedTrim(t, rig, 10)
		rig.now = queueWindowOpen
		first := rig.engine
		done := make(chan struct{})
		go func() {
			defer close(done)
			first.runQueuedCycle(context.Background())
		}()
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			close(release)
			<-done
			t.Fatal("the first process never reached the broker")
		}
		rig.advance(time.Minute)
		rig.restart()
		rig.queueCycle()
		rec := rig.queued(id)
		if rec.State != rpc.QueuedAuthFailed || rec.ReasonCode != queuedReasonSendUnclear {
			t.Fatalf("recovered record = %+v", rec)
		}
		close(release)
		<-done
		rig.advance(time.Minute)
		rig.queueCycle()
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("broker calls across the restart = %d, want 1", calls)
		}
		if final := rig.queued(id); final.State != rpc.QueuedAuthFailed {
			t.Fatalf("the first process overwrote the recovered outcome: %+v", final)
		}
	})
	t.Run("the process died before the broker call", func(t *testing.T) {
		t.Parallel()
		rig, broker := newQueueTradingRig(t)
		_, id := queueArmedTrim(t, rig, 10)
		rig.now = queueWindowOpen
		// The intent was persisted, then the process died: no journal
		// trace names the token.
		err := rig.engine.queued.update(context.Background(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
			r := records[id]
			r.SendingAt, r.PreviewTokenID, r.OrderRef = rig.now, "token-never-staged", "ref-never-staged"
			return []queuedAuthEvent{r.transition(queuedEventSending, rpc.QueuedAuthSending, rig.now)}
		})
		if err != nil {
			t.Fatal(err)
		}
		rig.advance(5 * time.Minute)
		rig.restart()
		rig.queueCycle()
		rec := rig.queued(id)
		if broker.count() != 1 || rec.State != rpc.QueuedAuthSent || !rec.Late || rec.PreviewTokenID == "token-never-staged" {
			t.Fatalf("after a restart before the send: %d calls, record %+v", broker.count(), rec)
		}
		recovered := false
		for _, ev := range rig.queuedEvents(id) {
			if ev.Type == queuedEventRecovered && ev.ReasonCode == queuedReasonRestartBeforeSend && ev.StateTo == rpc.QueuedAuthArmed {
				recovered = true
			}
		}
		if !recovered {
			t.Fatalf("no recovery event: %v", queuedEventTypes(rig.queuedEvents(id)))
		}
	})
}

func rewindQueuedToSending(t *testing.T, rig *automaticTestRig, id string) {
	t.Helper()
	err := rig.engine.queued.update(context.Background(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[id]
		r.SentAt = time.Time{}
		return []queuedAuthEvent{r.transition(queuedEventSending, rpc.QueuedAuthSending, rig.now)}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The origin gate accepts daemon-owner-queued only under the executor's
// grant for a live record; any other caller claiming it is refused.
func TestQueuedOriginNeedsTheExecutorsGrant(t *testing.T) {
	t.Parallel()
	rig, _ := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	status := rig.server.currentTradingStatus()
	refused := func(when string) {
		t.Helper()
		if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonOwnerQueued); len(blockers) != 1 || blockers[0].Code != "daemon_origin_unauthorised" {
			t.Fatalf("%s: origin blockers = %+v", when, blockers)
		}
	}
	refused("no grant")
	rig.server.queuedGrant.Store(&queuedWriteGrant{QueueID: "unknown", Key: "k"})
	refused("a grant for no record")
	rec := rig.queued(id)
	rig.server.queuedGrant.Store(&queuedWriteGrant{QueueID: id, Key: rec.Terms.Key, Bucket: rec.Terms.Bucket})
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonOwnerQueued); len(blockers) != 0 {
		t.Fatalf("grant for an armed record: %+v", blockers)
	}
	if _, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: id}); err != nil {
		t.Fatal(err)
	}
	refused("a grant for a cancelled record")
	rig.server.queuedGrant.Store(nil)
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, rpc.OrderOriginAgent} {
		if blockers := rig.server.brokerWriteOriginBlockers(status, origin); len(blockers) != 0 {
			t.Fatalf("origin %q blockers = %+v", origin, blockers)
		}
	}
}

// Price and WhatIf at the send: a limit beyond the worst price holds, a
// WhatIf the broker never answered holds, a WhatIf rejection cancels, and a
// spread over the policy's limit holds, none of them with a broker frame.
func TestQueuedSendHoldsOrCancelsOnPriceAndWhatIf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(rig *automaticTestRig)
		state  string
		code   string
	}{
		{"the bid fell through the worst price", func(rig *automaticTestRig) { rig.server.orderPreviewQuote = fixedPreviewQuote(10, 10.1) }, rpc.QueuedAuthHeld, queuedHoldWorstPrice},
		{"the spread is over the stock limit", func(rig *automaticTestRig) { rig.server.orderPreviewQuote = fixedPreviewQuote(24, 25) }, rpc.QueuedAuthHeld, rpc.ReadinessSpreadTooWide},
		{"WhatIf unanswered", func(rig *automaticTestRig) {
			rig.server.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable}, nil
			}
		}, rpc.QueuedAuthHeld, rpc.ReadinessBrokerUnavailable},
		{"WhatIf rejected", func(rig *automaticTestRig) {
			rig.server.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "margin"}, nil
			}
		}, rpc.QueuedAuthCancelled, queuedReasonWhatIfRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, broker := newQueueTradingRig(t)
			_, id := queueArmedTrim(t, rig, 10)
			tc.change(rig)
			rig.now = queueWindowOpen
			rig.queueCycle()
			rec := rig.queued(id)
			if broker.count() != 0 || rec.State != tc.state || rec.HoldCode != tc.code && rec.ReasonCode != tc.code {
				t.Fatalf("%d calls, record state %s hold %q reason %q (%s)", broker.count(), rec.State, rec.HoldCode, rec.ReasonCode, nonEmptyString(rec.HoldReason, rec.Reason))
			}
		})
	}
}

// A hand order placed after the arm holds the send until it settles; the
// send then goes out once.
func TestQueuedHandOrderHoldsUntilItSettles(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	rig.freshOrders = []ibkrlib.OrderLifecycleEvent{handOrder(777)}
	rig.now = queueWindowOpen
	rig.queueCycle()
	if rec := rig.queued(id); broker.count() != 0 || rec.State != rpc.QueuedAuthHeld || rec.HoldCode != queuedHoldHandOrder {
		t.Fatalf("behind a hand order: %d calls, record %+v", broker.count(), rec)
	}
	rig.freshOrders = nil
	rig.advance(time.Minute)
	rig.queueCycle()
	if rec := rig.queued(id); broker.count() != 1 || rec.State != rpc.QueuedAuthSent {
		t.Fatalf("after the hand order settled: %d calls, record %+v", broker.count(), rec)
	}
}

// A sent order is followed to its fill in the order journal.
func TestQueuedFollowsTheOrderToItsFill(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		filled float64
		status string
		state  string
	}{
		{"filled", 10, "Filled", rpc.QueuedAuthFilled},
		{"partly filled at the close", 4, "Cancelled", rpc.QueuedAuthPartiallyFilled},
		{"unfilled at the close", 0, "Cancelled", rpc.QueuedAuthExpiredUnfilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, _ := newQueueTradingRig(t)
			_, id := queueArmedTrim(t, rig, 10)
			rig.now = queueWindowOpen
			rig.queueCycle()
			rec := rig.queued(id)
			var attempt orderJournalEvent
			for _, ev := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
				if ev.Type == orderJournalEventSendAttempted {
					attempt = ev
				}
			}
			status := attempt
			status.At, status.Type, status.PermID, status.Status = rig.now.Add(time.Minute), orderJournalEventStatusUpdated, 9001, tc.status
			status.Filled, status.Remaining, status.AvgFillPrice = tc.filled, 10-tc.filled, 24.95
			status.AttemptID, status.ActionKind = "", ""
			if err := rig.server.orderJournal.AppendAll([]orderJournalEvent{status}); err != nil {
				t.Fatal(err)
			}
			rig.advance(2 * time.Minute)
			rig.queueCycle()
			rec = rig.queued(id)
			if rec.State != tc.state || rec.FilledQuantity != tc.filled || rec.PermID != 9001 {
				t.Fatalf("followed record = state %s filled %v perm %d, want %s %v", rec.State, rec.FilledQuantity, rec.PermID, tc.state, tc.filled)
			}
		})
	}
}
