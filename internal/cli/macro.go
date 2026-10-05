package cli

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
)

func runMacro(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "macro")
	details := fs.Bool("details", false, "include all returned events, publications and source diagnostics")
	jsonOut := fs.Bool("json", false, "emit retained public calendar, publications and source coverage")
	start := fs.String("window-start", "", "inclusive source-local YYYY-MM-DD; supply with --window-end, at most 31 days")
	end := fs.String("window-end", "", "inclusive source-local YYYY-MM-DD; supply with --window-start")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	var out rpc.MacroSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodMacroSnapshot, rpc.MacroSnapshotParams{WindowStart: *start, WindowEnd: *end}, &out); err != nil {
		return fail(env, "macro: %v", err)
	}
	if *jsonOut {
		return printJSON(env, out)
	}
	renderMacro(env, out, *details)
	return 0
}

func renderMacro(env *Env, out rpc.MacroSnapshotResult, details bool) {
	style := env.yellow
	if out.CoverageStatus == "available" {
		style = env.green
	}
	displayLine(env, "Economic calendar · "+strings.ToUpper(nonEmpty(out.CoverageStatus, "unknown")), style)
	displayLine(env, out.WindowStart+" to "+out.WindowEnd, env.dim)
	displayLine(env, fmt.Sprintf("%d events · %d publications · %d sources", len(out.Events), len(out.Publications), len(out.Sources)), nil)
	// Source limitations come before headlines; absence never proves a quiet day.
	for _, source := range out.Sources {
		outsideWindow := source.WindowStart != "" && (out.WindowStart < source.WindowStart || out.WindowEnd > source.WindowEnd)
		limited := source.Stale || source.Availability != "available" || source.ConsecutiveFailures > 0 || source.Detail != "" || outsideWindow
		if !details && !limited {
			continue
		}
		state := source.Availability
		if source.Stale {
			state += " · stale"
		}
		tone := env.dim
		if limited {
			tone = env.yellow
		}
		displayLine(env, nonEmpty(source.Name, source.ID)+" · "+state, tone)
		if source.Detail != "" {
			displayLine(env, "  "+source.Detail, tone)
		}
		if source.ConsecutiveFailures > 0 {
			displayLine(env, fmt.Sprintf("  %d failed attempts · last success %s", source.ConsecutiveFailures, displayTime(source.LastSuccess)), tone)
		}
		if details || outsideWindow {
			displayLine(env, "  Coverage: "+source.Coverage+" · "+source.WindowStart+" to "+source.WindowEnd, env.dim)
		}
	}
	if out.Truncated || out.EventsTruncated || out.PublicationsTruncated {
		var capped []string
		if out.EventsTruncated {
			capped = append(capped, "events")
		}
		if out.PublicationsTruncated {
			capped = append(capped, "publications")
		}
		displayLine(env, "Source limit: "+nonEmpty(strings.Join(capped, " and "), "results")+" incomplete. Narrow the date window for events.", env.yellow)
	}
	fmt.Fprintln(env.Stdout)
	displayLine(env, "Scheduled events", env.bold)
	n := len(out.Events)
	if !details {
		n = min(n, 8)
	}
	for _, e := range out.Events[:n] {
		when := e.Date
		if !e.ScheduledAt.IsZero() {
			when = e.ScheduledAt.Format("2006-01-02 15:04 MST")
		} else if e.TimeLabel != "" {
			when += " · " + e.TimeLabel + " (source label)"
		} else {
			when += " · time unspecified"
		}
		displayLine(env, "  "+when+" · "+e.Title, nil)
	}
	if len(out.Events) == 0 {
		displayLine(env, "No events returned for this window.", env.dim)
	}
	if n < len(out.Events) {
		displayLine(env, fmt.Sprintf("+%d more events · canary macro --details", len(out.Events)-n), env.dim)
	}
	if len(out.Publications) > 0 {
		fmt.Fprintln(env.Stdout)
		displayLine(env, "Official publications", env.bold)
		n = len(out.Publications)
		if !details {
			n = min(n, 3)
		}
		for _, p := range out.Publications[:n] {
			displayLine(env, "  "+p.Title, nil)
			if details {
				displayLine(env, "  "+p.SourceURL, env.dim)
			}
		}
		if n < len(out.Publications) {
			displayLine(env, fmt.Sprintf("+%d more publications · canary macro --details", len(out.Publications)-n), env.dim)
		}
	}
	displayLine(env, "Official source coverage only; missing feeds do not establish an event-free day.", env.dim)
}
