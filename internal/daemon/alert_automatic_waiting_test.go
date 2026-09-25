package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A pre-authorised record that still waits, deferred by trading.freeze or
// held by the settling rule, keeps its Protection notice open under the same
// occurrence with copy that names the wait. It never reads as recovered;
// only leaving the waiting set (submitted, vetoed, superseded) recovers it.
func TestAutomaticWaitingRecordsKeepTheirNoticeOpen(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	// mutate moves the record the way the scheduler does: a state change is
	// persisted together with its journal event.
	mutate := func(eventType string, apply func(*automaticSubmissionRecord)) {
		t.Helper()
		if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			r := records[automaticRecordKey(prop.Key, revision)]
			apply(r)
			return []automaticSubmissionEvent{{At: rig.now, Type: eventType, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, Reason: r.HoldReason}}
		}); err != nil {
			t.Fatal(err)
		}
	}
	notices := func() []automaticNoticeKey {
		t.Helper()
		got := rig.engine.automaticPendingNotices(rig.scope)
		if len(got) != 1 || got[0].Key != prop.Key || got[0].Revision != revision {
			t.Fatalf("waiting record left the notice list: %+v", got)
		}
		return got
	}

	pending := notices()
	if pending[0].Deferred || pending[0].Hold != "" {
		t.Fatalf("pending notice = %+v", pending[0])
	}
	mutate(automaticEventDeferred, func(r *automaticSubmissionRecord) {
		r.State, r.DeferredAt, r.ResubmitAt = rpc.TradeProposalAutomaticDeferred, rig.now, rig.now.Add(time.Minute)
	})
	deferred := notices()
	if !deferred[0].Deferred || deferred[0].Hold != "" {
		t.Fatalf("deferred notice = %+v", deferred[0])
	}
	mutate(automaticEventHeld, func(r *automaticSubmissionRecord) { r.HeldAt, r.HoldReason = rig.now, automaticHoldHandOrder })
	heldHand := notices()
	if heldHand[0].Hold != automaticNoticeHoldHandOrder {
		t.Fatalf("hand-order hold notice = %+v", heldHand[0])
	}
	mutate(automaticEventHeld, func(r *automaticSubmissionRecord) { r.HoldReason = automaticHoldUnavailable })
	heldUnverified := notices()
	if heldUnverified[0].Hold != automaticNoticeHoldUnverified {
		t.Fatalf("unverified hold notice = %+v", heldUnverified[0])
	}
	mutate(automaticEventSubmitted, func(r *automaticSubmissionRecord) {
		r.State, r.SubmittedAt, r.HeldAt, r.HoldReason = rpc.TradeProposalAutomaticSubmitted, rig.now, time.Time{}, ""
	})
	if got := rig.engine.automaticPendingNotices(rig.scope); len(got) != 0 {
		t.Fatalf("a submitted record kept its notice: %+v", got)
	}

	// Through the producer and the registry: the same occurrence stays open
	// while the copy follows the wait, and only an empty waiting set recovers.
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer func() { _ = store.Close() }()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	clock := base
	composer.now = func() time.Time { return clock }
	observe := func(at time.Time, notices ...automaticNoticeKey) *rpc.AlertCandidate {
		t.Helper()
		clock = at.Add(time.Second)
		snapshot, err := composer.ObserveProtection(t.Context(), alertShadowProtectionInput{
			AsOf: at, EvidenceAsOf: at, OrderSnapshotAsOf: at, OrderSnapshotComplete: true,
			OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope,
			Summary: rpc.ProtectionCoverageSummary{
				AsOf: at, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
				ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}},
			},
			Automatic: notices,
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range snapshot.Candidates {
			if snapshot.Candidates[i].Kind == rpc.AlertKindProtectionAutomatic {
				return &snapshot.Candidates[i]
			}
		}
		return nil
	}
	open := observe(base, pending...)
	if open == nil || open.State != rpc.AlertEpisodeOpen || open.PresentationCode != rpc.AlertPresentationProtectionAutoTrailingStop || open.Severity != rpc.AlertSeverityAct {
		t.Fatalf("pending notice candidate = %+v", open)
	}
	for i, step := range []struct {
		notices []automaticNoticeKey
		code    rpc.AlertPresentationCode
	}{
		{deferred, rpc.AlertPresentationProtectionAutoDeferred},
		{heldHand, rpc.AlertPresentationProtectionAutoHeld},
		{heldUnverified, rpc.AlertPresentationProtectionAutoHeldUnverified},
	} {
		got := observe(base.Add(time.Duration(i+1)*time.Minute), step.notices...)
		if got == nil || got.State == rpc.AlertEpisodeRecovered || got.OccurrenceKey != open.OccurrenceKey || got.PresentationCode != step.code {
			t.Fatalf("waiting step %d candidate = %+v, want the open occurrence %s with %s", i, got, open.OccurrenceKey, step.code)
		}
	}
	if gone := observe(base.Add(5 * time.Minute)); gone == nil || gone.State != rpc.AlertEpisodeRecovered || gone.OccurrenceKey != open.OccurrenceKey {
		t.Fatalf("leaving the waiting set did not recover the notice: %+v", gone)
	}
}
