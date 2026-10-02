package risk

import (
	"math"
	"slices"
	"time"
)

// CashSweepCalibrationLine is exact valuation and exit evidence for one held
// contract. Original clocks must be supplied by the source, not the planner.
type CashSweepCalibrationLine struct {
	ConID                                               int
	Currency, SecType, Right, ExerciseStyle             string
	Quantity, Multiplier, CurrentMark, Spot, Strike     float64
	Expiry                                              time.Time
	PriceOriginalAt, ExitOriginalAt                     time.Time
	PriceValidUntil, ExitValidUntil                     time.Time
	IV, RiskFreeRate, DividendYield                     *float64
	ADV20InPositionUnits, ExitSpreadUpper, ExitFeeUpper *float64
	ExactVanillaDeliverable                             bool
}

// CashSweepCalibrationInput freezes assumptions for a study. SessionEnds are
// real/frozen exchange session dates, never a weekday or calendar-day fallback.
// Shock numbers supplied in fixtures are synthetic, not new owner defaults.
type CashSweepCalibrationInput struct {
	AsOf                      time.Time
	Source, PolicyFingerprint string
	CommonReadSessionVerified bool
	SessionEnds               []time.Time
	Lines                     []CashSweepCalibrationLine
	BaseNAV, ProtectedFloor   *float64
	EURRates                  map[string]float64
	// NativeCash is the complete trade-date cash ledger, including borrowing.
	// It is valuation evidence, never settled cash or funding authority.
	NativeCash     map[string]float64
	CashReceiptAt  time.Time
	CashValidUntil time.Time
	// Valuation validity is the intersection of original NAV and FX source
	// validity. A planner clock cannot freshen either underlying observation.
	ValuationReceiptAt, ValuationValidUntil              time.Time
	ClusterDropPct, TakeoverGapPct, ExitParticipationPct float64
	VolShockPoints, RateShockBPS, FXAdversePct           *float64
}

// CashSweepCalibrationStudy is an advisory frozen/live-partial experiment.
// Even a complete study is not calibrated broker margin or spend authority.
type CashSweepCalibrationStudy struct {
	Sessions          int       `json:"sessions"`
	Source            string    `json:"source"`
	State             string    `json:"state"`
	PolicyFingerprint string    `json:"policy_fingerprint"`
	AsOf              time.Time `json:"as_of"`
	HorizonEnd        time.Time `json:"horizon_end,omitzero"`
	WorstLossEUR      *float64  `json:"worst_loss_eur,omitempty"`
	NAVAfterEUR       *float64  `json:"nav_after_eur,omitempty"`
	ProtectedFloor    *float64  `json:"protected_floor,omitempty"`
	ExitComplete      *bool     `json:"exit_complete,omitempty"`
	ExitFrictionEUR   *float64  `json:"exit_friction_eur,omitempty"`
	Gaps              []string  `json:"gaps,omitempty"`
}

// StudyCashSweepFiniteHorizons evaluates simultaneous approved downside and
// takeover-gap scenarios at 1/2/5 session horizons. It reprices vanilla options
// with a finite CRR tree, including American exercise, and tests measured exit
// participation. No future sale proceeds fund a reserve. Margin remains unknown.
func StudyCashSweepFiniteHorizons(in CashSweepCalibrationInput) []CashSweepCalibrationStudy {
	var out []CashSweepCalibrationStudy
	for _, horizon := range []int{1, 2, 5} {
		s := CashSweepCalibrationStudy{Sessions: horizon, Source: in.Source, State: "unavailable", PolicyFingerprint: in.PolicyFingerprint, AsOf: in.AsOf, ProtectedFloor: in.ProtectedFloor,
			Gaps: []string{"stressed_broker_margin_unavailable", "scenario_acceptance_uncommissioned", "assignment_exit_cash_timing_unreconciled"}}
		var missing []string
		if in.AsOf.IsZero() || in.Source != "frozen_synthetic" && in.Source != "live_partial" {
			missing = append(missing, "calibration_source_unavailable")
		}
		if in.Source == "live_partial" && !in.CommonReadSessionVerified {
			missing = append(missing, "common_read_session_provenance_unavailable")
		}
		if in.Source == "live_partial" && !sweepLiveEvidenceValid(in.ValuationReceiptAt, in.ValuationValidUntil, in.AsOf) {
			missing = append(missing, "valuation_source_validity_unavailable")
		}
		if len(in.SessionEnds) < horizon {
			missing = append(missing, "exchange_session_horizon_unavailable")
		} else {
			s.HorizonEnd = in.SessionEnds[horizon-1]
			prior := in.AsOf
			for _, end := range in.SessionEnds[:horizon] {
				if !end.After(prior) {
					missing = append(missing, "exchange_session_horizon_invalid")
				}
				prior = end
			}
		}
		if in.BaseNAV == nil || !sweepNonnegative(*in.BaseNAV) || in.ProtectedFloor == nil || !sweepNonnegative(*in.ProtectedFloor) {
			missing = append(missing, "protected_nav_source_unavailable")
		}
		if !sweepNonnegative(in.ClusterDropPct) || in.ClusterDropPct > 100 || !sweepNonnegative(in.TakeoverGapPct) || !sweepNonnegative(in.ExitParticipationPct) || in.ExitParticipationPct == 0 || in.ExitParticipationPct > 100 {
			missing = append(missing, "approved_risk_assumptions_unavailable")
		}
		cashLoss, cashGaps := sweepCalibrationCashLoss(in)
		missing = append(missing, cashGaps...)
		worstLoss, friction := 0., 0.
		exitComplete := true
		seen := map[int]bool{}
		for _, p := range in.Lines {
			if p.ConID <= 0 || seen[p.ConID] {
				missing = append(missing, "duplicate_or_missing_valuation_identity")
			}
			seen[p.ConID] = true
		}
		for _, direction := range []float64{-1, 1} {
			loss, scenarioFriction := cashLoss, 0.
			for _, p := range in.Lines {
				if p.Quantity == 0 {
					continue
				}
				if p.ConID <= 0 || p.Currency == "" || !sweepFinite(p.Quantity) || !sweepNonnegative(p.CurrentMark) || p.Multiplier <= 0 || !sweepFinite(p.Multiplier) || p.PriceOriginalAt.IsZero() || p.PriceOriginalAt.After(in.AsOf) {
					missing = append(missing, "valuation_identity_or_original_clock_unavailable")
					continue
				}
				if in.Source == "live_partial" && !sweepLiveEvidenceValid(p.PriceOriginalAt, p.PriceValidUntil, in.AsOf) {
					missing = append(missing, "price_source_validity_unavailable")
					continue
				}
				fx := in.EURRates[p.Currency]
				if !sweepNonnegative(fx) || fx == 0 || p.Currency == "EUR" && fx != 1 {
					missing = append(missing, "native_eur_valuation_unavailable")
					continue
				}
				fxStress := fx
				if p.Currency != "EUR" {
					if in.FXAdversePct == nil || !sweepNonnegative(*in.FXAdversePct) || *in.FXAdversePct >= 100 {
						missing = append(missing, "fx_shock_unreviewed")
						continue
					}
					if p.Quantity > 0 {
						fxStress *= 1 - *in.FXAdversePct/100
					} else {
						fxStress *= 1 + *in.FXAdversePct/100
					}
				}
				move := -in.ClusterDropPct / 100
				if direction > 0 {
					move = in.TakeoverGapPct / 100
				}
				mark := 0.
				switch p.SecType {
				case "STK":
					mark = p.CurrentMark * (1 + move)
				case "OPT":
					if !p.ExactVanillaDeliverable || p.IV == nil || p.RiskFreeRate == nil || p.DividendYield == nil || in.VolShockPoints == nil || in.RateShockBPS == nil || s.HorizonEnd.IsZero() || p.Expiry.IsZero() {
						missing = append(missing, "option_deliverable_vol_rate_dividend_or_expiry_unavailable")
						continue
					}
					var ok bool
					mark, ok = sweepOptionCRR(p.Right, p.ExerciseStyle, p.Spot*(1+move), p.Strike, max(0, p.Expiry.Sub(s.HorizonEnd).Hours()/(24*365)), *p.IV+*in.VolShockPoints, *p.RiskFreeRate+*in.RateShockBPS/10000, *p.DividendYield)
					if !ok {
						missing = append(missing, "option_repricing_unavailable")
						continue
					}
				default:
					missing = append(missing, "finite_instrument_repricing_unavailable")
					continue
				}
				loss += p.Quantity * p.Multiplier * (p.CurrentMark*fx - mark*fxStress)
				if p.ADV20InPositionUnits == nil || !sweepNonnegative(*p.ADV20InPositionUnits) || *p.ADV20InPositionUnits == 0 || p.ExitSpreadUpper == nil || !sweepNonnegative(*p.ExitSpreadUpper) || *p.ExitSpreadUpper >= 1 || p.ExitFeeUpper == nil || !sweepNonnegative(*p.ExitFeeUpper) || p.ExitOriginalAt.IsZero() || p.ExitOriginalAt.After(in.AsOf) {
					missing = append(missing, "exit_volume_window_spread_fee_or_original_clock_unavailable")
					continue
				}
				if in.Source == "live_partial" && !sweepLiveEvidenceValid(p.ExitOriginalAt, p.ExitValidUntil, in.AsOf) {
					missing = append(missing, "exit_source_validity_unavailable")
					continue
				}
				if math.Abs(p.Quantity) > *p.ADV20InPositionUnits*in.ExitParticipationPct/100*float64(horizon) {
					exitComplete = false
				}
				scenarioFriction += (math.Abs(p.Quantity)*p.Multiplier*mark**p.ExitSpreadUpper + *p.ExitFeeUpper) * fxStress
			}
			worstLoss = max(worstLoss, loss+scenarioFriction)
			friction = max(friction, scenarioFriction)
		}
		if !sweepNonnegative(worstLoss) || !sweepNonnegative(friction) {
			missing = append(missing, "calibration_arithmetic_invalid")
		}
		if len(missing) == 0 {
			s.State = "study_complete"
			s.WorstLossEUR = new(worstLoss)
			s.NAVAfterEUR = new(*in.BaseNAV - worstLoss)
			s.ExitComplete = new(exitComplete)
			s.ExitFrictionEUR = new(friction)
		}
		s.Gaps = append(s.Gaps, missing...)
		slices.Sort(s.Gaps)
		s.Gaps = slices.Compact(s.Gaps)
		out = append(out, s)
	}
	return out
}

// Cash FX belongs in total NAV stress even when no securities are held.
// As with security FX, opposing exposures receive no hedge credit here;
// the component-wise adverse envelope is conservative, not a forecast.
func sweepCalibrationCashLoss(in CashSweepCalibrationInput) (float64, []string) {
	if len(in.NativeCash) == 0 || in.CashReceiptAt.IsZero() || in.CashReceiptAt.After(in.AsOf) {
		return 0, []string{"complete_native_cash_valuation_unavailable"}
	}
	if in.Source == "live_partial" && !sweepLiveEvidenceValid(in.CashReceiptAt, in.CashValidUntil, in.AsOf) {
		return 0, []string{"cash_source_validity_unavailable"}
	}
	if _, ok := in.NativeCash["EUR"]; !ok || in.EURRates["EUR"] != 1 {
		return 0, []string{"complete_native_cash_valuation_unavailable"}
	}
	for _, p := range in.Lines {
		if p.Quantity != 0 {
			if _, known := in.NativeCash[p.Currency]; !known {
				return 0, []string{"complete_native_cash_valuation_unavailable"}
			}
		}
	}
	loss := 0.
	for ccy, cash := range in.NativeCash {
		fx := in.EURRates[ccy]
		if ccy == "" || !sweepFinite(cash) || !sweepFinite(fx) || fx <= 0 || ccy == "EUR" && fx != 1 {
			return 0, []string{"native_cash_eur_valuation_unavailable"}
		}
		if ccy == "EUR" || cash == 0 {
			continue
		}
		if in.FXAdversePct == nil || !sweepNonnegative(*in.FXAdversePct) || *in.FXAdversePct >= 100 {
			return 0, []string{"fx_shock_unreviewed"}
		}
		loss += math.Abs(cash) * fx * (*in.FXAdversePct / 100)
	}
	if !sweepNonnegative(loss) {
		return 0, []string{"calibration_arithmetic_invalid"}
	}
	return loss, nil
}

func sweepLiveEvidenceValid(originalAt, validUntil, now time.Time) bool {
	return !originalAt.IsZero() && !now.IsZero() && !originalAt.After(now) && validUntil.After(now) && validUntil.After(originalAt)
}

// sweepOptionCRR prices exact vanilla contracts under explicit frozen inputs.
// The tree has bounded cost; invalid risk-neutral probabilities remain unknown.
func sweepOptionCRR(right, style string, spot, strike, years, vol, rate, dividend float64) (float64, bool) {
	if right != "P" && right != "C" || style != "american" && style != "european" || !sweepNonnegative(spot) || !sweepNonnegative(strike) || !sweepNonnegative(years) || !sweepFinite(rate) || !sweepFinite(dividend) {
		return 0, false
	}
	intrinsic := func(s float64) float64 {
		if right == "C" {
			return max(0, s-strike)
		}
		return max(0, strike-s)
	}
	if years == 0 {
		return intrinsic(spot), true
	}
	if !sweepNonnegative(vol) || vol == 0 {
		return 0, false
	}
	const steps = 128
	dt := years / steps
	up := math.Exp(vol * math.Sqrt(dt))
	down := 1 / up
	prob := (math.Exp((rate-dividend)*dt) - down) / (up - down)
	discount := math.Exp(-rate * dt)
	if !sweepNonnegative(prob) || prob > 1 || !sweepNonnegative(discount) {
		return 0, false
	}
	values := make([]float64, steps+1)
	for j := range values {
		values[j] = intrinsic(spot * math.Pow(up, float64(j)) * math.Pow(down, float64(steps-j)))
	}
	for n := steps - 1; n >= 0; n-- {
		for j := 0; j <= n; j++ {
			values[j] = discount * (prob*values[j+1] + (1-prob)*values[j])
			if style == "american" {
				values[j] = max(values[j], intrinsic(spot*math.Pow(up, float64(j))*math.Pow(down, float64(n-j))))
			}
		}
	}
	return values[0], sweepNonnegative(values[0])
}

// CloneCashSweepCalibrationStudies isolates pointers and named study gaps.
func CloneCashSweepCalibrationStudies(in []CashSweepCalibrationStudy) []CashSweepCalibrationStudy {
	if in == nil {
		return nil
	}
	out := slices.Clone(in)
	clone := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		return new(*p)
	}
	for i := range out {
		s := &out[i]
		s.Gaps = slices.Clone(s.Gaps)
		s.WorstLossEUR = clone(s.WorstLossEUR)
		s.NAVAfterEUR = clone(s.NAVAfterEUR)
		s.ProtectedFloor = clone(s.ProtectedFloor)
		s.ExitFrictionEUR = clone(s.ExitFrictionEUR)
		if s.ExitComplete != nil {
			s.ExitComplete = new(*s.ExitComplete)
		}
	}
	return out
}
