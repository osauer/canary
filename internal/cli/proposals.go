package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runProposals(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	subIdx := proposalsSubcommandIndex(args)
	if subIdx < 0 {
		if len(args) == 1 && helpArg(args[0]) {
			return printCommandUsage(env, "proposals")
		}
		subIdx = 0
	}
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "status":
		return runProposalsStatus(ctx, env, args)
	case "refresh":
		return runProposalsRefresh(ctx, env, args)
	case "list":
		return runProposalsList(ctx, env, args)
	case "preview":
		return runProposalsPreview(ctx, env, args)
	case "submit":
		return runProposalsSubmit(ctx, env, args)
	case "reduce":
		return runProposalsReduce(ctx, env, args)
	case "request-stop":
		return runProposalsRequestStop(ctx, env, args)
	case "ignore":
		return runProposalsIgnore(ctx, env, args)
	default:
		return fail(env, "proposals: unknown subcommand %q", sub)
	}
}

func proposalsSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "status", "refresh", "list", "preview", "submit", "reduce", "request-stop", "ignore":
			return i
		}
	}
	return -1
}

func runProposalsStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.AutoTradeStatus
	if err := env.Conn.Call(ctx, rpc.MethodAutoTradeStatus, nil, &res); err != nil {
		return fail(env, "proposals status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalStatusText(env, &res)
	return 0
}

func runProposalsRefresh(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals refresh")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.TradeProposalSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsRefresh, rpc.TradeProposalRefreshParams{Show: true}, &res); err != nil {
		return fail(env, "proposals refresh: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalsText(env, &res)
	return 0
}

func runProposalsList(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals list")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.TradeProposalSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsSnapshot, rpc.TradeProposalSnapshotParams{Show: true}, &res); err != nil {
		return fail(env, "proposals list: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalsText(env, &res)
	return 0
}

func runProposalsPreview(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals preview")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to proposal quantity")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	fastPath := fs.Bool("fast-path", false, "use current snapshot for supported fast previews such as trailing stops")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals preview: usage is `canary proposals preview KEY REVISION`")
	}
	var res rpc.TradeProposalPreviewResult
	params := rpc.TradeProposalPreviewParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, TimeoutMs: int(timeout.Milliseconds()), FastPath: *fastPath}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPreview, params, &res); err != nil {
		return fail(env, "proposals preview: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalPreviewText(env, &res)
	return 0
}

func runProposalsSubmit(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals submit")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to proposal quantity")
	fastPath := fs.Bool("fast-path", true, "perform one-confirm preview+submit")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals submit: usage is `canary proposals submit KEY REVISION`")
	}
	var res rpc.TradeProposalSubmitResult
	params := rpc.TradeProposalSubmitParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, FastPath: *fastPath, TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsSubmit, params, &res); err != nil {
		return fail(env, "proposals submit: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalSubmitText(env, &res)
	return 0
}

// runProposalsReduce is a discretionary partial close of a held position by a
// chosen percentage. It previews by default and only places with --submit, so a
// bare invocation is always safe to run. The holding is identified by --con-id
// (preferred; required for option legs) or a unique stock SYMBOL.
//
// --portfolio trims net delta-adjusted portfolio risk by the chosen
// percentage: percent is the share of NET portfolio delta to remove, not a
// flat per-position cut. Hedges (positions whose delta is opposite-signed to
// net exposure) are never selected — there is no opt-out, because trimming
// one would increase net risk rather than reduce it. --include-hedges only
// applies to the single-holding path, so it is a hard error alongside
// --portfolio rather than a silent no-op.
func runProposalsReduce(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals reduce")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	percent := fs.Int("percent", 0, "percentage to reduce: 25, 50, 75, or 100 (in --portfolio mode: share of net portfolio delta to remove, not a flat per-position cut)")
	conID := fs.Int("con-id", 0, "contract ID of the holding (preferred; required for option legs)")
	includeHedges := fs.Bool("include-hedges", false, "allow trimming a protective short (hedge) such as long index puts")
	portfolio := fs.Bool("portfolio", false, "trim net portfolio delta-adjusted risk (risk-off sweep) instead of one holding")
	submit := fs.Bool("submit", false, "place the order; without this flag the command only previews")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *percent <= 0 || *percent > 100 {
		return fail(env, "proposals reduce: --percent must be between 1 and 100 (the app offers 25/50/75/100)")
	}
	if *portfolio {
		includeHedgesSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "include-hedges" {
				includeHedgesSet = true
			}
		})
		if includeHedgesSet {
			return fail(env, "proposals reduce --portfolio excludes hedges structurally (opposite-sign-to-net positions are never selected); --include-hedges has no effect here, drop it")
		}
		if fs.NArg() > 0 || *conID > 0 {
			return fail(env, "proposals reduce --portfolio sweeps the whole book; do not pass SYMBOL or --con-id")
		}
		pparams := rpc.TradeProposalReducePortfolioParams{Percent: *percent, TimeoutMs: int(timeout.Milliseconds())}
		method := rpc.MethodTradeProposalsReducePortfolioPreview
		if *submit {
			method = rpc.MethodTradeProposalsReducePortfolioSubmit
			pparams.Origin = env.Origin
		}
		var pres rpc.TradeProposalReducePortfolioResult
		if err := env.Conn.Call(ctx, method, pparams, &pres); err != nil {
			return fail(env, "proposals reduce: %v", err)
		}
		if *jsonOut {
			return printJSON(env, pres)
		}
		renderProposalReducePortfolioText(env, &pres, *submit)
		return 0
	}
	symbol := ""
	switch fs.NArg() {
	case 0:
	case 1:
		symbol = strings.TrimSpace(fs.Arg(0))
	default:
		return fail(env, "proposals reduce: usage is `canary proposals reduce [SYMBOL] --percent N [--con-id ID] [--include-hedges] [--submit]`")
	}
	if *conID <= 0 && symbol == "" {
		return fail(env, "proposals reduce: provide a SYMBOL or --con-id (or --portfolio for the whole book)")
	}
	params := rpc.TradeProposalReduceParams{
		ConID:         *conID,
		Symbol:        symbol,
		Percent:       *percent,
		IncludeHedges: *includeHedges,
		TimeoutMs:     int(timeout.Milliseconds()),
	}
	method := rpc.MethodTradeProposalsReducePreview
	if *submit {
		method = rpc.MethodTradeProposalsReduceSubmit
		params.Origin = env.Origin
	}
	var res rpc.TradeProposalReduceResult
	if err := env.Conn.Call(ctx, method, params, &res); err != nil {
		return fail(env, "proposals reduce: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalReduceText(env, &res, *submit)
	return 0
}

// runProposalsRequestStop asks the daemon to generate a protective
// trailing-stop proposal for one named stock/ETF holding now. Generation
// only: it never places an order. The printed KEY/REVISION feed the ordinary
// `canary proposals preview` and `canary proposals submit` gates.
func runProposalsRequestStop(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals request-stop")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	conID := fs.Int("con-id", 0, "contract ID of the holding (preferred when a symbol is ambiguous)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	symbol := ""
	switch fs.NArg() {
	case 0:
	case 1:
		symbol = strings.TrimSpace(fs.Arg(0))
	default:
		return fail(env, "proposals request-stop: usage is `canary proposals request-stop [SYMBOL] [--con-id ID]`")
	}
	if *conID <= 0 && symbol == "" {
		return fail(env, "proposals request-stop: provide a SYMBOL or --con-id")
	}
	params := rpc.TradeProposalRequestStopParams{ConID: *conID, Symbol: symbol, Show: true}
	var res rpc.TradeProposalRequestStopResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsRequestStop, params, &res); err != nil {
		return fail(env, "proposals request-stop: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalRequestStopText(env, &res)
	return 0
}

func renderProposalRequestStopText(env *Env, res *rpc.TradeProposalRequestStopResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Request Stop  accepted=%v\n", res.Accepted)
	holding := res.Symbol
	if res.ConID > 0 {
		holding = fmt.Sprintf("%s (conID %d)", res.Symbol, res.ConID)
	}
	statusRow(env, out, "Holding", strings.TrimSpace(holding))
	if res.ProposalKey != "" {
		statusRow(env, out, "Proposal", res.ProposalKey)
		statusRow(env, out, "Revision", res.Revision)
	}
	if res.IgnoreCleared {
		statusRow(env, out, "Ignore", "prior advisory ignore cleared by this request")
	}
	if p := res.Proposal; p != nil {
		head := fmt.Sprintf("%s %d %s", p.Action, p.Quantity, p.Symbol)
		if p.OrderType != "" {
			head += "  " + p.OrderType
		}
		if p.Trail != nil {
			head += " " + formatOrderTrail(p.Trail)
		}
		statusRow(env, out, "Order", head)
		if sizing := formatProposalTrailSizing(p.TrailSizing); sizing != "" {
			statusRow(env, out, "Trail sizing", sizing)
		}
		renderProposalRiskTicket(env, out, p)
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	if res.Accepted && res.ProposalKey != "" {
		statusRow(env, out, "Next", fmt.Sprintf("canary proposals preview %s %s", res.ProposalKey, res.Revision))
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

func renderProposalReduceText(env *Env, res *rpc.TradeProposalReduceResult, submitted bool) {
	out := env.Stdout
	fmt.Fprintln(out)
	title := "Reduce Preview"
	if submitted {
		title = "Reduce Submit"
	}
	fmt.Fprintf(out, "%s  accepted=%v submit_eligible=%v\n", title, res.Accepted, res.SubmitEligible)
	holding := res.Symbol
	if res.ConID > 0 {
		holding = fmt.Sprintf("%s (conID %d)", res.Symbol, res.ConID)
	}
	statusRow(env, out, "Holding", strings.TrimSpace(holding))
	if res.ReduceQuantity > 0 {
		posQty := res.PositionQuantity
		if posQty < 0 {
			posQty = -posQty
		}
		statusRow(env, out, "Reduce", fmt.Sprintf("%s %d of %g %s (%d%%)", res.Action, res.ReduceQuantity, posQty, positionUnit(res.SecType), res.Percent))
	}
	if res.HedgeLike {
		statusRow(env, out, "Hedge", "holding is a protective short")
	}
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	renderProposalOrderPreview(env, out, res.Preview)
	if submitted && res.Place != nil {
		statusRow(env, out, "Order ref", res.OrderRef)
		statusRow(env, out, "Broker ID", strconv.Itoa(res.Place.ReservedOrderID))
		statusRow(env, out, "Lifecycle", nonEmpty(res.Place.LifecycleStatus, res.Place.SendState))
		if res.Place.Status != "" {
			statusRow(env, out, "Status", res.Place.Status)
		}
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

func renderProposalReducePortfolioText(env *Env, res *rpc.TradeProposalReducePortfolioResult, submitted bool) {
	out := env.Stdout
	fmt.Fprintln(out)
	title := "Portfolio Reduce Preview"
	if submitted {
		title = "Portfolio Reduce Submit"
	}
	fmt.Fprintf(out, "%s  accepted=%v\n", title, res.Accepted)
	statusRow(env, out, "Percent", fmt.Sprintf("%d%% of net delta", res.Percent))
	verb := "eligible"
	if submitted {
		verb = "placed"
	}
	statusRow(env, out, "Legs", fmt.Sprintf("%d %s · %d blocked of %d", res.EligibleCount, verb, res.BlockedCount, res.LegCount))
	if res.TargetDollarDelta != 0 {
		statusRow(env, out, "Net delta (before)", formatProposalMoney(res.NetDollarDeltaBefore, res.BaseCurrency))
		statusRow(env, out, "Target removed", formatProposalMoney(res.TargetDollarDelta, res.BaseCurrency))
		achieved := formatProposalMoney(res.AchievedDollarDelta, res.BaseCurrency)
		if res.AchievedPctOfTarget != nil {
			achieved += fmt.Sprintf(" (%.0f%% of target)", *res.AchievedPctOfTarget)
		}
		statusRow(env, out, "Achieved", achieved)
	}
	if res.NetDeltaIncomplete {
		statusRow(env, out, "Note", "net delta is a partial-book estimate; some positions had no computable delta")
	}
	if res.TotalNotional != 0 || res.FXIncomplete {
		total := formatProposalMoney(res.TotalNotional, res.BaseCurrency)
		if res.FXIncomplete {
			total += " [fx incomplete]"
		}
		statusRow(env, out, "Total notional", total)
	}
	if res.Replayed {
		statusRow(env, out, "Replayed", "true (idempotent retry; nothing placed)")
	}
	printTradingBlockers(out, "  ", res.Blockers)
	for _, leg := range res.Legs {
		state := "eligible"
		switch {
		case submitted && leg.Placed:
			state = "placed"
		case submitted || !leg.SubmitEligible:
			state = "blocked"
		}
		head := strings.TrimSpace(fmt.Sprintf("%s %d %s", leg.Action, leg.ReduceQuantity, leg.Symbol))
		if leg.Action == "" {
			head = leg.Symbol
		}
		fmt.Fprintf(out, "  %s  [%s]\n", head, state)
		if leg.Notional != 0 {
			statusRow(env, out, "  notional", formatProposalMoney(leg.Notional, leg.NotionalCurrency))
		}
		if leg.OrderRef != "" {
			statusRow(env, out, "  order ref", leg.OrderRef)
		}
		printTradingBlockers(out, "      ", leg.Blockers)
	}
	fmt.Fprintln(out)
}

func runProposalsIgnore(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals ignore")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	reason := fs.String("reason", "", "ignore reason")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fail(env, "proposals ignore: usage is `canary proposals ignore KEY [REVISION]`")
	}
	params := rpc.TradeProposalIgnoreParams{Key: fs.Arg(0), Reason: strings.TrimSpace(*reason)}
	if fs.NArg() == 2 {
		params.Revision = fs.Arg(1)
	}
	var res rpc.TradeProposalIgnoreResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsIgnore, params, &res); err != nil {
		return fail(env, "proposals ignore: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintf(env.Stdout, "Ignored %s (%s)\n", res.Key, res.Message)
	return 0
}

func renderProposalStatusText(env *Env, st *rpc.AutoTradeStatus) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Protection Proposals  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(nonEmpty(st.Policy.Status, "unknown")), Level: statusConcernNotice}))
	statusRow(env, out, "Proposals", fmt.Sprint(st.ProposalsEnabled))
	statusRow(env, out, "Fast path", fmt.Sprint(st.FastPathEnabled))
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d %s", st.Policy.PolicyID, st.Policy.PolicyVersion, st.Policy.Fingerprint.Key))
	if len(st.Blockers) > 0 {
		fmt.Fprintln(out, "Blockers:")
		printTradingBlockers(out, "  ", st.Blockers)
	}
	fmt.Fprintln(out)
}

// printTradingBlockers renders blockers with their remediation action: under
// market stress the action text is the difference between a dead end and the
// next command to run.
func printTradingBlockers(out io.Writer, indent string, blockers []rpc.TradingBlocker) {
	for _, b := range blockers {
		fmt.Fprintf(out, "%s- %s: %s\n", indent, b.Code, b.Message)
		if b.Action != "" {
			fmt.Fprintf(out, "%s  action: %s\n", indent, b.Action)
		}
	}
}

func renderProposalsText(env *Env, snap *rpc.TradeProposalSnapshot) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Protection Proposals  %d actionable / %d total\n", snap.Counts.Actionable, snap.Counts.Total)
	statusRow(env, out, "Revision", snap.Revision)
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d", snap.PolicyID, snap.PolicyVersion))
	statusRow(env, out, "Theta/day", fmt.Sprintf("%.2f", snap.Counts.ThetaPerDay))
	printTradingBlockers(out, "  ", snap.Blockers)
	for _, p := range snap.Proposals {
		state := "ready"
		if len(p.Blockers) > 0 {
			state = "blocked"
		}
		head := fmt.Sprintf("%s  %s  %s %d %s", p.Key, p.Bucket, p.Action, p.Quantity, p.Symbol)
		if p.OrderType != "" {
			head += "  " + p.OrderType
		}
		if p.Trail != nil {
			head += " " + formatOrderTrail(p.Trail)
		}
		fmt.Fprintf(out, "  %s  %s  [%s]\n", head, p.Reason, state)
		if posLine := formatProposalPositionLine(env, &p); posLine != "" {
			fmt.Fprintf(out, "      Position   %s\n", posLine)
		}
		if sizing := formatProposalTrailSizing(p.TrailSizing); sizing != "" {
			fmt.Fprintf(out, "      Trail sizing: %s\n", sizing)
		}
		renderProposalRiskTicket(env, out, &p)
		for _, d := range p.Details {
			fmt.Fprintf(out, "      %s\n", d)
		}
		printTradingBlockers(out, "      ", p.Blockers)
	}
	fmt.Fprintln(out)
}

func formatProposalTrailSizing(sizing *rpc.TradeProposalTrailSizing) string {
	if sizing == nil {
		return ""
	}
	chosen := sizing.ChosenPct
	if chosen <= 0 {
		return ""
	}
	rangeText := ""
	if sizing.PolicyMinPct > 0 && sizing.PolicyMaxPct > 0 {
		rangeText = fmt.Sprintf("dynamic range %.1f-%.1f%%, ", sizing.PolicyMinPct, sizing.PolicyMaxPct)
	}
	if sizing.Fallback {
		fallback := sizing.PolicyFallbackPct
		if fallback <= 0 {
			fallback = chosen
		}
		return fmt.Sprintf("%s%.1f%% fallback trail used (dynamic stop unavailable; %.1f%% policy fallback)", rangeText, chosen, fallback)
	}
	source := strings.TrimSpace(sizing.SelectedBy)
	if source == "" {
		source = "policy"
	}
	suffix := ""
	if sizing.Capped && sizing.PolicyMaxPct > 0 {
		suffix = fmt.Sprintf(", capped at %.1f%%", sizing.PolicyMaxPct)
	}
	return fmt.Sprintf("%schosen %.1f%% by %s%s", rangeText, chosen, source, suffix)
}

func renderProposalPreviewText(env *Env, res *rpc.TradeProposalPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Preview  accepted=%v submit_eligible=%v\n", res.Accepted, res.SubmitEligible)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	renderProposalOrderPreview(env, out, res.Preview)
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

// renderProposalOrderPreview shows what the user is one submit away from
// placing: the bound draft (incl. trail spec), live quote, position impact,
// and the broker WhatIf verdict with its failure detail when present.
func renderProposalOrderPreview(env *Env, out io.Writer, p *rpc.TradeProposalOrderPreview) {
	if p == nil {
		return
	}
	statusRow(env, out, "Draft", formatOrderDraftSummary(p.Draft))
	statusRow(env, out, "Notional", fmt.Sprintf("%.2f", p.Notional))
	statusRow(env, out, "Position", fmt.Sprintf("%.4g -> %.4g (%s)", p.Position.Before, p.Position.After, p.Position.Effect))
	statusRow(env, out, "Quote", formatOrderPreviewQuote(p.Quote))
	renderProtectionRiskFields(env, out, p.Draft.Trail, p.Draft.TIF, p.Draft.Contract.Currency, p.ExecutionSemantics, p.StopRisk, nil)
	statusRow(env, out, "WhatIf", fmt.Sprintf("%s (required=%v)", p.WhatIf.Status, p.WhatIf.RequiredForSubmit))
	if p.WhatIf.Message != "" {
		statusRow(env, out, "WhatIf detail", p.WhatIf.Message)
	}
	if p.WhatIf.Action != "" {
		statusRow(env, out, "WhatIf action", p.WhatIf.Action)
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintln(out, "Warnings:")
		for _, w := range p.Warnings {
			fmt.Fprintf(out, "  - %s: %s\n", w.Code, w.Message)
			if w.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", w.Action)
			}
		}
	}
}

func renderProposalRiskTicket(env *Env, out io.Writer, p *rpc.TradeProposal) {
	if p == nil {
		return
	}
	renderProtectionRiskFields(env, out, p.Trail, p.TIF, p.Contract.Currency, p.ExecutionSemantics, p.StopRisk, p.StopLadder)
}

func renderProtectionRiskFields(env *Env, out io.Writer, trail *rpc.OrderTrailSpec, tif, currency string, sem *rpc.TradeProposalExecutionSemantics, risk *rpc.TradeProposalStopRisk, ladder []rpc.TradeProposalStopLadderStep) {
	ticket := proposalRiskTicketParts(trail, tif, currency, sem)
	if len(ticket) > 0 {
		statusRow(env, out, "Risk ticket", strings.Join(ticket, " | "))
	}
	if riskLine := formatProposalStopRisk(risk, currency); riskLine != "" {
		statusRow(env, out, "Estimated loss", riskLine)
	}
	if gapLine := formatProposalGapScenario(risk, currency); gapLine != "" {
		statusRow(env, out, "Gap scenario", gapLine)
	}
	if ladderLine := formatProposalStopLadder(ladder, currency); ladderLine != "" {
		statusRow(env, out, "Stop ladder", ladderLine)
	}
	for _, warning := range proposalRiskWarnings(sem, risk) {
		statusRow(env, out, "Warning", warning)
	}
}

func proposalRiskTicketParts(trail *rpc.OrderTrailSpec, tif, currency string, sem *rpc.TradeProposalExecutionSemantics) []string {
	var parts []string
	if trail != nil && trail.InitialStopPrice > 0 {
		parts = append(parts, "stop "+formatProposalMoney(trail.InitialStopPrice, currency))
	}
	if offset := formatProposalTrailOffset(trail, currency); offset != "" {
		parts = append(parts, "offset "+offset)
	}
	if tif = strings.TrimSpace(tif); tif != "" {
		parts = append(parts, "TIF "+strings.ToUpper(tif))
	}
	if trigger := formatProposalExecutionTrigger(sem); trigger != "" {
		parts = append(parts, trigger)
	}
	return parts
}

func formatProposalTrailOffset(trail *rpc.OrderTrailSpec, currency string) string {
	if trail == nil {
		return ""
	}
	if trail.TrailingAmount != nil {
		return formatProposalMoney(*trail.TrailingAmount, currency)
	}
	if trail.TrailingPercent != nil {
		return fmt.Sprintf("%.2f%%", *trail.TrailingPercent)
	}
	return ""
}

func formatProposalExecutionTrigger(sem *rpc.TradeProposalExecutionSemantics) string {
	if sem == nil {
		return ""
	}
	label := strings.TrimSpace(sem.TriggerMethodLabel)
	if label == "" {
		label = formatOrderTriggerMethod(sem.TriggerMethod)
	}
	if label == "" {
		return ""
	}
	if sem.ReferenceSide != "" && sem.ReferencePrice != nil {
		return fmt.Sprintf("trigger %s on %s %s", label, sem.ReferenceSide, formatProposalMoney(*sem.ReferencePrice, ""))
	}
	if sem.ReferenceSide != "" {
		return fmt.Sprintf("trigger %s on %s", label, sem.ReferenceSide)
	}
	return "trigger " + label
}

func formatProposalStopRisk(risk *rpc.TradeProposalStopRisk, fallbackCurrency string) string {
	if risk == nil || risk.EstimatedLoss == nil {
		return ""
	}
	currency := nonEmpty(strings.TrimSpace(risk.Currency), fallbackCurrency)
	parts := []string{formatProposalMoney(*risk.EstimatedLoss, currency)}
	if risk.EstimatedLossBase != nil && risk.BaseCurrency != "" && !strings.EqualFold(risk.BaseCurrency, currency) {
		parts = append(parts, formatProposalMoney(*risk.EstimatedLossBase, risk.BaseCurrency))
	}
	if risk.EstimatedLossPctNLV != nil {
		parts = append(parts, fmt.Sprintf("%.2f%% NLV", *risk.EstimatedLossPctNLV))
	}
	if risk.DistancePct != nil {
		parts = append(parts, fmt.Sprintf("%.2f%% stop distance", *risk.DistancePct))
	}
	return strings.Join(parts, " | ")
}

func formatProposalGapScenario(risk *rpc.TradeProposalStopRisk, fallbackCurrency string) string {
	if risk == nil || risk.GapScenario == nil {
		return ""
	}
	gap := risk.GapScenario
	currency := nonEmpty(strings.TrimSpace(risk.Currency), fallbackCurrency)
	parts := []string{}
	if gap.AssumedExecutionPrice != nil {
		parts = append(parts, fmt.Sprintf("%.1f%% beyond stop at %s", gap.GapPct, formatProposalMoney(*gap.AssumedExecutionPrice, currency)))
	}
	if gap.EstimatedLoss != nil {
		parts = append(parts, "loss "+formatProposalMoney(*gap.EstimatedLoss, currency))
	}
	if gap.EstimatedLossBase != nil && risk.BaseCurrency != "" && !strings.EqualFold(risk.BaseCurrency, currency) {
		parts = append(parts, formatProposalMoney(*gap.EstimatedLossBase, risk.BaseCurrency))
	}
	if gap.EstimatedLossPctNLV != nil {
		parts = append(parts, fmt.Sprintf("%.2f%% NLV", *gap.EstimatedLossPctNLV))
	}
	return strings.Join(parts, " | ")
}

func formatProposalStopLadder(ladder []rpc.TradeProposalStopLadderStep, fallbackCurrency string) string {
	if len(ladder) == 0 {
		return ""
	}
	parts := make([]string, 0, min(len(ladder), 5))
	for _, step := range ladder {
		if step.StopPrice == nil {
			continue
		}
		label := strings.TrimSpace(step.Label)
		if label == "" {
			label = strings.TrimSpace(step.Kind)
		}
		if label == "" {
			label = "step"
		}
		parts = append(parts, fmt.Sprintf("%s %s", label, formatProposalMoney(*step.StopPrice, fallbackCurrency)))
		if len(parts) == 5 {
			break
		}
	}
	return strings.Join(parts, " | ")
}

func proposalRiskWarnings(sem *rpc.TradeProposalExecutionSemantics, risk *rpc.TradeProposalStopRisk) []string {
	var warnings []string
	if sem != nil {
		switch sem.PriceGuarantee {
		case "stop_price_is_not_execution_price":
			warnings = append(warnings, "TRAIL converts to a market order when triggered; stop price is not the execution price.")
		case "stop_limit_can_leave_position_unfilled":
			warnings = append(warnings, "TRAIL LIMIT converts to a limit order when triggered; the position can remain open if the limit does not fill.")
		}
	}
	if risk != nil {
		warnings = append(warnings, risk.WarningCodes...)
	}
	return warnings
}

// formatProposalPositionLine renders holding-level decision context for a
// proposal: position size, market value and its share of NLV, and today's P&L
// move colored by sign. This is the exposure being acted on — distinct from the
// order size in the proposal head — so a human can judge the trade in context.
// Empty when the daemon attached no position context.
func formatProposalPositionLine(env *Env, p *rpc.TradeProposal) string {
	var parts []string
	// A share/contract count is exact only for a single-instrument proposal.
	// Risk reduction acts on a whole single-name group (stock + options), so a
	// leg's count would misrepresent the size the dollar market value reports —
	// lead with the dollar exposure there instead.
	if qty := p.PositionQuantity; qty != 0 && p.Bucket != rpc.TradeProposalBucketRiskReduction {
		if qty < 0 {
			qty = -qty
		}
		parts = append(parts, fmt.Sprintf("held %g %s", qty, positionUnit(p.SecType)))
	}
	if p.PositionMarketValue != 0 {
		mv := formatProposalMoney(p.PositionMarketValue, p.Contract.Currency)
		if p.MarketValuePctNLV != nil {
			mv += fmt.Sprintf(" (%.1f%% NLV)", *p.MarketValuePctNLV)
		}
		parts = append(parts, mv)
	}
	if p.PositionDayChangeMoney != nil {
		parts = append(parts, "today "+env.formatProposalDayChange(*p.PositionDayChangeMoney, p.PositionDayChangeCurrency, p.PositionDayChangePct))
	}
	return strings.Join(parts, " · ")
}

func positionUnit(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "OPT", "OPTION":
		return "ct"
	default:
		return "sh"
	}
}

// formatProposalDayChange renders today's position P&L with an explicit sign and
// green/red color (colorBySign), so direction is legible without relying on
// color alone (NO_COLOR, color-blindness). The percent carries its own sign.
func (e *Env) formatProposalDayChange(money float64, ccy string, pct *float64) string {
	s := formatProposalMoney(money, ccy)
	if money > 0 {
		s = "+" + s
	}
	if pct != nil {
		s += fmt.Sprintf(" (%+.1f%%)", *pct)
	}
	return e.colorBySign(money, s, signPnL)
}

func formatProposalMoney(v float64, currency string) string {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return fmt.Sprintf("%.2f", v)
	}
	return formatMoneyCcy(v, currency)
}

func renderProposalSubmitText(env *Env, res *rpc.TradeProposalSubmitResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Submit  accepted=%v\n", res.Accepted)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	statusRow(env, out, "Order ref", res.OrderRef)
	if res.Place != nil {
		statusRow(env, out, "Broker ID", strconv.Itoa(res.Place.ReservedOrderID))
		statusRow(env, out, "Lifecycle", nonEmpty(res.Place.LifecycleStatus, res.Place.SendState))
		if res.Place.Status != "" {
			statusRow(env, out, "Status", res.Place.Status)
		}
		if res.Place.Message != "" && res.Place.Message != res.Message {
			statusRow(env, out, "Broker message", res.Place.Message)
		}
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}
