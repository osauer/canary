package state

import (
	"encoding/json"
	"errors"

	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
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

func TestAlertDeliveryAuthorityScopeChangeRetiresPreviousContextWithoutRecoveryOrClear(t *testing.T) {
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
	base := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	oldCandidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "scope-a", "open-a", base)
	oldSnapshot := testAlertSnapshot(base, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, oldCandidate)
	oldSnapshot.AuthorityScope = scopeA
	oldView, err := store.ObserveAlertSnapshot(oldSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	oldDisplay := oldView.Occurrences[0].DisplayID
	oldAttentionHighWater := oldView.Attention.HighWaterSeq

	changedAt := base.Add(time.Minute)
	newUnknown := testAlertSnapshot(changedAt, []rpc.AlertSource{rpc.AlertSourceStress}, nil, rpc.AlertCoverageUnknown)
	newUnknown.AuthorityScope = scopeB
	view, err := store.ObserveAlertSnapshot(newUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if view.AuthorityScope != scopeB || view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.Freshness != rpc.AlertCoverageUnknown {
		t.Fatalf("new scope did not start unknown and clean: %+v", view)
	}
	if len(view.Occurrences) != 1 {
		t.Fatalf("dormant live occurrence escaped instead of one previous-context row: %+v", view.Occurrences)
	}
	previous := view.Occurrences[0]
	if previous.DisplayID == oldDisplay || previous.AttentionSeq != oldAttentionHighWater || view.Attention.HighWaterSeq != oldAttentionHighWater {
		t.Fatalf("scope archive lost the unread cursor or reused live identity: occurrence=%+v attention=%+v", previous, view.Attention)
	}
	if previous.State != rpc.AlertEpisodeOpen || previous.EndReason != AlertDeliveryEndAuthorityScopeChanged || !previous.EndedAt.Equal(changedAt) {
		t.Fatalf("previous scope was recovered/cleared instead of archived: %+v", previous)
	}
	privateA, privateAEpisode, ok := findAlertDeliveryOccurrence(store.data.AlertDelivery, scopeA, oldCandidate.OccurrenceKey)
	if !ok || !alertDeliveryOccurrenceActive(privateA, privateAEpisode) || !privateA.EndedAt.IsZero() || privateA.EndReason != "" {
		t.Fatalf("scope archive mutated producer lifecycle: occurrence=%+v episode=%+v", privateA, privateAEpisode)
	}
	if len(view.SourceWatermarks) != 0 || len(store.AlertDeliveriesDue(changedAt)) != 0 {
		t.Fatalf("previous scope retained authority or delivery work: watermarks=%+v due=%+v", view.SourceWatermarks, store.AlertDeliveriesDue(changedAt))
	}
	public, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"authority_scope"`, scopeA, scopeB} {
		if strings.Contains(string(public), forbidden) {
			t.Fatalf("public view leaked authority scope %q: %s", forbidden, public)
		}
	}

	currentAt := changedAt.Add(time.Minute)

	currentCandidate := reviseAlertCandidate(oldCandidate, currentAt, "b", rpc.AlertEpisodeOpen, rpc.AlertSeverityWatch)
	currentSnapshot := testAlertSnapshot(currentAt, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, currentCandidate)
	currentSnapshot.AuthorityScope = scopeB
	view, err = store.ObserveAlertSnapshot(currentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if view.CurrentState != rpc.AlertSnapshotActive || len(view.Occurrences) != 2 {
		t.Fatalf("current scope did not start independently: %+v", view)
	}
	previous = occurrenceByDisplay(t, view, previous.DisplayID)
	if previous.State != rpc.AlertEpisodeOpen || previous.EndReason != AlertDeliveryEndAuthorityScopeChanged {
		t.Fatalf("later current coverage reinterpreted previous context: %+v", previous)
	}
	currentBDisplay := alertDeliveryDisplayID(scopeB, currentCandidate.OccurrenceKey)
	if currentBDisplay == oldDisplay || occurrenceByDisplay(t, view, currentBDisplay).EndReason != "" || view.Attention.HighWaterSeq != oldAttentionHighWater+1 {
		t.Fatalf("scope B did not receive an independent live identity: %+v", view)
	}

	returnAt := currentAt.Add(time.Minute)
	resumedA := reviseAlertCandidate(oldCandidate, returnAt, "c", rpc.AlertEpisodeOpen, rpc.AlertSeverityWatch)
	returnSnapshot := testAlertSnapshot(returnAt, []rpc.AlertSource{rpc.AlertSourceStress}, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent, resumedA)
	returnSnapshot.AuthorityScope = scopeA
	view, err = store.ObserveAlertSnapshot(returnSnapshot)
	if err != nil {
		t.Fatalf("A -> B -> A re-entry rejected producer occurrence: %v", err)
	}
	currentA := occurrenceByDisplay(t, view, oldDisplay)
	if currentA.State != rpc.AlertEpisodeOpen || !currentA.EndedAt.IsZero() || currentA.EndReason != "" || view.Attention.HighWaterSeq != oldAttentionHighWater+1 {
		t.Fatalf("scope A lifecycle was not resumed intact: occurrence=%+v attention=%+v", currentA, view.Attention)
	}
	if len(view.Occurrences) != 3 || len(view.Attention.UnreadRefs) != 2 {
		t.Fatalf("re-entry did not preserve bounded context and coherent attention: %+v", view)
	}
	publicDisplays := make(map[string]bool, len(view.Occurrences))
	for _, occurrence := range view.Occurrences {
		publicDisplays[occurrence.DisplayID] = true
	}
	for _, ref := range view.Attention.UnreadRefs {
		if !publicDisplays[ref.DisplayID] {
			t.Fatalf("attention references hidden dormant identity: ref=%+v occurrences=%+v", ref, view.Occurrences)
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	durable := reopened.AlertDelivery(returnAt)
	if durable.AuthorityScope != scopeA || occurrenceByDisplay(t, durable, oldDisplay).EndReason != "" || durable.Attention.HighWaterSeq != oldAttentionHighWater+1 {
		t.Fatalf("scope partition and re-entry were not durable: %+v", durable)
	}
}

func TestAlertDeliveryInterruptedUncertaintySurvivesAuthorityScopeChange(t *testing.T) {
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
	candidate := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "scope-a-uncertain", "open-a", candidateAt)
	active := testAlertSnapshot(candidateAt, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent, candidate)
	active.AuthorityScope = scopeA
	if _, err := store.ObserveAlertSnapshot(active); err != nil {
		t.Fatal(err)
	}
	target := AlertDeliveryTargetRef("scope-a-device", "scope-a-subscription")
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, candidateAt.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("scope A reservation send=%v err=%v", send, err)
	}
	if _, confirmed, err := store.ConfirmAlertTransport(reservation.AttemptID, candidateAt.Add(2*time.Second)); err != nil || !confirmed {
		t.Fatalf("scope A confirmation confirmed=%v err=%v", confirmed, err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if health := recovered.AlertDelivery(time.Now().UTC()).DeliveryHealth; health.State != AlertDeliveryHealthDegraded || health.Class != AlertDeliveryHealthClassInterrupted {
		t.Fatalf("restart did not expose scope A uncertainty: %+v", health)
	}
	changedAt := time.Now().UTC().Add(time.Second)
	unknownB := testAlertSnapshot(changedAt, []rpc.AlertSource{source}, nil, rpc.AlertCoverageUnknown)
	unknownB.AuthorityScope = scopeB
	if _, err := recovered.ObserveAlertSnapshot(unknownB); err != nil {
		t.Fatal(err)
	}
	currentBAt := changedAt.Add(time.Second)
	currentB := testAlertSnapshot(currentBAt, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent)
	currentB.AuthorityScope = scopeB
	view, err := recovered.ObserveAlertSnapshot(currentB)
	if err != nil {
		t.Fatal(err)
	}
	if view.DeliveryHealth.State != AlertDeliveryHealthDegraded || view.DeliveryHealth.Class != AlertDeliveryHealthClassInterrupted || view.AttemptTotals.Interrupted != 1 {
		t.Fatalf("scope B refresh concealed unresolved scope A uncertainty: %+v", view)
	}
	if due := recovered.AlertDeliveriesDue(currentBAt); len(due) != 0 {
		t.Fatalf("old-scope uncertainty reactivated delivery work: %+v", due)
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

func TestAlertDeliveryAuthorityRecoveryReopenAndEscalation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	canary := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "book", "canary-open-1", base)
	regime := testAlertCandidate(t, rpc.AlertSourceRegime, rpc.AlertKindMarketState, "market", "regime-open-1", base)
	expected := []rpc.AlertSource{rpc.AlertSourceStress, rpc.AlertSourceRegime}
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, expected, expected, rpc.AlertCoverageCurrent, canary, regime))
	if err != nil {
		t.Fatal(err)
	}
	if view.Generation != 1 || view.Attention.HighWaterSeq != 2 {
		t.Fatalf("initial authority view = %+v", view)
	}

	partialAt := base.Add(time.Minute)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(partialAt, expected, []rpc.AlertSource{rpc.AlertSourceStress}, rpc.AlertCoverageCurrent))
	if err != nil {
		t.Fatal(err)
	}
	if occurrenceBySource(t, view, rpc.AlertSourceStress).EndReason != AlertDeliveryEndOmitted || !occurrenceBySource(t, view, rpc.AlertSourceRegime).EndedAt.IsZero() {
		t.Fatalf("partial authority did not resolve only Canary: %+v", view.Occurrences)
	}
	if !view.SourceWatermarks[rpc.AlertSourceStress].Equal(partialAt) || !view.SourceWatermarks[rpc.AlertSourceRegime].Equal(base) {
		t.Fatalf("source watermarks = %+v", view.SourceWatermarks)
	}

	staleAt := base.Add(2 * time.Minute)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(staleAt, expected, expected, rpc.AlertCoverageStale))
	if err != nil {
		t.Fatal(err)
	}
	if !occurrenceBySource(t, view, rpc.AlertSourceRegime).EndedAt.IsZero() || !view.SourceWatermarks[rpc.AlertSourceRegime].Equal(base) {
		t.Fatalf("stale coverage falsely resolved or advanced Regime authority: %+v", view)
	}

	recoveredAt := base.Add(3 * time.Minute)
	recovered := reviseAlertCandidate(regime, recoveredAt, "c", rpc.AlertEpisodeRecovered, rpc.AlertSeverityWatch)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(recoveredAt, expected, expected, rpc.AlertCoverageCurrent, recovered))
	if err != nil {
		t.Fatal(err)
	}
	view = store.AlertDelivery(recoveredAt)
	if occurrenceBySource(t, view, rpc.AlertSourceRegime).EndReason != AlertDeliveryEndRecovered || view.CurrentState != rpc.AlertSnapshotClear {
		t.Fatalf("exact recovery was not applied: %+v", view)
	}

	reopenAt := base.Add(4 * time.Minute)
	reopened := reviseAlertCandidate(regime, reopenAt, "d", rpc.AlertEpisodeOpen, rpc.AlertSeverityWatch)
	reopened.OccurrenceKey = mustAlertOccurrenceKey(t, regime.EpisodeKey, "regime-reopen-2")
	reopened.StateChangedAt = reopenAt
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(reopenAt, expected, expected, rpc.AlertCoverageCurrent, reopened))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Occurrences) != 3 || view.Attention.HighWaterSeq != 3 {
		t.Fatalf("reopen did not create one occurrence and attention item: %+v", view)
	}

	revisionAt := base.Add(5 * time.Minute)
	revision := reviseAlertCandidate(reopened, revisionAt, "e", rpc.AlertEpisodeOpen, rpc.AlertSeverityAct)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(revisionAt, expected, expected, rpc.AlertCoverageCurrent, revision))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Occurrences) != 3 || view.Attention.HighWaterSeq != 3 || occurrenceByDisplay(t, view, alertDeliveryDisplayID(defaultTestAlertAuthorityScope, reopened.OccurrenceKey)).Severity != rpc.AlertSeverityAct {
		t.Fatalf("evidence revision created attention/send identity churn: %+v", view)
	}

	nonQualifiedAt := base.Add(6 * time.Minute)
	nonQualified := reviseAlertCandidate(reopened, nonQualifiedAt, "f", rpc.AlertEpisodeEscalated, rpc.AlertSeverityAct)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(nonQualifiedAt, expected, expected, rpc.AlertCoverageCurrent, nonQualified))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Occurrences) != 3 || view.Attention.HighWaterSeq != 3 {
		t.Fatalf("same-occurrence escalation created a new occurrence: %+v", view)
	}

	qualifiedAt := base.Add(7 * time.Minute)
	qualified := reviseAlertCandidate(nonQualified, qualifiedAt, "a", rpc.AlertEpisodeEscalated, rpc.AlertSeverityUrgent)
	qualified.OccurrenceKey = mustAlertOccurrenceKey(t, regime.EpisodeKey, "qualified-escalation-3")
	qualified.StateChangedAt = qualifiedAt
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(qualifiedAt, expected, expected, rpc.AlertCoverageCurrent, qualified))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Occurrences) != 4 || view.Attention.HighWaterSeq != 4 || occurrenceByDisplay(t, view, alertDeliveryDisplayID(defaultTestAlertAuthorityScope, reopened.OccurrenceKey)).EndReason != AlertDeliveryEndSuperseded {
		t.Fatalf("qualified escalation lifecycle incorrect: %+v", view)
	}
	stableGeneration := view.Generation

	oldAt := base.Add(6500 * time.Millisecond)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(oldAt, expected, expected, rpc.AlertCoverageStale)); !errors.Is(err, ErrAlertDeliveryOldSnapshot) {
		t.Fatalf("candidate-less view rewind was not rejected: %v", err)
	}
	mismatchedRecovery := reviseAlertCandidate(qualified, base.Add(8*time.Minute), "b", rpc.AlertEpisodeRecovered, rpc.AlertSeverityUrgent)
	mismatchedRecovery.OccurrenceKey = mustAlertOccurrenceKey(t, regime.EpisodeKey, "wrong-recovery-key")
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base.Add(8*time.Minute), expected, expected, rpc.AlertCoverageCurrent, mismatchedRecovery)); !errors.Is(err, ErrAlertDeliveryInvalidTransition) {
		t.Fatalf("non-exact recovery was accepted: %v", err)
	}
	refused := store.AlertDelivery(base.Add(8 * time.Minute))
	if refused.Generation != stableGeneration+1 {
		t.Fatalf("refused producer snapshot did not publish one health edge: got %d want %d", refused.Generation, stableGeneration+1)
	}
	if refused.DeliveryHealth.State != AlertDeliveryHealthDegraded || refused.DeliveryHealth.Class != AlertDeliveryHealthClassObservation {
		t.Fatalf("refused intake stayed invisible in delivery health: %+v", refused.DeliveryHealth)
	}
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base.Add(9*time.Minute), expected, expected, rpc.AlertCoverageCurrent, mismatchedRecovery)); !errors.Is(err, ErrAlertDeliveryInvalidTransition) {
		t.Fatalf("repeated non-exact recovery was accepted: %v", err)
	}
	if got := store.AlertDelivery(base.Add(9 * time.Minute)).Generation; got != refused.Generation {
		t.Fatalf("repeated refusal republished the same health: got %d want %d", got, refused.Generation)
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

func TestAlertDeliveryCompactionAndIndependentAttentionGeneration(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	openedAt := now.Add(-100 * 24 * time.Hour)
	candidate := testAlertCandidate(t, rpc.AlertSourceGovernance, rpc.AlertKindGovernance, "governance", "open-1", openedAt)
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(openedAt, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate))
	if err != nil {
		t.Fatal(err)
	}
	recoveredAt := openedAt.Add(time.Hour)
	recovered := reviseAlertCandidate(candidate, recoveredAt, "e", rpc.AlertEpisodeRecovered, candidate.Severity)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(recoveredAt, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, recovered))
	if err != nil {
		t.Fatal(err)
	}
	legacy := store.Attention()
	attention, err := store.MarkAlertDeliveryAttentionRead(view.Attention.HighWaterSeq)
	if err != nil || attention.UnreadCount != 0 {
		t.Fatalf("v2 attention read failed: %+v err=%v", attention, err)
	}
	readGeneration := store.AlertDelivery(now).Generation
	if !reflect.DeepEqual(store.Attention(), legacy) {
		t.Fatal("v2 attention read changed legacy cursor")
	}
	if err := store.CompactAlertDelivery(now); err != nil {
		t.Fatal(err)
	}
	compacted := store.AlertDelivery(now)
	if len(compacted.Occurrences) != 0 || compacted.Generation != readGeneration+1 || compacted.Attention.HighWaterSeq != 1 || compacted.Attention.ReadThroughSeq != 1 {
		t.Fatalf("compaction/cursor state = %+v", compacted)
	}
}

func TestAlertDeliveryFirstSnapshotSaveFailureIsTypedAndRecoversMonotonically(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 19, 11, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", "cold-start-failure", base)
	store.saveHook = func(string) error { return errors.New("injected first snapshot failure") }
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err == nil {
		t.Fatal("first snapshot persistence failure was ignored")
	}
	failed := store.AlertDelivery(base)
	if failed.Initialized || failed.Generation != 1 || failed.DeliveryHealth.State != AlertDeliveryHealthUnavailable || failed.DeliveryHealth.Class != AlertDeliveryHealthClassStateWrite || !failed.DeliveryHealth.UpdatedAt.Equal(base) {
		t.Fatalf("cold-start failure was hidden: %+v", failed)
	}
	store.saveHook = nil
	recovered, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Initialized || recovered.Generation <= failed.Generation || recovered.DeliveryHealth.State != AlertDeliveryHealthHealthy {
		t.Fatalf("cold-start recovery was not monotonic: failed=%+v recovered=%+v", failed, recovered)
	}
}

func TestAlertDeliveryRecoverySkipsOwnedTransportUnderConcurrentSweeps(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	base := time.Date(2026, 7, 20, 19, 15, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", "owned", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, AlertDeliveryTargetRef("device", "subscription"), base.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("begin send=%v err=%v", send, err)
	}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			if err := store.RecoverAlertDeliveries(base.Add(time.Duration(offset+2) * time.Second)); err != nil {
				t.Errorf("recover: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if attempt := store.data.AlertDelivery.Attempts[0]; attempt.Class != AlertDeliveryAttemptReserved || !attempt.CompletedAt.IsZero() {
		t.Fatalf("owned reservation was recovered: %+v", attempt)
	}
	if _, allowed, err := store.ConfirmAlertTransport(reservation.AttemptID, base.Add(time.Minute)); err != nil || !allowed {
		t.Fatalf("confirm allowed=%v err=%v", allowed, err)
	}
	if _, err := store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionAccepted, base.Add(time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestAlertDeliveryPrerequisiteHealthPersistsAndAutoClears(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	base := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", "prerequisites", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	classes := []string{AlertDeliveryHealthClassNoSubscription, AlertDeliveryHealthClassSigningKeys, AlertDeliveryHealthClassSender}
	for i, healthClass := range classes {
		at := base.Add(time.Duration(i+1) * time.Second)
		if err := store.SetAlertDeliveryPrerequisiteHealth(healthClass, at); err != nil {
			t.Fatal(err)
		}
		view := store.AlertDelivery(at)
		if view.DeliveryHealth.State != AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != healthClass || len(store.AlertDeliveriesDue(at)) != 1 {
			t.Fatalf("class %q view=%+v due=%d", healthClass, view.DeliveryHealth, len(store.AlertDeliveriesDue(at)))
		}
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if health := reopened.AlertDelivery(base.Add(time.Minute)).DeliveryHealth; health.State != AlertDeliveryHealthUnavailable || health.Class != AlertDeliveryHealthClassSender {
		t.Fatalf("prerequisite outage was not durable: %+v", health)
	}
	if err := store.SetAlertDeliveryPrerequisiteHealth("", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if health := store.AlertDelivery(base.Add(time.Minute)).DeliveryHealth; health.State != AlertDeliveryHealthHealthy || health.Class != "" {
		t.Fatalf("prerequisite recovery did not auto-clear: %+v", health)
	}
	if err := store.SetAlertDeliveryPrerequisiteHealth("free_text", base.Add(2*time.Minute)); err == nil {
		t.Fatal("unallowlisted prerequisite class was accepted")
	}
}

func TestAlertDeliveryPersistedPublicEnumsRejectTamperingOnReopen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*alertDeliveryData, time.Time)
	}{
		{
			name: "arbitrary degraded health class",
			mutate: func(data *alertDeliveryData, at time.Time) {
				data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthDegraded, Class: "raw transport prose", UpdatedAt: at}
			},
		},
		{
			name: "arbitrary occurrence end reason",
			mutate: func(data *alertDeliveryData, at time.Time) {
				data.Occurrences[0].EndedAt = at
				data.Occurrences[0].EndReason = "raw producer prose"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 7, 20, 20, 15, 0, 0, time.UTC)
			candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", tc.name, at)
			if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(at, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
				t.Fatal(err)
			}
			tc.mutate(store.data.AlertDelivery, at.Add(time.Second))
			if err := store.save(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatalf("alert-only tampering did not isolate cleanly: %v", err)
			}
			assertAlertDeliveryQuarantined(t, reopened)
		})
	}
}

func TestAlertDeliveryPersistedReceiptAcceptanceCoherence(t *testing.T) {
	for _, mutation := range []string{"accepted without receipt", "receipt without latest acceptance"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			enableTestAlertDelivery(t, store)
			base := time.Date(2026, 7, 20, 20, 20, 0, 0, time.UTC)
			candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", mutation, base)
			if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
				t.Fatal(err)
			}
			reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, AlertDeliveryTargetRef("device", "subscription"), base.Add(time.Second))
			if err != nil || !send {
				t.Fatalf("begin send=%v err=%v", send, err)
			}
			if _, allowed, err := store.ConfirmAlertTransport(reservation.AttemptID, base.Add(2*time.Second)); err != nil || !allowed {
				t.Fatalf("confirm allowed=%v err=%v", allowed, err)
			}
			if _, err := store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionAccepted, base.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if mutation == "accepted without receipt" {
				store.data.AlertDelivery.Receipts = nil
			} else {
				store.data.AlertDelivery.Attempts[0].Class = AlertDeliveryAttemptRejected
			}
			if err := store.save(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatalf("incoherent transport truth did not isolate cleanly: %v", err)
			}
			assertAlertDeliveryQuarantined(t, reopened)
		})
	}
}

func TestAlertDeliveryPersistedAttemptSequenceAndIdentityRejectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Store, int)
	}{
		{name: "first number is not one", mutate: func(store *Store, index int) {
			a := &store.data.AlertDelivery.Attempts[index]
			a.AttemptNumber = 2
			a.ID = alertDeliveryAttemptID(a.ReceiptKey, a.AttemptNumber, a.ReservedAt)
		}},
		{name: "number gap", mutate: func(store *Store, index int) {
			a := &store.data.AlertDelivery.Attempts[index]
			a.AttemptNumber = 3
			a.ID = alertDeliveryAttemptID(a.ReceiptKey, a.AttemptNumber, a.ReservedAt)
		}},
		{name: "persisted order reversed", mutate: func(store *Store, _ int) { slices.Reverse(store.data.AlertDelivery.Attempts) }},
		{name: "deterministic id mismatch", mutate: func(store *Store, index int) {
			a := &store.data.AlertDelivery.Attempts[index]
			a.ID = alertDeliveryAttemptID(a.ReceiptKey, a.AttemptNumber, a.ReservedAt.Add(time.Second))
		}},
		{name: "terminal predecessor", mutate: func(store *Store, _ int) {
			a := &store.data.AlertDelivery.Attempts[0]
			a.Class = AlertDeliveryAttemptRejected
			a.RetryAt = time.Time{}
			a.Disposition = AlertDeliveryCompletionApplied
		}},
		{name: "successor precedes retry", mutate: func(store *Store, index int) {
			a := &store.data.AlertDelivery.Attempts[index]
			a.ReservedAt = store.data.AlertDelivery.Attempts[index-1].RetryAt.Add(-time.Second)
			a.ID = alertDeliveryAttemptID(a.ReceiptKey, a.AttemptNumber, a.ReservedAt)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			number := 2
			if tc.name == "first number is not one" || tc.name == "deterministic id mismatch" {
				number = 1
			}
			_, store, index := newAlertDeliveryAttemptValidationFixture(t, AlertDeliveryAttemptReserved, number, "", false)
			tc.mutate(store, index)
			if err := store.validateAlertDeliveryState(); !errors.Is(err, ErrInvalidPersistedState) {
				t.Fatalf("corrupt attempt sequence validated: %v", err)
			}
		})
	}
}

func newAlertDeliveryAttemptValidationFixture(t *testing.T, class string, number int, disposition AlertDeliveryCompletionDisposition, retired bool) (string, *Store, int) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "validator", class, base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	target := AlertDeliveryTargetRef("validator-device", "validator-subscription")
	receiptKey := alertDeliveryReceiptKey(defaultTestAlertAuthorityScope, candidate.OccurrenceKey, target)
	reservedAt := base.Add(time.Second)
	attempts := make([]alertDeliveryAttempt, 0, number)
	for attemptNumber := 1; attemptNumber < number; attemptNumber++ {
		completedAt := reservedAt.Add(time.Second)
		delay, ok := alertDeliveryRetryDelay(attemptNumber)
		if !ok {
			t.Fatalf("invalid predecessor number %d", attemptNumber)
		}
		attempts = append(attempts, alertDeliveryAttempt{
			AuthorityScope: defaultTestAlertAuthorityScope,
			ID:             alertDeliveryAttemptID(receiptKey, attemptNumber, reservedAt), OccurrenceKey: candidate.OccurrenceKey,
			TargetRef: target, ReceiptKey: receiptKey, AttemptNumber: attemptNumber, ReservedAt: reservedAt,
			CompletedAt: completedAt, Class: AlertDeliveryAttemptRetry, Disposition: AlertDeliveryCompletionApplied,
			RetryAt: completedAt.Add(delay),
		})
		reservedAt = completedAt.Add(delay)
	}
	final := alertDeliveryAttempt{
		AuthorityScope: defaultTestAlertAuthorityScope,
		ID:             alertDeliveryAttemptID(receiptKey, number, reservedAt), OccurrenceKey: candidate.OccurrenceKey,
		TargetRef: target, ReceiptKey: receiptKey, AttemptNumber: number, ReservedAt: reservedAt,
		Class: class, Disposition: disposition,
	}
	completedAt := reservedAt.Add(time.Second)
	if class != AlertDeliveryAttemptReserved && class != AlertDeliveryAttemptConfirmed {
		final.CompletedAt = completedAt
	}
	if retired {
		final.RetiredAt = completedAt.Add(time.Second)
		store.data.AlertDelivery.RetiredTargets[target] = final.RetiredAt
	}
	switch class {
	case AlertDeliveryAttemptConfirmed:
		if retired {
			final.Disposition = AlertDeliveryCompletionRetired
		}
	case AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRejected:

	case AlertDeliveryAttemptRetry:
		if disposition == "" || disposition == AlertDeliveryCompletionApplied {
			delay, _ := alertDeliveryRetryDelay(number)
			final.RetryAt = completedAt.Add(delay)
		}
	case AlertDeliveryAttemptInterrupted:
		if disposition == "" {
			if delay, ok := alertDeliveryRetryDelay(number); ok {
				final.RetryAt = completedAt.Add(delay)
			}
		}
	case AlertDeliveryAttemptRetired:
		final.Disposition = AlertDeliveryCompletionRetired
	case AlertDeliveryAttemptInactive:
		final.Disposition = AlertDeliveryCompletionInactive
	}
	attempts = append(attempts, final)
	if retired {
		for i := range attempts[:len(attempts)-1] {
			attempts[i].Class = AlertDeliveryAttemptRetired
			attempts[i].CompletedAt = final.RetiredAt
			attempts[i].RetryAt = time.Time{}
			attempts[i].RetiredAt = final.RetiredAt
			attempts[i].Disposition = AlertDeliveryCompletionRetired
		}
	}
	store.data.AlertDelivery.Attempts = attempts
	store.data.AlertDelivery.Receipts = nil
	if class == AlertDeliveryAttemptAccepted {
		store.data.AlertDelivery.Receipts = []alertDeliveryReceipt{{
			AuthorityScope: defaultTestAlertAuthorityScope,
			OccurrenceKey:  candidate.OccurrenceKey, TargetRef: target, ReceiptKey: receiptKey,
			AcceptedAt: final.CompletedAt, RetiredAt: final.RetiredAt,
		}}
	}
	if err := store.validateAlertDeliveryState(); err != nil {
		t.Fatalf("invalid test fixture: %v\n%+v", err, final)
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	return dir, store, len(attempts) - 1
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

func occurrenceBySource(t *testing.T, view AlertDeliveryView, source rpc.AlertSource) AlertDeliveryOccurrenceView {
	t.Helper()
	for _, occurrence := range slices.Backward(view.Occurrences) {
		if occurrence.Source == source {
			return occurrence
		}
	}
	t.Fatalf("occurrence for source %s not found: %+v", source, view.Occurrences)
	return AlertDeliveryOccurrenceView{}
}

func occurrenceByDisplay(t *testing.T, view AlertDeliveryView, displayID string) AlertDeliveryOccurrenceView {
	t.Helper()
	for _, occurrence := range view.Occurrences {
		if occurrence.DisplayID == displayID {
			return occurrence
		}
	}
	t.Fatalf("occurrence %s not found: %+v", displayID, view.Occurrences)
	return AlertDeliveryOccurrenceView{}
}
