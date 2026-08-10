package daemon

import "math"

// bsGamma computes the Black-Scholes gamma of a European option leg.
//
// Gamma is the second derivative of option price with respect to the
// underlying spot — i.e., how fast delta changes as spot moves. It's
// identical for calls and puts at the same strike/expiry/vol because
// put-call parity makes their first derivatives differ only by a
// constant. For dealer-gamma aggregation we compute one γ per (spot,
// strike, vol, t) tuple and let the caller assign the sign per the
// dealer-positioning convention.
//
// Formula: γ = φ(d1) / (S · σ · √t)
//
// where d1 = ( ln(S/K) + (r − q + σ²/2) · t ) / ( σ · √t )
// and φ is the standard normal probability density.
//
// Inputs:
//
//	spot   — current price of the underlying (S)
//	strike — option strike (K)
//	t      — time to expiry in years; must be > 0
//	vol    — implied volatility as a decimal (0.20 == 20 %); must be > 0
//	r      — risk-free rate (decimal); typically 0 for the zero-gamma
//	         aggregation since rate sensitivity over the [0.85, 1.15]
//	         sweep is negligible relative to vol-skew effects
//	q      — continuous dividend yield (decimal); 0 for SPX index options
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
// single option leg, signed under the Perfiliev convention: positive
// for calls, negative for puts. The aggregate over a chain is the
// dealer-positioning estimate at the given spot; a sign flip across
// adjacent spot scenarios is the "zero-gamma" crossing.
//
// Formula: GEX = sign · γ · OI · multiplier · spot² · 0.01
//
// Notes:
//   - multiplier is 100 for SPX index options (and most US-listed
//     equity options); the caller passes it explicitly to avoid
//     hardcoding the SPX assumption into the math.
//   - The 0.01 scales gamma per $1 of underlying into gamma per 1 %
//     of underlying — standard practitioner units that match every
//     published GEX number you can cross-check against.
//   - Returns 0 when γ degenerates (see bsGamma) so missing-data legs
//     don't poison the sum.
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
// Newton-Raphson on the BS call-price function. Puts are converted to
// the equivalent call via put-call parity (P + S·exp(-qT) = C + K·exp(-rT))
// before the solve, so a single forward function handles both rights.
//
// Inputs:
//
//	price    — observed option premium (per-share, not per-contract)
//	spot     — current underlying price
//	strike   — option strike
//	t        — years to expiry
//	r, q     — risk-free rate and dividend yield (decimals; both 0 is
//	           fine for the SPY zero-gamma compute, where the sweep
//	           horizon is hours-to-weeks and a 0.05 rate at 0.02 dividend
//	           perturbs the implied σ by < 0.5 vol points)
//	isCall   — true for calls, false for puts
//
// Initial guess uses Brenner-Subrahmanyam (1988): σ₀ ≈ √(2π/T)·(C/S).
// For puts we form the parity-equivalent call price first. The iteration
// terminates on |BSprice − target| < 1e-5 or 50 iterations.
//
// Returns 0 (the "unsolved" sentinel matching the bsGamma convention)
// when:
//
//   - any of price/spot/strike ≤ 0
//   - t < 1 hour: vega → 0 near expiry, Newton-Raphson becomes unstable
//     and a tiny pricing error produces an enormous IV swing
//   - intrinsic value > price: the price is stale or violates no-arbitrage,
//     the implied σ would be imaginary
//   - the solver converges to σ outside [0.01, 5.0]: a 1 % or 500 %
//     implied vol on a listed equity option is almost certainly a stale
//     deep-OTM print rather than a real market state
//
// The bounds are intentionally wide: SPY weeklies during quiet sessions
// price at ~12 % IV, 2018-style stress events touched 80 %+ on the
// front month, and the upper bound exists to refuse obvious-bad rather
// than to cap genuine signal.
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
		// a listed SPY weekly is almost certainly a stale deep-OTM print,
		// not real market state — refuse rather than propagate.
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
