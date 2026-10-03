package rpc

import (
	"math"
	"testing"
	"time"
)

func TestSetupContractRejectsUnsupportedRequests(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	base := SetupEvaluateParams{Spec: SetupSpec{Revision: "test"}, Contract: ContractParams{Symbol: "SYNTH"}}
	for _, tc := range []struct {
		name   string
		change func(*SetupEvaluateParams)
	}{
		{"template", func(p *SetupEvaluateParams) { p.Spec.Template = "expression" }},
		{"revision", func(p *SetupEvaluateParams) { p.Spec.Revision = "" }},
		{"NaN", func(p *SetupEvaluateParams) { p.Spec.SpikeMultiple = math.NaN() }},
		{"window", func(p *SetupEvaluateParams) { p.Spec.ResponseBars = 7 }},
		{"baseline", func(p *SetupEvaluateParams) { p.Spec.BaselineSessions = 19 }},
		{"future", func(p *SetupEvaluateParams) { p.At = now.Add(time.Second) }},
		{"too old", func(p *SetupEvaluateParams) { p.At = now.AddDate(-2, 0, 0) }},
		{"option", func(p *SetupEvaluateParams) { p.Contract.SecType = "OPT" }},
		{"multiple", func(p *SetupEvaluateParams) { p.Contract.Symbol = "AAA,BBB" }},
		{"wire delimiter", func(p *SetupEvaluateParams) { p.Contract.Symbol = "SYN\x00TH" }},
		{"nonascii", func(p *SetupEvaluateParams) { p.Contract.Symbol = "SYNΘ" }},
		{"oversized identity", func(p *SetupEvaluateParams) { p.Contract.ConID = math.MaxInt32 + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.change(&p)
			if _, err := NormalizeSetupEvaluateParams(p, now); err == nil {
				t.Fatal("accepted unsupported request")
			}
		})
	}
	p, err := NormalizeSetupEvaluateParams(base, now)
	if err != nil || p.Spec.SpikeMultiple != 3 || p.Spec.ResponseBars != 2 || p.Spec.BaselineSessions != 20 {
		t.Fatal(p, err)
	}
}
