package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func displayTime(t time.Time) string {
	if t.IsZero() {
		return "unavailable"
	}
	return t.Format("2006-01-02 15:04 MST")
}

func renderSetupCoverage(env *Env, r rpc.SetupCoverageResult) {
	displayLine(env, "Setup coverage · "+nonEmpty(r.SessionDate, "no retained session"), env.bold)
	displayRow(env, env.Stdout, "As of", displayTime(r.AsOf))
	displayLine(env, fmt.Sprintf("%d contracts · %d completed 5-minute slots", len(r.Contracts), r.SlotsCompleted), nil)
	if len(r.Contracts) == 0 {
		displayLine(env, "No evaluations retained for this selection.", env.dim)
	}
	for _, row := range r.Contracts {
		displayLine(env, fmt.Sprintf("%s · %d/%d scheduled slots covered", row.Symbol, row.SlotsCovered, row.SlotsScheduled), env.bold)
		displayLine(env, fmt.Sprintf("  %d evaluations · %d history requests · last %s", row.Evaluations, row.HistoryRequests, displayTime(row.LastEvaluatedAt)), nil)
		keys := make([]string, 0, len(row.States))
		for key := range row.States {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			style := env.dim
			if strings.HasPrefix(key, "unavailable") {
				style = env.yellow
			}
			displayLine(env, fmt.Sprintf("  %s: %d", strings.ReplaceAll(key, "_", " "), row.States[key]), style)
		}
	}
	if r.Truncated {
		displayLine(env, "Coverage truncated: more contracts were not recorded.", env.yellow)
	}
	displayLine(env, "Evaluation coverage only; not a trading signal.", env.dim)
}

func renderSetupOptions(env *Env, r rpc.SetupOptionsResult) {
	displayLine(env, "Setup options · "+r.Underlying.Symbol+" · "+nonEmpty(r.Expiry, "expiry discovery"), env.bold)
	displayRow(env, env.Stdout, "As of", displayTime(r.AsOf))
	if len(r.Expiries) > 0 {
		dates := make([]string, 0, len(r.Expiries))
		for _, e := range r.Expiries {
			dates = append(dates, e.Date)
		}
		displayRow(env, env.Stdout, "Expiries", strings.Join(dates, " · "))
	}
	for _, call := range r.Calls {
		displayLine(env, fmt.Sprintf("Call %.2f · %s", call.Strike, strings.ReplaceAll(call.Status, "_", " ")), nil)
	}
	if r.Contract != nil {
		displayRow(env, env.Stdout, "Selected", fmt.Sprintf("%s %s %s %.2f · contract %d", r.Contract.Symbol, r.Contract.Expiry, r.Contract.Right, r.Contract.Strike, r.Contract.ConID))
	}
	if q := r.Quote; q != nil {
		style := env.dim
		if q.Status == rpc.SetupQuoteMissing {
			style = env.yellow
		}
		displayLine(env, "Quote · "+q.Status+" · "+nonEmpty(q.DataType, "delivery unavailable"), style)
		displayRow(env, env.Stdout, "Bid / ask", displayNumber(q.Bid, 1, "%.2f")+" / "+displayNumber(q.Ask, 1, "%.2f")+" "+nonEmpty(r.Underlying.Currency, "currency unavailable"))
		displayRow(env, env.Stdout, "Quote as of", displayTime(q.AsOf))
	}
	if len(r.Expiries) == 0 && len(r.Calls) == 0 && r.Contract == nil {
		displayLine(env, "No listed options returned.", env.dim)
	}
	if r.Truncated {
		displayLine(env, "Discovery truncated; narrow the selection.", env.yellow)
	}
	displayLine(env, "Indicative discovery only; exact order preview and eligibility are separate. — = unavailable.", env.dim)
}

func renderSetupMarkouts(env *Env, r rpc.SetupMarkoutsResult) {
	displayLine(env, "Entry markouts · quote diagnostics", env.bold)
	displayLine(env, "Not realized profit; markouts exclude commission.", env.dim)
	displayRow(env, env.Stdout, "As of", displayTime(r.AsOf))
	displayRow(env, env.Stdout, "Tracking since", displayTime(r.Clock.TrackingSince))
	displayLine(env, fmt.Sprintf("%d targets · %d pending captures", len(r.Targets), r.Clock.Pending), nil)
	if r.Note != "" {
		displayLine(env, r.Note, env.dim)
	}
	for _, row := range r.Targets {
		style := env.dim
		if row.Status == rpc.SetupMarkoutMissing {
			style = env.yellow
		}
		displayLine(env, fmt.Sprintf("%s · %s · %s · %s", row.Contract.Symbol, row.Horizon, row.Status, row.Mode), style)
		displayRow(env, env.Stdout, "Contract", fmt.Sprintf("%s %s %s %.2f · ID %d", row.Contract.SecType, row.Contract.Expiry, row.Contract.Right, row.Contract.Strike, row.Contract.ConID))
		displayRow(env, env.Stdout, "Fill", fmt.Sprintf("%s %g × %.2f %s · %s", row.Side, row.FillQuantity, row.FillPrice, row.Currency, displayTime(row.FillAt)))
		displayRow(env, env.Stdout, "Target", displayTime(row.TargetAt))
		displayRow(env, env.Stdout, "Markout", displayNumber(row.Markout, 1, "%+.2f")+" "+row.Currency+" · "+row.MarkSide+" "+displayNumber(row.MarkPrice, 1, "%.2f"))
		displayRow(env, env.Stdout, "Quote", nonEmpty(row.DataType, "unavailable")+" · as of "+displayTime(row.QuoteAsOf))
		if row.Reason != "" {
			displayLine(env, "  Reason: "+row.Reason, env.yellow)
		}
	}
	if len(r.Targets) == 0 {
		displayLine(env, "No entry markouts retained for this selection.", env.dim)
	}
	displayLine(env, "Full capture evidence and execution references: canary setups markouts --json", env.dim)
}
