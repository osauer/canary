package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestWebLedgerCannotIncreaseTWSUsableCash(t *testing.T) {
	now := time.Now().UTC()
	scope := rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}
	account := &rpc.AccountResult{BaseCurrency: "EUR", Authority: &rpc.AccountDataAuthority{Scope: scope}}
	row := rpc.CurrencyExposure{Currency: "EUR", CashObserved: true, CashCcy: 7000, ExchangeRate: 123,
		WebCash: &rpc.WebCashObservation{Currency: "EUR", Scope: scope, CashBalance: 12000, SettledCash: 10000, AsOf: now}}
	got := cashSweepCashObservation(row, account, now)
	if got.TradeDate != 7000 || got.Settled == nil || *got.Settled != 10000 || got.ExchangeRate != 1 {
		t.Fatalf("cash/FX bound lost: %#v", got)
	}
	row.WebCash.CashBalance = 5000
	got = cashSweepCashObservation(row, account, now)
	if got.TradeDate != 5000 {
		t.Fatal("ignored lower Web cash balance")
	}
	row.SettledCashCcy = new(4000.0)
	got = cashSweepCashObservation(row, account, now)
	if *got.Settled != 4000 {
		t.Fatal("ignored lower native settled cash")
	}
}

func TestWebCashRequiresCurrentScopeTimeAndFiniteAmounts(t *testing.T) {
	now := time.Now().UTC()
	scope := rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}
	account := &rpc.AccountResult{BaseCurrency: "EUR", Authority: &rpc.AccountDataAuthority{Scope: scope}}
	for _, mutate := range []func(*rpc.WebCashObservation){
		func(w *rpc.WebCashObservation) { w.Scope.AccountMode = "live" },
		func(w *rpc.WebCashObservation) { w.Scope.AccountID = "DU7654321" },
		func(w *rpc.WebCashObservation) { w.Currency = "USD" },
		func(w *rpc.WebCashObservation) { w.AsOf = now.Add(-2 * time.Minute) },
		func(w *rpc.WebCashObservation) { w.AsOf = now.Add(time.Second) },
	} {
		web := &rpc.WebCashObservation{Currency: "EUR", Scope: scope, CashBalance: 12000, SettledCash: 10000, AsOf: now}
		mutate(web)
		row := rpc.CurrencyExposure{Currency: "EUR", CashObserved: true, CashCcy: 7000, WebCash: web}
		got := cashSweepCashObservation(row, account, now)
		if got.Settled != nil || got.SettledReason == "" || got.TradeDate != 7000 {
			t.Fatalf("uncertain Web cash admitted: %#v", got)
		}
	}
}
