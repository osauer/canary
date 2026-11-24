package daemon

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A put on a hedge symbol is index protection for the whole long book (owner,
// 2026-11-24 07:00 CET: "buying SPY puts" from Add, holding no SPY shares).
// Add sizes it under rule 12's over-hedge line, the line the hedge trim sells
// back from, after the held and working puts; a book with no long exposure
// or already past the line has no room; an unmeasured put leaves the line
// unmeasured, never zero room.
func TestIndexPutRoomUnderTheOverHedgeLine(t *testing.T) {
	engine := &proposalEngine{server: &Server{}}
	need := func(long float64, working int, edit func(*rpc.PositionsResult)) addIndexNeed {
		acct, pos := hedgeTrimBook(long)
		if edit != nil {
			edit(pos)
		}
		a, _, base := engine.hedgeTrimAssessment(context.Background(), acct, pos, mandateTestNow)
		return addIndexProtectionFrom(a, base, pos, "USD", working)
	}
	// A put of delta -0.25 on an index at 400, at 0.5 EUR a dollar, is short
	// 5,000 EUR a contract.
	delta, spot := new(-0.25), new(400.0)

	// 110,000 EUR of puts against 100,000 EUR long is 110%, inside the 140%
	// line: 30,000 EUR of room, 6 contracts, 4 after 2 working.
	n := need(100000, 0, nil)
	if !n.measured() || n.view.LongBase != 100000 || n.view.ProtectionBase != 110000 || n.view.LinePct != 140 || n.view.BandTopPct != 70 || math.Abs(n.view.RoomBase-30000) > 1e-6 {
		t.Fatalf("measurement: %+v", n.view)
	}
	if room, ok := n.room(delta, spot, 100); !ok || room != 6 {
		t.Fatalf("room %d (%t), want 6", room, ok)
	}
	if room, _ := need(100000, 2, nil).room(delta, spot, 100); room != 4 {
		t.Fatalf("room after 2 working puts: %d, want 4", room)
	}
	// Past the line, and with no long book left, there is no room.
	if n := need(50000, 0, nil); !n.measured() || n.view.RoomBase != 0 {
		t.Fatalf("past the line: %+v", n.view)
	}
	noLong := need(100000, 0, func(pos *rpc.PositionsResult) {
		pos.Options, pos.ByUnderlying = pos.Options[1:], pos.ByUnderlying[1:]
	})
	if !noLong.measured() || noLong.view.LongBase != 0 || noLong.view.RoomBase != 0 {
		t.Fatalf("no long book: %+v", noLong.view)
	}
	if err := addIndexPutFull(noLong.view); err == nil || !strings.Contains(err.Error(), "No long book") {
		t.Fatalf("no long book refusal: %v", err)
	}
	// A held put without its delta leaves the line unmeasured.
	unmeasured := need(100000, 0, func(pos *rpc.PositionsResult) {
		pos.Options[1].Delta, pos.ByUnderlying[1].Options[0].Delta = nil, nil
	})
	if unmeasured.measured() {
		t.Fatalf("an unmeasured put measured the line: %+v", unmeasured.view)
	}
	if _, ok := unmeasured.room(delta, spot, 100); ok {
		t.Fatal("an unmeasured line gave a room")
	}
	// A put without its own delta has no measured room either.
	if _, ok := n.room(nil, spot, 100); ok {
		t.Fatal("a put without a delta gave a room")
	}
}

// add market writes each row's room and reports the most any row may add;
// unmeasured, the room is the option cap with the reason, and a measured
// book with no room stays at zero whatever the rows read.
func TestIndexPutCoverOnTheMarketRows(t *testing.T) {
	engine := &proposalEngine{server: &Server{}}
	acct, pos := hedgeTrimBook(100000)
	a, _, base := engine.hedgeTrimAssessment(context.Background(), acct, pos, mandateTestNow)
	n := addIndexProtectionFrom(a, base, pos, "USD", 0)
	row := func(delta *float64) *rpc.AddCallCandidate {
		return &rpc.AddCallCandidate{Contract: &rpc.ContractParams{Multiplier: 100}, Quote: &rpc.SetupOptionQuote{Delta: delta}}
	}
	near, far, blind := row(new(-0.25)), row(new(-0.5)), row(nil)
	cover := addIndexPutCover(n, []*rpc.AddCallCandidate{near, far, blind}, new(400.0), 5)
	if cover.Kind != rpc.AddPutCoverIndexProtection || cover.RoomContracts != 6 || cover.IndexProtection == nil || cover.IndexProtection.Unmeasured != "" ||
		near.HedgeRoomContracts == nil || *near.HedgeRoomContracts != 6 || far.HedgeRoomContracts == nil || *far.HedgeRoomContracts != 3 || blind.HedgeRoomContracts != nil {
		t.Fatalf("cover %+v, rows %v %v %v", cover, near.HedgeRoomContracts, far.HedgeRoomContracts, blind.HedgeRoomContracts)
	}
	if cover := addIndexPutCover(n, []*rpc.AddCallCandidate{row(nil)}, new(400.0), 5); cover.RoomContracts != 5 || cover.IndexProtection.Unmeasured == "" {
		t.Fatalf("rows without deltas: %+v", cover)
	}
	unmeasured := addIndexNeed{view: rpc.AddIndexProtection{Unmeasured: "the book cannot be measured"}}
	if cover := addIndexPutCover(unmeasured, []*rpc.AddCallCandidate{row(new(-0.25))}, new(400.0), 5); cover.RoomContracts != 5 || cover.IndexProtection.Unmeasured == "" {
		t.Fatalf("unmeasured line: %+v", cover)
	}
	acct, pos = hedgeTrimBook(50000)
	a, _, base = engine.hedgeTrimAssessment(context.Background(), acct, pos, mandateTestNow)
	if cover := addIndexPutCover(addIndexProtectionFrom(a, base, pos, "USD", 0), []*rpc.AddCallCandidate{row(nil)}, new(400.0), 5); cover.RoomContracts != 0 || cover.IndexProtection.Unmeasured != "" {
		t.Fatalf("past the line: %+v", cover)
	}
}

// The plan's decision: measured room sizes the put; a measured book with no
// room refuses; a put whose delta cannot be read, or a line that cannot be
// measured, falls back to the option cap (-1: no room of its own) with the
// reason, and without a known cap nothing bounds the size, so it refuses.
func TestIndexPutPlanRoom(t *testing.T) {
	engine := &proposalEngine{server: &Server{}}
	need := func(long float64) addIndexNeed {
		acct, pos := hedgeTrimBook(long)
		a, _, base := engine.hedgeTrimAssessment(context.Background(), acct, pos, mandateTestNow)
		return addIndexProtectionFrom(a, base, pos, "USD", 0)
	}
	inside, past := need(100000), need(50000)
	unmeasured := addIndexNeed{view: rpc.AddIndexProtection{Unmeasured: "index protection cannot be measured against the book"}}
	delta, spot := new(-0.25), new(400.0)
	for _, tc := range []struct {
		name       string
		n          addIndexNeed
		delta      *float64
		cap, want  int
		refused    string
		unmeasured bool
	}{
		{"measured room", inside, delta, 5, 6, "", false},
		{"deep put fills the room", inside, new(-7.0), 5, 0, "over-hedge line", false},
		{"past the line", past, delta, 5, 0, "over-hedge line", false},
		{"put delta unread", inside, nil, 5, -1, "", true},
		{"line unmeasured", unmeasured, delta, 5, -1, "", true},
		{"line unmeasured, no cap", unmeasured, delta, 0, 0, "limit is unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			room, view, err := addIndexPutRoomFrom(tc.n, tc.delta, spot, 100, tc.cap)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("want a refusal saying %q, got room %d, %v", tc.refused, room, err)
				}
				return
			}
			if err != nil || room != tc.want || view == nil || (view.Unmeasured != "") != tc.unmeasured {
				t.Fatalf("room %d, view %+v, err %v", room, view, err)
			}
		})
	}
}
