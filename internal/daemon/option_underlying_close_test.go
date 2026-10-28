package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// An option on a name the book holds only through options used to get its
// underlying price from the broker's option-model tick alone: the cached
// previous close was filled only while prewarming held stock lines. After a
// restart outside market hours no tick arrives, the row carried neither
// price, and one such leg left the whole mandate verdict unknown, which
// refused every addition and the index hedge's sizing (live book,
// 2026-10-28 21:23 CET). The underlying's close is now read for every
// option-only name as it is for a held stock.
func TestOptionOnlyUnderlyingGetsItsClose(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	asked := map[string]int{}
	s.optionUnderlyingCloseForTest = func(_ context.Context, c rpc.ContractParams) (float64, bool) {
		asked[c.Symbol]++
		if c.SecType != "STK" || c.Exchange != "SMART" || c.Currency != "USD" {
			t.Errorf("underlying contract = %+v, want a SMART USD stock", c)
		}
		return 7.5, true
	}
	stocks := []rpc.PositionView{mandateReductionStock("SYNA", 9101, 100, 50)}
	stocks[0].Currency = "USD"
	call := rpc.PositionView{Symbol: "SYNO", SecType: rpc.SecTypeOption, ConID: 9201, Exchange: "SMART", Currency: "USD",
		Quantity: 8, Multiplier: 100, Mark: 0.4, Expiry: "20261120", Strike: 8, Right: "C", DataType: rpc.MarketDataLive}
	held := rpc.PositionView{Symbol: "SYNA", SecType: rpc.SecTypeOption, ConID: 9202, Exchange: "SMART", Currency: "USD",
		Quantity: 1, Multiplier: 100, Mark: 2, Expiry: "20261120", Strike: 55, Right: "C", DataType: rpc.MarketDataLive}
	options := []rpc.PositionView{call, call, held}
	options[1].ConID = 9203

	s.prewarmOptionUnderlyingCloses(t.Context(), nil, stocks, options)
	s.fillOptionUnderlyingPrevClose(options)

	if asked["SYNO"] != 1 || asked["SYNA"] != 0 {
		t.Fatalf("asked %v, want the option-only name once and the held stock left to its own prewarm", asked)
	}
	if options[0].PrevClose == nil || *options[0].PrevClose != 7.5 || options[1].PrevClose == nil {
		t.Fatalf("option rows carry no underlying close: %+v", options[0].PrevClose)
	}
	legs, _, _, reasons := mandateLegsFromPositions(&rpc.PositionsResult{Stocks: stocks, Options: options[:1]}, "USD", mandateTestNow)
	if len(reasons) > 0 {
		t.Fatal(reasons)
	}
	for _, l := range legs {
		if l.Symbol != "SYNA" && !(l.Spot == 7.5 && l.IV != nil) {
			t.Fatalf("option leg = %+v, want priced at the underlying close with a mark-implied volatility", l)
		}
	}
}

// Outside regular hours IBKR sends an option's model figures (implied
// volatility, delta, underlying price) only while its underlying streams:
// on the live book 2026-10-28 21:33 CET every option whose underlying
// Canary streamed (a held stock, SPY for the regime) had them after a
// restart, and the three on names held only through options had none. The
// underlying of every option-only name is now held for the whole capture.
func TestOptionOnlyUnderlyingIsHeldDuringTheGreeksCapture(t *testing.T) {
	s := &Server{}
	held := map[string]int{}
	s.optionUnderlyingHoldForTest = func(_ context.Context, sym string) (func(), error) {
		held[sym]++
		return func() { held[sym]-- }, nil
	}
	stocks := []rpc.PositionView{mandateReductionStock("SYNA", 9101, 100, 50)}
	options := []rpc.PositionView{
		{Symbol: "SYNO", SecType: rpc.SecTypeOption, Currency: "USD", Expiry: "20261120", Strike: 8, Right: "C"},
		{Symbol: "SYNO", SecType: rpc.SecTypeOption, Currency: "USD", Expiry: "20261120", Strike: 9, Right: "C"},
		{Symbol: "SYNA", SecType: rpc.SecTypeOption, Currency: "USD", Expiry: "20261120", Strike: 55, Right: "C"},
	}
	ran := false
	s.withOptionOnlyUnderlyingsHeld(t.Context(), stocks, options, func() {
		ran = true
		if held["SYNO"] != 1 || held["SYNA"] != 0 {
			t.Fatalf("during the capture held = %v, want the option-only name once and the held stock left alone", held)
		}
	})
	if !ran || held["SYNO"] != 0 {
		t.Fatalf("ran %v, held after = %v, want the capture run and every hold released", ran, held)
	}
}
