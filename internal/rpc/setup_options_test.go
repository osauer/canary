package rpc

import (
	"math"
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
