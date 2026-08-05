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
	for ind, ramp := range regimeShadowRamps {
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
	for ind, ramp := range regimeShadowRamps {
		sat := regimeGates[ind].FastDepth
		lo, hi := ramp.green-2, math.Max(sat, ramp.red)+2
		for _, sessions := range []int{1, 2, 5} {
			prevC, prevW := -1.0, -1.0
			for i := range 400 {
				depth := lo + (hi-lo)*float64(i)/399
				got := EvaluateRegimeShadow([]RegimeShadowIndicatorInput{
					shadowRow(ind, depth, sessions),
				}, 0, 3)
				if got.Confirming < prevC-1e-9 || got.Warning < prevW-1e-9 {
					t.Fatalf("%s at depth %g sessions %d: arms fell (%v,%v) -> (%v,%v)",
						ind, depth, sessions, prevW, prevC, got.Warning, got.Confirming)
				}
				prevC, prevW = got.Confirming, got.Warning
			}
		}
	}
}

// TestRegimeShadowMonotoneInPersistence pins that time never subtracts. A
// longer stress run may only raise the score, which is what lets the counter
// span band changes without reintroducing a cliff.
func TestRegimeShadowMonotoneInPersistence(t *testing.T) {
	t.Parallel()
	for ind, ramp := range regimeShadowRamps {
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
	for ind := range regimeShadowRamps {
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
			if got := regimeShadowStrength(ind, tc.depth); math.Abs(got-tc.want) > 1e-9 {
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
