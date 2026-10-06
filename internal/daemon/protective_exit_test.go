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
		{name: "whole-position trailing stop passes both gates", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5000), notional: 40860, inv: current},
		{name: "trail limit is a stop", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAILLIMIT, 5000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5000), notional: 40860, inv: current},
		{name: "ETF stop", draft: protectiveExitTestDraft("ETF", rpc.OrderTypeTRAIL, 300),
			position: protectiveExitTestPosition(300, rpc.OrderActionSell, 300), notional: 25000, inv: current},
		{name: "partial stop with a hand sale that fits", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 2000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 2000), notional: 16000, inv: protectiveExitInventory{Current: true, OtherWorkingSell: 3000}},
		{name: "limit sell keeps the notional cap", draft: protectiveExitTestDraft("STK", rpc.OrderTypeLMT, 5000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5000), notional: 40860, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "small limit sell keeps the short re-read", draft: protectiveExitTestDraft("STK", rpc.OrderTypeLMT, 10),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 10), notional: 80, inv: current, wantErr: "allow_stock_short"},
		{name: "flat position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 10),
			position: protectiveExitTestPosition(0, rpc.OrderActionSell, 10), notional: 80, inv: current, wantErr: "allow_stock_short"},
		{name: "short position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5000),
			position: protectiveExitTestPosition(-50, rpc.OrderActionSell, 5000), notional: 40860, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "quantity above the position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5001),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5001), notional: 40868, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "another working sell exceeds the position", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5000), notional: 40860, inv: protectiveExitInventory{Current: true, OtherWorkingSell: 1}, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "small stop with a competing sell keeps the short re-read", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 100),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 100), notional: 800, inv: protectiveExitInventory{Current: true, OtherWorkingSell: 4901}, wantErr: "allow_stock_short"},
		{name: "stale inventory fails closed", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5000),
			position: protectiveExitTestPosition(5000, rpc.OrderActionSell, 5000), notional: 40860, inv: protectiveExitInventory{}, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
		{name: "a shrink of a working stop passes despite a hand sale", draft: protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 3000),
			position: protectiveExitTestPosition(3000, rpc.OrderActionSell, 3000), notional: 24500, inv: protectiveExitInventory{Current: true, OtherWorkingSell: 2000, ReducesWorkingStop: true}},
		{name: "buy stop is not an exit of a long", draft: func() rpc.OrderDraft {
			d := protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 5000)
			d.Action = rpc.OrderActionBuy
			return d
		}(), position: protectiveExitTestPosition(-5000, rpc.OrderActionBuy, 5000), notional: 40860, inv: current, wantErr: "order cap in force 12,000 EUR (5% of NLV 240,000 EUR"},
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
			err := validateOrderRiskAuthority(limits, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", tc.inv)
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
		protectiveExitTestOrder(11, 7000, rpc.OrderActionSell, rpc.OrderTypeTRAIL, 5000), // the replace target
		protectiveExitTestOrder(0, 7001, rpc.OrderActionSell, rpc.OrderTypeLMT, 1200),    // a hand sale in TWS
		protectiveExitTestOrder(0, 7005, rpc.OrderActionBuy, rpc.OrderTypeLMT, 300),
		other, foreign, filled,
	}}
	draft := protectiveExitTestDraft("STK", rpc.OrderTypeTRAIL, 3800)
	inv := protectiveExitInventoryFromSnapshot(snapshot, protectiveExitTestScope, draft, orderPreviewReplaceTarget{ReservedOrderID: 11, PermID: 7000})
	if !inv.Current || inv.OtherWorkingSell != 1200 || !inv.ReducesWorkingStop {
		t.Fatalf("modify inventory = %+v, want current, 1200 competing, a reduction of the working stop", inv)
	}
	place := protectiveExitInventoryFromSnapshot(snapshot, protectiveExitTestScope, draft, orderPreviewReplaceTarget{})
	if place.OtherWorkingSell != 6200 || place.ReducesWorkingStop {
		t.Fatalf("place inventory = %+v, want 6200 competing and no reduction", place)
	}
}

func protectiveExitTestRow() rpc.TradeProposal {
	return rpc.TradeProposal{Key: "trailing_stop:synthetic", Revision: "sha256:synthetic", State: rpc.TradeProposalStateGenerated,
		Bucket: rpc.TradeProposalBucketTrailingStop, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, Quantity: 5000,
		PositionQuantity: 5000, PositionEffect: rpc.OrderPositionEffectClose, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC,
		Contract: rpc.ContractParams{ConID: 5001, Symbol: "SYNA", SecType: "STK", Exchange: "SMART", Currency: "EUR"},
		Trail:    &rpc.OrderTrailSpec{OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: new(8.0), InitialStopPrice: 7.5}}
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
