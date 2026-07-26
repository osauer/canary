package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runOrders(ctx context.Context, env *Env, args []string) int {
	if slicesContains(args, "history") {
		return runOrdersHistory(ctx, env, args)
	}
	if slicesContains(args, "open") {
		return runOrdersOpen(ctx, env, args)
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runOrdersOpen(ctx, env, args)
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "open":
		return runOrdersOpen(ctx, env, args)
	case "history":
		return runOrdersHistory(ctx, env, args)
	default:
		return fail(env, "orders: unknown subcommand %q (try `canary orders open` or `canary orders history`)", sub)
	}
}

func runOrdersOpen(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "orders")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "open" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "orders open: usage is `canary orders open [--json]`")
	}
	var res rpc.OrdersOpenResult
	if err := env.Conn.Call(ctx, rpc.MethodOrdersOpen, rpc.OrdersOpenParams{}, &res); err != nil {
		return fail(env, "orders open: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrdersOpenText(env, &res)
	return 0
}

func runOrdersHistory(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "orders")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	since := fs.String("since", "", "inclusive lower boundary: YYYY-MM-DD UTC day or RFC3339 timestamp")
	until := fs.String("until", "", "exclusive upper boundary: YYYY-MM-DD UTC day (inclusive date) or RFC3339 timestamp")
	limit := fs.Int("limit", 0, "max grouped orders to return (default 50, max 500)")
	eventLimit := fs.Int("event-limit", 0, "max lifecycle events per grouped order (default 20, max 200)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "history" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "orders history: usage is `canary orders history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--limit N] [--event-limit N] [--json]`")
	}
	params := rpc.OrdersHistoryParams{
		Since:      strings.TrimSpace(*since),
		Until:      strings.TrimSpace(*until),
		Limit:      *limit,
		EventLimit: *eventLimit,
	}
	var res rpc.OrdersHistoryResult
	if err := env.Conn.Call(ctx, rpc.MethodOrdersHistory, params, &res); err != nil {
		return fail(env, "orders history: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrdersHistoryText(env, &res)
	return 0
}

func runOrderStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "order status: usage is `canary order status <order-ref|order-id|perm-id>`")
	}
	var res rpc.OrderStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderStatus, rpc.OrderStatusParams{ID: strings.TrimSpace(rest[0])}, &res); err != nil {
		return fail(env, "order status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderStatusText(env, &res, rest[0])
	return 0
}

func renderOrdersOpenText(env *Env, res *rpc.OrdersOpenResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Open Orders  %s\n", env.statusBadge(statusConcern{Text: strconv.Itoa(len(res.Orders)), Level: statusConcernNone}))
	renderOrderReadScope(env, res.Account, res.Mode, res.NotBrokerStatement, res.LastLocalEventAt, res.Limitations)
	if len(res.Orders) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "No locally tracked open orders.")
		fmt.Fprintln(out)
		return
	}
	for _, order := range res.Orders {
		fmt.Fprintf(out, "  %s\n", formatOrderViewTitle(order))
		fmt.Fprintf(out, "    %s  %s  updated %s\n", order.LifecycleStatus, nonEmpty(order.Status, order.LastEvent), formatOrderTime(order.UpdatedAt))
		if order.LastMessage != "" {
			fmt.Fprintf(out, "    %s\n", order.LastMessage)
		}
		if order.WhyHeld != "" {
			fmt.Fprintf(out, "    held: %s\n", order.WhyHeld)
		}
		if order.TriggerMethod != 0 {
			fmt.Fprintf(out, "    trigger: %s\n", formatOrderTriggerMethod(order.TriggerMethod))
		}
		if order.MktCapPrice != 0 {
			fmt.Fprintf(out, "    capped price: %.4f\n", order.MktCapPrice)
		}
	}
	fmt.Fprintln(out)
}

func renderOrdersHistoryText(env *Env, res *rpc.OrdersHistoryResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Order History  %s\n", env.statusBadge(statusConcern{Text: strconv.Itoa(res.Count), Level: statusConcernNone}))
	statusRow(env, out, "Scope", strings.TrimSpace(nonEmpty(res.Account, "unknown")+" "+nonEmpty(res.Mode, "unknown")))
	statusRow(env, out, "Range", fmt.Sprintf("%s to %s", formatOrderTime(res.Since), formatOrderTime(res.Until)))
	if res.Truncated {
		statusRow(env, out, "Limit", fmt.Sprintf("%d of %d groups shown", res.Count, res.TotalCount))
	}
	if res.EventsTruncated {
		statusRow(env, out, "Events", fmt.Sprintf("%d of %d events shown; max %d per order", res.EventsCount, res.TotalEventsCount, res.EventLimit))
	}
	if res.NotBrokerStatement != "" {
		statusRow(env, out, "Source", res.NotBrokerStatement)
	}
	if len(res.Orders) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "No local order-journal events matched this range for the current account/mode.")
	} else {
		for _, row := range res.Orders {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "  %s\n", formatOrderViewTitle(row.Order))
			fmt.Fprintf(out, "    %s  %s  updated %s  events %d\n",
				row.Order.LifecycleStatus,
				nonEmpty(row.Order.Status, row.Order.LastEvent),
				formatOrderTime(row.Order.UpdatedAt),
				row.TotalEventsCount,
			)
			if row.EventsTruncated {
				fmt.Fprintf(out, "    showing %d of %d events\n", row.EventsCount, row.TotalEventsCount)
			}
			for _, ev := range row.Events {
				fmt.Fprintf(out, "    - %s  %s", formatOrderTime(ev.At), ev.Type)
				if ev.LifecycleStatus != "" {
					fmt.Fprintf(out, "  %s", ev.LifecycleStatus)
				}
				if ev.Filled != 0 || ev.Remaining != 0 {
					fmt.Fprintf(out, "  filled %.4g remaining %.4g", ev.Filled, ev.Remaining)
				}
				if ev.AvgFillPrice != 0 {
					fmt.Fprintf(out, " avg %.4f", ev.AvgFillPrice)
				}
				if ev.LastFillPrice != 0 {
					fmt.Fprintf(out, " last %.4f", ev.LastFillPrice)
				}
				if ev.Message != "" {
					fmt.Fprintf(out, "  %s", ev.Message)
				}
				fmt.Fprintln(out)
			}
		}
	}
	if len(res.Limitations) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Limitations:")
		for _, limitation := range res.Limitations {
			fmt.Fprintf(out, "  - %s\n", limitation)
		}
	}
	fmt.Fprintln(out)
}

func renderOrderStatusText(env *Env, res *rpc.OrderStatusResult, id string) {
	out := env.Stdout
	fmt.Fprintln(out)
	if !res.Found {
		fmt.Fprintf(out, "Canary Order Status  %s\n\n", env.statusBadge(statusConcern{Text: "NOT FOUND", Level: statusConcernWarn}))
		renderOrderReadScope(env, res.Account, res.Mode, res.NotBrokerStatement, res.LastLocalEventAt, res.Limitations)
		fmt.Fprintln(out)
		fmt.Fprintf(out, "No locally tracked order matched %s.\n\n", id)
		return
	}
	fmt.Fprintf(out, "Canary Order Status  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(res.Order.LifecycleStatus), Level: orderStatusConcernLevel(res.Order.LifecycleStatus)}))
	renderOrderReadScope(env, res.Account, res.Mode, res.NotBrokerStatement, res.LastLocalEventAt, res.Limitations)
	statusRow(env, out, "Order", formatOrderViewTitle(res.Order))
	if res.Order.ReservedOrderID != 0 {
		statusRow(env, out, "Broker ID", strconv.Itoa(res.Order.ReservedOrderID))
	}
	if res.Order.PermID != 0 {
		statusRow(env, out, "Perm ID", strconv.Itoa(res.Order.PermID))
	}
	statusRow(env, out, "Account", res.Order.Account)
	statusRow(env, out, "Status", nonEmpty(res.Order.Status, res.Order.LifecycleStatus))
	statusRow(env, out, "Filled", fmt.Sprintf("%.4g / %.4g", res.Order.Filled, res.Order.Quantity))
	if res.Order.TriggerMethod != 0 {
		statusRow(env, out, "Trigger", formatOrderTriggerMethod(res.Order.TriggerMethod))
	}
	if res.Order.LastMessage != "" {
		statusRow(env, out, "Message", res.Order.LastMessage)
	}
	if res.Order.WhyHeld != "" {
		statusRow(env, out, "Held", res.Order.WhyHeld)
	}
	if res.Order.MktCapPrice != 0 {
		statusRow(env, out, "Capped price", fmt.Sprintf("%.4f", res.Order.MktCapPrice))
	}
	if !res.Order.UpdatedAt.IsZero() {
		statusRow(env, out, "Updated", formatOrderTime(res.Order.UpdatedAt))
	}
	if len(res.Events) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Events:")
		for _, ev := range res.Events {
			fmt.Fprintf(out, "  - %s  %s", formatOrderTime(ev.At), ev.Type)
			if ev.LifecycleStatus != "" {
				fmt.Fprintf(out, "  %s", ev.LifecycleStatus)
			}
			if ev.Message != "" {
				fmt.Fprintf(out, "  %s", ev.Message)
			}
			if ev.WhyHeld != "" {
				fmt.Fprintf(out, "  held=%s", ev.WhyHeld)
			}
			if ev.TriggerMethod != 0 {
				fmt.Fprintf(out, "  trigger=%s", formatOrderTriggerMethod(ev.TriggerMethod))
			}
			if ev.MktCapPrice != 0 {
				fmt.Fprintf(out, "  capped_price=%.4f", ev.MktCapPrice)
			}
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out)
}

func renderOrderReadScope(env *Env, account, mode, source string, lastLocalEventAt time.Time, limitations []string) {
	out := env.Stdout
	statusRow(env, out, "Scope", strings.TrimSpace(nonEmpty(account, "unknown")+" "+nonEmpty(mode, "unknown")))
	if !lastLocalEventAt.IsZero() {
		statusRow(env, out, "Latest local event", formatOrderTime(lastLocalEventAt))
	}
	if source != "" {
		statusRow(env, out, "Source", source)
	}
	if len(limitations) > 0 {
		fmt.Fprintln(out, "Limitations:")
		for _, limitation := range limitations {
			fmt.Fprintf(out, "  - %s\n", limitation)
		}
	}
}

func formatOrderViewTitle(order rpc.OrderView) string {
	id := order.OrderRef
	if id == "" && order.ReservedOrderID != 0 {
		id = strconv.Itoa(order.ReservedOrderID)
	}
	if id == "" {
		id = "unknown-order"
	}
	qty := order.Quantity
	if qty == 0 && order.Filled+order.Remaining > 0 {
		qty = order.Filled + order.Remaining
	}
	title := fmt.Sprintf("%s  %s %.4g %s %s",
		id,
		nonEmpty(order.Action, "?"),
		qty,
		nonEmpty(order.Symbol, "?"),
		nonEmpty(order.OrderType, "?"),
	)
	switch {
	case order.Trail != nil:
		title += " " + formatOrderTrail(order.Trail)
	case order.LimitPrice != 0:
		title += fmt.Sprintf(" %.4f", order.LimitPrice)
	}
	if order.TriggerMethod != 0 {
		title += " trigger=" + formatOrderTriggerMethod(order.TriggerMethod)
	}
	return title + " " + nonEmpty(order.TIF, "?")
}

func slicesContains(values []string, target string) bool {
	return slices.Contains(values, target)
}

func formatOrderTriggerMethod(method int) string {
	switch method {
	case rpc.OrderTriggerMethodDoubleBidAsk:
		return "DOUBLE_BID_ASK"
	case rpc.OrderTriggerMethodLast:
		return "LAST"
	case rpc.OrderTriggerMethodDoubleLast:
		return "DOUBLE_LAST"
	case rpc.OrderTriggerMethodBidAsk:
		return "BID_ASK"
	case rpc.OrderTriggerMethodLastOrBidAsk:
		return "LAST_OR_BID_ASK"
	case rpc.OrderTriggerMethodMidpoint:
		return "MIDPOINT"
	default:
		return strconv.Itoa(method)
	}
}

func orderStatusConcernLevel(status string) statusConcernLevel {
	switch status {
	case rpc.OrderLifecycleFilled, rpc.OrderLifecycleCancelled:
		return statusConcernNone
	case rpc.OrderLifecycleRejected, rpc.OrderLifecycleInactive, rpc.OrderLifecycleUnknownReconcileRequired, rpc.OrderLifecycleExpiredInferred:
		return statusConcernWarn
	default:
		return statusConcernNotice
	}
}

func formatOrderTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}
