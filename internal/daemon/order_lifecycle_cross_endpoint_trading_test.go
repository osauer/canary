//go:build trading

package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Synthetic identity of Canary's stop in the stop guard rig: order 1002 of
// client 31 on DU1234567, reference ord-prot, permanent ID crossPermID.
const crossPermID = 790001

// stopOpenOrder is the broker's openOrder echo of Canary's stop at qty, the
// way IB Gateway or TWS reports it: client ID, account, order reference,
// order ID and permanent ID all on the callback.
func stopOpenOrder(qty float64) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1002, PermID: crossPermID, ClientID: 31, ClientIDPresent: true,
		Account: "DU1234567", OrderRef: "ord-prot", Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, TotalQuantity: qty, Remaining: qty,
		AuxPrice: 8.5, TrailStopPrice: 90, Status: "Submitted",
	}
}

// stopStatus is the broker's orderStatus for Canary's stop: it carries the
// client ID and permanent ID but no account or order reference.
func stopStatus(status string, filled, remaining float64) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 1002, PermID: crossPermID, ClientID: 31, ClientIDPresent: true,
		Status: status, Filled: filled, Remaining: remaining,
	}
}

func (r *stopGuardRig) deliver(t *testing.T, events ...ibkrlib.OrderLifecycleEvent) {
	t.Helper()
	for _, ev := range events {
		if !r.srv.appendOrderLifecycleEvent(ev) {
			t.Fatalf("callback %s for order %d was not committed", ev.Type, ev.OrderID)
		}
	}
}

// brokerAcknowledges delivers the broker's echo of the stop at qty, each
// callback twice (IBKR repeats openOrder and orderStatus freely), and makes
// the fake broker's inventory show it.
func (r *stopGuardRig) brokerAcknowledges(t *testing.T, qty float64) {
	t.Helper()
	open, status := stopOpenOrder(qty), stopStatus("Submitted", 0, qty)
	r.deliver(t, open, status, open, status)
	r.mu.Lock()
	r.broker.quantity, r.broker.permID = qty, crossPermID
	r.mu.Unlock()
}

// seedStopPermID records the broker's acknowledgement with its permanent ID
// on the stop's own route, as the daemon journaled it before the move.
func seedStopPermID(t *testing.T, r *stopGuardRig) {
	t.Helper()
	seed, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventBrokerAcknowledged)
	if !ok {
		t.Fatal("no seeded broker acknowledgement")
	}
	seed.PermID = crossPermID
	if err := r.srv.orderJournal.Append(seed); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.broker.permID = crossPermID
	r.mu.Unlock()
}

// onlyStopRow is the one journal row of Canary's stop, on any socket.
func onlyStopRow(t *testing.T, srv *Server, placed string) rpc.OrderView {
	t.Helper()
	rows := stopGuardRows(t, srv)
	if len(rows) != 1 {
		t.Fatalf("journal rows of the stop = %d (%+v), want one", len(rows), rows)
	}
	if rows[0].Endpoint != placed {
		t.Fatalf("stop row endpoint = %s, want the socket it was placed through, %s", rows[0].Endpoint, placed)
	}
	return rows[0]
}

func wantManageable(t *testing.T, row rpc.OrderView, qty float64) {
	t.Helper()
	if row.SendState != orderSendStateBrokerAcknowledged || row.Quantity != qty || row.PermID != crossPermID ||
		!row.ModifyEligible || !row.CancelEligible || !row.Open {
		t.Fatalf("stop row = send %s qty %v perm %d modify %t cancel %t open %t, want acknowledged at %v and changeable",
			row.SendState, row.Quantity, row.PermID, row.ModifyEligible, row.CancelEligible, row.Open, qty)
	}
}

var crossEndpointMoves = []struct {
	name   string
	placed string
	port   int
}{
	{"TWS to IB Gateway", "127.0.0.1:7497", 4002},
	{"IB Gateway to TWS", "127.0.0.1:4002", 7497},
	{"same socket (control)", "127.0.0.1:4002", 4002},
}

// A stop placed through one app keeps being managed through the other (the
// v3.25.4 review's finding 2): the guard shrinks it, the broker's
// acknowledgement reaches its one row, the guard shrinks it again, and the
// owner cancels it. Before the fix the acknowledgement was dropped for its
// endpoint, the row stayed send_attempted and every later change was
// refused ("order is not cancel-eligible").
func TestStopKeepsBeingManagedAcrossAnEndpointMove(t *testing.T) {
	t.Parallel()
	for _, tc := range crossEndpointMoves {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), tc.placed, tc.port, true)
			seedStopPermID(t, r)

			r.set(60, true)
			r.pass(t)
			if m, c := r.writes(); m != 1 || c != 0 || r.modified[0].TotalQty != 60 {
				t.Fatalf("first shrink: %d modifies %d cancels, want one to 60", m, c)
			}
			if row := onlyStopRow(t, r.srv, tc.placed); row.SendState != orderSendStateSendAttempted || row.ModifyEligible {
				t.Fatalf("before the acknowledgement: send %s modify %t, want send_attempted and not changeable", row.SendState, row.ModifyEligible)
			}
			r.brokerAcknowledges(t, 60)
			wantManageable(t, onlyStopRow(t, r.srv, tc.placed), 60)

			r.set(30, true)
			r.pass(t)
			if m, c := r.writes(); m != 2 || c != 0 || r.modified[1].TotalQty != 30 || r.modified[1].OrderID != 1002 {
				t.Fatalf("second shrink: %d modifies %d cancels, want a second one of order 1002 to 30", m, c)
			}
			r.brokerAcknowledges(t, 30)
			wantManageable(t, onlyStopRow(t, r.srv, tc.placed), 30)

			if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY}); err != nil {
				t.Fatalf("owner cancel after two shrinks: %v", err)
			}
			if _, c := r.writes(); c != 1 || r.cancelled[0] != 1002 {
				t.Fatalf("cancels = %v, want order 1002", r.cancelled)
			}
			cancelled := stopStatus("Cancelled", 0, 30)
			r.deliver(t, cancelled, cancelled)
			if row := onlyStopRow(t, r.srv, tc.placed); row.Open || row.LifecycleStatus != rpc.OrderLifecycleCancelled {
				t.Fatalf("after the broker's cancel: open %t status %s, want cancelled", row.Open, row.LifecycleStatus)
			}
		})
	}
}

// A fill reported through the other app, its execution and status each
// delivered twice, lands once on the stop's one row: no second row under the
// current socket, and the filled quantity is the broker's cumulative 60, not
// 120.
func TestStopFillAfterAnEndpointMoveLandsOnItsOneRow(t *testing.T) {
	t.Parallel()
	for _, tc := range crossEndpointMoves {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), tc.placed, tc.port, true)
			seedStopPermID(t, r)
			r.set(60, true)
			r.pass(t)
			r.brokerAcknowledges(t, 60)

			exec := ibkrlib.OrderLifecycleEvent{
				Type: ibkrlib.OrderLifecycleEventExecDetails, OrderID: 1002, PermID: crossPermID, ClientID: 31, ClientIDPresent: true,
				Account: "DU1234567", OrderRef: "ord-prot", Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD",
				ExecID: "0000e0f1.synthetic.01", Shares: 60, CumQty: 60, Price: 89.5, AvgFillPrice: 89.5, ExecutionSide: "SLD",
			}
			filled := stopStatus("Filled", 60, 0)
			r.deliver(t, exec, filled, exec, filled)

			row := onlyStopRow(t, r.srv, tc.placed)
			if row.Open || row.Filled != 60 || row.LifecycleStatus != rpc.OrderLifecycleFilled {
				t.Fatalf("after the fill: open %t filled %v status %s, want filled 60 once", row.Open, row.Filled, row.LifecycleStatus)
			}
		})
	}
}

// Issue #120 with the move: a restarted daemon on IB Gateway manages the
// stop its predecessor placed through TWS. The connect download (openOrder
// and orderStatus for the daemon's client) reaches the stop's one row, the
// broker's report names the row's permanent ID and account, and the stop is
// shrunk, shrunk again and cancelled. A report naming another permanent ID
// refuses both the change and the cancel.
func TestRestartedDaemonManagesAStopItsPredecessorPlacedThroughTheOtherApp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	before := newStopGuardRigIn(t, config.Trading{}, dir, "127.0.0.1:7497", 7497, true)
	before.deliver(t, stopOpenOrder(100), stopStatus("Submitted", 0, 100))
	if row := onlyStopRow(t, before.srv, "127.0.0.1:7497"); row.PermID != crossPermID {
		t.Fatalf("predecessor's row perm = %d, want %d", row.PermID, crossPermID)
	}
	// The predecessor stops before its successor opens the same authority.
	if err := before.srv.coreStore.Close(); err != nil {
		t.Fatal(err)
	}

	r := newStopGuardRigIn(t, config.Trading{}, dir, "", 4002, false)
	reported := ibkrlib.ReportedOwnOrder{PermID: crossPermID, Account: "DU1234567", OrderRef: "ord-prot"}
	r.srv.brokerOrderReportsForTest = &brokerOrderReports{
		report:     func(id int) (ibkrlib.ReportedOwnOrder, bool) { return reported, id == 1002 },
		placedHere: func(int) bool { return false },
	}
	r.brokerAcknowledges(t, 100) // the connect download
	wantManageable(t, onlyStopRow(t, r.srv, "127.0.0.1:7497"), 100)

	r.set(60, true)
	r.pass(t)
	r.brokerAcknowledges(t, 60)
	wantManageable(t, onlyStopRow(t, r.srv, "127.0.0.1:7497"), 60)
	r.set(30, true)
	r.pass(t)
	r.brokerAcknowledges(t, 30)
	if m, c := r.writes(); m != 2 || r.modified[0].TotalQty != 60 || r.modified[1].TotalQty != 30 || c != 0 {
		t.Fatalf("writes = %d modifies %d cancels, want shrinks to 60 and 30", m, c)
	}
	// Each modify names the order it changes, so the order-ID claim can
	// compare it with the broker's report under its lock.
	want := ibkrlib.OrderChangeIdentity{PermID: crossPermID, Account: "DU1234567", OrderRef: "ord-prot"}
	for i, sent := range r.modified {
		if sent.Changes == nil || *sent.Changes != want {
			t.Fatalf("modify %d names %+v, want %+v", i, sent.Changes, want)
		}
	}
	wantManageable(t, onlyStopRow(t, r.srv, "127.0.0.1:7497"), 30)

	reported.PermID = 790009 // the broker now names another order behind ID 1002
	r.set(10, true)
	r.pass(t)
	if m, _ := r.writes(); m != 2 {
		t.Fatalf("modifies = %d, want none sent for an order ID the broker reports under another permanent ID", m)
	}
	_, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY})
	if err == nil || !strings.Contains(err.Error(), "permanent ID 790009") {
		t.Fatalf("cancel with a mismatched broker report = %v, want the permanent ID refusal", err)
	}
	reported.PermID = crossPermID
	if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY}); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	if _, c := r.writes(); c != 1 || r.cancelled[0] != 1002 {
		t.Fatalf("cancels = %v, want order 1002", r.cancelled)
	}
}

// A callback without exact identity never resolves the stop's outstanding
// change across sockets: another client ID, an omitted client ID, another
// account, another permanent ID, none, or the stop's permanent ID under
// another reference or order ID. The change stays unresolved, no row is
// minted for the callback, and the stop is not cancel-eligible.
func TestCrossEndpointCallbackNeedsExactBrokerIdentity(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*ibkrlib.OrderLifecycleEvent){
		"another client ID":           func(ev *ibkrlib.OrderLifecycleEvent) { ev.ClientID = 32 },
		"client 0 (hand order)":       func(ev *ibkrlib.OrderLifecycleEvent) { ev.ClientID = 0 },
		"no client ID on the wire":    func(ev *ibkrlib.OrderLifecycleEvent) { ev.ClientIDPresent = false },
		"another account":             func(ev *ibkrlib.OrderLifecycleEvent) { ev.Account = "DU7654321" },
		"another permanent ID":        func(ev *ibkrlib.OrderLifecycleEvent) { ev.PermID = 790002 },
		"no permanent ID":             func(ev *ibkrlib.OrderLifecycleEvent) { ev.PermID = 0 },
		"perm ID under another ref":   func(ev *ibkrlib.OrderLifecycleEvent) { ev.OrderRef = "ord-other" },
		"perm ID under another order": func(ev *ibkrlib.OrderLifecycleEvent) { ev.OrderID = 1003 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:7497", 4002, true)
			seedStopPermID(t, r)
			r.set(60, true)
			r.pass(t)
			open, status := stopOpenOrder(60), stopStatus("Submitted", 0, 60)
			mutate(&open)
			mutate(&status)
			r.deliver(t, open, status)
			row := onlyStopRow(t, r.srv, "127.0.0.1:7497")
			if row.SendState != orderSendStateSendAttempted || row.ModifyEligible || row.CancelEligible {
				t.Fatalf("after a callback with %s: send %s modify %t cancel %t, want the change unresolved", name, row.SendState, row.ModifyEligible, row.CancelEligible)
			}
			if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY}); err == nil {
				t.Fatalf("cancel after a callback with %s was accepted", name)
			}
		})
	}
}

// A stop the broker never acknowledged before the move (no permanent ID in
// the journal) pairs only with a callback naming its order reference and
// order ID both: orderStatus, which carries neither, and an openOrder
// without the reference wait; the full openOrder resolves the change and
// introduces the permanent ID to the one row.
func TestCrossEndpointFirstAcknowledgementNeedsReferenceAndOrderID(t *testing.T) {
	t.Parallel()
	r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:7497", 4002, true)
	r.set(60, true)
	r.pass(t)
	noRef := stopOpenOrder(60)
	noRef.OrderRef = ""
	r.deliver(t, stopStatus("Submitted", 0, 60), noRef)
	if row := onlyStopRow(t, r.srv, "127.0.0.1:7497"); row.SendState != orderSendStateSendAttempted || row.PermID != 0 {
		t.Fatalf("after a status and a reference-less openOrder: send %s perm %d, want unresolved without a permanent ID", row.SendState, row.PermID)
	}
	r.brokerAcknowledges(t, 60)
	wantManageable(t, onlyStopRow(t, r.srv, "127.0.0.1:7497"), 60)
}

// A row journaled in live mode never pairs with a callback of the paper
// session, though account, client ID, reference, order ID and permanent ID
// all agree.
func TestCrossEndpointCallbackNeverCrossesTradingMode(t *testing.T) {
	t.Parallel()
	r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:7497", 4002, true)
	live, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventBrokerAcknowledged)
	if !ok {
		t.Fatal("no seeded broker acknowledgement")
	}
	live.OrderRef, live.Mode, live.Endpoint, live.PermID, live.ReservedOrderID = "ord-live", "live", "127.0.0.1:7496", 790005, 1005
	if err := r.srv.orderJournal.Append(live); err != nil {
		t.Fatal(err)
	}
	open := stopOpenOrder(40)
	open.OrderRef, open.PermID, open.OrderID = "ord-live", 790005, 1005
	r.deliver(t, open)
	views, _, err := r.srv.loadOrderViews()
	if err != nil {
		t.Fatal(err)
	}
	var rows []rpc.OrderView
	for _, v := range views {
		if v.OrderRef == "ord-live" {
			rows = append(rows, v)
		}
	}
	if len(rows) != 1 || rows[0].Mode != "live" || rows[0].Quantity != 100 {
		t.Fatalf("ord-live rows = %+v, want the one live row untouched by a paper callback", rows)
	}
}

// The v3.25.4 review's witness, kept as found: the guard resizes a stop
// journaled through 127.0.0.1:7496 while the daemon is on 127.0.0.1:4002,
// an orderStatus alone acknowledges it, and the owner's cancel then goes
// through. On v3.25.4 the status was dropped and the cancel refused "order
// is not cancel-eligible"; the unchanged-endpoint control passed.
func TestReviewWitnessStatusAcknowledgesAResizeAcrossTheMove(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"127.0.0.1:4002", "127.0.0.1:7496"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			r := newStopGuardRigPlacedThrough(t, config.Trading{}, endpoint)
			seedStopPermID(t, r)
			r.set(50, true)
			r.pass(t)
			if m, c := r.writes(); m != 1 || c != 0 {
				t.Fatalf("initial resize: modifies=%d cancels=%d", m, c)
			}
			status := stopStatus("Submitted", 0, 50)
			status.Account, status.OrderRef = "DU1234567", "ord-prot"
			r.deliver(t, status)
			row := onlyStopRow(t, r.srv, endpoint)
			if row.SendState != orderSendStateBrokerAcknowledged || !row.ModifyEligible || !row.CancelEligible {
				t.Fatalf("after the acknowledged resize: send %s modify %t cancel %t", row.SendState, row.ModifyEligible, row.CancelEligible)
			}
			if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY}); err != nil {
				t.Fatalf("owner cannot cancel the broker-acknowledged resized stop: %v", err)
			}
		})
	}
}

// A cancel of a row without a permanent ID (an order an earlier process
// sent and the broker never acknowledged in the journal) needs proof too:
// with no broker report of its order ID on this connection, or a report
// under another reference, it is refused in plain words; a report carrying
// its reference cancels it; an order this process placed needs no report.
func TestCancelOfAnUnacknowledgedRowNeedsTheBrokersReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		report  *ibkrlib.ReportedOwnOrder
		placed  bool
		refusal string // "" for a sent cancel
	}{
		{"not reported, not placed here", nil, false, "has not reported order ID 1002"},
		{"reported under another reference", &ibkrlib.ReportedOwnOrder{PermID: 799999, Account: "DU1234567", OrderRef: "ord-other"}, false, "another order reference"},
		{"reported without a reference", &ibkrlib.ReportedOwnOrder{PermID: 799999, Account: "DU1234567"}, false, "carries no order reference"},
		{"reported with its reference", &ibkrlib.ReportedOwnOrder{PermID: crossPermID, Account: "DU1234567", OrderRef: "ord-prot"}, false, ""},
		{"placed by this process", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:4002", 7497, true)
			r.srv.brokerOrderReportsForTest = &brokerOrderReports{
				report: func(id int) (ibkrlib.ReportedOwnOrder, bool) {
					if tc.report == nil || id != 1002 {
						return ibkrlib.ReportedOwnOrder{}, false
					}
					return *tc.report, true
				},
				placedHere: func(id int) bool { return tc.placed && id == 1002 },
			}
			_, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY})
			_, c := r.writes()
			switch {
			case tc.refusal == "" && (err != nil || c != 1):
				t.Fatalf("cancel = %v with %d sent, want it sent", err, c)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal) || c != 0):
				t.Fatalf("cancel = %v with %d sent, want refused naming %q", err, c, tc.refusal)
			}
		})
	}
}

// The cancel's identity check runs again at the send, under the write lock,
// on the bound socket (the refuter's second round, F5): a report that names
// another order by then refuses the cancel and nothing is sent.
func TestCancelRechecksTheBrokersReportAtTheSend(t *testing.T) {
	t.Parallel()
	r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:7497", 4002, true)
	seedStopPermID(t, r)
	var reads int
	r.srv.brokerOrderReportsForTest = &brokerOrderReports{
		report: func(id int) (ibkrlib.ReportedOwnOrder, bool) {
			reads++
			perm := crossPermID
			if reads > 1 {
				perm = 790009 // the broker now names another order behind ID 1002
			}
			return ibkrlib.ReportedOwnOrder{PermID: perm, Account: "DU1234567", OrderRef: "ord-prot"}, id == 1002
		},
		placedHere: func(int) bool { return false },
	}
	_, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY})
	if _, c := r.writes(); err == nil || !strings.Contains(err.Error(), "permanent ID 790009") || c != 0 || reads < 2 {
		t.Fatalf("cancel = %v with %d sent after %d report reads, want refused at the send naming permanent ID 790009", err, c, reads)
	}
}
