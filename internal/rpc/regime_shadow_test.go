package rpc

import (
	"math"
	"testing"
)

func shadowRow(indicator string, depth float64, sessions int) RegimeShadowIndicatorInput {
	return RegimeShadowIndicatorInput{
		Indicator: indicator, Depth: &depth, StressSessions: sessions,
		FreshnessClass: RegimeFreshnessFresh, Rankable: true,
	}
}

// TestRegimeShadowShallowRedCannotConfirm is the whole point of the model.
//
// On 2026-06-12 HYG closed 0.07% below its 50-day average — a red band by one
// side of a line — and the shipped machine of the day read confirmed stress.
// Here the same reading has to die on arithmetic, with no depth floor, no
// persistence gate and no eligibility bit involved.
func TestRegimeShadowShallowRedCannotConfirm(t *testing.T) {
	t.Parallel()
	got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
		shadowRow(RegimeIndicatorHYGSPY, 0.07, 1),
		shadowRow(RegimeIndicatorVIXTerm, 0.80, 1),
		shadowRow(RegimeIndicatorBreadth, -20, 1),
	}, 0, 3)
	if got.Confirming != 0 {
		t.Fatalf("confirming arm = %v, want 0 for a 7bp break", got.Confirming)
	}
	if got.Stage == LifecycleConfirmedStress || got.Stage == LifecyclePanic {
		t.Fatalf("stage = %q, want no stress stage", got.Stage)
	}
	// A 3% break on the same indicator must, by contrast, reach the arm —
	// otherwise the ramp is not discriminating, it is just silent.
	deep := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
		shadowRow(RegimeIndicatorHYGSPY, 3.0, 1),
		shadowRow(RegimeIndicatorVIXTerm, 0.80, 1),
		shadowRow(RegimeIndicatorBreadth, -20, 1),
	}, 0, 3)
	if deep.Confirming <= 0 {
		t.Fatalf("confirming arm = %v for a 3%% break, want > 0", deep.Confirming)
	}
}

// TestRegimeShadowSubRedCannotAccumulate pins the confirming arm's transform.
// Every cluster just under its red boundary must contribute exactly zero, no
// matter how many there are — the defect that sank the two earlier models was
// mild evidence summing its way to a confirmation.
func TestRegimeShadowSubRedCannotAccumulate(t *testing.T) {
	t.Parallel()
	var rows []RegimeShadowIndicatorInput
	for ind, ramp := range regimeShadowRampAnchors {
		// Just inside the red boundary, on the calm side.
		rows = append(rows, shadowRow(ind, ramp.red-1e-9, 9))
	}
	got := EvaluateRegimeShadow(rows, 0, 6)
	if got.Confirming != 0 {
		t.Fatalf("confirming arm = %v with every cluster below red, want 0", got.Confirming)
	}
	if got.Warning <= 0 {
		t.Fatalf("warning arm = %v, want > 0 — sub-red evidence must still warn", got.Warning)
	}
	if got.Stage == LifecycleConfirmedStress || got.Stage == LifecyclePanic {
		t.Fatalf("stage = %q on a board with nothing red", got.Stage)
	}
}

// TestRegimeShadowMonotoneUnderWorsening is the property the band machine does
// not have: no single indicator getting worse may lower either arm. The bug
// fixed in 5788510 was exactly this failure in the shipped tally.
func TestRegimeShadowMonotoneUnderWorsening(t *testing.T) {
	t.Parallel()
	for ind, ramp := range regimeShadowRampAnchors {
		sat := regimeGates[ind].FastDepth
		lo, hi := ramp.green-2, math.Max(sat, ramp.red)+2
		for _, sessions := range []int{1, 2, 5} {
			// Swept at every tape level too: the tape multiplies the confirming
			// arm, so a monotonicity that only holds on a flat tape is not the
			// property this test claims.
			for _, tape := range []float64{0, 1, 2, 3} {
				prevC, prevW := -1.0, -1.0
				for i := range 400 {
					depth := lo + (hi-lo)*float64(i)/399
					got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
						shadowRow(ind, depth, sessions),
					}, tape, 3)
					if got.Confirming < prevC-1e-9 || got.Warning < prevW-1e-9 {
						t.Fatalf("%s at depth %g sessions %d tape %g: arms fell (%v,%v) -> (%v,%v)",
							ind, depth, sessions, tape, prevW, prevC, got.Warning, got.Confirming)
					}
					prevC, prevW = got.Confirming, got.Warning
				}
			}
		}
	}
}

// TestRegimeShadowTapeCannotConfirmAlone is the property that decided where the
// tape arm sits. A day's price is the event the regime is supposed to
// anticipate, not evidence that one exists, so no tape reading may reach a
// stress stage over a board carrying nothing above red — including the −7%
// print the shipped machine panics on unconditionally.
func TestRegimeShadowTapeCannotConfirmAlone(t *testing.T) {
	t.Parallel()
	var green []RegimeShadowIndicatorInput
	for ind, ramp := range regimeShadowRampAnchors {
		green = append(green, shadowRow(ind, ramp.green, 0))
	}
	for _, tape := range []float64{0, 0.5, 1, 2, 3} {
		got := EvaluateRegimeShadow(green, tape, 6)
		if got.Confirming != 0 {
			t.Errorf("tape %g over a green board: confirming = %v, want 0", tape, got.Confirming)
		}
		if got.Stage == LifecycleConfirmedStress || got.Stage == LifecyclePanic {
			t.Errorf("tape %g over a green board: stage = %q", tape, got.Stage)
		}
	}
	// It must still warn, or the model reads a crash as calm.
	if got := EvaluateRegimeShadow(green, 3, 6); got.Stage != LifecycleEarlyWarning {
		t.Errorf("a −7%% tape over a green board: stage = %q, want %q", got.Stage, LifecycleEarlyWarning)
	}
}

// TestRegimeShadowTapeCannotLiftABoundaryRed is 2026-06-12 with a crash on top.
// A cluster sitting exactly on its red boundary carries zero depth above red,
// and multiplying zero is what stops the tape rescuing it — the failure mode of
// every additive placement, and of a placement gated on "at least one red".
func TestRegimeShadowTapeCannotLiftABoundaryRed(t *testing.T) {
	t.Parallel()
	rows := []RegimeShadowIndicatorInput{
		shadowRow(RegimeIndicatorHYGSPY, regimeShadowRampAnchors[RegimeIndicatorHYGSPY].red, 9),
		shadowRow(RegimeIndicatorCredit, regimeShadowRampAnchors[RegimeIndicatorCredit].red, 9),
		shadowRow(RegimeIndicatorFunding, regimeShadowRampAnchors[RegimeIndicatorFunding].red, 9),
	}
	for _, tape := range []float64{1, 2, 3} {
		got := EvaluateRegimeShadow(rows, tape, 3)
		if got.Confirming != 0 {
			t.Errorf("tape %g over boundary reds: confirming = %v, want 0", tape, got.Confirming)
		}
		if got.Stage == LifecycleConfirmedStress || got.Stage == LifecyclePanic {
			t.Errorf("tape %g over boundary reds: stage = %q", tape, got.Stage)
		}
	}
}

// TestRegimeShadowTapeMultiplies pins the arm's shape. The multiplier is
// 1 + tape, read off the shipped machine's own relaxation of its required red
// count (2 reds → 1 at −2.5%, 3 reds → 1 at −4%), so it carries no constant of
// its own. A change to the factor is a change to that derivation.
func TestRegimeShadowTapeMultiplies(t *testing.T) {
	t.Parallel()
	sat := regimeGates[RegimeIndicatorFunding].FastDepth
	rows := []RegimeShadowIndicatorInput{shadowRow(RegimeIndicatorFunding, sat, 3)}
	flat := EvaluateRegimeShadow(rows, 0, 3)
	if flat.Confirming <= 0 {
		t.Fatalf("a saturated cluster scores %v on a flat tape", flat.Confirming)
	}
	for _, tape := range []float64{0.5, 1, 2, 3} {
		got := EvaluateRegimeShadow(rows, tape, 3)
		if want := flat.Confirming * (1 + tape); math.Abs(got.Confirming-want) > 1e-9 {
			t.Errorf("tape %g: confirming = %v, want %v", tape, got.Confirming, want)
		}
		if want := flat.Warning + tape; math.Abs(got.Warning-want) > 1e-9 {
			t.Errorf("tape %g: warning = %v, want %v", tape, got.Warning, want)
		}
	}
}

// shadowTapeSnapshot is the minimum snapshot on which a SPY print is current
// enough to carry weight: both credit rows fresh and the cluster's source
// contract agreeing.
func shadowTapeSnapshot(spy float64) RegimeSnapshotResult {
	fresh := &RegimeFreshness{Class: RegimeFreshnessFresh}
	r := RegimeSnapshotResult{}
	r.HYGSPYDivergence.Status = RegimeStatusOK
	r.HYGSPYDivergence.Freshness = fresh
	r.HYGSPYDivergence.SPYChangePct = &spy
	r.CreditSpreads.Status = RegimeStatusOK
	r.CreditSpreads.Freshness = fresh
	r.SourceHealth = []SourceHealth{{
		Source: "credit", Status: SourceStatusOK,
		MaxAgeSeconds: RegimeSourceMaxAgeSeconds("credit"),
		RefreshState:  SourceRefreshCurrent,
	}}
	return r
}

// TestRegimeShadowTapeAnchors pins the tape term at the anchors the
// multiplier's derivation reads: 1 at −2.5% and 2 at −4% are the two points
// where the shipped machine states its own relaxation.
func TestRegimeShadowTapeAnchors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spy, want float64
	}{{1.0, 0}, {-1.5, 0}, {-2.5, 1}, {-4.0, 2}, {-7.0, 3}, {-12.0, 3}} {
		if got := RegimeShadowTape(shadowTapeSnapshot(tc.spy)); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("SPY %g%%: tape = %v, want %v", tc.spy, got, tc.want)
		}
	}
}

// TestRegimeShadowTapeNeedsACurrentPrint pins the gate that had to come with
// the arm. The term multiplies confirmation now, so a print that cannot
// confirm must score zero rather than amplify on frozen or overdue evidence.
func TestRegimeShadowTapeNeedsACurrentPrint(t *testing.T) {
	t.Parallel()
	closed := shadowTapeSnapshot(-7.0)
	closed.TapeSessionState = TapeSessionClosedDate
	if got := RegimeShadowTape(closed); got != 0 {
		t.Errorf("a frozen closed-date print scored %v, want 0", got)
	}
	stale := shadowTapeSnapshot(-7.0)
	stale.HYGSPYDivergence.Freshness = &RegimeFreshness{Class: RegimeFreshnessStale}
	if got := RegimeShadowTape(stale); got != 0 {
		t.Errorf("a stale credit cluster scored %v, want 0", got)
	}
	blind := shadowTapeSnapshot(-7.0)
	blind.SourceHealth = nil
	if got := RegimeShadowTape(blind); got != 0 {
		t.Errorf("a cluster with no source-health row scored %v, want 0", got)
	}
}

// TestRegimeShadowMonotoneInPersistence pins that time never subtracts. A
// longer stress run may only raise the score, which is what lets the counter
// span band changes without reintroducing a cliff.
func TestRegimeShadowMonotoneInPersistence(t *testing.T) {
	t.Parallel()
	for ind, ramp := range regimeShadowRampAnchors {
		prev := -1.0
		for sessions := range 8 {
			got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
				shadowRow(ind, ramp.red+0.01, sessions),
			}, 0, 3)
			if got.Warning < prev-1e-9 {
				t.Fatalf("%s: warning fell from %v to %v at %d sessions", ind, prev, got.Warning, sessions)
			}
			prev = got.Warning
		}
	}
}

// TestRegimeShadowCurrencyBlocksConfirmation pins the one row gate the ramp
// does not subsume. Stale evidence may band and may be visible; it may never
// carry weight.
func TestRegimeShadowCurrencyBlocksConfirmation(t *testing.T) {
	t.Parallel()
	stale := shadowRow(RegimeIndicatorCredit, 12.0, 9)
	stale.FreshnessClass = RegimeFreshnessStale
	got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{stale}, 0, 3)
	if got.Confirming != 0 || got.Warning != 0 {
		t.Fatalf("arms = (%v,%v) on stale evidence, want zero", got.Warning, got.Confirming)
	}
	if got.Indicators[RegimeIndicatorCredit].Zeroed != "currency" {
		t.Fatalf("zeroed = %q, want %q", got.Indicators[RegimeIndicatorCredit].Zeroed, "currency")
	}
	if got.Indicators[RegimeIndicatorCredit].Strength <= 0 {
		t.Fatal("a stale row should still measure a strength; it just cannot carry weight")
	}
}

// TestRegimeShadowRampAnchors pins that every indicator scores 0 at its
// green/yellow boundary, 0.5 at its yellow/red boundary and 1 at saturation.
// The anchors themselves are checked against the compiled classifiers by
// bisection in the analysis harness; this only pins the ramp's shape.
func TestRegimeShadowRampAnchors(t *testing.T) {
	t.Parallel()
	for ind := range regimeShadowRampAnchors {
		green, red, sat, ok := RegimeShadowRampFor(ind)
		if !ok {
			t.Fatalf("%s: no ramp", ind)
		}
		if sat <= red {
			t.Fatalf("%s: saturation %v is not above the red boundary %v", ind, sat, red)
		}
		for _, tc := range []struct {
			depth, want float64
		}{{green, 0}, {red, 0.5}, {sat, 1}} {
			if got := regimeShadowStrength(ind, tc.depth, nil); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("%s strength at %v = %v, want %v", ind, tc.depth, got, tc.want)
			}
		}
	}
}

// TestRegimeShadowVerdictFloor pins that too few ranked clusters reads as a
// data-quality problem rather than as calm, exactly as the shipped machine
// does.
func TestRegimeShadowVerdictFloor(t *testing.T) {
	t.Parallel()
	got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
		shadowRow(RegimeIndicatorCredit, 12.0, 9),
	}, 0, RegimeVerdictFloor-1)
	if got.Stage != LifecycleDataQuality {
		t.Fatalf("stage = %q below the verdict floor, want %q", got.Stage, LifecycleDataQuality)
	}
}

// TestRegimeShadowCreditScoresBothAxes pins the fix for a real silence in the
// model. Credit bands red on an OAS level >= 5.5 OR a 20-day widening >=
// 1.00pp. Scoring the level alone put a widening-driven red — OAS near 4.2,
// which is barely off the green boundary — at a strength of about 0.07, so a
// credit event arriving as fast repricing from a low base contributed nothing.
func TestRegimeShadowCreditScoresBothAxes(t *testing.T) {
	t.Parallel()
	levelOnly := 4.2
	widening := 1.2

	quiet := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
		shadowRow(RegimeIndicatorCredit, levelOnly, 3),
	}, 0, 3)
	if got := quiet.Indicators[RegimeIndicatorCredit].Strength; got > 0.2 {
		t.Fatalf("level %v alone scores %v, want a low strength", levelOnly, got)
	}

	row := shadowRow(RegimeIndicatorCredit, levelOnly, 3)
	row.SecondaryDepth = &widening
	repricing := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{row}, 0, 3)
	if got := repricing.Indicators[RegimeIndicatorCredit].Strength; got <= 0.5 {
		t.Fatalf("a %vpp 20-day widening scores %v, want above the red anchor 0.5", widening, got)
	}
	if repricing.Warning <= quiet.Warning {
		t.Error("the widening axis did not raise the warning arm")
	}
	// Worst-of, not sum: a benign second axis must never lower the score.
	benign := 0.0
	row.SecondaryDepth = &benign
	calm := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{row}, 0, 3)
	if calm.Indicators[RegimeIndicatorCredit].Strength != quiet.Indicators[RegimeIndicatorCredit].Strength {
		t.Error("a calm second axis changed the score; the axes must combine worst-of")
	}
}

// TestRegimeShadowSecondaryAxisMonotone pins that the second axis cannot break
// the property the whole model rests on.
func TestRegimeShadowSecondaryAxisMonotone(t *testing.T) {
	t.Parallel()
	for ind, ramp := range regimeShadowSecondaryRamps {
		prev := -1.0
		for i := range 300 {
			v := ramp.green - 1 + (ramp.saturation-ramp.green+2)*float64(i)/299
			row := shadowRow(ind, -1e9, 3)
			row.SecondaryDepth = &v
			got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{row}, 0, 3)
			if got.Warning < prev-1e-9 {
				t.Fatalf("%s secondary axis at %v: warning fell %v -> %v", ind, v, prev, got.Warning)
			}
			prev = got.Warning
		}
	}
}
