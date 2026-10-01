package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The sweep section shows cash-like figures, the settled-cash source, the
// resolved bill in the reason, and the evidence when no bill was named.
func TestRenderCashSweepPhaseBFigures(t *testing.T) {
	cash, eq, like, free := 60000.0, 9900.0, 69900.0, 55000.0
	st := &rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeActive, TaxReviewed: true, TaxReviewedAt: "2026-09-30", Currencies: []rpc.TradeProposalCashSweepCurrency{
		{Currency: "USD", State: rpc.CashSweepStateInvest, Cash: &cash, CashEquivalents: &eq, CashLike: &like, Free: &free, SettledCashSource: rpc.CashSweepSettledSourceBroker,
			Reason: "free cash is above min_tranche; the bill is CUSIP 912797ZZ3 maturing 2026-11-04 (35 days), quoted 99.6000 per 100 of face"},
		{Currency: "EUR", State: rpc.CashSweepStateInstrumentUnresolved, Reason: "none of the 1 listed EUR isins resolves to a bill maturing within 28–182 days",
			Evidence: []string{"DE000BU0ZZ19: matures 2026-10-10 (10 days), outside 28–182 days"}},
	}}
	var buf bytes.Buffer
	renderCashSweepSection(&Env{Stdout: &buf, Stderr: &buf}, &buf, st, nil)
	out := buf.String()
	for _, want := range []string{"tax reviewed 2026-09-30", "cash-like $ 69,900.00", "settled cash from broker", "CUSIP 912797ZZ3", "EUR  instrument unresolved", "DE000BU0ZZ19: matures 2026-10-10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

type bondCLIConn struct {
	method string
	params rpc.MarketBondParams
	result rpc.MarketBondResult
}

func (c *bondCLIConn) Call(_ context.Context, method string, p, out any) error {
	c.method = method
	if params, ok := p.(rpc.MarketBondParams); ok {
		c.params = params
		*out.(*rpc.MarketBondResult) = c.result
		return nil
	}
	return errors.New("unexpected call")
}

func (*bondCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}

// canary market --symbol <ISIN|CUSIP> --type BOND calls the bond check, keeps
// the currency empty unless given, and prints text or JSON; a gap reads as
// a gap, never a zero.
func TestMarketBondCLI(t *testing.T) {
	ask, days, minSize := 99.6, 35, 1000.0
	resolved := rpc.MarketBondResult{Identifier: "912797ZZ3", IdentifierType: "CUSIP", Currency: "USD", Resolved: true, Lines: 1, Quoted: true,
		Contract: &rpc.BondContract{ConID: 7101, CUSIP: "912797ZZ3", ISIN: "US912797ZZ37", Class: rpc.BondClassBill, Currency: "USD", Maturity: "2026-11-04", DaysToMaturity: &days,
			MinSize: &minSize, QuantityUnit: rpc.BondQuantityUnitFace1000, PriceConvention: rpc.BondPriceConventionPer100},
		Quote: &rpc.BondQuote{Ask: &ask, DataType: "live", Fresh: true}}
	conn := &bondCLIConn{result: resolved}
	var out bytes.Buffer
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "912797ZZ3", "--type", "BOND"}); code != 0 {
		t.Fatalf("code %d: %s", code, &out)
	}
	if conn.method != rpc.MethodMarketBond || conn.params.Identifier != "912797ZZ3" || conn.params.Currency != "" {
		t.Fatalf("call = %s %+v", conn.method, conn.params)
	}
	for _, want := range []string{"Bond  912797ZZ3 · CUSIP · USD", "con_id 7101", "bill", "matures 2026-11-04 (35 days)", "min size 1000", "unit face_1000 (assumed)", "ask 99.6000", "bid —", "live feed, fresh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, &out)
		}
	}
	out.Reset()
	conn.result = rpc.MarketBondResult{Identifier: "DE000BU0ZZ19", IdentifierType: "ISIN", Currency: "EUR", Reason: "contract details: IBKR lists no such bond line"}
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "DE000BU0ZZ19", "--type", "bond", "--currency", "EUR", "--json"}); code != 0 {
		t.Fatalf("code %d: %s", code, &out)
	}
	var got rpc.MarketBondResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Resolved || got.Contract != nil || conn.params.Currency != "EUR" {
		t.Fatalf("json = %s (%v), params %+v", &out, err, conn.params)
	}
	out.Reset()
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--type", "BOND"}); code == 0 {
		t.Fatal("a bond check without an identifier ran")
	}
	if conn.params.SecType != "BOND" {
		t.Fatalf("--type bond asked %q", conn.params.SecType)
	}
}

func TestMarketBondSessionCLI(t *testing.T) {
	const identifier = "912797ZZ3"
	open := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		session *rpc.BondSession
		want    []string
	}{
		{name: "broker", session: &rpc.BondSession{Source: rpc.BondSessionSourceLiquidHours, TimeZone: "America/New_York", Windows: []rpc.BondSessionWindow{{Open: open, Close: open.Add(9 * time.Hour)}}},
			want: []string{"Session    liquid_hours · America/New_York", "2026-10-01T12:00:00Z → 2026-10-01T21:00:00Z"}},
		{name: "assumed", session: &rpc.BondSession{Source: rpc.BondSessionSourceAssumed, TimeZone: "America/New_York", Windows: []rpc.BondSessionWindow{{Open: open, Close: open.Add(9 * time.Hour)}}},
			want: []string{"Session    assumed · America/New_York"}},
		{name: "closed", session: &rpc.BondSession{Source: rpc.BondSessionSourceTradingHours, TimeZone: "America/New_York"}, want: []string{"Session    trading_hours", "no trading windows"}},
		{name: "unknown", want: []string{"Session    unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &bondCLIConn{result: rpc.MarketBondResult{Identifier: identifier, IdentifierType: "CUSIP", Currency: "USD", Resolved: true,
				Contract: &rpc.BondContract{ConID: 7101, SecType: "BILL", Class: rpc.BondClassBill, Currency: "USD"}, Session: tc.session}}
			var out bytes.Buffer
			if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", identifier, "--type", "BILL"}); code != 0 {
				t.Fatalf("text code %d: %s", code, &out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in:\n%s", want, &out)
				}
			}
			out.Reset()
			if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", identifier, "--type", "BILL", "--json"}); code != 0 {
				t.Fatalf("json code %d: %s", code, &out)
			}
			var got rpc.MarketBondResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if (got.Session == nil) != (tc.session == nil) || tc.session != nil && (got.Session.Source != tc.session.Source || got.Session.TimeZone != tc.session.TimeZone || len(got.Session.Windows) != len(tc.session.Windows)) {
				t.Fatalf("JSON dropped session evidence: %+v", got.Session)
			}
		})
	}
}

// canary market --type BILL asks the daemon for a bill; a BOND check of a
// bill identifier prints the types asked and why, the type the line
// resolved as, and a gap line naming each attempt.
func TestMarketBillCLI(t *testing.T) {
	ask := 99.6
	conn := &bondCLIConn{result: rpc.MarketBondResult{Identifier: "912797SK4", IdentifierType: "CUSIP", Currency: "USD", SecTypes: []string{"BILL"}, Resolved: true, Lines: 1, Quoted: true,
		Contract: &rpc.BondContract{ConID: 7101, SecType: "BILL", CUSIP: "912797SK4", Class: rpc.BondClassBill, Currency: "USD", PriceConvention: rpc.BondPriceConventionPer100},
		Quote:    &rpc.BondQuote{Ask: &ask, DataType: "live", Fresh: true}}}
	var out bytes.Buffer
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "912797SK4", "--type", "bill"}); code != 0 {
		t.Fatalf("code %d: %s", code, &out)
	}
	if conn.method != rpc.MethodMarketBond || conn.params.SecType != "BILL" || conn.params.Identifier != "912797SK4" {
		t.Fatalf("call = %s %+v", conn.method, conn.params)
	}
	for _, want := range []string{"Asked      as BILL", "con_id 7101 · BILL · bill"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, &out)
		}
	}
	out.Reset()
	note := "asked as BOND, then as BILL: CUSIP 912797SK4 reads as a us_tbill bill, which Canary asks as BILL"
	gap := `contract details: IBKR lists no such bond line (BOND then BILL CUSIP 912797SK4 on SMART in USD; BOND by symbol: the search ended without a line; BOND by secIdType CUSIP: IBKR 200 "No security definition has been found for the request"; BILL by symbol: IBKR 200 "No security definition has been found for the request"; BILL by secIdType CUSIP: IBKR 200 "No security definition has been found for the request")`
	conn.result = rpc.MarketBondResult{Identifier: "912797SK4", IdentifierType: "CUSIP", Currency: "USD", SecTypes: []string{"BOND", "BILL"}, SecTypesNote: note, Reason: gap}
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "912797SK4", "--type", "BOND"}); code != 0 {
		t.Fatalf("code %d: %s", code, &out)
	}
	for _, want := range []string{"Asked      " + note, "Resolved   no (0 lines)", "Gap        " + gap} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, &out)
		}
	}
	// --json carries the lookup's attempts and the frames IBKR answered
	// each with.
	out.Reset()
	conn.result.Attempts = []rpc.BondLookupAttempt{{Form: "BILL by symbol, no exchange", ReqID: 43, SecType: "BILL", Symbol: "912797ZZ3", Currency: "USD", Outcome: rpc.BondAttemptNoLine,
		Message: "the search ended without a line: no frame named the request before its end marker", Frames: []rpc.BondLookupFrame{{MsgID: 52, Kind: "contractDataEnd", Fields: 4}}}}
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "912797SK4", "--type", "BILL", "--json"}); code != 0 {
		t.Fatalf("code %d: %s", code, &out)
	}
	for _, want := range []string{`"attempts": [`, `"form": "BILL by symbol, no exchange"`, `"exchange": ""`, `"outcome": "no_line"`, `"kind": "contractDataEnd"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s in:\n%s", want, &out)
		}
	}
	out.Reset()
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"--type", "BILL", "--watch"}); code == 0 || !strings.Contains(out.String(), "--type BILL needs --symbol") {
		t.Fatalf("a BILL watch ran: %d %s", code, &out)
	}
}

// canary positions prints classified bonds in their own section and keeps
// every other stock row (and any unclassified BOND row) in the stock table.
func TestPositionsTextBondsSection(t *testing.T) {
	days := 35
	r := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "SYNTH", SecType: "STK", ConID: 1, Currency: "USD", Quantity: 10, Mark: 50, MarketValue: 500},
			{Symbol: "SYNTHB", SecType: "BOND", ConID: 7401, Currency: "USD", Quantity: 5, Mark: 99.5, MarketValue: 4975},
			{Symbol: "SYNTHX", SecType: "BOND", ConID: 7404, Currency: "CAD", Quantity: 1000, Mark: 99, MarketValue: 990},
		},
		Options: []rpc.PositionView{},
		Bonds:   []rpc.PositionBond{{ConID: 7401, Symbol: "SYNTHB", Currency: "USD", Class: rpc.BondClassBill, CUSIP: "912797ZZ3", Maturity: "2026-11-04", DaysToMaturity: &days, Quantity: 5, Mark: 99.5, MarketValue: 4975}},
	}
	stocks := positionsStockTableRows(r)
	if len(stocks) != 2 || stocks[0].ConID != 1 || stocks[1].ConID != 7404 || len(r.Stocks) != 3 {
		t.Fatalf("stock table rows = %+v", stocks)
	}
	var buf bytes.Buffer
	renderBondsTable(&Env{Stdout: &buf, Stderr: &buf}, &buf, r.Bonds)
	out := buf.String()
	for _, want := range []string{"Bills & bonds", "SYNTHB", "bill", "912797ZZ3", "2026-11-04", "35"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	buf.Reset()
	renderBondsTable(&Env{Stdout: &buf, Stderr: &buf}, &buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("empty bonds rendered: %s", &buf)
	}
}
