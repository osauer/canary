package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestMandatePendingPremiumInventory(t *testing.T) {
	for _, tc := range []struct {
		name                                                                      string
		journal, broker, self, partial, terminal, combo, unknown, other, modified bool
		want                                                                      float64
		why                                                                       string
	}{
		{name: "journal not yet echoed", journal: true, want: 800},
		{name: "broker other client", broker: true, want: 800},
		{name: "journal broker deduplicated", journal: true, broker: true, want: 800},
		{name: "unresolved price modification", journal: true, broker: true, modified: true, why: "modified option"},
		{name: "replacement excludes original", journal: true, broker: true, self: true},
		{name: "reflected partial fill counts remaining", journal: true, broker: true, partial: true, want: 400},
		{name: "filled after positions fails closed", journal: true, terminal: true, why: "filled after"},
		{name: "unknown combination gross premium", broker: true, combo: true, why: "combination"},
		{name: "unbounded working buy", broker: true, unknown: true, why: "bounded premium"},
		{name: "other account excluded", journal: true, broker: true, other: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			result, input, _, _ := s.mandateVerdicts.current()
			exposure := *result.Exposure
			exposure.AsOf = s.orderNow().Add(time.Second)
			draft := rpc.OrderDraft{Action: "BUY", OrderRef: "next", Quantity: 1, OrderType: "LMT", LimitPrice: 8, Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100}}
			held := draft
			held.OrderRef = "pending"
			status := s.currentTradingStatus()
			event := orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: held.OrderRef, PreviewTokenID: "pending-token", ReservedOrderID: 1001, ClientID: status.ClientID, Account: status.Account, Mode: status.Mode, Endpoint: status.Endpoint, Symbol: held.Contract.Symbol, SecType: held.Contract.SecType, ConID: held.Contract.ConID, Currency: held.Contract.Currency, Multiplier: held.Contract.Multiplier, Quantity: float64(held.Quantity), Action: held.Action, OrderType: held.OrderType, LimitPrice: held.LimitPrice}
			event.SendState = orderSendStateUncertainSend
			order := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1001, ClientID: event.ClientID, ClientIDPresent: true, Account: event.Account, Symbol: "SYNA", SecType: "OPT", ConID: 9009, Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
			if tc.modified {
				event.LimitPrice = 16
			}
			if tc.partial {
				event.Filled, event.Remaining = 0.5, 0.5
				order.Filled, order.Remaining = 0.5, 0.5
			}
			if tc.terminal {
				event.Type = orderJournalEventStatusUpdated
				event.Filled = 1
				event.Status = "Filled"
				exposure.AsOf = s.orderNow().Add(-time.Second)
			}
			if tc.combo {
				order.SecType = "BAG"
			}
			if tc.unknown {
				order.OrderType = "MKT"
				order.LimitPrice = 0
			}
			if tc.other {
				event.Account = "DU7654321"
				order.Account = event.Account
			}
			if tc.self {
				draft.OrderRef = held.OrderRef
			}
			if tc.journal {
				if err := s.orderJournal.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				snapshot := ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}
				if tc.broker {
					snapshot.Orders = []ibkr.OrderLifecycleEvent{order}
				}
				return snapshot, s.currentBrokerStateScope(), nil
			}
			got, err := s.mandatePendingPremium(draft, input, &exposure, "EUR")
			if tc.why != "" {
				if err == nil || !strings.Contains(err.Error(), tc.why) {
					t.Fatalf("got %v, want %s", err, tc.why)
				}
				return
			}
			if err != nil || math.Abs(got["SYNA"]-tc.want) > 1e-8 {
				t.Fatalf("premium=%v err=%v want=%g", got, err, tc.want)
			}
		})
	}
}

func TestMandatePendingPremiumBookAndUnavailableInventory(t *testing.T) {
	s, _ := mandateTestOverLossServer(t)
	result, input, _, _ := s.mandateVerdicts.current()
	exposure := *result.Exposure
	exposure.Names = nil
	exposure.Book.PremiumAtRiskBase = new(104000.0)
	resultCopy := *result
	resultCopy.Exposure = &exposure
	draft := rpc.OrderDraft{Action: "BUY", OrderRef: "next", Quantity: 1, OrderType: "LMT", LimitPrice: 8, Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100}}
	order := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1001, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{order}}, s.currentBrokerStateScope(), nil
	}
	blocker := s.mandatePremiumGate(draft, &resultCopy, input, "EUR", "")
	if blocker == nil || !strings.Contains(blocker.Message, "book's premium at risk would be 105,600") {
		t.Fatalf("book cap ignored other-name commitment: %+v", blocker)
	}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{}, s.currentBrokerStateScope(), errors.New("unavailable")
	}
	if blocker = s.mandatePremiumGate(draft, &resultCopy, input, "EUR", ""); blocker == nil || !strings.Contains(blocker.Message, "commitments are unavailable") {
		t.Fatalf("unknown commitments passed: %+v", blocker)
	}
}
