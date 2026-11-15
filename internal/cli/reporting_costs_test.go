package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// goldenReportingCosts is a synthetic month of fills: a Canary option buy and
// stock sale with quotes, a TWS fill without a quote, and a TWS option fill
// whose multiplier the broker did not report.
func goldenReportingCosts() rpc.ReportingCostsResult {
	at := goldenAt.Add(3 * time.Second)
	quoted := func(bid, ask float64, gapMS int64) rpc.FillQuote {
		return rpc.FillQuote{Status: rpc.FillQuoteQuoted, Bid: &bid, Ask: &ask, Mid: new((bid + ask) / 2), BidSize: new(40), AskSize: new(25), DataType: rpc.MarketDataLive,
			QuoteAt: at.Add(time.Duration(gapMS) * time.Millisecond), ReadStartedAt: at.Add(time.Second), ReadAt: at.Add(time.Duration(gapMS) * time.Millisecond), GapMS: &gapMS}
	}
	return rpc.ReportingCostsResult{
		SchemaVersion: rpc.ReportingCostsSchemaVersion, AsOf: goldenAt, Mode: "paper", BaseCurrency: "EUR",
		Coverage: rpc.CostCoverage{Fills: 4, Quoted: 3, Unknown: 1, Ratio: new(0.75)},
		Observation: rpc.ExecutionObservation{Polling: true, IntervalSeconds: 60, LastPollAt: goldenAt.Add(5 * time.Second), LastPollComplete: true, LastPollExecutions: 2,
			Visibility: rpc.ExecutionVisibilityUnproven,
			Action:     "No fills from TWS or other API clients have been observed for this account and API client. To include them, check that TWS Global Configuration → API → Settings → Master API client ID is Canary's client id (7); the owner decides that setting."},
		ByMonth:  []rpc.CostTotal{{Key: "2026-09", Fills: 4, Measured: 2, Unknown: 2, HalfSpreadBase: new(18.0)}},
		BySource: []rpc.CostTotal{{Key: rpc.FillQuoteSourceBroker, Fills: 2, Unknown: 2}, {Key: rpc.FillQuoteSourceCanary, Fills: 2, Measured: 2, HalfSpreadBase: new(18.0)}},
		ByClass:  []rpc.CostTotal{{Key: rpc.CostClassOption, Fills: 2, Measured: 1, Unknown: 1, HalfSpreadBase: new(9.0)}, {Key: rpc.CostClassStock, Fills: 2, Measured: 1, Unknown: 1, HalfSpreadBase: new(9.0)}},
		Fills: []rpc.CostFill{
			{ExecID: "0000e0d5.01", Source: rpc.FillQuoteSourceCanary, At: at, Symbol: "SYNX", LocalSymbol: "SYNX  261016C00100000", SecType: "OPT", Class: rpc.CostClassOption, Side: rpc.OrderActionBuy, Quantity: 2, Multiplier: 100, Price: 1.30, Currency: "USD", BaseCurrency: "EUR",
				Quote: quoted(1.20, 1.30, 2200), HalfSpread: new(10.0), HalfSpreadBase: new(9.0), FXRate: new(0.9), Status: rpc.CostFillMeasured},
			{ExecID: "0000e0d5.02", Source: rpc.FillQuoteSourceCanary, At: at.Add(5 * time.Minute), Symbol: "AAA", SecType: "STK", Class: rpc.CostClassStock, Side: rpc.OrderActionSell, Quantity: 100, Multiplier: 1, Price: 10.40, Currency: "USD", BaseCurrency: "EUR",
				Quote: quoted(10.40, 10.60, 1500), HalfSpread: new(10.0), HalfSpreadBase: new(9.0), FXRate: new(0.9), Status: rpc.CostFillMeasured},
			{ExecID: "0000e0d5.03", Source: rpc.FillQuoteSourceBroker, At: at.Add(time.Hour), Symbol: "BBB", SecType: "STK", Class: rpc.CostClassStock, Side: rpc.OrderActionBuy, Quantity: 10, Multiplier: 1, Price: 5, Currency: "USD", BaseCurrency: "EUR",
				Quote: rpc.FillQuote{Status: rpc.FillQuoteUnavailable, Reason: "timeout", ReadStartedAt: at.Add(time.Hour + time.Minute), ReadAt: at.Add(time.Hour + time.Minute + 4*time.Second)}, Status: rpc.CostFillUnknown, Reason: "timeout"},
			{ExecID: "0000e0d5.04", Source: rpc.FillQuoteSourceBroker, At: at.Add(2 * time.Hour), Symbol: "SYNX", LocalSymbol: "SYNX  261120P00090000", SecType: "OPT", Class: rpc.CostClassOption, Side: rpc.OrderActionSell, Quantity: 1, Price: 2, Currency: "USD", BaseCurrency: "EUR",
				Quote: quoted(1.90, 2.10, 40000), FXRate: new(0.9), Status: rpc.CostFillUnknown, Reason: "multiplier_unknown"},
		},
	}
}

func goldenReportingCostsEmpty() rpc.ReportingCostsResult {
	return rpc.ReportingCostsResult{
		SchemaVersion: rpc.ReportingCostsSchemaVersion, AsOf: goldenAt, Month: "2026-08", Mode: "paper",
		Observation: rpc.ExecutionObservation{Polling: true, IntervalSeconds: 60, Visibility: rpc.ExecutionVisibilityUnproven,
			Action: "No fills from TWS or other API clients have been observed for this account and API client. To include them, check that TWS Global Configuration → API → Settings → Master API client ID is Canary's client id (7); the owner decides that setting."},
		ByMonth: []rpc.CostTotal{}, BySource: []rpc.CostTotal{}, ByClass: []rpc.CostTotal{}, Fills: []rpc.CostFill{},
	}
}

type reportingCostsCLIConn struct {
	method string
	params rpc.ReportingCostsParams
	result rpc.ReportingCostsResult
}

func (c *reportingCostsCLIConn) Call(_ context.Context, method string, params any, out any) error {
	c.method = method
	if p, ok := params.(rpc.ReportingCostsParams); ok {
		c.params = p
	}
	*out.(*rpc.ReportingCostsResult) = c.result
	return nil
}

func (*reportingCostsCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestReportingCostsPassesTheMonthAndPrintsNoAccount(t *testing.T) {
	conn := &reportingCostsCLIConn{result: goldenReportingCosts()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "reporting", []string{"costs", "--month", "2026-09", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if conn.method != rpc.MethodReportingCosts || conn.params.Month != "2026-09" {
		t.Fatalf("call = %s %+v", conn.method, conn.params)
	}
	for _, required := range []string{`"half_spread_base": 9`, `"source": "broker"`, `"reason": "multiplier_unknown"`, `"visibility": "own_orders_unproven"`} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("JSON omitted %q: %s", required, stdout.String())
		}
	}
	var payload any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var checkAccountKeys func(any)
	checkAccountKeys = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if strings.Contains(strings.ToLower(key), "account") {
					t.Fatalf("JSON exposes account field %q", key)
				}
				checkAccountKeys(child)
			}
		case []any:
			for _, child := range v {
				checkAccountKeys(child)
			}
		}
	}
	checkAccountKeys(payload)
	if code := Run(t.Context(), env, "reporting", []string{"costs", "--month", "September"}); code == 0 {
		t.Fatal("a month that is not YYYY-MM must be refused")
	}
	// An envelope whose coverage disagrees with its fills never prints as facts.
	conn.result.Coverage.Quoted = 0
	if code := Run(t.Context(), env, "reporting", []string{"costs", "--json"}); code == 0 {
		t.Fatal("inconsistent coverage must fail")
	}
}
