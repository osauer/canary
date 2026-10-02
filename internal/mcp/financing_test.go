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
