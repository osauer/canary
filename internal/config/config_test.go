package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_MissingFileGivesFullAuto(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "no-such.toml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Gateway.PortPinned() {
		t.Errorf("Port should be unpinned in zero config, got %v", *res.Gateway.Port)
	}
	if res.Gateway.TLSPinned() {
		t.Errorf("TLS should be unpinned in zero config, got %v", *res.Gateway.TLS)
	}
	if res.Gateway.HostOrDefault() != "127.0.0.1" {
		t.Errorf("HostOrDefault = %q, want 127.0.0.1", res.Gateway.HostOrDefault())
	}
	if res.Gateway.ClientIDOrDefault() != 15 {
		t.Errorf("ClientIDOrDefault = %d, want 15", res.Gateway.ClientIDOrDefault())
	}
	if res.Gateway.BreadthClientIDOrDefault() != 16 {
		t.Errorf("BreadthClientIDOrDefault = %d, want 16", res.Gateway.BreadthClientIDOrDefault())
	}
	if res.Daemon.IdleTimeout.Std() != 15*time.Minute {
		t.Errorf("default idle = %v, want 15m", res.Daemon.IdleTimeout.Std())
	}
	if res.Daemon.LogLevel != "info" {
		t.Errorf("default log_level = %q, want info", res.Daemon.LogLevel)
	}
	if res.Trading.Mode != TradingModeDisabled {
		t.Errorf("trading mode = %q, want %q", res.Trading.Mode, TradingModeDisabled)
	}
	if res.Trading.MaxNotional != 10000 {
		t.Errorf("trading max_notional = %v, want 10000", res.Trading.MaxNotional)
	}
	if res.Trading.MaxOptionContracts != 5 {
		t.Errorf("trading max_option_contracts = %d, want 5", res.Trading.MaxOptionContracts)
	}
	if res.Trading.PaperSmokeMaxAgeDuration() != 168*time.Hour {
		t.Errorf("trading paper_smoke_max_age = %v, want 168h", res.Trading.PaperSmokeMaxAgeDuration())
	}
	if !res.AutoTrade.ProposalsEnabledResolved() {
		t.Error("manual proposals should default enabled")
	}
	if !res.AutoTrade.FastPathEnabledResolved() {
		t.Error("manual fast path should default enabled")
	}
	if res.AutoTrade.ReloadIntervalDuration() != 30*time.Second {
		t.Errorf("auto_trade reload_interval = %v, want 30s", res.AutoTrade.ReloadIntervalDuration())
	}
	if res.AutoTrade.ProposalCadenceDuration() != 30*time.Second {
		t.Errorf("auto_trade proposal_cadence = %v, want 30s", res.AutoTrade.ProposalCadenceDuration())
	}
	if !res.Opportunities.EnabledResolved() {
		t.Error("opportunities should default enabled")
	}
	if res.Opportunities.PolicyFile != "~/.config/ibkr/policies/opportunity-policy.toml" {
		t.Errorf("opportunities policy_file = %q, want default opportunity-policy path", res.Opportunities.PolicyFile)
	}
	if !res.Opportunities.HotReloadEnabled() {
		t.Error("opportunity hot_reload should default enabled")
	}
	if res.Opportunities.ReloadIntervalDuration() != 30*time.Second {
		t.Errorf("opportunities reload_interval = %v, want 30s", res.Opportunities.ReloadIntervalDuration())
	}
	if res.Opportunities.RefreshCadenceDuration() != 2*time.Minute {
		t.Errorf("opportunities refresh_cadence = %v, want 2m", res.Opportunities.RefreshCadenceDuration())
	}
	if _, ok := res.Scans["top-movers"]; !ok {
		t.Errorf("top-movers preset missing from defaults")
	}
}

func TestLoad_PinnedFieldsAreBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `[gateway]
host               = "127.0.0.1"
port               = 4002
client_id          = 16
breadth_client_id  = 17
account            = "DU111"
tls                = false

[daemon]
idle_timeout = "10m"
log_level    = "debug"

[trading]
mode = "live"
max_notional = 25000
max_option_contracts = 3
allow_stock_short = true
allow_option_sell_to_open = true
paper_smoke_max_age = "24h"

[rulebook]
terminal_evidence_file = "/tmp/earnings-terminal-evidence.json"

[opportunities]
enabled = false
policy_file = "/tmp/opportunity-policy.toml"
refresh_cadence = "5m"
hot_reload = false
reload_interval = "45s"

[scans.movers]
type       = "TOP_PERC_GAIN"
exchange   = "STK.EU.IBIS"
instrument = "STOCK.EU"
limit      = 10
timeout    = "30s"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Gateway.PortPinned() || *res.Gateway.Port != 4002 {
		t.Errorf("Port should be pinned to 4002, got %+v", res.Gateway.Port)
	}
	if !res.Gateway.TLSPinned() || *res.Gateway.TLS != false {
		t.Errorf("TLS should be pinned to false, got %+v", res.Gateway.TLS)
	}
	if res.Gateway.ClientIDOrDefault() != 16 {
		t.Errorf("ClientID = %d, want 16", res.Gateway.ClientIDOrDefault())
	}
	if res.Gateway.BreadthClientIDOrDefault() != 17 {
		t.Errorf("BreadthClientID = %d, want 17 (pinned via TOML)", res.Gateway.BreadthClientIDOrDefault())
	}
	if res.Gateway.Account != "DU111" {
		t.Errorf("Account = %q, want DU111", res.Gateway.Account)
	}
	if res.Daemon.IdleTimeout.Std() != 10*time.Minute {
		t.Errorf("idle = %v, want 10m", res.Daemon.IdleTimeout.Std())
	}
	if res.Daemon.LogLevel != "debug" {
		t.Errorf("log_level = %q, want debug", res.Daemon.LogLevel)
	}
	if res.Trading.Mode != TradingModeLive {
		t.Errorf("Trading.Mode = %q, want %q", res.Trading.Mode, TradingModeLive)
	}
	if res.Trading.MaxNotional != 25000 {
		t.Errorf("Trading.MaxNotional = %v, want 25000", res.Trading.MaxNotional)
	}
	if res.Trading.MaxOptionContracts != 3 {
		t.Errorf("Trading.MaxOptionContracts = %d, want 3", res.Trading.MaxOptionContracts)
	}
	if !res.Trading.AllowStockShort {
		t.Error("Trading.AllowStockShort should parse true")
	}
	if !res.Trading.AllowOptionSellToOpen {
		t.Error("Trading.AllowOptionSellToOpen should parse true")
	}
	if res.Trading.PaperSmokeMaxAgeDuration() != 24*time.Hour {
		t.Errorf("Trading.PaperSmokeMaxAge = %v, want 24h", res.Trading.PaperSmokeMaxAgeDuration())
	}
	if res.Rulebook.TerminalEvidenceFile != "/tmp/earnings-terminal-evidence.json" {
		t.Errorf("Rulebook.TerminalEvidenceFile = %q", res.Rulebook.TerminalEvidenceFile)
	}
	if res.Opportunities.EnabledResolved() {
		t.Error("Opportunities.Enabled should parse false")
	}
	if res.Opportunities.PolicyFile != "/tmp/opportunity-policy.toml" {
		t.Errorf("Opportunities.PolicyFile = %q", res.Opportunities.PolicyFile)
	}
	if res.Opportunities.RefreshCadenceDuration() != 5*time.Minute {
		t.Errorf("Opportunities.RefreshCadence = %v, want 5m", res.Opportunities.RefreshCadenceDuration())
	}
	if res.Opportunities.HotReloadEnabled() {
		t.Error("Opportunities.HotReload should parse false")
	}
	if res.Opportunities.ReloadIntervalDuration() != 45*time.Second {
		t.Errorf("Opportunities.ReloadInterval = %v, want 45s", res.Opportunities.ReloadIntervalDuration())
	}
	got, ok := res.Scans["movers"]
	if !ok {
		t.Fatalf("scans[movers] missing")
	}
	if got.Limit != 10 {
		t.Errorf("scans[movers].Limit = %d, want 10", got.Limit)
	}
	if got.Instrument != "STOCK.EU" {
		t.Errorf("scans[movers].Instrument = %q, want STOCK.EU", got.Instrument)
	}
	if got.Timeout.Std() != 30*time.Second {
		t.Errorf("scans[movers].Timeout = %v, want 30s", got.Timeout.Std())
	}
}

// TestLoad_PartialConfig_AutoForOmittedFields proves that omitting a field
// leaves it nil (= auto), even when sibling fields are pinned.
func TestLoad_PartialConfig_AutoForOmittedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `[gateway]
client_id = 42
# port and tls intentionally omitted — should remain auto
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Gateway.PortPinned() {
		t.Errorf("Port should remain auto when omitted, got pinned %v", *res.Gateway.Port)
	}
	if res.Gateway.TLSPinned() {
		t.Errorf("TLS should remain auto when omitted, got pinned %v", *res.Gateway.TLS)
	}
	if res.Gateway.ClientIDOrDefault() != 42 {
		t.Errorf("ClientID = %d, want 42", res.Gateway.ClientIDOrDefault())
	}
}

// TestLoad_UnknownKeys_Rejected guards the silent-acceptance regression we
// hit shipping v0.8.0: a `[profiles.live]` config (from an older proposal)
// parsed cleanly because BurntSushi/toml drops unknown keys by default,
// leaving every Gateway field nil and silently demoting the daemon to AUTO
// discovery with default client_id=15. After this fix, Load must reject
// unknown keys and the error must name them so the user can find them.
func TestLoad_UnknownKeys_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `default_profile = "live"

[profiles.live]
host      = "127.0.0.1"
port      = 4001
client_id = 15
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown TOML keys, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"unknown key", "default_profile", "profiles"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
	if !strings.Contains(msg, "[trading]") {
		t.Errorf("error %q must mention supported [trading] schema", msg)
	}
}

// TestLoad_RemovedLiveAckKeys_TargetedError guards the live-gate
// simplification: a leftover allow_live / live_ack_* key must fail load with
// a "removed, delete it" message, not the generic unknown-key error — a
// failed load kills the autospawned daemon and with it every CLI command.
func TestLoad_RemovedLiveAckKeys_TargetedError(t *testing.T) {
	for _, key := range []string{"allow_live = true", `live_ack_account = "DU111"`, `live_ack_endpoint = "127.0.0.1:7497"`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		body := "[trading]\nmode = \"paper\"\n" + key + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatalf("expected removed-key error for %q, got nil", key)
		}
		msg := err.Error()
		for _, want := range []string{"was removed", "delete this key"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q for %q must mention %q", msg, key, want)
			}
		}
		if strings.Contains(msg, "unknown key") {
			t.Errorf("error %q for %q must use the targeted message, not the generic unknown-key one", msg, key)
		}
	}
}

func TestLoad_RemovedNoOpKnobs_TargetedError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"option market orders", "[trading]\nallow_option_market_orders = true\n"},
		{"mcp enabled", "[trading]\nmcp_enabled = true\n"},
		{"mcp mode", "[trading]\nmcp_mode = \"paper-write\"\n"},
		{"mcp nonce ttl", "[trading]\nmcp_nonce_ttl = \"2m\"\n"},
		{"auto trade enabled", "[auto_trade]\nenabled = true\n"},
		{"auto submit", "[auto_trade]\nauto_submit = true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected removed-key error, got nil")
			}
			msg := err.Error()
			for _, want := range []string{"was removed", "delete this key"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q must mention %q", msg, want)
				}
			}
			if strings.Contains(msg, "unknown key") {
				t.Errorf("error %q must use the targeted message, not the generic unknown-key one", msg)
			}
		})
	}
}

// A non-removed unknown [trading] key must still get the generic message —
// the removed-key special case must not widen.
func TestLoad_UnknownTradingKey_StillGeneric(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[trading]\nmode = \"paper\"\nnot_a_real_knob = true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown TOML key, got nil")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error %q must use the generic unknown-key message", err.Error())
	}
}

// TestLoad_TLSPinnedFalse_StaysFalse guards the #3 fix: tls=false must be
// distinguishable from "tls absent." The pointer encoding is the mechanism.
func TestLoad_TLSPinnedFalse_StaysFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `[gateway]
tls = false
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Gateway.TLSPinned() {
		t.Fatal("tls=false should register as pinned")
	}
	if *res.Gateway.TLS != false {
		t.Errorf("TLS = %v, want false", *res.Gateway.TLS)
	}
}

// TestSPX_MembersAutoRefreshDefaultsToTrue: absent [spx] block →
// MembersAutoRefreshEnabled() = true. Documents the opt-out posture.
func TestSPX_MembersAutoRefreshDefaultsToTrue(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "no-such.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.SPX.MembersAutoRefreshEnabled() {
		t.Error("MembersAutoRefreshEnabled should default to true when [spx] block absent")
	}
}

// TestSPX_MembersAutoRefreshExplicitFalse: pinned TOML disables the
// refresher. Status renderer surfaces this as "disabled (config)".
func TestSPX_MembersAutoRefreshExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `[spx]
members_auto_refresh = false
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.SPX.MembersAutoRefreshEnabled() {
		t.Error("members_auto_refresh=false should disable the refresher")
	}
}

// TestSPX_MembersAutoRefreshExplicitTrue: pinned TOML = true matches
// the default but the pointer-typed field carries the "user opted in"
// signal for any future feature that distinguishes the two.
func TestSPX_MembersAutoRefreshExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `[spx]
members_auto_refresh = true
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.SPX.MembersAutoRefreshEnabled() {
		t.Error("members_auto_refresh=true should enable the refresher")
	}
	if res.SPX.MembersAutoRefresh == nil || !*res.SPX.MembersAutoRefresh {
		t.Error("explicit true should round-trip as non-nil pointer")
	}
}

// TestSPXMembersAutoRefreshFromEnv covers the env-override precedence.
// Symmetric semantics: IBKR_SPX_MEMBERS_AUTO_REFRESH=1 force-enables,
// =0 force-disables, unset / anything else defers to TOML.
func TestSPXMembersAutoRefreshFromEnv(t *testing.T) {
	cases := []struct {
		name        string
		set         bool
		value       string
		wantEnabled bool
		wantForced  bool
	}{
		{"unset", false, "", false, false},
		{"explicit zero", true, "0", false, true},
		{"explicit one", true, "1", true, true},
		{"garbage", true, "yes", false, false},
		{"empty string set", true, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("IBKR_SPX_MEMBERS_AUTO_REFRESH", c.value)
			} else {
				_ = os.Unsetenv("IBKR_SPX_MEMBERS_AUTO_REFRESH")
			}
			enabled, forced := SPXMembersAutoRefreshFromEnv()
			if enabled != c.wantEnabled {
				t.Errorf("enabled: want %v got %v", c.wantEnabled, enabled)
			}
			if forced != c.wantForced {
				t.Errorf("forced: want %v got %v", c.wantForced, forced)
			}
		})
	}
}

// TestLoad_ShippedTemplatesLoad gates template drift: every example config
// template in examples/ must pass the strict loader, because the documented
// activation path (docs/docs/operate/orders.md) is "rename it to
// config.toml" — a template with an unknown key kills the daemon and every
// CLI command on first use.
func TestLoad_ShippedTemplatesLoad(t *testing.T) {
	templates, err := filepath.Glob(filepath.Join("..", "..", "examples", "config.toml*"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no config.toml* templates found under examples/ — glob path is stale")
	}
	for _, path := range templates {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("shipped template must pass the strict loader: %v", err)
			}
			if _, err := cfg.Resolve(); err != nil {
				t.Fatalf("shipped template must resolve: %v", err)
			}
		})
	}
}
