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

[rulebook]
terminal_evidence_file = "/tmp/earnings-terminal-evidence.json"

[opportunities]
enabled = false
policy_file = "/tmp/opportunity-policy.toml"
refresh_cadence = "5m"
hot_reload = false
reload_interval = "45s"

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
}

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
