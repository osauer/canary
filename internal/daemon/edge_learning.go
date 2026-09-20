package daemon

import (
	"fmt"
	"math"
	"strings"

	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/rpc"
)

func rpcEdgeDecisionPattern(in edgecore.DecisionPattern) rpc.EdgeDecisionPattern {
	out := rpc.EdgeDecisionPattern{
		Action:             in.Action,
		Direction:          in.Direction,
		EligibleChanges:    in.EligibleChanges,
		NotionalKnownCount: in.NotionalKnownCount,
		KnownNotionalBase:  in.KnownNotionalBase,
		Horizons:           make([]rpc.EdgePatternHorizon, 0, len(in.Horizons)),
	}
	for _, row := range in.Horizons {
		out.Horizons = append(out.Horizons, rpcEdgePatternHorizon(row))
	}
	out.Comparisons = make([]rpc.EdgeHorizonComparison, 0, len(in.Comparisons))
	for _, row := range in.Comparisons {
		out.Comparisons = append(out.Comparisons, rpcEdgeHorizonComparison(row))
	}
	return out
}
func rpcEdgePatternHorizon(in edgecore.PatternHorizon) rpc.EdgePatternHorizon {
	out := rpc.EdgePatternHorizon{
		MarketContext:         rpcEdgeMarketContextRollups(in.MarketContext),
		LinkedProtectionCount: in.LinkedProtectionCount, PartialProtectionCount: in.PartialProtectionCount,
		Sessions:                in.Sessions,
		SampleCount:             in.SampleCount,
		ScoredNotionalBase:      in.ScoredNotionalBase,
		NotionalCoveragePct:     in.NotionalCoveragePct,
		TotalBase:               in.TotalBase,
		MedianBase:              in.MedianBase,
		MedianImpactPct:         in.MedianImpactPct,
		PositiveCount:           in.PositiveCount,
		NegativeCount:           in.NegativeCount,
		FlatCount:               in.FlatCount,
		DistinctDates:           in.DistinctDates,
		DistinctContracts:       in.DistinctContracts,
		LargestDateSharePct:     in.LargestDateSharePct,
		LargestContractSharePct: in.LargestContractSharePct,
		WithoutLargestBase:      in.WithoutLargestBase,
		Exclusions:              in.Exclusions,
		Months:                  make([]rpc.EdgePatternMonth, 0, len(in.Months)),
	}
	for _, row := range in.Months {
		out.Months = append(out.Months, rpcEdgePatternMonth(row))
	}
	return out
}
func rpcEdgePatternMonth(in edgecore.PatternMonth) rpc.EdgePatternMonth {
	out := rpc.EdgePatternMonth{
		Month:       in.Month,
		SampleCount: in.SampleCount,
		TotalBase:   in.TotalBase,
		MedianBase:  in.MedianBase,
	}
	return out
}
func rpcEdgeHorizonComparison(in edgecore.HorizonComparison) rpc.EdgeHorizonComparison {
	out := rpc.EdgeHorizonComparison{
		EarlierSessions:      in.EarlierSessions,
		LaterSessions:        in.LaterSessions,
		SampleCount:          in.SampleCount,
		EarlierTotalBase:     in.EarlierTotalBase,
		LaterTotalBase:       in.LaterTotalBase,
		DifferenceBase:       in.DifferenceBase,
		MedianDifferenceBase: in.MedianDifferenceBase,
	}
	return out
}

// populateEdgeLearningSummary selects a described sample, never a skill grade.
func populateEdgeLearningSummary(out *rpc.EdgeResult) {
	if out.Account == nil || out.Account.StartingEquityBase <= 0 || !out.HorizonSelection.Adequate {
		return
	}
	var selected *rpc.EdgeDecisionPattern
	var lens *rpc.EdgePatternHorizon
	for i := range out.Patterns {
		p := &out.Patterns[i]
		for j := range p.Horizons {
			h := &p.Horizons[j]
			if h.Sessions != out.HorizonSessions || h.SampleCount < edgecore.MinimumPatternSample || h.TotalBase == nil || h.MedianBase == nil {
				continue
			}
			if math.Abs(*h.TotalBase)/out.Account.StartingEquityBase*100 < edgecore.MinimumPatternTotalImpactEquityPct || math.Abs(*h.MedianBase)/out.Account.StartingEquityBase*100 < edgecore.MinimumFindingImpactEquityPct {
				continue
			}
			if lens == nil || h.SampleCount > lens.SampleCount {
				selected, lens = p, h
			}
		}
	}
	if selected == nil {
		return
	}
	out.ReviewAction, out.ReviewDirection = selected.Action, selected.Direction
	out.MarketContext = append([]rpc.EdgeMarketContextRollup{}, lens.MarketContext...)
	out.MarketContextMissing = []string{}
	present := map[string]bool{}
	for _, c := range lens.MarketContext {
		present[c.Key] = true
	}
	for _, b := range edgecore.MarketBenchmarks() {
		if !present[b.Key] {
			out.MarketContextMissing = append(out.MarketContextMissing, b.Key)
		}
	}
	out.Headline = fmt.Sprintf("%s %s: %+.2f %s price impact across %d of %d changes at %d %s; median %+.2f %s.", strings.ToUpper(selected.Direction[:1])+selected.Direction[1:], edgeActionPlural(selected.Action), *lens.TotalBase, out.Account.BaseCurrency, lens.SampleCount, selected.EligibleChanges, lens.Sessions, pluralNoun(lens.Sessions, "session"), *lens.MedianBase, out.Account.BaseCurrency)
	positive, negative := 0, 0
	for _, m := range lens.Months {
		if m.TotalBase > 0 {
			positive++
		} else if m.TotalBase < 0 {
			negative++
		}
	}
	out.ReviewNote = fmt.Sprintf("The covered decisions span %d %s and %d execution %s: %d positive %s, %d negative. This is a price outcome, not proof of skill or risk-management quality.", len(lens.Months), pluralNoun(len(lens.Months), "month"), lens.DistinctDates, pluralNoun(lens.DistinctDates, "date"), positive, pluralNoun(positive, "month"), negative)
}

func rpcEdgeProtectionContext(in edgecore.ProtectionContext) rpc.EdgeProtectionContext {
	return rpc.EdgeProtectionContext{Status: in.Status, ExecutionCount: in.ExecutionCount, MatchedExecutions: in.MatchedExecutions, Bucket: in.Bucket, EvidenceAt: in.EvidenceAt}
}

func rpcEdgeOptionCycles(in edgecore.OptionCycles) rpc.EdgeOptionCycles {
	out := rpc.EdgeOptionCycles{CompletedCount: in.CompletedCount, CompletePNLCount: in.CompletePNLCount, OpenContractCount: in.OpenContractCount, ExcludedContracts: in.ExcludedContracts, Reasons: in.Reasons, KnownPNLBase: in.KnownPNLBase, Cycles: []rpc.EdgeOptionCycle{}}
	rows := seatEdgeOptionSigns(in.Cycles, rpc.MaxEdgeOptionResults, func(row edgecore.OptionCycle) float64 {
		if row.RealizedPNLBase == nil {
			return 0
		}
		return *row.RealizedPNLBase
	})
	for _, row := range rows {
		out.Cycles = append(out.Cycles, rpc.EdgeOptionCycle{ID: row.ID, Symbol: row.Symbol, Direction: row.Direction, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, ExecutionCount: row.ExecutionCount, RealizedPNLBase: row.RealizedPNLBase, PNLStatus: row.PNLStatus, MissingEvidence: row.MissingEvidence, LinkedProtectionCount: row.LinkedProtectionCount})
	}
	out.Truncated = len(out.Cycles) < out.CompletedCount
	return out
}

func withholdEdgeProtectionContext(out *rpc.EdgeResult) {
	for i := range out.Patterns {
		for j := range out.Patterns[i].Horizons {
			out.Patterns[i].Horizons[j].LinkedProtectionCount = 0
			out.Patterns[i].Horizons[j].PartialProtectionCount = 0
		}
	}
	for i := range out.Options.Cycles.Cycles {
		out.Options.Cycles.Cycles[i].LinkedProtectionCount = 0
	}
	if out.Change != nil {
		out.Change.ProtectionContext = rpc.EdgeProtectionContext{Status: "unavailable", ExecutionCount: out.Change.ProtectionContext.ExecutionCount}
	}
}
