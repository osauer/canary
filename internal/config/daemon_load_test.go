package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedConfig pins every account-identity setting; the broken parts under
// test are appended after it, or spliced into it.
const pinnedConfig = `[gateway]
host = "127.0.0.1"
port = 4002
client_id = 21
account = "DU111"
tls = false

[trading]
mode = "paper"
max_notional = 2500
`

func writeDaemonConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requirePins(t *testing.T, cfg *Config) {
	t.Helper()
	g := cfg.Gateway
	if g.Host != "127.0.0.1" || g.Port == nil || *g.Port != 4002 || g.ClientID == nil || *g.ClientID != 21 ||
		g.Account != "DU111" || g.TLS == nil || *g.TLS || cfg.Trading.Mode != TradingModePaper {
		t.Fatalf("account-identity pins not read: gateway %+v, trading mode %q", g, cfg.Trading.Mode)
	}
}

// A broken non-account part never stops the daemon (owner decision
// 2026-09-26): the pins are read, the broken part is left at Canary's
// default and named, and every readable value elsewhere stays.
func TestLoadForDaemonStartsOnABrokenNonAccountPart(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		issue string
		check func(t *testing.T, cfg *Config)
	}{
		{name: "wrong type in [flex]", body: pinnedConfig + "[flex]\nenabled = \"yes\"\nquery_id = \"123\"\n", issue: "flex.enabled",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Flex.Enabled || cfg.Flex.QueryID != "123" {
					t.Fatalf("flex = %+v, want enabled at its default and query_id kept", cfg.Flex)
				}
			}},
		{name: "syntax error in [spx]", body: pinnedConfig + "[spx]\nmembers_auto_refresh = tru\n\n[flex]\nenabled = true\n", issue: "[spx]",
			check: func(t *testing.T, cfg *Config) {
				if cfg.SPX.MembersAutoRefresh != nil || !cfg.Flex.Enabled {
					t.Fatalf("spx %+v / flex %+v, want spx at its default and flex read", cfg.SPX, cfg.Flex)
				}
			}},
		{name: "syntax error before the pins", body: "[daemon]\nidle_timeout = \"1h\n\n" + pinnedConfig, issue: "[daemon]"},
		{name: "unknown key in [daemon]", body: pinnedConfig + "[daemon]\nidle_timout = \"1h\"\n", issue: "daemon.idle_timout"},
		{name: "unusable logging value", body: pinnedConfig + "[daemon]\nlog_calendar_mode = \"loud\"\nlog_level = \"info\"\n", issue: "daemon.log_calendar_mode",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Daemon.LogCalendarMode != "" || cfg.Daemon.LogLevel != "info" {
					t.Fatalf("daemon = %+v, want the calendar mode reset and log_level kept", cfg.Daemon)
				}
				if _, err := cfg.Resolve(); err != nil {
					t.Fatalf("Resolve after the reset: %v", err)
				}
			}},
		{name: "order limit of the wrong type", body: strings.Replace(pinnedConfig, "max_notional = 2500", "max_notional = \"2.5k\"", 1), issue: "trading.max_notional",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Trading.MaxNotional != 0 {
					t.Fatalf("max_notional = %v, want the default", cfg.Trading.MaxNotional)
				}
			}},
		{name: "maintenance windows of the wrong type", body: strings.Replace(pinnedConfig, "tls = false", "tls = false\nmaintenance_windows = 3", 1), issue: "gateway.maintenance_windows"},
		{name: "unknown section", body: pinnedConfig + "[extras]\ncolour = \"blue\"\n", issue: "extras"},
		{name: "retired automation key", body: pinnedConfig + "[auto_trade]\nauto_submit = true\n", issue: "auto_trade.auto_submit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, issues, err := LoadForDaemon(writeDaemonConfig(t, tc.body))
			if err != nil {
				t.Fatalf("a broken non-account part stopped the daemon: %v", err)
			}
			requirePins(t, cfg)
			if cfg.Trading.MaxNotional != 2500 && tc.issue != "trading.max_notional" {
				t.Fatalf("max_notional = %v, want 2500 kept", cfg.Trading.MaxNotional)
			}
			found := false
			for _, issue := range issues {
				if issue.Key == tc.issue && issue.Problem != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("issues = %+v, want one on %s", issues, tc.issue)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// When the account-identity pins cannot be read, the daemon stops: it cannot
// tell which broker account to act on.
func TestLoadForDaemonStopsWhenAnAccountPinCannotBeRead(t *testing.T) {
	cases := map[string]string{
		"port of the wrong type":           strings.Replace(pinnedConfig, "port = 4002", "port = \"4002\"", 1),
		"account of the wrong type":        strings.Replace(pinnedConfig, `account = "DU111"`, "account = 111", 1),
		"paper/live pin of the wrong type": strings.Replace(pinnedConfig, `mode = "paper"`, "mode = 1", 1),
		"misspelled account pin":           strings.Replace(pinnedConfig, "account =", "acount =", 1),
		"syntax error inside [gateway]":    strings.Replace(pinnedConfig, "port = 4002", "port = 40 02", 1),
		"syntax error at the top":          "stray\n" + pinnedConfig,
		"pins under a legacy profile":      "[profiles.live]\nhost = \"127.0.0.1\"\nport = 4001\n",
		"pin in the wrong section":         pinnedConfig + "[daemon]\naccount = \"DU222\"\n",
		"retired live acknowledgement":     strings.Replace(pinnedConfig, `mode = "paper"`, "mode = \"paper\"\nallow_live = true", 1),
		"string swallowing [trading]":      "[spx]\nnote = \"\"\"never closed\n" + pinnedConfig,
		"[gateway] twice":                  pinnedConfig + "[gateway]\nport = 4001\n",
		"[gateway] as an array":            "[[gateway]]\nport = 4002\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := LoadForDaemon(writeDaemonConfig(t, body))
			var identity *IdentityError
			if !errors.As(err, &identity) || !strings.Contains(err.Error(), "troubleshooting") {
				t.Fatalf("err = %v, want an IdentityError pointing at the troubleshooting doc", err)
			}
		})
	}
	unreadable := writeDaemonConfig(t, pinnedConfig)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if _, _, err := LoadForDaemon(unreadable); err == nil {
			t.Fatal("an unreadable file must stop the daemon")
		}
	}
}

// A file the strict loader accepts reads exactly as before, and a missing
// file stays fully automatic.
func TestLoadForDaemonReadsACleanFileAsLoadDoes(t *testing.T) {
	path := writeDaemonConfig(t, pinnedConfig+"[flex]\nenabled = true\nquery_id = \"123\"\n")
	strict, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues, err := LoadForDaemon(path)
	if err != nil || len(issues) != 0 {
		t.Fatalf("clean file: err %v issues %+v", err, issues)
	}
	requirePins(t, cfg)
	if cfg.Flex != strict.Flex || cfg.Trading != strict.Trading {
		t.Fatalf("daemon load %+v differs from Load %+v", cfg, strict)
	}
	if cfg, issues, err := LoadForDaemon(filepath.Join(t.TempDir(), "missing.toml")); err != nil || len(issues) != 0 || cfg == nil {
		t.Fatalf("missing file: %v %+v %v", cfg, issues, err)
	}
}
