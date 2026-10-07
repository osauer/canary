//go:build trading

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashCommitmentBondDraftJournalKeepsIndependentTerms(t *testing.T) {
	scope, order, original, _ := syntheticWorkingBondCommitment()
	draft := rpc.OrderDraft{Action: order.Action, Quantity: int(order.TotalQuantity), OrderType: order.OrderType,
		LimitPrice: order.LimitPrice, TIF: order.TIF, OrderRef: order.OrderRef, Bond: original.Bond,
		Contract: rpc.ContractParams{SecType: order.SecType, ConID: order.ConID, Exchange: order.Exchange, Currency: order.Currency}}
	status := rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, Endpoint: original.Endpoint, ClientID: order.ClientID}
	event := orderJournalEventForDraft(draft, orderJournalEventSendAttempted, status, "synthetic-bond-token", order.OrderID, original.At)
	draft.Bond.AccruedBound, *draft.Bond.Coupon = 0, 0
	if event.Bond == nil || event.Bond.AccruedBound != 2000 || event.Bond.Coupon == nil || *event.Bond.Coupon != 5 {
		t.Fatalf("production send-event construction lost or aliases the signed terms: %+v", event.Bond)
	}
}

func TestCashCommitmentBondEngineReadsUnacknowledgedJournal(t *testing.T) {
	scope, _, event, evidence := syntheticWorkingBondCommitment()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	srv.now = func() time.Time { return evidence.Now }
	event.Endpoint, event.ClientID = "127.0.0.1:4002", 31
	if err := srv.orderJournal.Append(event); err != nil {
		t.Fatal(err)
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: evidence.Now}, scope, nil
	}
	engine := &proposalEngine{server: srv, now: srv.now, queued: &queuedAuthStore{},
		levelingRatesForTest: func(time.Time) (map[string]currencyLevelingRate, string, string) {
			return levelingRates(), "2026-10-05", ""
		}}
	acct := &rpc.AccountResult{AccountID: scope.Account, BaseCurrency: "EUR",
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: -50000, CashObserved: true, ExchangeRate: 1 / 1.17}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 60000, CashObserved: true, ExchangeRate: 1},
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: scope.Account, AccountMode: scope.Mode}, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: evidence.Now, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}}}
	in := engine.currencyLevelingInput(t.Context(), acct, scope, evidence.Now)
	if in.CommittedUnknown[""] == "" {
		t.Fatalf("production input erased a sent bond absent from the broker snapshot: %+v", in)
	}
	if b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD"); b != nil {
		t.Fatalf("unacknowledged bond left a repayment eligible: %+v", b)
	}
}
