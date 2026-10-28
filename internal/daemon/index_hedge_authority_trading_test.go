//go:build trading

package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"strings"
	"testing"
	"time"
)

func appendHedgeAuthorityFill(t *testing.T, s *Server, conID int, qty float64) {
	t.Helper()
	scope := s.currentBrokerStateScope()
	if err := s.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-filled-hedge", PreviewTokenID: "synthetic-filled-token", ReservedOrderID: 301, Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", ConID: conID, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Expiry: "20270319", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, LimitPrice: 6000, Quantity: qty, Filled: qty, Status: "Filled", Source: hedgeOrderSource}); err != nil {
		t.Fatal(err)
	}
}

func hedgeAuthorityToken(t *testing.T, s *Server) string {
	t.Helper()
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
		t.Fatalf("preview %+v", preview)
	}
	return preview.Legs[0].PreviewToken
}

func TestIndexHedgeAuthorityDirectPlaceCountsOtherHeldMonths(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	pos, _ := s.reducePositionsForTest(t.Context())
	pos.Stocks = append(pos.Stocks, mandateReductionFuture(hedgeTestConID+1, -25, 6000))
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 || !strings.Contains(err.Error(), "whole-book") {
		t.Fatalf("other held month: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityFillMustReachWholeBook(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	appendHedgeAuthorityFill(t, s, hedgeTestConID+1, 2)
	s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
		return []*ibkr.RawPosition{{Account: s.currentBrokerStateScope().Account, Contract: ibkr.Contract{ConID: hedgeTestConID + 1}, Position: -2}}, nil
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 || !strings.Contains(err.Error(), "caught up") {
		t.Fatalf("unreflected other-month fill: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityCoherentFillKeepsRemainingCapacity(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(hedgeTestConID+1, -2, 6000))
	r := newHedgeSendRig(t, s)
	appendHedgeAuthorityFill(t, s, hedgeTestConID+1, 2)
	token := hedgeAuthorityToken(t, s)
	res, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err != nil || res == nil || !res.Accepted || len(r.sends()) != 1 || r.sends()[0].TotalQty != 3 {
		t.Fatalf("coherent fill: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityRechecksCapAndLifecycleAtWire(t *testing.T) {
	for _, change := range []string{"cap", "orders"} {
		t.Run(change, func(t *testing.T) {
			s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(hedgeTestConID+1, -2, 6000))
			r := newHedgeSendRig(t, s)
			generation := uint64(1)
			s.reduceFillGenerationForTest = func() uint64 { return generation }
			token := hedgeAuthorityToken(t, s)
			s.orderWriteBeforeBrokerSend = func() {
				if change == "cap" {
					s.riskPolicies.active.Mandate.IndexHedge.MaxContracts = new(4)
				} else {
					generation++
				}
			}
			_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
			if err == nil || len(r.sends()) != 0 {
				t.Fatalf("%s changed before wire: err=%v sends=%d", change, err, len(r.sends()))
			}
			if change == "cap" && !strings.Contains(err.Error(), "whole-book") {
				t.Fatalf("cap refusal came from another gate: %v", err)
			}
		})
	}
}

func TestIndexHedgeAuthorityWireReadDoesNotRelockEvidence(t *testing.T) {
	broker := startFakeTWS(t)
	s, _ := sessionLossServer(t, broker, "2026-10-14T15:00:00Z")
	keepMandateEvaluationAt(t, s, mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50)), 0.5)
	a := &indexHedgeAuthority{orders: s.reduceFillBinding()}
	limits := risk.OrderLimitsInForce{IndexHedgeMaxContracts: 25}
	if err := s.validateIndexHedgeAuthority(a, hedgeSale(5), limits); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		a.orders.connector.WithBrokerEvidenceMutation(func() { finished <- s.validateIndexHedgeAuthority(a, hedgeSale(5), limits) })
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("hedge authority deadlocked under broker evidence barrier")
	}
}

func TestIndexHedgeAuthorityUnknownFutureBAGCannotFreeCapacity(t *testing.T) {
	for _, action := range []string{rpc.OrderActionBuy, rpc.OrderActionSell} {
		for _, ratio := range []int{-2, 2} {
			s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
			r := newHedgeSendRig(t, s)
			token := hedgeAuthorityToken(t, s)
			scope := s.currentBrokerStateScope()
			c := hedgeSale(1).Contract
			ev := orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-bag", PreviewTokenID: "synthetic-bag-token", ReservedOrderID: 301, Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", Symbol: "MES", SecType: "BAG", Currency: "USD", Action: action, OrderType: "LMT", LimitPrice: 1, Quantity: 2, Remaining: 2, Status: "Submitted", StrategyGroup: &rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{{Contract: c, Ratio: ratio, Action: rpc.OrderActionBuy}}}}
			if err := s.orderJournal.Append(ev); err != nil {
				t.Fatal(err)
			}
			_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
			if err == nil || len(r.sends()) != 0 || !strings.Contains(err.Error(), "combination") {
				t.Fatalf("BAG %s ratio%d: err=%v sends=%d", action, ratio, err, len(r.sends()))
			}
		}
	}
}

func TestIndexHedgeAuthorityPendingBuyDoesNotReleaseHeldCapacity(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	pos, _ := s.reducePositionsForTest(t.Context())
	pos.Stocks = append(pos.Stocks, mandateReductionFuture(hedgeTestConID+1, -25, 6000))
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		scope := s.currentBrokerStateScope()
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 222, ClientID: 77, ClientIDPresent: true, Account: scope.Account, ConID: hedgeTestConID + 1, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Action: "BUY", OrderType: "LMT", LimitPrice: 6000, TotalQuantity: 25, Remaining: 25, Status: "Submitted"}}}, scope, nil
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 || !strings.Contains(err.Error(), "whole-book") {
		t.Fatalf("pending buy released held short: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityPartialFillCountsOnlyRemainingOnce(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(hedgeTestConID, -2, 6000))
	r := newHedgeSendRig(t, s)
	scope := s.currentBrokerStateScope()
	if err := s.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-partial", PreviewTokenID: "synthetic-partial-token", ReservedOrderID: 301, Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Expiry: "20261218", Action: "SELL", OrderType: "LMT", LimitPrice: 6000, Quantity: 3, Filled: 2, Remaining: 1, Status: "Submitted", Source: hedgeOrderSource}); err != nil {
		t.Fatal(err)
	}
	// The broker order row still has the older cumulative fill; journaled
	// fill evidence plus a correlated position receipt must prevent double count.
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 301, ClientID: 31, ClientIDPresent: true, Account: scope.Account, ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Action: "SELL", OrderType: "LMT", LimitPrice: 6000, TotalQuantity: 3, Remaining: 3, Status: "Submitted"}}}, scope, nil
	}
	c := hedgeSale(2).Contract
	preview, err := s.previewOrder(t.Context(), rpc.OrderPreviewParams{Contract: c, Action: "SELL", Quantity: 2, OrderType: "LMT", Strategy: rpc.OrderStrategyAllowanceLimit, Allowance: &rpc.OrderAllowanceLimit{Pct: 0}, TIF: "DAY", Source: hedgeOrderSource})
	if err != nil || preview.PreviewToken == "" {
		t.Fatalf("partial preview: %v", err)
	}
	res, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
	if err != nil || res == nil || !res.Accepted || len(r.sends()) != 1 {
		t.Fatalf("two held + one pending + two reviewed should fit: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityUncertainOtherMonthRemainsReserved(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	scope := s.currentBrokerStateScope()
	if err := s.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-uncertain", PreviewTokenID: "synthetic-uncertain-token", ReservedOrderID: 301, Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", ConID: hedgeTestConID + 1, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Expiry: "20270319", Action: "SELL", OrderType: "LMT", LimitPrice: 6000, Quantity: 25, SendState: orderSendStateUncertainSend, Source: hedgeOrderSource}); err != nil {
		t.Fatal(err)
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 {
		t.Fatalf("uncertain sale lost reservation: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityIncompleteBrokerCommitmentRefuses(t *testing.T) {
	for _, kind := range []string{"direction", "quantity", "security type", "BAG legs", "terminal fill"} {
		t.Run(kind, func(t *testing.T) {
			s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
			r := newHedgeSendRig(t, s)
			token := hedgeAuthorityToken(t, s)
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				scope := s.currentBrokerStateScope()
				o := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 222, ClientID: 77, ClientIDPresent: true, Account: scope.Account, ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Action: "SELL", OrderType: "LMT", LimitPrice: 6000, TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
				switch kind {
				case "direction":
					o.Action = ""
				case "quantity":
					o.TotalQuantity, o.Remaining = 0, 0
				case "security type":
					o.SecType = ""
				case "BAG legs":
					o.SecType = "BAG"
				case "terminal fill":
					o.Status = "Filled"
					o.Remaining = 0
				}
				return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{o}}, scope, nil
			}
			_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
			if err == nil || len(r.sends()) != 0 {
				t.Fatalf("unknown %s sent: err=%v sends=%d", kind, err, len(r.sends()))
			}
		})
	}
}

func TestIndexHedgeAuthorityReceiptCannotHideNewOtherMonth(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	// An external fill is no longer in open orders or the local journal.
	// The correlated whole-book receipt already shows its new future month,
	// while the portfolio stream used by the mandate still has no future.
	s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
		return []*ibkr.RawPosition{{Account: s.currentBrokerStateScope().Account, Contract: ibkr.Contract{ConID: hedgeTestConID + 1, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5}, Position: -25}}, nil
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 {
		t.Fatalf("new other-month receipt ignored: err=%v sends=%d", err, len(r.sends()))
	}
}

func TestIndexHedgeAuthorityReceiptPreservesModelExclusions(t *testing.T) {
	bond := rpc.PositionView{ConID: 9201, Symbol: "UST", SecType: "BOND", Currency: "EUR", Quantity: 2}
	cash := rpc.PositionView{ConID: 9203, Symbol: "USD", SecType: "CASH", Currency: "EUR", Quantity: 100}
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), bond, cash)
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	original := s.reduceFillSnapshotForTest
	calls := 0
	s.reduceFillSnapshotForTest = func(ctx context.Context, d time.Duration) ([]*ibkr.RawPosition, error) {
		calls++
		rows, err := original(ctx, d)
		for _, row := range rows {
			if row.Contract.ConID == 9201 {
				row.Position = 2000
			}
			if row.Contract.ConID == 9203 {
				row.Position = 200
			}
		}
		return rows, err
	}
	res, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err != nil || res == nil || !res.Accepted || len(r.sends()) != 1 || calls != 1 {
		t.Fatalf("excluded bond units or duplicate receipt: err=%v sends=%d receipts=%d", err, len(r.sends()), calls)
	}
}

func TestIndexHedgeAuthorityReceiptChecksOptionContracts(t *testing.T) {
	option := mandateReductionCall("SPY", 9202, 2, 50, 50, 2.5, "20261218")
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), option)
	r := newHedgeSendRig(t, s)
	token := hedgeAuthorityToken(t, s)
	original := s.reduceFillSnapshotForTest
	s.reduceFillSnapshotForTest = func(ctx context.Context, d time.Duration) ([]*ibkr.RawPosition, error) {
		rows, err := original(ctx, d)
		for _, row := range rows {
			if row.Contract.ConID == 9202 {
				row.Position = 1
			}
		}
		return rows, err
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
	if err == nil || len(r.sends()) != 0 {
		t.Fatalf("option contract mismatch ignored: err=%v sends=%d", err, len(r.sends()))
	}
}
