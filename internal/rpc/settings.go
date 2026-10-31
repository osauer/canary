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
	// SettingsSourcePolicy marks a value read from a policy file, such as
	// the order limits of risk-policy.toml [order_limits].
	SettingsSourcePolicy = "policy"
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
	// Unset is true when the source does not write the value: Value is
	// then not a figure, and a surface says "not set", never 0.
	Unset bool `json:"unset,omitempty"`
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
	CashSweep  PlatformCashSweepSettings `json:"cash_sweep"`
	Display    PlatformDisplaySettings   `json:"display"`
	Features   PlatformFeatureSettings   `json:"features"`
	Trading    PlatformTradingSettings   `json:"trading"`
	AutoTrade  PlatformAutoTradeSettings `json:"auto_trade"`
	Regime     PlatformRegimeSettings    `json:"regime"`
	Stress     PlatformStressSettings    `json:"stress"`
	Chains     PlatformChainsSettings    `json:"chains"`
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

// PlatformChainsSettings holds the option-chain recorder's preferences.
type PlatformChainsSettings struct {
	Recorder ChainRecorderSettings `json:"recorder"`
}

// ChainRecorderSettings is the recorder's switch (off by default) and its
// explicit underlyings; held option underlyings are added at run time.
type ChainRecorderSettings struct {
	Enabled SettingsBool       `json:"enabled"`
	Symbols SettingsStringList `json:"symbols"`
}

// SettingsStringList is a string list annotated with access and source
// authority.
type SettingsStringList struct {
	Value  []string `json:"value"`
	Access string   `json:"access"`
	Source string   `json:"source"`
	Reason string   `json:"reason,omitempty"`
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
	// TWSOrderControl governs cancelling and changing orders placed by hand
	// in TWS through a separate client-0 connection (owner decision "TWS B",
	// 2026-10-11 19:36 CEST).
	TWSOrderControl TWSOrderControlSettings `json:"tws_order_control"`
}

// TWSOrderControlSettings is the TWS order-control switch and its proof.
// Enabled is the owner's runtime switch, off by default and terminal-only
// like the freeze. PaperProvenAt is recorded by the daemon when a control
// action on the paper account ends confirmed or pending; it is empty until
// then and read-only. Status says whether the path is available in the
// current mode: "available", "off", "paper proof required", or the reason it
// cannot run. Live is available only when Enabled and PaperProvenAt are both
// set.
type TWSOrderControlSettings struct {
	Enabled       SettingsBool   `json:"enabled"`
	PaperProvenAt SettingsString `json:"paper_proven_at"`
	Status        SettingsString `json:"status"`
}

// TWS order-control status values.
const (
	TWSOrderControlStatusAvailable     = "available"
	TWSOrderControlStatusOff           = "off"
	TWSOrderControlStatusProofRequired = "paper proof required"
)

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

// TradingLimitSettings reports the order limits in force, read-only and
// sourced from risk-policy.toml [order_limits] since the owner decision of
// 2026-10-05 19:56 CEST: MaxNotional is the cap in force, and every Reason
// says how it is bound or which key is missing. The runtime overrides are
// retired. MaxBondMaturityYears is the longest maturity, in whole years
// from today, a bond or bill buy may have (owner decision 2026-10-06 20:17
// CEST).
type TradingLimitSettings struct {
	MaxNotional           SettingsFloat `json:"max_notional"`
	MaxOptionContracts    SettingsInt   `json:"max_option_contracts"`
	AllowStockShort       SettingsBool  `json:"allow_stock_short"`
	AllowOptionSellToOpen SettingsBool  `json:"allow_option_sell_to_open"`
	MaxBondMaturityYears  SettingsInt   `json:"max_bond_maturity_years"`
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
	// SettingsKindCashSweepPriority accepts one closed currency ordering value or null.
	SettingsKindCashSweepPriority SettingsKeyKind = "cash-sweep-priority"
	// SettingsKindSymbolList accepts a list of underlying symbols (the CLI
	// takes them comma-separated, [] for none) or null to restore the default.
	SettingsKindSymbolList SettingsKeyKind = "symbol-list"
)

// Writability classes the daemon enforces by the write's origin, deny by
// default (internal-docs/design/strategy-register.md, "Settings: classes the
// daemon enforces"; owner decision 2026-10-18 09:01 CEST: strategies declare
// the data Canary collects, and the owner keeps the safety controls).
const (
	// SettingsClassSafety keys govern whether a broker write may happen at
	// all: only a human terminal may change them.
	SettingsClassSafety = "safety"
	// SettingsClassOwner keys are the owner's: the protection control, the
	// collection off switches and every preference. A human terminal or the
	// owner's paired device may change them; an agent or a missing origin
	// may not.
	SettingsClassOwner = "owner"
	// SettingsClassCollection keys widen or narrow what Canary records and
	// govern market-data reads only: any origin may change them.
	SettingsClassCollection = "collection"
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
		{Key: "cash_sweep.currency_priority", Kind: SettingsKindCashSweepPriority, Class: SettingsClassOwner, Doc: "Cash-sweep ordering only: usd_first, balanced, eur_first, or null for policy/default; never activates reserve policy or changes funding, caps, floors, freeze, or execution authority."},
		{
			Key: "display.date_format", Kind: SettingsKindDateFormat, Class: SettingsClassOwner,
			Doc: "Calendar-date presentation in the SPA: us, eu, us_weekday, or eu_weekday; it never changes typed timestamps or market-session authority (default us).",
		},
		{
			Key: "features.stock_protection.enabled", Kind: SettingsKindBool, Class: SettingsClassOwner,
			Doc: "Allows stock/ETF protection proposal actions; false blocks them with a stock_protection_disabled blocker while proposal snapshots stay readable (default true).",
		},
		{
			Key: "features.rulebook.enabled", Kind: SettingsKindBool, Class: SettingsClassOwner,
			Doc: "Turns the advisory daily trading-rulebook checklist on; false hides the SPA card, empties rules.snapshot, and stops advisory rule_* preview warnings — it can never affect broker-write gating (default true).",
		},
		{
			Key: "features.rulebook.earnings_overrides", Kind: SettingsKindDateMap, Class: SettingsClassOwner,
			Doc: "Manual SYMBOL → YYYY-MM-DD (optional Tamc/Tbmo suffix) earnings pins, authoritative over fetched dates for rules 6-8 and 20; patches merge per symbol.",
		},
		{
			Key: "trading.freeze", Kind: SettingsKindBool, Class: SettingsClassSafety,
			Doc: "Runtime trading brake: true blocks every new broker write while cancels stay allowed; human-only by policy, and the write origin is audited (default false).",
		},
		{
			Key: "trading.tws_order_control.enabled", Kind: SettingsKindBool, Class: SettingsClassSafety,
			Doc: "Lets an owner-confirmed cancel or change reach an order placed by hand in TWS through a separate client-0 connection; paper works once enabled, live also needs the daemon-recorded paper proof (trading.tws_order_control.paper_proven_at); human-only by policy (default false).",
		},
		{
			Key: "regime.journal.enabled", Kind: SettingsKindBool, Class: SettingsClassOwner,
			Doc: "Turns forward regime decision-event collection in daemon.db on (default true).",
		},
		{
			Key: "stress.journal.enabled", Kind: SettingsKindBool, Class: SettingsClassOwner,
			Doc: "Turns forward portfolio-stress decision-event collection in daemon.db on, mirroring regime.journal.enabled (default true).",
		},
		{
			Key: "chains.recorder.enabled", Kind: SettingsKindBool, Class: SettingsClassOwner,
			Doc: "The owner's off switch for the daily option-chain snapshot (quotes, sizes, IV, greeks, open interest) 30 minutes before the US close into option-chains.db beside daemon.db: unset, it records what holdings and registered strategies declare; false stops it. Market-data reads only, within a line reserve for the live display (default unset: on).",
		},
		{
			Key: "chains.recorder.symbols", Kind: SettingsKindSymbolList, Class: SettingsClassCollection,
			Doc: "Research names the option-chain recorder covers after held option underlyings and registered strategies' declared chains, at most 12 in all; a collection setting any origin may change; null restores none.",
		},
	}
}
