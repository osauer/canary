package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

var protectiveExitTestScope = brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}

func protectiveExitTestDraft(secType, orderType string, qty int) rpc.OrderDraft {
	return rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{ConID: 5001, Symbol: "SYNA", SecType: secType, Exchange: "SMART", Currency: "EUR"},
		Quantity:  qty,
		OrderType: orderType,
		TIF:       rpc.OrderTIFGTC,
	}
}

func protectiveExitTestPosition(before float64, action string, qty int) rpc.OrderPositionImpact {
	delta := float64(qty)
	if action == rpc.OrderActionSell {
		delta = -delta
	}
	return rpc.OrderPositionImpact{Before: before, After: before + delta, Effect: classifyPositionEffect(before, before+delta)}
}

func protectiveExitTestNotional(base float64) orderNotionalAuthority {
	return orderNotionalAuthority{QuoteNotional: base, ContractCurrency: "EUR", BaseNotional: base, BaseCurrency: "EUR",
		BasePerContract: 1, EvidenceAt: time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC), Source: orderFXSourceIdentity}
}

// The exemption boundary: only a stock/ETF sell stop that sells at most the
// long position, with complete current inventory and no competing sell,
// passes the order cap in force and the apparent-exit short re-read. The cap
// here scales with the book: 5% of NLV 240,000 EUR is 12,000 EUR.
func TestProtectiveStockExitExemptionBoundary(t *testing.T) {
	t.Parallel()
	current := protectiveExitInventory{Current: true}
	cases := []struct {
		name     string
		edit     func(*risk.ConstitutionOrderLimits)
		draft    rpc.OrderDraft
		position rpc.OrderPositionImpact
		notional float64
		inv      protectiveExitInventory
		wantErr  string
	}{
		{name: "whole-position trailing stop passes both gates", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4000), notional: 30000, inv: current},
		{name: "trail limit is a stop", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAILLIMIT, 4000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4000), notional: 30000, inv: current},
		{name: "ETF stop", draft: protectiveExitTestDraft("ETF", rpc.OrderTypeTRAIL, 300),
			position: protectiveExitTestPosition(300, rpc.OrderActionSell, 300), notional: 25000, inv: current},
		{name: "partial stop with a hand sale that fits", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 2000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 2000), notional: 15000, inv: protectiveExitInventory{Current: true, OtherWorkingSameSide: 2000}},
		{name: "limit sell keeps the notional cap", draft: protectiveExitTestDraft("STK", rpc.OrderTypeLMT, 4000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4000), notional: 30000, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "small limit sell keeps the short re-read", draft: protectiveExitTestDraft("STK", rpc.OrderTypeLMT, 10),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 10), notional: 75, inv: current, wantErr: "allow_stock_short"},
		{name: "flat position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 10),
			position: protectiveExitTestPosition(0, rpc.OrderActionSell, 10), notional: 75, inv: current, wantErr: "allow_stock_short"},
		{name: "short position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4000),
			position: protectiveExitTestPosition(-50, rpc.OrderActionSell, 4000), notional: 30000, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "quantity above the position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4001),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4001), notional: 30007.5, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "another working sell exceeds the position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4000), notional: 30000, inv: protectiveExitInventory{Current: true, OtherWorkingSameSide: 1}, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "small stop with a competing sell keeps the short re-read", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 100),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 100), notional: 750, inv: protectiveExitInventory{Current: true, OtherWorkingSameSide: 3901}, wantErr: "allow_stock_short"},
		{name: "stale inventory fails closed", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4000),
			position: protectiveExitTestPosition(4000, rpc.OrderActionSell, 4000), notional: 30000, inv: protectiveExitInventory{}, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "a shrink of a working stop passes despite a hand sale", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 3000),
			position: protectiveExitTestPosition(3000, rpc.OrderActionSell, 3000), notional: 22500, inv: protectiveExitInventory{Current: true, OtherWorkingSameSide: 1000, ReducesWorkingStop: true}},
		{name: "buy stop is not an exit of a long", draft: func() rpc.OrderDraft {
			d := protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 4000)
			d.Action = rpc.OrderActionBuy
			return d
		}(), position: protectiveExitTestPosition(-4000, rpc.OrderActionBuy, 4000), notional: 30000, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "option sell stop is unaffected", draft: func() rpc.OrderDraft {
			d := protectiveExitTestDraft("OPT", rpc.OrderTypeTRAIL, 5)
			d.Contract.Right, d.Contract.Strike, d.Contract.Expiry, d.Contract.Multiplier = "C", 50, "20261120", 100
			return d
		}(), position: protectiveExitTestPosition(5, rpc.OrderActionSell, 5), notional: 20000, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "option contract cap is unaffected", edit: func(o *risk.ConstitutionOrderLimits) { o.MaxOptionContracts = new(2) }, draft: func() rpc.OrderDraft {
			d := protectiveExitTestDraft("OPT", rpc.OrderTypeTRAIL, 5)
			d.Contract.Right, d.Contract.Strike, d.Contract.Expiry, d.Contract.Multiplier = "C", 50, "20261120", 100
			return d
		}(), position: protectiveExitTestPosition(5, rpc.OrderActionSell, 5), notional: 500, inv: current, wantErr: "max_option_contracts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := testOrderLimitsTable(10000)
			if tc.edit != nil {
				tc.edit(table)
			}
			limits := risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
			err := validateOrderRiskAuthority(limits, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", tc.inv, deltaReductionEvidence{})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want the protective exit to pass", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want a refusal naming %s", err, tc.wantErr)
			}
		})
	}
}

func protectiveExitTestOrder(orderID, permID int, action, orderType string, qty float64) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: orderID, PermID: permID, Account: "DU1234567",
		Symbol: "SYNA", SecType: "STK", ConID: 5001, Currency: "EUR", Action: action, OrderType: orderType,
		TotalQuantity: qty, Remaining: qty, Status: "Submitted"}
}

// The inventory counts every other working sell for the exact contract, hand
// orders included, and excludes the replace target, buys, other contracts,
// other accounts and finished orders.
func TestProtectiveExitInventoryCountsCompetingSells(t *testing.T) {
	t.Parallel()
	other := protectiveExitTestOrder(0, 7002, rpc.OrderActionSell, rpc.OrderTypeTRAIL, 400)
	other.ConID, other.Symbol = 5002, "SYNB"
	foreign := protectiveExitTestOrder(0, 7003, rpc.OrderActionSell, rpc.OrderTypeLMT, 900)
	foreign.Account = "DU7654321"
	filled := protectiveExitTestOrder(0, 7004, rpc.OrderActionSell, rpc.OrderTypeLMT, 50)
	filled.Status, filled.Filled, filled.Remaining = "Filled", 50, 0
	snapshot := ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: time.Now(), Orders: []ibkrlib.OrderLifecycleEvent{
		protectiveExitTestOrder(11, 7000, rpc.OrderActionSell, rpc.OrderTypeTRAIL, 4000), // the replace target
		protectiveExitTestOrder(0, 7001, rpc.OrderActionSell, rpc.OrderTypeLMT, 1200),    // a hand sale in TWS
		protectiveExitTestOrder(0, 7005, rpc.OrderActionBuy, rpc.OrderTypeLMT, 300),
		other, foreign, filled,
	}}
	draft := protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 2800)
	inv := protectiveExitInventoryFromSnapshot(snapshot, nil, protectiveExitTestScope, draft, orderPreviewReplaceTarget{ReservedOrderID: 11, PermID: 7000})
	if !inv.Current || inv.OtherWorkingSameSide != 1200 || !inv.ReducesWorkingStop {
		t.Fatalf("modify inventory = %+v, want current, 1200 competing, a reduction of the working stop", inv)
	}
	place := protectiveExitInventoryFromSnapshot(snapshot, nil, protectiveExitTestScope, draft, orderPreviewReplaceTarget{})
	if place.OtherWorkingSameSide != 5200 || place.ReducesWorkingStop {
		t.Fatalf("place inventory = %+v, want 5200 competing and no reduction", place)
	}
}

// The inventory follows the order's direction and class: beside a
// buy-to-close of a short option it counts the other working buys on that
// exact option (a buy without a ConID that names the underlying counts, a
// sell or another option does not), and beside a strategy combo it counts
// per leg in each leg's direction.
func TestProtectiveExitInventoryFollowsTheOrdersDirection(t *testing.T) {
	t.Parallel()
	optionOrder := func(permID int, conID int, action string, qty float64) ibkrlib.OrderLifecycleEvent {
		o := protectiveExitTestOrder(0, permID, action, rpc.OrderTypeLMT, qty)
		o.SecType, o.ConID, o.Symbol, o.Expiry, o.Strike, o.Right = "OPT", conID, "SYNB", "20261218", 75, "P"
		return o
	}
	handBuy := optionOrder(8004, 0, rpc.OrderActionBuy, 1) // a hand order the API reports without a ConID
	snapshot := ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: time.Now(), Orders: []ibkrlib.OrderLifecycleEvent{
		optionOrder(8001, deltaTestShortPuts, rpc.OrderActionBuy, 3),
		optionOrder(8002, deltaTestShortPuts, rpc.OrderActionSell, 2),
		optionOrder(8003, deltaTestLongCalls, rpc.OrderActionBuy, 4),
		handBuy,
	}}
	buyBack := deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 8)
	inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, buyBack, orderPreviewReplaceTarget{})
	if !inv.Current || inv.OtherWorkingSameSide != 4 || inv.OtherWorkingSameSideByLeg != nil {
		t.Fatalf("buy-back inventory = %+v, want 4 competing buys (3 on the contract, 1 hand order without a ConID)", inv)
	}
	sellCalls := deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionSell, 8)
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, sellCalls, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 0 {
		t.Fatalf("sell inventory = %+v, want no competing sells", inv)
	}
	stock := deltaTestStockDraft(rpc.OrderActionBuy, 100)
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, stock, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 0 {
		t.Fatalf("stock inventory = %+v, want option orders not to count against the stock", inv)
	}
	combo, _ := deltaTestStrategyDraft(8, deltaTestLeg(deltaTestLongCalls, 1, rpc.OrderActionSell, 8, 8), deltaTestLeg(deltaTestShortPuts, -1, rpc.OrderActionBuy, 8, -8))
	inv = protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, combo, orderPreviewReplaceTarget{})
	if !inv.Current || inv.OtherWorkingSameSideByLeg[deltaTestLongCalls] != 0 || inv.OtherWorkingSameSideByLeg[deltaTestShortPuts] != 4 ||
		inv.otherWorkingSameSide(deltaTestShortPuts) != 4 {
		t.Fatalf("combo inventory = %+v, want per-leg counts in each leg's direction (calls sold: 0; puts bought back: 3 on the contract plus the hand buy without a ConID)", inv)
	}

	// A working combo close (BAG) reports no legs, so its units count against
	// every option leg of its underlying whatever the exit's direction, as a
	// lower bound; it never counts against the stock.
	bag := protectiveExitTestOrder(0, 8005, rpc.OrderActionSell, rpc.OrderTypeLMT, 2)
	bag.SecType, bag.ConID, bag.Symbol = "BAG", 0, "SYNB"
	snapshot.Orders = append(snapshot.Orders, bag)
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, buyBack, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 6 {
		t.Fatalf("buy-back inventory with a working combo = %+v, want 4 + 2 units of the combo", inv)
	}
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, sellCalls, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 2 {
		t.Fatalf("sell inventory with a working combo = %+v, want the combo's 2 units", inv)
	}
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, stock, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 0 {
		t.Fatalf("stock inventory with a working combo = %+v, want the combo not to count against the stock", inv)
	}
	if inv := protectiveExitInventoryFromSnapshot(snapshot, nil, deltaTestScope, combo, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSideByLeg[deltaTestLongCalls] != 2 || inv.OtherWorkingSameSideByLeg[deltaTestShortPuts] != 6 {
		t.Fatalf("combo inventory with a working combo = %+v, want the combo's units on every leg", inv)
	}
}

func protectiveExitTestRow() rpc.TradeProposal {
	return rpc.TradeProposal{Key: "trailing_stop:synthetic", Revision: "sha256:synthetic", State: rpc.TradeProposalStateGenerated,
		Bucket: rpc.TradeProposalBucketTrailingStop, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, Quantity: 4000,
		PositionQuantity: 4000, PositionEffect: rpc.OrderPositionEffectClose, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC,
		Contract: rpc.ContractParams{ConID: 5001, Symbol: "SYNA", SecType: "STK", Exchange: "SMART", Currency: "EUR"},
		Trail:    &rpc.OrderTrailSpec{OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: new(8.0), InitialStopPrice: 6.9}}
}

// A trailing-stop row no longer reads ready when its placement would be
// refused: a competing hand sale makes it not executable with a plain
// message, an unreadable inventory makes it wait for the broker, and a clean
// inventory leaves it ready.
func TestTrailingStopRowReadinessFollowsTheExemption(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 5, 15, 40, 0, 0, time.UTC)
	cases := []struct {
		name      string
		inventory func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error)
		code      string
		readiness string
		message   string
	}{
		{name: "clean inventory", readiness: rpc.ReadinessReady,
			inventory: func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: at}, protectiveExitTestScope, nil
			}},
		{name: "hand sale in TWS", code: protectiveExitCompetingSellCode, readiness: rpc.ReadinessNotExecutable, message: "another working sell order already covers 1000",
			inventory: func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: at, Orders: []ibkrlib.OrderLifecycleEvent{
					protectiveExitTestOrder(0, 7001, rpc.OrderActionSell, rpc.OrderTypeLMT, 1000)}}, protectiveExitTestScope, nil
			}},
		{name: "inventory unavailable", code: protectiveExitInventoryUnavailableCode, readiness: rpc.ReadinessBrokerUnavailable, message: "cannot read the broker's open orders",
			inventory: func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{}, protectiveExitTestScope, errors.New("synthetic outage")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readinessPreviewServer(t, t.TempDir(), at)
			srv.openOrderInventoryForTest = tc.inventory
			engine := &proposalEngine{server: srv, now: srv.now, scope: func() brokerStateScope { return protectiveExitTestScope }}
			row := protectiveExitTestRow()
			var book protectiveExitBook
			if b, ok := engine.protectiveExitRowBlocker(context.Background(), row, &book); ok {
				proposalBlockWith(&row, []rpc.TradingBlocker{b})
			}
			if tc.code == "" {
				if row.State == rpc.TradeProposalStateBlocked || len(row.Blockers) != 0 {
					t.Fatalf("row = %+v, want it unblocked", row)
				}
			} else if row.State != rpc.TradeProposalStateBlocked || len(row.Blockers) != 1 || row.Blockers[0].Code != tc.code {
				t.Fatalf("row blockers = %+v, want %s", row.Blockers, tc.code)
			}
			r := engine.classifyReadiness(row, row.Blockers, false, readinessSessions{})
			if r.Code != tc.readiness || !strings.Contains(r.Message, tc.message) {
				t.Fatalf("readiness = %+v, want %s naming %q", r, tc.readiness, tc.message)
			}
		})
	}
	// With allow_stock_short the row reads no exemption blocker: the notional
	// cap decides at preview from exact FX evidence.
	if _, ok := protectiveExitProposalBlocker(true, protectiveExitTestRow(), protectiveExitInventory{}); ok {
		t.Fatal("allow_stock_short row must not carry an exemption blocker")
	}
}
