package daemon

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
)

type gammaDeltaIV struct{ delta, iv float64 }

func gammaOptionSkews(legs []legData, spot float64) []rpc.OptionSkew {
	groups := map[string][]legData{}
	for _, l := range legs {
		groups[gammaSkewKey(l)] = append(groups[gammaSkewKey(l)], l)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]rpc.OptionSkew, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		first := group[0]
		row := rpc.OptionSkew{TradingClass: first.tradingClass, Expiry: first.expiryYMD, DTE: first.dte * 365, Status: "unavailable"}
		var puts, calls []gammaDeltaIV
		for _, l := range group {
			if l.ivSource != gammaIVSourceModelTick && l.ivSource != gammaIVSourceLiveMid {
				continue
			}
			if spot <= 0 || l.strike <= 0 || l.dte <= 0 || l.iv <= 0 || math.IsNaN(l.iv) || math.IsInf(l.iv, 0) {
				continue
			}
			// Match observed OTM IVs using BS spot delta with the same zero
			// rate/dividend approximation as the scenario model.
			d1 := (math.Log(spot/l.strike) + 0.5*l.iv*l.iv*l.dte) / (l.iv * math.Sqrt(l.dte))
			if l.isCall && l.strike >= spot {
				calls = append(calls, gammaDeltaIV{normCDF(d1), l.iv})
			}
			if !l.isCall && l.strike <= spot {
				puts = append(puts, gammaDeltaIV{normCDF(-d1), l.iv})
			}
		}
		row.PutIV, row.PutDeltaBracket = gammaIVAtDelta(puts, 0.25)
		row.CallIV, row.CallDeltaBracket = gammaIVAtDelta(calls, 0.25)
		switch {
		case row.PutIV == nil && row.CallIV == nil:
			row.Reason = "both_25_delta_brackets_missing"
		case row.PutIV == nil:
			row.Reason = "put_25_delta_bracket_missing"
		case row.CallIV == nil:
			row.Reason = "call_25_delta_bracket_missing"
		default:
			row.Status = "available"
			row.SkewVolPoints = new((*row.PutIV - *row.CallIV) * 100)
		}
		out = append(out, row)
	}
	return out
}

func gammaIVAtDelta(points []gammaDeltaIV, target float64) (*float64, []float64) {
	slices.SortFunc(points, func(a, b gammaDeltaIV) int {
		if a.delta < b.delta {
			return -1
		}
		if a.delta > b.delta {
			return 1
		}
		return 0
	})
	for i, p := range points {
		if p.delta == target {
			return new(p.iv), []float64{target, target}
		}
		if i == 0 || p.delta < target {
			continue
		}
		prev := points[i-1]
		if prev.delta < target && p.delta > target {
			iv := prev.iv + (p.iv-prev.iv)*(target-prev.delta)/(p.delta-prev.delta)
			return &iv, []float64{prev.delta, p.delta}
		}
	}
	return nil, nil
}

func buildGammaInsight(c *rpc.GammaZeroComputed) *rpc.GammaInsight {
	if c == nil || c.Scope == rpc.GammaZeroScopeCombined || c.SpotUnderlying <= 0 {
		return nil
	}
	underlying := strings.ToUpper(c.Scope)
	if underlying != "SPX" && underlying != "SPY" {
		return nil
	}
	i := &rpc.GammaInsight{
		Underlying: underlying, Regime: rpc.GammaComputedRegime(c),
		AsOf: c.AsOf, SpotAt: c.SpotAt, DataType: c.DataType, Method: c.Method, Rankability: rpc.GammaRankabilityUnavailable,
		PositioningAssumption: "OI model: calls positive, puts negative; gross gamma measures sampled convexity, not observed dealer hedges.",
		DirectionalInference:  "Bullish/bearish positioning is unknown: open interest does not identify owners, trade direction or intraday 0DTE inventory.",
		SkewInterpretation:    "25-delta skew unavailable: no fully bracketed 7–60 day expiry with eligible observed IV on both sides.",
		HorizonAgreement:      classifyHorizonAgreement(c),
	}
	if c.Quality != nil {
		i.Rankability, i.RankabilityReason = c.Quality.Rankability, c.Quality.RankabilityReason
	}
	i.Provenance = fmt.Sprintf("%s feed; computed %s; %s. OI sign assumption: calls positive, puts negative.", c.DataType, c.AsOf.UTC().Format("2006-01-02 15:04 UTC"), i.Rankability)
	if !c.SpotAt.IsZero() {
		i.Provenance += " Spot observed " + c.SpotAt.UTC().Format("2006-01-02 15:04 UTC") + "."
	}
	switch i.Regime {
	case "long_gamma":
		i.Interpretation = "Modeled hedging would dampen moves in either direction."
	case "short_gamma":
		i.Interpretation = "Modeled hedging would amplify moves in either direction."
	case "transition_gamma":
		i.Interpretation = "Modeled gamma is near a sign transition; the response to moves is less stable."
		if c.ZeroGamma == nil && c.ProfileMetrics != nil && c.ProfileMetrics.GEXAtSpot == 0 {
			i.Interpretation = "Signed gamma balances at spot; gross option exposure remains."
		}
	default:
		i.Interpretation = "The local gamma response is unavailable."
	}
	if c.ProfileMetrics != nil && len(c.ProfileMetrics.Crossings) > 1 {
		i.Interpretation += fmt.Sprintf(" %d crossings detected; the displayed level is nearest spot.", len(c.ProfileMetrics.Crossings))
	}
	for _, b := range []struct {
		name    string
		zero    *float64
		sign    string
		metrics *rpc.GammaProfileMetrics
	}{
		{"0dte", c.ZeroGamma0DTE, c.GammaSign0DTE, c.ProfileMetrics0DTE},
		{"1to7", c.ZeroGamma1to7, c.GammaSign1to7, c.ProfileMetrics1to7},
		{"term", c.ZeroGammaTerm, c.GammaSignTerm, c.ProfileMetricsTerm},
	} {
		h := rpc.GammaHorizonInsight{Horizon: b.name, Regime: rpc.GammaMeasuredRegime(c.SpotUnderlying, b.zero, b.sign, b.metrics)}
		if h.Regime == "" {
			h.Regime = "unavailable"
		}
		if b.metrics != nil {
			h.GEXAtSpot = new(b.metrics.GEXAtSpot)
			if c.GammaTotalAbs > 0 {
				h.GrossSharePct = new(b.metrics.GrossGEXAtSpot / c.GammaTotalAbs * 100)
			}
		}
		i.Horizons = append(i.Horizons, h)
	}
	var horizonWords []string
	for _, h := range i.Horizons {
		label := h.Horizon + ": " + strings.ReplaceAll(h.Regime, "_", " ")
		if h.GrossSharePct != nil {
			label += fmt.Sprintf(" (%.0f%% gross)", *h.GrossSharePct)
		}
		horizonWords = append(horizonWords, label)
	}
	i.HorizonInterpretation = strings.Join(horizonWords, "; ") + "."
	for _, s := range c.OptionSkews {
		if s.Status != "available" || s.SkewVolPoints == nil || s.DTE < 7 || s.DTE > 60 {
			continue
		}
		if i.SelectedSkew == nil || math.Abs(s.DTE-30) < math.Abs(i.SelectedSkew.DTE-30) {
			i.SelectedSkew = new(s)
		}
	}
	if s := i.SelectedSkew; s != nil {
		premium := "equal put/call volatility pricing"
		if *s.SkewVolPoints > 0 {
			premium = "richer downside protection"
		}
		if *s.SkewVolPoints < 0 {
			premium = "richer upside exposure"
		}
		i.SkewInterpretation = fmt.Sprintf("%s %s (%.1f days): 25-delta put minus call IV %+.2f vol points — %s. This is option pricing, not a directional forecast.", s.TradingClass, s.Expiry, s.DTE, *s.SkewVolPoints, premium)
	}
	return i
}
