package cli

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// runMandate reads the daemon's mandate risk verdict
// (internal-docs/design/mandate-risk-budget.md): the book's loss measure
// (premium at risk plus the loss to armed stops) against the mandate's
// maximum loss (spec TM-04.12), its ex-ante volatility beside it as the
// sizing input, what drives it, the named stresses and the evidence clocks.
// The daemon evaluates; this renders.
func runMandate(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "mandate")
	jsonOut := fs.Bool("json", false, "emit the typed mandate verdict")
	refresh := fs.Bool("refresh", false, "read the account and positions again before judging")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	res, err := rpc.FetchMandateRisk(ctx, env.Conn, *refresh)
	if err != nil {
		return fail(env, "mandate: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderMandate(env, res)
	return 0
}

// mandateAmount prints a base-currency amount in whole units.
func mandateAmount(v float64, ccy string) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "unmeasurable"
	}
	sign := ""
	if v < 0 {
		sign = "−"
	}
	return sign + groupThousands(fmt.Sprintf("%.0f", math.Abs(v))) + " " + ccy
}

func mandatePct(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "∞"
	}
	return strings.Replace(fmt.Sprintf("%.0f%%", v), "-", "−", 1)
}

// mandateStressPnL states a stress figure, a loss when positive, as P&L in
// words: "loses 12,000 EUR" or "gains 73,847 EUR", never a signed loss a
// reader takes for the opposite.
func mandateStressPnL(loss float64, ccy string) string {
	if loss < 0 {
		return "gains " + mandateAmount(-loss, ccy)
	}
	return "loses " + mandateAmount(loss, ccy)
}

// mandateStressRange says a stress range with one verb where both ends share
// it: "loses 8,100 to 8,700 EUR", else "gains 200 EUR to loses 400 EUR".
func mandateStressRange(low, high float64, ccy string) string {
	switch {
	case low >= 0:
		return "loses " + mandateCap(low) + " to " + mandateAmount(high, ccy)
	case high < 0:
		return "gains " + mandateCap(-high) + " to " + mandateAmount(-low, ccy)
	}
	return mandateStressPnL(low, ccy) + " to " + mandateStressPnL(high, ccy)
}

// mandateCap prints a gap cap as a bare grouped figure for a parenthesis.
func mandateCap(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "unmeasurable"
	}
	return groupThousands(fmt.Sprintf("%.0f", math.Abs(v)))
}

// mandateHeldWords says how long the verdict has read as it does: days and
// hours past a day, else hours and minutes.
func mandateHeldWords(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days, hours, minutes := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// mandateShare prints a contribution share with one decimal.
func mandateShare(v float64) string {
	return strings.Replace(fmt.Sprintf("%.1f%%", v), "-", "−", 1)
}

func renderMandate(env *Env, res rpc.MandateRiskResult) {
	if !res.Active || res.Verdict == nil {
		riskDisplayLine(env, env.bold("Mandate")+"  ", "INACTIVE", env.dim)
		riskDisplayLine(env, "", nonEmpty(res.Message, "the risk constitution carries no [mandate] table"), nil)
		riskReadLine(env, "Next", "write [mandate] into risk-policy.toml with a higher policy_version; canary policy default constitution shows the keys")
		return
	}
	v := res.Verdict
	ccy := nonEmpty(v.BaseCurrency, "")
	headline, tone := "UNKNOWN", "yellow"
	switch v.Status {
	case mandate.StatusWithin:
		headline, tone = "WITHIN THE MAXIMUM LOSS", "green"
	case mandate.StatusOver:
		// A verdict kept by an older daemon, whose headline was the volatility.
		headline, tone = "OVER BUDGET", "yellow"
	case mandate.StatusReduce:
		headline, tone = "REDUCE", "red"
	}
	riskDisplayLine(env, env.bold("Mandate")+"  ", headline, func(s string) string { return riskTone(env, tone, env.bold(s)) })
	if !res.StatusSince.IsZero() {
		riskReadLine(env, "Since", res.StatusSince.Local().Format("Mon 2 Jan 15:04 MST"), mandateHeldWords(res.AsOf.Sub(res.StatusSince)))
	}
	renderMandateLoss(env, v.Loss, ccy)
	if v.Risk != nil {
		share := "sizing budget 0"
		if v.Capital.BudgetAnnualSD > 0 {
			share = mandatePct(v.Risk.RatioToBudget*100) + " of the sizing budget " + mandateAmount(v.Capital.BudgetAnnualSD, ccy)
		}
		riskReadLine(env, "Volatility", mandateAmount(v.Risk.SDAnnualBase, ccy)+" a year", "1-day swing "+mandateAmount(v.Risk.SD1DBase, ccy), "99% expected shortfall "+mandateAmount(v.Risk.ES99Base, ccy), share)
	}
	if c := v.Capital; c.Known && res.Policy != nil {
		riskReadLine(env, "Loss room", fmt.Sprintf("%s × %s at room %s", mandatePct(res.Policy.MaxLossPct), mandateAmount(c.BudgetBase, ccy), mandatePct(c.Room*100)),
			fmt.Sprintf("loss %s of the %s maximum from the high-water mark %s", mandatePct(c.DrawdownPct), mandatePct(res.Policy.MaxLossPct), mandateAmount(c.HWMRiskBase, ccy)))
		riskReadLine(env, "Sizing budget", fmt.Sprintf("%s × %s at room %s a year", mandatePct(res.Policy.VolTargetPct), mandateAmount(c.BudgetBase, ccy), mandatePct(c.Room*100)), "sizes the sleeve allocations, never the verdict")
		flows := ""
		if c.FlowsSinceStart != 0 {
			flows = "flows since " + c.StartedAt + " " + mandateAmount(c.FlowsSinceStart, ccy)
		}
		riskReadLine(env, "Risk base", mandateAmount(c.RiskBase, ccy)+" = equity "+mandateAmount(c.EquityBase, ccy)+" − tax reserve "+mandateAmount(c.TaxReserveBase, ccy), flows)
	}
	renderMandateOwnerStop(env, v.OwnerStop, ccy)
	renderMandateGate(env, res.Gate)
	renderMandateReduction(env, res.Reduction, ccy)
	renderMandateAllocations(env, v, ccy)
	renderMandateExposure(env, res.Exposure, ccy)
	renderMandateRegister(env, res.Register)
	if len(v.Reasons) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Why unknown"))
		for _, reason := range v.Reasons {
			riskDisplayLine(env, "  ", reason, nil)
		}
	}
	if len(v.Underlyings) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Contributions")+"  share of the risk · beta to "+mandate.IndexSymbol+" · one-name gap against its cap")
		for _, u := range v.Underlyings {
			shock := u.GapShockPct
			if shock <= 0 {
				shock = v.Stresses.GapShockPct
			}
			gap := fmt.Sprintf("gap −%s %s", mandatePct(shock), mandateStressPnL(u.GapDownLossBase, ccy))
			if u.GapUpLossBase != nil {
				gap += fmt.Sprintf(", +%s %s", mandatePct(shock), mandateStressPnL(*u.GapUpLossBase, ccy))
			}
			gap += " (cap " + mandateCap(u.GapCapBase) + ")"
			line := briefJoin(u.Underlying, mandateShare(u.ContributionPct), fmt.Sprintf("beta %.2f", u.Beta), gap)
			style := func(s string) string { return s }
			if u.GapBreach {
				line += " · BREACH"
				style = env.red
			}
			riskDisplayLine(env, "  ", line, style)
		}
	}
	if len(v.Legs) > 1 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Positions"))
		for _, l := range v.Legs {
			if l.Unknown != "" {
				riskDisplayLine(env, "  ", fmt.Sprintf("%s: %s", l.Symbol, l.Unknown), env.yellow)
				continue
			}
			loss := ""
			if l.LossBase > 0 {
				loss = "loss " + mandateAmount(l.LossBase, ccy)
			}
			riskDisplayLine(env, "  ", briefJoin(l.Symbol, mandateShare(l.ContributionPct), mandateAmount(l.ValueBase, ccy), loss), nil)
		}
	}
	if s := v.Stresses; s != nil {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Stresses"))
		riskReadLine(env, "  Index −"+mandatePct(s.IndexShockPct)+" through beta", mandateStressPnL(s.IndexLossBase, ccy))
		breaches := "none"
		if len(s.GapBreaches) > 0 {
			breaches = strings.Join(s.GapBreaches, ", ")
		}
		riskReadLine(env, "  Gap cap per name", mandateAmount(s.GapCapBase, ccy), "breaches: "+breaches)
		if v.Risk != nil {
			riskReadLine(env, "  Currency share of the risk", mandatePct(v.Risk.FXContributionPct))
		}
	}
	renderMandateIVFallback(env, v.IVFallback, ccy)
	if len(v.NotModelled) > 0 {
		riskReadLine(env, "Not modelled", strings.Join(v.NotModelled, "; "))
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Evidence"))
	if e := res.Evidence; e != nil {
		riskReadLine(env, "  Verdict", formatTimeShort(e.EvaluatedAt.Local()))
		if !e.EquityAsOf.IsZero() {
			riskReadLine(env, "  Equity", formatTimeShort(e.EquityAsOf.Local()))
		}
		if !e.PositionsAsOf.IsZero() {
			riskReadLine(env, "  Positions", formatTimeShort(e.PositionsAsOf.Local()))
		}
		if v.Scenarios > 0 {
			riskReadLine(env, "  Scenarios", fmt.Sprintf("%d daily returns, %s to %s", v.Scenarios, v.ScenariosFrom, v.ScenariosTo))
		}
		for _, s := range e.Series {
			switch {
			case s.Missing:
				riskWarnReadLine(env, "  "+s.Key, "no history yet")
			case s.Stale:
				riskWarnReadLine(env, "  "+s.Key, fmt.Sprintf("stale: %d closes to %s", s.Closes, s.LastClose), "read "+formatTimeShort(s.FetchedAt.Local()))
			default:
				riskReadLine(env, "  "+s.Key, fmt.Sprintf("%d closes to %s", s.Closes, s.LastClose), "read "+formatTimeShort(s.FetchedAt.Local()))
			}
		}
	}
	riskReadLine(env, "", "The mandate refuses stock and ETF additions while the loss measure is over the maximum loss or your sleeve is over its allocation, and option buys outside the premium limits. Its reduction is a set of proposals (canary proposals); the daemon places them itself only when mandate_reduction is pre-authorised, after the veto window.")
}

// renderMandateLoss prints the loss measure the verdict is judged by (spec
// TM-04.12): premium at risk plus the cash loss to armed stops against the
// loss room, the names counting most and the share lines no stop bounds.
func renderMandateLoss(env *Env, l *mandate.LossMeasure, ccy string) {
	if l == nil {
		return
	}
	room := "no loss room left"
	if l.RoomBase > 0 {
		room = mandatePct(l.RatioToRoom*100) + " of the loss room " + mandateAmount(l.RoomBase, ccy)
	}
	parts := []string{mandateAmount(l.TotalBase, ccy) + " = premium at risk " + mandateAmount(l.PremiumBase, ccy) + " + loss to armed stops " + mandateAmount(l.StopLossBase, ccy), room}
	if len(l.Unstopped) > 0 {
		parts = append(parts, "unstopped, counting nothing: "+strings.Join(l.Unstopped, ", "))
	}
	riskReadLine(env, "Loss", parts...)
}

// renderMandateGate prints the additions gate as it reads now: for stock
// and ETF additions the volatility budget, for option buys the premium
// limits with the volatility verdict as their warning, and an active
// override of the volatility budget with its reason and expiry (owner
// decision 2026-10-23 18:22 CEST).
func renderMandateGate(env *Env, g *rpc.MandateGateView) {
	if g == nil {
		return
	}
	if g.RefusesAdditions {
		riskDisplayLine(env, "Stock additions · ", briefJoin("refused", g.Reason), env.red)
	} else {
		riskDisplayLine(env, "Stock additions · ", briefJoin("open", nonEmpty(g.Reason, "reductions, closes, cancels and hedges always are")), env.green)
	}
	if o := g.Options; o != nil {
		switch {
		case o.RefusesAdditions:
			riskDisplayLine(env, "Option buys · ", briefJoin("refused by the premium limits", o.Reason), env.red)
		case o.Warning != "":
			riskDisplayLine(env, "Option buys · ", briefJoin("open within the premium limits", "warning: "+o.Warning), env.yellow)
		default:
			riskDisplayLine(env, "Option buys · ", "open within the premium limits (per name and for the book)", env.green)
		}
	}
	if o := g.Override; o != nil {
		riskDisplayLine(env, "Override · ", briefJoin(o.Control+" until "+formatTimeShort(o.ExpiresAt.Local()), fmt.Sprintf("%q", o.Reason), "granted "+formatTimeShort(o.GrantedAt.Local()), o.ID), env.yellow)
	}
}

// renderMandateExposure prints the book through the greeks and the premium
// at risk against its caps: the book's totals, then one row per underlying.
// An absent figure prints as "?" and the reasons follow the rows.
func renderMandateExposure(env *Env, e *rpc.MandateExposureView, ccy string) {
	if e == nil {
		return
	}
	figure := func(v *float64) string {
		if v == nil {
			return "?"
		}
		return mandateAmount(*v, ccy)
	}
	premium := func(r rpc.MandateExposureRow, capPct float64) string {
		if r.PremiumAtRiskBase == nil {
			return "premium ?"
		}
		out := "premium " + mandateAmount(*r.PremiumAtRiskBase, ccy)
		if r.PremiumPctNLV != nil {
			out += fmt.Sprintf(" (%.1f%% of NLV, cap %s%%)", *r.PremiumPctNLV, strconv.FormatFloat(capPct, 'f', -1, 64))
		}
		if r.PremiumOver {
			out += " · AT CAP"
		}
		return out
	}
	nlv := "NLV unknown"
	if e.NLVBase != nil {
		nlv = "NLV " + mandateAmount(*e.NLVBase, ccy)
	} else if e.NLVReason != "" {
		nlv += " (" + e.NLVReason + ")"
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Exposure")+"  net delta in money · through beta to "+mandate.IndexSymbol+" · gamma per 1% move · theta a day · vega a point · premium at risk against its cap · "+nlv)
	book := e.Book
	riskDisplayLine(env, "  Book · ", briefJoin("delta "+figure(book.DeltaBase), "β-delta "+figure(book.DeltaBetaBase), "gamma "+figure(book.GammaBase), "theta "+figure(book.ThetaBase), "vega "+figure(book.VegaBase), premium(book, e.BookPremiumCapPct)+nonEmptyPrefix(" · ", e.BookPremiumSet, " set")), nil)
	for _, r := range e.Names {
		share := ""
		if r.ContributionPct != nil {
			share = mandateShare(*r.ContributionPct) + " of the risk"
		}
		beta := ""
		if r.Beta != nil {
			beta = fmt.Sprintf("beta %.2f", *r.Beta)
		}
		style := func(s string) string { return s }
		if r.PremiumOver {
			style = env.red
		}
		riskDisplayLine(env, "  ", briefJoin(r.Underlying, share, beta, "delta "+figure(r.DeltaBase), "β-delta "+figure(r.DeltaBetaBase), "gamma "+figure(r.GammaBase), "theta "+figure(r.ThetaBase), "vega "+figure(r.VegaBase), premium(r, e.NamePremiumCapPct)), style)
		for _, why := range r.Unknown {
			riskDisplayLine(env, "    ", why, env.yellow)
		}
	}
	for _, why := range book.Unknown {
		riskDisplayLine(env, "  ", "book: "+why, env.yellow)
	}
}

// nonEmptyPrefix wraps a value in its prefix and suffix, or prints nothing.
func nonEmptyPrefix(prefix, value, suffix string) string {
	if value == "" {
		return ""
	}
	return prefix + value + suffix
}

// renderMandateOwnerStop prints the owner sleeve's drawdown stop
// (mandate-risk-budget.md, "The forced reduction"; owner delegation
// 2026-10-18 21:41 CEST): the sleeve's fall from its own high-water mark
// against the stop, and, engaged, the risk the reduction cuts it to.
func renderMandateOwnerStop(env *Env, s *mandate.SleeveStop, ccy string) {
	if s == nil {
		return
	}
	engaged := ""
	if s.Engaged {
		engaged = "STOP ENGAGED since " + s.EngagedAt.Local().Format("Mon 2 Jan 15:04 MST")
	}
	if !s.Known {
		why := "unknown"
		if len(s.Reasons) > 0 {
			why = s.Reasons[0]
		}
		riskDisplayLine(env, "Owner sleeve · ", briefJoin(engaged, "stop state held: "+why), env.yellow)
		return
	}
	stop := "stop " + mandatePct(s.StopPct)
	if s.Engaged {
		stop += ", releases under " + mandatePct(s.ReleasePct)
	}
	line := briefJoin(engaged, fmt.Sprintf("%s below its high-water mark %s", mandatePct(s.DrawdownPct), mandateAmount(s.HWMRiskBase, ccy)), stop)
	if s.MachinePnLBase != 0 {
		line = briefJoin(line, "machine P&L since the start "+mandateAmount(s.MachinePnLBase, ccy)+" set aside")
	}
	if s.Engaged {
		line = briefJoin(line, fmt.Sprintf("reduce-only to %s a year, half its risk %s at the stop", mandateAmount(s.TargetUseBase, ccy), mandateAmount(s.UseAtStopBase, ccy)), "risk now "+mandateAmount(s.UseBase, ccy))
		riskDisplayLine(env, "Owner sleeve · ", line, env.red)
		return
	}
	riskDisplayLine(env, "Owner sleeve · ", briefJoin(line, "risk "+mandateAmount(s.UseBase, ccy)+" a year"), nil)
}

// renderMandateReduction prints the forced reduction under the gate: its
// state, who places the rows and when, and the rows in dispatch order.
func renderMandateReduction(env *Env, r *rpc.MandateReductionView, ccy string) {
	if r == nil {
		return
	}
	state := strings.ReplaceAll(r.State, "_", " ")
	var style func(string) string
	var head string
	switch {
	case r.Held && r.Rows > 0:
		head, style = fmt.Sprintf("%d %s not sent: %s", r.Rows, pluralWord(r.Rows, "row", "rows"), r.HoldReason), env.yellow
	case r.Rows > 0 && r.PreAuthorised:
		head, style = fmt.Sprintf("%d %s, pre-authorised", r.Rows, pluralWord(r.Rows, "row", "rows")), env.yellow
		if r.Pending > 0 && !r.VetoUntil.IsZero() {
			head += fmt.Sprintf(": the daemon places %d after the veto window, the last at %s", r.Pending, formatTimeShort(r.VetoUntil.Local()))
		}
	case r.Rows > 0:
		head, style = fmt.Sprintf("%d %s wait for your approval; mandate_reduction is not pre-authorised", r.Rows, pluralWord(r.Rows, "row", "rows")), env.yellow
	default:
		head = r.Reason
	}
	riskDisplayLine(env, "Reduction · ", briefJoin(state, head), style)
	for _, o := range r.Orders {
		verb := strings.ToLower(o.Action)
		parts := []string{fmt.Sprintf("%d. %s %d %s", o.Rank, verb, o.Quantity, queuedContractLabel(o.Contract)), mandatePhaseWords(o.Phase)}
		if o.LossBase > 0 {
			parts = append(parts, mandateAmount(o.LossBase, ccy)+" of the loss measure")
		}
		parts = append(parts, mandateShare(o.ContributionPct)+" of the risk")
		switch {
		case o.Held:
			parts = append(parts, "held")
		case o.Blocked != "":
			parts = append(parts, "blocked: "+o.Blocked)
		case !o.SubmitAt.IsZero():
			parts = append(parts, "places itself at "+formatTimeShort(o.SubmitAt.Local())+" unless vetoed")
		case o.Automatic != "":
			parts = append(parts, o.Automatic)
		case !r.PreAuthorised:
			parts = append(parts, "waits for your approval")
		}
		riskDisplayLine(env, "  ", briefJoin(parts...), nil)
	}
}

// mandatePhaseWords names why a reduction row is in the batch.
// renderMandateAllocations lists each sleeve's use against its allocation (allocations,
// 2026-10-18): yours, the rest of the budget, then the machine's, each its
// stage times its allocation while it sends orders.
func renderMandateAllocations(env *Env, v *mandate.Verdict, ccy string) {
	if len(v.Allocations) == 0 {
		return
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Risk allocations")+"  each sleeve's use against its allocation, a year · alone: its own risk")
	for _, s := range v.Allocations {
		name, detail := strings.ToUpper(s.Sleeve[:1])+s.Sleeve[1:], s.Note
		switch {
		case s.Sleeve == mandate.SleeveOwner:
			name, detail = "Owner", "the budget less the machine's allocations"
		case s.StagePct > 0:
			detail = fmt.Sprintf("stage %s of its %s allocation", mandatePct(s.StagePct), mandatePct(s.TargetPct))
		}
		line := briefJoin(name, mandateAmount(s.UseBase, ccy)+" of "+mandateAmount(s.AllocationBase, ccy), "alone "+mandateAmount(s.StandaloneBase, ccy))
		style := func(s string) string { return s }
		if s.Over {
			line += " · OVER by " + mandateAmount(s.ExcessBase, ccy)
			style = env.red
		}
		riskDisplayLine(env, "  ", briefJoin(line, detail), style)
	}
}

// renderMandateRegister lists the strategy register
// (internal-docs/design/strategy-register.md): each machine strategy, its
// rule and whether the policy cites its spec, its stage as written and as
// this build runs it, the part of its allocation it reserves, and why it
// holds.
func renderMandateRegister(env *Env, entries []rpc.MandateStrategyView) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Strategy register")+"  each machine strategy as the policy writes it and this build runs it")
	for _, e := range entries {
		state := e.State
		if e.Stage != "" && e.Stage != e.State {
			state = e.State + " (written " + e.Stage + ")"
		}
		reserve := "reserves nothing"
		if e.ReservedPct > 0 {
			reserve = fmt.Sprintf("reserves %s of its %s allocation", mandatePct(e.ReservedPct), mandatePct(e.AllocationPct))
		}
		spec := "spec compiled, not cited"
		if e.SpecCited {
			spec = "spec cited " + mandateShortHash(e.SpecHash)
		}
		style := func(s string) string { return s }
		if e.State == "hold" {
			style = env.yellow
		}
		rule := e.Rule
		if rule == "" {
			rule = "no rule"
		}
		riskDisplayLine(env, "  ", briefJoin(e.ID, rule, state, reserve, spec), style)
		for _, r := range e.Reasons {
			riskDisplayLine(env, "    ", "holds: "+r, env.yellow)
		}
		for _, n := range e.Notes {
			riskDisplayLine(env, "    ", n, nil)
		}
		if e.ISIN != "" || e.Signal != "" {
			riskDisplayLine(env, "    ", briefJoin("signal "+nonEmpty(e.Signal, "unwritten"), strings.TrimSpace(e.ISIN+" "+e.Currency)), nil)
		}
		for _, d := range append(append([]string(nil), e.DeclaredSeries...), e.DeclaredChains...) {
			riskDisplayLine(env, "    ", "collects "+d, nil)
		}
	}
}

// mandateShortHash is a spec hash's first twelve characters.
func mandateShortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func mandatePhaseWords(phase string) string {
	switch phase {
	case rpc.MandatePhaseExpiry:
		return "expires in the money first"
	case rpc.MandatePhaseGap:
		return "gap over its cap"
	case rpc.MandatePhasePremium:
		return "largest premium at risk"
	case rpc.MandatePhaseRisk:
		return "largest risk"
	case rpc.MandatePhaseStop:
		return "owner sleeve stop"
	}
	return phase
}

// renderMandateIVFallback says which options are priced on the volatility
// their own mark implies, the S&P-fall range across their bands and how the
// verdict judged it.
func renderMandateIVFallback(env *Env, r *mandate.IVFallbackReport, ccy string) {
	if r == nil {
		return
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Fallback volatility"))
	judged := "measured"
	switch r.Tier {
	case mandate.IVTierRange:
		judged = "measured: the verdict is the same at both edges"
	case mandate.IVTierUnknown:
		judged = "too wide: the verdict is unknown"
	}
	width := "± " + mandateAmount(r.UncertaintyBase, ccy)
	if r.RatioToRoom != nil {
		width += ", " + mandatePct(*r.RatioToRoom*100) + " of the loss room"
	}
	riskReadLine(env, "  Index fall range", mandateStressRange(r.IndexLossLowBase, r.IndexLossHighBase, ccy), width, judged)
	for _, l := range r.Legs {
		as := ""
		if !l.AsOf.IsZero() {
			as = "mark of " + formatTimeShort(l.AsOf.Local())
		}
		riskReadLine(env, "  "+l.Symbol, briefJoin(fmt.Sprintf("IV %.0f%% implied from its mark", l.IV*100), fmt.Sprintf("band ±%g%%, %d days to expiry", l.BandPct, l.DaysToExpiry), as))
	}
}
