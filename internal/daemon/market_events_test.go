package daemon

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"testing"
	"time"
)

func openMarketTestCoreStore(t *testing.T) *corestore.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod market authority dir: %v", err)
	}
	store, err := corestore.Open(context.Background(), corestore.Options{
		Path: filepath.Join(dir, "daemon.db"),
	})
	if err != nil {
		t.Fatalf("open market authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMarketEventBorrowFeeFailurePersistsAcrossRestartAndSuccessSupersedes(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	failedAt := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return failedAt })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}

	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect, true)
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	_, health, err := cache.loadBorrowFees(context.Background(), failedAt)
	if err == nil || fetchCalls != 1 || health.LastFailure == nil {
		t.Fatalf("first failure calls=%d health=%+v err=%v", fetchCalls, health, err)
	}
	if health.LastFailure.Code != rpc.SourceFailureTimeout || health.LastFailure.Stage != rpc.SourceFailureStageFTPControlConnect || !health.LastFailure.Retryable {
		t.Fatalf("typed failure=%+v", health.LastFailure)
	}
	if strings.Contains(strings.Join(health.Notes, " "), "dial tcp") {
		t.Fatalf("raw transport text crossed health boundary: %+v", health.Notes)
	}

	if _, ok, err := authority.LatestObservation(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind); err != nil || ok {
		t.Fatalf("failure observation retained ok=%v err=%v", ok, err)
	}

	within := failedAt.Add(time.Minute)
	restarted := newMarketEventCache(func() time.Time { return within })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	_, health, err = restarted.loadBorrowFees(context.Background(), within)
	if err == nil || fetchCalls != 1 || health.RefreshState != rpc.SourceRefreshFetchFailedBackoff || health.LastFailure == nil {
		t.Fatalf("restart backoff calls=%d health=%+v err=%v", fetchCalls, health, err)
	}

	recoveredAt := failedAt.Add(marketEventsBorrowFeeRetryAfter + time.Minute)
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{
			AsOf: recoveredAt, SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
			Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65, Available: 1500}},
		}, nil
	}
	entry, health, err := restarted.loadBorrowFees(context.Background(), recoveredAt)
	if err != nil || fetchCalls != 2 || len(entry.Symbols) != 1 || health.LastFailure != nil || health.Status != rpc.SourceStatusOK {
		t.Fatalf("recovery calls=%d entry=%+v health=%+v err=%v", fetchCalls, entry, health, err)
	}

	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: marketEventBorrowFeesScope, Source: marketEventBorrowFeesSource, Kind: marketEventBorrowFeesObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("fetch outcome observations=%d err=%v", len(observations), err)
	}
	doc, ok, err := authority.GetStateDocument(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
	if err != nil || !ok {
		t.Fatalf("borrow-fee state ok=%v err=%v", ok, err)
	}
	var state marketEventBorrowFeesState
	if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
		t.Fatalf("decode recovered state: %v", err)
	}
	if state.Version != marketEventBorrowFeesStateVersion || state.LastAttempt == nil || state.LastAttempt.Outcome != marketEventBorrowFeeOutcomeSuccess || state.LastAttempt.Failure != nil {
		t.Fatalf("success did not supersede failure: %+v", state)
	}

	afterRestart := newMarketEventCache(func() time.Time { return recoveredAt.Add(time.Minute) })
	if err := afterRestart.UseCoreStore(authority); err != nil {
		t.Fatalf("post-recovery restart UseCoreStore: %v", err)
	}
	_, health, err = afterRestart.loadBorrowFees(context.Background(), recoveredAt.Add(time.Minute))
	if err != nil || fetchCalls != 2 || health.LastFailure != nil || health.Status != rpc.SourceStatusOK {
		t.Fatalf("post-recovery restart calls=%d health=%+v err=%v", fetchCalls, health, err)
	}
}

func TestGammaQualityRankableCombinedSPYSPX(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-10 * time.Minute))

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red for rankable short-gamma fixture", got)
	}
}

func TestGammaQualityClosedSessionCachePredatingLastCompletedSessionBlocksRanking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 3, 11, 55, 0, 0, time.UTC)

	combined := rankableCombinedGammaFixture(now.Add(-25 * time.Hour))

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked for stale closed-session cache: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "" {
		t.Fatalf("bandForGamma = %q, want unranked for stale closed-session cache", got)
	}
}

func TestGammaQualityPartialFanoutIsContextOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))

	spx.CollectionDiagnostics = []rpc.GammaCollectionDiagnostic{
		{Underlying: "SPX", RequestedLegs: 578},
	}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Coverage.RequestedLegs; got != 578 {
		t.Fatalf("requested legs = %d, want 578 summed from collection diagnostics", got)
	}
	if got := spx.Quality.Coverage.FanoutCompletePct; got >= gammaContextFanoutPct {
		t.Fatalf("fanout complete = %.1f%%, want below the %.0f%% bar", got, gammaContextFanoutPct)
	}
	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("rankability = %q, want context_only for a 34.6%% chain: %+v", got, spx.Quality)
	}
}

func rankableCombinedGammaFixture(asOf time.Time) *rpc.GammaZeroComputed {
	return hydrateGammaComputed(combineGammaResults(
		rankableGammaFixture(rpc.GammaZeroScopeSPY, asOf),
		rankableGammaFixture(rpc.GammaZeroScopeSPX, asOf),
	))
}

func rankableGammaFixture(scope string, asOf time.Time) *rpc.GammaZeroComputed {
	label := "SPY"
	spot := 750.0
	if scope == rpc.GammaZeroScopeSPX {
		label = "SPX"
		spot = 7500.0
	}
	return &rpc.GammaZeroComputed{
		Scope:                   scope,
		SpotUnderlying:          spot,
		GammaSign:               "negative",
		GammaTotalAbs:           4_000_000_000,
		GammaTotalAbsConvention: "sign-agnostic",
		TopConcentrationPct:     10,
		TopStrikes: []rpc.StrikeConcentration{{
			Underlying: label,
			Strike:     spot,
			Expiry:     "2026-06-02",
			Right:      "P",
			AbsGEX:     400_000_000,
			OI:         10_000,
		}},
		Expirations:    []string{"2026-06-02", "2026-06-05", "2026-06-19"},
		LegCount:       180,
		PricedLegCount: 200,
		DerivedIVLegs:  10,
		LegDiagnostics: &rpc.GammaLegDiagnostics{Total: rpc.GammaLegDiagnosticCounts{
			PricedLegs:               200,
			OpenInterestObservedLegs: 198,
			OpenInterestLegs:         180,
			GammaPositiveLegs:        200,
			AbsGEXLegs:               180,
		}},
		GammaSign0DTE: "negative",
		LegCount0DTE:  40,
		GammaSign1to7: "negative",
		LegCount1to7:  100,
		GammaSignTerm: "negative",
		LegCountTerm:  40,
		SkewFitQuality: map[string]rpc.SkewFitInfo{
			"20260602": {Points: 100, RSquared: 0.92},
			"20260605": {Points: 100, RSquared: 0.90},
			"20260619": {Points: 100, RSquared: 0.88},
		},
		Params: rpc.GammaZeroParams{
			ExpiryCount:    6,
			StrikeWidthPct: 0.10,
			SweepRangePct:  0.15,
			WorkerCount:    4,
		},
		Source: "test gamma fixture " + label,
		Method: gammaMethodToken,
		AsOf:   asOf,
	}
}

func hygSpyRedSnapshot(at time.Time) *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{
		AsOf: at,
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:     rpc.RegimeStatusOK,
			HYGPrice:   new(78.0),
			HYG50DMA:   new(80.0),
			SPYPrice:   new(757.67),
			SPY52WHigh: new(760.40),
			HYGQuality: &rpc.Quality{AsOf: at, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
		},
	}
}

func hygSpyBlindSnapshot(at time.Time) *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{
		AsOf: at,
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:       rpc.RegimeStatusError,
			ErrorMessage: "HYG or SPY spot missing",
		},
	}
}

func TestRegimeBandHeldWhenInputsUnavailable(t *testing.T) {
	ny := newYorkLocation()
	store := NewStreakStore(t.TempDir())
	measured := time.Date(2026, 8, 4, 2, 24, 0, 0, ny)

	if p := (&Server{}).populateStreaksWithStore(hygSpyRedSnapshot(measured), store)[rpc.RegimeIndicatorHYGSPY]; p.band != "red" || p.held {
		t.Fatalf("live pass: band=%q held=%v, want a computed red", p.band, p.held)
	}
	latchedBefore := store.Latched(StreakKeyHYGSPY)
	sessionsBefore := 0
	if info := store.Get(StreakKeyHYGSPY); info != nil {
		sessionsBefore = info.Sessions
	}

	blind := hygSpyBlindSnapshot(measured.Add(4 * time.Minute))
	policy := (&Server{}).populateStreaksWithStore(blind, store)[rpc.RegimeIndicatorHYGSPY]

	if policy.band != "red" {
		t.Fatalf("band=%q, want the last measured red held", policy.band)
	}
	if !policy.held {
		t.Fatal("band must be marked held, or a consumer cannot tell memory from measurement")
	}
	if !policy.heldAt.Equal(measured) {
		t.Errorf("heldAt=%v, want the time of the last live measurement %v", policy.heldAt, measured)
	}

	if policy.eligibility != nil {
		t.Errorf("held band carried eligibility %+v, want nil — memory must never confirm", policy.eligibility)
	}
	if store.Latched(StreakKeyHYGSPY) != latchedBefore {
		t.Errorf("hold changed the eligibility latch (%v -> %v); memory must not advance confirmation state",
			latchedBefore, store.Latched(StreakKeyHYGSPY))
	}
	if info := store.Get(StreakKeyHYGSPY); info != nil && info.Sessions != sessionsBefore {
		t.Errorf("hold banked a persistence session (%d -> %d); an outage is not evidence of persistence",
			sessionsBefore, info.Sessions)
	}

	annotateRegimeMetadata(blind, map[string]regimeRowPolicy{rpc.RegimeIndicatorHYGSPY: policy})
	got := blind.HYGSPYDivergence.BandReason
	if !strings.Contains(got, "held from the last measured reading") {
		t.Errorf("band_reason=%q, want it to disclose the band is held", got)
	}
	if !strings.Contains(got, measured.UTC().Format("2006-01-02 15:04Z")) {
		t.Errorf("band_reason=%q, want it to date the held reading", got)
	}

	if partial := partialRegimeClusters(blind); !slices.Contains(partial, "credit") {
		t.Errorf("partial clusters=%v, want credit still reported while the band is held", partial)
	}
}

func openRegimeSnapshotTestStore(t *testing.T) *corestore.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open daemon SQLite authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func regimeSnapshotTestNow() time.Time {
	return time.Now().UTC().Add(time.Second)
}

func regimeSnapshotCacheFixture(at time.Time, label string) *rpc.RegimeSnapshotResult {
	snapshot := &rpc.RegimeSnapshotResult{
		AsOf:             at.UTC(),
		Summary:          rpc.RegimeSummary{Label: label, DominantRisks: []string{"credit", "volatility"}},
		VIXTermStructure: rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK},
		VolOfVol:         rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusOK},
		CreditSpreads:    rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK},
		FundingStress:    rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK},
		USDJPY:           rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Symbol: "USD.JPY"},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{Status: "ready", Result: &rpc.GammaZeroComputed{
				Scope: "combined", LegDiagnostics: &rpc.GammaLegDiagnostics{ByUnderlying: map[string]rpc.GammaLegDiagnosticCounts{
					"SPX": {PricedLegs: 10, OpenInterestLegs: 8},
				}},
			}},
		},
		Breadth:        rpc.RegimeBreadth{Status: rpc.RegimeStatusOK},
		WarningDetails: []rpc.RegimeWarning{{Code: "fixture", Scope: "test", Severity: "info", Message: "fixture warning"}},
		SpecDoc:        "https://osauer.dev/canary/docs/internals/regime-dashboard.html",
	}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	return snapshot
}

func TestRegimeProjectionReceiptIdentityAndGapRejection(t *testing.T) {
	tests := []struct {
		name            string
		snapshotRevs    int
		receiptRevision int64
		mutateReceipt   func(regimeSnapshotPublication) regimeSnapshotPublication
		wantErr         string
		wantRevision    int64
	}{
		{name: "first snapshot bootstraps missing receipt", snapshotRevs: 1, wantRevision: 1},
		{name: "exact receipt", snapshotRevs: 3, receiptRevision: 3, wantRevision: 3},
		{name: "one revision crash gap", snapshotRevs: 3, receiptRevision: 2, wantRevision: 3},
		{name: "missing receipt only valid for first snapshot", snapshotRevs: 3, wantErr: "receipt is missing"},
		{name: "gap larger than one", snapshotRevs: 3, receiptRevision: 1, wantErr: "cannot safely recover"},
		{name: "receipt ahead", snapshotRevs: 3, receiptRevision: 4, wantErr: "ahead of snapshot"},
		{
			name:            "same revision different fingerprint",
			snapshotRevs:    3,
			receiptRevision: 3,
			mutateReceipt: func(publication regimeSnapshotPublication) regimeSnapshotPublication {
				publication.Fingerprint.Key = "sha256:wrong-publication"
				return publication
			},
			wantErr: "cannot safely recover",
		},
		{
			name:            "same revision different publication time",
			snapshotRevs:    3,
			receiptRevision: 3,
			mutateReceipt: func(publication regimeSnapshotPublication) regimeSnapshotPublication {
				publication.PublishedAt = publication.PublishedAt.Add(-time.Second)
				return publication
			},
			wantErr: "cannot safely recover",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			snapshot := regimeSnapshotCacheFixture(regimeSnapshotTestNow(), test.name)
			snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
			snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
			cache := projectionRecoveryPersistSnapshot(t, store, snapshot, test.snapshotRevs)
			publication, _, err := cache.publication()
			if err != nil {
				t.Fatalf("read snapshot publication: %v", err)
			}

			server := &Server{
				coreStore:              store,
				rulesRegimeStageLoaded: true,
				logger:                 NewLogger(&bytes.Buffer{}, "error"),
			}
			if test.receiptRevision > 0 {
				receiptPublication := publication
				receiptPublication.Revision = test.receiptRevision
				receiptPublication.PublishedAt = publication.PublishedAt.Add(-time.Duration(publication.Revision-test.receiptRevision) * time.Second)
				projectionRecoverySeedExactProjections(t, server, snapshot, receiptPublication, regimeDecisionEventRecorded)
				if test.mutateReceipt != nil {
					receiptPublication = test.mutateReceipt(receiptPublication)
				}
				if err := server.recordRegimeProjectionReceipt(t.Context(), receiptPublication); err != nil {
					t.Fatalf("seed projection receipt: %v", err)
				}
			}

			err = server.reconcileRegimeSnapshotProjections(t.Context(), cache)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("reconcile error=%v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcile projections: %v", err)
			}
			receipt, ok, err := server.loadRegimeProjectionReceipt(t.Context())
			if err != nil || !ok {
				t.Fatalf("load reconciled receipt: ok=%v err=%v", ok, err)
			}
			if receipt.SnapshotRevision != test.wantRevision ||
				!receipt.SnapshotPublishedAt.Equal(publication.PublishedAt) ||
				receipt.SnapshotFingerprint != publication.Fingerprint {
				t.Fatalf("receipt=%+v, publication=%+v", receipt, publication)
			}
		})
	}
}

func TestRegimeStreakProjectionSurvivesNotDueFreezeAcrossRestart(t *testing.T) {
	ny := newYorkLocation()

	asOf := time.Date(2026, 7, 20, 1, 5, 0, 0, ny)
	frozenRatio, movedRatio := 0.8632345293811753, 0.8633766233766235
	prior := StreakEntry{
		LastBand: "green", SinceDate: "2026-07-15", LastSession: "2026-07-17",
		Sessions: 3, LastValue: frozenRatio,
	}
	previous := regimeSnapshotPublication{
		Revision: 1, PublishedAt: asOf.Add(-time.Hour).UTC(),
		Fingerprint: rpc.Fingerprint{Version: "test", Key: "prior-publication"},
	}

	store := openRegimeSnapshotTestStore(t)
	snapshot := regimeSnapshotCacheFixture(asOf.UTC(), "not-due-freeze")
	snapshot.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusStale, Ratio: &movedRatio,
		VIX3MCrossCheck: rpc.VIX3MCrossCheckAgree,
		VIXQuality:      &rpc.Quality{AsOf: asOf, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm},
		VIX3MQuality:    &rpc.Quality{AsOf: asOf, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm},
	}
	streaks := projectionRecoverySeedStreakStore(t, store, previous, map[string]StreakEntry{StreakKeyVIXTerm: prior})

	evaluated := streaks.cloneForRegimeEvaluation()
	annotateRegimeMetadata(snapshot, (&Server{}).populateStreaksWithStore(snapshot, evaluated))
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 2, PublishedAt: asOf.UTC(), Fingerprint: snapshot.Fingerprint}
	plan := regimeProjectionPlan{publication: publication, previous: &previous}

	if class := snapshot.VIXTermStructure.Freshness; class == nil || class.Class != rpc.RegimeFreshnessNotDue {
		t.Fatalf("vix_term freshness=%+v, want not_due", class)
	}
	if snapshot.VIXTermStructure.Streak == nil {
		t.Fatal("vix_term served no streak; the frozen row cannot be reconciled")
	}
	if err := streaks.commitRegimeEvaluation(t.Context(), evaluated, plan); err != nil {
		t.Fatalf("commit regime evaluation: %v", err)
	}

	restarted := NewStreakStore("")
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileRegimeProjection(t.Context(), snapshot, plan); err != nil {
		t.Fatalf("startup reconcile after a not_due freeze: %v", err)
	}
	restarted.mu.Lock()
	got := restarted.entries[StreakKeyVIXTerm]
	restarted.mu.Unlock()
	if !reflect.DeepEqual(got, prior) {
		t.Fatalf("frozen entry=%+v, want unchanged %+v", got, prior)
	}

	fresh := regimeSnapshotCacheFixture(asOf.UTC(), "fresh-row")
	fresh.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, Ratio: &movedRatio,
		Streak: &rpc.StreakInfo{Band: prior.LastBand, Sessions: prior.Sessions, Since: prior.SinceDate},
		Band:   prior.LastBand, Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh},
	}
	fresh.Fingerprint = rpc.BuildRegimeFingerprint(fresh)
	freshPublication := regimeSnapshotPublication{Revision: 2, PublishedAt: asOf.UTC(), Fingerprint: fresh.Fingerprint}
	stale := projectionRecoverySeedStreakStore(t, openRegimeSnapshotTestStore(t), freshPublication,
		map[string]StreakEntry{StreakKeyVIXTerm: prior})
	err := stale.reconcileRegimeProjection(t.Context(), fresh, regimeProjectionPlan{publication: freshPublication})
	if err == nil || !strings.Contains(err.Error(), "content mismatch at snapshot revision 2") {
		t.Fatalf("fresh row with a stale stored value err=%v, want a content mismatch", err)
	}
}

func projectionRecoveryPersistSnapshot(t *testing.T, store *corestore.Store, snapshot *rpc.RegimeSnapshotResult, revisions int) *regimeSnapshotCache {
	t.Helper()
	raw, _, err := encodeRegimeSnapshotDocument(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	var saved corestore.StateDocument
	for revision := range revisions {
		saved, err = store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind,
			ExpectedRevision: int64(revision), JSON: raw,
		})
		if err != nil {
			t.Fatalf("persist snapshot revision %d: %v", revision+1, err)
		}
	}
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: time.Minute,
		Now: func() time.Time { return saved.UpdatedAt.Add(time.Second) },
	})
	if err != nil {
		t.Fatalf("load snapshot cache: %v", err)
	}
	return cache
}

func projectionRecoverySeedStreakStore(t *testing.T, store *corestore.Store, publication regimeSnapshotPublication, entries map[string]StreakEntry) *StreakStore {
	t.Helper()
	streaks := NewStreakStore("")
	if err := streaks.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	streaks.mu.Lock()
	streaks.entries = cloneStreakEntries(entries)
	streaks.loaded = true
	err := streaks.saveLockedContextPublication(t.Context(), publication)
	streaks.mu.Unlock()
	if err != nil {
		t.Fatalf("seed streak state: %v", err)
	}
	return streaks
}

func projectionRecoverySeedExactProjections(t *testing.T, server *Server, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication, decisionEvent string) {
	t.Helper()
	streaks := NewStreakStore("")
	if err := streaks.UseCoreStore(server.coreStore); err != nil {
		t.Fatal(err)
	}
	streaks.mu.Lock()
	streaks.loaded = true
	if err := streaks.saveLockedContextPublication(t.Context(), publication); err != nil {
		streaks.mu.Unlock()
		t.Fatalf("seed exact streak projection: %v", err)
	}
	streaks.mu.Unlock()
	server.streaks = streaks

	projectionRecoverySeedRuleProjection(t, server.coreStore, snapshot, publication)
	server.regimeDecisions = &regimeDecisionJournal{core: server.coreStore}
	if decisionEvent == regimeDecisionEventRecorded {
		if err := server.regimeDecisions.appendPublicationContext(t.Context(), publication.PublishedAt, snapshot, publication); err != nil {
			t.Fatalf("seed exact decision event: %v", err)
		}
	}
	if err := server.persistRegimeDecisionProjectionState(t.Context(), corestore.StateDocument{}, false, publication, decisionEvent); err != nil {
		t.Fatalf("seed exact decision projection marker: %v", err)
	}
}

func projectionRecoverySeedRuleProjection(t *testing.T, store *corestore.Store, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) {
	t.Helper()
	ruleBase := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	if publication.Revision > 1 {
		ruleBase = rulesRegimeStageState{
			Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketCalm,
			Stage: rpc.LifecycleQuiet, AsOf: publication.PublishedAt.Add(-time.Second),
			publication: regimeSnapshotPublication{
				Revision: publication.Revision - 1, PublishedAt: publication.PublishedAt.Add(-time.Second),
				Fingerprint: publication.Fingerprint,
			},
		}
	}
	ruleState := projectedRulesRegimeStageState(ruleBase, snapshot, publication)
	raw, err := json.Marshal(ruleState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage, JSON: raw,
	}); err != nil {
		t.Fatalf("seed exact rule-stage projection: %v", err)
	}
}

func TestRegimeSnapshotDataQualityCombinesGammaAndRegime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status: rpc.RegimeStatusOK,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					AsOf:    now,
					Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "spx_unavailable:354",
					}},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeSnapshotDataQuality(res)
	if len(got) != 2 {
		t.Fatalf("regimeSnapshotDataQuality len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Surface != "gamma" || got[0].Summary != "degraded: SPX excluded" {
		t.Fatalf("first quality = %+v, want gamma degraded", got[0])
	}
	if got[1].Surface != "regime" || got[1].Summary != "stale: vol" {
		t.Fatalf("second quality = %+v, want regime stale vol", got[1])
	}
}

type stressAuthorityTestReader struct {
	at            time.Time
	accountResult *rpc.AccountResult
	positionsBook *rpc.PositionsResult
	regimeResult  *rpc.RegimeSnapshotResult
	eventsResult  *rpc.MarketEventsResult
	eventSymbols  []string
}

func (r *stressAuthorityTestReader) ready() bool { return true }

func (r *stressAuthorityTestReader) account(context.Context) (*rpc.AccountResult, error) {
	return r.accountResult, nil
}

func (r *stressAuthorityTestReader) positions(context.Context) (*rpc.PositionsResult, error) {
	return r.positionsBook, nil
}

func (r *stressAuthorityTestReader) regime(context.Context) (*rpc.RegimeSnapshotResult, error) {
	return r.regimeResult, nil
}

func (r *stressAuthorityTestReader) marketEvents(_ context.Context, symbols []string) (*rpc.MarketEventsResult, error) {
	r.eventSymbols = slices.Clone(symbols)
	return r.eventsResult, nil
}

func (r *stressAuthorityTestReader) now() time.Time { return r.at }

func TestStressEvaluationTickRejectsRestampedCachedAccountFallback(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	account := stressAuthorityTestAccount(now)

	account.GrossPositionValue = 200_000

	account.AsOf = accountResultAuthorityAsOf(accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceCachedFallback,
		AsOf:       now,
	}, now)
	positions := &rpc.PositionsResult{
		AsOf:      positionsResultAuthorityAsOf(stressAuthorityTestScope(), stressAuthorityCurrentPortfolioHealth(now), now),
		Stocks:    []rpc.PositionView{},
		Options:   []rpc.PositionView{},
		Portfolio: &rpc.PositionsPortfolio{},
	}
	reader := &stressAuthorityTestReader{
		at: now, accountResult: account, positionsBook: positions,
		regimeResult: stressAuthorityHealthyRegime(now),
		eventsResult: stressAuthorityHealthyMarketEvents(now),
	}

	line := runStressAuthorityTick(t, reader)
	if !line.SourceAsOf.Account.IsZero() {
		t.Fatalf("cached account fallback source_as_of = %s, want unavailable", line.SourceAsOf.Account)
	}
	if line.InputHealth != "degraded" || line.Action != "confirm_inputs" || line.PortfolioFit != "unknown" {
		t.Fatalf("cached account fallback decision = %s/%s fit=%s, want degraded/confirm_inputs/unknown", line.InputHealth, line.Action, line.PortfolioFit)
	}
	if line.PortfolioAlertRelevant == nil || !*line.PortfolioAlertRelevant {
		t.Fatalf("cached account fallback was treated as a clean irrelevant book: %+v", line.PortfolioAlertRelevant)
	}
}

func runStressAuthorityTick(t *testing.T, reader *stressAuthorityTestReader) stressDecisionLine {
	t.Helper()
	server := &Server{
		logger:                              NewLogger(&bytes.Buffer{}, "error"),
		stressDecisions:                     &stressDecisionJournal{path: filepath.Join(t.TempDir(), "canary-decisions.jsonl")},
		stressEvaluationSourceReaderForTest: reader,
	}
	if !server.stressEvaluationTick(t.Context()) {
		t.Fatal("stress evaluation tick did not publish")
	}
	raw, err := os.ReadFile(server.stressDecisions.path)
	if err != nil {
		t.Fatalf("read stress decision: %v", err)
	}
	var line stressDecisionLine
	if err := json.Unmarshal(bytes.TrimSpace(raw), &line); err != nil {
		t.Fatalf("decode stress decision: %v", err)
	}
	return line
}

func stressAuthorityTestScope() brokerStateScope {
	return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
}

func stressAuthorityCurrentPortfolioHealth(now time.Time) ibkrlib.PortfolioStreamHealth {
	return ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-time.Minute)}
}

func stressAuthorityTestAccount(now time.Time) *rpc.AccountResult {
	dailyPnL := 0.0
	return &rpc.AccountResult{
		AccountID: "DU123", BaseCurrency: "USD", NetLiquidation: 100_000,
		DailyPnL: &dailyPnL, AsOf: now,
	}
}

func stressAuthorityHealthyRegime(now time.Time) *rpc.RegimeSnapshotResult {
	green := rpc.RegimeIndicatorMeta{Band: "green"}
	return &rpc.RegimeSnapshotResult{
		AsOf:             now,
		Composite:        rpc.RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: rpc.RegimeVIXTerm{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		VolOfVol:         rpc.RegimeVolOfVol{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		CreditSpreads:    rpc.RegimeCreditSpreads{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		FundingStress:    rpc.RegimeFundingStress{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		USDJPY:           rpc.RegimeUSDJPY{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: &rpc.GammaZeroComputed{
				Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
			}},
		},
		Breadth: rpc.RegimeBreadth{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
	}
}

func stressAuthorityHealthyMarketEvents(now time.Time, symbols ...string) *rpc.MarketEventsResult {
	return &rpc.MarketEventsResult{
		Kind: rpc.MarketEventsKind, SchemaVersion: rpc.MarketEventsSchemaVersion,
		AsOf: now, Symbols: slices.Clone(symbols), BySymbol: map[string][]rpc.MarketEventFlag{},
		SourceHealth: []rpc.SourceHealth{
			{Source: "reg_sho_threshold", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 3600},
			{Source: "trading_halts", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 3600},
		},
	}
}
