package cli

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/rpc"
)

// runCore reads the daemon's core sleeve target
// (internal-docs/design/core-sleeve.md): the month's decision under the
// compiled rule, the target, the holding, the band and the action the rule
// would take, and while the core is live its pending row and the last
// decision. The daemon evaluates; this renders and changes nothing.
func runCore(ctx context.Context, env *Env, args []string) int {
	if idx := firstPositionalIndex(args); idx >= 0 && args[idx] == "resolve" {
		return runCoreResolve(ctx, env, append(append([]string{}, args[:idx]...), args[idx+1:]...))
	}
	fs := flagSet(env, "core")
	jsonOut := fs.Bool("json", false, "emit the typed core target")
	refresh := fs.Bool("refresh", false, "measure the mandate again before deciding")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	res, err := rpc.FetchCoreTarget(ctx, env.Conn, *refresh)
	if err != nil {
		return fail(env, "core: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderCore(env, res)
	return 0
}

// corePrice prints a fund price with its cents.
func corePrice(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "unmeasurable"
	}
	return fmt.Sprintf("%.2f", v)
}

func corePct(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

func coreShares(n int) string {
	return groupThousands(fmt.Sprint(n)) + " " + pluralWord(n, "share", "shares")
}

// renderCore renders a strategy target: the core's, headed "Core", or any
// register entry's, headed by its id (strategy-evaluator.md §6).
func renderCore(env *Env, res rpc.CoreTargetResult) {
	isCore := res.Strategy == "" || res.Strategy == "core"
	heading, name := "Core", "the core"
	if !isCore {
		heading, name = res.Strategy, "the "+res.Strategy+" strategy"
	}
	if !res.Active {
		riskDisplayLine(env, env.bold(heading)+"  ", "INACTIVE", env.dim)
		riskDisplayLine(env, "", nonEmpty(res.Message, "the risk constitution carries no [mandate.core] table"), nil)
		riskReadLine(env, "Next", "write [mandate.core] into risk-policy.toml with a higher policy_version; canary policy default constitution shows the keys")
		return
	}
	ccy := core.Currency
	sym := core.Symbol
	if !isCore {
		ccy, sym = "", res.Strategy
	}
	if res.Instrument != nil {
		ccy, sym = nonEmpty(res.Instrument.Currency, ccy), nonEmpty(res.Instrument.Symbol, nonEmpty(res.Instrument.ISIN, sym))
	}
	a := res.Action
	headline, tone := "HOLD", "yellow"
	switch {
	case res.Row != nil:
		state := "PENDING"
		if res.Row.State == rpc.TradeProposalStateBlocked {
			state, tone = "BLOCKED", "red"
		} else {
			tone = "green"
		}
		headline = fmt.Sprintf("ROW %s: %s %s %s", state, strings.ToUpper(res.Row.Action), groupThousands(fmt.Sprint(res.Row.Quantity)), sym)
	case res.Status == rpc.CoreStatusReady && a != nil:
		switch a.Kind {
		case core.ActionBuy:
			headline, tone = fmt.Sprintf("WOULD BUY %s %s", groupThousands(fmt.Sprint(a.Quantity)), sym), "green"
		case core.ActionSell:
			headline, tone = fmt.Sprintf("WOULD SELL %s %s", groupThousands(fmt.Sprint(a.Quantity)), sym), "yellow"
		case core.ActionSellAll:
			headline, tone = fmt.Sprintf("WOULD SELL ALL %s %s", groupThousands(fmt.Sprint(a.Quantity)), sym), "yellow"
		case core.ActionWait:
			headline, tone = "WAITS", "yellow"
		default:
			headline, tone = "NO TRADE", "green"
		}
	}
	riskDisplayLine(env, env.bold(heading)+"  ", headline, func(s string) string { return riskTone(env, tone, env.bold(s)) })
	spec := res.SpecHash
	if len(spec) > 12 {
		spec = spec[:12]
	}
	fund := ""
	if in := res.Instrument; in != nil {
		fund = strings.TrimSpace(in.ISIN + " " + in.Currency)
	}
	if isCore {
		riskReadLine(env, "Rule", res.Rule, "spec "+res.SpecID+" "+spec, fund)
	} else {
		riskReadLine(env, "Rule", res.Rule, "signal "+nonEmpty(res.Signal, "unwritten"), "spec cited "+nonEmpty(spec, "(none)"), fund, "sleeve "+res.Sleeve)
	}
	if st := res.Stage; st != nil {
		reserve := "reserves nothing"
		if st.ReservedPct > 0 {
			reserve = fmt.Sprintf("reserves %g%% of its allocation", st.ReservedPct)
		}
		cited := "spec cited"
		if !st.SpecHashCited {
			cited = "spec compiled, not cited"
		}
		riskReadLine(env, "Stage", append([]string{st.State, reserve, cited}, st.Notes...)...)
	}
	if in := res.Instrument; in != nil {
		riskReadLine(env, "Contract", coreContractLine(*in)...)
	}
	if a != nil {
		action := a.Why
		if a.Quantity > 0 && a.Kind != core.ActionHold {
			action = briefJoin(strings.ReplaceAll(a.Kind, "_", " ")+" "+coreShares(a.Quantity), a.Why)
		}
		riskReadLine(env, "Action", action)
	}
	renderCoreRow(env, res.Row, sym)
	renderCoreLastDecision(env, res.LastDecision)
	if len(res.Reasons) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Why it holds"))
		for _, r := range res.Reasons {
			riskDisplayLine(env, "  ", r, nil)
		}
	}
	if d := res.Decision; d != nil && d.Status == core.StatusOK {
		trend := fmt.Sprintf("close %s above its 10-month average %s: trend on", corePrice(d.Close), corePrice(d.SMA10))
		if !d.TrendOn {
			trend = fmt.Sprintf("close %s not above its 10-month average %s: trend off", corePrice(d.Close), corePrice(d.SMA10))
		}
		frozen := ""
		if !d.FrozenAt.IsZero() {
			frozen = "frozen " + formatTimeShort(d.FrozenAt.Local())
		}
		if d.Drift {
			frozen = env.red("drift since " + formatTimeShort(d.FrozenAt.Local()))
		}
		measured := ""
		if d.SignalFrom != "" {
			measured = "measured on " + d.SignalFrom + "; sized on " + sym + "'s close " + corePrice(d.TradedClose)
		}
		riskReadLine(env, "Decision", d.Decision, trend, frozen, measured)
		binding := map[string]string{core.BindingFloor: "the 15% floor", core.BindingEWMA: "the EWMA", core.Binding252: "the 252-day"}[d.Binding]
		riskReadLine(env, "Volatility", corePct(d.Sigma)+" from "+binding, "EWMA "+corePct(d.SigmaEWMA), "252-day "+corePct(d.Sigma252))
	} else if d != nil && d.Reason != "" {
		riskReadLine(env, "Decision", d.Decision, d.Reason)
	}
	if s := res.Sizing; s != nil {
		on := s.CurrentOn
		sigma := ""
		if d := res.Decision; d != nil && d.Sigma > 0 {
			sigma = " ÷ " + corePct(d.Sigma) + " volatility"
		}
		riskReadLine(env, "Target", coreShares(s.Target)+" ("+mandateAmount(s.TargetBase, ccy)+")",
			fmt.Sprintf("%s%% of %s × room %s%s, at most %s%% (%s)", fmtPctNum(on.AllocationPct), mandateAmount(on.BudgetBase, ccy), mandatePct(on.Room*100), sigma, fmtPctNum(on.MaxValuePct), mandateAmount(s.Current.Cap, ccy)),
			"scale "+fmtPctNum(on.ScalePct)+"%")
		if s.Frozen.TargetShares != s.Current.TargetShares {
			riskReadLine(env, "", fmt.Sprintf("frozen at %s, today's capital gives %s; the lower stands", coreShares(s.Frozen.TargetShares), coreShares(s.Current.TargetShares)))
		}
	}
	if h := res.Holding; h != nil {
		price := "no price yet"
		if h.Price > 0 {
			price = "price " + corePrice(h.Price) + " " + map[string]string{"mark": "mark", "close": "last close"}[h.PriceSource]
		}
		riskReadLine(env, "Holding", coreShares(h.Shares)+" ("+mandateAmount(h.ValueBase, ccy)+")", price)
	}
	if a != nil && a.Kind != core.ActionHold && res.Policy != nil && res.Policy.BandPct != nil && res.Policy.MinOrderBase != nil {
		band := fmt.Sprintf("%s%% of the target (%.0f %s)", fmtPctNum(*res.Policy.BandPct), a.BandShares, pluralWord(int(math.Round(a.BandShares)), "share", "shares"))
		order := ""
		if a.OrderValue > 0 {
			order = "order " + mandateAmount(a.OrderValue, ccy)
		}
		riskReadLine(env, "Band", band, "minimum order "+mandateAmount(*res.Policy.MinOrderBase, ccy), order)
	}
	if m := res.Mandate; m != nil {
		line := []string{m.Status}
		if m.BudgetAnnualSD > 0 {
			line = append(line, fmt.Sprintf("risk %s a year of the budget %s", mandateAmount(m.RiskAnnualSD, nonEmpty(m.BaseCurrency, "currency unavailable")), mandateAmount(m.BudgetAnnualSD, nonEmpty(m.BaseCurrency, "currency unavailable"))))
		}
		if res.Sizing != nil && res.Sizing.Holdable != nil {
			line = append(line, fmt.Sprintf("headroom admits a holding of %s (95%% of the budget and of the gap cap)", coreShares(*res.Sizing.Holdable)))
		}
		riskReadLine(env, "Mandate", line...)
	}
	if k := res.Kill; k != nil && (k.MaxDrawdownK != nil || k.Measured) {
		line := []string{}
		if k.Measured {
			on := "the shadow record"
			if k.Source == rpc.KillSourceOwnFills {
				on = "its own fills"
				if isCore {
					on = "its holding"
				}
			}
			line = append(line, fmt.Sprintf("drawdown %.2f K on %s at %d %s", k.DrawdownK, on, k.Decisions, pluralWord(k.Decisions, "decision", "decisions")))
		}
		// K is the sleeve fully invested at its stage (its allocation over
		// the 15% floor, times the scale), so the bound reads the same at
		// every stage.
		stage := ""
		if k.ScalePct > 0 {
			stage = fmt.Sprintf(" of the sleeve at scale %g%%", k.ScalePct)
		}
		switch {
		case k.MaxDrawdownK != nil && k.Breached:
			line = append(line, env.red(fmt.Sprintf("past the bound %g K%s: buys stop; suspend it with stage = \"suspended\"", *k.MaxDrawdownK, stage)))
		case k.MaxDrawdownK != nil:
			line = append(line, fmt.Sprintf("bound %g K%s", *k.MaxDrawdownK, stage))
		default:
			line = append(line, "no bound written: cannot fire")
		}
		if k.Note != "" && k.MaxDrawdownK != nil {
			line = append(line, k.Note)
		}
		riskReadLine(env, "Kill rule", line...)
	}
	if res.NextDecision != "" {
		riskReadLine(env, "Next decision", res.NextDecision+" after the "+coreVenue(res.Instrument)+" close")
	}
	if d := res.Decision; d != nil && len(d.MonthEnds) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Month-ends"))
		parts := make([]string, len(d.MonthEnds))
		for i, m := range d.MonthEnds {
			parts[i] = m + " " + corePrice(d.MonthEndCloses[i])
		}
		riskDisplayLine(env, "  ", strings.Join(parts, " · "), nil)
	}
	renderCoreShadow(env, res.Shadow)
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Evidence"))
	if e := res.Evidence; e != nil {
		calendar := coreVenue(res.Instrument) + " calendar"
		switch {
		case e.Missing:
			riskWarnReadLine(env, "  "+e.SeriesKey, "no history yet", calendar)
		case e.Stale:
			riskWarnReadLine(env, "  "+e.SeriesKey, fmt.Sprintf("stale: %d closes to %s", e.Closes, e.LastClose), "read "+formatTimeShort(e.FetchedAt.Local()), calendar)
		default:
			riskReadLine(env, "  "+e.SeriesKey, fmt.Sprintf("%d closes to %s", e.Closes, e.LastClose), "read "+formatTimeShort(e.FetchedAt.Local()), calendar)
		}
		if e.ReadError != "" {
			riskWarnReadLine(env, "  Last read", "failed "+formatTimeShort(e.ReadErrorAt.Local()), e.ReadError)
		}
		if sg := e.Signal; sg != nil {
			line := fmt.Sprintf("%s %s in %s", nonEmpty(sg.Symbol, sg.ISIN), sg.ISIN, nonEmpty(sg.MeasuredIn, sg.Currency))
			use := fmt.Sprintf("decides while %s's own closes cover %d of the window's %d sessions", sym, sg.OwnSessions, core.WindowCloses)
			if !sg.InUse {
				use = fmt.Sprintf("not read: %s's own closes fill the window", sym)
			}
			parts := []string{line, use}
			if sg.Closes > 0 {
				parts = append(parts, fmt.Sprintf("%d closes to %s", sg.Closes, sg.LastClose))
			}
			if sg.FXKey != "" {
				parts = append(parts, "converted at "+sg.Currency+"."+sg.MeasuredIn)
			}
			riskReadLine(env, "  Signal", parts...)
		}
	}
	if m := res.Mandate; m != nil && !m.EvaluatedAt.IsZero() {
		riskReadLine(env, "  Mandate verdict", formatTimeShort(m.EvaluatedAt.Local()))
	}
	switch {
	case isCore && !res.ReadOnly:
		riskReadLine(env, "", "Live: the core serves its month's row among the proposals, sent on your confirmation on your device or by Desk's controller under the standing mandate you confirmed there; a row served while the core holds goes to you alone, and the core is never pre-authorised. It is a beta allocation with a drawdown filter that you chose, not an edge claim.")
	case isCore:
		riskReadLine(env, "", "Read-only: the core proposes nothing and places no order while it is not live. It is a beta allocation with a drawdown filter that you chose, not an edge claim.")
	case !res.ReadOnly:
		riskReadLine(env, "", "Live: "+name+" serves its month's row among the proposals, sent on your confirmation on your device or by Desk's controller under the standing mandate you confirmed there; it is never pre-authorised. It is a rule you chose, not an edge claim.")
	default:
		riskReadLine(env, "", "Read-only: "+name+" proposes nothing and places no order while it is not live.")
	}
}

// runStrategy reads the daemon's target for one registered strategy by its
// id in the strategy register (internal-docs/design/strategy-evaluator.md),
// in the core's shape, with a live entry's row and last decision. The daemon
// evaluates; this renders, and sends nothing.
func runStrategy(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "strategy")
	jsonOut := fs.Bool("json", false, "emit the typed strategy target")
	refresh := fs.Bool("refresh", false, "measure the mandate again before deciding")
	replay := fs.Bool("replay", false, "replay the rule over the month-ends FROM TO (YYYY-MM-DD) on the daemon's own history, in the lab's table shape; read-only")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *replay {
		if fs.NArg() != 3 {
			fmt.Fprintln(env.Stderr, "canary strategy --replay: a strategy ID, FROM and TO (YYYY-MM-DD) are required")
			fs.Usage()
			return 2
		}
		res, err := rpc.FetchStrategyReplay(ctx, env.Conn, fs.Arg(0), fs.Arg(1), fs.Arg(2))
		if err != nil {
			return fail(env, "strategy: %v", err)
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		renderStrategyReplay(env, res)
		return 0
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.Stderr, "canary strategy: exactly one strategy ID is required")
		fs.Usage()
		return 2
	}
	res, err := rpc.FetchStrategyTarget(ctx, env.Conn, fs.Arg(0), *refresh)
	if err != nil {
		return fail(env, "strategy: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderCore(env, res)
	return 0
}

// renderCoreRow is the core's pending row as the proposal engine serves
// it: the order, the session it is for, its state and what blocks it.
func renderCoreRow(env *Env, r *rpc.CoreRowView, sym string) {
	if r == nil {
		return
	}
	order := fmt.Sprintf("%s %s %s", strings.ToLower(r.Action), coreShares(r.Quantity), sym)
	terms := fmt.Sprintf("%s %s at %s", r.Terms.OrderType, r.Terms.TIF, r.Terms.Limit)
	session := "for the " + r.Session + " session"
	if n := len(r.Terms.Window); n > 0 {
		session += fmt.Sprintf(" (window %s to %s)", r.Terms.Window[0], r.Terms.Window[n-1])
	}
	state := r.State
	if r.Readiness != "" {
		state += ", " + strings.ReplaceAll(r.Readiness, "_", " ")
	}
	if r.State == rpc.TradeProposalStateBlocked {
		why := make([]string, 0, len(r.Blockers))
		for _, b := range r.Blockers {
			why = append(why, b.Message)
		}
		riskWarnReadLine(env, "Row", state, order, terms, session, strings.Join(why, "; "))
		return
	}
	riskReadLine(env, "Row", order, terms, session, state, "key "+r.Key)
}

// renderCoreLastDecision is the last month the core decided while live and
// what became of its row.
func renderCoreLastDecision(env *Env, d *rpc.CoreDecisionRecord) {
	if d == nil {
		return
	}
	what := d.Why
	if d.Quantity > 0 {
		what = briefJoin(strings.ReplaceAll(d.Kind, "_", " ")+" "+coreShares(d.Quantity), d.Why)
	}
	parts := []string{d.Decision, what, "decided " + formatTimeShort(d.DecidedAt.Local())}
	o := d.Outcome
	switch {
	case o.Submitted:
		parts = append(parts, "sent "+formatTimeShort(o.SubmittedAt.Local())+" after your confirmation (order "+o.OrderRef+")")
	case o.Refused != "":
		parts = append(parts, "last refused "+formatTimeShort(o.RefusedAt.Local())+": "+o.Refused)
	case o.Served:
		parts = append(parts, "row served, not sent")
	default:
		parts = append(parts, "no row")
	}
	riskReadLine(env, "Last decision", parts...)
}

// coreContractLine is the contract the core reads and would trade, as
// IBKR's record for the ISIN names it, and the calendar it is judged on;
// before IBKR has answered, the fund the policy names and that it is not
// resolved.
func coreContractLine(in rpc.CoreInstrument) []string {
	if !in.Resolved {
		line := []string{"not resolved"}
		if in.ConID > 0 {
			line = append(line, fmt.Sprintf("con_id %d written in [mandate.core]", in.ConID))
		}
		return line
	}
	name := in.Symbol
	if in.TradingClass != "" && in.TradingClass != in.Symbol {
		name += " (class " + in.TradingClass + ")"
	}
	return []string{fmt.Sprintf("%d %s on %s, %s", in.ConID, name, in.Exchange, in.Currency), nonEmpty(in.CalendarLabel, in.Calendar) + " calendar", "from IBKR's record for the ISIN"}
}

// coreVenue names the core's exchange calendar for a line: its label once
// the contract is resolved, else "exchange".
func coreVenue(in *rpc.CoreInstrument) string {
	if in == nil || !in.Resolved {
		return "exchange"
	}
	return nonEmpty(in.CalendarLabel, nonEmpty(in.Calendar, in.Exchange))
}

// fmtPctNum prints a percentage number without a trailing zero fraction.
func fmtPctNum(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%g", v)
}

// runCoreResolve lists every listing IBKR knows for the core's ISIN (the
// flag's, else the one [mandate.core] names, else the compiled rule's) and
// marks the one the core uses, so the owner can choose among several by
// writing its contract id. Read-only: it never chooses a listing and never
// writes the policy.
func runCoreResolve(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "core resolve")
	jsonOut := fs.Bool("json", false, "emit the typed listings")
	isin := fs.String("isin", "", "the ISIN to resolve (default: [mandate.core] isin, else the rule's)")
	currency := fs.String("currency", "", "ask only for listings in this currency")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	res, err := rpc.FetchCoreResolve(ctx, env.Conn, rpc.CoreResolveParams{ISIN: *isin, Currency: *currency})
	if err != nil {
		return fail(env, "core resolve: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderCoreResolve(env, res)
	return 0
}

func renderCoreResolve(env *Env, res rpc.CoreResolveResult) {
	source := map[string]string{"flag": "asked", "policy": "from [mandate.core]", "rule": "the rule's instrument"}[res.ISINSource]
	headline, tone := fmt.Sprintf("%d %s", len(res.Listings), pluralWord(len(res.Listings), "LISTING", "LISTINGS")), "green"
	switch {
	case len(res.Listings) == 0:
		headline, tone = "NO LISTING", "red"
	case len(res.Listings) > 1 && !res.PinnedFound:
		tone = "yellow"
	}
	riskDisplayLine(env, env.bold("Core resolve")+"  ", headline+" for "+res.ISIN, func(s string) string { return riskTone(env, tone, env.bold(s)) })
	riskReadLine(env, "ISIN", res.ISIN, source, nonEmpty(res.Currency, "every currency"))
	for _, l := range res.Listings {
		parts := []string{fmt.Sprintf("con_id %d", l.ConID), l.Symbol}
		if l.LocalSymbol != "" && l.LocalSymbol != l.Symbol {
			parts[1] += " (local " + l.LocalSymbol + ")"
		}
		venue := nonEmpty(l.Exchange, "no exchange")
		if l.PrimaryExch != "" {
			venue += ", primary " + l.PrimaryExch
		}
		parts = append(parts, venue, l.Currency)
		if l.TradingClass != "" {
			parts = append(parts, "class "+l.TradingClass)
		}
		parts = append(parts, l.LongName)
		switch {
		case l.ISIN == "":
			parts = append(parts, "ISIN not named by IBKR")
		case l.ISIN != res.ISIN:
			parts = append(parts, "ISIN "+l.ISIN)
		}
		parts = append(parts, "found by "+strings.Join(l.Forms, ", "))
		style := func(s string) string { return s }
		if l.Pinned {
			parts = append(parts, "PINNED")
			style = env.green
		}
		if l.Used {
			parts = append(parts, "THE CORE USES IT")
			style = env.green
		}
		riskDisplayLine(env, "  ", briefJoin(parts...), style)
	}
	pinned := "not written"
	if res.PinnedConID != nil {
		pinned = fmt.Sprintf("%d", *res.PinnedConID)
		if !res.PinnedFound {
			pinned += ", not among these listings"
		}
	}
	riskReadLine(env, "Pinned", "[mandate.core] con_id "+pinned)
	riskReadLine(env, "Next", res.Message)
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Asked"))
	for _, a := range res.Attempts {
		answer := a.Outcome
		switch a.Outcome {
		case "line":
			answer = fmt.Sprintf("%d %s", a.Lines, pluralWord(a.Lines, "listing", "listings"))
		case "rejected":
			answer = fmt.Sprintf("IBKR %d \"%s\"", a.Code, a.Message)
		case "no_line":
			answer = nonEmpty(a.Message, "no line")
		case "failed":
			answer = "failed: " + a.Message
		}
		riskReadLine(env, "  "+a.Form, answer)
	}
	riskReadLine(env, "", "Read-only: nothing is chosen for you and the policy is not written.")
}

// renderCoreShadow lists the core's shadow record
// (internal-docs/design/strategy-register.md, section 3): the paper position
// its production code holds and the newest records. Nothing in it is sent.
func renderCoreShadow(env *Env, v *rpc.ShadowView) {
	if v == nil {
		return
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Shadow")+"  the rule on a paper position; nothing is sent")
	if v.Unavailable != "" {
		riskWarnReadLine(env, "  Record", "unavailable", v.Unavailable)
		return
	}
	riskReadLine(env, "  Paper position", coreShares(v.Holding), fmt.Sprintf("%d %s", v.Records, pluralWord(v.Records, "record", "records")))
	for _, r := range v.Recent {
		switch {
		case r.Kind == rpc.ShadowKindDecision && r.Decision != nil:
			trend := "trend off"
			if r.Decision.TrendOn {
				trend = "trend on"
			}
			riskReadLine(env, "  "+r.Session, "decision", trend, "target "+coreShares(r.Decision.TargetShares)+" at scale "+fmtPctNum(r.Decision.ScalePct)+"%")
		case r.Kind == rpc.ShadowKindIntent && r.Intent != nil:
			what := strings.ReplaceAll(r.Intent.Kind, "_", " ")
			if q := r.Intent.Quantity; q != 0 {
				what += " " + coreShares(int(math.Abs(float64(q))))
			}
			riskReadLine(env, "  "+r.Session, "intent for "+r.Intent.ForSession, what, r.Intent.Why)
		case r.Kind == rpc.ShadowKindFill && r.Fill != nil && r.Fill.Unobserved != "":
			riskWarnReadLine(env, "  "+r.Session, "fill unobserved", r.Fill.Unobserved)
		case r.Kind == rpc.ShadowKindFill && r.Fill != nil:
			side := "bought"
			if r.Fill.Quantity < 0 {
				side = "sold"
			}
			riskReadLine(env, "  "+r.Session, "fill", side+" "+coreShares(int(math.Abs(float64(r.Fill.Quantity))))+" at "+corePrice(r.Fill.Price)+" (the close)", "paper position "+coreShares(r.Fill.HoldingAfter))
		case r.Kind == rpc.ShadowKindHold:
			riskWarnReadLine(env, "  "+r.Session, "held", r.Reasons...)
		}
	}
}

// renderStrategyReplay renders a replay (strategy-evaluator.md §9): what
// the rule was run on, the fixed paper sizing, and one line per month-end
// with the lab's columns, so the eye can hold the two tables side by side
// before `lab replay-compare` does it exactly.
func renderStrategyReplay(env *Env, res rpc.StrategyReplayResult) {
	heading := "Core"
	if res.Strategy != "" && res.Strategy != "core" {
		heading = res.Strategy
	}
	decided, orders := 0, 0
	for _, r := range res.Rows {
		if r.Status == core.StatusOK {
			decided++
		}
		switch r.Action {
		case core.ActionBuy, core.ActionSell, core.ActionSellAll:
			orders++
		}
	}
	riskDisplayLine(env, env.bold(heading+" replay")+"  ", fmt.Sprintf("%s to %s: %d month-ends, %d decided, %d paper orders", res.From, res.To, len(res.Rows), decided, orders), env.bold)
	spec := res.SpecHash
	if len(spec) > 12 {
		spec = spec[:12]
	}
	riskReadLine(env, "Rule", res.Rule, "signal "+res.Signal, "spec "+nonEmpty(spec, "(none)"))
	if in := res.Instrument; in != nil {
		riskReadLine(env, "Line", coreContractLine(*in)...)
	}
	series := fmt.Sprintf("%s closes %s to %s", groupThousands(fmt.Sprint(len(res.Dates))), res.SeriesFrom, res.SeriesTo)
	riskReadLine(env, "Series", series, nonEmpty(res.Source, "source unnamed"), "sessions on the "+nonEmpty(res.CalendarLabel, res.Calendar)+" calendar")
	if n := len(res.OffCalendar); n > 0 {
		riskReadLine(env, "", fmt.Sprintf("%d %s on days the exchange was closed, skipped: %s", n, pluralWord(n, "bar", "bars"), strings.Join(res.OffCalendar, ", ")))
	}
	s := res.Sizing
	riskReadLine(env, "Paper", fmt.Sprintf("%s%% of %s × room %s, at most %s%%, scale %s%%", fmtPctNum(s.AllocationPct), groupThousands(fmt.Sprintf("%.0f", s.BudgetBase)), fmtPctNum(s.Room), fmtPctNum(s.MaxValuePct), fmtPctNum(s.ScalePct)),
		fmt.Sprintf("band %s%%, minimum %s", fmtPctNum(res.Terms.BandPct), groupThousands(fmt.Sprintf("%.0f", res.Terms.MinOrderBase))), "the holding carried from zero; nothing frozen, recorded or sent")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "%-11s %9s %9s %-5s %6s %7s %-14s %7s\n", "Decision", "Close", "SMA10", "Trend", "Sigma", "Target", "Action", "Held")
	for _, r := range res.Rows {
		if r.Status != core.StatusOK {
			fmt.Fprintf(env.Stdout, "%-11s %s: %s\n", r.Decision, r.Status, r.Reason)
			continue
		}
		trend := "off"
		if r.TrendOn {
			trend = "on"
		}
		action := r.Action
		switch r.Action {
		case core.ActionBuy, core.ActionSell:
			action = fmt.Sprintf("%s %d", r.Action, r.Quantity)
		case core.ActionSellAll:
			action = fmt.Sprintf("sell all %d", r.Quantity)
		}
		fmt.Fprintf(env.Stdout, "%-11s %9.2f %9.2f %-5s %5.1f%% %7d %-14s %7d\n", r.Decision, r.Close, r.SMA10, trend, r.Sigma*100, r.TargetShares, action, r.HeldAfter)
	}
}
