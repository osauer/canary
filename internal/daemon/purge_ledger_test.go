package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPurgeLedgerFillDeltasAreIdempotentAndRowsAreRetained(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 19, 0, 0, 0, time.UTC)
	store := newTestPurgeLedgerStore(t, filepath.Join(t.TempDir(), "purge-ledger.json"), func() time.Time { return now })
	contract := purgeLedgerTestStockContract()

	if err := commitTestPurgeLedgerEvent(store, purgeLedgerEventWithContract(orderJournalEvent{
		Type:     orderJournalEventSendAttempted,
		Source:   purgeExecuteSource,
		OrderRef: "purge-1",
		PurgeID:  "purge-test",
		LegID:    "leg-aapl",
		Account:  "DU123",
		Action:   rpc.OrderActionSell,
		Quantity: 4,
	}, contract)); err != nil {
		t.Fatalf("send attempt CommitOrderLifecycle: %v", err)
	}
	rows, totals, err := store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after send attempt: %v", err)
	}
	if len(rows) != 0 || totals.ActiveRows != 0 {
		t.Fatalf("send attempt mutated ledger rows=%+v totals=%+v", rows, totals)
	}

	purgePartial := purgeLedgerFillEvent(purgeExecuteSource, "purge-1", "purge-test", "leg-aapl", contract, rpc.OrderActionSell, 4, 2, 100)
	if err := commitTestPurgeLedgerEvent(store, purgePartial); err != nil {
		t.Fatalf("partial purge fill: %v", err)
	}
	if err := commitTestPurgeLedgerEvent(store, purgePartial); err != nil {
		t.Fatalf("duplicate purge fill: %v", err)
	}
	rows, _, err = store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after partial purge: %v", err)
	}
	if len(rows) != 1 || rows[0].PurgedQuantity != 2 || rows[0].RemainingQuantity != 2 || rows[0].PurgeValue != 200 {
		t.Fatalf("partial purge row = %+v", rows)
	}

	purgeFull := purgeLedgerFillEvent(purgeExecuteSource, "purge-1", "purge-test", "leg-aapl", contract, rpc.OrderActionSell, 4, 4, 101)
	if err := commitTestPurgeLedgerEvent(store, purgeFull); err != nil {
		t.Fatalf("full purge fill: %v", err)
	}
	rows, _, err = store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after full purge: %v", err)
	}
	if len(rows) != 1 || rows[0].PurgedQuantity != 4 || rows[0].PurgeValue != 404 || rows[0].RemainingQuantity != 4 {
		t.Fatalf("full purge row = %+v", rows)
	}

	restorePartial := purgeLedgerFillEvent(purgeRestoreSource, "restore-1", "purge-test", "leg-aapl", contract, rpc.OrderActionBuy, 4, 1, 95)
	if err := commitTestPurgeLedgerEvent(store, restorePartial); err != nil {
		t.Fatalf("partial restore fill: %v", err)
	}
	if err := commitTestPurgeLedgerEvent(store, restorePartial); err != nil {
		t.Fatalf("duplicate restore fill: %v", err)
	}
	rows, _, err = store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after partial restore: %v", err)
	}
	if len(rows) != 1 || rows[0].RestoredQuantity != 1 || rows[0].RemainingQuantity != 3 || rows[0].RestoreValue != 95 || rows[0].ShadowPnL != 6 {
		t.Fatalf("partial restore row = %+v", rows)
	}

	restoreFull := purgeLedgerFillEvent(purgeRestoreSource, "restore-2", "purge-test", "leg-aapl", contract, rpc.OrderActionBuy, 3, 3, 96)
	if err := commitTestPurgeLedgerEvent(store, restoreFull); err != nil {
		t.Fatalf("full restore fill: %v", err)
	}
	rows, totals, err = store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after full restore: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("restored row count = %d, want retained row", len(rows))
	}
	row := rows[0]
	if row.Status != purgeLedgerStatusRestored || row.RemainingQuantity != 0 || row.RestoredQuantity != 4 {
		t.Fatalf("restored row = %+v", row)
	}
	if totals.ActiveRows != 0 || totals.RestoredRows != 1 || totals.RemainingQuantity != 0 || totals.ShadowPnL != 21 {
		t.Fatalf("restored totals = %+v", totals)
	}
}

func TestPurgeLedgerShadowPnLForShortRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 19, 5, 0, 0, time.UTC)
	store := newTestPurgeLedgerStore(t, filepath.Join(t.TempDir(), "purge-ledger.json"), func() time.Time { return now })
	contract := purgeLedgerTestStockContract()

	purgeFill := purgeLedgerFillEvent(purgeExecuteSource, "purge-short", "purge-test", "leg-aapl-short", contract, rpc.OrderActionBuy, 2, 2, 50)
	restoreFill := purgeLedgerFillEvent(purgeRestoreSource, "restore-short", "purge-test", "leg-aapl-short", contract, rpc.OrderActionSell, 2, 2, 45)
	if err := commitTestPurgeLedgerEvent(store, purgeFill); err != nil {
		t.Fatalf("short purge fill: %v", err)
	}
	if err := commitTestPurgeLedgerEvent(store, restoreFill); err != nil {
		t.Fatalf("short restore fill: %v", err)
	}
	rows, totals, err := store.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("short snapshot: %v", err)
	}
	if len(rows) != 1 || rows[0].OriginalSide != purgeOriginalSideShort || rows[0].ShadowPnL != -10 || totals.ShadowPnL != -10 {
		t.Fatalf("short row/totals = %+v / %+v", rows, totals)
	}
}

func TestPurgeLedgerSnapshotFiltersByBrokerScopeMode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 8, 18, 45, 0, 0, time.UTC)
	store := newTestPurgeLedgerStore(t, filepath.Join(t.TempDir(), "purge-ledger.json"), func() time.Time { return now })
	contract := purgeLedgerTestStockContract()

	paper := purgeLedgerFillEvent(purgeExecuteSource, "paper-purge", "purge-paper", "leg-aapl", contract, rpc.OrderActionSell, 1, 1, 100)
	paper.Account = "DU7654321"
	paper.Mode = rpc.AccountModePaper
	live := purgeLedgerFillEvent(purgeExecuteSource, "live-purge", "purge-live", "leg-aapl", contract, rpc.OrderActionSell, 2, 2, 101)
	live.Account = "U1234567"
	live.Mode = rpc.AccountModeLive
	if err := commitTestPurgeLedgerEvent(store, paper); err != nil {
		t.Fatalf("paper fill: %v", err)
	}
	if err := commitTestPurgeLedgerEvent(store, live); err != nil {
		t.Fatalf("live fill: %v", err)
	}

	rows, totals, err := store.Snapshot(brokerStateScope{Account: "All", Mode: rpc.AccountModeLive}, "")
	if err != nil {
		t.Fatalf("live aggregate snapshot: %v", err)
	}
	if len(rows) != 0 || totals.PurgedQuantity != 0 {
		t.Fatalf("live aggregate rows=%+v totals=%+v, want no concrete-account rows", rows, totals)
	}
	rows, totals, err = store.Snapshot(brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}, "")
	if err != nil {
		t.Fatalf("live account snapshot: %v", err)
	}
	if len(rows) != 1 || rows[0].PurgeID != "purge-live" || rows[0].Mode != rpc.AccountModeLive || totals.PurgedQuantity != 2 {
		t.Fatalf("live account rows=%+v totals=%+v, want only live row", rows, totals)
	}
	rows, totals, err = store.Snapshot(brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, "")
	if err != nil {
		t.Fatalf("paper account snapshot: %v", err)
	}
	if len(rows) != 1 || rows[0].PurgeID != "purge-paper" || rows[0].Mode != rpc.AccountModePaper || totals.PurgedQuantity != 1 {
		t.Fatalf("paper rows=%+v totals=%+v, want only paper row", rows, totals)
	}
}

func TestLegacyPurgeLedgerV1ImportsThroughOrderRouteWithoutMutatingSource(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "purge-ledger.json")
	original := []byte(`{
  "kind": "ibkr.purge_ledger",
  "schema_version": "purge-ledger-v1",
  "updated_at": "2026-01-07T12:00:00Z",
  "rows": [{
    "leg_id": "legacy-active",
    "purge_id": "purge-v1",
    "symbol": "SAP",
    "sec_type": "STK",
    "contract": {"symbol":"SAP","sec_type":"STK","exchange":"SMART","currency":"EUR"},
    "account": "DU123",
    "currency": "EUR",
    "original_side": "long",
    "original_quantity": 4,
    "purge_action": "SELL",
    "restore_action": "BUY",
    "multiplier": 1,
    "purged_quantity": 4,
    "restored_quantity": 1,
    "remaining_quantity": 3,
    "status": "active",
    "last_purge_order_ref": "purge-order",
    "last_restore_order_ref": "restore-order",
    "created_at": "2026-01-07T10:00:00Z",
    "updated_at": "2026-01-07T12:00:00Z",
    "order_fills": {
      "purge-order": {"source":"purge_execute","order_ref":"purge-order","filled":4,"avg_fill_price":120},
      "restore-order": {"source":"purge_restore","order_ref":"restore-order","filled":1,"avg_fill_price":115}
    }
  }]
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write v1 ledger: %v", err)
	}
	routeEvent := func(ref string) orderJournalEvent {
		return orderJournalEvent{
			Version: 1, Type: orderJournalEventStatusUpdated, OrderRef: ref,
			Endpoint: "127.0.0.1:4002", ClientID: 15, clientIDPresent: true,
			Account: "DU123", Mode: "paper",
		}
	}
	selection, err := loadLegacyPurgeImportSelection(path, legacyOrderImportSelection{
		SourceEvents: []orderJournalEvent{routeEvent("purge-order"), routeEvent("restore-order")},
	})
	if err != nil {
		t.Fatalf("load v1 purge ledger: %v", err)
	}
	if selection.Kind != purgeLedgerKind || selection.SchemaVersion != purgeLedgerSchemaVersion {
		t.Fatalf("selection identity = %q/%q, want current v2", selection.Kind, selection.SchemaVersion)
	}
	if len(selection.Rows) != 1 {
		t.Fatalf("selected rows = %d, want 1", len(selection.Rows))
	}
	row := selection.Rows[0]
	if row.Endpoint != "127.0.0.1:4002" || row.ClientID != 15 || row.Account != "DU123" || row.Mode != "paper" {
		t.Fatalf("derived route = endpoint=%q client=%d account=%q mode=%q", row.Endpoint, row.ClientID, row.Account, row.Mode)
	}
	if row.RemainingQuantity != 3 || len(row.OrderFills) != 2 {
		t.Fatalf("v1 restore continuity = remaining=%v fills=%+v", row.RemainingQuantity, row.OrderFills)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("legacy purge source was deleted: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("legacy purge source was mutated")
	}
}

func TestLegacyPurgeLedgerUnknownSchemaFailsWithoutMutatingSource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "wrong kind", raw: `{"kind":"other","schema_version":"purge-ledger-v1","rows":[]}`},
		{name: "future schema", raw: `{"kind":"ibkr.purge_ledger","schema_version":"purge-ledger-v3","rows":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "purge-ledger.json")
			original := []byte(tc.raw + "\n")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatalf("write unsupported ledger: %v", err)
			}
			if _, err := loadLegacyPurgeImportSelection(path, legacyOrderImportSelection{}); err == nil {
				t.Fatal("unsupported purge schema must fail cutover")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("legacy purge source was deleted: %v", err)
			}
			if string(after) != string(original) {
				t.Fatal("legacy purge source was mutated")
			}
		})
	}
}

func TestOrderLifecycleFillUpdatesPurgeLedgerFromJournalIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 19, 10, 0, 0, time.UTC)
	dir := t.TempDir()
	journal := newTestOrderJournalStore(t, filepath.Join(dir, "order-journal.jsonl"))
	authority, err := journal.coreStore()
	if err != nil {
		t.Fatalf("order authority: %v", err)
	}
	ledger := newPurgeLedgerStore(filepath.Join(dir, "purge-ledger.json"), func() time.Time { return now })
	if err := ledger.UseCoreStore(authority); err != nil {
		t.Fatalf("attach purge authority: %v", err)
	}
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(31), Account: "DU123"},
			Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
		},
		orderJournal: journal,
		purgeLedger:  ledger,
		coreStore:    authority,
		endpoint:     discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU123"},
		now:          func() time.Time { return now },
	}
	contract := purgeLedgerTestStockContract()
	if err := srv.orderJournal.Append(purgeLedgerEventWithContract(orderJournalEvent{
		At:              now,
		Type:            orderJournalEventSendAttempted,
		Source:          purgeExecuteSource,
		PurgeID:         "purge-test",
		LegID:           "leg-aapl",
		OrderRef:        "purge-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU123",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		LimitPrice:      99,
		SendState:       orderSendStateSendAttempted,
	}, contract)); err != nil {
		t.Fatalf("append purge send: %v", err)
	}
	rows, _, err := srv.purgeLedger.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot before lifecycle: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("send attempt created ledger rows: %+v", rows)
	}

	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type:            ibkrlib.OrderLifecycleEventStatus,
		OrderID:         1001,
		PermID:          9001,
		ClientID:        31,
		ClientIDPresent: true,
		Status:          "Filled",
		Filled:          2,
		Remaining:       0,
		AvgFillPrice:    100,
	})
	rows, _, err = srv.purgeLedger.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after purge lifecycle: %v", err)
	}
	if len(rows) != 1 || rows[0].Symbol != "AAPL" || rows[0].PurgeAction != rpc.OrderActionSell || rows[0].PurgedQuantity != 2 || rows[0].RemainingQuantity != 2 {
		t.Fatalf("purge lifecycle row = %+v", rows)
	}

	if err := srv.orderJournal.Append(purgeLedgerEventWithContract(orderJournalEvent{
		At:              now.Add(time.Second),
		Type:            orderJournalEventSendAttempted,
		Source:          purgeRestoreSource,
		PurgeID:         "purge-test",
		LegID:           "leg-aapl",
		OrderRef:        "restore-1",
		ReservedOrderID: 1002,
		ClientID:        31,
		Account:         "DU123",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Action:          rpc.OrderActionBuy,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		LimitPrice:      90,
		SendState:       orderSendStateSendAttempted,
	}, contract)); err != nil {
		t.Fatalf("append restore send: %v", err)
	}
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type:            ibkrlib.OrderLifecycleEventStatus,
		OrderID:         1002,
		PermID:          9002,
		ClientID:        31,
		ClientIDPresent: true,
		Status:          "Filled",
		Filled:          2,
		Remaining:       0,
		AvgFillPrice:    90,
	})
	rows, totals, err := srv.purgeLedger.Snapshot(brokerStateScope{}, "")
	if err != nil {
		t.Fatalf("snapshot after restore lifecycle: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != purgeLedgerStatusRestored || rows[0].RemainingQuantity != 0 || rows[0].RestoredQuantity != 2 {
		t.Fatalf("restore lifecycle row = %+v", rows)
	}
	if totals.ActiveRows != 0 || totals.RestoredRows != 1 || totals.ShadowPnL != 20 {
		t.Fatalf("restore lifecycle totals = %+v", totals)
	}
}

func TestAtomicPurgeLifecycleCursorSurvivesRestartAndDeduplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dbPath := filepath.Join(t.TempDir(), "authority", "daemon.db")
	authority, err := corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	journal := newOrderJournalStore("")
	ledger := newPurgeLedgerStore("", func() time.Time { return now })
	if err := journal.UseCoreStore(authority); err != nil {
		t.Fatalf("attach order authority: %v", err)
	}
	if err := ledger.UseCoreStore(authority); err != nil {
		t.Fatalf("attach purge authority: %v", err)
	}
	fill := purgeLedgerEventWithContract(orderJournalEvent{
		At: now, Type: orderJournalEventStatusUpdated, Source: purgeExecuteSource,
		PurgeID: "purge-restart", LegID: "leg-aapl", OrderRef: "purge-order",
		ReservedOrderID: 1001, ClientID: 31, Account: "DU123", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Action: rpc.OrderActionSell, Quantity: 2, Filled: 2, AvgFillPrice: 100, Status: "Filled", SendState: orderSendStateTerminal,
	}, purgeLedgerTestStockContract())
	if err := ledger.CommitOrderLifecycle(journal, fill); err != nil {
		t.Fatalf("commit lifecycle and cursor: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}

	authority, err = corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer authority.Close()
	journal = newOrderJournalStore("")
	ledger = newPurgeLedgerStore("", func() time.Time { return now.Add(time.Minute) })
	if err := journal.UseCoreStore(authority); err != nil {
		t.Fatalf("reattach order authority: %v", err)
	}
	if err := ledger.UseCoreStore(authority); err != nil {
		t.Fatalf("reattach purge authority: %v", err)
	}
	fill.At = now.Add(time.Minute)
	if err := ledger.CommitOrderLifecycle(journal, fill); err != nil {
		t.Fatalf("commit duplicate cumulative lifecycle: %v", err)
	}
	rows, err := ledger.AllRows()
	if err != nil {
		t.Fatalf("load restarted purge ledger: %v", err)
	}
	if len(rows) != 1 || rows[0].PurgedQuantity != 2 || rows[0].OrderFills[fill.OrderRef].Filled != 2 {
		t.Fatalf("restarted cumulative cursor = %+v", rows)
	}
	events, err := journal.LoadEvents(0)
	if err != nil {
		t.Fatalf("load restarted lifecycle events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("lifecycle event count = %d, want both callbacks while cursor stays idempotent", len(events))
	}
}

func purgeLedgerFillEvent(source, orderRef, purgeID, legID string, contract rpc.ContractParams, action string, quantity, filled, avgFillPrice float64) orderJournalEvent {
	return purgeLedgerEventWithContract(orderJournalEvent{
		Type:         orderJournalEventStatusUpdated,
		Source:       source,
		OrderRef:     orderRef,
		PurgeID:      purgeID,
		LegID:        legID,
		Account:      "DU123",
		Mode:         rpc.AccountModePaper,
		Action:       action,
		OrderType:    rpc.OrderTypeLMT,
		TIF:          rpc.OrderTIFDay,
		Quantity:     quantity,
		Filled:       filled,
		Remaining:    max(quantity-filled, 0),
		AvgFillPrice: avgFillPrice,
		SendState:    orderSendStateTerminal,
	}, contract)
}

func commitTestPurgeLedgerEvent(store *purgeLedgerStore, ev orderJournalEvent) error {
	if ev.At.IsZero() {
		ev.At = store.currentTime()
	}
	if ev.Endpoint == "" {
		ev.Endpoint = "127.0.0.1:4002"
	}
	if ev.ClientID == 0 {
		ev.ClientID = 31
	}
	if ev.Mode == "" {
		ev.Mode = rpc.AccountModePaper
	}
	if ev.OrderType == "" {
		ev.OrderType = rpc.OrderTypeLMT
	}
	if ev.TIF == "" {
		ev.TIF = rpc.OrderTIFDay
	}
	if ev.SendState == "" {
		ev.SendState = orderSendStateSendAttempted
	}
	authority, err := store.coreStore()
	if err != nil {
		return err
	}
	journal := newOrderJournalStore("")
	if err := journal.UseCoreStore(authority); err != nil {
		return err
	}
	return store.CommitOrderLifecycle(journal, ev)
}

func purgeLedgerEventWithContract(ev orderJournalEvent, contract rpc.ContractParams) orderJournalEvent {
	ev.ConID = contract.ConID
	ev.Symbol = contract.Symbol
	ev.SecType = contract.SecType
	ev.Exchange = contract.Exchange
	ev.PrimaryExch = contract.PrimaryExch
	ev.Currency = contract.Currency
	ev.LocalSymbol = contract.LocalSymbol
	ev.TradingClass = contract.TradingClass
	ev.Expiry = contract.Expiry
	ev.Strike = contract.Strike
	ev.Right = contract.Right
	ev.Multiplier = contract.Multiplier
	return ev
}

func purgeLedgerTestStockContract() rpc.ContractParams {
	return rpc.ContractParams{
		ConID:       265598,
		Symbol:      "AAPL",
		SecType:     "STK",
		Exchange:    "SMART",
		PrimaryExch: "NASDAQ",
		Currency:    "USD",
		Multiplier:  1,
	}
}
