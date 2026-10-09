package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPortfolioPlanAdaptersPreserveEvidenceAndCannotWrite(t *testing.T) {
	tool, ok := lookupTool("canary_portfolio_plan")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint || !slices.Equal(tool.RPCMethods, []string{rpc.MethodPortfolioPlan}) {
		t.Fatalf("unsafe tool: %+v", tool)
	}
	for _, raw := range []string{`{"submit":true}`, `{"max":true}`, `{"targets":[]}`, `{"portfolio":{}}`, `[]`, `{} {}`} {
		if _, err := tool.Handler(t.Context(), nil, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted injected policy or authority: %s", raw)
		}
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	want := rpc.PortfolioPlanResult{Kind: "ibkr.portfolio_plan", AsOf: now, State: "held", Reason: "Missing source evidence", WatchlistRevision: 7, ProposalRevision: "synthetic", ProposalAsOf: now.Add(-time.Second), Targets: []risk.PortfolioIntent{{ConID: 101, Symbol: "SYNA", Decision: "cannot_evaluate", Reason: "No current valuation"}}, Blockers: []rpc.TradingBlocker{{Code: "portfolio_book_unavailable", Message: "Current portfolio required"}}}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join("/tmp", fmt.Sprintf("canary-portfolio-test-%d.sock", time.Now().UnixNano()))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(socket) })
	calls := make(chan rpc.Request, 2)
	go func() {
		for range 2 {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var req rpc.Request
			if json.NewDecoder(c).Decode(&req) != nil {
				_ = c.Close()
				return
			}
			calls <- req
			_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
			_ = c.Close()
		}
	}()
	for _, adapter := range []string{"MCP", "CLI"} {
		conn, err := dial.Connect(socket)
		if err != nil {
			t.Fatal(err)
		}
		var gotRaw []byte
		if adapter == "MCP" {
			gotRaw, err = tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
		} else {
			var out, errs bytes.Buffer
			if code := cli.Run(t.Context(), &cli.Env{Conn: conn, Stdout: &out, Stderr: &errs}, "portfolio", []string{"plan", "--json"}); code != 0 {
				t.Fatalf("CLI exit %d: %s", code, errs.String())
			}
			gotRaw = out.Bytes()
		}
		_ = conn.Close()
		var got rpc.PortfolioPlanResult
		if err != nil || json.Unmarshal(gotRaw, &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed source evidence: %+v %v", adapter, got, err)
		}
		req := <-calls
		if req.Method != rpc.MethodPortfolioPlan || rpc.ValidatePortfolioPlanParams(req.Params) != nil {
			t.Fatalf("unsafe request: %+v", req)
		}
	}
}
