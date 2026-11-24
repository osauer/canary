package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
)

func runStrategies(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	subIndex := -1
	for i, arg := range args {
		switch arg {
		case "list", rpc.StrategyOperationClose, rpc.StrategyOperationReduce, rpc.StrategyOperationAdd, rpc.StrategyOperationOpen, "spreads", "spread-plan":
			subIndex = i
		}
		if subIndex >= 0 {
			break
		}
	}
	if subIndex < 0 {
		if len(args) == 1 && helpArg(args[0]) {
			return printCommandUsage(env, "strategies")
		}
		return fail(env, "strategies: use list, close, reduce, add, open, spreads, or spread-plan")
	}
	sub := args[subIndex]
	args = append(append([]string{}, args[:subIndex]...), args[subIndex+1:]...)
	switch sub {
	case "list":
		return runStrategiesList(ctx, env, args)
	case rpc.StrategyOperationClose, rpc.StrategyOperationReduce, rpc.StrategyOperationAdd, rpc.StrategyOperationOpen:
		return runStrategyOperation(ctx, env, sub, args)
	case "spreads":
		return runStrategySpreads(ctx, env, args)
	case "spread-plan":
		return runStrategySpreadPlan(ctx, env, args)
	default:
		return fail(env, "strategies: use list, close, reduce, add, open, spreads, or spread-plan")
	}
}

func runStrategiesList(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "strategies list")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "strategies list: no positional arguments are accepted")
	}
	var res rpc.PositionsResult
	if err := rpc.Call(ctx, env.Conn, api.PositionsList, rpc.PositionsListParams{}, &res); err != nil {
		return fail(env, "strategies list: %v", err)
	}
	// An unavailable read exits 0 with its authority and reason, as
	// `canary positions` does: the read succeeded and says what it could not
	// establish.
	view := rpc.NewPositionStrategiesView(&res)
	if *jsonOut {
		return printJSON(env, view)
	}
	renderStrategies(env, view)
	return 0
}

func runStrategyOperation(ctx context.Context, env *Env, operation string, args []string) int {
	fs := flagSet(env, "strategies "+operation)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	units := fs.Int("units", 0, "whole strategy units to reduce, add or open")
	limitText := fs.String("limit", "", "net limit per strategy unit; close and reduce: positive is the least received, negative the most paid; add: positive is the most paid, negative the least received; open: the net debit paid, above zero and below the strike width")
	submit := fs.Bool("submit", false, "place the guaranteed combo after an accepted broker preview")
	timeout := fs.Duration("timeout", 10*time.Second, "quote and broker-preview timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.StrategyPreviewParams{Operation: operation, Units: *units, TimeoutMs: int(timeout.Milliseconds()), Source: "strategy_cli"}
	if operation == rpc.StrategyOperationOpen {
		// A new debit vertical reads its two exact contracts from stdin, as
		// `canary strategies spreads` returns them: {"long": {...}, "short": {...}}.
		if fs.NArg() != 0 || env.Stdin == nil {
			return fail(env, "strategies open: usage is `canary strategies open --units N [--limit DEBIT] [--submit] < legs.json`, legs.json being {\"long\": CONTRACT, \"short\": CONTRACT}")
		}
		d := json.NewDecoder(io.LimitReader(env.Stdin, 8193))
		d.DisallowUnknownFields()
		var legs rpc.StrategyOpenLegs
		if d.Decode(&legs) != nil || d.Decode(new(any)) != io.EOF {
			return fail(env, "strategies open: stdin must be one JSON object with long and short contracts")
		}
		params.Open = &legs
	} else {
		if fs.NArg() != 2 {
			return fail(env, "strategies %s: usage is `canary strategies %s ID REVISION [--units N] [--limit PRICE] [--submit]`", operation, operation)
		}
		revision, err := strconv.ParseInt(fs.Arg(1), 10, 64)
		if err != nil || revision <= 0 {
			return fail(env, "strategies %s: revision must be a positive integer", operation)
		}
		params.StrategyID, params.ExpectedRevision = fs.Arg(0), revision
	}
	if operation == rpc.StrategyOperationClose && *units != 0 {
		return fail(env, "strategies close: close always applies to every remaining unit; drop --units")
	}
	if operation != rpc.StrategyOperationClose && *units <= 0 {
		return fail(env, "strategies %s: --units must be positive", operation)
	}
	var limit *float64
	if strings.TrimSpace(*limitText) != "" {
		value, err := strconv.ParseFloat(strings.TrimSpace(*limitText), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return fail(env, "strategies %s: --limit must be a finite number", operation)
		}
		limit = &value
	}
	params.LimitPrice = limit
	var preview rpc.OrderPreviewResult
	if err := rpc.Call(ctx, env.Conn, api.StrategyPreview, params, &preview); err != nil {
		return fail(env, "strategies %s: %v", operation, err)
	}
	if !*submit {
		if *jsonOut {
			return printJSON(env, preview)
		}
		renderStrategyPreview(env, preview)
		return 0
	}
	if !preview.SubmitEligible || preview.PreviewToken == "" {
		return fail(env, "strategies %s: broker preview did not accept this combo; no order was sent", operation)
	}
	var placed rpc.OrderPlaceResult
	if err := rpc.Call(ctx, env.Conn, api.OrderPlace, rpc.OrderPlaceParams{
		PreviewToken: preview.PreviewToken, TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin,
	}, &placed); err != nil {
		return fail(env, "strategies %s: %v", operation, err)
	}
	if *jsonOut {
		return printJSON(env, struct {
			Preview rpc.OrderPreviewResult `json:"preview"`
			Order   rpc.OrderPlaceResult   `json:"order"`
		}{preview, placed})
	}
	renderStrategyPreview(env, preview)
	fmt.Fprintln(env.Stdout, "Order sent as one broker-guaranteed combo.")
	statusRow(env, env.Stdout, "Order", placed.OrderRef)
	statusRow(env, env.Stdout, "State", placed.LifecycleStatus)
	fmt.Fprintln(env.Stdout)
	return 0
}

// renderStrategies prints the daemon's grouping with the account it belongs
// to. When the positions read cannot stand as account truth, the data note
// says why and an empty list reads as unavailable, never as no strategies;
// rows a stale read retained stay visible under that note as context.
func renderStrategies(env *Env, view rpc.PositionStrategiesView) {
	fmt.Fprintln(env.Stdout)
	header := "Option strategies"
	if a := view.Authority; a != nil && (a.Scope.AccountID != "" || a.Scope.AccountMode != "") {
		header += "  " + nonEmpty(a.Scope.AccountID, "—")
		if a.Scope.AccountMode != "" {
			header += " · " + a.Scope.AccountMode
		}
	}
	fmt.Fprintln(env.Stdout, header)
	writeAccountAuthorityNotice(env, env.Stdout, view.Authority)
	if len(view.Strategies) == 0 {
		if view.Unavailable() {
			fmt.Fprintln(env.Stdout, "  Option strategies unavailable.")
		} else {
			fmt.Fprintln(env.Stdout, "  No option strategies can be grouped safely.")
		}
	}
	for _, group := range view.Strategies {
		// The daemon's name for the held position leads; the shape stands in
		// where it gives none.
		name := nonEmpty(group.Name, strings.ReplaceAll(group.Kind, "_", " "))
		fmt.Fprintf(env.Stdout, "  %s  %s  %d unit(s)  %s  revision %d\n", group.ID, group.Underlying, group.Units, name, group.Revision)
		for _, leg := range group.Legs {
			direction := "long"
			if leg.Ratio < 0 {
				direction = "short"
			}
			fmt.Fprintf(env.Stdout, "    %s %d × %s %s %.2f %s  conID %d\n", direction, absCLIInt(leg.Ratio), leg.Contract.Expiry, leg.Contract.Right, leg.Contract.Strike, leg.Contract.TradingClass, leg.Contract.ConID)
		}
		if stock := group.Stock; stock != nil {
			fmt.Fprintf(env.Stdout, "    against %.0f shares of %s  conID %d\n", stock.Quantity, stock.Contract.Symbol, stock.Contract.ConID)
		}
		if !group.Actionable && group.Reason != "" {
			fmt.Fprintf(env.Stdout, "    %s\n", group.Reason)
		}
	}
	for _, issue := range view.Issues {
		fmt.Fprintf(env.Stdout, "  %s remains ungrouped: %s.\n", issue.Underlying, issue.Reason)
	}
	fmt.Fprintln(env.Stdout)
}

func renderStrategyPreview(env *Env, preview rpc.OrderPreviewResult) {
	fmt.Fprintln(env.Stdout)
	group := preview.Draft.StrategyGroup
	if group == nil {
		fmt.Fprintln(env.Stdout, "Strategy preview is incomplete.")
		return
	}
	// A close or reduction sells the combination, so a positive limit is
	// received; an add buys it, so a positive limit is paid.
	adding := group.Operation == rpc.StrategyOperationAdd || group.Operation == rpc.StrategyOperationOpen
	switch {
	case group.Operation == rpc.StrategyOperationOpen:
		fmt.Fprintf(env.Stdout, "Open %d spread unit(s) of %s as one combination\n", group.Units, preview.Draft.Contract.Symbol)
	case adding:
		fmt.Fprintf(env.Stdout, "Add %d strategy unit(s) to %d held\n", group.Units, group.UnitsBefore)
	case group.Operation == rpc.StrategyOperationClose:
		fmt.Fprintf(env.Stdout, "Close %d of %d strategy unit(s)\n", group.Units, group.UnitsBefore)
	default:
		fmt.Fprintf(env.Stdout, "Reduce %d of %d strategy unit(s)\n", group.Units, group.UnitsBefore)
	}
	switch {
	case preview.Draft.LimitPrice == 0:
		statusRow(env, env.Stdout, "Limit", "No net debit or credit")
	case (preview.Draft.LimitPrice > 0) != adding:
		statusRow(env, env.Stdout, "Limit", fmt.Sprintf("Receive at least %.2f per strategy", math.Abs(preview.Draft.LimitPrice)))
	default:
		statusRow(env, env.Stdout, "Limit", fmt.Sprintf("Pay up to %.2f per strategy", math.Abs(preview.Draft.LimitPrice)))
	}
	if adding {
		statusRow(env, env.Stdout, "After", fmt.Sprintf("%d strategy unit(s) held", group.UnitsAfter))
	} else {
		statusRow(env, env.Stdout, "After", fmt.Sprintf("%d strategy unit(s) remain", group.UnitsAfter))
	}
	for _, warning := range preview.Warnings {
		if adding && warning.Message != "" {
			statusRow(env, env.Stdout, "Note", warning.Message)
		}
	}
	statusRow(env, env.Stdout, "Broker preview", nonEmpty(preview.WhatIf.Status, "unavailable"))
	// The label fits the row's column; a longer one pushed its value onto a
	// line of its own.
	ready := "no"
	if preview.SubmitEligible {
		ready = "yes"
	}
	statusRow(env, env.Stdout, "Can submit", ready)
	for _, leg := range group.Legs {
		fmt.Fprintf(env.Stdout, "  %s %d  %s %s %.2f  %.0f → %.0f\n", leg.Action, leg.Quantity, leg.Contract.Expiry, leg.Contract.Right, leg.Contract.Strike, leg.Before, leg.After)
	}
	if !preview.SubmitEligible {
		fmt.Fprintln(env.Stdout, "No order was sent. Resolve the broker-preview message before trying again.")
	} else {
		fmt.Fprintln(env.Stdout, "No order was sent. Add --submit to send this exact operation after a fresh preview.")
	}
	fmt.Fprintln(env.Stdout)
}

func absCLIInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// runStrategySpreads reads the pricing evidence for opening a debit vertical:
// the sold strikes beyond one exact bought option, as JSON only, like add
// market, whose exact contract it takes.
func runStrategySpreads(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "strategies spreads", "Price debit verticals for one exact bought option: sold strikes beyond it, net debit, maximum gain and loss, breakeven and a default; no orders placed", "canary strategies spreads [--json] < request.json")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		renderAddRequestHelp(env, []string{
			"Stdin: one JSON object; unknown fields and extra objects are rejected. Output is always JSON, with or without --json.",
			"underlying (required): stock contract with symbol, sec_type STK, currency USD and exchange SMART; con_id is optional when known.",
			"long (required): the exact option to buy, as add market returns it: sec_type OPT, right C (bull call spread) or P (bear put spread), con_id, expiry, strike, multiplier 100, trading_class.",
			"candidates lists the sold strikes of the same expiry beyond it (above a call, below a put), each with its quote, width, net_bid, net_ask, net_mid, max_loss_per_unit, max_gain_per_unit (one spread, in USD at the net midpoint), breakeven, saving and eligible. default is the index of the proposed sold strike, -1 with default_reason; rule says how it was chosen and whether its numbers come from the opportunity policy's [add].spread or Canary's default.",
			"Read option_mode: planning prices are not executable quotes. Open the chosen spread with `canary strategies open --units N --limit DEBIT < legs.json`, legs.json holding {\"long\": the long contract, \"short\": the candidate's short contract}.",
		}, `canary strategies spreads <<'JSON'
{
  "underlying": {"symbol": "SYNA", "sec_type": "STK", "currency": "USD", "exchange": "SMART"},
  "long": {"con_id": 910604, "symbol": "SYNA", "sec_type": "OPT", "exchange": "SMART", "currency": "USD", "expiry": "20270115", "strike": 50, "right": "C", "multiplier": 100, "trading_class": "SYNA"}
}
JSON`)
	}
	_ = fs.Bool("json", false, "emit typed spread evidence")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || env.Stdin == nil {
		return fail(env, "strategies spreads reads one JSON request from stdin")
	}
	d := json.NewDecoder(io.LimitReader(env.Stdin, 8193))
	d.DisallowUnknownFields()
	var p rpc.StrategySpreadsParams
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return fail(env, "invalid strategies spreads request")
	}
	var out rpc.StrategySpreadsResult
	if err := rpc.Call(ctx, env.Conn, api.StrategySpreads, p, &out); err != nil {
		return fail(env, "strategies spreads: %v", err)
	}
	return printJSON(env, out)
}

// runStrategySpreadPlan sizes a debit vertical for a budget, as add
// call-plan sizes one option; JSON only.
func runStrategySpreadPlan(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "strategies spread-plan", "Size a debit vertical for a budget with exact broker previews of the combination; no orders placed", "canary strategies spread-plan [--json] < request.json")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		renderAddRequestHelp(env, []string{
			"Stdin: one JSON object; unknown fields and extra objects are rejected. Output is always JSON, with or without --json.",
			"long and short (required): the exact contracts strategies spreads returned. limit_price (required): the net debit per spread, per share. budget (required): total USD debit including the broker fee bound.",
			"units is the most spreads that fit; held_by says what held it: budget, option_cap ([order_limits].max_option_contracts per leg), order_cap (the order cap on the net premium, order_cap_units) or hedge_room (a bear put spread on a Rulebook hedge symbol, sized under rule 12's over-hedge line with the spread's net_delta; index_protection carries the measurement, its unmeasured reason when only the other limits held it).",
			"The options session must be open. Sizing returns no submission token and reserves no money.",
		}, `canary strategies spread-plan <<'JSON'
{"long": {...}, "short": {...}, "limit_price": 3.45, "budget": 2000}
JSON`)
	}
	_ = fs.Bool("json", false, "emit the typed plan")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || env.Stdin == nil {
		return fail(env, "strategies spread-plan reads one JSON request from stdin")
	}
	d := json.NewDecoder(io.LimitReader(env.Stdin, 8193))
	d.DisallowUnknownFields()
	var p rpc.StrategySpreadPlanParams
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return fail(env, "invalid strategies spread-plan request")
	}
	var out rpc.StrategySpreadPlanResult
	if err := rpc.Call(ctx, env.Conn, api.StrategySpreadPlan, p, &out); err != nil {
		return fail(env, "strategies spread-plan: %v", err)
	}
	return printJSON(env, out)
}
