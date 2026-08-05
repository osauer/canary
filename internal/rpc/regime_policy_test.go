package rpc

import (
	"encoding/json"
	"testing"
)

// TestEvaluateRegimeEligibility pins the depth/persistence/freshness gates
// per indicator, including the fast paths, the latch, and the fresh-install
// (sessions<=0) default.
//
//go:fix inline
func TestEvaluateRegimeEligibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		in           RegimeEligibilityInput
		wantEligible bool
		wantReason   string
	}{
		{
			// The 2026-06-12 incident HYG read: 0.07% below the DMA, day 1.
			name:         "hyg incident depth fails floor",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(0.07), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: false,
			wantReason:   "depth_below_min",
		},
		{
			name:         "hyg deep break is fast-path eligible day one",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(1.3), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: true,
		},
		{
			name:         "hyg moderate depth needs two sessions",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(0.4), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: false,
			wantReason:   "streak_1_of_2",
		},
		{
			name:         "hyg moderate depth second session confirms",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(0.4), StreakSessions: 2, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: true,
		},
		{
			name:         "overdue data never eligible",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorGammaZero, Band: "red", Depth: new(3.0), StreakSessions: 5, Fresh: false},
			wantEligible: false,
			wantReason:   "data_overdue",
		},
		{
			name:         "latch holds eligibility through depth wobble",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(0.1), StreakSessions: 3, Fresh: true, FreshnessClass: RegimeFreshnessFresh, Latched: true},
			wantEligible: true,
		},
		{
			name:         "latch never overrides freshness",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", Depth: new(0.5), StreakSessions: 3, Fresh: false, Latched: true},
			wantEligible: false,
			wantReason:   "data_overdue",
		},
		{
			name:         "latch never overrides not-due cadence",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorVIXTerm, Band: "red", Depth: new(1.06), StreakSessions: 3, Fresh: false, FreshnessClass: RegimeFreshnessNotDue, Latched: true},
			wantEligible: false,
			wantReason:   "data_not_due",
		},
		{
			name:         "vix inversion needs two sessions",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorVIXTerm, Band: "red", Depth: new(1.01), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: false,
			wantReason:   "streak_1_of_2",
		},
		{
			name:         "deep vix inversion is fast-path eligible day one",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorVIXTerm, Band: "red", Depth: new(1.06), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: true,
		},
		{
			name:         "fresh-install nil streak treated as one session",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorBreadth, Band: "red", Depth: new(3.0), StreakSessions: 0, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: false,
			wantReason:   "streak_1_of_2",
		},
		{
			name:         "streak-one indicators confirm immediately",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorUSDJPY, Band: "red", StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: true,
		},
		{
			// Currency is an allowlist on fresh: every other class, and any
			// class the vocabulary does not know yet, fails closed even when
			// the row's own status says fresh.
			name:         "pending refresh never eligible",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorGammaZero, Band: "red", Depth: new(3.0), StreakSessions: 5, Fresh: true, FreshnessClass: RegimeFreshnessPending},
			wantEligible: false,
			wantReason:   "data_refresh_pending",
		},
		{
			name:         "carried-within-tolerance stale never eligible",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorVIXTerm, Band: "red", Depth: new(1.06), StreakSessions: 3, Fresh: true, FreshnessClass: RegimeFreshnessStale},
			wantEligible: false,
			wantReason:   "data_stale",
		},
		{
			name:         "unknown currency class fails closed",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorUSDJPY, Band: "red", StreakSessions: 3, Fresh: true, FreshnessClass: "someday_maybe"},
			wantEligible: false,
			wantReason:   "data_overdue",
		},
		{
			name:         "untyped currency class fails closed",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorUSDJPY, Band: "red", StreakSessions: 3, Fresh: true},
			wantEligible: false,
			wantReason:   "data_overdue",
		},
		{
			name:         "gamma shallow gap fails the transition floor",
			in:           RegimeEligibilityInput{Indicator: RegimeIndicatorGammaZero, Band: "red", Depth: new(0.2), StreakSessions: 1, Fresh: true, FreshnessClass: RegimeFreshnessFresh},
			wantEligible: false,
			wantReason:   "depth_below_min",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateRegimeEligibility(tc.in)
			if got == nil {
				t.Fatal("nil eligibility for red band")
			}
			if got.Eligible != tc.wantEligible {
				t.Fatalf("eligible = %v (%+v), want %v", got.Eligible, got, tc.wantEligible)
			}
			if tc.wantReason != "" {
				if len(got.Reasons) == 0 || got.Reasons[0] != tc.wantReason {
					t.Fatalf("reasons = %v, want first %q", got.Reasons, tc.wantReason)
				}
			}
		})
	}
	if got := EvaluateRegimeEligibility(RegimeEligibilityInput{Indicator: RegimeIndicatorVIXTerm, Band: "green"}); got != nil {
		t.Fatalf("eligibility on non-red band = %+v, want nil", got)
	}
}

// TestRegimeBandAloneGates pins the four indicators whose eligibility turns on
// the band alone. credit, funding and usdjpy carry a depth scale — FastDepth,
// the saturation point — but no depth floor; gamma's floor and fast path both
// sit below its own red boundary, so neither can fire. Making any of them gate
// on depth or persistence is a risk-policy change, not a refactor.
func TestRegimeBandAloneGates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		indicator string
		depths    []float64 // spanning that indicator's red range
	}{
		// 2.7 is a red reached through the 20-day-widening trigger, far below
		// the 5.5 level trigger — the reason credit carries no depth floor.
		{RegimeIndicatorCredit, []float64{2.7, 5.5, 6.5, 12.0}},
		{RegimeIndicatorFunding, []float64{75, 105, 400}},
		{RegimeIndicatorUSDJPY, []float64{2.0, 7.0, 10.0}},
		// 100 is the wholly-short profile with no crossing.
		{RegimeIndicatorGammaZero, []float64{2.01, 4.5, 100}},
	}
	for _, tc := range cases {
		for _, depth := range tc.depths {
			for sessions := range 4 { // 0 is the fresh-install default
				got := EvaluateRegimeEligibility(RegimeEligibilityInput{
					Indicator:      tc.indicator,
					Band:           "red",
					Depth:          new(depth),
					StreakSessions: sessions,
					Fresh:          true,
					FreshnessClass: RegimeFreshnessFresh,
				})
				if got == nil || !got.Eligible {
					t.Errorf("%s depth=%v sessions=%d: want eligible, got %+v", tc.indicator, depth, sessions, got)
				}
			}
		}
	}
}

// TestRegimeGammaDepth pins gamma's three red-producing paths: gap crossing,
// wholly-short no-crossing profile (fast-path by construction), and the
// combined-scope per-index average.
func TestRegimeGammaDepth(t *testing.T) {
	t.Parallel()
	if d := RegimeGammaDepth(&GammaZeroComputed{GapPct: new(-1.4)}); d == nil || *d != 1.4 {
		t.Fatalf("gap path depth = %v, want 1.4", d)
	}
	if d := RegimeGammaDepth(&GammaZeroComputed{GammaSign: "negative"}); d == nil || *d < 2.0 {
		t.Fatalf("wholly-short depth = %v, want fast-path level", d)
	}
	combined := &GammaZeroComputed{
		Scope: GammaZeroScopeCombined,
		PerIndex: map[string]*GammaZeroComputed{
			"SPY": {GapPct: new(-1.0)},
			"SPX": {GapPct: new(-3.0)},
		},
	}
	if d := RegimeGammaDepth(combined); d == nil || *d != 2.0 {
		t.Fatalf("combined depth = %v, want per-index average 2.0", d)
	}
	if d := RegimeGammaDepth(&GammaZeroComputed{GammaSign: "positive"}); d != nil {
		t.Fatalf("long-gamma no-crossing depth = %v, want nil", d)
	}
}

// TestRegimeSeverityGovernorProvenanceGate pins the Q4 policy: heuristic
// (pending_backtest) confirmation without a tape co-sign reads one severity
// rung down, with the cap disclosed in governors; a tape co-sign lifts it.
func TestRegimeSeverityGovernorProvenanceGate(t *testing.T) {
	t.Parallel()
	pending := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:        "red",
			Eligibility: &RegimeEligibility{Eligible: true},
			Thresholds:  &RegimeThresholds{Label: "test_v1", Heuristic: true, PendingBacktest: true},
		}
	}
	calm := 0.3
	base := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &calm},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: pending()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: pending()},
	}
	got := buildRegimeLifecycleFixture(&base)
	if got.Stage != LifecycleConfirmedStress {
		t.Fatalf("stage = %q, want confirmed_stress (the evidence is real)", got.Stage)
	}
	if got.Severity != "watch" {
		t.Fatalf("severity = %q, want watch — heuristic confirmation without co-sign is capped", got.Severity)
	}
	if len(got.Governors) == 0 || got.Governors[0].Reason != "pending_backtest_no_tape_cosign" {
		t.Fatalf("governors = %+v, want disclosed pending_backtest cap", got.Governors)
	}

	cosigned := base
	drop := -1.8
	cosigned.HYGSPYDivergence.SPYChangePct = &drop
	got = buildRegimeLifecycleFixture(&cosigned)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" {
		t.Fatalf("co-signed = %q/%q, want confirmed_stress/act", got.Stage, got.Severity)
	}
	if len(got.Governors) != 0 {
		t.Fatalf("governors = %+v, want none with tape co-sign", got.Governors)
	}

	promoted := base
	promoted.FundingStress.Thresholds = &RegimeThresholds{Label: "test_v2", Heuristic: true}
	promoted.Breadth.Thresholds = &RegimeThresholds{Label: "test_v2", Heuristic: true}
	got = buildRegimeLifecycleFixture(&promoted)
	if got.Severity != "act" || len(got.Governors) != 0 {
		t.Fatalf("promoted sets = %q %+v, want act with no governor", got.Severity, got.Governors)
	}
}

// TestRegimeSeverityGovernorPanicLadder pins the monotone ladder: heuristic-
// only 3 eligible reds read act (urgent withheld, disclosed); pure-tape panic
// stays urgent regardless of provenance.
func TestRegimeSeverityGovernorPanicLadder(t *testing.T) {
	t.Parallel()
	pending := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:        "red",
			Eligibility: &RegimeEligibility{Eligible: true},
			Thresholds:  &RegimeThresholds{Label: "test_v1", Heuristic: true, PendingBacktest: true},
		}
	}
	calm := 0.2
	threeReds := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &calm},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: pending()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: pending()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: pending()},
	}
	got := buildRegimeLifecycleFixture(&threeReds)
	if got.Stage != LifecyclePanic || got.Severity != "act" {
		t.Fatalf("heuristic 3-red panic = %q/%q, want panic/act (urgent withheld)", got.Stage, got.Severity)
	}
	if len(got.Governors) == 0 || got.Governors[0].From != "urgent" || got.Governors[0].To != "act" {
		t.Fatalf("governors = %+v, want urgent→act disclosure", got.Governors)
	}

	crash := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: new(-7.2)},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
	}
	got = buildRegimeLifecycleFixture(&crash)
	if got.Stage != LifecyclePanic || got.Severity != "urgent" {
		t.Fatalf("gap crash = %q/%q, want panic/urgent (pure tape, never governed)", got.Stage, got.Severity)
	}
}

// TestRegimeSeverityGovernorQualityCap pins the evidence-keyed readiness cap:
// a confirming cluster with impaired source health caps severity at watch;
// an impaired UNRELATED cluster does not.
func TestRegimeSeverityGovernorQualityCap(t *testing.T) {
	t.Parallel()
	eligible := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{Band: "red", Eligibility: &RegimeEligibility{Eligible: true}}
	}
	base := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligible()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: eligible()},
		SourceHealth: []SourceHealth{
			{Source: "funding", Status: SourceStatusPartial},
		},
	}
	got := buildRegimeLifecycleFixture(&base)
	if got.Stage != LifecycleDataQuality || got.Severity != "watch" || got.Readiness != "blocked" {
		t.Fatalf("state = %q/%q/%q, want data_quality/watch/blocked when confirmation depends on an impaired cluster", got.Stage, got.Severity, got.Readiness)
	}

	unrelated := base
	unrelated.SourceHealth = []SourceHealth{{Source: "fx", Status: SourceStatusStale}}
	got = buildRegimeLifecycleFixture(&unrelated)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" || got.Readiness != "degraded" {
		t.Fatalf("state = %q/%q/%q, want confirmed_stress/act/degraded — unrelated impairment must not mute current independent stress", got.Stage, got.Severity, got.Readiness)
	}
}

// TestRegimeLifecycleCrashSensitivity pins the design's crash-day claims:
// an August-2024-style carry unwind confirms day one through fast paths and
// tape co-signs despite the persistence gates.
func TestRegimeLifecycleCrashSensitivity(t *testing.T) {
	t.Parallel()
	pendingEligible := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:        "red",
			Eligibility: &RegimeEligibility{Eligible: true},
			Thresholds:  &RegimeThresholds{Heuristic: true, PendingBacktest: true},
		}
	}
	spy := -3.0
	vix := 45.0
	ratio := 1.06
	carry := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: pendingEligible(), Ratio: &ratio, VIXChangePct: &vix, Status: RegimeStatusOK},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &spy},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: pendingEligible()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
	}
	got := buildRegimeLifecycleFixture(&carry)
	if got.Stage != LifecycleConfirmedStress && got.Stage != LifecyclePanic {
		t.Fatalf("carry unwind stage = %q, want confirmed_stress or panic on day one", got.Stage)
	}
	if got.Severity != "act" && got.Severity != "urgent" {
		t.Fatalf("carry unwind severity = %q, want act+ (tape co-sign present)", got.Severity)
	}
}

// TestLifecycleFingerprintStability pins the v2 projection: governor cluster
// lists and continuous values stay out, so an age-only change does not
// re-key alert dedupe, while an eligibility flip does.
func TestLifecycleFingerprintStability(t *testing.T) {
	t.Parallel()
	a := LifecycleState{
		Stage: LifecycleConfirmedStress, Severity: "watch",
		Governors: []GovernorAction{{Action: "severity_capped", From: "act", To: "watch", Reason: "pending_backtest_no_tape_cosign", Clusters: []string{"credit", "gamma"}}},
	}
	b := a
	b.Governors = []GovernorAction{{Action: "severity_capped", From: "act", To: "watch", Reason: "pending_backtest_no_tape_cosign", Clusters: []string{"funding"}}}
	if BuildLifecycleFingerprint(a) != BuildLifecycleFingerprint(b) {
		t.Fatal("governor cluster membership must not re-key the lifecycle fingerprint")
	}
	c := a
	c.Governors = nil
	if BuildLifecycleFingerprint(a) == BuildLifecycleFingerprint(c) {
		t.Fatal("governor presence is semantic and must re-key the fingerprint")
	}
	if BuildLifecycleFingerprint(a).Version != "lifecycle-fp-v2" {
		t.Fatalf("fingerprint version = %q, want lifecycle-fp-v2", BuildLifecycleFingerprint(a).Version)
	}
}

// TestCompactRegimeMonitorCarriesEligibilityWithoutTickingFields pins the
// SSE-hash contract: monitor rows expose eligibility + freshness class, and
// marshalling the same snapshot twice yields identical bytes.
func TestCompactRegimeMonitorCarriesEligibilityWithoutTickingFields(t *testing.T) {
	t.Parallel()
	mk := func() RegimeSnapshotResult {
		return RegimeSnapshotResult{
			FundingStress: RegimeFundingStress{
				RegimeIndicatorMeta: RegimeIndicatorMeta{
					Band:        "red",
					Eligibility: &RegimeEligibility{Eligible: false, Reasons: []string{"data_overdue"}},
					Freshness:   &RegimeFreshness{Class: RegimeFreshnessOverdue, MaxAgeSeconds: 604800},
				},
				Status: RegimeStatusStale,
			},
		}
	}
	one := mk()
	two := mk()
	a, err := json.Marshal(CompactRegimeMonitor(&one))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(CompactRegimeMonitor(&two))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("monitor view must be byte-stable for semantically identical snapshots")
	}
	var decoded RegimeMonitorResult
	if err := json.Unmarshal(a, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, ind := range decoded.Indicators {
		if ind.Name != "Funding" {
			continue
		}
		if ind.Eligibility == nil || ind.Eligibility.Eligible {
			t.Fatalf("monitor funding eligibility = %+v, want provisional", ind.Eligibility)
		}
		if ind.FreshnessClass != RegimeFreshnessOverdue {
			t.Fatalf("monitor funding freshness = %q, want overdue", ind.FreshnessClass)
		}
		return
	}
	t.Fatal("funding indicator missing from monitor view")
}

// TestGammaRegimeFromGap pins the ±GammaTransitionGapPct band edges: the
// band is closed (±2.0 stays transitional), only strict exceedance claims
// direction, and a nil gap never does.
func TestGammaRegimeFromGap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		gap  *float64
		want string
	}{
		{name: "nil gap is transition", gap: nil, want: "transition_gamma"},
		{name: "above band is long", gap: new(2.01), want: "long_gamma"},
		{name: "upper edge stays transition", gap: new(2.0), want: "transition_gamma"},
		{name: "flat is transition", gap: new(0.0), want: "transition_gamma"},
		{name: "lower edge stays transition", gap: new(-2.0), want: "transition_gamma"},
		{name: "below band is short", gap: new(-2.01), want: "short_gamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GammaRegimeFromGap(tc.gap); got != tc.want {
				t.Fatalf("GammaRegimeFromGap(%v) = %q, want %q", tc.gap, got, tc.want)
			}
		})
	}
}
