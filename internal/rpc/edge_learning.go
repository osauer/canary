package rpc

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// EdgeDecisionPattern describes one action and direction without grading skill.
type EdgeDecisionPattern struct {
	Action             string                  `json:"action"`
	Direction          string                  `json:"direction"`
	EligibleChanges    int                     `json:"eligible_changes"`
	NotionalKnownCount int                     `json:"notional_known_count"`
	KnownNotionalBase  *float64                `json:"known_notional_base,omitempty"`
	Horizons           []EdgePatternHorizon    `json:"horizons"`
	Comparisons        []EdgeHorizonComparison `json:"comparisons"`
}

// EdgePatternHorizon discloses the sample and concentration behind a price outcome.
type EdgePatternHorizon struct {
	MarketContext           []EdgeMarketContextRollup `json:"market_context"`
	LinkedProtectionCount   int                       `json:"linked_protection_count"`
	PartialProtectionCount  int                       `json:"partial_protection_count"`
	Sessions                int                       `json:"sessions"`
	SampleCount             int                       `json:"sample_count"`
	ScoredNotionalBase      *float64                  `json:"scored_notional_base,omitempty"`
	NotionalCoveragePct     *float64                  `json:"notional_coverage_pct,omitempty"`
	TotalBase               *float64                  `json:"total_base,omitempty"`
	MedianBase              *float64                  `json:"median_base,omitempty"`
	MedianImpactPct         *float64                  `json:"median_impact_pct,omitempty"`
	PositiveCount           int                       `json:"positive_count"`
	NegativeCount           int                       `json:"negative_count"`
	FlatCount               int                       `json:"flat_count"`
	DistinctDates           int                       `json:"distinct_dates"`
	DistinctContracts       int                       `json:"distinct_contracts"`
	LargestDateSharePct     *float64                  `json:"largest_date_share_pct,omitempty"`
	LargestContractSharePct *float64                  `json:"largest_contract_share_pct,omitempty"`
	WithoutLargestBase      *float64                  `json:"without_largest_base,omitempty"`
	Months                  []EdgePatternMonth        `json:"months"`
	Exclusions              map[string]int            `json:"exclusions"`
}

// EdgePatternMonth is one observed calendar-month sample, not a significance test.
type EdgePatternMonth struct {
	Month       string  `json:"month"`
	SampleCount int     `json:"sample_count"`
	TotalBase   float64 `json:"total_base"`
	MedianBase  float64 `json:"median_base"`
}

// EdgeHorizonComparison compares the identical decisions at two horizons.
type EdgeHorizonComparison struct {
	EarlierSessions      int      `json:"earlier_sessions"`
	LaterSessions        int      `json:"later_sessions"`
	SampleCount          int      `json:"sample_count"`
	EarlierTotalBase     *float64 `json:"earlier_total_base,omitempty"`
	LaterTotalBase       *float64 `json:"later_total_base,omitempty"`
	DifferenceBase       *float64 `json:"difference_base,omitempty"`
	MedianDifferenceBase *float64 `json:"median_difference_base,omitempty"`
}

// EdgeProtectionContext is local protection provenance, not a risk verdict.
type EdgeProtectionContext struct {
	Status            string    `json:"status"`
	ExecutionCount    int       `json:"execution_count"`
	MatchedExecutions int       `json:"matched_executions"`
	Bucket            string    `json:"bucket,omitempty"`
	EvidenceAt        time.Time `json:"evidence_at,omitzero"`
}

// EdgeOptionCycles describes proven exact-contract positions opened and fully
// closed inside the review window. It does not infer multi-leg strategies.
type EdgeOptionCycles struct {
	Truncated         bool              `json:"truncated"`
	CompletedCount    int               `json:"completed_count"`
	CompletePNLCount  int               `json:"complete_pnl_count"`
	OpenContractCount int               `json:"open_contract_count"`
	ExcludedContracts int               `json:"excluded_contracts"`
	Reasons           map[string]int    `json:"reasons"`
	KnownPNLBase      *float64          `json:"known_pnl_base,omitempty"`
	Cycles            []EdgeOptionCycle `json:"cycles"`
}

// EdgeOptionCycle is a broker-reconciled flat-to-flat exact-contract history.
type EdgeOptionCycle struct {
	ID                    string    `json:"id"`
	Symbol                string    `json:"symbol"`
	Direction             string    `json:"direction"`
	OpenedAt              time.Time `json:"opened_at"`
	ClosedAt              time.Time `json:"closed_at"`
	ExecutionCount        int       `json:"execution_count"`
	RealizedPNLBase       *float64  `json:"realized_pnl_base,omitempty"`
	PNLStatus             string    `json:"pnl_status"`
	MissingEvidence       []string  `json:"missing_evidence"`
	LinkedProtectionCount int       `json:"linked_protection_count"`
}

func validateEdgeLearning(result EdgeResult) error {
	switch result.ProtectionState {
	case "", "current", "unavailable", "changed":
	default:
		return fmt.Errorf("invalid Edge protection state")
	}
	if result.ProtectionState == "current" && result.ProtectionAsOf.IsZero() {
		return fmt.Errorf("missing Edge protection evidence date")
	}
	if result.Change != nil {
		if err := validateEdgeProtectionContext(result.Change.ProtectionContext); err != nil {
			return err
		}
	}

	if len(result.Patterns) > 8 {
		return fmt.Errorf("too many Edge decision patterns")
	}
	seen := map[string]bool{}
	eligible := 0
	for _, p := range result.Patterns {
		key := p.Action + "/" + p.Direction
		if !validEdgeAction(p.Action) || !validEdgeDirection(p.Direction) || seen[key] || p.EligibleChanges < 1 || p.NotionalKnownCount < 0 || p.NotionalKnownCount > p.EligibleChanges {
			return fmt.Errorf("invalid Edge decision pattern")
		}
		seen[key] = true
		eligible += p.EligibleChanges
		if (p.NotionalKnownCount > 0) != (p.KnownNotionalBase != nil) || !edgeLearningAmount(p.KnownNotionalBase) {
			return fmt.Errorf("invalid Edge notional evidence")
		}
		horizons := map[int]bool{}
		for _, h := range p.Horizons {
			if !validEdgeHorizon(h.Sessions) || horizons[h.Sessions] || h.SampleCount < 0 || h.SampleCount > p.EligibleChanges {
				return fmt.Errorf("invalid Edge pattern horizon")
			}
			horizons[h.Sessions] = true
			if err := validateEdgeMarketContextRollups(h.MarketContext); err != nil {
				return err
			}
			if h.PositiveCount < 0 || h.NegativeCount < 0 || h.FlatCount < 0 || h.PositiveCount+h.NegativeCount+h.FlatCount != h.SampleCount || h.LinkedProtectionCount < 0 || h.PartialProtectionCount < 0 || h.LinkedProtectionCount+h.PartialProtectionCount > h.SampleCount {
				return fmt.Errorf("invalid Edge pattern counts")
			}
			if (h.SampleCount > 0) != (h.TotalBase != nil) || (h.TotalBase == nil) != (h.MedianBase == nil) {
				return fmt.Errorf("invalid Edge pattern amount presence")
			}
			for _, v := range []*float64{h.TotalBase, h.MedianBase, h.MedianImpactPct, h.ScoredNotionalBase, h.WithoutLargestBase} {
				if !edgeLearningAmount(v) {
					return fmt.Errorf("invalid Edge pattern amount")
				}
			}
			for _, v := range []*float64{h.NotionalCoveragePct, h.LargestDateSharePct, h.LargestContractSharePct} {
				if v != nil && (!finite(*v) || *v < 0 || *v > 100.000000001) {
					return fmt.Errorf("invalid Edge pattern percentage")
				}
			}
			if h.NotionalCoveragePct != nil && p.NotionalKnownCount != p.EligibleChanges {
				return fmt.Errorf("incomplete notional reported as coverage")
			}
			if h.DistinctDates < 0 || h.DistinctDates > h.SampleCount || h.DistinctContracts < 0 || h.DistinctContracts > h.SampleCount {
				return fmt.Errorf("invalid Edge pattern concentration")
			}
			excluded := 0
			for _, count := range h.Exclusions {
				if count < 0 {
					return fmt.Errorf("invalid Edge exclusion count")
				}
				excluded += count
			}
			if excluded+h.SampleCount != p.EligibleChanges {
				return fmt.Errorf("inconsistent Edge pattern coverage")
			}
			months := map[string]bool{}
			count := 0
			for _, m := range h.Months {
				if _, err := time.Parse("2006-01", m.Month); err != nil || months[m.Month] || m.SampleCount < 1 || !finite(m.TotalBase) || !finite(m.MedianBase) {
					return fmt.Errorf("invalid Edge month")
				}
				months[m.Month] = true
				count += m.SampleCount
			}
			if count != h.SampleCount {
				return fmt.Errorf("inconsistent Edge month counts")
			}
		}
		if len(horizons) != 3 || len(p.Comparisons) != 2 {
			return fmt.Errorf("incomplete Edge comparison set")
		}
		comparisons := map[int]bool{}
		for _, c := range p.Comparisons {
			if c.EarlierSessions != 1 || (c.LaterSessions != 5 && c.LaterSessions != 20) || comparisons[c.LaterSessions] || c.SampleCount < 0 || c.SampleCount > p.EligibleChanges {
				return fmt.Errorf("invalid Edge matched comparison")
			}
			comparisons[c.LaterSessions] = true
			for _, v := range []*float64{c.EarlierTotalBase, c.LaterTotalBase, c.DifferenceBase, c.MedianDifferenceBase} {
				if (c.SampleCount > 0) != (v != nil) || !edgeLearningAmount(v) {
					return fmt.Errorf("invalid Edge matched amount")
				}
			}
			if c.SampleCount > 0 && math.Abs(*c.LaterTotalBase-*c.EarlierTotalBase-*c.DifferenceBase) > 1e-7*math.Max(1, math.Abs(*c.DifferenceBase)) {
				return fmt.Errorf("inconsistent Edge paired difference")
			}
		}
	}
	if len(result.Patterns) > 0 && eligible != result.Coverage.EligibleChanges {
		return fmt.Errorf("inconsistent Edge pattern population")
	}
	if result.ReviewAction != "" && (!seen[result.ReviewAction+"/"+result.ReviewDirection] || result.ReviewNote == "") {
		return fmt.Errorf("unknown Edge review pattern")
	}
	c := result.Options.Cycles
	if c.CompletedCount < 0 || c.CompletePNLCount < 0 || c.CompletePNLCount > c.CompletedCount || c.OpenContractCount < 0 || c.ExcludedContracts < 0 || len(c.Cycles) > MaxEdgeOptionResults || len(c.Cycles) > c.CompletedCount || c.Truncated != (len(c.Cycles) < c.CompletedCount) || !edgeLearningAmount(c.KnownPNLBase) {
		return fmt.Errorf("invalid Edge option cycles")
	}
	ids := map[string]bool{}
	for _, row := range c.Cycles {
		if !strings.HasPrefix(row.ID, "option-cycle_") || ids[row.ID] || row.Symbol == "" || !validEdgeDirection(row.Direction) || row.OpenedAt.IsZero() || row.ClosedAt.Before(row.OpenedAt) || row.ExecutionCount < 2 || row.LinkedProtectionCount < 0 || row.LinkedProtectionCount > row.ExecutionCount || !edgeLearningAmount(row.RealizedPNLBase) {
			return fmt.Errorf("invalid Edge option cycle")
		}
		ids[row.ID] = true
		if row.PNLStatus != "complete" && row.PNLStatus != "partial" && row.PNLStatus != "unavailable" {
			return fmt.Errorf("invalid Edge cycle P/L state")
		}
		if (row.PNLStatus != "unavailable") != (row.RealizedPNLBase != nil) {
			return fmt.Errorf("invalid Edge cycle P/L presence")
		}
	}
	return nil
}

func edgeLearningAmount(value *float64) bool { return value == nil || finite(*value) }

func validateEdgeProtectionContext(c EdgeProtectionContext) error {
	if c.ExecutionCount < 0 || c.MatchedExecutions < 0 || c.MatchedExecutions > c.ExecutionCount {
		return fmt.Errorf("invalid Edge protection counts")
	}
	switch c.Status {
	case "", "unavailable":
		if c.MatchedExecutions != 0 || c.Bucket != "" || !c.EvidenceAt.IsZero() {
			return fmt.Errorf("unavailable Edge protection has evidence")
		}
	case "partial":
		if c.MatchedExecutions == 0 || c.Bucket != "" || c.EvidenceAt.IsZero() {
			return fmt.Errorf("invalid partial Edge protection")
		}
	case "linked":
		if c.ExecutionCount == 0 || c.MatchedExecutions != c.ExecutionCount || c.EvidenceAt.IsZero() {
			return fmt.Errorf("incomplete linked Edge protection")
		}
		switch c.Bucket {
		case TradeProposalBucketThetaHygiene, TradeProposalBucketRiskReduction, TradeProposalBucketTrailingStop, TradeProposalBucketOptionLossExit, TradeProposalBucketOptionExpiryClose, TradeProposalBucketOptionExitReview:
		default:
			return fmt.Errorf("unknown Edge protection bucket")
		}
	default:
		return fmt.Errorf("unknown Edge protection status")
	}
	return nil
}
