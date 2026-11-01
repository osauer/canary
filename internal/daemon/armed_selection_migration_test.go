package daemon

import (
	"strconv"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func armedSelectionLegacyRecord(t *testing.T, rig *automaticTestRig, state string) rpc.TradeProposal {
	t.Helper()
	book := armedSelectionBook(300)
	p := rig.mandateProposal(book.Stocks[0], 100, 1, 1)
	policy, status := rig.server.protectionPolicies.Active()
	sources := armedSelectionSources(book)
	// The pre-upgrade identity binds only size/effect/session and semantic
	// portfolio buckets; deliberately do not call the revised term builder.
	legacy := newProposalRevisionProjection(status.EffectiveFingerprint, sources, rig.scope)
	legacy.Row = []string{p.Key + ":" + strconv.Itoa(p.Quantity) + ":" + p.PositionEffect, "mandate-session:" + p.Mandate.Session}
	p.Revision = legacy.hash()
	snap := rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: rig.now,
		Revision: p.Revision, EffectiveRevision: p.Revision, AccountID: rig.scope.Account, AccountMode: rig.scope.Mode,
		PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, EffectivePolicyFingerprint: status.EffectiveFingerprint,
		PolicyStatus: status, SourceFingerprints: sources, Proposals: []rpc.TradeProposal{p}}
	if err := rig.engine.installSnapshot(snap, false); err != nil {
		t.Fatal(err)
	}
	rig.engine.reconcileAutomatic(t.Context())
	rig.notice(rig.record(p.Key, p.Revision))
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(p.Key, p.Revision)]
		r.State = state
		if state != rpc.TradeProposalAutomaticPending {
			r.SubmittingAt = rig.now
			r.PreviewTokenID = "synthetic-legacy-attempt"
		}
		if state == rpc.TradeProposalAutomaticSubmitted {
			r.SubmittedAt, r.ResolvedAt = rig.now, rig.now
			r.OrderRef = "synthetic-legacy-order"
		}
		if state == rpc.TradeProposalAutomaticFailed {
			r.ResolvedAt, r.Reason = rig.now, "order outcome is unknown"
		}
		return []automaticSubmissionEvent{{At: rig.now, Type: state, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode}}
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArmedSelectionUpgradePreservesLegacyAttempt(t *testing.T) {
	for _, state := range []string{rpc.TradeProposalAutomaticPending, rpc.TradeProposalAutomaticSubmitted, rpc.TradeProposalAutomaticSubmitting, rpc.TradeProposalAutomaticFailed} {
		t.Run(state, func(t *testing.T) {
			rig := newAutomaticTestRig(t, automaticMandateAuthority)
			legacy := armedSelectionLegacyRecord(t, rig, state)
			before := rig.record(legacy.Key, legacy.Revision)
			rig.advance(5 * time.Minute)
			rig.restart()
			for range 2 {
				current := armedSelectionInstall(t, rig, legacy, armedSelectionSources(armedSelectionBook(300)))
				rig.engine.reconcileAutomatic(t.Context())
				got := rig.record(current.Key, current.Revision)
				if got.State != state || len(rig.engine.automatic.list()) != 1 || !got.CreatedAt.Equal(before.CreatedAt) || !got.SubmitAt.Equal(before.SubmitAt) || !got.NoticedAt.Equal(before.NoticedAt) {
					t.Fatalf("upgrade created or restarted an unchanged legacy order: before=%+v after=%+v records=%d", before, got, len(rig.engine.automatic.list()))
				}
				rig.restart()
			}
		})
	}
}

func TestArmedSelectionLegacySubmittedNeedsConfirmedHoldingChange(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	legacy := armedSelectionLegacyRecord(t, rig, rpc.TradeProposalAutomaticSubmitted)
	rig.restart()
	filled := armedSelectionBook(200)
	next := rig.mandateProposal(filled.Stocks[0], 100, 1, 1)
	next = armedSelectionInstall(t, rig, next, armedSelectionSources(filled))
	rig.engine.reconcileAutomatic(t.Context())
	if got := rig.record(next.Key, next.Revision); got.State != rpc.TradeProposalAutomaticPending || len(rig.engine.automatic.list()) != 2 {
		t.Fatalf("confirmed changed holding did not create the next installment: %+v", got)
	}
	// Once adopted, the exact-holding identity stays adopted, including
	// across a restart, even though the legacy record remains in the journal.
	rig.restart()
	current := armedSelectionInstall(t, rig, next, armedSelectionSources(filled))
	rig.engine.reconcileAutomatic(t.Context())
	if current.Revision != next.Revision || current.Revision == legacy.Revision || len(rig.engine.automatic.list()) != 2 {
		t.Fatal("migrated installment reverted to the legacy attempt")
	}
}

func TestArmedSelectionLegacyUncertainAttemptFencesChangedHolding(t *testing.T) {
	for _, state := range []string{rpc.TradeProposalAutomaticSubmitting, rpc.TradeProposalAutomaticFailed} {
		t.Run(state, func(t *testing.T) {
			rig := newAutomaticTestRig(t, automaticMandateAuthority)
			legacy := armedSelectionLegacyRecord(t, rig, state)
			rig.restart()
			filled := armedSelectionBook(200)
			next := rig.mandateProposal(filled.Stocks[0], 100, 1, 1)
			next = armedSelectionInstall(t, rig, next, armedSelectionSources(filled))
			rig.engine.reconcileAutomatic(t.Context())
			if got := rig.record(next.Key, next.Revision); got.State != state || next.Revision != legacy.Revision || len(rig.engine.automatic.list()) != 1 {
				t.Fatalf("unresolved external attempt was replaced on upgrade: %+v", got)
			}
		})
	}
}

func TestArmedSelectionLegacyWithoutHoldingBaselineStaysFenced(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	legacy := armedSelectionLegacyRecord(t, rig, rpc.TradeProposalAutomaticSubmitted)
	// A prior empty proposal snapshot gives no proof of how much was held
	// for the submitted order. The first new reading must not imply a fill.
	rig.install()
	rig.restart()
	filled := armedSelectionBook(200)
	next := rig.mandateProposal(filled.Stocks[0], 100, 1, 1)
	next = armedSelectionInstall(t, rig, next, armedSelectionSources(filled))
	rig.engine.reconcileAutomatic(t.Context())
	if got := rig.record(next.Key, next.Revision); got.State != rpc.TradeProposalAutomaticSubmitted || next.Revision != legacy.Revision || len(rig.engine.automatic.list()) != 1 {
		t.Fatalf("missing baseline was treated as proof of a fill: %+v", got)
	}
}

func TestArmedSelectionNoticeIdentityFollowsVetoOpportunity(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	book := armedSelectionBook(300)
	first := rig.mandateProposal(book.Stocks[0], 100, 1, 1)
	first = armedSelectionInstall(t, rig, first, armedSelectionSources(book))
	rig.engine.reconcileAutomatic(t.Context())
	original := rig.record(first.Key, first.Revision)
	id := automaticRecordNoticeID(original)
	if id == "" {
		t.Fatal("pending opportunity has no notice identity")
	}
	rig.advance(5 * time.Minute)
	smaller := rig.mandateProposal(book.Stocks[0], 99, 1, 1)
	smaller = armedSelectionInstall(t, rig, smaller, armedSelectionSources(book))
	rig.engine.reconcileAutomatic(t.Context())
	rec := rig.record(smaller.Key, smaller.Revision)
	if automaticRecordNoticeID(rec) != id || !rec.CreatedAt.Equal(original.CreatedAt) || rec.clockQuantity() != original.clockQuantity() {
		t.Fatal("a smaller order with the same veto clock created another notice")
	}
	rec.State, rec.SubmitAt, rec.ResubmitAt = rpc.TradeProposalAutomaticDeferred, rig.now.Add(time.Hour), rig.now.Add(2*time.Hour)
	if automaticRecordNoticeID(rec) != id {
		t.Fatal("a deferral or send-time adjustment changed the veto opportunity")
	}
	larger := rig.mandateProposal(book.Stocks[0], 102, 1, 1)
	larger = armedSelectionInstall(t, rig, larger, armedSelectionSources(book))
	rig.engine.reconcileAutomatic(t.Context())
	if got := rig.record(larger.Key, larger.Revision); automaticRecordNoticeID(got) == id || !got.CreatedAt.Equal(rig.now) {
		t.Fatal("an order beyond the noticed lot kept the old notice identity")
	}
}
