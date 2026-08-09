package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/osauer/canary/v2/internal/rpc"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"testing"
	"time"
)

var defaultTestAlertAuthorityScope = func() string {
	scope, err := rpc.BuildAlertAuthorityScope("TEST-ACCOUNT", "paper")
	if err != nil {
		panic(err)
	}
	return scope
}()

func TestAlertDeliveryCutoverIdentityRedactionAndLegacyIsolation(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(AlertModeActOnly); err != nil {
		t.Fatal(err)
	}
	beforeRaw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(beforeRaw), `"alert_delivery"`) {
		t.Fatalf("legacy state unexpectedly persisted optional alert_delivery: %s", beforeRaw)
	}
	if err := store.RecordAlert(AlertRecord{ID: "canary-legacy", Fingerprint: "legacy-fp", Title: "legacy", Body: "legacy", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	legacyAttention := store.Attention()
	legacyHistory := store.AlertHistory(0)
	legacyDiagnostic := store.GovernanceDiagnostic()

	at := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "private-account-symbol", "opening-1", at)
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(at, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if !view.Initialized || view.Generation != 1 || len(view.Occurrences) != 1 || view.Attention.HighWaterSeq != 1 || view.Attention.UnreadCount != 1 {
		t.Fatalf("unexpected cutover view: %+v", view)
	}
	if view.DeliveryHealth.State != AlertDeliveryHealthHealthy || view.DeliveryHealth.Class != "" || view.Occurrences[0].Disposition != AlertDispositionCutoverExisting {
		t.Fatalf("new ledger must preserve visible cutover state without arming transport: %+v", view)
	}
	if got := store.AlertDeliveriesDue(at); len(got) != 0 {
		t.Fatalf("cutover observation produced transport work: %+v", got)
	}
	if !reflect.DeepEqual(store.Attention(), legacyAttention) || !reflect.DeepEqual(store.AlertHistory(0), legacyHistory) || store.GovernanceDiagnostic() != legacyDiagnostic {
		t.Fatal("source-neutral observation changed legacy Canary state, diagnostic, or attention")
	}
	public, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{candidate.EpisodeKey, candidate.OccurrenceKey, candidate.EvidenceFingerprint, "private-account-symbol"} {
		if strings.Contains(string(public), private) {
			t.Fatalf("public view leaked private identity %q: %s", private, public)
		}
	}
	if view.Occurrences[0].DisplayID == "" || strings.Contains(view.Occurrences[0].DisplayID, candidate.OccurrenceKey) {
		t.Fatalf("display id is not independent and opaque: %+v", view.Occurrences[0])
	}
	if _, send, err := store.BeginAlertDelivery(view.Occurrences[0].DisplayID, AlertDeliveryTargetRef("device", "subscription"), at); err == nil || send {
		t.Fatalf("display id was accepted as private delivery authority: send=%v err=%v", send, err)
	}
	persisted, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{candidate.EpisodeKey, candidate.OccurrenceKey, candidate.EvidenceFingerprint} {
		if !strings.Contains(string(persisted), private) {
			t.Fatalf("durable private ledger omitted %q", private)
		}
	}
}

func TestAlertDeliveryRestartAfterAuthorityScopeChangeKeepsInterruptedUncertainty(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	scopeA, err := rpc.BuildAlertAuthorityScope("ACCOUNT-A", "paper")
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := rpc.BuildAlertAuthorityScope("ACCOUNT-B", "live")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	source := rpc.AlertSourceStress
	baseline := testAlertSnapshot(base, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent)
	baseline.AuthorityScope = scopeA
	if _, err := store.ObserveAlertSnapshot(baseline); err != nil {
		t.Fatal(err)
	}
	candidateAt := base.Add(10 * time.Second)
	candidate := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "scope-a-restart", "open-a", candidateAt)
	active := testAlertSnapshot(candidateAt, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent, candidate)
	active.AuthorityScope = scopeA
	if _, err := store.ObserveAlertSnapshot(active); err != nil {
		t.Fatal(err)
	}
	target := AlertDeliveryTargetRef("scope-restart-device", "scope-restart-subscription")
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, candidateAt.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("scope A reservation send=%v err=%v", send, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, candidateAt.Add(2*time.Second)); err != nil || !confirmed {
		t.Fatalf("scope A confirmation confirmed=%v err=%v", confirmed, err)
	}
	changedAt := candidateAt.Add(3 * time.Second)
	unknownB := testAlertSnapshot(changedAt, []rpc.AlertSource{source}, nil, rpc.AlertCoverageUnknown)
	unknownB.AuthorityScope = scopeB
	if _, err := store.ObserveAlertSnapshot(unknownB); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	view := restarted.AlertDelivery(time.Now().UTC())
	if view.DeliveryHealth.State != AlertDeliveryHealthDegraded || view.DeliveryHealth.Class != AlertDeliveryHealthClassInterrupted || view.AttemptTotals.Interrupted != 1 {
		t.Fatalf("restart in scope B concealed interrupted scope A send: %+v", view)
	}
	if attempt := restarted.data.AlertDelivery.Attempts[0]; attempt.AuthorityScope != scopeA || attempt.Class != AlertDeliveryAttemptInterrupted || attempt.Disposition != AlertDeliveryCompletionInactive {
		t.Fatalf("restart did not preserve scoped interruption evidence: %+v", attempt)
	}
	if due := restarted.AlertDeliveriesDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("restart reactivated old-scope delivery work: %+v", due)
	}
}

func TestAlertDeliveryPersistenceFailureAndDurableOverflow(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	first := testAlertCandidate(t, rpc.AlertSourceRegime, rpc.AlertKindMarketState, "market", "open-1", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{rpc.AlertSourceRegime}, []rpc.AlertSource{rpc.AlertSourceRegime}, rpc.AlertCoverageCurrent, first)); err != nil {
		t.Fatal(err)
	}
	before := store.AlertDelivery(base)
	revision := reviseAlertCandidate(first, base.Add(time.Minute), "b", rpc.AlertEpisodeOpen, rpc.AlertSeverityAct)
	store.saveHook = func(string) error { return errors.New("injected alert ledger save failure") }
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base.Add(time.Minute), []rpc.AlertSource{rpc.AlertSourceRegime}, []rpc.AlertSource{rpc.AlertSourceRegime}, rpc.AlertCoverageCurrent, revision)); err == nil {
		t.Fatal("injected persistence failure was ignored")
	}
	failed := store.AlertDelivery(base.Add(time.Minute))
	if len(failed.Occurrences) != len(before.Occurrences) || failed.Occurrences[0].Severity != before.Occurrences[0].Severity || failed.DeliveryHealth.State != AlertDeliveryHealthUnavailable || failed.Generation != before.Generation+1 {
		t.Fatalf("save failure was not atomic/fail-visible: before=%+v after=%+v", before, failed)
	}
	store.saveHook = nil
	recoveredView, err := store.ObserveAlertSnapshot(testAlertSnapshot(base.Add(time.Minute), []rpc.AlertSource{rpc.AlertSourceRegime}, []rpc.AlertSource{rpc.AlertSourceRegime}, rpc.AlertCoverageCurrent, revision))
	if err != nil {
		t.Fatal(err)
	}
	if recoveredView.Generation <= failed.Generation || recoveredView.DeliveryHealth.State != AlertDeliveryHealthHealthy {
		t.Fatalf("persistence recovery did not advance past volatile health generation: %+v", recoveredView)
	}

	dir := t.TempDir()
	overflowStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	overflowStore.alertDeliveryMaxItems = 1
	if _, err := overflowStore.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent,
		testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "book-one", "open-one", base))); err != nil {
		t.Fatal(err)
	}
	second := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindMarginSafety, "book-two", "open-two", base.Add(time.Minute))
	if _, err := overflowStore.ObserveAlertSnapshot(testAlertSnapshot(base.Add(time.Minute), []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, second)); !errors.Is(err, ErrAlertDeliveryOverflow) {
		t.Fatalf("occurrence overflow did not fail loud: %v", err)
	}
	overflow := overflowStore.AlertDelivery(base.Add(time.Minute))
	if len(overflow.Occurrences) != 1 || overflow.DeliveryHealth.State != AlertDeliveryHealthOverflow || overflow.Generation != 2 {
		t.Fatalf("overflow mutated semantic state or failed to persist health: %+v", overflow)
	}
	reopenedStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopenedStore.AlertDelivery(base.Add(time.Minute)); got.DeliveryHealth.State != AlertDeliveryHealthOverflow || len(got.Occurrences) != 1 {
		t.Fatalf("overflow health did not survive restart: %+v", got)
	}
	firstRecoveredAt := base.Add(2 * time.Minute)
	firstRecovered := reviseAlertCandidate(testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "book-one", "open-one", base), firstRecoveredAt, "c", rpc.AlertEpisodeRecovered, rpc.AlertSeverityWatch)
	view, err := overflowStore.ObserveAlertSnapshot(testAlertSnapshot(firstRecoveredAt, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, firstRecovered))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := overflowStore.MarkAlertDeliveryAttentionRead(view.Attention.HighWaterSeq); err != nil {
		t.Fatal(err)
	}
	if err := overflowStore.CompactAlertDelivery(firstRecoveredAt.Add(100 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if recoveredCapacity := overflowStore.AlertDelivery(firstRecoveredAt.Add(100 * 24 * time.Hour)); recoveredCapacity.DeliveryHealth.State != AlertDeliveryHealthHealthy || len(recoveredCapacity.Occurrences) != 0 {
		t.Fatalf("proven below-capacity compaction did not automate overflow recovery: %+v", recoveredCapacity)
	}
}

func TestAlertDeliveryReserveConfirmRetryReceiptAndActiveRecheck(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	base := time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceOrderIntegrity, rpc.AlertKindOrderIntegrity, "order-integrity", "open-1", base)
	snapshot := testAlertSnapshot(base, []rpc.AlertSource{rpc.AlertSourceOrderIntegrity}, []rpc.AlertSource{rpc.AlertSourceOrderIntegrity}, rpc.AlertCoverageCurrent, candidate)
	if _, err := store.ObserveAlertSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	due := store.AlertDeliveriesDue(base)
	if len(due) != 1 || due[0].OccurrenceKey != candidate.OccurrenceKey || due[0].Candidate.OccurrenceKey != candidate.OccurrenceKey {
		t.Fatalf("durable due-work scan lost private dispatch authority: %+v", due)
	}
	dueJSON, _ := json.Marshal(due[0])
	if strings.Contains(string(dueJSON), candidate.OccurrenceKey) || strings.Contains(string(dueJSON), candidate.EpisodeKey) {
		t.Fatalf("due-work JSON leaked private identity: %s", dueJSON)
	}
	target := AlertDeliveryTargetRef("device-one", "subscription-one")
	now := base.Add(time.Second)
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, now)
	if err != nil || !send {
		t.Fatalf("initial reservation send=%v err=%v reservation=%+v", send, err, reservation)
	}
	if _, again, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, now); err != nil || again {
		t.Fatalf("persisted reservation did not dedupe concurrent begin: send=%v err=%v", again, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, now); err != nil || !confirmed {
		t.Fatalf("transport confirmation failed: confirmed=%v err=%v", confirmed, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, now); err != nil || confirmed {
		t.Fatalf("same confirmed attempt authorized a second sender call: confirmed=%v err=%v", confirmed, err)
	}
	outcome, err := store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionRetryable, now)
	if err != nil || outcome.Class != AlertDeliveryAttemptRetry || !outcome.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first retry outcome=%+v err=%v", outcome, err)
	}
	if _, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, now.Add(30*time.Second)); err != nil || send {
		t.Fatalf("retry sent before one-minute deadline: send=%v err=%v", send, err)
	}
	for attemptNumber, delay := range []time.Duration{5 * time.Minute, 15 * time.Minute} {
		reservation, send, err = store.BeginAlertDelivery(candidate.OccurrenceKey, target, outcome.RetryAt)
		if err != nil || !send || reservation.AttemptNumber != attemptNumber+2 {
			t.Fatalf("retry %d reservation=%+v send=%v err=%v", attemptNumber+2, reservation, send, err)
		}
		if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, outcome.RetryAt); err != nil || !confirmed {
			t.Fatalf("retry confirmation failed: %v %v", confirmed, err)
		}
		outcome, err = store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionRetryable, outcome.RetryAt)
		if err != nil || !outcome.RetryAt.Equal(reservation.ReservedAt.Add(delay)) {
			t.Fatalf("retry schedule step %d = %+v err=%v", attemptNumber+2, outcome, err)
		}
	}
	reservation, send, err = store.BeginAlertDelivery(candidate.OccurrenceKey, target, outcome.RetryAt)
	if err != nil || !send || reservation.AttemptNumber != 4 {
		t.Fatalf("fourth reservation=%+v send=%v err=%v", reservation, send, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, outcome.RetryAt); err != nil || !confirmed {
		t.Fatalf("accept confirmation failed: %v %v", confirmed, err)
	}
	acceptedAt := outcome.RetryAt
	outcome, err = store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionAccepted, acceptedAt)
	if err != nil || outcome.Class != AlertDeliveryAttemptAccepted {
		t.Fatalf("accept outcome=%+v err=%v", outcome, err)
	}
	if _, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, outcome.RetryAt.Add(time.Minute)); err != nil || send {
		t.Fatalf("accepted receipt did not dedupe: send=%v err=%v", send, err)
	}
	if len(store.data.AlertDelivery.Receipts) != 1 {
		t.Fatalf("receipt count=%d", len(store.data.AlertDelivery.Receipts))
	}
	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatal(err)
	}
	suppressed := store.AlertDelivery(acceptedAt)
	if suppressed.DeliveryHealth.State != AlertDeliveryHealthHealthy || suppressed.AttemptTotals.Accepted != 1 || !suppressed.DeliveryHealth.LastAcceptedAt.Equal(acceptedAt) || len(store.AlertDeliveriesDue(acceptedAt)) != 0 {
		t.Fatalf("mode downgrade lost history or retained transport: %+v", suppressed)
	}
	enableTestAlertDelivery(t, store)
	receipt := store.data.AlertDelivery.Receipts[0]
	if receipt.ReceiptKey != alertDeliveryReceiptKey(defaultTestAlertAuthorityScope, candidate.OccurrenceKey, target) || strings.Contains(receipt.ReceiptKey, alertDeliveryDisplayID(defaultTestAlertAuthorityScope, candidate.OccurrenceKey)) {
		t.Fatalf("receipt was not internally keyed by private occurrence+target: %+v", receipt)
	}

	raceTarget := AlertDeliveryTargetRef("device-two", "subscription-two")
	raceReservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, raceTarget, base.Add(30*time.Minute))
	if err != nil || !send {
		t.Fatalf("race reservation send=%v err=%v", send, err)
	}
	recovered := reviseAlertCandidate(candidate, base.Add(31*time.Minute), "d", rpc.AlertEpisodeRecovered, candidate.Severity)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base.Add(31*time.Minute), []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, recovered)); err != nil {
		t.Fatal(err)
	}
	if confirmedReservation, confirmed, err := store.ConfirmAlertTransport(raceReservation.AttemptID, base.Add(31*time.Minute)); err != nil || confirmed || confirmedReservation.AttemptID == "" {
		t.Fatalf("recovery between reserve/confirm authorized send: reservation=%+v confirmed=%v err=%v", confirmedReservation, confirmed, err)
	}
	if got := store.data.AlertDelivery.Attempts[len(store.data.AlertDelivery.Attempts)-1].Class; got != AlertDeliveryAttemptInactive {
		t.Fatalf("race attempt class=%q, want inactive", got)
	}

	reopenAt := base.Add(32 * time.Minute)
	reopened := reviseAlertCandidate(recovered, reopenAt, "e", rpc.AlertEpisodeOpen, candidate.Severity)
	reopened.OccurrenceKey = mustAlertOccurrenceKey(t, candidate.EpisodeKey, "open-2")
	reopened.StateChangedAt = reopenAt
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(reopenAt, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, reopened)); err != nil {
		t.Fatal(err)
	}
	uncertainTarget := AlertDeliveryTargetRef("device-three", "subscription-three")
	uncertain, send, err := store.BeginAlertDelivery(reopened.OccurrenceKey, uncertainTarget, reopenAt.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("uncertain-window reservation send=%v err=%v", send, err)
	}
	confirmedView, confirmed, err := store.ConfirmAlertTransport(uncertain.AttemptID, reopenAt.Add(2*time.Second))
	if err != nil || !confirmed || confirmedView.DisplayID != uncertain.DisplayID || uncertain.DisplayID != alertDeliveryDisplayID(defaultTestAlertAuthorityScope, reopened.OccurrenceKey) {
		t.Fatalf("confirmed transport lost stable display tag: begin=%+v confirm=%+v confirmed=%v err=%v", uncertain, confirmedView, confirmed, err)
	}
	recoveredAgain := reviseAlertCandidate(reopened, reopenAt.Add(time.Minute), "a", rpc.AlertEpisodeRecovered, reopened.Severity)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(reopenAt.Add(time.Minute), []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, recoveredAgain)); err != nil {
		t.Fatal(err)
	}
	uncertainOutcome, err := store.CompleteAlertDelivery(uncertain.AttemptID, AlertDeliveryCompletionAccepted, reopenAt.Add(time.Minute+time.Second))
	if err != nil || uncertainOutcome.Disposition != AlertDeliveryCompletionInactive || uncertainOutcome.Class != AlertDeliveryAttemptAccepted || len(store.data.AlertDelivery.Receipts) != 2 {
		t.Fatalf("accepted transport truth was lost across recovery: outcome=%+v receipts=%d err=%v", uncertainOutcome, len(store.data.AlertDelivery.Receipts), err)
	}

	retireOpenAt := reopenAt.Add(2 * time.Minute)
	retireOpen := reviseAlertCandidate(recoveredAgain, retireOpenAt, "b", rpc.AlertEpisodeOpen, candidate.Severity)
	retireOpen.OccurrenceKey = mustAlertOccurrenceKey(t, candidate.EpisodeKey, "open-3")
	retireOpen.StateChangedAt = retireOpenAt
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(retireOpenAt, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, retireOpen)); err != nil {
		t.Fatal(err)
	}
	retiredTarget := AlertDeliveryTargetRef("device-four", "subscription-four")
	retiredReservation, send, err := store.BeginAlertDelivery(retireOpen.OccurrenceKey, retiredTarget, retireOpenAt.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("retirement-race reservation send=%v err=%v", send, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(retiredReservation.AttemptID, retireOpenAt.Add(2*time.Second)); err != nil || !confirmed {
		t.Fatalf("retirement-race confirmation confirmed=%v err=%v", confirmed, err)
	}
	retiredAt := retireOpenAt.Add(3 * time.Second)
	if err := store.RetireAlertDeliveryTarget(retiredTarget, retiredAt); err != nil {
		t.Fatal(err)
	}
	retiredOutcome, err := store.CompleteAlertDelivery(retiredReservation.AttemptID, AlertDeliveryCompletionAccepted, retireOpenAt.Add(4*time.Second))
	if err != nil || retiredOutcome.Disposition != AlertDeliveryCompletionRetired || retiredOutcome.Class != AlertDeliveryAttemptAccepted {
		t.Fatalf("accepted transport truth was lost across target retirement: outcome=%+v err=%v", retiredOutcome, err)
	}
	lastReceipt := store.data.AlertDelivery.Receipts[len(store.data.AlertDelivery.Receipts)-1]
	if lastReceipt.TargetRef != retiredTarget || !lastReceipt.RetiredAt.Equal(retiredAt) {
		t.Fatalf("retired accepted receipt lost retirement evidence: %+v", lastReceipt)
	}
}

func testAlertCandidate(t *testing.T, source rpc.AlertSource, kind rpc.AlertKind, episodeIdentity, occurrenceIdentity string, at time.Time) rpc.AlertCandidate {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(source, kind, episodeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: mustAlertOccurrenceKey(t, episode, occurrenceIdentity),
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64), Source: source, Kind: kind,
		PresentationCode: testAlertPresentationCode(source), State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityWatch,
		EvidenceHealth: rpc.AlertEvidenceCurrent, Destination: rpc.AlertDestinationAlerts,
		EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}
}

func testAlertPresentationCode(source rpc.AlertSource) rpc.AlertPresentationCode {
	switch source {
	case rpc.AlertSourceStress:
		return rpc.AlertPresentationPortfolioStress
	case rpc.AlertSourceRegime:
		return rpc.AlertPresentationRegimeMarketStress
	case rpc.AlertSourceRulebook:
		return rpc.AlertPresentationRulebookSingleNameExposure
	case rpc.AlertSourceRiskPolicy:
		return rpc.AlertPresentationRiskPolicyDrift
	case rpc.AlertSourceProtection:
		return rpc.AlertPresentationProtectionReconciliationRequired
	case rpc.AlertSourceOrderIntegrity:
		return rpc.AlertPresentationOrderIntegrityMismatch
	case rpc.AlertSourceReconciliation:
		return rpc.AlertPresentationReconciliationException
	case rpc.AlertSourceGovernance:
		return rpc.AlertPresentationGovernanceMonthlyPulse
	case rpc.AlertSourceDataHealth:
		return rpc.AlertPresentationDataHealthQuality
	default:
		return rpc.AlertPresentationDeliveryHealth
	}
}

func enableTestAlertDelivery(t *testing.T, store *Store) {
	t.Helper()
	if err := store.SetAlertMode(AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.data.AlertDelivery == nil {
		store.data.AlertDelivery = newAlertDeliveryData()
	}
	if store.data.AlertDelivery.Baselines == nil {
		store.data.AlertDelivery.Baselines = make(map[string]alertDeliveryBaseline)
	}
	baselineAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	store.data.AlertDelivery.Baselines[defaultTestAlertAuthorityScope] = alertDeliveryBaseline{
		EstablishedAt: baselineAt,
		SnapshotAsOf:  baselineAt,
	}
}

func reviseAlertCandidate(candidate rpc.AlertCandidate, at time.Time, fingerprintDigit string, state rpc.AlertEpisodeState, severity rpc.AlertSeverity) rpc.AlertCandidate {
	priorState := candidate.State
	candidate.EvidenceFingerprint = "sha256:" + strings.Repeat(fingerprintDigit, 64)
	candidate.State = state
	candidate.Severity = severity
	candidate.EvidenceAsOf = at
	candidate.ObservedAt = at
	if state != priorState {
		candidate.StateChangedAt = at
	}
	if state == rpc.AlertEpisodeRecovered {
		candidate.EvidenceHealth = rpc.AlertEvidenceCurrent
	}
	return candidate
}

func mustAlertOccurrenceKey(t *testing.T, episodeKey, identity string) string {
	t.Helper()
	key, err := rpc.BuildAlertOccurrenceKey(episodeKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testAlertSnapshot(at time.Time, expected, covered []rpc.AlertSource, freshness rpc.AlertCoverageFreshness, candidates ...rpc.AlertCandidate) rpc.AlertCandidateSnapshot {
	if candidates == nil {
		candidates = []rpc.AlertCandidate{}
	}
	state := rpc.AlertCoveragePartial
	switch {
	case len(covered) == 0:
		state = rpc.AlertCoverageUnavailable
		freshness = rpc.AlertCoverageUnknown
	case len(covered) == len(expected):
		state = rpc.AlertCoverageComplete
	}
	current := rpc.AlertSnapshotUnknown
	for _, candidate := range candidates {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			current = rpc.AlertSnapshotActive
			break
		}
	}
	if current != rpc.AlertSnapshotActive && state == rpc.AlertCoverageComplete && freshness == rpc.AlertCoverageCurrent {
		current = rpc.AlertSnapshotClear
	}
	sources := make([]rpc.AlertSourceCoverage, 0, len(expected))
	for _, source := range expected {
		row := rpc.AlertSourceCoverage{
			Source: source, Status: "test_unavailable", Reason: "test_unavailable",
			EvidenceHealth: rpc.AlertEvidenceUnavailable,
		}
		if slices.Contains(covered, source) {
			row.Status, row.Reason, row.Covered = "test_current", "test_current", true
			row.EvidenceHealth = rpc.AlertEvidenceCurrent
			if freshness == rpc.AlertCoverageStale {
				row.Status, row.Reason, row.EvidenceHealth = "test_stale", "test_stale", rpc.AlertEvidenceStale
			}
			row.InputAsOf, row.ObservedAt, row.EvidenceAsOf = at, at, at
			row.FreshUntil = at.Add(time.Hour)
		}
		sources = append(sources, row)
	}
	slices.SortFunc(sources, func(a, b rpc.AlertSourceCoverage) int { return strings.Compare(string(a.Source), string(b.Source)) })
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: defaultTestAlertAuthorityScope, AsOf: at, CurrentState: current,
		Coverage:   rpc.AlertCoverage{State: state, Freshness: freshness, AsOf: at, ExpectedSources: append([]rpc.AlertSource{}, expected...), CoveredSources: append([]rpc.AlertSource{}, covered...)},
		Sources:    sources,
		Candidates: append([]rpc.AlertCandidate{}, candidates...),
	}
}

func TestOpenQuarantinesUnsupportedAlertLedgerWithoutClearingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"typed decode", json.RawMessage(`{"version":17,"generation":9,"private_marker":"typed"}`)},
		{"unsupported schema", json.RawMessage(`{"version":"alert-delivery-v3","generation":41,"private_marker":"old"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAlertDeliveryQuarantineFixture(t, dir, tc.raw)
			store, err := Open(dir)
			if err != nil {
				t.Fatalf("Open isolated alert ledger: %v", err)
			}
			assertAlertDeliveryQuarantined(t, store)
			if history := store.AlertHistory(10); len(history) != 1 || history[0].ID != "existing-alert" {
				t.Fatalf("unrelated app state unavailable after quarantine: %+v", history)
			}
			artifact := filepath.Join(dir, alertDeliveryQuarantineArtifactName(tc.raw))
			assertExactPrivateFile(t, artifact, tc.raw)

			if err := store.SetAlertMode(AlertModeNone); err != nil {
				t.Fatalf("save unrelated state: %v", err)
			}
			persisted, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(persisted, &top); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(top["alert_delivery"], tc.raw) {
				t.Fatalf("unsupported alert ledger was normalized or cleared: %s", top["alert_delivery"])
			}
		})
	}
}

func writeAlertDeliveryQuarantineFixture(t *testing.T, dir string, alertDelivery json.RawMessage) {
	t.Helper()
	raw := append([]byte(`{"alert_settings":{"mode":"watch_and_act"},"alert_history":[{"id":"existing-alert","title":"existing","body":"usable"}],"alert_delivery":`), alertDelivery...)
	raw = append(raw, '}')
	if !json.Valid(raw) {
		t.Fatalf("invalid state fixture: %s", raw)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAlertDeliveryQuarantined(t *testing.T, store *Store) {
	t.Helper()
	if store == nil || !store.alertDeliveryQuarantinedLocked() || store.data.AlertDelivery != nil {
		t.Fatalf("store did not retain quarantine boundary: %+v", store)
	}
	view := store.AlertDelivery(time.Now().UTC())
	if view.Initialized || view.Generation != alertDeliveryQuarantineGeneration || len(view.Occurrences) != 0 ||
		view.Attention.UnreadCount != 0 || view.DeliveryHealth.State != AlertDeliveryHealthUnavailable ||
		view.DeliveryHealth.Class != AlertDeliveryHealthClassInvalidPersistedState {
		t.Fatalf("quarantine view is not uninitialized/default-deny: %+v", view)
	}
	if due := store.AlertDeliveriesDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("quarantined delivery produced due work: %+v", due)
	}
}

func assertExactPrivateFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("preserved bytes changed\ngot: %q\nwant: %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("preserved artifact mode=%v", info.Mode())
	}
}
