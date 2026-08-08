package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPlatformSettingsSQLiteMutationAndAuditAreAtomicAndNoopSilent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	at := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(privateTestDir(t), "settings-audit.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := initializeFreshDaemonState(ctx, core); err != nil {
		t.Fatal(err)
	}
	store := &platformSettingsStore{}
	if err := store.bindCore(ctx, core); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Resolved{Trading: config.Trading{Mode: config.TradingModeDisabled}}, platformSettings: store, now: func() time.Time { return at }}
	beforeDoc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatalf("load settings before update: ok=%v err=%v", ok, err)
	}

	if _, err := srv.handleSettingsUpdate(ctx, &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":false}}}`)}); err != nil {
		t.Fatal(err)
	}
	afterDoc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatalf("load settings after update: ok=%v err=%v", ok, err)
	}
	events, err := core.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: coreEventPlatformSettings})
	if err != nil {
		t.Fatal(err)
	}
	if afterDoc.Revision != beforeDoc.Revision+1 || len(events) != 1 || events[0].Origin != rpc.OrderOriginAgent || events[0].Action != coreEventActionUpdate {
		t.Fatalf("revision %d->%d events=%+v", beforeDoc.Revision, afterDoc.Revision, events)
	}
	var event platformSettingsUpdateEventV1
	if err := json.Unmarshal(events[0].PayloadJSON, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Keys) != 1 || event.Keys[0] != "features.stock_protection.enabled" ||
		string(event.Before[event.Keys[0]]) != "null" || string(event.After[event.Keys[0]]) != "false" ||
		event.ExpectedRevision != beforeDoc.Revision || event.NewRevision != afterDoc.Revision ||
		event.OldTradingControlGeneration != 0 || event.NewTradingControlGeneration != 0 {
		t.Fatalf("settings audit event=%+v", event)
	}

	// Reapplying the same semantic value changes neither authority surface.
	if _, err := srv.handleSettingsUpdate(ctx, &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":false}}}`)}); err != nil {
		t.Fatal(err)
	}
	noOpDoc, _, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil {
		t.Fatal(err)
	}
	noOpEvents, err := core.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: coreEventPlatformSettings})
	if err != nil {
		t.Fatal(err)
	}
	if noOpDoc.Revision != afterDoc.Revision || len(noOpEvents) != 1 {
		t.Fatalf("semantic no-op changed revision/events: revision=%d events=%d", noOpDoc.Revision, len(noOpEvents))
	}

	if _, err := srv.handleSettingsUpdate(ctx, &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"freeze":true}}`)}); err != nil {
		t.Fatal(err)
	}
	controlEvents, err := core.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: coreEventPlatformSettings})
	if err != nil {
		t.Fatal(err)
	}
	var control platformSettingsUpdateEventV1
	if len(controlEvents) != 2 || controlEvents[1].Origin != rpc.OrderOriginHumanTTY || json.Unmarshal(controlEvents[1].PayloadJSON, &control) != nil {
		t.Fatalf("control events=%+v", controlEvents)
	}
	if len(control.Keys) != 1 || control.Keys[0] != "trading.freeze" || control.OldTradingControlGeneration != 0 || control.NewTradingControlGeneration != 1 {
		t.Fatalf("control audit event=%+v", control)
	}
}

func TestPlatformSettingsAuditInsertFailureRollsBackStateCAS(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	at := time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(privateTestDir(t), "settings-audit-rollback.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := initializeFreshDaemonState(ctx, core); err != nil {
		t.Fatal(err)
	}
	store := &platformSettingsStore{}
	if err := store.bindCore(ctx, core); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Resolved{Trading: config.Trading{Mode: config.TradingModeDisabled}}, platformSettings: store, now: func() time.Time { return at }}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatalf("load settings: ok=%v err=%v", ok, err)
	}
	key := "features.stock_protection.enabled"
	event := platformSettingsUpdateEventV1{
		Version: 1, Keys: []string{key},
		Before:           map[string]json.RawMessage{key: json.RawMessage("null")},
		After:            map[string]json.RawMessage{key: json.RawMessage("false")},
		ExpectedRevision: doc.Revision, NewRevision: doc.Revision + 1,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := corestore.EventInput{
		ScopeKey: daemonStateScope, EventKey: coreEventKey(coreEventPlatformSettings, at, raw, int(event.NewRevision)),
		Type: coreEventPlatformSettings, Action: coreEventActionUpdate, Origin: rpc.OrderOriginAgent,
		OccurredAt: at, PayloadJSON: raw,
	}
	if _, err := core.AppendEvents(ctx, []corestore.EventInput{duplicate}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.handleSettingsUpdate(ctx, &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":false}}}`)}); err == nil {
		t.Fatal("duplicate audit key did not fail the atomic settings mutation")
	}
	after, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatalf("load settings after rollback: ok=%v err=%v", ok, err)
	}
	if after.Revision != doc.Revision || store.snapshot().Features.StockProtection.Enabled != nil {
		t.Fatalf("failed audit insert published state: revision=%d data=%+v", after.Revision, store.snapshot())
	}
	events, err := core.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: coreEventPlatformSettings})
	if err != nil || len(events) != 1 {
		t.Fatalf("events after rollback=%d err=%v", len(events), err)
	}
}

func TestPlatformSettingsTradingControlGenerationPatchABAAndUnrelatedSetting(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeDisabled})
	if got := srv.platformSettings.tradingControlGeneration(); got != 0 {
		t.Fatalf("initial trading-control generation = %d, want 0", got)
	}

	patch := func(raw string) {
		t.Helper()
		if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(raw)}); err != nil {
			t.Fatalf("settings patch %s: %v", raw, err)
		}
	}
	patch(`{"origin":"human-tty","trading":{"freeze":true}}`)
	if got := srv.platformSettings.tradingControlGeneration(); got != 1 {
		t.Fatalf("generation after freeze=true = %d, want 1", got)
	}
	patch(`{"origin":"human-tty","trading":{"freeze":true}}`)
	if got := srv.platformSettings.tradingControlGeneration(); got != 1 {
		t.Fatalf("generation after equal freeze patch = %d, want 1", got)
	}
	patch(`{"features":{"stock_protection":{"enabled":false}}}`)
	if got := srv.platformSettings.tradingControlGeneration(); got != 1 {
		t.Fatalf("generation after unrelated feature patch = %d, want 1", got)
	}
	patch(`{"origin":"human-tty","trading":{"freeze":false}}`)
	patch(`{"origin":"human-tty","trading":{"freeze":true}}`)
	if got := srv.platformSettings.tradingControlGeneration(); got != 3 {
		t.Fatalf("generation after freeze ABA = %d, want 3", got)
	}

	reopened, err := newPlatformSettingsStore(srv.platformSettings.path)
	if err != nil {
		t.Fatalf("reopen legacy settings store: %v", err)
	}
	if got := reopened.tradingControlGeneration(); got != 3 {
		t.Fatalf("reopened legacy generation = %d, want 3", got)
	}
	if freeze := reopened.snapshot().Trading.Freeze; freeze == nil || !*freeze {
		t.Fatalf("reopened legacy freeze = %v, want true", freeze)
	}
}

func TestPlatformSettingsTradingControlGenerationCoversExactlyFiveControls(t *testing.T) {
	t.Parallel()
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatalf("newPlatformSettingsStore: %v", err)
	}
	set := func(want uint64, mutate func(*platformSettingsData)) {
		t.Helper()
		if err := store.update(func(next *platformSettingsData) error {
			mutate(next)
			return nil
		}); err != nil {
			t.Fatalf("update for generation %d: %v", want, err)
		}
		if got := store.tradingControlGeneration(); got != want {
			t.Fatalf("trading-control generation = %d, want %d", got, want)
		}
	}

	set(1, func(next *platformSettingsData) {
		notional, contracts, allowStock, allowSTO, freeze := 1000.0, 2, true, true, true
		next.Trading.MaxNotional = &notional
		next.Trading.MaxOptionContracts = &contracts
		next.Trading.AllowStockShort = &allowStock
		next.Trading.AllowOptionSellToOpen = &allowSTO
		next.Trading.Freeze = &freeze
	})
	// Fresh pointers carrying the same five values are not a material change,
	// and changing all five in the preceding update advanced only once.
	set(1, func(next *platformSettingsData) {
		notional, contracts, allowStock, allowSTO, freeze := 1000.0, 2, true, true, true
		next.Trading.MaxNotional = &notional
		next.Trading.MaxOptionContracts = &contracts
		next.Trading.AllowStockShort = &allowStock
		next.Trading.AllowOptionSellToOpen = &allowSTO
		next.Trading.Freeze = &freeze
	})
	set(2, func(next *platformSettingsData) {
		value := 900.0
		next.Trading.MaxNotional = &value
	})
	set(3, func(next *platformSettingsData) {
		value := 1
		next.Trading.MaxOptionContracts = &value
	})
	set(4, func(next *platformSettingsData) {
		value := false
		next.Trading.AllowStockShort = &value
	})
	set(5, func(next *platformSettingsData) {
		value := false
		next.Trading.AllowOptionSellToOpen = &value
	})
	set(6, func(next *platformSettingsData) {
		value := false
		next.Trading.Freeze = &value
	})
	base := config.Trading{
		MaxNotional:           10_000,
		MaxOptionContracts:    10,
		AllowStockShort:       true,
		AllowOptionSellToOpen: true,
	}
	effective, generation := store.tradingControlSnapshot(base, true)
	if generation != 6 || effective.MaxNotional != 900 || effective.MaxOptionContracts != 1 ||
		effective.AllowStockShort || effective.AllowOptionSellToOpen {
		t.Fatalf("effective control snapshot = %+v generation=%d", effective, generation)
	}
	unmodified, generation := store.tradingControlSnapshot(base, false)
	if generation != 6 || unmodified.MaxNotional != base.MaxNotional ||
		unmodified.MaxOptionContracts != base.MaxOptionContracts ||
		unmodified.AllowStockShort != base.AllowStockShort ||
		unmodified.AllowOptionSellToOpen != base.AllowOptionSellToOpen {
		t.Fatalf("non-applying control snapshot = %+v generation=%d, want base %+v generation=6", unmodified, generation, base)
	}
}

func TestPlatformSettingsTradingControlGenerationSQLitePersistenceFailureDoesNotPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatalf("open core store: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = core.Close()
		}
	})
	if err := initializeFreshDaemonState(ctx, core); err != nil {
		t.Fatalf("initialize fresh daemon state: %v", err)
	}

	store := &platformSettingsStore{}
	if err := store.bindCore(ctx, core); err != nil {
		t.Fatalf("bind core: %v", err)
	}
	if err := store.update(func(next *platformSettingsData) error {
		value := true
		next.Trading.Freeze = &value
		return nil
	}); err != nil {
		t.Fatalf("persist initial freeze: %v", err)
	}
	if got := store.tradingControlGeneration(); got != 1 {
		t.Fatalf("SQLite generation after commit = %d, want 1", got)
	}

	reloaded := &platformSettingsStore{}
	if err := reloaded.bindCore(ctx, core); err != nil {
		t.Fatalf("reload SQLite settings: %v", err)
	}
	if got := reloaded.tradingControlGeneration(); got != 1 {
		t.Fatalf("reloaded SQLite generation = %d, want 1", got)
	}

	if err := core.Close(); err != nil {
		t.Fatalf("close core store: %v", err)
	}
	closed = true
	if err := store.update(func(next *platformSettingsData) error {
		value := false
		next.Trading.Freeze = &value
		return nil
	}); err == nil {
		t.Fatal("closed SQLite authority accepted settings mutation")
	}
	if got := store.tradingControlGeneration(); got != 1 {
		t.Fatalf("failed persistence published generation = %d, want 1", got)
	}
	if freeze := store.snapshot().Trading.Freeze; freeze == nil || !*freeze {
		t.Fatalf("failed persistence published freeze = %v, want retained true", freeze)
	}
}
