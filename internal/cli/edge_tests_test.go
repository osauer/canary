package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Synthetic forward-test replies. The rules and their facts are the shipped
// ones, served as the daemon serves them; every count is invented.
var (
	goldenForwardRegistered = time.Date(2026, 11, 21, 7, 0, 0, 0, goldenZone)
	goldenForwardLater      = time.Date(2027, 3, 19, 23, 0, 0, 0, goldenZone)
)

// goldenForwardRules serves the seven rules with their facts, registered,
// and without a count.
func goldenForwardRules() []rpc.EdgeForwardTest {
	var out []rpc.EdgeForwardTest
	for _, rule := range edgecore.ForwardTests() {
		facts, _ := edgecore.ForwardTestFacts(rule.ID)
		test := rpc.EdgeForwardTest{ID: rule.ID, Title: rule.Title, Rule: rule.Rule, RuleHash: rule.RuleHash(), RegisteredAt: goldenForwardRegistered,
			Question: facts.Question, Informs: facts.Informs, Condition: facts.Condition, Unit: facts.Unit, UnitText: facts.UnitText,
			ReadAt: facts.ReadAt, ReadGroup: facts.ReadGroup, ReadDays: facts.ReadDays, SampleLabel: facts.SampleLabel, Policy: facts.Policy,
			ProtectionApart: facts.ProtectionApart, GroupNotRead: facts.GroupNotRead}
		for _, g := range facts.Groups {
			test.Groups = append(test.Groups, rpc.EdgeForwardGroup{ID: g.ID, Label: g.Label})
		}
		out = append(out, test)
	}
	return out
}

// goldenForwardCount is one rule's synthetic count: final and pending per
// group in the rule's group order (not_read last where the rule has it).
type goldenForwardCount struct {
	groups     [][2]int
	days       int
	protection *[2]int
	unknown    int
	months     []int
	expected   string
	basis      string
	beyond     bool
	noDate     string
	excluded   map[string]int
	first      string
	last       string
}

func goldenForwardSample(test rpc.EdgeForwardTest, c goldenForwardCount, asOf time.Time) *rpc.EdgeForwardSample {
	s := &rpc.EdgeForwardSample{State: rpc.EdgeForwardCollecting, AsOf: asOf, WindowDays: 365, Excluded: map[string]int{}, Months: []rpc.EdgeForwardMonth{},
		SenderUnknown: c.unknown, NoDate: c.noDate, FirstUnit: c.first, LastUnit: c.last}
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
	if test.GroupNotRead != "" || len(c.groups) > len(ids) {
		ids = append(ids, "not_read")
	}
	have := 0
	for i, id := range ids {
		var n [2]int
		if i < len(c.groups) {
			n = c.groups[i]
		}
		g := rpc.EdgeForwardGroupCount{ID: id, Final: n[0], Pending: n[1]}
		if id == test.ReadGroup {
			g.Days = c.days
		}
		s.Groups = append(s.Groups, g)
		s.Test.Final += n[0]
		s.Test.Pending += n[1]
		if id != "not_read" && (test.ReadGroup == "all" || test.ReadGroup == "any" || test.ReadGroup == id) {
			have = max(have, n[0])
		}
	}
	s.ToRead = max(test.ReadAt-have, 0)
	if have >= test.ReadAt {
		s.State = rpc.EdgeForwardDue
	}
	if test.ProtectionApart {
		s.Protection = &rpc.EdgeForwardCount{}
		if c.protection != nil {
			s.Protection.Final, s.Protection.Pending = c.protection[0], c.protection[1]
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
	return s
}

func goldenForwardReply(counts map[string]goldenForwardCount, asOf time.Time, today bool) rpc.EdgeResult {
	result := edgeStateResultForTests()
	result.ForwardTests = goldenForwardRules()
	for i := range result.ForwardTests {
		test := &result.ForwardTests[i]
		c, ok := counts[test.ID]
		if !ok {
			continue
		}
		test.Sample = goldenForwardSample(*test, c, asOf)
		if !today {
			continue
		}
		switch test.ID {
		case "option_buys_after_regime_warning":
			test.Sample.Today = &rpc.EdgeForwardToday{Session: asOf.Format(time.DateOnly), RegimeStage: "early_warning", Group: "after_warning"}
		case "option_buys_below_premium_line":
			test.Sample.Today = &rpc.EdgeForwardToday{Session: asOf.Format(time.DateOnly), PremiumBudgetActPct: new(2.5), PremiumBudgetSet: "calm", Group: "not_read"}
		}
	}
	return result
}

// edgeStateResultForTests is a valid current Edge reply with nothing in it.
func edgeStateResultForTests() rpc.EdgeResult {
	result := edgeCLIResult()
	result.Window = "365d"
	return result
}

// goldenForwardEmpty is the normal state for months: every rule registered
// a day ago, nothing final, dates from the past year's pace where one can
// be measured.
func goldenForwardEmpty() rpc.EdgeResult {
	return goldenForwardReply(map[string]goldenForwardCount{
		"stock_sales_vs_nearby_day":              {groups: [][2]int{{0, 1}}, expected: "2027-09-14", basis: "past_year"},
		"entries_vs_nearby_day":                  {expected: "2028-06-02", basis: "past_year", beyond: true},
		"option_buys_below_premium_line":         {noDate: "group_not_read"},
		"option_buys_after_regime_warning":       {noDate: "not_recorded_before"},
		"final_option_sales_vs_close_5_sessions": {expected: "2027-05-20", basis: "past_year"},
		"floor_after_partial_sale":               {noDate: "no_pace"},
		"option_sales_after_adverse_move":        {noDate: "group_not_read"},
	}, goldenForwardRegistered.AddDate(0, 0, 1), true)
}

// goldenForwardPartly is four months on: every rule part filled.
func goldenForwardPartly() rpc.EdgeResult {
	return goldenForwardReply(goldenForwardPartlyCounts(), goldenForwardLater, true)
}

func goldenForwardPartlyCounts() map[string]goldenForwardCount {
	return map[string]goldenForwardCount{
		"stock_sales_vs_nearby_day": {groups: [][2]int{{22, 3}}, protection: &[2]int{4, 1}, months: []int{1, 6, 4, 7, 4}, expected: "2027-12-03", basis: "since_registration",
			first: "2026-11-24", last: "2027-03-16"},
		"entries_vs_nearby_day": {groups: [][2]int{{41, 2}}, months: []int{2, 11, 9, 12, 7}, expected: "2028-02-10", basis: "since_registration", beyond: true,
			first: "2026-11-23", last: "2027-03-17"},
		"option_buys_below_premium_line": {groups: [][2]int{{0, 0}, {0, 0}, {9, 4}}, months: []int{0, 3, 1, 3, 2}, noDate: "group_not_read",
			first: "2026-12-02", last: "2027-03-15", excluded: map[string]int{"closed_by_delivery": 1}},
		"option_buys_after_regime_warning": {groups: [][2]int{{6, 2}, {14, 3}, {2, 0}}, days: 5, months: []int{0, 5, 3, 8, 6}, expected: "2028-04-21", basis: "since_registration", beyond: true,
			first: "2026-12-02", last: "2027-03-15"},
		"final_option_sales_vs_close_5_sessions": {groups: [][2]int{{9, 1}, {13, 0}}, protection: &[2]int{2, 0}, months: []int{0, 4, 6, 7, 5}, expected: "2027-06-28", basis: "since_registration",
			first: "2026-12-09", last: "2027-03-11", excluded: map[string]int{"no_closing_quote": 3, "sold_at_cost": 1}},
		"floor_after_partial_sale": {groups: [][2]int{{7, 5}}, months: []int{0, 1, 2, 2, 2}, expected: "2028-03-01", basis: "since_registration", beyond: true,
			first: "2026-12-04", last: "2027-03-02"},
		"option_sales_after_adverse_move": {groups: [][2]int{{0, 0}, {0, 0}, {18, 2}}, protection: &[2]int{3, 0}, months: []int{1, 4, 5, 4, 4}, noDate: "group_not_read",
			first: "2026-11-27", last: "2027-03-12"},
	}
}

// goldenForwardDue has one rule's group at its read point.
func goldenForwardDue() rpc.EdgeResult {
	counts := goldenForwardPartlyCounts()
	due := counts["final_option_sales_vs_close_5_sessions"]
	due.groups = [][2]int{{17, 1}, {30, 2}}
	due.months = []int{0, 8, 11, 14, 14}
	counts["final_option_sales_vs_close_5_sessions"] = due
	return goldenForwardReply(counts, goldenForwardLater, true)
}

// goldenForwardRead puts a synthetic reading on one rule, to prove the
// interval renderer this build reaches only once a reading exists.
func goldenForwardRead() rpc.EdgeResult {
	counts := goldenForwardPartlyCounts()
	read := counts["option_buys_after_regime_warning"]
	read.groups, read.days = [][2]int{{40, 0}, {97, 4}, {2, 0}}, 24
	counts["option_buys_after_regime_warning"] = read
	result := goldenForwardReply(counts, goldenForwardLater, false)
	for i := range result.ForwardTests {
		if test := &result.ForwardTests[i]; test.ID == "option_buys_after_regime_warning" {
			test.Sample.State = rpc.EdgeForwardRead
			test.Sample.Reading = &rpc.EdgeForwardReading{ReadAt: goldenForwardLater, Tone: "unfavourable",
				Finding:   "Buys after a warning returned less on their premium than the other buys, in both halves.",
				Measure:   "return on premium paid, 95% interval",
				Intervals: []rpc.EdgeForwardInterval{{Group: "after_warning", Low: -48, Estimate: -21, High: 6}, {Group: "other", Low: 2, Estimate: 14, High: 27}}}
		}
	}
	return result
}

// goldenForwardUncounted is a reply from a daemon that serves the rules
// without counting them.
func goldenForwardUncounted() rpc.EdgeResult {
	return goldenForwardReply(nil, goldenForwardRegistered, false)
}

// Every rendered line fits 80 columns and every bar has the same width, so
// rules compare at a glance and nothing wraps in a terminal.
func TestEdgeTestsFitEightyColumnsWithBarsAligned(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	for name, result := range map[string]rpc.EdgeResult{"empty": goldenForwardEmpty(), "partly": goldenForwardPartly(), "due": goldenForwardDue(), "read": goldenForwardRead()} {
		for _, all := range []bool{false, true} {
			var out bytes.Buffer
			env := &Env{Stdout: &out, Color: true}
			renderEdgeTests(env, edgeTestsView{ForwardTests: result.ForwardTests}, all)
			barStart := -1
			for line := range strings.SplitSeq(out.String(), "\n") {
				if n := visibleLen(line); n > 80 {
					t.Errorf("%s: %d cells: %q", name, n, line)
				}
				plain := stripANSIForTest(line)
				if strings.Contains(plain, "final per month") {
					continue
				}
				if i := strings.IndexAny(plain, "█░▏▎▍▌▋▊▉"); i >= 0 {
					cells := []rune(plain)
					col := len([]rune(plain[:i]))
					if barStart < 0 {
						barStart = col
					}
					width := 0
					for _, r := range cells[barStart:min(len(cells), barStart+edgeTestsBar)] {
						if strings.ContainsRune("█░▏▎▍▌▋▊▉", r) {
							width++
						}
					}
					if col != barStart || width != edgeTestsBar {
						t.Errorf("%s: bar at column %d width %d, want %d and %d: %q", name, col, width, barStart, edgeTestsBar, plain)
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

// The JSON of one rule carries what an automation reads: the decisions it
// informs, the checkable fact (empty for a rule that conditions on
// nothing), the state and the counts.
func TestEdgeTestsJSONForOneRule(t *testing.T) {
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
	rule := view.ForwardTests[0]
	sample, _ := rule["sample"].(map[string]any)
	if rule["condition"] != "" || rule["informs"].([]any)[0] != "buy" || sample["state"] != "collecting" || sample["reading_built"] != false {
		t.Fatalf("rule json = %s", stdout.String())
	}
	if code := Run(t.Context(), env, "edge", []string{"tests", "no_such_rule"}); code == 0 {
		t.Fatal("an unknown rule id was accepted")
	}
}
