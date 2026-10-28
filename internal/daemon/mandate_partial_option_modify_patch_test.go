//go:build trading

package daemon

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"strings"
	"testing"
	"time"
)

func TestPostreleasePartialOptionBuyModify(t *testing.T) {
	for _, lateFill := range []bool{false, true} {
		t.Run(fmt.Sprint("lateFill=", lateFill), func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			result, input, _, _ := s.mandateVerdicts.current()
			input.Legs = append(input.Legs, mandate.Leg{ID: "con:9009", Kind: mandate.KindOption, Symbol: "SYNA", Underlying: "SYNA", Currency: "EUR", Quantity: 5, Multiplier: 100, LossPerUnitBase: 800})
			result.Exposure.Book.PremiumAtRiskBase = new(*result.Exposure.Book.PremiumAtRiskBase + 4000)
			for i := range result.Exposure.Names {
				if result.Exposure.Names[i].Underlying == "SYNA" {
					result.Exposure.Names[i].PremiumAtRiskBase = new(*result.Exposure.Names[i].PremiumAtRiskBase + 4000)
				}
			}
			contract := rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Exchange: "SMART", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}
			draft := rpc.OrderDraft{Contract: contract, OrderRef: "part-filled-buy", Action: "BUY", Quantity: 10, OrderType: "LMT", LimitPrice: 8, TIF: "DAY", OpenClose: "O"}
			event := orderJournalEventForDraft(draft, orderJournalEventBrokerAcknowledged, s.currentTradingStatus(), "seed", 1002, s.orderNow().Add(-2*time.Minute))
			event.Filled = 5
			event.Remaining = 5
			event.Status = "Submitted"
			event.SendState = orderSendStateBrokerAcknowledged
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			s.orderPreviewPositionImpact = fixedPreviewPosition(5, 10, rpc.OrderPositionEffectIncrease)
			s.orderPreviewQuote = fixedPreviewQuote(6.9, 7.1)
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
				return []*ibkr.RawPosition{{Account: s.currentBrokerStateScope().Account, Contract: ibkr.Contract{ConID: 9009}, Position: 5}}, nil
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 31, ClientIDPresent: true, Account: s.currentBrokerStateScope().Account, ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100, Action: "BUY", OrderType: "LMT", LimitPrice: 8, TotalQuantity: 10, Filled: 5, Remaining: 5, Status: "Submitted"}}}, s.currentBrokerStateScope(), nil
			}
			sent := 0
			s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
			preview, err := s.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "BUY", Contract: contract, Quantity: 5, OrderType: "LMT", LimitPrice: new(7.0), TIF: "DAY", ReplaceID: draft.OrderRef})
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			generation := uint64(1)
			s.reduceFillGenerationForTest = func() uint64 { return generation }
			if lateFill {
				s.orderWriteBeforeBrokerSend = func() { generation++ }
			}
			modified, err := s.modifyOrder(t.Context(), rpc.OrderModifyParams{ID: draft.OrderRef, PreviewToken: preview.PreviewToken})
			if lateFill {
				if err == nil || !strings.Contains(err.Error(), "broker fills or session changed") || sent != 0 {
					t.Fatalf("late fill did not fence modify: err=%v sends=%d", err, sent)
				}
				return
			}
			if err != nil || modified == nil || !modified.Accepted || sent != 1 {
				t.Fatalf("reflected partial-fill lower-price modification refused: err=%v sends=%d", err, sent)
			}
		})
	}
}
