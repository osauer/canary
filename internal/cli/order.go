package cli

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runOrder(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		return fail(env, "order: subcommand required (try `canary order preview ...`)")
	}
	subIdx := orderSubcommandIndex(args)
	if subIdx < 0 {
		return runOrderPreview(ctx, env, args)
	}
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "preview":
		return runOrderPreview(ctx, env, args)
	case "status":
		return runOrderStatus(ctx, env, args)
	case "place":
		return runOrderPlace(ctx, env, args)
	case "modify":
		return runOrderModify(ctx, env, args)
	case "cancel":
		return runOrderCancel(ctx, env, args)
	default:
		return fail(env, "order: unknown subcommand %q (try `canary order preview` or `canary order status`)", sub)
	}
}

func orderSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "preview", "status", "place", "modify", "cancel":
			return i
		}
	}
	return -1
}

func runOrderPreview(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	limit := fs.Float64("limit", 0, "explicit LMT limit price")
	strategy := fs.String("strategy", "", "pricing strategy: patient-limit (default) or explicit-limit")
	orderTypeFlag := fs.String("order-type", "", "order type: LMT, TRAIL, or TRAIL-LIMIT")
	trailPercent := fs.Float64("trail-percent", 0, "broker trail offset percent; 2 means 2%, not 0.02")
	trailAmount := fs.Float64("trail-amount", 0, "broker trail offset amount")
	initialStop := fs.Float64("initial-stop", 0, "optional initial broker trail stop price; omitted means use live bid/ask")
	limitOffset := fs.Float64("limit-offset", 0, "TRAIL LIMIT offset from the dynamic stop")
	triggerMethod := fs.Int("trigger-method", 0, "IBKR stop trigger method for TRAIL/TRAIL-LIMIT: 1,2,3,4,7,8")
	tif := fs.String("tif", "", "time in force: DAY (default), or GTC for TRAIL/TRAIL-LIMIT")
	outsideRTH := fs.Bool("outside-rth", false, "allow outside regular trading hours when supported")
	replaceID := fs.String("replace-order", "", "preview a replacement for an existing open order ref/order-id/perm-id")
	timeout := fs.Duration("timeout", 5*time.Second, "quote snapshot timeout")
	market := fs.String("market", "", "stock market routing shortcut: us (default) or de")
	exchange := fs.String("exchange", "", "IBKR stock exchange/venue override (e.g. SMART, IBIS)")
	primary := fs.String("primary", "", "IBKR stock primary-exchange hint when routing through SMART")
	currency := fs.String("currency", "", "stock quote/order currency override (e.g. USD, EUR)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "preview" {
		rest = rest[1:]
	}
	if len(rest) != 3 && len(rest) != 6 {
		return fail(env, "order preview: usage is `canary order preview buy|sell SYMBOL QTY` or `canary order preview buy|sell SYMBOL YYYYMMDD C|P STRIKE QTY`")
	}
	qtyArg := rest[2]
	if len(rest) == 6 {
		qtyArg = rest[5]
	}
	qty, err := strconv.Atoi(qtyArg)
	if err != nil || qty <= 0 {
		return fail(env, "order preview: quantity must be a positive integer")
	}
	var limitPtr *float64
	var trailPercentPtr, trailAmountPtr, initialStopPtr, limitOffsetPtr *float64
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			v := *limit
			limitPtr = &v
		}
		if f.Name == "trail-percent" {
			v := *trailPercent
			trailPercentPtr = &v
		}
		if f.Name == "trail-amount" {
			v := *trailAmount
			trailAmountPtr = &v
		}
		if f.Name == "initial-stop" {
			v := *initialStop
			initialStopPtr = &v
		}
		if f.Name == "limit-offset" {
			v := *limitOffset
			limitOffsetPtr = &v
		}
	})
	orderType, err := previewCLIOrderType(*orderTypeFlag, trailPercentPtr != nil || trailAmountPtr != nil || initialStopPtr != nil, limitOffsetPtr != nil)
	if err != nil {
		return fail(env, "order preview: %v", err)
	}
	if err := validatePreviewTriggerMethod(*triggerMethod); err != nil {
		return fail(env, "order preview: %v", err)
	}
	var trail *rpc.OrderTrailSpec
	if orderType == rpc.OrderTypeTRAIL || orderType == rpc.OrderTypeTRAILLIMIT {
		trail = &rpc.OrderTrailSpec{
			Basis:           rpc.OrderTrailBasisInstrumentPrice,
			TrailingPercent: trailPercentPtr,
			TrailingAmount:  trailAmountPtr,
		}
		if trail.TrailingPercent != nil {
			trail.OffsetType = rpc.OrderTrailOffsetPercent
		}
		if trail.TrailingAmount != nil {
			trail.OffsetType = rpc.OrderTrailOffsetAmount
		}
		if initialStopPtr != nil {
			trail.InitialStopPrice = *initialStopPtr
		}
		trail.LimitOffset = limitOffsetPtr
	}
	contract := rpc.ContractParams{
		Symbol:      strings.ToUpper(strings.TrimSpace(rest[1])),
		SecType:     "STK",
		Market:      strings.TrimSpace(*market),
		Exchange:    strings.ToUpper(strings.TrimSpace(*exchange)),
		PrimaryExch: strings.ToUpper(strings.TrimSpace(*primary)),
		Currency:    strings.ToUpper(strings.TrimSpace(*currency)),
	}
	if len(rest) == 6 {
		strike, err := strconv.ParseFloat(rest[4], 64)
		if err != nil || strike <= 0 {
			return fail(env, "order preview: option strike must be positive")
		}
		right := strings.ToUpper(strings.TrimSpace(rest[3]))
		if right != "C" && right != "P" && right != "CALL" && right != "PUT" {
			return fail(env, "order preview: option right must be C or P")
		}
		if right == "CALL" {
			right = "C"
		}
		if right == "PUT" {
			right = "P"
		}
		contract.SecType = "OPT"
		contract.Expiry = strings.TrimSpace(rest[2])
		contract.Right = right
		contract.Strike = strike
		contract.Multiplier = 100
		if contract.Exchange == "" {
			contract.Exchange = "SMART"
		}
	}
	params := rpc.OrderPreviewParams{
		Action:        strings.ToUpper(strings.TrimSpace(rest[0])),
		Contract:      contract,
		Quantity:      qty,
		OrderType:     orderType,
		LimitPrice:    limitPtr,
		Trail:         trail,
		TriggerMethod: *triggerMethod,
		Strategy:      strings.TrimSpace(*strategy),
		TIF:           strings.ToUpper(strings.TrimSpace(*tif)),
		OutsideRTH:    *outsideRTH,
		ReplaceID:     strings.TrimSpace(*replaceID),
		TimeoutMs:     int(timeout.Milliseconds()),
	}
	if params.Contract.Currency == "" && params.Contract.Market == "" && params.Contract.Exchange == "" && params.Contract.PrimaryExch == "" {
		params.Contract.Currency = "USD"
	}
	var res rpc.OrderPreviewResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderPreview, params, &res); err != nil {
		return fail(env, "order preview: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderPreviewText(env, &res)
	return 0
}

func validatePreviewTriggerMethod(value int) error {
	switch value {
	case 0, 1, 2, 3, 4, 7, 8:
		return nil
	default:
		return fmt.Errorf("trigger method must be one of 1, 2, 3, 4, 7, or 8")
	}
}

func previewCLIOrderType(raw string, hasTrail, hasLimitOffset bool) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, "_", " ")
	normalized = strings.ReplaceAll(normalized, "-", " ")
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		if hasLimitOffset {
			return rpc.OrderTypeTRAILLIMIT, nil
		}
		if hasTrail {
			return rpc.OrderTypeTRAIL, nil
		}
		return rpc.OrderTypeLMT, nil
	}
	switch normalized {
	case rpc.OrderTypeLMT:
		if hasTrail || hasLimitOffset {
			return "", fmt.Errorf("LMT order type cannot include trail fields")
		}
		return normalized, nil
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return normalized, nil
	default:
		return "", fmt.Errorf("order type must be LMT, TRAIL, or TRAIL-LIMIT")
	}
}

func runOrderPlace(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order place")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	token := fs.String("preview-token", "", "submit-capable preview token")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	if strings.TrimSpace(*token) == "" {
		return fail(env, "order place: --preview-token is required")
	}
	var res rpc.OrderPlaceResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderPlace, rpc.OrderPlaceParams{PreviewToken: strings.TrimSpace(*token), Origin: env.Origin}, &res); err != nil {
		return fail(env, "order place: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderPlaceText(env, &res)
	return 0
}

func runOrderModify(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order modify")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	token := fs.String("preview-token", "", "submit-capable preview token for the replacement draft")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "order modify: usage is `canary order modify <order-ref|order-id|perm-id> --preview-token TOKEN`")
	}
	if strings.TrimSpace(*token) == "" {
		return fail(env, "order modify: --preview-token is required")
	}
	var res rpc.OrderModifyResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderModify, rpc.OrderModifyParams{ID: strings.TrimSpace(fs.Arg(0)), PreviewToken: strings.TrimSpace(*token), Origin: env.Origin}, &res); err != nil {
		return fail(env, "order modify: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderModifyText(env, &res)
	return 0
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
	if err := env.Conn.Call(ctx, rpc.MethodOrderCancel, rpc.OrderCancelParams{ID: strings.TrimSpace(fs.Arg(0)), Origin: env.Origin}, &res); err != nil {
		return fail(env, "order cancel: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderCancelText(env, &res)
	return 0
}

func renderOrderPreviewText(env *Env, res *rpc.OrderPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Order Preview  %s\n", env.statusBadge(statusConcern{Text: "TOKEN", Level: statusConcernNotice}))
	statusRow(env, out, "Mode", res.Mode)
	statusRow(env, out, "Account", res.Account)
	statusRow(env, out, "Endpoint", fmt.Sprintf("%s client %d", res.Endpoint, res.ClientID))
	statusRow(env, out, "Draft", formatOrderDraftSummary(res.Draft))
	statusRow(env, out, "Strategy", res.Draft.Strategy)
	notional := fmt.Sprintf("%.2f", res.Notional)
	if res.NotionalCurrency != "" {
		notional += " " + res.NotionalCurrency
	}
	statusRow(env, out, "Notional", notional)
	if res.BaseCurrency != "" {
		statusRow(env, out, "Notional (base)", fmt.Sprintf("%.2f %s", res.NotionalBase, res.BaseCurrency))
	}
	statusRow(env, out, "Position", fmt.Sprintf("%.4g -> %.4g (%s)", res.Position.Before, res.Position.After, res.Position.Effect))
	statusRow(env, out, "Quote", formatOrderPreviewQuote(res.Quote))
	statusRow(env, out, "WhatIf", fmt.Sprintf("%s (required=%v)", res.WhatIf.Status, res.WhatIf.RequiredForSubmit))
	statusRow(env, out, "Token minted", fmt.Sprint(res.TokenMinted))
	statusRow(env, out, "Submit eligible", fmt.Sprint(res.SubmitEligible))
	statusRow(env, out, "Token scope", res.PreviewTokenScope)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	if !res.PreviewTokenExpiresAt.IsZero() {
		statusRow(env, out, "Expires", res.PreviewTokenExpiresAt.Format(time.RFC3339))
	}
	if res.PreviewToken != "" {
		statusRow(env, out, "Token", res.PreviewToken)
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "  - %s: %s\n", w.Code, w.Message)
			if w.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", w.Action)
			}
		}
	}
	fmt.Fprintln(out)
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

func renderOrderPlaceText(env *Env, res *rpc.OrderPlaceResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Order Place  %s\n", env.statusBadge(statusConcern{Text: "SENT", Level: statusConcernNotice}))
	statusRow(env, out, "Mode", res.Mode)
	statusRow(env, out, "Account", res.Account)
	statusRow(env, out, "Order", fmt.Sprintf("%s broker_id=%d", res.OrderRef, res.ReservedOrderID))
	statusRow(env, out, "Draft", formatOrderDraftSummary(res.Draft))
	statusRow(env, out, "State", nonEmpty(res.LifecycleStatus, res.SendState))
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	fmt.Fprintln(out)
}

func renderOrderModifyText(env *Env, res *rpc.OrderModifyResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Order Modify  %s\n", env.statusBadge(statusConcern{Text: "SENT", Level: statusConcernNotice}))
	statusRow(env, out, "Order", fmt.Sprintf("%s broker_id=%d", res.OrderRef, res.ReservedOrderID))
	statusRow(env, out, "Draft", formatOrderDraftSummary(res.Draft))
	statusRow(env, out, "State", nonEmpty(res.LifecycleStatus, res.SendState))
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	fmt.Fprintln(out)
}

func renderOrderCancelText(env *Env, res *rpc.OrderCancelResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Order Cancel  %s\n", env.statusBadge(statusConcern{Text: "SENT", Level: statusConcernNotice}))
	statusRow(env, out, "Order", formatOrderViewTitle(res.Order))
	statusRow(env, out, "State", nonEmpty(res.LifecycleStatus, res.SendState))
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	fmt.Fprintln(out)
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
		if q.StaleReason != "" {
			parts = append(parts, q.StaleReason)
		}
	}
	if q.PriceAsOf != "" {
		parts = append(parts, q.PriceAsOf)
	}
	if q.SessionContext != nil && !q.SessionContext.IsOpen {
		parts = append(parts, "session "+nonEmpty(q.SessionContext.State, "closed"))
	}
	return strings.Join(parts, " | ")
}
