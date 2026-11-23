package daemon

import (
	"maps"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The account figures an Add sized from hold while marks tick them within
// the tolerance; a move beyond it, or a cash row appearing or going, does
// not hold.
func TestStockAddAccountMarkHolds(t *testing.T) {
	raw := map[string]string{"NetLiquidation": "1111000", "ExcessLiquidity": "333000", "CashBalance_USD": "22000", "SettledCash_USD": "21000", "CashBalance_EUR": "55000",
		"TotalCashValue": "77000", "GrossPositionValue": "999000"}
	was, ok := stockAddAccountMarkFrom(raw)
	if !ok || was.NetLiquidation != 1111000 || len(was.Cash) != 3 {
		t.Fatalf("mark %+v %v", was, ok)
	}
	tick := map[string]string{"NetLiquidation": "1114000", "ExcessLiquidity": "335500", "CashBalance_USD": "22000", "SettledCash_USD": "21000", "CashBalance_EUR": "55000",
		"TotalCashValue": "77000", "GrossPositionValue": "1002000"}
	now, _ := stockAddAccountMarkFrom(tick)
	if !stockAddAccountMarkHolds(was, now) {
		t.Fatal("a mark tick within the tolerance refused the Add")
	}
	for name, edit := range map[string]func(map[string]string){
		"net liquidation moved":  func(m map[string]string) { m["NetLiquidation"] = "1080000" },
		"excess liquidity moved": func(m map[string]string) { m["ExcessLiquidity"] = "300000" },
		"cash spent":             func(m map[string]string) { m["CashBalance_USD"] = "15000" },
		"a cash row appeared":    func(m map[string]string) { m["CashBalance_JPY"] = "100" },
		"a cash row went":        func(m map[string]string) { delete(m, "SettledCash_USD") },
	} {
		m := map[string]string{}
		maps.Copy(m, raw)
		edit(m)
		now, _ := stockAddAccountMarkFrom(m)
		if stockAddAccountMarkHolds(was, now) {
			t.Errorf("%s: held", name)
		}
	}
	if _, ok := stockAddAccountMarkFrom(map[string]string{"NetLiquidation": "1"}); ok {
		t.Fatal("a mark without excess liquidity")
	}
}

// Only buys and orders on the Add's contract matter to it.
func TestStockAddOrderMark(t *testing.T) {
	scope := brokerStateScope{Account: "U1234567", Mode: "live"}
	working := func(id, conID int, action string, qty float64) ibkrlib.OrderLifecycleEvent {
		return ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "U1234567", OrderID: id, PermID: 7000 + id, ConID: conID, Action: action,
			OrderType: "LMT", TotalQuantity: qty, Remaining: qty, LimitPrice: 10, Currency: "USD", Status: "Submitted"}
	}
	base := []ibkrlib.OrderLifecycleEvent{working(1, 900002, "SELL", 5), working(2, 900003, "BUY", 1)}
	was := stockAddOrderMark(base, 900001, scope)
	if got := stockAddOrderMark(append(base, working(3, 900002, "SELL", 7)), 900001, scope); got != was {
		t.Fatal("another contract's sell moved the Add's orders")
	}
	if got := stockAddOrderMark(append(base, working(4, 900004, "BUY", 2)), 900001, scope); got == was {
		t.Fatal("a buy on other cash did not move the Add's orders")
	}
	if got := stockAddOrderMark(append(base, working(5, 900001, "SELL", 2)), 900001, scope); got == was {
		t.Fatal("an order on the Add's contract did not move its orders")
	}
	changed := []ibkrlib.OrderLifecycleEvent{base[0], working(2, 900003, "BUY", 3)}
	if got := stockAddOrderMark(changed, 900001, scope); got == was {
		t.Fatal("a modified buy did not move the Add's orders")
	}
}

// A missing commission bound says which case it was.
func TestStockAddFeeProblem(t *testing.T) {
	for want, w := range map[string]rpc.OrderWhatIfResult{
		"no accepted answer in time": {Status: "timeout"},
		"no commission figures":      {Status: rpc.OrderWhatIfStatusAccepted, Available: true},
		"commission in EUR, not USD": {Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{CommissionCurrency: "EUR"}},
		"no usable bound":            {Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{CommissionCurrency: "USD"}},
	} {
		if got := stockAddFeeProblem(w, "USD"); !strings.Contains(got, want) {
			t.Errorf("%q: got %q", want, got)
		}
	}
}
