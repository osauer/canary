package rpc

import (
	"time"
)

// Settings access and source values state whether a field is mutable and which
// authority supplied it.
const (
	SettingsAccessRead  = "read"
	SettingsAccessWrite = "write"

	SettingsSourceRuntime  = "runtime"
	SettingsSourceConfig   = "config"
	SettingsSourceBuild    = "build"
	SettingsSourceObserved = "observed"
)

// SettingsBool is a boolean value annotated with access and source authority.
type SettingsBool struct {
	Value  bool   `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

// SettingsFloat is a floating-point value annotated with access and source
// authority.
type SettingsFloat struct {
	Value  float64 `json:"value"`
	Access string  `json:"access"`
	Source string  `json:"source"`
	Reason string  `json:"reason,omitempty"`
}

// SettingsInt is an integer value annotated with access and source authority.
type SettingsInt struct {
	Value  int    `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

// SettingsString is a string value annotated with access and source authority.
type SettingsString struct {
	Value  string `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

// PlatformSettings is the typed, daemon-authored settings view. It combines
type PlatformSettings struct {
	Kind       string                    `json:"kind"`
	Display    PlatformDisplaySettings   `json:"display"`
	Features   PlatformFeatureSettings   `json:"features"`
	Trading    PlatformTradingSettings   `json:"trading"`
	AutoTrade  PlatformAutoTradeSettings `json:"auto_trade"`
	Regime     PlatformRegimeSettings    `json:"regime"`
	Stress     PlatformStressSettings    `json:"stress"`
	History    PlatformHistorySettings   `json:"history"`
	MarketData PlatformMarketDataSetting `json:"market_data"`
	Build      PlatformBuildSettings     `json:"build"`
	AsOf       time.Time                 `json:"as_of"`
}

// PlatformDisplaySettings holds presentation preferences. These values alter
// only adapter rendering; timestamps and market-session authority stay typed
// and unchanged.
type PlatformDisplaySettings struct {
	DateFormat SettingsString `json:"date_format"`
}

// DisplayDateFormat values are the closed calendar-date presentations shared
// by platform settings and adapters.
const (
	DisplayDateFormatUS        = "us"
	DisplayDateFormatEU        = "eu"
	DisplayDateFormatUSWeekday = "us_weekday"
	DisplayDateFormatEUWeekday = "eu_weekday"
)

// PlatformRegimeSettings holds the regime engine's runtime preferences.
type PlatformRegimeSettings struct {
	Journal RegimeJournalSettings `json:"journal"`
}

// RegimeJournalSettings retains its public name while controlling
type RegimeJournalSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// PlatformStressSettings holds the portfolio-stress evidence-collection
type PlatformStressSettings struct {
	Journal StressJournalSettings `json:"journal"`
}

// StressJournalSettings controls typed stress-decision event collection in
// daemon.db, mirroring RegimeJournalSettings.
type StressJournalSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// PlatformHistorySettings is retained to preserve the settings response wire
// shape. Decision-journal rotation is retired under daemon.db authority.
type PlatformHistorySettings struct {
	Rotation HistoryRotationSettings `json:"rotation"`
}

// HistoryRotationSettings is a retired compatibility shape. Its fields do not
// enable a rotation worker or authorize writes to legacy journals/archives.
type HistoryRotationSettings struct {
	Enabled SettingsBool `json:"enabled"`
	// KeepRawMonths is retained for wire compatibility and has no live
	KeepRawMonths SettingsInt `json:"keep_raw_months"`
}

// PlatformFeatureSettings groups runtime feature preferences.
type PlatformFeatureSettings struct {
	StockProtection StockProtectionSettings `json:"stock_protection"`
	Rulebook        RulebookSettings        `json:"rulebook"`
}

// StockProtectionSettings controls stock-protection proposal actions without
// enabling broker writes.
type StockProtectionSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// RulebookSettings controls the advisory trading rulebook
// affect broker-write gating in either direction.
type RulebookSettings struct {
	Enabled SettingsBool `json:"enabled"`
	// EarningsOverrides maps SYMBOL → "YYYY-MM-DD" (optional "Tamc"/"Tbmo"
	EarningsOverrides SettingsStringMap `json:"earnings_overrides"`
}

// SettingsStringMap is a map-valued setting with the standard
type SettingsStringMap struct {
	Value  map[string]string `json:"value,omitempty"`
	Access string            `json:"access"`
	Source string            `json:"source"`
	Reason string            `json:"reason,omitempty"`
}

// PlatformTradingSettings combines the runtime freeze brake with read-only
type PlatformTradingSettings struct {
	// Freeze is the runtime trading brake: true blocks every new broker
	// `canary settings set trading.freeze=true|false`.
	Freeze               SettingsBool         `json:"freeze"`
	Mode                 SettingsString       `json:"mode"`
	Account              SettingsString       `json:"account"`
	Endpoint             SettingsString       `json:"endpoint"`
	ClientID             SettingsInt          `json:"client_id"`
	MCPTrading           SettingsString       `json:"mcp_trading"`
	LiveOverride         SettingsString       `json:"live_override"`
	BuildWritesAvailable SettingsBool         `json:"build_writes_available"`
	Limits               TradingLimitSettings `json:"limits"`
}

// PlatformAutoTradeSettings exposes proposal-generation preferences and loaded
// configuration; none of its fields are broker-write authority.
type PlatformAutoTradeSettings struct {
	ProposalsEnabled SettingsBool   `json:"proposals_enabled"`
	FastPathEnabled  SettingsBool   `json:"fast_path_enabled"`
	PolicyFile       SettingsString `json:"policy_file"`
	HotReload        SettingsBool   `json:"hot_reload"`
	ReloadInterval   SettingsString `json:"reload_interval"`
	ProposalCadence  SettingsString `json:"proposal_cadence"`
}

// TradingLimitSettings reports effective safety limits with per-field access
type TradingLimitSettings struct {
	MaxNotional           SettingsFloat `json:"max_notional"`
	MaxOptionContracts    SettingsInt   `json:"max_option_contracts"`
	AllowStockShort       SettingsBool  `json:"allow_stock_short"`
	AllowOptionSellToOpen SettingsBool  `json:"allow_option_sell_to_open"`
}

// PlatformMarketDataSetting exposes observed data quality and never persists
// broker entitlements.
type PlatformMarketDataSetting struct {
	Quality PlatformMarketDataQuality `json:"quality"`
}

// PlatformMarketDataQuality summarizes current observed feed quality. A zero
type PlatformMarketDataQuality struct {
	Status      string              `json:"status"`
	Summary     string              `json:"summary,omitempty"`
	QuoteCounts map[string]int      `json:"quote_counts,omitempty"`
	DataQuality []DataQualityHealth `json:"data_quality,omitempty"`
	Access      string              `json:"access"`
	Source      string              `json:"source"`
	Reason      string              `json:"reason,omitempty"`
	ObservedAt  time.Time           `json:"observed_at,omitzero"`
}

// PlatformBuildSettings exposes immutable build-channel capabilities.
type PlatformBuildSettings struct {
	Channel                 SettingsString `json:"channel"`
	TradingWritesAvailable  SettingsBool   `json:"trading_writes_available"`
	ExperimentalTradingNote string         `json:"experimental_trading_note,omitempty"`
}

// This file is the single registry of writable runtime platform settings.
// The daemon's patch validation, the CLI's `settings set` key list, and the
// generated configuration reference (scripts/docgen/config-ref) all consume
// it, so a key added here is automatically accepted, advertised, and
// documented — and a key missing here is rejected everywhere. Read-only
// observed fields (mode, endpoint, market-data quality, ...) are not
// settings and do not belong in this registry.

// SettingsKeyKind is the value grammar of a writable runtime setting.
type SettingsKeyKind string

const (
	// SettingsKindBool accepts true, false, or null (clear the override).
	SettingsKindBool SettingsKeyKind = "bool"
	// SettingsKindFloat accepts a positive number or null.
	SettingsKindFloat SettingsKeyKind = "float"
	// SettingsKindInt accepts a positive integer or null.
	SettingsKindInt SettingsKeyKind = "int"
	// SettingsKindDateMap accepts an object of SYMBOL → "YYYY-MM-DD" (optional
	// Tamc/Tbmo suffix) entries; a null entry clears that symbol, a null map
	// clears all of them, and patches merge per symbol.
	SettingsKindDateMap SettingsKeyKind = "date-map"
	// SettingsKindDateFormat accepts one closed calendar-date presentation
	// value or null to restore the US default.
	SettingsKindDateFormat SettingsKeyKind = "date-format"
)

// Writability classes the daemon enforces beyond per-kind parsing.
const (
	// SettingsClassRuntime keys are runtime-owned. Individual keys may add a
	// stricter origin policy; trading.freeze is human-terminal-only in every mode.
	SettingsClassRuntime = "runtime"
	// SettingsClassTradingLimit keys are writable only while trading limits
	// are writable (experimental trading build with paper/live mode), and every
	// write requires a human-terminal origin in every mode.
	SettingsClassTradingLimit = "trading-limit"
)

// SettingsKeySpec declares one writable runtime setting.
type SettingsKeySpec struct {
	// Key is the dotted path used by the CLI, the JSON patch body, and the
	// generated docs, e.g. "features.rulebook.enabled".
	Key string
	// Kind selects the value grammar.
	Kind SettingsKeyKind
	// Class selects the writability gate.
	Class string
	// Doc is the one-sentence plain-English description rendered in
	// `canary settings set --help` and the generated configuration reference.
	Doc string
}

// SettingsKeys returns the registry in stable display order.
func SettingsKeys() []SettingsKeySpec {
	return []SettingsKeySpec{
		{
			Key: "display.date_format", Kind: SettingsKindDateFormat, Class: SettingsClassRuntime,
			Doc: "Calendar-date presentation in the SPA: us, eu, us_weekday, or eu_weekday; it never changes typed timestamps or market-session authority (default us).",
		},
		{
			Key: "features.stock_protection.enabled", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Allows stock/ETF protection proposal actions; false blocks them with a stock_protection_disabled blocker while proposal snapshots stay readable (default true).",
		},
		{
			Key: "features.rulebook.enabled", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Turns the advisory daily trading-rulebook checklist on; false hides the SPA card, empties rules.snapshot, and stops advisory rule_* preview warnings — it can never affect broker-write gating (default true).",
		},
		{
			Key: "features.rulebook.earnings_overrides", Kind: SettingsKindDateMap, Class: SettingsClassRuntime,
			Doc: "Manual SYMBOL → YYYY-MM-DD (optional Tamc/Tbmo suffix) earnings pins, authoritative over fetched dates for rules 6-8; patches merge per symbol.",
		},
		{
			Key: "trading.freeze", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Runtime trading brake: true blocks every new broker write while cancels stay allowed; human-only by policy, and the write origin is audited (default false).",
		},
		{
			Key: "trading.limits.max_notional", Kind: SettingsKindFloat, Class: SettingsClassTradingLimit,
			Doc: "Runtime override of [trading].max_notional, the notional cap for every equity/ETF order including apparent exits; null falls back to the TOML value.",
		},
		{
			Key: "trading.limits.max_option_contracts", Kind: SettingsKindInt, Class: SettingsClassTradingLimit,
			Doc: "Runtime override of [trading].max_option_contracts, the quantity cap for every single-leg option order including apparent exits; null falls back to the TOML value.",
		},
		{
			Key: "trading.limits.allow_stock_short", Kind: SettingsKindBool, Class: SettingsClassTradingLimit,
			Doc: "Runtime override of [trading].allow_stock_short; null falls back to the TOML value.",
		},
		{
			Key: "trading.limits.allow_option_sell_to_open", Kind: SettingsKindBool, Class: SettingsClassTradingLimit,
			Doc: "Runtime override of [trading].allow_option_sell_to_open; null falls back to the TOML value.",
		},
		{
			Key: "regime.journal.enabled", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Turns forward regime decision-event collection in daemon.db on (default true).",
		},
		{
			Key: "stress.journal.enabled", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Turns forward portfolio-stress decision-event collection in daemon.db on, mirroring regime.journal.enabled (default true).",
		},
	}
}
