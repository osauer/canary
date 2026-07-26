package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimeTestFinalize runs the post-fanout pipeline exactly as
// handleRegimeSnapshot does — policy pass (fresh streak store), metadata,
// composite, lifecycle, unified verdict — so tests assert the served
// contract, not an intermediate. Fresh store ⇒ every red is streak day 1.
func regimeTestFinalize(t *testing.T, r *rpc.RegimeSnapshotResult) rpc.RegimeComposite {
	t.Helper()
	if r.AsOf.IsZero() {
		r.AsOf = time.Date(2026, 7, 20, 12, 0, 0, 0, newYorkLocation())
	}
	if r.VIXTermStructure.Status == rpc.RegimeStatusOK {
		if r.VIXTermStructure.VIXQuality == nil {
			r.VIXTermStructure.VIXQuality = &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm}
		}
		if r.VIXTermStructure.VIX3MQuality == nil {
			r.VIXTermStructure.VIX3MQuality = &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm}
		}
	}
	if r.VolOfVol.Status == rpc.RegimeStatusOK && r.VolOfVol.AsOfDate == "" {
		r.VolOfVol.AsOfDate = nyTime(r.AsOf).Format("2006-01-02")
	}
	if r.GammaZero.Status == rpc.RegimeStatusOK && r.GammaZero.Envelope.Result != nil && r.GammaZero.Envelope.Result.AsOf.IsZero() {
		r.GammaZero.Envelope.Result.AsOf = r.AsOf
	}
	s := &Server{streaks: NewStreakStore(t.TempDir())}
	policies := s.populateStreaks(r)
	annotateRegimeMetadata(r, policies)
	r.Composite = buildRegimeComposite(r)
	r.Summary = buildRegimeSummary(r)
	r.WarningDetails = buildRegimeWarnings(r)
	r.DataQuality = regimeSnapshotDataQuality(r)
	r.SourceHealth = rpc.BuildRegimeSourceHealth(r, r.AsOf)
	r.Lifecycle = rpc.BuildRegimeLifecycle(r)
	r.Composite.Verdict = rpc.RegimeHeadline(r.Composite, r.Lifecycle.Stage)
	r.Summary.Label = r.Composite.Verdict
	r.Posture = rpc.BuildRegimePosture(r)
	return r.Composite
}

// TestBuildRegimeComposite_AllGreenIsNormalRegime pins the happy path:
// eight green rows produce a "Normal regime" verdict with six ranked
// clusters, no unranked.
func TestBuildRegimeComposite_AllGreenIsNormalRegime(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	c := regimeTestFinalize(t, r)
	if c.Verdict != "Normal regime" {
		t.Errorf("verdict: got %q want %q", c.Verdict, "Normal regime")
	}
	if c.GreenCount != 8 {
		t.Errorf("green: got %d want 8", c.GreenCount)
	}
	if c.RankedCount != 8 {
		t.Errorf("ranked: got %d want 8", c.RankedCount)
	}
	if c.UnrankedCount != 0 {
		t.Errorf("unranked: got %d want 0", c.UnrankedCount)
	}
	if c.ClusterGreenCount != 6 || c.ClusterRankedCount != 6 {
		t.Errorf("clusters: got green=%d ranked=%d want 6/6", c.ClusterGreenCount, c.ClusterRankedCount)
	}
}

func TestBreadthEnvelopeStaleUsesSessionKey(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	envelope := &rpc.BreadthSPXResult{
		SessionKey: "2026-05-29",
		AsOf:       time.Date(2026, 5, 29, 17, 0, 0, 0, loc),
	}
	weekend := time.Date(2026, 5, 31, 23, 55, 0, 0, loc)
	if breadthEnvelopeStale(envelope, weekend) {
		t.Fatal("Friday breadth should remain current through the weekend")
	}
	mondayPostClose := time.Date(2026, 6, 1, 17, 0, 0, 0, loc)
	if !breadthEnvelopeStale(envelope, mondayPostClose) {
		t.Fatal("Friday breadth should be stale after Monday's post-close refresh window")
	}
}

func TestBreadthPublicationWindowNormalizesOnlyActivePriorLastGood(t *testing.T) {
	loc := newYorkLocation()
	build := func(now time.Time, active bool) *rpc.RegimeSnapshotResult {
		r := mkAllGreenRegime()
		r.AsOf = now
		r.Breadth.Status = rpc.RegimeStatusStale
		r.Breadth.Envelope = rpc.BreadthSPXResult{
			State: rpc.BreadthStateReady, Refreshing: active,
			SessionKey: "2026-05-29", AsOf: time.Date(2026, 5, 29, 17, 0, 0, 0, loc),
			PctAbove50DMA: 65,
		}
		regimeTestFinalize(t, r)
		return r
	}

	before := build(time.Date(2026, 6, 1, 17, 30, 0, 0, loc), true)
	if before.Breadth.Status != rpc.RegimeStatusStale {
		t.Fatalf("raw breadth status=%q, want stale evidence", before.Breadth.Status)
	}
	if before.Breadth.Freshness == nil || before.Breadth.Freshness.Class != rpc.RegimeFreshnessNotDue {
		t.Fatalf("breadth freshness=%+v, want not_due before deadline", before.Breadth.Freshness)
	}
	assertRegimeClusterProjection(t, before, "breadth", rpc.SourceStatusOK, rpc.SourceRefreshNotDue, false, false)

	deadline, ok := spx.PublicationDeadline("2026-06-01")
	if !ok {
		t.Fatal("resolve breadth publication deadline")
	}
	missed := build(deadline, true)
	if missed.Breadth.Freshness == nil || missed.Breadth.Freshness.Class != rpc.RegimeFreshnessOverdue {
		t.Fatalf("breadth freshness=%+v, want overdue at deadline", missed.Breadth.Freshness)
	}
	assertRegimeClusterProjection(t, missed, "breadth", rpc.SourceStatusStale, "", true, true)

	stuck := build(time.Date(2026, 6, 1, 17, 30, 0, 0, loc), false)
	if stuck.Breadth.Freshness == nil || stuck.Breadth.Freshness.Class != rpc.RegimeFreshnessOverdue {
		t.Fatalf("idle breadth freshness=%+v, want overdue before deadline", stuck.Breadth.Freshness)
	}
	assertRegimeClusterProjection(t, stuck, "breadth", rpc.SourceStatusStale, "", true, true)
}

func TestVIXNotDueAggregateRequiresCurrentVVIX(t *testing.T) {
	loc := newYorkLocation()
	quality := func(at time.Time, class string) *rpc.Quality {
		return &rpc.Quality{AsOf: at, FreshnessClass: class, Confidence: rpc.ConfidenceFirm}
	}
	build := func(now time.Time, vvixStatus string) *rpc.RegimeSnapshotResult {
		r := mkAllGreenRegime()
		r.AsOf = now
		r.VIXTermStructure.Status = rpc.RegimeStatusStale
		r.VIXTermStructure.VIXQuality = quality(now, rpc.FreshnessLive)
		r.VIXTermStructure.VIX3MQuality = quality(now, rpc.FreshnessFrozen)
		r.VolOfVol.Status = vvixStatus
		r.VolOfVol.AsOfDate = now.Format("2006-01-02")
		regimeTestFinalize(t, r)
		return r
	}

	notDue := build(time.Date(2026, 7, 20, 4, 0, 0, 0, loc), rpc.RegimeStatusOK)
	if notDue.VIXTermStructure.Freshness == nil || notDue.VIXTermStructure.Freshness.Class != rpc.RegimeFreshnessNotDue {
		t.Fatalf("VIX freshness=%+v, want not_due", notDue.VIXTermStructure.Freshness)
	}
	assertRegimeClusterProjection(t, notDue, "vol", rpc.SourceStatusOK, rpc.SourceRefreshNotDue, false, false)

	staleVVIX := build(time.Date(2026, 7, 20, 4, 0, 0, 0, loc), rpc.RegimeStatusStale)
	assertRegimeClusterProjection(t, staleVVIX, "vol", rpc.SourceStatusStale, "", true, true)

	due := build(time.Date(2026, 7, 20, 9, 31, 0, 0, loc), rpc.RegimeStatusOK)
	if due.VIXTermStructure.Freshness == nil || due.VIXTermStructure.Freshness.Class != rpc.RegimeFreshnessOverdue {
		t.Fatalf("VIX freshness=%+v, want overdue at due boundary", due.VIXTermStructure.Freshness)
	}
	assertRegimeClusterProjection(t, due, "vol", rpc.SourceStatusStale, "", true, true)
}

func assertRegimeClusterProjection(t *testing.T, r *rpc.RegimeSnapshotResult, cluster, wantStatus, wantRefresh string, wantQuality, wantWarning bool) {
	t.Helper()
	var health *rpc.SourceHealth
	for i := range r.SourceHealth {
		if strings.EqualFold(r.SourceHealth[i].Source, cluster) {
			health = &r.SourceHealth[i]
			break
		}
	}
	if health == nil || health.Status != wantStatus || health.RefreshState != wantRefresh {
		t.Fatalf("%s source health=%+v, want status=%q refresh=%q", cluster, health, wantStatus, wantRefresh)
	}
	hasQuality := false
	for _, q := range r.DataQuality {
		if regimeTestClustersContain(q.StaleClusters, cluster) ||
			regimeTestClustersContain(q.PartialClusters, cluster) ||
			regimeTestClustersContain(q.DegradedClusters, cluster) {
			hasQuality = true
		}
	}
	if hasQuality != wantQuality {
		t.Fatalf("%s data-quality presence=%v, want %v: %+v", cluster, hasQuality, wantQuality, r.DataQuality)
	}
	hasWarning := false
	for _, warning := range r.WarningDetails {
		if regimeWarningCluster(warning.Scope) == cluster {
			hasWarning = true
		}
	}
	if hasWarning != wantWarning {
		t.Fatalf("%s warning presence=%v, want %v: %+v", cluster, hasWarning, wantWarning, r.WarningDetails)
	}
}

func regimeTestClustersContain(clusters []string, want string) bool {
	for _, cluster := range clusters {
		if strings.EqualFold(cluster, want) {
			return true
		}
	}
	return false
}

// TestBuildRegimeComposite_ThreeRedTriggersRegimeShift pins the spec
// interpretation table: ≥3 red bands surface as a broad stress label.
func TestBuildRegimeComposite_ThreeRedTriggersRegimeShift(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	// Force VIX, USD/JPY, breadth into red bands.
	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	weekly := -3.0 // yen strengthening 3% = red
	r.USDJPY.WeeklyChange = &weekly
	r.Breadth.Envelope.PctAbove50DMA = 30 // <40 = red
	c := regimeTestFinalize(t, r)
	if c.RedCount != 3 {
		t.Errorf("red: got %d want 3", c.RedCount)
	}
	if c.Verdict != "Broad stress regime" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_SingleRedTriggersWatch pins the
// one-red branch: a single red row reads as a stress signal.
func TestBuildRegimeComposite_SingleRedTriggersWatch(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 {
		t.Errorf("red: got %d want 1", c.RedCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

func TestBuildRegimeComposite_RedClusterCannotBeHiddenByGreenRows(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	vvix := 125.0
	r.VolOfVol.Last = &vvix
	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 || c.GreenCount != 7 {
		t.Fatalf("red/green counts = %d/%d, want 1/7", c.RedCount, c.GreenCount)
	}
	if c.ClusterRedCount != 1 {
		t.Fatalf("equity-vol cluster should be red despite green VIX term row, got red clusters=%d", c.ClusterRedCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

func TestBuildRegimeComposite_IsolatedVVIXRedStaysVisibleButClusterIsWatch(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	vvix := 112.0
	r.VolOfVol.Last = &vvix
	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want isolated VVIX row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("isolated non-severe VVIX red should count as cluster yellow, got %+v", c)
	}
	// Unified headline: a raw red row — even downgraded at cluster level —
	// reads "Stress signal present" (early_warning), never "Normal regime".
	// Pre-unification the verdict said Normal while the lifecycle warned.
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want provisional red disclosed as stress signal", c.Verdict)
	}
}

func TestBuildRegimeComposite_MixedClusterDisagreementUsesWorstBand(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 740.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52
	c := regimeTestFinalize(t, r)
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("unconfirmed HYG/SPY proxy red should be cluster yellow with green OAS companion, got %+v", c)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q, want the raw red disclosed", c.Verdict)
	}
}

// TestBuildRegimeComposite_UnrankedDoesntCountTowardBand pins the
// honesty contract: computing / unavailable rows don't contribute to
// the green tally — they sit in unranked so a consumer sees the
// reduced coverage explicitly.
func TestBuildRegimeComposite_UnrankedDoesntCountTowardBand(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.GammaZero.Envelope.Result = nil
	c := regimeTestFinalize(t, r)
	if c.GreenCount != 7 {
		t.Errorf("green: got %d want 7 (gamma row should not count)", c.GreenCount)
	}
	if c.UnrankedCount != 1 {
		t.Errorf("unranked: got %d want 1", c.UnrankedCount)
	}
	if c.RankedCount != 7 {
		t.Errorf("ranked: got %d want 7", c.RankedCount)
	}
}

func TestBuildRegimeComposite_GammaRequiresExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		quality *rpc.GammaSignalQuality
	}{
		{name: "nil_quality"},
		{name: "blocked_quality", quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "OI coverage blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := mkAllGreenRegime()
			r.GammaZero.Envelope.Result.Quality = tc.quality

			c := regimeTestFinalize(t, r)
			if c.GreenCount != 7 || c.RankedCount != 7 || c.UnrankedCount != 1 {
				t.Fatalf("indicator counts = green %d ranked %d unranked %d, want 7/7/1", c.GreenCount, c.RankedCount, c.UnrankedCount)
			}
			if c.ClusterGreenCount != 5 || c.ClusterRankedCount != 5 || c.ClusterUnrankedCount != 1 {
				t.Fatalf("cluster counts = green %d ranked %d unranked %d, want 5/5/1", c.ClusterGreenCount, c.ClusterRankedCount, c.ClusterUnrankedCount)
			}
			if got := bandForGamma(r.GammaZero); got != "" {
				t.Fatalf("bandForGamma = %q, want unranked", got)
			}
		})
	}
}

func TestBuildRegimeComposite_HYGSPYNearHighCountsRed(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 749.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52
	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 {
		t.Errorf("HYG/SPY current divergence should count as one red row, got %d", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("unconfirmed HYG/SPY proxy red should be cluster yellow, got %+v", c)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q, want the raw red disclosed", c.Verdict)
	}
}

func TestBuildRegimeComposite_HYGSPYRedRequiresConfirmation(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 740.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52

	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want HYG/SPY row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("cluster counts = red %d yellow %d, want 0/1 for unconfirmed proxy red", c.ClusterRedCount, c.ClusterYellowCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want the unconfirmed proxy red disclosed", c.Verdict)
	}

	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	c = regimeTestFinalize(t, r)
	if c.ClusterRedCount != 2 {
		t.Fatalf("confirmed proxy red should count once vol is also red, got %+v", c)
	}
	// HYG 1.25% below its DMA is fast-path eligible and the vol inversion
	// is fast-path eligible — two eligible reds confirm.
	if c.ClusterEligibleRedCount != 2 {
		t.Fatalf("eligible red count = %d, want 2", c.ClusterEligibleRedCount)
	}
	if c.Verdict != "Confirmed stress regime" {
		t.Fatalf("verdict = %q, want confirmed stress with two eligible reds", c.Verdict)
	}
}

func TestBuildRegimeComposite_USDJPYRedRequiresConfirmation(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	weekly := -2.5
	r.USDJPY.WeeklyChange = &weekly

	c := regimeTestFinalize(t, r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want USD/JPY row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("cluster counts = red %d yellow %d, want 0/1 for unconfirmed FX proxy red", c.ClusterRedCount, c.ClusterYellowCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want the unconfirmed FX red disclosed", c.Verdict)
	}

	// Breadth at 30 is fast-path eligible (≤30) — an ELIGIBLE independent
	// red, which is what the FX rescue now requires. A marginal day-one
	// breadth red (e.g. 38) would no longer rescue FX.
	breadth := 30.0
	r.Breadth.Envelope.PctAbove50DMA = breadth
	c = regimeTestFinalize(t, r)
	if c.ClusterRedCount != 2 {
		t.Fatalf("confirmed FX red should count once breadth is eligibly red, got %+v", c)
	}
	if c.Verdict != "Confirmed stress regime" {
		t.Fatalf("verdict = %q, want confirmed stress with two eligible reds", c.Verdict)
	}
}

// TestBuildRegimeComposite_BelowFloorIsInsufficient pins the
// honesty-floor: a market verdict can't be claimed when fewer than 3 rows
// are ranked; the operator sees an explicit undefined data state.
func TestBuildRegimeComposite_BelowFloorIsInsufficient(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	// Kill enough clusters to leave only equity-vol + gamma ranked.
	r.HYGSPYDivergence.Status = rpc.RegimeStatusError
	r.CreditSpreads.Status = rpc.RegimeStatusError
	r.FundingStress.Status = rpc.RegimeStatusError
	r.USDJPY.Status = rpc.RegimeStatusError
	r.Breadth.Status = rpc.RegimeStatusError
	c := regimeTestFinalize(t, r)
	if c.ClusterRankedCount >= verdictFloor {
		t.Fatalf("test setup wrong: ranked clusters %d, want < %d", c.ClusterRankedCount, verdictFloor)
	}
	if c.Verdict != "Market state undefined — data incomplete" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_NilReturnsHonestVerdict guards the
// defensive nil path so a handler that returns the helper directly
// doesn't crash on a fresh result envelope.
func TestBuildRegimeComposite_NilReturnsHonestVerdict(t *testing.T) {
	t.Parallel()
	c := buildRegimeComposite(nil)
	if c.RankedCount != 0 || c.UnrankedCount != 0 {
		t.Errorf("nil should produce zero counts: %+v", c)
	}
	if c.Verdict == "" {
		t.Errorf("nil verdict should be non-empty")
	}
}

func TestBuildRegimeSummaryAndWarnings(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.USDJPY.Status = rpc.RegimeStatusUnavailable
	r.USDJPY.WeeklyChange = nil
	r.USDJPY.ErrorMessage = "USD.JPY: gateway delivered no FX tick"
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.GammaZero.Envelope = rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusComputing, EtaSeconds: 42}
	regimeTestFinalize(t, r)

	s := r.Summary
	if s.Label != "Market state undefined — data incomplete" {
		t.Errorf("summary label: got %q", s.Label)
	}
	if r.Lifecycle.Stage != rpc.LifecycleDataQuality || r.Lifecycle.Readiness != "blocked" || r.Lifecycle.Confidence != "low" {
		t.Errorf("broken FX and computing gamma must fail closed as data quality: %+v", r.Lifecycle)
	}
	if s.Evidence != "4 green clusters / 2 unranked clusters" {
		t.Errorf("summary evidence: got %q", s.Evidence)
	}
	if s.IndicatorEvidence != "6 green / 2 unranked" {
		t.Errorf("summary indicator evidence: got %q", s.IndicatorEvidence)
	}
	if s.Confidence != "medium" {
		t.Errorf("summary confidence: got %q", s.Confidence)
	}
	if s.NotAdvice == "" {
		t.Errorf("summary should carry non-advice disclosure")
	}
	if s.PunchLine == "" || !containsAll(s.PunchLine, "constructive", "unavailable", "computing") {
		t.Errorf("summary punch line missing evidence states: %q", s.PunchLine)
	}

	warnings := buildRegimeWarnings(r)
	if len(warnings) != 2 {
		t.Fatalf("warnings len=%d want 2: %+v", len(warnings), warnings)
	}
	if warnings[0].Scope == "" || warnings[0].Impact == "" || warnings[0].Action == "" {
		t.Errorf("warnings should be scoped prose with impact/action: %+v", warnings[0])
	}
}

func TestBuildRegimeSummaryStaleRankedRowsLowerConfidence(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.CreditSpreads.Status = rpc.RegimeStatusStale
	regimeTestFinalize(t, r)

	s := r.Summary
	if s.Confidence != "medium" {
		t.Fatalf("confidence=%q, want medium when a ranked row is stale", s.Confidence)
	}
	if !strings.Contains(s.PunchLine, "cash credit spreads is ranked from stale data") {
		t.Errorf("punch line should disclose stale ranked row: %q", s.PunchLine)
	}
}

func TestAnnotateRegimeMetadataAddsBandThresholdsAndAsOf(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("CEST", 2*60*60)
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, loc)
	r := mkAllGreenRegime()
	r.AsOf = now
	r.VIXTermStructure.DataType = rpc.MarketDataLive
	r.VIXTermStructure.VIXQuality = &rpc.Quality{AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm, Source: "VIX tick"}
	r.VolOfVol.AsOfDate = "2026-05-22"
	r.Breadth.Envelope.AsOf = now.Add(-18 * time.Minute)
	r.Breadth.Envelope.Source = "local breadth cache"
	r.Breadth.Envelope.Method = "constituent-fanout"

	annotateRegimeMetadata(r, nil)

	if r.VIXTermStructure.Band != "green" || r.VIXTermStructure.AsOf.Label != "live" {
		t.Fatalf("VIX metadata = band %q as_of %#v, want green/live", r.VIXTermStructure.Band, r.VIXTermStructure.AsOf)
	}
	if r.VIXTermStructure.Thresholds == nil || !r.VIXTermStructure.Thresholds.Heuristic || !r.VIXTermStructure.Thresholds.PendingBacktest {
		t.Fatalf("threshold metadata should mark heuristic pending backtest: %#v", r.VIXTermStructure.Thresholds)
	}
	if r.VolOfVol.AsOf.Label != "2d old" {
		t.Fatalf("VVIX as_of=%#v, want 2d old", r.VolOfVol.AsOf)
	}
	if r.Breadth.AsOf.Label == "" || !strings.HasPrefix(r.Breadth.AsOf.Label, "cached ") {
		t.Errorf("breadth as_of should be cached clock label, got %#v", r.Breadth.AsOf)
	}
}

// mkAllGreenRegime returns a fixture where every row classifies green
// under the spec defaults. Used as the baseline so each test can
// perturb one indicator and verify the rollup tracks.
func mkAllGreenRegime() *rpc.RegimeSnapshotResult {
	ratio := 0.85
	hyg := 80.0
	hyg50 := 78.0
	spy := 580.0
	spy52 := 600.0
	weekly := -0.5 // <1% weekly = green
	vvix := 75.0
	hyOAS := 3.2
	igOAS := 1.2
	hyIG := 2.0
	hy20d := 0.1
	funding := 12.0
	zg := 580.0
	gap := 5.0 // >2% above γ-zero = green
	return &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			Ratio:  &ratio,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
			Last:   &vvix,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:     rpc.RegimeStatusOK,
			HYGPrice:   &hyg,
			HYG50DMA:   &hyg50,
			SPYPrice:   &spy,
			SPY52WHigh: &spy52,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status:      rpc.RegimeStatusOK,
			HYOAS:       &hyOAS,
			IGOAS:       &igOAS,
			HYIGSpread:  &hyIG,
			HY20DChange: &hy20d,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status:    rpc.RegimeStatusOK,
			SpreadBps: &funding,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusOK,
			WeeklyChange: &weekly,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					SpotUnderlying: 580.0,
					ZeroGamma:      &zg,
					GapPct:         &gap,
					Quality:        rankableGammaQuality(),
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{
				State:         rpc.BreadthStateReady,
				PctAbove50DMA: 65,
			},
		},
	}
}

func rankableGammaQuality() *rpc.GammaSignalQuality {
	return &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}
}

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
