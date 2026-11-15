package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The v3.25.4 review's finding 4: canary_strategies kept only strategies,
// issues and as_of, so an unresolved or mismatched account scope reached an
// agent as null strategies and a zero time. The tool now carries the positions
// read's authority unchanged and never returns null strategies.
func TestStrategiesToolCarriesPositionsAuthority(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	account := rpc.AccountDataScope{AccountID: "U1234567", AccountMode: rpc.AccountModeLive}
	strategy := rpc.PositionStrategy{ID: "strat-aaa-1", Revision: 3, Underlying: "AAA", Kind: "vertical_spread", Source: rpc.PositionStrategySourceInferred,
		Status: rpc.PositionStrategyStatusCurrent, Units: 2, PositionFingerprint: "fp-synthetic", GuaranteedCombo: true, Actionable: true,
		Legs: []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Right: "C", Strike: 100, ConID: 900001}, Quantity: 2, Ratio: 1}}}
	read := func(scope rpc.AccountDataScope, availability rpc.AccountDataAvailability, freshness rpc.AccountDataFreshness, reason rpc.AccountDataReason, asOf time.Time, strategies []rpc.PositionStrategy) rpc.PositionsResult {
		return rpc.PositionsResult{AsOf: asOf, Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{}, Strategies: strategies,
			Authority: &rpc.AccountDataAuthority{Scope: scope, Source: rpc.AccountDataSourcePortfolioStream, Availability: availability, Freshness: freshness, Reason: reason, AsOf: asOf}}
	}
	for _, tc := range []struct {
		name        string
		res         rpc.PositionsResult
		unavailable bool
	}{
		{"scope unresolved", read(rpc.AccountDataScope{}, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonScopeUnresolved, time.Time{}, nil), true},
		{"account mismatch", read(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonAccountMismatch, time.Time{}, nil), true},
		{"initial download incomplete", read(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonUnprimed, time.Time{}, nil), true},
		{"stale receipt", read(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessStale, rpc.AccountDataReasonReceiptStale, at, []rpc.PositionStrategy{strategy}), true},
		{"current and empty", read(account, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", at, nil), false},
		{"current with strategies", read(account, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", at, []rpc.PositionStrategy{strategy}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, calls := riskToolConn(t, map[string]any{rpc.MethodPositionsList: tc.res})
			tool, ok := lookupTool("canary_strategies")
			if !ok {
				t.Fatal("canary_strategies missing")
			}
			raw, err := tool.Handler(t.Context(), conn, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(wire["strategies"])) == "null" {
				t.Fatalf("strategies is null: %s", raw)
			}
			var got rpc.PositionStrategiesView
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Authority == nil {
				t.Fatalf("authority dropped: %s", raw)
			}
			want := *tc.res.Authority
			if !reflect.DeepEqual(*got.Authority, want) {
				t.Fatalf("authority changed:\n got %+v\nwant %+v", *got.Authority, want)
			}
			if got.Unavailable() != tc.unavailable {
				t.Fatalf("unavailable = %v, want %v: %s", got.Unavailable(), tc.unavailable, raw)
			}
			if len(got.Strategies) != len(tc.res.Strategies) {
				t.Fatalf("strategies %d, want %d", len(got.Strategies), len(tc.res.Strategies))
			}
			if !reflect.DeepEqual(<-calls, []string{rpc.MethodPositionsList}) {
				t.Fatal("canary_strategies must read positions.list only")
			}
		})
	}
}
