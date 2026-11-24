package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestMarketReadToolPreservesTheRead(t *testing.T) {
	want := rpc.MarketReadResult{Session: rpc.MarketReadSession{Date: "2026-09-23", State: rpc.MarketReadOpen}, Thresholds: rpc.MarketReadThresholds{Heuristic: true, VIXLine: 25},
		Index:          &rpc.MarketReadIndex{Symbol: "SPX", Bars: []rpc.MarketReadBar{}, PriorUnavailable: "synthetic"},
		VIXUnavailable: "synthetic",
		Tags:           []rpc.MarketReadTag{{Code: "inside_prior_range", Scope: "index", Tone: "neutral", Evidence: map[string]float64{"close": 101}}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodMarketRead: want})
	tool, ok := lookupTool("canary_market_read")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only tool")
	}
	if _, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"symbol":"SPY"}`)); err == nil {
		t.Fatal("caller-supplied symbol accepted")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.MarketReadResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(<-calls, []string{rpc.MethodMarketRead}) {
		t.Fatal("adapter changed the read or called another surface")
	}
}
