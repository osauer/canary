package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Synthetic forward-test replies. The tests and their facts are the shipped
// ones, served as the daemon serves them; every count is invented.
var (
	goldenForwardRegistered = time.Date(2026, 11, 21, 7, 0, 0, 0, goldenZone)
	goldenForwardLater      = time.Date(2027, 3, 19, 23, 0, 0, 0, goldenZone)
)

// goldenForwardRules serves the seven tests with their facts, registered,
// and without a count.
func goldenForwardRules() []rpc.EdgeForwardTest {
	var out []rpc.EdgeForwardTest
	for _, rule := range edgecore.ForwardTests() {
		facts, _ := edgecore.ForwardTestFacts(rule.ID)
		test := rpc.EdgeForwardTest{ID: rule.ID, Title: rule.Title, Rule: rule.Rule, RuleHash: rule.RuleHash(), RegisteredAt: goldenForwardRegistered,
			Question: facts.Question, Informs: facts.Informs, Condition: facts.Condition, Unit: facts.Unit, UnitText: facts.UnitText,
			Needs: facts.Needs, NeedsGroup: facts.NeedsGroup, NeedsDays: facts.NeedsDays, SampleLabel: facts.SampleLabel, Policy: facts.Policy,
			AboutSales: facts.AboutSales, NotSorted: facts.NotSorted, CannotSort: facts.CannotSort, PaceNoun: facts.PaceNoun, InShort: facts.InShort}
		for _, g := range facts.Groups {
			test.Groups = append(test.Groups, rpc.EdgeForwardGroup{ID: g.ID, Label: g.Label})
		}
		out = append(out, test)
	}
	return out
}

// goldenForwardCount is one test's synthetic count: final and waiting per
// group in the test's group order (not_sorted, then cannot_sort, last).
type goldenForwardCount struct {
	groups     [][2]int
	cannot     *[2]int
	days       int
	protection *[2]int
	months     []int
	expected   string
	basis      string
	beyond     bool
	noDate     string
	excluded   map[string]int
	waiting    []rpc.EdgeForwardUnit
}

func goldenForwardSample(test rpc.EdgeForwardTest, c goldenForwardCount, asOf time.Time) *rpc.EdgeForwardSample {
	s := &rpc.EdgeForwardSample{State: rpc.EdgeForwardCollecting, AsOf: asOf, WindowDays: 365, Excluded: map[string]int{}, Months: []rpc.EdgeForwardMonth{},
		NoDate: c.noDate, CanFill: true, WaitingUnits: c.waiting, NotCounted: []rpc.EdgeForwardUnit{}}
	if c.waiting == nil {
		s.WaitingUnits = []rpc.EdgeForwardUnit{}
	}
	if c.excluded != nil {
		s.Excluded = c.excluded
	}
	ids := []string{"all"}
	if len(test.Groups) > 0 {
		ids = ids[:0]
		for _, g := range test.Groups {
			ids = append(ids, g.ID)
		}
	}
	if test.NotSorted != "" {
		ids = append(ids, "not_sorted")
	}
	have := 0
	for i, id := range ids {
		var n [2]int
		if i < len(c.groups) {
			n = c.groups[i]
		}
		g := rpc.EdgeForwardGroupCount{ID: id, Final: n[0], Waiting: n[1]}
		if id == test.NeedsGroup {
			g.Days = c.days
		}
		s.Groups = append(s.Groups, g)
		s.Test.Final += n[0]
		s.Test.Waiting += n[1]
		if id != "not_sorted" && (test.NeedsGroup == "all" || test.NeedsGroup == "any" || test.NeedsGroup == id) {
			have = max(have, n[0])
		}
	}
	if c.cannot != nil {
		s.Groups = append(s.Groups, rpc.EdgeForwardGroupCount{ID: "cannot_sort", Final: c.cannot[0], Waiting: c.cannot[1]})
		s.Test.Final += c.cannot[0]
		s.Test.Waiting += c.cannot[1]
	}
	s.ToFill = max(test.Needs-have, 0)
	if have >= test.Needs {
		s.State = rpc.EdgeForwardFull
	}
	// The protection evidence is current in these replies: a sale test says
	// how many of its counted units Canary's protection sent.
	if test.AboutSales {
		s.SentByProtection = &rpc.EdgeForwardCount{}
		if c.protection != nil {
			s.SentByProtection.Final, s.SentByProtection.Waiting = c.protection[0], c.protection[1]
		}
	}
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; !month.After(asOf); i++ {
		n := 0
		if i < len(c.months) {
			n = c.months[i]
		}
		s.Months = append(s.Months, rpc.EdgeForwardMonth{Month: month.Format("2006-01"), Final: n})
		month = month.AddDate(0, 1, 0)
	}
	if c.expected != "" && s.State == rpc.EdgeForwardCollecting {
		s.Expected = &rpc.EdgeForwardExpected{Date: c.expected, Basis: c.basis, BeyondWindow: c.beyond}
	}
	switch {
	case test.NotSorted != "":
		s.CanFill, s.CannotFill = false, "group_not_worked_out"
	case s.Expected != nil && s.Expected.BeyondWindow:
		s.CanFill, s.CannotFill = false, "beyond_window"
	}
	return s
}

func goldenForwardReply(counts map[string]goldenForwardCount, asOf time.Time, today string) rpc.EdgeResult {
	result := edgeCLIResult()
	result.Window = "365d"
	result.ForwardTests = goldenForwardRules()
	for i := range result.ForwardTests {
		test := &result.ForwardTests[i]
		c, ok := counts[test.ID]
		if !ok {
			continue
		}
		test.Sample = goldenForwardSample(*test, c, asOf)
		test.State = test.Sample.State
		if today == "" {
			continue
		}
		switch test.ID {
		case "option_buys_after_regime_warning":
			test.Sample.Today = &rpc.EdgeForwardToday{Session: today, RegimeStage: "early_warning", Group: "after_warning"}
		case "option_buys_below_premium_line":
			test.Sample.Today = &rpc.EdgeForwardToday{Session: today, PremiumBudgetActPct: new(2.5), PremiumBudgetSet: "calm", Group: "not_sorted"}
		}
	}
	return result
}

// goldenForwardEmpty is the normal state for months: every test registered
// a day ago, nothing with a result yet, dates from last year's pace where
// one can be measured. The last close before registration is a Friday.
func goldenForwardEmpty() rpc.EdgeResult {
	return goldenForwardReply(map[string]goldenForwardCount{
		"stock_sales_vs_nearby_day":              {groups: [][2]int{{0, 1}}, expected: "2027-09-14", basis: "past_year"},
		"entries_vs_nearby_day":                  {expected: "2028-06-02", basis: "past_year", beyond: true},
		"option_buys_below_premium_line":         {noDate: "group_not_worked_out"},
		"option_buys_after_regime_warning":       {noDate: "not_recorded_before"},
		"final_option_sales_vs_close_5_sessions": {expected: "2027-05-20", basis: "past_year"},
		"floor_after_partial_sale":               {noDate: "no_pace"},
		"option_sales_after_adverse_move":        {noDate: "group_not_worked_out"},
	}, goldenForwardRegistered.AddDate(0, 0, 1), "2026-11-20")
}

// goldenForwardNoDate is the day after registration with no date from a
// test that can fill: the only date is one the screen marks as unable to
// fill, so the first line names none.
func goldenForwardNoDate() rpc.EdgeResult {
	return goldenForwardReply(map[string]goldenForwardCount{
		"stock_sales_vs_nearby_day":              {noDate: "no_pace"},
		"entries_vs_nearby_day":                  {expected: "2028-03-02", basis: "past_year", beyond: true},
		"option_buys_below_premium_line":         {noDate: "group_not_worked_out"},
		"option_buys_after_regime_warning":       {noDate: "not_recorded_before"},
		"final_option_sales_vs_close_5_sessions": {noDate: "no_pace"},
		"floor_after_partial_sale":               {noDate: "no_pace"},
		"option_sales_after_adverse_move":        {noDate: "group_not_worked_out"},
	}, goldenForwardRegistered.AddDate(0, 0, 1), "2026-11-20")
}

// goldenForwardPartly is four months on: every test part filled.
func goldenForwardPartly() rpc.EdgeResult {
	return goldenForwardReply(goldenForwardPartlyCounts(), goldenForwardLater, "2027-03-19")
}

func goldenForwardPartlyCounts() map[string]goldenForwardCount {
	return map[string]goldenForwardCount{
		"stock_sales_vs_nearby_day": {groups: [][2]int{{22, 3}}, protection: &[2]int{4, 1}, months: []int{1, 6, 4, 7, 4}, expected: "2027-12-03", basis: "since_registration"},
		"entries_vs_nearby_day":     {groups: [][2]int{{41, 2}}, months: []int{2, 11, 9, 12, 7}, expected: "2028-02-10", basis: "since_registration", beyond: true},
		"option_buys_below_premium_line": {groups: [][2]int{{0, 0}, {0, 0}, {9, 4}}, noDate: "group_not_worked_out",
			excluded: map[string]int{"closed_by_delivery": 1}},
		"option_buys_after_regime_warning": {groups: [][2]int{{6, 2}, {14, 3}}, cannot: &[2]int{2, 0}, days: 5, months: []int{0, 2, 1, 2, 1}, expected: "2028-04-21", basis: "since_registration", beyond: true,
			waiting: []rpc.EdgeForwardUnit{{Key: "k1", Label: "SYNA 270115C00050000", Date: "2027-03-15", Group: "after_warning"}, {Key: "k2", Label: "SYNB 270219C00030000", Date: "2027-03-16", Group: "other"}}},
		"final_option_sales_vs_close_5_sessions": {groups: [][2]int{{9, 1}, {13, 0}}, protection: &[2]int{2, 0}, months: []int{0, 4, 6, 7, 5}, expected: "2027-06-28", basis: "since_registration",
			excluded: map[string]int{"no_closing_quote": 3, "sold_at_cost": 1}},
		"floor_after_partial_sale":        {groups: [][2]int{{7, 5}}, months: []int{0, 1, 2, 2, 2}, expected: "2028-03-01", basis: "since_registration", beyond: true},
		"option_sales_after_adverse_move": {groups: [][2]int{{0, 0}, {0, 0}, {18, 2}}, protection: &[2]int{3, 0}, noDate: "group_not_worked_out"},
	}
}

// goldenForwardFull has one test whose count is complete.
func goldenForwardFull() rpc.EdgeResult {
	counts := goldenForwardPartlyCounts()
	full := counts["final_option_sales_vs_close_5_sessions"]
	full.groups = [][2]int{{17, 1}, {30, 2}}
	full.months = []int{0, 8, 11, 14, 14}
	counts["final_option_sales_vs_close_5_sessions"] = full
	return goldenForwardReply(counts, goldenForwardLater, "2027-03-19")
}

// goldenForwardNotRegistered is a reply in which no test is registered yet.
func goldenForwardNotRegistered() rpc.EdgeResult {
	result := goldenForwardReply(nil, goldenForwardRegistered, "")
	for i := range result.ForwardTests {
		result.ForwardTests[i].RegisteredAt = time.Time{}
		result.ForwardTests[i].State, result.ForwardTests[i].NotCountedReason = rpc.EdgeForwardNotCounted, rpc.EdgeForwardNotRegistered
	}
	return result
}

// goldenForwardOlderDaemon is a reply from a daemon that serves the tests
// without counting them: no state, no facts.
func goldenForwardOlderDaemon() rpc.EdgeResult {
	result := goldenForwardReply(nil, goldenForwardRegistered, "")
	for i, rule := range edgecore.ForwardTests() {
		result.ForwardTests[i] = rpc.EdgeForwardTest{ID: rule.ID, Title: rule.Title, Rule: rule.Rule, RuleHash: rule.RuleHash(), RegisteredAt: goldenForwardRegistered}
	}
	return result
}

// TestRenderGoldenEdgeTests pins `canary edge tests` at 80 columns with
// colour off and on, in a UTF-8 locale and, for one state, in an ASCII one.
// Every value is synthetic.
func TestRenderGoldenEdgeTests(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	for _, tc := range []struct {
		name   string
		argv   []string
		result rpc.EdgeResult
		locale string
	}{
		{"empty", []string{"edge", "tests"}, goldenForwardEmpty(), "en_US.UTF-8"},
		{"partly", []string{"edge", "tests"}, goldenForwardPartly(), "en_US.UTF-8"},
		{"full", []string{"edge", "tests"}, goldenForwardFull(), "en_US.UTF-8"},
		{"not_registered", []string{"edge", "tests"}, goldenForwardNotRegistered(), "en_US.UTF-8"},
		{"older_daemon", []string{"edge", "tests"}, goldenForwardOlderDaemon(), "en_US.UTF-8"},
		{"one_test", []string{"edge", "tests", "option_buys_after_regime_warning"}, goldenForwardPartly(), "en_US.UTF-8"},
		{"partly_ascii", []string{"edge", "tests"}, goldenForwardPartly(), "C"},
		{"no_date", []string{"edge", "tests"}, goldenForwardNoDate(), "en_US.UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LC_ALL", tc.locale)
			var doc strings.Builder
			for _, color := range []bool{false, true} {
				mode := "colour off"
				if color {
					mode = `colour on, ESC written as \x1b`
				}
				fmt.Fprintf(&doc, "# canary %s · COLUMNS=80 · LC_ALL=%s · %s\n", strings.Join(tc.argv, " "), tc.locale, mode)
				out := renderGolden(t, renderGoldenCase{name: tc.name, argv: tc.argv, conn: goldenConn{rpc.MethodEdgeSnapshot: tc.result}}, color)
				if tc.locale == "C" && strings.ContainsFunc(stripANSIForTest(out), func(r rune) bool { return r > 127 }) {
					t.Fatalf("ASCII locale output carries a non-ASCII character:\n%s", out)
				}
				doc.WriteString(goldenText(out))
			}
			compareGolden(t, filepath.Join("testdata", "golden_edge_tests", tc.name+".txt"), doc.String())
		})
	}
}

// Every row stays left of column 79, every mark starts at column 29 and
// every bar with a track is 24 cells, so tests compare at a glance and
// nothing wraps in a terminal.
func TestEdgeTestsKeepTheGrid(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	for name, result := range map[string]rpc.EdgeResult{"empty": goldenForwardEmpty(), "partly": goldenForwardPartly(), "full": goldenForwardFull(), "no_date": goldenForwardNoDate()} {
		for _, all := range []bool{false, true} {
			var out bytes.Buffer
			renderEdgeTests(&Env{Stdout: &out, Color: true}, edgeTestsView{ForwardTests: result.ForwardTests}, all)
			for line := range strings.SplitSeq(out.String(), "\n") {
				if n := visibleLen(line); n > 79 {
					t.Errorf("%s: %d cells: %q", name, n, line)
				}
				plain := []rune(stripANSIForTest(line))
				if i := strings.IndexRune(string(plain), '░'); i >= 0 && !strings.Contains(string(plain), "most ") {
					start := strings.IndexAny(string(plain), "█░▏▎▍▌▋▊▉")
					col := len([]rune(string(plain)[:start]))
					width := 0
					for _, r := range plain[col:min(len(plain), col+edgeTestsBar)] {
						if strings.ContainsRune("█░▏▎▍▌▋▊▉", r) {
							width++
						}
					}
					if col != edgeTestsMarkCol || width != edgeTestsBar {
						t.Errorf("%s: bar at column %d width %d: %q", name, col, width, string(plain))
					}
				}
			}
		}
	}
}

func stripANSIForTest(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case in:
			in = r != 'm'
		case r == '\x1b':
			in = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// The JSON of one test carries what an automation reads: the decisions it
// helps with, the checkable fact (empty for a test that conditions on
// nothing), the state, the counts and whether it can fill.
func TestEdgeTestsJSONForOneTest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: goldenConn{rpc.MethodEdgeSnapshot: goldenForwardPartly()}}
	if code := Run(t.Context(), env, "edge", []string{"tests", "entries_vs_nearby_day", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var view struct {
		ForwardTests []map[string]any `json:"forward_tests"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil || len(view.ForwardTests) != 1 {
		t.Fatalf("json=%s err=%v", stdout.String(), err)
	}
	test := view.ForwardTests[0]
	sample, _ := test["sample"].(map[string]any)
	if test["condition"] != "" || test["informs"].([]any)[0] != "buy" || test["state"] != "collecting" || sample["answers_built"] != false || sample["can_fill"] != false || sample["cannot_fill"] != "beyond_window" {
		t.Fatalf("test json = %s", stdout.String())
	}
	if code := Run(t.Context(), env, "edge", []string{"tests", "no_such_test"}); code == 0 {
		t.Fatal("an unknown test name was accepted")
	}
}
