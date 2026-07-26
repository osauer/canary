package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func runTrading(ctx context.Context, env *Env, args []string) int {
	// The dispatcher hoists flags ahead of positionals, so the subcommand
	// token can sit anywhere in args (mirrors runProposals).
	sub := "status"
	if idx := tradingSubcommandIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "status":
		return runTradingStatus(ctx, env, args)
	case "paper-smoke":
		return runTradingPaperSmoke(ctx, env, args)
	default:
		return fail(env, "trading: unknown subcommand %q (try `canary trading status` or `canary trading paper-smoke`)", sub)
	}
}

func tradingSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "status", "paper-smoke":
			return i
		}
	}
	return -1
}

func runTradingPaperSmoke(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "trading paper-smoke")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum wait for broker acknowledgement")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	var res rpc.TradingPaperSmokeResult
	if err := env.Conn.Call(ctx, rpc.MethodTradingPaperSmoke, rpc.TradingPaperSmokeParams{TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}, &res); err != nil {
		return fail(env, "trading paper-smoke: %v", err)
	}
	if *jsonOut {
		printJSON(env, res)
	} else {
		renderTradingPaperSmokeText(env, &res)
	}
	if res.Passed {
		return 0
	}
	return 1
}

func renderTradingPaperSmokeText(env *Env, res *rpc.TradingPaperSmokeResult) {
	out := env.Stdout
	verdict := statusConcern{Text: "PASSED", Level: statusConcernNone}
	if !res.Passed {
		verdict = statusConcern{Text: "FAILED", Level: statusConcernWarn}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Paper Smoke  %s\n", env.statusBadge(verdict))
	fmt.Fprintln(out)
	statusRow(env, out, "Gate", fmt.Sprintf("%s %s via %s (client %d)", nonEmpty(res.Mode, "unknown"), nonEmpty(res.Account, "unknown"), nonEmpty(res.Endpoint, "unknown"), res.ClientID))
	order := fmt.Sprintf("BUY %d %s LMT %.2f DAY", res.Quantity, res.Symbol, res.LimitPrice)
	if res.OrderRef != "" {
		order += " (" + res.OrderRef + ")"
	}
	statusRow(env, out, "Order", order)
	if res.ReservedOrderID != 0 {
		statusRow(env, out, "Broker ID", fmt.Sprint(res.ReservedOrderID))
	}
	statusRow(env, out, "Ack", nonEmpty(res.AckLifecycleStatus, "none"))
	statusRow(env, out, "Cancel", nonEmpty(res.CancelLifecycleStatus, "none"))
	if res.EvidenceSaved && res.EvidenceAt != nil {
		statusRow(env, out, "Evidence", fmt.Sprintf("%s at %s (max age %s)", res.Result, res.EvidenceAt.Format(time.RFC3339), res.EvidenceMaxAge))
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  %s\n", env.dim(w.Code+": "+w.Message))
	}
	if res.Message != "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, res.Message)
	}
	if res.Passed {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("Evidence is bound to this binary version; rerun after every install."))
	}
	fmt.Fprintln(out)
}

func runTradingStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "trading status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "trading: unknown subcommand %q (try `canary trading status` or `canary trading paper-smoke`)", fs.Arg(0))
	}
	var res rpc.TradingStatus
	if err := env.Conn.Call(ctx, rpc.MethodTradingStatus, nil, &res); err != nil {
		return fail(env, "trading status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderTradingStatusText(env, &res)
	if res.Blocked {
		return 1
	}
	return 0
}

func renderTradingStatusText(env *Env, st *rpc.TradingStatus) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Trading  %s\n", env.statusBadge(tradingStatusVerdict(*st)))
	fmt.Fprintln(out)
	statusRow(env, out, "Mode", formatTradingMode(env, *st))
	statusRow(env, out, "Endpoint", nonEmpty(st.Endpoint, "auto-detect"))
	statusRow(env, out, "Account", nonEmpty(st.Account, "auto-detect")+" ("+nonEmpty(st.AccountOrigin, "auto")+")")
	statusRow(env, out, "Client ID", fmt.Sprintf("%d (%s)", st.ClientID, nonEmpty(st.ClientIDOrigin, "default")))
	statusRow(env, out, "MCP trading", nonEmpty(st.MCPTrading, rpc.TradingMCPDisabled))
	statusRow(env, out, "Capabilities", formatTradingCapabilities(*st))
	statusRow(env, out, "Open orders", fmt.Sprint(st.OpenOrders))
	if st.LastOrderEvent != "" {
		statusRow(env, out, "Last event", st.LastOrderEvent)
	}
	if st.Mode == config.TradingModeLive {
		statusRow(env, out, "Live override", nonEmpty(st.LiveOverride, rpc.TradingLiveOverrideBlocked))
		if st.PaperSmoke != "" {
			statusRow(env, out, "Paper smoke", formatPaperSmokeValue(*st))
		}
	}
	if len(st.Blockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Blockers:")
		for _, b := range st.Blockers {
			fmt.Fprintf(out, "  - %s: %s\n", b.Code, b.Message)
			if b.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", b.Action)
			}
		}
	}
	if len(st.WriteBlockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Write blockers:")
		for _, b := range st.WriteBlockers {
			fmt.Fprintf(out, "  - %s: %s\n", b.Code, b.Message)
			if b.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", b.Action)
			}
		}
	}
	fmt.Fprintln(out)
}

func formatTradingCapabilities(st rpc.TradingStatus) string {
	return fmt.Sprintf("preview=%v write=%v", st.CanPreview, st.CanWrite)
}

func formatPaperSmokeValue(st rpc.TradingStatus) string {
	value := st.PaperSmoke
	if st.PaperSmokeAt != nil && !st.PaperSmokeAt.IsZero() {
		value += " at " + st.PaperSmokeAt.Format(time.RFC3339)
	}
	if st.PaperSmokeAccount != "" || st.PaperSmokeEndpoint != "" || st.PaperSmokeClientID != 0 {
		value += fmt.Sprintf(" (%s via %s, client %d)", nonEmpty(st.PaperSmokeAccount, "unknown-account"), nonEmpty(st.PaperSmokeEndpoint, "unknown-endpoint"), st.PaperSmokeClientID)
	}
	if st.PaperSmokeMaxAge != "" {
		value += "; max age " + st.PaperSmokeMaxAge
	}
	return value
}

func tradingStatusVerdict(st rpc.TradingStatus) statusConcern {
	switch {
	case st.Mode == config.TradingModeDisabled:
		return statusConcern{Text: "DISABLED", Level: statusConcernNotice}
	case st.Blocked:
		return statusConcern{Text: "BLOCKED", Level: statusConcernWarn}
	default:
		return statusConcern{Text: "READY", Level: statusConcernNone}
	}
}

func formatTradingMode(env *Env, st rpc.TradingStatus) string {
	if st.Mode == config.TradingModeDisabled {
		return env.dim(config.TradingModeDisabled)
	}
	if st.Blocked {
		return env.yellow(st.Mode + " blocked")
	}
	if st.Mode == config.TradingModeLive {
		return env.yellow(st.Mode + " ready")
	}
	return env.green(nonEmpty(st.Mode, "unknown") + " ready")
}

func formatTradingStatusValue(env *Env, st rpc.TradingStatus) string {
	if st.Mode == "" {
		return env.dim("unknown")
	}
	if st.Mode == config.TradingModeDisabled {
		return env.dim("disabled")
	}
	if st.Blocked {
		msg := "blocked"
		if len(st.Blockers) > 0 {
			msg += ": " + st.Blockers[0].Message
		}
		return env.yellow(st.Mode + " " + msg)
	}
	if st.Mode == config.TradingModeLive {
		return env.yellow("live ready")
	}
	return env.green(st.Mode + " ready")
}
