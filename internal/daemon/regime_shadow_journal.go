package daemon

import "github.com/osauer/canary/v2/internal/rpc"

// Shadow scoring for the decisions journal.
//
// The shipped stage machine and rpc.EvaluateRegimeShadow read the same
// snapshot and disagree; recording both on every decision is how that
// disagreement gets settled on real market readings rather than on a
// synthetic enumeration, which cannot exercise input currency or a real
// depth at all.
//
// Strictly write-only. Nothing in the daemon reads the shadow read back, no
// surface serves it, and no rule or alert consults it. Keep it that way until
// the corpus is large enough to say which model is right.

// regimeShadowRead scores a published snapshot under the shadow model, or
// returns nil when the snapshot carries no ranked clusters to score.
func regimeShadowRead(res *rpc.RegimeSnapshotResult) *rpc.RegimeShadowRead {
	if res == nil {
		return nil
	}
	rows := make([]rpc.RegimeShadowIndicatorInput, 0, len(streakIndicators))
	for _, ind := range streakIndicators {
		key := ind.key()
		_, meta, streak := regimeDecisionRowView(res, key)
		in := rpc.RegimeShadowIndicatorInput{
			Indicator: key,
			Depth:     ind.depth(res),
			// A row with no band at all was never ranked; gamma additionally
			// carries an explicit rankability veto, which reaches here as an
			// empty band.
			Rankable: meta.Band != "" && meta.Band != "unranked",
		}
		if meta.Freshness != nil {
			in.FreshnessClass = meta.Freshness.Class
		}
		if streak != nil {
			in.StressSessions = streak.StressSessions
		}
		rows = append(rows, in)
	}
	return rpc.EvaluateRegimeShadow(rows, rpc.RegimeShadowTape(*res), res.Composite.ClusterRankedCount)
}
