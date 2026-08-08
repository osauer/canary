package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runOpportunities(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	subIdx := opportunitiesSubcommandIndex(args)
	if subIdx < 0 {
		if len(args) == 1 && helpArg(args[0]) {
			return printCommandUsage(env, "opportunities")
		}
		subIdx = 0
	}
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "status":
		return runOpportunitiesStatus(ctx, env, args)
	case "refresh":
		return runOpportunitiesRefresh(ctx, env, args)
	case "list":
		return runOpportunitiesList(ctx, env, args)
	case "preview":
		return runOpportunitiesPreview(ctx, env, args)
	case "exercise":
		return runOpportunitiesExercise(ctx, env, args)
	case "ignore":
		return runOpportunitiesIgnore(ctx, env, args)
	default:
		return fail(env, "opportunities: unknown subcommand %q", sub)
	}
}

func opportunitiesSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "status", "refresh", "list", "preview", "exercise", "ignore":
			return i
		}
	}
	return -1
}

func runOpportunitiesStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.OpportunityStatus
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesStatus, nil, &res); err != nil {
		return fail(env, "opportunities status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOpportunityStatusText(env, &res)
	return 0
}

func runOpportunitiesRefresh(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities refresh")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.OpportunitySnapshot
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesRefresh, rpc.OpportunityRefreshParams{Show: true}, &res); err != nil {
		return fail(env, "opportunities refresh: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOpportunitiesText(env, &res)
	return 0
}

func runOpportunitiesList(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities list")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.OpportunitySnapshot
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesSnapshot, rpc.OpportunitySnapshotParams{Show: true}, &res); err != nil {
		return fail(env, "opportunities list: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOpportunitiesText(env, &res)
	return 0
}

func runOpportunitiesPreview(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities preview")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to opportunity quantity")
	timeout := fs.Duration("timeout", 5*time.Second, "preview timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "opportunities preview: usage is `canary opportunities preview KEY REVISION`")
	}
	var res rpc.OpportunityExercisePreviewResult
	params := rpc.OpportunityExercisePreviewParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesPreviewExercise, params, &res); err != nil {
		return fail(env, "opportunities preview: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOpportunityPreviewText(env, &res)
	return 0
}

func runOpportunitiesExercise(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities exercise")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to opportunity quantity")
	token := fs.String("preview-token", "", "signed token returned by opportunities preview")
	timeout := fs.Duration("timeout", 5*time.Second, "preview/submit timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "opportunities exercise: usage is `canary opportunities exercise KEY REVISION --preview-token TOKEN`")
	}
	if strings.TrimSpace(*token) == "" {
		return fail(env, "opportunities exercise: --preview-token is required; run opportunities preview first")
	}
	var res rpc.OpportunityExerciseSubmitResult
	params := rpc.OpportunityExerciseSubmitParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, PreviewToken: strings.TrimSpace(*token), TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesSubmitExercise, params, &res); err != nil {
		return fail(env, "opportunities exercise: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOpportunitySubmitText(env, &res)
	return 0
}

func runOpportunitiesIgnore(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "opportunities ignore")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	reason := fs.String("reason", "", "ignore reason")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fail(env, "opportunities ignore: usage is `canary opportunities ignore KEY [REVISION]`")
	}
	params := rpc.OpportunityIgnoreParams{Key: fs.Arg(0), Reason: strings.TrimSpace(*reason)}
	if fs.NArg() == 2 {
		params.Revision = fs.Arg(1)
	}
	var res rpc.OpportunityIgnoreResult
	if err := env.Conn.Call(ctx, rpc.MethodOpportunitiesIgnore, params, &res); err != nil {
		return fail(env, "opportunities ignore: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintf(env.Stdout, "Ignored %s (%s)\n", res.Key, res.Message)
	return 0
}

func renderOpportunityStatusText(env *Env, st *rpc.OpportunityStatus) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Opportunities  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(nonEmpty(st.Policy.Status, "unknown")), Level: statusConcernNotice}))
	statusRow(env, out, "Enabled", fmt.Sprint(st.Enabled))
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d %s", st.Policy.PolicyID, st.Policy.PolicyVersion, st.Policy.Fingerprint.Key))
	statusRow(env, out, "Refresh", st.RefreshCadence)
	if len(st.Blockers) > 0 {
		fmt.Fprintln(out, "Blockers:")
		printTradingBlockers(out, "  ", st.Blockers)
	}
	fmt.Fprintln(out)
}

func renderOpportunitiesText(env *Env, snap *rpc.OpportunitySnapshot) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Opportunities  %d actionable / %d total\n", snap.Counts.Actionable, snap.Counts.Total)
	statusRow(env, out, "Revision", snap.Revision)
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d", snap.PolicyID, snap.PolicyVersion))
	if snap.Counts.ExpectedGainCurrency != "" {
		statusRow(env, out, "Expected gain", formatMoneyCcy(snap.Counts.ExpectedGain, snap.Counts.ExpectedGainCurrency))
	}
	printTradingBlockers(out, "  ", snap.Blockers)
	for _, opp := range snap.Opportunities {
		state := "ready"
		if len(opp.Blockers) > 0 {
			state = "blocked"
		}
		head := fmt.Sprintf("%s  %s  %s %d %s %s %.4g",
			opp.Key, opp.Bucket, opp.Action, opp.Quantity, opp.Symbol, opp.Contract.Right, opp.Contract.Strike)
		fmt.Fprintf(out, "  %s  gain=%s  effect=%s  [%s]\n", head, formatMoneyCcy(opp.ExpectedGain, opp.ExpectedGainCurrency), opp.PositionEffect, state)
		if risk := opportunityPostExerciseRiskSummary(opp); risk != "" {
			fmt.Fprintf(out, "      post-exercise risk: %s\n", risk)
		}
		for _, d := range opp.Details {
			fmt.Fprintf(out, "      %s\n", d)
		}
		printTradingBlockers(out, "      ", opp.Blockers)
	}
	fmt.Fprintln(out)
}

func renderOpportunityPreviewText(env *Env, res *rpc.OpportunityExercisePreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Opportunity Exercise Preview  accepted=%v token_minted=%v submit_eligible=%v\n", res.Accepted, res.TokenMinted, res.SubmitEligible)
	statusRow(env, out, "Opportunity", res.Opportunity.Key)
	if res.PreviewTokenID != "" {
		statusRow(env, out, "Token ID", res.PreviewTokenID)
	}
	if res.PreviewToken != "" {
		statusRow(env, out, "Token", res.PreviewToken)
	}
	statusRow(env, out, "Exercise", opportunityExerciseSummary(res.Opportunity))
	statusRow(env, out, "Expected gain", formatMoneyCcy(res.Opportunity.ExpectedGain, res.Opportunity.ExpectedGainCurrency))
	statusRow(env, out, "Position", fmt.Sprintf("%.4g -> %.4g (%s)", res.Opportunity.UnderlyingQuantityBefore, res.Opportunity.UnderlyingQuantityAfter, res.Opportunity.PositionEffect))
	if risk := opportunityPostExerciseRiskSummary(res.Opportunity); risk != "" {
		statusRow(env, out, "Post-exercise risk", risk)
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

func renderOpportunitySubmitText(env *Env, res *rpc.OpportunityExerciseSubmitResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Opportunity Exercise Submit  accepted=%v\n", res.Accepted)
	statusRow(env, out, "Opportunity", res.Opportunity.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	statusRow(env, out, "Order ref", res.OrderRef)
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	if risk := opportunityPostExerciseRiskSummary(res.Opportunity); risk != "" {
		statusRow(env, out, "Post-exercise risk", risk)
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

func opportunityExerciseSummary(opp rpc.Opportunity) string {
	return fmt.Sprintf("%s %d %s %s %.4g exp %s",
		opp.Action,
		opp.Quantity,
		opp.Symbol,
		opp.Contract.Right,
		opp.Contract.Strike,
		opp.Contract.Expiry,
	)
}

func opportunityPostExerciseRiskSummary(opp rpc.Opportunity) string {
	risk := opp.PostExerciseRisk
	before := opp.UnderlyingQuantityBefore
	after := opp.UnderlyingQuantityAfter
	change := opp.UnderlyingShareChange
	effect := opp.PositionEffect
	underlying := strings.TrimSpace(opp.UnderlyingContract.Symbol)
	if underlying == "" {
		underlying = strings.TrimSpace(opp.Symbol)
	}
	if risk != nil {
		before = risk.BeforeQuantity
		after = risk.AfterQuantity
		change = risk.ShareChange
		if risk.PositionEffect != "" {
			effect = risk.PositionEffect
		}
		if risk.Underlying != "" {
			underlying = risk.Underlying
		}
	}
	if underlying == "" && before == 0 && after == 0 && change == 0 && effect == "" {
		return ""
	}
	parts := []string{fmt.Sprintf("%s %.4g -> %.4g shares", nonEmpty(underlying, "underlying"), before, after)}
	if change != 0 {
		parts = append(parts, fmt.Sprintf("change %.4g", change))
	}
	if effect != "" {
		parts = append(parts, fmt.Sprintf("effect %s", effect))
	}
	if risk != nil {
		if risk.RiskOpened || risk.RiskIncreased || risk.RiskFlipped {
			parts = append(parts, "risk "+nonEmpty(risk.RiskChange, "increased"))
		} else if risk.RiskChange != "" {
			parts = append(parts, "risk "+risk.RiskChange)
		}
		if risk.ProtectionCoverageState != "" {
			parts = append(parts, "coverage "+risk.ProtectionCoverageState)
		}
		if risk.ProtectionReviewNeeded {
			reason := strings.TrimSpace(risk.ProtectionReviewReason)
			if reason != "" {
				parts = append(parts, "protection review needed: "+reason)
			} else {
				parts = append(parts, "protection review needed")
			}
		} else {
			parts = append(parts, "protection review not required")
		}
	}
	return strings.Join(parts, "; ")
}
