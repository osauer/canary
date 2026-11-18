package edge

import (
	"maps"
	"math"
	"slices"
)

// Verdict labels, the evidence a verdict rests on, and the horizon it prefers.
const (
	VerdictHelped       = "helped"
	VerdictHurt         = "hurt"
	VerdictNoDifference = "no_difference"
	VerdictNone         = "none"

	VerdictBasisHorizon  = "horizon"
	VerdictBasisPosition = "position"

	VerdictSessions = 5
)

// Verdict says in one word whether a decision helped. It rests on Decision
// price impact after costs at five sessions, or at one session when five has
// no figure. An option decision with neither falls back to the outcome of
// its own position. A result no larger than the decision's own costs is no
// difference. Label none carries the reason no figure exists.
type Verdict struct {
	Label      string   `json:"label"`
	Basis      string   `json:"basis,omitempty"`
	Sessions   int      `json:"sessions,omitempty"`
	ImpactBase *float64 `json:"impact_base,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func changeVerdict(change Change) Verdict {
	costs := 0.0
	if change.DirectCostsBase != nil {
		costs = math.Abs(*change.DirectCostsBase)
	}
	label := func(impact float64) string {
		switch {
		case math.Abs(impact) <= costs:
			return VerdictNoDifference
		case impact > 0:
			return VerdictHelped
		default:
			return VerdictHurt
		}
	}
	reason := ""
	for _, sessions := range []int{VerdictSessions, 1} {
		for _, score := range change.Scores {
			if score.Sessions != sessions {
				continue
			}
			if score.DecisionImpactBase != nil {
				return Verdict{Label: label(*score.DecisionImpactBase), Basis: VerdictBasisHorizon, Sessions: sessions, ImpactBase: cloneFloat(score.DecisionImpactBase)}
			}
			if reason == "" {
				reason = score.Reason
			}
		}
	}
	if change.Position != nil && change.Position.ResultBase != nil {
		// The position reading is before costs; the decision's own are taken off.
		impact := *change.Position.ResultBase - costs
		return Verdict{Label: label(impact), Basis: VerdictBasisPosition, ImpactBase: &impact}
	}
	return Verdict{Label: VerdictNone, Reason: reason}
}

// VerdictCount counts verdicts for one asset class. Verdict money is never
// totalled: an option's buy and sale readings describe the same money.
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
		row := byClass[change.AssetClass]
		if row == nil {
			row = &VerdictCount{AssetClass: change.AssetClass}
			byClass[change.AssetClass] = row
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
