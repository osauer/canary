package cli

import "slices"

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
		{Group: GroupDesk, Title: "Desk", Tagline: "the account, its positions, orders, and risk"},
		{Group: GroupMarkets, Title: "Markets", Tagline: "named-symbol technical evidence"},
		{Group: GroupSystem, Title: "System", Tagline: "running Canary itself"},
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
		"status":        {Group: GroupDesk, Flags: flags(boolFlag("json"))},
		"account":       {Group: GroupDesk, Flags: flags(boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"positions":     {Group: GroupDesk, Flags: flags(valueFlag("symbol", nil), valueFlag("type", []string{"stk", "opt"}), valueFlag("sort", []string{"alpha", "pnl", "value"}), boolFlag("quotes"), valueFlag("by", []string{"underlying"}), valueFlag("view", []string{"full", "risk"}), boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"strategies":    {Group: GroupDesk, Flags: flags(valueFlag("units", nil), valueFlag("limit", nil), valueFlag("timeout", nil), boolFlag("submit"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "list", Guard: GuardReadOnly}, {Name: "close", Guard: GuardConfirm}, {Name: "reduce", Guard: GuardConfirm}}, Guard: GuardConfirm},
		"technical":     {Group: GroupMarkets, Flags: flags(valueFlag("benchmark", nil), valueFlag("market", []string{"us", "de"}), valueFlag("lookback-days", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"rules":         {Group: GroupDesk, Brief: "Advisory 14-rule daily checklist, hardest breach first", Flags: flags(boolFlag("all"), valueFlag("symbol", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("rule", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: subcommands("history")},
		"brief":         {Group: GroupDesk, Brief: "Combined post- and pre-trade operator brief", Flags: flags(boolFlag("json")), Guard: GuardReadOnly},
		"proposals":     {Group: GroupDesk, Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), boolFlag("fast-path"), valueFlag("reason", nil), valueFlag("percent", []string{"25", "50", "75", "100"}), valueFlag("con-id", nil), boolFlag("include-hedges"), boolFlag("portfolio"), boolFlag("submit"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "submit", Guard: GuardConfirm}, {Name: "reduce", Guard: GuardConfirm}, {Name: "request-stop", Guard: GuardLocal}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"opportunities": {Group: GroupDesk, Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), valueFlag("reason", nil), valueFlag("preview-token", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "exercise", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"trading":       {Group: GroupDesk, Flags: flags(boolFlag("json")), Subcommands: subcommands("status")},
		"policy":        {Group: GroupDesk, Brief: "Risk constitution: limits, capital and drawdown state", Flags: flags(boolFlag("explain"), valueFlag("amount", nil), valueFlag("effective-at", nil), valueFlag("note", nil), valueFlag("control", nil), valueFlag("reason", nil), valueFlag("hours", nil), valueFlag("report", nil), valueFlag("peak", nil), boolFlag("from-statements"), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "capital-event", Guard: GuardConfirm}, {Name: "override", Guard: GuardConfirm}, {Name: "reset-drawdown", Guard: GuardConfirm}, {Name: "correct-peak", Guard: GuardConfirm}, {Name: "default", Guard: GuardLocal}}, Guard: GuardReadOnly},
		"recon":         {Group: GroupDesk, Brief: "Post-trade reconciliation against the capital ledger", Flags: flags(boolFlag("refresh"), valueFlag("line", nil), valueFlag("reason", nil), valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "backtest", Guard: GuardReadOnly}, {Name: "equity", Guard: GuardReadOnly}, {Name: "dismiss", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		// settings set stays GuardConfirm: it writes runtime preferences, and
		// trading.freeze plus the trading-limit keys are accepted only from an
		// interactive human terminal.
		"settings": {Group: GroupSystem, Flags: flags(boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "show", Guard: GuardReadOnly}, {Name: "set", Guard: GuardConfirm}}, Guard: GuardReadOnly},
		"orders":   {Group: GroupDesk, Brief: "Read the local order journal; never transmits", Flags: flags(valueFlag("since", nil), valueFlag("until", nil), valueFlag("limit", nil), valueFlag("event-limit", nil), boolFlag("json")), Subcommands: subcommands("open", "history")},
		"order":    {Group: GroupDesk, Flags: flags(boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "cancel", Guard: GuardConfirm}}},
		"app":      {Group: GroupSystem, Flags: flags(valueFlag("addr", nil), valueFlag("public-url", nil), valueFlag("state-dir", nil), boolFlag("remote"), valueFlag("remote-url", nil), valueFlag("keep-days", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "pair", Guard: GuardLocal}, {Name: "serve", Guard: GuardLocal}, {Name: "devices", Guard: GuardReadOnly}, {Name: "restart", Guard: GuardConfirm}}, Guard: GuardLocal},
		"mcp":      {Group: GroupSystem, Flags: flags(valueFlag("profile", []string{"full", "monitor"})), Guard: GuardLocal},
		"daemon":   {Group: GroupSystem, Flags: flags(boolFlag("foreground"), boolFlag("version"), valueFlag("config", nil), valueFlag("socket", nil), valueFlag("log", nil)), Guard: GuardLocal},
		"setup":    {Group: GroupSystem, Guard: GuardLocal},
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
