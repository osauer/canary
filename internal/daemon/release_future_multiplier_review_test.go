package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
)

func TestReviewHeldFutureMissingMultiplierUnknown(t *testing.T) {
	row := mandateReductionFuture(750001, -2, 6000)
	in := *mandateReductionFixture(t, mandateReductionBook(row), 0.5, 1000000, 0).Input
	row.Multiplier = positionViewMultiplier("FUT", 0)
	in.Legs, in.NotModelled, in.FXNow, in.Reasons = mandateLegsFromPositions(mandateReductionBook(row), "EUR", mandateTestNow)
	v := mandate.Evaluate(in)
	if v.Status != mandate.StatusUnknown {
		t.Fatalf("missing multiplier measured as %s: loss=%+v stresses=%+v", v.Status, v.Loss, v.Stresses)
	}
}

func TestReviewFutureDollarDeltaRequiresMultiplier(t *testing.T) {
	row := mandateReductionFuture(750001, -2, 6000)
	row.Multiplier = positionViewMultiplier("FUT", 0)
	if dd, ok := positionDollarDelta(row, false); ok {
		t.Fatalf("missing multiplier produced delta %g", dd)
	}
	if got := positionViewMultiplier("STK", 0); got != 1 {
		t.Fatalf("stock multiplier=%d", got)
	}
}

func TestReviewFutureMultiplierPreservesKnownTerms(t *testing.T) {
	for _, mult := range []int{5, 100} {
		row := mandateReductionFuture(750001, -2, 6000)
		row.Multiplier = positionViewMultiplier("FUT", mult)
		if dd, ok := positionDollarDelta(row, false); !ok || dd != -12000*float64(mult) {
			t.Fatalf("multiplier %d: delta=%g known=%v", mult, dd, ok)
		}
	}
}

func TestReviewPlanLossUnknownWithoutFutureEvidence(t *testing.T) {
	row := mandateReductionFuture(750001, -2, 6000)
	in := *mandateReductionFixture(t, mandateReductionBook(row), 0.5, 1000000, 0).Input
	delete(in.FXNow, "USD")
	score := planLossMeasureBetween(in, in)
	if score.LossMeasureUnknown == "" || score.LossMeasureBeforeBase != nil || score.LossMeasureAfterBase != nil {
		t.Fatalf("missing FX produced known plan score: %+v", score)
	}
}
