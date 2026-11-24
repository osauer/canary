package daemon

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"runtime"
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

// syntheticOrderJournal is n events over n/10 working sell orders: a send
// attempt and nine status updates each.
func syntheticOrderJournal(n int) []orderJournalEvent {
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	events := make([]orderJournalEvent, 0, n)
	for i := range n {
		events = append(events, syntheticOrderJournalEvent(base, i))
	}
	return events
}

func syntheticOrderJournalEvent(base time.Time, i int) orderJournalEvent {
	order := i / 10
	ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: fmt.Sprintf("ref%05d", order),
		ReservedOrderID: 1000 + order, ClientID: 15, Account: "U1234567", Endpoint: "127.0.0.1:4001", Mode: "live", Symbol: "SYNA", SecType: "STK", ConID: 900001,
		Action: "SELL", OrderType: "LMT", Quantity: 10, Status: "Submitted", PermID: 500000 + order}
	if i%10 == 0 {
		ev.Type = orderJournalEventSendAttempted
	}
	return ev
}

// fastestOf is the shortest of reps timings of f, after a collection, so a
// busy machine slows a measurement down without changing the comparison.
func fastestOf(reps int, f func()) time.Duration {
	best := time.Duration(math.MaxInt64)
	for range reps {
		runtime.GC()
		start := time.Now()
		f()
		best = min(best, time.Since(start))
	}
	return best
}

// The alias pass and the projection build are linear in the journal. The
// build is timed at two sizes in the same process, so the comparison holds
// on any machine: four times the events cost about four times as much, and
// at most eight. A pass quadratic in the journal costs sixteen times as much
// (the old alias pass, kept above as the reference, took seconds at 50,000).
func TestOrderJournalProjectionBuildIsLinear(t *testing.T) {
	const small, large = 12500, 50000
	build := func(events []orderJournalEvent) func() {
		return func() {
			if views := buildOrderViews(events); len(views) != len(events)/10 {
				t.Fatalf("%d views from %d events", len(views), len(events))
			}
			_ = buildOrderEventsByKey(events)
		}
	}
	smallEvents, largeEvents := syntheticOrderJournal(small), syntheticOrderJournal(large)
	tSmall := fastestOf(3, build(smallEvents))
	tLarge := fastestOf(3, build(largeEvents))
	ratio := float64(tLarge) / float64(tSmall)
	if ratio > 8 {
		t.Fatalf("four times the journal cost %.1f times as much to build (%s at %d events, %s at %d): not linear", ratio, tSmall, small, tLarge, large)
	}
	t.Logf("build: %s at %d events, %s at %d (x%.1f)", tSmall, small, tLarge, large, ratio)
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

// An appended status folds into its one order: it never folds the whole
// journal again. Timed in the same process, an append at 50,000 events costs
// a small fraction of a whole fold of them (about a hundredth measured;
// failing at a tenth), so an append that refolds the journal fails on any
// machine and a slow one passes.
func TestOrderReadStateAppendIsFast(t *testing.T) {
	const n = 50000
	base := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	events := syntheticOrderJournal(n)
	var state *orderReadState
	whole := fastestOf(3, func() { state = newOrderReadState(events) })
	const appends = 32
	perAppend := fastestOf(3, func() {
		st := state
		for i := range appends {
			next, ok := st.apply([]orderJournalEvent{syntheticOrderJournalEvent(base, n-10+i%10)})
			if !ok {
				t.Fatal("an acknowledged order's status did not fold in place")
			}
			st = next
		}
	}) / appends
	if perAppend*10 > whole {
		t.Fatalf("an append took %s against %s for a whole fold of %d events: it is not folding in place", perAppend, whole, n)
	}
	t.Logf("an append took %s, a whole fold %s, at %d events", perAppend, whole, n)
}
