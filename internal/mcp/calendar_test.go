package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestCalendarToolPreservesSchedulingAuthorityAndErrors(t *testing.T) {
	closeAt, _ := time.Parse(time.RFC3339, "2026-11-27T13:00:00-05:00")
	want := rpc.MarketCalendarResult{
		Market: "us_equity", Timezone: "America/New_York", CoverageStart: "2026-01-01", CoverageEnd: "2028-12-31",
		Source: "official_exchange_calendar", SourceURL: "https://www.nyse.com/markets/hours-calendars",
		Session:  rpc.MarketSession{Date: "2026-11-27", State: "early_close", Open: closeAt.Add(-210 * time.Minute), Close: closeAt},
		Sessions: []rpc.MarketSession{{Date: "2029-01-02", State: "unknown", Reason: "outside embedded official calendar coverage"}},
	}
	dir, err := os.MkdirTemp("/tmp", "canary-calendar-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rpc.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	requests := make(chan rpc.Request, 2)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		dec, enc := json.NewDecoder(c), json.NewEncoder(c)
		for i := range 2 {
			var req rpc.Request
			if dec.Decode(&req) != nil {
				return
			}
			requests <- req
			raw, _ := json.Marshal(want)
			res := rpc.Response{ID: req.ID, Ok: true, Result: raw}
			if i == 1 {
				res = rpc.Response{ID: req.ID, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: "invalid date"}}
			}
			if enc.Encode(res) != nil {
				return
			}
		}
	}()
	conn, err := dial.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	server := NewServer(conn, "test")
	var out bytes.Buffer
	server.out = bufio.NewWriter(&out)
	for _, args := range []string{`{"market":"us","at":"2026-11-27T12:00:00-05:00","days":400}`, `{"date":"2026-99-01"}`, `{"at":"not-an-instant"}`} {
		out.Reset()
		params, _ := json.Marshal(callParams{Name: "canary_calendar", Arguments: json.RawMessage(args)})
		server.handleToolsCall(t.Context(), json.RawMessage(`1`), params)
		_ = server.out.Flush()
		var response struct {
			Result toolResultPayload `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if args == `{"market":"us","at":"2026-11-27T12:00:00-05:00","days":400}` {
			var got rpc.MarketCalendarResult
			if response.Result.IsError || len(response.Result.Content) != 1 || json.Unmarshal([]byte(response.Result.Content[0].Text), &got) != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("MCP changed official calendar evidence", out.String())
			}
		} else if !response.Result.IsError {
			t.Fatal("invalid scheduling input became a successful tool result", out.String())
		}
	}
	for i := range 2 {
		req := <-requests
		if req.Method != rpc.MethodMarketCalendar {
			t.Fatalf("calendar used unexpected daemon method %s", req.Method)
		}
		if i == 0 {
			var params rpc.MarketCalendarParams
			if json.Unmarshal(req.Params, &params) != nil || params.Market != "us" || params.Days != 400 || !params.At.Equal(closeAt.Add(-time.Hour)) {
				t.Fatal("MCP changed query time or market")
			}
		}
	}
	tool, ok := lookupTool("canary_calendar")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint || !reflect.DeepEqual(tool.RPCMethods, []string{rpc.MethodMarketCalendar}) {
		t.Fatal("calendar must remain a read-only daemon adapter")
	}
	if _, ok := (&Server{profile: ProfileMonitor}).lookupVisibleTool("canary_calendar"); ok {
		t.Fatal("calendar changed the compact monitor profile")
	}
}
