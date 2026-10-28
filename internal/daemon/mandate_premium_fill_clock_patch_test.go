package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPremiumFillClockSeparatesNewFillFromRepeatedCallback(t *testing.T) {
	for _, newFill := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated callback", true: "new fill"}[newFill], func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			result, input, _, _ := s.currentMandateVerdict()
			exposure := *result.Exposure
			exposure.AsOf = s.orderNow().Add(-time.Minute)
			scope := s.currentBrokerStateScope()
			seed := orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, At: s.orderNow().Add(-2 * time.Minute), OrderRef: "part-filled", PreviewTokenID: "seed", ReservedOrderID: 1001, PermID: 90001, ClientID: 31, Endpoint: "127.0.0.1:4002", Account: scope.Account, Mode: scope.Mode, Symbol: "SYNA", SecType: "OPT", ConID: 9009, Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, Quantity: 3, Filled: 1, Remaining: 2, Status: "Submitted"}
			if err := s.orderJournal.Append(seed); err != nil {
				t.Fatal(err)
			}
			echo := seed
			echo.Type = orderJournalEventStatusUpdated
			echo.At = s.orderNow()
			echo.OrderRef = "" // broker callback identity must alias the reference
			if newFill {
				echo.Filled = 2
				echo.Remaining = 1
			}
			if err := s.orderJournal.Append(echo); err != nil {
				t.Fatal(err)
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1001, PermID: 90001, ClientID: 31, ClientIDPresent: true, Account: scope.Account, ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, TotalQuantity: 3, Filled: echo.Filled, Remaining: echo.Remaining, Status: "Submitted"}}}, scope, nil
			}
			draft := rpc.OrderDraft{OrderRef: "part-filled", Action: "BUY", Quantity: 1, LimitPrice: 7, Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100}}
			_, err := s.mandatePendingPremium(draft, input, &exposure, "EUR")
			if newFill {
				if err == nil || !strings.Contains(err.Error(), "filled after") {
					t.Fatalf("new fill bypassed own-order guard: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unchanged cumulative fill became a new fill: %v", err)
			}
		})
	}
}

func TestPremiumFillClockCannotDateAnUndatedIncrease(t *testing.T) {
	old := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	seed := orderJournalEvent{At: old, OrderRef: "partial", Account: "DU1234567", Mode: "paper", Filled: 1}
	next := seed
	next.Filled = 2
	next.At = time.Time{}
	clocks := mandatePremiumFillClock([]orderJournalEvent{seed, next})
	if clock := clocks[orderJournalKey(seed)]; clock.quantity != 2 || !clock.at.IsZero() {
		t.Fatalf("undated new fill borrowed an old fill's date: %+v", clock)
	}
}
