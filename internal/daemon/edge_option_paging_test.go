package daemon

import (
	"fmt"
	"testing"
	"time"

	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Every option row is reachable: pages of any size walk one canonical order
// without a gap or a repeat, the default page is the one the review always
// showed, and the totals describe every row, never the page.
func TestEdgeOptionListsPageThroughEveryRow(t *testing.T) {
	review := edgeOptionPagingReview()
	full := rpcEdgeOptionReview(review, 0, rpc.MaxEdgeOptionPage)
	if got := len(full.Realized.Episodes); got != 48 || full.Realized.Truncated || full.Realized.NextOffset != nil {
		t.Fatalf("full realized page = %d rows, truncated=%v next=%v", got, full.Realized.Truncated, full.Realized.NextOffset)
	}
	if got := len(full.Cycles.Cycles); got != 45 || full.Cycles.Truncated || full.Cycles.NextOffset != nil {
		t.Fatalf("full cycle page = %d rows", got)
	}
	if got := len(full.Open.Positions); got != 25 || full.Open.Truncated || full.Open.NextOffset != nil {
		t.Fatalf("full open page = %d rows", got)
	}
	// Rows without a numeric P/L follow the ranked rows.
	for i, row := range full.Realized.Episodes {
		if (row.RealizedPNLBase == nil) != (i >= 45) {
			t.Fatalf("realized row %d out of order: %+v", i, row)
		}
	}

	def := rpcEdgeOptionReview(review, 0, rpc.MaxEdgeOptionResults)
	want := selectEdgeOptionEpisodes(review.Realized.Episodes, rpc.MaxEdgeOptionResults)
	seatedLoss := false
	for i := range want {
		if def.Realized.Episodes[i].ID != want[i].ID {
			t.Fatalf("default page changed at %d: %s, want %s", i, def.Realized.Episodes[i].ID, want[i].ID)
		}
		seatedLoss = seatedLoss || *def.Realized.Episodes[i].RealizedPNLBase < 0
	}
	if len(def.Realized.Episodes) != rpc.MaxEdgeOptionResults || !seatedLoss || def.Realized.NextOffset == nil || *def.Realized.NextOffset != rpc.MaxEdgeOptionResults {
		t.Fatalf("default page = %d rows, seated loss %v, next %v", len(def.Realized.Episodes), seatedLoss, def.Realized.NextOffset)
	}

	for _, size := range []int{1, 7, 20} {
		var realized, cycles, open []string
		for offset := 0; ; {
			page := rpcEdgeOptionReview(review, offset, size)
			if page.Realized.TotalCount != 48 || page.Realized.KnownPNLBase == nil || *page.Realized.KnownPNLBase != *full.Realized.KnownPNLBase || page.Cycles.CompletedCount != 45 || page.Open.TotalCount != 25 {
				t.Fatalf("page %d/%d totals follow the page: %+v", offset, size, page.Realized)
			}
			if err := rpc.ValidateEdgeResult(edgePagingResult(page)); err != nil {
				t.Fatalf("page %d/%d invalid: %v", offset, size, err)
			}
			for _, row := range page.Realized.Episodes {
				realized = append(realized, row.ID)
			}
			for _, row := range page.Cycles.Cycles {
				cycles = append(cycles, row.ID)
			}
			for _, row := range page.Open.Positions {
				open = append(open, row.ID)
			}
			next := -1
			for _, candidate := range []*int{page.Realized.NextOffset, page.Cycles.NextOffset, page.Open.NextOffset} {
				if candidate != nil {
					if next >= 0 && *candidate != next {
						t.Fatalf("lists disagree on the next offset")
					}
					next = *candidate
				}
			}
			if next < 0 {
				break
			}
			offset = next
		}
		for name, pair := range map[string][2][]string{
			"realized": {realized, edgePagingIDs(full.Realized.Episodes, func(r rpc.EdgeOptionEpisodeSummary) string { return r.ID })},
			"cycles":   {cycles, edgePagingIDs(full.Cycles.Cycles, func(r rpc.EdgeOptionCycle) string { return r.ID })},
			"open":     {open, edgePagingIDs(full.Open.Positions, func(r rpc.EdgeOptionOpenPositionSummary) string { return r.ID })},
		} {
			if fmt.Sprint(pair[0]) != fmt.Sprint(pair[1]) {
				t.Fatalf("size %d %s pages walk %d rows, want the canonical %d", size, name, len(pair[0]), len(pair[1]))
			}
		}
	}

	beyond := rpcEdgeOptionReview(review, 1000, 20)
	if len(beyond.Realized.Episodes) != 0 || beyond.Realized.NextOffset != nil || beyond.Realized.Offset != 1000 || !beyond.Realized.Truncated {
		t.Fatalf("page past the end = %+v", beyond.Realized)
	}
}

func edgePagingIDs[T any](rows []T, id func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, id(row))
	}
	return out
}

func edgePagingResult(options rpc.EdgeOptionReview) rpc.EdgeResult {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	return rpc.EdgeResult{SchemaVersion: edgecore.SchemaVersion, State: rpc.EdgeStateCurrent, AsOf: now, Window: "365d", HorizonSessions: 20,
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "explicit", Reason: "explicit_override", MinimumSample: 3, MinimumCoveragePct: 25},
		Coverage:         rpc.EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}},
		Method:           rpc.EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", MaterialityGate: "disclosed", AutomaticHorizon: "disclosed", MarketContext: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Options:          options, Fingerprint: "edge_paging", LastFullRevalidation: now, NotExecution: true}
}

func edgeOptionPagingReview() edgecore.OptionReview {
	day := time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)
	var episodes []edgecore.OptionEpisode
	var cycles []edgecore.OptionCycle
	var open []edgecore.OptionOpenPosition
	for i := range 48 {
		row := edgecore.OptionEpisode{ID: fmt.Sprintf("option_e%02d", i), Grouping: edgecore.OptionGroupingExactOrder, Lifecycle: edgecore.OptionLifecycleClosing, Underlying: "SYNX", ActivityFrom: day.AddDate(0, 0, i), ActivityTo: day.AddDate(0, 0, i), PNLStatus: edgecore.OptionPNLUnavailable, MissingEvidence: []string{edgecore.OptionMissingRealizedPNL},
			Legs: []edgecore.OptionEpisodeLeg{{Symbol: "SYNX CALL", Underlying: "SYNX", Expiry: "2026-03-20", Strike: new(float64(100)), PutCall: "call"}}}
		if i < 45 {
			// Every gain is larger than every loss, so a top-20 cut needs a
			// seated loss.
			value := float64(1000 - i)
			if i%15 == 14 {
				value = -float64(i)
			}
			row.RealizedPNLBase, row.PNLStatus, row.MissingEvidence = new(value), edgecore.OptionPNLComplete, []string{}
		}
		episodes = append(episodes, row)
	}
	for i := range 45 {
		value := float64(500 - i)
		cycles = append(cycles, edgecore.OptionCycle{ID: fmt.Sprintf("option-cycle_c%02d", i), Symbol: "SYNX CALL", Direction: edgecore.DirectionLong, OpenedAt: day, ClosedAt: day.AddDate(0, 0, 1), ClosedBy: "trade", ExecutionCount: 2, RealizedPNLBase: &value, PNLStatus: edgecore.OptionPNLComplete, MissingEvidence: []string{}})
	}
	for i := range 25 {
		value := float64(-100 + i)
		open = append(open, edgecore.OptionOpenPosition{ID: fmt.Sprintf("option_o%02d", i), Symbol: "SYNX PUT", Underlying: "SYNX", SnapshotDate: day, Expiry: "2026-03-20", Strike: new(float64(90)), PutCall: "put", OpenPNLBase: &value, PNLStatus: edgecore.OptionPNLComplete, MissingEvidence: []string{}})
	}
	total := 0.0
	for _, row := range cycles {
		total += *row.RealizedPNLBase
	}
	out := edgecore.OptionReview{Cycles: edgecore.OptionCycles{CompletedCount: 45, CompletePNLCount: 45, Reasons: map[string]int{}, KnownPNLBase: &total, Cycles: cycles}}
	realized := edgecore.OptionRealizedReview{Episodes: episodes, KnownPNLBase: new(0.0)}
	for _, row := range episodes {
		if row.RealizedPNLBase == nil {
			realized.UnavailableCount++
			continue
		}
		realized.CompleteCount++
		*realized.KnownPNLBase += *row.RealizedPNLBase
		if *row.RealizedPNLBase > 0 {
			realized.PositiveCount++
		} else {
			realized.NegativeCount++
		}
	}
	openReview := edgecore.OptionOpenReview{SnapshotDate: day, Positions: open, KnownPNLBase: new(0.0), CompleteCount: len(open), NegativeCount: len(open)}
	for _, row := range open {
		*openReview.KnownPNLBase += *row.OpenPNLBase
	}
	out.Realized, out.Open = realized, openReview
	return out
}
