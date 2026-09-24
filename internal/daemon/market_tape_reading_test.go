package daemon

import (
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestTapeReadingObservedRetracementAndMissingEvidence(t *testing.T) {
	rows := []rpc.MarketTapeSession{
		{Date: "2026-09-21", SPX: &rpc.MarketTapePrice{Close: 102, ChangePct: new(2.0)}},
		{Date: "2026-09-22", SPX: &rpc.MarketTapePrice{Close: 102, ChangePct: new(0.0)}},
		{Date: "2026-09-23", SPX: &rpc.MarketTapePrice{Close: 101, ChangePct: new(-100.0 / 102)}, Breadth: &rpc.MarketTapeBreadth{PctAbove50DMA: new(30.0), Change50PP: new(-1.0), Coverage50: 90, MemberCount: 100}},
	}
	r := describeMarketTape(rows)
	if r.Headline != "S&P 500 fell; fewer stocks above average" || r.Rally == nil || r.Rally.Session != "2026-09-21" || math.Abs(r.Rally.GivebackPct-50) > 1e-8 {
		t.Fatalf("incorrect relationship: %+v", r)
	}
	for _, e := range r.Evidence {
		if e.Key == "advance_decline" && !strings.Contains(e.Value, "Not collected") {
			t.Fatal("missing daily breadth was called weak")
		}
	}
	rows[1].SPX = nil
	if describeMarketTape(rows).Rally != nil {
		t.Fatal("retracement bridged an unknown close")
	}
	rows[2].Breadth = nil
	if describeMarketTape(rows).Headline != "S&P 500 fell" {
		t.Fatal("missing breadth fabricated confirmation")
	}
}

func TestTapeBreadthChangeRequiresComparableCountsAndUniverse(t *testing.T) {
	a := &rpc.MarketTapeBreadth{PctAbove50DMA: new(30.0), MemberCount: 100, Coverage50: 90}
	b := *a
	b.Coverage50 = 89
	if comparableTapeBreadth(a, &b) {
		t.Fatal("changing denominator became daily participation change")
	}
	b.Coverage50 = 90
	a.Participation = &rpc.BreadthParticipation{MembershipID: strings.Repeat("a", 64)}
	b.Participation = &rpc.BreadthParticipation{MembershipID: strings.Repeat("b", 64)}
	if comparableTapeBreadth(a, &b) {
		t.Fatal("changed membership hidden")
	}
}
