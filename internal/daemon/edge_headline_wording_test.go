package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The daemon composes the headline that the CLI, the app and the brief all
// show verbatim, so its grammar cannot be left to a CLI-side test whose
// fixture carries a hand-written headline.
func TestEdgeHeadlineUsesSingularSessionAndMonth(t *testing.T) {
	t.Parallel()
	total, median := -300.0, -100.0
	result := &rpc.EdgeResult{
		Window: "365d", HorizonSessions: 1, AutomaticHorizon: true,
		Account:          &rpc.EdgeAccountResult{BaseCurrency: "USD", StartingEquityBase: 100_000},
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 3, ScoredChanges: 3, CoveragePct: 100, LargestActionSample: 3, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true},
		Coverage:         rpc.EdgeCoverage{TradeChanges: 3, EligibleChanges: 3},
		Patterns: []rpc.EdgeDecisionPattern{{Action: "add", Direction: "long", EligibleChanges: 3, Horizons: []rpc.EdgePatternHorizon{{
			Sessions: 1, SampleCount: 3, TotalBase: &total, MedianBase: &median, DistinctDates: 1,
			Months: []rpc.EdgePatternMonth{{Month: "2026-04", SampleCount: 3, TotalBase: 300, MedianBase: 100}},
		}}}},
	}
	got := edgeHeadline(result)
	if !strings.Contains(got, "at 1 session;") || strings.Contains(got, "1 sessions") {
		t.Fatalf("one-session headline=%q", got)
	}
	if note := result.ReviewNote; !strings.Contains(note, "span 1 month and 1 execution date: 1 positive month, 0 negative") {
		t.Fatalf("one-month review note=%q", note)
	}
}

// Under an explicit --horizon the coverage floor is usually the gate that
// fails; the message must name it instead of quoting a sample gate that
// passed.
func TestEdgeUnselectedHeadlineNamesTheFailingGate(t *testing.T) {
	t.Parallel()
	result := &rpc.EdgeResult{
		Window: "365d", HorizonSessions: 20,
		Account:          &rpc.EdgeAccountResult{BaseCurrency: "EUR", StartingEquityBase: 100_000},
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "explicit", Reason: "explicit_override", EligibleChanges: 296, ScoredChanges: 29, CoveragePct: 9.8, LargestActionSample: 25, MinimumSample: 3, MinimumCoveragePct: 25},
		Coverage:         rpc.EdgeCoverage{TradeChanges: 1009, EligibleChanges: 296},
	}
	got := edgeHeadline(result)
	if !strings.HasPrefix(got, "No repeated 20-session pattern clears the evidence gates: 29 of 296 eligible changes were scored; coverage is 9.8%; at least 25% is required.") {
		t.Fatalf("coverage-gate headline=%q", got)
	}
	if strings.Contains(got, "largest action sample") {
		t.Fatalf("headline quotes a gate that passed: %q", got)
	}

	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "best_available", EligibleChanges: 8, ScoredChanges: 2, CoveragePct: 25, LargestActionSample: 2, MinimumSample: 3, MinimumCoveragePct: 25}
	got = edgeHeadline(result)
	for _, want := range []string{"2 of 8 eligible changes were scored", "at least 3 scored changes are required", "the largest action sample is 2; at least 3 is required within one action and direction"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sample-gate headline missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "coverage is") {
		t.Fatalf("headline quotes the coverage gate that passed: %q", got)
	}

	total, median := 20.0, 5.0
	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 8, ScoredChanges: 8, CoveragePct: 100, LargestActionSample: 5, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true}
	result.Patterns = []rpc.EdgeDecisionPattern{{Action: "open", Direction: "long", EligibleChanges: 5, Horizons: []rpc.EdgePatternHorizon{{Sessions: 20, SampleCount: 5, TotalBase: &total, MedianBase: &median}}}}
	got = edgeHeadline(result)
	if !strings.HasPrefix(got, "No repeated 20-session pattern clears the account-materiality gates:") || !strings.Contains(got, "largest action and direction group has 5 observations") || !strings.Contains(got, "0.10% of starting equity") {
		t.Fatalf("materiality-gate headline=%q", got)
	}
}
