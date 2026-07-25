package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func runBrief(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "brief")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (never stamps)")
	kind := fs.String("kind", "", "stamp morning or eod instead of the first incomplete daily artefact")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	*kind = strings.ToLower(strings.TrimSpace(*kind))
	if *kind != "" && *kind != rpc.BriefKindMorning && *kind != rpc.BriefKindEOD {
		return fail(env, "brief: --kind must be morning or eod")
	}

	var res rpc.BriefResult
	if err := env.Conn.Call(ctx, rpc.MethodBriefSnapshot, rpc.BriefSnapshotParams{}, &res); err != nil {
		return fail(env, "brief: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderBrief(env, res)

	if !briefHumanOrigin(env.Origin) {
		fmt.Fprintln(env.Stdout, "\nagent-origin render — not stamped")
		return 0
	}
	target := res.StampTarget
	if *kind != "" {
		target = *kind
	}
	if target == rpc.BriefKindMonthly {
		fmt.Fprintln(env.Stdout, "\nmonthly foreground render not recorded — paired-device origin required")
		return 0
	}
	if target == "" {
		fmt.Fprintf(env.Stdout, "\nnot stamped — %s\n", nonEmpty(res.StampTargetReason, "no daily artefact target"))
		return 0
	}
	var ack rpc.BriefAckResult
	err := env.Conn.Call(ctx, rpc.MethodBriefAck, rpc.BriefAckParams{
		Kind: target, BriefFingerprint: res.BriefFingerprint, Origin: env.Origin,
	}, &ack)
	if err != nil {
		// Rendering succeeded. Stamping is advisory, but the failure must be
		// conspicuous and must not turn a useful brief into a failing command.
		fmt.Fprintf(env.Stderr, "ibkr: brief rendered but stamp failed: %v\n", err)
		return 0
	}
	if ack.AlreadyStamped {
		fmt.Fprintf(env.Stdout, "\nstamp: %s artefact for %s — already stamped\n", ack.Kind, ack.Day)
	} else {
		fmt.Fprintf(env.Stdout, "\nstamp: %s artefact for %s\n", ack.Kind, ack.Day)
	}
	return 0
}

func briefHumanOrigin(origin string) bool {
	return origin == rpc.OrderOriginHumanTTY || origin == rpc.OrderOriginPairedDevice
}

func renderBrief(env *Env, res rpc.BriefResult) {
	fmt.Fprintf(env.Stdout, "Daily brief — %s  %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), shortFingerprint(res.BriefFingerprint))
	renderBriefReview(env, res.Review)
	renderBriefReady(env, res.Ready)
}

func renderBriefReview(env *Env, review rpc.BriefReviewSection) {
	fmt.Fprintln(env.Stdout, "\nReview  (last completed session)")
	acct := "—"
	if review.SessionPnL.EquityBase != nil {
		acct = formatMoneyCcy(*review.SessionPnL.EquityBase, review.SessionPnL.BaseCurrency)
	}
	if review.SessionPnL.DailyPnLBase != nil {
		acct += " · day " + formatMoneyCcy(*review.SessionPnL.DailyPnLBase, review.SessionPnL.BaseCurrency)
	}
	briefLine(env, "session P&L", review.SessionPnL.BriefRowState, acct)
	movers := make([]string, 0, len(review.Attribution.Rows)+1)
	for _, mover := range review.Attribution.Rows {
		movers = append(movers, fmt.Sprintf("%s %s", mover.Symbol, formatMoneyCcy(mover.DailyPnLBase, review.SessionPnL.BaseCurrency)))
	}
	if review.Attribution.OtherPnLBase != nil && review.Attribution.OtherCount > 0 {
		unit := "others"
		if review.Attribution.OtherCount == 1 {
			unit = "other"
		}
		movers = append(movers, fmt.Sprintf("%d %s %s", review.Attribution.OtherCount, unit,
			formatMoneyCcy(*review.Attribution.OtherPnLBase, review.SessionPnL.BaseCurrency)))
	}
	briefLine(env, "by underlying", review.Attribution.BriefRowState, strings.Join(movers, " · "))
	delta := fmt.Sprintf("%d transition(s), %d added, %d removed", len(review.RulesDelta.Transitions), len(review.RulesDelta.Added), len(review.RulesDelta.Removed))
	if review.RulesDelta.RulebookFingerprintChanged {
		delta += " · fingerprint changed"
	}
	briefLine(env, "rules delta", review.RulesDelta.BriefRowState, delta)
	briefLine(env, "proposals", review.Proposals.BriefRowState, fmt.Sprintf("%d offered · %d acted", review.Proposals.Offered, review.Proposals.Acted))
	briefLine(env, "overrides used", review.Overrides.BriefRowState, fmt.Sprintf("%d", len(review.Overrides.Rows)))
	capitalEvents := "no capital events"
	if review.CapitalEvents.Latched {
		capitalEvents = "LATCHED"
		if review.CapitalEvents.LatchAgeDays != nil {
			unit := "days"
			if *review.CapitalEvents.LatchAgeDays == 1 {
				unit = "day"
			}
			capitalEvents += fmt.Sprintf(" · %d %s", *review.CapitalEvents.LatchAgeDays, unit)
		}
		if review.CapitalEvents.ConsumedPctAtLatch != nil {
			capitalEvents += fmt.Sprintf(" · engaged at %.1f%%", *review.CapitalEvents.ConsumedPctAtLatch)
		}
	}
	if !review.CapitalEvents.PeakAsOf.IsZero() {
		capitalEvents = briefJoin(capitalEvents, "peak set "+review.CapitalEvents.PeakAsOf.Local().Format("2006-01-02 15:04"))
	}
	briefLine(env, "capital events", review.CapitalEvents.BriefRowState, capitalEvents)
	reconcile := "never"
	if !review.Reconcile.LastReconciledAt.IsZero() {
		reconcile = review.Reconcile.LastReconciledAt.Local().Format("2006-01-02 15:04")
		if review.Reconcile.Source != "" {
			reconcile += " · " + review.Reconcile.Source
		}
	}
	if !review.Reconcile.Deadline.IsZero() {
		reconcile += " · due " + review.Reconcile.Deadline.Local().Format("2006-01-02")
		if review.Reconcile.DaysRemaining != nil {
			reconcile += fmt.Sprintf(" (%d day(s))", *review.Reconcile.DaysRemaining)
		}
	}
	briefLine(env, "reconcile", review.Reconcile.BriefRowState, reconcile)
	briefLine(env, "auto-extend", review.AutoExtend.BriefRowState, review.AutoExtend.ReportID)
	oneTap := "blocked"
	if review.OneTap.Signable {
		oneTap = "signable · ibkr policy capital-event reconcile"
	} else if len(review.OneTap.Blockers) > 0 {
		oneTap += " · " + strings.Join(review.OneTap.Blockers, "; ")
	}
	briefLine(env, "one-tap sign-off", review.OneTap.BriefRowState, oneTap)
	orders := "—"
	if review.WorkingOrders.Count != nil {
		orders = fmt.Sprintf("%d", *review.WorkingOrders.Count)
	}
	briefLine(env, "working orders", review.WorkingOrders.BriefRowState, orders)
}

func renderBriefReady(env *Env, ready rpc.BriefReadySection) {
	fmt.Fprintln(env.Stdout, "\nReady  (today)")
	briefLine(env, "regime", ready.Regime.BriefRowState,
		briefJoin(ready.Regime.Stage, ready.Regime.Verdict))
	breadth := "—"
	if ready.Breadth.PctAbove50DMA != nil {
		breadth = fmt.Sprintf("50-DMA %.1f%%", *ready.Breadth.PctAbove50DMA)
		if ready.Breadth.PctAbove200DMA != nil {
			breadth += fmt.Sprintf(" · 200-DMA %.1f%%", *ready.Breadth.PctAbove200DMA)
		}
	}
	briefLine(env, "breadth", ready.Breadth.BriefRowState, breadth)
	gamma := "—"
	if ready.Gamma.Spot != nil {
		gamma = fmt.Sprintf("spot %.2f", *ready.Gamma.Spot)
		if ready.Gamma.ZeroGamma != nil {
			gamma += fmt.Sprintf(" · zero %.2f", *ready.Gamma.ZeroGamma)
		}
		if ready.Gamma.GapPct != nil {
			gamma += fmt.Sprintf(" · gap %+.1f%%", *ready.Gamma.GapPct)
		}
	}
	briefLine(env, "dealer gamma", ready.Gamma.BriefRowState, gamma)
	// Action and severity are usually the same word; printing both reads as a
	// stutter, so the pair collapses when equal (the SPA does the same).
	severity := ready.Stress.Severity
	if strings.EqualFold(severity, ready.Stress.Action) {
		severity = ""
	}
	briefLine(env, "stress", ready.Stress.BriefRowState,
		briefJoin(ready.Stress.Action, severity, ready.Stress.Summary))
	briefLine(env, "session", ready.Session.BriefRowState, briefJoin(ready.Session.Market, ready.Session.State))
	for _, event := range ready.MarketEvents {
		value := fmt.Sprintf("%d", event.Count)
		if len(event.Symbols) > 0 {
			value += " · " + strings.Join(event.Symbols, ", ")
		}
		briefLine(env, event.Kind, event.BriefRowState, value)
	}
	capital := ""
	if ready.Capital.Tier != "" {
		capital = "tier " + ready.Capital.Tier
	}
	if ready.Capital.Enforcement != "" {
		capital = briefJoin(capital, "enforcement "+ready.Capital.Enforcement)
	}
	if ready.Capital.ConsumedPct != nil {
		capital = briefJoin(capital, fmt.Sprintf("%.1f%% consumed", *ready.Capital.ConsumedPct))
	}
	if !ready.Capital.PeakAsOf.IsZero() {
		capital = briefJoin(capital, "peak set "+ready.Capital.PeakAsOf.Local().Format("2006-01-02 15:04"))
	}
	briefLine(env, "capital", ready.Capital.BriefRowState, capital)
	latch := "open"
	if ready.Latch.Latched {
		latch = "LATCHED"
		if ready.Latch.AgeDays != nil {
			unit := "days"
			if *ready.Latch.AgeDays == 1 {
				unit = "day"
			}
			latch += fmt.Sprintf(" · %d %s", *ready.Latch.AgeDays, unit)
		}
		if ready.Latch.ConsumedPctAtLatch != nil {
			latch += fmt.Sprintf(" · engaged at %.1f%%", *ready.Latch.ConsumedPctAtLatch)
		}
	}
	briefLine(env, "drawdown latch", ready.Latch.BriefRowState, latch)
	briefLine(env, "premium at risk", ready.PremiumAtRisk.BriefRowState, briefMoney(ready.PremiumAtRisk))
	briefLine(env, "hedge cost / day", ready.HedgeCost.BriefRowState, briefMoney(ready.HedgeCost))
	briefLine(env, "policy drift", ready.PolicyDrift.BriefRowState, fmt.Sprintf("%d", len(ready.PolicyDrift.Rows)))
	for _, artefact := range ready.Artefacts.Rows {
		state := "not declared"
		if artefact.Declared {
			state = "not completed"
		}
		if artefact.Completed {
			state = "completed"
		}
		briefLine(env, "artefact "+artefact.Kind, artefact.BriefRowState, state)
	}
	if monthly := ready.MonthlyPulse; monthly != nil {
		value := briefJoin(monthly.Status, monthly.Month)
		if !monthly.DueAt.IsZero() {
			value += " · due " + monthly.DueAt.Local().Format("2006-01-02 15:04")
		}
		if !monthly.CompletedAt.IsZero() {
			value += " · rendered " + monthly.CompletedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(env.Stdout, "  %-18s %-11s %s\n", "monthly pulse", monthly.Status, value)
	}
}

func briefLine(env *Env, label string, state rpc.BriefRowState, value string) {
	if strings.TrimSpace(value) == "" {
		value = "—"
	}
	fmt.Fprintf(env.Stdout, "  %-18s %-11s %s\n", label, state.Status, value)
	fmt.Fprintf(env.Stdout, "    %s\n", state.Detail)
}

func briefJoin(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func briefMoney(row rpc.BriefMoneyCoverageRow) string {
	if row.AmountBase == nil {
		return "—"
	}
	return formatMoneyCcy(*row.AmountBase, row.BaseCurrency)
}
