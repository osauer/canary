package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runOrder intentionally exposes only lifecycle inspection and cancellation.
// New stops, reductions, liquidation, and exercise use their constrained
// proposal/opportunity flows; v3 has no free-form order-entry command.
func runOrder(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		return fail(env, "order: subcommand required (status or cancel)")
	}
	subcommand := ""
	index := -1
	for i, arg := range args {
		if arg == "status" || arg == "cancel" {
			subcommand, index = arg, i
			break
		}
	}
	if index < 0 {
		return fail(env, "order: only status and cancel are available; use proposals or opportunities for new actions")
	}
	args = append(append([]string{}, args[:index]...), args[index+1:]...)
	if subcommand == "status" {
		return runOrderStatus(ctx, env, args)
	}
	return runOrderCancel(ctx, env, args)
}

func runOrderCancel(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order cancel")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "order cancel: usage is `canary order cancel <order-ref|order-id|perm-id>`")
	}
	var res rpc.OrderCancelResult
	params := rpc.OrderCancelParams{ID: strings.TrimSpace(fs.Arg(0)), Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodOrderCancel, params, &res); err != nil {
		return fail(env, "order cancel: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "Canary Order Cancel  %s\n", env.statusBadge(statusConcern{Text: "SENT", Level: statusConcernNotice}))
	statusRow(env, env.Stdout, "Order", formatOrderViewTitle(res.Order))
	statusRow(env, env.Stdout, "State", nonEmpty(res.LifecycleStatus, res.SendState))
	if res.Message != "" {
		statusRow(env, env.Stdout, "Message", res.Message)
	}
	fmt.Fprintln(env.Stdout)
	return 0
}

func formatOrderDraftSummary(draft rpc.OrderDraft) string {
	price := fmt.Sprintf("%.4f", draft.LimitPrice)
	if draft.Trail != nil {
		price = formatOrderTrail(draft.Trail)
	}
	summary := fmt.Sprintf("%s %d %s %s %s %s outside_rth=%v",
		draft.Action, draft.Quantity, draft.Contract.Symbol, draft.OrderType, price, draft.TIF, draft.OutsideRTH)
	if draft.TriggerMethod != 0 {
		summary += " trigger=" + formatOrderTriggerMethod(draft.TriggerMethod)
	}
	return summary
}

func formatOrderTrail(trail *rpc.OrderTrailSpec) string {
	if trail == nil {
		return "--"
	}
	parts := make([]string, 0, 4)
	if trail.TrailingPercent != nil {
		parts = append(parts, fmt.Sprintf("trail %.4g%%", *trail.TrailingPercent))
	}
	if trail.TrailingAmount != nil {
		parts = append(parts, fmt.Sprintf("trail %.4f", *trail.TrailingAmount))
	}
	if trail.InitialStopPrice > 0 {
		parts = append(parts, fmt.Sprintf("stop %.4f", trail.InitialStopPrice))
	}
	if trail.LimitOffset != nil {
		parts = append(parts, fmt.Sprintf("limit_offset %.4f", *trail.LimitOffset))
	}
	if len(parts) == 0 {
		return "trail --"
	}
	return strings.Join(parts, " ")
}

func formatOrderPreviewQuote(q rpc.OrderQuoteSnapshot) string {
	parts := []string{q.Symbol}
	if q.Bid != nil {
		parts = append(parts, fmt.Sprintf("bid %.4f", *q.Bid))
	}
	if q.Ask != nil {
		parts = append(parts, fmt.Sprintf("ask %.4f", *q.Ask))
	}
	if q.Midpoint != nil {
		parts = append(parts, fmt.Sprintf("mid %.4f", *q.Midpoint))
	}
	if q.DataType != "" {
		parts = append(parts, "data "+q.DataType)
	}
	if q.QuoteQuality != "" {
		parts = append(parts, "quality "+q.QuoteQuality)
	}
	if q.Stale {
		parts = append(parts, "stale")
	}
	if q.PriceAsOf != "" {
		parts = append(parts, q.PriceAsOf)
	}
	return strings.Join(parts, " | ")
}
