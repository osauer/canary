package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

func runOpportunityExercises(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "opportunities exercises", "Read pending exercise requests and matching IBKR evidence; sends no broker instruction", "canary opportunities exercises [--json]")
	jsonOut := fs.Bool("json", false, "emit pending requests, revisions, evidence IDs and blockers")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	var out rpc.ExercisePendingResult
	if err := rpc.Call(ctx, env.Conn, api.OpportunitiesExercises, rpc.NoParams{}, &out); err != nil {
		return fail(env, "opportunities exercises: %v", err)
	}
	if *jsonOut {
		return printJSON(env, out)
	}
	renderPendingExercises(env, out)
	return 0
}

func runOpportunityReconcileExercise(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "opportunities reconcile-exercise", "Confirm one fully resolved exercise using matching IBKR evidence; human terminal only", "canary opportunities reconcile-exercise REQUEST_REF REVISION --evidence EVIDENCE_ID --confirm-complete [--json]")
	evidence := fs.String("evidence", "", "unused matching evidence ID from opportunities exercises")
	confirmed := fs.Bool("confirm-complete", false, "attest that this exact request is fully resolved with no quantity still pending; matching broker evidence remains required")
	jsonOut := fs.Bool("json", false, "emit the recorded human-attested broker-evidence resolution")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || strings.TrimSpace(fs.Arg(0)) == "" || strings.TrimSpace(fs.Arg(1)) == "" || strings.TrimSpace(*evidence) == "" || !*confirmed {
		return fail(env, "opportunities reconcile-exercise requires REQUEST_REF REVISION --evidence EVIDENCE_ID --confirm-complete; read opportunities exercises first")
	}
	if env.Origin != rpc.OrderOriginHumanTTY {
		return fail(env, "opportunities reconcile-exercise requires an interactive human terminal; agents cannot attest that an exercise request is fully resolved")
	}
	p := rpc.ExerciseReconcileParams{RequestRef: strings.TrimSpace(fs.Arg(0)), Revision: strings.TrimSpace(fs.Arg(1)), EvidenceID: strings.TrimSpace(*evidence), ConfirmComplete: *confirmed, Origin: env.Origin}
	var out rpc.ExerciseReconcileResult
	if err := rpc.Call(ctx, env.Conn, api.OpportunitiesReconcileExercise, p, &out); err != nil {
		return fail(env, "opportunities reconcile-exercise: %v", err)
	}
	if *jsonOut {
		if code := printJSON(env, out); code != 0 {
			return code
		}
		if !out.Accepted {
			return 1
		}
		return 0
	}
	if !out.Accepted {
		return fail(env, "exercise reconciliation was not accepted; the request remains pending")
	}
	renderExerciseReconciliation(env, out)
	return 0
}

func renderPendingExercises(env *Env, out rpc.ExercisePendingResult) {
	if len(out.Requests) == 0 {
		fmt.Fprintln(env.Stdout, "No pending exercise requests.")
		return
	}
	fmt.Fprintf(env.Stdout, "Pending exercise requests · %d\n", len(out.Requests))
	for _, r := range out.Requests {
		symbol := r.Symbol
		if symbol == "" {
			symbol = "Option identity unavailable"
		}
		quantity := "quantity unavailable"
		if r.Quantity > 0 {
			quantity = fmt.Sprintf("%d contracts", r.Quantity)
		}
		fmt.Fprintf(env.Stdout, "\n%s · %s · %s\n", symbol, quantity, r.State)
		statusRow(env, env.Stdout, "Request", r.RequestRef)
		statusRow(env, env.Stdout, "Revision", r.Revision)
		option := "unavailable; request remains pending"
		if r.Option.ConID > 0 {
			option = fmt.Sprintf("%s %s %g %s · contract %d", r.Option.Symbol, r.Option.Expiry, r.Option.Strike, r.Option.Right, r.Option.ConID)
		}
		statusRow(env, env.Stdout, "Option", option)
		underlying := "unavailable; request remains pending"
		if r.Underlying.ConID > 0 {
			underlying = fmt.Sprintf("%s · contract %d", r.Underlying.Symbol, r.Underlying.ConID)
		}
		statusRow(env, env.Stdout, "Underlying", underlying)
		requested := "unavailable"
		if !r.SubmittedAt.IsZero() {
			requested = r.SubmittedAt.Format(time.RFC3339)
		}
		statusRow(env, env.Stdout, "Requested", requested)
		for _, e := range r.Evidence {
			statusRow(env, env.Stdout, "Evidence", e.ID+" · "+e.At.Format(time.RFC3339))
		}
		printTradingBlockers(env.Stdout, "  ", r.Blockers)
	}
	fmt.Fprintln(env.Stdout, "\nReservations remain until matching IBKR evidence and your confirmation resolve the exact request in full.")
}

func renderExerciseReconciliation(env *Env, out rpc.ExerciseReconcileResult) {
	fmt.Fprintln(env.Stdout, "Exercise reconciliation recorded; no broker instruction sent.")
	statusRow(env, env.Stdout, "Request", out.RequestRef)
	statusRow(env, env.Stdout, "Revision", out.Revision)
	statusRow(env, env.Stdout, "Evidence", out.EvidenceID)
	statusRow(env, env.Stdout, "Provenance", out.Provenance)
	if out.Replay {
		statusRow(env, env.Stdout, "Already recorded", "true; this retry made no new resolution")
	}
}
