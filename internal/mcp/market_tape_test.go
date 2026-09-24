package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestMarketTapeToolPreservesTypedEvidence(t *testing.T) {
	want := rpc.MarketTapeResult{SchemaVersion: "market-tape-v1", NotPredictive: true, HistoricalAvailability: "unknown", Sessions: []rpc.MarketTapeSession{{Date: "2026-09-23", QQQ: &rpc.MarketTapePrice{Close: 123, Volume: new(int64(0))}}}, Sources: []rpc.MarketTapeSource{{Key: "qqq", Status: "partial", MissingMetrics: map[string]int{"relative_volume_20": 1}}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodMarketTape: want})
	tool, ok := lookupTool("canary_market_tape")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only tool")
	}
	if _, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"sessions":61}`)); err == nil {
		t.Fatal("unbounded acquisition accepted")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"sessions":5}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.MarketTapeResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(<-calls, []string{rpc.MethodMarketTape}) {
		t.Fatal("adapter changed evidence or called another surface")
	}
}
