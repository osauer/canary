package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runAlerts renders the daemon alert producers' redacted coverage view: which
// expected sources currently claim coverage, the rulebook's disclosed per-rule
// gaps, and the aggregate snapshot state the push-delivery baseline waits on.
// Read-only; the underlying RPCs carry no candidate, account, order, or
// delivery-target identity.
func runAlerts(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "alerts")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	var status rpc.AlertStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodAlertStatus, rpc.AlertStatusParams{}, &status); err != nil {
		return fail(env, "alerts: %v", err)
	}
	var snapshot rpc.AlertCandidateSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodAlertCandidates, rpc.AlertCandidatesParams{}, &snapshot); err != nil {
		return fail(env, "alerts: %v", err)
	}
	if *jsonOut {
		return printJSON(env, struct {
			CurrentState rpc.AlertSnapshotState  `json:"current_state"`
			Coverage     rpc.AlertCoverage       `json:"coverage"`
			Sources      []rpc.AlertSourceStatus `json:"sources"`
		}{snapshot.CurrentState, snapshot.Coverage, status.Sources})
	}

	coverage := snapshot.Coverage
	active := 0
	for _, candidate := range snapshot.Candidates {
		if candidate.State != rpc.AlertEpisodeRecovered {
			active++
		}
	}
	fmt.Fprintf(env.Stdout, "Alert coverage — %s  state %s (%d/%d)  freshness %s  active %d\n",
		coverage.AsOf.Local().Format("2006-01-02 15:04 MST"),
		coverage.State, len(coverage.CoveredSources), len(coverage.ExpectedSources), coverage.Freshness, active)
	covered := make(map[rpc.AlertSource]struct{}, len(coverage.CoveredSources))
	for _, source := range coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	missing := make([]string, 0, len(coverage.ExpectedSources))
	for _, source := range coverage.ExpectedSources {
		if _, ok := covered[source]; !ok {
			missing = append(missing, string(source))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(env.Stdout, "  missing: %s\n", strings.Join(missing, ", "))
	}
	fmt.Fprintln(env.Stdout)

	fmt.Fprintf(env.Stdout, "%-18s %-4s %-12s %-24s %6s  %s\n", "SOURCE", "COV", "STATUS", "REASON", "ACTIVE", "COVERED/EVALS")
	for _, source := range status.Sources {
		mark := "no"
		if source.Covered {
			mark = "yes"
		}
		m := source.Measurements
		fmt.Fprintf(env.Stdout, "%-18s %-4s %-12s %-24s %6d  %d/%d\n",
			source.Source, mark, source.Status, source.Reason, source.Active, m.CoveredEvaluations, m.Evaluations)
		if len(source.UncoveredRules) > 0 {
			fmt.Fprintf(env.Stdout, "%-18s      gaps: %s\n", "", strings.Join(source.UncoveredRules, ", "))
		}
	}
	return 0
}
