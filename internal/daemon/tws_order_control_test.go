package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
)

func newTWSOrderControlSettingsServer(t *testing.T, mode string) *Server {
	t.Helper()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: mode})
	return srv
}

func enableTWSOrderControl(t *testing.T, srv *Server, origin string) error {
	t.Helper()
	patch := map[string]json.RawMessage{"trading": json.RawMessage(`{"tws_order_control":{"enabled":true}}`)}
	return srv.applyPlatformSettingsPatch(context.Background(), patch, origin)
}

// The switch is off by default, terminal-only like the freeze, and a change
// advances the trading-control generation so an admission in flight sees it.
func TestTWSOrderControlSettingIsOffAndTerminalOnly(t *testing.T) {
	srv := newTWSOrderControlSettingsServer(t, config.TradingModePaper)
	if got := srv.twsOrderControlAvailability(rpc.AccountModePaper); got.Enabled || got.Available || got.Status != rpc.TWSOrderControlStatusOff {
		t.Fatalf("default availability = %+v, want off", got)
	}
	if err := enableTWSOrderControl(t, srv, rpc.OrderOriginPairedDevice); err == nil || !strings.Contains(err.Error(), "terminal-only") {
		t.Fatalf("paired-device enable err = %v, want the terminal-only refusal", err)
	}
	before := srv.platformSettings.tradingControlGeneration()
	if err := enableTWSOrderControl(t, srv, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if after := srv.platformSettings.tradingControlGeneration(); after != before+1 {
		t.Fatalf("trading control generation %d -> %d, want one step", before, after)
	}
	if got := srv.twsOrderControlAvailability(rpc.AccountModePaper); !got.Enabled || !got.Available || got.Status != rpc.TWSOrderControlStatusAvailable {
		t.Fatalf("paper availability after enable = %+v, want available", got)
	}
	patch := map[string]json.RawMessage{"trading": json.RawMessage(`{"tws_order_control":{"paper_proven_at":"2026-10-11T19:36:00Z"}}`)}
	if err := srv.applyPlatformSettingsPatch(context.Background(), patch, rpc.OrderOriginHumanTTY); err == nil {
		t.Fatal("a patch set paper_proven_at; it is the daemon's to record")
	}
}

// Live waits for the paper proof; the proof is recorded once and kept.
func TestTWSOrderControlLiveNeedsThePaperProof(t *testing.T) {
	srv := newTWSOrderControlSettingsServer(t, config.TradingModeLive)
	if err := enableTWSOrderControl(t, srv, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got := srv.twsOrderControlAvailability(rpc.AccountModeLive)
	if got.Available || (orderWritesAvailable && got.Status != rpc.TWSOrderControlStatusProofRequired) {
		t.Fatalf("live availability before the proof = %+v, want proof required", got)
	}
	first, err := srv.recordTWSOrderControlPaperProof(context.Background(), rpc.OrderOriginPairedDevice)
	if err != nil || first.IsZero() {
		t.Fatalf("record proof: at=%v err=%v", first, err)
	}
	srv.now = func() time.Time { return first.Add(time.Hour) }
	again, err := srv.recordTWSOrderControlPaperProof(context.Background(), rpc.OrderOriginPairedDevice)
	if err != nil || !again.Equal(first) {
		t.Fatalf("second proof = %v err=%v, want the first %v kept", again, err, first)
	}
	got = srv.twsOrderControlAvailability(rpc.AccountModeLive)
	if orderWritesAvailable {
		if !got.Available || got.Status != rpc.TWSOrderControlStatusAvailable || !got.PaperProvenAt.Equal(first) {
			t.Fatalf("live availability after the proof = %+v, want available", got)
		}
	} else if got.Available || !strings.Contains(got.Status, "build") {
		t.Fatalf("live availability in the read-only build = %+v, want unavailable for the build", got)
	}
	settings := srv.twsOrderControlSettings(rpc.AccountModeLive)
	if !settings.Enabled.Value || settings.PaperProvenAt.Value != first.Format(time.RFC3339) || settings.Status.Value != got.Status {
		t.Fatalf("settings projection = %+v, want enabled with the proof and status %q", settings, got.Status)
	}
	if got := srv.twsOrderControlAvailability("disabled"); got.Available || !strings.Contains(got.Status, "trading mode") {
		t.Fatalf("availability with trading disabled = %+v, want unavailable", got)
	}
}

// Untracked rows gain eligibility only from the path: every working row
// with a permanent id may be cancelled, a DAY limit stock order changed.
func TestTWSOrderControlLendsEligibilityToUntrackedRows(t *testing.T) {
	rows := func() []rpc.OrderView {
		return []rpc.OrderView{
			{PermID: 31, Open: true, LifecycleStatus: rpc.OrderLifecycleSubmitted, SecType: "STK", OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 100, LimitPrice: 150},
			{PermID: 32, Open: true, LifecycleStatus: rpc.OrderLifecycleSubmitted, SecType: "STK", OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Quantity: 100},
			{PermID: 33, Open: true, LifecycleStatus: rpc.OrderLifecycleSubmitted, SecType: "STK", OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFGTC, Quantity: 100, LimitPrice: 150},
			{PermID: 0, Open: true, LifecycleStatus: rpc.OrderLifecycleSubmitted, SecType: "STK", OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 100, LimitPrice: 150},
			{PermID: 34, Open: true, LifecycleStatus: rpc.OrderLifecycleSubmitted, SecType: "OPT", OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 1, LimitPrice: 2},
		}
	}
	srv := newTWSOrderControlSettingsServer(t, config.TradingModePaper)
	off := rows()
	srv.markTWSOrderControlEligibility(off, rpc.AccountModePaper)
	for _, row := range off {
		if row.CancelEligible || row.ModifyEligible {
			t.Fatalf("row %d eligible while the control is off: %+v", row.PermID, row)
		}
	}
	if err := enableTWSOrderControl(t, srv, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("enable: %v", err)
	}
	on := rows()
	srv.markTWSOrderControlEligibility(on, rpc.AccountModePaper)
	want := map[int][2]bool{31: {true, true}, 32: {true, false}, 33: {true, false}, 0: {false, false}, 34: {true, false}}
	for _, row := range on {
		if got := [2]bool{row.CancelEligible, row.ModifyEligible}; got != want[row.PermID] {
			t.Fatalf("row %d eligibility (cancel, modify) = %v, want %v", row.PermID, got, want[row.PermID])
		}
	}
}

// A version-4 document upgrades to version 5 with the control off; a
// version-4 document that already carries the control is refused, so a
// rollback never reads a newer preference as an old one.
func TestTWSOrderControlSettingsDocumentUpgradesStrictly(t *testing.T) {
	v4 := `{"version":4,"trading_control_generation":9,"display":{},"cash_sweep":{},"features":{},"trading":{"freeze":true},"regime":{},"stress":{},"history":{}}`
	data, err := decodePlatformSettings([]byte(v4))
	if err != nil {
		t.Fatalf("decode v4: %v", err)
	}
	if data.Version != platformSettingsDocVersion || data.TradingControlGeneration != 9 || data.Trading.Freeze == nil || !*data.Trading.Freeze || data.Trading.TWSOrderControl.Enabled != nil || data.Trading.TWSOrderControl.PaperProvenAt != nil {
		t.Fatalf("upgraded v4 = %+v, want v%d with the freeze kept and the control unset", data, platformSettingsDocVersion)
	}
	leaked := strings.Replace(v4, `"freeze":true`, `"freeze":true,"tws_order_control":{"enabled":true}`, 1)
	if _, err := decodePlatformSettings([]byte(leaked)); err == nil {
		t.Fatal("a v4 document carrying tws_order_control decoded")
	}
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 11, 17, 36, 0, 0, time.UTC)
	if err := store.updateWithAudit(context.Background(), at, rpc.OrderOriginHumanTTY, []string{"trading.tws_order_control.enabled", "trading.tws_order_control.paper_proven_at"}, func(next *platformSettingsData) error {
		next.Trading.TWSOrderControl.Enabled = new(true)
		next.Trading.TWSOrderControl.PaperProvenAt = &at
		return nil
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := newPlatformSettingsStore(store.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reloaded.snapshot().Trading.TWSOrderControl
	if got.Enabled == nil || !*got.Enabled || got.PaperProvenAt == nil || !got.PaperProvenAt.Equal(at) {
		t.Fatalf("reloaded control = %+v, want enabled with the proof at %v", got, at)
	}
}
