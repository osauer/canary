package cli

import (
	"context"
	"fmt"

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
	default:
		return fail(env, "trading: unknown subcommand %q (try `canary trading status`)", sub)
	}
}

func tradingSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "status":
			return i
		}
	}
	return -1
}

func runTradingStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "trading status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "trading: unknown subcommand %q (try `canary trading status`)", fs.Arg(0))
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
