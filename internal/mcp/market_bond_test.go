package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// canary_market with bond_identifier is the read-only bond check, parity
// with canary market --symbol <ISIN|CUSIP> --type BOND; the identifier is
// synthetic.
func TestMarketToolBondCheckParity(t *testing.T) {
	ask := 99.4
	want := rpc.MarketBondResult{Identifier: "DE000BU0ZZ19", IdentifierType: "ISIN", Currency: "EUR", Resolved: true, Lines: 1, Quoted: true,
		Contract: &rpc.BondContract{ConID: 7501, ISIN: "DE000BU0ZZ19", Class: rpc.BondClassBill, Currency: "EUR", PriceConvention: rpc.BondPriceConventionPer100},
		Quote:    &rpc.BondQuote{Ask: &ask, Fresh: true, DataType: "live", PriceConvention: rpc.BondPriceConventionPer100}}
	conn, calls := riskToolConn(t, map[string]any{rpc.MethodMarketBond: want})
	tool, ok := lookupTool("canary_market")
	if !ok {
		t.Fatal("missing canary_market")
	}
	raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{"bond_identifier":"DE000BU0ZZ19"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got rpc.MarketBondResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(<-calls, []string{rpc.MethodMarketBond}) {
		t.Fatalf("bond check = %+v", got)
	}
}
