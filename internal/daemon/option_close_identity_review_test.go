package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func closeIdentityOption(currency string) rpc.PositionView {
	return rpc.PositionView{Symbol: "SYNA", SecType: rpc.SecTypeOption, Currency: currency, Mark: 5, Quantity: 1, Multiplier: 100, Right: "C", Strike: 110, Expiry: "20261218"}
}

func TestOptionUnderlyingCloseSeparatesCurrenciesAndSharesMatchingStockClose(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	var mu sync.Mutex
	calls := map[string]int{}
	s.optionUnderlyingCloseForTest = func(_ context.Context, c rpc.ContractParams) (float64, bool) {
		mu.Lock()
		calls[c.Currency]++
		mu.Unlock()
		if c.Currency == "USD" {
			return 100, true
		}
		return 140, true
	}
	options := []rpc.PositionView{closeIdentityOption("USD"), closeIdentityOption("USD"), closeIdentityOption("CAD")}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, options)
	s.fillOptionUnderlyingPrevClose(options)
	for i, want := range []float64{100, 100, 140} {
		if options[i].PrevClose == nil || *options[i].PrevClose != want {
			t.Fatalf("option%d close=%v want%g", i, options[i].PrevClose, want)
		}
	}
	if calls["USD"] != 1 || calls["CAD"] != 1 {
		t.Fatalf("one request per currency wanted, got%v", calls)
	}
	stocks := []rpc.PositionView{{Symbol: "SYNA", SecType: rpc.SecTypeStock, Currency: "USD", Mark: 105}, {Symbol: "SYNA", SecType: rpc.SecTypeStock, Currency: "CAD", Mark: 145}}
	s.fillDailyChange(stocks)
	for i, want := range []float64{100, 140} {
		if stocks[i].PrevClose == nil || *stocks[i].PrevClose != want {
			t.Fatalf("stock%d close=%v want%g", i, stocks[i].PrevClose, want)
		}
	}
}

func TestOptionUnderlyingCloseHeldOtherCurrencyDoesNotSuppressRead(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	asked := false
	s.optionUnderlyingCloseForTest = func(_ context.Context, c rpc.ContractParams) (float64, bool) {
		asked = true
		if c.Currency != "CAD" {
			t.Errorf("wrong request %+v", c)
		}
		return 140, true
	}
	options := []rpc.PositionView{closeIdentityOption("CAD")}
	stocks := []rpc.PositionView{{Symbol: "SYNA", SecType: rpc.SecTypeStock, Currency: "USD"}}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, stocks, options)
	s.fillOptionUnderlyingPrevClose(options)
	if !asked || options[0].PrevClose == nil || *options[0].PrevClose != 140 {
		t.Fatalf("CAD option suppressed by USD stock: asked%v close%v", asked, options[0].PrevClose)
	}
}

func TestOptionUnderlyingCloseRejectsLegacyAndMissingCurrency(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	s.prevCloses.put("SYNA", prevCloseEntry{value: 99}, time.Now())
	options := []rpc.PositionView{closeIdentityOption("USD"), closeIdentityOption("")}
	stocks := []rpc.PositionView{{Symbol: "SYNA", SecType: rpc.SecTypeStock, Currency: "USD", Mark: 105}}
	s.fillOptionUnderlyingPrevClose(options)
	s.fillDailyChange(stocks)
	for _, row := range append(options, stocks...) {
		if row.PrevClose != nil {
			t.Fatalf("legacy unidentified close leaked into %q row", row.Currency)
		}
	}
	asked := false
	s.optionUnderlyingCloseForTest = func(context.Context, rpc.ContractParams) (float64, bool) { asked = true; return 100, true }
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, options[1:])
	s.fillOptionUnderlyingPrevClose(options[1:])
	if asked || options[1].PrevClose != nil {
		t.Fatal("missing currency was guessed")
	}
}

func TestOptionUnderlyingCloseFailedReadCannotReuseOtherCurrency(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	s.optionUnderlyingCloseForTest = func(_ context.Context, c rpc.ContractParams) (float64, bool) { return 100, c.Currency == "USD" }
	usd := []rpc.PositionView{closeIdentityOption("USD")}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, usd)
	cad := []rpc.PositionView{closeIdentityOption("CAD")}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, cad)
	s.fillOptionUnderlyingPrevClose(cad)
	if cad[0].PrevClose != nil {
		t.Fatalf("failed CAD read reused USD close %g", *cad[0].PrevClose)
	}
}

func TestOptionUnderlyingCloseUsesKnownIndexContract(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	var got []rpc.ContractParams
	var mu sync.Mutex
	s.optionUnderlyingCloseForTest = func(_ context.Context, c rpc.ContractParams) (float64, bool) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		return 6000, true
	}
	options := []rpc.PositionView{closeIdentityOption("USD"), closeIdentityOption("CAD"), closeIdentityOption("")}
	for i := range options {
		options[i].Symbol = "SPX"
	}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, options)
	s.fillOptionUnderlyingPrevClose(options)
	if len(got) != 1 || got[0].SecType != "IND" || got[0].Exchange != "CBOE" || got[0].Currency != "USD" {
		t.Fatalf("underlying request %+v want verified USD index", got)
	}
	if options[0].PrevClose == nil || *options[0].PrevClose != 6000 || options[1].PrevClose != nil || options[2].PrevClose != nil {
		t.Fatalf("index close crossed identity: %+v", options)
	}
}

func TestOptionUnderlyingCloseMissingCurrencyDoesNotRequestUSD(t *testing.T) {
	s := &Server{prevCloses: newPrevCloseCache()}
	asked := false
	s.optionUnderlyingCloseForTest = func(context.Context, rpc.ContractParams) (float64, bool) { asked = true; return 100, true }
	options := []rpc.PositionView{closeIdentityOption("")}
	s.prewarmOptionUnderlyingCloses(t.Context(), nil, nil, options)
	s.fillOptionUnderlyingPrevClose(options)
	if asked || options[0].PrevClose != nil {
		t.Fatal("missing currency requested and consumed an assumed USD close")
	}
}
