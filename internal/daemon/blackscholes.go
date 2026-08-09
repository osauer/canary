package daemon

import "math"

// bsGamma computes the Black-Scholes gamma of a European option leg.
// put-call parity makes their first derivatives differ only by a
//
//	t      — time to expiry in years; must be > 0
//	vol    — implied volatility as a decimal (0.20 == 20 %); must be > 0
func bsGamma(spot, strike, t, vol, r, q float64) float64 {
	if spot <= 0 || strike <= 0 || t <= 0 || vol <= 0 {
		return 0
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(spot/strike) + (r-q+0.5*vol*vol)*t) / (vol * sqrtT)
	// Standard-normal pdf: φ(x) = exp(-x²/2) / √(2π).
	pdf := math.Exp(-0.5*d1*d1) / math.Sqrt(2*math.Pi)
	return pdf / (spot * vol * sqrtT)
}

// dealerGEX returns the dollar gamma per 1 % move attributable to a
func dealerGEX(gamma, openInt float64, multiplier int, spot float64, isCall bool) float64 {
	if openInt == 0 || multiplier == 0 || spot <= 0 {
		return 0
	}
	contrib := gamma * openInt * float64(multiplier) * spot * spot * 0.01
	if isCall {
		return contrib
	}
	return -contrib
}

// absGEX returns the sign-agnostic magnitude contribution of a single
func absGEX(gamma, openInt float64, multiplier int, spot float64) float64 {
	if openInt == 0 || multiplier == 0 || spot <= 0 {
		return 0
	}
	return math.Abs(gamma) * openInt * float64(multiplier) * spot * spot * 0.01
}

// normCDF is the standard-normal cumulative distribution function. Used
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// bsCallPrice returns the Black-Scholes call price for the given inputs.
func bsCallPrice(spot, strike, t, vol, r, q float64) float64 {
	if spot <= 0 || strike <= 0 || t <= 0 || vol <= 0 {
		return 0
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(spot/strike) + (r-q+0.5*vol*vol)*t) / (vol * sqrtT)
	d2 := d1 - vol*sqrtT
	return spot*math.Exp(-q*t)*normCDF(d1) - strike*math.Exp(-r*t)*normCDF(d2)
}

// bsVega returns the Black-Scholes vega — ∂C/∂σ — for the given inputs.
func bsVega(spot, strike, t, vol, r, q float64) float64 {
	if spot <= 0 || strike <= 0 || t <= 0 || vol <= 0 {
		return 0
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(spot/strike) + (r-q+0.5*vol*vol)*t) / (vol * sqrtT)
	pdf := math.Exp(-0.5*d1*d1) / math.Sqrt(2*math.Pi)
	return spot * math.Exp(-q*t) * pdf * sqrtT
}

// bsImpliedVolatility back-solves for σ from an observed option price via
func bsImpliedVolatility(price, spot, strike, t, r, q float64, isCall bool) float64 {
	const (
		// Minimum DTE: 1 hour. Below this, vega → 0 and Newton-Raphson
		minDTE = 1.0 / (365.0 * 24.0)

		// Initial-guess band. Brenner-Subrahmanyam (1988) is accurate ATM
		minInitialSigma = 0.15
		fallbackSigma   = 0.3
		maxInitialBound = 2.0 // BS-S above this is degenerate input
		maxInitialClamp = 1.0 // …pin it to a high-but-realistic start

		// Convergence + acceptance bounds. A 1 % or 500 % implied vol on
		tolerance       = 1e-5
		maxIters        = 50
		minAcceptSigma  = 0.01
		maxAcceptSigma  = 5.0
		minVega         = 1e-8
		minIterateSigma = 1e-4
		maxIterateSigma = 10.0
	)
	if price <= 0 || spot <= 0 || strike <= 0 {
		return 0
	}
	if t < minDTE {
		return 0
	}
	// Convert put price to equivalent call price via put-call parity:
	target := price
	if !isCall {
		target = price + spot*math.Exp(-q*t) - strike*math.Exp(-r*t)
		if target <= 0 {
			// Parity says the equivalent call has non-positive value —
			return 0
		}
	}
	// Intrinsic check (on the call-equivalent target). Discount factors
	intrinsic := math.Max(0, spot*math.Exp(-q*t)-strike*math.Exp(-r*t))
	if target < intrinsic {
		return 0
	}

	// Brenner-Subrahmanyam initial guess. Low-side outliers (deep OTM
	sigma := math.Sqrt(2*math.Pi/t) * (target / spot)
	if math.IsNaN(sigma) || sigma < minInitialSigma {
		sigma = fallbackSigma
	} else if sigma > maxInitialBound {
		sigma = maxInitialClamp
	}

	for range maxIters {
		modelPrice := bsCallPrice(spot, strike, t, sigma, r, q)
		diff := modelPrice - target
		if math.Abs(diff) < tolerance {
			if sigma < minAcceptSigma || sigma > maxAcceptSigma {
				return 0
			}
			return sigma
		}
		vega := bsVega(spot, strike, t, sigma, r, q)
		if vega < minVega {
			// Vega collapsed — typically far OTM with very little time
			return 0
		}
		sigma -= diff / vega
		// Recovery clamp: prevent an overshoot into pathological land.
		if sigma <= 0 {
			sigma = minIterateSigma
		}
		if sigma > maxIterateSigma {
			sigma = maxIterateSigma
		}
	}
	// No convergence inside maxIters; refuse rather than return whatever
	return 0
}
