package mcp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestStockAddMCPOnlyRequestsReadOnlyPlan(t *testing.T) {
	tool, ok := lookupTool("canary_add")
	if !ok || tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint || !slices.Equal(tool.RPCMethods, []string{rpc.MethodAddPlan}) {
		t.Fatalf("unsafe tool %+v", tool)
	}
	path := filepath.Join("/tmp", fmt.Sprintf("canary-add-test-%d.sock", time.Now().UnixNano()))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(path) })
	calls := make(chan rpc.Request, 2)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		decoder := json.NewDecoder(c)
		for range 2 {
			var req rpc.Request
			if decoder.Decode(&req) != nil {
				return
			}
			calls <- req
			_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: json.RawMessage(`{"quantity":3,"before":0,"after":3}`)})
		}
	}()
	conn, err := dial.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = tool.Handler(t.Context(), conn, json.RawMessage(`{"symbol":"syna","currency":"usd","limit_price":100,"quantity":3}`))
	if err != nil {
		t.Fatal(err)
	}
	req := <-calls
	var p rpc.AddParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if req.Method != rpc.MethodAddPlan || p.Contract.Symbol != "SYNA" || p.Quantity != 3 || p.LimitPrice != 100 {
		t.Fatalf("unexpected call %+v %+v", req, p)
	}
	_, err = tool.Handler(t.Context(), conn, json.RawMessage(`{"symbol":"syna","currency":"usd","limit_price":100,"max":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req = <-calls
	p = rpc.AddParams{}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if req.Method != rpc.MethodAddPlan || !p.Max || p.Quantity != 0 {
		t.Fatalf("explicit Max lost: %+v %+v", req, p)
	}
	for _, args := range []string{`{"symbol":"SYNA","currency":"USD","limit_price":100}`, `{"symbol":"SYNA","currency":"USD","limit_price":100,"quantity":0}`, `{"symbol":"SYNA","currency":"USD","limit_price":100,"quantity":3,"max":true}`, `{"symbol":"SYNA","currency":"USD","limit_price":100,"existing_position_size":0}`, `{"symbol":"SYNA","currency":"USD","limit_price":100,"submit":true}`, `{"symbol":"SYNA","limit_price":100}`} {
		if _, err := tool.Handler(t.Context(), nil, json.RawMessage(args)); err == nil {
			t.Fatalf("accepted authority input %s", args)
		}
	}
}
