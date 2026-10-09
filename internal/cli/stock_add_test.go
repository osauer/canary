package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
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
	return rpc.AddPlanResult{Contract: rpc.ContractParams{Symbol: "SYNA"}, Currency: "USD", BaseCurrency: "USD", LimitPrice: 100,
		Before: 4, After: 13, Quantity: 9, MaxQuantity: 9, MaximumKnown: true, Sizing: "max", OrderUpperBound: 10, AllocationRoom: 20, Effect: "increase_long", Cost: 905, CashAfter: 95, Binding: "cash",
		StockPctBefore: 10, StockPctAfter: 10.9, UnderlyingPctBefore: 0.6, UnderlyingPctAfter: 1.5,
		Allowances: []risk.StockAddAllowance{{Code: "cash", Limit: 1000, Remaining: 1000, Shares: 10}, {Code: "underlying_stock", Limit: 2600, Used: 600, Remaining: 2000, Shares: 20}},
		Cash:       &risk.StockAddCash{Available: 1600, Committed: 200, CurrencyFloat: 100, ReserveInCurrency: 300, AccountReserveBase: 1000, OtherReserveFundingBase: 700, Spendable: 1000},
		Protection: &risk.StockAddProtection{StopQuantity: 3, UncoveredBefore: 1, UncoveredAfter: 10, PendingQuantity: 2},
		RiskChecks: []risk.StockAddRiskCheck{{ID: "margin_headroom", Title: "Margin headroom", BeforeStatus: "pass", BeforeEvidence: "Excess liquidity is 50% of account value.", AfterStatus: "pass", AfterEvidence: "Broker simulation leaves 49% of account value as excess liquidity."}},
	}
}

func TestStockAddCLIRequiresExplicitSizingIntent(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		valid bool
	}{
		{[]string{"--max"}, true},
		{[]string{"--quantity", "2"}, true},
		{nil, false},
		{[]string{"--quantity", "0"}, false},
		{[]string{"--max", "--quantity", "2"}, false},
	} {
		c := &stockAddConn{}
		var out bytes.Buffer
		args := append([]string{"plan", "SYNA", "--currency", "USD", "--limit", "100", "--json"}, tc.flags...)
		code := Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &out}, "add", args)
		if (code == 0) != tc.valid || (!tc.valid && c.method != "") {
			t.Fatalf("%v: exit=%d called=%s %s", tc.flags, code, c.method, out.String())
		}
	}
}
