package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// edgeBriefColumn is the label column of the Edge summary blocks.
const edgeBriefColumn = 16

// edgeBriefTitle names the review in the reader's words: what was reviewed,
// over how long, up to which day.
func edgeBriefTitle(env *Env, result rpc.EdgeResult) string {
	period := result.Window + " of decisions"
	switch result.Window {
	case "365d":
		period = "one year of decisions"
	case "90d":
		period = "90 days of decisions"
	}
	parts := []string{"Canary Edge", period}
	if !result.AsOf.IsZero() {
		parts = append(parts, "to "+result.AsOf.Format(time.DateOnly))
	}
	if result.State != rpc.EdgeStateCurrent {
		parts = append(parts, env.yellow(string(result.State)))
	}
	return strings.Join(parts, " · ")
}

// edgeBriefRow prints one labelled row of a summary block, values wrapped
// beneath their column. Money keeps its prefix and amount together.
func edgeBriefRow(env *Env, label, value string) {
	glued := edgeMoneyGlue.ReplaceAllString(value, "$1"+edgeGlue+"$2")
	glued = strings.ReplaceAll(glued, " · ", edgeGlue+"· ")
	prefix := "  " + env.dim(padRightVisible(label, edgeBriefColumn-1)) + " "
	for i, line := range wrapVisibleText(glued, briefProseWidth(env.Stdout)-2-edgeBriefColumn) {
		if i > 0 {
			prefix = strings.Repeat(" ", 2+edgeBriefColumn)
		}
		fmt.Fprintln(env.Stdout, prefix+strings.ReplaceAll(line, edgeGlue, " "))
	}
}

// renderEdgeBottomLine is the conclusion, first on both views: what the
// account made, how the graded decisions split, and the one pattern the
// review singles out. Every figure here is repeated with its evidence in the
// detailed view.
func renderEdgeBottomLine(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	fmt.Fprintf(out, "\n%s\n", env.bold("Bottom line"))
	if account := result.Account; account != nil {
		edgeBriefRow(env, "Account", fmt.Sprintf("%s from %s to %s, after deposits and withdrawals", env.bold(edgeSignedMoney(env, account.ProfitLossBase, account.BaseCurrency)), account.ActualFrom.Format(time.DateOnly), account.ActualTo.Format(time.DateOnly)))
	}
	names := map[string]string{"STK": "Stocks", "ETF": "ETFs", "CASH": "Currency"}
	for _, row := range result.Verdicts.Counts {
		if row.Helped+row.Hurt+row.NoDifference == 0 {
			continue
		}
		name := names[row.AssetClass]
		if name == "" {
			name = row.AssetClass
		}
		value := fmt.Sprintf("%d %s helped, %d hurt", row.Helped, pluralWord(row.Helped, "decision", "decisions"), row.Hurt)
		if row.NoDifference > 0 {
			value += fmt.Sprintf(", %d made no difference", row.NoDifference)
		}
		edgeBriefRow(env, name, value+" · judged 5 sessions later, after costs")
	}
	if cycles := result.Options.Cycles; cycles.CompletedCount > 0 {
		value := fmt.Sprintf("%d of %d closed %s made money, %d lost money", cycles.MadeMoney, cycles.CompletedCount, pluralWord(cycles.CompletedCount, "position", "positions"), cycles.LostMoney)
		if rest := cycles.Flat + cycles.Ungraded; rest > 0 {
			value += fmt.Sprintf(", %d flat or not graded", rest)
		}
		edgeBriefRow(env, "Options", value)
	}
	if pattern, lens := edgeSelectedPattern(result); pattern != nil && lens.TotalBase != nil && lens.MedianBase != nil && pattern.Direction != "" {
		direction := strings.ToUpper(pattern.Direction[:1]) + pattern.Direction[1:]
		edgeBriefRow(env, "Finding", fmt.Sprintf("%s %s: %s over the %s after them, across %d of %d; median %s each", direction, edgeActionPlural(pattern.Action), edgeSignedMoney(env, *lens.TotalBase, ccy), edgeSessionCount(lens.Sessions), lens.SampleCount, pattern.EligibleChanges, edgeSignedMoney(env, *lens.MedianBase, ccy)))
	} else if headline := sanitizeRunText(result.Headline); headline != "" {
		edgeBriefRow(env, "Finding", headline)
	}
}

// renderEdgeBrief is the default Edge view: the conclusion, the two tables a
// trader acts on, and how far the figures can be trusted. The full review
// is one flag away and loses nothing.
func renderEdgeBrief(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	renderEdgeBottomLine(env, result)

	if decisions := result.Options.Decisions; decisions.WithOutcome > 0 {
		fmt.Fprintf(out, "\n%s  %s\n", env.bold("Option trades"), env.dim("each kind against its own position, before costs"))
		labels := map[string]string{"open": "Opens", "add": "Adds", "trim": "Partial sales", "exit": "Final sales"}
		for _, roll := range decisions.Actions {
			if roll.WithResult == 0 {
				continue
			}
			value := fmt.Sprintf("%d of %d sold below the entry price", roll.Losses, roll.WithResult)
			if roll.Action == "open" || roll.Action == "add" {
				value = fmt.Sprintf("%d of %d ended below the price paid", roll.Losses, roll.WithResult)
			}
			if roll.TotalResultBase != nil {
				value += " · all together " + edgeSignedMoney(env, *roll.TotalResultBase, ccy)
			}
			edgeBriefRow(env, labels[roll.Action], value)
		}
	}

	if len(result.Findings) > 0 {
		fmt.Fprintf(out, "\n%s  %s\n", env.bold("Biggest single decisions"), env.dim("price move over the "+edgeSessionCount(result.HorizonSessions)+" after"))
		for _, finding := range result.Findings {
			fmt.Fprintf(out, "  %s  %s  %s %s\n", padLeftVisible(edgeSignedMoney(env, finding.DecisionImpactBase, ccy), edgeMoneyWidth), padLeftVisible(fmt.Sprintf("%+.2f%%", finding.DecisionImpactPct), 8), edgeCompactSymbol(sanitizeRunText(finding.Symbol)), finding.Action)
		}
	}

	fmt.Fprintf(out, "\n%s\n", env.bold("How far to trust this"))
	selection := result.HorizonSelection
	if selection.EligibleChanges > 0 {
		edgeBriefRow(env, "Scored", fmt.Sprintf("%s of %s decisions have %s of prices behind them (%.1f%%)", edgeCount(selection.ScoredChanges), edgeCount(selection.EligibleChanges), edgeSessionCount(result.HorizonSessions), selection.CoveragePct))
	}
	edgeBriefRow(env, "Reading", "price outcomes from broker records; they do not grade skill or risk management")
	switch {
	case result.ForwardTestsUnavailable == "registry_unreadable":
		edgeBriefRow(env, "Forward tests", "could not be read this time; ask again")
	case len(result.ForwardTests) > 0:
		value := fmt.Sprintf("%d registered", len(result.ForwardTests))
		if counted := edgeForwardCounted(result.ForwardTests); counted != "" {
			value += " · " + counted
		}
		edgeBriefRow(env, "Forward tests", value+" · canary edge tests")
	}
	fmt.Fprintln(out, "\nFull review: canary edge --details")
}
