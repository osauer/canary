//go:build trading

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Trading-build witnesses of the #41 review of queued authorisations.

// While a queue is armed, a manual submit or preview of the very row it
// covers is refused before any broker frame; the executor still sends it
// once in its window.
func TestQueuedRowRefusesEveryOtherSubmitWhileLive(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	prop, id := queueArmedTrim(t, rig, 10)
	rig.now = queueWindowOpen
	revision := rig.install(prop)
	ctx := context.Background()
	sub, err := rig.engine.Submit(ctx, rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || sub.Accepted || len(sub.Blockers) == 0 || sub.Blockers[0].Code != queuedIntentExistsCode {
		t.Fatalf("manual submit of the queued row = accepted %v blockers %+v err %v", sub.Accepted, sub.Blockers, err)
	}
	pre, err := rig.engine.Preview(ctx, rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: revision})
	if err != nil || len(pre.Blockers) == 0 || pre.Blockers[0].Code != queuedIntentExistsCode {
		t.Fatalf("preview of the queued row = blockers %+v err %v", pre.Blockers, err)
	}
	if broker.count() != 0 {
		t.Fatalf("broker calls before the executor = %d", broker.count())
	}
	rig.queueCycle()
	if rec := rig.queued(id); broker.count() != 1 || rec.State != rpc.QueuedAuthSent {
		t.Fatalf("executor: %d broker calls, record %+v", broker.count(), rec)
	}
}

// An owner cancel that lands while the order is being placed ends the record
// cancelled when the attempt proves unsent (here the freeze stops it at the
// wire), and nothing is sent later.
func TestQueuedCancelDuringTheSendEndsCancelledWhenUnsent(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	var during rpc.TradeProposalQueueResult
	rig.server.orderWriteBeforeBrokerSend = func() {
		var err error
		during, err = rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: id, Origin: rpc.OrderOriginHumanTTY})
		if err != nil {
			t.Errorf("cancel during the send: %v", err)
		}
		rig.server.platformSettings = frozenPlatformSettings()
	}
	rig.now = queueWindowOpen
	rig.queueCycle()
	if during.Accepted || len(during.Blockers) == 0 || during.Blockers[0].Code != "queued_sending" || during.Queue == nil || !during.Queue.CancelRequested {
		t.Fatalf("cancel during the send = %+v", during)
	}
	rec := rig.queued(id)
	if broker.count() != 0 || rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonOwnerCancelled {
		t.Fatalf("after an unsent attempt with a cancel request: %d calls, record %+v", broker.count(), rec)
	}
	rig.server.orderWriteBeforeBrokerSend = nil
	rig.server.platformSettings = nil
	rig.advance(time.Minute)
	rig.queueCycle()
	if broker.count() != 0 {
		t.Fatalf("a cancelled record sent later: %d calls", broker.count())
	}
}

// The window is checked again when the intent is staged: a send whose
// preview ran past not_after never reaches the broker, and the record then
// expires.
func TestQueuedSendStaysInsideItsWindow(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	end := rig.queued(id).Terms.NotAfter
	impact := rig.server.orderPreviewPositionImpact
	rig.server.orderPreviewPositionImpact = func(ctx context.Context, c rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		rig.now = end
		return impact(ctx, c, action, qty)
	}
	rig.now = end.Add(-time.Second)
	rig.queueCycle()
	if rec := rig.queued(id); broker.count() != 0 || rec.State == rpc.QueuedAuthSending || rec.State == rpc.QueuedAuthSent {
		t.Fatalf("a send staged at the window's end: %d calls, record %+v", broker.count(), rec)
	}
	rig.queueCycle()
	if rec := rig.queued(id); rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonWindowEnded || broker.count() != 0 {
		t.Fatalf("after the window: %d calls, record %+v", broker.count(), rec)
	}
}

// A record this process left sending because its outcome write failed is
// resolved from the order journal once stale, without a restart, and never
// resent.
func TestQueuedStaleSendingInProcessIsResolvedFromTheJournal(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	_, id := queueArmedTrim(t, rig, 10)
	rig.now = queueWindowOpen
	rig.queueCycle()
	sent := rig.queued(id)
	rewindQueuedToSending(t, rig, id)
	rig.queueCycle()
	if rec := rig.queued(id); rec.State != rpc.QueuedAuthSending {
		t.Fatalf("a fresh sending record was resolved early: %+v", rec)
	}
	rig.advance(queuedSendingStale + time.Second)
	rig.queueCycle()
	if rec := rig.queued(id); rec.State != rpc.QueuedAuthSent || rec.OrderRef != sent.OrderRef || broker.count() != 1 {
		t.Fatalf("stale in-process sending: record %+v, %d calls", rec, broker.count())
	}
}

// A preparation made before the queue was armed cannot be redeemed beside
// it: the prepared submit is refused, and only the executor sends.
func TestQueuedRowRefusesAnEarlierPreparation(t *testing.T) {
	t.Parallel()
	rig, broker := newQueueTradingRig(t)
	prop := rig.trimProposal(10)
	rig.now = time.Date(2026, 9, 28, 13, 31, 0, 0, time.UTC)
	revision := rig.install(prop)
	prepared, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: revision, FastPath: true})
	if err != nil || !prepared.Accepted || prepared.PreparedRef == "" {
		t.Fatalf("immediate preparation in the opening window: accepted=%v blockers=%+v err=%v", prepared.Accepted, prepared.Blockers, err)
	}
	id := rig.queueAndArm(prop, revision)
	out := preparedSubmit(t, rig, prepared)
	if out.Accepted || len(out.Blockers) == 0 || out.Blockers[0].Code != queuedIntentExistsCode || broker.count() != 0 {
		t.Fatalf("earlier preparation redeemed beside the queue: accepted=%v blockers=%+v calls=%d", out.Accepted, out.Blockers, broker.count())
	}
	rig.now = queueWindowOpen
	rig.queueCycle()
	if rec := rig.queued(id); broker.count() != 1 || rec.State != rpc.QueuedAuthSent {
		t.Fatalf("executor: %d broker calls, record %+v", broker.count(), rec)
	}
}
