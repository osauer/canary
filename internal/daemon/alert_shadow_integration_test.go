package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

func TestAlertShadowHandlersAreScopedColdRedactedAndDeliveryInactive(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	port := 4002
	base := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	server := &Server{
		coreStore: store,
		cfg: &config.Resolved{Gateway: config.Gateway{
			Host: "127.0.0.1", Port: &port, Account: "DU-HANDLER-A",
		}},
		now: func() time.Time { return base },
	}
	if err := server.attachAlertShadowAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.alertShadow.now = server.now
	server.postConnectSetupDone.Store(true)

	cold, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	wantA, err := rpc.BuildAlertAuthorityScope("DU-HANDLER-A", rpc.AccountModePaper)
	if err != nil {
		t.Fatal(err)
	}
	if cold.AuthorityScope != wantA || cold.CurrentState != rpc.AlertSnapshotUnknown ||
		cold.Coverage.State != rpc.AlertCoverageUnavailable || len(cold.Candidates) != 0 {
		t.Fatalf("cold scoped snapshot=%+v", cold)
	}

	scopeA, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	relevant := true
	result := alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "handler-a")
	if _, err := server.alertShadow.ObserveStress(t.Context(), scopeA, result); err != nil {
		t.Fatal(err)
	}
	active, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if active.AuthorityScope != wantA || len(active.Candidates) != 1 || active.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("active scoped snapshot=%+v", active)
	}
	status, err := server.handleAlertStatus(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.ExpectedSources) != 9 {
		t.Fatalf("alert status=%+v", status)
	}

	rawSnapshot, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	rawStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{rawSnapshot, rawStatus} {
		text := string(raw)
		if strings.Contains(text, "DU-HANDLER-A") || strings.Contains(text, `"account"`) || strings.Contains(text, `"account_mode"`) {
			t.Fatalf("handler exposed raw broker scope: %s", text)
		}
	}
	if strings.Contains(string(rawStatus), "alert-authority-scope-v1:") {
		t.Fatalf("status exposed private opaque scope: %s", rawStatus)
	}

	server.cfg.Gateway.Account = "DU-HANDLER-B"
	coldB, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := rpc.BuildAlertAuthorityScope("DU-HANDLER-B", rpc.AccountModePaper)
	if err != nil {
		t.Fatal(err)
	}
	if coldB.AuthorityScope != wantB || coldB.AuthorityScope == wantA || len(coldB.Candidates) != 0 || coldB.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("foreign scope leaked prior candidates: %+v", coldB)
	}

	server.cfg.Gateway.Account = "DU-HANDLER-A"
	server.now = func() time.Time { return base.Add(alertShadowCanarySilenceHorizon + time.Nanosecond) }
	server.alertShadow.now = server.now
	stale, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if stale.AuthorityScope != wantA || stale.Coverage.Freshness != rpc.AlertCoverageStale ||
		len(stale.Candidates) != 1 || stale.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale || stale.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("silent producer did not project stale: %+v", stale)
	}
}

func TestDataHealthWorkerPreservesOutageBeforeRecoveryWhileRunning(t *testing.T) {
	scope := alertShadowTestBrokerScope(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	applied := []alertShadowDataHealthInput{}
	server := &Server{alertShadow: &alertShadowComposer{}}
	server.dataHealthObserveTest = func(_ context.Context, input alertShadowDataHealthInput) error {
		mu.Lock()
		applied = append(applied, input)
		call := len(applied)
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}
	base := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{AsOf: base, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	<-firstStarted
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{AsOf: base.Add(time.Second), Scope: scope, GatewayPhase: alertShadowGatewayFailed})
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{AsOf: base.Add(2 * time.Second), Scope: scope, GatewayPhase: alertShadowGatewayReady})
	close(releaseFirst)
	waitForDataHealthWorkerCalls(t, &mu, &applied, 3)
	server.stopDataHealthAlertShadowWorker()
	mu.Lock()
	defer mu.Unlock()
	if applied[1].GatewayPhase != alertShadowGatewayFailed || applied[2].GatewayPhase != alertShadowGatewayReady {
		t.Fatalf("outage/recovery transition order was lost: %+v", applied)
	}
}

func TestDataHealthWorkerRetriesOutageBeforeRecoveryDuringFailureBackoff(t *testing.T) {
	scope := alertShadowTestBrokerScope(t)
	firstFailed := make(chan struct{})
	var mu sync.Mutex
	applied := []alertShadowDataHealthInput{}
	server := &Server{alertShadow: &alertShadowComposer{}, dataHealthObserveBackoff: 5 * time.Millisecond}
	server.dataHealthObserveTest = func(_ context.Context, input alertShadowDataHealthInput) error {
		mu.Lock()
		applied = append(applied, input)
		call := len(applied)
		mu.Unlock()
		if call == 1 {
			close(firstFailed)
			return errors.New("injected SQLite failure")
		}
		return nil
	}
	base := time.Date(2026, 7, 21, 18, 30, 0, 0, time.UTC)
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{AsOf: base, Scope: scope, GatewayPhase: alertShadowGatewayFailed})
	<-firstFailed
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{AsOf: base.Add(time.Second), Scope: scope, GatewayPhase: alertShadowGatewayReady})
	waitForDataHealthWorkerCalls(t, &mu, &applied, 3)
	server.stopDataHealthAlertShadowWorker()
	mu.Lock()
	defer mu.Unlock()
	if applied[1].GatewayPhase != alertShadowGatewayFailed || applied[2].GatewayPhase != alertShadowGatewayReady {
		t.Fatalf("failed outage was not retried before recovery: %+v", applied)
	}
}

func TestDataHealthWorkerShutdownDoesNotRequeueFailedApply(t *testing.T) {
	scope := alertShadowTestBrokerScope(t)
	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	server := &Server{alertShadow: &alertShadowComposer{}}
	server.dataHealthObserveTest = func(_ context.Context, _ alertShadowDataHealthInput) error {
		close(applyStarted)
		<-releaseApply
		return errors.New("injected shutdown failure")
	}
	server.enqueueDataHealthAlertShadow(alertShadowDataHealthInput{
		AsOf: time.Date(2026, 7, 21, 18, 45, 0, 0, time.UTC), Scope: scope, GatewayPhase: alertShadowGatewayFailed,
	})
	<-applyStarted
	stopped := make(chan struct{})
	go func() {
		server.stopDataHealthAlertShadowWorker()
		close(stopped)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		server.dataHealthObserveMu.Lock()
		stopMarked := server.dataHealthObserveStopped
		server.dataHealthObserveMu.Unlock()
		if stopMarked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not mark the worker stopped")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("worker stopped before the in-flight apply returned")
	default:
	}
	close(releaseApply)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after the in-flight apply returned")
	}
	server.dataHealthObserveMu.Lock()
	defer server.dataHealthObserveMu.Unlock()
	if server.dataHealthObserveRunning || server.dataHealthObservePending != nil || server.dataHealthObserveRetryAt != nil {
		t.Fatalf("worker retained shutdown state: running=%t pending=%v retry=%v",
			server.dataHealthObserveRunning, server.dataHealthObservePending, server.dataHealthObserveRetryAt)
	}
}

func TestProtectionHeartbeatPreservesContractIdentityAndRejectsAmbiguousFallback(t *testing.T) {
	asOf := time.Date(2026, 7, 21, 19, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	positions, scoped := protectionHeartbeatPositions([]*ibkrlib.RawPosition{{
		Account: "DU123",
		Contract: ibkrlib.Contract{
			ConID: 101, Symbol: "abc", SecType: "STK", Exchange: "SMART", Currency: "USD",
			LocalSymbol: "ABC", TradingClass: "NMS",
		},
		Position: 10,
	}}, scope, asOf)
	if !scoped {
		t.Fatal("current-account Protection projection was rejected")
	}
	if len(positions.Stocks) != 1 {
		t.Fatalf("stock rows=%d want 1", len(positions.Stocks))
	}
	got := positions.Stocks[0]
	if got.ConID != 101 || got.Symbol != "ABC" || got.Exchange != "SMART" || got.Currency != "USD" ||
		got.LocalSymbol != "ABC" || got.TradingClass != "NMS" {
		t.Fatalf("contract identity was dropped: %+v", got)
	}
	if foreign, ok := protectionHeartbeatPositions([]*ibkrlib.RawPosition{{
		Account: "DU999", Contract: ibkrlib.Contract{ConID: 202, Symbol: "ABC", SecType: "STK"}, Position: 10,
	}}, scope, asOf); ok || len(foreign.Stocks) != 0 {
		t.Fatalf("foreign-account Protection projection = %+v scoped=%t, want fail-closed empty", foreign, ok)
	}
	protectiveOrder := func(conID int) rpc.OrderView {
		return rpc.OrderView{Symbol: "ABC", SecType: "STK", ConID: conID, Open: true, OrderType: "STP", OpenClose: "C"}
	}
	if protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{{Symbol: "ABC", SecType: "STK", OrderType: "STP", OpenClose: "C"}}) {
		t.Fatal("closed zero-ConID order must not block Protection")
	}
	if protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{{Symbol: "ABC", SecType: "STK", Open: true, OrderType: "LMT", OpenClose: "C"}}) {
		t.Fatal("non-protective zero-ConID order must not block Protection")
	}
	if !protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{protectiveOrder(0)}) {
		t.Fatal("single-position zero-ConID order must fail closed")
	}
	positions.Stocks[0].ConID = 0
	if !protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{protectiveOrder(101)}) {
		t.Fatal("single zero-ConID position must fail closed against a relevant order")
	}
	positions.Stocks[0].ConID = 101

	positions.Stocks = append(positions.Stocks, rpc.PositionView{Symbol: "ABC", SecType: "STOCK", ConID: 202, Quantity: 5})
	if !protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{protectiveOrder(0)}) {
		t.Fatal("zero-ConID order must fail closed across same-symbol positions")
	}
	if protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{protectiveOrder(101)}) {
		t.Fatal("exact positive ConID order should disambiguate same-symbol positions")
	}
	positions.Stocks[1].ConID = 0
	if !protectionHeartbeatIdentityAmbiguous(positions, []rpc.OrderView{protectiveOrder(101)}) {
		t.Fatal("zero-ConID same-symbol position must fail closed")
	}
}

func TestAlertShadowGatewayPhaseDistinguishesStartupFromFailure(t *testing.T) {
	tests := []struct {
		name                       string
		connected, setup, inFlight bool
		lastError                  string
		uptime                     time.Duration
		want                       alertShadowGatewayPhase
	}{
		{"ready", true, true, false, "", time.Minute, alertShadowGatewayReady},
		{"initial handshake", false, false, true, "", time.Minute, alertShadowGatewayConnecting},
		{"startup grace", false, false, false, "", 10 * time.Second, alertShadowGatewayConnecting},
		{"discovery failed", false, false, false, "no endpoint", time.Second, alertShadowGatewayFailed},
		{"startup grace expired", false, false, false, "", time.Minute, alertShadowGatewayFailed},
		{"reconnect after ready", false, true, true, "", time.Minute, alertShadowGatewayFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := alertShadowGatewayPhaseForHealth(tc.connected, tc.setup, tc.inFlight, tc.lastError, tc.uptime); got != tc.want {
				t.Fatalf("phase=%q want %q", got, tc.want)
			}
		})
	}
}

func waitForDataHealthWorkerCalls(t *testing.T, mu *sync.Mutex, applied *[]alertShadowDataHealthInput, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*applied)
		mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("Data Health worker calls=%d want %d", len(*applied), want)
}

func TestAlertShadowApprovedProducerIntegrationIsDurableAndRecordOnly(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	port := 4002
	base := time.Date(2026, 7, 21, 17, 0, 0, 0, time.UTC)
	server := &Server{
		coreStore: store,
		cfg:       &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, Account: "DU-PRODUCERS"}},
		now:       func() time.Time { return base.Add(time.Second) },
	}
	if err := server.attachAlertShadowAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.alertShadow.now = server.now
	server.postConnectSetupDone.Store(true)
	scope := server.currentBrokerStateScope()
	shadowScope, err := newAlertShadowBrokerScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	regime := alertShadowTestRegime(base, rpc.LifecycleEarlyWarning, "ready")
	server.observeRegimeAlertShadow(t.Context(), &regime, scope)
	server.observeProtectionAlertShadow(t.Context(), alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, Status: orderIntegrityHealthCurrent, Scope: shadowScope,
		OrderSnapshotAsOf: base, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: base, Status: "ok"},
	})
	server.observeDataHealthAlertShadow(&rpc.HealthResult{
		Connected: true, Subsystems: alertShadowTestHealthySubsystems(),
	}, scope, alertShadowGatewayReady, base.Add(time.Second))
	waitForAlertShadowSourceCovered(t, server.alertShadow, shadowScope, rpc.AlertSourceDataHealth)

	snapshot, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil || len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Source != rpc.AlertSourceRegime ||
		snapshot.Candidates[0].PresentationCode != rpc.AlertPresentationRegimeMarketStress {
		t.Fatalf("approved producer snapshot=%+v err=%v", snapshot, err)
	}
	assertAlertShadowCoverage(t, snapshot.Coverage, []rpc.AlertSource{
		rpc.AlertSourceRegime, rpc.AlertSourceProtection, rpc.AlertSourceDataHealth,
	})
	status, err := server.handleAlertStatus(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatalf("alert status: %+v err=%v", status, err)
	}
	var protectionStatus *rpc.AlertSourceStatus
	for i := range status.Sources {
		if status.Sources[i].Source == rpc.AlertSourceProtection {
			protectionStatus = &status.Sources[i]
			break
		}
	}
	if protectionStatus == nil || protectionStatus.AuthorityUniverse != rpc.AlertAuthorityUniverseJournaledAPIOrders {
		t.Fatalf("Protection source did not disclose its narrow authority universe: %+v", protectionStatus)
	}

	restarted := &Server{coreStore: store, cfg: server.cfg, now: func() time.Time { return base.Add(2 * time.Second) }}
	if err := restarted.attachAlertShadowAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted.alertShadow.now = restarted.now
	restarted.postConnectSetupDone.Store(true)
	cold, err := restarted.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil || cold.Coverage.State != rpc.AlertCoverageUnavailable || len(cold.Candidates) != 1 ||
		cold.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent {
		t.Fatalf("restart fabricated coverage or lost active episode: %+v err=%v", cold, err)
	}

	restarted.observeRegimeAlertShadow(t.Context(), &regime, scope)
	restarted.observeProtectionAlertShadow(t.Context(), alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, Status: orderIntegrityHealthCurrent, Scope: shadowScope,
		OrderSnapshotAsOf: base, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: base, Status: "ok"},
	})
	restarted.observeDataHealthAlertShadow(&rpc.HealthResult{
		Connected: true, Subsystems: alertShadowTestHealthySubsystems(),
	}, scope, alertShadowGatewayReady, base.Add(2*time.Second))
	waitForAlertShadowSourceCovered(t, restarted.alertShadow, shadowScope, rpc.AlertSourceDataHealth)
	restored, err := restarted.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, restored.Coverage, []rpc.AlertSource{
		rpc.AlertSourceRegime, rpc.AlertSourceProtection, rpc.AlertSourceDataHealth,
	})
}

func alertShadowTestHealthySubsystems() []rpc.SubsystemHealth {
	return []rpc.SubsystemHealth{
		{Name: "storage", Status: "ready"}, {Name: "quote", Status: "ready"},
		{Name: "history", Status: "ready"}, {Name: "chain", Status: "ready"},
		{Name: "proposals", Status: "ready"}, {Name: "opportunities", Status: "ready"},
	}
}

func waitForAlertShadowSourceCovered(t *testing.T, composer *alertShadowComposer, scope alertShadowBrokerScope, source rpc.AlertSource) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last alertShadowSourceStatus
	for time.Now().Before(deadline) {
		last = alertShadowTestSourceStatus(t, composer.Status(scope), source)
		if last.Covered {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("alert registry source %s did not become covered: %+v", source, last)
}
