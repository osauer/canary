package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaCadenceFixture builds the morning shape: the served gamma result is the
// last completed options session's, and the current session's first compute is
// or is not in flight behind it.
func gammaCadenceFixture(t *testing.T, now time.Time, refreshing bool, gates []rpc.GammaQualityGate, freshness string) *rpc.RegimeSnapshotResult {
	t.Helper()
	ny := newYorkLocation()
	r := mkAllGreenRegime()
	r.AsOf = now
	r.GammaZero.Status = rpc.RegimeStatusStale
	r.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status:     rpc.GammaZeroStatusReady,
		Refreshing: refreshing,
		Result: &rpc.GammaZeroComputed{
			AsOf: time.Date(2026, 7, 17, 15, 0, 0, 0, ny),
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityBlocked,
				RankabilityReason: "freshness: computed for session 2026-07-17; current session is 2026-07-20",
				Freshness:         freshness,
				Gates:             gates,
			},
		},
	}
	return r
}

func gammaSessionMismatchGates() []rpc.GammaQualityGate {
	return []rpc.GammaQualityGate{{
		Name: rpc.GammaQualityGateFreshness, Status: rpc.GammaQualityGateBlock,
		Reason: "computed for session 2026-07-17; current session is 2026-07-20",
	}}
}

// TestGammaMorningGraceIsTypedAndBounded is half the symptom-2 regression: the
// cadence class. A current-session compute in flight inside the window anchored
// to the options open is pending; without the typed in-flight marker, or past
// the window, it is overdue, so a hung compute still surfaces.
func TestGammaMorningGraceIsTypedAndBounded(t *testing.T) {
	t.Parallel()
	ny := newYorkLocation()
	open := time.Date(2026, 7, 20, 9, 30, 0, 0, ny)
	for _, tc := range []struct {
		name       string
		now        time.Time
		refreshing bool
		want       string
	}{
		{name: "compute in flight just after the open", now: open.Add(5 * time.Minute), refreshing: true, want: rpc.RegimeFreshnessPending},
		{name: "compute in flight late in the window", now: open.Add(29 * time.Minute), refreshing: true, want: rpc.RegimeFreshnessPending},
		{name: "no compute in flight", now: open.Add(5 * time.Minute), refreshing: false, want: rpc.RegimeFreshnessOverdue},
		{name: "past the bounded window", now: open.Add(31 * time.Minute), refreshing: true, want: rpc.RegimeFreshnessOverdue},
		{name: "before the open is not_due", now: time.Date(2026, 7, 20, 8, 0, 0, 0, ny), refreshing: false, want: rpc.RegimeFreshnessNotDue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := gammaCadenceFixture(t, tc.now, tc.refreshing, gammaSessionMismatchGates(), rpc.GammaFreshnessSessionMismatch)
			if got := gammaCadenceClass(r, tc.now); got != tc.want {
				t.Fatalf("gamma cadence = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGammaMorningGraceDoesNotReportDegradedInputs is the other half: the
// rankability path. A gamma blocked on session cadence alone, while its
// currency is a scheduled state, is the schedule rather than an input defect —
// but a gamma blocked on anything else still fails closed.
func TestGammaMorningGraceDoesNotReportDegradedInputs(t *testing.T) {
	t.Parallel()
	ny := newYorkLocation()
	now := time.Date(2026, 7, 20, 9, 40, 0, 0, ny)

	pending := gammaCadenceFixture(t, now, true, gammaSessionMismatchGates(), rpc.GammaFreshnessSessionMismatch)
	c := regimeTestFinalize(t, pending)
	if pending.GammaZero.Freshness == nil || pending.GammaZero.Freshness.Class != rpc.RegimeFreshnessPending {
		t.Fatalf("gamma freshness = %+v, want pending", pending.GammaZero.Freshness)
	}
	if pending.Lifecycle.Stage == rpc.LifecycleDataQuality {
		t.Fatalf("a bounded in-flight compute must not blank the market state: %+v", pending.Lifecycle)
	}
	if pending.Lifecycle.Readiness != "ready" {
		t.Fatalf("readiness = %q, want ready while the refresh is inside its window", pending.Lifecycle.Readiness)
	}
	if c.Verdict == "Market state undefined — data incomplete" {
		t.Fatalf("verdict = %q, want a real market read", c.Verdict)
	}
	// Disclosed rather than silent: the source reads ok/pending while the
	// rankability blocker stays visible in data_quality, and no warning cries
	// wolf about a schedule.
	assertRegimeClusterProjection(t, pending, "gamma", rpc.SourceStatusOK, rpc.SourceRefreshPending, true, false)
	// Disclosed, and still unable to vote: rankability keeps gamma unranked.
	if c.ClusterUnrankedCount != 1 {
		t.Fatalf("cluster_unranked_count = %d, want gamma unranked while pending", c.ClusterUnrankedCount)
	}

	coverage := gammaCadenceFixture(t, now, true, []rpc.GammaQualityGate{{
		Name: "oi_coverage", Status: rpc.GammaQualityGateBlock, Reason: "observed OI below the floor",
	}}, "fresh")
	regimeTestFinalize(t, coverage)
	if coverage.Lifecycle.Readiness != "degraded" ||
		!regimeTestGovernorNames(coverage.Lifecycle, "readiness_degraded", "input_currency", "gamma") {
		t.Fatalf("a coverage-blocked gamma must stay an input defect: %+v", coverage.Lifecycle)
	}
}

// vixCarryFixture builds the in-session shape: the previous snapshot observed
// both legs live, and this one lost the VIX3M poll.
func vixCarryFixture(observed, now time.Time, prevVIX, vix3m, nowVIX float64) (res, prev *rpc.RegimeSnapshotResult) {
	prev = &rpc.RegimeSnapshotResult{
		AsOf: observed,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			VIX:    &prevVIX, VIX3M: &vix3m, VIX3MAnchorVIX: &prevVIX,
			VIXQuality:   &rpc.Quality{AsOf: observed, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
			VIX3MQuality: &rpc.Quality{AsOf: observed, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
		},
	}
	res = mkAllGreenRegime()
	res.AsOf = now
	res.VIXTermStructure = rpc.RegimeVIXTerm{
		Status:       rpc.RegimeStatusError,
		ErrorMessage: "VIX3M: no tick within budget (thin CBOE index, common off-hours)",
		VIX:          &nowVIX,
		VIXQuality:   &rpc.Quality{AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
	}
	return res, prev
}

// TestVIXTermInSessionCarryIsStaleContext is the symptom-3 regression: a missed
// VIX3M poll during market hours no longer blanks the vol cluster. The carried
// leg is stale — a failed refresh, not a closed window — so it bands and stays
// visible while never confirming.
func TestVIXTermInSessionCarryIsStaleContext(t *testing.T) {
	t.Parallel()
	ny := newYorkLocation()
	observed := time.Date(2026, 7, 20, 10, 0, 0, 0, ny)
	now := observed.Add(5 * time.Minute)

	res, prev := vixCarryFixture(observed, now, 20.0, 21.0, 20.1)
	if !carryVIXTermFromLastGood(res, prev, now) {
		t.Fatal("a missed in-window poll inside tolerance must carry the previous print")
	}
	if res.VIXTermStructure.Status != rpc.RegimeStatusStale {
		t.Fatalf("carried row status = %q, want stale — a failed refresh is not not_due", res.VIXTermStructure.Status)
	}
	if res.VIXTermStructure.VIX3MAnchorVIX == nil {
		t.Fatal("the carried leg must keep its VIX anchor or the tolerance degrades to a timer")
	}
	if got := vixTermCadenceClass(res, nyTime(now)); got != rpc.RegimeFreshnessStale {
		t.Fatalf("cadence class = %q, want stale", got)
	}

	regimeTestFinalize(t, res)
	if got := rpc.RegimeClusterCurrency(*res, "vol"); got != rpc.RegimeFreshnessStale {
		t.Fatalf("vol cluster currency = %q, want stale context", got)
	}
	if !rpc.RegimeCurrencyMayContext(rpc.RegimeClusterCurrency(*res, "vol")) {
		t.Fatal("the carried vol cluster must stay usable context")
	}
	if rpc.RegimeCurrencyMayConfirm(rpc.RegimeClusterCurrency(*res, "vol")) {
		t.Fatal("a carried VIX3M must never confirm")
	}
	if res.Lifecycle.Stage == rpc.LifecycleDataQuality {
		t.Fatalf("one missed thin-index poll must not blank the market state: %+v", res.Lifecycle)
	}

	for _, tc := range []struct {
		name   string
		vix    float64
		now    time.Time
		anchor bool
	}{
		{name: "VIX moved past the tolerance", vix: 21.0, now: now, anchor: true},
		{name: "past the wall-clock ceiling", vix: 20.1, now: observed.Add(20 * time.Minute), anchor: true},
		{name: "no anchor to measure against", vix: 20.1, now: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, prev := vixCarryFixture(observed, tc.now, 20.0, 21.0, tc.vix)
			if !tc.anchor {
				prev.VIXTermStructure.VIX3MAnchorVIX = nil
			}
			if carryVIXTermFromLastGood(res, prev, tc.now) {
				t.Fatal("carry must be refused outside the agreed tolerance")
			}
			if res.VIXTermStructure.Status != rpc.RegimeStatusError {
				t.Fatalf("row status = %q, want the untouched error", res.VIXTermStructure.Status)
			}
		})
	}
}

// TestVIXTermCarryFreezesTheStreak pins the invariant the shipped streak fix
// established: a session is banked only from cadence-fresh evidence, so a
// carried leg cannot spend one of the sessions the confirmation gate requires.
func TestVIXTermCarryFreezesTheStreak(t *testing.T) {
	t.Parallel()
	ny := newYorkLocation()
	observed := time.Date(2026, 7, 20, 10, 0, 0, 0, ny)
	now := observed.Add(5 * time.Minute)
	// An inverted ratio: red on display, and confirmable only from fresh legs.
	res, prev := vixCarryFixture(observed, now, 21.0, 20.0, 21.1)
	if !carryVIXTermFromLastGood(res, prev, now) {
		t.Fatal("expected the carry to apply")
	}
	s := &Server{streaks: NewStreakStore(t.TempDir())}
	policies := s.populateStreaks(res)
	policy, ok := policies[StreakKeyVIXTerm]
	if !ok || policy.band != "red" {
		t.Fatalf("carried inversion must stay visible as red: %+v", policy)
	}
	if policy.eligibility == nil || policy.eligibility.Eligible {
		t.Fatalf("carried inversion must not be confirmation-eligible: %+v", policy.eligibility)
	}
	if banked := s.streaks.Get(StreakKeyVIXTerm); banked != nil && banked.Band == "red" {
		t.Fatalf("a carried leg banked a red session: %+v", banked)
	}
}
