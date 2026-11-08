package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// runOrderTWSControl cancels or changes an order placed by hand in TWS
// through the daemon's client-0 control path (owner decision "TWS B",
// 2026-10-11 19:36 CEST). The order is named by its permanent id. The terms
// the daemon checks the order against come from --terms when a caller that
// already reviewed them (Desk) passes them, otherwise from the current
// `orders open` row, which the command prints before sending. A refusal or
// an uncertain outcome is printed in full; in text mode it exits 1, with
// --json the result carries it. Nothing is retried.
func runOrderTWSControl(ctx context.Context, env *Env, args []string, method rpc.Method[rpc.OrderTWSControlParams, rpc.OrderTWSControlResult]) int {
	modify := method == api.OrderTWSModify
	name := "order tws-cancel"
	if modify {
		name = "order tws-modify"
	}
	fs := flagSet(env, name)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	terms := fs.String("terms", "", "the reviewed terms as JSON (symbol, action, quantity, order_type, limit_price, tif, filled); defaults to the current orders open row")
	requestID := fs.String("request-id", "", "idempotency key: a repeat with the same intent returns the first outcome and sends nothing")
	var quantity, limit *float64
	if modify {
		quantity = fs.Float64("quantity", 0, "the new total quantity (whole number, at most the reviewed quantity)")
		limit = fs.Float64("limit", 0, "the new limit price")
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		if modify {
			return fail(env, "order tws-modify: usage is `canary order tws-modify PERM_ID --quantity N --limit PRICE [--terms JSON] [--request-id ID] [--json]`")
		}
		return fail(env, "order tws-cancel: usage is `canary order tws-cancel PERM_ID [--terms JSON] [--request-id ID] [--json]`")
	}
	permID, err := strconv.Atoi(strings.TrimSpace(fs.Arg(0)))
	if err != nil || permID <= 0 {
		return fail(env, "%s: PERM_ID must be the order's positive permanent id (see `canary orders open`)", name)
	}
	params := rpc.OrderTWSControlParams{RequestID: strings.TrimSpace(*requestID), PermID: permID, Origin: env.Origin}
	if modify {
		params.Replacement = &rpc.OrderTWSControlReplacement{Quantity: *quantity, LimitPrice: *limit}
	}
	if strings.TrimSpace(*terms) != "" {
		if err := json.Unmarshal([]byte(*terms), &params.Terms); err != nil {
			return fail(env, "%s: --terms must be JSON: %v", name, err)
		}
	} else {
		row, err := untrackedOrderRow(ctx, env, permID)
		if err != nil {
			return fail(env, "%s: %v", name, err)
		}
		params.Terms = rpc.OrderTWSControlTerms{ConID: row.ConID, Symbol: row.Symbol, SecType: row.SecType, Action: row.Action, Quantity: row.Quantity,
			Filled: row.Filled, OrderType: row.OrderType, LimitPrice: row.LimitPrice, TIF: row.TIF}
		if !*jsonOut {
			fmt.Fprintf(env.Stdout, "Order placed in TWS: %s\n", formatOrderViewTitle(row))
		}
	}
	if err := params.Validate(modify); err != nil {
		return fail(env, "%s: %v", name, err)
	}
	var res rpc.OrderTWSControlResult
	if err := rpc.Call(ctx, env.Conn, method, params, &res); err != nil {
		return fail(env, "%s: %v", name, err)
	}
	if *jsonOut {
		// The JSON is the answer, whatever the outcome: a caller reads
		// accepted, outcome and the blockers from it.
		return printJSON(env, res)
	}
	renderOrderTWSControlText(env, &res)
	if !res.Accepted {
		return 1
	}
	return 0
}

// untrackedOrderRow reads the current untracked row for permID.
func untrackedOrderRow(ctx context.Context, env *Env, permID int) (rpc.OrderView, error) {
	var open rpc.OrdersOpenResult
	if err := rpc.Call(ctx, env.Conn, api.OrdersOpen, rpc.OrdersOpenParams{}, &open); err != nil {
		return rpc.OrderView{}, fmt.Errorf("read open orders: %w", err)
	}
	if open.UntrackedStatus != rpc.OrdersUntrackedCurrent {
		return rpc.OrderView{}, fmt.Errorf("the broker's open-order list is unavailable; nothing was sent")
	}
	for _, row := range open.Untracked {
		if row.PermID == permID {
			return row, nil
		}
	}
	return rpc.OrderView{}, fmt.Errorf("no order placed outside Canary carries permanent id %d; see `canary orders open`", permID)
}

func renderOrderTWSControlText(env *Env, res *rpc.OrderTWSControlResult) {
	out := env.Stdout
	badge := statusConcern{Text: strings.ToUpper(res.Outcome), Level: statusConcernWarn}
	switch res.Outcome {
	case rpc.TWSOrderControlOutcomeConfirmed:
		badge.Level = statusConcernNone
	case rpc.TWSOrderControlOutcomePending:
		badge.Level = statusConcernNotice
	}
	title := "Canary TWS Order Cancel"
	if res.Action == "modify" {
		title = "Canary TWS Order Change"
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s  %s\n", title, env.statusBadge(badge))
	if res.Order != nil {
		statusRow(env, out, "Order", formatOrderViewTitle(*res.Order))
	} else {
		statusRow(env, out, "Order", "perm "+strconv.Itoa(res.PermID))
	}
	if res.BoundOrderID != 0 {
		statusRow(env, out, "Bound as", fmt.Sprintf("API order %d on client 0", res.BoundOrderID))
	}
	if res.BrokerStatus != "" || res.BrokerCode != 0 {
		statusRow(env, out, "Broker", strings.TrimSpace(fmt.Sprintf("%s %s", res.BrokerStatus, nonZeroCode(res.BrokerCode))))
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	for _, b := range res.Blockers {
		if b.Code != "" && b.Message != res.Message {
			statusRow(env, out, "Blocker", b.Code+": "+b.Message)
		} else if b.Code != "" {
			statusRow(env, out, "Blocker", b.Code)
		}
	}
	if !res.PaperProvenAt.IsZero() {
		statusRow(env, out, "Paper proof", formatOrderTime(res.PaperProvenAt))
	}
	if res.RequestID != "" {
		statusRow(env, out, "Request", res.RequestID)
	}
	fmt.Fprintln(out)
}

func nonZeroCode(code int) string {
	if code == 0 {
		return ""
	}
	return "(code " + strconv.Itoa(code) + ")"
}
