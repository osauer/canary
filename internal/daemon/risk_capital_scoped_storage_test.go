package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRiskCapitalSQLiteScopesAccountModeAndRestart(t *testing.T) {
	dbPath := filepath.Join(privateTestDir(t), "daemon.db")
	core := openFreshRiskCapitalCore(t, dbPath)
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	capital := &riskCapitalStore{now: func() time.Time { return now }}
	if err := capital.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}

	policy := testConstitution()
	liveA := brokerStateScope{Account: "U-SCOPE-A", Mode: rpc.AccountModeLive}
	liveB := brokerStateScope{Account: "U-SCOPE-B", Mode: rpc.AccountModeLive}
	paperA := brokerStateScope{Account: liveA.Account, Mode: rpc.AccountModePaper}

	if !capital.Observe(260000, now.Add(-4*time.Minute), policy, liveA) {
		t.Fatal("first account observation was not accepted")
	}
	if _, err := capital.ApplyCapitalEventForPolicyScope(rpc.CapitalEventParams{
		Type: "reconcile",
	}, rpc.OrderOriginHumanTTY, policy, liveA); err != nil {
		t.Fatal(err)
	}
	capital.Observe(240000, now.Add(-3*time.Minute), policy, liveA)
	if _, err := capital.ApplyCapitalEventForPolicyScope(rpc.CapitalEventParams{
		Type: "deposit", AmountBase: 1000, EffectiveAt: now.Add(-2 * time.Minute),
	}, rpc.OrderOriginHumanTTY, policy, liveA); err != nil {
		t.Fatal(err)
	}
	if !capital.Observe(310000, now.Add(-time.Minute), policy, liveB) {
		t.Fatal("second account observation was not accepted")
	}

	reportA := capital.Report(policy, nil, liveA)
	reportB := capital.Report(policy, nil, liveB)
	if reportA.AdjustedPeakBase == nil || *reportA.AdjustedPeakBase != 260000 || !reportA.BlockLatched {
		t.Fatalf("account A report lost its own peak or latch: %+v", reportA)
	}
	if reportB.AdjustedPeakBase == nil || *reportB.AdjustedPeakBase != 310000 || reportB.BlockLatched {
		t.Fatalf("account B report inherited account A state: %+v", reportB)
	}
	eventsA, err := capital.CapitalFlowEventsContextForScope(t.Context(), nil, liveA)
	if err != nil || len(eventsA) != 1 || eventsA[0].AmountBase != 1000 {
		t.Fatalf("account A capital events = %+v, err=%v", eventsA, err)
	}
	eventsB, err := capital.CapitalFlowEventsContextForScope(t.Context(), nil, liveB)
	if err != nil || len(eventsB) != 0 {
		t.Fatalf("account B inherited capital events = %+v, err=%v", eventsB, err)
	}

	if capital.Observe(999999, now, policy, paperA) {
		t.Fatal("paper observation was accepted into the capital ladder")
	}
	paperKey, err := riskCapitalScopeKey(paperA)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.GetStateDocument(t.Context(), paperKey, stateKindRiskCapital); err != nil || ok {
		t.Fatalf("paper observation created a capital document: ok=%v err=%v", ok, err)
	}
	liveAKey, _ := riskCapitalScopeKey(liveA)
	if liveAKey == paperKey {
		t.Fatal("live and paper modes resolved to the same capital scope")
	}
	assertRiskCapitalScopedDocument(t, core, liveA, 260000, true)
	assertRiskCapitalScopedDocument(t, core, liveB, 310000, false)

	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := &riskCapitalStore{now: func() time.Time { return now }}
	if err := restarted.bindCore(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	restartedA := restarted.Report(policy, nil, liveA)
	restartedB := restarted.Report(policy, nil, liveB)
	if restartedA.AdjustedPeakBase == nil || *restartedA.AdjustedPeakBase != 260000 || !restartedA.BlockLatched {
		t.Fatalf("restarted account A report = %+v", restartedA)
	}
	if restartedB.AdjustedPeakBase == nil || *restartedB.AdjustedPeakBase != 310000 || restartedB.BlockLatched {
		t.Fatalf("restarted account B report = %+v", restartedB)
	}
	restartedEvents, err := restarted.CapitalFlowEventsContextForScope(t.Context(), nil, liveA)
	if err != nil || len(restartedEvents) != 1 {
		t.Fatalf("restarted account A events = %+v, err=%v", restartedEvents, err)
	}
}

func TestRiskCapitalMigratesBoundSingletonOnce(t *testing.T) {
	core := openFreshRiskCapitalCore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	t.Cleanup(func() { _ = core.Close() })
	now := time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "U-LEGACY", Mode: rpc.AccountModeLive}
	legacy := riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer,
		State: riskCapitalStateFileV1{
			Version: riskCapitalStateVer, GenesisAt: now.Add(-30 * 24 * time.Hour), Seeded: true,
			AccountID: scope.Account, AdjustedPeakBase: 250000, PeakAsOf: now.Add(-2 * time.Hour),
			LastEquityBase: 230000, LastEquityAsOf: now.Add(-time.Hour), LastTier: risk.CapitalTierBlock,
			BlockLatched: true, LatchedAt: now.Add(-time.Hour), LatchEpisodeSeq: 4, LatchConsumedPct: 40,
			StatementAuthorityActive: true, StatementFlowsBase: 500,
		},
	}
	capitalEvent := capitalEventV1{
		Version: 1, At: now.Add(-3 * time.Hour), Type: "deposit", AmountBase: 500, EffectiveAt: now.Add(-3 * time.Hour),
	}
	capitalRaw, err := json.Marshal(capitalEvent)
	if err != nil {
		t.Fatal(err)
	}
	capitalInput := corestore.EventInput{
		ScopeKey: daemonStateScope,
		EventKey: coreEventKey(coreEventCapital, capitalEvent.At, capitalRaw, 1),
		Type:     coreEventCapital, Action: "import", Origin: "test",
		OccurredAt: capitalEvent.At, PayloadJSON: capitalRaw,
		Projection: corestore.EventProjection{CapitalEvent: &corestore.CapitalEventProjection{
			Kind: capitalEvent.Type, AmountBaseText: "500", EffectiveAt: capitalEvent.EffectiveAt.UTC().Format(time.RFC3339Nano),
		}},
	}
	governancePayload, err := json.Marshal(map[string]any{
		"version": 1, "at": now.Add(-2 * time.Hour), "kind": "recon_dismiss", "line_id": "legacy-line", "reason": "confirmed",
	})
	if err != nil {
		t.Fatal(err)
	}
	governanceInput := corestore.EventInput{
		ScopeKey: daemonStateScope, EventKey: coreEventKey(coreEventRiskPolicy, now.Add(-2*time.Hour), governancePayload, 2),
		Type: coreEventRiskPolicy, Action: "import", Origin: "test",
		OccurredAt: now.Add(-2 * time.Hour), PayloadJSON: governancePayload,
		Projection: corestore.EventProjection{RiskPolicyEvent: &corestore.RiskPolicyEventProjection{Kind: "recon_dismiss"}},
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	singletonBefore, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindRiskCapital)
	if err != nil || !ok {
		t.Fatalf("load singleton before migration: ok=%v err=%v", ok, err)
	}
	if _, _, err := core.CompareAndSwapStateDocumentWithEvents(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRiskCapital, ExpectedRevision: singletonBefore.Revision, JSON: legacyRaw,
	}, []corestore.EventInput{capitalInput, governanceInput}); err != nil {
		t.Fatal(err)
	}
	singletonBefore, _, err = core.GetStateDocument(t.Context(), daemonStateScope, stateKindRiskCapital)
	if err != nil {
		t.Fatal(err)
	}

	first := &riskCapitalStore{now: func() time.Time { return now }}
	if err := first.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	report := first.Report(testV3Constitution(), nil, scope)
	if report.AdjustedPeakBase == nil || *report.AdjustedPeakBase != 250000 || !report.BlockLatched {
		t.Fatalf("migrated report lost safety continuity: %+v", report)
	}
	assertRiskCapitalScopedDocument(t, core, scope, 250000, true)
	scopeKey, _ := riskCapitalScopeKey(scope)
	capitalEvents, err := loadCoreEventsForScope(t.Context(), core, scopeKey, coreEventCapital)
	if err != nil || len(capitalEvents) != 1 {
		t.Fatalf("migrated capital events = %d, err=%v", len(capitalEvents), err)
	}
	governanceEvents, err := loadCoreEventsForScope(t.Context(), core, scopeKey, coreEventRiskPolicy)
	if err != nil || len(governanceEvents) != 1 {
		t.Fatalf("migrated governance events = %d, err=%v", len(governanceEvents), err)
	}

	second := &riskCapitalStore{now: func() time.Time { return now }}
	if err := second.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	capitalEvents, _ = loadCoreEventsForScope(t.Context(), core, scopeKey, coreEventCapital)
	governanceEvents, _ = loadCoreEventsForScope(t.Context(), core, scopeKey, coreEventRiskPolicy)
	if len(capitalEvents) != 1 || len(governanceEvents) != 1 {
		t.Fatalf("restart duplicated migrated events: capital=%d governance=%d", len(capitalEvents), len(governanceEvents))
	}
	singletonAfter, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindRiskCapital)
	if err != nil || !ok {
		t.Fatalf("load singleton after migration: ok=%v err=%v", ok, err)
	}
	if singletonAfter.Revision != singletonBefore.Revision || !bytes.Equal(singletonAfter.JSON, singletonBefore.JSON) {
		t.Fatal("migration modified the compatibility singleton")
	}
}

func TestRiskCapitalRefusesUnboundLegacySafetyState(t *testing.T) {
	core := openFreshRiskCapitalCore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	t.Cleanup(func() { _ = core.Close() })
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	legacy := riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer,
		State: riskCapitalStateFileV1{
			Version: riskCapitalStateVer, GenesisAt: now.Add(-24 * time.Hour), Seeded: true,
			AdjustedPeakBase: 250000, PeakAsOf: now.Add(-2 * time.Hour), LastEquityBase: 230000,
			LastEquityAsOf: now.Add(-time.Hour), BlockLatched: true, LatchedAt: now.Add(-time.Hour), LatchEpisodeSeq: 2,
		},
	}
	overwriteRiskCapitalSingleton(t, core, legacy)

	capital := &riskCapitalStore{now: func() time.Time { return now }}
	if err := capital.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	scope := brokerStateScope{Account: "U-RECOVERY", Mode: rpc.AccountModeLive}
	report := capital.Report(testConstitution(), nil, scope)
	if report.Tier != risk.CapitalTierUnknown || !report.BlockLatched || len(report.Reasons) != 1 {
		t.Fatalf("unbound legacy safety report = %+v", report)
	}
	if !strings.Contains(report.Reasons[0], "no account identity") || strings.Contains(report.Reasons[0], "SQLite") || strings.Contains(report.Reasons[0], "daemon.db") {
		t.Fatalf("unbound legacy refusal leaked implementation detail or hid the recovery reason: %q", report.Reasons[0])
	}
	if capital.Observe(999999, now, testConstitution(), scope) {
		t.Fatal("unbound legacy safety state was silently adopted")
	}
	scopeKey, _ := riskCapitalScopeKey(scope)
	if _, ok, err := core.GetStateDocument(t.Context(), scopeKey, stateKindRiskCapital); err != nil || ok {
		t.Fatalf("unbound legacy state was assigned to a selected account: ok=%v err=%v", ok, err)
	}
}

func openFreshRiskCapitalCore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	core, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeFreshDaemonState(t.Context(), core); err != nil {
		_ = core.Close()
		t.Fatal(err)
	}
	return core
}

func assertRiskCapitalScopedDocument(t *testing.T, core *corestore.Store, scope brokerStateScope, wantPeak float64, wantLatched bool) {
	t.Helper()
	scopeKey, err := riskCapitalScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	doc, ok, err := core.GetStateDocument(t.Context(), scopeKey, stateKindRiskCapital)
	if err != nil || !ok {
		t.Fatalf("load scoped capital document: ok=%v err=%v", ok, err)
	}
	var got riskCapitalSQLiteDocument
	if err := json.Unmarshal(doc.JSON, &got); err != nil {
		t.Fatal(err)
	}
	if got.State.AccountID != scope.Account || got.State.AccountMode != scope.Mode || got.State.AdjustedPeakBase != wantPeak || got.State.BlockLatched != wantLatched {
		t.Fatalf("scoped capital document = %+v", got.State)
	}
}

func overwriteRiskCapitalSingleton(t *testing.T, core *corestore.Store, value riskCapitalSQLiteDocument) {
	t.Helper()
	doc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindRiskCapital)
	if err != nil || !ok {
		t.Fatalf("load capital singleton: ok=%v err=%v", ok, err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRiskCapital, ExpectedRevision: doc.Revision, JSON: raw,
	}); err != nil {
		t.Fatal(err)
	}
}
