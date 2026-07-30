package alerts

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestDispatcherObserveConfirmsSendsAndDeduplicates(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	ensureAlertKeys(t, store, base)

	sender := &recordingSender{results: []state.PushAttempt{{OK: true, Class: state.GovernanceTransportAccepted}}}
	sender.onSend = func(_ int, _ state.PushSubscription, payload push.Payload) {
		view := store.AlertDelivery(base.Add(time.Minute))
		if view.AttemptTotals.Confirmed != 1 {
			t.Errorf("send ran without one durable confirmed attempt: %+v", view.AttemptTotals)
		}
		if payload.AlertID != "" || payload.Action != "" {
			t.Errorf("legacy payload fields escaped the allowlist: %+v", payload)
		}
	}
	now := base.Add(time.Minute)
	dispatcher := Dispatcher{Store: store, Sender: sender, URL: "https://app.example/alerts", Now: func() time.Time { return now }}
	candidate := alertCandidate(t, rpc.AlertPresentationPortfolioStress, "opening-one", now)
	snapshot := alertSnapshot(t, now, candidate)

	view, err := dispatcher.Observe(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if sender.callCount() != 1 || view.AttemptTotals.Accepted != 1 || view.AttemptTotals.Confirmed != 0 {
		t.Fatalf("unexpected accepted delivery result: calls=%d totals=%+v", sender.callCount(), view.AttemptTotals)
	}
	if view.DeliveryHealth.State != state.AlertDeliveryHealthHealthy || view.DeliveryHealth.Class != "" {
		t.Fatalf("accepted delivery did not leave healthy state: %+v", view.DeliveryHealth)
	}
	payload := sender.payloadAt(t, 0)
	if payload.Title != "Portfolio stress" || payload.DisplayID == "" || payload.Severity != string(rpc.AlertSeverityWatch) {
		t.Fatalf("unexpected fixed payload: %+v", payload)
	}
	if strings.Contains(payload.Title+payload.Body, candidate.OccurrenceKey) || strings.Contains(payload.Title+payload.Body, candidate.EvidenceFingerprint) {
		t.Fatal("private candidate identity escaped into push copy")
	}

	if _, err := dispatcher.Observe(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sender.callCount() != 1 {
		t.Fatalf("accepted occurrence-target pair was sent again: %d calls", sender.callCount())
	}
}

func TestDispatcherUsesCandidateReturnedByFinalConfirm(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	addAlertTarget(t, store, "device-two", "subscription-two", base)
	ensureAlertKeys(t, store, base)

	now := base.Add(time.Minute)
	initial := alertCandidateForSource(t, rpc.AlertSourceRulebook, rpc.AlertKindPolicyDrift,
		rpc.AlertPresentationRulebookSingleNameExposure, "opening-revision", now)
	revisedAt := now.Add(time.Minute)
	revised := initial
	revised.EvidenceFingerprint = "sha256:" + strings.Repeat("b", 64)
	revised.PresentationCode = rpc.AlertPresentationRulebookFXExposure
	revised.Severity = rpc.AlertSeverityAct
	revised.EvidenceAsOf = revisedAt
	revised.ObservedAt = revisedAt

	sender := &recordingSender{results: []state.PushAttempt{
		{OK: true, Class: state.GovernanceTransportAccepted},
		{OK: true, Class: state.GovernanceTransportAccepted},
	}}
	sender.onSend = func(index int, _ state.PushSubscription, _ push.Payload) {
		if index != 0 {
			return
		}
		now = revisedAt
		if _, err := store.ObserveAlertSnapshot(alertSnapshot(t, revisedAt, revised)); err != nil {
			t.Errorf("revise candidate between target confirmations: %v", err)
		}
	}
	dispatcher := Dispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now, initial)); err != nil {
		t.Fatal(err)
	}
	if sender.callCount() != 2 {
		t.Fatalf("expected both active targets, got %d sends", sender.callCount())
	}
	first, second := sender.payloadAt(t, 0), sender.payloadAt(t, 1)
	if first.Title != "Single-name exposure" || first.Severity != string(rpc.AlertSeverityWatch) {
		t.Fatalf("first target did not use its confirmed candidate: %+v", first)
	}
	if second.Title != "Currency exposure" || second.Severity != string(rpc.AlertSeverityAct) {
		t.Fatalf("second target reused stale due-work instead of final confirmation: %+v", second)
	}
}

func TestDispatcherRecordsTypedPrerequisiteHealth(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		addTarget bool
		addKeys   bool
		sender    push.Sender
		class     string
	}{
		{name: "no active subscription", addKeys: true, sender: &recordingSender{}, class: state.AlertDeliveryHealthClassNoSubscription},
		{name: "signing keys unavailable", addTarget: true, sender: &recordingSender{}, class: state.AlertDeliveryHealthClassSigningKeys},
		{name: "sender unavailable", addTarget: true, addKeys: true, class: state.AlertDeliveryHealthClassSender},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newAlertStore(t, t.TempDir(), base)
			if tc.addTarget {
				addAlertTarget(t, store, "device-one", "subscription-one", base)
			}
			if tc.addKeys {
				ensureAlertKeys(t, store, base)
			}
			now := base.Add(time.Minute)
			dispatcher := Dispatcher{Store: store, Sender: tc.sender, Now: func() time.Time { return now }}
			view, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now,
				alertCandidate(t, rpc.AlertPresentationPortfolioStress, tc.name, now)))
			if err != nil {
				t.Fatal(err)
			}
			if view.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != tc.class {
				t.Fatalf("wrong prerequisite health: %+v", view.DeliveryHealth)
			}
		})
	}
}

func TestDispatcherKeepsPrerequisiteHealthTruthfulWhenNoWorkRemains(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	ensureAlertKeys(t, store, base)
	now := base.Add(time.Minute)
	candidate := alertCandidate(t, rpc.AlertPresentationPortfolioStress, "recover-after-prerequisite", now)
	dispatcher := Dispatcher{Store: store, Sender: &recordingSender{}, Now: func() time.Time { return now }}

	view, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if view.DeliveryHealth.Class != state.AlertDeliveryHealthClassNoSubscription {
		t.Fatalf("prerequisite outage not established: %+v", view.DeliveryHealth)
	}

	now = now.Add(time.Minute)
	recovered := candidate
	recovered.State = rpc.AlertEpisodeRecovered
	recovered.EvidenceFingerprint = "sha256:" + strings.Repeat("c", 64)
	recovered.StateChangedAt = now
	recovered.EvidenceAsOf = now
	recovered.ObservedAt = now
	view, err = dispatcher.Observe(context.Background(), alertSnapshot(t, now, recovered))
	if err != nil {
		t.Fatal(err)
	}
	if view.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != state.AlertDeliveryHealthClassNoSubscription {
		t.Fatalf("idle active mode hid its missing subscription: %+v", view.DeliveryHealth)
	}

	if err := dispatcher.SetAlertMode(state.AlertModeNone); err != nil {
		t.Fatal(err)
	}
	view, err = dispatcher.DispatchPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.DeliveryHealth.State == state.AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != "" {
		t.Fatalf("disabled mode was reported as a prerequisite outage: %+v", view.DeliveryHealth)
	}
}

func TestDispatcherReportsIdlePrerequisiteHealth(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 11, 30, 0, 0, time.UTC)
	tests := []struct {
		name      string
		addTarget bool
		addKeys   bool
		sender    push.Sender
		class     string
	}{
		{name: "no active subscription", addKeys: true, sender: &recordingSender{}, class: state.AlertDeliveryHealthClassNoSubscription},
		{name: "signing keys unavailable", addTarget: true, sender: &recordingSender{}, class: state.AlertDeliveryHealthClassSigningKeys},
		{name: "sender unavailable", addTarget: true, addKeys: true, class: state.AlertDeliveryHealthClassSender},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newAlertStore(t, t.TempDir(), base)
			if tc.addTarget {
				addAlertTarget(t, store, "device-one", "subscription-one", base)
			}
			if tc.addKeys {
				ensureAlertKeys(t, store, base)
			}
			now := base.Add(time.Minute)
			dispatcher := Dispatcher{Store: store, Sender: tc.sender, Now: func() time.Time { return now }}
			view, err := dispatcher.DispatchPending(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if view.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != tc.class {
				t.Fatalf("idle readiness is not truthful: %+v", view.DeliveryHealth)
			}
		})
	}
}

func TestControllerMutationsWaitForConfirmedTransport(t *testing.T) {
	base := time.Date(2026, 7, 22, 11, 45, 0, 0, time.UTC)
	tests := []struct {
		name      string
		mutate    func(*Dispatcher) error
		committed func(*state.Store) bool
	}{
		{
			name: "disable alerts",
			mutate: func(dispatcher *Dispatcher) error {
				return dispatcher.SetAlertMode(state.AlertModeNone)
			},
			committed: func(store *state.Store) bool {
				return store.AlertSettings().Mode == state.AlertModeNone
			},
		},
		{
			name: "retire target",
			mutate: func(dispatcher *Dispatcher) error {
				return dispatcher.RemovePushSubscription("subscription-one")
			},
			committed: func(store *state.Store) bool {
				return len(store.ActivePushSubscriptions()) == 0
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newAlertStore(t, t.TempDir(), base)
			addAlertTarget(t, store, "device-one", "subscription-one", base)
			ensureAlertKeys(t, store, base)
			now := base.Add(time.Minute)
			sender := newBlockingSender()
			dispatcher := &Dispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
			snapshot := alertSnapshot(t, now,
				alertCandidate(t, rpc.AlertPresentationPortfolioStress, tc.name, now))

			observeDone := make(chan error, 1)
			go func() {
				_, err := dispatcher.Observe(context.Background(), snapshot)
				observeDone <- err
			}()
			<-sender.entered
			if view := store.AlertDelivery(now); view.AttemptTotals.Confirmed != 1 {
				t.Fatalf("sender did not block after durable confirmation: %+v", view.AttemptTotals)
			}

			mutationStarted := make(chan struct{})
			mutationDone := make(chan error, 1)
			go func() {
				close(mutationStarted)
				mutationDone <- tc.mutate(dispatcher)
			}()
			<-mutationStarted
			select {
			case err := <-mutationDone:
				t.Fatalf("mutation committed while confirmed transport was blocked: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if tc.committed(store) {
				t.Fatal("mode or target mutation became visible before transport finished")
			}

			close(sender.release)
			if err := <-observeDone; err != nil {
				t.Fatalf("observe: %v", err)
			}
			if err := <-mutationDone; err != nil {
				t.Fatalf("mutation: %v", err)
			}
			select {
			case <-sender.finished:
			default:
				t.Fatal("mutation committed before transport returned")
			}
			if !tc.committed(store) {
				t.Fatal("serialized mutation did not commit after transport finished")
			}
		})
	}
}

func TestSafeDiagnosticRetiresDeadTargetThroughController(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 11, 50, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	ensureAlertKeys(t, store, base)
	dispatcher := Dispatcher{
		Store:  store,
		Sender: &recordingSender{results: []state.PushAttempt{{Class: state.GovernanceTransportDead}}},
		Now:    func() time.Time { return base.Add(time.Minute) },
	}
	status, accepted, err := dispatcher.SendSafeDiagnostic(context.Background(), "device-one")
	if err != nil {
		t.Fatal(err)
	}
	if accepted || status.State != state.GovernanceTransportDead {
		t.Fatalf("unexpected diagnostic result: status=%+v accepted=%v", status, accepted)
	}
	if len(store.ActivePushSubscriptions()) != 0 {
		t.Fatal("dead diagnostic target remained active")
	}
}

func TestDispatcherRetriesAfterRestartAndRetainsDedupe(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := newAlertStore(t, dir, base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	ensureAlertKeys(t, store, base)
	now := base.Add(time.Minute)
	retrying := &recordingSender{results: []state.PushAttempt{{Class: state.GovernanceTransportNetworkRetry}}}
	dispatcher := Dispatcher{Store: store, Sender: retrying, Now: func() time.Time { return now }}
	candidate := alertCandidate(t, rpc.AlertPresentationPortfolioStress, "restart-retry", now)

	view, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if view.AttemptTotals.RetryPending != 1 || view.DeliveryHealth.Class != state.AlertDeliveryHealthClassRetry {
		t.Fatalf("retry evidence not durable before restart: totals=%+v health=%+v", view.AttemptTotals, view.DeliveryHealth)
	}

	restarted, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	accepted := &recordingSender{results: []state.PushAttempt{{OK: true, Class: state.GovernanceTransportAccepted}}}
	retryDispatcher := Dispatcher{Store: restarted, Sender: accepted, Now: func() time.Time { return now }}
	view, err = retryDispatcher.DispatchPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accepted.callCount() != 1 || view.AttemptTotals.Attempts != 2 || view.AttemptTotals.Accepted != 1 || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("restart retry did not converge: calls=%d totals=%+v", accepted.callCount(), view.AttemptTotals)
	}
	if _, err := retryDispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accepted.callCount() != 1 {
		t.Fatalf("accepted retry was sent again: %d calls", accepted.callCount())
	}
}

func TestDispatcherRetiresDeadSubscription(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	ensureAlertKeys(t, store, base)
	now := base.Add(time.Minute)
	sender := &recordingSender{results: []state.PushAttempt{{Class: state.GovernanceTransportDead}}}
	dispatcher := Dispatcher{
		Store: store, Sender: sender,
		Now: func() time.Time { return now },
	}
	view, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now,
		alertCandidate(t, rpc.AlertPresentationPortfolioStress, "dead-target", now)))
	if err != nil {
		t.Fatal(err)
	}
	if len(store.ActivePushSubscriptions()) != 0 || view.AttemptTotals.Rejected != 1 {
		t.Fatalf("dead target was not atomically retired: active=%d totals=%+v", len(store.ActivePushSubscriptions()), view.AttemptTotals)
	}
	if _, err := dispatcher.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.callCount() != 1 {
		t.Fatalf("retired dead target was retried: %d calls", sender.callCount())
	}
}

func TestDispatcherFinalizesConfirmedAttemptWhenFixedCopyIsUnavailable(t *testing.T) {
	base := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	store := newAlertStore(t, t.TempDir(), base)
	addAlertTarget(t, store, "device-one", "subscription-one", base)
	ensureAlertKeys(t, store, base)
	now := base.Add(time.Minute)
	code := rpc.AlertPresentationPortfolioStress
	fixed := presentations[code]
	delete(presentations, code)
	defer func() { presentations[code] = fixed }()

	sender := &recordingSender{}
	dispatcher := Dispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(context.Background(), alertSnapshot(t, now,
		alertCandidate(t, code, "missing-fixed-copy", now))); err == nil {
		t.Fatal("missing fixed copy did not fail closed")
	}
	view := store.AlertDelivery(now)
	if sender.callCount() != 0 || view.AttemptTotals.Confirmed != 0 || view.AttemptTotals.Rejected != 1 {
		t.Fatalf("confirmed attempt was not finalized after copy failure: calls=%d totals=%+v", sender.callCount(), view.AttemptTotals)
	}
}

func TestPresentationForCoversClosedCodesAndStates(t *testing.T) {
	t.Parallel()
	codes := []rpc.AlertPresentationCode{
		rpc.AlertPresentationPortfolioStress,
		rpc.AlertPresentationMarginCushion,
		rpc.AlertPresentationRegimeMarketStress,
		rpc.AlertPresentationRulebookSingleNameExposure,
		rpc.AlertPresentationRulebookOptionLinePremium,
		rpc.AlertPresentationRulebookCashSellOnly,
		rpc.AlertPresentationRulebookExtrinsicBudget,
		rpc.AlertPresentationRulebookExpiryRunway,
		rpc.AlertPresentationRulebookCatalystCoverage,
		rpc.AlertPresentationRulebookOverwriteEarnings,
		rpc.AlertPresentationRulebookEarningsSizeFreeze,
		rpc.AlertPresentationRulebookRedOnGreen,
		rpc.AlertPresentationRulebookWinnerTrim,
		rpc.AlertPresentationRulebookGreenDayAction,
		rpc.AlertPresentationRulebookHedgeIntegrity,
		rpc.AlertPresentationRulebookExitDiscipline,
		rpc.AlertPresentationRulebookFXExposure,
		rpc.AlertPresentationProtectionOrphanedOrder,
		rpc.AlertPresentationProtectionReconciliationRequired,
		rpc.AlertPresentationOrderIntegrityMismatch,
		rpc.AlertPresentationDataHealthGateway,
		rpc.AlertPresentationDataHealthStorage,
		rpc.AlertPresentationDataHealthProposals,
		rpc.AlertPresentationDataHealthOpportunities,
		rpc.AlertPresentationDataHealthDataFarms,
		rpc.AlertPresentationDataHealthRegime,
		rpc.AlertPresentationDataHealthGamma,
		rpc.AlertPresentationDataHealthQuality,
		rpc.AlertPresentationRiskPolicyLimitWouldBlock,
		rpc.AlertPresentationRiskPolicyDrawdownLatched,
		rpc.AlertPresentationRiskPolicyDrift,
		rpc.AlertPresentationReconciliationDue,
		rpc.AlertPresentationReconciliationException,
		rpc.AlertPresentationReconciliationConfirmedFlow,
		rpc.AlertPresentationGovernanceMonthlyPulse,
		rpc.AlertPresentationDeliveryHealth,
		rpc.AlertPresentationRulebookLegacyCondition,
		rpc.AlertPresentationRiskPolicyLegacyCondition,
		rpc.AlertPresentationReconciliationLegacyCondition,
		rpc.AlertPresentationGovernanceLegacyCondition,
	}
	states := []rpc.AlertEpisodeState{rpc.AlertEpisodeOpen, rpc.AlertEpisodeEscalated, rpc.AlertEpisodeRecovered}
	for _, code := range codes {
		for _, episodeState := range states {
			presentation, ok := PresentationFor(code, episodeState)
			if !ok || strings.TrimSpace(presentation.Title) == "" || strings.TrimSpace(presentation.Body) == "" {
				t.Fatalf("closed presentation code %q state %q has no fixed copy: %+v", code, episodeState, presentation)
			}
		}
	}
	if len(codes) != len(presentations) {
		t.Fatalf("coverage list and fixed presentation table diverged: tested=%d table=%d", len(codes), len(presentations))
	}
	if presentation, ok := PresentationFor("producer_supplied_unknown", rpc.AlertEpisodeOpen); ok || presentation != (Presentation{}) {
		t.Fatalf("unknown producer code did not fail closed: %+v, %v", presentation, ok)
	}
	if presentation, ok := PresentationFor(rpc.AlertPresentationPortfolioStress, "producer_supplied_state"); ok || presentation != (Presentation{}) {
		t.Fatalf("unknown lifecycle state did not fail closed: %+v, %v", presentation, ok)
	}
}

type recordingSender struct {
	mu      sync.Mutex
	results []state.PushAttempt
	payload []push.Payload
	calls   int
	onSend  func(int, state.PushSubscription, push.Payload)
}

type blockingSender struct {
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func newBlockingSender() *blockingSender {
	return &blockingSender{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
}

func (s *blockingSender) Send(_ context.Context, subscription state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	close(s.entered)
	<-s.release
	close(s.finished)
	return state.PushAttempt{SubscriptionID: subscription.ID, OK: true, Class: state.GovernanceTransportAccepted}
}

func (s *recordingSender) Send(_ context.Context, subscription state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.payload = append(s.payload, payload)
	result := state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}
	if index < len(s.results) {
		result = s.results[index]
	}
	hook := s.onSend
	s.mu.Unlock()
	if hook != nil {
		hook(index, subscription, payload)
	}
	return result
}

func (s *recordingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *recordingSender) payloadAt(t *testing.T, index int) push.Payload {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.payload) {
		t.Fatalf("payload %d not found in %d sends", index, len(s.payload))
	}
	return s.payload[index]
}

func newAlertStore(t *testing.T, dir string, baselineAt time.Time) *state.Store {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(state.AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveAlertSnapshot(alertSnapshot(t, baselineAt)); err != nil {
		t.Fatalf("establish alert cutover baseline: %v", err)
	}
	return store
}

func addAlertTarget(t *testing.T, store *state.Store, deviceID, subscriptionID string, at time.Time) {
	t.Helper()
	if err := store.AddDevice(state.DeviceGrant{ID: deviceID, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(state.PushSubscription{
		ID: subscriptionID, DeviceID: deviceID, Endpoint: "https://push.example/" + subscriptionID,
		P256DH: "p256dh", Auth: "auth", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func ensureAlertKeys(t *testing.T, store *state.Store, at time.Time) {
	t.Helper()
	if _, err := store.EnsureVAPID(at, func() (string, string, error) { return "private-key", "public-key", nil }); err != nil {
		t.Fatal(err)
	}
}

func alertCandidate(t *testing.T, code rpc.AlertPresentationCode, opening string, at time.Time) rpc.AlertCandidate {
	t.Helper()
	return alertCandidateForSource(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, code, opening, at)
}

func alertCandidateForSource(t *testing.T, source rpc.AlertSource, kind rpc.AlertKind, code rpc.AlertPresentationCode, opening string, at time.Time) rpc.AlertCandidate {
	t.Helper()
	episodeKey, err := rpc.BuildAlertEpisodeKey(source, kind, "dispatcher-test-episode")
	if err != nil {
		t.Fatal(err)
	}
	occurrenceKey, err := rpc.BuildAlertOccurrenceKey(episodeKey, opening)
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidate{
		EpisodeKey: episodeKey, OccurrenceKey: occurrenceKey, EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source: source, Kind: kind, PresentationCode: code, State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityWatch,
		EvidenceHealth: rpc.AlertEvidenceCurrent, Destination: rpc.AlertDestinationAlerts,
		EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}
}

func alertSnapshot(t *testing.T, at time.Time, candidates ...rpc.AlertCandidate) rpc.AlertCandidateSnapshot {
	t.Helper()
	scope, err := rpc.BuildAlertAuthorityScope("TEST-ACCOUNT", "paper")
	if err != nil {
		t.Fatal(err)
	}
	source := rpc.AlertSourceStress
	if len(candidates) > 0 {
		source = candidates[0].Source
		for _, candidate := range candidates[1:] {
			if candidate.Source != source {
				t.Fatal("test snapshot helper supports one source")
			}
		}
	}
	currentState := rpc.AlertSnapshotClear
	for _, candidate := range candidates {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			currentState = rpc.AlertSnapshotActive
		}
	}
	candidateRows := make([]rpc.AlertCandidate, len(candidates))
	copy(candidateRows, candidates)
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: scope, AsOf: at, CurrentState: currentState,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
			ExpectedSources: []rpc.AlertSource{source}, CoveredSources: []rpc.AlertSource{source},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source: source, Status: "current", Reason: "current", EvidenceHealth: rpc.AlertEvidenceCurrent,
			InputAsOf: at, ObservedAt: at, EvidenceAsOf: at, FreshUntil: at.Add(time.Hour), Covered: true,
		}},
		Candidates: candidateRows,
	}
}
