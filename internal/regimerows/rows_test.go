package regimerows

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestRowsPreserveServedBandsAndUnknownCoverage(t *testing.T) {
	r := rpc.RegimeBreadth{Status: rpc.RegimeStatusOK, Envelope: rpc.BreadthSPXResult{PctAbove50DMA: 55}}
	if got := Breadth(time.Now(), r); got.Band != BandYellow || strings.Contains(got.Value, "0% above 200d") {
		t.Fatalf("threshold/coverage drift: %+v", got)
	}
	r.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{Band: "red", BandReason: "held red until exit condition", Thresholds: rpc.RegimeThresholdsFor(rpc.RegimeIndicatorBreadth)}
	r.Envelope.PctAbove50DMA = 60
	if got := Breadth(time.Now(), r); got.Band != BandRed || got.Reason != r.BandReason {
		t.Fatalf("discarded daemon hysteresis: %+v", got)
	}
	r.Band = ""
	if got := Breadth(time.Now(), r); got.Band != BandUnranked {
		t.Fatal("reconstructed rank from explicitly unranked source")
	}
}

// A VVIX row with no served band is banded by the renderer's own copy of the
// daemon's rule: red from 120.
func TestVolOfVolRawBandFollowsTheRedLine(t *testing.T) {
	row := func(last, rise float64) Row {
		return VolOfVol(time.Now(), rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Last: &last, Change5D: &rise})
	}
	if got := row(115, 0); got.Band != BandGreen {
		t.Errorf("VVIX 115, not rising: band %v, want green", got.Band)
	}
	if got := row(120, 0); got.Band != BandRed {
		t.Errorf("VVIX 120: band %v, want red", got.Band)
	}
}
