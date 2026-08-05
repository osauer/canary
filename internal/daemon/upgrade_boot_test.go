package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The upgrade-boot gate: the current binary must start against a store the
// desk was already running on and reach the point where the socket is
// published. Package tests cover each reconciliation guard in isolation and
// `make smoke` boots a fresh authority; neither exercises a current binary
// against prior-version retained content, and v2.6.2 shipped a boot-blocking
// refusal through both — the daemon crash-looped on any store carrying
// pre-cutover regime history, and the live desk found it.
//
// These fixtures cover retained content: a store at the current schema still
// holds documents and events rendered under a prior policy, and the
// reconciliation guards run on every boot whether or not the schema moved.
// That is the axis v2.6.2 fell down. The schema axis is covered separately by
// the tag-generated corpus under testdata/upgrades, which cannot be booted as
// it stands; internal-docs/design/daemon-sqlite-authority.md has the boundary.
//
// A future cutover adds a fixture whose seeder renders retained content the
// way the prior policy wrote it. It does not need a new harness.

// upgradeBootFixture is one prior-version retained-content epoch. seed writes
// that epoch's state into an authority the daemon itself published, so the
// fixture differs from a live store only in the content under test. verify
// runs after the upgrade boot: reaching ready by rewriting retained evidence
// into current form would be a worse defect than refusing to start.
type upgradeBootFixture struct {
	name   string
	seed   func(t *testing.T, store *corestore.Store)
	verify func(t *testing.T, store *corestore.Store)
}

func upgradeBootFixtures() []upgradeBootFixture {
	return []upgradeBootFixture{
		{
			name:   "pre-currency-policy-regime-history",
			seed:   seedPreCurrencyPolicyRegimeHistory,
			verify: verifyPreCurrencyPolicyRegimeHistory,
		},
		{
			name:   "pre-depth-scale-regime-history",
			seed:   seedPreDepthScaleRegimeHistory,
			verify: verifyPreDepthScaleRegimeHistory,
		},
	}
}

func TestUpgradeBootReachesReadyOnPriorVersionStores(t *testing.T) {
	for _, fixture := range upgradeBootFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			root := upgradeBootStateRoot(t)
			// The prior version's own boot: it publishes daemon.db, its
			// watermark, the cutover manifest, and every fresh-state document.
			bootDaemonToReady(t, root, "prior-version boot")
			withUpgradeBootAuthority(t, root, fixture.seed)
			// The upgrade boot: a current binary over that retained content.
			bootDaemonToReady(t, root, "upgrade boot")
			withUpgradeBootAuthority(t, root, fixture.verify)
		})
	}
}

// bootDaemonToReady runs the real Start against the state root and waits for
// the published socket. Schema inspection, migration, and all three regime
// reconciliation guards run strictly before the socket exists, so a socket
// serving on the fixture is proof that every one of them accepted the store.
func bootDaemonToReady(t *testing.T, root, label string) {
	t.Helper()
	// Redirect every XDG root so a boot cannot read or write the developer's
	// own daemon state. daemon.db is pinned explicitly rather than resolved
	// from the state root: that keeps this gate on the schema and projection
	// paths and out of the legacy file-authority import.
	for _, variable := range []string{"XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		dir := filepath.Join(root, "xdg", variable)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(variable, dir)
	}

	tlsFalse := false
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99), TLS: &tlsFalse},
	}
	cfg.Daemon.SetIdleTimeout(0)

	socketPath := filepath.Join(root, "ibkrd.sock")
	logs := &bytes.Buffer{}
	srv := New(Options{
		Config:            cfg,
		SocketPath:        socketPath,
		Version:           "upgrade-boot-test",
		Logger:            NewLogger(logs, "error"),
		StateDatabasePath: upgradeBootDatabasePath(root),
	})
	srv.attempterFactory = func(discover.Endpoint) connectAttempter {
		return &fakeAttempter{blockUntilCtxDone: true}
	}
	ready := make(chan error, 1)
	srv.initialAcceptLoopStartedForTest = func() {
		info, err := os.Stat(socketPath)
		switch {
		case err != nil:
			ready <- err
		case info.Mode()&os.ModeSocket == 0:
			ready <- fmt.Errorf("published path is not a socket: mode=%v", info.Mode())
		default:
			ready <- nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startReturned := make(chan error, 1)
	go func() { startReturned <- srv.Start(ctx) }()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	case err := <-startReturned:
		t.Fatalf("%s refused the store instead of reaching ready: %v\ndaemon log:\n%s", label, err, logs)
	case <-time.After(90 * time.Second):
		t.Fatalf("%s did not reach ready\ndaemon log:\n%s", label, logs)
	}

	cancel()
	select {
	case <-startReturned:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not return after cancellation", label)
	}
	srv.Stop()
}

func upgradeBootStateRoot(t *testing.T) string {
	t.Helper()
	// /tmp-rooted so the socket path stays inside macOS's SUN_LEN limit.
	root := shortTempDir(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func upgradeBootDatabasePath(root string) string { return filepath.Join(root, "daemon.db") }

// withUpgradeBootAuthority opens the stopped authority for one fixture step
// and advances the watermark the way a committing daemon would, so the next
// boot sees an ordinary state root rather than a rolled-back one.
func withUpgradeBootAuthority(t *testing.T, root string, step func(*testing.T, *corestore.Store)) {
	t.Helper()
	databasePath := upgradeBootDatabasePath(root)
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatalf("open published authority: %v", err)
	}
	step(t, store)
	head, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatalf("read seeded authority head: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded authority: %v", err)
	}
	if err := writeAuthorityWatermark(databasePath+".head", head); err != nil {
		t.Fatalf("advance seeded authority watermark: %v", err)
	}
}

// seedPreCurrencyPolicyRegimeHistory builds the store shape that crash-looped
// the desk on v2.6.2: a complete, receipted regime publication whose retained
// decision line was rendered before the input-currency cutover.
func seedPreCurrencyPolicyRegimeHistory(t *testing.T, store *corestore.Store) {
	seedUpgradeBootRegimeHistory(t, store, nil, seedPriorCurrencyPolicyDecisionEvent)
}

func seedPreDepthScaleRegimeHistory(t *testing.T, store *corestore.Store) {
	seedUpgradeBootRegimeHistory(t, store, func(snapshot *rpc.RegimeSnapshotResult) {
		depth := 42.0
		snapshot.FundingStress.SpreadBps = &depth
	}, seedPriorDepthScaleDecisionEvent)
}

func seedUpgradeBootRegimeHistory(
	t *testing.T,
	store *corestore.Store,
	configure func(*rpc.RegimeSnapshotResult),
	seedDecision func(*testing.T, *corestore.Store, *rpc.RegimeSnapshotResult, regimeSnapshotPublication),
) {
	t.Helper()
	snapshot := upgradeBootRegimeSnapshot(time.Date(2026, 7, 20, 15, 10, 0, 0, time.UTC))
	if configure != nil {
		configure(snapshot)
		snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	}
	raw, _, err := encodeRegimeSnapshotDocument(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	// The publication time is the state document's commit timestamp, not a
	// field the writer chooses, so every projection below is derived from the
	// saved document rather than from the fixture's as-of.
	saved, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatalf("persist authoritative snapshot: %v", err)
	}
	publication := regimeSnapshotPublication{
		Revision: saved.Revision, PublishedAt: saved.UpdatedAt.UTC(), Fingerprint: snapshot.Fingerprint,
	}

	entries, err := projectedRegimeStreakEntries(nil, snapshot, publication)
	if err != nil {
		t.Fatalf("project seed streak entries: %v", err)
	}
	projectionRecoverySeedStreakStore(t, store, publication, entries)
	seedUpgradeBootRuleStageProjection(t, store, snapshot, publication)
	seedDecision(t, store, snapshot, publication)

	server := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := server.persistRegimeDecisionProjectionState(
		t.Context(), corestore.StateDocument{}, false, publication, regimeDecisionEventRecorded,
	); err != nil {
		t.Fatalf("seed decision projection marker: %v", err)
	}
	if err := server.recordRegimeProjectionReceiptWithDecision(
		t.Context(), publication, regimeDecisionEventRecorded,
	); err != nil {
		t.Fatalf("seed projection receipt: %v", err)
	}
}

// verifyPreCurrencyPolicyRegimeHistory requires the retained line to still be
// prior-policy history after the boot. Recovery may repair a projection it can
// prove incomplete, but a decision event already bound to its publication is
// retained evidence: rewriting it into current form would erase the very
// partition the currency marker exists to preserve.
func verifyPreCurrencyPolicyRegimeHistory(t *testing.T, store *corestore.Store) {
	t.Helper()
	events, err := loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision)
	if err != nil {
		t.Fatalf("load decision events after upgrade boot: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decision events after upgrade boot=%d, want the one retained line", len(events))
	}
	var line regimeDecisionLine
	if err := json.Unmarshal(events[0].PayloadJSON, &line); err != nil {
		t.Fatalf("decode retained decision line: %v", err)
	}
	if line.CurrencyPolicy != "" {
		t.Fatalf("upgrade boot stamped the current currency policy onto retained history: %q", line.CurrencyPolicy)
	}
	for key, indicator := range line.Indicators {
		if indicator.Freshness != rpc.RegimeFreshnessNotDue {
			t.Fatalf("upgrade boot rewrote retained %s freshness to %q", key, indicator.Freshness)
		}
	}
}

func verifyPreDepthScaleRegimeHistory(t *testing.T, store *corestore.Store) {
	t.Helper()
	events, err := loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision)
	if err != nil {
		t.Fatalf("load decision events after upgrade boot: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decision events after upgrade boot=%d, want the one retained line", len(events))
	}
	var line regimeDecisionLine
	if err := json.Unmarshal(events[0].PayloadJSON, &line); err != nil {
		t.Fatalf("decode retained decision line: %v", err)
	}
	if line.V != 2 {
		t.Fatalf("upgrade boot rewrote retained decision version to %d", line.V)
	}
	if got := line.Indicators[StreakKeyFunding].Depth; got != nil {
		t.Fatalf("upgrade boot backfilled retained funding depth: %v", *got)
	}
}

// seedUpgradeBootRuleStageProjection advances the rule-stage latch the
// published store already carries. It cannot reuse the package's exact-seed
// helper, which assumes a store with no prior document.
func seedUpgradeBootRuleStageProjection(
	t *testing.T,
	store *corestore.Store,
	snapshot *rpc.RegimeSnapshotResult,
	publication regimeSnapshotPublication,
) {
	t.Helper()
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, stateKindRulesRegimeStage)
	if err != nil {
		t.Fatalf("read published rule-stage projection: %v", err)
	}
	base := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	expectedRevision := int64(0)
	if ok {
		decoded, err := decodeRulesRegimeStageState(doc.JSON)
		if err != nil {
			t.Fatalf("decode published rule-stage projection: %v", err)
		}
		base = decoded.withoutProjectionHistory()
		base.Version = rulesRegimeStageStateVer
		expectedRevision = doc.Revision
	}
	raw, err := json.Marshal(projectedRulesRegimeStageState(base, snapshot, publication))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage,
		ExpectedRevision: expectedRevision, JSON: raw,
	}); err != nil {
		t.Fatalf("seed rule-stage projection: %v", err)
	}
}

// seedPriorCurrencyPolicyDecisionEvent appends the retained decision event as
// the pre-cutover binary wrote it: no currency_policy marker, and the
// freshness classes that policy assigned to the same rows. A recompute under
// the current policy legitimately renders different bytes, which is what the
// marker was introduced to partition — and what the boot-time byte comparison
// mistook for corruption.
func seedPriorCurrencyPolicyDecisionEvent(
	t *testing.T,
	store *corestore.Store,
	snapshot *rpc.RegimeSnapshotResult,
	publication regimeSnapshotPublication,
) {
	t.Helper()
	line := buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication)
	line.CurrencyPolicy = ""
	for key, indicator := range line.Indicators {
		indicator.Freshness = rpc.RegimeFreshnessNotDue
		line.Indicators[key] = indicator
	}

	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	// Without a divergence the guard would accept the line for the wrong
	// reason and the fixture would prove nothing while staying green.
	current, err := json.Marshal(buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, current) {
		t.Fatal("retained line no longer diverges from a current recompute; the fixture would pass vacuously")
	}
	appendUpgradeBootDecisionEvent(t, store, line, publication)
}

// seedPriorDepthScaleDecisionEvent reproduces the v2 payload that was already
// durable when funding, credit, USD/JPY, and gamma gained explicit depth. The
// old line is immutable evidence: upgrade boot must accept the missing
// additive field without backfilling it or weakening current-v3 validation.
func seedPriorDepthScaleDecisionEvent(
	t *testing.T,
	store *corestore.Store,
	snapshot *rpc.RegimeSnapshotResult,
	publication regimeSnapshotPublication,
) {
	t.Helper()
	line := buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication)
	line.V = 2
	indicator := line.Indicators[StreakKeyFunding]
	if indicator.Depth == nil {
		t.Fatal("depth-scale fixture has no current funding depth")
	}
	indicator.Depth = nil
	line.Indicators[StreakKeyFunding] = indicator
	appendUpgradeBootDecisionEvent(t, store, line, publication)
}

func appendUpgradeBootDecisionEvent(
	t *testing.T,
	store *corestore.Store,
	line regimeDecisionLine,
	publication regimeSnapshotPublication,
) {
	t.Helper()
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}

	key := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
	indicators := make([]corestore.RegimeIndicatorProjection, 0, len(line.Indicators))
	for _, indicator := range streakIndicators {
		value, ok := line.Indicators[indicator.key()]
		if !ok {
			continue
		}
		var streak *int64
		if value.StreakSessions != 0 {
			streak = new(int64(value.StreakSessions))
		}
		indicators = append(indicators, corestore.RegimeIndicatorProjection{
			Indicator: indicator.key(), Status: value.Status, Band: value.Band,
			Value: value.Value, Depth: value.Depth, StreakSessions: streak,
			Freshness: value.Freshness, Eligible: value.Eligible,
			Latched: value.Latched, ThresholdsLabel: value.ThresholdsLabel,
		})
	}
	if _, err := store.AppendEvents(t.Context(), []corestore.EventInput{{
		ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRegimeDecision,
		Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
		OccurredAt: publication.PublishedAt, PayloadJSON: raw,
		Projection: corestore.EventProjection{RegimeDecision: &corestore.RegimeDecisionProjection{
			DecisionKey: key, Stage: line.Stage, Severity: line.Severity,
			Readiness: line.Readiness, Confidence: line.Confidence,
			Verdict: line.Verdict, Fingerprint: line.Fingerprint, Indicators: indicators,
		}},
	}}); err != nil {
		t.Fatalf("append retained decision event: %v", err)
	}
}

// upgradeBootRegimeSnapshot adds a lifecycle stage, ranked streaks, and
// freshness to the shared cache fixture so the streak, rule-stage, and
// decision guards reconcile real rows rather than an empty projection.
func upgradeBootRegimeSnapshot(asOf time.Time) *rpc.RegimeSnapshotResult {
	snapshot := regimeSnapshotCacheFixture(asOf, "pre-currency-policy history")
	snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
	since := asOf.Format("2006-01-02")
	fresh := func() *rpc.RegimeFreshness {
		return &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
	}
	snapshot.VIXTermStructure.Band = "green"
	snapshot.VIXTermStructure.Freshness = fresh()
	snapshot.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "green", Sessions: 1, Since: since}
	snapshot.CreditSpreads.Band = "green"
	snapshot.CreditSpreads.Freshness = fresh()
	snapshot.CreditSpreads.Streak = &rpc.StreakInfo{Band: "green", Sessions: 1, Since: since}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	return snapshot
}
