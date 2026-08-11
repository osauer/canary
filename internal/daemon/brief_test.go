package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"

	"os"
	"path/filepath"
	"slices"
	"strings"

	"testing"
	"time"
)

func dailyBriefPolicyTOML() string {
	return validRiskPolicyTOML
}

func TestBriefSnapshotPurityAndDegradedRows(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	for range 3 {
		res, _ := s.composeBrief(context.Background())
		if res.Ready.Regime.Status != rpc.BriefStatusUnavailable || res.Review.SessionPnL.Status != rpc.BriefStatusUnavailable {
			t.Fatalf("gateway rows not unavailable: regime=%+v session_pnl=%+v", res.Ready.Regime, res.Review.SessionPnL)
		}
		if res.Ready.Capital.Status == "" || res.Review.Reconcile.Status == "" || res.BriefFingerprint == "" {
			t.Fatalf("policy/process rows did not render: %+v", res)
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("brief.snapshot mutated state tree: before=%v after=%v", before, after)
	}
}

func stateTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func TestBriefNilMoneyAndGreeksDegradeWithoutZeroFill(t *testing.T) {
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "AAPL", SecType: "OPT", Right: "C", Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 1, Multiplier: 100},
	}}
	premium := briefPremiumAtRisk(pos, "EUR")
	if premium.Status != rpc.BriefStatusDegraded || premium.AmountBase != nil || premium.ExcludedLegs != 2 {
		t.Fatalf("premium=%+v", premium)
	}
	hedge := briefHedgeCost(pos, "EUR")
	if hedge.Status != rpc.BriefStatusDegraded || hedge.AmountBase != nil || hedge.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", hedge)
	}
}

func briefTestCurrentAccountDataAuthority(source rpc.AccountDataSource) *rpc.AccountDataAuthority {
	return &rpc.AccountDataAuthority{
		Scope:        rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
		Source:       source,
		Availability: rpc.AccountDataAvailable,
		Freshness:    rpc.AccountDataFreshnessCurrent,
		AsOf:         time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC),
	}
}

func TestBriefPortfolioRequiresCurrentAccountDataAuthority(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	dailyPnL := 125.0
	account := &rpc.AccountResult{
		NetLiquidation: 230175, DailyPnL: &dailyPnL, BaseCurrency: "EUR",
		Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourceAccountSummaryRequest),
	}
	currentPositions := briefTestCurrentAccountDataAuthority(rpc.AccountDataSourcePortfolioStream)

	tests := []struct {
		name      string
		authority *rpc.AccountDataAuthority
	}{
		{name: "missing"},
		{name: "unavailable", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}},
		{name: "stale", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessStale}},
		{name: "unknown age", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessUnknown}},
	}
	for _, tc := range tests {
		t.Run("positions "+tc.name, func(t *testing.T) {
			out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: tc.authority}, nil, nil, true)
			for name, row := range map[string]rpc.BriefRowState{
				"movers": out.Movers.BriefRowState, "premium": out.PremiumAtRisk.BriefRowState, "hedge": out.HedgeCost.BriefRowState,
			} {
				if row.Status != rpc.BriefStatusUnavailable {
					t.Fatalf("%s row=%+v, want unavailable", name, row)
				}
			}
			if out.PremiumAtRisk.AmountBase != nil || out.HedgeCost.AmountBase != nil {
				t.Fatalf("unavailable positions produced clean zero amounts: premium=%v hedge=%v", out.PremiumAtRisk.AmountBase, out.HedgeCost.AmountBase)
			}
		})
	}

	t.Run("account unavailable", func(t *testing.T) {
		unavailable := *account
		unavailable.Authority = &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}
		out := s.composeBriefPortfolio(&unavailable, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.Account.Status != rpc.BriefStatusUnavailable || out.Account.EquityBase != nil || out.Account.DailyPnLBase != nil {
			t.Fatalf("unavailable account row=%+v, want unavailable without money", out.Account)
		}
	})

	t.Run("current empty book", func(t *testing.T) {
		out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.PremiumAtRisk.Status != rpc.BriefStatusOK || out.HedgeCost.Status != rpc.BriefStatusOK ||
			out.PremiumAtRisk.AmountBase == nil || *out.PremiumAtRisk.AmountBase != 0 ||
			out.HedgeCost.AmountBase == nil || *out.HedgeCost.AmountBase != 0 {
			t.Fatalf("current empty book lost its genuine zero: premium=%+v hedge=%+v", out.PremiumAtRisk, out.HedgeCost)
		}
	})
}

func TestAlertEpisodeRegistryCurrentLifecycleSurvivesRestart(t *testing.T) {
	path := alertRegistryTestPath(t)
	store := openAlertRegistryTestStore(t, path)
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	positive := alertRegistryObservation(t, "lifecycle", base, true)
	opened, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), positive))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, opened, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	occurrence := opened.Candidates[0].OccurrenceKey

	outageAt := base.Add(time.Minute)
	outage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: outageAt,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{},
	}
	held, err := registry.Apply(t.Context(), alertRegistryEvaluation(outageAt, outage))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, held, rpc.AlertEpisodeOpen, rpc.AlertEvidenceUnavailable)
	if held.Candidates[0].OccurrenceKey != occurrence {
		t.Fatal("outage rotated active occurrence")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAlertRegistryTestStore(t, path)
	registry, err = newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	restarted, ok, err := registry.Snapshot(alertRegistryAuthority(), outageAt)
	if err != nil || !ok || restarted.Candidates[0].OccurrenceKey != occurrence {
		t.Fatalf("restart snapshot=%+v ok=%v err=%v", restarted, ok, err)
	}

	negativeAt := base.Add(2 * time.Minute)
	negative := positive
	negative.Active = false
	negative.ObservedAt, negative.EvidenceAsOf = negativeAt, negativeAt
	negative.EvidenceFingerprint = alertRegistryFingerprint("authoritative-negative")
	negative.ProducerDecisionReason = "classified_clear"
	recovered, err := registry.Apply(t.Context(), alertRegistryEvaluation(negativeAt, alertRegistryCompleteCoverage(negativeAt), negative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, recovered, rpc.AlertEpisodeRecovered, rpc.AlertEvidenceCurrent)
	if recovered.CurrentState != rpc.AlertSnapshotClear || recovered.Candidates[0].OccurrenceKey != occurrence {
		t.Fatalf("recovery changed occurrence or clear state: %+v", recovered)
	}

	confirmedAt := base.Add(3 * time.Minute)
	negative.ObservedAt, negative.EvidenceAsOf = confirmedAt, confirmedAt
	negative.EvidenceFingerprint = alertRegistryFingerprint("still-clear")
	clear, err := registry.Apply(t.Context(), alertRegistryEvaluation(confirmedAt, alertRegistryCompleteCoverage(confirmedAt), negative))
	if err != nil || clear.CurrentState != rpc.AlertSnapshotClear || len(clear.Candidates) != 0 {
		t.Fatalf("confirmed clear snapshot=%+v err=%v", clear, err)
	}

	reopenAt := base.Add(4 * time.Minute)
	positive.ObservedAt, positive.EvidenceAsOf = reopenAt, reopenAt
	positive.EvidenceFingerprint = alertRegistryFingerprint("reopened")
	reopened, err := registry.Apply(t.Context(), alertRegistryEvaluation(reopenAt, alertRegistryCompleteCoverage(reopenAt), positive))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, reopened, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	if reopened.Candidates[0].OccurrenceKey == occurrence {
		t.Fatal("reopen reused recovered occurrence")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func alertRegistryTestPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "daemon.db")
}

func openAlertRegistryTestStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("open alert registry test store: %v", err)
	}
	return store
}

func alertRegistryObservation(t *testing.T, identity string, at time.Time, active bool) alertEpisodeObservation {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, identity)
	if err != nil {
		t.Fatal(err)
	}
	return alertEpisodeObservation{
		EpisodeKey: episode, Source: rpc.AlertSourceStress, Kind: rpc.AlertKindPortfolioRisk,
		PresentationCode: rpc.AlertPresentationPortfolioStress, Active: active, Severity: rpc.AlertSeverityWatch,
		EvidenceFingerprint: alertRegistryFingerprint("evidence-" + identity), EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, ObservedAt: at,
		PolicyFingerprint: alertRegistryFingerprint("policy-v1"), ProducerDecisionReason: "classified_active",
	}
}

func alertRegistryEvaluation(at time.Time, coverage rpc.AlertCoverage, observations ...alertEpisodeObservation) alertEpisodeEvaluation {
	if observations == nil {
		observations = []alertEpisodeObservation{}
	}
	return alertEpisodeEvaluation{AuthorityScope: alertRegistryAuthority(), AsOf: at, Coverage: coverage, Observations: observations}
}

func alertRegistryAuthority() string {
	authority, err := rpc.BuildAlertAuthorityScope("DU-REGISTRY", rpc.AccountModePaper)
	if err != nil {
		panic(err)
	}
	return authority
}

func alertRegistryCompleteCoverage(at time.Time) rpc.AlertCoverage {
	return rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceStress},
	}
}

func alertRegistryFingerprint(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertAlertRegistryCandidate(t *testing.T, snapshot rpc.AlertCandidateSnapshot, state rpc.AlertEpisodeState, health rpc.AlertEvidenceHealth) {
	t.Helper()
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		t.Fatalf("invalid snapshot: %v", err)
	}
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].State != state || snapshot.Candidates[0].EvidenceHealth != health {
		t.Fatalf("candidate=%+v want state=%s health=%s", snapshot.Candidates, state, health)
	}
}

func alertShadowTestBrokerScope(t *testing.T) alertShadowBrokerScope {
	t.Helper()
	scope, err := newAlertShadowBrokerScope(brokerStateScope{Account: "DU-SHADOW", Mode: rpc.AccountModePaper})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestProtectionPersistenceUncertaintyIsPartialAndCannotClear(t *testing.T) {
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	summary := rpc.ProtectionCoverageSummary{
		AsOf: base, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
		ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}},
	}
	markProtectionSummaryPersistenceUncertain(&summary)
	batch := alertShadowMapProtection(alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, OrderSnapshotAsOf: base, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent,
		Scope: alertShadowTestBrokerScope(t), Summary: summary,
	}, base.Add(time.Second))
	if batch.Covered || batch.NegativeReady || batch.Status != alertShadowStatusPartial || batch.EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("uncertain lifecycle persistence was trusted as a negative: %+v", batch)
	}
	journalUnknown := false
	for _, row := range summary.ByUnderlying {
		journalUnknown = journalUnknown || row.Underlying == "ORDER_JOURNAL" && row.State == rpc.ProtectionCoverageStateUnknown
	}
	if len(summary.ByUnderlying) != 2 || !journalUnknown {
		t.Fatalf("uncertain journal row was not projected explicitly: %+v", summary)
	}
}

func TestDailyPnLCloseCaptureAuthorityPersistsAcrossRestart(t *testing.T) {
	databasePath := filepath.Join(privateTestDir(t), "daemon.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	first := dailyPnLCloseCaptureAuthority{}
	if err := first.bindCore(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	source := "paper|DU123"
	capture := persistedDailyPnLCloseCapture{
		SessionKey: "2026-07-31", DailyPnL: -433.7, BaseCurrency: "EUR",
		SessionClose: time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC),
		CapturedAt:   time.Date(2026, 7, 31, 20, 0, 9, 0, time.UTC),
	}
	if err := first.capture(t.Context(), source, capture); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, dailyPnLCloseCaptureStateKind)
	if err != nil || !ok {
		t.Fatalf("persisted capture document missing: ok=%v err=%v", ok, err)
	}
	if bytes.Contains(doc.JSON, []byte("DU123")) {
		t.Fatalf("persisted capture exposed account identity: %s", doc.JSON)
	}

	drifted := capture
	drifted.DailyPnL = -500
	if err := first.capture(t.Context(), source, drifted); err != nil {
		t.Fatal(err)
	}
	if got, _ := first.captureFor(source); got.DailyPnL != capture.DailyPnL {
		t.Fatalf("same-session recapture overwrote the close print: %+v", got)
	}

	liveCapture := capture
	liveCapture.DailyPnL = 12
	if err := first.capture(t.Context(), "live|U999", liveCapture); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedStore.Close() })
	restarted := dailyPnLCloseCaptureAuthority{}
	if err := restarted.bindCore(t.Context(), restartedStore); err != nil {
		t.Fatal(err)
	}
	got, ok := restarted.captureFor(source)
	if !ok || got != capture {
		t.Fatalf("restarted capture = %+v ok=%v, want %+v", got, ok, capture)
	}
	if live, ok := restarted.captureFor("live|U999"); !ok || live.DailyPnL != 12 {
		t.Fatalf("second scope capture = %+v ok=%v", live, ok)
	}

	next := capture
	next.SessionKey = "2026-08-03"
	next.DailyPnL = 88.25
	if err := restarted.capture(t.Context(), source, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := restarted.captureFor(source); got.SessionKey != "2026-08-03" || got.DailyPnL != 88.25 {
		t.Fatalf("newer session did not replace retained capture: %+v", got)
	}
}

func TestRestartAfterRawRetentionResumesProjectionWithoutRedownload(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	dbPath := filepath.Join(stateHome, "daemon.db")
	firstCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	first := &Server{now: func() time.Time { return now }, cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}}, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := first.flexFetch.bindCore(t.Context(), firstCore); err != nil {
		t.Fatal(err)
	}
	writeFlexFixture(t, "flex-crash.xml", "20260721;070000", "20260714", "20260720", "")
	dir, _ := flexStatementsDirPath()
	if err := os.Chtimes(filepath.Join(dir, "flex-crash.xml"), now, now); err != nil {
		t.Fatal(err)
	}
	target, _ := flexDailyWindow(now)
	first.flexFetch.mu.Lock()
	first.flexFetch.state.Stage = rpc.ReconReportStateChecking
	first.flexFetch.state.LastAttempt = now
	first.flexFetch.state.TargetDate = target
	if err := first.flexFetch.persistLocked(t.Context()); err != nil {
		first.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	first.flexFetch.mu.Unlock()
	if err := firstCore.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(flexRetryAfterFail - time.Second)
	restartedCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedCore.Close() })
	restarted := &Server{now: func() time.Time { return now }, cfg: first.cfg, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := restarted.flexFetch.bindCore(t.Context(), restartedCore); err != nil {
		t.Fatal(err)
	}
	if got := restarted.flexFetchStatusAt(now); got.State != rpc.ReconReportStateRetryScheduled || got.Reason != rpc.ReconReportReasonProjectionFailed {
		t.Fatalf("recovered post-retain status = %+v, want scheduled projection retry", got)
	}
	now = now.Add(time.Second)
	var fetchCalls, projectionCalls int
	restarted.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return flexFetchOutcome{}, errors.New("broker redownload must not run")
	}
	restarted.flexProjectionFn = func(context.Context) error { projectionCalls++; return nil }
	if !restarted.startFlexFetch(t.Context(), false) {
		t.Fatal("projection recovery did not start")
	}
	restarted.flexFetch.wg.Wait()
	if fetchCalls != 0 || projectionCalls != 1 || restarted.flexFetchStatusAt(now).State != rpc.ReconReportStateCurrent {
		t.Fatalf("recovery fetch=%d projection=%d status=%+v", fetchCalls, projectionCalls, restarted.flexFetchStatusAt(now))
	}
}

func TestDrawdownLatchFlexRechecksBackOffUntilCoverage(t *testing.T) {
	current := berlinTestTime(t, 2026, 8, 10, 9, 0)
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	s := &Server{
		now:    func() time.Time { return current },
		cfg:    &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}},
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := s.flexFetch.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	latchAt := current.Add(-30 * time.Minute)
	s.flexFetch.mu.Lock()
	s.flexFetch.state.LastAttempt = latchAt.Add(-time.Hour)
	if err := s.flexFetch.persistLocked(t.Context()); err != nil {
		s.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	s.flexFetch.mu.Unlock()
	var fetchCalls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return flexFetchOutcome{}, errors.New("test fetch stopped")
	}
	fire := func(want bool, note string) {
		t.Helper()
		if got := s.maybeFetchFlexForLatch(t.Context(), latchAt); got != want {
			t.Fatalf("%s: recheck fired=%v, want %v (calls=%d)", note, got, want, fetchCalls)
		}
		s.flexFetch.wg.Wait()
	}

	fire(true, "first post-latch recheck")
	fire(false, "immediate repeat")
	// The failed attempt scheduled its own retry window; the young-latch
	// half-hourly cadence resumes once that window passes.
	current = current.Add(31 * time.Minute)
	fire(true, "half-hourly recheck after the retry window")
	// An older latch widens to the two-hourly cadence.
	current = current.Add(time.Hour + 29*time.Minute)
	fire(false, "two-hourly cadence not yet due")
	current = current.Add(32 * time.Minute)
	fire(true, "two-hourly recheck")
	if fetchCalls != 3 {
		t.Fatalf("fetch calls=%d, want three", fetchCalls)
	}

	// Retained coverage reaching the latch day ends the rechecks.
	writeFlexFixture(t, "flex-latch-covered.xml", "20260810;170000", "20260810", "20260810", "")
	current = current.Add(6 * time.Hour)
	fire(false, "coverage reached the latch day")
	if fetchCalls != 3 {
		t.Fatalf("fetch calls after coverage=%d, want three", fetchCalls)
	}
}

func berlinTestTime(t *testing.T, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	berlin, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, berlin)
}

func writeFlexFixture(t *testing.T, name, whenGenerated, from, to, body string) {
	writeFlexFixtureForAccount(t, name, "U1234567", whenGenerated, from, to, body)
}

func writeFlexFixtureForAccount(t *testing.T, name, account, whenGenerated, from, to, body string) {
	t.Helper()
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="%s" fromDate="%s" toDate="%s" whenGenerated="%s">
%s
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`, account, from, to, whenGenerated, body)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cashLine(id, typ string, amount float64, date string) string {
	return fmt.Sprintf(`   <CashTransactions><CashTransaction transactionID=%q type=%q currency="EUR" fxRateToBase="1" amount="%f" dateTime="%s;120000" settleDate=%q description="FIXTURE" /></CashTransactions>`, id, typ, amount, date, date)
}

func equityRow(date string, total float64) string {
	return fmt.Sprintf(`   <EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate=%q total="%f" /></EquitySummaryInBase>`, date, total)
}

func newReconTestServer(t *testing.T) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	return s
}

func newReconV3TestServer(t *testing.T) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	return s
}

func declare(t *testing.T, s *Server, typ string, amount float64, effectiveAt string) {
	t.Helper()
	var eff time.Time
	if effectiveAt != "" {
		var err error
		if eff, err = time.Parse("2006-01-02", effectiveAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.riskCapital.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: typ, AmountBase: amount, EffectiveAt: eff}, rpc.OrderOriginHumanTTY,
		s.riskPolicies.snapshot().policy); err != nil {
		t.Fatal(err)
	}
}

func recentGenerated() string { return time.Now().UTC().Format("20060102") + ";060000" }

func seedReconRuntime(s *Server, genesis time.Time) {
	s.riskCapital.mu.Lock()
	defer s.riskCapital.mu.Unlock()
	s.riskCapital.state.GenesisAt = genesis
	s.riskCapital.state.Seeded = true
}

func TestReconFiltersSiblingAccountsBeforeMerge(t *testing.T) {
	s := newReconV3TestServer(t)
	generated := recentGenerated()
	writeFlexFixtureForAccount(t, "flex-selected.xml", "U1234567", generated, "20260708", "20260708",
		cashLine("selected-flow", "Deposits/Withdrawals", 100, "20260708")+"\n"+equityRow("20260708", 1000))
	writeFlexFixtureForAccount(t, "flex-sibling.xml", "SIBLING-SECRET", generated, "20260708", "20260708",
		cashLine("sibling-flow", "Deposits/Withdrawals", 900, "20260708")+"\n"+equityRow("20260708", 9000))

	report := s.buildReconReport()
	if report.Status != rpc.ReconStatusActive || report.Counts[reconSkippedSiblingStatementsCount] != 1 {
		t.Fatalf("account-scoped report status=%s counts=%v", report.Status, report.Counts)
	}
	if report.StatementCumFlowsBase == nil || *report.StatementCumFlowsBase != 100 || len(report.Confirmed) != 1 || report.Confirmed[0].LineID != "cash-selected-flow" {
		t.Fatalf("sibling statement participated in reconciliation: %+v", report)
	}
	if report.Equity == nil || report.Equity.StatementTotalBase != 1000 {
		t.Fatalf("sibling equity won the selected account's day: %+v", report.Equity)
	}

	backtest := s.buildReconBacktest()
	if backtest.Status != rpc.ReconStatusActive || backtest.FlowCounts[reconSkippedSiblingStatementsCount] != 1 || len(backtest.Flows) != 1 || backtest.Flows[0].LineID != "cash-selected-flow" || backtest.EquityDays != 1 {
		t.Fatalf("account-scoped backtest = %+v", backtest)
	}
	raw, err := json.Marshal(struct {
		Report   *rpc.ReconResult
		Backtest *rpc.ReconBacktestResult
	}{report, backtest})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SIBLING-SECRET", "sibling-flow", "9000"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("account-scoped result leaked sibling detail %q: %s", forbidden, raw)
		}
	}
}

func TestReconAmbiguityNeverAutoResolves(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("amb1", "Deposits/Withdrawals", 10000, "20260708"))
	declare(t, s, "deposit", 10000, "2026-07-08")
	declare(t, s, "deposit", 10001, "2026-07-09")

	rep := s.buildReconReport()
	if rep.Counts[rpc.ReconAmbiguous] != 1 {
		t.Fatalf("counts = %v, want one ambiguous", rep.Counts)
	}
	if rep.Counts["matched"] != 0 {
		t.Fatalf("ambiguous line must not also match (counts %v)", rep.Counts)
	}
}

func TestReconV3StatementAuthorityAndBridgeBoundary(t *testing.T) {
	s := newReconV3TestServer(t)
	seedReconRuntime(s, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	writeFlexFixture(t, "flex-bridge.xml", recentGenerated(), "20260701", "20260710",
		cashLine("matched", "Deposits/Withdrawals", 1000, "20260705"))
	declare(t, s, "deposit", 1005, "2026-07-05")
	declare(t, s, "withdrawal", 200, "2026-07-10")
	declare(t, s, "deposit", 300, "2026-07-11")
	declare(t, s, "deposit", 400, "2026-06-30")

	rep := s.buildReconReport()
	if rep.StatementCumFlowsBase == nil || *rep.StatementCumFlowsBase != 1300 {
		t.Fatalf("statement-authoritative flows = %v, want 1300", rep.StatementCumFlowsBase)
	}
	if rep.Counts["matched"] != 1 || rep.Counts[rpc.ReconLedgerOnly] != 2 || rep.Unresolved != 2 {
		t.Fatalf("counts=%v unresolved=%d", rep.Counts, rep.Unresolved)
	}
	for _, ex := range rep.Exceptions {
		if ex.EventAt.Format("2006-01-02") == "2026-07-11" {
			t.Fatalf("bridge declaration became an exception: %+v", ex)
		}
	}
}

var testLiveObserveScope = brokerStateScope{Account: "U111", Mode: rpc.AccountModeLive}

func testConstitution() *risk.Constitution {
	return &risk.Constitution{
		Kind:          risk.ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: risk.ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: risk.ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
		},
		Override: risk.ConstitutionOverride{MaxDurationHours: new(24)},
		Recon: risk.ConstitutionRecon{
			AmountTolerancePct:     new(0.5),
			AmountToleranceMin:     new(5.0),
			DateWindowBusinessDays: new(3),
			MaxReportAgeDays:       new(4),
		},
		Cadence: risk.ConstitutionCadence{
			Morning: risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
		},
	}
}

func newTestRiskCapitalStore(t *testing.T) *riskCapitalStore {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return &riskCapitalStore{now: time.Now}
}

func reconcileNow(t *testing.T, st *riskCapitalStore) {
	t.Helper()
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRiskCapitalObserveSeedsAndTracksPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier = %s (%v), want ok", rep.Tier, rep.Reasons)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak = %v, want 260000", rep.AdjustedPeakBase)
	}

	st.Observe(252000, now.Add(-time.Minute), c, testLiveObserveScope)
	rep = st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierWarn {
		t.Fatalf("tier = %s, want warn", rep.Tier)
	}
	if rep.BlockLatched {
		t.Fatal("warn tier must not latch")
	}

	st.Observe(258000, now, c, testLiveObserveScope)
	if rep = st.Report(c, nil, testLiveObserveScope); rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after recovery = %s, want ok (warn is self-clearing)", rep.Tier)
	}
}

func TestRiskCapitalBlockLatchPersistsAndResets(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierBlock || !rep.BlockLatched {
		t.Fatalf("tier = %s latched = %v, want block/true", rep.Tier, rep.BlockLatched)
	}

	st.Observe(262000, now.Add(-time.Minute), c, testLiveObserveScope)
	if rep = st.Report(c, nil, testLiveObserveScope); rep.Tier != risk.CapitalTierBlock {
		t.Fatalf("tier after recovery = %s, want block (latched)", rep.Tier)
	}

	st2 := &riskCapitalStore{now: time.Now}
	if rep = st2.Report(c, nil, testLiveObserveScope); !rep.BlockLatched {
		t.Fatal("latch must survive a restart via risk-capital-state.json")
	}

	if err := st2.ResetDrawdown("", c); err == nil {
		t.Fatal("reset without a reason must fail")
	}
	if err := st2.ResetDrawdown("weekly review 2026-07-12: de-risked, resuming at reduced size", c); err != nil {
		t.Fatal(err)
	}
	rep = st2.Report(c, nil, testLiveObserveScope)
	if rep.BlockLatched || rep.Tier == risk.CapitalTierBlock {
		t.Fatalf("after reset: tier = %s latched = %v, want unlatched", rep.Tier, rep.BlockLatched)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 262000 {
		t.Fatalf("peak after reset = %v, want re-based to last equity 262000", rep.AdjustedPeakBase)
	}
}

func testConstitutionV3() *risk.Constitution {
	c := testConstitution()
	c.PolicyVersion = 3
	c.Recon.MaxEquityDivergencePct = new(1.0)
	return c
}

func TestDrawdownLatchEngagesProvisionallyAndWithdrawalDissolvesIt(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitutionV3()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if !rep.BlockLatched || !rep.LatchProvisional {
		t.Fatalf("latched=%v provisional=%v, want a provisional latch", rep.BlockLatched, rep.LatchProvisional)
	}
	if !strings.Contains(strings.Join(rep.Reasons, " "), "provisionally") {
		t.Fatalf("provisional latch reasons = %v", rep.Reasons)
	}

	// A statement window ending before the latch day decides nothing.
	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now.Add(-72 * time.Hour),
	}, c); err != nil {
		t.Fatal(err)
	}
	if rep = st.Report(c, nil, testLiveObserveScope); !rep.BlockLatched || !rep.LatchProvisional {
		t.Fatalf("short coverage decided the latch: latched=%v provisional=%v", rep.BlockLatched, rep.LatchProvisional)
	}

	// Coverage reaching the latch day with a withdrawal that explains the
	// whole drop dissolves the latch without any human action.
	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now,
		Flows:     []reconFlow{{id: "wd-1", typ: "Deposits/Withdrawals", valueDate: now.Add(-2 * time.Minute), amountBase: -20000}},
		FlowsBase: -20000,
	}, c); err != nil {
		t.Fatal(err)
	}
	rep = st.Report(c, nil, testLiveObserveScope)
	if rep.BlockLatched || rep.LatchProvisional || !rep.LatchedAt.IsZero() {
		t.Fatalf("withdrawal-explained latch did not dissolve: %+v", rep)
	}
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after dissolution = %s (%v), want ok", rep.Tier, rep.Reasons)
	}
}

func TestDrawdownLatchDepositNeverAssistsDissolution(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitutionV3()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	if rep := st.Report(c, nil, testLiveObserveScope); !rep.BlockLatched || !rep.LatchProvisional {
		t.Fatalf("latched=%v provisional=%v, want a provisional latch", rep.BlockLatched, rep.LatchProvisional)
	}

	// A latch-day deposit deepens the replayed drawdown; it must promote,
	// never dissolve — only withdrawals explain a drop.
	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now,
		Flows:     []reconFlow{{id: "dep-1", typ: "Deposits/Withdrawals", valueDate: now.Add(-2 * time.Minute), amountBase: 20000}},
		FlowsBase: 20000,
	}, c); err != nil {
		t.Fatal(err)
	}
	rep := st.Report(c, nil, testLiveObserveScope)
	if !rep.BlockLatched || rep.LatchProvisional {
		t.Fatalf("deposit-day latch: latched=%v provisional=%v, want durable promotion", rep.BlockLatched, rep.LatchProvisional)
	}
}

func TestDrawdownLatchProvisionalSurvivesRestart(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitutionV3()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	if rep := st.Report(c, nil, testLiveObserveScope); !rep.BlockLatched || !rep.LatchProvisional {
		t.Fatalf("latched=%v provisional=%v, want a provisional latch", rep.BlockLatched, rep.LatchProvisional)
	}

	// A restart must reload the latch still provisional, so the statement
	// window can decide it later instead of defaulting to a durable latch.
	st2 := &riskCapitalStore{now: time.Now}
	rep := st2.Report(c, nil, testLiveObserveScope)
	if !rep.BlockLatched || !rep.LatchProvisional {
		t.Fatalf("after restart: latched=%v provisional=%v, want provisional preserved", rep.BlockLatched, rep.LatchProvisional)
	}
}

func TestDrawdownLatchPromotesToDurableWithoutExplainingFlow(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitutionV3()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-4*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	// Mark recovery keeps the latch: the engagement equity is frozen.
	st.Observe(258000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	if rep := st.Report(c, nil, testLiveObserveScope); rep.Tier != risk.CapitalTierBlock {
		t.Fatalf("tier after mark recovery = %s, want block (latched)", rep.Tier)
	}

	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now,
	}, c); err != nil {
		t.Fatal(err)
	}
	rep := st.Report(c, nil, testLiveObserveScope)
	if !rep.BlockLatched || rep.LatchProvisional {
		t.Fatalf("unexplained latch did not promote: latched=%v provisional=%v", rep.BlockLatched, rep.LatchProvisional)
	}
	if !strings.Contains(strings.Join(rep.Reasons, " "), "human reset") {
		t.Fatalf("durable latch reasons = %v", rep.Reasons)
	}

	// A later window with a fresh withdrawal cannot dissolve a durable latch.
	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now.Add(24 * time.Hour),
		Flows:     []reconFlow{{id: "wd-late", typ: "Deposits/Withdrawals", valueDate: now, amountBase: -20000}},
		FlowsBase: -20000,
	}, c); err != nil {
		t.Fatal(err)
	}
	if rep = st.Report(c, nil, testLiveObserveScope); !rep.BlockLatched {
		t.Fatal("durable latch dissolved from statement flows; only a human reset may clear it")
	}

	if err := st.ResetDrawdown("reviewed the confirmed trading loss; resuming at reduced size", c); err != nil {
		t.Fatal(err)
	}
	if rep = st.Report(c, nil, testLiveObserveScope); rep.BlockLatched || rep.LatchProvisional {
		t.Fatalf("reset left latch state behind: %+v", rep)
	}
}

func TestPreTwoStageLatchStaysDurableUnderStatements(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitutionV3()
	reconcileNow(t, st)
	now := time.Now()
	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	st.mu.Lock()
	// A latch persisted before the two-stage semantics carries neither field.
	st.state.LatchProvisional = false
	st.state.LatchEquityBase = 0
	st.mu.Unlock()

	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{
		Scope: testLiveObserveScope, CoverageTo: now,
		Flows:     []reconFlow{{id: "wd-legacy", typ: "Deposits/Withdrawals", valueDate: now.Add(-2 * time.Minute), amountBase: -20000}},
		FlowsBase: -20000,
	}, c); err != nil {
		t.Fatal(err)
	}
	if rep := st.Report(c, nil, testLiveObserveScope); !rep.BlockLatched || rep.LatchProvisional {
		t.Fatalf("pre-two-stage latch was decided by statements: %+v", rep)
	}
}

func TestDrawdownLatchStatementReplayDecidesProvisionalLatch(t *testing.T) {
	day := time.Now().UTC().Format("20060102")
	for _, tc := range []struct {
		name      string
		body      string
		dissolved bool
	}{
		{"withdrawal_explains_drop", cashLine("latch-wd", "Deposits/Withdrawals", -20000, day) + "\n" + equityRow(day, 240000), true},
		{"no_flow_promotes_to_durable", equityRow(day, 240000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newReconV3TestServer(t)
			pol := s.riskPolicies.snapshot().policy
			scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
			now := time.Now()
			reconcileNow(t, s.riskCapital)
			s.riskCapital.Observe(260000, now.Add(-90*time.Minute), pol, scope)
			s.riskCapital.Observe(240000, now.Add(-60*time.Minute), pol, scope)
			rep := s.riskCapital.Report(pol, nil, scope)
			if !rep.BlockLatched || !rep.LatchProvisional {
				t.Fatalf("latched=%v provisional=%v, want a provisional latch", rep.BlockLatched, rep.LatchProvisional)
			}

			writeFlexFixture(t, "flex-latch.xml", recentGenerated(), day, day, tc.body)
			s.evaluateRiskPolicyV3Reconciliation()

			rep = s.riskCapital.Report(pol, nil, scope)
			if tc.dissolved {
				if rep.BlockLatched || rep.LatchProvisional {
					t.Fatalf("withdrawal-explained latch did not dissolve: %+v", rep)
				}
			} else if !rep.BlockLatched || rep.LatchProvisional {
				t.Fatalf("unexplained latch did not promote: latched=%v provisional=%v", rep.BlockLatched, rep.LatchProvisional)
			}
		})
	}
}

func newRiskPolicyTestServer(t *testing.T, policyTOML string) *Server {
	t.Helper()
	m, _ := newTestRiskPolicyManager(t, policyTOML)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{
		now:          time.Now,
		riskPolicies: m,
		riskCapital:  &riskCapitalStore{now: time.Now},
		endpoint:     discover.Endpoint{Port: 7496, Account: "U1234567"},
	}
	return s
}

func rawParams(t *testing.T, v any) *rpc.Request {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &rpc.Request{Params: raw}
}

func TestRiskPolicyWritesRejectAgentOrigin(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(origin string) error
	}{
		{"capital_event", func(origin string) error {
			_, err := s.handleRiskPolicyCapitalEvent(ctx, rawParams(t, rpc.CapitalEventParams{Type: "deposit", AmountBase: 100, Origin: origin}))
			return err
		}},
		{"override", func(origin string) error {
			_, err := s.handleRiskPolicyOverride(ctx, rawParams(t, rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 1, Origin: origin}))
			return err
		}},
		{"reset_drawdown", func(origin string) error {
			_, err := s.handleRiskPolicyResetDrawdown(ctx, rawParams(t, rpc.ResetDrawdownParams{Reason: "r", Origin: origin}))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, origin := range []string{rpc.OrderOriginAgent, "", "made-up-origin"} {
				if err := tc.call(origin); err == nil || !strings.Contains(err.Error(), "human-only") {
					t.Fatalf("origin %q: err = %v, want human-only rejection", origin, err)
				}
			}
			if err := tc.call(rpc.OrderOriginHumanTTY); err != nil {
				t.Fatalf("human origin: err = %v, want success", err)
			}
		})
	}
}

const validRiskPolicyTOML = `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1

[capital]
base_currency = "EUR"
protected_floor = 200000.0
declared_risk_capital = 50000.0
max_equity_age_minutes = 240
max_unreconciled_days = 7

[drawdown]
warn_consumed_pct = 15.0
block_consumed_pct = 30.0
block_enforcement = "shadow"

[override]
max_duration_hours = 24

[recon]
amount_tolerance_pct = 0.5
amount_tolerance_min = 5.0
date_window_business_days = 3
max_report_age_days = 4

[cadence.morning]
class = "advisory"
`

func validRiskPolicyV3TOML() string {
	v3 := strings.Replace(validRiskPolicyTOML, "policy_version = 1", "policy_version = 3", 1)
	return strings.Replace(v3, "max_report_age_days = 4", "max_report_age_days = 4\nmax_equity_divergence_pct = 1.0", 1)
}

func newTestRiskPolicyManager(t *testing.T, contents string) (*riskPolicyManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "risk-policy.toml")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newRiskPolicyManager(path, time.Second, time.Now)
	m.reload()
	return m, path
}

func TestRiskPolicyManagerLoadsV3AndRejectsV3KeyUnderV2(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyV3TOML())
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive || snap.policy == nil || snap.policy.PolicyVersion != 3 || snap.policy.Recon.MaxEquityDivergencePct == nil {
		t.Fatalf("v3 snapshot = %+v", snap)
	}
	v2WithKey := strings.Replace(validRiskPolicyTOML, "max_report_age_days = 4", "max_report_age_days = 4\nmax_equity_divergence_pct = 1.0", 1)
	m, _ = newTestRiskPolicyManager(t, v2WithKey)
	snap = m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError || !strings.Contains(snap.message, "requires policy_version >= 3") {
		t.Fatalf("v2 key snapshot status=%s message=%q", snap.status, snap.message)
	}
}

func TestBriefNarrativeMarksOnlyAccountMoneySensitive(t *testing.T) {
	amount := 12345.67
	review := briefNarrativeReview(rpc.BriefReviewSection{
		SessionPnL: rpc.BriefAccountRow{BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusOK}, DailyPnLBase: &amount, EquityBase: &amount, BaseCurrency: "USD"},
	}, rpc.BriefSessionRow{})
	ready := briefNarrativeReady(rpc.BriefReadySection{
		Capital:       rpc.BriefCapitalRow{BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusOK}, Tier: "normal", DrawdownBase: &amount, AdjustedPeakBase: &amount, BaseCurrency: "USD"},
		PremiumAtRisk: rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusOK}, AmountBase: &amount, BaseCurrency: "USD"},
		HedgeCost:     rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusOK}, AmountBase: &amount, BaseCurrency: "USD"},
	})

	p := &briefProse{}
	p.figure("42.5%")
	var sensitive, publicFigures int
	for _, paragraph := range append(append(review, ready...), p.done()...) {
		for _, run := range paragraph.Runs {
			if run.AccountSensitive {
				sensitive++
				if run.Role != rpc.BriefRunRoleFigure {
					t.Fatalf("sensitive run role=%q text=%q", run.Role, run.Text)
				}
			} else if run.Role == rpc.BriefRunRoleFigure {
				publicFigures++
			}
		}
	}
	if sensitive < 5 || publicFigures == 0 {
		t.Fatalf("sensitive=%d public_figures=%d", sensitive, publicFigures)
	}
}

func TestBriefNarrativeNamesPostLatchReportCheck(t *testing.T) {
	latchedAt := time.Date(2026, 8, 10, 6, 14, 0, 0, time.UTC)
	checkedAt := time.Date(2026, 8, 10, 14, 44, 0, 0, time.UTC)
	paragraphs := briefNarrativeReady(rpc.BriefReadySection{
		Latch: rpc.BriefLatchRow{
			BriefRowState:    rpc.BriefRowState{Status: rpc.BriefStatusAttention},
			Latched:          true,
			At:               latchedAt,
			ReportCoverageTo: latchedAt.Add(-72 * time.Hour),
			ReportCheckedAt:  checkedAt,
		},
	})
	var text strings.Builder
	for _, paragraph := range paragraphs {
		for _, run := range paragraph.Runs {
			text.WriteString(run.Text)
		}
	}
	got := text.String()
	if !strings.Contains(got, "Canary checked IBKR again") || !strings.Contains(got, "The newest daily report still covers through") {
		t.Fatalf("drawdown report status missing from narrative: %q", got)
	}
}

func TestBriefNarrativeUsesHumanMarketWords(t *testing.T) {
	got := briefRegimeReading(rpc.BriefRegimeRow{Stage: "early_warning", Verdict: "Stress signal present"})
	if got != "market conditions are in an early warning" {
		t.Fatalf("regime words = %q", got)
	}
	if strings.Contains(got, "_") || strings.Contains(got, "verdict") || strings.Contains(got, "stage") {
		t.Fatalf("technical regime vocabulary leaked into narrative: %q", got)
	}
}

func TestBriefNarrativeDoesNotAssignUnresolvedEarningsToOperator(t *testing.T) {
	res := &rpc.BriefResult{Ready: rpc.BriefReadySection{MarketEvents: []rpc.BriefMarketEventRow{{
		BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusAttention},
		Kind:          "earnings",
		Count:         1,
	}}}}
	for _, topic := range briefFlaggedTopics(briefTopics(res)) {
		if topic.label == "held-name earnings" {
			t.Fatal("unresolved earnings date was assigned to the operator")
		}
	}
}

func TestBriefGammaSnapshotServesCurrentCompletedResult(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	asOf := now.Add(-48 * time.Hour)
	job := &gammaComputation{
		sessionKey:  "2026-08-07",
		scope:       rpc.GammaZeroScopeCombined,
		startedAt:   asOf.Add(-time.Minute),
		completedAt: asOf,
		done:        make(chan struct{}),
		result:      &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeCombined, AsOf: asOf},
	}
	close(job.done)
	cache := newGammaZeroCache()
	cache.slots = map[string]*gammaSlot{rpc.GammaZeroScopeCombined: {current: job}}
	s := &Server{zeroGamma: cache, now: func() time.Time { return now }}

	got := s.briefGammaSnapshot()
	if got == nil || got.Status != rpc.GammaZeroStatusReady || got.Result == nil || !got.Result.AsOf.Equal(asOf) {
		t.Fatalf("brief gamma snapshot=%+v, want completed current cache result", got)
	}
}

func TestBriefRulesStatusPreservesEveryCanonicalOutcome(t *testing.T) {
	current := &rpc.RulesResult{Rules: []risk.RuleRow{
		{Status: risk.RuleStatusPass},
		{Status: risk.RuleStatusInfo},
		{Status: risk.RuleStatusWatch},
		{Status: risk.RuleStatusAct},
		{Status: risk.RuleStatusUnknown},
		{Status: risk.RuleStatusNotEvaluated},
	}}
	got := briefRulesStatus(current)
	if got.Pass != 1 || got.Info != 1 || got.Watch != 1 || got.Act != 1 || got.Unknown != 1 || got.NotEvaluated != 1 || got.Status != rpc.BriefStatusAttention {
		t.Fatalf("brief rules=%+v, want one of every canonical outcome", got)
	}

	neutral := briefRulesStatus(&rpc.RulesResult{Rules: []risk.RuleRow{
		{Status: risk.RuleStatusPass}, {Status: risk.RuleStatusInfo}, {Status: risk.RuleStatusNotEvaluated},
	}})
	if neutral.Status != rpc.BriefStatusOK || neutral.Unknown != 0 || neutral.Info != 1 || neutral.NotEvaluated != 1 || !strings.Contains(neutral.Detail, "1 not evaluated") {
		t.Fatalf("neutral brief rules=%+v", neutral)
	}

	future := briefRulesStatus(&rpc.RulesResult{Rules: []risk.RuleRow{{Status: "future_status"}}})
	if future.Status != rpc.BriefStatusDegraded || future.Unknown != 1 {
		t.Fatalf("future brief rules=%+v, want fail-closed unknown", future)
	}
}

func TestRulebookEconomicNamesNeutralizesOnlyVerifiedTerminalStock(t *testing.T) {
	terminal := risk.NameInput{
		Symbol: "CANCELLED", StockConID: 101, StockSecType: "STK", UnderlyingSecType: "STK",
		ExposureBase: 9_999, MarketValueBase: 9_999, HasStockLeg: true,
	}
	active := risk.NameInput{Symbol: "ACTIVE", ExposureBase: 10_000, ExposureBaseComplete: true, HasStockLeg: true}
	earnings := map[string]risk.EarningsInput{
		"CANCELLED": {TerminalNonReporting: true, Source: "verified_terminal", Reason: "equity_interests_cancelled"},
	}

	projected := rulebookEconomicNames([]risk.NameInput{terminal, active}, earnings)
	if got := projected[0]; got.Symbol != terminal.Symbol || got.StockConID != terminal.StockConID || got.StockSecType != terminal.StockSecType || !got.ExposureBaseComplete || got.ExposureBase != 0 || got.MarketValueBase != 0 || got.HasStockLeg || len(got.Legs) != 0 {
		t.Fatalf("terminal projection=%+v", got)
	}
	if got := projected[1]; got.Symbol != active.Symbol || got.ExposureBase != active.ExposureBase || got.ExposureBaseComplete != active.ExposureBaseComplete || got.HasStockLeg != active.HasStockLeg {
		t.Fatalf("active name changed: got=%+v want=%+v", got, active)
	}

	nlv := 100_000.0
	eval := risk.EvaluateRulebook(risk.RuleInputs{
		Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true}, NLVBase: &nlv,
		Names: projected, Earnings: earnings,
	}, risk.DefaultRulebookPolicy())
	if got := eval.Rows[11]; got.Status == risk.RuleStatusUnknown {
		t.Fatalf("verified terminal stock still poisoned hedge integrity: %+v", got)
	}
	for _, index := range []int{5, 6, 7} {
		if len(eval.Rows[index].Exempt) != 1 || eval.Rows[index].Exempt[0].Symbol != terminal.Symbol {
			t.Fatalf("rule %d lost terminal exemption: %+v", index+1, eval.Rows[index])
		}
	}

	unverified := rulebookEconomicNames([]risk.NameInput{terminal}, map[string]risk.EarningsInput{})
	if unverified[0].ExposureBase != terminal.ExposureBase || unverified[0].ExposureBaseComplete {
		t.Fatalf("unverified terminal candidate was neutralized: %+v", unverified[0])
	}
	failedClosed := risk.EvaluateRulebook(risk.RuleInputs{
		Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true}, NLVBase: &nlv,
		Names: unverified,
	}, risk.DefaultRulebookPolicy())
	if got := failedClosed.Rows[11]; got.Status != risk.RuleStatusUnknown {
		t.Fatalf("unverified candidate did not fail closed: %+v", got)
	}
}
