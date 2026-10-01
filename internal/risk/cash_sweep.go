package risk

import "math"

// CashSweepIncrementalGain compares bill redemption at par with all-in entry
// cost and a conservative annual cash-interest bound, in account base currency.
// The quoted entry includes spread. Continuous compounding bounds interest
// reinvested at the stated nominal annual upper rate. It is a planning estimate,
// never a guaranteed return or an early-liquidation valuation.
func CashSweepIncrementalGain(face, entry, fee, annualUpper, days, fx float64) (float64, bool) {
	for _, v := range []float64{face, entry, fee, annualUpper, days, fx} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false
		}
	}
	if face <= 0 || entry <= 0 || days <= 0 || fx <= 0 {
		return 0, false
	}
	allIn := entry + fee
	interest := allIn * math.Expm1(annualUpper*days/365)
	gain := (face - allIn - interest) * fx
	return gain, !math.IsNaN(gain) && !math.IsInf(gain, 0)
}
