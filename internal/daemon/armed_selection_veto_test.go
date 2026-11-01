package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestArmedSelectionMandateVetoDeadlineProtectsFirstOrder(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	later := rig.mandateProposal(automaticTestStockRow(), 10, 1, 2)
	earlier := rig.mandateProposal(automaticTestStockRowB(), 10, 2, 2)
	revision := rig.install(later, earlier)
	rig.engine.mu.Lock()
	rig.engine.snapshot.MandateReduction = &rpc.TradeProposalMandateStatus{State: rpc.MandateReductionReducing, Rows: 2}
	rig.engine.mu.Unlock()
	rig.engine.reconcileAutomatic(t.Context())
	firstDeadline, lastDeadline := rig.now.Add(35*time.Minute), rig.now.Add(45*time.Minute)
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		var events []automaticSubmissionEvent
		for _, r := range records {
			r.SubmitAt = lastDeadline
			if r.Key == earlier.Key {
				r.SubmitAt = firstDeadline
			}
			events = append(events, automaticSubmissionEvent{At: rig.now, Type: automaticEventRescheduled, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, SubmitAt: r.SubmitAt})
		}
		return events
	}); err != nil {
		t.Fatal(err)
	}
	view := rig.server.mandateReductionView()
	if view == nil || view.Pending != 2 || !view.VetoUntil.Equal(firstDeadline) {
		t.Fatalf("batch deadline must allow vetoing both orders before the first can leave: %+v", view)
	}
	if len(view.Orders) != 2 || !view.Orders[0].SubmitAt.Equal(lastDeadline) || !view.Orders[1].SubmitAt.Equal(firstDeadline) {
		t.Fatalf("individual deadlines were not preserved: %+v", view.Orders)
	}
	for _, order := range view.Orders {
		if order.NoticeID == "" || order.NoticeID != automaticRecordNoticeID(rig.record(order.Key, revision)) {
			t.Fatalf("mandate order did not carry its durable veto opportunity: %+v", order)
		}
	}
	// Once that order has left the pending set, the summary follows the next
	// live deadline rather than retaining a completed order's time.
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(earlier.Key, revision)]
		r.State, r.SubmittedAt, r.ResolvedAt = rpc.TradeProposalAutomaticSubmitted, rig.now, rig.now
		r.OrderRef = "synthetic-first-deadline-complete"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventSubmitted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode}}
	}); err != nil {
		t.Fatal(err)
	}
	view = rig.server.mandateReductionView()
	if view.Pending != 1 || !view.VetoUntil.Equal(lastDeadline) {
		t.Fatalf("completed order still determines pending deadline: %+v", view)
	}
}

// An order being placed is visible but no longer a veto opportunity. Keeping
// it in Pending after its NoticeID disappears would turn the monitor's
// subject-set fallback into a second urgent notice for an expired window.
func TestArmedSelectionSubmittingOrderLeavesVetoSummary(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	first := rig.mandateProposal(automaticTestStockRow(), 10, 1, 2)
	later := rig.mandateProposal(automaticTestStockRowB(), 10, 2, 2)
	revision := rig.install(first, later)
	snapshot := rig.engine.snapshotForRead(false, false)
	snapshot.MandateReduction = &rpc.TradeProposalMandateStatus{State: rpc.MandateReductionReducing, Rows: 2}
	if err := rig.engine.installSnapshot(snapshot, false); err != nil {
		t.Fatal(err)
	}
	rig.engine.reconcileAutomatic(t.Context())
	firstDeadline, lastDeadline := rig.now.Add(30*time.Minute), rig.now.Add(45*time.Minute)
	for _, p := range []rpc.TradeProposal{first, later} {
		rig.notice(rig.record(p.Key, revision))
	}
	rig.advance(31 * time.Minute)
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		a := records[automaticRecordKey(first.Key, revision)]
		a.State, a.SubmittingAt, a.PreviewTokenID = rpc.TradeProposalAutomaticSubmitting, rig.now, "synthetic-in-flight-first"
		b := records[automaticRecordKey(later.Key, revision)]
		b.State, b.DeferredAt, b.SubmitAt = rpc.TradeProposalAutomaticDeferred, rig.now, lastDeadline
		return []automaticSubmissionEvent{
			{At: rig.now, Type: automaticEventSubmitting, Key: a.Key, Revision: a.Revision, Bucket: a.Bucket, State: a.State, AccountID: a.AccountID, AccountMode: a.AccountMode},
			{At: rig.now, Type: automaticEventDeferred, Key: b.Key, Revision: b.Revision, Bucket: b.Bucket, State: b.State, AccountID: b.AccountID, AccountMode: b.AccountMode},
		}
	}); err != nil {
		t.Fatal(err)
	}
	view := rig.server.mandateReductionView()
	if view == nil || view.Pending != 1 || !view.VetoUntil.Equal(lastDeadline) {
		t.Errorf("in-flight order still changes the live veto count or deadline: %+v", view)
	}
	if view == nil || len(view.Orders) != 2 || view.Orders[0].Automatic != rpc.TradeProposalAutomaticSubmitting ||
		view.Orders[0].NoticeID != "" || !view.Orders[0].SubmitAt.Equal(firstDeadline) ||
		view.Orders[1].Automatic != rpc.TradeProposalAutomaticDeferred || view.Orders[1].NoticeID == "" {
		t.Fatalf("in-flight row or deferred veto opportunity was hidden: %+v", view)
	}
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(later.Key, revision)]
		r.State, r.SubmittingAt, r.PreviewTokenID = rpc.TradeProposalAutomaticSubmitting, rig.now, "synthetic-in-flight-later"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventSubmitting, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode}}
	}); err != nil {
		t.Fatal(err)
	}
	// Reopen the durable records and served snapshot. The empty set of veto
	// opportunities must not look like a legacy batch waiting to be noticed.
	rig.restart()
	view = rig.server.mandateReductionView()
	if view == nil || view.Pending != 0 || !view.VetoUntil.IsZero() || view.Rows != 2 || len(view.Orders) != 2 {
		t.Fatalf("all in-flight rows must stay visible without a veto summary after restart: %+v", view)
	}
	for _, order := range view.Orders {
		if order.Automatic != rpc.TradeProposalAutomaticSubmitting || order.NoticeID != "" || order.SubmitAt.IsZero() {
			t.Fatalf("in-flight state was replaced or its timing was lost: %+v", order)
		}
	}
}
