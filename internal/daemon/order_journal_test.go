package daemon

import (
	"context"
	"encoding/json"
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

func newTestPurgeLedgerStore(t *testing.T, path string, now func() time.Time) *purgeLedgerStore {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open test purge authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ledger := newPurgeLedgerStore(path, now)
	if err := ledger.UseCoreStore(store); err != nil {
		t.Fatalf("attach test purge authority: %v", err)
	}
	return ledger
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

func TestLegacyOrderImportSelectionRetainsSafetyStateAndGlobalFloor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	routeA := legacyOrderRoute{Endpoint: "127.0.0.1:4002", ClientID: 31, ClientIDKnown: true, Account: "DU123", Mode: "paper"}
	routeB := legacyOrderRoute{Endpoint: "127.0.0.1:7497", ClientID: 44, ClientIDKnown: true, Account: "DU456", Mode: "paper"}
	event := func(route legacyOrderRoute, ref, token, typ string, id int, at time.Time) orderJournalEvent {
		return orderJournalEvent{
			Version: orderJournalFileVersion, At: at, Type: typ,
			OrderRef: ref, PreviewTokenID: token, ReservedOrderID: id,
			Endpoint: route.Endpoint, ClientID: route.ClientID, Account: route.Account, Mode: route.Mode,
			Symbol: "AAPL", SecType: "STK", Action: "BUY", Quantity: 1,
		}
	}
	events := []orderJournalEvent{
		event(routeA, "terminal", "terminal-token", orderJournalEventSendAttempted, 900, now),
		event(routeA, "terminal", "terminal-token", orderJournalEventStatusUpdated, 900, now.Add(time.Second)),
		event(routeA, "active", "active-token", orderJournalEventSendAttempted, 100, now.Add(2*time.Second)),
		event(routeB, "reconciled", "reconciled-token", orderJournalEventSendAttempted, 100, now.Add(3*time.Second)),
		event(routeB, "reconciled", "reconciled-token", orderJournalEventReconciledAbsent, 100, now.Add(4*time.Second)),
	}
	events[0].SendState = orderSendStateSendAttempted
	events[1].Status = "Filled"
	events[1].Filled = 1
	events[1].SendState = orderSendStateTerminal
	events[2].SendState = orderSendStateUncertainSend
	events[3].SendState = orderSendStateBrokerAcknowledged
	events[4].SendState = orderSendStateTerminal

	var raw []byte
	for i, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		raw = append(raw, line...)
		if i != len(events)-1 { // prove a valid unterminated final row is counted
			raw = append(raw, '\n')
		}
	}
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy journal: %v", err)
	}

	got, err := loadLegacyOrderImportSelection(path)
	if err != nil {
		t.Fatalf("loadLegacyOrderImportSelection: %v", err)
	}
	if got.GlobalOrderIDFloor != 900 {
		t.Fatalf("global floor = %d, want omitted terminal id 900", got.GlobalOrderIDFloor)
	}
	if floor := got.ScopedOrderIDFloors[routeA.key()].Floor; floor != 900 {
		t.Fatalf("route A floor = %d, want 900", floor)
	}
	if floor := got.ScopedOrderIDFloors[routeB.key()].Floor; floor != 100 {
		t.Fatalf("route B floor = %d, want 100", floor)
	}
	if len(got.ConsumedTokens) != 3 {
		t.Fatalf("consumed tokens = %+v, want all three global tombstones", got.ConsumedTokens)
	}
	if got.ReconciliationEvents != 1 {
		t.Fatalf("reconciliation events = %d, want 1", got.ReconciliationEvents)
	}
	if len(got.Events) != 3 || got.Events[0].OrderRef != "active" || got.Events[1].OrderRef != "reconciled" || got.Events[2].Type != orderJournalEventReconciledAbsent {
		t.Fatalf("retained events = %+v, want active and complete reconciled chains only", got.Events)
	}
}

func TestLegacyOrderRouteDistinguishesExplicitZeroClientID(t *testing.T) {
	t.Parallel()

	explicitZero, err := decodeOrderJournalLine([]byte(`{"version":1,"type":"send-attempted","endpoint":"127.0.0.1:4002","client_id":0,"account":"DU123","mode":"paper"}`))
	if err != nil {
		t.Fatalf("decode explicit zero: %v", err)
	}
	explicitRoute := legacyOrderRouteFromEvent(explicitZero)
	if explicitRoute.ClientID != 0 || !explicitRoute.ClientIDKnown || !explicitRoute.complete() {
		t.Fatalf("explicit client_id=0 route = %+v, want complete zero-client route", explicitRoute)
	}

	omitted, err := decodeOrderJournalLine([]byte(`{"version":1,"type":"send-attempted","endpoint":"127.0.0.1:4002","account":"DU123","mode":"paper"}`))
	if err != nil {
		t.Fatalf("decode omitted client: %v", err)
	}
	omittedRoute := legacyOrderRouteFromEvent(omitted)
	if omittedRoute.ClientIDKnown || omittedRoute.complete() {
		t.Fatalf("omitted client_id route = %+v, want incomplete route", omittedRoute)
	}
	if legacyOrderRoutePartitionKey(explicitZero) == legacyOrderRoutePartitionKey(omitted) {
		t.Fatal("explicit client_id=0 and omitted client_id share a legacy partition")
	}
	for _, raw := range []string{
		`{"version":1,"type":"send-attempted","client_id":null}`,
		`{"version":1,"type":"send-attempted","client_id":"0"}`,
	} {
		if _, err := decodeOrderJournalLine([]byte(raw)); err == nil {
			t.Fatalf("decodeOrderJournalLine(%s) succeeded, want invalid client_id", raw)
		}
	}
}

func TestLegacyOrderImportFailsClosedForIncompleteSafetyRoutes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 9, 15, 0, 0, time.UTC)
	configured := corestore.BrokerScope{Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper"}
	tests := []struct {
		name    string
		event   orderJournalEvent
		wantErr string
	}{
		{
			name: "active chain",
			event: orderJournalEvent{
				Version: 1, At: now, Type: orderJournalEventSendAttempted,
				OrderRef: "missing-client-active", ReservedOrderID: 701,
				Endpoint: configured.Endpoint, Account: configured.Account, Mode: configured.Mode,
				SendState: orderSendStateUncertainSend,
			},
			wantErr: "retained legacy order event",
		},
		{
			name: "consumed token",
			event: orderJournalEvent{
				Version: 1, At: now, Type: orderJournalEventTokenConfirmed,
				OrderRef: "missing-client-token", PreviewTokenID: "consumed-token", ReservedOrderID: 702,
				Endpoint: configured.Endpoint, Account: configured.Account, Mode: configured.Mode,
			},
			wantErr: "consumed preview token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "order-journal.jsonl")
			raw, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = importLegacyOrderAuthority(t.Context(), nil, path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("import error = %v, want %q; configured scope must not rebind the route", err, tt.wantErr)
			}
		})
	}
}

func TestLegacyOrderImportMayOmitIncompleteTerminalRowButKeepsGlobalFloor(t *testing.T) {
	t.Parallel()
	ev := orderJournalEvent{
		Version: 1, At: time.Date(2026, 7, 20, 9, 20, 0, 0, time.UTC),
		Type: orderJournalEventStatusUpdated, OrderRef: "old-terminal", ReservedOrderID: 903,
		Endpoint: "127.0.0.1:4002", Account: "DU123", Mode: "paper",
		Status: "Filled", Filled: 1, SendState: orderSendStateTerminal,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := loadLegacyOrderImportSelection(path)
	if err != nil {
		t.Fatalf("load selection: %v", err)
	}
	if len(selection.Events) != 0 || selection.GlobalOrderIDFloor != 903 || len(selection.ScopedOrderIDFloors) != 0 {
		t.Fatalf("selection = %+v, want omitted terminal row and conservative global floor 903", selection)
	}
}

func TestLegacyOrderImportRetainsUntypedOrUnknownBrokerErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		errorCode int
		status    string
		sendState string
	}{
		{name: "code-less legacy reject", status: "Rejected", sendState: orderSendStateTerminal},
		{name: "unknown typed error", errorCode: 399, sendState: orderSendStateUncertainSend},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 7, 20, 9, 25, 0, 0, time.UTC)
			events := []orderJournalEvent{
				{Version: 1, At: now, Type: orderJournalEventSendAttempted, OrderRef: "still-uncertain", ReservedOrderID: 77, Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", SendState: orderSendStateSendAttempted},
				{Version: 1, At: now.Add(time.Second), Type: orderJournalEventBrokerError, OrderRef: "still-uncertain", ReservedOrderID: 77, Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper", ErrorCode: tt.errorCode, Status: tt.status, SendState: tt.sendState, Message: "untrusted text says reject and can't find order"},
			}
			var raw []byte
			for _, ev := range events {
				line, err := json.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, line...)
				raw = append(raw, '\n')
			}
			path := filepath.Join(t.TempDir(), "order-journal.jsonl")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			selection, err := loadLegacyOrderImportSelection(path)
			if err != nil {
				t.Fatalf("load selection: %v", err)
			}
			if len(selection.Events) != 2 || selection.Events[1].Type != orderJournalEventBrokerError {
				t.Fatalf("retained events = %+v, want uncertain broker-error chain", selection.Events)
			}
		})
	}
}

func TestCoreBrokerScopeCanonicalizesAccountCase(t *testing.T) {
	t.Parallel()
	lower, err := coreBrokerScopeFromEvent(orderJournalEvent{
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "du123", Mode: "paper",
	})
	if err != nil {
		t.Fatalf("lowercase scope: %v", err)
	}
	upper, err := coreBrokerScopeFromEvent(orderJournalEvent{
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: " DU123 ", Mode: "PAPER",
	})
	if err != nil {
		t.Fatalf("uppercase scope: %v", err)
	}
	if lower != upper || lower.Account != "DU123" {
		t.Fatalf("case variants bind differently: lower=%+v upper=%+v", lower, upper)
	}
}

func TestLegacyOrderAuthorityImportPreservesSafetyStateAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	route := corestore.BrokerScope{Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper"}
	legacy := []orderJournalEvent{
		{Version: 1, At: now, Type: orderJournalEventSendAttempted, OrderRef: "terminal", PreviewTokenID: "terminal-token", ReservedOrderID: 900, Endpoint: route.Endpoint, ClientID: route.ClientID, Account: route.Account, Mode: route.Mode, SendState: orderSendStateSendAttempted},
		{Version: 1, At: now.Add(time.Second), Type: orderJournalEventStatusUpdated, OrderRef: "terminal", ReservedOrderID: 900, Endpoint: route.Endpoint, ClientID: route.ClientID, Account: route.Account, Mode: route.Mode, Status: "Filled", Filled: 1, SendState: orderSendStateTerminal},
		{Version: 1, At: now.Add(2 * time.Second), Type: orderJournalEventSendAttempted, OrderRef: "active", PreviewTokenID: "active-token", ReservedOrderID: 100, Endpoint: route.Endpoint, ClientID: route.ClientID, Account: route.Account, Mode: route.Mode, SendState: orderSendStateUncertainSend},
	}
	legacyPath := filepath.Join(t.TempDir(), "order-journal.jsonl")
	f, err := os.OpenFile(legacyPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create legacy journal: %v", err)
	}
	for _, ev := range legacy {
		if err := json.NewEncoder(f).Encode(ev); err != nil {
			_ = f.Close()
			t.Fatalf("write legacy journal: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close legacy journal: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "authority", "daemon.db")
	authority, err := corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	purgePath := filepath.Join(filepath.Dir(legacyPath), "purge-ledger.json")
	cutover, err := importLegacyTradingAuthority(ctx, authority, legacyPath, purgePath)
	if err != nil {
		t.Fatalf("import legacy trading authority: %v", err)
	}
	parity := cutover.Orders
	if parity.GlobalOrderIDFloor != 900 || parity.RetainedEventCount != 1 || parity.ConsumedTokenCount != 2 {
		t.Fatalf("import parity = %+v", parity)
	}
	if cutover.Purge.ActiveRows != 0 || cutover.Purge.FillCursors != 0 {
		t.Fatalf("empty purge parity = %+v", cutover.Purge)
	}
	if _, err := importLegacyTradingAuthority(ctx, authority, legacyPath, purgePath); err != nil {
		t.Fatalf("idempotent coherent reimport: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}

	authority, err = corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer authority.Close()
	journal := newOrderJournalStore(legacyPath)
	if err := journal.UseCoreStore(authority); err != nil {
		t.Fatalf("attach reopened authority: %v", err)
	}
	loaded, err := journal.LoadEvents(0)
	if err != nil {
		t.Fatalf("load imported events: %v", err)
	}
	if len(loaded) != 1 || loaded[0].OrderRef != "active" {
		t.Fatalf("loaded retained events = %+v", loaded)
	}
	if floor, err := authority.GlobalOrderIDFloor(ctx); err != nil || floor != 900 {
		t.Fatalf("global floor after restart = %d, %v; want 900", floor, err)
	}
	head, err := authority.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("authority head: %v", err)
	}
	err = journal.StagePreTransmit("terminal-token", head.AuthorityEpoch, head.SignerGeneration, 901, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{{
		At: now.Add(3 * time.Second), Type: orderJournalEventSendAttempted, OrderRef: "should-not-send",
		PreviewTokenID: "terminal-token", ReservedOrderID: 901,
		Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "du999", Mode: "paper",
	}})
	if !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("omitted-chain token after restart err = %v, want consumed", err)
	}
}

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
	doc, ok, err := authority.GetStateDocument(t.Context(), purgeLedgerStateScope, purgeLedgerStateKind)
	if err != nil || !ok {
		t.Fatalf("fresh purge document: found=%v error=%v", ok, err)
	}
	var ledger purgeLedgerFile
	if err := json.Unmarshal(doc.JSON, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Rows) != 0 || !ledger.UpdatedAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("fresh purge authority = %+v", ledger)
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
