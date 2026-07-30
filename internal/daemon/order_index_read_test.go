package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestOrderReadsUseSQLiteAndIgnoreSealedLegacyJournal(t *testing.T) {
	t.Parallel()
	legacyPath := filepath.Join(t.TempDir(), "order-journal.jsonl")
	journal := newTestOrderJournalStore(t, legacyPath)
	srv := &Server{orderJournal: journal}
	now := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	if err := journal.Append(orderJournalEvent{
		At: now, Type: orderJournalEventSendAttempted, OrderRef: "sqlite-only", ReservedOrderID: 101,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
		SendState: orderSendStateSendAttempted,
	}); err != nil {
		t.Fatalf("append authoritative event: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("this legacy file must never be read at runtime\n"), 0o600); err != nil {
		t.Fatalf("write sealed legacy decoy: %v", err)
	}

	events, ok := srv.indexedOrderEvents("orders.open", nil, nil)
	if !ok || len(events) != 1 || events[0].OrderRef != "sqlite-only" {
		t.Fatalf("authoritative read ok=%v events=%+v", ok, events)
	}
	loaded, err := srv.loadOrderJournalEventsForRead("orders.open")
	if err != nil || len(loaded) != 1 || loaded[0].OrderRef != "sqlite-only" {
		t.Fatalf("direct authoritative read events=%+v err=%v", loaded, err)
	}
}

func TestAuthoritativeOrderReadWindowPreservesEventSequence(t *testing.T) {
	t.Parallel()
	journal := newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	srv := &Server{orderJournal: journal}
	base := time.Date(2026, 7, 20, 13, 30, 0, 0, time.UTC)
	events := []orderJournalEvent{
		{At: base.Add(time.Second), Type: orderJournalEventSendAttempted, OrderRef: "first", Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper"},
		{At: base, Type: orderJournalEventStatusUpdated, OrderRef: "second", Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper"},
	}
	if err := journal.AppendAll(events); err != nil {
		t.Fatalf("append events: %v", err)
	}
	since, until := base.UnixMilli(), base.Add(2*time.Second).UnixMilli()
	got, ok := srv.indexedOrderEvents("orders.history", &since, &until)
	if !ok || len(got) != 2 || got[0].OrderRef != "first" || got[1].OrderRef != "second" {
		t.Fatalf("windowed event-seq read ok=%v events=%+v", ok, got)
	}
}

func TestReservedOrderIDFloorComesOnlyFromSQLiteAuthority(t *testing.T) {
	t.Parallel()
	legacyPath := filepath.Join(t.TempDir(), "order-journal.jsonl")
	journal := newTestOrderJournalStore(t, legacyPath)
	srv := &Server{orderJournal: journal}
	for _, ev := range []orderJournalEvent{
		{At: time.Now().UTC(), Type: orderJournalEventSendAttempted, OrderRef: "floor-1", ReservedOrderID: 311, Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper"},
		{At: time.Now().UTC(), Type: orderJournalEventSendAttempted, OrderRef: "floor-2", ReservedOrderID: 4200, Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "DU999", Mode: "paper"},
	} {
		if err := journal.StagePreTransmit("", "", 0, ev.ReservedOrderID, corestore.ActionPlace, corestore.OriginDaemon, []orderJournalEvent{ev}); err != nil {
			t.Fatalf("stage authoritative floor: %v", err)
		}
	}
	if err := os.WriteFile(legacyPath, []byte(`{"version":1,"type":"send-attempted","reserved_order_id":9999}`+"\n"), 0o600); err != nil {
		t.Fatalf("write legacy floor decoy: %v", err)
	}
	floor, err := srv.reservedBrokerOrderIDFloor()
	if err != nil || floor != 4200 {
		t.Fatalf("authoritative floor=%d err=%v, want 4200", floor, err)
	}
}

func TestUnattachedOrderAuthorityFailsClosedWithoutLegacyFallback(t *testing.T) {
	t.Parallel()
	legacyPath := filepath.Join(t.TempDir(), "order-journal.jsonl")
	if err := os.WriteFile(legacyPath, []byte(`{"version":1,"type":"previewed","order_ref":"legacy"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{orderJournal: newOrderJournalStore(legacyPath)}
	if events, ok := srv.indexedOrderEvents("orders.open", nil, nil); ok || events != nil {
		t.Fatalf("unattached indexed read ok=%v events=%+v", ok, events)
	}
	if _, err := srv.loadOrderJournalEventsForRead("orders.open"); err == nil {
		t.Fatal("unattached order read succeeded through legacy fallback")
	}
	if _, err := srv.reservedBrokerOrderIDFloor(); err == nil {
		t.Fatal("unattached order floor succeeded through legacy fallback")
	}
}

// A configured order journal that cannot be read defers idle exit and
// reports itself as a degraded status row; a daemon with no journal at all
// keeps its normal idle exit — no broker-write capability means no broker
// orders to go dark on.
func TestUnreadableOrderJournalDefersIdleExit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	dbPath := filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open order authority: %v", err)
	}
	journal := newOrderJournalStore(path)
	if err := journal.UseCoreStore(store); err != nil {
		t.Fatalf("attach order authority: %v", err)
	}
	_ = store.Close()

	srv := &Server{orderJournal: journal}
	if !srv.isBusy() {
		t.Fatal("unreadable order journal must defer idle exit")
	}
	found := false
	for _, task := range srv.backgroundTasks() {
		if task.Name == "open-orders" && strings.Contains(task.Status, "journal unreadable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("degraded open-orders row missing: %+v", srv.backgroundTasks())
	}

	bare := &Server{}
	if bare.isBusy() {
		t.Fatal("a daemon with no order journal must keep its normal idle exit")
	}
}
