package mcp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestBriefToolIsReadOnlySnapshotOnly(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("canary_brief")
	if !ok {
		t.Fatal("missing canary_brief tool")
	}
	if tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatalf("canary_brief ReadOnlyHint=%v, want true", tool.ReadOnlyHint)
	}
	if !reflect.DeepEqual(tool.RPCMethods, []string{rpc.MethodBriefSnapshot}) {
		t.Fatalf("canary_brief RPCMethods=%v, want snapshot only", tool.RPCMethods)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if schema.Type != "object" || len(schema.Properties) != 0 {
		t.Fatalf("canary_brief schema=%s, want empty object", tool.JSONSchema)
	}
	desc := strings.ToLower(tool.Description)
	for _, want := range []string{"read-only", "never acknowledges", "writes to the journal", "canary_positions", "canary_account"} {
		if !strings.Contains(desc, want) {
			t.Errorf("canary_brief description missing %q", want)
		}
	}
}

func TestBriefToolReturnsDaemonResultUnchanged(t *testing.T) {
	t.Parallel()
	fixture := rpc.BriefResult{
		AsOf:             time.Date(2026, 8, 4, 7, 45, 0, 0, time.UTC),
		BriefFingerprint: "brief-fingerprint",
		StampTarget:      rpc.BriefKindMorning,
		Review: rpc.BriefReviewSection{
			BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusAttention, Detail: "review detail"},
		},
		Ready: rpc.BriefReadySection{
			BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusDegraded, Detail: "ready detail"},
		},
		Narrative: &rpc.BriefNarrative{
			Coda: []rpc.BriefRun{{Text: "Check the unavailable evidence before trading."}},
		},
	}
	conn, calls := startBriefFakeDaemon(t, fixture)
	defer conn.Close()
	tool, _ := lookupTool("canary_brief")
	out, err := tool.Handler(context.Background(), conn, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	call := <-calls
	if call.Method != rpc.MethodBriefSnapshot {
		t.Fatalf("method=%q, want %q", call.Method, rpc.MethodBriefSnapshot)
	}
	if strings.TrimSpace(string(call.Params)) != "{}" {
		t.Fatalf("params=%s, want exact empty object", call.Params)
	}
	var got rpc.BriefResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if !reflect.DeepEqual(got, fixture) {
		t.Fatalf("brief changed across MCP adapter\ngot:  %+v\nwant: %+v", got, fixture)
	}
}

type briefFakeCall struct {
	Method string
	Params json.RawMessage
}

func startBriefFakeDaemon(t *testing.T, result rpc.BriefResult) (*dial.Conn, <-chan briefFakeCall) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "canary-mcp-brief-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "brief.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	calls := make(chan briefFakeCall, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req rpc.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		calls <- briefFakeCall{Method: req.Method, Params: append(json.RawMessage(nil), req.Params...)}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(conn).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()
	conn, err := dial.Connect(path)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return conn, calls
}
