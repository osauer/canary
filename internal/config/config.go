// Package config loads and validates operator-owned TOML configuration for
// gateway identity, daemon behavior, and capability enablement.
//
// A missing file or absent gateway field leaves that dimension discoverable;
// an explicitly configured value is binding. Runtime platform preferences are
// separate daemon.db state and are not read or written by this package.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Gateway holds the four pinnable connection knobs. Pointer fields
// auto-discovery only probes loopback. A non-loopback host implies "I know
// Account is plain string because empty already means "auto-detect via
// managedAccounts" in the SDK.
type Gateway struct {
	// Host pins the IB Gateway / TWS host; empty (the default) defers to auto-discovery on loopback (127.0.0.1), any non-empty value skips probing.
	Host string `toml:"host"`
	// Port pins the IB Gateway / TWS API port (typically 4001/4002 for IB Gateway live/paper, 7496/7497 for TWS live/paper); absent (nil) defers to port-probing during discovery.
	Port *int `toml:"port"`
	// ClientID pins the IBKR API clientID for the primary connection (default 15); collisions are treated as a stale-client/operator issue and are not auto-walked to neighboring reserved IDs.
	ClientID *int `toml:"client_id"`
	// BreadthClientID is the IBKR clientID used by the dedicated
	// historical-bar connector that backs the SPX breadth refresh.
	BreadthClientID *int `toml:"breadth_client_id"`
	// Account pins the IBKR account ID like "U1234567"; empty (default) defers to the gateway's managedAccounts list — fine for single-account logins, required disambiguator when the login carries multiple accounts.
	Account string `toml:"account"`
	// TLS pins TLS mode for the API socket: absent (nil) auto-tries plain first then TLS, `true` forces TLS-only with no plain fallback, `false` forces plain — setting the field disables fallback in either direction.
	TLS *bool `toml:"tls"`
	// MaintenanceWindows lists the broker's scheduled reset windows as "DAY[-DAY] HH:MM-HH:MM TZ" specs, e.g. "Sat-Thu 23:45-00:45 America/New_York". Backend-link losses inside a window are annotated as expected maintenance rather than incident evidence. Absent (nil) uses IBKR's documented North America schedule; an explicit empty list disables the annotation.
	MaintenanceWindows []string `toml:"maintenance_windows"`
}

// PortPinned reports whether the user pinned a port. Discovery skips the
// port-probe step when true and uses Port directly.
func (g Gateway) PortPinned() bool { return g.Port != nil }

// TLSPinned reports whether the user pinned a TLS mode. The SDK's
func (g Gateway) TLSPinned() bool { return g.TLS != nil }

// HostOrDefault returns Host if set, else 127.0.0.1.
func (g Gateway) HostOrDefault() string {
	if g.Host == "" {
		return "127.0.0.1"
	}
	return g.Host
}

// ClientIDOrDefault returns ClientID if pinned, else 15.
func (g Gateway) ClientIDOrDefault() int {
	if g.ClientID == nil {
		return 15
	}
	return *g.ClientID
}

// BreadthClientIDOrDefault returns the clientID for the bulk-historical
func (g Gateway) BreadthClientIDOrDefault() int {
	if g.BreadthClientID == nil {
		return 16
	}
	return *g.BreadthClientID
}

// PortOrZero returns Port (dereferenced) or 0 if unset. Callers should
// check PortPinned first; the zero is a sentinel for "discover."
func (g Gateway) PortOrZero() int {
	if g.Port == nil {
		return 0
	}
	return *g.Port
}

// TLSOrFalse returns TLS (dereferenced) or false if unset. Callers should
// check TLSPinned first; the false is a sentinel meaning "auto, try plain
// first" — distinct from a binding tls=false.
func (g Gateway) TLSOrFalse() bool {
	if g.TLS == nil {
		return false
	}
	return *g.TLS
}

// Daemon holds runtime knobs for the daemon process.
type Daemon struct {
	// IdleTimeout is how long the auto-spawned daemon stays alive between CLI calls (default 15m, accepts any Go duration string like "1h" or "0s"); set "0s" to disable idle-shutdown when running long cold-start jobs such as the first breadth fan-out under `canary daemon --foreground`.
	IdleTimeout duration `toml:"idle_timeout"`
	// LogLevel is the daemon's log verbosity — one of "debug", "info", "warn" (default), or "error". The warn default keeps the log to actionable lines; set "info" to trace routine broker traffic.
	LogLevel string `toml:"log_level"`
}

// Trading holds local order-entry gates for experimental trading builds.
// Stable ibkr releases are read-only; a missing [trading] section resolves to
// override. TWS / Gateway broker permissions remain the final authority even
type Trading struct {
	// Mode selects the local order-entry state: "disabled" (default), "paper", or "live".
	Mode string `toml:"mode"`
	// MaxNotional caps every equity/ETF order before broker WhatIf; apparent close/reduce orders are not exempt because this client cannot prove that a manual TWS order has not already consumed the exit capacity. Default 10000 in account currency.
	MaxNotional float64 `toml:"max_notional"`
	// MaxOptionContracts caps every single-leg option order; apparent close/reduce orders are not exempt because account-global working-order authority is incomplete. Default 5.
	MaxOptionContracts int `toml:"max_option_contracts"`
	// AllowStockShort permits stock short/opening flip previews when true. Default false.
	AllowStockShort bool `toml:"allow_stock_short"`
	// AllowOptionSellToOpen permits option sell-to-open previews when true. Default false.
	AllowOptionSellToOpen bool `toml:"allow_option_sell_to_open"`
}

// Rulebook configures operator-owned evidence inputs for the advisory trading
// live authority.
type Rulebook struct {
	// TerminalEvidenceFile points to an optional JSON document of reviewed,
	// exact-contract terminal/non-reporting issuer evidence for rules 6-8. An
	// empty path leaves the retained daemon.db authority unchanged.
	TerminalEvidenceFile string `toml:"terminal_evidence_file"`
}

// AutoTrade configures advisory protection-proposal production and policy
// automatic broker submission.
type AutoTrade struct {
	// ProposalsEnabled controls whether the daemon may produce advisory protection proposals; default true, and proposals are not broker orders unless separately submitted by an explicitly enabled trading path — the `[auto_trade]` section name is historical: nothing auto-trades, and the policy's auto_submit stays false.
	ProposalsEnabled *bool `toml:"proposals_enabled"`
	// PolicyFile points to the local protection-policy TOML; default ~/.config/ibkr/policies/protection-policy.toml.
	PolicyFile string `toml:"policy_file"`
	// HotReload controls whether policy changes are reloaded while the daemon runs; default true.
	HotReload *bool `toml:"hot_reload"`
	// ReloadInterval controls how often the daemon checks policy-file changes; default 30s.
	ReloadInterval duration `toml:"reload_interval"`
	// ProposalCadence controls how often the daemon refreshes protection proposals; default 30s.
	ProposalCadence duration `toml:"proposal_cadence"`
	// FastPathEnabled allows manual proposal preview/submit to use the immediate
	// revalidation path; default true so paper protection stops remain usable.
	// Trading write gates still own broker-submit authority.
	FastPathEnabled *bool `toml:"fast_path_enabled"`
}

// Opportunities configures advisory opportunity production and policy
// reloads. Broker submission remains subject to the independent trading path.
type Opportunities struct {
	// Enabled controls whether the daemon may produce advisory opportunities; default true, and opportunities are not broker writes unless separately submitted by an explicitly enabled trading path.
	Enabled *bool `toml:"enabled"`
	// PolicyFile points to the local opportunity-policy TOML; default ~/.config/ibkr/policies/opportunity-policy.toml.
	PolicyFile string `toml:"policy_file"`
	// RefreshCadence controls how often the daemon refreshes opportunities; default 2m.
	RefreshCadence duration `toml:"refresh_cadence"`
	// HotReload controls whether opportunity policy changes are reloaded while the daemon runs; default true.
	HotReload *bool `toml:"hot_reload"`
	// ReloadInterval controls how often the daemon checks the opportunity policy file for changes; default 30s.
	ReloadInterval duration `toml:"reload_interval"`
}

// Supported local trading modes.
const (
	TradingModeDisabled = "disabled"
	TradingModePaper    = "paper"
	TradingModeLive     = "live"
)

// WithDefaults returns the protection-proposal configuration with missing
// operational values resolved. It does not enable broker submission.
func (a AutoTrade) WithDefaults() AutoTrade {
	if a.PolicyFile == "" {
		a.PolicyFile = "~/.config/ibkr/policies/protection-policy.toml"
	}
	if a.HotReload == nil {
		v := true
		a.HotReload = &v
	}
	if a.ReloadInterval == 0 {
		a.ReloadInterval = duration(30 * time.Second)
	}
	if a.ProposalCadence == 0 {
		a.ProposalCadence = duration(defaultProposalCadence)
	}
	return a
}

// defaultProposalCadence is the protection-proposal refresh interval when
// reqAccountSummary round-trip plus cache reads, so 30s keeps the panel
// fresh in fast markets while staying predictable; sustained-failure retries
const defaultProposalCadence = 30 * time.Second

// ProposalsEnabledResolved reports the effective advisory-proposal toggle;
func (a AutoTrade) ProposalsEnabledResolved() bool {
	if a.ProposalsEnabled == nil {
		return true
	}
	return *a.ProposalsEnabled
}

// Flex configures daily IBKR Flex statement ingestion for post-trade
// broker: statements feed the recon report; nothing here can touch order
type Flex struct {
	// Enabled turns the daily Flex statement fetch on; default false.
	Enabled bool `toml:"enabled"`
	// QueryID is the IBKR Flex query id to fetch (create the query in
	// Account Management with cash transactions, transfers, and equity
	// summary sections); required when enabled.
	QueryID string `toml:"query_id"`
	// TokenPath points to a file holding only the Flex Web Service token;
	// default ~/.config/ibkr/flex-token (mode 0600). The token itself never
	TokenPath string `toml:"token_path"`
}

// WithDefaults returns the Flex configuration with its default token path.
func (f Flex) WithDefaults() Flex {
	if f.TokenPath == "" {
		f.TokenPath = "~/.config/ibkr/flex-token"
	}
	return f
}

// FastPathEnabledResolved reports whether manual proposal actions may use the
// immediate revalidation path; absence defaults to enabled.
func (a AutoTrade) FastPathEnabledResolved() bool {
	if a.FastPathEnabled == nil {
		return true
	}
	return *a.FastPathEnabled
}

// HotReloadEnabled reports the effective protection-policy reload toggle;
func (a AutoTrade) HotReloadEnabled() bool {
	if a.HotReload == nil {
		return true
	}
	return *a.HotReload
}

// ReloadIntervalDuration returns the effective protection-policy reload
func (a AutoTrade) ReloadIntervalDuration() time.Duration {
	if a.ReloadInterval == 0 {
		return 30 * time.Second
	}
	return a.ReloadInterval.Std()
}

// ProposalCadenceDuration returns the effective advisory-proposal refresh
func (a AutoTrade) ProposalCadenceDuration() time.Duration {
	if a.ProposalCadence == 0 {
		return defaultProposalCadence
	}
	return a.ProposalCadence.Std()
}

// WithDefaults returns the opportunity configuration with missing operational
// values resolved. It does not enable broker submission.
func (o Opportunities) WithDefaults() Opportunities {
	if o.PolicyFile == "" {
		o.PolicyFile = "~/.config/ibkr/policies/opportunity-policy.toml"
	}
	if o.HotReload == nil {
		v := true
		o.HotReload = &v
	}
	if o.ReloadInterval == 0 {
		o.ReloadInterval = duration(30 * time.Second)
	}
	if o.RefreshCadence == 0 {
		o.RefreshCadence = duration(defaultOpportunityRefreshCadence)
	}
	return o
}

const defaultOpportunityRefreshCadence = 2 * time.Minute

// EnabledResolved reports the effective advisory-opportunity toggle; absence
func (o Opportunities) EnabledResolved() bool {
	if o.Enabled == nil {
		return true
	}
	return *o.Enabled
}

// HotReloadEnabled reports the effective opportunity-policy reload toggle;
func (o Opportunities) HotReloadEnabled() bool {
	if o.HotReload == nil {
		return true
	}
	return *o.HotReload
}

// ReloadIntervalDuration returns the effective opportunity-policy reload
func (o Opportunities) ReloadIntervalDuration() time.Duration {
	if o.ReloadInterval == 0 {
		return 30 * time.Second
	}
	return o.ReloadInterval.Std()
}

// RefreshCadenceDuration returns the effective advisory-opportunity refresh
func (o Opportunities) RefreshCadenceDuration() time.Duration {
	if o.RefreshCadence == 0 {
		return defaultOpportunityRefreshCadence
	}
	return o.RefreshCadence.Std()
}

// WithDefaults returns t with default values applied without granting trading.
func (t Trading) WithDefaults() Trading {
	if t.Mode == "" {
		t.Mode = TradingModeDisabled
	}
	if t.MaxNotional == 0 {
		t.MaxNotional = 10000
	}
	if t.MaxOptionContracts == 0 {
		t.MaxOptionContracts = 5
	}
	return t
}

// OrderEntryEnabled reports whether the configured trading mode can progress
func (t Trading) OrderEntryEnabled() bool {
	return t.Mode == TradingModePaper || t.Mode == TradingModeLive
}

// SPX holds the SPX-related daemon knobs. Currently just the members
// (or the binary's embedded fallback) and never reaches out.
type SPX struct {
	// MembersAutoRefresh controls whether the daemon refreshes the S&P 500 constituent list from Wikipedia daily at 02:30 ET (default true; set false to pin the embedded baseline) — overridden symmetrically by the `CANARY_SPX_MEMBERS_AUTO_REFRESH` env var (`1` force-on, `0` force-off).
	MembersAutoRefresh *bool `toml:"members_auto_refresh"`
}

// MembersAutoRefreshEnabled returns the resolved value of
// [spx] members_auto_refresh. Defaults to true when the field is
// absent — the refresher is opt-out, not opt-in.
func (s SPX) MembersAutoRefreshEnabled() bool {
	if s.MembersAutoRefresh == nil {
		return true
	}
	return *s.MembersAutoRefresh
}

// Config is the on-disk shape of ~/.config/ibkr/config.toml.
type Config struct {
	Gateway       Gateway       `toml:"gateway"`
	Daemon        Daemon        `toml:"daemon"`
	Trading       Trading       `toml:"trading"`
	Rulebook      Rulebook      `toml:"rulebook"`
	AutoTrade     AutoTrade     `toml:"auto_trade"`
	Opportunities Opportunities `toml:"opportunities"`
	Flex          Flex          `toml:"flex"`
	SPX           SPX           `toml:"spx"`
}

// Resolved is the validated, defaults-applied view a daemon actually uses.
type Resolved struct {
	Gateway       Gateway
	Daemon        Daemon
	Trading       Trading
	Rulebook      Rulebook
	AutoTrade     AutoTrade
	Opportunities Opportunities
	Flex          Flex
	SPX           SPX
}

// duration is a time.Duration that decodes from a TOML string ("5m").
type duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(v)
	return nil
}

// Std returns the underlying time.Duration.
func (d duration) Std() time.Duration { return time.Duration(d) }

// SetIdleTimeout overrides the daemon's idle timeout. Used by --foreground
func (d *Daemon) SetIdleTimeout(t time.Duration) {
	d.IdleTimeout = duration(t)
}

// DefaultPath returns the canonical config path for the current user.
func DefaultPath() string {
	// docgen:env CANARY_CONFIG | Override the config.toml path. Defaults to `$XDG_CONFIG_HOME/ibkr/config.toml` or `$HOME/.config/ibkr/config.toml`.
	if v := os.Getenv("CANARY_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ibkr", "config.toml")
}

// Load reads and parses the config file at path. A missing file yields a
// zero-value Config — every field nil/empty, meaning "fully auto."
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Reject unknown keys instead of silently dropping them. The previous
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
			if msg, ok := removedKeys[keys[i]]; ok {
				return nil, fmt.Errorf("config %s: key %s was removed: %s", path, keys[i], msg)
			}
		}
		return nil, fmt.Errorf("config %s: unknown key(s): %s (see README §Configuration for the supported schema: [gateway], [daemon], [trading], [rulebook], [auto_trade], [opportunities], [flex], [spx])", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}

// removedKeys maps once-valid config keys to a targeted load error. A failed
var removedKeys = map[string]string{
	"trading.allow_live":        "live trading needs only [trading].mode = \"live\" plus the [gateway] pins — delete this key",
	"trading.live_ack_account":  "live trading needs only [trading].mode = \"live\" plus the [gateway] pins — delete this key",
	"trading.live_ack_endpoint": "live trading needs only [trading].mode = \"live\" plus the [gateway] pins — delete this key",

	"trading.allow_option_market_orders": "option market orders are not a supported local knob — delete this key",
	"trading.mcp_enabled":                "MCP broker-write controls are not exposed — delete this key",
	"trading.mcp_mode":                   "MCP broker-write controls are not exposed — delete this key",
	"trading.mcp_nonce_ttl":              "MCP human nonces are not a supported surface — delete this key",
	"auto_trade.enabled":                 "autonomous auto-trading is not a supported config surface — delete this key",
	"auto_trade.auto_submit":             "autonomous submit is not a supported config surface — delete this key",
}

// Resolve applies daemon-level defaults and returns the Resolved view.
func (c *Config) Resolve() (*Resolved, error) {
	dae := c.Daemon
	if dae.IdleTimeout == 0 {
		// 15 min default (was 5 min). Combined with the persistent option
		dae.IdleTimeout = duration(15 * time.Minute)
	}
	if dae.LogLevel == "" {
		dae.LogLevel = "warn"
	}

	return &Resolved{
		Gateway:       c.Gateway,
		Daemon:        dae,
		Trading:       c.Trading.WithDefaults(),
		Rulebook:      c.Rulebook,
		AutoTrade:     c.AutoTrade.WithDefaults(),
		Opportunities: c.Opportunities.WithDefaults(),
		Flex:          c.Flex.WithDefaults(),
		SPX:           c.SPX,
	}, nil
}

// SPXMembersAutoRefreshFromEnv resolves CANARY_SPX_MEMBERS_AUTO_REFRESH
// as a bidirectional override of the [spx] members_auto_refresh TOML
// field:
//
//   - "1"               → returns (true, true): explicit force-on.
//   - "0"               → returns (false, true): explicit force-off.
//   - unset / other     → returns (false, false): defer to TOML.
//     Garbage values are silently ignored rather than rejected; env-var
//     typos are a CI friction we'd rather not fail-loud on, and there's
//     no realistic compliance posture that wants "fail when the env is
//     present but malformed."
//
// The second return ("forced") distinguishes "env actively overrode
// the TOML" from "env unset, TOML governs." The status renderer uses
// this to pick the "disabled (env)" vs "disabled (config)" suffix.
//
// Lives next to the SPX type so the precedence rules don't have to be
// re-derived at every call site.
func SPXMembersAutoRefreshFromEnv() (enabled bool, forced bool) {
	// docgen:env CANARY_SPX_MEMBERS_AUTO_REFRESH | Symmetric override of `[spx] members_auto_refresh`. `1` force-enables, `0` force-disables, unset / other defers to TOML.
	switch os.Getenv("CANARY_SPX_MEMBERS_AUTO_REFRESH") {
	case "1":
		return true, true
	case "0":
		return false, true
	default:
		return false, false
	}
}
