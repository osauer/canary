package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

type edgeCLIConn struct {
	method string
	params rpc.EdgeSnapshotParams
	result rpc.EdgeResult
}

func (c *edgeCLIConn) Call(_ context.Context, method string, params, out any) error {
	c.method = method
	c.params = params.(rpc.EdgeSnapshotParams)
	result := c.result
	result.Window = c.params.Window
	result.HorizonSessions = c.params.HorizonSessions
	result.AutomaticHorizon = c.params.AutomaticHorizon
	result.HorizonSelection.Mode = "explicit"
	result.HorizonSelection.Reason = "explicit_override"
	if c.params.AutomaticHorizon {
		result.HorizonSelection.Mode = "automatic"
		result.HorizonSelection.Reason = "best_available"
	}
	result.HorizonSelection.ScoredChanges = result.Coverage.ScoredByHorizon[c.params.HorizonSessions]
	result.HorizonSelection.CoveragePct = float64(result.HorizonSelection.ScoredChanges) / float64(result.HorizonSelection.EligibleChanges) * 100
	for i := range result.Findings {
		result.Findings[i].HorizonSessions = c.params.HorizonSessions
	}
	*out.(*rpc.EdgeResult) = result
	return nil
}

func (*edgeCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestEdgeCLIUsesBoundedTypedRPCParameters(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	args := []string{"--window", "365d", "--horizon", "5", "--limit", "2", "--change", "change_opaque", "--json"}
	if code := Run(t.Context(), env, "edge", args); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if conn.method != rpc.MethodEdgeSnapshot {
		t.Fatalf("method=%q", conn.method)
	}
	want := (rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 5, Limit: 2, OptionLimit: rpc.MaxEdgeOptionResults, ChangeID: "change_opaque"})
	if conn.params != want {
		t.Fatalf("params=%+v want %+v", conn.params, want)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"not_execution": true`)) {
		t.Fatalf("missing not_execution marker: %s", stdout.String())
	}
}

func TestEdgeCLIForwardsOpaqueOptionDetail(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", []string{"--option", "option_opaque", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings, OptionLimit: rpc.MaxEdgeOptionResults, OptionID: "option_opaque"}
	if conn.params != want {
		t.Fatalf("params=%+v want %+v", conn.params, want)
	}
}

func TestEdgeCLIDefaultsToTheAutomaticOneYearReview(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings, OptionLimit: rpc.MaxEdgeOptionResults}
	if conn.params != want {
		t.Fatalf("default params=%+v want %+v", conn.params, want)
	}
	// The bare command answers first: the period reviewed, then the bottom
	// line, with the full review named as the next step.
	title, rest, _ := strings.Cut(stdout.String(), "\n")
	if !strings.Contains(title, "one year of decisions") || !strings.Contains(rest, "Bottom line") || !strings.Contains(rest, "canary edge --details") {
		t.Fatalf("default output does not lead with the conclusion: %s", stdout.String())
	}
}

func TestDefaultEdgeHumanOutputIsConciseAndComplete(t *testing.T) {
	result := edgeCLIResult()
	var summary, details bytes.Buffer
	renderEdgeSummary(&Env{Stdout: &summary}, result)
	renderEdgeText(&Env{Stdout: &details}, result)
	// The summary is one screen: the cap counts content lines.
	content := 0
	for line := range strings.SplitSeq(strings.TrimSpace(summary.String()), "\n") {
		if strings.TrimSpace(line) != "" {
			content++
		}
	}
	if content > 24 {
		t.Fatalf("summary has %d content lines, want at most 24:\n%s", content, summary.String())
	}
	for _, required := range []string{"Canary Edge", "Bottom line", "Account", "How far to trust this", "Full review: canary edge --details"} {
		if !strings.Contains(summary.String(), required) {
			t.Errorf("summary missing %q:\n%s", required, summary.String())
		}
	}
	// The detailed view keeps every section the review has.
	for _, required := range []string{"Canary Edge", "Bottom line", "Evidence", "Coverage", "Horizon", "Account P/L", "Decisions by action", "OPEN", "ADD", "TRIM", "EXIT", "Findings", "Realized episodes", "Open snapshot", "Details: canary edge"} {
		if !strings.Contains(details.String(), required) {
			t.Errorf("detailed view missing %q:\n%s", required, details.String())
		}
	}
}

func TestEdgeHumanOutputDistinguishesConfirmedEmptyFromMissingOptionSnapshot(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.Options = rpc.EdgeOptionReview{
		Realized: rpc.EdgeOptionRealizedReview{Episodes: []rpc.EdgeOptionEpisodeSummary{}},
		Open:     rpc.EdgeOptionOpenReview{SnapshotDate: result.AsOf, Positions: []rpc.EdgeOptionOpenPositionSummary{}},
	}

	var stdout bytes.Buffer
	renderEdgeText(&Env{Stdout: &stdout}, result)
	if !strings.Contains(stdout.String(), "Open snapshot "+result.AsOf.Format(time.DateOnly)+" · 0 positions · confirmed empty") || strings.Contains(stdout.String(), "no dated Flex") {
		t.Fatalf("confirmed-empty snapshot output=%s", stdout.String())
	}

	result.Options.Open.SnapshotDate = time.Time{}
	stdout.Reset()
	renderEdgeText(&Env{Stdout: &stdout}, result)
	if !strings.Contains(stdout.String(), "Open snapshot · no dated Flex Open Positions snapshot available") || strings.Contains(stdout.String(), "confirmed empty") {
		t.Fatalf("missing snapshot output=%s", stdout.String())
	}
}

func TestAutomaticOneSessionOutputUsesSingularGrammar(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.AutomaticHorizon = true
	result.HorizonSessions = 1
	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 6, ScoredChanges: 6, CoveragePct: 100, LargestActionSample: 3, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true}
	var stdout bytes.Buffer
	renderEdgeText(&Env{Stdout: &stdout}, result)
	if strings.Contains(stdout.String(), "1 sessions") || !strings.Contains(stdout.String(), "selected 1 session") || !strings.Contains(stdout.String(), "at 1 session") {
		t.Fatalf("one-session grammar=%s", stdout.String())
	}
}

func TestEdgeSetupNamesProvenMissingQueryRequirements(t *testing.T) {
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v3", State: rpc.EdgeStateActionRequired, Reason: "flex_query_incomplete", Window: "90d", HorizonSessions: 20,
		Setup: &rpc.EdgeSetup{Steps: []string{"one", "two", "three"}, MissingRequirements: []string{"trades.ibOrderID", "open_positions.markPrice"}},
	}
	var stdout bytes.Buffer
	renderEdgeText(&Env{Stdout: &stdout}, result)
	for _, requirement := range result.Setup.MissingRequirements {
		if !strings.Contains(stdout.String(), requirement) {
			t.Fatalf("setup output omitted %q: %s", requirement, stdout.String())
		}
	}
}

func edgeCLIResult() rpc.EdgeResult {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	gain, loss, open, realizedTotal := 125.5, -50.0, -40.0, 75.5
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v3", State: rpc.EdgeStateCurrent, AsOf: now, Window: "90d", HorizonSessions: 20,
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "explicit", Reason: "explicit_override", EligibleChanges: 6, ScoredChanges: 4, CoveragePct: 4.0 / 6 * 100, LargestActionSample: 1, MinimumSample: 3, MinimumCoveragePct: 25},
		Headline:         "Best observed result: ABC add, USD +125.50 after 20 sessions.",
		Account:          &rpc.EdgeAccountResult{BaseCurrency: "USD", RequestedFrom: now.AddDate(0, 0, -90), ActualFrom: now.AddDate(0, 0, -89), ActualTo: now, ProfitLossBase: 450, ExternalFlowsBase: 100, Definition: "Ending equity minus starting equity minus external flows."},
		Coverage:         rpc.EdgeCoverage{TradeChanges: 8, EligibleChanges: 6, ScoredByHorizon: map[int]int{1: 6, 5: 5, 20: 4}, ReasonCounts: map[string]int{}},
		Method:           rpc.EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", MaterialityGate: "disclosed", AutomaticHorizon: "disclosed", MarketContext: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Fingerprint:      "edge_0123456789abcdef", LastFullRevalidation: now, NotExecution: true,
		Options: rpc.EdgeOptionReview{
			Coverage: rpc.EdgeOptionCoverage{ExecutionEpisodes: 2, ClosingEpisodes: 2},
			Realized: rpc.EdgeOptionRealizedReview{KnownPNLBase: &realizedTotal, PositiveCount: 1, NegativeCount: 1, CompleteCount: 2, TotalCount: 2, Episodes: []rpc.EdgeOptionEpisodeSummary{
				{ID: "option_gain", Grouping: "exact_order", Lifecycle: "closing", Underlying: "ABC", ActivityFrom: now.AddDate(0, 0, -5), ActivityTo: now.AddDate(0, 0, -5), RealizedPNLBase: &gain, PNLStatus: "complete", Legs: []rpc.EdgeOptionLegIdentity{{Symbol: "ABC CALL", Underlying: "ABC", Expiry: "2026-09-18", Strike: new(float64(100)), PutCall: "call"}}},
				{ID: "option_loss", Grouping: "unlinked_execution", Lifecycle: "closing", Underlying: "XYZ", ActivityFrom: now.AddDate(0, 0, -3), ActivityTo: now.AddDate(0, 0, -3), RealizedPNLBase: &loss, PNLStatus: "complete", Legs: []rpc.EdgeOptionLegIdentity{{Symbol: "XYZ PUT", Underlying: "XYZ", Expiry: "2026-10-16", Strike: new(float64(90)), PutCall: "put"}}},
			}},
			Open: rpc.EdgeOptionOpenReview{SnapshotDate: now, KnownPNLBase: &open, NegativeCount: 1, CompleteCount: 1, TotalCount: 1, Positions: []rpc.EdgeOptionOpenPositionSummary{{ID: "option_open", Symbol: "ABC CALL", Underlying: "ABC", SnapshotDate: now, Expiry: "2026-09-18", Strike: new(float64(100)), PutCall: "call", OpenPNLBase: &open, PNLStatus: "complete"}}},
		},
	}
	for _, action := range []string{"open", "add", "trim", "exit"} {
		row := rpc.EdgeActionRollup{Action: action}
		for _, sessions := range []int{1, 5, 20} {
			amount := float64(sessions)
			row.Horizons = append(row.Horizons, rpc.EdgeHorizonRollup{Sessions: sessions, SampleCount: 1, TotalBase: &amount, MedianBase: &amount})
		}
		result.ActionRollups = append(result.ActionRollups, row)
	}
	for i, symbol := range []string{"ABC", "DEF", "GHI"} {
		result.Findings = append(result.Findings, rpc.EdgeFinding{ChangeID: "change_" + strings.ToLower(symbol), Symbol: symbol, Action: "add", Direction: "long", ExecutedAt: now.Add(time.Duration(i) * time.Hour), HorizonSessions: 20, DecisionNotionalBase: 1_000, DecisionImpactBase: float64(100 - i), DecisionImpactPct: float64(10 - i)})
	}
	return result
}

// The CLI composes the headline from the typed selected pattern so its money
// matches every other line, and lays the screen out so each caveat sits next
// to what it qualifies.
func TestEdgeHumanOutputComposesHeadlineAndOrdersSections(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.AutomaticHorizon = true
	result.HorizonSessions = 1
	result.Account.BaseCurrency = "EUR"
	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 296, ScoredChanges: 121, CoveragePct: 40.9, LargestActionSample: 40, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true}
	result.Coverage = rpc.EdgeCoverage{TradeChanges: 1009, EligibleChanges: 296, ScoredByHorizon: map[int]int{1: 121, 5: 56, 20: 29}, ReasonCounts: map[string]int{}}
	result.Headline = "Long trims: -4208.84 EUR price impact across 40 of 89 changes at 1 sessions; median -53.76 EUR."
	result.ReviewAction, result.ReviewDirection = "trim", "long"
	total, median, without, notional := -4208.84, -53.76, -2132.94, 42.5
	earlier, later, paired := -1030.15, -4613.02, -32.14
	result.Patterns = []rpc.EdgeDecisionPattern{{Action: "trim", Direction: "long", EligibleChanges: 89,
		Horizons: []rpc.EdgePatternHorizon{{Sessions: 1, SampleCount: 40, TotalBase: &total, MedianBase: &median, NotionalCoveragePct: &notional, WithoutLargestBase: &without, LargestDateSharePct: new(float64(17.4)), LargestContractSharePct: new(float64(46.7)), DistinctDates: 29,
			Months: []rpc.EdgePatternMonth{{Month: "2025-11", SampleCount: 1, TotalBase: 128.09, MedianBase: 128.09}}}},
		Comparisons: []rpc.EdgeHorizonComparison{{EarlierSessions: 1, LaterSessions: 5, SampleCount: 12, EarlierTotalBase: &earlier, LaterTotalBase: &later, MedianDifferenceBase: &paired}},
	}}
	var stdout bytes.Buffer
	renderEdgeText(&Env{Stdout: &stdout}, result)
	got := stdout.String()
	// Rows wrap at the prose measure with a four-space hanging indent; join
	// continuation lines before matching whole rows.
	flat := strings.ReplaceAll(got, "\n    ", " ")
	// The bottom line wraps beneath its own column; compare it by its words.
	words := strings.Join(strings.Fields(got), " ")
	if want := "Long trims: -€ 4,208.84 over the 1 session after them, across 40 of 89; median -€ 53.76 each"; !strings.Contains(words, want) {
		t.Fatalf("output missing %q:\n%s", want, got)
	}
	for _, want := range []string{
		"Coverage · 121 of 296 eligible changes scored at 1 session (40.9%) · 1,009 broker position changes found, 713 outside the review",
		"Horizon · 1 session selected automatically · 20 sessions 9.8% coverage · 5 sessions 18.9% coverage",
		"Selected sample · 40 of 89 long trims · size coverage 42.5% of execution notional",
		"2025-11   1 decision   total",
		"Same decisions 1 to 5 sessions · n=12 · -€ 1,030.15 becomes -€ 4,613.02 · median paired change -€ 32.14",
		"Findings  by absolute impact at 1 session; not the headline group",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{"-4208.84 EUR", "1 sessions", "1 decisions", "EUR -"} {
		if strings.Contains(flat, stale) {
			t.Fatalf("output still contains %q:\n%s", stale, got)
		}
	}
	order := []string{"Canary Edge", "Bottom line", "Long trims:", "\nAccount P/L\n", "\nDecisions by action\n", "separate decision sets", "ACTION", "TRIM", "\nFindings", "change_abc", "\nOptions", "Realized episodes", "\nEvidence\n", "\nDetails: canary edge"}
	last := -1
	for _, marker := range order {
		at := strings.Index(got, marker)
		if at <= last {
			t.Fatalf("%q out of order (at %d, previous %d):\n%s", marker, at, last, got)
		}
		last = at
	}
	// The matrix caveat must sit directly above the matrix header.
	caveat := strings.Index(got, "separate decision sets")
	header := strings.Index(got, "ACTION")
	if between := got[caveat:header]; strings.Count(between, "\n") > 2 {
		t.Fatalf("matrix caveat is not adjacent to the matrix:\n%s", got)
	}
}

// A cycle closed by a lifecycle event says so; after an exercise or
// assignment the row says its P/L is the option leg's alone.
func TestEdgeCycleRowsNameLifecycleClose(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	now := result.AsOf
	gain, loss, flat := 4890.0, -200.0, 0.0
	total := gain + loss + flat
	result.Options.Cycles = rpc.EdgeOptionCycles{CompletedCount: 3, CompletePNLCount: 3, LostMoney: 1, Flat: 1, Ungraded: 1, Reasons: map[string]int{}, KnownPNLBase: &total, Cycles: []rpc.EdgeOptionCycle{
		{ID: "option-cycle_a", Symbol: "SYNX 260918C00100000", Direction: "short", OpenedAt: now.AddDate(0, 0, -30), ClosedAt: now.AddDate(0, 0, -2), ClosedBy: "assignment", ExecutionCount: 2, RealizedPNLBase: &gain, PNLStatus: "complete", MissingEvidence: []string{}, Grade: "none", GradeReason: "closed_by_delivery"},
		{ID: "option-cycle_b", Symbol: "SYNX 260918P00090000", Direction: "long", OpenedAt: now.AddDate(0, 0, -30), ClosedAt: now.AddDate(0, 0, -2), ClosedBy: "expiry", ExecutionCount: 2, RealizedPNLBase: &loss, PNLStatus: "complete", MissingEvidence: []string{}, Grade: "lost_money"},
		{ID: "option-cycle_c", Symbol: "SYNX 260918C00110000", Direction: "long", OpenedAt: now.AddDate(0, 0, -30), ClosedAt: now.AddDate(0, 0, -2), ClosedBy: "trade", ExecutionCount: 2, RealizedPNLBase: &flat, PNLStatus: "complete", MissingEvidence: []string{}, Grade: "flat"},
	}}
	if err := rpc.ValidateEdgeResult(result); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	renderEdgeText(&Env{Stdout: &stdout}, result)
	got := stdout.String()
	for _, want := range []string{"2 executions · closed by assignment, option leg only · complete", "2 executions · closed by expiry · complete", "2026-08-22 · 2 executions · complete"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// edgePagingConn serves the golden Edge review one page at a time and
// records each requested page; a changed fingerprint simulates a snapshot
// republished between pages.
type edgePagingConn struct {
	pages        []rpc.EdgeSnapshotParams
	changeAtPage int
}

func (c *edgePagingConn) Call(_ context.Context, _ string, params, out any) error {
	p := params.(rpc.EdgeSnapshotParams)
	c.pages = append(c.pages, p)
	// The fixture has five rows at most; serve pages of two whatever the
	// request asks, so following next_offset takes several calls.
	result := goldenEdge(p.OptionOffset, min(p.OptionLimit, 2))
	if c.changeAtPage > 0 && len(c.pages) >= c.changeAtPage {
		result.Fingerprint = "edge_republished"
	}
	*out.(*rpc.EdgeResult) = result
	return nil
}

func (*edgePagingConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestEdgeCLIPagesOptionRows(t *testing.T) {
	conn := &edgePagingConn{}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", []string{"--option-offset", "2", "--option-limit", "50", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(conn.pages) != 1 || conn.pages[0].OptionOffset != 2 || conn.pages[0].OptionLimit != 50 {
		t.Fatalf("pages=%+v", conn.pages)
	}
	if code := Run(t.Context(), env, "edge", []string{"--option-limit", "501"}); code == 0 {
		t.Fatal("an option page past the bound was accepted")
	}
}

// --all-options follows next_offset until every list is whole and returns
// one page carrying every row, with the totals unchanged.
func TestEdgeCLIAllOptionsFetchesEveryRow(t *testing.T) {
	conn := &edgePagingConn{}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", []string{"--all-options", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got rpc.EdgeResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	o := got.Options
	if len(o.Cycles.Cycles) != 5 || len(o.Realized.Episodes) != 5 || len(o.Open.Positions) != 4 || o.Realized.TotalCount != 5 || o.Cycles.CompletedCount != 5 {
		t.Fatalf("merged rows=%d/%d/%d", len(o.Cycles.Cycles), len(o.Realized.Episodes), len(o.Open.Positions))
	}
	if o.Cycles.Truncated || o.Realized.Truncated || o.Open.Truncated || o.Cycles.NextOffset != nil || o.Realized.NextOffset != nil || o.Open.NextOffset != nil {
		t.Fatalf("merged lists still read as partial: %+v", o.Realized)
	}
	seen := map[string]bool{}
	for _, row := range o.Realized.Episodes {
		if seen[row.ID] {
			t.Fatalf("row %s repeated", row.ID)
		}
		seen[row.ID] = true
	}
	if len(conn.pages) != 3 || conn.pages[0].OptionLimit != rpc.MaxEdgeOptionPage || conn.pages[1].OptionOffset != 2 || conn.pages[2].OptionOffset != 4 {
		t.Fatalf("pages=%+v", conn.pages)
	}

	changed := &edgePagingConn{changeAtPage: 2}
	stdout.Reset()
	stderr.Reset()
	env.Conn = changed
	if code := Run(t.Context(), env, "edge", []string{"--all-options", "--json"}); code == 0 || !strings.Contains(stderr.String(), "changed while reading") {
		t.Fatalf("pages of two snapshots were merged: exit %d %s", code, stderr.String())
	}
	if code := Run(t.Context(), env, "edge", []string{"--all-options", "--option-offset", "3"}); code == 0 {
		t.Fatal("--all-options with an offset was accepted")
	}
}

// The option decisions block says how buys and sales turned out inside their
// own positions and keeps the two money readings apart.
func TestEdgeOptionDecisionsRows(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	median, total, laterTotal := -12.5, -1500.0, 800.0
	result.Options.Decisions = rpc.EdgeOptionDecisions{Changes: 9, WithOutcome: 8, Reasons: map[string]int{"closed_by_delivery": 1}, Actions: []rpc.EdgeOptionDecisionRoll{
		{Action: "add", Changes: 5, WithResult: 4, Losses: 3, MedianResultPct: &median, TotalResultBase: &total},
		{Action: "trim", Changes: 4, WithResult: 4, Losses: 1, WithLaterExit: 4, LaterExitBetter: 3, TotalLaterExitBase: &laterTotal},
	}}
	if err := rpc.ValidateEdgeResult(result); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderEdgeText(&Env{Stdout: &out}, result)
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"9 option decisions read against their own position", "3 of 4 ended below the price paid", "the contracts kept went on to fetch more after 3 of 4", "1 closed by delivery"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	result.Options.Decisions.WithOutcome = 9
	if err := rpc.ValidateEdgeResult(result); err == nil {
		t.Fatal("inconsistent option decision counts validated")
	}
}

// A stock decision gets its verdict at five sessions. An option decision gets
// no helped or hurt: the line states what its own position says about the
// fill, and the closed positions are counted by their booked result. A
// written option reads from the seller's side.
func TestEdgeVerdictRows(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	gain := 1240.0
	graded := rpc.EdgeVerdict{Label: "none", Reason: "graded_by_position"}
	fact := func(grade, kind string, pct float64) *rpc.EdgeChangePosition {
		state := "closed"
		if grade == "" {
			state = "open"
		}
		return &rpc.EdgeChangePosition{State: state, PositionID: "option-cycle_a", PositionGrade: grade, ReferenceKind: kind, ResultPct: &pct}
	}
	at := result.AsOf
	result.Verdicts = rpc.EdgeVerdicts{Counts: []rpc.EdgeVerdictCount{{AssetClass: "STK", Helped: 3, Hurt: 2, None: 1}}, Recent: []rpc.EdgeTradeVerdict{
		{ChangeID: "change_a", Symbol: "SYNX 260918C00100000", AssetClass: "OPT", Action: "add", Direction: "long", ExecutedAt: at.AddDate(0, 0, -3), Verdict: graded, Position: fact("made_money", "exit_average", 22)},
		{ChangeID: "change_b", Symbol: "SYNX 260918C00100000", AssetClass: "OPT", Action: "exit", Direction: "long", ExecutedAt: at.AddDate(0, 0, -2), Verdict: graded, Position: fact("made_money", "entry_average", -14)},
		{ChangeID: "change_c", Symbol: "SYNX 260918P00090000", AssetClass: "OPT", Action: "open", Direction: "short", ExecutedAt: at.AddDate(0, 0, -2), Verdict: graded, Position: fact("", "exit_average_and_mark", 30)},
		{ChangeID: "change_d", Symbol: "SYNX 260918P00090000", AssetClass: "OPT", Action: "trim", Direction: "short", ExecutedAt: at.AddDate(0, 0, -2), Verdict: graded, Position: fact("", "entry_average", 40)},
		{ChangeID: "change_e", Symbol: "SYNX 260918C00110000", AssetClass: "OPT", Action: "exit", Direction: "long", ExecutedAt: at.AddDate(0, 0, -2), Verdict: graded, Position: &rpc.EdgeChangePosition{State: "closed", PositionGrade: "none", Reason: "closed_by_delivery", ClosedBy: "exercise"}},
		// Positions that ended by expiry: nothing was sold or bought back at zero.
		{ChangeID: "change_h", Symbol: "SYNX 260821P00080000", AssetClass: "OPT", Action: "open", Direction: "long", ExecutedAt: at.AddDate(0, 0, -20), Verdict: graded, Position: &rpc.EdgeChangePosition{State: "closed", ClosedBy: "expiry", PositionGrade: "lost_money", ReferenceKind: "exit_average", ResultPct: new(-100.0)}},
		{ChangeID: "change_i", Symbol: "SYNX 260821C00130000", AssetClass: "OPT", Action: "open", Direction: "short", ExecutedAt: at.AddDate(0, 0, -20), Verdict: graded, Position: &rpc.EdgeChangePosition{State: "closed", ClosedBy: "expiry", PositionGrade: "made_money", ReferenceKind: "exit_average", ResultPct: new(100.0)}},
		{ChangeID: "change_j", Symbol: "SYNX 260821P00085000", AssetClass: "OPT", Action: "add", Direction: "long", ExecutedAt: at.AddDate(0, 0, -20), Verdict: graded, Position: &rpc.EdgeChangePosition{State: "closed", ClosedBy: "expiry", PositionGrade: "lost_money", ReferenceKind: "exit_average", ResultPct: new(-60.0)}},
		{ChangeID: "change_f", Symbol: "SYNX", AssetClass: "STK", Action: "add", Direction: "long", ExecutedAt: at.AddDate(0, 0, -9), Verdict: rpc.EdgeVerdict{Label: "helped", Basis: "horizon", Sessions: 5, ImpactBase: &gain}},
		{ChangeID: "change_g", Symbol: "SYNX", AssetClass: "STK", Action: "open", Direction: "long", ExecutedAt: at.AddDate(0, 0, -1), Verdict: rpc.EdgeVerdict{Label: "none", Reason: "missing_horizon"}},
	}}
	result.Options.Cycles = rpc.EdgeOptionCycles{CompletedCount: 5, CompletePNLCount: 5, MadeMoney: 3, LostMoney: 1, Ungraded: 1, Reasons: map[string]int{}, Cycles: []rpc.EdgeOptionCycle{}}
	for i, grade := range []string{"made_money", "made_money", "made_money", "lost_money", "none"} {
		booked, reason, closedBy := 100.0, "", "trade"
		switch grade {
		case "lost_money":
			booked = -100
		case "none":
			reason, closedBy = "closed_by_delivery", "exercise"
		}
		result.Options.Cycles.Cycles = append(result.Options.Cycles.Cycles, rpc.EdgeOptionCycle{ID: fmt.Sprintf("option-cycle_%d", i), Symbol: "SYNX 260918C00100000", Direction: "long", OpenedAt: at.AddDate(0, 0, -30), ClosedAt: at.AddDate(0, 0, -2), ClosedBy: closedBy, ExecutionCount: 2, RealizedPNLBase: &booked, PNLStatus: "complete", MissingEvidence: []string{}, Grade: grade, GradeReason: reason})
	}
	if err := rpc.ValidateEdgeResult(result); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderEdgeText(&Env{Stdout: &out}, result)
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"Stocks · 3 helped · 2 hurt · 0 no difference · 1 without a verdict",
		"Options · 3 of 5 closed positions made money · 1 lost money · 1 without a grade",
		"these contracts were later sold 22.0% above what you paid",
		"sold 14.0% below the position's average cost",
		"these contracts were bought back since, or stand at the latest statement value, 30.0% below what you received",
		"bought back 40.0% below the position's average price received",
		"closed by exercise or assignment, so no price comparison",
		"open long SYNX 260821P00080000 · these contracts expired worthless",
		"these contracts expired worthless, so you kept what you received",
		"these contracts were sold or expired, on average 60.0% below what you paid",
		"helped $ 1,240.00 after 5 sessions",
		"no verdict: missing horizon",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// No option line carries a verdict word, whatever its position did.
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "260918") && (strings.Contains(line, "helped") || strings.Contains(line, "hurt")) {
			t.Fatalf("an option decision is graded: %q", line)
		}
	}
	// The contract refuses a verdict resting on anything but five sessions.
	result.Verdicts.Recent[8].Verdict.Sessions = 1
	if err := rpc.ValidateEdgeResult(result); err == nil {
		t.Fatal("a one-session verdict validated")
	}
	result.Verdicts.Recent[8].Verdict = rpc.EdgeVerdict{Label: "hurt", Basis: "position", ImpactBase: &gain}
	if err := rpc.ValidateEdgeResult(result); err == nil {
		t.Fatal("a verdict on the position's outcome validated")
	}
}

// The detail of an option sale shows what the contract was quoted at
// afterwards as a price reading that names its source, states the fill
// against its position, and gives the position's grade. It carries no helped
// or hurt, however the quote moved: this sale closed a position that made
// money and the contract was quoted higher five sessions later.
func TestEdgeChangeDetailGradesThePositionNotTheSale(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	money := func(v float64) *float64 { return &v }
	sale := rpc.EdgeChangeDetail{ID: "change_sale", Symbol: "SYNX 260918C00100000", AssetClass: "OPT", Action: "exit", Direction: "long", ExecutedAt: day.AddDate(0, 0, -7),
		DeltaQuantity: -2, PositionBefore: 2, ExecutionVWAP: money(3), Multiplier: money(100), DirectCostsBase: money(2),
		Scores: []rpc.EdgeHorizonScore{
			{Sessions: 1, HorizonDay: &day, HorizonClose: money(3.5), CloseSource: "statement_mark", HorizonFX: money(1), DecisionNotionalBase: money(600), DecisionImpactBase: money(-102), DecisionImpactPct: money(-17)},
			{Sessions: 5, HorizonDay: &day, HorizonClose: money(9), CloseSource: "closing_quote", HorizonFX: money(1), DecisionNotionalBase: money(600), DecisionImpactBase: money(-1202), DecisionImpactPct: money(-200.33)},
			{Sessions: 20, Reason: "missing_horizon"},
		},
		Verdict:  rpc.EdgeVerdict{Label: "none", Reason: "graded_by_position"},
		Position: &rpc.EdgeChangePosition{State: "closed", PositionID: "option-cycle_a", ClosedBy: "trade", PositionGrade: "made_money", PositionPNLBase: money(196), ReferenceKind: "entry_average", ResultPct: money(50)},
	}
	var out bytes.Buffer
	renderEdgeChange(&Env{Stdout: &out}, sale, "USD")
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"statement value 3.5", "closing quote 9", "In its position · sold 50.0% above the position's average cost", "Position grade · closed, made money · $ 196.00 booked by the broker"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(got, "hurt") || strings.Contains(got, "helped") || strings.Contains(got, "Verdict") {
		t.Fatalf("an option sale is graded:\n%s", out.String())
	}
	stock := sale
	stock.Symbol, stock.AssetClass, stock.Position = "SYNX", "STK", nil
	stock.Scores = []rpc.EdgeHorizonScore{{Sessions: 5, HorizonDay: &day, HorizonClose: money(9), CloseSource: "daily_bar", HorizonFX: money(1), DecisionNotionalBase: money(600), DecisionImpactBase: money(-1202), DecisionImpactPct: money(-200.33)}}
	stock.Verdict = rpc.EdgeVerdict{Label: "hurt", Basis: "horizon", Sessions: 5, ImpactBase: money(-1202)}
	out.Reset()
	renderEdgeChange(&Env{Stdout: &out}, stock, "USD")
	if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, "Verdict · hurt -$ 1,202.00 after 5 sessions") || !strings.Contains(got, "close 9") {
		t.Fatalf("stock detail:\n%s", out.String())
	}
}

// When the daemon could not read the forward-test registry the reply names no
// rule and says so. `canary edge` prints that, because printing nothing would
// read as "none registered", and the contract refuses a reply that claims
// both a list and that the registry was unreadable.
func TestEdgeSaysWhenTheForwardTestRegistryCouldNotBeRead(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.ForwardTests, result.ForwardTestsUnavailable = []rpc.EdgeForwardTest{}, "registry_unreadable"
	if err := rpc.ValidateEdgeResult(result); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderEdgeText(&Env{Stdout: &out}, result)
	if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, "could not be read this time") || !strings.Contains(got, "this does not mean none are registered") {
		t.Fatalf("the unreadable registry is not said:\n%s", out.String())
	}
	result.ForwardTests = []rpc.EdgeForwardTest{{ID: "x", Title: "t", Rule: "r", RuleHash: "rule_x"}}
	if err := rpc.ValidateEdgeResult(result); err == nil {
		t.Fatal("a reply with rules and an unreadable registry validated")
	}
	result.ForwardTests, result.ForwardTestsUnavailable = []rpc.EdgeForwardTest{}, "something_else"
	if err := rpc.ValidateEdgeResult(result); err == nil {
		t.Fatal("an unknown reason validated")
	}
}
