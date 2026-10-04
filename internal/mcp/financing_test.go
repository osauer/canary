package mcp

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/financing"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestLendingToolPreservesNetFeesAndReadOnlyBounds(t *testing.T) {
	raw, err := os.ReadFile("../flexstmt/testdata/lending-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	core := financing.Calculate(statements, "synthetic-paper", from, from.AddDate(0, 0, 2), "EUR")
	want := rpc.FinancingFeesResult{Summary: core.Summary, Fees: core.Fees, FilteredCount: len(core.Fees)}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodFinancingFees: want})
	tool, ok := lookupTool("canary_lending_fees")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only lending tool")
	}
	for _, args := range []string{`{"limit":101}`, `{"con_id":-1}`, `{"from":"2026-09-29"}`, `{"window":"all"}`} {
		if _, err := tool.Handler(t.Context(), conn, json.RawMessage(args)); err == nil {
			t.Fatal("unbounded lending read accepted")
		}
	}
	result, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.FinancingFeesResult
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(<-calls, []string{rpc.MethodFinancingFees}) {
		t.Fatal("MCP changed evidence or reached another method")
	}
}

func TestLendingRatesToolReadOnlyScopeAndUnavailable(t *testing.T) {
	want := rpc.MarketEventsResult{Symbols: []string{"AAA"}, BorrowFeeCoverage: []rpc.MarketEventBorrowFeeCoverage{{Symbol: "AAA", Status: rpc.BorrowFeeCoverageMissing}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodMarketEventsSnapshot: want})
	tool, ok := lookupTool("canary_lending_rates")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only rates tool")
	}
	for _, args := range []string{`{}`, `{"symbols":[]}`, `{"symbols":["$BAD"]}`} {
		if _, err := tool.Handler(t.Context(), conn, json.RawMessage(args)); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"symbols":["aaa"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.MarketEventsResult
	if json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("evidence changed")
	}
	_ = conn.Close()
	if !reflect.DeepEqual(<-calls, []string{rpc.MethodMarketEventsSnapshot}) {
		t.Fatal("wrong RPC boundary")
	}
}

func TestLendingScreenToolReadOnlyAndScope(t *testing.T) {
	want := rpc.LendingScreenResult{Kind: "lending_screen", Universe: "us_short_stock", Status: "unavailable", Params: rpc.LendingScreenParams{MinRate: 50, Limit: 25, Exclude: []string{"AAA"}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodLendingScreen: want})
	tool, ok := lookupTool("canary_lending_screen")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only tool")
	}
	for _, args := range []string{`{"limit":101}`, `{"min_rate":-1}`, `{"exclude":["$BAD"]}`} {
		if _, err := tool.Handler(t.Context(), conn, json.RawMessage(args)); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"exclude":["aaa"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.LendingScreenResult
	if json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("evidence changed")
	}
	_ = conn.Close()
	if !reflect.DeepEqual(<-calls, []string{rpc.MethodLendingScreen}) {
		t.Fatal("wrong RPC boundary")
	}
}

func TestLendingMarketToolReadOnlyAndScope(t *testing.T) {
	want := rpc.LendingMarketResult{Kind: "lending_market", Symbols: []string{"AAA"}, Rows: []rpc.LendingMarketRow{{Symbol: "AAA", Status: "pending"}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodLendingMarket: want})
	tool, ok := lookupTool("canary_lending_market")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing read-only market context")
	}
	if _, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"symbols":[]}`)); err == nil {
		t.Fatal("unbounded empty input accepted")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"symbols":["aaa"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.LendingMarketResult
	if json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("market context changed")
	}
	_ = conn.Close()
	if !reflect.DeepEqual(<-calls, []string{rpc.MethodLendingMarket}) {
		t.Fatal("wrong RPC boundary")
	}
}
