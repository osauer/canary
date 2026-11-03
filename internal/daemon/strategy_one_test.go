package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/breadth/spx"
)

// A constituent's undated closes are laid on the sessions ending at its last
// bar, and kept only where its dated bars agree.
func TestStrategyOneDatesClosesAgainstTheBars(t *testing.T) {
	sessions := []string{"2026-10-26", "2026-10-27", "2026-10-28", "2026-10-29", "2026-10-30", "2026-11-02"}
	vol := int64(1000)
	w := spx.ConstituentWindow{Symbol: "SYN", Closes: []float64{10, 11, 12, 13}, LastBarAt: "2026-10-30",
		Bars: []spx.Bar{{Date: "2026-10-29", Close: 12, Volume: &vol}, {Date: "2026-10-30", Close: 13, Volume: &vol}}}
	s, ok := strategyOneSeries(w, sessions, "2026-11-02")
	if !ok || s.Dates[0] != "2026-10-27" || s.Dates[3] != "2026-10-30" || s.Volumes[3] != 1000 || s.Volumes[0] != 0 {
		t.Fatalf("series %+v ok %v", s, ok)
	}
	// A dated bar that disagrees with the close laid on its day: not dated.
	w.Bars[0].Close = 11.5
	if _, ok := strategyOneSeries(w, sessions, "2026-11-02"); ok {
		t.Fatal("a window whose bars disagree with its closes was dated")
	}
	// A last bar off the calendar, or more closes than sessions: not dated.
	w.Bars[0].Close, w.LastBarAt = 12, "2026-10-31"
	if _, ok := strategyOneSeries(w, sessions, "2026-11-02"); ok {
		t.Fatal("a window ending on a non-session was dated")
	}
	// Closes after the latest settled session are dropped.
	w.LastBarAt = "2026-10-30"
	if s, ok := strategyOneSeries(w, sessions, "2026-10-29"); !ok || len(s.Closes) != 3 || s.Dates[2] != "2026-10-29" {
		t.Fatalf("series %+v", s)
	}
}
