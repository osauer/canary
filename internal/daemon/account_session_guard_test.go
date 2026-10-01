package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAccountSummaryFinalSessionGuardRetiresAllAuthorityTogether(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, tc := range []struct {
		name    string
		scope   brokerStateScope
		session bool
	}{
		{"account changed", brokerStateScope{Account: "DU7654321", Mode: "paper"}, true},
		{"mode changed", brokerStateScope{Account: scope.Account, Mode: "live"}, true},
		{"same-account socket replaced", scope, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web := &rpc.WebCashObservation{Currency: "USD", Scope: accountDataScope(scope), CashBalance: 12000, SettledCash: 9000, AsOf: now}
			res := &rpc.AccountResult{AccountID: scope.Account, NetLiquidation: 100000, AsOf: now,
				Authority: &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable,
					Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now, Source: rpc.AccountDataSourceAccountSummaryRequest},
				BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "USD", CashObserved: true, CashCcy: 12000, WebCash: web},
				CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashObserved: true, CashCcy: 12000, WebCash: web}},
				CashLedger:         &rpc.CashLedgerHealth{Source: "ibkr-web-api", Status: "ok", AsOf: now}}
			authority := accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: now,
				NetLiquidationAvailable: true, TotalCashAvailable: true, AvailableFundsAvailable: true, BaseCurrencyAvailable: true,
				ExcessLiquidityAvailable: true, InitialMarginAvailable: true, MaintenanceMarginAvailable: true}
			got := finalizeAccountSummarySession(res, authority, tc.session && sameBrokerScope(scope, tc.scope))
			if got != (accountSummaryAuthority{}) || res.Authority.Availability != rpc.AccountDataUnavailable ||
				res.Authority.Freshness != rpc.AccountDataFreshnessUnknown || res.Authority.Reason != rpc.AccountDataReasonSessionChanged {
				t.Fatalf("old session retained account decision authority: %+v / %+v", got, res.Authority)
			}
			if res.BaseCurrencyLedger.WebCash != nil || res.CurrencyExposure[0].WebCash != nil ||
				res.CashLedger.Status != "unavailable" || !res.CashLedger.AsOf.IsZero() || res.CashLedger.Reason == "" {
				t.Fatalf("old session retained supplemental cash authority: %+v", res)
			}
			if res.AccountID != scope.Account || res.NetLiquidation != 100000 || !res.AsOf.Equal(now) || res.CurrencyExposure[0].CashCcy != 12000 {
				t.Fatal("retiring authority destroyed the prior account's display context")
			}
		})
	}
}

func TestAccountSummaryFinalSessionGuardPreservesCurrentEvidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	authority := accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: now, TotalCashAvailable: true}
	res := &rpc.AccountResult{AccountID: "DU1234567", Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable},
		CashLedger: &rpc.CashLedgerHealth{Status: "partial", AsOf: now}}
	before := *res
	if got := finalizeAccountSummarySession(res, authority, true); got != authority || !reflect.DeepEqual(*res, before) {
		t.Fatal("current account evidence was modified")
	}
	if got := finalizeAccountSummarySession(nil, authority, false); got != (accountSummaryAuthority{}) {
		t.Fatal("an absent result retained internal authority")
	}
}
