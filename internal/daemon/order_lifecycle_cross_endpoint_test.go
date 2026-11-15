package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A callback pairs with a row placed through another socket only when
// exactly one row qualifies and no row of the scope contradicts it (the
// v3.25.4 review's finding 2); since the refuter's review two rows naming
// the callback's reference are ambiguous on any socket.
func TestCrossEndpointPairingRefusesAmbiguousOrContradictedRows(t *testing.T) {
	t.Parallel()
	row := func(endpoint, ref string, orderID, perm int) rpc.OrderView {
		return rpc.OrderView{Endpoint: endpoint, ClientID: 31, Account: "DU1234567", Mode: "paper", OrderRef: ref, ReservedOrderID: orderID, PermID: perm, Open: true}
	}
	callback := orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, Endpoint: "127.0.0.1:4002", ClientID: 31, clientIDPresent: true,
		Account: "DU1234567", Mode: "paper", OrderRef: "ord-prot", ReservedOrderID: 1002, PermID: 790001}
	for _, tc := range []struct {
		name     string
		ev       func(orderJournalEvent) orderJournalEvent
		views    []rpc.OrderView
		endpoint string // the paired row's endpoint, "" for no pairing
	}{
		{"one row on another socket", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 790001)}, "127.0.0.1:7497"},
		{"an unacknowledged row on another socket", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0)}, "127.0.0.1:7497"},
		{"an unacknowledged row under another order ID",
			func(ev orderJournalEvent) orderJournalEvent { ev.ReservedOrderID = 1003; return ev },
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0)}, ""},
		{"an unacknowledged row, callback without the order ID",
			func(ev orderJournalEvent) orderJournalEvent { ev.ReservedOrderID = 0; return ev },
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0)}, ""},
		{"two unacknowledged rows naming the reference, one on this socket", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0), row("127.0.0.1:4002", "ord-prot", 1002, 0)}, ""},
		{"the permanent ID's holder on this socket", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0), row("127.0.0.1:4002", "ord-prot", 1002, 790001)}, "127.0.0.1:4002"},
		{"two unacknowledged rows on other sockets", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 0), row("127.0.0.1:7496", "ord-prot", 1002, 0)}, ""},
		{"the reference on this socket under another permanent ID", nil,
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 790001), row("127.0.0.1:4002", "ord-prot", 1002, 790002)}, ""},
		{"an error callback without client or permanent ID",
			func(ev orderJournalEvent) orderJournalEvent {
				ev.Type, ev.OrderRef, ev.PermID, ev.clientIDPresent = orderJournalEventBrokerError, "", 0, false
				return ev
			},
			[]rpc.OrderView{row("127.0.0.1:7497", "ord-prot", 1002, 790001)}, ""},
		{"a row of another client on another socket", nil,
			[]rpc.OrderView{func() rpc.OrderView { v := row("127.0.0.1:7497", "ord-prot", 1002, 790001); v.ClientID = 0; return v }()}, ""},
	} {
		ev := callback
		if tc.ev != nil {
			ev = tc.ev(ev)
		}
		view, ok := orderJournalViewForLifecycleEvent(ev, tc.views)
		switch {
		case tc.endpoint == "" && ok:
			t.Errorf("%s: paired with the row on %s, want no pairing", tc.name, view.Endpoint)
		case tc.endpoint != "" && (!ok || view.Endpoint != tc.endpoint):
			t.Errorf("%s: paired=%t endpoint %s, want the row on %s", tc.name, ok, view.Endpoint, tc.endpoint)
		}
	}
}

// Pairing reads the whole scope (client ID, account, mode) before it accepts
// any row, on the callback's own socket as across sockets (the refuter's
// review of fix-stop-lifecycle, 2026-10-20): a never-acknowledged row is not
// paired by order ID alone when the callback names another reference, when
// another row of the scope already holds the callback's permanent ID, or when
// the row is terminal. Otherwise a stale row of order 40 on IB Gateway adopts
// the permanent ID of another order 40 placed through TWS, and a later change
// of the stale row would be sent onto that order.
func TestLifecyclePairingRespectsTheWholeScope(t *testing.T) {
	t.Parallel()
	row := func(endpoint, ref string, orderID, perm int, open bool) rpc.OrderView {
		return rpc.OrderView{Endpoint: endpoint, ClientID: 31, Account: "DU1234567", Mode: "paper", OrderRef: ref, ReservedOrderID: orderID, PermID: perm, Open: open}
	}
	stale := row("127.0.0.1:4002", "ord-stale", 40, 0, true)
	other := row("127.0.0.1:7497", "ord-other", 40, 799999, true)
	open := orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, Endpoint: "127.0.0.1:4002", ClientID: 31, clientIDPresent: true,
		Account: "DU1234567", Mode: "paper", OrderRef: "ord-other", ReservedOrderID: 40, PermID: 799999}
	status := open
	status.Type, status.OrderRef = orderJournalEventStatusUpdated, ""
	placed := row("127.0.0.1:7497", "ord-prot", 1002, 790001, true)
	placed.PreviewTokenID = "tok-prot"
	phantom := row("127.0.0.1:4002", "ord-prot", 1002, 790001, false)
	stopAck := open
	stopAck.OrderRef, stopAck.ReservedOrderID, stopAck.PermID = "ord-prot", 1002, 790001
	for _, tc := range []struct {
		name     string
		ev       orderJournalEvent
		views    []rpc.OrderView
		wantRef  string // the paired row's reference, "" for no pairing
		wantSock string
	}{
		{"openOrder naming another reference", open, []rpc.OrderView{stale, other}, "ord-other", "127.0.0.1:7497"},
		{"orderStatus whose permanent ID another row holds", status, []rpc.OrderView{stale, other}, "ord-other", "127.0.0.1:7497"},
		{"openOrder naming another reference, no other row", open, []rpc.OrderView{stale}, "", ""},
		{"orderStatus for a terminal never-acknowledged row", status, []rpc.OrderView{row("127.0.0.1:4002", "ord-stale", 40, 0, false)}, "", ""},
		{"orderStatus, a second never-acknowledged order 40 on another socket", status,
			[]rpc.OrderView{stale, row("127.0.0.1:7497", "ord-other", 40, 0, true)}, "", ""},
		{"orderStatus never introduces a permanent ID", status, []rpc.OrderView{stale}, "", ""},
		{"openOrder naming the one never-acknowledged row (control)", func() orderJournalEvent { ev := open; ev.OrderRef = "ord-stale"; return ev }(), []rpc.OrderView{stale}, "ord-stale", "127.0.0.1:4002"},
		{"a callback-born duplicate row loses to the placed row", stopAck, []rpc.OrderView{phantom, placed}, "ord-prot", "127.0.0.1:7497"},
		{"two placed rows holding one permanent ID", stopAck, []rpc.OrderView{func() rpc.OrderView { v := phantom; v.PreviewTokenID = "tok-x"; return v }(), placed}, "", ""},
	} {
		view, ok := orderJournalViewForLifecycleEvent(tc.ev, tc.views)
		switch {
		case tc.wantRef == "" && ok:
			t.Errorf("%s: paired with %s on %s, want no pairing", tc.name, view.OrderRef, view.Endpoint)
		case tc.wantRef != "" && (!ok || view.OrderRef != tc.wantRef || view.Endpoint != tc.wantSock):
			t.Errorf("%s: paired=%t with %s on %s, want %s on %s", tc.name, ok, view.OrderRef, view.Endpoint, tc.wantRef, tc.wantSock)
		}
	}
}

// Only an openOrder naming the row's reference introduces a permanent ID to
// a row that has none (the refuter's second round): an orderStatus carries no
// reference, so a foreign order 1002 of the daemon's client ID that is not in
// the journal would otherwise hand its permanent ID to Canary's
// never-acknowledged order 1002 by order ID alone, and its later fills and
// cancels would land there too.
func TestOnlyAnOpenOrderNamingTheReferenceIntroducesAPermanentID(t *testing.T) {
	t.Parallel()
	row := rpc.OrderView{Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU1234567", Mode: "paper", OrderRef: "ord-prot", ReservedOrderID: 1002, Open: true}
	base := orderJournalEvent{Endpoint: "127.0.0.1:4002", ClientID: 31, clientIDPresent: true, Account: "DU1234567", Mode: "paper", ReservedOrderID: 1002, PermID: 799999}
	for _, tc := range []struct {
		name string
		typ  string
		ref  string
		want bool
	}{
		{"orderStatus without a reference", orderJournalEventStatusUpdated, "", false},
		{"execution without a reference", orderJournalEventStatusUpdated, "", false},
		{"openOrder without a reference", orderJournalEventBrokerAcknowledged, "", false},
		{"openOrder under another reference", orderJournalEventBrokerAcknowledged, "ord-other", false},
		{"openOrder naming the reference", orderJournalEventBrokerAcknowledged, "ord-prot", true},
	} {
		ev := base
		ev.Type, ev.OrderRef = tc.typ, tc.ref
		if _, ok := orderJournalViewForLifecycleEvent(ev, []rpc.OrderView{row}); ok != tc.want {
			t.Errorf("%s: paired=%t, want %t", tc.name, ok, tc.want)
		}
	}
}
