package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestAddMarketAndCallPlanDispatchThroughCLI(t *testing.T) {
	for _, tc := range []struct{ action, method, body string }{
		{"market", rpc.MethodAddMarket, `{"underlying":{"symbol":"SYNA","sec_type":"STK","currency":"USD","exchange":"SMART"},"discover":true}`},
		{"call-plan", rpc.MethodAddCallPlan, `{"contract":{"symbol":"SYNA","sec_type":"OPT"},"limit_price":5.5,"budget":5000,"round_size":true}`},
	} {
		for _, args := range [][]string{{tc.action, "--json"}, {"--json", tc.action}, {tc.action}} {
			c := &stockAddConn{}
			var out bytes.Buffer
			code := Run(t.Context(), &Env{Conn: c, Stdin: strings.NewReader(tc.body), Stdout: &out, Stderr: &out}, "add", args)
			if code != 0 || c.method != tc.method {
				t.Fatalf("%v: exit=%d method=%s %s", args, code, c.method, out.String())
			}
		}
		for _, args := range [][]string{{tc.action, "--limit", "5"}, {tc.action, "unexpected"}} {
			c := &stockAddConn{}
			var out bytes.Buffer
			if Run(t.Context(), &Env{Conn: c, Stdin: strings.NewReader(tc.body), Stdout: &out, Stderr: &out}, "add", args) == 0 || c.method != "" {
				t.Fatalf("unexpected arguments reached RPC: %v", args)
			}
		}
	}
}

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
		code := Run(t.Context(), &Env{Conn: conn, Stdout: &stdout, Stderr: &stderr}, "add", []string{action, "syna", "--currency", "usd", "--limit", "100", "--quantity", "3", "--adaptive-priority", "Normal", "--json"})
		want := rpc.MethodAddPlan
		if action == "preview" {
			want = rpc.MethodAddPreview
		}
		if code != 0 || conn.method != want || conn.params.Quantity != 3 || conn.params.Contract.Symbol != "SYNA" || conn.params.AdaptivePriority != "Normal" {
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
func TestStockAddCLIStopPriceSurvivesDispatch(t *testing.T) {
	for _, action := range []string{"plan", "preview"} {
		for _, stopFlags := range [][]string{{"--stop-price", "90.25", "--json"}, {"--json", "--stop-price", "90.25"}, {"--stop-price=90.25", "--json"}} {
			t.Run(action+"/"+strings.Join(stopFlags, "_"), func(t *testing.T) {
				conn := &stockAddConn{}
				var out bytes.Buffer
				args := append([]string{action, "SYNA", "--currency", "USD", "--limit", "100", "--quantity", "3", "--con-id", "10001"}, stopFlags...)
				code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "add", args)
				want := rpc.MethodAddPlan
				if action == "preview" {
					want = rpc.MethodAddPreview
				}
				if code != 0 || conn.method != want || conn.params.StopPrice != 90.25 || conn.params.LimitPrice != 100 || conn.params.Quantity != 3 || conn.params.Contract.ConID != 10001 {
					t.Fatalf("%v: exit=%d method=%s params=%+v output=%s", args, code, conn.method, conn.params, out.String())
				}
			})
		}
	}
}

func TestStockAddCatalogDeclaresReadOnlySubcommands(t *testing.T) {
	for _, spec := range Catalog() {
		if spec.Name != "add" {
			continue
		}
		if spec.Guard != GuardReadOnly {
			t.Fatalf("Add guard = %s", spec.Guard)
		}
		seen := map[string]bool{}
		for _, sub := range spec.Subcommands {
			if sub.Guard != GuardReadOnly {
				t.Fatalf("Add %s guard = %s", sub.Name, sub.Guard)
			}
			seen[sub.Name] = true
		}
		for _, name := range []string{"plan", "preview", "market", "call-plan"} {
			if !seen[name] {
				t.Fatalf("Add %s is missing from the catalog", name)
			}
		}
		return
	}
	t.Fatal("Add is missing from the catalog")
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

func TestAddHelpDispatchesWithoutReadingOrCalling(t *testing.T) {
	for _, tc := range []struct {
		action string
		fields []string
	}{
		{"market", []string{"underlying", "discover", "selected", "refresh", "horizon_days"}},
		{"call-plan", []string{"contract", "limit_price", "budget", "round_size"}},
	} {
		for _, args := range [][]string{{tc.action, "--help"}, {"--help", tc.action}, {tc.action, "--json", "-h"}} {
			t.Run(strings.Join(args, "_"), func(t *testing.T) {
				c := &stockAddConn{}
				var out bytes.Buffer
				code := Run(t.Context(), &Env{Conn: c, Stdin: addHelpNoRead{t}, Stdout: &out, Stderr: &out, HelpOnly: true}, "add", args)
				if code != 0 || c.method != "" {
					t.Fatalf("help exit=%d method=%s output=%s", code, c.method, out.String())
				}
				if !strings.Contains(out.String(), "canary add "+tc.action) || strings.Contains(out.String(), "--stop-price") {
					t.Fatalf("wrong command help: %s", out.String())
				}
				for _, field := range tc.fields {
					if !strings.Contains(out.String(), field) {
						t.Errorf("stdin field %q missing from help: %s", field, out.String())
					}
				}
			})
		}
	}
}

type addHelpNoRead struct{ t *testing.T }

func (r addHelpNoRead) Read([]byte) (int, error) {
	r.t.Fatal("help read stdin")
	return 0, errors.New("unexpected stdin read")
}

func TestAddHelpDoesNotTreatFlagValuesAsActions(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "SYNA", "--exchange", "market", "--help"},
		{"plan", "SYNA", "--currency", "call-plan", "--help"},
		{"plan", "market", "--help"},
	} {
		var out bytes.Buffer
		c := &stockAddConn{}
		code := Run(t.Context(), &Env{Conn: c, Stdin: addHelpNoRead{t}, Stdout: &out, Stderr: &out, HelpOnly: true}, "add", args)
		if code != 0 || c.method != "" || !strings.Contains(out.String(), "--stop-price") {
			t.Fatalf("%v: exit=%d method=%s help=%s", args, code, c.method, out.String())
		}
	}
}
