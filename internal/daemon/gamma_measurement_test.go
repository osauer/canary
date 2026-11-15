package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestGammaSweepFindsNarrowIntradayNegativePocket(t *testing.T) {
	legs := []legData{
		{strike: 100, dte: 2.0 / (365 * 24 * 60), iv: 0.2, oi: 1000},
		{strike: 100, dte: 7.0 / 365, iv: 0.2, oi: 100, isCall: true},
	}
	profile := sweepProfile(legs, 100, 0.15, nil)
	zero, sign := findZeroCrossing(profile)
	if zero == nil || sign != "" {
		t.Fatalf("missed negative 0DTE pocket: zero=%v sign=%s", zero, sign)
	}
	found := false
	for _, p := range profile {
		if p.Spot == 100 {
			found = true
			if p.GEX >= 0 {
				t.Fatal("local GEX should be negative")
			}
		}
	}
	if !found {
		t.Fatal("profile omitted exact spot")
	}
}

func TestGammaCrossingsAreNearestGenuineTransitions(t *testing.T) {
	profile := []rpc.GammaProfilePoint{{Spot: 80, GEX: 0}, {Spot: 90, GEX: 10}, {Spot: 100, GEX: 0}, {Spot: 110, GEX: -10}, {Spot: 120, GEX: 10}, {Spot: 130, GEX: 0}}
	zero, sign := findZeroCrossing(profile, 114)
	if zero == nil || math.Abs(*zero-115) > 1e-10 || sign != "" {
		t.Fatalf("nearest root: %v %s", zero, sign)
	}
	if len(gammaCrossings(profile)) != 2 {
		t.Fatal("tail zero is not a root")
	}
	if roots := gammaCrossings([]rpc.GammaProfilePoint{{Spot: 90, GEX: 1}, {Spot: 100, GEX: 0}, {Spot: 110, GEX: 1}}); len(roots) != 0 {
		t.Fatal("same-sign touch counted as a crossing")
	}
}

func TestGammaRejectsNonpositiveSmileWithoutFabricatedRoot(t *testing.T) {
	legs := []legData{{strike: 100, dte: .1, iv: .1, oi: 10, isCall: true, tradingClass: "SPX", expiryYMD: "20260918"}, {strike: 100, dte: .1, iv: .3, oi: 1, tradingClass: "SPX", expiryYMD: "20260918"}}
	curves := map[string]SkewCurve{"SPX:20260918": {A: .2, B: 2, ok: true, mLo: -.2, mHi: .2}}
	if gammaSkewCurvePositive(curves["SPX:20260918"]) {
		t.Fatal("negative fitted IV admitted")
	}
	profile := sweepProfile(legs, 100, .15, curves)
	for _, root := range gammaCrossings(profile) {
		net, gross := gammaScenarioGEX(legs, root, 100, curves)
		if math.Abs(net) > gross*1e-7 {
			t.Fatalf("root is not a zero of the actual model: net=%g gross=%g", net, gross)
		}
	}
	got, _ := gammaScenarioGEX(legs, 100, 100, curves)
	want, _ := gammaScenarioGEX(legs, 100, 100, nil)
	if got != want {
		t.Fatal("snapshot IV anchor changed")
	}
}

func TestGammaSkewSeparatesClassesAndPreservesWarningScope(t *testing.T) {
	var legs []legData
	for _, class := range []string{"SPX", "SPXW"} {
		vol := .2
		if class == "SPXW" {
			vol = .4
		}
		for _, strike := range []float64{90, 100, 110} {
			legs = append(legs, legData{strike: strike, iv: vol, expiryYMD: "20260917", tradingClass: class})
		}
	}
	curves, _, fallbacks := buildSkewCurves(legs, 100)
	if len(curves) != 2 || len(fallbacks) != 0 {
		t.Fatalf("class separation: %v %v", curves, fallbacks)
	}
	for key, curve := range curves {
		want := .2
		if strings.HasPrefix(key, "SPXW:") {
			want = .4
		}
		if math.Abs(curve.IVAtMoneyness(0)-want) > 1e-10 {
			t.Fatal("mixed class IVs")
		}
	}
	res := hydrateGammaComputed(&rpc.GammaZeroComputed{Warnings: []string{"skew_fallback:SPX:20260917"}})
	if len(res.WarningDetails) != 1 || res.WarningDetails[0].Code != "skew_fallback:spx:20260917" || res.WarningDetails[0].Severity != "methodology" {
		t.Fatalf("lost warning contract: %+v", res.WarningDetails)
	}
}

func TestGammaOptionSkewEvidenceBoundaries(t *testing.T) {
	if v, _ := gammaIVAtDelta([]gammaDeltaIV{{.1, .2}, {.2, .3}}, .25); v != nil {
		t.Fatal("extrapolated uncovered delta")
	}
	v, bracket := gammaIVAtDelta([]gammaDeltaIV{{.1, .2}, {.4, .5}}, .25)
	if v == nil || math.Abs(*v-.35) > 1e-12 || len(bracket) != 2 {
		t.Fatal("delta interpolation")
	}
	for _, putVol := range []float64{.15, .2, .3} {
		var legs []legData
		for _, call := range []bool{false, true} {
			vol := putVol
			if call {
				vol = .2
			}
			for _, delta := range []float64{.15, .35} {
				// Independent bisection of the Gaussian CDF gives strikes
				// bracketing the target at a constant observed volatility.
				lo, hi := -5.0, 5.0
				target := delta
				if !call {
					target = 1 - delta
				}
				for range 60 {
					mid := (lo + hi) / 2
					if normCDF(mid) < target {
						lo = mid
					} else {
						hi = mid
					}
				}
				tau := 30.0 / 365
				strike := 100 * math.Exp(.5*vol*vol*tau-(lo+hi)/2*vol*math.Sqrt(tau))
				legs = append(legs, legData{strike: strike, iv: vol, dte: tau, isCall: call, expiryYMD: "20261005", tradingClass: "SPXW", ivSource: gammaIVSourceModelTick})
			}
		}
		rows := gammaOptionSkews(legs, 100)
		if len(rows) != 1 || rows[0].SkewVolPoints == nil || math.Abs(*rows[0].SkewVolPoints-(putVol-.2)*100) > 1e-9 {
			t.Fatalf("skew without OI: %+v", rows)
		}
		for i := range legs {
			if !legs[i].isCall {
				legs[i].ivSource = gammaIVSourcePrevClose
			}
		}
		rows = gammaOptionSkews(legs, 100)
		if rows[0].SkewVolPoints != nil || rows[0].Reason != "put_25_delta_bracket_missing" {
			t.Fatal("prior-close IV counted as observed comparable skew")
		}
	}
}

func TestBriefGammaClassificationAndQuality(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		scope, sign string
		zero        *float64
		net         float64
		want        string
	}{
		{"spx", "", new(100.0), -10, "short_gamma"},
		{"spx", "positive", nil, 10, "long_gamma"},
		{"spy", "negative", nil, -10, "short_gamma"},
	} {
		c := &rpc.GammaZeroComputed{Scope: test.scope, AsOf: now, SpotUnderlying: 106, ZeroGamma: test.zero, GammaSign: test.sign, ProfileMetrics: &rpc.GammaProfileMetrics{GEXAtSpot: test.net}, Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}}
		if c.ZeroGamma != nil {
			c.GapPct = new(6.0)
		}
		row := composeBriefGamma(&rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: c}, true, now)
		if row.Regime != test.want || row.Insight == nil || row.Underlying != strings.ToUpper(test.scope) {
			t.Fatalf("misclassified brief: %+v", row)
		}
		if test.scope == "spy" && row.Status != rpc.BriefStatusDegraded {
			t.Fatal("SPY proxy presented as canonical")
		}
	}
}

func TestGammaBalancedAndPartialHorizonsRemainMeasured(t *testing.T) {
	c := &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX, SpotUnderlying: 100, GammaSign: "no_data", GammaTotalAbs: 20, ProfileMetrics: &rpc.GammaProfileMetrics{GEXAtSpot: 0, GrossGEXAtSpot: 20}, GammaSign1to7: "positive", GammaSignTerm: "positive"}
	status, regime := gammaZeroStatusAndRegime(c)
	if status != "none_in_window" || regime != "transition_gamma" || perIndexRegime(c) != "transition-gamma" {
		t.Fatal("balanced signed model became no data")
	}
	if got := classifyHorizonAgreement(c); got != "agree:partial_long" {
		t.Fatalf("partial agreement = %s", got)
	}
	if got := buildGammaInsight(c); got.Regime != "transition_gamma" || got.Horizons[0].Regime != "unavailable" {
		t.Fatal("missing horizon hidden")
	}
}
