package mcp

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestBriefToolPreservesRegimeEvidence(t *testing.T) {
	want := rpc.BriefResult{Ready: rpc.BriefReadySection{
		Breadth: rpc.BriefBreadthRow{PctAbove50DMA: new(55.0), MemberCount: 500, Coverage50: 480},
		Gamma: rpc.BriefGammaRow{Underlying: "SPX", Regime: "short_gamma", Insight: &rpc.GammaInsight{
			Rankability: "blocked", RankabilityReason: "stale", DataType: "frozen",
			DirectionalInference: "Positioning unknown", SelectedSkew: &rpc.OptionSkew{SkewVolPoints: new(3.0)},
			Horizons: []rpc.GammaHorizonInsight{{Horizon: "0dte", Regime: "unavailable"}},
		}},
	}}
	dir, err := os.MkdirTemp("/tmp", "canary-brief-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "brief.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	calls := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req rpc.Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			return
		}
		calls <- req.Method
		raw, _ := json.Marshal(want)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()
	conn, err := dial.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tool, ok := lookupTool("canary_brief")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("brief must remain read-only")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.BriefResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || <-calls != rpc.MethodBriefSnapshot {
		t.Fatal("MCP changed the daemon's measured evidence")
	}
}
