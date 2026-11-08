package cli

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

func runStrategies(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	subIndex := -1
	for i, arg := range args {
		switch arg {
		case "list", rpc.StrategyOperationClose, rpc.StrategyOperationReduce:
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
		return fail(env, "strategies: use list, close, or reduce")
	}
	sub := args[subIndex]
	args = append(append([]string{}, args[:subIndex]...), args[subIndex+1:]...)
	switch sub {
	case "list":
		return runStrategiesList(ctx, env, args)
	case rpc.StrategyOperationClose, rpc.StrategyOperationReduce:
		return runStrategyOperation(ctx, env, sub, args)
	default:
		return fail(env, "strategies: use list, close, or reduce")
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
	units := fs.Int("units", 0, "whole strategy units to reduce")
	limitText := fs.String("limit", "", "net limit per strategy; positive is a credit, negative is a debit")
	submit := fs.Bool("submit", false, "place the guaranteed combo after an accepted broker preview")
	timeout := fs.Duration("timeout", 10*time.Second, "quote and broker-preview timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "strategies %s: usage is `canary strategies %s ID REVISION [--units N] [--limit PRICE] [--submit]`", operation, operation)
	}
	revision, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil || revision <= 0 {
		return fail(env, "strategies %s: revision must be a positive integer", operation)
	}
	if operation == rpc.StrategyOperationClose && *units != 0 {
		return fail(env, "strategies close: close always applies to every remaining unit; drop --units")
	}
	if operation == rpc.StrategyOperationReduce && *units <= 0 {
		return fail(env, "strategies reduce: --units must be positive")
	}
	var limit *float64
	if strings.TrimSpace(*limitText) != "" {
		value, err := strconv.ParseFloat(strings.TrimSpace(*limitText), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return fail(env, "strategies %s: --limit must be a finite number", operation)
		}
		limit = &value
	}
	params := rpc.StrategyPreviewParams{
		StrategyID: fs.Arg(0), ExpectedRevision: revision, Operation: operation, Units: *units,
		LimitPrice: limit, TimeoutMs: int(timeout.Milliseconds()), Source: "strategy_cli",
	}
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
		fmt.Fprintf(env.Stdout, "  %s  %s  %d unit(s)  %s  revision %d\n", group.ID, group.Underlying, group.Units, strings.ReplaceAll(group.Kind, "_", " "), group.Revision)
		for _, leg := range group.Legs {
			direction := "long"
			if leg.Ratio < 0 {
				direction = "short"
			}
			fmt.Fprintf(env.Stdout, "    %s %d × %s %s %.2f %s  conID %d\n", direction, absCLIInt(leg.Ratio), leg.Contract.Expiry, leg.Contract.Right, leg.Contract.Strike, leg.Contract.TradingClass, leg.Contract.ConID)
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
	label := "Reduce"
	if group.Operation == rpc.StrategyOperationClose {
		label = "Close"
	}
	fmt.Fprintf(env.Stdout, "%s %d of %d strategy unit(s)\n", label, group.Units, group.UnitsBefore)
	switch {
	case preview.Draft.LimitPrice > 0:
		statusRow(env, env.Stdout, "Limit", fmt.Sprintf("Receive at least %.2f per strategy", preview.Draft.LimitPrice))
	case preview.Draft.LimitPrice < 0:
		statusRow(env, env.Stdout, "Limit", fmt.Sprintf("Pay up to %.2f per strategy", math.Abs(preview.Draft.LimitPrice)))
	default:
		statusRow(env, env.Stdout, "Limit", "No net debit or credit")
	}
	statusRow(env, env.Stdout, "After", fmt.Sprintf("%d strategy unit(s) remain", group.UnitsAfter))
	statusRow(env, env.Stdout, "Broker preview", nonEmpty(preview.WhatIf.Status, "unavailable"))
	statusRow(env, env.Stdout, "Ready to submit", fmt.Sprintf("%v", preview.SubmitEligible))
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
