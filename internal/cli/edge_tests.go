package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// runEdgeTests is `canary edge tests [NAME] [--all] [--json]`: the forward
// tests told top to bottom, full tests first, then collecting tests ordered
// by how far their count is, then tests with no count. One test by name, or
// --all, shows the test in full: its rule, what is waiting and what gave no
// result.
func runEdgeTests(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge tests")
	all := fs.Bool("all", false, "every test in full: its rule as registered, what is waiting and what gave no result")
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
	if name := fs.Arg(0); name != "" {
		i := slices.IndexFunc(result.ForwardTests, func(t rpc.EdgeForwardTest) bool { return t.ID == name })
		if i < 0 {
			names := make([]string, 0, len(result.ForwardTests))
			for _, t := range result.ForwardTests {
				names = append(names, t.ID)
			}
			return fail(env, "edge tests: no test %q; the tests are %s", name, strings.Join(names, ", "))
		}
		view.ForwardTests, *all = result.ForwardTests[i:i+1], true
	}
	if *jsonOut {
		return printJSON(env, view)
	}
	renderEdgeTests(env, view, *all)
	return 0
}

// edgeTestsView is the JSON of `canary edge tests`: the tests as the daemon
// serves them in canary edge --json.
type edgeTestsView struct {
	ForwardTests            []rpc.EdgeForwardTest `json:"forward_tests"`
	ForwardTestsUnavailable string                `json:"forward_tests_unavailable,omitempty"`
}

// The grid every row with a mark or a count sits on: label from column 4,
// mark from 29 for 24 cells, counts right-aligned to end at 57, "of" at 59,
// the number needed from 62, "+k waiting" from 67, dates from 55. No row
// passes column 78; prose wraps at 79.
const (
	edgeTestsMarkCol  = 29
	edgeTestsBar      = 24
	edgeTestsCountEnd = 58
	edgeTestsNoteCol  = 67
	edgeTestsDateCol  = 55
	edgeTestsWidth    = 79
	edgeTestsMonths   = 12
)

// edgeTestsGlyphs is the drawing set: block elements in a UTF-8 locale,
// plain ASCII otherwise.
type edgeTestsGlyphs struct {
	ascii   bool
	full    string
	track   string
	eighths []string
	sparks  []string
	noMonth string
	over    string
	unknown string
	rule    string
}

var edgeTestsUTF8 = edgeTestsGlyphs{full: "█", track: "░", eighths: []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"},
	sparks: []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}, noMonth: "·", over: "»", unknown: "–", rule: "─"}

var edgeTestsASCIIGlyphs = edgeTestsGlyphs{ascii: true, full: "#", track: ".",
	sparks: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, noMonth: "0", over: ">", unknown: "-", rule: "-"}

// edgeTestsLocaleUTF8 reads the locale's charset as the C library does:
// LC_ALL, then LC_CTYPE, then LANG, the first that is set.
func edgeTestsLocaleUTF8() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := os.Getenv(key); value != "" {
			value = strings.ToLower(value)
			return strings.Contains(value, "utf-8") || strings.Contains(value, "utf8")
		}
	}
	return false
}

// edgeTestsASCIIText replaces the few non-ASCII characters the words use.
var edgeTestsASCIIText = strings.NewReplacer("·", "-", "–", "-", "─", "-", "»", ">", "’", "'")

// edgeTestsScreen collects the lines of one screen and what its legend
// must explain.
type edgeTestsScreen struct {
	env       *Env
	g         edgeTestsGlyphs
	b         strings.Builder
	waiting   bool
	trackless bool
	beyond    bool
	// protection is set when a row says how many sales Canary's protection
	// sent.
	protection bool
	spark      string
	axis       []string
	// cause is set when one cause for no count is said for the whole screen.
	cause bool
}

func newEdgeTestsScreen(env *Env) *edgeTestsScreen {
	screen := &edgeTestsScreen{env: env, g: edgeTestsUTF8}
	if !edgeTestsLocaleUTF8() {
		screen.g = edgeTestsASCIIGlyphs
	}
	return screen
}

func (s *edgeTestsScreen) line(text string) {
	s.b.WriteString(text)
	s.b.WriteByte('\n')
}

// prose wraps text at 79 columns, indent on the first line and indent plus
// hang on the rest, with a style applied to each line.
func (s *edgeTestsScreen) prose(indent, hang, text string, style func(string) string) {
	if style == nil {
		style = func(v string) string { return v }
	}
	glued := strings.ReplaceAll(text, " · ", edgeGlue+"· ")
	lines := wrapVisibleText(glued, edgeTestsWidth-len(indent))
	if len(lines) > 1 && hang != "" {
		lines = append(lines[:1], wrapVisibleText(strings.Join(lines[1:], " "), edgeTestsWidth-len(indent)-len(hang))...)
	}
	for i, line := range lines {
		lead := indent
		if i > 0 {
			lead = indent + hang
		}
		s.line(lead + style(strings.ReplaceAll(line, edgeGlue, " ")))
	}
}

func (s *edgeTestsScreen) flush() {
	text := s.b.String()
	if s.g.ascii {
		text = edgeTestsASCIIText.Replace(text)
	}
	fmt.Fprint(s.env.Stdout, text)
}

func renderEdgeTests(env *Env, view edgeTestsView, all bool) {
	s := newEdgeTestsScreen(env)
	defer s.flush()
	switch view.ForwardTestsUnavailable {
	case "registry_unreadable":
		s.line(env.bold("Forward tests"))
		s.prose("  ", "", "The registered tests could not be read this time. This does not mean none are registered; ask again.", nil)
		return
	case "account_scope_unavailable":
		s.line(env.bold("Forward tests"))
		s.prose("  ", "", "No account is connected, so this account's tests cannot be shown. Ask again once Canary is connected to the broker.", nil)
		return
	}
	tests := edgeTestsOrdered(view.ForwardTests)
	s.axis = edgeTestsAxis(tests)
	if all {
		for i, t := range tests {
			if i > 0 {
				s.line("")
			}
			s.fullView(t)
		}
		return
	}
	s.overview(tests)
}

// edgeTestsRank orders the sections: full, collecting, no count.
func edgeTestsRank(t rpc.EdgeForwardTest) int {
	switch {
	case t.Sample == nil:
		return 2
	case t.Sample.State == rpc.EdgeForwardFull:
		return 0
	}
	return 1
}

func edgeTestsOrdered(in []rpc.EdgeForwardTest) []rpc.EdgeForwardTest {
	tests := slices.Clone(in)
	slices.SortStableFunc(tests, func(a, b rpc.EdgeForwardTest) int {
		if ra, rb := edgeTestsRank(a), edgeTestsRank(b); ra != rb {
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
	return tests
}

// edgeTestsFullness is how far the needed group is toward the number
// needed; a test whose groups are not sorted is at zero.
func edgeTestsFullness(t rpc.EdgeForwardTest) float64 {
	if t.Sample == nil || t.Needs <= 0 || t.NotSorted != "" {
		return 0
	}
	return float64(t.Needs-t.Sample.ToFill) / float64(t.Needs)
}

func (s *edgeTestsScreen) overview(tests []rpc.EdgeForwardTest) {
	env := s.env
	full, next := 0, ""
	for _, t := range tests {
		if t.Sample == nil {
			continue
		}
		if t.Sample.State == rpc.EdgeForwardFull {
			full++
		} else if e := t.Sample.Expected; e != nil && (next == "" || e.Date < next) {
			next = e.Date
		}
	}
	top := []string{edgeTestsCount(len(tests))}
	if full == 0 {
		top = append(top, "none full")
	} else {
		top = append(top, fmt.Sprintf("%d full", full))
	}
	if next != "" {
		top = append(top, "next about "+edgeTestsMonth(next[:7]))
	}
	s.line(env.bold("Forward tests") + "  " + strings.Join(top, " · "))
	s.prose("  ", "", "Questions about your own trading. Each was fixed as a written rule on a set date, and only trading after that date counts. Canary is filling the counts. This version does not work out the answers yet, and nothing here changes a verdict, a warning or an order.", nil)
	if cause := edgeTestsNoCountCause(tests); cause != "" {
		s.cause = true
		s.line("")
		head, rest, _ := strings.Cut(cause, ":")
		s.prose("  ", "", env.yellow(head+":")+rest, nil)
	}
	last := -1
	for _, t := range tests {
		if r := edgeTestsRank(t); r != last {
			s.line("")
			s.heading(r, tests)
			last = r
		}
		s.line("")
		s.block(t, false)
	}
	s.legend(tests)
	s.line("One test in full: canary edge tests <name> · all in full: --all · data: --json")
}

func edgeTestsCount(n int) string {
	return fmt.Sprintf("%d %s", n, pluralWord(n, "test", "tests"))
}

// edgeTestsNoCountCause names the one cause when no test is counted, with
// one action; empty when any test is counted.
func edgeTestsNoCountCause(tests []rpc.EdgeForwardTest) string {
	if len(tests) == 0 || slices.ContainsFunc(tests, func(t rpc.EdgeForwardTest) bool { return t.Sample != nil }) {
		return ""
	}
	for _, t := range tests {
		if t.State == "" {
			return "The running Canary does not count yet: restart it with canary restart."
		}
	}
	for _, t := range tests {
		if t.NotCountedReason == rpc.EdgeForwardNotRegistered {
			return "These tests are not registered yet: they register when Canary next connects to the broker."
		}
	}
	return "Canary has not finished reviewing your trades yet: ask again in a few minutes."
}

// heading is a section word, a faint rule and what the section holds, 79
// columns wide.
func (s *edgeTestsScreen) heading(rank int, tests []rpc.EdgeForwardTest) {
	n := 0
	since := ""
	shared := true
	for _, t := range tests {
		if edgeTestsRank(t) != rank {
			continue
		}
		n++
		date := t.RegisteredAt.Local().Format("2 Jan 2006")
		if since == "" {
			since = date
		} else if since != date {
			shared = false
		}
	}
	word, text := "Collecting", edgeTestsCount(n)
	switch rank {
	case 0:
		word, text = "Full", edgeTestsCount(n)+" · the count is complete; this version does not answer it yet"
	case 2:
		s.line(s.env.bold("No counts yet") + "  " + edgeTestsCount(n))
		return
	}
	if rank == 1 && shared && since != "" {
		text += " · counting since " + since
	}
	dashes := max(3, edgeTestsWidth-len(word)-2-visibleLen(text))
	s.line(s.env.bold(word) + " " + s.env.dim(strings.Repeat(s.g.rule, dashes)) + " " + text)
}

// block draws one test: its question, what it helps with and its name, the
// policy it asks about, what it counts, its rows and the pace.
func (s *edgeTestsScreen) block(t rpc.EdgeForwardTest, full bool) {
	env := s.env
	s.prose("  ", "", sanitizeRunText(firstNonEmptyCLI(t.Question, t.Title)), env.bold)
	if full {
		s.line("    " + edgeTestsHelps(t.Informs) + " · " + edgeTestsPolicy(t))
		if !t.RegisteredAt.IsZero() {
			s.line("    registered " + t.RegisteredAt.Local().Format("2 Jan 2006 15:04 MST") + " · rule text fingerprint " + edgeTestsFingerprint(t.RuleHash))
		}
		s.prose("    ", "  ", edgeTestsNeeds(t), nil)
		condition := "checks nothing first: every trade counts"
		if t.Condition != "" {
			condition = "checks " + sanitizeRunText(t.Condition)
		}
		s.prose("    ", "  ", condition, nil)
	} else {
		helps := "    "
		if len(t.Informs) > 0 {
			helps += edgeTestsHelps(t.Informs)
		}
		s.line(helps + strings.Repeat(" ", max(2, edgeTestsWidth-visibleLen(helps)-len(t.ID))) + t.ID)
		if t.Policy != "" {
			s.line("    policy: " + sanitizeRunText(t.Policy))
		}
	}
	if t.UnitText != "" {
		s.prose("    ", "  ", "counts "+sanitizeRunText(t.UnitText), nil)
	}
	if t.Sample == nil {
		if text := strings.TrimPrefix(s.notCountedLine(t), " · "); text != "" {
			s.prose("    ", "  ", text, nil)
		}
		return
	}
	s.rows(t, full)
}

func edgeTestsHelps(informs []string) string {
	words := map[string]string{"buy": "buying", "hold": "holding", "sell": "selling"}
	parts := make([]string, 0, len(informs))
	for _, i := range informs {
		parts = append(parts, firstNonEmptyCLI(words[i], i))
	}
	if len(parts) == 0 {
		return "helps with no decision named"
	}
	return "helps with " + strings.Join(parts, " or ")
}

func edgeTestsPolicy(t rpc.EdgeForwardTest) string {
	if t.Policy == "" {
		return "tests no limit from your written policy"
	}
	return "tests " + sanitizeRunText(t.Policy)
}

func edgeTestsFingerprint(hash string) string {
	hex := strings.TrimPrefix(hash, "rule_")
	return hex[:min(16, len(hex))]
}

// edgeTestsNeeds says the number the test needs, in which group and on how
// many days.
func edgeTestsNeeds(t rpc.EdgeForwardTest) string {
	text := fmt.Sprintf("needs %d", t.Needs)
	switch t.NeedsGroup {
	case "", "all":
	case "any":
		text += " in either group"
	default:
		text += " " + edgeTestsLabel(t, t.NeedsGroup)
	}
	if t.NeedsDays > 0 {
		text += fmt.Sprintf(", on %d separate days", t.NeedsDays)
	}
	return text
}

func (s *edgeTestsScreen) notCountedLine(t rpc.EdgeForwardTest) string {
	text := edgeTestsNeeds(t)
	if t.Needs <= 0 {
		text = ""
	}
	if s.cause {
		return text
	}
	switch t.NotCountedReason {
	case rpc.EdgeForwardNotRegistered:
		text += " · not registered yet: it registers when Canary next connects to the broker"
	case rpc.EdgeForwardNoReview:
		text += " · counted once Canary has reviewed your trades"
	}
	return text
}

func edgeTestsLabel(t rpc.EdgeForwardTest, id string) string {
	switch id {
	case "all":
		return firstNonEmptyCLI(t.SampleLabel, "trades")
	case "not_sorted":
		return "not sorted yet"
	case "cannot_sort":
		return "cannot be sorted"
	}
	for _, g := range t.Groups {
		if g.ID == id {
			return g.Label
		}
	}
	return id
}

// edgeTestsCounts says whether a group's units count toward the number
// needed.
func edgeTestsCounts(t rpc.EdgeForwardTest, id string) bool {
	if id == "not_sorted" || id == "cannot_sort" {
		return false
	}
	return t.NeedsGroup == "all" || t.NeedsGroup == "any" || t.NeedsGroup == id
}

// row places a label, a mark, a count, "of N" and a note on the grid.
func (s *edgeTestsScreen) row(label, mark, count, of, note string) string {
	line := "    " + label
	if mark != "" {
		line = padRightVisible(line, edgeTestsMarkCol) + mark
	}
	if count != "" {
		line = padRightVisible(line, max(visibleLen(line)+1, edgeTestsCountEnd-visibleLen(count))) + count
	}
	if of != "" {
		line += " of " + of
	}
	if note != "" {
		line = padRightVisible(line, max(visibleLen(line)+2, edgeTestsNoteCol)) + note
	}
	return line
}

// bar is a count at the test's scale: a fill in eighths of a cell, and a
// faint track when the row has a number to reach. A fill without a track
// that runs off the scale ends in the overflow mark.
func (s *edgeTestsScreen) bar(n, of int, track bool) string {
	if of <= 0 {
		return ""
	}
	cells := float64(n) / float64(of) * edgeTestsBar
	over := cells > edgeTestsBar
	cells = math.Min(cells, edgeTestsBar)
	var fill string
	used := 0
	if s.g.ascii {
		used = int(math.Round(cells))
		if n > 0 {
			used = max(used, 1)
		}
		fill = strings.Repeat(s.g.full, used)
	} else {
		eighths := int(math.Round(cells * 8))
		if n > 0 {
			eighths = max(eighths, 1)
		}
		used = eighths / 8
		fill = strings.Repeat(s.g.full, used)
		if part := eighths % 8; part > 0 {
			fill += s.g.eighths[part]
			used++
		}
	}
	if track {
		if rest := edgeTestsBar - used; rest > 0 {
			fill += s.env.dim(strings.Repeat(s.g.track, rest))
		}
		return fill
	}
	s.trackless = s.trackless || n > 0
	if over {
		fill += s.g.over
	}
	return fill
}

func edgeTestsWaiting(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("+%s waiting", edgeCount(n))
}

// rows draws the groups, their notes, the protection sample, the pace and
// a buy made now.
func (s *edgeTestsScreen) rows(t rpc.EdgeForwardTest, full bool) {
	sample := t.Sample
	notWorkedOut := t.NotSorted != ""
	for _, g := range sample.Groups {
		label := edgeTestsLabel(t, g.ID)
		counts := edgeTestsCounts(t, g.ID)
		note := edgeTestsWaiting(g.Waiting)
		switch {
		case notWorkedOut && g.ID != "not_sorted":
			// The test's own groups cannot be counted in this version.
			mark, of := "", ""
			if counts {
				mark, of = s.env.dim(strings.Repeat(s.g.track, edgeTestsBar)), fmt.Sprint(t.Needs)
			}
			s.line(s.row(label, mark, s.g.unknown, of, ""))
			note = ""
		case counts:
			s.line(s.row(label, s.bar(g.Final, t.Needs, true), edgeCount(g.Final), fmt.Sprint(t.Needs), note))
			if t.NeedsDays > 0 && g.ID == t.NeedsGroup {
				s.line(s.row("  on separate days", s.bar(g.Days, t.NeedsDays, true), edgeCount(g.Days), fmt.Sprint(t.NeedsDays), ""))
			}
		case g.Final+g.Waiting > 0:
			s.line(s.row(label, s.bar(g.Final, t.Needs, false), edgeCount(g.Final), "", note))
		default:
			note = ""
		}
		s.waiting = s.waiting || note != ""
		switch {
		case g.ID == "not_sorted":
			s.prose("      ", "", sanitizeRunText(t.NotSorted), nil)
		case g.ID == "cannot_sort" && g.Final+g.Waiting > 0 && t.CannotSort != "":
			s.prose("      ", "", sanitizeRunText(t.CannotSort), nil)
		}
	}
	if t.NeedsGroup == "any" && len(t.Groups) > 1 && sample.State != rpc.EdgeForwardFull {
		s.line(fmt.Sprintf("      full when either group reaches %d", t.Needs))
	}
	if sample.State == rpc.EdgeForwardFull {
		for _, g := range sample.Groups {
			if edgeTestsCounts(t, g.ID) && g.Final >= t.Needs && g.Days >= t.NeedsDays {
				text := fmt.Sprintf("      full: %s reached %d", edgeTestsLabel(t, g.ID), t.Needs)
				if t.NeedsDays > 0 {
					text += fmt.Sprintf(" on %d separate days", g.Days)
				}
				s.line(text)
				break
			}
		}
	}
	// Every sale counts, whoever sent it. Where Canary knows which of the
	// counted sales its protection sent, one row says how many, without a bar.
	if p := sample.SentByProtection; p != nil && p.Final+p.Waiting > 0 {
		note := edgeTestsWaiting(p.Waiting)
		s.waiting = s.waiting || note != ""
		s.protection = true
		s.line(s.row("of these, sent by Canary's protection", "", edgeCount(p.Final), "", note))
	}
	if sample.State != rpc.EdgeForwardFull {
		s.pace(t, full)
	}
	if sample.Today != nil {
		s.prose("    ", "  ", edgeTestsToday(t, *sample.Today), nil)
	}
}

// edgeTestsAxis is the complete months shared by every test on screen: from
// the first month that starts after a registration to the month before the
// count, the last twelve at most.
func edgeTestsAxis(tests []rpc.EdgeForwardTest) []string {
	seen := map[string]bool{}
	var months []string
	for _, t := range tests {
		for _, m := range edgeTestsComplete(t) {
			if !seen[m.Month] {
				seen[m.Month] = true
				months = append(months, m.Month)
			}
		}
	}
	slices.Sort(months)
	if len(months) > edgeTestsMonths {
		months = months[len(months)-edgeTestsMonths:]
	}
	return months
}

// edgeTestsComplete is a test's complete months: each starts after the
// test was registered and ends before the count.
func edgeTestsComplete(t rpc.EdgeForwardTest) []rpc.EdgeForwardMonth {
	if t.Sample == nil || t.RegisteredAt.IsZero() {
		return nil
	}
	registered := t.RegisteredAt.Local().Format("2006-01")
	current := t.Sample.AsOf.Local().Format("2006-01")
	var out []rpc.EdgeForwardMonth
	for _, m := range t.Sample.Months {
		if m.Month > registered && m.Month < current {
			out = append(out, m)
		}
	}
	return out
}

// pace draws the pace row: the complete months on the shared axis once
// three exist, the most in a month, and the month the count is expected to
// be full; or why there is no date.
func (s *edgeTestsScreen) pace(t rpc.EdgeForwardTest, full bool) {
	sample := t.Sample
	expected := sample.Expected
	if expected == nil {
		s.prose("    ", "  ", edgeTestsNoDate(t), nil)
		return
	}
	label := "at last year's pace"
	if expected.Basis == "since_registration" {
		label = "at this pace"
	}
	line := padRightVisible("    "+label, edgeTestsMarkCol)
	if months := edgeTestsComplete(t); len(months) >= 3 {
		finals := map[string]int{}
		peak := 0
		for _, m := range months {
			finals[m.Month] = m.Final
			peak = max(peak, m.Final)
		}
		if peak > 0 {
			var b strings.Builder
			for _, month := range s.axis {
				n, ok := finals[month]
				switch {
				case !ok:
					b.WriteString(" ")
				case n == 0:
					b.WriteString(s.g.noMonth)
				case s.g.ascii && peak <= 9:
					// A month's own count where it fits one digit.
					b.WriteString(fmt.Sprint(n))
				default:
					b.WriteString(s.g.sparks[(n*len(s.g.sparks)-1)/peak])
				}
			}
			if s.spark == "" {
				s.spark = strings.TrimSpace(b.String())
			}
			line += b.String() + fmt.Sprintf("  most %d a month", peak)
		}
	}
	date := "full about " + edgeTestsMonth(expected.Date[:7])
	if expected.BeyondWindow && !full {
		date += " *"
		s.beyond = true
	}
	s.line(padRightVisible(line, max(visibleLen(line)+2, edgeTestsDateCol)) + date)
	if expected.BeyondWindow && full {
		s.prose("      ", "", fmt.Sprintf("more than a year away, and Canary counts only the last %d days: this count cannot fill in this version", sample.WindowDays), nil)
	}
}

// edgeTestsNoDate names what is missing for a date.
func edgeTestsNoDate(t rpc.EdgeForwardTest) string {
	switch t.Sample.NoDate {
	case "group_not_worked_out":
		return "no date for a full count"
	case "not_recorded_before":
		return "no date yet: Canary kept no Regime record before " + t.RegisteredAt.Local().Format("2 Jan 2006") + ", so there is no pace"
	case "none_in_needed_group":
		return "no date yet: no result so far is " + edgeTestsLabel(t, t.NeedsGroup)
	}
	return "no date yet: you made no " + firstNonEmptyCLI(t.PaceNoun, "such trade") + " in the past year to set a pace"
}

// edgeTestsToday says how a buy made now would be counted, from the record
// of the last US close only.
func edgeTestsToday(t rpc.EdgeForwardTest, today rpc.EdgeForwardToday) string {
	day := edgeTestsDay(today.Session)
	if today.Missing != "" {
		return fmt.Sprintf("a buy now cannot be sorted: Canary has no record of the %s close", day)
	}
	if today.PremiumBudgetActPct == nil {
		stage := strings.ReplaceAll(sanitizeRunText(today.RegimeStage), "_", " ")
		if today.Group != "not_sorted" && today.Group != "cannot_sort" {
			return fmt.Sprintf("a buy now counts as %s: Regime was at %s at the %s close", edgeTestsLabel(t, today.Group), stage, day)
		}
		return fmt.Sprintf("a buy now cannot be sorted: Regime was at %s at the %s close", stage, day)
	}
	pct := strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", *today.PremiumBudgetActPct), "0"), ".0") + "%"
	text := fmt.Sprintf("a buy now: the premium budget line was %s of net liquidation value at the %s close", pct, day)
	if today.PremiumBudgetSet != "" {
		text += ", the Rulebook's line for " + strings.ReplaceAll(sanitizeRunText(today.PremiumBudgetSet), "_", " ") + " markets"
	}
	return text
}

func edgeTestsMonth(month string) string {
	at, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	return at.Format("Jan 2006")
}

func edgeTestsDay(value string) string {
	at, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return value
	}
	return at.Format("2 Jan 2006")
}

// legend explains only what the screen above it shows.
func (s *edgeTestsScreen) legend(tests []rpc.EdgeForwardTest) {
	var items []string
	if s.waiting {
		items = append(items, "waiting: a trade whose result is not known yet")
	}
	if s.spark != "" && len(s.axis) > 0 {
		span := edgeTestsMonth(s.axis[0])
		if len(s.axis) > 1 {
			span += "–" + edgeTestsMonth(s.axis[len(s.axis)-1])
		}
		items = append(items, s.spark+" counted per complete month, "+span)
	}
	if s.trackless {
		items = append(items, "a bar without a track has no number to reach")
	}
	s.line("")
	if len(items) > 0 {
		s.prose("  ", "", strings.Join(items, " · "), nil)
	}
	if s.beyond {
		s.prose("  ", "  ", fmt.Sprintf("* more than a year away, and Canary counts only the last %d days: this count cannot fill in this version", edgeTestsWindow(tests)), nil)
	}
	if s.protection {
		s.prose("  ", "  ", "sent by Canary's protection: orders its own protection sent; a stop placed at the broker directly is not recognised", nil)
	}
}

func edgeTestsWindow(tests []rpc.EdgeForwardTest) int {
	for _, t := range tests {
		if t.Sample != nil && t.Sample.WindowDays > 0 {
			return t.Sample.WindowDays
		}
	}
	return 365
}

// fullView is one test in full: its state, the block with its registration
// and needs, what is waiting and what gave no result, the test in short and
// its rule as registered.
func (s *edgeTestsScreen) fullView(t rpc.EdgeForwardTest) {
	state := firstNonEmptyCLI(t.State, "no count yet")
	if state == rpc.EdgeForwardNotCounted {
		state = "no count yet"
	}
	s.line(s.env.bold("Forward tests") + " · " + t.ID + " · " + state)
	s.line("")
	s.block(t, true)
	if sample := t.Sample; sample != nil {
		if n := len(sample.WaitingUnits); n > 0 {
			shown := sample.WaitingUnits[:min(n, 8)]
			parts := make([]string, 0, len(shown))
			for _, u := range shown {
				parts = append(parts, sanitizeRunText(u.Label)+" "+edgeTestsDay(u.Date))
			}
			text := "waiting: " + strings.Join(parts, ", ")
			if n > len(shown) {
				text += fmt.Sprintf(" and %d more", n-len(shown))
			}
			s.prose("    ", "  ", text, nil)
		}
		if len(sample.Excluded) > 0 {
			words := map[string]string{
				"closed_by_delivery": "closed by delivery", "position_not_proved": "with a position the statements do not prove",
				"no_close_stored": "without a stored close", "no_closing_quote": "without a stored closing quote",
				"sold_at_cost": "sold at exactly cost, in neither group", "underlying_not_known": "whose statement names no underlying",
			}
			keys := make([]string, 0, len(sample.Excluded))
			for k := range sample.Excluded {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%d %s", sample.Excluded[k], firstNonEmptyCLI(words[k], strings.ReplaceAll(k, "_", " "))))
			}
			s.prose("    ", "  ", "not counted: "+strings.Join(parts, ", "), nil)
		}
	}
	if t.InShort != "" {
		s.line("")
		s.line("  " + s.env.bold("In short"))
		s.prose("    ", "", sanitizeRunText(t.InShort), nil)
	}
	s.line("")
	s.line("  " + s.env.bold("The rule as registered"))
	s.prose("    ", "", sanitizeRunText(t.Rule), nil)
}
