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
	return CashSweepCalibrationInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: pol.FingerprintKey(), SessionEnds: ends, BaseNAV: new(250000.), ProtectedFloor: new(200000.), EURRates: map[string]float64{"EUR": 1, "USD": .9}, NativeCash: map[string]float64{"EUR": 0, "USD": 0}, CashReceiptAt: now.Add(-time.Second), ClusterDropPct: pol.ClusterDropPct, TakeoverGapPct: pol.TakeoverGapPct, ExitParticipationPct: pol.ExitParticipationPct,
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

func TestCashSweepCalibrationCashFXAndBorrowingAffectWholeNAV(t *testing.T) {
	for _, native := range []float64{100000, -100000} {
		in := sweepCalibrationFixture()
		in.Lines = nil
		in.NativeCash["USD"] = native
		in.FXAdversePct = new(10.)
		for _, s := range StudyCashSweepFiniteHorizons(in) {
			if s.State != "study_complete" || s.WorstLossEUR == nil || math.Abs(*s.WorstLossEUR-9000) > 1e-8 || s.NAVAfterEUR == nil || math.Abs(*s.NAVAfterEUR-241000) > 1e-8 {
				t.Fatalf("cash or borrowing FX disappeared from total NAV: native=%v study=%+v", native, s)
			}
		}
	}
}

func TestCashSweepCalibrationIncompleteCashCannotCertifyNAV(t *testing.T) {
	for _, variant := range []string{"missing", "empty", "base", "position_currency", "clock", "future", "cash_nonfinite", "cash_fx", "shock", "overflow"} {
		t.Run(variant, func(t *testing.T) {
			in := sweepCalibrationFixture()
			switch variant {
			case "missing":
				in.NativeCash = nil
			case "empty":
				in.NativeCash = map[string]float64{}
			case "base":
				delete(in.NativeCash, "EUR")
			case "position_currency":
				in.Lines[0].Currency = "USD"
				delete(in.NativeCash, "USD")
			case "clock":
				in.CashReceiptAt = time.Time{}
			case "future":
				in.CashReceiptAt = in.AsOf.Add(time.Second)
			case "cash_nonfinite":
				in.NativeCash["USD"] = math.NaN()
			case "cash_fx":
				delete(in.EURRates, "USD")
			case "shock":
				in.NativeCash["USD"] = 100000
			case "overflow":
				in.NativeCash["USD"] = math.MaxFloat64
				in.EURRates["USD"] = math.MaxFloat64
				in.FXAdversePct = new(10.)
			}
			for _, s := range StudyCashSweepFiniteHorizons(in) {
				if s.State == "study_complete" || s.WorstLossEUR != nil || s.NAVAfterEUR != nil {
					t.Fatalf("unknown cash certified total NAV: %+v", s)
				}
			}
		})
	}
}

func liveSweepCalibrationFixture() CashSweepCalibrationInput {
	in := sweepCalibrationFixture()
	in.Source, in.CommonReadSessionVerified = "live_partial", true
	in.CashValidUntil = in.CashReceiptAt.Add(15 * time.Second)
	in.ValuationReceiptAt, in.ValuationValidUntil = in.AsOf.Add(-time.Second), in.AsOf.Add(time.Second)
	for i := range in.Lines {
		in.Lines[i].PriceValidUntil = in.AsOf.Add(time.Second)
		// The measured volume window may be historical. Its source, spread
		// and fee validity ends at an explicit expiry, not at the window date.
		in.Lines[i].ExitValidUntil = in.AsOf.Add(time.Second)
	}
	return in
}

func TestCashSweepLiveSourceExpiryDoesNotRefreshOriginalClocks(t *testing.T) {
	for _, s := range StudyCashSweepFiniteHorizons(liveSweepCalibrationFixture()) {
		if s.State != "study_complete" || s.NAVAfterEUR == nil {
			t.Fatalf("current original-source control failed: %+v", s)
		}
	}
	for _, source := range []string{"cash", "valuation", "price", "exit"} {
		for _, variant := range []string{"missing", "expired", "boundary", "invalid_interval", "future_original", "old_receipt_and_expiry"} {
			t.Run(source+"/"+variant, func(t *testing.T) {
				in := liveSweepCalibrationFixture()
				var original, until *time.Time
				switch source {
				case "cash":
					original, until = &in.CashReceiptAt, &in.CashValidUntil
				case "valuation":
					original, until = &in.ValuationReceiptAt, &in.ValuationValidUntil
				case "price":
					original, until = &in.Lines[0].PriceOriginalAt, &in.Lines[0].PriceValidUntil
				case "exit":
					original, until = &in.Lines[0].ExitOriginalAt, &in.Lines[0].ExitValidUntil
				}
				switch variant {
				case "missing":
					*until = time.Time{}
				case "expired":
					*until = in.AsOf.Add(-time.Nanosecond)
				case "boundary":
					*until = in.AsOf
				case "invalid_interval":
					*until = original.Add(-time.Second)
				case "future_original":
					*original = in.AsOf.Add(time.Nanosecond)
				case "old_receipt_and_expiry":
					*original = in.AsOf.Add(-365 * 24 * time.Hour)
					*until = original.Add(15 * time.Second)
				}
				for _, s := range StudyCashSweepFiniteHorizons(in) {
					if s.State == "study_complete" || s.WorstLossEUR != nil || s.NAVAfterEUR != nil {
						t.Fatalf("invalid original source emitted numeric total NAV: %+v", s)
					}
				}
			})
		}
	}
	// Frozen experiments retain historical source timestamps; no current
	// live-source validity is invented for a locked synthetic snapshot.
	in := sweepCalibrationFixture()
	in.CashReceiptAt = in.AsOf.Add(-365 * 24 * time.Hour)
	for _, s := range StudyCashSweepFiniteHorizons(in) {
		if s.State != "study_complete" {
			t.Fatalf("frozen snapshot was treated as a current live read: %+v", s)
		}
	}
}
