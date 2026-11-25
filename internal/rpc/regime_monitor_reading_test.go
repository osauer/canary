package rpc

import (
	"strings"
	"testing"
)

// A monitor prints the breadth reading under the signal's name. It keeps
// every figure, the three coverage counts included, and names the index's
// member count once.
func TestRegimeMonitorBreadthReadingNamesTheMemberCountOnce(t *testing.T) {
	t.Parallel()
	r := &RegimeSnapshotResult{Breadth: RegimeBreadth{
		Status: RegimeStatusOK, PctAbove50DMA: 54.98, PctAbove200DMA: new(53.20), NetNewHighsPct: new(-12.60),
		Envelope: BreadthSPXResult{MemberCount: 503, Coverage50: 502, Coverage200: 499, CoverageHighsLows: 498},
	}}
	var got string
	for _, row := range CompactRegimeMonitor(r).Indicators {
		if row.Name == "Breadth" {
			got = row.Reading
		}
	}
	for _, figure := range []string{"54.98", "53.20", "-12.60", "502", "499", "498"} {
		if !strings.Contains(got, figure) {
			t.Errorf("reading %q lacks %s", got, figure)
		}
	}
	if n := strings.Count(got, "503"); n != 1 {
		t.Errorf("reading names the member count %d times, want once: %q", n, got)
	}
	if n := len([]rune(got)); n > 80 {
		t.Errorf("reading is %d characters, want at most 80: %q", n, got)
	}
}
