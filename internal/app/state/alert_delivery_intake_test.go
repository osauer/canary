package state

import (
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The producer emits a recovery for one registry evaluation only, and the app
// samples the snapshot once per poll. A recovery that falls between two
// samples used to make every later snapshot an invalid transition, which held
// intake shut for every source from 2026-08-15 onward.
func TestAlertDeliveryReopenAfterUnobservedRecoveryKeepsIntakeOpen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	source := rpc.AlertSourceRulebook
	sources := []rpc.AlertSource{source}
	base := time.Date(2026, 8, 14, 5, 45, 0, 0, time.UTC)
	first := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "exit-discipline", "sequence:1", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, sources, sources, rpc.AlertCoverageCurrent, first)); err != nil {
		t.Fatal(err)
	}

	// Recovery at base+1m and reopen at base+2m both happen between samples:
	// the next sample shows only the reopened occurrence under a new key.
	reopenAt := base.Add(2 * time.Minute)
	reopened := reviseAlertCandidate(first, reopenAt, "b", rpc.AlertEpisodeOpen, rpc.AlertSeverityAct)
	reopened.OccurrenceKey = mustAlertOccurrenceKey(t, first.EpisodeKey, "sequence:2")
	reopened.StateChangedAt = reopenAt
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(reopenAt, sources, sources, rpc.AlertCoverageCurrent, reopened))
	if err != nil {
		t.Fatalf("reopen after an unobserved recovery refused intake: %v", err)
	}
	if view.DeliveryHealth.State != AlertDeliveryHealthHealthy {
		t.Fatalf("delivery health = %+v, want healthy", view.DeliveryHealth)
	}
	prior := findTestOccurrence(t, view, alertDeliveryDisplayID(defaultTestAlertAuthorityScope, first.OccurrenceKey))
	if prior.State != rpc.AlertEpisodeRecovered || prior.EndReason != AlertDeliveryEndUnobservedRecovery || !prior.EndedAt.Equal(reopenAt) {
		t.Fatalf("held occurrence was not closed as an unobserved recovery: %+v", prior)
	}
	due := store.AlertDeliveriesDue(reopenAt)
	if len(due) != 1 || due[0].OccurrenceKey != reopened.OccurrenceKey {
		t.Fatalf("reopened act occurrence is not transport-due: %+v", due)
	}

	// The persisted ledger stays valid across a restart.
	reopenedStore, err := Open(dir)
	if err != nil {
		t.Fatalf("persisted ledger with an unobserved recovery failed validation: %v", err)
	}
	if got := reopenedStore.AlertDelivery(reopenAt); got.DeliveryHealth.State != AlertDeliveryHealthHealthy {
		t.Fatalf("restarted health = %+v", got.DeliveryHealth)
	}

	// Producer-bug guards still refuse the snapshot.
	replay := reviseAlertCandidate(reopened, reopenAt.Add(time.Minute), "c", rpc.AlertEpisodeOpen, rpc.AlertSeverityAct)
	replay.OccurrenceKey = first.OccurrenceKey
	if _, err := reopenedStore.ObserveAlertSnapshot(testAlertSnapshot(reopenAt.Add(time.Minute), sources, sources, rpc.AlertCoverageCurrent, replay)); !errors.Is(err, ErrAlertDeliveryInvalidTransition) {
		t.Fatalf("an old occurrence replayed as current was accepted: %v", err)
	}
	regressed := reviseAlertCandidate(reopened, reopenAt.Add(2*time.Minute), "d", rpc.AlertEpisodeOpen, rpc.AlertSeverityAct)
	regressed.OccurrenceKey = mustAlertOccurrenceKey(t, first.EpisodeKey, "sequence:3")
	regressed.StateChangedAt = reopenAt
	if _, err := reopenedStore.ObserveAlertSnapshot(testAlertSnapshot(reopenAt.Add(2*time.Minute), sources, sources, rpc.AlertCoverageCurrent, regressed)); !errors.Is(err, ErrAlertDeliveryInvalidTransition) {
		t.Fatalf("a rotation that regressed lifecycle time was accepted: %v", err)
	}
}

func TestAlertDeliveryUnseenRecoveriesAreSkippedNotRefused(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	source := rpc.AlertSourceStress
	sources := []rpc.AlertSource{source}
	base := time.Date(2026, 8, 27, 6, 0, 0, 0, time.UTC)

	// An episode that opened and recovered between two samples is first seen
	// recovered: nothing is recorded and intake stays open.
	flash := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "flash", "sequence:1", base)
	flash = reviseAlertCandidate(flash, base, "e", rpc.AlertEpisodeRecovered, rpc.AlertSeverityWatch)
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, sources, sources, rpc.AlertCoverageCurrent, flash))
	if err != nil {
		t.Fatalf("recovery of an episode never seen open refused intake: %v", err)
	}
	if len(view.Occurrences) != 0 {
		t.Fatalf("unseen recovered episode was recorded: %+v", view.Occurrences)
	}

	// A held-open occurrence whose successor opened and recovered unseen is
	// closed as an unobserved recovery; a later reopen proceeds normally.
	openAt := base.Add(time.Minute)
	held := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "held", "sequence:1", openAt)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(openAt, sources, sources, rpc.AlertCoverageCurrent, held)); err != nil {
		t.Fatal(err)
	}
	recoveredAt := openAt.Add(3 * time.Minute)
	unseen := reviseAlertCandidate(held, recoveredAt, "f", rpc.AlertEpisodeRecovered, rpc.AlertSeverityWatch)
	unseen.OccurrenceKey = mustAlertOccurrenceKey(t, held.EpisodeKey, "sequence:2")
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(recoveredAt, sources, sources, rpc.AlertCoverageCurrent, unseen))
	if err != nil {
		t.Fatalf("recovery under an unseen occurrence key refused intake: %v", err)
	}
	prior := findTestOccurrence(t, view, alertDeliveryDisplayID(defaultTestAlertAuthorityScope, held.OccurrenceKey))
	if prior.EndReason != AlertDeliveryEndUnobservedRecovery || prior.State != rpc.AlertEpisodeRecovered {
		t.Fatalf("held occurrence was not closed: %+v", prior)
	}
	if len(view.Occurrences) != 1 {
		t.Fatalf("unseen recovered occurrence was recorded: %+v", view.Occurrences)
	}
	reopenAt := recoveredAt.Add(time.Minute)
	reopen := reviseAlertCandidate(unseen, reopenAt, "a", rpc.AlertEpisodeOpen, rpc.AlertSeverityWatch)
	reopen.OccurrenceKey = mustAlertOccurrenceKey(t, held.EpisodeKey, "sequence:3")
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(reopenAt, sources, sources, rpc.AlertCoverageCurrent, reopen)); err != nil {
		t.Fatalf("reopen after a skipped recovery refused intake: %v", err)
	}
	if due := store.AlertDeliveriesDue(reopenAt); len(due) != 1 || due[0].OccurrenceKey != reopen.OccurrenceKey {
		t.Fatalf("reopened occurrence is not due: %+v", due)
	}
}

func findTestOccurrence(t *testing.T, view AlertDeliveryView, displayID string) AlertDeliveryOccurrenceView {
	t.Helper()
	for _, occurrence := range view.Occurrences {
		if occurrence.DisplayID == displayID {
			return occurrence
		}
	}
	t.Fatalf("occurrence %s not in view: %+v", displayID, view.Occurrences)
	return AlertDeliveryOccurrenceView{}
}
