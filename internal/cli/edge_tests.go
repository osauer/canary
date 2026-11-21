package cli

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// runEdgeTests is `canary edge tests [RULE] [--all] [--json]`: the forward
// tests told top to bottom, read rules first, then due, then collecting
// ordered by how full they are. One rule by id, or --all, adds the rule's
// registered text and the details behind its counts.
func runEdgeTests(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge tests")
	all := fs.Bool("all", false, "every detail of every rule: its registered text, unit, policy and what gave no reading")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 1 {
		return failUnexpectedArgs(env, fs)
	}
	params, err := rpc.NormalizeEdgeSnapshotParams(rpc.EdgeSnapshotParams{})
	if err != nil {
		return fail(env, "edge tests: %v", err)
	}
	result, err := fetchEdge(ctx, env, params, false)
	if err != nil {
		return fail(env, "edge tests: %v", err)
	}
	view := edgeTestsView{ForwardTests: result.ForwardTests, ForwardTestsUnavailable: result.ForwardTestsUnavailable}
	if id := fs.Arg(0); id != "" {
		i := slices.IndexFunc(result.ForwardTests, func(t rpc.EdgeForwardTest) bool { return t.ID == id })
		if i < 0 {
			ids := make([]string, 0, len(result.ForwardTests))
			for _, t := range result.ForwardTests {
				ids = append(ids, t.ID)
			}
			return fail(env, "edge tests: no rule %q; the rules are %s", id, strings.Join(ids, ", "))
		}
		view.ForwardTests, *all = result.ForwardTests[i:i+1], true
	}
	if *jsonOut {
		return printJSON(env, view)
	}
	renderEdgeTests(env, view, *all)
	return 0
}

// edgeTestsView is the JSON of `canary edge tests`: the rules as the
// daemon serves them in canary edge --json.
type edgeTestsView struct {
	ForwardTests            []rpc.EdgeForwardTest `json:"forward_tests"`
	ForwardTestsUnavailable string                `json:"forward_tests_unavailable,omitempty"`
}

// Layout of a rule's marks: label, bar and count columns shared by every
// rule, so rules compare at a glance. 4 + 24 + 1 + 24 + 2 + 10 + 2 + 12 = 79.
const (
	edgeTestsIndent  = "    "
	edgeTestsLabel   = 24
	edgeTestsBar     = 24
	edgeTestsCount   = 10
	edgeTestsAxis    = 40
	edgeTestsMeasure = 80
	edgeTestsTrack   = "░"
	edgeTestsEighths = " ▏▎▍▌▋▊▉"
	edgeTestsSparks  = "▁▂▃▄▅▆▇█"
	edgeTestsNoMonth = "·"
)

func renderEdgeTests(env *Env, view edgeTestsView, all bool) {
	out := env.Stdout
	fmt.Fprintln(out, env.bold("Forward tests"))
	if view.ForwardTestsUnavailable == "registry_unreadable" {
		edgeTestsParagraph(env, "The registered rules could not be read this time. This does not mean none are registered; ask again.")
		return
	}
	if view.ForwardTestsUnavailable == "account_scope_unavailable" {
		edgeTestsParagraph(env, "No account is connected, so the rules of this account cannot be shown. Ask again once the daemon names its account.")
		return
	}
	edgeTestsParagraph(env, "A forward test is a rule written down before the decisions it is read on: only decisions made after the rule was registered count. Canary counts them; the reading that turns a full count into an answer is not built yet. In the rules about sales, a sale a protection rule sent is read apart from one you chose; a stop placed directly at the broker cannot be told apart yet.")
	tests := slices.Clone(view.ForwardTests)
	counted := 0
	for _, t := range tests {
		if t.Sample != nil {
			counted++
		}
	}
	if counted == 0 && len(tests) > 0 {
		fmt.Fprintln(out)
		edgeTestsParagraph(env, "Nothing is counted in this reply: the daemon has no decision review yet, the rules are not registered yet, or the daemon runs a build that does not count. The rules and their read points are below.")
	}
	rank := func(t rpc.EdgeForwardTest) int {
		switch {
		case t.Sample == nil:
			return 3
		case t.Sample.State == rpc.EdgeForwardRead:
			return 0
		case t.Sample.State == rpc.EdgeForwardDue:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(tests, func(a, b rpc.EdgeForwardTest) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		fa, fb := edgeTestsFullness(a), edgeTestsFullness(b)
		switch {
		case fa > fb:
			return -1
		case fa < fb:
			return 1
		}
		return 0
	})
	headings := map[int]string{0: "Read", 1: "Due", 2: "Collecting", 3: "Not counted in this reply"}
	notes := map[int]string{1: " · the count is full; the reading is not built yet"}
	last := -1
	for _, t := range tests {
		if r := rank(t); r != last {
			n := 0
			for _, other := range tests {
				if rank(other) == r {
					n++
				}
			}
			fmt.Fprintf(out, "\n%s  %s\n", env.bold(headings[r]), env.dim(fmt.Sprintf("%d %s", n, pluralWord(n, "rule", "rules"))+notes[r]))
			last = r
		}
		renderEdgeTest(env, t, all)
	}
	if !all {
		fmt.Fprintf(out, "\nDetails: canary edge tests <rule> · canary edge tests --all · --json\n")
	}
}

// edgeTestsFullness is how far the read group is toward the read point.
func edgeTestsFullness(t rpc.EdgeForwardTest) float64 {
	if t.Sample == nil || t.ReadAt <= 0 {
		return 0
	}
	return float64(t.ReadAt-t.Sample.ToRead) / float64(t.ReadAt)
}

func renderEdgeTest(env *Env, t rpc.EdgeForwardTest, all bool) {
	out := env.Stdout
	fmt.Fprintln(out)
	question := t.Question
	if question == "" {
		question = t.Title
	}
	edgeTestsStyled(env, "  ", "", sanitizeRunText(question), env.bold)
	condition := "nothing to check first: every decision counts"
	if t.Condition != "" {
		condition = "checks " + sanitizeRunText(t.Condition)
	}
	edgeTestsProse(env, edgeTestsIndent, briefJoin(edgeTestsInforms(t.Informs), condition))
	s := t.Sample
	if s == nil {
		edgeTestsProse(env, edgeTestsIndent, edgeTestsNotCounted(t))
		if all {
			renderEdgeTestDetails(env, t)
		}
		return
	}
	labels := map[string]string{"all": firstNonEmptyCLI(t.SampleLabel, "decisions"), "not_read": "group not read yet"}
	for _, g := range t.Groups {
		labels[g.ID] = g.Label
	}
	collecting := s.State == rpc.EdgeForwardCollecting
	for _, g := range s.Groups {
		counts := g.ID == t.ReadGroup || t.ReadGroup == "any" || g.ID == "all"
		renderEdgeTestRow(env, labels[g.ID], g, t.ReadAt, counts, collecting)
	}
	if t.ReadDays > 0 {
		for _, g := range s.Groups {
			if g.ID == t.ReadGroup {
				edgeTestsProse(env, edgeTestsIndent, fmt.Sprintf("%s also needs %d separate days: %d so far", labels[g.ID], t.ReadDays, g.Days))
			}
		}
	}
	if t.GroupNotRead != "" {
		edgeTestsProse(env, edgeTestsIndent, "the groups are not told apart yet: "+sanitizeRunText(t.GroupNotRead))
	} else if slices.ContainsFunc(s.Groups, func(g rpc.EdgeForwardGroupCount) bool { return g.ID == "not_read" && g.Final+g.Pending > 0 }) {
		edgeTestsProse(env, edgeTestsIndent, "group not read yet: the fact that places them was not recorded")
	}
	if spark := edgeTestsSparkline(env, s.Months, collecting); spark != "" {
		edgeTestsProse(env, edgeTestsIndent, spark)
	}
	edgeTestsProse(env, edgeTestsIndent, edgeTestsWhen(t, s.AsOf))
	if t.ProtectionApart {
		edgeTestsProse(env, edgeTestsIndent, edgeTestsProtection(s))
	}
	if s.Today != nil {
		edgeTestsProse(env, edgeTestsIndent, edgeTestsToday(t.ID, *s.Today, labels, s.AsOf))
	}
	if s.Reading != nil {
		renderEdgeTestReading(env, *s.Reading, labels)
	}
	if all {
		renderEdgeTestDetails(env, t)
	}
}

func edgeTestsInforms(informs []string) string {
	words := map[string]string{"buy": "buying", "hold": "holding", "sell": "selling"}
	var parts []string
	for _, i := range informs {
		parts = append(parts, firstNonEmptyCLI(words[i], i))
	}
	if len(parts) == 0 {
		return "informs no decision named"
	}
	return "informs " + strings.Join(parts, " or ")
}

func edgeTestsNotCounted(t rpc.EdgeForwardTest) string {
	text := ""
	if t.RegisteredAt.IsZero() {
		text = "not registered yet: it starts counting once the daemon registers it · "
	}
	if t.ReadAt > 0 {
		text += fmt.Sprintf("read at %d", t.ReadAt)
		if t.UnitText != "" {
			text += " · counts " + sanitizeRunText(t.UnitText)
		}
	}
	return strings.TrimSuffix(text, " · ")
}

// renderEdgeTestRow prints one group: label, bar, count, and the units
// not final yet. A group whose fact is not read has no bar: its units are
// counted but cannot be placed.
func renderEdgeTestRow(env *Env, label string, g rpc.EdgeForwardGroupCount, readAt int, counts, collecting bool) {
	bar := strings.Repeat(" ", edgeTestsBar)
	count := edgeCount(g.Final)
	if g.ID != "not_read" {
		bar = edgeTestsBarText(env, g.Final, readAt, collecting)
		if counts {
			count = fmt.Sprintf("%s of %s", edgeCount(g.Final), edgeCount(readAt))
		}
	}
	line := edgeTestsIndent + padRightVisible(label, edgeTestsLabel) + " " + bar + "  " + padLeftVisible(count, edgeTestsCount)
	if g.Pending > 0 {
		line += "  " + env.dim(fmt.Sprintf("+%s not final", edgeCount(g.Pending)))
	}
	fmt.Fprintln(env.Stdout, line)
}

// edgeTestsBarText is a bar of edgeTestsBar cells in eighths of a cell on a
// track of a different glyph, so it reads without colour. A count above
// zero shows at least an eighth.
func edgeTestsBarText(env *Env, n, of int, collecting bool) string {
	eighths := 0
	if of > 0 && n > 0 {
		eighths = max(1, min(edgeTestsBar*8, int(math.Round(float64(n)/float64(of)*edgeTestsBar*8))))
	}
	full, part := eighths/8, eighths%8
	fill := strings.Repeat("█", full)
	if part > 0 {
		fill += string([]rune(edgeTestsEighths)[part])
	}
	cells := full
	if part > 0 {
		cells++
	}
	if collecting && fill != "" {
		fill = env.dim(fill)
	}
	if track := strings.Repeat(edgeTestsTrack, edgeTestsBar-cells); track != "" {
		fill += env.dim(track)
	}
	return fill
}

// edgeTestsSparkline is final units per month since registration, a dot
// for a month with none, with the months it spans.
func edgeTestsSparkline(env *Env, months []rpc.EdgeForwardMonth, collecting bool) string {
	peak := 0
	for _, m := range months {
		peak = max(peak, m.Final)
	}
	if peak == 0 || len(months) == 0 {
		return ""
	}
	sparks := []rune(edgeTestsSparks)
	var b strings.Builder
	for _, m := range months {
		if m.Final == 0 {
			b.WriteString(edgeTestsNoMonth)
			continue
		}
		b.WriteRune(sparks[(m.Final*len(sparks)-1)/peak])
	}
	line := b.String()
	if collecting {
		line = env.dim(line)
	}
	span := edgeTestsMonth(months[0].Month)
	if len(months) > 1 {
		span += "–" + edgeTestsMonth(months[len(months)-1].Month)
	}
	return fmt.Sprintf("final per month  %s  %s, at most %d", line, span, peak)
}

func edgeTestsMonth(month string) string {
	at, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	return at.Format("Jan 2006")
}

// edgeTestsDay is a date as "21 Nov", with the year when it is not the
// year of the count.
func edgeTestsDay(value string, asOf time.Time) string {
	at, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return value
	}
	if at.Year() == asOf.Year() {
		return at.Format("2 Jan")
	}
	return at.Format("2 Jan 2006")
}

// edgeTestsWhen says since when the rule counts, when its read point is
// expected and on what pace, or why no date can be given.
func edgeTestsWhen(t rpc.EdgeForwardTest, asOf time.Time) string {
	s := t.Sample
	parts := []string{"counting from " + edgeTestsDay(t.RegisteredAt.Local().Format(time.DateOnly), asOf)}
	switch {
	case s.State == rpc.EdgeForwardRead && s.Reading != nil:
		parts = append(parts, "read "+edgeTestsDay(s.Reading.ReadAt.Local().Format(time.DateOnly), asOf))
	case s.State == rpc.EdgeForwardDue:
		parts = append(parts, "the read point is met")
	case s.Expected != nil:
		pace := "at this pace"
		if s.Expected.Basis == "past_year" {
			pace = "at the past year's pace"
		}
		when := pace + " about " + edgeTestsMonth(s.Expected.Date[:7])
		parts = append(parts, when)
		if s.Expected.BeyondWindow {
			when += fmt.Sprintf(", more than %d days out: it cannot fill until counted decisions are kept, which is not built yet", s.WindowDays)
		}
		parts[len(parts)-1] = when
	case s.NoDate == "group_not_read":
		parts = append(parts, "no date until the group is read")
	case s.NoDate == "sender_not_known":
		parts = append(parts, "no date: who sent the sales could not be told this time")
	case s.NoDate == "not_recorded_before":
		parts = append(parts, "no date yet: the closes before registration have no record to set a pace")
	case s.NoDate == "none_in_read_group":
		parts = append(parts, "no date yet: none of the final decisions is in the group it is read on")
	default:
		parts = append(parts, "no date: no such decision in the past year to set a pace")
	}
	return briefJoin(parts...)
}

func edgeTestsProtection(s *rpc.EdgeForwardSample) string {
	text := "sales a protection rule sent are read apart and count neither here nor toward the read point"
	if s.Protection != nil {
		text = fmt.Sprintf("sent by a protection rule, read apart: %s final", edgeCount(s.Protection.Final))
		if s.Protection.Pending > 0 {
			text += fmt.Sprintf(", %s not final", edgeCount(s.Protection.Pending))
		}
	}
	if s.SenderUnknown > 0 {
		text += fmt.Sprintf(" · %s whose sender could not be told this time %s in neither", edgeTestsUnits(s.SenderUnknown), pluralWord(s.SenderUnknown, "counts", "count"))
	}
	return text
}

func edgeTestsUnits(n int) string {
	return fmt.Sprintf("%s %s", edgeCount(n), pluralWord(n, "unit", "units"))
}

// edgeTestsToday says how a buy made now would be counted, from the record
// of the last US close only.
func edgeTestsToday(id string, today rpc.EdgeForwardToday, labels map[string]string, asOf time.Time) string {
	day := edgeTestsDay(today.Session, asOf)
	if today.Missing != "" {
		return fmt.Sprintf("a buy now cannot be grouped: Canary has no record of the %s close", day)
	}
	if id == "option_buys_after_regime_warning" {
		stage := strings.ReplaceAll(sanitizeRunText(today.RegimeStage), "_", " ")
		if label := labels[today.Group]; today.Group != "not_read" && label != "" {
			return fmt.Sprintf("a buy now counts as %s: Regime was at %s at the %s close", label, stage, day)
		}
		return fmt.Sprintf("a buy now cannot be grouped: Regime was at %s at the %s close", stage, day)
	}
	line := "the line at the " + day + " close is not recorded"
	if today.PremiumBudgetActPct != nil {
		line = fmt.Sprintf("the line was %s of net liquidation at the %s close", strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", *today.PremiumBudgetActPct), "0"), ".0")+"%", day)
		if today.PremiumBudgetSet != "" {
			line += " (" + sanitizeRunText(today.PremiumBudgetSet) + " set)"
		}
	}
	return "a buy now: " + line + "; its group is not known until premium at risk is read"
}

func renderEdgeTestDetails(env *Env, t rpc.EdgeForwardTest) {
	if t.UnitText != "" {
		edgeTestsProse(env, edgeTestsIndent, briefJoin("counts "+sanitizeRunText(t.UnitText), fmt.Sprintf("read at %d", t.ReadAt)))
	}
	policy := "touches no written policy"
	if t.Policy != "" {
		policy = "policy: " + sanitizeRunText(t.Policy)
	}
	edgeTestsProse(env, edgeTestsIndent, policy)
	if s := t.Sample; s != nil && len(s.Excluded) > 0 {
		words := map[string]string{
			"closed_by_delivery": "closed by delivery", "position_not_proved": "position not proved from the statements",
			"no_close_stored": "no close stored", "no_closing_quote": "no closing quote stored",
			"sold_at_cost": "sold at exactly cost, in neither group", "underlying_not_known": "underlying not named",
		}
		keys := make([]string, 0, len(s.Excluded))
		for k := range s.Excluded {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := []string{"no reading"}
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d %s", s.Excluded[k], firstNonEmptyCLI(words[k], strings.ReplaceAll(k, "_", " "))))
		}
		edgeTestsProse(env, edgeTestsIndent, briefJoin(parts...))
	}
	if !t.RegisteredAt.IsZero() {
		edgeTestsProse(env, edgeTestsIndent, "registered "+t.RegisteredAt.Local().Format("2006-01-02 15:04 MST")+" as "+t.RuleHash)
	}
	edgeTestsStyled(env, edgeTestsIndent, "  ", "Rule: "+sanitizeRunText(t.Rule), env.dim)
}

// renderEdgeTestReading draws a read rule: the finding in one sentence,
// then one interval per group on a shared axis with the zero marked, and
// the axis labelled once underneath. Colour carries the finding's tone;
// the sentence carries it in words.
func renderEdgeTestReading(env *Env, reading rpc.EdgeForwardReading, labels map[string]string) {
	tone := func(line string) string { return line }
	switch reading.Tone {
	case "favourable":
		tone = env.green
	case "unfavourable":
		tone = env.red
	}
	edgeTestsStyled(env, edgeTestsIndent, "  ", sanitizeRunText(reading.Finding), tone)
	lo, hi := 0.0, 0.0
	for _, iv := range reading.Intervals {
		lo, hi = min(lo, iv.Low), max(hi, iv.High)
	}
	if hi-lo <= 0 {
		hi = lo + 1
	}
	pos := func(v float64) int {
		return int(math.Round((v - lo) / (hi - lo) * (edgeTestsAxis - 1)))
	}
	zero := pos(0)
	for _, iv := range reading.Intervals {
		cells := []rune(strings.Repeat(" ", edgeTestsAxis))
		a, b, m := pos(iv.Low), pos(iv.High), pos(iv.Estimate)
		for i := a; i <= b; i++ {
			cells[i] = '─'
		}
		cells[zero] = '┼'
		cells[a], cells[b] = '├', '┤'
		cells[m] = '●'
		fmt.Fprintln(env.Stdout, edgeTestsIndent+padRightVisible(firstNonEmptyCLI(labels[iv.Group], iv.Group), edgeTestsLabel)+" "+strings.TrimRight(string(cells), " "))
	}
	axis := []rune(strings.Repeat(" ", edgeTestsAxis))
	// place writes a label where it fits with a space either side, and
	// drops it where it would touch another.
	place := func(text string, at int) {
		r := []rune(text)
		start := max(0, min(at-len(r)/2, edgeTestsAxis-len(r)))
		for i := start - 1; i <= start+len(r); i++ {
			if i >= 0 && i < edgeTestsAxis && axis[i] != ' ' {
				return
			}
		}
		copy(axis[start:], r)
	}
	place(edgeTestsTick(lo), 0)
	place(edgeTestsTick(hi), edgeTestsAxis)
	place("0", zero)
	fmt.Fprintln(env.Stdout, edgeTestsIndent+strings.Repeat(" ", edgeTestsLabel+1)+strings.TrimRight(string(axis), " "))
	edgeTestsStyled(env, edgeTestsIndent+strings.Repeat(" ", edgeTestsLabel+1), "", sanitizeRunText(reading.Measure), env.dim)
}

func edgeTestsTick(v float64) string {
	if v == 0 {
		return "0"
	}
	return fmt.Sprintf("%+.0f%%", v)
}

// edgeTestsStyled is edgeTestsProse with a style applied to each line, so
// a wrapped line never carries an open escape into the next.
func edgeTestsStyled(env *Env, indent, hang, text string, style func(string) string) {
	width := min(briefProseWidth(env.Stdout), edgeTestsMeasure) - len(indent) - len(hang)
	for i, line := range wrapVisibleText(text, width) {
		lead := indent
		if i > 0 {
			lead = indent + hang
		}
		fmt.Fprintln(env.Stdout, lead+style(line))
	}
}

// edgeTestsParagraph is a paragraph at two cells, without a hanging indent.
func edgeTestsParagraph(env *Env, text string) {
	edgeTestsStyled(env, "  ", "", text, func(s string) string { return s })
}

// edgeTestsProse wraps prose at the 80-column measure with indent on the
// first line and two more cells on the rest. Marks never pass through it.
func edgeTestsProse(env *Env, indent, text string) {
	width := min(briefProseWidth(env.Stdout), edgeTestsMeasure) - len(indent) - 2
	glued := strings.ReplaceAll(text, " · ", edgeGlue+"· ")
	for i, line := range wrapVisibleText(glued, width) {
		lead := indent
		if i > 0 {
			lead = indent + "  "
		}
		fmt.Fprintln(env.Stdout, lead+strings.ReplaceAll(line, edgeGlue, " "))
	}
}
