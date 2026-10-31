//go:build trading

package daemon

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// A completed external order is no longer in reqAllOpenOrders, and an
// unmatched orderStatus callback is deliberately not adopted by our journal.
// Its fill must still invalidate cover before updatePortfolio catches up.
func TestProtectivePutExternalCompletedFillRequiresPositionsReceipt(t *testing.T) {
	for _, name := range []string{"shares sold", "different put acquired"} {
		t.Run(name, func(t *testing.T) {
			s, p := protectivePutTestServer(t)
			scope := s.currentBrokerStateScope()
			filled := false
			s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
				positions := mandateTestLongOptionBook(t, s, 60)
				var receipt []*ibkr.RawPosition
				for _, rows := range [][]rpc.PositionView{positions.Stocks, positions.Options} {
					for _, row := range rows {
						if filled && name == "shares sold" && row.ConID == 9002 {
							continue
						}
						receipt = append(receipt, &ibkr.RawPosition{Account: scope.Account,
							Contract: ibkr.Contract{ConID: row.ConID, Symbol: row.Symbol, SecType: positionWireSecType(row.SecType), Currency: row.Currency, Right: row.Right, Multiplier: row.Multiplier}, Position: row.Quantity})
					}
				}
				if filled && name == "different put acquired" {
					receipt = append(receipt, &ibkr.RawPosition{Account: scope.Account,
						Contract: ibkr.Contract{ConID: 9011, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Multiplier: 100}, Position: 1})
				}
				return receipt, nil
			}
			preview, err := s.previewOrder(t.Context(), p)
			if err != nil || preview.PreviewToken == "" {
				t.Fatalf("initial covered purchase must be previewable: err=%v", err)
			}
			filled = true
			fillQuantity := 1.0
			if name == "shares sold" {
				fillQuantity = 100
			}
			if !s.appendOrderLifecycleEvent(ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventStatus,
				OrderID: 2001, ClientID: 77, ClientIDPresent: true, Account: scope.Account,
				Status: "Filled", Filled: fillQuantity}) {
				t.Fatal("unmatched external completion must not imply journal persistence loss")
			}
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			sent := 0
			s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
			if _, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken}); err == nil || sent != 0 {
				t.Errorf("completed external fill released stale cover: err=%v sends=%d", err, sent)
			}
		})
	}
}

func TestProtectivePutPositionsReceiptAdmission(t *testing.T) {
	for _, name := range []string{"coherent", "other account", "shares missing", "new option on another name", "duplicate shares", "missing contract identity", "unknown security", "conflicting currency", "unknown account", "wrong account", "nonfinite quantity", "receipt failed"} {
		t.Run(name, func(t *testing.T) {
			s, p := protectivePutTestServer(t)
			read := s.reduceFillSnapshotForTest
			s.reduceFillSnapshotForTest = func(ctx context.Context, timeout time.Duration) ([]*ibkr.RawPosition, error) {
				receipt, err := read(ctx, timeout)
				if err != nil {
					return nil, err
				}
				var stock *ibkr.RawPosition
				for _, row := range receipt {
					if row.Contract.ConID == 9002 {
						stock = row
					}
				}
				if stock == nil {
					t.Fatal("fixture has no covering shares")
				}
				extra := &ibkr.RawPosition{Account: stock.Account, Contract: ibkr.Contract{ConID: 9020, SecType: "OPT", Symbol: "SYNX", Currency: "EUR", Right: "C", Multiplier: 100}, Position: 1}
				switch name {
				case "other account":
					extra.Account = "another-test-account"
					receipt = append(receipt, extra)
				case "shares missing":
					stock.Position = 0
				case "new option on another name":
					receipt = append(receipt, extra)
				case "duplicate shares":
					receipt = append(receipt, stock)
				case "missing contract identity":
					extra.Contract.ConID = 0
					receipt = append(receipt, extra)
				case "unknown security":
					extra.Contract.SecType = "UNKNOWN"
					receipt = append(receipt, extra)
				case "conflicting currency":
					stock.Contract.Currency = "USD"
				case "unknown account":
					stock.Account = ""
				case "wrong account":
					stock.Account = "another-test-account"
				case "nonfinite quantity":
					stock.Position = math.NaN()
				case "receipt failed":
					return nil, errors.New("broker positions timed out")
				}
				return receipt, nil
			}
			whatIfs := 0
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				whatIfs++
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			preview, err := s.previewOrder(t.Context(), p)
			allowed := name == "coherent" || name == "other account"
			if allowed {
				if err != nil || preview.PreviewToken == "" || whatIfs != 1 {
					t.Fatalf("coherent cover was refused: err=%v WhatIfs=%d", err, whatIfs)
				}
			} else if err == nil || whatIfs != 0 {
				t.Fatalf("unproven cover reached the broker preview: err=%v WhatIfs=%d", err, whatIfs)
			}
		})
	}
}
