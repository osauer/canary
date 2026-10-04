package rpc

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSetupOptionsRequireExactUnderlyingAndValidTuple(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	base := SetupOptionsParams{Underlying: ContractParams{Symbol: "SYNTH", ConID: 17}}
	if p, err := NormalizeSetupOptionsParams(base, now); err != nil || p.Underlying.Exchange != "SMART" {
		t.Fatal(p, err)
	}
	for _, change := range []func(*SetupOptionsParams){
		func(p *SetupOptionsParams) { p.Underlying.ConID = 0 },
		func(p *SetupOptionsParams) { p.Underlying.Currency = "EUR" },
		func(p *SetupOptionsParams) { p.Expiry = "20261001" },
		func(p *SetupOptionsParams) { p.Expiry = "20260230" },
		func(p *SetupOptionsParams) { p.Strike = new(100.0) },
		func(p *SetupOptionsParams) { p.Expiry = "20261120"; p.Strike = new(math.NaN()) },
		func(p *SetupOptionsParams) { p.Expiry = "20261120"; p.Strike = new(0.0) },
	} {
		p := base
		change(&p)
		if _, err := NormalizeSetupOptionsParams(p, now); err == nil {
			t.Fatal("invalid option discovery request admitted", p)
		}
	}
	// Date validation is the New York date, not the following UTC date.
	base.Expiry = "20261002"
	if _, err := NormalizeSetupOptionsParams(base, now); err != nil {
		t.Fatal(err)
	}
}

func TestSetupOptionQuoteAppearsOnlyAtExactSelectionAndMissingCarriesNoPrices(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	listing := SetupOptionsResult{Version: 1, Underlying: ContractParams{Symbol: "SYNX", ConID: 17}, AsOf: asOf, Expiries: []SetupOptionExpiry{}, Expiry: "20261120", Calls: []SetupOptionCall{{Strike: 100, Status: SetupQuoteNotRequested}}}
	raw, err := json.Marshal(listing)
	if err != nil || strings.Contains(string(raw), `"quote"`) || !strings.Contains(string(raw), `{"strike":100,"status":"not_requested"}`) {
		t.Fatalf("listing stage gained a quote or row prices: %s %v", raw, err)
	}
	for _, tc := range []struct {
		quote SetupOptionQuote
		want  string
	}{
		{SetupOptionQuote{Status: SetupQuoteMissing}, `{"status":"missing"}`},
		{SetupOptionQuote{Bid: new(1.2), Ask: new(1.35), AsOf: asOf, DataType: MarketDataDelayed, Status: SetupQuoteQuoted}, `{"bid":1.2,"ask":1.35,"as_of":"2026-10-02T15:00:00Z","data_type":"delayed","status":"quoted"}`},
	} {
		exact := listing
		exact.Calls = []SetupOptionCall{}
		exact.Contract = &ContractParams{ConID: 900002, Symbol: "SYNX", SecType: "OPT", Expiry: "20261120", Strike: 102.5, Right: "C", Multiplier: 100}
		exact.Quote = &tc.quote
		raw, err := json.Marshal(exact)
		if err != nil || !strings.Contains(string(raw), `"quote":`+tc.want) {
			t.Fatalf("exact selection quote shape changed: %s %v", raw, err)
		}
	}
}
