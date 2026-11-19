package edge

import (
	"maps"
	"math"
	"slices"
	"strings"
)

// Verdict labels, the evidence a verdict rests on, its one horizon, and the
// reason an option decision has none.
const (
	VerdictHelped       = "helped"
	VerdictHurt         = "hurt"
	VerdictNoDifference = "no_difference"
	VerdictNone         = "none"

	VerdictBasisHorizon = "horizon"

	VerdictSessions = 5

	VerdictGradedByPosition = "graded_by_position"
)

// Verdict says in one word whether a stock or ETF decision helped. It rests
// on one measure only, Decision price impact at five sessions, so the word
// cannot change its meaning when more data arrives: with no five-session
// figure the label is none and carries the reason. A price move within the
// decision's own costs, in either direction, is no difference.
//
// An option decision has no verdict. Profit grades the position (owner,
// 2026-11-19 06:45 CET), and the price after an option trade is collected
// only to build sell-or-hold rules; so the closed position carries the one
// grade (OptionCycle.Grade) and each fill states its facts (PositionOutcome).
// Until 2026-11-19 the verdict fell back from five sessions to one and then
// to the position, so the first read of option closes after a sale changed
// the word on decisions that had not changed.
type Verdict struct {
	Label      string   `json:"label"`
	Basis      string   `json:"basis,omitempty"`
	Sessions   int      `json:"sessions,omitempty"`
	ImpactBase *float64 `json:"impact_base,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func changeVerdict(change Change) Verdict {
	if strings.EqualFold(change.AssetClass, "OPT") {
		return Verdict{Label: VerdictNone, Reason: VerdictGradedByPosition}
	}
	costs := 0.0
	if change.DirectCostsBase != nil {
		costs = *change.DirectCostsBase
	}
	for _, score := range change.Scores {
		if score.Sessions != VerdictSessions {
			continue
		}
		if score.DecisionImpactBase == nil {
			return Verdict{Label: VerdictNone, Reason: score.Reason}
		}
		// The impact is after costs. The band is on the price move before
		// them, so a gain and a loss of the same size read alike; taking the
		// costs off the net figure again made a small gain no difference
		// and every small loss hurt.
		label := VerdictHurt
		switch move := *score.DecisionImpactBase + costs; {
		case math.Abs(move) <= math.Abs(costs):
			label = VerdictNoDifference
		case move > 0:
			label = VerdictHelped
		}
		return Verdict{Label: label, Basis: VerdictBasisHorizon, Sessions: VerdictSessions, ImpactBase: cloneFloat(score.DecisionImpactBase)}
	}
	return Verdict{Label: VerdictNone}
}

// VerdictCount counts the verdicts of one asset class's decisions. Options
// are not in it: their positions are graded (OptionCycles). Verdict money is
// never totalled.
type VerdictCount struct {
	AssetClass   string `json:"asset_class"`
	Helped       int    `json:"helped"`
	Hurt         int    `json:"hurt"`
	NoDifference int    `json:"no_difference"`
	None         int    `json:"none"`
}

func buildVerdictCounts(changes []Change) []VerdictCount {
	byClass := map[string]*VerdictCount{}
	for _, change := range changes {
		if change.Verdict.Reason == VerdictGradedByPosition {
			continue
		}
		// A blank class fails the served result's validation, and one such
		// row would take the whole read down.
		class := strings.TrimSpace(change.AssetClass)
		if class == "" {
			class = "unknown"
		}
		row := byClass[class]
		if row == nil {
			row = &VerdictCount{AssetClass: class}
			byClass[class] = row
		}
		switch change.Verdict.Label {
		case VerdictHelped:
			row.Helped++
		case VerdictHurt:
			row.Hurt++
		case VerdictNoDifference:
			row.NoDifference++
		default:
			row.None++
		}
	}
	out := make([]VerdictCount, 0, len(byClass))
	for _, class := range slices.Sorted(maps.Keys(byClass)) {
		out = append(out, *byClass[class])
	}
	return out
}
