package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runEdge(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge")
	window := fs.String("window", "365d", "optional review override: 90d or 365d (default 365d)")
	horizon := fs.Int("horizon", 0, "optional highlighted horizon override: 1, 5, or 20 (default automatic)")
	limit := fs.Int("limit", rpc.MaxEdgeFindings, "maximum findings: 1-3")
	change := fs.String("change", "", "opaque change ID for one detailed result")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	params, err := rpc.NormalizeEdgeSnapshotParams(rpc.EdgeSnapshotParams{Window: *window, HorizonSessions: *horizon, Limit: *limit, ChangeID: *change})
	if err != nil {
		return fail(env, "edge: %v", err)
	}
	var result rpc.EdgeResult
	if err := env.Conn.Call(ctx, rpc.MethodEdgeSnapshot, params, &result); err != nil {
		return fail(env, "edge: %v", err)
	}
	if err := rpc.ValidateEdgeResult(result); err != nil {
		return fail(env, "edge: daemon returned an invalid result: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderEdgeText(env.Stdout, result)
	return 0
}

func renderEdgeText(out io.Writer, result rpc.EdgeResult) {
	heading := fmt.Sprintf("%s decision review · %d-session headline", result.Window, result.HorizonSessions)
	if result.AutomaticHorizon {
		period := result.Window
		if result.Window == "365d" {
			period = "one-year"
		}
		heading = fmt.Sprintf("automatic %s decision review · selected %s", period, edgeSessionCount(result.HorizonSessions))
	}
	fmt.Fprintf(out, "Canary Edge — %s", heading)
	if result.State != rpc.EdgeStateCurrent {
		fmt.Fprintf(out, " · %s", result.State)
	}
	fmt.Fprintln(out)
	if result.State == rpc.EdgeStateActionRequired && result.Setup != nil {
		fmt.Fprintln(out, "  Flex setup is required before Canary can calculate broker-truth results.")
		for i, step := range result.Setup.Steps {
			fmt.Fprintf(out, "  %d. %s\n", i+1, step)
		}
		if missing := result.Setup.MissingRequirements; len(missing) > 0 {
			shown := missing
			suffix := ""
			if len(shown) > 8 {
				shown = shown[:8]
				suffix = fmt.Sprintf(" (+%d more; use --json)", len(missing)-len(shown))
			}
			fmt.Fprintf(out, "  Missing (%d): %s%s\n", len(missing), strings.Join(shown, ", "), suffix)
		}
		return
	}
	if result.Headline != "" {
		fmt.Fprintln(out, "  "+result.Headline)
	}
	if len(result.MarketContext) > 0 || len(result.MarketContextMissing) > 0 {
		parts := make([]string, 0, len(result.MarketContext)+len(result.MarketContextMissing))
		for _, context := range result.MarketContext {
			value := edgeSignedPercent(context.MedianChangePct)
			if context.Kind == "volatility_index" && context.MedianChangePoints != nil {
				value = fmt.Sprintf("%+.2f pts", *context.MedianChangePoints)
			}
			parts = append(parts, fmt.Sprintf("%s %s (n=%d)", context.Label, value, context.SampleCount))
		}
		for _, key := range result.MarketContextMissing {
			parts = append(parts, edgeMarketContextLabel(key)+" unavailable")
		}
		fmt.Fprintln(out, "  Market context  "+strings.Join(parts, " · "))
	}
	if result.Account != nil {
		fmt.Fprintf(out, "  Account P/L  %s  %s → %s  flows %s\n", edgeMoney(result.Account.ProfitLossBase, result.Account.BaseCurrency), result.Account.ActualFrom.Format("2006-01-02"), result.Account.ActualTo.Format("2006-01-02"), edgeMoney(result.Account.ExternalFlowsBase, result.Account.BaseCurrency))
	}
	if len(result.ActionRollups) > 0 {
		for _, row := range result.ActionRollups {
			values := make([]string, 0, 3)
			for _, horizon := range row.Horizons {
				values = append(values, edgeRollupCell(horizon, edgeBaseCurrency(result)))
			}
			for len(values) < 3 {
				values = append(values, "—")
			}
			fmt.Fprintf(out, "  %-5s  1s %s · 5s %s · 20s %s\n", strings.ToUpper(row.Action), values[0], values[1], values[2])
		}
	}
	for _, finding := range result.Findings {
		context := ""
		if len(finding.MarketContext) > 0 {
			context = " · " + edgeFindingContext(finding.MarketContext)
		}
		fmt.Fprintf(out, "  %s (%+.2f%%)  %s %s · %s%s\n", edgeMoney(finding.DecisionImpactBase, edgeBaseCurrency(result)), finding.DecisionImpactPct, finding.Symbol, finding.Action, finding.ChangeID, context)
	}
	if result.OptionsTotalCount > 0 {
		shown := fmt.Sprintf("%d result(s)", result.OptionsTotalCount)
		if result.OptionsTruncated {
			shown = fmt.Sprintf("%d of %d result(s)", len(result.Options), result.OptionsTotalCount)
		}
		fmt.Fprintf(out, "  Options · actual only  %s, ranked by absolute actual P/L; no historical counterfactual\n", shown)
	}
	fmt.Fprintf(out, "  Coverage  %d/%d eligible · scored %d/%d eligible at %s (%.1f%%) · largest action n=%d", result.Coverage.EligibleChanges, result.Coverage.TradeChanges, result.HorizonSelection.ScoredChanges, result.HorizonSelection.EligibleChanges, edgeSessionCount(result.HorizonSessions), result.HorizonSelection.CoveragePct, result.HorizonSelection.LargestActionSample)
	if !result.LastFullRevalidation.IsZero() {
		fmt.Fprintf(out, " · full %s", result.LastFullRevalidation.Local().Format("2006-01-02"))
	}
	fmt.Fprintln(out)
	if result.Change != nil {
		renderEdgeChange(out, *result.Change, edgeBaseCurrency(result))
	}
}

func renderEdgeChange(out io.Writer, change rpc.EdgeChangeDetail, currency string) {
	fmt.Fprintf(out, "\nChange %s — %s %s %+.4g on %s\n", change.ID, change.Symbol, change.Action, change.DeltaQuantity, change.ExecutedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "  position  %.4g → %.4g", change.PositionBefore, change.PositionAfter)
	if change.ExecutionVWAP != nil {
		fmt.Fprintf(out, " · execution VWAP %.4g", *change.ExecutionVWAP)
	}
	if change.Multiplier != nil {
		fmt.Fprintf(out, " · multiplier %.4g", *change.Multiplier)
	}
	if change.DirectCostsBase != nil {
		fmt.Fprintf(out, " · direct costs %s", edgeMoney(*change.DirectCostsBase, currency))
	}
	fmt.Fprintln(out)
	for _, score := range change.Scores {
		value := "unavailable (" + score.Reason + ")"
		if score.DecisionImpactBase != nil {
			value = edgeMoney(*score.DecisionImpactBase, currency)
			if score.DecisionImpactPct != nil && score.DecisionNotionalBase != nil {
				value += fmt.Sprintf(" · %+.2f%% of %s", *score.DecisionImpactPct, edgeMoney(*score.DecisionNotionalBase, currency))
			}
			if score.HorizonDay != nil && score.HorizonClose != nil && score.HorizonFX != nil {
				value += fmt.Sprintf(" · %s close %.4g FX %.6g", score.HorizonDay.Format("2006-01-02"), *score.HorizonClose, *score.HorizonFX)
			}
		}
		fmt.Fprintf(out, "  %2d sessions  %s\n", score.Sessions, value)
		if len(score.MarketContext) > 0 {
			fmt.Fprintf(out, "               context %s\n", edgeFindingContext(score.MarketContext))
		}
	}
}

func edgeFindingContext(rows []rpc.EdgeMarketContext) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		value := fmt.Sprintf("%+.2f%%", row.ChangePct)
		if row.Kind == "volatility_index" && row.ChangePoints != nil {
			value = fmt.Sprintf("%+.2f pts", *row.ChangePoints)
		}
		parts = append(parts, row.Label+" "+value)
	}
	return strings.Join(parts, ", ")
}

func edgeMarketContextLabel(key string) string {
	switch key {
	case "spy":
		return "S&P 500 proxy (SPY)"
	case "qqq":
		return "Nasdaq-100 proxy (QQQ)"
	case "dia":
		return "Dow proxy (DIA)"
	case "vix":
		return "CBOE VIX"
	default:
		return strings.ToUpper(key)
	}
}

func edgeSignedPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%+.2f%%", *value)
}

func edgeSessionCount(sessions int) string {
	label := "sessions"
	if sessions == 1 {
		label = "session"
	}
	return fmt.Sprintf("%d %s", sessions, label)
}

func edgeRollupCell(value rpc.EdgeHorizonRollup, currency string) string {
	if value.TotalBase == nil {
		return fmt.Sprintf("— (n=%d)", value.SampleCount)
	}
	return fmt.Sprintf("%s n=%d", edgeMoney(*value.TotalBase, currency), value.SampleCount)
}

func edgeBaseCurrency(result rpc.EdgeResult) string {
	if result.Account != nil && result.Account.BaseCurrency != "" {
		return result.Account.BaseCurrency
	}
	return "BASE"
}

func edgeMoney(value float64, currency string) string {
	if currency == "" {
		currency = "BASE"
	}
	return fmt.Sprintf("%s %+.2f", strings.ToUpper(currency), value)
}
