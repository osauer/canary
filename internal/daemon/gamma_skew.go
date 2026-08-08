package daemon

import "math"

// SkewCurve is a quadratic fit of implied volatility against
// log-moneyness for one option expiry: σ(m) = A + B·m + C·m², with
// m = ln(K / S). The sweep uses it to reprice each leg's IV at every
// scenario spot's moneyness rather than holding the captured snapshot
// IV fixed — the sticky-moneyness convention.
//
// Why bother: the legacy sticky-IV recipe biases zero-gamma upward
// because real SPX skew is steep (OTM puts trade richer than ATM, OTM
// calls cheaper). When the sweep walks spot down 5 %, the dealer-short
// puts that used to be 5 % OTM are now ATM and their true IV is lower,
// not the captured-at-snapshot value. Sticky-moneyness recomputes σ at
// each scenario spot's strike/spot ratio so the leg's gamma reflects
// the IV the leg WOULD have at that scenario spot. Empirically this
// shifts zero-gamma by ~30-80 SPX points and tracks SpotGamma's posted
// numbers materially better.
//
// mLo / mHi are the moneyness range we fitted over. Evaluating outside
// the range extrapolates a parabola — wild. The IVAtMoneyness method
// clamps to [mLo, mHi] before evaluating; the curve outside that
// window flattens to the boundary value rather than projecting.
//
// nPoints is the number of (m, σ) samples in the fit; ok=false when
// fewer than 3 points were available (degenerate; the caller falls
// back to sticky-IV for that expiry).
type SkewCurve struct {
	A, B, C  float64
	nPoints  int
	ok       bool
	mLo, mHi float64
}

// IVAtMoneyness evaluates the curve at moneyness m = ln(K / S). Clamps
// m to the fitted range before evaluating so the parabolic
// extrapolation outside the fit window doesn't return runaway IVs at
// the sweep's outer edges. Returns 0 when the curve is unfit (ok=false)
// — the caller maps that to sticky-IV fallback for the affected expiry.
//
// The boundary clamp is the right honest call: outside the fitted
// range we have no information about how skew curves, so freezing the
// IV to the closest boundary value reads as "best guess from observed
// data" rather than "parabolic projection beyond the data."
func (s *SkewCurve) IVAtMoneyness(m float64) float64 {
	if !s.ok {
		return 0
	}
	if m < s.mLo {
		m = s.mLo
	} else if m > s.mHi {
		m = s.mHi
	}
	return s.A + s.B*m + s.C*m*m
}

// fitSkewCurve runs a least-squares fit of σ against (m, m²) over the
// legs of one expiry. snapshotSpot is the SPY spot used to convert
// strikes into moneyness. Minimum 3 points to fit a quadratic; below
// that the curve is marked !ok and the caller falls back to sticky-IV
// for that expiry.
//
// The normal equations solve a 3×3 system on the moments of (m, σ).
// For 50-100 legs per expiry this is microseconds — the call site
// runs it once per expiry after the fan-out completes, well before
// the sweep itself.
//
// Calls and puts are both included on the same expiry curve: at a
// given strike call IV and put IV must match (put-call parity), so
// the two rights' data points fall on the same skew surface and
// pooling them doubles the fit's effective sample size.
func fitSkewCurve(legs []legData, snapshotSpot float64) SkewCurve {
	if snapshotSpot <= 0 {
		return SkewCurve{}
	}
	// Build (m, σ) samples and bound the moneyness range.
	mLo := math.Inf(1)
	mHi := math.Inf(-1)
	var ms, sigmas []float64
	for _, l := range legs {
		if l.iv <= 0 || l.strike <= 0 {
			continue
		}
		m := math.Log(l.strike / snapshotSpot)
		ms = append(ms, m)
		sigmas = append(sigmas, l.iv)
		if m < mLo {
			mLo = m
		}
		if m > mHi {
			mHi = m
		}
	}
	if len(ms) < 3 {
		return SkewCurve{nPoints: len(ms)}
	}
	// Normal-equation solve for σ = A + B·m + C·m². The design matrix
	// has columns (1, m, m²); X^T·X is a 3×3 with the standard
	// moment-of-m structure:
	//
	//   [ n     Σm    Σm²  ] [A]   [Σσ      ]
	//   [ Σm    Σm²   Σm³  ] [B] = [Σ(m·σ)  ]
	//   [ Σm²   Σm³   Σm⁴  ] [C]   [Σ(m²·σ) ]
	//
	// Solved analytically below (Cramer's rule on a 3×3 — cheap and
	// avoids pulling in a matrix library for one solve).
	n := float64(len(ms))
	var s1, s2, s3, s4 float64
	var t0, t1, t2 float64
	for i, m := range ms {
		σ := sigmas[i]
		mm := m * m
		s1 += m
		s2 += mm
		s3 += mm * m
		s4 += mm * mm
		t0 += σ
		t1 += m * σ
		t2 += mm * σ
	}
	// Cramer's rule. det of the 3×3 X^T·X matrix:
	det := n*(s2*s4-s3*s3) - s1*(s1*s4-s2*s3) + s2*(s1*s3-s2*s2)
	if math.Abs(det) < 1e-12 {
		// Degenerate fit (collinear m values, or all-zero σ). Mark unfit.
		return SkewCurve{nPoints: len(ms)}
	}
	// Replace columns one at a time for each unknown:
	detA := t0*(s2*s4-s3*s3) - s1*(t1*s4-t2*s3) + s2*(t1*s3-t2*s2)
	detB := n*(t1*s4-t2*s3) - t0*(s1*s4-s2*s3) + s2*(s1*t2-s2*t1)
	detC := n*(s2*t2-s3*t1) - s1*(s1*t2-s2*t1) + t0*(s1*s3-s2*s2)
	return SkewCurve{
		A:       detA / det,
		B:       detB / det,
		C:       detC / det,
		nPoints: len(ms),
		ok:      true,
		mLo:     mLo,
		mHi:     mHi,
	}
}

// skewFitStats computes R² and the residual RMS (in IV units) in one
// pass. The two diagnose different failures: R² is relative to the
// smile's amplitude across strikes, so it collapses on flat smiles
// regardless of fit error; the RMS bounds the absolute IV error the
// sweep's repricing inherits regardless of amplitude. Both are zero
// when the curve is unfit; on a zero-variance smile R² is 0 (undefined,
// clamped) while the RMS stays meaningful.
func skewFitStats(curve SkewCurve, legs []legData, snapshotSpot float64) (r2, residualRMS float64) {
	if !curve.ok || snapshotSpot <= 0 {
		return 0, 0
	}
	var sigmas []float64
	var residSqSum float64
	for _, l := range legs {
		if l.iv <= 0 || l.strike <= 0 {
			continue
		}
		m := math.Log(l.strike / snapshotSpot)
		pred := curve.A + curve.B*m + curve.C*m*m
		resid := l.iv - pred
		residSqSum += resid * resid
		sigmas = append(sigmas, l.iv)
	}
	if len(sigmas) == 0 {
		return 0, 0
	}
	residualRMS = math.Sqrt(residSqSum / float64(len(sigmas)))
	var mean float64
	for _, σ := range sigmas {
		mean += σ
	}
	mean /= float64(len(sigmas))
	var totSqSum float64
	for _, σ := range sigmas {
		d := σ - mean
		totSqSum += d * d
	}
	if totSqSum < 1e-12 {
		return 0, residualRMS
	}
	r2 = 1.0 - residSqSum/totSqSum
	if r2 < 0 {
		// A negative R² means the fit is worse than the mean — keep
		// the renderer's expectation that R² is in [0, 1] by clamping
		// rather than confusing readers with a negative.
		r2 = 0
	}
	return r2, residualRMS
}
