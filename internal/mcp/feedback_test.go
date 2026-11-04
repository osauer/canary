package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/feedback"
)

// canary_feedback answers without a daemon: the server passes a nil
// connection and the tool reports that none was reachable, with the URL.
func TestFeedbackToolAnswersWithoutADaemon(t *testing.T) {
	t.Setenv("USER", "synthuser")
	tool, ok := lookupTool("canary_feedback")
	if !ok || !tool.DaemonOptional || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatalf("tool = %+v", tool)
	}
	ctx := context.WithValue(context.Background(), versionKey{}, "v3.33.0")
	out, err := tool.Handler(ctx, nil, json.RawMessage(`{"error":"account U1234567 on synth-mbp.local: panic in handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	var res feedback.Result
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Fields.Daemon.Reachable || res.Fields.Surface != feedback.SurfaceMCP || !res.Redacted {
		t.Fatalf("result = %+v", res)
	}
	for _, leaked := range []string{"U1234567", "synth-mbp", "synthuser"} {
		if strings.Contains(string(out), leaked) {
			t.Errorf("answer leaks %q: %s", leaked, out)
		}
	}
	q, err := url.ParseQuery(strings.SplitN(res.URL, "?", 2)[1])
	if err != nil || q.Get("surface") != "MCP server" || q.Get("version") != "v3.33.0 (standard build)" && q.Get("version") != "v3.33.0 (trading build)" {
		t.Fatalf("url fields = %v (%v)", q, err)
	}
	if _, err := tool.Handler(ctx, nil, json.RawMessage(`{"surface":"web"}`)); !errors.Is(err, feedback.ErrSurface) {
		t.Fatalf("surface error = %v", err)
	}
}

// The server routes a DaemonOptional tool through the probe dialer and
// turns its failure into a nil connection; every other tool still fails.
func TestDaemonOptionalToolsUseTheProbeDialer(t *testing.T) {
	s := NewServer(nil, "v3.33.0")
	probed := 0
	s.SetProbeDialer(func(context.Context) (*dial.Conn, error) { probed++; return nil, dial.ErrSocketMissing })
	s.SetContextDialer(func(context.Context) (*dial.Conn, error) {
		return nil, errors.New("request dialer must not run for an optional tool")
	})
	tool, _ := lookupTool("canary_feedback")
	conn, closeConn, err := s.dialForTool(context.Background(), tool)
	closeConn()
	if err != nil || conn != nil || probed != 1 {
		t.Fatalf("optional dial = %v, %v, probed %d", conn, err, probed)
	}
	status, _ := lookupTool("canary_status")
	if _, closeConn, err := s.dialForTool(context.Background(), status); err == nil {
		closeConn()
		t.Fatal("a daemon-bound tool must fail when the daemon is unreachable")
	}
}

func TestFeedbackDispatchPreservesBuildVersion(t *testing.T) {
	server := NewServer(nil, "v3.33.1")
	server.SetProbeDialer(func(context.Context) (*dial.Conn, error) { return nil, dial.ErrSocketMissing })
	var out bytes.Buffer
	server.out = bufio.NewWriter(&out)
	server.handleToolsCall(t.Context(), json.RawMessage(`1`), json.RawMessage(`{"name":"canary_feedback","arguments":{}}`))
	if err := server.out.Flush(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result toolResultPayload `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("feedback dispatch failed: %s", out.String())
	}
	var got feedback.Result
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fields.Version != "v3.33.1" {
		t.Fatalf("feedback version %q, want v3.33.1", got.Fields.Version)
	}
}
