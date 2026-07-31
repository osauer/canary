package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRegimeIncident20260612Regression replays the 2026-06-12 pre-open false
// positive through the full post-fanout pipeline: HYG 7 bps below its 50DMA
// (one session, thin pre-open context) plus a prior-evening gamma cache
// mutually confirmed "Broad stress regime / confirmed_stress / act" against a
// green tape. Under the confirmation gates both reds are provisional and
// neither may confirm — that is the incident invariant.
//
// The overdue gamma cache is one defective cluster beside five current ones,
// so the read warns with degraded readiness and names the defect rather than
// discarding the five (internal-docs/design/regime-input-currency.md). Break a
// second input and the state goes undefined, as it always did.
func TestRegimeIncident20260612Regression(t *testing.T) {
	t.Parallel()
	ratio := 18.84 / 21.42
	spyChange := 0.3
	vixChange := -3.45
	vvix := 100.6
	// The snapshot clock stays zero so regimeTestFinalize pins its weekday
	// session clock; a wall-clock AsOf makes the vol cadence classifier
	// weekend-dependent (a live VIX3M quality is impossible off-window).
	r := &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			VIX:    new(18.84), VIX3M: new(21.42), Ratio: &ratio,
			VIXChangePct: &vixChange,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK, Last: &vvix,
			// Prior trading day relative to the pinned Monday clock.
			AsOfDate: "2026-07-17",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:   rpc.RegimeStatusOK,
			HYGPrice: new(79.95), HYG50DMA: new(80.008),
			SPYPrice: new(740.06), SPY52WHigh: new(760.39),
			SPYChangePct: &spyChange,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: new(2.80), HY20DChange: new(0.04)},
		FundingStress: rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK, SpreadBps: new(float64(9))},
		USDJPY:        rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, WeeklyChange: new(0.13)},
		GammaZero: rpc.RegimeGammaZero{
			// fetchRegimeGamma downgrades a prior-trading-date compute to
			// stale; the band stays visible for awareness.
			Status: rpc.RegimeStatusStale,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					GammaSign: "negative",
					Quality:   &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status:   rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{State: rpc.BreadthStateReady, PctAbove50DMA: 52},
		},
	}
	c := regimeTestFinalize(t, r)

	if r.Lifecycle.Stage != rpc.LifecycleEarlyWarning || r.Lifecycle.Severity != "watch" {
		t.Fatalf("lifecycle = %s/%s, want early_warning/watch (incident produced confirmed_stress/act)", r.Lifecycle.Stage, r.Lifecycle.Severity)
	}
	if len(r.Lifecycle.ConfirmedBy) != 0 {
		t.Fatalf("confirmed_by = %v, want empty — marginal reds must not confirm", r.Lifecycle.ConfirmedBy)
	}
	for _, want := range []string{"credit", "gamma"} {
		if !slices.Contains(r.Lifecycle.Unconfirmed, want) {
			t.Fatalf("unconfirmed = %v, want %s disclosed", r.Lifecycle.Unconfirmed, want)
		}
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want the provisional red disclosed without confirmation", c.Verdict)
	}
	if c.ClusterEligibleRedCount != 0 {
		t.Fatalf("eligible reds = %d, want 0", c.ClusterEligibleRedCount)
	}
	if r.Posture.Label != c.Verdict {
		t.Fatalf("posture label %q != verdict %q — headline drift", r.Posture.Label, c.Verdict)
	}
	// The stale gamma row degrades readiness (as it did in the real
	// incident), so the non-confirmed stage renders the data_quality tone —
	// a muted "verify inputs" read, never the red stress headline.
	if r.Posture.Tone != rpc.RegimeToneDataQuality || r.Posture.Severity != "watch" {
		t.Fatalf("posture = %s/%s, want data_quality/watch", r.Posture.Tone, r.Posture.Severity)
	}
	if r.Lifecycle.Readiness != "degraded" {
		t.Fatalf("readiness = %q, want degraded with one overdue required input", r.Lifecycle.Readiness)
	}
	if !regimeTestGovernorNames(r.Lifecycle, "readiness_degraded", "input_currency", "gamma") {
		t.Fatalf("governors = %+v, want the overdue gamma cache named", r.Lifecycle.Governors)
	}
	// Row-level disclosures: the gamma red stays visible on the stale row,
	// and HYG's eligibility names the failed depth gate.
	if r.GammaZero.Band != "red" || r.GammaZero.Status != rpc.RegimeStatusStale {
		t.Fatalf("gamma row = band %q status %q, want red/stale awareness", r.GammaZero.Band, r.GammaZero.Status)
	}
	if e := r.HYGSPYDivergence.Eligibility; e == nil || e.Eligible || !slices.Contains(e.Reasons, "depth_below_min") {
		t.Fatalf("hyg eligibility = %+v, want provisional with depth_below_min", e)
	}
	if e := r.GammaZero.Eligibility; e == nil || e.Eligible || !slices.Contains(e.Reasons, "data_overdue") {
		t.Fatalf("gamma eligibility = %+v, want provisional with data_overdue", e)
	}

	// A second broken input and the market state is undefined again.
	second := *r
	second.FundingStress.Status = rpc.RegimeStatusError
	secondComposite := regimeTestFinalize(t, &second)
	if second.Lifecycle.Stage != rpc.LifecycleDataQuality || second.Lifecycle.Readiness != "blocked" {
		t.Fatalf("two broken inputs = %s/%s, want data_quality/blocked", second.Lifecycle.Stage, second.Lifecycle.Readiness)
	}
	if secondComposite.Verdict != "Market state undefined — data incomplete" {
		t.Fatalf("verdict = %q, want explicit undefined data state", secondComposite.Verdict)
	}
	if secondComposite.ClusterEligibleRedCount != 0 {
		t.Fatalf("eligible reds = %d, want 0", secondComposite.ClusterEligibleRedCount)
	}
}

// regimeTestGovernorNames reports whether the disclosed governors carry the
// action/reason and name the cluster.
func regimeTestGovernorNames(state rpc.LifecycleState, action, reason, cluster string) bool {
	for _, g := range state.Governors {
		if g.Action != action || g.Reason != reason {
			continue
		}
		if slices.Contains(g.Clusters, cluster) {
			return true
		}
	}
	return false
}

// TestRegimeOffHoursGammaStaleAloneStaysNormal pins the 2026-07-01 pre-open
// case: every ranked cluster (vol, credit, funding, fx, breadth) is green and
// gamma — the one context_only/unranked cluster (rankableLifecycleGammaBand
// returns "" whenever Rankability != rankable) — is status=stale from the
// EXPECTED off-hours prior-NY-trading-date cache described in gammaNotes
// ("...a prior-date cache serves status=stale and stays visible as not-due
// context", internal/daemon/regime.go) and TestGammaStatusQualityTreatsContext
// OnlyAsContextNotDegraded (status_quality_test.go). With zero yellow/red in
// the ranked evidence, this must read tone=normal / readiness=ready, not the
// amber "verify inputs" data_quality read: the only "weak" thing is gamma's
// intentionally-unranked, by-design-warns-only cache going stale, which must
// never by itself drag down a clean ranked tally.
func TestRegimeOffHoursGammaStaleAloneStaysNormal(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	ny := newYorkLocation()
	r.AsOf = time.Date(2026, 7, 20, 7, 5, 0, 0, ny)
	// At 07:05 ET VIX is live but VIX3M is not disseminated yet, so the term
	// row is the exact typed not-due carry rather than broken volatility data.
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VIXTermStructure.VIXQuality = &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm}
	r.VIXTermStructure.VIX3MQuality = &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm}
	r.VIXTermStructure.VIX3MCrossCheck = rpc.VIX3MCrossCheckAgree
	// Off-hours prior-trading-date compute: fetchRegimeGamma would map this
	// to RegimeStatusStale (see gammaNotes) while Rankability degrades to
	// context_only ("market is closed; cached gamma is context only").
	r.GammaZero.Status = rpc.RegimeStatusStale
	r.GammaZero.Envelope.Result.AsOf = time.Date(2026, 7, 17, 16, 7, 0, 0, ny)
	r.GammaZero.Envelope.Result.Quality = &rpc.GammaSignalQuality{
		Rankability:       rpc.GammaRankabilityContextOnly,
		RankabilityReason: "freshness: market is closed; cached gamma is context only",
	}
	c := regimeTestFinalize(t, r)

	if c.ClusterUnrankedCount != 1 {
		t.Fatalf("unranked clusters = %d, want 1 (gamma context-only)", c.ClusterUnrankedCount)
	}
	if c.ClusterRankedCount != 5 || c.ClusterYellowCount != 0 || c.ClusterRedCount != 0 {
		t.Fatalf("ranked=%d yellow=%d red=%d, want 5/0/0", c.ClusterRankedCount, c.ClusterYellowCount, c.ClusterRedCount)
	}
	if r.Lifecycle.Readiness != "ready" {
		t.Fatalf("readiness = %q, want ready — off-hours gamma-only staleness must not degrade readiness", r.Lifecycle.Readiness)
	}
	if r.Posture.Tone != rpc.RegimeToneNormal {
		t.Fatalf("posture.tone = %q, want %q — off-hours gamma-only staleness must not flip amber", r.Posture.Tone, rpc.RegimeToneNormal)
	}
	if r.Lifecycle.Stage != rpc.LifecycleQuiet {
		t.Fatalf("lifecycle.stage = %q, want %q", r.Lifecycle.Stage, rpc.LifecycleQuiet)
	}
	// The raw observation remains stale/context-only, while the aggregate
	// source health says the next closed-session compute is not due. Normal
	// schedule gaps must not appear as data-quality failures.
	if r.GammaZero.Status != rpc.RegimeStatusStale {
		t.Fatalf("gamma status = %q, want raw stale context", r.GammaZero.Status)
	}
	foundGammaHealth := false
	for _, health := range r.SourceHealth {
		if health.Source != "gamma" {
			continue
		}
		foundGammaHealth = true
		if health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue {
			t.Fatalf("gamma source health = %+v, want ok/not_due", health)
		}
	}
	if !foundGammaHealth {
		t.Fatal("gamma source health missing")
	}
	for _, item := range r.DataQuality {
		if slices.Contains(item.StaleClusters, "gamma") {
			t.Fatalf("data_quality = %+v, normal not_due gamma must not be stale", r.DataQuality)
		}
	}
}

// TestRegimeRankedClusterStaleDegradesReadiness is the regression guard for
// TestRegimeOffHoursGammaStaleAloneStaysNormal: a RANKED cluster's source
// (funding, here) going stale must still force degraded readiness / a
// non-normal tone. This proves the off-hours-gamma fix is scoped to
// context-only/unranked sources and does not overshoot into masking real
// ranked-cluster data problems.
func TestRegimeRankedClusterStaleDegradesReadiness(t *testing.T) {
	t.Parallel()
	// Zero AsOf: regimeTestFinalize pins the weekday session clock, keeping
	// the vol cadence classifier out of weekend scheduled-context states.
	r := mkAllGreenRegime()
	r.FundingStress.Status = rpc.RegimeStatusStale
	c := regimeTestFinalize(t, r)

	if c.ClusterRankedCount != 6 || c.ClusterYellowCount != 0 || c.ClusterRedCount != 0 {
		t.Fatalf("ranked=%d yellow=%d red=%d, want 6/0/0 (funding stays green — only its source status is stale)", c.ClusterRankedCount, c.ClusterYellowCount, c.ClusterRedCount)
	}
	if r.Lifecycle.Readiness != "degraded" || !regimeTestGovernorNames(r.Lifecycle, "readiness_degraded", "input_currency", "funding") {
		t.Fatalf("state = %q/%q governors=%+v, want degraded readiness naming funding", r.Lifecycle.Stage, r.Lifecycle.Readiness, r.Lifecycle.Governors)
	}
	if r.Posture.Tone != rpc.RegimeToneDataQuality {
		t.Fatalf("posture.tone = %q, want %q — ranked-cluster staleness must still flip amber", r.Posture.Tone, rpc.RegimeToneDataQuality)
	}
	// A second stale ranked cluster leaves too little current evidence to
	// describe the market at all.
	second := mkAllGreenRegime()
	second.FundingStress.Status = rpc.RegimeStatusStale
	second.CreditSpreads.Status = rpc.RegimeStatusStale
	regimeTestFinalize(t, second)
	if second.Lifecycle.Readiness != "blocked" || second.Lifecycle.Stage != rpc.LifecycleDataQuality {
		t.Fatalf("state = %q/%q, want data_quality/blocked for two stale ranked clusters", second.Lifecycle.Stage, second.Lifecycle.Readiness)
	}
}

// TestStreakStoreCountsTradingDays pins the weekend fix: a Saturday or
// Sunday poll keys to Friday's trading session and cannot inflate the
// counter; Monday increments it.
func TestStreakStoreCountsTradingDays(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStreakStore(t.TempDir())
	friday := time.Date(2026, 6, 5, 10, 0, 0, 0, loc)
	saturday := time.Date(2026, 6, 6, 12, 0, 0, 0, loc)
	sunday := time.Date(2026, 6, 7, 12, 0, 0, 0, loc)
	monday := time.Date(2026, 6, 8, 10, 0, 0, 0, loc)

	if got := s.Tick("test_ind", 1.0, "red", friday); got.Sessions != 1 {
		t.Fatalf("friday sessions = %d, want 1", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", saturday); got.Sessions != 1 {
		t.Fatalf("saturday sessions = %d, want 1 (weekend keys to Friday)", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", sunday); got.Sessions != 1 {
		t.Fatalf("sunday sessions = %d, want 1", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", monday); got.Sessions != 2 {
		t.Fatalf("monday sessions = %d, want 2", got.Sessions)
	}
}

// TestStreakStoreEligibilityLatch pins the latch lifecycle: earned on a red
// streak, held across ticks, dropped on band change.
func TestStreakStoreEligibilityLatch(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	s := NewStreakStore(t.TempDir())
	day1 := time.Date(2026, 6, 4, 10, 0, 0, 0, loc)
	day2 := time.Date(2026, 6, 5, 10, 0, 0, 0, loc)

	s.Tick("test_ind", 1.0, "red", day1)
	if s.Latched("test_ind") {
		t.Fatal("latch must not pre-exist")
	}
	s.Latch("test_ind")
	if !s.Latched("test_ind") {
		t.Fatal("latch should be set on a live red streak")
	}
	s.Tick("test_ind", 1.0, "red", day2)
	if !s.Latched("test_ind") {
		t.Fatal("latch must survive same-band ticks")
	}
	s.Tick("test_ind", 0.5, "green", day2)
	if s.Latched("test_ind") {
		t.Fatal("band change must drop the latch")
	}
	// Latch on a non-red entry is a no-op.
	s.Latch("test_ind")
	if s.Latched("test_ind") {
		t.Fatal("latch must not decorate a non-red streak")
	}
}

// TestPopulateStreaksExitHysteresisHoldsRed pins the boundary-flap guard: a
// prior red holds while the exit threshold has not cleared, and releases
// once it does.
func TestPopulateStreaksExitHysteresisHoldsRed(t *testing.T) {
	t.Parallel()
	s := &Server{streaks: NewStreakStore(t.TempDir())}
	// Pin a regular-session time when both VIX and VIX3M can legitimately be
	// live. Using time.Now made the assertion change after VIX3M's daily
	// dissemination window, when the same live-labelled fixture is correctly
	// classified overdue and the copy describes prior-session awareness.
	asOf := time.Date(2026, time.June, 5, 14, 0, 0, 0, time.UTC)
	mk := func(ratio float64) *rpc.RegimeSnapshotResult {
		r := &rpc.RegimeSnapshotResult{AsOf: asOf}
		r.VIXTermStructure = rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK, Ratio: new(ratio),
			VIXQuality:   &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
			VIX3MQuality: &rpc.Quality{AsOf: r.AsOf, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
		}
		return r
	}
	// Enter red.
	res := mk(1.02)
	policies := s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band != "red" {
		t.Fatalf("entry band = %q, want red", policies[StreakKeyVIXTerm].band)
	}
	// Wobble below the entry threshold but above the 0.98 exit: held red.
	res = mk(0.99)
	policies = s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band != "red" {
		t.Fatalf("hysteresis band = %q, want red held at ratio 0.99", policies[StreakKeyVIXTerm].band)
	}
	annotateRegimeMetadata(res, policies)
	if !strings.Contains(res.VIXTermStructure.BandReason, "hysteresis") {
		t.Fatalf("band reason = %q, want hysteresis disclosure", res.VIXTermStructure.BandReason)
	}
	// Clear the exit threshold: release.
	res = mk(0.97)
	policies = s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band == "red" {
		t.Fatalf("band = %q, want released below the exit threshold", policies[StreakKeyVIXTerm].band)
	}
}

// TestRegimeDecisionJournalDedupesAndHeartbeats pins the journal contract:
// fingerprint-deduped, hourly heartbeat, one valid JSON line per write.
func TestRegimeDecisionJournalDedupesAndHeartbeats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := &regimeDecisionJournal{path: filepath.Join(dir, "regime-decisions.jsonl")}
	res := &rpc.RegimeSnapshotResult{
		Fingerprint:      rpc.Fingerprint{Version: "v", Key: "sha256:aaa"},
		TapeSessionState: rpc.TapeSessionClosedDate,
		Lifecycle:        rpc.LifecycleState{Stage: rpc.LifecycleQuiet, Severity: "observe"},
		Composite:        rpc.RegimeComposite{Verdict: "Normal regime"},
	}
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	if err := j.append(now, res); err != nil {
		t.Fatal(err)
	}
	if err := j.append(now.Add(time.Minute), res); err != nil {
		t.Fatal(err)
	}
	lines := journalLines(t, j.path)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (same fingerprint dedupes)", len(lines))
	}
	// Heartbeat after an hour even with an unchanged fingerprint.
	if err := j.append(now.Add(61*time.Minute), res); err != nil {
		t.Fatal(err)
	}
	// Semantic change writes immediately.
	res.Fingerprint.Key = "sha256:bbb"
	if err := j.append(now.Add(62*time.Minute), res); err != nil {
		t.Fatal(err)
	}
	lines = journalLines(t, j.path)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3 (heartbeat + semantic change)", len(lines))
	}
	for i, line := range lines {
		var decoded regimeDecisionLine
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if decoded.V != 2 || decoded.Stage == "" {
			t.Fatalf("line %d = %+v, want v2 with stage", i, decoded)
		}
		// The calibration corpus must be partitionable across policy cutovers.
		if decoded.CurrencyPolicy != rpc.RegimeCurrencyPolicyVersion {
			t.Fatalf("line %d currency_policy = %q, want %q", i, decoded.CurrencyPolicy, rpc.RegimeCurrencyPolicyVersion)
		}
		if decoded.TapeSession != rpc.TapeSessionClosedDate {
			t.Fatalf("line %d tape_session = %q, want the snapshot's session state journaled", i, decoded.TapeSession)
		}
	}
}

func journalLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
