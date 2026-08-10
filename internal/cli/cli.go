// Package cli adapts typed daemon contracts and local workflows into Canary
// commands, machine-readable output, and terminal rendering. Broker-connected
// for broker state, policy decisions, and gated broker writes.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/rpc"
	"golang.org/x/sys/unix"
)

// DaemonConn is the CLI's typed daemon-call surface. *dial.Conn implements
type DaemonConn interface {
	Call(context.Context, string, any, any) error
	Stream(context.Context, string, any, func(json.RawMessage) error) error
}

// Env is the per-invocation context shared by every subcommand.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is the interactive input used for live-write confirmation
	Stdin io.Reader
	Conn  DaemonConn
	// Origin is this process's broker-write origin classification
	// Empty classifies as agent at the daemon (fail closed).
	Origin string
	// Version is the running CLI version stamped by cmd/canary. Empty in
	// renderer tests and local-only helper paths that do not need parity
	// checks against the daemon.
	Version string
	// Color is true when ANSI color escapes should be emitted on Stdout.
	Color bool
}

// CommandFunc is the signature implemented by every subcommand handler.
type CommandFunc func(ctx context.Context, env *Env, args []string) int

// Run dispatches the subcommand named by cmd. Returns the process exit code.
// flag package stops at the first non-flag token, but users naturally write
func Run(ctx context.Context, env *Env, cmd string, args []string) int {
	c, ok := lookupCommand(cmd)
	if !ok || c.Fn == nil {
		fmt.Fprintf(env.Stderr, "%s: unknown subcommand %q\n\n", productidentity.Executable, cmd)
		PrintUsage(env.Stderr)
		return 2
	}
	return c.Fn(ctx, env, hoistFlags(args))
}

// parseExit converts a *flag.FlagSet.Parse error into a process exit code.
func parseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func helpArg(arg string) bool {
	switch arg {
	case "help", "--help", "-h", "-help":
		return true
	default:
		return false
	}
}

func printCommandUsage(env *Env, name string) int {
	fs := flagSet(env, name)
	fs.Usage()
	return 0
}

// hoistFlags moves -flag and --flag tokens (and their values, if separate)
// Long-form `--flag=value` is treated as a single token.
func hoistFlags(in []string) []string {
	flags, positional := []string{}, []string{}
	skipNext := false
	for i, a := range in {
		if skipNext {
			skipNext = false
			flags = append(flags, a)
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			// Detect "--flag value" (value on next token) vs "--flag=value".
			if !strings.Contains(a, "=") && i+1 < len(in) && !strings.HasPrefix(in[i+1], "-") {
				// Heuristic: only treat next as value if the flag is one of the
				// known value-taking flags. False positives are tolerable since
				// runQuote's positional parser re-checks shape.
				if isValueFlag(strings.TrimLeft(a, "-")) {
					skipNext = true
				}
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func isValueFlag(name string) bool {
	name = strings.TrimLeft(name, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	for _, cmd := range Catalog() {
		for _, f := range cmd.Flags {
			if f.Name == name && f.TakesValue {
				return true
			}
		}
	}
	return false
}

func outputColumns(w io.Writer) int {
	if raw := strings.TrimSpace(os.Getenv("COLUMNS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n >= 40 {
			return n
		}
	}
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil || ws.Col < 40 {
		return 0
	}
	return int(ws.Col)
}

// runWatch is the one shared polling loop retained for account and position
func runWatch(ctx context.Context, env *Env, rate time.Duration, label string, render func(io.Writer) int) int {
	if rate <= 0 {
		rate = time.Second
	}
	ticker := time.NewTicker(rate)
	defer ticker.Stop()
	first := true
	lastErr := 0
	for {
		var buf bytes.Buffer
		if code := render(&buf); code != 0 {
			if first {
				_, _ = env.Stdout.Write(buf.Bytes())
				return code
			}
			lastErr = code
		} else {
			lastErr = 0
			if isTerminal(env.Stdout) {
				fmt.Fprint(env.Stdout, "\x1b[2J\x1b[H")
			} else if !first {
				fmt.Fprintln(env.Stdout, env.dim(strings.Repeat("─", 60)))
			}
			_, _ = env.Stdout.Write(buf.Bytes())
			if isTerminal(env.Stdout) {
				fmt.Fprintf(env.Stdout, "  %s · refresh every %s · Ctrl-C to stop\n", env.dim(label), rate)
			}
			first = false
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
		}
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Command bundles a subcommand's name, one-line summary, optional usage
type Command struct {
	Name    string
	Summary string
	Usage   string // optional one-line usage example shown in `canary X --help`
	Fn      CommandFunc
}

// commands is populated in init() to break the package-init cycle that
// would otherwise form: var → handler → flagSet → lookupCommand → var.
// Order is load-bearing for the help table (status first).
var commands []Command

func init() {
	commands = []Command{
		{"status", "Daemon + gateway health (run this first if anything fails)", "canary status [--json]", runStatus},
		{"account", "Account summary snapshot (NLV, BP, cash, margin, daily P&L)", "canary account [--watch --rate 1s] [--json]", runAccount},
		{"positions", "List open positions (stocks + options)", "canary positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--quotes] [--by underlying] [--watch --rate 1s] [--json]", runPositions},
		{"technical", "Trend, relative strength, ATR, and liquidity from daily bars", "canary technical SYM[,SYM...] [--benchmark SPY] [--market us|de] [--json]", runTechnical},
		{"brief", "Combined post- and pre-trade operator brief with disclosed source degradation", "canary brief [--json]", runBrief},
		{"rules", "Advisory 14-rule daily trading checklist, hardest breach first", "canary rules [--all] [--symbol SYM] [--json] | canary rules history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--rule ID] [--limit N] [--json]", runRules},
		{"policy", "Risk constitution: effective limits, capital/drawdown state, overrides (human-only writes)", "canary policy show [--explain] [--json] | canary policy capital-event deposit|withdrawal [--amount F] [--effective-at TIME] [--note S] | canary policy capital-event reconcile [--report ID] | canary policy override --control KEY --reason S --hours N | canary policy reset-drawdown --reason S | canary policy correct-peak (--from-statements|--peak F) --reason S | canary policy default protection|opportunity", runPolicy},
		{"recon", "Post-trade reconciliation: broker statement flows vs the declared capital ledger", "canary recon show [--refresh] [--json] | canary recon backtest [--refresh] [--json] | canary recon equity [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--limit N] [--json] | canary recon dismiss --line ID --reason S", runRecon},
		{"proposals", "Daemon-owned close/reduce-only protection proposals", "canary proposals status|refresh|list|preview|submit|reduce|ignore [--json]", runProposals},
		{"opportunities", "Daemon-owned option exercise opportunities", "canary opportunities status|refresh|list|preview|exercise|ignore [--json]", runOpportunities},
		{"trading", "Local trading gate status and configuration", "canary trading status [--json]", runTrading},
		{"settings", "Runtime platform preferences and observed read-only state", "canary settings show [--json] | canary settings set <supported-key>=true|false|null|number", runSettings},
		{"orders", "Read current-context local order lifecycle state without transmitting orders", "canary orders open [--json] | canary orders history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--limit N] [--event-limit N] [--json]", runOrders},
		{"order", "Inspect or cancel a Canary-owned order", "canary order status ID [--json] | canary order cancel ID [--json]", runOrder},
		{"app", "Run the paired mobile PWA application layer", "canary app [--addr HOST:PORT] | canary app pair", nil},                                                // dispatched in cmd/canary/main.go — long-lived app server
		{"mcp", "Run the stdio MCP server for local AI clients", "canary mcp", nil},                                                                                   // dispatched in cmd/canary/main.go — long-lived stdio server
		{"daemon", "Run the stateful gateway daemon (normally autospawned)", "canary daemon [--foreground] [--config PATH] [--socket PATH] [--log PATH|stderr]", nil}, // dispatched in cmd/canary/main.go — long-lived daemon
		{"setup", "Wire Canary into a local AI client (default: claude-desktop)", "canary setup [claude-desktop]", nil},                                               // dispatched in cmd/canary/main.go — no daemon contact
		{"update", "Self-update the Canary binary from the latest GitHub release", "canary update [--check] [--force] [--restart|--no-restart]", nil},                 // dispatched in cmd/canary/main.go — no daemon contact
		{"restart", "Gracefully restart the daemon and any running app process", "canary restart [--app] [--force] [--timeout 15s] [--json]", nil},                    // dispatched in cmd/canary/main.go — process management
		{"stop", "Stop the local daemon and app processes", "canary stop [--app] [--daemon] [--force] [--timeout 15s] [--yes] [--json]", nil},                         // dispatched in cmd/canary/main.go — process management
		{"version", "Print version, commit, build date", "canary version", nil},                                                                                       // version is handled in cmd/canary/main.go before dispatch
	}
	for i := range commands {
		commands[i].Usage = strings.ReplaceAll(commands[i].Usage, "canary ", productidentity.Executable+" ")
	}
}

// lookupCommand returns the Command with the given name. n=7, scan is fine
func lookupCommand(name string) (Command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// IsKnown reports whether name is a registered subcommand. Used by
// cmd/canary to skip the daemon autospawn for typos and unknown commands —
// otherwise `canary nonsense` would spawn the daemon just to fail with
// "unknown subcommand", which is wasteful and confusing if it tips a
// dormant install into a long startup.
func IsKnown(name string) bool {
	_, ok := lookupCommand(name)
	return ok
}

// Commands returns the registered subcommand entries in declaration order.
func Commands() []Command {
	out := make([]Command, len(commands))
	copy(out, commands)
	return out
}

// PrintUsage writes the top-level help text. Commands are listed under their
// catalog group, in registry order inside each one, so `status` stays the
// first line. The listing shows the catalog's short form and the full summary
// stays in `canary <subcommand> --help` — a flat list of 36 long summaries
// wraps into a wall on an 80-column terminal.
func PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "%s — local trading harness (Interactive Brokers connectivity; broker writes are gated)\n", productidentity.ProductName)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Usage: %s <subcommand> [flags] [args]\n", productidentity.Executable)
	specs := Catalog()
	for _, group := range HelpGroups() {
		fmt.Fprintf(w, "\n%s — %s\n", group.Title, group.Tagline)
		for _, spec := range specs {
			if spec.Group == group.Group {
				fmt.Fprintf(w, "  %-13s  %s\n", spec.Name, spec.listing())
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Run `%s <subcommand> --help` to see the flags it supports.\n", productidentity.Executable)
	fmt.Fprintln(w, "Add --json to data/query subcommands to emit machine-readable output.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Color: respects NO_COLOR=1 to disable; CANARY_COLOR=always|never overrides.")
	fmt.Fprintf(w, "First run? Try `%s status` to verify the gateway is reachable.\n", productidentity.Executable)
}

// flagSet builds a *flag.FlagSet wired to the env's writers and equipped
func flagSet(env *Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(productidentity.Executable+" "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	cmd, known := lookupCommand(name)
	fs.Usage = func() {
		w := env.Stdout
		if known {
			fmt.Fprintf(w, "%s %s — %s\n\n", productidentity.Executable, cmd.Name, cmd.Summary)
			if cmd.Usage != "" {
				fmt.Fprintf(w, "Usage: %s\n\n", cmd.Usage)
			}
		} else {
			fmt.Fprintf(w, "Usage of %s %s\n\n", productidentity.Executable, name)
		}
		var any bool
		fs.VisitAll(func(f *flag.Flag) {
			if !any {
				fmt.Fprintln(w, "Flags:")
				any = true
			}
			fmt.Fprintf(w, "  --%-10s  %s\n", f.Name, f.Usage)
		})
	}
	return fs
}

// printJSON writes obj as indented JSON, returning a non-zero exit code if
// marshal fails (which would indicate a programming error).
func printJSON(env *Env, obj any) int {
	return printJSONTo(env, env.Stdout, obj)
}

// printJSONTo is printJSON with an explicit destination writer. Used by
// renderers that emit to a buffer (watch loop) before flushing to stdout.
func printJSONTo(env *Env, out io.Writer, obj any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		fmt.Fprintf(env.Stderr, "%s: encode json: %v\n", productidentity.Executable, err)
		return 1
	}
	return 0
}

// fail writes a friendly error line and returns code 1. If the underlying
func fail(env *Env, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(env.Stderr, "%s: %s\n", productidentity.Executable, msg)
	if isGatewayUnavailable(msg) {
		fmt.Fprintf(env.Stderr, "  hint: run `%s status` to see whether the daemon is still\n", productidentity.Executable)
		fmt.Fprintln(env.Stderr, "        connecting (retry in a few seconds) or the gateway is")
		fmt.Fprintf(env.Stderr, "        down (start IB Gateway; check %s).\n", dial.DisplayPath(dial.DefaultLogPath()))
	}
	return 1
}

// isGatewayUnavailable matches the error.Code prefix the daemon emits for
// CodeGatewayUnavailable. Kept loose because the message arrives flattened.
func isGatewayUnavailable(msg string) bool {
	return strings.Contains(msg, "gateway_unavailable")
}

// failUnexpectedArgs rejects a stray positional argument with the command's
func failUnexpectedArgs(env *Env, fs *flag.FlagSet) int {
	arg := fs.Arg(0)
	fmt.Fprintf(env.Stderr, "%s: unexpected argument %q", fs.Name(), arg)
	if name := strings.TrimLeft(arg, "-"); name != "" && fs.Lookup(name) != nil {
		fmt.Fprintf(env.Stderr, " (did you mean --%s?)", name)
	}
	fmt.Fprintln(env.Stderr)
	fmt.Fprintln(env.Stderr)
	fs.Usage()
	return 2
}

// formatMoney renders a USD-style amount with grouping; "$ 248,310.42".
func formatMoney(v float64) string {
	return formatMoneyCcy(v, "USD")
}

// formatMoneyCcy renders a money amount with the right currency prefix.
func formatMoneyCcy(v float64, ccy string) string {
	prefix := moneyPrefix(ccy)
	if v == 0 {
		// Em-dash placeholder; width matches the legacy "$         —"
		return prefix + "        —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	grouped := groupThousands(intPart)
	out := prefix + grouped + frac
	if neg {
		return "-" + out
	}
	return out
}

// formatMoneyBare renders the amount with no currency prefix at all.
func formatMoneyBare(v float64) string {
	if v == 0 {
		return "         —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	out := groupThousands(intPart) + frac
	if neg {
		return "-" + out
	}
	return out
}

// moneyPrefix maps an ISO currency code to a short prefix suitable for
// inline money rendering. Symbols for the handful of currencies that
// have one; the ISO code itself for everything else. Always ends in a
// space so callers can concatenate cleanly without extra glue.
func moneyPrefix(ccy string) string {
	switch strings.ToUpper(strings.TrimSpace(ccy)) {
	case "", "USD":
		return "$ "
	case "EUR":
		return "€ "
	case "GBP":
		return "£ "
	case "JPY":
		return "¥ "
	default:
		return strings.ToUpper(strings.TrimSpace(ccy)) + " "
	}
}

func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var out strings.Builder
	for i, r := range s {
		if i > 0 && (n-i)%3 == 0 {
			out.WriteString(",")
		}
		out.WriteString(string(r))
	}
	return out.String()
}

// formatTimeShort returns "HH:MM:SS Z" suitable for status lines.
func formatTimeShort(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("15:04:05 MST")
}

// dataTypeBadge surfaces non-live data clearly. On the happy path (live or
// empty) it returns the empty string — the badge is signal only when data
func (e *Env) dataTypeBadge(dt string) string {
	if rpc.IsLiveDataType(dt) {
		return ""
	}
	return e.yellow("data=" + dt + " ⚠")
}

// suffixBadge prefixes the badge with `  ·  ` when present, returning the
// empty string otherwise. Callers can append this to a header line without
// trailing whitespace on the live path.
func (e *Env) suffixBadge(dt string) string {
	b := e.dataTypeBadge(dt)
	if b == "" {
		return ""
	}
	return "  ·  " + b
}

// colorBySign wraps s with the env's red/green/dim color based on v.
//
//	signNeg  — only negative is colored (red); zero/positive plain;
func (e *Env) colorBySign(v float64, s string, mode signMode) string {
	switch mode {
	case signNeg:
		if v < 0 {
			return e.red(s)
		}
		if v == 0 {
			return e.dim(s)
		}
		return s
	case signPnL:
		if v > 0 {
			return e.green(s)
		}
		if v < 0 {
			return e.red(s)
		}
		return e.dim(s)
	}
	return s
}

type signMode int

const (
	signNeg signMode = iota // negative red, positive plain, zero dim
	signPnL                 // positive green, negative red, zero dim
)

// formatMoneyNegCcyRight is formatMoneyNegCcy with right-alignment
// color state. Used by the account renderer so a column of mixed
func (e *Env) formatMoneyNegCcyRight(v float64, ccy string, w int) string {
	s := formatMoneyCcy(v, ccy)
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signNeg)
}

// formatPnLRight is formatPnL but right-aligns the value within the
// given visible width by prepending spaces. Used by the Portfolio
// aggregate where numeric columns line up on the right edge — a
// thousands-grouped money string varies in width and would otherwise
// leave the trailing unit text at random positions.
func (e *Env) formatPnLRight(v float64, width int) string {
	s := formatMoney(v)
	if pad := width - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

// formatPnLCcyRight is formatPnLRight with a currency prefix attached
// (using the same prefix table as formatMoneyCcy). For account-level
// non-USD accounts. Width counts visible cells of the full prefix+value.
func (e *Env) formatPnLCcyRight(v float64, ccy string, width int) string {
	s := formatMoneyCcyForPnL(v, ccy)
	if pad := width - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

// formatMoneyCcyForPnL is formatMoneyCcy without the zero-as-em-dash
// branch — for sign-coloured P&L lines a value of exactly zero is a
// real result ("flat day") and must render as a number, not a dash.
func formatMoneyCcyForPnL(v float64, ccy string) string {
	prefix := moneyPrefix(ccy)
	neg := v < 0
	abs := v
	if neg {
		abs = -v
	}
	s := fmt.Sprintf("%.2f", abs)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	out := prefix + groupThousands(intPart) + frac
	if neg {
		return "-" + out
	}
	return out
}

// formatSignedGrouped renders v with a leading sign and the integer
func formatSignedGrouped(v float64, decimals int) string {
	neg := v < 0
	abs := v
	if neg {
		abs = -v
	}
	s := fmt.Sprintf("%.*f", decimals, abs)
	dot := strings.IndexByte(s, '.')
	var intPart, frac string
	if dot >= 0 {
		intPart, frac = s[:dot], s[dot:]
	} else {
		intPart, frac = s, ""
	}
	sign := "+"
	if neg {
		sign = "-"
	}
	return sign + groupThousands(intPart) + frac
}

// visibleLen returns the visible (terminal-cell) length of s, ignoring
func visibleLen(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		if r == '\x1b' {
			in = true
			continue
		}
		n++
	}
	return n
}

func truncateVisible(s string, width int) string {
	if visibleLen(s) <= width {
		return s
	}
	if width <= 1 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "."
}

// padRightVisible pads s on the right with spaces until its visible
func padRightVisible(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeftVisible is padRightVisible's right-aligning counterpart.
func padLeftVisible(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// padDash returns a right-aligned em-dash placeholder of visible width w.
func padDash(w int) string {
	if w <= 1 {
		return "—"
	}
	return strings.Repeat(" ", w-1) + "—"
}
