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
// distinguish "user wrote this value" (binding) from "field absent" (auto).
//
// Host is plain string because "" and "127.0.0.1" carry the same meaning:
// auto-discovery only probes loopback. A non-loopback host implies "I know
// where my gateway is" and is treated as pinned.
//
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
	// Default 16 (one above the primary's default 15). Pinned to its
	// own connection so breadth's 503-name fan-out runs against a
	// separate 40-msg/sec and 60-historical/10-min rate-limit budget
	// rather than competing with interactive RPCs and the gamma
	// option-leg fan-out on the primary client. Collisions are treated
	// as stale-client/operator issues, not silently shifted to another
	// role's reserved ID.
	BreadthClientID *int `toml:"breadth_client_id"`
	// Account pins the IBKR account ID like "U1234567"; empty (default) defers to the gateway's managedAccounts list — fine for single-account logins, required disambiguator when the login carries multiple accounts.
	Account string `toml:"account"`
	// TLS pins TLS mode for the API socket: absent (nil) auto-tries plain first then TLS, `true` forces TLS-only with no plain fallback, `false` forces plain — setting the field disables fallback in either direction.
	TLS *bool `toml:"tls"`
}

// PortPinned reports whether the user pinned a port. Discovery skips the
// port-probe step when true and uses Port directly.
func (g Gateway) PortPinned() bool { return g.Port != nil }

// TLSPinned reports whether the user pinned a TLS mode. The SDK's
// EnableTLSFallback is set to false (no fallback) when true, regardless of
// which mode was pinned — matches the strict-on-explicit-set contract.
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
// breadth connector. Default 16 — one above the primary default — so a
// fresh install gets two non-colliding role-owned IDs without any config tweak.
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
	// LogLevel is the daemon's log verbosity — one of "debug", "info" (default), "warn", or "error".
	LogLevel string `toml:"log_level"`
}

// Trading holds local order-entry gates for experimental trading builds.
// Stable ibkr releases are read-only; a missing [trading] section resolves to
// mode="disabled". Treat active trading config as an explicit, as-is operator
// override. TWS / Gateway broker permissions remain the final authority even
// after these local gates pass.
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
	// PaperSmokeMaxAge is how long a paper trading smoke remains acceptable for live enablement. Defaults to 168h.
	PaperSmokeMaxAge duration `toml:"paper_smoke_max_age"`
}

// Rulebook configures operator-owned evidence inputs for the advisory trading
// rulebook. The import file is not read on snapshot paths: daemon startup
// validates and transactionally publishes it into daemon.db, which remains the
// live authority.
type Rulebook struct {
	// TerminalEvidenceFile points to an optional JSON document of reviewed,
	// exact-contract terminal/non-reporting issuer evidence for rules 6-8. An
	// empty path leaves the retained daemon.db authority unchanged.
	TerminalEvidenceFile string `toml:"terminal_evidence_file"`
}

// AutoTrade configures advisory protection-proposal production and policy
// reloads. Despite the historical section name, these fields do not authorize
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
// [auto_trade].proposal_cadence is unset. A refresh costs one
// reqAccountSummary round-trip plus cache reads, so 30s keeps the panel
// fresh in fast markets while staying predictable; sustained-failure retries
// are governed separately by the engine's backoff cap.
const defaultProposalCadence = 30 * time.Second

// ProposalsEnabledResolved reports the effective advisory-proposal toggle;
// absence defaults to enabled.
func (a AutoTrade) ProposalsEnabledResolved() bool {
	if a.ProposalsEnabled == nil {
		return true
	}
	return *a.ProposalsEnabled
}

// Flex configures daily IBKR Flex statement ingestion for post-trade
// reconciliation (internal-docs/design/post-trade-truth.md). Read-only toward the
// broker: statements feed the recon report; nothing here can touch order
// entry.
type Flex struct {
	// Enabled turns the daily Flex statement fetch on; default false.
	Enabled bool `toml:"enabled"`
	// QueryID is the IBKR Flex query id to fetch (create the query in
	// Account Management with cash transactions, transfers, and equity
	// summary sections); required when enabled.
	QueryID string `toml:"query_id"`
	// TokenPath points to a file holding only the Flex Web Service token;
	// default ~/.config/ibkr/flex-token (mode 0600). The token itself never
	// belongs in config.toml, and no surface ever echoes it.
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
// absence defaults to enabled.
func (a AutoTrade) HotReloadEnabled() bool {
	if a.HotReload == nil {
		return true
	}
	return *a.HotReload
}

// ReloadIntervalDuration returns the effective protection-policy reload
// interval.
func (a AutoTrade) ReloadIntervalDuration() time.Duration {
	if a.ReloadInterval == 0 {
		return 30 * time.Second
	}
	return a.ReloadInterval.Std()
}

// ProposalCadenceDuration returns the effective advisory-proposal refresh
// cadence.
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
// defaults to enabled.
func (o Opportunities) EnabledResolved() bool {
	if o.Enabled == nil {
		return true
	}
	return *o.Enabled
}

// HotReloadEnabled reports the effective opportunity-policy reload toggle;
// absence defaults to enabled.
func (o Opportunities) HotReloadEnabled() bool {
	if o.HotReload == nil {
		return true
	}
	return *o.HotReload
}

// ReloadIntervalDuration returns the effective opportunity-policy reload
// interval.
func (o Opportunities) ReloadIntervalDuration() time.Duration {
	if o.ReloadInterval == 0 {
		return 30 * time.Second
	}
	return o.ReloadInterval.Std()
}

// RefreshCadenceDuration returns the effective advisory-opportunity refresh
// cadence.
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
	if t.PaperSmokeMaxAge == 0 {
		t.PaperSmokeMaxAge = duration(168 * time.Hour)
	}
	return t
}

// OrderEntryEnabled reports whether the configured trading mode can progress
// beyond read-only status/preview diagnostics.
func (t Trading) OrderEntryEnabled() bool {
	return t.Mode == TradingModePaper || t.Mode == TradingModeLive
}

// PaperSmokeMaxAgeDuration returns the resolved paper-smoke freshness window.
func (t Trading) PaperSmokeMaxAgeDuration() time.Duration {
	if t.PaperSmokeMaxAge == 0 {
		return 168 * time.Hour
	}
	return t.PaperSmokeMaxAge.Std()
}

// SPX holds the SPX-related daemon knobs. Currently just the members
// auto-refresh toggle; grouping under [spx] gives future SPX-scoped
// configs (e.g. fetcher concurrency, sweep tunables) a natural home
// without proliferating top-level TOML sections.
//
// When MembersAutoRefresh is true (default) the daemon fetches
// Wikipedia's constituent list daily at 02:30 ET plus on startup if
// the cached file is stale; when false it loads whatever is on disk
// (or the binary's embedded fallback) and never reaches out.
//
// Use case for pinning off: regulated traders running reproducibility
// audits, air-gapped boxes, anyone debugging breadth drift. The
// CANARY_SPX_MEMBERS_AUTO_REFRESH env var overrides this field at
// runtime: "1" force-enables, "0" force-disables, anything else
// (including unset) defers to the TOML value. Symmetric semantics —
// the env is a bidirectional override, not a one-way kill switch.
type SPX struct {
	// MembersAutoRefresh controls whether the daemon refreshes the S&P 500 constituent list from Wikipedia daily at 02:30 ET (default true; set false to pin the embedded baseline) — overridden symmetrically by the `CANARY_SPX_MEMBERS_AUTO_REFRESH` env var (`1` force-on, `0` force-off).
	//
	// Pointer type lets the daemon distinguish an explicit `members_auto_refresh = true` from "field absent" — both enable the refresher today, but a future "user opted in" vs "default behaviour" distinction stays additive.
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

// Scan holds a single scanner preset. Timeout is per-preset and optional;
// <=0 falls back to the daemon's default (20s).
type Scan struct {
	// Type is the IBKR scanner code, such as TOP_PERC_GAIN; dump your gateway's catalog with `canary scan params`.
	Type string `toml:"type"`
	// Exchange is the IBKR scanner locationCode, such as STK.US.MAJOR or STK.NASDAQ.
	Exchange string `toml:"exchange"`
	// Instrument is the IBKR scanner instrument token, such as STK for US stocks or STOCK.EU for European stocks; empty defaults to STK.
	Instrument string `toml:"instrument"`
	// Limit caps returned rows for this preset.
	Limit int `toml:"limit"`
	// Timeout is the per-preset scan timeout; <=0 falls back to the daemon default (20s).
	Timeout duration `toml:"timeout"`
}

// Config is the on-disk shape of ~/.config/ibkr/config.toml.
type Config struct {
	Gateway       Gateway         `toml:"gateway"`
	Daemon        Daemon          `toml:"daemon"`
	Trading       Trading         `toml:"trading"`
	Rulebook      Rulebook        `toml:"rulebook"`
	AutoTrade     AutoTrade       `toml:"auto_trade"`
	Opportunities Opportunities   `toml:"opportunities"`
	Flex          Flex            `toml:"flex"`
	SPX           SPX             `toml:"spx"`
	Scans         map[string]Scan `toml:"scans"`
}

// Resolved is the validated, defaults-applied view a daemon actually uses.
// Gateway carries the raw (pointer-fielded) user input; discovery happens
// later in internal/discover and produces concrete values.
type Resolved struct {
	Gateway       Gateway
	Daemon        Daemon
	Trading       Trading
	Rulebook      Rulebook
	AutoTrade     AutoTrade
	Opportunities Opportunities
	Flex          Flex
	SPX           SPX
	Scans         map[string]Scan
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
// (set 0 to disable idle-shutdown) and by tests that want a fast-firing
// idle watcher; the underlying field type is unexported so callers outside
// this package cannot construct it directly.
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
	// behavior masked stale-schema configs (e.g. `[profiles.live]` from an
	// older proposal) as "fully auto" — the daemon then probed all ports
	// and the user's pinned settings were silently ignored.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
			if msg, ok := removedKeys[keys[i]]; ok {
				return nil, fmt.Errorf("config %s: key %s was removed: %s", path, keys[i], msg)
			}
		}
		return nil, fmt.Errorf("config %s: unknown key(s): %s (see README §Configuration for the supported schema: [gateway], [daemon], [trading], [rulebook], [auto_trade], [opportunities], [flex], [spx], [scans.<name>])", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}

// removedKeys maps once-valid config keys to a targeted load error. A failed
// load kills the autospawned daemon and with it every CLI command, so a
// leftover key from an older schema deserves "removed, delete it" rather
// than the generic unknown-key message.
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
// Gateway is passed through verbatim (pointer fields preserved) so
// discovery can see what was pinned vs. left auto.
func (c *Config) Resolve() (*Resolved, error) {
	dae := c.Daemon
	if dae.IdleTimeout == 0 {
		// 15 min default (was 5 min). Combined with the persistent option
		// contract cache (PrewarmOptionChain) and the soft-TTL gamma
		// refresh, the cost of a daemon restart is now multi-minute
		// recompute — short idle windows cost more than they save.
		dae.IdleTimeout = duration(15 * time.Minute)
	}
	if dae.LogLevel == "" {
		dae.LogLevel = "info"
	}

	scans := c.Scans
	if scans == nil {
		scans = defaultScans()
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
		Scans:         scans,
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

// defaultScans is the built-in preset set, used when the user has no
// [scans.*] block in config.toml. Every Type / Exchange string here was
// validated against an IB Gateway scanner catalog via `canary scan params`.
// If a future gateway drops one of these scanCodes the
// preset will return an error to the user; `canary scan params` is the
// canonical recovery path.
//
// Selection rationale (US stock + options trader):
//   - Direction symmetric: top-movers + top-losers, not just gainers.
//   - Volume both ways: most-active is raw share volume (mega-caps);
//     unusual-vol is volume relative to a stock's own history (catches
//     names that are unusual *for themselves*).
//   - Two options scans because they cover different use cases:
//     high-iv-rank surfaces names where IV is elevated vs. their own
//     history (the option-seller's "something brewing" signal);
//     unusual-opt-vol surfaces names where option volume is hot vs.
//     average (the flow signal).
//   - gappers catches the open-print earnings/news reactions that drive
//     the first hour of every session.
//
// Absolute-IV (HIGH_OPT_IMP_VOLAT) was deliberately not included — it
// surfaces the same structural high-IV biotech/SPAC names every day and
// is rarely the actionable signal an option seller wants. Users who
// still want it can copy this map into their config.toml and add the
// extra preset.
func defaultScans() map[string]Scan {
	return map[string]Scan{
		"top-movers":      {Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 20},
		"top-losers":      {Type: "TOP_PERC_LOSE", Exchange: "STK.US.MAJOR", Limit: 20},
		"most-active":     {Type: "MOST_ACTIVE", Exchange: "STK.US.MAJOR", Limit: 20},
		"unusual-vol":     {Type: "HOT_BY_VOLUME", Exchange: "STK.US.MAJOR", Limit: 20},
		"gappers":         {Type: "HIGH_OPEN_GAP", Exchange: "STK.US.MAJOR", Limit: 20},
		"high-iv-rank":    {Type: "HIGH_OPT_IMP_VOLAT_OVER_HIST", Exchange: "STK.US", Limit: 20},
		"unusual-opt-vol": {Type: "HOT_BY_OPT_VOLUME", Exchange: "STK.US.MAJOR", Limit: 20},
	}
}
