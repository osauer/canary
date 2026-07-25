package rpc

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
	// `ibkr settings set --help` and the generated configuration reference.
	Doc string
}

// SettingsKeys returns the registry in stable display order.
func SettingsKeys() []SettingsKeySpec {
	return []SettingsKeySpec{
		{
			Key: "features.purge_restore.enabled", Kind: SettingsKindBool, Class: SettingsClassRuntime,
			Doc: "Controls the purge/restore workflow/read surface while purge status stays readable; true does not authorize preview or broker submission, which remains unavailable until exact per-leg portfolio and account-global working-order authority exists (default true).",
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
