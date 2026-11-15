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
