package canary_test

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
	"testing"
)

func TestSetupClientPreservesIdentityWithoutPolicyAuthority(t *testing.T) {
	s := canarytest.Serve(t)
	want := canary.SetupEvaluateParams{Spec: canary.SetupSpec{Revision: "test"}, Contract: canary.SetupContract{Symbol: "SYNTH", ConID: 17, SecType: "STK", Currency: "USD"}}
	s.Handle("setups.evaluate", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var got canary.SetupEvaluateParams
		if err := json.Unmarshal(raw, &got); err != nil || got != want {
			t.Fatalf("request changed: %+v %v", got, err)
		}
		return json.Marshal(canary.SetupResult{Version: 1, Spec: got.Spec, Contract: got.Contract, State: "unavailable", SetupMatch: nil})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	result, err := c.EvaluateSetup(t.Context(), want)
	if err != nil || result.Contract.ConID != 17 || result.SetupMatch != nil || result.PolicyChecked {
		t.Fatal(result, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "setups.evaluate" {
				t.Fatal("model tool unexpectedly enabled")
			}
		}
	}
}

func TestSetupOptionsClientPreservesExactDiscoveryTuple(t *testing.T) {
	s := canarytest.Serve(t)
	want := canary.SetupOptionsParams{Underlying: canary.SetupContract{Symbol: "SYNTH", ConID: 17, SecType: "STK", Currency: "USD", Exchange: "SMART"}, Expiry: "20351120", Strike: new(100.0)}
	s.Handle("setups.options", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var got canary.SetupOptionsParams
		if err := json.Unmarshal(raw, &got); err != nil || got.Underlying != want.Underlying || got.Expiry != want.Expiry || got.Strike == nil || *got.Strike != *want.Strike {
			t.Fatalf("discovery request changed: %+v %v", got, err)
		}
		return json.Marshal(canary.SetupOptionsResult{Version: 1, Underlying: got.Underlying, Expiries: []canary.SetupOptionExpiry{}, Calls: []canary.SetupOptionCall{}})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	out, err := c.DiscoverSetupOptions(t.Context(), want)
	if err != nil || out.Underlying.ConID != 17 || out.Expiries == nil || out.Calls == nil {
		t.Fatal(out, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "setups.options" {
				t.Fatal("model option-discovery tool unexpectedly enabled")
			}
		}
	}
}

func TestSetupOptionsClientCarriesExactCallQuote(t *testing.T) {
	s := canarytest.Serve(t)
	quote := canary.SetupOptionQuote{Bid: new(1.2), Ask: new(1.35), DataType: "live", Status: "quoted"}
	s.Handle("setups.options", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var got canary.SetupOptionsParams
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		call := canary.SetupContract{ConID: 900002, Symbol: "SYNX", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: got.Expiry, Strike: *got.Strike, Right: "C", Multiplier: 100}
		return json.Marshal(canary.SetupOptionsResult{Version: 1, Underlying: got.Underlying, Expiries: []canary.SetupOptionExpiry{}, Expiry: got.Expiry, Calls: []canary.SetupOptionCall{}, Contract: &call, Quote: &quote})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	out, err := c.DiscoverSetupOptions(t.Context(), canary.SetupOptionsParams{Underlying: canary.SetupContract{Symbol: "SYNX", ConID: 17, SecType: "STK", Currency: "USD", Exchange: "SMART"}, Expiry: "20351120", Strike: new(102.5)})
	if err != nil || out.Contract == nil || out.Quote == nil || out.Quote.Status != "quoted" || *out.Quote.Bid != 1.2 || *out.Quote.Ask != 1.35 || out.Quote.DataType != "live" {
		t.Fatal(out, err)
	}
}

func TestSetupCoverageClientForwardsFiltersWithoutModelTool(t *testing.T) {
	s := canarytest.Serve(t)
	want := canary.SetupCoverageParams{Session: "2026-09-30", Symbol: "SYNX"}
	s.Handle("setups.coverage", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var got canary.SetupCoverageParams
		if err := json.Unmarshal(raw, &got); err != nil || got != want {
			t.Fatalf("coverage request changed: %+v %v", got, err)
		}
		return json.Marshal(canary.SetupCoverageResult{Version: 1, SessionDate: got.Session, Sessions: []string{got.Session}, Contracts: []canary.SetupCoverageContract{{Symbol: "SYNX", Evaluations: 3, States: map[string]int{"watching": 3}}}})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	out, err := c.SetupCoverage(t.Context(), want)
	if err != nil || out.SessionDate != "2026-09-30" || len(out.Contracts) != 1 || out.Contracts[0].States["watching"] != 3 {
		t.Fatal(out, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "setups.coverage" {
				t.Fatal("model coverage tool unexpectedly enabled")
			}
		}
	}
}
