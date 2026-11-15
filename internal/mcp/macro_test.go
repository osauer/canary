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

func TestMacroToolPreservesWindowAndIndependentFailureEvidence(t *testing.T) {
	want := rpc.MacroSnapshotResult{CoverageStatus: "partial", Truncated: true, PublicationsTruncated: true, WindowStart: "2026-09-11", WindowEnd: "2026-09-11", Sources: []rpc.MacroSource{{ID: "bls-calendar", Availability: "unavailable", Detail: "source returned HTTP 403", ConsecutiveFailures: 4}, {ID: "nyfed-calendar", Availability: "available", WindowStart: "2026-09-01", WindowEnd: "2026-09-30"}}}

	dir, err := os.MkdirTemp("/tmp", "canary-macro-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "macro.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	calls := make(chan rpc.Request, 1)
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
		calls <- req
		raw, _ := json.Marshal(want)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()
	conn, err := dial.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tool, ok := lookupTool("canary_macro")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatal("macro must remain read-only")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"window_start":"2026-09-11","window_end":"2026-09-11"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.MacroSnapshotResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	req := <-calls
	var params rpc.MacroSnapshotParams
	if json.Unmarshal(req.Params, &params) != nil || params.WindowStart != "2026-09-11" || params.WindowEnd != "2026-09-11" {
		t.Fatal("MCP discarded review window")
	}
	if !reflect.DeepEqual(got, want) || req.Method != rpc.MethodMacroSnapshot {
		t.Fatal("MCP changed the daemon's measured evidence")
	}
}
