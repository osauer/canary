//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestStockAddTradingRechecksBeforeSendingAndNeverResizes(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "cash spent after preview"}[change], func(t *testing.T) {
			s, in := stockAddTestServer(t)
			sent := 0
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			s.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, o *ibkrlib.RawOrder) error {
				sent++
				if o.TotalQty != 3 {
					t.Errorf("changed quantity %v", o.TotalQty)
				}
				return nil
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, s.currentBrokerStateScope(), nil
			}
			p := stockAddTestParams()
			p.Quantity = 3
			raw, _ := json.Marshal(p)
			preview, err := s.handleAddPreview(t.Context(), &rpc.Request{Params: raw})
			if err != nil {
				t.Fatal(err)
			}
			if change {
				in.FreeCash = 100
			}
			result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if change {
				if err == nil || sent != 0 {
					t.Fatalf("changed budget sent: %+v %v calls %d", result, err, sent)
				}
			} else if err != nil || !result.Accepted || sent != 1 {
				t.Fatalf("valid exact addition failed: %+v %v calls %d", result, err, sent)
			}
			if !change {
				events, err := s.orderJournal.LoadEvents(0)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ev := range events {
					if ev.Type == orderJournalEventSendAttempted && ev.Add != nil && ev.Add.Plan.Quantity == 3 {
						found = true
					}
				}
				if !found {
					t.Fatal("send lost signed Add sizing evidence")
				}
			}
		})
	}
}
func TestStockAddWireGuardRejectsChangedAuthority(t *testing.T) {
	for _, kind := range []string{"policy", "orders", "cash"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := stockAddTestServer(t)
			s.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
				t.Fatal("unexpected send")
				return nil
			}
			auth, binding, err := s.authorizeBrokerWriteTransaction("", false)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := s.stockAddEvidence(t.Context(), stockAddTestParams())
			if err != nil {
				t.Fatal(err)
			}
			binding.stockAddReview = &ev.review
			switch kind {
			case "policy":
				binding.stockAddReview.PolicyFingerprint = "different"
			case "orders":
				binding.stockAddOrdersGeneration = 99
			case "cash":
				binding.stockAddAccountFingerprint = "previous cash"
			}
			guard, release := s.brokerWireGuard(binding, auth.Status, false)
			defer release()
			if err := guard(); err == nil || !strings.Contains(err.Error(), "Add policy, cash or working orders changed") {
				t.Fatalf("%s change passed final guard: %v", kind, err)
			}
		})
	}
}
