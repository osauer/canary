//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// An unchanged positions read cannot make room for successive live buys.
// All previews fit individually; only pending purchase commitments make the
// third order exceed the existing policy's name premium cap.
func TestMandatePendingPremiumBlocksStackedPlacements(t *testing.T) {
	s, _ := mandateTestOverLossServer(t)
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 5, rpc.OrderPositionEffectOpen)
	s.orderPreviewQuote = fixedPreviewQuote(7.9, 8.1)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, s.currentBrokerStateScope(), nil
	}
	nextID, sent := 1000, 0
	s.orderReserveBrokerID = func(context.Context) (int, error) { nextID++; return nextID, nil }
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 5, LimitPrice: new(8.0), Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	var tokens []string
	for range 3 {
		preview, err := s.previewOrder(t.Context(), p)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		tokens = append(tokens, preview.PreviewToken)
	}
	for i, token := range tokens {
		result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
		if i < 2 {
			if err != nil || !result.Accepted {
				t.Fatalf("within-cap placement %d: %v", i, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "Mandate premium") || sent != 2 {
			t.Fatalf("stacked buy escaped name premium cap: result=%+v err=%v sends=%d", result, err, sent)
		}
	}
}

// A different buy arriving after admission is still charged by the physical
// first-byte guard, while the draft's own staged send is counted only once.
func TestMandatePendingPremiumRecheckedAtWire(t *testing.T) {
	s, _ := mandateTestOverLossServer(t)
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 5, rpc.OrderPositionEffectOpen)
	s.orderPreviewQuote = fixedPreviewQuote(7.9, 8.1)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	sent := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 5, LimitPrice: new(8.0), Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	s.orderWriteBeforeBrokerSend = func() {
		other := preview.Draft
		other.OrderRef = "other-pending"
		other.Quantity = 10
		event := orderJournalEventForDraft(other, orderJournalEventSendAttempted, s.currentTradingStatus(), "other-token", 1002, s.orderNow())
		event.SendState = orderSendStateUncertainSend
		if err := s.orderJournal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
	if err == nil || !strings.Contains(err.Error(), "Mandate premium") || sent != 0 {
		t.Fatalf("wire accepted new commitment: result=%+v err=%v sends=%d", result, err, sent)
	}
}

func TestMandatePendingPremiumNewTimestampDoesNotProveFilledQuantity(t *testing.T) {
	for _, kind := range []string{"single", "combo", "external partial"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			s.orderPreviewPositionImpact = fixedPreviewPosition(0, 5, rpc.OrderPositionEffectOpen)
			s.orderPreviewQuote = fixedPreviewQuote(7.9, 8.1)
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			sent := 0
			s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
			p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 5, LimitPrice: new(8.0), Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
			preview, err := s.previewOrder(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			filled := preview.Draft
			filled.OrderRef = "filled-other"
			event := orderJournalEventForDraft(filled, orderJournalEventStatusUpdated, s.currentTradingStatus(), "filled-token", 1002, s.orderNow())
			event.Filled = 5
			event.Status = "Filled"
			if kind == "combo" {
				event.SecType = "BAG"
				event.ConID = 0
				event.StrategyGroup = &rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{{Contract: p.Contract, Ratio: 1, Action: "BUY", Quantity: 5}}}
			}
			if kind == "external partial" {
				s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
					return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 77, ClientIDPresent: true, Account: s.currentBrokerStateScope().Account, Symbol: "SYNA", SecType: "OPT", ConID: 9009, Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, TotalQuantity: 6, Filled: 5, Remaining: 1, Status: "Submitted"}}}, s.currentBrokerStateScope(), nil
				}
			} else if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			// A valuation tick publishes a later time while quantity 9009 is still
			// absent from the mandate's input. The correlated broker receipt has it.
			result, input, _, _ := s.mandateVerdicts.current()
			result.Exposure.AsOf = s.orderNow()
			readPositions := s.reduceFillSnapshotForTest
			s.reduceFillSnapshotForTest = func(ctx context.Context, timeout time.Duration) ([]*ibkr.RawPosition, error) {
				receipt, err := readPositions(ctx, timeout)
				return append(receipt, &ibkr.RawPosition{Account: s.currentBrokerStateScope().Account,
					Contract: ibkr.Contract{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Multiplier: 100}, Position: 5}), err
			}
			_, err = s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if err == nil || sent != 0 {
				t.Fatalf("new timestamp released unreflected premium: err=%v sends=%d", err, sent)
			}
			// Once actual held quantities agree with the receipt, a new reviewed
			// purchase inside the unchanged cap is admitted.
			input.Legs = append(input.Legs, mandate.Leg{ID: "con:9009", Kind: mandate.KindOption, Symbol: "SYNA", Underlying: "SYNA", Currency: "EUR", Quantity: 5, Multiplier: 100, LossPerUnitBase: 800})
			result.Exposure.Book.PremiumAtRiskBase = new(*result.Exposure.Book.PremiumAtRiskBase + 4000)
			for i := range result.Exposure.Names {
				if result.Exposure.Names[i].Underlying == "SYNA" {
					result.Exposure.Names[i].PremiumAtRiskBase = new(*result.Exposure.Names[i].PremiumAtRiskBase + 4000)
				}
			}
			s.orderPreviewPositionImpact = fixedPreviewPosition(5, 10, rpc.OrderPositionEffectIncrease)
			preview, err = s.previewOrder(t.Context(), p)
			if err != nil {
				t.Fatalf("coherent preview: %v", err)
			}
			placed, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if err != nil || placed == nil || !placed.Accepted || sent != 1 {
				t.Fatalf("coherent purchase refused: %v sends=%d", err, sent)
			}
		})
	}
}
