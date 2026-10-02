package risk

import (
	"math"
	"slices"
	"testing"
	"time"
)

func sweepCalibrationFixture() CashSweepCalibrationInput {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	pol := DefaultRulebookPolicy()
	ends := []time.Time{now.AddDate(0, 0, 3), now.AddDate(0, 0, 4), now.AddDate(0, 0, 5), now.AddDate(0, 0, 6), now.AddDate(0, 0, 7)}
	return CashSweepCalibrationInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: pol.FingerprintKey(), SessionEnds: ends, BaseNAV: new(250000.), ProtectedFloor: new(200000.), EURRates: map[string]float64{"EUR": 1, "USD": .9}, ClusterDropPct: pol.ClusterDropPct, TakeoverGapPct: pol.TakeoverGapPct, ExitParticipationPct: pol.ExitParticipationPct,
		Lines: []CashSweepCalibrationLine{{ConID: 101, Currency: "EUR", SecType: "STK", Quantity: 50, Multiplier: 1, CurrentMark: 100, PriceOriginalAt: now.Add(-time.Second), ExitOriginalAt: now.Add(-time.Minute), ADV20InPositionUnits: new(100.), ExitSpreadUpper: new(.01), ExitFeeUpper: new(1.)}}}
}

func TestCashSweepFrozenOneTwoFiveSessionExperiments(t *testing.T) {
	in := sweepCalibrationFixture()
	out := StudyCashSweepFiniteHorizons(in)
	for i, s := range out {
		if s.State != "study_complete" || s.WorstLossEUR == nil || math.Abs(*s.WorstLossEUR-1536) > 1e-6 || s.NAVAfterEUR == nil || !slices.Contains(s.Gaps, "stressed_broker_margin_unavailable") {
			t.Fatal(s)
		}
		if *s.ExitComplete != (i == 2) {
			t.Fatal("1/2/5 horizon did not reflect measured participation", s)
		}
		t.Logf("frozen horizon=%d sessions lossEUR=%.2f NAV=%.2f exitComplete=%t; margin/admission unavailable", s.Sessions, *s.WorstLossEUR, *s.NAVAfterEUR, *s.ExitComplete)
	}
}

func TestCashSweepFiniteOptionRepricingEuropeanAndAmerican(t *testing.T) {
	call, ok := sweepOptionCRR("C", "european", 100, 100, 1, .2, 0, 0)
	if !ok || math.Abs(call-7.965567) > 0.03 {
		t.Fatalf("European tree mismatches analytic frozen call: %.8f %t", call, ok)
	}
	eur, ok := sweepOptionCRR("P", "european", 60, 100, 1, .2, .05, 0)
	am, ok2 := sweepOptionCRR("P", "american", 60, 100, 1, .2, .05, 0)
	if !ok || !ok2 || am < 40 || am < eur {
		t.Fatal("American early exercise ignored", eur, am)
	}
}

func TestCashSweepFrozenStressSensitivityMatrix(t *testing.T) {
	for _, shock := range []struct {
		name          string
		vol, rate, fx float64
	}{{"study-A", .10, 100, 5}, {"study-B", .20, 200, 10}} {
		in := sweepCalibrationFixture()
		in.VolShockPoints = new(shock.vol)
		in.RateShockBPS = new(shock.rate)
		in.FXAdversePct = new(shock.fx)
		expiry := time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC)
		mark, ok := sweepOptionCRR("P", "american", 100, 100, expiry.Sub(in.AsOf).Hours()/(24*365), .25, .02, 0)
		if !ok {
			t.Fatal("fixture option price unavailable")
		}
		in.Lines = append(in.Lines, CashSweepCalibrationLine{ConID: 201, Currency: "USD", SecType: "OPT", Right: "P", ExerciseStyle: "american", Quantity: -5, Multiplier: 100, CurrentMark: mark, Spot: 100, Strike: 100, Expiry: expiry,
			PriceOriginalAt: in.AsOf.Add(-time.Second), ExitOriginalAt: in.AsOf.Add(-time.Minute), IV: new(.25), RiskFreeRate: new(.02), DividendYield: new(0.), ExactVanillaDeliverable: true,
			ADV20InPositionUnits: new(100.), ExitSpreadUpper: new(.02), ExitFeeUpper: new(5.)})
		for _, s := range StudyCashSweepFiniteHorizons(in) {
			if s.State != "study_complete" || s.WorstLossEUR == nil || *s.WorstLossEUR <= 1536 {
				t.Fatal(s)
			}
			t.Logf("frozen %s horizon=%d volShock=%.0fpoints rateShock=%.0fbps FXShock=%.0f%% lossEUR=%.2f NAV=%.2f exitComplete=%t; no admission", shock.name, s.Sessions, shock.vol*100, shock.rate, shock.fx, *s.WorstLossEUR, *s.NAVAfterEUR, *s.ExitComplete)
		}
	}
}

func TestCashSweepCalibrationMissingEvidenceDoesNotBecomeZero(t *testing.T) {
	for _, kind := range []string{"session", "fx", "volume", "fee", "clock", "identity", "volatility"} {
		t.Run(kind, func(t *testing.T) {
			in := sweepCalibrationFixture()
			switch kind {
			case "session":
				in.SessionEnds = nil
			case "fx":
				in.Lines[0].Currency = "USD"
			case "volume":
				in.Lines[0].ADV20InPositionUnits = nil
			case "fee":
				in.Lines[0].ExitFeeUpper = nil
			case "clock":
				in.Lines[0].PriceOriginalAt = time.Time{}
			case "identity":
				in.Lines = append(in.Lines, in.Lines[0])
			case "volatility":
				in.Lines[0].SecType = "OPT"
				in.Lines[0].Right = "C"
			}
			for _, s := range StudyCashSweepFiniteHorizons(in) {
				if s.State != "unavailable" || s.WorstLossEUR != nil || s.NAVAfterEUR != nil || s.ExitComplete != nil {
					t.Fatal("missing calibration evidence became known", s)
				}
			}
		})
	}
}
