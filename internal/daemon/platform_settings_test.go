package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPlatformSettingsDefaultsAndPersistence(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{})

	got, err := srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet: %v", err)
	}
	if !got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled default = false, want true")
	}
	if !got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled default = false, want true")
	}
	if got.Features.PurgeRestore.Enabled.Access != rpc.SettingsAccessWrite {
		t.Fatalf("purge_restore.enabled access = %q, want write", got.Features.PurgeRestore.Enabled.Access)
	}
	if got.Features.StockProtection.Enabled.Access != rpc.SettingsAccessWrite {
		t.Fatalf("stock_protection.enabled access = %q, want write", got.Features.StockProtection.Enabled.Access)
	}
	if !got.AutoTrade.ProposalsEnabled.Value {
		t.Fatal("auto_trade.proposals_enabled default = false, want true")
	}
	if got.AutoTrade.ProposalsEnabled.Access != rpc.SettingsAccessRead || got.AutoTrade.ProposalsEnabled.Source != rpc.SettingsSourceConfig {
		t.Fatalf("auto_trade.proposals_enabled meta = %s/%s, want read/config", got.AutoTrade.ProposalsEnabled.Access, got.AutoTrade.ProposalsEnabled.Source)
	}
	if !got.AutoTrade.FastPathEnabled.Value {
		t.Fatal("auto_trade.fast_path_enabled default = false, want true")
	}
	if !got.Regime.Journal.Enabled.Value || !got.Stress.Journal.Enabled.Value {
		t.Fatalf("calibration journals default disabled: regime=%t stress=%t", got.Regime.Journal.Enabled.Value, got.Stress.Journal.Enabled.Value)
	}

	patch := mustRaw(t, map[string]any{
		"features": map[string]any{
			"purge_restore":    map[string]any{"enabled": false},
			"stock_protection": map[string]any{"enabled": false},
		},
	})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: patch}); err != nil {
		t.Fatalf("disable runtime settings: %v", err)
	}
	reopened, err := newPlatformSettingsStore(srv.platformSettings.path)
	if err != nil {
		t.Fatalf("reopen settings store: %v", err)
	}
	srv.platformSettings = reopened
	got, err = srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet after reopen: %v", err)
	}
	if got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled persisted true, want false")
	}
	if got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled persisted true, want false")
	}

	reset := []byte(`{"features":{"purge_restore":{"enabled":null},"stock_protection":{"enabled":null}}}`)
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: reset}); err != nil {
		t.Fatalf("reset runtime settings: %v", err)
	}
	got, _ = srv.handleSettingsGet()
	if !got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled reset = false, want default true")
	}
	if !got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled reset = false, want default true")
	}
}

func TestObservedMarketDataQualityUsesDaemonQuoteReadiness(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	quality := observedMarketDataQuality(&platformSettingsObserved{MarketDataReady: true, ObservedAt: now})
	if quality.Status != "ok" || !strings.Contains(quality.Summary, "successful quote") {
		t.Fatalf("observed market-data quality = %+v, want quote-path confirmation", quality)
	}
	if quality.ObservedAt != now {
		t.Fatalf("observed_at = %s, want %s", quality.ObservedAt, now)
	}

	unknown := observedMarketDataQuality(&platformSettingsObserved{ObservedAt: now})
	if unknown.Status != "unknown" {
		t.Fatalf("unwitnessed market-data quality = %+v, want unknown", unknown)
	}
}

func TestPlatformSettingsRejectsUnknownAndReadOnlyWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeDisabled})

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"bogus":true}`)}); err == nil {
		t.Fatal("unknown top-level field succeeded")
	}
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"mode":"paper"}}`)}); err == nil {
		t.Fatal("read-only trading.mode write succeeded")
	}
	_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"limits":{"max_notional":5000}}}`)})
	if err == nil {
		t.Fatal("trading limit write succeeded while trading disabled")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("limit write error = %v, want read-only", err)
	}
}

func TestPlatformSettingsTradingPatchOriginMatrix(t *testing.T) {
	t.Parallel()
	modes := []string{config.TradingModeDisabled, config.TradingModePaper, config.TradingModeLive}
	origins := []struct {
		name    string
		field   string
		allowed bool
	}{
		{name: "missing"},
		{name: "agent", field: `,"origin":"agent"`},
		{name: "human tty", field: `,"origin":"human-tty"`, allowed: true},
		{name: "paired device", field: `,"origin":"human-paired-device"`},
	}
	for _, mode := range modes {
		for _, origin := range origins {
			t.Run(mode+"/"+origin.name, func(t *testing.T) {
				srv := newPlatformSettingsTestServer(t, config.Trading{Mode: mode})
				params := `{"trading":{"freeze":true}` + origin.field + `}`
				_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(params)})
				if !origin.allowed {
					if err == nil || !strings.Contains(err.Error(), "terminal-only") {
						t.Fatalf("trading patch err=%v, want terminal-only refusal", err)
					}
					if srv.tradingFrozen() || srv.platformSettings.tradingControlGeneration() != 0 {
						t.Fatal("refused origin mutated trading controls")
					}
					return
				}
				if err != nil {
					t.Fatalf("human trading patch: %v", err)
				}
				if !srv.tradingFrozen() || srv.platformSettings.tradingControlGeneration() != 1 {
					t.Fatal("human trading patch did not publish one control generation")
				}
			})
		}
	}

	// Feature toggles remain origin-free because they cannot relax the five
	// broker-write controls protected by the human-only boundary.
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeLive})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":true}},"origin":"agent"}`)}); err != nil {
		t.Fatalf("live agent feature patch err = %v, want success", err)
	}
}

func TestPlatformSettingsPurgeDisabledBlocksPurgeWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"purge_restore":{"enabled":false}}}`)}); err != nil {
		t.Fatalf("disable purge_restore: %v", err)
	}
	blockers := srv.purgeExecuteBlockers(rpc.TradingStatus{Mode: config.TradingModePaper})
	if !hasBlocker(blockers, "purge_restore_disabled") {
		t.Fatalf("purge blockers missing purge_restore_disabled: %#v", blockers)
	}
	preview := srv.purgeRestorePreviewBlockers(rpc.TradingStatus{Mode: config.TradingModePaper})
	if !hasBlocker(preview, "purge_restore_disabled") {
		t.Fatalf("restore preview blockers missing purge_restore_disabled: %#v", preview)
	}
}

func TestPlatformSettingsStockProtectionDisabledBlocksStockTrailProposal(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":false}}}`)}); err != nil {
		t.Fatalf("disable stock_protection: %v", err)
	}
	bid, ask := 100.0, 100.2
	status := protectionPolicyStatus(defaultProtectionPolicy(), rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := trailingStopStockProposal(defaultProtectionPolicy(), status, rpc.PositionView{Symbol: "MSFT", SecType: "STK", Quantity: 10, Bid: &bid, Ask: &ask, Mark: 100.1, Multiplier: 1, Currency: "USD"}, rpc.TradeProposalSourceFingerprints{}, time.Now(), srv.stockProtectionEnabled(), 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "stock_protection_disabled") {
		t.Fatalf("proposal blockers = %+v, want stock_protection_disabled", prop.Blockers)
	}
}

func newPlatformSettingsTestServer(t *testing.T, tr config.Trading) *Server {
	t.Helper()
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatalf("newPlatformSettingsStore: %v", err)
	}
	return &Server{
		cfg:              &config.Resolved{Trading: tr},
		platformSettings: store,
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func hasBlocker(blockers []rpc.TradingBlocker, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

func TestPlatformSettingsTradingFreezeBlocksWritesAllowsCancels(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderWritesEnabled = func() bool { return true }
	srv.orderJournal = newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl"))

	if srv.tradingFrozen() {
		t.Fatal("tradingFrozen default = true, want false")
	}
	got, err := srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet: %v", err)
	}
	if got.Trading.Freeze.Value || got.Trading.Freeze.Access != rpc.SettingsAccessWrite || got.Trading.Freeze.Source != rpc.SettingsSourceRuntime {
		t.Fatalf("freeze setting = %+v, want writable runtime false", got.Trading.Freeze)
	}

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"freeze":true}}`)}); err != nil {
		t.Fatalf("engage freeze: %v", err)
	}
	if !srv.tradingFrozen() {
		t.Fatal("tradingFrozen after freeze=true patch = false, want true")
	}

	status := rpc.TradingStatus{Mode: config.TradingModePaper}
	auth := srv.brokerWriteAuthorization(status)
	if auth.Allowed || !hasBlocker(auth.Blockers, tradingFrozenBlockerCode) {
		t.Fatalf("frozen write authorization = %+v, want trading_frozen blocker", auth)
	}
	cancelAuth := auth.forCancel()
	if hasBlocker(cancelAuth.Blockers, tradingFrozenBlockerCode) {
		t.Fatalf("frozen cancel authorization retained trading_frozen: %+v", cancelAuth)
	}
	if !hasBlocker(srv.purgeExecuteBlockers(status), tradingFrozenBlockerCode) {
		t.Fatal("purge execute blockers missing trading_frozen while frozen")
	}

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"freeze":null}}`)}); err != nil {
		t.Fatalf("reset freeze: %v", err)
	}
	if srv.tradingFrozen() {
		t.Fatal("tradingFrozen after freeze=null reset = true, want false")
	}
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"freeze":"yes"}}`)}); err == nil {
		t.Fatal("non-boolean trading.freeze accepted")
	}

	// The brake is deliberately not gated on tradingLimitWritability: it
	// must engage even while order entry is disabled or misconfigured.
	disabled := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeDisabled})
	if _, err := disabled.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"freeze":true}}`)}); err != nil {
		t.Fatalf("freeze while trading disabled: %v", err)
	}
	if !disabled.tradingFrozen() {
		t.Fatal("freeze did not engage while trading disabled")
	}
}

// TestPlatformSettingsJournalsReportWhatTheDaemonWrites binds the reported
// value of both calibration journals to the writer's own gate. The surface and
// the writers resolved the unset preference independently once, and the two
// defaults drifted apart: settings reported both journals disabled while the
// daemon kept appending decision events. The agreement across every stored
// state is the property; the default itself is only pinned as the value the
// platform-settings design doc states.
func TestPlatformSettingsJournalsReportWhatTheDaemonWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		server func(t *testing.T) *Server
		want   bool
	}{
		{
			name:   "no settings store",
			server: func(*testing.T) *Server { return &Server{cfg: &config.Resolved{}} },
			want:   true,
		},
		{
			name:   "no stored preference",
			server: func(t *testing.T) *Server { return newPlatformSettingsTestServer(t, config.Trading{}) },
			want:   true,
		},
		{
			name:   "stored enabled",
			server: func(t *testing.T) *Server { return journalSettingsServer(t, true) },
			want:   true,
		},
		{
			name:   "stored disabled",
			server: func(t *testing.T) *Server { return journalSettingsServer(t, false) },
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := tc.server(t)
			got, err := srv.handleSettingsGet()
			if err != nil {
				t.Fatalf("handleSettingsGet: %v", err)
			}
			if writes := srv.regimeJournalEnabled(); got.Regime.Journal.Enabled.Value != writes {
				t.Fatalf("regime.journal.enabled reported %t while the daemon journals %t", got.Regime.Journal.Enabled.Value, writes)
			}
			if writes := srv.stressJournalEnabled(); got.Stress.Journal.Enabled.Value != writes {
				t.Fatalf("stress.journal.enabled reported %t while the daemon journals %t", got.Stress.Journal.Enabled.Value, writes)
			}
			if got.Regime.Journal.Enabled.Value != tc.want || got.Stress.Journal.Enabled.Value != tc.want {
				t.Fatalf("journals = regime:%t stress:%t, want %t", got.Regime.Journal.Enabled.Value, got.Stress.Journal.Enabled.Value, tc.want)
			}
		})
	}
}

func journalSettingsServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	srv := newPlatformSettingsTestServer(t, config.Trading{})
	patch := mustRaw(t, map[string]any{
		"regime": map[string]any{"journal": map[string]any{"enabled": enabled}},
		"stress": map[string]any{"journal": map[string]any{"enabled": enabled}},
	})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: patch}); err != nil {
		t.Fatalf("set both journals to %t: %v", enabled, err)
	}
	data := srv.platformSettings.snapshot()
	if data.Regime.Journal.Enabled == nil || data.Stress.Journal.Enabled == nil {
		t.Fatalf("explicit %t did not store a preference: %+v", enabled, data)
	}
	return srv
}

// TestPlatformSettingsStressJournalMigratesStoredCanaryKey pins the upgrade of
// an installed daemon.db across the canary→stress rename. Runtime preferences
// live in daemon.db and survive restarts, so a version-1 document that holds
// canary.journal.enabled=false must keep the journal disabled under the new
// key, the settings surface must report the stored value rather than the
// default, and the next write must persist the document in the new spelling.
func TestPlatformSettingsStressJournalMigratesStoredCanaryKey(t *testing.T) {
	t.Parallel()
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	defer core.Close()

	// Exactly what a pre-rename daemon persisted after `canary settings set
	// canary.journal.enabled=false`.
	legacy := []byte(`{"version":1,"trading_control_generation":0,` +
		`"features":{"purge_restore":{},"stock_protection":{},"rulebook":{}},` +
		`"trading":{},"regime":{"journal":{}},` +
		`"canary":{"journal":{"enabled":false}},"history":{"rotation":{}}}`)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings, JSON: legacy,
	}); err != nil {
		t.Fatalf("seed version-1 settings document: %v", err)
	}

	store := &platformSettingsStore{}
	if err := store.bindCore(t.Context(), core); err != nil {
		t.Fatalf("bindCore over a version-1 document: %v", err)
	}
	data := store.snapshot()
	if data.Version != platformSettingsDocVersion {
		t.Fatalf("upgraded version = %d, want %d", data.Version, platformSettingsDocVersion)
	}
	if data.Stress.Journal.Enabled == nil || *data.Stress.Journal.Enabled {
		t.Fatalf("stored canary.journal.enabled=false did not survive as stress: %+v", data.Stress)
	}
	if stressJournalEnabledFrom(data) {
		t.Fatal("stress journal reads enabled after upgrading a document that disabled it")
	}

	srv := &Server{cfg: &config.Resolved{}, platformSettings: store}
	if out := srv.platformSettingsSnapshot(nil); out.Stress.Journal.Enabled.Value {
		t.Fatalf("settings surface = %+v, want the stored false, not the default true", out.Stress.Journal.Enabled)
	}

	// An unrelated write rewrites the whole document: it must land as version 2
	// under "stress", drop "canary" entirely, and carry the migrated value.
	if _, err := srv.handleSettingsUpdate(t.Context(), &rpc.Request{
		Params: []byte(`{"features":{"rulebook":{"enabled":false}}}`),
	}); err != nil {
		t.Fatalf("settings update after upgrade: %v", err)
	}
	doc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatalf("read persisted settings document: ok=%v err=%v", ok, err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(doc.JSON, &persisted); err != nil {
		t.Fatalf("decode persisted settings document: %v", err)
	}
	if _, stale := persisted["canary"]; stale {
		t.Fatalf("rewritten document still carries the canary key: %s", doc.JSON)
	}
	if string(persisted["version"]) != "2" {
		t.Fatalf("persisted version = %s, want 2", persisted["version"])
	}
	if got := string(persisted["stress"]); got != `{"journal":{"enabled":false}}` {
		t.Fatalf("persisted stress = %s, want the migrated false", got)
	}

	// A restart over the rewritten document reads the same value back.
	reopened := &platformSettingsStore{}
	if err := reopened.bindCore(t.Context(), core); err != nil {
		t.Fatalf("bindCore over the rewritten document: %v", err)
	}
	if stressJournalEnabledFrom(reopened.snapshot()) {
		t.Fatal("stress journal reads enabled after restarting over the rewritten document")
	}
}

// TestPlatformSettingsDocumentUpgradeRules pins the decode contract itself:
// each document version accepts exactly its own spelling and shape. This is a
// safety boundary because accepting a legacy false under the wrong key can
// silently reveal the current true default.
func TestPlatformSettingsDocumentUpgradeRules(t *testing.T) {
	t.Parallel()
	disabled := false
	for _, tc := range []struct {
		name    string
		raw     string
		want    *bool
		wantErr string
	}{
		{name: "unversioned legacy document", raw: `{"canary":{"journal":{"enabled":false}}}`, want: &disabled},
		{name: "version 1 canary", raw: `{"version":1,"canary":{"journal":{"enabled":false}}}`, want: &disabled},
		{name: "version 1 without a preference", raw: `{"version":1}`},
		{name: "version 2 stress", raw: `{"version":2,"stress":{"journal":{"enabled":false}}}`, want: &disabled},
		{name: "version 2 rejects legacy canary", raw: `{"version":2,"canary":{"journal":{"enabled":false}}}`, wantErr: `unknown field "canary"`},
		{name: "version 1 rejects current stress", raw: `{"version":1,"stress":{"journal":{"enabled":false}}}`, wantErr: `unknown field "stress"`},
		{name: "unversioned rejects current stress", raw: `{"stress":{"journal":{"enabled":false}}}`, wantErr: `unknown field "stress"`},
		{name: "version 1 rejects mixed spellings", raw: `{"version":1,"canary":{"journal":{"enabled":false}},"stress":{"journal":{"enabled":true}}}`, wantErr: `unknown field "stress"`},
		{name: "version 2 rejects mixed spellings", raw: `{"version":2,"stress":{"journal":{"enabled":true}},"canary":{"journal":{"enabled":false}}}`, wantErr: `unknown field "canary"`},
		{name: "rejects unknown top-level field", raw: `{"version":2,"surprise":true}`, wantErr: `unknown field "surprise"`},
		{name: "rejects unknown nested field", raw: `{"version":2,"stress":{"journal":{"enabled":false,"surprise":true}}}`, wantErr: `unknown field "surprise"`},
		{name: "rejects trailing JSON", raw: `{"version":2} {"version":2}`, wantErr: "trailing JSON"},
		{name: "rejects explicit null version", raw: `{"version":null}`, wantErr: "version must be an integer"},
		{name: "rejects explicit version zero", raw: `{"version":0}`, wantErr: "unsupported version 0"},
		{name: "unknown version", raw: `{"version":3}`, wantErr: "unsupported version 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := decodePlatformSettings([]byte(tc.raw))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("decode error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if data.Version != platformSettingsDocVersion {
				t.Fatalf("version = %d, want %d", data.Version, platformSettingsDocVersion)
			}
			switch got := data.Stress.Journal.Enabled; {
			case tc.want == nil && got != nil:
				t.Fatalf("stress.journal.enabled = %v, want unset", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("stress.journal.enabled = %v, want %v", got, *tc.want)
			}
		})
	}
}
