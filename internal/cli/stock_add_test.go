package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type stockAddConn struct {
	method string
	params rpc.AddParams
}

func (c *stockAddConn) Call(_ context.Context, m string, in, out any) error {
	c.method = m
	b, _ := json.Marshal(in)
	if err := json.Unmarshal(b, &c.params); err != nil {
		return err
	}
	return nil
}
func (*stockAddConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}
func TestStockAddCLIOnlyPlansOrPreviews(t *testing.T) {
	for _, action := range []string{"plan", "preview"} {
		conn := &stockAddConn{}
		var stdout, stderr bytes.Buffer
		code := Run(t.Context(), &Env{Conn: conn, Stdout: &stdout, Stderr: &stderr}, "add", []string{action, "syna", "--currency", "usd", "--limit", "100", "--quantity", "3", "--json"})
		want := rpc.MethodAddPlan
		if action == "preview" {
			want = rpc.MethodAddPreview
		}
		if code != 0 || conn.method != want || conn.params.Quantity != 3 || conn.params.Contract.Symbol != "SYNA" {
			t.Fatalf("%d %+v %s", code, conn, stderr.String())
		}
	}
	for _, args := range [][]string{{"submit", "SYNA"}, {"plan", "SYNA", "--limit", "100"}, {"plan", "SYNA", "--currency", "USD", "--limit", "100", "--existing-position-size", "0"}} {
		conn := &stockAddConn{}
		var buf bytes.Buffer
		if Run(t.Context(), &Env{Conn: conn, Stdout: &buf, Stderr: &buf}, "add", args) == 0 || conn.method != "" {
			t.Fatalf("unsafe input called RPC: %v", args)
		}
	}
}
func goldenStockAdd() rpc.AddPlanResult {
	return rpc.AddPlanResult{Contract: rpc.ContractParams{Symbol: "SYNA"}, Currency: "USD", LimitPrice: 100, Before: 0, After: 9, Quantity: 9, MaxQuantity: 9, Effect: "open_long", Cost: 905, CashAfter: 95, Binding: "cash"}
}
