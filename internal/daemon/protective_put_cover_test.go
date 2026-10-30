//go:build trading

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func protectivePutTestServer(t *testing.T) (*Server, rpc.OrderPreviewParams) {
	t.Helper()
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return mandateTestLongOptionBook(t, s, 60), nil
	}
	s.evaluateMandate(t.Context(), false)
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	s.orderPreviewQuote = fixedPreviewQuote(2.9, 3.1)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	return s, rpc.OrderPreviewParams{Action: "BUY", Quantity: 1, LimitPrice: new(3.0), Contract: rpc.ContractParams{ConID: 9010, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Strike: 75, Expiry: "20270115", Multiplier: 100}}
}

// Two independently reviewed purchases cannot each reserve the same 100
// held shares as protection while both remain live at the broker.
func TestProtectivePutPendingPurchaseReservesCover(t *testing.T) {
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return mandateTestLongOptionBook(t, s, 60), nil
	}
	s.evaluateMandate(t.Context(), false)
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	s.orderPreviewQuote = fixedPreviewQuote(2.9, 3.1)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	nextID, sent := 1000, 0
	s.orderReserveBrokerID = func(context.Context) (int, error) { nextID++; return nextID, nil }
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 1, LimitPrice: new(3.0), Contract: rpc.ContractParams{ConID: 9010, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Strike: 75, Expiry: "20270115", Multiplier: 100}}
	var tokens []string
	for range 2 {
		preview, err := s.previewOrder(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, preview.PreviewToken)
	}
	first, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: tokens[0]})
	if err != nil || first == nil || !first.Accepted || sent != 1 {
		t.Fatalf("first protective purchase: result=%+v err=%v sends=%d", first, err, sent)
	}
	second, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: tokens[1]})
	if err == nil || sent != 1 {
		t.Fatalf("second purchase reused the same stock cover: accepted=%v err=%v sends=%d", second != nil && second.Accepted, err, sent)
	}
}

// A hand-entered pending put consumes the same cover as a Canary intent.
func TestProtectivePutExternalPendingPurchaseReservesCover(t *testing.T) {
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return mandateTestLongOptionBook(t, s, 60), nil
	}
	s.evaluateMandate(t.Context(), false)
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		scope := s.currentBrokerStateScope()
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{
			Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 2001, ClientID: 77, ClientIDPresent: true,
			Account: scope.Account, Symbol: "SYNB", SecType: "OPT", ConID: 9011, Currency: "EUR", Multiplier: 100,
			Right: "P", Strike: 70, Expiry: "20270115", Action: "BUY", OrderType: "LMT", LimitPrice: 2,
			TotalQuantity: 1, Remaining: 1, Status: "Submitted",
		}}}, scope, nil
	}
	draft := rpc.OrderDraft{Action: "BUY", Quantity: 1, LimitPrice: 3, OrderType: "LMT", Contract: rpc.ContractParams{ConID: 9010, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Strike: 75, Expiry: "20270115", Multiplier: 100}}
	if blocker := s.mandateBudgetGate(draft, mandateOpen); blocker == nil {
		t.Fatal("external put already consumes every held share, but the new purchase bypassed the premium cap")
	}
}

func TestProtectivePutCoverCommitments(t *testing.T) {
	for _, name := range []string{"working stock sale", "working stock stop", "unknown combination", "unknown security type", "unavailable inventory", "other account", "currency mismatch", "authenticated replacement", "untrusted replacement", "broker journal deduplicated"} {
		t.Run(name, func(t *testing.T) {
			s, p := protectivePutTestServer(t)
			draft := rpc.OrderDraft{Action: "BUY", Quantity: 1, LimitPrice: 3, OrderType: "LMT", Contract: p.Contract, OrderRef: "new-review"}
			scope := s.currentBrokerStateScope()
			order := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 2001, ClientID: 77, ClientIDPresent: true,
				Account: scope.Account, Symbol: "SYNB", SecType: "OPT", ConID: 9011, Currency: "EUR", Multiplier: 100,
				Right: "P", Strike: 70, Expiry: "20270115", Action: "BUY", OrderType: "LMT", LimitPrice: 2,
				TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
			wantOpen := false
			switch name {
			case "working stock sale", "working stock stop":
				order.SecType, order.ConID, order.Action, order.TotalQuantity, order.Remaining = "STK", 9002, "SELL", 100, 100
				if name == "working stock stop" {
					order.OrderType, order.AuxPrice = "STP", 70
				}
			case "unknown combination":
				order.SecType = "BAG"
			case "unknown security type":
				order.SecType = ""
			case "other account":
				order.Account, wantOpen = "DU7654321", true
			case "currency mismatch":
				draft.Contract.Currency = "USD"
			case "authenticated replacement", "broker journal deduplicated":
				prior := draft
				prior.Contract.ConID, prior.Contract.Strike, prior.LimitPrice = 9011, 70, 2
				prior.OrderRef = "authenticated-order"
				event := orderJournalEventForDraft(prior, orderJournalEventBrokerAcknowledged, s.currentTradingStatus(), "checked-token", 2001, s.orderNow())
				event.Remaining, event.Status = 1, "Submitted"
				order.ClientID = event.ClientID
				if err := s.orderJournal.Append(event); err != nil {
					t.Fatal(err)
				}
				if name == "authenticated replacement" {
					draft.OrderRef = prior.OrderRef
				} else {
					_, input, _, _ := s.currentMandateVerdict()
					for i := range input.Legs {
						if input.Legs[i].ID == "con:9002" {
							input.Legs[i].Quantity = 200
						}
					}
				}
				wantOpen = true
			case "untrusted replacement":
				order.OrderRef = draft.OrderRef
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				if name == "unavailable inventory" {
					return ibkr.OpenOrderSnapshot{}, scope, errors.New("unavailable")
				}
				return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{order}}, scope, nil
			}
			if got := s.mandateBudgetGate(draft, mandateOpen) == nil; got != wantOpen {
				t.Fatalf("protected purchase open=%v, want %v", got, wantOpen)
			}
		})
	}
}

func TestProtectivePutRechecksCoverAtWire(t *testing.T) {
	s, p := protectivePutTestServer(t)
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	sent := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	s.orderWriteBeforeBrokerSend = func() {
		other := preview.Draft
		other.OrderRef = "other-cover"
		event := orderJournalEventForDraft(other, orderJournalEventSendAttempted, s.currentTradingStatus(), "other-token", 1002, s.orderNow())
		event.SendState = orderSendStateUncertainSend
		if err := s.orderJournal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken}); err == nil || sent != 0 {
		t.Fatalf("wire guard reused occupied cover: err=%v sends=%d", err, sent)
	}
}

func TestProtectivePutRequiresReflectedFills(t *testing.T) {
	for _, name := range []string{"stock sale", "put purchase", "broker combination ahead of journal"} {
		t.Run(name, func(t *testing.T) {
			s, p := protectivePutTestServer(t)
			preview, err := s.previewOrder(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			sent := 0
			s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
			draft := preview.Draft
			draft.OrderRef = "unreflected-fill"
			if name == "stock sale" {
				draft.Action, draft.Quantity = "SELL", 100
				draft.Contract = rpc.ContractParams{ConID: 9002, Symbol: "SYNB", SecType: "STK", Currency: "EUR"}
			}
			event := orderJournalEventForDraft(draft, orderJournalEventStatusUpdated, s.currentTradingStatus(), "fill-token", 1002, s.orderNow())
			event.Status, event.Filled = "Filled", float64(draft.Quantity)
			if name == "broker combination ahead of journal" {
				event.SecType, event.ConID, event.Status, event.Filled, event.Quantity, event.Remaining = "BAG", 0, "Submitted", 0, 2, 2
				event.StrategyGroup = &rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{{Contract: p.Contract, Ratio: 1, Quantity: 2, Action: "BUY"}}}
				// Zero remaining after a fully filled broker BAG leaves no pending
				// reservation. Its typed legs still need the correlated receipt.
				s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
					scope := s.currentBrokerStateScope()
					return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{
						Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: event.ClientID, ClientIDPresent: true,
						Account: scope.Account, SecType: "BAG", Action: "BUY", TotalQuantity: 2, Filled: 2, Status: "Filled",
					}}}, scope, nil
				}
			}
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
				if name == "stock sale" {
					return nil, nil // the shares have gone, unlike the kept book
				}
				return []*ibkr.RawPosition{{Account: s.currentBrokerStateScope().Account, Contract: ibkr.Contract{ConID: p.Contract.ConID}, Position: 2}}, nil
			}
			if _, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken}); err == nil || sent != 0 {
				t.Fatalf("unreflected fill released cover: err=%v sends=%d", err, sent)
			}
		})
	}
}

func TestProtectivePutKeepsOnlyUnreservedCoverOpenUnderBrake(t *testing.T) {
	s, p := protectivePutTestServer(t)
	c := s.riskPolicies.snapshot().policy
	s.riskCapital.Observe(240000, s.orderNow().Add(-time.Minute), c, testLiveObserveScope, true)
	call := rpc.OrderDraft{Action: "BUY", Quantity: 1, Contract: p.Contract}
	call.Contract.Right = "C"
	if blocker, _ := s.riskPolicyBrake(call, mandateOpen); blocker == nil || blocker.Code != drawdownBrakeCode {
		t.Fatal("fixture must latch the drawdown brake")
	}
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatalf("covered protection was refused under the brake: %v", err)
	}
	other := preview.Draft
	other.OrderRef = "already-protected"
	event := orderJournalEventForDraft(other, orderJournalEventSendAttempted, s.currentTradingStatus(), "other-token", 1002, s.orderNow())
	event.SendState = orderSendStateUncertainSend
	if err := s.orderJournal.Append(event); err != nil {
		t.Fatal(err)
	}
	if _, err := s.previewOrder(t.Context(), p); err == nil || !strings.Contains(err.Error(), "Drawdown brake") {
		t.Fatalf("second purchase bypassed the drawdown brake: %v", err)
	}
}

func TestProtectivePutBindsFillFrontierThroughWire(t *testing.T) {
	s, p := protectivePutTestServer(t)
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(1)
	s.reduceFillGenerationForTest = func() uint64 { return generation }
	s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	sent := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	s.orderWriteBeforeBrokerSend = func() { generation++ }
	if _, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken}); err == nil || !strings.Contains(err.Error(), "broker fills or session changed") || sent != 0 {
		t.Fatalf("new fill reached the wire on old cover evidence: err=%v sends=%d", err, sent)
	}
}

func TestProtectivePutModificationExcludesOnlyItsOwnReservation(t *testing.T) {
	s, p := protectivePutTestServer(t)
	p.Contract.Exchange = "SMART"
	draft := rpc.OrderDraft{Contract: p.Contract, OrderRef: "held-protection", Action: "BUY", Quantity: 1, OrderType: "LMT", LimitPrice: 4, TIF: "DAY", OpenClose: "O"}
	event := orderJournalEventForDraft(draft, orderJournalEventBrokerAcknowledged, s.currentTradingStatus(), "original-token", 1002, s.orderNow().Add(-time.Minute))
	event.Remaining, event.Status, event.SendState = 1, "Submitted", orderSendStateBrokerAcknowledged
	if err := s.orderJournal.Append(event); err != nil {
		t.Fatal(err)
	}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		scope := s.currentBrokerStateScope()
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{
			Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: event.ClientID, ClientIDPresent: true, Account: scope.Account,
			ConID: p.Contract.ConID, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Multiplier: 100,
			Action: "BUY", OrderType: "LMT", LimitPrice: 4, TotalQuantity: 1, Remaining: 1, Status: "Submitted",
		}}}, scope, nil
	}
	p.ReplaceID, p.TIF, p.OrderType = draft.OrderRef, "DAY", "LMT"
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatalf("same covered purchase could not be repriced: %v", err)
	}
	sent := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	result, err := s.modifyOrder(t.Context(), rpc.OrderModifyParams{ID: draft.OrderRef, PreviewToken: preview.PreviewToken})
	if err != nil || result == nil || !result.Accepted || sent != 1 {
		t.Fatalf("same covered purchase modification: result=%+v err=%v sends=%d", result, err, sent)
	}
}

func TestProtectivePutAddRoomUsesCurrentCommitments(t *testing.T) {
	for _, name := range []string{"unreserved", "put purchase", "stock sale", "stock stop", "both", "inventory unavailable"} {
		t.Run(name, func(t *testing.T) {
			s, p := protectivePutTestServer(t)
			_, input, _, _ := s.currentMandateVerdict()
			for i := range input.Legs {
				if input.Legs[i].ID == "con:9002" {
					input.Legs[i].Quantity = 300
				}
			}
			scope := s.currentBrokerStateScope()
			put := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 2001, ClientID: 77, ClientIDPresent: true,
				Account: scope.Account, Symbol: "SYNB", SecType: "OPT", ConID: 9011, Currency: "EUR", Multiplier: 100,
				Right: "P", Strike: 70, Expiry: "20270115", Action: "BUY", OrderType: "LMT", LimitPrice: 2,
				TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
			stock := put
			stock.OrderID, stock.SecType, stock.ConID, stock.Action, stock.TotalQuantity, stock.Remaining = 2002, "STK", 9002, "SELL", 100, 100
			var orders []ibkr.OrderLifecycleEvent
			want := 3
			switch name {
			case "put purchase":
				orders, want = []ibkr.OrderLifecycleEvent{put}, 2
			case "stock sale", "stock stop":
				if name == "stock stop" {
					stock.OrderType, stock.AuxPrice = "STP", 70
				}
				orders, want = []ibkr.OrderLifecycleEvent{stock}, 2
			case "both":
				orders, want = []ibkr.OrderLifecycleEvent{put, stock}, 1
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				if name == "inventory unavailable" {
					return ibkr.OpenOrderSnapshot{}, scope, errors.New("unavailable")
				}
				return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: orders}, scope, nil
			}
			cover := s.readAddPutCover(t.Context(), p.Contract)
			if name == "inventory unavailable" {
				if cover != nil {
					t.Fatalf("missing inventory fabricated room: %+v", cover)
				}
				return
			}
			if cover == nil || cover.RoomContracts != want || cover.HeldShares != 300 || cover.CoveredShares != 0 {
				t.Fatalf("cover=%+v, want held=300 covered=0 room=%d", cover, want)
			}
			draft := rpc.OrderDraft{Action: "BUY", Contract: p.Contract, Quantity: want, LimitPrice: 3, OrderType: "LMT"}
			if !s.protectivePut(draft) {
				t.Fatal("displayed room disagrees with the protective-put gate")
			}
			draft.Quantity++
			if s.protectivePut(draft) {
				t.Fatal("the gate admitted beyond displayed protection room")
			}
		})
	}
}
