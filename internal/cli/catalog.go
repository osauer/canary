package cli

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"slices"
)

// GuardClass describes whether a command is read-only, changes local state, or
// needs a human confirmation. It is metadata only; existing CLI gates still
// enforce the real safety policy.
type GuardClass string

// Guard classifications used by the command catalog.
const (
	GuardReadOnly GuardClass = "read-only"
	GuardLocal    GuardClass = "local"
	GuardConfirm  GuardClass = "confirm"
)

// HelpGroup buckets commands in the top-level help listing. It is a reading
// aid for the command registry and carries no policy; guard classes remain
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
		{Group: GroupDesk, Title: "Desk", Tagline: "Account, positions, orders and risk"},
		{Group: GroupMarkets, Title: "Markets", Tagline: "Prices, signals and market calendars"},
		{Group: GroupSystem, Title: "System", Tagline: "Run, configure and update Canary"},
	}
}

// FlagSpec is the shared flag metadata used by command-line flag hoisting and
// generated reference documentation. Values is intentionally small and
// enum-like.
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

// CommandSpec is the user-facing command catalog. Name/Summary/Usage are copied
// from Commands() at runtime so the help table and catalog cannot silently
// drift.
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
// listing, generated reference, guard descriptions, and flag-value handling.
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
		out = append(out, spec)
	}
	return out
}

func catalogExtras() map[string]CommandSpec {
	return map[string]CommandSpec{
		"short-interest": {Group: GroupDesk, Flags: flags(valueFlag("min-price", nil), valueFlag("min-avg-dollar-volume-20d", nil), valueFlag("min-average-volume", nil), valueFlag("min-days-to-cover", nil), valueFlag("sort-by", rpc.ShortInterestSortKeys), valueFlag("sort-dir", []string{"asc", "desc"}), valueFlag("limit", nil), valueFlag("exclude", nil), boolFlag("listed-only"), boolFlag("json")), Subcommands: subcommands("screen"), Guard: GuardReadOnly},
		"status":         {Group: GroupDesk, Brief: "Check Canary and the broker connection", Flags: flags(boolFlag("json"))},
		"data":           {Group: GroupMarkets, Brief: "Check data availability and freshness", Flags: flags(valueFlag("offset", nil), valueFlag("limit", nil), valueFlag("revision", nil), boolFlag("json")), Subcommands: subcommands("health", "check"), Guard: GuardReadOnly},
		"account":        {Group: GroupDesk, Brief: "Account value, buying power, cash, margin and daily P&L", Flags: flags(boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"), boolFlag("settlement-probe"), boolFlag("currency-settlement-probe"))},
		"positions":      {Group: GroupDesk, Flags: flags(valueFlag("symbol", nil), valueFlag("type", []string{"stk", "opt"}), valueFlag("sort", []string{"alpha", "pnl", "value"}), boolFlag("quotes"), valueFlag("by", []string{"underlying"}), valueFlag("view", []string{"full", "risk"}), boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"strategies":     {Group: GroupDesk, Brief: "View option strategies; close or reduce them as one order", Flags: flags(valueFlag("units", nil), valueFlag("limit", nil), valueFlag("timeout", nil), boolFlag("submit"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "list", Guard: GuardReadOnly}, {Name: "close", Guard: GuardConfirm}, {Name: "reduce", Guard: GuardConfirm}}, Guard: GuardConfirm},
		"market":         {Group: GroupMarkets, Brief: "Prices, history and daily market activity", Flags: flags(boolFlag("watch"), valueFlag("symbol", nil), valueFlag("range", []string{"1D", "5D", "1M", "6M", "YTD", "1Y", "5Y"}), valueFlag("sessions", nil), boolFlag("explain"), boolFlag("history"), valueFlag("before", nil), valueFlag("exchange", nil), valueFlag("type", []string{"STK", "IND", "CASH", "BILL", "BOND"}), valueFlag("currency", nil), boolFlag("json")), Subcommands: subcommands("tape")},
		"add":            {Group: GroupDesk, Flags: flags(valueFlag("limit", nil), valueFlag("stop-price", nil), valueFlag("quantity", nil), boolFlag("max"), valueFlag("currency", nil), valueFlag("con-id", nil), valueFlag("exchange", nil), valueFlag("adaptive-priority", []string{"Normal", "Patient", "Urgent"}), boolFlag("json")), Subcommands: subcommands("plan", "preview", "market", "call-plan"), Guard: GuardReadOnly},
		"portfolio":      {Group: GroupDesk, Brief: "Holdings by asset class and sector; review the next planned action", Flags: flags(boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "plan", Guard: GuardReadOnly}}},
		"technical":      {Group: GroupMarkets, Brief: "Price trends, relative strength, volatility and trading volume", Flags: flags(valueFlag("benchmark", nil), valueFlag("market", []string{"us", "de", "uk"}), valueFlag("lookback-days", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"locks":          {Group: GroupDesk, Brief: "Keep every held contract of a name out of bulk and automatic reductions", Flags: flags(valueFlag("con-id", nil), valueFlag("role", nil), valueFlag("expected-revision", nil), valueFlag("request-id", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "list", Guard: GuardReadOnly}, {Name: "lock", Guard: GuardLocal}, {Name: "unlock", Guard: GuardLocal}}, Guard: GuardLocal},
		"watchlist":      {Group: GroupMarkets, Brief: "View and edit your saved watchlist", Flags: flags(valueFlag("spec", nil), valueFlag("con-id", nil), valueFlag("expected-revision", nil), valueFlag("request-id", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "list", Guard: GuardReadOnly}, {Name: "add", Guard: GuardLocal}, {Name: "remove", Guard: GuardLocal}, {Name: "replace", Guard: GuardLocal}}, Guard: GuardLocal},
		"setups":         {Group: GroupMarkets, Brief: "Review volume signals, find call options and check prices after entry", Flags: flags(valueFlag("spec", nil), valueFlag("symbol", nil), valueFlag("con-id", nil), valueFlag("at", nil), valueFlag("expiry", nil), valueFlag("strike", nil), valueFlag("session", nil), valueFlag("order-ref", nil), valueFlag("since", nil), boolFlag("json")), Subcommands: subcommands("evaluate", "options", "coverage", "markouts"), Guard: GuardReadOnly},
		"chains":         {Group: GroupMarkets, Brief: "Recorded daily option chains and their coverage", Flags: flags(valueFlag("symbol", nil), valueFlag("date", nil), valueFlag("limit", nil), boolFlag("json"), boolFlag("csv")), Subcommands: subcommands("recorded"), Guard: GuardReadOnly},
		"calendar":       {Group: GroupMarkets, Brief: "Exchange hours, holidays and early closes", Flags: flags(valueFlag("market", []string{"us", "us-options", "de", "uk", "jp", "hk"}), valueFlag("date", nil), valueFlag("at", nil), valueFlag("days", nil), boolFlag("json"))},
		"rules":          {Group: GroupDesk, Brief: "Check trading rules, biggest breaches first; set limits with rules policy", Flags: flags(boolFlag("all"), valueFlag("symbol", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("rule", nil), valueFlag("limit", nil), valueFlag("file", nil), boolFlag("json")), Subcommands: subcommands("history", "policy")},
		"macro":          {Group: GroupMarkets, Brief: "Economic calendar and official releases", Flags: flags(boolFlag("details"), valueFlag("window-start", nil), valueFlag("window-end", nil), boolFlag("json"))},
		"regime":         {Group: GroupMarkets, Brief: "Market conditions across eight indicators", Flags: flags(boolFlag("explain"), valueFlag("view", []string{"full", "monitor"}), boolFlag("profiles"), boolFlag("json"))},
		"stress":         {Group: GroupDesk, Brief: "Check portfolio risks and the evidence behind them", Flags: flags(boolFlag("details"), boolFlag("json"))},
		"mandate":        {Group: GroupDesk, Brief: "Check the book's loss measure against the mandate's loss room", Flags: flags(boolFlag("refresh"), boolFlag("json"))},
		"core":           {Group: GroupDesk, Brief: "See the core sleeve's monthly target, its pending row and what it would trade", Flags: flags(boolFlag("refresh"), valueFlag("isin", nil), valueFlag("currency", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "resolve", Guard: GuardReadOnly}}},
		"strategy":       {Group: GroupDesk, Brief: "See a registered strategy's monthly target and what it would trade, or replay its rule over past month-ends", Flags: flags(boolFlag("refresh"), boolFlag("replay"), boolFlag("json"))},
		"brief":          {Group: GroupDesk, Brief: "Portfolio summary, urgent items first; more with --details", Flags: flags(boolFlag("details"), boolFlag("json")), Guard: GuardReadOnly},
		"edge":           {Group: GroupDesk, Brief: "Review past trading decisions using broker records", Flags: flags(valueFlag("window", []string{"90d", "365d"}), valueFlag("horizon", []string{"1", "5", "20"}), valueFlag("limit", nil), valueFlag("change", nil), valueFlag("option", nil), valueFlag("option-offset", nil), valueFlag("option-limit", nil), boolFlag("all-options"), valueFlag("from", nil), valueFlag("to", nil), valueFlag("rules", nil), boolFlag("json")), Subcommands: subcommands("replay"), Guard: GuardReadOnly},
		"scorecard":      {Group: GroupDesk, Brief: "Score past decisions per strategy, with the evidence and its gates", Flags: flags(valueFlag("sleeve", nil), valueFlag("horizon", []string{"5", "10", "21"}), valueFlag("limit", nil), boolFlag("json")), Guard: GuardReadOnly},
		"lending":        {Group: GroupDesk, Flags: flags(valueFlag("min-rate", nil), valueFlag("min-price", nil), valueFlag("min-avg-dollar-volume-20d", nil), valueFlag("min-high-dates", nil), valueFlag("sort-by", []string{"symbol", "price", "day_change_pct", "volume", "avg_dollar_volume_20d", "ytd_change_pct", "fee_rate", "high_dates"}), valueFlag("sort-dir", []string{"asc", "desc"}), valueFlag("exclude", nil), valueFlag("symbols", nil), valueFlag("window", []string{"90d", "365d"}), valueFlag("from", nil), valueFlag("to", nil), valueFlag("con-id", nil), valueFlag("limit", nil), valueFlag("cursor", nil), valueFlag("fingerprint", nil), boolFlag("json")), Subcommands: subcommands("fees", "rates", "screen", "market"), Guard: GuardReadOnly},
		"proposals":      {Group: GroupDesk, Brief: "Protection proposals to close or reduce positions", Flags: flags(boolFlag("details"), valueFlag("quantity", nil), valueFlag("timeout", nil), boolFlag("fast-path"), boolFlag("prepared-ref-stdin"), boolFlag("bundle-ref-stdin"), valueFlag("reason", nil), valueFlag("percent", []string{"25", "50", "75", "100"}), valueFlag("con-id", nil), boolFlag("include-hedges"), boolFlag("portfolio"), valueFlag("mode", []string{"net", "longs", "all", "hedge"}), valueFlag("sector", nil), valueFlag("min-beta", nil), boolFlag("plan"), valueFlag("proposal", nil), valueFlag("adaptive-priority", []string{"Normal", "Patient", "Urgent", "none"}), valueFlag("preview-token", nil), valueFlag("request-ref", nil), boolFlag("submit"), boolFlag("json"), boolFlag("all"), boolFlag("live"), boolFlag("stdin"), boolFlag("prepared-only")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "prepare", Guard: GuardReadOnly}, {Name: "prepared-status", Guard: GuardReadOnly}, {Name: "prepare-bundle", Guard: GuardReadOnly}, {Name: "submit-bundle", Guard: GuardConfirm}, {Name: "bundle-status", Guard: GuardReadOnly}, {Name: "submit", Guard: GuardConfirm}, {Name: "reduce", Guard: GuardConfirm}, {Name: "request-stop", Guard: GuardLocal}, {Name: "ignore", Guard: GuardLocal}, {Name: "veto", Guard: GuardLocal}, {Name: "queue", Guard: GuardConfirm}}, Guard: GuardConfirm},
		"opportunities":  {Group: GroupDesk, Brief: "Option exercise opportunities", Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), valueFlag("reason", nil), valueFlag("preview-token", nil), valueFlag("evidence", nil), boolFlag("confirm-complete"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "exercise", Guard: GuardConfirm}, {Name: "exercises", Guard: GuardReadOnly}, {Name: "reconcile-exercise", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"trading":        {Group: GroupDesk, Flags: flags(boolFlag("json")), Subcommands: subcommands("status")},
		"policy":         {Group: GroupDesk, Brief: "Risk limits, capital and losses from the portfolio peak", Flags: flags(boolFlag("explain"), valueFlag("amount", nil), valueFlag("effective-at", nil), valueFlag("note", nil), valueFlag("control", nil), valueFlag("reason", nil), valueFlag("hours", nil), valueFlag("report", nil), valueFlag("peak", nil), boolFlag("from-statements"), boolFlag("dry-run"), valueFlag("apply-plan", nil), valueFlag("config", nil), boolFlag("offline"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "check", Guard: GuardReadOnly}, {Name: "capital-event", Guard: GuardConfirm}, {Name: "override", Guard: GuardConfirm}, {Name: "reset-drawdown", Guard: GuardConfirm}, {Name: "correct-peak", Guard: GuardConfirm}, {Name: "default", Guard: GuardLocal}, {Name: "ensure", Guard: GuardLocal}}, Guard: GuardReadOnly},
		"recon":          {Group: GroupDesk, Brief: "Compare broker statements with recorded deposits and withdrawals", Flags: flags(boolFlag("refresh"), valueFlag("line", nil), valueFlag("reason", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "show", Guard: GuardReadOnly}, {Name: "backtest", Guard: GuardReadOnly}, {Name: "equity", Guard: GuardReadOnly}, {Name: "dismiss", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		"reporting":      {Group: GroupDesk, Brief: "Check broker statement setup, performance, currency data and execution costs", Flags: flags(boolFlag("json"), boolFlag("backfill"), valueFlag("month", nil)), Subcommands: subcommands("status", "performance", "fx", "costs"), Guard: GuardReadOnly},
		// settings set stays GuardConfirm: it writes runtime preferences, and
		// trading.freeze is accepted only from an interactive human terminal.
		"settings": {Group: GroupSystem, Flags: flags(boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "set", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		"orders":   {Group: GroupDesk, Brief: "View open orders and locally recorded order history", Flags: flags(valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), valueFlag("event-limit", nil), boolFlag("json")), Subcommands: subcommands("open", "history")},
		"order":    {Group: GroupDesk, Flags: flags(valueFlag("limit", nil), valueFlag("strategy", []string{"patient-limit", "explicit-limit"}), valueFlag("adaptive-priority", []string{"Normal", "Patient", "Urgent"}), valueFlag("order-type", []string{"LMT", "TRAIL", "TRAIL-LIMIT"}), valueFlag("trail-percent", nil), valueFlag("trail-amount", nil), valueFlag("initial-stop", nil), valueFlag("limit-offset", nil), valueFlag("trigger-method", []string{"1", "2", "3", "4", "7", "8"}), valueFlag("tif", []string{"DAY", "GTC"}), boolFlag("outside-rth"), valueFlag("replace-order", nil), valueFlag("timeout", nil), valueFlag("market", []string{"us", "de", "uk"}), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), valueFlag("type", []string{"BOND", "BILL"}), valueFlag("preview-token", nil), valueFlag("quantity", nil), valueFlag("terms", nil), valueFlag("request-id", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "preview", Guard: GuardReadOnly}, {Name: "status", Guard: GuardReadOnly}, {Name: "place", Guard: GuardConfirm}, {Name: "modify", Guard: GuardConfirm}, {Name: "cancel", Guard: GuardConfirm}, {Name: "tws-cancel", Guard: GuardConfirm}, {Name: "tws-modify", Guard: GuardConfirm}}},
		"app":      {Group: GroupSystem, Flags: flags(valueFlag("addr", nil), valueFlag("public-url", nil), valueFlag("state-dir", nil), boolFlag("remote"), valueFlag("remote-url", nil), valueFlag("keep-days", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "pair", Guard: GuardLocal}, {Name: "serve", Guard: GuardLocal}, {Name: "devices", Guard: GuardReadOnly}, {Name: "push-test", Guard: GuardLocal}, {Name: "restart", Guard: GuardConfirm}}, Guard: GuardLocal},
		"mcp":      {Group: GroupSystem, Flags: flags(valueFlag("profile", []string{"full", "monitor"})), Guard: GuardLocal},
		"daemon":   {Group: GroupSystem, Flags: flags(boolFlag("foreground"), boolFlag("version"), valueFlag("config", nil), valueFlag("socket", nil), valueFlag("log", nil)), Guard: GuardLocal},
		"setup":    {Group: GroupSystem, Flags: flags(boolFlag("accept-unproved"), boolFlag("no-restart")), Subcommands: []SubcommandSpec{{Name: "claude-desktop", Guard: GuardLocal}, {Name: "app", Guard: GuardLocal}, {Name: "reporting", Guard: GuardLocal}}, Guard: GuardLocal},
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
