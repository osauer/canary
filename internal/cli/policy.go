package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// runPolicy renders and operates the risk constitution
// (internal-docs/design/risk-policy.md). show is read-only; the write verbs are
// governance acts (capital events, one-shot overrides, drawdown reset,
// artefact completions) — the daemon accepts them from human origins only,
// so agent sessions can read this surface but never operate it. No verb
// here touches broker writes, freeze, or trading limits.
func runPolicy(ctx context.Context, env *Env, args []string) int {
	sub := "show"
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "show":
		return runPolicyShow(ctx, env, args)
	case "default":
		return runPolicyDefault(ctx, env, args)
	case "capital-event":
		return runPolicyCapitalEvent(ctx, env, args)
	case "override":
		return runPolicyOverride(ctx, env, args)
	case "reset-drawdown":
		return runPolicyResetDrawdown(ctx, env, args)
	case "correct-peak":
		return runPolicyCorrectPeak(ctx, env, args)
	case "artefact":
		return runPolicyArtefact(ctx, env, args)
	default:
		return fail(env, "policy: unknown subcommand %q (try `canary policy show --explain`)", sub)
	}
}

// firstPositionalIndex finds the first non-flag token, skipping the values
// of catalog-known value flags. Needed because Run() hoists flags before
// positionals, so a subcommand can arrive after its own flags
// (settingsSubcommandIndex precedent).
func firstPositionalIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if isValueFlag(arg) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		return i
	}
	return -1
}

func runPolicyShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	explain := fs.Bool("explain", false, "show every limit with its plain-English meaning, source, and enforcement class")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.RiskPolicyResult
	if err := env.Conn.Call(ctx, rpc.MethodRiskPolicySnapshot, struct{}{}, &res); err != nil {
		return fail(env, "policy: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	fmt.Fprintf(env.Stdout, "Risk constitution — %s  status %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.Status)
	if res.PolicyID != "" {
		fp := ""
		if res.PolicyFingerprint != nil {
			fp = "  " + shortFingerprint(res.PolicyFingerprint.Key)
		}
		fmt.Fprintf(env.Stdout, "  policy %s v%d%s  (%s)\n", res.PolicyID, res.PolicyVersion, fp, res.Path)
	} else {
		fmt.Fprintf(env.Stdout, "  no policy file at %s\n", res.Path)
	}
	if res.Message != "" {
		fmt.Fprintf(env.Stdout, "  note: %s\n", res.Message)
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-11s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}

	c := res.Capital
	fmt.Fprintf(env.Stdout, "\nCapital: %s\n", capitalHeadline(c, res.Limits))
	cur := c.BaseCurrency
	if cur == "" {
		cur = "base"
	}
	if c.EquityBase != nil {
		fmt.Fprintf(env.Stdout, "  account equity      %14.2f %s  last seen %s%s\n", *c.EquityBase, cur, c.EquityAsOf.Local().Format("2006-01-02 15:04"), staleTag(c.EquityStale))
	}
	if c.EffectiveRiskCapitalBase != nil {
		fmt.Fprintf(env.Stdout, "  money at risk (max) %14.2f %s  the lower of your declared risk capital and what sits above the protected floor\n", *c.EffectiveRiskCapitalBase, cur)
	}
	if c.AdjustedPeakBase != nil {
		fmt.Fprintf(env.Stdout, "  high-water mark     %14.2f %s  best equity so far, corrected for deposits and withdrawals\n", *c.AdjustedPeakBase, cur)
	}
	if c.StatementCumFlowsBase != nil && c.DeclaredCumFlowsBase != nil {
		fmt.Fprintln(env.Stdout, capitalFlowComparison(c, cur))
	}
	if c.ConsumedPct != nil {
		fmt.Fprintf(env.Stdout, "  loss from the mark  %14.2f %s  = %.1f%% of your declared risk capital%s\n", deref(c.DrawdownBase), cur, *c.ConsumedPct, drawdownLadderHint(res.Limits))
	}
	if c.BlockLatched {
		fmt.Fprintf(env.Stdout, "  RISK BRAKE ENGAGED since %s — it stays on until you release it: `canary policy reset-drawdown --reason \"...\"`\n", c.LatchedAt.Local().Format("2006-01-02 15:04"))
	}
	if c.LastReconciledAt.IsZero() {
		fmt.Fprintln(env.Stdout, ledgerNeverVerifiedMessage(c))
	} else {
		evidence := reconcileEvidenceDetail(c)
		fmt.Fprintf(env.Stdout, "  ledger check        verified against broker statements %s%s%s\n", c.LastReconciledAt.Local().Format("2006-01-02 15:04"), evidence, staleTag(c.ReconcileStale))
	}
	for _, r := range c.Reasons {
		fmt.Fprintf(env.Stdout, "  (%s)\n", r)
	}

	if len(res.Unapproved) > 0 {
		fmt.Fprintf(env.Stdout, "\nWaiting on your decisions — these keys are absent from the policy file, so the controls that need them stay off:\n")
		for _, k := range res.Unapproved {
			fmt.Fprintf(env.Stdout, "  • %s\n", k)
		}
	}

	if *explain {
		activeOverride := map[string]rpc.OverrideRecord{}
		for _, o := range res.Overrides {
			if o.Active {
				activeOverride[o.Control] = o
			}
		}
		fmt.Fprintln(env.Stdout, "\nEffective limits:")
		for _, l := range res.Limits {
			mark := ""
			if o, ok := activeOverride[l.Key]; ok {
				mark = fmt.Sprintf("  [override until %s: %s]", o.ExpiresAt.Local().Format("15:04"), o.Reason)
			}
			fmt.Fprintf(env.Stdout, "  %-34s %-14s %-10s %-9s%s\n", l.Key, l.Value, l.Source, l.Enforcement, mark)
			fmt.Fprintf(env.Stdout, "      %s\n", l.Meaning)
		}
	}

	if len(res.Overrides) > 0 {
		fmt.Fprintln(env.Stdout, "\nTemporary exceptions you granted:")
		for _, o := range res.Overrides {
			state := "expired"
			if o.Active {
				state = "active until " + o.ExpiresAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(env.Stdout, "  %s  %s (%s) — %s\n", o.ID, o.Control, state, o.Reason)
		}
	}
	if len(res.Cadence) > 0 {
		fmt.Fprintln(env.Stdout, "\nRoutine reviews (recorded, never blocking):")
		for _, a := range res.Cadence {
			fmt.Fprintf(env.Stdout, "  %-8s last completed %s %s\n", a.Artefact, a.CompletedAt.Local().Format("2006-01-02 15:04"), a.Note)
		}
	}
	if len(res.Inventory) > 0 {
		fmt.Fprintln(env.Stdout, "\nOther policies on this system, compared with the versions you approved this constitution against:")
		for _, p := range res.Inventory {
			switch p.Status {
			case "match":
				fmt.Fprintf(env.Stdout, "  %-10s unchanged since approval (%s)\n", p.Policy, policyIdentity(p.LiveID, p.LiveVersion))
			case "drift":
				fmt.Fprintf(env.Stdout, "  %-10s CHANGED since approval: was %s, now %s — review it, then update [inventory] in the policy file\n", p.Policy, policyIdentity(p.PinnedID, p.PinnedVersion), policyIdentity(p.LiveID, p.LiveVersion))
			case "unpinned":
				fmt.Fprintf(env.Stdout, "  %-10s not recorded at approval time (currently %s) — add it under [inventory] to be told when it changes\n", p.Policy, policyIdentity(p.LiveID, p.LiveVersion))
			default:
				fmt.Fprintf(env.Stdout, "  %-10s %s\n", p.Policy, p.Status)
			}
		}
	}
	return 0
}

func capitalFlowComparison(c rpc.CapitalStateReport, currency string) string {
	if c.StatementCumFlowsBase == nil || c.DeclaredCumFlowsBase == nil {
		return ""
	}
	return fmt.Sprintf("  cumulative flows    declared %14.2f %s  statement-authoritative %14.2f %s  (using %s)",
		*c.DeclaredCumFlowsBase, currency, *c.StatementCumFlowsBase, currency, c.FlowSource)
}

func reconcileEvidenceDetail(c rpc.CapitalStateReport) string {
	if c.StatementCumFlowsBase == nil || (c.LastReconcileReportID == "" && c.LastReconcileSource == "") {
		return ""
	}
	source := "human sign-off"
	if c.LastReconcileSource == rpc.ReconcileSourceAutomatic {
		source = "automatic"
	}
	return fmt.Sprintf(" (report %s, %s)", nonEmpty(c.LastReconcileReportID, "legacy"), source)
}

func ledgerNeverVerifiedMessage(c rpc.CapitalStateReport) string {
	if c.StatementCumFlowsBase != nil {
		return "  ledger check        never verified against broker statements — run `canary recon`; a qualifying clean report extends automatically, otherwise use human sign-off"
	}
	return "  ledger check        never verified against broker statements — run `canary recon`, then sign off the report it prints"
}

// capitalHeadline renders the tier as a sentence a human can act on, with
// the ladder thresholds pulled from the explain rows so the numbers always
// come from the same source every other surface uses.
func capitalHeadline(c rpc.CapitalStateReport, limits []risk.ConstitutionLimit) string {
	switch c.Tier {
	case risk.CapitalTierOK:
		return "OK — losses from the high-water mark are within your limits"
	case risk.CapitalTierWarn:
		return "WARNING — losses have crossed your early-warning line" + drawdownLadderHint(limits)
	case risk.CapitalTierBlock:
		if c.Enforcement == risk.EnforcementShadow {
			return "BLOCK LINE CROSSED — recorded only for now (shadow mode): nothing is stopped yet"
		}
		return "BLOCK LINE CROSSED — risk-increasing orders are flagged; reducing and closing stay available"
	case risk.CapitalTierUnapproved:
		return "NOT ARMED — the policy file is missing decisions (listed below)"
	default:
		return "UNKNOWN — a required input is missing or stale (details below); this never counts as OK"
	}
}

// policyIdentity joins a policy id with its version without mangling
// string versions ("rulebook-v2 v2" but "active-v1 risk-policy-v1").
func policyIdentity(id, version string) string {
	if version == "" {
		return id
	}
	for _, r := range version {
		if r < '0' || r > '9' {
			return id + " " + version
		}
	}
	return id + " v" + version
}

// drawdownLadderHint appends the warn/block thresholds when both exist.
func drawdownLadderHint(limits []risk.ConstitutionLimit) string {
	var warn, block string
	for _, l := range limits {
		switch l.Key {
		case "drawdown.warn_consumed_pct":
			warn = l.Value
		case "drawdown.block_consumed_pct":
			block = l.Value
		}
	}
	if warn == "" || warn == "unapproved" || block == "" || block == "unapproved" {
		return ""
	}
	return fmt.Sprintf("  (warn at %s, block at %s)", warn, block)
}

func runPolicyCapitalEvent(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy capital-event")
	amount := fs.Float64("amount", 0, "amount in the policy base currency (deposit/withdrawal)")
	effectiveAt := fs.String("effective-at", "", "when the flow hit the account (YYYY-MM-DD or RFC3339; default now)")
	note := fs.String("note", "", "free-text note for the journal")
	report := fs.String("report", "", "recon report id being signed off (default: current report)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy capital-event: exactly one of deposit|withdrawal|reconcile is required")
	}
	params := rpc.CapitalEventParams{Type: fs.Arg(0), AmountBase: *amount, Note: *note, Report: *report, Origin: env.Origin}
	if *effectiveAt != "" {
		t, err := parseFlexibleTime(*effectiveAt)
		if err != nil {
			return fail(env, "policy capital-event: %v", err)
		}
		params.EffectiveAt = t
	}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyCapitalEvent, params, *jsonOut)
}

func runPolicyOverride(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy override")
	control := fs.String("control", "", "constitution key being excepted (see `canary policy show --explain`)")
	reason := fs.String("reason", "", "why this exception is justified (journaled verbatim)")
	hours := fs.Int("hours", 0, "override lifetime; capped by override.max_duration_hours")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.OverrideParams{Control: *control, Reason: *reason, Hours: *hours, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyOverride, params, *jsonOut)
}

func runPolicyResetDrawdown(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy reset-drawdown")
	reason := fs.String("reason", "", "why risk resumes (journaled verbatim; the reset re-bases the peak)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.ResetDrawdownParams{Reason: *reason, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyResetDrawdown, params, *jsonOut)
}

func runPolicyCorrectPeak(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy correct-peak")
	fromStatements := fs.Bool("from-statements", false, "anchor the corrected peak to the retained-statement replay (evidence-based)")
	peak := fs.Float64("peak", 0, "explicit corrected peak in base currency (corrections may only lower the peak)")
	reason := fs.String("reason", "", "why the recorded peak is wrong (journaled verbatim; the latch is untouched)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.CorrectPeakParams{FromStatements: *fromStatements, PeakBase: *peak, Reason: *reason, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyCorrectPeak, params, *jsonOut)
}

func runPolicyArtefact(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy artefact")
	note := fs.String("note", "", "free-text note for the journal")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy artefact: exactly one of morning|eod|weekly is required")
	}
	params := rpc.ArtefactParams{Artefact: fs.Arg(0), Note: *note, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyArtefact, params, *jsonOut)
}

func callPolicyWrite(ctx context.Context, env *Env, method string, params any, jsonOut bool) int {
	var res rpc.RiskPolicyWriteResult
	if err := env.Conn.Call(ctx, method, params, &res); err != nil {
		return fail(env, "policy: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintln(env.Stdout, res.Message)
	if res.Override != nil {
		fmt.Fprintf(env.Stdout, "  %s  %s expires %s\n", res.Override.ID, res.Override.Control, res.Override.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	return 0
}

func parseFlexibleTime(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use YYYY-MM-DD or RFC3339)", v)
}

func staleTag(stale bool) string {
	if stale {
		return "  [STALE]"
	}
	return ""
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func shortFingerprint(key string) string {
	if len(key) > 19 {
		return key[:19] + "…"
	}
	return key
}
