package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// goldenStrategy is one synthetic vertical spread on AAA.
func goldenStrategy() rpc.PositionStrategy {
	leg := func(strike float64, qty float64, ratio, conID int) rpc.PositionStrategyLeg {
		return rpc.PositionStrategyLeg{Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Right: "C", Strike: strike, TradingClass: "AAA", ConID: conID}, Quantity: qty, Ratio: ratio}
	}
	return rpc.PositionStrategy{ID: "strat-aaa-1", Revision: 3, Underlying: "AAA", Kind: "vertical_spread", Source: rpc.PositionStrategySourceInferred,
		Status: rpc.PositionStrategyStatusCurrent, Units: 2, Legs: []rpc.PositionStrategyLeg{leg(100, 2, 1, 900001), leg(110, -2, -1, 900002)},
		PositionFingerprint: "fp-synthetic", GuaranteedCombo: true, Actionable: true}
}

// strategiesPositions is a positions read with the given authority, as the
// daemon serves it: an unavailable scope carries no rows and the zero time.
func strategiesPositions(scope rpc.AccountDataScope, availability rpc.AccountDataAvailability, freshness rpc.AccountDataFreshness, reason rpc.AccountDataReason, strategies []rpc.PositionStrategy) rpc.PositionsResult {
	res := rpc.PositionsResult{Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{}, Strategies: strategies,
		Authority: &rpc.AccountDataAuthority{Scope: scope, Source: rpc.AccountDataSourcePortfolioStream, Availability: availability, Freshness: freshness, Reason: reason}}
	if availability == rpc.AccountDataAvailable || freshness == rpc.AccountDataFreshnessStale {
		res.AsOf = goldenAt.Add(-time.Minute)
		res.Authority.AsOf = res.AsOf
	}
	return res
}

type strategiesAuthorityCase struct {
	name string
	res  rpc.PositionsResult
	// text is what the human view must say; absent what it must not.
	text, absent []string
}

func strategiesAuthorityCases() []strategiesAuthorityCase {
	account := rpc.AccountDataScope{AccountID: "U1234567", AccountMode: rpc.AccountModeLive}
	return []strategiesAuthorityCase{
		{name: "scope unresolved",
			res:    strategiesPositions(rpc.AccountDataScope{}, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonScopeUnresolved, nil),
			text:   []string{"Data note: one account has not been selected", "Option strategies unavailable."},
			absent: []string{"No option strategies can be grouped safely."}},
		{name: "account mismatch",
			res:    strategiesPositions(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonAccountMismatch, nil),
			text:   []string{"Option strategies  U1234567 · live", "Data note: the broker stream account does not match the selected account", "Option strategies unavailable."},
			absent: []string{"No option strategies can be grouped safely."}},
		{name: "scope conflict",
			res:    strategiesPositions(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonScopeConflict, nil),
			text:   []string{"Data note: the broker data belongs to a different account", "Option strategies unavailable."},
			absent: []string{"No option strategies can be grouped safely."}},
		{name: "initial download incomplete",
			res:    strategiesPositions(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessUnknown, rpc.AccountDataReasonUnprimed, nil),
			text:   []string{"Data note: the initial portfolio download is not complete", "Option strategies unavailable."},
			absent: []string{"No option strategies can be grouped safely."}},
		{name: "stale receipt keeps rows as context",
			res:    strategiesPositions(account, rpc.AccountDataUnavailable, rpc.AccountDataFreshnessStale, rpc.AccountDataReasonReceiptStale, []rpc.PositionStrategy{goldenStrategy()}),
			text:   []string{"Data note: the last complete portfolio receipt is stale", "strat-aaa-1  AAA  2 unit(s)  vertical spread  revision 3"},
			absent: []string{"No option strategies can be grouped safely.", "Option strategies unavailable."}},
		{name: "current and empty",
			res:    strategiesPositions(account, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", nil),
			text:   []string{"Option strategies  U1234567 · live", "No option strategies can be grouped safely."},
			absent: []string{"Data note", "unavailable"}},
		{name: "current with strategies",
			res:    strategiesPositions(account, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", []rpc.PositionStrategy{goldenStrategy()}),
			text:   []string{"strat-aaa-1  AAA  2 unit(s)  vertical spread  revision 3", "long 1 × 20261218 C 100.00 AAA  conID 900001", "short 1 × 20261218 C 110.00 AAA  conID 900002"},
			absent: []string{"Data note", "unavailable", "No option strategies"}},
	}
}

// The v3.25.4 review's finding 4: `strategies list` read an unavailable
// positions authority as an empty book. Text, JSON and exit code now carry
// the read's authority; an unavailable read exits 0 with its reason, as
// `canary positions` does.
func TestStrategiesListCarriesPositionsAuthority(t *testing.T) {
	for _, tc := range strategiesAuthorityCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := goldenConn{rpc.MethodPositionsList: tc.res}
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}, "strategies", []string{"list"}); code != 0 {
				t.Fatalf("text exit %d; stderr %s", code, stderr.String())
			}
			text := stdout.String()
			for _, want := range tc.text {
				if !strings.Contains(text, want) {
					t.Errorf("text missing %q:\n%s", want, text)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(text, unwanted) {
					t.Errorf("text shows %q:\n%s", unwanted, text)
				}
			}

			stdout.Reset()
			if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}, "strategies", []string{"list", "--json"}); code != 0 {
				t.Fatalf("json exit %d; stderr %s", code, stderr.String())
			}
			assertStrategiesViewJSON(t, stdout.Bytes(), tc.res)
		})
	}
}

// assertStrategiesViewJSON checks a strategies projection against the
// positions read it came from: the strategies as a list, never null, and the
// read's authority unchanged.
func assertStrategiesViewJSON(t *testing.T, raw []byte, res rpc.PositionsResult) {
	t.Helper()
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("%v: %s", err, raw)
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
	want := *res.Authority
	if got.Authority.Scope != want.Scope || got.Authority.Availability != want.Availability || got.Authority.Freshness != want.Freshness ||
		got.Authority.Reason != want.Reason || !got.Authority.AsOf.Equal(want.AsOf) || got.Authority.Source != want.Source {
		t.Fatalf("authority changed: got %+v want %+v", *got.Authority, want)
	}
	if len(got.Strategies) != len(res.Strategies) {
		t.Fatalf("strategies %d, want %d", len(got.Strategies), len(res.Strategies))
	}
}
