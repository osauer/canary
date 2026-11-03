package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	"sync/atomic"
	"testing"
	"time"
)

func TestConditionalVetoWireMethodUsesTheCanonicalVetoAndRequiresItsWindow(t *testing.T) {
	for _, name := range []string{"complete", "identity missing", "deadline missing", "both missing", "agent origin"} {
		t.Run(name, func(t *testing.T) {
			rig, rec, request := conditionalVetoFixture(t)
			switch name {
			case "identity missing":
				request.NoticeID = ""
			case "deadline missing":
				request.VetoUntil = time.Time{}
			case "both missing":
				request.NoticeID, request.VetoUntil = "", time.Time{}
			case "agent origin":
				request.Origin = rpc.OrderOriginAgent
			}
			params, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			rig.server.dispatch(t.Context(), &rpc.Request{ID: "synthetic-veto", Method: rpc.MethodTradeProposalsVetoWindow, Params: params}, json.NewEncoder(&output), nil)
			var response rpc.Response
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if name == "complete" {
				var result rpc.TradeProposalVetoResult
				if err := json.Unmarshal(response.Result, &result); err != nil || !response.Ok || !result.Accepted ||
					rig.record(rec.Key, rec.Revision).State != rpc.TradeProposalAutomaticVetoed {
					t.Fatalf("conditional method did not execute the canonical veto: %s", output.Bytes())
				}
			} else if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeBadRequest ||
				rig.record(rec.Key, rec.Revision).State != rpc.TradeProposalAutomaticPending {
				t.Fatalf("conditional method accepted incomplete authority or bypassed the human gate: %s", output.Bytes())
			}
		})
	}
}

func conditionalVetoFixture(t *testing.T) (*automaticTestRig, automaticSubmissionRecord, rpc.TradeProposalVetoParams) {
	t.Helper()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	rig.notice(rec)
	rec = rig.record(prop.Key, revision)
	return rig, rec, rpc.TradeProposalVetoParams{Key: rec.Key, Revision: rec.Revision,
		Origin: rpc.OrderOriginPairedDevice, NoticeID: automaticRecordNoticeID(rec), VetoUntil: rec.SubmitAt}
}

func TestConditionalVetoRequiresCompleteWindowAndCannotExtendIt(t *testing.T) {
	for _, name := range []string{"identity missing", "deadline missing", "deadline extended", "identity different", "agent origin"} {
		t.Run(name, func(t *testing.T) {
			rig, rec, request := conditionalVetoFixture(t)
			switch name {
			case "identity missing":
				request.NoticeID = ""
			case "deadline missing":
				request.VetoUntil = time.Time{}
			case "deadline extended":
				request.VetoUntil = request.VetoUntil.Add(time.Second)
			case "identity different":
				request.NoticeID = "another-notice-window"
			case "agent origin":
				request.Origin = rpc.OrderOriginAgent
			}
			result, err := rig.engine.Veto(t.Context(), request)
			if result.Accepted || rig.record(rec.Key, rec.Revision).State != rpc.TradeProposalAutomaticPending ||
				rig.eventCount(rec.Key, rec.Revision, automaticEventVetoed) != 0 {
				t.Fatalf("invalid notification authority changed the record: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestConditionalVetoExpiryUsesTheDaemonClockAfterWaiting(t *testing.T) {
	rig, rec, request := conditionalVetoFixture(t)
	var clock atomic.Int64
	clock.Store(rig.now.UnixNano())
	var calls atomic.Int32
	started := make(chan struct{})
	rig.engine.now = func() time.Time {
		now := time.Unix(0, clock.Load())
		if calls.Add(1) == 1 {
			close(started)
		}
		return now
	}
	rig.engine.automatic.mu.Lock()
	done := make(chan rpc.TradeProposalVetoResult, 1)
	go func() {
		result, _ := rig.engine.Veto(t.Context(), request)
		done <- result
	}()
	<-started
	clock.Store(request.VetoUntil.UnixNano())
	rig.engine.automatic.mu.Unlock()
	select {
	case result := <-done:
		if result.Accepted || rig.record(rec.Key, rec.Revision).State != rpc.TradeProposalAutomaticPending {
			t.Fatalf("request valid at entry crossed its deadline while waiting and still vetoed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("conditional veto did not complete after the store was released")
	}
}

func TestConditionalVetoRetryAcknowledgesOnlyTheSameStoredWindow(t *testing.T) {
	rig, rec, request := conditionalVetoFixture(t)
	// A batch's earliest deadline may precede this row's own deadline.
	request.VetoUntil = request.VetoUntil.Add(-time.Minute)
	result, err := rig.engine.Veto(t.Context(), request)
	if err != nil || !result.Accepted {
		t.Fatalf("current notification veto refused: %+v %v", result, err)
	}
	rig.advance(time.Hour)
	rig.restart()
	result, err = rig.engine.Veto(t.Context(), request)
	if err != nil || !result.Accepted || result.State != rpc.TradeProposalAutomaticVetoed ||
		result.Automatic == nil || result.Automatic.NoticeID != "" ||
		rig.eventCount(rec.Key, rec.Revision, automaticEventVetoed) != 1 {
		t.Fatalf("exact retry must acknowledge the durable veto without a new event or terminal notice: %+v %v", result, err)
	}
	request.NoticeID = "different-stored-window"
	if result, err = rig.engine.Veto(t.Context(), request); err != nil || result.Accepted {
		t.Fatalf("another window was acknowledged as this veto: %+v %v", result, err)
	}
}

func TestConditionalVetoCannotCreateAnUnnoticedHold(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revision := rig.install(prop)
	result, err := rig.engine.Veto(t.Context(), rpc.TradeProposalVetoParams{Key: prop.Key, Revision: revision,
		Origin: rpc.OrderOriginPairedDevice, NoticeID: "absent-window", VetoUntil: rig.now.Add(time.Minute)})
	if err != nil || result.Accepted {
		t.Fatalf("a notification without a current record created a hold: %+v %v", result, err)
	}
	rig.noRecord(prop.Key, revision)
}

func TestUnboundVetoRetainsTheFreshConsoleBehavior(t *testing.T) {
	rig, rec, request := conditionalVetoFixture(t)
	rig.advance(time.Hour)
	request.NoticeID, request.VetoUntil = "", time.Time{}
	result, err := rig.engine.Veto(t.Context(), request)
	if err != nil || !result.Accepted || rig.record(rec.Key, rec.Revision).State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("the owner's fresh console veto was changed by notification fencing: %+v %v", result, err)
	}
}

func TestConditionalVetoRejectsRenewedSameRevisionBeforeOldDeadline(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	original := rig.record(prop.Key, revision)
	rig.notice(original)
	original = rig.record(prop.Key, revision)
	oldID := automaticRecordNoticeID(original)
	if oldID == "" || !rig.now.Before(original.SubmitAt) {
		t.Fatalf("original notice not vetoable: %+v", original)
	}
	// The client has read and matched this exact notice and deadline. Before
	// its key/revision-only RPC reaches Canary, the owner ignores the row and
	// takes it back: the row returns at the same revision with a fresh
	// window, which the prior notification must not veto.
	rig.engine.mu.Lock()
	rig.engine.ignored[scopedIgnoreKey(rig.scope, prop.Key)] = struct{}{}
	rig.engine.mu.Unlock()
	rig.advance(time.Second)
	rig.install()
	rig.cycle()
	if got := rig.record(prop.Key, revision); got.State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatalf("ignored record was not superseded: %+v", got)
	}
	rig.engine.mu.Lock()
	delete(rig.engine.ignored, scopedIgnoreKey(rig.scope, prop.Key))
	rig.engine.mu.Unlock()
	rig.advance(time.Second)
	if got := rig.install(prop); got != revision {
		t.Fatalf("the row taken back changed its revision")
	}
	rig.cycle()
	renewed := rig.record(prop.Key, revision)
	newID := automaticRecordNoticeID(renewed)
	if renewed.State != rpc.TradeProposalAutomaticPending || oldID == newID || newID == "" ||
		!renewed.CreatedAt.After(original.CreatedAt) || !rig.now.Before(original.SubmitAt) {
		t.Fatalf("did not produce a new opportunity before the old deadline: original=%+v renewed=%+v", original, renewed)
	}
	result, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{
		Key: original.Key, Revision: original.Revision, Origin: rpc.OrderOriginPairedDevice,
		Reason:   "owner acted on the prior notification",
		NoticeID: oldID, VetoUntil: original.SubmitAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || rig.record(prop.Key, revision).State == rpc.TradeProposalAutomaticVetoed {
		t.Fatal("prior notification vetoed a different notice window; the old deadline did not fence renewal")
	}
}

// A transient blocker that comes and goes within the breach keeps the
// window the owner was notified of (2026-11-03 carry rule): the notice and
// its deadline are unchanged, so the owner's veto of it stops the row.
func TestConditionalVetoHoldsAcrossATransientBlock(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	original := rig.record(prop.Key, revision)
	oldID := automaticRecordNoticeID(original)
	blocked := prop
	blocked.State = rpc.TradeProposalStateBlocked
	blocked.Blockers = []rpc.TradingBlocker{{Code: "quote_stale", Message: "synthetic temporary quote gap"}}
	rig.advance(time.Second)
	if got := rig.install(blocked); got != revision {
		t.Fatalf("test blocker changed the proposal revision")
	}
	rig.cycle()
	rig.advance(time.Second)
	rig.install(prop)
	rig.cycle()
	back := rig.record(prop.Key, revision)
	if back.State != rpc.TradeProposalAutomaticPending || automaticRecordNoticeID(back) != oldID || !back.SubmitAt.Equal(original.SubmitAt) {
		t.Fatalf("a transient block changed the notified window: original=%+v back=%+v", original, back)
	}
	result, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{
		Key: original.Key, Revision: original.Revision, Origin: rpc.OrderOriginPairedDevice,
		Reason:   "owner acted on the notification",
		NoticeID: oldID, VetoUntil: original.SubmitAt,
	})
	if err != nil || !result.Accepted || rig.record(prop.Key, revision).State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("the owner's veto of the notified window did not stop the row: %+v %v", result, err)
	}
}
