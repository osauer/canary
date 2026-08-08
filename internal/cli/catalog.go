package cli

import "slices"

// GuardClass describes whether a command can run directly inside the TUI or
// needs a human confirmation first. It is metadata only; existing CLI gates
// still enforce the real safety policy.
type GuardClass string

// Guard classifications used by the command catalog.
const (
	GuardReadOnly GuardClass = "read-only"
	GuardLocal    GuardClass = "local"
	GuardConfirm  GuardClass = "confirm"
)

// TUISupport describes how the full-screen terminal app should handle a
// command. External commands are advertised for discovery but should be run in
// a regular terminal because they own a process, stdio stream, or installer
// lifecycle outside the TUI's prompt/output model.
type TUISupport string

// TUI support classifications used by the command catalog.
const (
	TUISupported TUISupport = "supported"
	TUIExternal  TUISupport = "external"
)

// HelpGroup buckets commands in the top-level help listing. It is a reading
// aid for a 36-command registry and carries no policy; guard classes remain
// the only statement about what a command may do.
type HelpGroup string

// Help groups used by the command catalog.
const (
	GroupDesk    HelpGroup = "desk"
	GroupMarkets HelpGroup = "markets"
	GroupSystem  HelpGroup = "system"
)

// HelpGroupSpec is one heading in the top-level help listing.
type HelpGroupSpec struct {
	Group   HelpGroup
	Title   string
	Tagline string
}

// HelpGroups returns the listing groups in render order. Commands keep
// registry order inside their group, so `status` stays the first line of
// the first group.
func HelpGroups() []HelpGroupSpec {
	return []HelpGroupSpec{
		{Group: GroupDesk, Title: "Desk", Tagline: "the account, its positions, orders, and risk"},
		{Group: GroupMarkets, Title: "Markets", Tagline: "quotes, chains, screens, and regime research"},
		{Group: GroupSystem, Title: "System", Tagline: "running Canary itself"},
	}
}

// FlagSpec is the shared flag metadata used by command-line flag hoisting and
// TUI completion. Values is intentionally small and enum-like; dynamic
// completion (symbols, watchlist names) lives in the TUI layer.
type FlagSpec struct {
	Name       string
	TakesValue bool
	Values     []string
	Summary    string
}

// SubcommandSpec captures nested command words that are useful for completion
// and guard classification. The existing handlers remain authoritative for
// parsing and validation.
type SubcommandSpec struct {
	Name  string
	Guard GuardClass
}

// CommandSpec is the user-facing command catalog shared by the one-shot CLI
// and the TUI. Name/Summary/Usage are copied from Commands() at runtime so the
// help table and catalog cannot silently drift.
type CommandSpec struct {
	Name    string
	Summary string
	// Brief is the listing line for commands whose Summary is too long for
	// a terminal row. `canary <command> --help` and the generated CLI
	// reference always render Summary, so the detail is moved, not lost.
	Brief       string
	Usage       string
	Flags       []FlagSpec
	Subcommands []SubcommandSpec
	Guard       GuardClass
	TUI         TUISupport
	Group       HelpGroup
}

// listing returns the one-line summary the top-level help shows.
func (s CommandSpec) listing() string {
	if s.Brief != "" {
		return s.Brief
	}
	return s.Summary
}

// Catalog returns the registered commands with shared metadata for the help
// listing, completion, TUI guard decisions, and flag-value handling.
func Catalog() []CommandSpec {
	extras := catalogExtras()
	out := make([]CommandSpec, 0, len(commands))
	for _, cmd := range commands {
		spec := extras[cmd.Name]
		spec.Name = cmd.Name
		spec.Summary = cmd.Summary
		spec.Usage = cmd.Usage
		if spec.Guard == "" {
			spec.Guard = GuardReadOnly
		}
		if spec.TUI == "" {
			spec.TUI = TUISupported
		}
		out = append(out, spec)
	}
	return out
}

func catalogExtras() map[string]CommandSpec {
	return map[string]CommandSpec{
		"status":    {Group: GroupDesk, Flags: flags(boolFlag("json"))},
		"account":   {Group: GroupDesk, Flags: flags(boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"positions": {Group: GroupDesk, Flags: flags(valueFlag("symbol", nil), valueFlag("type", []string{"stk", "opt"}), valueFlag("sort", []string{"alpha", "pnl", "value"}), boolFlag("quotes"), valueFlag("by", []string{"underlying"}), valueFlag("view", []string{"full", "risk"}), boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"quote":     {Group: GroupMarkets, Flags: flags(valueFlag("market", []string{"us", "de"}), boolFlag("watch"), valueFlag("rate", nil), valueFlag("timeout", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"watch": {
			Group: GroupMarkets,
			Brief: "Local watchlist: add, remove, or quote the list live",
			Flags: flags(boolFlag("quotes"), valueFlag("timeout", nil), boolFlag("json"), boolFlag("list"), boolFlag("watch"), valueFlag("rate", nil), boolFlag("add"), boolFlag("remove"), boolFlag("clear")),
			// add/remove/clear edit the saved watchlist, so they are local
			// writes rather than reads; the subcommands() helper would stamp
			// all four read-only.
			Subcommands: []SubcommandSpec{{Name: "add", Guard: GuardLocal}, {Name: "remove", Guard: GuardLocal}, {Name: "list", Guard: GuardReadOnly}, {Name: "clear", Guard: GuardLocal}},
			Guard:       GuardLocal,
		},
		"calendar":      {Group: GroupMarkets, Brief: "Official trading sessions for US equities, options, Xetra", Flags: flags(valueFlag("market", []string{"us", "us-options", "de"}), valueFlag("date", nil), valueFlag("next", nil), boolFlag("json"))},
		"chain":         {Group: GroupMarkets, Flags: flags(valueFlag("expiry", nil), valueFlag("width", nil), valueFlag("side", []string{"calls", "puts", "both"}), valueFlag("class", []string{"SPX", "SPXW"}), boolFlag("no-iv"), boolFlag("all-expiries"), boolFlag("require-live-iv"), valueFlag("min-dte", nil), valueFlag("max-dte", nil), valueFlag("target-dte", nil), boolFlag("json"))},
		"history":       {Group: GroupMarkets, Flags: flags(valueFlag("days", nil), boolFlag("json"))},
		"technical":     {Group: GroupMarkets, Flags: flags(valueFlag("benchmark", nil), valueFlag("market", []string{"us", "de"}), valueFlag("lookback-days", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"market-events": {Group: GroupMarkets, Brief: "Borrow, Reg SHO, LULD, and halt flags for held symbols", Flags: flags(valueFlag("symbol", nil), boolFlag("json"))},
		"breadth":       {Group: GroupMarkets, Brief: "S&P 500 breadth: % above 50/200-DMA, new highs and lows", Flags: flags(valueFlag("days", nil), boolFlag("json"))},
		"gamma":         {Group: GroupMarkets, Brief: "SPX-canonical dealer zero-gamma with SPY context", Flags: flags(boolFlag("no-wait"), boolFlag("force"), valueFlag("only", []string{"spy", "spx"}), boolFlag("explain"), boolFlag("diagnostics"), boolFlag("profiles"), boolFlag("json"))},
		"regime":        {Group: GroupMarkets, Brief: "Broad-market stress lifecycle across six input clusters", Flags: flags(boolFlag("explain"), boolFlag("diagnostics"), boolFlag("watch"), valueFlag("rate", nil), valueFlag("log", nil), valueFlag("view", []string{"detail", "monitor"}), valueFlag("since", nil), valueFlag("until", nil), valueFlag("stage", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: subcommands("history")},
		"rules":         {Group: GroupDesk, Brief: "Advisory 14-rule daily checklist, hardest breach first", Flags: flags(boolFlag("all"), valueFlag("symbol", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("rule", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: subcommands("history")},
		"alerts":        {Group: GroupDesk, Brief: "Alert-source coverage and disclosed per-rule gaps", Flags: flags(boolFlag("json")), Guard: GuardReadOnly},
		"stress":        {Group: GroupMarkets, Brief: "Portfolio-aware stress read with action and evidence", Flags: flags(boolFlag("details"), valueFlag("view", []string{"full", "alert"}), valueFlag("since", nil), valueFlag("until", nil), valueFlag("severity", nil), valueFlag("action", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: subcommands("history")},
		"brief":         {Group: GroupDesk, Brief: "Morning and end-of-day operator brief", Flags: flags(boolFlag("json"), valueFlag("kind", []string{"morning", "eod"})), Guard: GuardReadOnly},
		"proposals":     {Group: GroupDesk, Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), boolFlag("fast-path"), valueFlag("reason", nil), valueFlag("percent", []string{"25", "50", "75", "100"}), valueFlag("con-id", nil), boolFlag("include-hedges"), boolFlag("portfolio"), boolFlag("submit"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "submit", Guard: GuardConfirm}, {Name: "reduce", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"opportunities": {Group: GroupDesk, Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), valueFlag("reason", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "exercise", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"purge":         {Group: GroupDesk, Brief: "Emergency fast-path close of open positions", Flags: flags(boolFlag("all"), boolFlag("json"), valueFlag("timeout", nil), boolFlag("watch"), valueFlag("rate", nil), valueFlag("scale", nil), boolFlag("record"), boolFlag("save"), boolFlag("execute"), valueFlag("wait", nil), valueFlag("account", nil)), Subcommands: []SubcommandSpec{{Name: "dry-run", Guard: GuardReadOnly}, {Name: "status", Guard: GuardReadOnly}, {Name: "monitor", Guard: GuardReadOnly}, {Name: "restore", Guard: GuardReadOnly}, {Name: "execute", Guard: GuardConfirm}}, Guard: GuardConfirm},
		"backtest":      {Group: GroupMarkets, Brief: "Offline backtest harness over local JSONL snapshots", Flags: flags(valueFlag("input", nil), valueFlag("max-slots", nil), valueFlag("bars", nil), valueFlag("bars-manifest", nil), valueFlag("symbols", nil), valueFlag("preset", nil), valueFlag("type", nil), valueFlag("market", []string{"us", "de"}), valueFlag("exchange", nil), valueFlag("instrument", nil), valueFlag("limit", nil), valueFlag("min-price", nil), valueFlag("min-volume", nil), valueFlag("min-dollar-volume", nil), boolFlag("require-live"), boolFlag("exclude-penny"), boolFlag("include-etfs"), boolFlag("include-regime"), valueFlag("split", []string{"holdout", "tuning"}), valueFlag("holdout-plan", nil), valueFlag("market-cluster", nil), valueFlag("theme", nil), valueFlag("benchmark", nil), valueFlag("horizon-days", nil), valueFlag("round-trip-cost-bps", nil), valueFlag("cost-model", nil), valueFlag("lookback-days", nil), valueFlag("append", nil), valueFlag("target-policy", []string{"net-excess-positive"}), valueFlag("start-date", nil), valueFlag("end-date", nil), valueFlag("holdout-start-date", nil), valueFlag("sample-step-bars", nil), valueFlag("plan", nil), boolFlag("list-plans"), boolFlag("json")), Subcommands: subcommands("stress", "regime", "opportunity", "research-opportunity", "build-regime", "build-opportunity", "build-opportunity-pit", "score-opportunity", "capture-opportunity", "export-opportunity-bars"), Guard: GuardLocal},
		"scan":          {Group: GroupMarkets, Brief: "Run a scanner preset or an ad-hoc market scan", Flags: flags(valueFlag("type", nil), valueFlag("exchange", nil), valueFlag("instrument", nil), valueFlag("limit", nil), valueFlag("min-price", nil), valueFlag("min-volume", nil), valueFlag("min-dollar-volume", nil), boolFlag("require-live"), boolFlag("exclude-penny"), boolFlag("raw"), boolFlag("json")), Subcommands: subcommands("list", "params")},
		"size":          {Group: GroupDesk, Flags: flags(valueFlag("symbol", nil), valueFlag("entry", nil), valueFlag("stop", nil), valueFlag("target", nil), valueFlag("risk-pct", nil), valueFlag("side", []string{"long", "short"}), valueFlag("lot", nil), valueFlag("fx", nil), boolFlag("json"))},
		"trading":       {Group: GroupDesk, Flags: flags(valueFlag("timeout", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "paper-smoke", Guard: GuardConfirm}}},
		"policy":        {Group: GroupDesk, Brief: "Risk constitution: limits, capital and drawdown state", Flags: flags(boolFlag("explain"), valueFlag("amount", nil), valueFlag("effective-at", nil), valueFlag("note", nil), valueFlag("control", nil), valueFlag("reason", nil), valueFlag("hours", nil), valueFlag("report", nil), valueFlag("peak", nil), boolFlag("from-statements"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "capital-event", Guard: GuardConfirm}, {Name: "override", Guard: GuardConfirm}, {Name: "reset-drawdown", Guard: GuardConfirm}, {Name: "correct-peak", Guard: GuardConfirm}, {Name: "artefact", Guard: GuardConfirm}, {Name: "default", Guard: GuardLocal}}, Guard: GuardReadOnly},
		"recon":         {Group: GroupDesk, Brief: "Post-trade reconciliation against the capital ledger", Flags: flags(boolFlag("refresh"), valueFlag("line", nil), valueFlag("reason", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "backtest", Guard: GuardReadOnly}, {Name: "equity", Guard: GuardReadOnly}, {Name: "dismiss", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		// settings set stays GuardConfirm: it writes runtime preferences, and
		// trading.freeze plus the trading-limit keys are accepted precisely
		// because the TUI is an interactive human terminal.
		"settings": {Group: GroupSystem, Flags: flags(boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "set", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		"orders":   {Group: GroupDesk, Brief: "Read the local order journal; never transmits", Flags: flags(valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), valueFlag("event-limit", nil), boolFlag("json")), Subcommands: subcommands("open", "history")},
		"order":    {Group: GroupDesk, Flags: flags(valueFlag("limit", nil), valueFlag("strategy", []string{"patient-limit", "explicit-limit", "broker-trail"}), valueFlag("order-type", []string{"LMT", "TRAIL", "TRAIL-LIMIT"}), valueFlag("trail-percent", nil), valueFlag("trail-amount", nil), valueFlag("initial-stop", nil), valueFlag("limit-offset", nil), valueFlag("trigger-method", []string{"1", "2", "3", "4", "7", "8"}), valueFlag("tif", []string{"DAY", "GTC"}), boolFlag("outside-rth"), valueFlag("replace-order", nil), valueFlag("timeout", nil), valueFlag("market", []string{"us", "de"}), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), valueFlag("preview-token", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "preview", Guard: GuardReadOnly}, {Name: "status", Guard: GuardReadOnly}, {Name: "place", Guard: GuardConfirm}, {Name: "modify", Guard: GuardConfirm}, {Name: "cancel", Guard: GuardConfirm}}},
		"app":      {Group: GroupSystem, Flags: flags(valueFlag("addr", nil), valueFlag("public-url", nil), valueFlag("state-dir", nil), boolFlag("remote"), valueFlag("remote-url", nil), valueFlag("keep-days", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "pair", Guard: GuardLocal}, {Name: "serve", Guard: GuardLocal}, {Name: "devices", Guard: GuardReadOnly}, {Name: "restart", Guard: GuardConfirm}}, Guard: GuardLocal, TUI: TUIExternal},
		"mcp":      {Group: GroupSystem, Flags: flags(valueFlag("profile", []string{"full", "monitor"})), Guard: GuardLocal, TUI: TUIExternal},
		"daemon":   {Group: GroupSystem, Flags: flags(boolFlag("foreground"), boolFlag("version"), valueFlag("config", nil), valueFlag("socket", nil), valueFlag("log", nil)), Guard: GuardLocal, TUI: TUIExternal},
		"setup":    {Group: GroupSystem, Guard: GuardLocal, TUI: TUIExternal},
		"update":   {Group: GroupSystem, Flags: flags(boolFlag("check"), boolFlag("force"), boolFlag("restart"), boolFlag("no-restart")), Guard: GuardConfirm},
		"restart":  {Group: GroupSystem, Flags: flags(boolFlag("app"), boolFlag("force"), valueFlag("timeout", nil), valueFlag("addr", nil), valueFlag("public-url", nil), boolFlag("remote"), valueFlag("remote-url", nil), valueFlag("state-dir", nil), boolFlag("json")), Guard: GuardConfirm},
		"stop":     {Group: GroupSystem, Flags: flags(boolFlag("app"), boolFlag("daemon"), boolFlag("force"), boolFlag("yes"), valueFlag("timeout", nil), boolFlag("json")), Guard: GuardConfirm},
		"version":  {Group: GroupSystem, Guard: GuardLocal},
	}
}

func boolFlag(name string) FlagSpec {
	return FlagSpec{Name: name}
}

func valueFlag(name string, values []string) FlagSpec {
	return FlagSpec{Name: name, TakesValue: true, Values: slices.Clone(values)}
}

func flags(in ...FlagSpec) []FlagSpec {
	return append([]FlagSpec(nil), in...)
}

func subcommands(names ...string) []SubcommandSpec {
	out := make([]SubcommandSpec, 0, len(names))
	for _, name := range names {
		out = append(out, SubcommandSpec{Name: name, Guard: GuardReadOnly})
	}
	return out
}
