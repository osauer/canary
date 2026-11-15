package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// What names an order is its mode, account and client id; the socket it was
// placed through is not a gate (owner, 2026-10-20 08:00 CEST).
func TestValidateOrderViewMatchesGateNamesTheOrderByModeAccountAndClient(t *testing.T) {
	t.Parallel()
	status := rpc.TradingStatus{Mode: "live", Account: "U1234567", ClientID: 15, Endpoint: "127.0.0.1:4001"}
	view := rpc.OrderView{Mode: "live", Account: "U1234567", ClientID: 15, Endpoint: "127.0.0.1:7496", Open: true}
	if err := validateOrderViewMatchesGate(view, status); err != nil {
		t.Fatalf("order placed through TWS, daemon on the Gateway = %v, want accepted", err)
	}
	for name, differs := range map[string]func(rpc.OrderView) rpc.OrderView{
		"mode":    func(v rpc.OrderView) rpc.OrderView { v.Mode = "paper"; return v },
		"account": func(v rpc.OrderView) rpc.OrderView { v.Account = "U7654321"; return v },
		"client":  func(v rpc.OrderView) rpc.OrderView { v.ClientID = 0; return v },
	} {
		err := validateOrderViewMatchesGate(differs(view), status)
		if err == nil || !strings.Contains(err.Error(), "does not match current "+name) {
			t.Fatalf("another %s = %v, want the %s refusal", name, err, name)
		}
	}
}

// An order ID the broker reports for the daemon's client may be claimed
// though an earlier process placed it (#120), so a change proves the journal
// row and the broker's report on the bound socket mean the same order. An
// order this process placed is refused only on a report naming another
// order; any other order needs the report, with the row's account, reference
// and permanent ID; a row without a permanent ID can be cancelled on a report
// carrying its reference, never modified.
func TestValidateReportedOwnOrderRefusesAnotherOrder(t *testing.T) {
	t.Parallel()
	reports := map[int]ibkrlib.ReportedOwnOrder{
		1002: {PermID: 790001, Account: "DU1234567", OrderRef: "ord-a"},
		1003: {PermID: 790003, Account: "DU1234567", OrderRef: "ord-a", Conflicted: true},
		1004: {PermID: 790004, Account: "DU7654321", OrderRef: "ord-a"},
		1005: {PermID: 790005, Account: "DU1234567"},
	}
	broker := func(placed bool) *brokerOrderReports {
		return &brokerOrderReports{
			report: func(id int) (ibkrlib.ReportedOwnOrder, bool) {
				r, ok := reports[id]
				return r, ok
			},
			placedHere: func(int) bool { return placed },
		}
	}
	row := func(id, perm int, ref string) rpc.OrderView {
		return rpc.OrderView{ReservedOrderID: id, PermID: perm, Account: "du1234567", OrderRef: ref}
	}
	for _, tc := range []struct {
		name                         string
		view                         rpc.OrderView
		placed                       bool
		modifyRefused, cancelRefused bool
	}{
		{"same order", row(1002, 790001, "ord-a"), false, false, false},
		{"another order behind the ID", row(1002, 790009, "ord-a"), false, true, true},
		{"another reference behind the ID", row(1002, 790001, "ord-b"), false, true, true},
		{"not reported", row(1001, 790009, "ord-a"), false, true, true},
		{"not reported, placed here", row(1001, 790009, "ord-a"), true, false, false},
		{"no permanent ID, report carries the reference", row(1002, 0, "ord-a"), false, true, false},
		{"no permanent ID, report without a reference", row(1005, 0, "ord-a"), false, true, true},
		{"no permanent ID, placed here", row(1002, 0, "ord-a"), true, false, false},
		{"report without a reference for a row with one", row(1005, 790005, "ord-a"), false, true, true},
		{"report without a reference, placed here", row(1005, 790005, "ord-a"), true, false, false},
		{"reported twice with different identities", row(1003, 790003, "ord-a"), false, true, true},
		{"reported twice, placed here", row(1003, 790003, "ord-a"), true, true, true},
		{"reported for another account", row(1004, 790004, "ord-a"), false, true, true},
		{"no broker order ID", row(0, 790001, "ord-a"), false, false, false},
	} {
		if err := validateReportedOwnOrder(broker(tc.placed), tc.view, true); (err != nil) != tc.modifyRefused {
			t.Errorf("%s: modify err=%v, refused want %t", tc.name, err, tc.modifyRefused)
		}
		if err := validateReportedOwnOrder(broker(tc.placed), tc.view, false); (err != nil) != tc.cancelRefused {
			t.Errorf("%s: cancel err=%v, refused want %t", tc.name, err, tc.cancelRefused)
		}
	}
	if err := validateReportedOwnOrder(nil, row(1002, 790009, "ord-a"), true); err != nil {
		t.Errorf("no connector: err=%v, want nothing to compare", err)
	}
}

// The modify preview's what-if claims the order's own ID like the modify, so
// it names the order it changes; a row without a permanent ID names none.
func TestModifyPreviewWhatIfNamesTheOrderItChanges(t *testing.T) {
	t.Parallel()
	view := rpc.OrderView{ReservedOrderID: 1002, PermID: 790001, Account: "DU1234567", OrderRef: "ord-prot"}
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 50, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC,
		Contract: rpc.ContractParams{ConID: 910001, Symbol: "SYW", SecType: "STK", Exchange: "SMART", Currency: "USD"}}
	status := rpc.TradingStatus{Account: "DU1234567", ClientID: 31}
	order, err := modifyPreviewWhatIfOrder(view, draft, status)
	if err != nil {
		t.Fatal(err)
	}
	if order.Changes == nil || *order.Changes != (ibkrlib.OrderChangeIdentity{PermID: 790001, Account: "DU1234567", OrderRef: "ord-prot"}) {
		t.Fatalf("what-if names %+v, want the row's permanent ID, account and reference", order.Changes)
	}
	view.PermID = 0
	if order, err := modifyPreviewWhatIfOrder(view, draft, status); err != nil {
		t.Fatal(err)
	} else if order.Changes != nil {
		t.Fatalf("what-if of a row without a permanent ID names %+v, want none", order.Changes)
	}
}
