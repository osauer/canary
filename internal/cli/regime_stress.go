package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/risk"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

func runRegime(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit the detailed typed regime snapshot")
	explain := fs.Bool("explain", false, "include served thresholds, confirmation reasons, and source health")
	profiles := fs.Bool("profiles", false, "include large gamma profile arrays in JSON")
	view := fs.String("view", "full", "full detail (default) or monitor dashboard projection; monitor requires --json")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	if *view != "full" && *view != "monitor" {
		return fail(env, "regime: --view must be full or monitor")
	}
	if *view == "monitor" && (!*jsonOut || *profiles || *explain) {
		return fail(env, "regime: --view monitor requires --json and cannot combine with --profiles or --explain")
	}
	if *profiles && !*jsonOut {
		return fail(env, "regime: --profiles requires --json")
	}
	var res rpc.RegimeSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
		return fail(env, "regime: %v", err)
	}
	if *jsonOut {
		if *view == "monitor" {
			return printJSON(env, rpc.CompactRegimeMonitor(&res))
		}
		if !*profiles {
			rpc.StripRegimeGammaProfiles(&res)
		}
		return printJSON(env, res)
	}
	renderRegime(env, res, *explain)
	return 0
}

func renderRegime(env *Env, res rpc.RegimeSnapshotResult, explain bool) {
	monitor := rpc.CompactRegimeMonitor(&res)
	current := res.AuthorityHealth != nil && res.AuthorityHealth.Status == rpc.RegimeAuthorityFresh
	verdict := res.Composite.Verdict
	if verdict == "" {
		verdict = "unavailable"
	} else if !current {
		verdict = "recorded verdict: " + verdict
	}
	tone := monitor.Posture.Tone
	if !current {
		tone = ""
	}
	riskDisplayLine(env, env.bold("Regime")+"  ", verdict, func(s string) string { return riskTone(env, tone, env.bold(s)) })
	if current && res.Summary.PunchLine != "" {
		riskDisplayLine(env, "", res.Summary.PunchLine, nil)
	}
	if h := res.AuthorityHealth; h != nil {
		riskReadLine(env, "Evidence", string(h.Status), strings.ReplaceAll(string(h.FailureCode), "_", " "))
		if h.LastSuccessAt != nil {
			if explain || !current {
				riskReadLine(env, "Last successful refresh", h.LastSuccessAt.Local().Format("2 Jan 15:04 MST"))
			}
		}
	}
	riskReadLine(env, "Readiness", res.Lifecycle.Readiness, string(res.Lifecycle.Severity))
	if explain {
		riskReadLine(env, "Snapshot generated", res.AsOf.Local().Format("2 Jan 15:04 MST"), res.Lifecycle.Stage)
		riskReadLine(env, "Clusters", res.Summary.Evidence)
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Indicators"))
	if current {
		riskDisplayLine(env, "  ", "Green constructive · Amber mixed · Red stressed · — unrated", env.dim)
	}
	values := regimeDisplayValues(res)
	for _, row := range monitor.Indicators {
		renderRegimeIndicator(env, row, values[row.Name], current, explain)
		if explain && (!current || row.Status != rpc.RegimeStatusOK) && row.Band != "" {
			riskReadLine(env, "    Recorded band", row.Band, "not a current rating")
		}
		if row.Eligibility != nil && !row.Eligibility.Eligible {
			riskReadLine(env, "    Confirmation", "not eligible", strings.Join(row.Eligibility.Reasons, "; "))
		}
		if explain {
			if row.AsOf != nil {
				observed := row.AsOf.Date
				if observed == "" && !row.AsOf.Time.IsZero() {
					observed = row.AsOf.Time.Local().Format("2 Jan 15:04 MST")
				}
				if observed == "" {
					observed = row.AsOf.Label
				}
				riskReadLine(env, "    Observed", observed, row.AsOf.Source)
			}
			if th := row.Thresholds; th != nil {
				riskReadLine(env, "    Thresholds", "green: "+th.Green, "yellow: "+th.Yellow, "red: "+th.Red)
			}
		}
	}
	if !explain {
		if len(res.WarningDetails) > 0 {
			riskReadLine(env, "\nSource issues", fmt.Sprintf("%d reported; see --explain", len(res.WarningDetails)))
		}
		fmt.Fprintln(env.Stdout, "\nDetails: canary regime --explain")
	}
	if explain {
		fmt.Fprintln(env.Stdout, "\nSource diagnostics")
		for _, warning := range res.WarningDetails {
			riskReadLine(env, "  "+warning.Code, warning.Message)
		}
		for _, insight := range monitor.GammaInsights {
			if insight != nil {
				riskReadLine(env, "  Gamma", insight.Interpretation, insight.HorizonInterpretation, insight.SkewInterpretation, insight.Provenance)
			}
		}
		var pending []string
		for _, row := range monitor.Indicators {
			if row.Thresholds != nil && row.Thresholds.PendingBacktest {
				pending = append(pending, row.Name)
			}
		}
		if len(pending) > 0 {
			riskReadLine(env, "Pending backtest", strings.Join(pending, ", "))
		}
	}
	if explain {
		for _, governor := range res.Lifecycle.Governors {
			riskReadLine(env, "Governor", governor.Action, governor.From, governor.To, governor.Reason)
		}
		for _, source := range res.SourceHealth {
			riskReadLine(env, "Source", source.Source, source.Status, source.AsOf.Format("2006-01-02 15:04 MST"), strings.Join(source.Notes, "; "))
		}
	}
}

func runStress(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "stress")
	jsonOut := fs.Bool("json", false, "emit the full typed portfolio-stress assessment")
	details := fs.Bool("details", false, "include market indicator and source-health details")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	res, err := stress.FetchStress(ctx, env.Conn)
	if err != nil {
		if *jsonOut {
			return fail(env, "stress: %v", err)
		}
		diagnostic := &Env{Stdout: env.Stderr}
		riskReadLine(diagnostic, "Portfolio stress", "unavailable")
		var rpcErr *rpc.Error
		if errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeGatewayUnavailable {
			riskReadLine(diagnostic, "", "Gateway unavailable; required inputs could not be read.")
		} else {
			riskReadLine(diagnostic, "", "A required input could not be read.")
		}
		riskReadLine(diagnostic, "Next", "canary status · canary stress --details")
		if *details {
			riskReadLine(diagnostic, "Diagnostic", err.Error())
		}
		return 1
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderStress(env, res, *details)
	return 0
}

func renderStress(env *Env, res rpc.StressResult, details bool) {
	action := strings.ToUpper(strings.ReplaceAll(nonEmpty(res.Action, "unavailable"), "_", " "))
	riskDisplayLine(env, env.bold("Portfolio stress")+"  ", action, func(s string) string { return riskTone(env, string(res.Severity), env.bold(s)) })
	riskDisplayLine(env, "", res.Summary, nil)
	riskReadLine(env, "Inputs", res.InputHealth, "market confirmation: "+res.MarketConfirmation, "portfolio relevance: "+res.PortfolioFit)
	rows := slices.Clone(res.Rows)
	// Rank only the display order. A severe finding does not override the
	// producer's overall assessment or its confirmation/coverage gates.
	rank := func(s risk.SignalSeverity) int {
		switch s {
		case risk.SeverityUrgent:
			return 3
		case risk.SeverityAct:
			return 2
		case risk.SeverityWatch:
			return 1
		default:
			return 0
		}
	}
	slices.SortStableFunc(rows, func(a, b rpc.StressRow) int { return rank(b.Severity) - rank(a.Severity) })
	for _, row := range rows {
		if row.Title != "Portfolio stress" && rank(row.Severity) > rank(res.Severity) {
			riskReadLine(env, "Finding levels", "Individual findings exceed the overall rating; see below.")
			break
		}
	}
	for _, group := range []struct {
		title   string
		quality bool
	}{{"Findings", false}, {"Coverage gaps", true}} {
		printed := false
		for _, row := range rows {
			if row.Title == "Portfolio stress" {
				continue
			}
			if (row.Direction == risk.DirectionDataQuality) != group.quality {
				continue
			}
			if !details && row.Severity == risk.SeverityObserve {
				continue
			}
			if !printed {
				fmt.Fprintln(env.Stdout, "\n"+env.bold(group.title))
				printed = true
			}
			badge := strings.ToUpper(nonEmpty(sanitizeRunText(string(row.Severity)), "unknown"))
			prefix := "  " + riskTone(env, string(row.Severity), fmt.Sprintf("%-7s", badge)) + " "
			riskDisplayLine(env, prefix, row.Title, env.bold)
			riskDisplayLine(env, "    ", row.Evidence, nil)
			if details || !group.quality {
				riskDisplayLine(env, "    ", row.Guidance, env.dim)
			}
		}
	}
	if details {
		riskReadLine(env, "\nSnapshot generated", res.AsOf.Local().Format("2 Jan 15:04 MST"))
		if len(res.Rows) > 0 && res.Rows[0].Title == "Portfolio stress" {
			riskReadLine(env, "Overall evidence", res.Rows[0].Evidence)
		}
		for _, warning := range res.Warnings {
			riskReadLine(env, "Warning", warning)
		}
		for _, row := range res.MarketIndicators {
			riskReadLine(env, row.Name, row.Status, row.Reading, row.AsOf, row.Comment)
		}
		for _, source := range res.SourceHealth {
			riskReadLine(env, "Source", source.Source, source.Status, source.AsOf.Format("2006-01-02 15:04 MST"), strings.Join(source.Notes, "; "))
		}
	} else {
		fmt.Fprintln(env.Stdout)
		riskReadLine(env, "Details", fmt.Sprintf("canary stress --details · %d source notes", len(res.Warnings)))
	}
	riskReadLine(env, "", res.NotExecution)
}

func riskReadLine(env *Env, label string, values ...string) {
	indent := label[:len(label)-len(strings.TrimLeft(label, " "))]
	text := sanitizeRunText(briefJoin(append([]string{label}, values...)...))
	width := briefProseWidth(env.Stdout)
	for i, line := range wrapVisibleText(text, width-len(indent)-2) {
		if i == 0 {
			fmt.Fprintln(env.Stdout, indent+line)
		} else {
			fmt.Fprintln(env.Stdout, indent+"  "+line)
		}
	}
}
