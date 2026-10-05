package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/rpc"
)

// runPolicyCheck reads config.toml and every policy file against each other
// and, when the daemon answers, against the live account, and prints what is
// implausible. It is read-only. It exits non-zero only when a finding is an
// error: an order path that can never work or a contradiction.
func runPolicyCheck(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy check")
	jsonOut := fs.Bool("json", false, "emit the report as JSON")
	offline := fs.Bool("offline", false, "check the files only; do not ask the daemon for the live account")
	configPath := fs.String("config", "", "config file that names the policy paths and the trading limits (default: the daemon's config)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "policy check: usage is `canary policy check [--offline] [--config PATH] [--json]`")
	}
	in, cfgErr := daemon.PolicyCheckInputFromConfigFile(*configPath)
	if cfgErr != nil {
		fmt.Fprintf(env.Stderr, "policy check: config unreadable (%v); using the default policy paths and trading limits\n", cfgErr)
	}
	in.Now = time.Now()
	switch {
	case *offline:
		in.BookSkipped = "--offline"
	case env.Conn == nil:
		in.BookSkipped = "no daemon"
	default:
		policyCheckLiveInputs(ctx, env, &in)
	}
	report := daemon.CheckPolicy(in)
	code := 0
	if report.Errors > 0 {
		code = 1
	}
	if *jsonOut {
		if c := printJSON(env, report); c != 0 {
			return c
		}
		return code
	}
	renderPolicyCheck(env.Stdout, report)
	return code
}

// policyCheckLiveInputs fills the book, the trading cap in force and the
// daemon's file statuses from read-only daemon calls. Each one that fails
// leaves its checks skipped; none fails the command.
func policyCheckLiveInputs(ctx context.Context, env *Env, in *daemon.PolicyCheckInput) {
	var acct rpc.AccountResult
	if err := env.Conn.Call(ctx, rpc.MethodAccountSummary, struct{}{}, &acct); err != nil {
		in.BookSkipped = "the account read failed (" + firstLine(err.Error()) + ")"
	} else {
		var pos rpc.PositionsResult
		posPtr := &pos
		if err := env.Conn.Call(ctx, rpc.MethodPositionsList, struct{}{}, &pos); err != nil {
			posPtr = nil
		}
		in.Book = daemon.PolicyCheckBookFrom(&acct, posPtr)
		if in.Book == nil {
			in.BookSkipped = "the account reported no net liquidation value"
		}
	}
	var settings rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsGet, nil, &settings); err == nil {
		if mn := settings.Trading.Limits.MaxNotional; mn.Value > 0 && mn.Source == rpc.SettingsSourceRuntime {
			in.Trading.MaxNotional, in.TradingCapSource = mn.Value, daemon.PolicyCheckCapFromRuntime
		}
	}
	var snap rpc.RiskPolicyResult
	if err := env.Conn.Call(ctx, rpc.MethodRiskPolicySnapshot, struct{}{}, &snap); err == nil {
		in.FileStatus = map[string]string{}
		for _, f := range snap.Files {
			in.FileStatus[f.Policy] = f.Status
		}
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// renderPolicyCheck prints the report for a person: the counts first, then
// each finding with its keys, what is wrong and what to write instead.
func renderPolicyCheck(out io.Writer, r rpc.PolicyCheckReport) {
	fmt.Fprintf(out, "Policy check — %s  %s\n", r.AsOf.Local().Format("2006-01-02 15:04 MST"), policyCheckCounts(r))
	for _, f := range r.Files {
		state := f.State
		if f.Review != "" {
			state += ", " + f.Review
		}
		fmt.Fprintf(out, "  %-12s %s (%s)\n", f.Policy, f.Path, state)
	}
	if b := r.Book; b != nil {
		fmt.Fprintf(out, "  live book    NLV %s %s, %d positions, as of %s\n", groupThousands(fmt.Sprintf("%.0f", b.NetLiquidation)), b.BaseCurrency, b.Positions, b.AsOf.Local().Format("2006-01-02 15:04"))
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "  skipped      %s\n", s)
	}
	for _, a := range r.Assumptions {
		fmt.Fprintf(out, "  assumed      %s\n", a)
	}
	if len(r.Findings) == 0 {
		fmt.Fprintln(out, "\nNothing implausible found.")
		return
	}
	for _, f := range r.Findings {
		fmt.Fprintf(out, "\n%-5s %s (%s)\n", strings.ToUpper(f.Severity), f.Rule, f.Category)
		for _, k := range f.Keys {
			fmt.Fprintf(out, "  %s = %s   [%s]\n", k.Key, k.Value, k.File)
		}
		fmt.Fprintf(out, "  %s\n", f.Message)
		if f.Suggestion != "" {
			fmt.Fprintf(out, "  Suggest: %s\n", f.Suggestion)
		}
	}
}

func policyCheckCounts(r rpc.PolicyCheckReport) string {
	return fmt.Sprintf("%d %s, %d %s, %d %s", r.Errors, plural(r.Errors, "error", "errors"), r.Warnings, plural(r.Warnings, "warning", "warnings"), r.Infos, plural(r.Infos, "note", "notes"))
}
