package rpc

import "testing"

func TestNormalizeMarketTapeHistory(t *testing.T) {
	for _, p := range []MarketTapeParams{{}, {Sessions: 5, History: true}, {History: true, Before: "2026-09-21"}} {
		got, err := NormalizeMarketTapeParams(p)
		if err != nil || got.History != p.History || got.Before != p.Before || got.Sessions < 5 {
			t.Fatalf("normalization: %+v %v", got, err)
		}
	}
	for _, p := range []MarketTapeParams{{Sessions: 61}, {Before: "2026-09-21"}, {History: true, Before: "2026-02-30"}, {History: true, Before: "26-9-21"}} {
		if _, err := NormalizeMarketTapeParams(p); err == nil {
			t.Fatalf("unbounded or ambiguous request accepted: %+v", p)
		}
	}
}
