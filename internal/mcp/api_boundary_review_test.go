package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/osauer/canary/v2/internal/dial"
)

func TestAPIBoundaryTradingToolCallsNeverDial(t *testing.T) {
	for _, name := range []string{"order.place", "canary_order_place", "canary_order_preview", "canary_proposals_submit", "canary_opportunities_exercise"} {
		t.Run(name, func(t *testing.T) {
			server := NewServer(nil, "test")
			calls := 0
			server.SetContextDialer(func(context.Context) (*dial.Conn, error) {
				calls++
				return nil, fmt.Errorf("unexpected daemon connection")
			})
			var out bytes.Buffer
			server.out = bufio.NewWriter(&out)
			params, err := json.Marshal(callParams{Name: name, Arguments: json.RawMessage(`{"preview_token":"synthetic-token","submit":true}`)})
			if err != nil {
				t.Fatal(err)
			}
			server.handleToolsCall(t.Context(), json.RawMessage(`7`), params)
			var response rpcResponse
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Code != codeMethodNotFound || calls != 0 {
				t.Fatalf("forbidden tool response=%+v daemon_calls=%d", response, calls)
			}
		})
	}
}

func TestAPIBoundaryAddRejectsMalformedNumbersBeforeRPC(t *testing.T) {
	tool, ok := lookupTool("canary_add")
	if !ok {
		t.Fatal("missing stock addition plan")
	}
	for _, args := range []string{
		`{"symbol":"SYNTH","currency":"USD","limit_price":100,"quantity":0.5}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":100,"quantity":-1}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":100,"quantity":1000001}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":100,"quantity":9223372036854775808}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":1e309,"quantity":1}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":NaN,"quantity":1}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":-1,"quantity":1}`,
		`{"symbol":"SYNTH","currency":"USD","limit_price":100,"quantity":1,"con_id":-1}`,
	} {
		// A malformed plan must fail before it needs a daemon connection.
		if _, err := tool.Handler(t.Context(), nil, json.RawMessage(args)); err == nil {
			t.Errorf("malformed plan was accepted: %s", args)
		}
	}
}
