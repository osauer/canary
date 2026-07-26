package daemon

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAlertShadowNudgeHeartbeatObservesAllOwnersWithoutNudgeMutation(t *testing.T) {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	server := newV4NudgeTestServer(t, now)
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })

	server.nudges.mu.Lock()
	server.nudges.loadLocked()
	before := cloneNudgeStateForTest(t, server.nudges.state)
	server.nudges.mu.Unlock()

	server.observeNudgesAlertShadowHeartbeat(t.Context())

	server.nudges.mu.Lock()
	after := cloneNudgeStateForTest(t, server.nudges.state)
	server.nudges.mu.Unlock()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only Nudge heartbeat mutated governance state: before=%+v after=%+v", before, after)
	}

	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	status := server.alertShadow.Status(scope)
	for _, source := range []rpc.AlertSource{
		rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	} {
		got := alertShadowTestSourceStatus(t, status, source)
		if got.Status == alertShadowStatusNotObserved || got.Measurements.Evaluations != 1 {
			t.Fatalf("Nudge owner %s was not observed by the heartbeat: %+v", source, got)
		}
	}
}

func TestAlertShadowRulebookHeartbeatIsUnfilteredReadOnlyAndFailClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, time.Now)
	server.rulesRegimeStage = rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Stage: rpc.LifecycleQuiet,
		Bucket: bucketRegimeStage(rpc.LifecycleQuiet), AsOf: now,
	}
	prior := &rpc.RulesResult{AsOf: now.Add(-time.Minute), Status: "sentinel"}
	server.lastRules = prior

	server.observeRulebookAlertShadowHeartbeat(t.Context())

	if server.lastRules != prior || !server.lastRulesAt.IsZero() {
		t.Fatalf("heartbeat replaced the rulebook cache/journal baseline: last=%p want=%p at=%s", server.lastRules, prior, server.lastRulesAt)
	}
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	got := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if got.Status == alertShadowStatusNotObserved || got.Measurements.Evaluations != 1 {
		t.Fatalf("Rulebook heartbeat was not observed: %+v", got)
	}
	if got.Covered || got.Status == alertShadowStatusCurrent {
		t.Fatalf("gateway-unavailable rulebook read fabricated a current negative: %+v", got)
	}
}

func TestAlertShadowRulebookHeartbeatIsCacheOnlyDuringBusyCanonicalEvaluation(t *testing.T) {
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, time.Now)
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}

	server.rulesEvaluationMu.Lock()
	server.observeRulebookAlertShadowHeartbeat(t.Context())
	server.rulesEvaluationMu.Unlock()

	got := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if got.Status != alertShadowStatusUnavailable || got.Covered || got.Measurements.Evaluations != 1 {
		t.Fatalf("cache-only heartbeat did not publish explicit uncovered state: %+v", got)
	}
}

func TestRulebookCanonicalRefreshAnchorsToLastSuccessfulPublish(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
	binding := server.currentRulebookBinding()
	evaluations := 0
	evaluate := func(_ context.Context, includeTape, allowMaintenance bool) *rpc.RulesResult {
		if !includeTape || !allowMaintenance {
			t.Fatalf("canonical flags includeTape=%t allowMaintenance=%t", includeTape, allowMaintenance)
		}
		if server.rulesEvaluationMu.TryLock() {
			server.rulesEvaluationMu.Unlock()
			t.Fatal("refresh evaluator ran outside the single-flight")
		}
		evaluations++
		result := alertShadowTestRulebook(now, risk.RuleStatusPass)
		return &result
	}

	if !server.refreshRulebookCanonicalCacheWith(t.Context(), evaluate) || evaluations != 1 {
		t.Fatalf("initial daemon refresh evaluations=%d", evaluations)
	}
	now = now.Add(30 * time.Second)
	if !server.refreshRulebookCanonicalCacheWith(t.Context(), evaluate) || evaluations != 1 {
		t.Fatalf("phase-offset tick duplicated fresh canonical read: evaluations=%d", evaluations)
	}

	// Simulate any other canonical publisher (app/CLI/preview) halfway through
	// the minute. Its completion becomes the new due-time anchor.
	appResult := alertShadowTestRulebook(now, risk.RuleStatusPass)
	if !server.publishCanonicalRulebookResult(t.Context(), &appResult, binding) {
		t.Fatal("phase-offset app canonical publish failed")
	}
	now = now.Add(30 * time.Second)
	if !server.refreshRulebookCanonicalCacheWith(t.Context(), evaluate) || evaluations != 1 {
		t.Fatalf("fixed-phase daemon tick ignored app publish anchor: evaluations=%d", evaluations)
	}
	now = now.Add(30 * time.Second)
	if !server.refreshRulebookCanonicalCacheWith(t.Context(), evaluate) || evaluations != 2 {
		t.Fatalf("refresh did not become due one minute after latest success: evaluations=%d", evaluations)
	}
}

func TestRulebookQueuedInteractiveReadReusesBackgroundPublication(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
	binding := server.currentRulebookBinding()
	entered := make(chan struct{})
	release := make(chan struct{})
	backgroundDone := make(chan bool, 1)
	seed := alertShadowTestRulebook(now, risk.RuleStatusPass)
	go func() {
		backgroundDone <- server.refreshRulebookCanonicalCacheWith(t.Context(), func(context.Context, bool, bool) *rpc.RulesResult {
			close(entered)
			<-release
			copyResult := seed
			return &copyResult
		})
	}()
	<-entered

	interactiveDone := make(chan *rpc.RulesResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		interactiveDone <- server.canonicalRulebookResult(ctx, binding)
	}()
	close(release)
	if ok := <-backgroundDone; !ok {
		t.Fatal("background canonical refresh failed")
	}
	interactive := <-interactiveDone
	if interactive == nil || !interactive.AsOf.Equal(seed.AsOf) || interactive.Status != seed.Status {
		t.Fatalf("queued interactive read did not reuse background publication: %+v", interactive)
	}
}

func TestAlertShadowRulebookStaleReceiptHoldsAndCurrentEmptyRecovers(t *testing.T) {
	inputAt := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	observedAt := inputAt.Add(time.Second)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return observedAt })
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}

	observe := func(result rpc.RulesResult) {
		t.Helper()
		server.observeRulebookAlertShadow(t.Context(), &result, server.currentBrokerStateScope())
	}

	observe(alertShadowTestRulebook(inputAt, risk.RuleStatusAct))
	opened := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if opened.Active != 1 || opened.Measurements.EpisodesOpened != 1 {
		t.Fatalf("current breach did not open a rulebook episode: %+v", opened)
	}

	inputAt = inputAt.Add(time.Minute)
	observedAt = inputAt.Add(time.Second)
	stale := alertShadowTestRulebook(inputAt, risk.RuleStatusPass)
	stale.Status = "degraded"
	for i := range stale.InputHealth {
		if stale.InputHealth[i].Source == "positions" {
			stale.InputHealth[i].Status = rpc.SourceStatusStale
			stale.InputHealth[i].AsOf = inputAt.Add(-portfolioStreamReceiptMaxAge - time.Second)
		}
	}
	observe(stale)
	held := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if held.Active != 1 || held.Covered || held.Status != alertShadowStatusStale || held.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("stale cached portfolio cleared or covered the rulebook episode: %+v", held)
	}

	inputAt = inputAt.Add(time.Minute)
	observedAt = inputAt.Add(time.Second)
	observe(alertShadowTestRulebook(inputAt, risk.RuleStatusPass))
	recovered := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if recovered.Active != 0 || !recovered.Covered || recovered.Status != alertShadowStatusCurrent || recovered.Measurements.EpisodesRecovered != 1 {
		t.Fatalf("current complete empty rulebook result did not recover the episode: %+v", recovered)
	}
}

func TestRulebookCurrentNegativeCannotPublishWithoutReadyBrokerBinding(t *testing.T) {
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	observedNow := base.Add(time.Second)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return observedNow })
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.alertShadow.ObserveRulebook(t.Context(), scope, alertShadowTestRulebook(base, risk.RuleStatusAct)); err != nil {
		t.Fatal(err)
	}
	unboundClear := alertShadowTestRulebook(base.Add(30*time.Second), risk.RuleStatusPass)
	observedNow = base.Add(31 * time.Second)
	server.observeRulebookAlertShadowHeartbeatWith(t.Context(), func(context.Context, bool, bool) (*rpc.RulesResult, bool) {
		return &unboundClear, true
	})
	unboundStatus := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if unboundStatus.Active != 1 || unboundStatus.Covered || unboundStatus.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("unbound current-looking heartbeat cleared the Rulebook episode: %+v", unboundStatus)
	}

	server.mu.Lock()
	server.connector = ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	server.connectorEpoch++
	server.mu.Unlock()
	binding := server.currentRulebookBinding()
	if binding.brokerCaptured {
		t.Fatal("disconnected test connector unexpectedly produced ready broker evidence")
	}
	clear := alertShadowTestRulebook(base.Add(time.Minute), risk.RuleStatusPass)
	observedNow = base.Add(time.Minute + time.Second)
	if server.publishCanonicalRulebookResult(t.Context(), &clear, binding) {
		t.Fatal("current negative published without a ready socket/session binding")
	}
	status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
	if status.Active != 1 || status.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("unready broker binding cleared the Rulebook episode: %+v", status)
	}
}

func TestAlertShadowOrderIntegrityHeartbeatRetainsOnFailureAndClearsOnlyCurrentNegative(t *testing.T) {
	inputAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	observedAt := inputAt.Add(time.Second)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return observedAt })
	brokerScope := server.currentBrokerStateScope()
	shadowScope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		t.Fatal(err)
	}
	order := alertShadowTestMismatchedOrder(inputAt, shadowScope)

	observe := func(status string, views []rpc.OrderView, readErr error) {
		t.Helper()
		server.observeOrderIntegrityAlertShadowHeartbeatWith(t.Context(), func(context.Context) ([]rpc.OrderView, orderIntegrityEvaluation, error) {
			return views, orderIntegrityEvaluation{
				AsOf: inputAt, EvidenceAsOf: inputAt, Status: status, Scope: brokerScope,
			}, readErr
		})
	}

	observe(orderIntegrityHealthCurrent, []rpc.OrderView{order}, nil)
	inputAt = inputAt.Add(30 * time.Second)
	observedAt = inputAt.Add(time.Second)
	order.BrokerTruthAsOf = inputAt
	observe(orderIntegrityHealthCurrent, []rpc.OrderView{order}, nil)
	opened := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
	if opened.Active != 1 || opened.Measurements.EpisodesOpened != 1 {
		t.Fatalf("two current mismatch reads did not open one episode: %+v", opened)
	}

	inputAt = inputAt.Add(30 * time.Second)
	observedAt = inputAt.Add(time.Second)
	observe(orderIntegrityHealthCurrent, nil, errors.New("order journal unavailable"))
	held := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
	if held.Active != 1 || held.Covered || held.Status != alertShadowStatusUnavailable || held.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("failed empty read cleared or covered the mismatch: %+v", held)
	}

	inputAt = inputAt.Add(30 * time.Second)
	observedAt = inputAt.Add(time.Second)
	server.orderLifecyclePersistenceUncertain.Store(true)
	observe(orderIntegrityHealthCurrent, nil, nil)
	latched := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
	if latched.Active != 1 || latched.Covered || latched.Status != alertShadowStatusUnavailable || latched.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("lifecycle persistence latch cleared or covered the mismatch: %+v", latched)
	}
	server.orderLifecyclePersistenceUncertain.Store(false)

	inputAt = inputAt.Add(30 * time.Second)
	observedAt = inputAt.Add(time.Second)
	foreign := order
	foreign.Account = "DU-OTHER"
	foreign.BrokerTruthAsOf = inputAt
	closed := order
	closed.Open = false
	closed.BrokerTruthAsOf = inputAt
	observe(orderIntegrityHealthCurrent, []rpc.OrderView{foreign, closed}, nil)
	cleared := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
	if cleared.Active != 0 || !cleared.Covered || cleared.Status != alertShadowStatusCurrent || cleared.Measurements.EpisodesRecovered != 1 {
		t.Fatalf("current scoped empty read did not recover the mismatch: %+v", cleared)
	}
}

func TestAlertShadowObservationLoopsDrainOnDaemonCancellation(t *testing.T) {
	server := &Server{alertShadow: &alertShadowComposer{}}
	ctx, cancel := context.WithCancel(t.Context())
	server.startAlertShadowObservationLoops(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		server.alertShadowLoopWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("alert producer heartbeat loops did not drain after daemon cancellation")
	}
}

func TestDataHealthHeartbeatSnapshotDoesNotTriggerReconnect(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
	server.startedAt = now.Add(-time.Minute)
	serverCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.serverCtx = serverCtx
	defer server.stopDataHealthAlertShadowWorker()

	_ = server.statusHealthSnapshot()
	server.mu.Lock()
	inFlight := server.connectInFlight
	lastAttempt := server.lastReconnectAttemptAt
	server.mu.Unlock()
	if inFlight || !lastAttempt.IsZero() {
		t.Fatalf("cached Data Health projection triggered reconnect: in_flight=%t last_attempt=%s", inFlight, lastAttempt)
	}
}

func TestOrdinaryPositionsProjectionCannotObserveProtection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	summary := &rpc.ProtectionCoverageSummary{AsOf: now, Status: "ok"}
	health := ibkrlib.PortfolioStreamHealth{
		Account: server.currentBrokerStateScope().Account, InitialCompletedAt: now, LastUpdateAt: now,
	}

	projectionCtx := suppressProtectionAlertShadowObservation(t.Context())
	server.observeProtectionCoverageAlertShadow(projectionCtx, summary, "", "", health)
	suppressed := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection)
	if suppressed.Status != alertShadowStatusNotObserved || suppressed.Measurements.Evaluations != 0 {
		t.Fatalf("heartbeat-owned positions projection leaked a Protection observation: %+v", suppressed)
	}

	server.observeProtectionCoverageAlertShadow(t.Context(), summary, "", "", health)
	normal := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection)
	if normal.Status != alertShadowStatusNotObserved || normal.Measurements.Evaluations != 0 {
		t.Fatalf("ordinary positions RPC bypassed broker-snapshot Protection authority: %+v", normal)
	}
}

func TestAlertShadowReadHeartbeatsFailClosedWhenChildDeadlineExpires(t *testing.T) {
	expired := func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithDeadline(parent, time.Now().Add(-time.Second))
	}

	t.Run("rulebook", func(t *testing.T) {
		now := time.Now().UTC().Add(-time.Second)
		server := &Server{}
		attachAlertShadowCadenceTestAuthority(t, server, time.Now)
		policy := risk.DefaultRulebookPolicy()
		fingerprint := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: policy.FingerprintKey()}
		server.observeRulebookAlertShadowHeartbeatWithReadContext(t.Context(), func(context.Context, bool, bool) (*rpc.RulesResult, bool) {
			return &rpc.RulesResult{
				AsOf: now, Enabled: true, Status: "ok", PolicyID: policy.ID, PolicyVersion: policy.Version,
				PolicyFingerprint: &fingerprint,
				InputHealth: []rpc.SourceHealth{
					{Source: "account", Status: rpc.SourceStatusOK, AsOf: now},
					{Source: "positions", Status: rpc.SourceStatusOK, AsOf: now},
				},
			}, true
		}, expired)

		scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
		if err != nil {
			t.Fatal(err)
		}
		got := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceRulebook)
		if got.Covered || got.Status != alertShadowStatusUnavailable || got.Measurements.Evaluations != 1 {
			t.Fatalf("timed-out Rulebook result was accepted as a current negative: %+v", got)
		}
	})

	t.Run("order integrity", func(t *testing.T) {
		now := time.Now().UTC().Add(-time.Second)
		server := &Server{}
		attachAlertShadowCadenceTestAuthority(t, server, time.Now)
		scope := server.currentBrokerStateScope()
		server.observeOrderIntegrityAlertShadowHeartbeatWithReadContext(t.Context(), func(context.Context) ([]rpc.OrderView, orderIntegrityEvaluation, error) {
			return nil, orderIntegrityEvaluation{
				AsOf: now, EvidenceAsOf: now, Status: orderIntegrityHealthCurrent, Scope: scope,
			}, nil
		}, expired)

		shadowScope, err := newAlertShadowBrokerScope(scope)
		if err != nil {
			t.Fatal(err)
		}
		got := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
		if got.Covered || got.Status != alertShadowStatusUnavailable || got.Measurements.Evaluations != 1 {
			t.Fatalf("timed-out Order Integrity result was accepted as a current negative: %+v", got)
		}
	})
}

func TestAlertShadowReadHeartbeatsDropEvaluationAcrossReconnectScopeChange(t *testing.T) {
	t.Run("nudge", func(t *testing.T) {
		now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
		server := newV4NudgeTestServer(t, now)
		attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
		server.cfg.Gateway.Port = nil
		server.cfg.Gateway.Account = ""
		before := server.currentBrokerStateScope()

		server.observeNudgesAlertShadowHeartbeatWith(t.Context(), func(ctx context.Context, input *alertShadowNudgeInput) (rpc.NudgesSnapshotResult, error) {
			result, err := server.composeNudgesSnapshotContextWithAuthority(ctx, input)
			switchAlertShadowCadenceTestToLive(server)
			return result, err
		})

		after := server.currentBrokerStateScope()
		if sameBrokerScope(before, after) {
			t.Fatalf("test reconnect did not change scope: before=%+v after=%+v", before, after)
		}
		for _, scope := range []brokerStateScope{before, after} {
			shadowScope, err := newAlertShadowBrokerScope(scope)
			if err != nil {
				t.Fatal(err)
			}
			status := server.alertShadow.Status(shadowScope)
			for _, source := range alertShadowNudgeSources {
				got := alertShadowTestSourceStatus(t, status, source)
				if got.Status != alertShadowStatusNotObserved || got.Measurements.Evaluations != 0 {
					t.Fatalf("cross-scope Nudge result was observed for %s in %+v: %+v", source, scope, got)
				}
			}
		}
	})

	t.Run("rulebook", func(t *testing.T) {
		server := &Server{}
		attachAlertShadowCadenceTestAuthority(t, server, time.Now)
		server.cfg.Gateway.Port = nil
		server.cfg.Gateway.Account = ""
		before := server.currentBrokerStateScope()

		server.observeRulebookAlertShadowHeartbeatWith(t.Context(), func(_ context.Context, includeTape, allowMaintenance bool) (*rpc.RulesResult, bool) {
			if !includeTape || allowMaintenance {
				t.Fatalf("rulebook heartbeat flags includeTape=%t allowMaintenance=%t", includeTape, allowMaintenance)
			}
			switchAlertShadowCadenceTestToLive(server)
			return &rpc.RulesResult{AsOf: time.Now().UTC()}, true
		})

		after := server.currentBrokerStateScope()
		if sameBrokerScope(before, after) {
			t.Fatalf("test reconnect did not change scope: before=%+v after=%+v", before, after)
		}
		for _, scope := range []brokerStateScope{before, after} {
			shadowScope, err := newAlertShadowBrokerScope(scope)
			if err != nil {
				t.Fatal(err)
			}
			got := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceRulebook)
			if got.Status != alertShadowStatusNotObserved || got.Measurements.Evaluations != 0 {
				t.Fatalf("cross-scope rulebook result was observed for %+v: %+v", scope, got)
			}
		}
	})

	t.Run("order integrity", func(t *testing.T) {
		server := &Server{}
		attachAlertShadowCadenceTestAuthority(t, server, time.Now)
		server.cfg.Gateway.Port = nil
		server.cfg.Gateway.Account = ""
		before := server.currentBrokerStateScope()

		server.observeOrderIntegrityAlertShadowHeartbeatWith(t.Context(), func(context.Context) ([]rpc.OrderView, orderIntegrityEvaluation, error) {
			switchAlertShadowCadenceTestToLive(server)
			now := time.Now().UTC()
			return nil, orderIntegrityEvaluation{
				AsOf: now, EvidenceAsOf: now, Status: orderIntegrityHealthCurrent, Scope: before,
			}, nil
		})

		after := server.currentBrokerStateScope()
		if sameBrokerScope(before, after) {
			t.Fatalf("test reconnect did not change scope: before=%+v after=%+v", before, after)
		}
		for _, scope := range []brokerStateScope{before, after} {
			shadowScope, err := newAlertShadowBrokerScope(scope)
			if err != nil {
				t.Fatal(err)
			}
			got := alertShadowTestSourceStatus(t, server.alertShadow.Status(shadowScope), rpc.AlertSourceOrderIntegrity)
			if got.Status != alertShadowStatusNotObserved || got.Measurements.Evaluations != 0 {
				t.Fatalf("cross-scope order-integrity result was observed for %+v: %+v", scope, got)
			}
		}
	})
}

func attachAlertShadowCadenceTestAuthority(t *testing.T, server *Server, now func() time.Time) {
	t.Helper()
	port := 4002
	server.cfg = &config.Resolved{Gateway: config.Gateway{
		Host: "127.0.0.1", Port: &port, Account: "DU-HEARTBEAT",
	}}
	server.endpoint = discover.Endpoint{Host: "127.0.0.1", Port: port, Account: "DU-HEARTBEAT"}
	server.now = now
	server.logger = NewLogger(io.Discard, "error")
	server.coreStore = openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	if err := server.attachAlertShadowAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.alertShadow.now = now
}

func switchAlertShadowCadenceTestToLive(server *Server) {
	server.mu.Lock()
	server.endpoint.Port = 4001
	server.endpoint.Account = "U-HEARTBEAT"
	server.mu.Unlock()
}
