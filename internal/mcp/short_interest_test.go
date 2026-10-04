package mcp

import (
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	"reflect"
	"testing"
)

func TestShortInterestToolReadOnlyAndScope(t *testing.T) {
	p, _ := rpc.NormalizeShortInterestScreenParams(rpc.ShortInterestScreenParams{})
	want := rpc.ShortInterestScreenResult{Kind: "short_interest_screen", Status: "pending", Source: "FINRA equity short interest", SourceURL: "https://www.finra.org/finra-data/browse-catalog/equity-short-interest/files", Params: p, Coverage: rpc.ShortInterestCoverage{Complete: true}, Rows: []rpc.ShortInterestRow{}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodShortInterestScreen: want})
	tool, ok := lookupTool("canary_short_interest_screen")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("missing readonly tool")
	}
	for _, args := range []string{`{"limit":101}`, `{"min_price":-1}`, `{"sort_by":"short_interest_pct_float"}`} {
		if _, err := tool.Handler(t.Context(), conn, json.RawMessage(args)); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.ShortInterestScreenResult
	if json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("changed evidence")
	}
	_ = conn.Close()
	if !reflect.DeepEqual(<-calls, []string{rpc.MethodShortInterestScreen}) {
		t.Fatal("wrong RPC boundary")
	}
}
