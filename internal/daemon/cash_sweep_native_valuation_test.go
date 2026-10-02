package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepNativeValuationUsesFullAccountCashNotSpendingMinimum(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	acct := &rpc.AccountResult{BaseCurrency: "EUR", Authority: &rpc.AccountDataAuthority{AsOf: now, LedgerCurrencyCount: 2,
		Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashObserved: true, CashCcy: 20000},
		CurrencyExposure: []rpc.CurrencyExposure{{Currency: "USD", CashObserved: true, CashCcy: 100000,
			WebCash: &rpc.WebCashObservation{CashBalance: 1000, SettledCash: 1000}}}}
	values, at := cashSweepNativeValuationCash(acct)
	if !at.Equal(now) || values["EUR"] != 20000 || values["USD"] != 100000 {
		t.Fatalf("spending lower bound valued total NAV: %v at %v", values, at)
	}
	acct.CurrencyExposure[0].CashCcy = -100000
	values, _ = cashSweepNativeValuationCash(acct)
	if values["USD"] != -100000 {
		t.Fatal("native borrowing was replaced by zero")
	}
	for _, variant := range []string{"count", "unobserved", "nonfinite", "duplicate", "base", "fields"} {
		t.Run(variant, func(t *testing.T) {
			copy := *acct
			authority := *acct.Authority
			fields := *acct.Authority.Fields
			authority.Fields = &fields
			copy.Authority = &authority
			copy.CurrencyExposure = append([]rpc.CurrencyExposure(nil), acct.CurrencyExposure...)
			switch variant {
			case "count":
				copy.Authority.LedgerCurrencyCount = 3 // a filtered currency remains unknown
			case "unobserved":
				copy.CurrencyExposure[0].CashObserved = false
			case "nonfinite":
				copy.CurrencyExposure[0].CashCcy = math.Inf(1)
			case "duplicate":
				copy.CurrencyExposure[0].Currency = "EUR"
			case "base":
				copy.BaseCurrency = "USD"
			case "fields":
				copy.Authority.Fields.CurrencyExposure = false
			}
			if values, at := cashSweepNativeValuationCash(&copy); values != nil || !at.IsZero() {
				t.Fatalf("incomplete cash admitted for valuation: %v at %v", values, at)
			}
		})
	}
}
