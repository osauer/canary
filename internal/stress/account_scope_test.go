package stress

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func scopedStressInput() StressInput {
	scope := rpc.AccountDataScope{AccountID: "SYNTHETIC", AccountMode: rpc.AccountModePaper}
	authority := func(source rpc.AccountDataSource) *rpc.AccountDataAuthority {
		return &rpc.AccountDataAuthority{Scope: scope, Source: source, Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, AsOf: stressTestNow}
	}
	account := baseStressAccount()
	account.AccountID, account.AsOf = scope.AccountID, stressTestNow
	account.Authority = authority(rpc.AccountDataSourceAccountSummaryRequest)
	positions := freshStressPositions()
	positions.AccountID, positions.Authority = scope.AccountID, authority(rpc.AccountDataSourcePortfolioStream)
	return StressInput{Account: account, Positions: positions, Regime: healthyStressRegime(), Now: stressTestNow}
}

func TestComputeStressAccountScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*StressInput)
		bound  bool
	}{
		{"matching paper", func(*StressInput) {}, true},
		{"matching live", func(in *StressInput) {
			in.Account.Authority.Scope.AccountMode = rpc.AccountModeLive
			in.Positions.Authority.Scope.AccountMode = rpc.AccountModeLive
		}, true},
		{"account cache current", func(in *StressInput) { in.Account.Authority.Source = rpc.AccountDataSourceAccountUpdatesCache }, true},
		{"legacy account", func(in *StressInput) { in.Account.Authority = nil }, false},
		{"legacy positions", func(in *StressInput) { in.Positions.Authority = nil }, false},
		{"different accounts", func(in *StressInput) {
			in.Positions.Authority.Scope.AccountID = "OTHER"
			in.Positions.AccountID = "OTHER"
		}, false},
		{"different modes", func(in *StressInput) { in.Positions.Authority.Scope.AccountMode = rpc.AccountModeLive }, false},
		{"missing mode", func(in *StressInput) {
			in.Account.Authority.Scope.AccountMode = ""
			in.Positions.Authority.Scope.AccountMode = ""
		}, false},
		{"unknown mode", func(in *StressInput) {
			in.Account.Authority.Scope.AccountMode = "unknown"
			in.Positions.Authority.Scope.AccountMode = "unknown"
		}, false},
		{"missing account", func(in *StressInput) { setStressInputAccount(in, "") }, false},
		{"aggregate all", func(in *StressInput) { setStressInputAccount(in, "All") }, false},
		{"aggregate accounts", func(in *StressInput) { setStressInputAccount(in, "SYNTHETIC,OTHER") }, false},
		{"whitespace account", func(in *StressInput) { setStressInputAccount(in, "SYNTHETIC OTHER") }, false},
		{"account payload conflict", func(in *StressInput) { in.Account.AccountID = "OTHER" }, false},
		{"positions payload conflict", func(in *StressInput) { in.Positions.AccountID = "OTHER" }, false},
		{"account unavailable", func(in *StressInput) { in.Account.Authority.Availability = rpc.AccountDataUnavailable }, false},
		{"positions unavailable", func(in *StressInput) { in.Positions.Authority.Availability = rpc.AccountDataUnavailable }, false},
		{"account stale", func(in *StressInput) { in.Account.Authority.Freshness = rpc.AccountDataFreshnessStale }, false},
		{"positions unknown freshness", func(in *StressInput) { in.Positions.Authority.Freshness = rpc.AccountDataFreshnessUnknown }, false},
		{"unresolved authority", func(in *StressInput) { in.Account.Authority.Reason = rpc.AccountDataReasonScopeUnresolved }, false},
		{"wrong account source", func(in *StressInput) { in.Account.Authority.Source = rpc.AccountDataSourcePortfolioStream }, false},
		{"wrong positions source", func(in *StressInput) { in.Positions.Authority.Source = rpc.AccountDataSourceAccountSummaryRequest }, false},
		{"missing receipt", func(in *StressInput) { in.Account.Authority.AsOf = time.Time{} }, false},
		{"missing input timestamp", func(in *StressInput) { in.Positions.AsOf = time.Time{} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := scopedStressInput()
			tc.change(&in)
			result := ComputeStress(in)
			if (result.AccountScope != nil) != tc.bound {
				t.Fatalf("bound = %t, want %t", result.AccountScope != nil, tc.bound)
			}
			if tc.bound && *result.AccountScope != in.Account.Authority.Scope {
				t.Fatal("bound scope differs from source authority")
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if _, ok := wire["account_scope"]; ok != tc.bound {
				t.Fatal("wire scope does not match established binding")
			}
			if tc.bound {
				var scope rpc.AccountDataScope
				if err := json.Unmarshal(wire["account_scope"], &scope); err != nil || scope != *result.AccountScope {
					t.Fatal("wire scope lost account or mode")
				}
				in.Account.Authority.Scope.AccountID = "CHANGED"
				if result.AccountScope.AccountID == "CHANGED" {
					t.Fatal("result borrows mutable input authority")
				}
			}
		})
	}
}

func setStressInputAccount(in *StressInput, account string) {
	in.Account.AccountID, in.Positions.AccountID = account, account
	in.Account.Authority.Scope.AccountID, in.Positions.Authority.Scope.AccountID = account, account
}

func TestComputeStressAccountScopePreservesDecisionAndFingerprint(t *testing.T) {
	t.Parallel()
	in := scopedStressInput()
	scoped := ComputeStress(in)
	in.Account.Authority, in.Positions.Authority = nil, nil
	legacy := ComputeStress(in)
	if scoped.AccountScope == nil {
		t.Fatal("missing established account binding")
	}
	scoped.AccountScope = nil
	if !reflect.DeepEqual(scoped, legacy) {
		t.Fatal("scope binding changed risk result, source health, fingerprint, or established alert projection")
	}
}
