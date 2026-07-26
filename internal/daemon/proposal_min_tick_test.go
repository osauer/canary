package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Xetra trades MiFID-banded grids: a €155 name ticks at 0.02, where the old
// hardcoded 0.01 produced broker-rejected prices (error 110). With the
// broker-resolved MinTick threaded through, both the trail amount and the
// initial stop land on the venue grid.
func TestTrailingStopStockProposalRoundsOnResolvedMinTick(t *testing.T) {
	t.Parallel()
	bid, ask := 154.80, 154.82
	row := rpc.PositionView{
		Symbol: "SAP", SecType: "STK", ConID: 14204, Quantity: 1,
		Bid: &bid, Ask: &ask, Mark: 154.81, Multiplier: 1, Currency: "EUR",
		Exchange: "IBIS",
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())

	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0.02)
	if !ok || prop.Trail == nil || prop.Trail.TrailingAmount == nil {
		t.Fatalf("proposal = %+v ok=%v, want trail proposal", prop, ok)
	}
	// With no dynamic ATR sizing supplied, the 10% fallback policy is used:
	// bid 154.80 * 10% = 15.48, already on the 0.02 grid.
	if *prop.Trail.TrailingAmount != 15.48 {
		t.Fatalf("trailing amount = %.4f, want 15.48 on the 0.02 grid", *prop.Trail.TrailingAmount)
	}
	if prop.Trail.InitialStopPrice != 139.32 {
		t.Fatalf("initial stop = %.4f, want 139.32 on the 0.02 grid", prop.Trail.InitialStopPrice)
	}
	if prop.TrailSizing == nil || !prop.TrailSizing.Fallback || prop.TrailSizing.ChosenPct != 10 {
		t.Fatalf("trail sizing = %+v, want 10%% fallback", prop.TrailSizing)
	}
	if prop.Contract.MinTick != 0.02 {
		t.Fatalf("contract min tick = %v, want 0.02 carried for preview-side rounding", prop.Contract.MinTick)
	}
}

// Position rows carry the canonical AssetType enum ("STOCK"), not the IBKR
// wire code. Sending the enum in the min-tick contract-details fetch made
// TWS reject every held stock row with error 321 "Please enter a valid
// security type" on each proposal refresh; the failure is never cached, so
// the rejection repeated every cadence (observed 2026-06-11). The fetch
// must carry the same wire-coded contract shape proposals hand to previews.
func TestRowMinTickContractCarriesWireSecType(t *testing.T) {
	t.Parallel()
	row := rpc.PositionView{
		Symbol: "AMD", SecType: rpc.SecTypeStock, ConID: 4391, Quantity: 150,
		Currency: "USD", Exchange: "NASDAQ", LocalSymbol: "AMD",
		TradingClass: "NMS", Multiplier: 1,
	}
	params := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	if params.SecType != "STK" {
		t.Fatalf("contract params SecType = %q, want STK", params.SecType)
	}
	wire := previewIBKRContract(params)
	if wire.SecType != "STK" {
		t.Fatalf("wire contract SecType = %q, want STK", wire.SecType)
	}
	if wire.Exchange != "SMART" || wire.PrimaryExch != "NASDAQ" {
		t.Fatalf("wire routing = %s/%s, want SMART routing with NASDAQ primary", wire.Exchange, wire.PrimaryExch)
	}
}

func TestPositionWireSecType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		rpc.SecTypeStock:  "STK",
		rpc.SecTypeOption: "OPT",
		"OPT":             "OPT",
		"opt":             "OPT",
		"ETF":             "ETF",
		"STK":             "STK",
		"":                "STK",
	}
	for in, want := range cases {
		if got := positionWireSecType(in); got != want {
			t.Errorf("positionWireSecType(%q) = %q, want %q", in, got, want)
		}
	}
}
