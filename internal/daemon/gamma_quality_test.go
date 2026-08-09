package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func hasGammaWarning(details []rpc.GammaWarningDetail, code string) bool {
	for _, detail := range details {
		if detail.Code == code {
			return true
		}
	}
	return false
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

func TestGammaQualityClosedSessionCacheFromLastCompletedSessionRemainsRankable(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 6, 2, 19, 20, 0, 0, time.UTC) // 15:20 ET, options RTH
	now := time.Date(2026, 6, 3, 11, 55, 0, 0, time.UTC)  // 07:55 ET, options closed
	if cls := gammaClassifySession(asOf); cls != rpc.SessionRTH {
		t.Fatalf("asOf fixture should be RTH, got %s", cls)
	}
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("now fixture should be closed, got %s", cls)
	}
	combined := rankableCombinedGammaFixture(asOf)

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable for healthy closed-session cache: %+v", got, combined.Quality)
	}
	if got := combined.Quality.Freshness; got != "closed_session_cache" {
		t.Fatalf("freshness = %q, want closed_session_cache: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red for rankable cached short-gamma fixture", got)
	}
}

func TestGammaQualityClosedSessionCachePredatingLastCompletedSessionBlocksRanking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 3, 11, 55, 0, 0, time.UTC) // 07:55 ET, options closed
	// 25h back is 06:55 ET the prior day — before that session's open, so the
	// cache carries the session before it and must not rank.
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

func TestGammaQualityWeekendCacheFromFridayCloseRemainsRankable(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 6, 5, 20, 10, 0, 0, time.UTC) // Fri 16:10 ET, options RTH
	now := time.Date(2026, 6, 7, 18, 0, 0, 0, time.UTC)   // Sun 14:00 ET, ~46h later
	if cls := gammaClassifySession(asOf); cls != rpc.SessionRTH {
		t.Fatalf("asOf fixture should be RTH, got %s", cls)
	}
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("now fixture should be closed, got %s", cls)
	}
	combined := rankableCombinedGammaFixture(asOf)

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable for Friday-close cache on the weekend: %+v", got, combined.Quality)
	}
	if got := combined.Quality.Freshness; got != "closed_session_cache" {
		t.Fatalf("freshness = %q, want closed_session_cache: %+v", got, combined.Quality)
	}
	if got := combined.Quality.MaxAgeSeconds; got <= int64(24*3600) {
		t.Fatalf("max_age_seconds = %d, want a horizon past 24h reaching the next session open", got)
	}
}

func TestGammaQualitySingleScopesCanRankIndependently(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{"spy", rpc.GammaZeroScopeSPY},
		{"spx", rpc.GammaZeroScopeSPX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := rankableGammaFixture(tc.scope, now.Add(-5*time.Minute))
			annotateGammaQuality(c, now)
			if got := c.Quality.Rankability; got != rpc.GammaRankabilityRankable {
				t.Fatalf("rankability = %q, want rankable: %+v", got, c.Quality)
			}
		})
	}
}

func TestGammaQualityDelayedClockAlignedResultCanRank(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
	result := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-20*time.Minute))
	result.DataType = rpc.MarketDataDelayed

	annotateGammaQuality(result, now)
	if got := result.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("delayed aligned rankability = %q, want rankable: %+v", got, result.Quality)
	}
	assertGammaQualityGate(t, result.Quality, "market_data_type", rpc.GammaQualityGatePass)
}

func TestGammaQualityDelayedFrozenResultBlocks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
	result := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-20*time.Minute))
	result.DataType = rpc.MarketDataDelayedFrozen

	annotateGammaQuality(result, now)
	if got := result.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("delayed-frozen rankability = %q, want blocked: %+v", got, result.Quality)
	}
	assertGammaQualityGate(t, result.Quality, "market_data_type", rpc.GammaQualityGateBlock)
}

func assertGammaQualityGate(t *testing.T, q *rpc.GammaSignalQuality, name, status string) {
	t.Helper()
	if q == nil {
		t.Fatal("gamma quality missing")
	}
	for _, gate := range q.Gates {
		if gate.Name == name {
			if gate.Status != status {
				t.Fatalf("gate %s status = %q, want %q: %+v", name, gate.Status, status, gate)
			}
			return
		}
	}
	t.Fatalf("gate %s missing: %+v", name, q.Gates)
}

func TestGammaQualitySPXCanonicalRanksWithSPYUnavailable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.Warnings = []string{"spy_unavailable:throttled"}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable canonical SPX despite SPY unavailability: %+v", got, spx.Quality)
	}
	for _, blocker := range append(spx.Quality.Blockers, spx.Quality.Context...) {
		if strings.Contains(blocker, "spy_coverage") {
			t.Fatalf("SPY unavailability should not downgrade canonical SPX quality: %+v", spx.Quality)
		}
	}
	var sawPass bool
	for _, gate := range spx.Quality.Gates {
		if gate.Name == "spy_coverage" && gate.Status == rpc.GammaQualityGatePass {
			sawPass = true
		}
	}
	if !sawPass {
		t.Fatalf("quality gates did not record SPY unavailability as non-blocking pass: %+v", spx.Quality.Gates)
	}
}

func TestGammaQualitySPXRanksWithout0DTEWhenNearAndTermCovered(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.LegCount0DTE = 0
	spx.GammaSign0DTE = "no_data"
	spx.Warnings = []string{"0dte_no_legs"}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable without 0DTE when near+term buckets are covered: %+v", got, spx.Quality)
	}
	if spx.Quality.Coverage.Has0DTE {
		t.Fatalf("coverage should still disclose missing 0DTE: %+v", spx.Quality.Coverage)
	}
	for _, ctx := range spx.Quality.Context {
		if strings.Contains(ctx, "horizon_0dte") {
			t.Fatalf("missing 0DTE alone should not make SPX gamma context-only: %+v", spx.Quality)
		}
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: spx}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red for rankable short-gamma SPX without 0DTE", got)
	}
}

func TestGammaQualityMissing0DTERemainsContextWhenTermMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.LegCount0DTE = 0
	spx.GammaSign0DTE = "no_data"
	spx.LegCountTerm = 0
	spx.GammaSignTerm = "no_data"

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("rankability = %q, want context_only when 0DTE and term buckets are missing: %+v", got, spx.Quality)
	}
	if !strings.Contains(spx.Quality.RankabilityReason, "horizon_0dte") {
		t.Fatalf("rankability reason = %q, want 0DTE context reason", spx.Quality.RankabilityReason)
	}
}

func TestGammaQualitySPYLivePartialOIStillRankable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.PricedLegCount = 784
	spy.LegCount = 159
	spy.DerivedIVLegs = 6
	spy.TopConcentrationPct = 21.5
	spy.LegCount0DTE = 26
	spy.LegCount1to7 = 71
	spy.LegCountTerm = 62
	spy.LegDiagnostics.Total.PricedLegs = 784
	spy.LegDiagnostics.Total.OpenInterestObservedLegs = 454
	spy.LegDiagnostics.Total.OILiveObservedLegs = 453
	spy.LegDiagnostics.Total.OICarriedForwardLegs = 1
	spy.LegDiagnostics.Total.OpenInterestLegs = 159
	spy.LegDiagnostics.Total.AbsGEXLegs = 159
	spy.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 150, RSquared: 0.33},
		"20260603": {Points: 151, RSquared: 0.61},
		"20260604": {Points: 151, RSquared: 0.75},
		"20260605": {Points: 150, RSquared: 0.69},
		"20260717": {Points: 152, RSquared: 0.98},
		"20260918": {Points: 30, RSquared: 0.99},
	}
	spy.Warnings = []string{"oi_missing", "strike_budget_capped", "no_crossing_in_window"}

	annotateGammaQuality(spy, now)

	if got := spy.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable SPY live partial-OI signal: %+v", got, spy.Quality)
	}
	if spy.Quality.Coverage.OIObservedPct < gammaMinSPYOIObservedPct {
		t.Fatalf("test fixture should cover SPY OI threshold: %+v", spy.Quality.Coverage)
	}
}

// TestGammaQualityPartialFanoutIsContextOnly pins the fan-out completeness
// gate against the failure it was written for: on 2026-07-24 an SPX slice
// landed 332 of 960 requested legs and still published a confident
// zero-gamma level, because every other gate is either an absolute floor or
// a ratio whose denominator is the legs that survived.
func TestGammaQualityPartialFanoutIsContextOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	// The fixture prices 200 legs and clears every other gate; 578 requested
	// puts it at the incident's 34.6% completeness without disturbing the
	// OI/IV ratios, which divide by priced legs.
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

// TestGammaQualityCompleteFanoutStaysRankable guards the other direction: the
// gate must not demote healthy chains, which land 86-99% of requested legs.
func TestGammaQualityCompleteFanoutStaysRankable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.CollectionDiagnostics = []rpc.GammaCollectionDiagnostic{
		{Underlying: "SPX", RequestedLegs: 218},
	}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Coverage.FanoutCompletePct; got < gammaContextFanoutPct {
		t.Fatalf("fanout complete = %.1f%%, want at or above the %.0f%% bar", got, gammaContextFanoutPct)
	}
	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable for a 91.7%% chain: %+v", got, spx.Quality)
	}
}

func TestGammaQualitySPXStillRequiresHighOIObservedCoverage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.PricedLegCount = 784
	spx.LegCount = 159
	spx.LegDiagnostics.Total.PricedLegs = 784
	spx.LegDiagnostics.Total.OpenInterestObservedLegs = 454
	spx.LegDiagnostics.Total.OILiveObservedLegs = 453
	spx.LegDiagnostics.Total.OICarriedForwardLegs = 1
	spx.LegDiagnostics.Total.OpenInterestLegs = 159
	spx.LegDiagnostics.Total.AbsGEXLegs = 159

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked SPX under strict OI gate: %+v", got, spx.Quality)
	}
}

func TestGammaQualityStaleActiveCacheBlocksRanking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-2 * time.Hour))

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "" {
		t.Fatalf("bandForGamma = %q, want unranked for stale gamma", got)
	}
}

func TestGammaQualityColdEnvelopeIsDataQualityPartial(t *testing.T) {
	t.Parallel()
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status:         rpc.GammaZeroStatusCold,
		ColdReasonCode: "session_closed_no_cache",
		ColdReason:     "no gamma cache is available",
	})
	if !ok {
		t.Fatal("gammaStatusQuality returned ok=false for cold envelope")
	}
	if got.Status != "partial" || !strings.Contains(got.Summary, "no gamma cache") {
		t.Fatalf("quality = %+v, want partial cold-cache summary", got)
	}
}

func TestGammaQualityForcedRefreshKeepsCurrentCacheRankable(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	current := rankableCombinedGammaFixture(now.Add(-5 * time.Minute))
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(current, rpc.GammaZeroScopeCombined, now)},
	}
	block := make(chan struct{})
	job := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, computeETA, func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		<-block
		return rankableCombinedGammaFixture(now), nil
	})
	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	close(block)
	<-job.done

	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		t.Fatalf("snapshot = %+v, want served current cache while force runs", env)
	}
	if got := env.Result.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable served cache: %+v", got, env.Result.Quality)
	}
}

func TestGammaQualityFailedRefreshBlocksServedCache(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	current := rankableCombinedGammaFixture(now.Add(-70 * time.Minute))
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(current, rpc.GammaZeroScopeCombined, now)},
	}
	computeErr := func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return nil, errors.New("farm timeout")
	}
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, computeETA, computeErr)
	refresh := c.slots[rpc.GammaZeroScopeCombined].refresh
	if refresh == nil {
		t.Fatal("expected soft-TTL refresh to start")
	}
	<-refresh.done
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now.Add(time.Second), computeETA, computeErr)

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now.Add(time.Second) })
	if env.Result == nil || env.Result.Quality == nil {
		t.Fatalf("snapshot missing quality: %+v", env)
	}
	if got := env.Result.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked after failed refresh: %+v", got, env.Result.Quality)
	}
	if !hasGammaWarning(env.Result.WarningDetails, "refresh_failed:timeout") {
		t.Fatalf("warning_details missing refresh failure: %+v", env.Result.WarningDetails)
	}
}

func TestGammaQualitySPXCanonicalRanksWhenSPYDegrades(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.LegCount = 2
	spy.GammaTotalAbs = 10_000
	spy.TopConcentrationPct = 99
	spy.LegCount0DTE = 0
	spy.GammaSign0DTE = "no_data"
	spy.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 100, RSquared: 0.20},
	}
	spy.LegDiagnostics = &rpc.GammaLegDiagnostics{Total: rpc.GammaLegDiagnosticCounts{
		PricedLegs:               200,
		OpenInterestObservedLegs: 2,
		OpenInterestLegs:         2,
		GammaPositiveLegs:        200,
		AbsGEXLegs:               2,
	}}
	spy.Warnings = []string{"oi_missing"}
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("combined rankability = %q, want rankable SPX-canonical signal: %+v", got, combined.Quality)
	}
	for _, gate := range combined.Quality.Gates {
		if gate.Name == "spy_coverage" && gate.Status != rpc.GammaQualityGatePass {
			t.Fatalf("degraded SPY should be non-blocking when SPX is rankable: %+v", combined.Quality.Gates)
		}
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want SPX-ranked red band despite degraded SPY", got)
	}
}

func TestGammaQualitySPYProxyWithSPXUnavailableIsContextOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.Warnings = []string{"spx_unavailable:timeout"}

	annotateGammaQuality(spy, now)

	if got := spy.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("rankability = %q, want context_only SPY proxy when SPX is unavailable: %+v", got, spy.Quality)
	}
	if len(spy.Quality.Blockers) != 0 {
		t.Fatalf("SPY proxy should be degraded but not blocked: %+v", spy.Quality)
	}
	var sawContext bool
	for _, gate := range spy.Quality.Gates {
		if gate.Name == "spx_coverage" && gate.Status == rpc.GammaQualityGateContext {
			sawContext = true
		}
	}
	if !sawContext {
		t.Fatalf("SPY proxy should carry SPX-unavailable context gate: %+v", spy.Quality.Gates)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: spy}}
	if got := bandForGamma(row); got != "" {
		t.Fatalf("bandForGamma = %q, want unranked for SPY proxy context-only gamma", got)
	}
}

func TestGammaQualitySPXOIDegradationBlocksCombined(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.LegDiagnostics.Total.OpenInterestObservedLegs = 180
	spx.Warnings = []string{"oi_missing"}
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("combined rankability = %q, want blocked: %+v", got, combined.Quality)
	}
}

func TestGammaQualityCombinedNamesSPXModelSourceBlocker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.DerivedIVLegs = spx.PricedLegCount
	spx.DerivedPrevCloseLegs = spx.PricedLegCount
	spx.ModelTickLegs = 0
	spx.Warnings = []string{"all_iv_derived"}
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("combined rankability = %q, want blocked: %+v", got, combined.Quality)
	}
	if !strings.Contains(combined.Quality.RankabilityReason, "SPX model source blocked") ||
		!strings.Contains(combined.Quality.RankabilityReason, "100.0%") {
		t.Fatalf("combined rankability reason = %q, want SPX model-source blocker", combined.Quality.RankabilityReason)
	}
}

func TestGammaQualityCoverageReportsLiveAndCarriedOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.LegDiagnostics.Total.OpenInterestObservedLegs = 198
	spy.LegDiagnostics.Total.OILiveObservedLegs = 120
	spy.LegDiagnostics.Total.OICarriedForwardLegs = 78

	annotateGammaQuality(spy, now)
	cov := spy.Quality.Coverage
	if cov.OIObservedLegs != 198 || cov.OILiveObservedLegs != 120 || cov.OICarriedForwardLegs != 78 {
		t.Fatalf("coverage split = %+v", cov)
	}
	if cov.OILiveObservedPct != 60 || cov.OICarriedForwardPct != 39 {
		t.Fatalf("coverage percentages = live %.1f carried %.1f, want 60/39", cov.OILiveObservedPct, cov.OICarriedForwardPct)
	}
}

func TestRegimeCompositeDoesNotRankContextOnlyGamma(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-2 * time.Hour))
	annotateGammaQuality(combined, now)
	res := &rpc.RegimeSnapshotResult{
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: combined,
			},
		},
	}

	got := buildRegimeComposite(res)
	if got.ClusterRedCount != 0 || got.ClusterRankedCount != 0 {
		t.Fatalf("composite = %+v, want stale gamma unranked and no red vote", got)
	}
}

func TestGammaQualitySubPreferredSkewRanksWithDisclosure(t *testing.T) {
	// Morning regression: 2026-06-11 printed sub-preferred medians
	// (0.60-0.68) all morning on constant, healthy leg coverage and the
	// dashboard showed dealer gamma unranked until midday. A median in
	// [block, preferred) must rank with a disclosing pass instead.
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 134, RSquared: 0.50},
		"20260605": {Points: 148, RSquared: 0.55},
		"20260619": {Points: 148, RSquared: 0.62},
	}
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 160, RSquared: 0.55},
		"20260605": {Points: 160, RSquared: 0.62},
		"20260619": {Points: 160, RSquared: 0.68},
	}
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("combined rankability = %q, want rankable with sub-preferred skew fits: %+v", got, combined.Quality)
	}
	for _, gate := range combined.Quality.Gates {
		if gate.Name == "skew_fit_quality" {
			t.Fatalf("combined node must not gate on pooled skew values: %+v", combined.Quality.Gates)
		}
	}
	// Pooled medians stay visible as disclosed coverage diagnostics.
	if got := combined.Quality.Coverage.MedianSkewRSquared; got < 0.58 || got > 0.59 {
		t.Fatalf("combined coverage median = %.3f, want pooled ~0.585", got)
	}
	spxQ := combined.PerIndex["SPX"].Quality
	var sawDisclosure bool
	for _, gate := range spxQ.Gates {
		if gate.Name == "skew_fit_quality" {
			if gate.Status != rpc.GammaQualityGatePass || !strings.Contains(gate.Reason, "below preferred") {
				t.Fatalf("SPX skew gate = %+v, want disclosing pass", gate)
			}
			sawDisclosure = true
		}
	}
	if !sawDisclosure {
		t.Fatalf("SPX slice missing skew_fit_quality gate: %+v", spxQ.Gates)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red ranked band with sub-preferred skew fits", got)
	}
	res := &rpc.RegimeSnapshotResult{GammaZero: rpc.RegimeGammaZero{
		Status:   rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined},
	}}
	annotateRegimeMetadata(res, nil)
	if got := buildRegimeComposite(res); got.ClusterRankedCount == 0 || got.ClusterRedCount == 0 {
		t.Fatalf("composite = %+v, want gamma ranked as a red cluster vote", got)
	}
}

func TestGammaQualitySkewMedianBelowBlockBlocks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 147, RSquared: 0.05},
		"20260605": {Points: 154, RSquared: 0.22},
		"20260619": {Points: 160, RSquared: 0.71},
	}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked below the skew block bar: %+v", got, spx.Quality)
	}
	if !strings.Contains(spx.Quality.RankabilityReason, "median skew-fit R2") {
		t.Fatalf("rankability reason = %q, want skew block reason", spx.Quality.RankabilityReason)
	}
}

func TestGammaCombinedBandCountsSubPreferredSPYVote(t *testing.T) {
	// A SPY slice in the sub-preferred skew window is now rankable, so
	// it votes in the combined band weighting. Disagreeing sub-bands
	// where the red (SPX) weight is under half resolve to yellow —
	// previously SPY was context_only and SPX alone made the band red.
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.GammaSign = "positive"
	spy.GammaTotalAbs = 10_000_000_000
	spy.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 134, RSquared: 0.55},
		"20260605": {Points: 148, RSquared: 0.60},
		"20260619": {Points: 148, RSquared: 0.65},
	}
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)

	if got := combined.PerIndex["SPY"].Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("SPY rankability = %q, want rankable in sub-preferred skew window: %+v", got, combined.PerIndex["SPY"].Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "yellow" {
		t.Fatalf("bandForGamma = %q, want yellow when dominant-weight SPY long-gamma vote disagrees with SPX", got)
	}
}

func TestGammaCombinedSkipsPooledDerivedIVAndConcentrationGates(t *testing.T) {
	// Same structural contradiction the pooled skew gate had: the
	// combined node's DerivedIVPct pools leg counts across both chains
	// and its TopConcentrationPct is a cross-book ratio, so a present-
	// but-degraded SPY could drag either across the context/block bars
	// while the canonical SPX slice is clean. The combined node must
	// not gate on pooled model numbers; each slice gates its own and
	// the SPX verdict propagates through spx_coverage.
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.DerivedIVLegs = spy.PricedLegCount // SPY slice: 100% derived IV (block on its own slice)
	spy.GammaTotalAbs = 8_000_000_000
	spy.TopConcentrationPct = 95
	spy.TopStrikes = []rpc.StrikeConcentration{{
		Underlying: "SPY",
		Strike:     750,
		Expiry:     "2026-06-02",
		Right:      "P",
		AbsGEX:     7_600_000_000,
		OI:         10_000,
	}}
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)

	// Pooled numbers would cross the bars if gated: derived-IV share
	// (200+10)/400 = 52.5% ≥ 40% context, concentration 7.6/12 = 63.3%
	// ≥ 50% context. They stay visible as coverage diagnostics only.
	if got := combined.Quality.Coverage.DerivedIVPct; got < 52 || got > 53 {
		t.Fatalf("pooled DerivedIVPct = %.1f, want ~52.5 visible in coverage", got)
	}
	if got := combined.Quality.Coverage.TopConcentrationPct; got < 63 || got > 64 {
		t.Fatalf("pooled TopConcentrationPct = %.1f, want ~63.3 visible in coverage", got)
	}
	for _, gate := range combined.Quality.Gates {
		if gate.Name == "derived_iv_share" || gate.Name == "top_strike_concentration" {
			t.Fatalf("combined node must not gate on pooled model values: %+v", combined.Quality.Gates)
		}
	}
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("combined rankability = %q, want rankable with clean SPX and degraded SPY: %+v", got, combined.Quality)
	}
	spyQ := combined.PerIndex["SPY"].Quality
	if spyQ.Rankability != rpc.GammaRankabilityBlocked {
		t.Fatalf("SPY slice rankability = %q, want blocked on its own model gates: %+v", spyQ.Rankability, spyQ)
	}
	if !gammaQualityHasBlocker(spyQ, "derived_iv_share") || !gammaQualityHasBlocker(spyQ, "top_strike_concentration") {
		t.Fatalf("SPY slice missing its own model blockers: %+v", spyQ.Blockers)
	}
	for _, gate := range combined.Quality.Gates {
		if gate.Name == "spy_coverage" && gate.Status != rpc.GammaQualityGatePass {
			t.Fatalf("degraded SPY must stay non-blocking when SPX is rankable: %+v", gate)
		}
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red ranked SPX-canonical band", got)
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
