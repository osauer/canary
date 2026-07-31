package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
)

func TestMCPToolCallTimeoutBudgets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args json.RawMessage
		want time.Duration
	}{
		{name: "canary_status", want: mcpFastToolTimeout},
		{name: "canary_scan", args: json.RawMessage(`{}`), want: mcpFastToolTimeout},
		{name: "canary_scan", args: json.RawMessage(`{"preset":"top-movers"}`), want: mcpScannerToolTimeout},
		{name: "canary_watch", args: json.RawMessage(`{"include_quotes":false}`), want: mcpFastToolTimeout},
		{name: "canary_watch", args: json.RawMessage(`{}`), want: mcpWatchQuoteTimeout},
		{name: "canary_chain", args: json.RawMessage(`{"symbol":"AAPL","expiry":"2026-07-17"}`), want: mcpLongToolTimeout},
		{name: "canary_technical", args: json.RawMessage(`{"symbols":["ASTS","IREN"]}`), want: mcpScannerToolTimeout},
		{name: "canary_regime", want: mcpRegimeToolTimeout},
		{name: "canary_stress", want: mcpScannerToolTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name+" "+string(tc.args), func(t *testing.T) {
			t.Parallel()
			if got := mcpToolCallTimeout(tc.name, tc.args); got != tc.want {
				t.Fatalf("mcpToolCallTimeout(%q, %s) = %s, want %s", tc.name, tc.args, got, tc.want)
			}
		})
	}
}

// A starting daemon verifies its database before it publishes its socket, and
// that wait grows with the file. Opening the connection must get that budget
// rather than the tool's own: charging it to canary_status's two seconds
// turned a healthy boot into a tool timeout.
func TestMCPToolDialGetsStartupBudgetNotToolBudget(t *testing.T) {
	var dialBudget time.Duration
	srv := NewServer(nil, "test")
	srv.SetContextDialer(func(ctx context.Context) (*dial.Conn, error) {
		if deadline, ok := ctx.Deadline(); ok {
			dialBudget = time.Until(deadline)
		}
		return nil, errors.New("no daemon")
	})

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}` + "\n")
	if err := srv.Serve(context.Background(), in, &bytes.Buffer{}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	startup := dial.StartupBudget()
	if dialBudget <= mcpFastToolTimeout || dialBudget > startup {
		t.Fatalf("dial budget = %s, want more than the %s tool budget and at most the %s startup budget", dialBudget, mcpFastToolTimeout, startup)
	}
}

func TestMCPToolCallTimesOutHungDaemon(t *testing.T) {
	dialer, stop := silentDaemonDialer(t)
	defer stop()

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}` + "\n")
	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	srv.SetDialer(dialer)

	start := time.Now()
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("hung daemon response took %s, want bounded below MCP host timeout", elapsed)
	}

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	if !resp.Result.IsError {
		t.Fatalf("expected isError=true, got: %s", out.String())
	}
	if len(resp.Result.Content) != 1 || !strings.Contains(resp.Result.Content[0].Text, "canary_status timed out after 2s") {
		t.Fatalf("timeout message = %+v", resp.Result.Content)
	}
}

func silentDaemonDialer(t *testing.T) (func() (*dial.Conn, error), func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "canary-mcp-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	path := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}

	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			go func(c net.Conn) {
				<-done
				_ = c.Close()
			}(conn)
		}
	}()

	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			_ = ln.Close()
			_ = os.RemoveAll(dir)
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range conns {
				_ = conn.Close()
			}
		})
	}
	dialer := func() (*dial.Conn, error) {
		return dial.Connect(path)
	}
	return dialer, stop
}
