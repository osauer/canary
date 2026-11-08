package cli

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// runEdgeReplay is `canary edge replay`: the daemon replays long-option exit
// rules over the owner's own option trades and this view prints the result.
func runEdgeReplay(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge")
	from := fs.String("from", "", "first day YYYY-MM-DD (default: every retained statement)")
	to := fs.String("to", "", "last day YYYY-MM-DD (default: the latest statement)")
	rules := fs.String("rules", "", "comma-separated sets A-D and single rules "+strings.Join(rpc.EdgeReplayRules, ", ")+" (default: all)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	params := rpc.EdgeReplayParams{From: *from, To: *to}
	if strings.TrimSpace(*rules) != "" {
		params.Rules = []string{*rules}
	}
	params, err := rpc.NormalizeEdgeReplayParams(params)
	if err != nil {
		return fail(env, "edge replay: %v", err)
	}
	var result rpc.EdgeReplayResult
	if err := rpc.Call(ctx, env.Conn, api.EdgeReplay, params, &result); err != nil {
		return fail(env, "edge replay: %v", err)
	}
	if err := rpc.ValidateEdgeReplayResult(result); err != nil {
		return fail(env, "edge replay: daemon returned an invalid result: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderEdgeReplay(env, result)
	return 0
}

// edgeReplayLabels names each run for a reader.
var edgeReplayLabels = map[string]string{
	rpc.EdgeReplaySetActual: "A actual", rpc.EdgeReplaySetTodays: "B today's rules", rpc.EdgeReplaySetHarvest: "C harvest", rpc.EdgeReplaySetStockTrail: "D stock trail",
	rpc.EdgeReplayRuleLossExit: "loss exit", rpc.EdgeReplayRuleExpiryClose: "expiry close", rpc.EdgeReplayRuleProfitTrail: "profit trail",
	rpc.EdgeReplayRuleRecoverCost: "recover cost", rpc.EdgeReplayRuleTrimToCap: "trim to cap", rpc.EdgeReplayRuleStockTrail: "stock trail",
	"sold": "sold", "expiry": "expired", "exercise": "exercised", "assignment": "assigned", "expiry_unbooked": "expired",
}

func edgeReplayLabel(id string) string {
	if label, ok := edgeReplayLabels[id]; ok {
		return label
	}
	return sanitizeRunText(id)
}

func renderEdgeReplay(env *Env, result rpc.EdgeReplayResult) {
	out := env.Stdout
	width := briefProseWidth(out)
	ccy := result.BaseCurrency
	title := []string{"Canary Edge", "exit replay"}
	if !result.From.IsZero() {
		title = append(title, result.From.Format(time.DateOnly)+" to "+result.To.Format(time.DateOnly))
	}
	if result.State != rpc.EdgeStateCurrent {
		title = append(title, env.yellow(result.State))
	} else if result.Incomplete {
		title = append(title, env.yellow("incomplete"))
	}
	fmt.Fprintln(out, strings.Join(title, " · "))
	if result.State != rpc.EdgeStateCurrent {
		edgeProse(env, "No replay: "+edgeReplayReasonText(result.Reason)+".", width)
		renderEdgeReplayMissing(env, result)
		return
	}
	renderEdgeReplayMissing(env, result)
	edgeProse(env, env.dim("Your own long-option buys, with each rule set's exits in place of yours. A review of past trades; it places nothing."), width)

	scope := result.Scope
	fmt.Fprintln(out, "\nScope")
	edgeRow(env, "Contracts", fmt.Sprintf("%d long options on %s", scope.Contracts, strings.Join(scope.Underlyings, ", ")),
		fmt.Sprintf("%d buy lots, %d held at the start of %s", scope.BuyLots, scope.PreheldLots, result.StartStateDate.Format(time.DateOnly)),
		groupThousands(strconv.FormatFloat(scope.ContractsBought, 'f', 0, 64))+" contracts bought")
	excluded := []string{fmt.Sprintf("%d hedge puts (%d executions)", scope.ExcludedHedgeContracts, scope.ExcludedHedgeExecutions), fmt.Sprintf("%d short options (%d executions)", scope.ExcludedShortContracts, scope.ExcludedShortExecutions)}
	for _, reason := range slices.Sorted(maps.Keys(scope.ExcludedOther)) {
		excluded = append(excluded, fmt.Sprintf("%d %s", scope.ExcludedOther[reason], strings.ReplaceAll(reason, "_", " ")))
	}
	if n := len(result.MissingHistory); n > 0 {
		contracts := 0
		for _, gap := range result.MissingHistory {
			contracts += gap.Contracts
		}
		excluded = append(excluded, fmt.Sprintf("%d %s without daily history (%d %s)", n, pluralWord(n, "underlying", "underlyings"), contracts, pluralWord(contracts, "contract", "contracts")))
	}
	edgeRow(env, "Excluded", excluded...)
	params := result.Parameters
	costs := []string{fmt.Sprintf("%d statement days", result.StatementDays)}
	if params.HalfSpreadPct != nil {
		costs = append(costs, fmt.Sprintf("exits pay a half-spread of %.2f%% of the mark, the median of %d measured option fills", *params.HalfSpreadPct, params.HalfSpreadSamples))
	} else {
		costs = append(costs, "no measured option fill: exits sell at the mark")
	}
	if params.CommissionPerContractBase != nil {
		costs = append(costs, fmt.Sprintf("commission %s a contract, your average", formatMoneyCcyForPnL(*params.CommissionPerContractBase, ccy)))
	}
	edgeRow(env, "Prices", costs...)

	for _, run := range result.Runs {
		if run.Prices != rpc.EdgeReplayPricesRecorded {
			continue
		}
		cov := run.Coverage
		parts := []string{fmt.Sprintf("held contract-days priced by %s statement marks, %s execution closing prices and %s recorded chain quotes", edgeCount(cov.StatementDays), edgeCount(cov.TradeCloseDays), edgeCount(cov.ChainMidDays))}
		if cov.ClosedSessionDays > 0 {
			parts = append(parts, fmt.Sprintf("%s carried over days the exchange was closed", edgeCount(cov.ClosedSessionDays)))
		}
		parts = append(parts, fmt.Sprintf("%d untestable %s", cov.UntestableEvents, pluralWord(cov.UntestableEvents, "day", "days")))
		edgeRow(env, "Coverage in "+edgeReplayLabel(run.ID), parts...)
		break
	}

	fmt.Fprintln(out, "\nRules in force")
	edgeRow(env, "Policies", sanitizeRunText(params.PolicySource))
	value := func(name string) float64 {
		for _, rule := range params.Rules {
			if rule.Name == name {
				return rule.Value
			}
		}
		return math.NaN()
	}
	edgeRow(env, "Today's", fmt.Sprintf("loss exit at -%g%%", value("loss_exit_pct")), fmt.Sprintf("expiry close in the money at %g days", value("expiry_close_dte")),
		fmt.Sprintf("profit trail arms at +%g%%, trails %g%%, keeps +%g%%, from %g days", value("profit_arm_gain_pct"), value("profit_trail_pct"), value("locked_gain_pct"), value("profit_trail_min_dte")))
	edgeRow(env, "Harvest", fmt.Sprintf("recover cost at +%g%%", value("recover_cost_gain_pct")), fmt.Sprintf("trim each name to %g%% of NLV", value("option_name_act_pct")))
	edgeRow(env, "Stock trail", fmt.Sprintf("%g × the %g-session average true range below the high since the buy", value("stock_trail_range_multiple"), value("stock_trail_atr_sessions")))

	for _, prices := range []string{rpc.EdgeReplayPricesRecorded, rpc.EdgeReplayPricesModelled} {
		renderEdgeReplayRuns(env, result, prices, width)
	}
	renderEdgeReplayNames(env, result)
	renderEdgeReplayReconciliation(env, result)
	fmt.Fprintln(out, "\nDetails: canary edge replay --json · the method and every amount")
}

func edgeReplayReasonText(reason string) string {
	switch reason {
	case "flex_configuration_required":
		return "Flex statements are not configured; see canary edge"
	case "account_scope_unavailable", "account_scope_changed":
		return "the broker account is not settled yet"
	case "statement_account_pending":
		return "no statements are retained yet"
	case "open_positions_unavailable":
		return "the statements carry no Open Positions section"
	case "statement_history_too_short", "start_snapshot_unavailable":
		return "no statement before the window gives a starting position"
	case "no_long_options_in_window":
		return "no long option was held or bought in the window"
	case "underlying_history_unavailable":
		return "no underlying in the window has the daily history its days need"
	}
	return strings.ReplaceAll(sanitizeRunText(reason), "_", " ")
}

func renderEdgeReplayRuns(env *Env, result rpc.EdgeReplayResult, prices string, width int) {
	out := env.Stdout
	ccy := result.BaseCurrency
	var runs []rpc.EdgeReplayRun
	for _, run := range result.Runs {
		if run.Prices == prices || prices == rpc.EdgeReplayPricesRecorded && run.Prices == rpc.EdgeReplayPricesActual {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return
	}
	if prices == rpc.EdgeReplayPricesRecorded {
		fmt.Fprintln(out, "\nRecorded prices  "+env.dim("a day without a recorded mark keeps your actual outcome"))
	} else {
		fmt.Fprintf(out, "\nModelled prices  %s\n", env.dim(fmt.Sprintf("gaps priced by Black-Scholes off the last mark, rate %g%%", result.Parameters.ModelRatePct)))
	}
	fmt.Fprintf(out, "  %-17s %13s %14s %6s %12s %7s\n", "RUN", "P/L", "VS ACTUAL", "EXITS", "PEAK VALUE", "OF NLV")
	for _, run := range runs {
		diff := "—"
		if run.Prices != rpc.EdgeReplayPricesActual {
			diff = edgeSignedMoney(env, run.DiffVsActualBase, ccy)
		}
		share := ""
		if run.PeakPctNLV != nil {
			share = fmt.Sprintf("%.0f%%", *run.PeakPctNLV)
		}
		fmt.Fprintf(out, "  %-17s %s %s %6d %12s %7s\n", edgeReplayLabel(run.ID), padLeftVisible(edgeSignedMoney(env, run.PNLBase, ccy), 13), padLeftVisible(diff, 14), run.Exits, edgeReplayWhole(run.PeakValueBase, ccy), share)
		var detail []string
		for _, reason := range run.ExitsByReason {
			detail = append(detail, fmt.Sprintf("%s %d", edgeReplayLabel(reason.Reason), reason.Events))
		}
		if run.Prices != rpc.EdgeReplayPricesActual {
			t := run.VsOwner
			detail = append(detail, fmt.Sprintf("contracts sold %s earlier, %s later, %s same as you", edgeCount(t.Earlier), edgeCount(t.Later), edgeCount(t.Same)))
			cov := run.Coverage
			if t.Untestable > 0 {
				detail = append(detail, fmt.Sprintf("%s kept your outcome after %d untestable %s", edgeCount(t.Untestable), cov.UntestableEvents, pluralWord(cov.UntestableEvents, "day", "days")))
			}
			if cov.ModelledDays > 0 {
				detail = append(detail, fmt.Sprintf("%s modelled contract-days", edgeCount(cov.ModelledDays)))
			}
			if cov.CloseOnlyRangeDays > 0 {
				detail = append(detail, fmt.Sprintf("%s daily %s measured from closes only", edgeCount(cov.CloseOnlyRangeDays), pluralWord(cov.CloseOnlyRangeDays, "range", "ranges")))
			}
			if cov.TrailUnmeasuredDays > 0 {
				detail = append(detail, fmt.Sprintf("trail unmeasured on %s contract-%s, under 21 sessions of history", edgeCount(cov.TrailUnmeasuredDays), pluralWord(cov.TrailUnmeasuredDays, "day", "days")))
			}
		}
		if len(detail) == 0 {
			continue
		}
		text := env.dim(strings.Join(detail, " · "))
		for _, line := range wrapVisibleText(text, width-6) {
			fmt.Fprintln(out, "    "+line)
		}
	}
}

// renderEdgeReplayMissing heads the view when names were left out for want of
// daily history: which, why, and the owner's own P/L the totals leave out.
func renderEdgeReplayMissing(env *Env, result rpc.EdgeReplayResult) {
	if !result.Incomplete || len(result.MissingHistory) == 0 {
		return
	}
	n := len(result.MissingHistory)
	edgeRow(env, env.yellow("Incomplete"), fmt.Sprintf("%d of %d underlyings left out of every run, yours included: their daily history does not span the days their options were held", n, len(result.Scope.Underlyings)+n))
	byReason := map[string][]string{}
	pnl := 0.0
	for _, gap := range result.MissingHistory {
		byReason[gap.Reason] = append(byReason[gap.Reason], sanitizeRunText(gap.Underlying))
		pnl += gap.ActualPNLBase
	}
	var parts []string
	for _, reason := range slices.Sorted(maps.Keys(byReason)) {
		parts = append(parts, strings.Join(byReason[reason], ", ")+": "+edgeReplayHistoryReasonText(reason))
	}
	parts = append(parts, "your own P/L on "+pluralWord(n, "it", "them")+", "+edgeSignedMoney(env, pnl, result.BaseCurrency)+", is in no total below")
	edgeRow(env, "Left out", parts...)
}

func edgeReplayHistoryReasonText(reason string) string {
	switch reason {
	case rpc.EdgeReplayHistoryGatewayOffline:
		return "the broker gateway is offline and Canary has not retained these days"
	case rpc.EdgeReplayHistoryNotRetained:
		return "the broker no longer serves these days and Canary did not retain them"
	case rpc.EdgeReplayHistoryInstrumentUnavailable:
		return "the broker has no history for the instrument"
	case rpc.EdgeReplayHistoryReadFailed:
		return "the broker's history read failed"
	case rpc.EdgeReplayHistoryFetching:
		return "the broker is still sending this history; the next run includes it"
	}
	return strings.ReplaceAll(sanitizeRunText(reason), "_", " ")
}

// edgeReplayWhole is a peak value in whole currency units.
func edgeReplayWhole(v float64, ccy string) string {
	return moneyPrefix(ccy) + groupThousands(strconv.FormatFloat(math.Round(v), 'f', 0, 64))
}

// renderEdgeReplayNames compares the sets name by name on recorded prices.
func renderEdgeReplayNames(env *Env, result rpc.EdgeReplayResult) {
	out := env.Stdout
	var runs []rpc.EdgeReplayRun
	for _, run := range result.Runs {
		if (run.Prices == rpc.EdgeReplayPricesActual || run.Prices == rpc.EdgeReplayPricesRecorded) && len(run.ID) == 1 {
			runs = append(runs, run)
		}
	}
	if len(runs) < 2 || len(result.Scope.Underlyings) == 0 {
		return
	}
	fmt.Fprintln(out, "\nBy underlying  "+env.dim("recorded prices"))
	var header strings.Builder
	fmt.Fprintf(&header, "  %-8s", "NAME")
	for _, run := range runs {
		fmt.Fprintf(&header, " %13s", "SET "+run.ID)
	}
	fmt.Fprintln(out, header.String())
	for _, name := range result.Scope.Underlyings {
		var line strings.Builder
		fmt.Fprintf(&line, "  %-8s", sanitizeRunText(name))
		for _, run := range runs {
			value := 0.0
			for _, row := range run.ByUnderlying {
				if row.Underlying == name {
					value = row.PNLBase
				}
			}
			line.WriteString(" " + padLeftVisible(edgeSignedMoney(env, value, result.BaseCurrency), 13))
		}
		fmt.Fprintln(out, line.String())
	}
}

func renderEdgeReplayReconciliation(env *Env, result rpc.EdgeReplayResult) {
	out := env.Stdout
	rec := result.Reconciliation
	ccy := result.BaseCurrency
	status := env.green("reconciled")
	switch {
	case !rec.Reconciled || rec.Status == rpc.EdgeReplayUnreconciled:
		status = env.yellow("does not reconcile")
	case rec.Status == rpc.EdgeReplayReconciledPartly:
		status = env.green("balanced") + fmt.Sprintf(" on %d of %d contracts", rec.ComparedContracts, rec.EdgeCycleContracts)
	}
	fmt.Fprintln(out, "\nReconciliation  "+status)
	edgeRow(env, "Actual", edgeSignedMoney(env, rec.ActualUnitsBase, ccy)+" = cash flows "+edgeSignedMoney(env, rec.FlowsBase, ccy)+" + end value "+formatMoneyCcyForPnL(rec.EndValueBase, ccy)+" − start value "+formatMoneyCcyForPnL(rec.StartValueBase, ccy))
	edgeRow(env, "Edge cycles", fmt.Sprintf("%d proven flat-to-flat in scope, %d of %d contracts compared", rec.EdgeCycles, rec.ComparedContracts, rec.EdgeCycleContracts),
		"Edge "+edgeSignedMoney(env, rec.EdgeCyclesPNLBase, ccy)+" + FX translation "+edgeSignedMoney(env, rec.FXTranslationBase, ccy)+" = actual "+edgeSignedMoney(env, rec.ActualOnCyclesBase, ccy),
		"residual "+formatMoneyCcyForPnL(rec.ResidualBase, ccy))
	if len(rec.ResidualContracts) > 0 {
		edgeRow(env, env.yellow("Residual on"), edgeReplayReconContracts(rec.ResidualContracts)...)
	}
	if len(rec.NotCompared) > 0 {
		edgeRow(env, "Not compared", edgeReplayReconContracts(rec.NotCompared)...)
	}
	if rec.BrokerRealizedBase != nil {
		realized := "broker realized on the window's executions " + edgeSignedMoney(env, *rec.BrokerRealizedBase, ccy)
		if rec.BrokerRealizedGaps > 0 {
			realized += fmt.Sprintf(" (%d executions without broker P/L)", rec.BrokerRealizedGaps)
		}
		edgeRow(env, "Realized", realized)
	}
}

// edgeReplayReconContracts lists reconciliation contracts by reason: the
// contracts, then why.
func edgeReplayReconContracts(rows []rpc.EdgeReplayReconContract) []string {
	byReason := map[string][]string{}
	for _, row := range rows {
		byReason[row.Reason] = append(byReason[row.Reason], sanitizeRunText(row.Contract))
	}
	var parts []string
	for _, reason := range slices.Sorted(maps.Keys(byReason)) {
		parts = append(parts, strings.Join(byReason[reason], ", ")+": "+edgeReplayReconReasonText(reason))
	}
	return parts
}

func edgeReplayReconReasonText(reason string) string {
	switch reason {
	case rpc.EdgeReplayNotComparedHeldAtStart:
		return "held at the window's start"
	case rpc.EdgeReplayNotComparedOutsideCycle:
		return "also traded outside Edge's cycle"
	case rpc.EdgeReplayNotComparedBrokerPNL:
		return "the broker's P/L is incomplete"
	case rpc.EdgeReplayNotComparedExercised:
		return "exercised or assigned, so the broker's P/L on the option carries value moved into or out of the stock"
	case rpc.EdgeReplayResidualCashVsRealized:
		return "cash flows differ from the broker's realized P/L"
	}
	return strings.ReplaceAll(sanitizeRunText(reason), "_", " ")
}
