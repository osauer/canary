package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAccountResultDataAuthorityPreservesObservedZeroAndMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	zero := 0.0
	raw := &ibkrlib.RawAccountSummary{
		AccountID: "DU123", AccountType: "CASH",
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		NetLiquidation: &zero,
	}
	result := &rpc.AccountResult{AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 0, AsOf: now}

	got := accountResultDataAuthority(
		brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper},
		raw, ibkrlib.AccountSummaryProvenanceRequest, result,
	)
	if got.Scope.AccountID != "DU123" || got.Scope.AccountMode != rpc.AccountModePaper {
		t.Fatalf("scope = %+v, want selected paper account", got.Scope)
	}
	if got.Source != rpc.AccountDataSourceAccountSummaryRequest || got.Availability != rpc.AccountDataAvailable || got.Freshness != rpc.AccountDataFreshnessCurrent {
		t.Fatalf("authority = %+v, want available current request", got)
	}
	if got.Fields == nil || !got.Fields.NetLiquidation {
		t.Fatalf("zero net liquidation was not retained as observed: %+v", got.Fields)
	}
	if got.Fields.TotalCash {
		t.Fatalf("missing total cash was marked available: %+v", got.Fields)
	}
	if !got.Fields.BaseCurrency {
		t.Fatalf("proven base currency was marked missing: %+v", got.Fields)
	}
}

func TestAccountResultDataAuthorityRefusesLegacyCurrencyAsBaseProof(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	raw := &ibkrlib.RawAccountSummary{
		AccountID: "DU123", Currency: "USD",
		BaseCurrency: "", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyUnknown,
	}
	result := &rpc.AccountResult{AccountID: "DU123", AsOf: now}

	got := accountResultDataAuthority(
		brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper},
		raw, ibkrlib.AccountSummaryProvenanceRequest, result,
	)
	if got.Fields == nil || got.Fields.BaseCurrency {
		t.Fatalf("legacy Currency fallback was accepted as base proof: %+v", got.Fields)
	}
}

func TestAccountResultDataAuthorityNamesUnstampedCache(t *testing.T) {
	t.Parallel()
	raw := &ibkrlib.RawAccountSummary{AccountID: "DU123"}
	got := accountResultDataAuthority(
		brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper},
		raw, ibkrlib.AccountSummaryProvenanceCachedFallback, &rpc.AccountResult{},
	)
	if got.Source != rpc.AccountDataSourceAccountUpdatesCache || got.Freshness != rpc.AccountDataFreshnessUnknown || got.Reason != rpc.AccountDataReasonUnstampedCache || !got.AsOf.IsZero() {
		t.Fatalf("cached authority = %+v, want unstamped/unknown", got)
	}
}

func TestPositionsResultDataAuthorityClassifiesPortfolioReceipt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	tests := []struct {
		name         string
		scope        brokerStateScope
		health       ibkrlib.PortfolioStreamHealth
		availability rpc.AccountDataAvailability
		freshness    rpc.AccountDataFreshness
		reason       rpc.AccountDataReason
	}{
		{name: "current", scope: scope, health: ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-time.Minute)}, availability: rpc.AccountDataAvailable, freshness: rpc.AccountDataFreshnessCurrent},
		{name: "stale", scope: scope, health: ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-portfolioStreamReceiptMaxAge - time.Second)}, availability: rpc.AccountDataUnavailable, freshness: rpc.AccountDataFreshnessStale, reason: rpc.AccountDataReasonReceiptStale},
		{name: "unprimed", scope: scope, health: ibkrlib.PortfolioStreamHealth{Account: "DU123"}, availability: rpc.AccountDataUnavailable, freshness: rpc.AccountDataFreshnessUnknown, reason: rpc.AccountDataReasonUnprimed},
		{name: "scope conflict", scope: scope, health: ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-time.Minute), ScopeConflictAt: now}, availability: rpc.AccountDataUnavailable, freshness: rpc.AccountDataFreshnessUnknown, reason: rpc.AccountDataReasonScopeConflict},
		{name: "unresolved selected account", scope: brokerStateScope{Account: "DU123,DU456", Mode: rpc.AccountModePaper}, health: ibkrlib.PortfolioStreamHealth{}, availability: rpc.AccountDataUnavailable, freshness: rpc.AccountDataFreshnessUnknown, reason: rpc.AccountDataReasonScopeUnresolved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := positionsResultDataAuthority(tc.scope, tc.health, now)
			if got.Source != rpc.AccountDataSourcePortfolioStream || got.Availability != tc.availability || got.Freshness != tc.freshness || got.Reason != tc.reason {
				t.Fatalf("authority = %+v, want availability=%q freshness=%q reason=%q", got, tc.availability, tc.freshness, tc.reason)
			}
			if !brokerScopeAccountConcrete(tc.scope.Account) && got.Scope.AccountID != "" {
				t.Fatalf("unresolved account inventory leaked into selected scope: %+v", got.Scope)
			}
		})
	}
}

func TestNewPositionsResultPopulatesSelectedAccountIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	result := newPositionsResult(scope, ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now}, now)
	if result.AccountID != "DU123" || result.Authority == nil || result.Authority.Scope.AccountID != "DU123" || result.Authority.Scope.AccountMode != rpc.AccountModePaper {
		t.Fatalf("positions identity = account_id %q authority %+v", result.AccountID, result.Authority)
	}
}
