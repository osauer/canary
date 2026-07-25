package rpc

import "time"

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
// writable runtime preferences with read-only config, build, and observations.
type PlatformSettings struct {
	Kind       string                    `json:"kind"`
	Features   PlatformFeatureSettings   `json:"features"`
	Trading    PlatformTradingSettings   `json:"trading"`
	AutoTrade  PlatformAutoTradeSettings `json:"auto_trade"`
	Regime     PlatformRegimeSettings    `json:"regime"`
	Canary     PlatformCanarySettings    `json:"canary"`
	History    PlatformHistorySettings   `json:"history"`
	MarketData PlatformMarketDataSetting `json:"market_data"`
	Build      PlatformBuildSettings     `json:"build"`
	AsOf       time.Time                 `json:"as_of"`
}

// PlatformRegimeSettings holds the regime engine's runtime preferences.
// Deliberately one knob: the confirmation-gate values (depth, streaks,
// co-sign, max ages) are code-owned pending_backtest policy — user-tunable
// gates would fork the decision corpus's comparability
// (internal-docs/design/regime-calibration.md Part 6).
type PlatformRegimeSettings struct {
	Journal RegimeJournalSettings `json:"journal"`
}

// RegimeJournalSettings retains its public name while controlling
// forward collection of typed regime-decision events in daemon.db.
type RegimeJournalSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// PlatformCanarySettings holds the canary evidence-collection runtime
// preference (internal-docs/design/history-index.md).
type PlatformCanarySettings struct {
	Journal CanaryJournalSettings `json:"journal"`
}

// CanaryJournalSettings retains its public name while controlling typed
// canary-decision event collection in daemon.db, mirroring
// RegimeJournalSettings.
type CanaryJournalSettings struct {
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
	// journal-retention effect.
	KeepRawMonths SettingsInt `json:"keep_raw_months"`
}

// PlatformFeatureSettings groups runtime feature preferences.
type PlatformFeatureSettings struct {
	PurgeRestore    PurgeRestoreSettings    `json:"purge_restore"`
	StockProtection StockProtectionSettings `json:"stock_protection"`
	Rulebook        RulebookSettings        `json:"rulebook"`
}

// PurgeRestoreSettings controls purge/restore actions while leaving status
// readable.
type PurgeRestoreSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// StockProtectionSettings controls stock-protection proposal actions without
// enabling broker writes.
type StockProtectionSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// RulebookSettings controls the advisory trading rulebook
// (internal-docs/design/trading-rulebook.md): the 14-rule daily checklist plus its
// manual earnings-date overrides. Disabling hides the SPA card, empties
// rules.snapshot, and stops advisory rule_* preview warnings; it cannot
// affect broker-write gating in either direction.
type RulebookSettings struct {
	Enabled SettingsBool `json:"enabled"`
	// EarningsOverrides maps SYMBOL → "YYYY-MM-DD" (optional "Tamc"/"Tbmo"
	// suffix). Overrides are authoritative over fetched dates for rules
	// 6-8; set a symbol to null to clear it.
	EarningsOverrides SettingsStringMap `json:"earnings_overrides"`
}

// SettingsStringMap is a map-valued setting with the standard
// access/source/reason contract.
type SettingsStringMap struct {
	Value  map[string]string `json:"value,omitempty"`
	Access string            `json:"access"`
	Source string            `json:"source"`
	Reason string            `json:"reason,omitempty"`
}

// PlatformTradingSettings combines the runtime freeze brake with read-only
// trading configuration, build capability, and limits.
type PlatformTradingSettings struct {
	// Freeze is the runtime trading brake: true blocks every new broker
	// write while cancels stay allowed. Toggled via
	// `ibkr settings set trading.freeze=true|false`.
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
// and source metadata.
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
// ObservedAt means no observation is available.
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
