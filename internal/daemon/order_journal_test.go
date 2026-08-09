package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func newTestOrderJournalStore(t *testing.T, path string) *orderJournalStore {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open test order authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	journal := newOrderJournalStore(path)
	if err := journal.UseCoreStore(store); err != nil {
		t.Fatalf("attach test order authority: %v", err)
	}
	return journal
}

func TestOrderJournalDefaultPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/ibkr-state")

	got, err := defaultOrderJournalPath()
	if err != nil {
		t.Fatalf("defaultOrderJournalPath: %v", err)
	}
	want := filepath.Join("/tmp/ibkr-state", "ibkr", "order-journal.jsonl")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestOrderJournalAppendWritesPrivateSQLiteAuthority(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "order-journal.jsonl")
	store := newTestOrderJournalStore(t, path)
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	if err := store.Append(orderJournalEvent{
		At:              now,
		Type:            orderJournalEventSendAttempted,
		OrderRef:        "ibkr-20260528-test",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "AAPL",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       "LMT",
		TIF:             "DAY",
		Quantity:        1,
		LimitPrice:      100,
		SendState:       orderSendStateSendAttempted,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	info, err := os.Stat(filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db"))
	if err != nil {
		t.Fatalf("stat authority database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy JSONL path was written: %v", err)
	}

	events, err := store.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].OrderRef != "ibkr-20260528-test" {
		t.Fatalf("events = %+v", events)
	}
}

func TestOrderJournalPersistsBrokerErrorCode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	store := newTestOrderJournalStore(t, path)
	if err := store.Append(orderJournalEvent{
		At: time.Date(2026, 7, 20, 9, 10, 0, 0, time.UTC), Type: orderJournalEventBrokerError,
		OrderRef: "typed-error", ReservedOrderID: 1001,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
		ErrorCode: 201, Status: "Rejected", SendState: orderSendStateTerminal,
		Message: "audit text",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, err := store.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].ErrorCode != 201 || events[0].Message != "audit text" {
		t.Fatalf("events = %+v, want durable typed code 201 and separate audit text", events)
	}
}

func TestOrderJournalSummaryCountsNonTerminalLatestState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	store := newTestOrderJournalStore(t, path)
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	events := []orderJournalEvent{
		{At: now, Type: orderJournalEventSendAttempted, OrderRef: "open", Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", SendState: orderSendStateSendAttempted},
		{At: now.Add(time.Minute), Type: orderJournalEventStatusUpdated, OrderRef: "closed", Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", SendState: orderSendStateSendAttempted},
		{At: now.Add(2 * time.Minute), Type: orderJournalEventStatusUpdated, OrderRef: "closed", Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", SendState: orderSendStateTerminal},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	summary, err := store.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.OpenOrders != 1 {
		t.Fatalf("OpenOrders = %d, want 1", summary.OpenOrders)
	}
	if !strings.Contains(summary.LastEvent, "closed") {
		t.Fatalf("LastEvent = %q, want closed order ref", summary.LastEvent)
	}
}

func TestMaxReservedBrokerOrderID(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	store := newTestOrderJournalStore(t, path)
	now := time.Date(2026, 6, 8, 8, 55, 0, 0, time.UTC)
	for _, id := range []int{10, 15, 12} {
		if err := store.Append(orderJournalEvent{
			At:              now,
			Type:            orderJournalEventSendAttempted,
			OrderRef:        "ord-" + strconv.Itoa(id),
			ReservedOrderID: id,
			Endpoint:        "127.0.0.1:4002",
			ClientID:        31,
			Account:         "DU123",
			Mode:            "paper",
			SendState:       orderSendStateSendAttempted,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := maxReservedBrokerOrderID(store)
	if err != nil {
		t.Fatalf("maxReservedBrokerOrderID: %v", err)
	}
	if got != 15 {
		t.Fatalf("max reserved id = %d, want 15", got)
	}
}

// prove a valid unterminated final row is counted

func TestInitializeFreshTradingAuthorityNeverReadsAdjacentLegacyFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyOrderPath := filepath.Join(dir, "order-journal.jsonl")
	legacyPurgePath := filepath.Join(dir, "purge-ledger.json")
	if err := os.WriteFile(legacyOrderPath, []byte("malformed legacy order data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPurgePath, []byte("malformed legacy purge data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	authority, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	defer authority.Close()
	if err := initializeFreshTradingAuthority(t.Context(), authority); err != nil {
		t.Fatalf("initialize fresh trading authority: %v", err)
	}
	events, err := authority.LoadOrderEvents(t.Context(), corestore.OrderQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("fresh authority imported adjacent order events: %+v", events)
	}
	if err := initializeFreshTradingAuthority(t.Context(), authority); !errors.Is(err, corestore.ErrFreshAuthorityConflict) {
		t.Fatalf("second initialization error = %v, want fresh-authority conflict", err)
	}
}

func TestPreviewTokenRedemptionHasOneWinnerAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "authority", "daemon.db")
	authority, err := corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	journal := newOrderJournalStore(filepath.Join(t.TempDir(), "legacy.jsonl"))
	if err := journal.UseCoreStore(authority); err != nil {
		t.Fatalf("attach authority: %v", err)
	}
	head, err := authority.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("authority head: %v", err)
	}
	const contenders = 24
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- journal.StagePreTransmit("one-token", head.AuthorityEpoch, head.SignerGeneration, 1001, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{{
				At: time.Date(2026, 7, 20, 11, 0, 0, i, time.UTC), Type: orderJournalEventSendAttempted,
				OrderRef: "one-winner", PreviewTokenID: "one-token", ReservedOrderID: 1001,
				Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
			}})
		}(i)
	}
	wg.Wait()
	close(results)
	winners, consumed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errOrderPreviewTokenAlreadyUsed):
			consumed++
		default:
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if winners != 1 || consumed != contenders-1 {
		t.Fatalf("winners=%d consumed=%d, want 1/%d", winners, consumed, contenders-1)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}
	authority, err = corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer authority.Close()
	reopened := newOrderJournalStore("")
	if err := reopened.UseCoreStore(authority); err != nil {
		t.Fatalf("attach reopened authority: %v", err)
	}
	head, err = authority.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("reopened head: %v", err)
	}
	err = reopened.StagePreTransmit("one-token", head.AuthorityEpoch, head.SignerGeneration, 1002, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{{
		At: time.Date(2026, 7, 20, 11, 1, 0, 0, time.UTC), Type: orderJournalEventSendAttempted,
		OrderRef: "restart-loser", PreviewTokenID: "one-token", ReservedOrderID: 1002,
		Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "DU999", Mode: "paper",
	}})
	if !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("restarted redemption err = %v, want consumed", err)
	}
}

func TestOrderFoldIsolatesCompleteBrokerRoute(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC)
	events := []orderJournalEvent{
		{At: now, Type: orderJournalEventSendAttempted, OrderRef: "same-ref", ReservedOrderID: 77, Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", Symbol: "AAPL", SendState: orderSendStateSendAttempted},
		{At: now.Add(time.Second), Type: orderJournalEventSendAttempted, OrderRef: "same-ref", ReservedOrderID: 77, Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "DU123", Mode: "paper", Symbol: "MSFT", SendState: orderSendStateSendAttempted},
	}
	views := buildOrderViews(events)
	if len(views) != 2 {
		t.Fatalf("views = %+v, want two route-isolated orders", views)
	}
	byKey := buildOrderEventsByKey(events)
	for _, view := range views {
		if len(byKey[orderViewKey(view)]) != 1 {
			t.Fatalf("events for route %+v = %+v, want one", view, byKey[orderViewKey(view)])
		}
	}
	matched, ok := orderJournalViewForLifecycleEvent(orderJournalEvent{
		ReservedOrderID: 77, Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "DU123", Mode: "paper",
		clientIDPresent: true,
	}, views)
	if !ok || matched.Symbol != "MSFT" {
		t.Fatalf("route-aware lifecycle match = %+v ok=%v, want MSFT route", matched, ok)
	}
}

func TestLifecycleMatchKnownPermIDOutranksCollidingOrderRef(t *testing.T) {
	t.Parallel()
	views := []rpc.OrderView{{
		OrderRef: "reused-ref", ReservedOrderID: 77, PermID: 555,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
	}}
	event := orderJournalEvent{
		OrderRef: "reused-ref", ReservedOrderID: 77, PermID: 999,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
		clientIDPresent: true,
	}
	if matched, ok := orderJournalViewForLifecycleEvent(event, views); ok {
		t.Fatalf("known conflicting PermID matched a reused order ref: %+v", matched)
	}
}

func TestLifecycleMatchCanAttachFirstPermIDToExactLocalReference(t *testing.T) {
	t.Parallel()
	views := []rpc.OrderView{{
		OrderRef: "new-ref", ReservedOrderID: 77,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
	}}
	event := orderJournalEvent{
		OrderRef: "new-ref", ReservedOrderID: 77, PermID: 999,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
		clientIDPresent: true,
	}
	matched, ok := orderJournalViewForLifecycleEvent(event, views)
	if !ok || matched.OrderRef != "new-ref" || matched.PermID != 0 {
		t.Fatalf("first broker PermID did not attach to the exact pre-transmit row: %+v ok=%v", matched, ok)
	}
}

func TestOrderFoldUsesAuthoritativeInsertionOrderNotBrokerTimestamp(t *testing.T) {
	t.Parallel()
	newerClock := time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC)
	base := orderJournalEvent{
		OrderRef: "clock-regression", ReservedOrderID: 77,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
	}
	first := base
	first.At, first.Type, first.Status, first.SendState = newerClock, orderJournalEventBrokerAcknowledged, "Submitted", orderSendStateBrokerAcknowledged
	second := base
	second.At, second.Type, second.Status, second.SendState = newerClock.Add(-time.Hour), orderJournalEventStatusUpdated, "Cancelled", orderSendStateTerminal
	views := buildOrderViews([]orderJournalEvent{first, second})
	if len(views) != 1 || views[0].LastEvent != orderJournalEventStatusUpdated || views[0].Status != "Cancelled" || views[0].Open {
		t.Fatalf("event-seq fold = %+v, want later-inserted cancellation", views)
	}
	if !views[0].UpdatedAt.Equal(second.At) {
		t.Fatalf("updated_at = %s, want last inserted event time %s", views[0].UpdatedAt, second.At)
	}
	events := buildOrderEventsByKey([]orderJournalEvent{first, second})[orderJournalKey(first)]
	if len(events) != 2 || events[0].Type != first.Type || events[1].Type != second.Type {
		t.Fatalf("event order = %+v, want authoritative insertion order", events)
	}
}
