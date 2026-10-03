package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
)

func TestMCPPingCancelAndShutdownWhileDaemonUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input, send := io.Pipe()
	output, receive := io.Pipe()
	defer input.Close()
	defer send.Close()
	defer output.Close()
	defer receive.Close()
	s := NewServer(nil, "test")
	entered := make(chan struct{}, 1)
	s.SetContextDialer(func(ctx context.Context) (*dial.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- s.ServeWithOptions(ctx, input, receive, ServeOptions{}) }()
	responses := make(chan rpcResponse, 10)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			var p rpcResponse
			if json.Unmarshal(scanner.Bytes(), &p) == nil {
				responses <- p
			}
		}
	}()
	sendLine := func(line string) {
		t.Helper()
		if _, err := fmt.Fprintln(send, line); err != nil {
			t.Fatal(err)
		}
	}
	next := func() rpcResponse {
		t.Helper()
		select {
		case p := <-responses:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("MCP control response blocked by tool call")
			return rpcResponse{}
		}
	}
	sendLine(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}`)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("tool not started")
	}
	sendLine(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if p := next(); string(p.ID) != "2" || p.Error != nil {
		t.Fatalf("ping: %+v", p)
	}
	sendLine(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}`)
	sendLine(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}`)
	if p := next(); string(p.ID) != "4" || p.Error == nil {
		t.Fatalf("unbounded call admitted: %+v", p)
	}
	sendLine(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`)
	sendLine(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	if p := next(); string(p.ID) != "1" {
		t.Fatalf("cancel result: %+v", p)
	}
	if p := next(); string(p.ID) != "3" {
		t.Fatalf("queued cancel result: %+v", p)
	}
	sendLine(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}`)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second tool not started")
	}
	sendLine(`{"jsonrpc":"2.0","id":7,"method":"shutdown"}`)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked")
	}
}
