package daemon

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// orderJournalKeyAliasesQuadratic is the alias pass as it was before it was
// made linear, kept as the reference the linear pass must equal.
func orderJournalKeyAliasesQuadratic(events []orderJournalEvent) map[string]string {
	aliases := map[string]string{}
	ambiguousOrderIDs := ambiguousReservedOrderIDs(events)
	prelinkedOrderIDs := reservedOrderIDsWithPrelinkedBrokerOnlyEvents(events)
	for _, ev := range events {
		keys := orderJournalIdentityKeysForAliases(ev, ambiguousOrderIDs, prelinkedOrderIDs)
		if len(keys) == 0 {
			continue
		}

		canonical := ""
		for _, key := range keys {
			if resolved := resolveOrderJournalAlias(key, aliases); resolved != "" {
				canonical = resolved
				break
			}
		}
		if ev.OrderRef != "" {
			canonical = orderJournalRouteIdentity(ev.Endpoint, ev.ClientID, ev.Account, ev.Mode) + "|ref:" + ev.OrderRef
		}
		if canonical == "" {
			canonical = keys[0]
		}
		for _, key := range keys {
			aliases[key] = canonical
		}
		for key, resolved := range aliases {
			if resolved == canonical {
				continue
			}
			if slices.Contains(keys, resolved) {
				aliases[key] = canonical
			}
		}
	}
	for key := range aliases {
		aliases[key] = resolveOrderJournalAlias(key, aliases)
	}
	return aliases
}

// The linear alias pass equals the pass it replaced on journals that mix
// references, reserved order ids, permanent ids, clients and routes, reused
// ids and events that link keys late.
func TestOrderJournalKeyAliasesMatchesTheReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	for round := range 300 {
		var events []orderJournalEvent
		for i := range 2 + r.IntN(60) {
			ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, Account: "U1234567",
				Endpoint: []string{"127.0.0.1:4001", "127.0.0.1:7496"}[r.IntN(2)], ClientID: 15 + r.IntN(2), Mode: "live"}
			if r.IntN(3) > 0 {
				ev.OrderRef = fmt.Sprintf("ref%d", r.IntN(8))
			}
			if r.IntN(3) > 0 {
				ev.ReservedOrderID = 100 + r.IntN(8)
			}
			if r.IntN(2) == 0 {
				ev.PermID = 5000 + r.IntN(8)
			}
			if r.IntN(4) == 0 {
				ev.Type = orderJournalEventSendAttempted
			}
			events = append(events, ev)
		}
		want, got := orderJournalKeyAliasesQuadratic(events), orderJournalKeyAliases(events)
		if !maps.Equal(want, got) {
			t.Fatalf("round %d: aliases differ\nwant %v\n got %v", round, want, got)
		}
	}
}

// The alias pass and the projection build are linear in the journal: at
// 50,000 events a rebuild costs a fraction of a second, not seconds.
func TestOrderJournalProjectionBuildIsLinear(t *testing.T) {
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	events := make([]orderJournalEvent, 0, 50000)
	for i := range 50000 {
		order := i / 10
		ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: fmt.Sprintf("ref%05d", order),
			ReservedOrderID: 1000 + order, ClientID: 15, Account: "U1234567", Endpoint: "127.0.0.1:4001", Mode: "live", Symbol: "SYNA", SecType: "STK", ConID: 900001,
			Action: "SELL", OrderType: "LMT", Quantity: 10, Status: "Submitted", PermID: 500000 + order}
		if i%10 == 0 {
			ev.Type = orderJournalEventSendAttempted
		}
		events = append(events, ev)
	}
	start := time.Now()
	views := buildOrderViews(events)
	_ = buildOrderEventsByKey(events)
	if took := time.Since(start); took > 1500*time.Millisecond || len(views) != 5000 {
		t.Fatalf("a 50,000-event rebuild took %s (%d views)", took, len(views))
	} else {
		t.Logf("a 50,000-event rebuild took %s", took)
	}
}

// The read cache decodes only what was appended since its last read, and the
// projection it builds equals one built from the whole journal.
func TestOrderReadCacheDecodesOnlyNewEvents(t *testing.T) {
	srv := newTestServer(t)
	srv.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	add := func(from, to int) {
		var batch []orderJournalEvent
		for i := from; i < to; i++ {
			order := i / 4
			ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: fmt.Sprintf("ref%04d", order),
				ReservedOrderID: 1000 + order, ClientID: 15, Account: "U1234567", Endpoint: "127.0.0.1:4001", Mode: "live", Symbol: "SYNA", SecType: "STK", ConID: 900001,
				Action: "SELL", OrderType: "LMT", Quantity: 10, Status: "Submitted"}
			if i%4 == 0 {
				ev.Type = orderJournalEventSendAttempted
			}
			if i%4 == 3 {
				ev.PermID = 70000 + order
			}
			batch = append(batch, ev)
		}
		if err := srv.orderJournal.AppendAll(batch); err != nil {
			t.Fatal(err)
		}
	}
	add(0, 400)
	if _, _, err := srv.loadOrderViews(); err != nil {
		t.Fatal(err)
	}
	first := srv.orderReads.eventsSeq
	add(400, 410)
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatal(err)
	}
	if srv.orderReads.eventsSeq <= first || len(srv.orderReads.events) != 410 {
		t.Fatalf("cache holds %d events through %d (was %d)", len(srv.orderReads.events), srv.orderReads.eventsSeq, first)
	}
	all, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	want := buildOrderViews(all)
	key := func(v []rpc.OrderView) map[string]rpc.OrderView {
		out := map[string]rpc.OrderView{}
		for _, x := range v {
			out[orderViewKey(x)] = x
		}
		return out
	}
	got, exp := key(views), key(inferredAt(want, srv))
	if len(got) != len(exp) {
		t.Fatalf("%d views, want %d", len(got), len(exp))
	}
	for k, v := range exp {
		if g, ok := got[k]; !ok || !reflect.DeepEqual(g, v) {
			t.Fatalf("view %s differs:\n got %+v\nwant %+v", k, g, v)
		}
	}
}

// inferredAt applies what viewsAt adds to a freshly built fold.
func inferredAt(views []rpc.OrderView, srv *Server) []rpc.OrderView {
	p := &orderReadProjection{views: views}
	all, _ := srv.orderJournal.LoadEvents(0)
	p.eventsByKey = buildOrderEventsByKey(all)
	return p.viewsAt(srv.orderNow())
}

// Folding appended events into the kept state gives exactly what folding the
// whole journal gives, whether an append folds in place or falls back, over
// journals with new orders, first acknowledgements, fills, cancels, send
// attempts, reused order ids and late links.
func TestOrderReadStateAppendEqualsAWholeFold(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	statuses := []string{"Submitted", "PreSubmitted", "Filled", "Cancelled", "Inactive", ""}
	types := []string{orderJournalEventStatusUpdated, orderJournalEventBrokerAcknowledged, orderJournalEventSendAttempted, orderJournalEventSendCompleted, orderJournalEventBrokerError}
	inPlace := 0
	for round := range 200 {
		var all []orderJournalEvent
		state := newOrderReadState(nil)
		for i := range 5 + r.IntN(80) {
			order := r.IntN(10)
			ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: types[r.IntN(len(types))], Account: "U1234567", Endpoint: "127.0.0.1:4001",
				ClientID: 15, Mode: "live", Symbol: "SYNA", SecType: "OPT", ConID: 910000 + order, Action: "SELL", OrderType: "LMT", Quantity: 20,
				Status: statuses[r.IntN(len(statuses))]}
			if r.IntN(5) > 0 {
				ev.OrderRef = fmt.Sprintf("ref%d", order)
			}
			if r.IntN(4) > 0 {
				ev.ReservedOrderID = 100 + order + r.IntN(2)*r.IntN(2)
			}
			if r.IntN(3) == 0 {
				ev.PermID = 5000 + order
			}
			if ev.Status == "Filled" || r.IntN(4) == 0 {
				ev.Filled = float64(r.IntN(21))
			}
			if r.IntN(3) == 0 {
				ev.BrokerParentID = new(0)
			}
			if ev.Type == orderJournalEventSendAttempted || ev.Type == orderJournalEventSendCompleted {
				ev.AttemptID, ev.ActionKind = fmt.Sprintf("a%d", order), "place"
			}
			all = append(all, ev)
			if next, ok := state.apply([]orderJournalEvent{ev}); ok {
				state = next
				inPlace++
			} else {
				state = newOrderReadState(all)
			}
		}
		want := newOrderReadState(all)
		byKey := func(v []rpc.OrderView) map[string]rpc.OrderView {
			out := map[string]rpc.OrderView{}
			for _, x := range v {
				out[orderViewKey(x)] = x
			}
			return out
		}
		got, exp := byKey(state.views), byKey(want.views)
		if !reflect.DeepEqual(got, exp) {
			for k, v := range exp {
				if !reflect.DeepEqual(got[k], v) {
					t.Fatalf("round %d: view %s\n got %+v\nwant %+v", round, k, got[k], v)
				}
			}
			t.Fatalf("round %d: %d views, want %d", round, len(got), len(exp))
		}
		if !reflect.DeepEqual(state.eventsBy, want.eventsBy) {
			t.Fatalf("round %d: event history differs", round)
		}
		if !reflect.DeepEqual(buildOrderViewsByKey(buildOrderViews(all)), exp) {
			t.Fatalf("round %d: the kept fold differs from buildOrderViews", round)
		}
	}
	if inPlace == 0 {
		t.Fatal("no append folded in place")
	}
	t.Logf("%d appends folded in place", inPlace)
}

func buildOrderViewsByKey(v []rpc.OrderView) map[string]rpc.OrderView {
	out := map[string]rpc.OrderView{}
	for _, x := range v {
		out[orderViewKey(x)] = x
	}
	return out
}

// At 50,000 events an appended status folds in place in milliseconds.
func TestOrderReadStateAppendIsFast(t *testing.T) {
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	events := make([]orderJournalEvent, 0, 50000)
	mk := func(i int) orderJournalEvent {
		order := i / 10
		ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: fmt.Sprintf("ref%05d", order),
			ReservedOrderID: 1000 + order, ClientID: 15, Account: "U1234567", Endpoint: "127.0.0.1:4001", Mode: "live", Symbol: "SYNA", SecType: "STK", ConID: 900001,
			Action: "SELL", OrderType: "LMT", Quantity: 10, Status: "Submitted", PermID: 500000 + order}
		if i%10 == 0 {
			ev.Type = orderJournalEventSendAttempted
		}
		return ev
	}
	for i := range 50000 {
		events = append(events, mk(i))
	}
	state := newOrderReadState(events)
	start := time.Now()
	for i := range 32 {
		next, ok := state.apply([]orderJournalEvent{mk(49990 + i%10)})
		if !ok {
			t.Fatal("an acknowledged order's status did not fold in place")
		}
		state = next
	}
	if per := time.Since(start) / 32; per > 25*time.Millisecond {
		t.Fatalf("an append took %s", per)
	} else {
		t.Logf("an append took %s at 50,000 events", per)
	}
}
