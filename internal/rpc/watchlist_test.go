package rpc

import (
	"math"
	"testing"
)

func TestWatchlistContractBounds(t *testing.T) {
	for _, bad := range []WatchlistContract{{Symbol: "$SYNTH"}, {Symbol: "SYN/TH"}, {Symbol: "SYNTH\nOTHER"}, {Symbol: "SYNTH", ConID: -1}, {Symbol: "SYNTH", ConID: math.MaxInt32 + 1}, {Symbol: "SYNTH", SecType: "OPT"}, {Symbol: "SYNTH", Currency: "EUR"}, {Symbol: "SYNTH", Exchange: "NYSE"}} {
		if _, err := NormalizeWatchlistContract(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	for _, bad := range [][]WatchlistContract{nil, {{Symbol: "SYNTH"}, {Symbol: "synth"}}, {{Symbol: "SYNTH", ConID: 17}, {Symbol: "OTHER", ConID: 17}}, make([]WatchlistContract, 21)} {
		if _, err := NormalizeWatchlistSymbols(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if out, err := NormalizeWatchlistSymbols([]WatchlistContract{{Symbol: " synth "}, {Symbol: "other"}}); err != nil || out[0].Symbol != "SYNTH" || out[0].ConID != 0 {
		t.Fatal(out, err)
	}
	for _, rev := range []int64{-1, math.MaxInt64} {
		if ValidateWatchlistMutation(rev, "id") == nil {
			t.Fatal("revision overflow")
		}
	}
	for _, id := range []string{"", "bad id", "\n"} {
		if ValidateWatchlistMutation(0, id) == nil {
			t.Fatal("invalid receipt ID")
		}
	}
}
