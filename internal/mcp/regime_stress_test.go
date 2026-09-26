package mcp

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

func riskToolConn(t *testing.T, results map[string]any) (*dial.Conn, <-chan []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "canary-risk-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "rpc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	calls := make(chan []string, 1)
	go func() {
		var methods []string
		defer func() { calls <- methods }()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		dec, enc := json.NewDecoder(c), json.NewEncoder(c)
		for {
			var req rpc.Request
			if dec.Decode(&req) != nil {
				return
			}
			methods = append(methods, req.Method)
			value, ok := results[req.Method]
			if !ok {
				_ = enc.Encode(rpc.Response{ID: req.ID, Error: &rpc.Error{Code: rpc.CodeUnknownMethod, Message: "unexpected method"}})
				continue
			}
			raw, _ := json.Marshal(value)
			if enc.Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw}) != nil {
				return
			}
		}
	}()
	conn, err := dial.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, calls
}

func TestRegimeToolRetainsDetailedEvidence(t *testing.T) {
	want := rpc.RegimeSnapshotResult{FundingStress: rpc.RegimeFundingStress{Status: "unavailable"}, VIXTermStructure: rpc.RegimeVIXTerm{Status: "stale", Ratio: new(1.1), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "red", Eligibility: &rpc.RegimeEligibility{Eligible: false, Reasons: []string{"stale evidence"}}}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodRegimeSnapshot: want})
	tool, ok := lookupTool("canary_regime")
	if !ok {
		t.Fatal("regime missing")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.RegimeSnapshotResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(<-calls, []string{rpc.MethodRegimeSnapshot}) {
		t.Fatal("regime evidence changed")
	}
}

func TestStressToolReturnsFullSharedAssessment(t *testing.T) {
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodAccountSummary: rpc.AccountResult{}, rpc.MethodPositionsList: rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "TEST", SecType: "STK", Quantity: 1}}}, rpc.MethodMarketEventsSnapshot: rpc.MarketEventsResult{}, rpc.MethodRegimeSnapshot: rpc.RegimeSnapshotResult{}})
	tool, ok := lookupTool("canary_stress")
	if !ok {
		t.Fatal("stress missing")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.StressResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if len(got.Rows) != 11 || len(got.MarketIndicators) != 8 || got.InputHealth == "ok" || got.NotExecution == "" {
		t.Fatalf("incomplete or falsely healthy assessment: rows=%d indicators=%d health=%q", len(got.Rows), len(got.MarketIndicators), got.InputHealth)
	}
	if !reflect.DeepEqual(<-calls, []string{rpc.MethodAccountSummary, rpc.MethodPositionsList, rpc.MethodRegimeSnapshot, rpc.MethodMarketEventsSnapshot, rpc.MethodRulesSnapshot, rpc.MethodAccountSummary}) {
		t.Fatal("unexpected stress read sequence")
	}
	if mcpToolCallTimeout("canary_stress", nil) < stress.FetchTimeout(5*time.Second) {
		t.Fatal("deadline does not cover sequential reads")
	}
	for _, tool := range (&Server{profile: ProfileMonitor}).visibleTools() {
		if tool.Name == "canary_regime" || tool.Name == "canary_stress" {
			t.Fatal("drill-down escaped into compact monitor profile")
		}
	}
}

func TestRegimeMonitorProjectionRetainsUnavailableEligibility(t *testing.T) {
	want := rpc.RegimeSnapshotResult{AuthorityHealth: &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityStale, LastSuccessAt: new(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)), LastSuccessAgeSeconds: new(int64(600))}, VIXTermStructure: rpc.RegimeVIXTerm{Status: "stale", Ratio: new(1.1), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "red", Eligibility: &rpc.RegimeEligibility{Eligible: false, Reasons: []string{"stale evidence"}}}}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodRegimeSnapshot: want})
	tool, _ := lookupTool("canary_regime")
	for _, args := range []string{`{"view":"other"}`, `{"view":"monitor","include_profiles":true}`} {
		if _, err := tool.Handler(t.Context(), conn, json.RawMessage(args)); err == nil {
			t.Fatal("invalid monitor request was accepted")
		}
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"view":"monitor"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.RegimeMonitorResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	expectedRaw, _ := json.Marshal(rpc.CompactRegimeMonitor(&want))
	var expected rpc.RegimeMonitorResult
	_ = json.Unmarshal(expectedRaw, &expected)
	if !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(<-calls, []string{rpc.MethodRegimeSnapshot}) {
		t.Fatal("compact MCP adapter changed authority or dispatched invalid inputs")
	}
}
