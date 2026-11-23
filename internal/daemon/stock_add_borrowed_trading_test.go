//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A borrowing purchase's warning is confirmed with the order like any other:
// unaccepted it never reaches the broker, accepted it is sent once, and a
// borrowed amount that changed after the preview needs another review.
func TestStockAddBorrowedCurrencyWarningIsConfirmedWithTheOrder(t *testing.T) {
	for _, tc := range []string{"not accepted", "accepted", "borrowing changed"} {
		t.Run(tc, func(t *testing.T) {
			s, in := stockAddTestServer(t)
			in.Manual = true
			in.Cash = &risk.StockAddCash{Available: 100, Spendable: 10000, BorrowAllowed: true, Currency: "USD"}
			sent := 0
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			s.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { sent++; return nil }
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, s.currentBrokerStateScope(), nil
			}
			p := stockAddTestParams()
			p.Quantity, p.Max = 3, false
			raw, _ := json.Marshal(p)
			preview, err := s.handleAddPreview(t.Context(), &rpc.Request{Params: raw})
			if err != nil {
				t.Fatal(err)
			}
			w := preview.Draft.Add.Plan.Warnings
			if i := slicesIndexCode(w, "add_borrowed_currency"); i < 0 || !strings.Contains(w[i].Message, "borrows 205 USD") || preview.Draft.Add.Plan.Cash.Borrowed != 205 {
				t.Fatalf("preview warnings %+v cash %+v", w, preview.Draft.Add.Plan.Cash)
			}
			if tc == "borrowing changed" {
				in.Cash = &risk.StockAddCash{Available: 50, Spendable: 10000, BorrowAllowed: true, Currency: "USD"}
			}
			result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, AcceptAddWarnings: tc != "not accepted"})
			switch tc {
			case "not accepted":
				if err == nil || sent != 0 || !strings.Contains(err.Error(), "acknowledge") {
					t.Fatalf("unaccepted borrowing reached dispatch: %+v %v sent=%d", result, err, sent)
				}
			case "borrowing changed":
				if err == nil || sent != 0 || !strings.Contains(err.Error(), "add warnings changed") {
					t.Fatalf("changed borrowing reached dispatch: %+v %v sent=%d", result, err, sent)
				}
			default:
				if err != nil || !result.Accepted || sent != 1 {
					t.Fatalf("accepted borrowing refused: %+v %v sent=%d", result, err, sent)
				}
			}
		})
	}
}
