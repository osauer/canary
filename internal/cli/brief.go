package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
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
		fmt.Fprintf(env.Stderr, "canary: brief rendered but stamp failed: %v\n", err)
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
	narrative := servedBriefNarrative(res.Narrative)
	if narrative == nil {
		renderBriefReview(env, res.Review)
		renderBriefReady(env, res.Ready)
		return
	}
	renderBriefNarrative(env, narrative, res, briefProseWidth(env.Stdout))
}

func renderBriefReview(env *Env, review rpc.BriefReviewSection) {
	fmt.Fprintln(env.Stdout, "\nReview  (since the last close)")
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
		oneTap = briefOneTapSignable
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
	// Detail and value carry broker-sourced text (symbols, blockers); the same
	// control-byte hazard the narrative render strips applies here.
	fmt.Fprintf(env.Stdout, "  %-18s %-11s %s\n", label, state.Status, sanitizeRunText(value))
	fmt.Fprintf(env.Stdout, "    %s\n", sanitizeRunText(state.Detail))
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

// ---- narrative render -------------------------------------------------
//
// The daemon composes the brief's prose; this renderer only projects the typed
// runs onto a terminal. It invents no sentence, re-derives no role, and states
// no fact the payload did not serve. When the daemon serves no narrative — an
// older daemon, or a payload whose movements did not compose — the row render
// above stays the surface.

const (
	// briefProseMeasure caps the prose line length. Long measures read badly
	// even on a wide terminal, and the cap keeps a render comparable between
	// windows.
	briefProseMeasure = 80
	// briefProseIndent sets prose in from the section headers, matching the
	// row render's own indent.
	briefProseIndent = "  "
	// briefOneTapSignable is the row view's sign-off wording, shared so the
	// prose render can name the command in exactly the same words.
	briefOneTapSignable = "signable · canary policy capital-event reconcile"
)

// briefProseWidth resolves the measure for one render. The width is passed
// into the renderer explicitly: an ambient terminal read inside the renderer
// would make every rendered line depend on the caller's environment.
func briefProseWidth(w io.Writer) int {
	cols := outputColumns(w)
	if cols <= 0 {
		return briefProseMeasure
	}
	return min(cols, briefProseMeasure)
}

// briefNarrativeView is a served narrative: runs and paragraphs that survived
// filtering. It mirrors servedNarrative in web/app/brief.js exactly — empty
// runs drop, empty paragraphs drop, and a narrative counts as served only when
// the lead or one of the movements survives. A coda-only narrative is absent,
// so the two surfaces can never disagree about what "served" means.
type briefNarrativeView struct {
	lead   []rpc.BriefRun
	review [][]rpc.BriefRun
	ready  [][]rpc.BriefRun
	coda   []rpc.BriefRun
}

func servedBriefNarrative(narrative *rpc.BriefNarrative) *briefNarrativeView {
	if narrative == nil {
		return nil
	}
	lead := briefRunList(narrative.Lead)
	review := briefParagraphList(narrative.Review)
	ready := briefParagraphList(narrative.Ready)
	if len(lead) == 0 && len(review) == 0 && len(ready) == 0 {
		return nil
	}
	return &briefNarrativeView{lead: lead, review: review, ready: ready, coda: briefRunList(narrative.Coda)}
}

func briefRunList(runs []rpc.BriefRun) []rpc.BriefRun {
	out := make([]rpc.BriefRun, 0, len(runs))
	for _, run := range runs {
		if run.Text != "" {
			out = append(out, run)
		}
	}
	return out
}

func briefParagraphList(paragraphs []rpc.BriefParagraph) [][]rpc.BriefRun {
	out := make([][]rpc.BriefRun, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		if runs := briefRunList(paragraph.Runs); len(runs) > 0 {
			out = append(out, runs)
		}
	}
	return out
}

func renderBriefNarrative(env *Env, narrative *briefNarrativeView, res rpc.BriefResult, width int) {
	if len(narrative.lead) > 0 {
		fmt.Fprintln(env.Stdout)
		briefProseParagraph(env, narrative.lead, width)
	}
	fmt.Fprintln(env.Stdout, "\nReview  (since the last close)")
	briefProseParagraphs(env, narrative.review, width)
	if res.Review.OneTap.Signable {
		// The row view is the only place the CLI names the sign-off command;
		// prose keeps that line in the row view's own words.
		fmt.Fprintln(env.Stdout)
		briefLabelLine(env, "one-tap sign-off", briefOneTapSignable, width)
	}
	fmt.Fprintln(env.Stdout, "\nReady  (today)")
	briefProseParagraphs(env, narrative.ready, width)
	if len(narrative.coda) > 0 {
		fmt.Fprintln(env.Stdout)
		briefProseParagraph(env, narrative.coda, width)
	}
	briefDegradedFooter(env, res, width)
}

// briefProseParagraphs separates paragraphs with a blank line and leaves none
// before the first, so a section header stays attached to its opening sentence.
func briefProseParagraphs(env *Env, paragraphs [][]rpc.BriefRun, width int) {
	for i, runs := range paragraphs {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		briefProseParagraph(env, runs, width)
	}
}

func briefProseParagraph(env *Env, runs []rpc.BriefRun, width int) {
	for _, line := range briefWrapWords(briefWords(runs), width-len(briefProseIndent)) {
		fmt.Fprintln(env.Stdout, briefProseIndent+briefLineText(env, line))
	}
}

// briefDegradedFooter keeps the brief's per-row disclosure contract alive under
// prose. The narrative names an unread input but never carries the row's own
// reason, and the terminal has no tap-through to reach it, so every degraded or
// unavailable row states its reason beneath the prose. A brief whose inputs all
// read prints nothing here.
func briefDegradedFooter(env *Env, res rpc.BriefResult, width int) {
	rows := briefDegradedRows(res)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(env.Stdout, "\nDegraded inputs")
	for _, row := range rows {
		briefLabelLine(env, row.label, row.detail, width)
	}
}

// briefLabelLine prints one "label: value" line beneath the prose, wrapped to
// the measure with a hanging indent so a long reason stays inside it.
func briefLabelLine(env *Env, label, value string, width int) {
	hanging := briefProseIndent + briefProseIndent
	for i, line := range wrapVisibleText(label+": "+value, width-len(hanging)) {
		indent := briefProseIndent
		if i > 0 {
			indent = hanging
		}
		fmt.Fprintln(env.Stdout, indent+line)
	}
}

// briefDisclosure is one row's degradation, labelled as the row render labels
// it so the two modes name the same input the same way.
type briefDisclosure struct{ label, detail string }

// briefDegradedRows walks every leaf row the brief carries, in render order,
// and keeps the ones whose input did not fully read. Section rollups are
// skipped: they restate their children and would double-count.
func briefDegradedRows(res rpc.BriefResult) []briefDisclosure {
	var out []briefDisclosure
	add := func(label string, state rpc.BriefRowState) {
		if state.Status != rpc.BriefStatusDegraded && state.Status != rpc.BriefStatusUnavailable {
			return
		}
		out = append(out, briefDisclosure{
			label:  sanitizeRunText(label),
			detail: sanitizeRunText(nonEmpty(state.Detail, state.Status)),
		})
	}
	review, ready := res.Review, res.Ready
	add("session P&L", review.SessionPnL.BriefRowState)
	add("by underlying", review.Attribution.BriefRowState)
	add("rules delta", review.RulesDelta.BriefRowState)
	add("proposals", review.Proposals.BriefRowState)
	add("overrides used", review.Overrides.BriefRowState)
	add("capital events", review.CapitalEvents.BriefRowState)
	add("reconcile", review.Reconcile.BriefRowState)
	add("auto-extend", review.AutoExtend.BriefRowState)
	add("one-tap sign-off", review.OneTap.BriefRowState)
	add("working orders", review.WorkingOrders.BriefRowState)
	add("regime", ready.Regime.BriefRowState)
	add("breadth", ready.Breadth.BriefRowState)
	add("dealer gamma", ready.Gamma.BriefRowState)
	add("stress", ready.Stress.BriefRowState)
	add("session", ready.Session.BriefRowState)
	for _, event := range ready.MarketEvents {
		add(event.Kind, event.BriefRowState)
	}
	add("capital", ready.Capital.BriefRowState)
	add("drawdown latch", ready.Latch.BriefRowState)
	add("premium at risk", ready.PremiumAtRisk.BriefRowState)
	add("hedge cost / day", ready.HedgeCost.BriefRowState)
	// The row render has no line for the Ready-side proposal projection; the
	// disclosure must not inherit that gap.
	add("protection proposals", ready.Proposals.BriefRowState)
	add("policy drift", ready.PolicyDrift.BriefRowState)
	for _, artefact := range ready.Artefacts.Rows {
		add("artefact "+artefact.Kind, artefact.BriefRowState)
	}
	return out
}

// briefSeg is one tinted fragment. briefWord is one unbreakable token, which
// may span runs: the composer emits "AAPL ", a figure "+900.00", then ", NVDA ",
// so a token can carry fragments of two runs and a plain split on spaces would
// lose the boundary between them.
type briefSeg struct{ text, role string }

type briefWord []briefSeg

func briefWords(runs []rpc.BriefRun) []briefWord {
	var words []briefWord
	var word briefWord
	for _, run := range runs {
		leading := true
		for fragment := range strings.SplitSeq(sanitizeRunText(run.Text), " ") {
			if !leading && len(word) > 0 {
				words, word = append(words, word), nil
			}
			leading = false
			if fragment != "" {
				word = append(word, briefSeg{text: fragment, role: run.Role})
			}
		}
	}
	if len(word) > 0 {
		words = append(words, word)
	}
	return words
}

func briefWordLen(word briefWord) int {
	n := 0
	for _, seg := range word {
		n += visibleLen(seg.text)
	}
	return n
}

// briefWrapWords packs words greedily into lines of at most width cells. A
// token longer than the whole measure is split rather than allowed to overrun.
func briefWrapWords(words []briefWord, width int) [][]briefWord {
	if len(words) == 0 {
		return nil
	}
	if width <= 0 {
		return [][]briefWord{words}
	}
	var lines [][]briefWord
	var line []briefWord
	used := 0
	for _, word := range words {
		for briefWordLen(word) > width {
			var head briefWord
			head, word = splitBriefWord(word, width)
			if len(line) > 0 {
				lines, line, used = append(lines, line), nil, 0
			}
			lines = append(lines, []briefWord{head})
		}
		n := briefWordLen(word)
		switch {
		case len(line) == 0:
			line, used = []briefWord{word}, n
		case used+1+n <= width:
			line, used = append(line, word), used+1+n
		default:
			lines = append(lines, line)
			line, used = []briefWord{word}, n
		}
	}
	if len(line) > 0 {
		lines = append(lines, line)
	}
	return lines
}

func splitBriefWord(word briefWord, width int) (briefWord, briefWord) {
	var head, tail briefWord
	used := 0
	for _, seg := range word {
		switch n := visibleLen(seg.text); {
		case used >= width:
			tail = append(tail, seg)
		case used+n <= width:
			head = append(head, seg)
			used += n
		default:
			first, rest := splitVisibleWord(seg.text, width-used)
			head = append(head, briefSeg{text: first, role: seg.role})
			used = width
			if rest != "" {
				tail = append(tail, briefSeg{text: rest, role: seg.role})
			}
		}
	}
	return head, tail
}

// briefLineText renders one packed line. Adjacent fragments sharing a role
// coalesce into a single span so a composed run stays inside one escape pair,
// and every open span closes before the caller's newline: an escape must never
// survive a line break.
func briefLineText(env *Env, line []briefWord) string {
	var out strings.Builder
	open := ""
	write := func(text, role string) {
		code := ""
		if env != nil && env.Color {
			code = briefRunSGR(role)
		}
		if code != open {
			if open != "" {
				out.WriteString(ansiReset)
			}
			out.WriteString(code)
			open = code
		}
		out.WriteString(text)
	}
	for i, word := range line {
		if i > 0 {
			write(" ", briefSeparatorRole(line[i-1], word))
		}
		for _, seg := range word {
			write(seg.text, seg.role)
		}
	}
	if open != "" {
		out.WriteString(ansiReset)
	}
	return out.String()
}

// briefSeparatorRole keeps the space that once sat inside a run inside that
// run's span; a space between two differently-tinted words stays plain.
func briefSeparatorRole(prev, next briefWord) string {
	if len(prev) == 0 || len(next) == 0 {
		return ""
	}
	if role := prev[len(prev)-1].role; role == next[0].role {
		return role
	}
	return ""
}

// briefRunSGR maps a run role onto one combined SGR code. Roles are claims
// about the row beneath them, and the terminal answers watch with amber and act
// with red, the same severity mapping the stress render uses. Figures and body
// prose keep the terminal's own ink: a brief carries too many figures for
// emphasis to stay signal. One code per span, never nested, so a single reset
// always closes it.
func briefRunSGR(role string) string {
	switch role {
	case rpc.BriefRunRoleWatch:
		return ansiYellow
	case rpc.BriefRunRoleAct:
		return ansiRed
	default:
		return ""
	}
}

// sanitizeRunText drops control bytes from composed text. The daemon builds run
// text from broker-sourced symbols and policy identifiers, and this renderer is
// the one path that gives text an ANSI meaning — an escape arriving inside a
// broker field must never reach the terminal as a command.
func sanitizeRunText(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, text)
}
