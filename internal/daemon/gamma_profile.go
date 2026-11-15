package daemon

import (
	"math"
	"slices"

	"github.com/osauer/canary/v3/internal/rpc"
)

func gammaSkewKey(l legData) string { return l.tradingClass + ":" + l.expiryYMD }

// gammaScenarioGEX preserves each observed IV anchor as the smile moves.
// At the original spot both signed and gross measurements therefore use the
// same observed IVs. A fit does not silently replace the original observation.
func gammaScenarioGEX(legs []legData, spot, snapshotSpot float64, curves map[string]SkewCurve) (net, gross float64) {
	for _, l := range legs {
		vol := l.iv
		if curve, ok := curves[gammaSkewKey(l)]; ok && gammaSkewCurvePositive(curve) {
			vol = l.iv * curve.IVAtMoneyness(math.Log(l.strike/spot)) / curve.IVAtMoneyness(math.Log(l.strike/snapshotSpot))
		}
		g := bsGamma(spot, l.strike, l.dte, vol, 0, 0)
		net += dealerGEX(g, float64(l.oi), 100, spot, l.isCall)
		gross += absGEX(g, float64(l.oi), 100, spot)
	}
	return net, gross
}

// Reject an invalid quadratic for the whole expiry, never switch recipes at
// one scenario price: such a discontinuity can fabricate a zero crossing.
func gammaSkewCurvePositive(curve SkewCurve) bool {
	if !curve.ok {
		return false
	}
	points := []float64{curve.mLo, curve.mHi}
	if curve.C > 0 {
		vertex := -curve.B / (2 * curve.C)
		if vertex > curve.mLo && vertex < curve.mHi {
			points = append(points, vertex)
		}
	}
	for _, m := range points {
		v := curve.IVAtMoneyness(m)
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// gammaSweep includes exact spot and strikes. Narrow near-expiry kernels get
// additional nodes around their own volatility width; bracketed sign changes
// are then bisected. This is CPU-only: the broker request budget is unchanged.
func gammaSweep(legs []legData, spot, width float64, curves map[string]SkewCurve) []rpc.GammaProfilePoint {
	if len(legs) == 0 || spot <= 0 || width <= 0 || width >= 1 {
		return nil
	}
	lo, hi := spot*(1-width), spot*(1+width)
	step := (hi - lo) / float64(sweepPoints-1)
	nodes := make([]float64, 0, sweepPoints+len(legs))
	for i := range sweepPoints {
		nodes = append(nodes, lo+float64(i)*step)
	}
	nodes = append(nodes, spot)
	for _, l := range legs {
		if l.strike >= lo && l.strike <= hi {
			nodes = append(nodes, l.strike)
		}
		kernel := l.iv * math.Sqrt(l.dte)
		if kernel <= 0 || kernel*spot >= 4*step {
			continue
		}
		for _, n := range []float64{-4, -2, -1, 1, 2, 4} {
			x := l.strike * math.Exp(n*kernel)
			if x > lo && x < hi {
				nodes = append(nodes, x)
			}
		}
	}
	slices.Sort(nodes)
	nodes = slices.Compact(nodes)
	out := make([]rpc.GammaProfilePoint, 0, len(nodes))
	for _, x := range nodes {
		g, _ := gammaScenarioGEX(legs, x, spot, curves)
		if len(out) > 0 {
			prev := out[len(out)-1]
			if oppositeGammaSigns(prev.GEX, g) {
				a, b, fa := prev.Spot, x, prev.GEX
				for range 32 {
					mid := (a + b) / 2
					fm, _ := gammaScenarioGEX(legs, mid, spot, curves)
					if fm == 0 {
						a, b = mid, mid
						break
					}
					if oppositeGammaSigns(fa, fm) {
						b = mid
					} else {
						a, fa = mid, fm
					}
				}
				out = append(out, rpc.GammaProfilePoint{Spot: (a + b) / 2, GEX: 0})
			}
		}
		out = append(out, rpc.GammaProfilePoint{Spot: x, GEX: g})
	}
	return out
}

func oppositeGammaSigns(a, b float64) bool { return a < 0 && b > 0 || a > 0 && b < 0 }

// gammaCrossings ignores same-sign touches and underflow zeros in far tails.
func gammaCrossings(profile []rpc.GammaProfilePoint) []float64 {
	roots := []float64{}
	last := -1
	for i, p := range profile {
		if p.GEX == 0 {
			continue
		}
		if last >= 0 && oppositeGammaSigns(profile[last].GEX, p.GEX) {
			prev := profile[last]
			x := prev.Spot - prev.GEX*(p.Spot-prev.Spot)/(p.GEX-prev.GEX)
			if i-last > 1 {
				x = (profile[last+1].Spot + profile[i-1].Spot) / 2
			}
			roots = append(roots, x)
		}
		last = i
	}
	return roots
}

func gammaProfileMetrics(legs []legData, spot float64, curves map[string]SkewCurve, profile []rpc.GammaProfilePoint) *rpc.GammaProfileMetrics {
	net, gross := gammaScenarioGEX(legs, spot, spot, curves)
	if gross <= 0 || math.IsNaN(net) || math.IsInf(net, 0) {
		return nil
	}
	resolution := 0.0
	for i := 1; i < len(profile); i++ {
		resolution = math.Max(resolution, (profile[i].Spot-profile[i-1].Spot)/spot*100)
	}
	return &rpc.GammaProfileMetrics{GEXAtSpot: net, GrossGEXAtSpot: gross, NetToGrossPct: net / gross * 100, Crossings: gammaCrossings(profile), ResolutionPct: resolution}
}
