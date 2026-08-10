package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// handleGammaZeroSPX returns the current dealer zero-gamma estimate for
// and reports Status="computing" only when no serveable result exists.
//
//	only on success; otherwise force supersedes the in-flight/error state.
func (s *Server) handleGammaZeroSPX(ctx context.Context, req *rpc.Request) (*rpc.GammaZeroSPXResult, error) {
	var p rpc.GammaZeroSPXParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}

	// Scope: which underlying(s) to compute. Empty defaults to combined
	// promoted over only if the forced run succeeds.
	scope, scopeErr := gammaScopeForRequest(p.Scope)
	if scopeErr != nil {
		return nil, fmt.Errorf("zero-gamma: %w", scopeErr)
	}

	if !p.Force {
		now := time.Now()
		if env, ok := s.zeroGamma.snapshotCombinedSlice(scope, func() time.Time { return now }); ok {
			own := s.zeroGamma.snapshotCurrent(scope, func() time.Time { return now })
			if preferOwnGammaSnapshot(env, own) {
				return &own, nil
			}
			return &env, nil
		}
	}

	// Background ctx for the compute goroutine — independent of the
	// per-RPC ctx because the compute outlives any single client call.
	// serverCtx is set on Start and matches the daemon's lifetime, so
	// daemon shutdown cancels the compute cleanly.
	s.mu.Lock()
	parent := s.serverCtx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}

	// Build the compute closure. The cache layer owns goroutine
	// lifecycle; we hand it a function that closes over the gateway
	// connector + params.
	//
	// The closure acquires a refcounted Hold on the underlying for
	// the entire lifetime of the compute. IBKR's TWS API requires a
	// market-data subscription on the option's underlying to push
	// OPTION_COMPUTATION (msg 21) ticks for OPT subscriptions; without
	// it the model engine has no live spot anchor and the per-leg fan-
	// out lands ~0% IV/greeks (observed: 12/1256 legs at 1% coverage
	// pre-market). subManager.Hold is refcounted, so a concurrent
	// regime snapshot on the same symbol is safe — the line stays
	// open until the compute releases.
	//
	// Per-scope compute selection:
	//   combined  → SPY phase then SPX phase, with separate Holds
	//               (computeGammaCombined enforces the underlying-hold
	//               transition audit checklist item from design §7.1).
	//   spy / spx → single-underlying phase under one Hold.
	params := normalizeGammaParams(rpc.GammaZeroParams{})
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		switch scope {
		case rpc.GammaZeroScopeCombined:
			return computeGammaCombined(bgCtx, s, c, params, prog)
		case rpc.GammaZeroScopeSPX:
			return runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 0)
		default: // GammaZeroScopeSPY
			return runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
		}
	}

	var job *gammaComputation
	if p.Force {
		job = s.zeroGamma.force(parent, scope, time.Now(), computeETA, compute)
	} else {
		job, _ = s.zeroGamma.kickOrJoin(parent, scope, time.Now(), computeETA, compute)
	}

	// kickOrJoin returns (nil, false) when the session is closed and no
	// persisted result is available — the off-hours "never compute"
	// contract from gamma_zero_cache.go. There's no job to wait on; go
	// straight to snapshot, which will report Cold.
	if job != nil && p.WaitMs > 0 {
		// Cap the wait at the RPC deadline so we always return before
		// the dispatcher times us out. The per-method deadline for
		// GammaZeroSPX is intentionally long enough to make WaitMs
		// usable but shorter than the bg compute itself, so a high
		// WaitMs still returns "computing" if the compute hasn't
		// finished.
		waitCtx, waitCancel := context.WithTimeout(ctx, time.Duration(p.WaitMs)*time.Millisecond)
		defer waitCancel()
		select {
		case <-job.done:
			// compute finished — fall through to snapshot
		case <-waitCtx.Done():
			// either WaitMs elapsed or the RPC deadline fired —
		}
	}

	env := s.zeroGamma.snapshotForScope(scope, job, time.Now)
	return &env, nil
}

func preferOwnGammaSnapshot(canonical, own rpc.GammaZeroSPXResult) bool {
	if own.Status != rpc.GammaZeroStatusReady || own.Result == nil || canonical.Result == nil {
		return false
	}
	if own.Result.AsOf.After(canonical.Result.AsOf) {
		return true
	}
	if own.Result.AsOf.Before(canonical.Result.AsOf) {
		return false
	}
	return gammaSnapshotHasSPXCacheFallback(canonical.Result) && !gammaSnapshotHasSPXCacheFallback(own.Result)
}

func gammaSnapshotHasSPXCacheFallback(result *rpc.GammaZeroComputed) bool {
	if result == nil {
		return false
	}
	for _, code := range result.Warnings {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(code)), "spx_cache_fallback") {
			return true
		}
	}
	for _, detail := range result.WarningDetails {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(detail.Code)), "spx_cache_fallback") {
			return true
		}
	}
	return false
}

func buildGammaLegDiagnostics(underlying string, legs []legData, spot float64) *rpc.GammaLegDiagnostics {
	out := &rpc.GammaLegDiagnostics{
		ByUnderlying:   make(map[string]rpc.GammaLegDiagnosticCounts),
		ByTradingClass: make(map[string]rpc.GammaLegDiagnosticCounts),
	}
	underlying = gammaDiagnosticKey(underlying, "UNKNOWN")
	for _, leg := range legs {
		gamma := bsGamma(spot, leg.strike, leg.dte, leg.iv, 0, 0)
		abs := absGEX(gamma, float64(leg.oi), 100, spot)
		counts := gammaLegDiagnosticCounts(leg, gamma, abs)

		out.Total = addGammaLegDiagnosticCounts(out.Total, counts)
		out.ByUnderlying[underlying] = addGammaLegDiagnosticCounts(out.ByUnderlying[underlying], counts)
		classKey := gammaDiagnosticKey(leg.tradingClass, underlying)
		out.ByTradingClass[classKey] = addGammaLegDiagnosticCounts(out.ByTradingClass[classKey], counts)
	}
	if len(out.ByUnderlying) == 0 {
		out.ByUnderlying = nil
	}
	if len(out.ByTradingClass) == 0 {
		out.ByTradingClass = nil
	}
	return out
}

func gammaLegDiagnosticCounts(leg legData, gamma, absGEX float64) rpc.GammaLegDiagnosticCounts {
	counts := rpc.GammaLegDiagnosticCounts{PricedLegs: 1}
	switch leg.ivSource {
	case gammaIVSourceLiveMid:
		counts.DerivedLiveMidLegs = 1
	case gammaIVSourcePrevClose:
		counts.DerivedPrevCloseLegs = 1
	default:
		counts.ModelTickLegs = 1
	}
	if leg.oiObserved {
		counts.OpenInterestObservedLegs = 1
	}
	if leg.oiLive {
		counts.OILiveObservedLegs = 1
	}
	if leg.oiCarried {
		counts.OICarriedForwardLegs = 1
	}
	if leg.oi > 0 {
		counts.OpenInterestLegs = 1
	}
	if gamma > 0 {
		counts.GammaPositiveLegs = 1
	}
	if absGEX > 0 {
		counts.AbsGEXLegs = 1
	}
	return counts
}

func combineGammaLegDiagnostics(inputs ...*rpc.GammaLegDiagnostics) *rpc.GammaLegDiagnostics {
	out := &rpc.GammaLegDiagnostics{}
	for _, in := range inputs {
		if in == nil {
			continue
		}
		out.Total = addGammaLegDiagnosticCounts(out.Total, in.Total)
		out.ByUnderlying = mergeGammaLegDiagnosticMap(out.ByUnderlying, in.ByUnderlying)
		out.ByTradingClass = mergeGammaLegDiagnosticMap(out.ByTradingClass, in.ByTradingClass)
	}
	if out.Total == (rpc.GammaLegDiagnosticCounts{}) && len(out.ByUnderlying) == 0 && len(out.ByTradingClass) == 0 {
		return nil
	}
	return out
}

func mergeGammaLegDiagnosticMap(dst, src map[string]rpc.GammaLegDiagnosticCounts) map[string]rpc.GammaLegDiagnosticCounts {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]rpc.GammaLegDiagnosticCounts, len(src))
	}
	for key, counts := range src {
		dst[key] = addGammaLegDiagnosticCounts(dst[key], counts)
	}
	return dst
}

func addGammaLegDiagnosticCounts(a, b rpc.GammaLegDiagnosticCounts) rpc.GammaLegDiagnosticCounts {
	return rpc.GammaLegDiagnosticCounts{
		PricedLegs:               a.PricedLegs + b.PricedLegs,
		ModelTickLegs:            a.ModelTickLegs + b.ModelTickLegs,
		DerivedLiveMidLegs:       a.DerivedLiveMidLegs + b.DerivedLiveMidLegs,
		DerivedPrevCloseLegs:     a.DerivedPrevCloseLegs + b.DerivedPrevCloseLegs,
		OpenInterestObservedLegs: a.OpenInterestObservedLegs + b.OpenInterestObservedLegs,
		OILiveObservedLegs:       a.OILiveObservedLegs + b.OILiveObservedLegs,
		OICarriedForwardLegs:     a.OICarriedForwardLegs + b.OICarriedForwardLegs,
		OpenInterestLegs:         a.OpenInterestLegs + b.OpenInterestLegs,
		GammaPositiveLegs:        a.GammaPositiveLegs + b.GammaPositiveLegs,
		AbsGEXLegs:               a.AbsGEXLegs + b.AbsGEXLegs,
	}
}

func formatGammaLegDiagnostics(d *rpc.GammaLegDiagnostics) string {
	if d == nil {
		return "diagnostics unavailable"
	}
	parts := []string{"total " + formatGammaLegDiagnosticCounts(d.Total)}
	if len(d.ByUnderlying) > 0 {
		parts = append(parts, "by_underlying "+formatGammaLegDiagnosticMap(d.ByUnderlying))
	}
	if len(d.ByTradingClass) > 0 {
		parts = append(parts, "by_trading_class "+formatGammaLegDiagnosticMap(d.ByTradingClass))
	}
	return strings.Join(parts, "; ")
}

func formatGammaLegDiagnosticMap(m map[string]rpc.GammaLegDiagnosticCounts) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %s", key, formatGammaLegDiagnosticCounts(m[key])))
	}
	return strings.Join(parts, ", ")
}

func formatGammaLegDiagnosticCounts(c rpc.GammaLegDiagnosticCounts) string {
	return fmt.Sprintf("priced=%d model_tick_iv=%d derived_mid_iv=%d derived_close_iv=%d oi_seen=%d oi>0=%d gamma>0=%d abs_gex>0=%d oi_live=%d oi_carried=%d",
		c.PricedLegs, c.ModelTickLegs, c.DerivedLiveMidLegs, c.DerivedPrevCloseLegs,
		c.OpenInterestObservedLegs, c.OpenInterestLegs, c.GammaPositiveLegs, c.AbsGEXLegs,
		c.OILiveObservedLegs, c.OICarriedForwardLegs)
}

func gammaOIMissingCount(d *rpc.GammaLegDiagnostics) int {
	if d == nil {
		return 0
	}
	observed := max(d.Total.OpenInterestObservedLegs, d.Total.OpenInterestLegs)
	missing := d.Total.PricedLegs - observed
	if missing < 0 {
		return 0
	}
	return missing
}

func gammaDiagnosticKey(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = "UNKNOWN"
	}
	return strings.ToUpper(value)
}

// optionSessionOpen reports whether the official U.S. listed-options session
// is open at now. marketcal is the authority — holidays, early closes, and the
// 16:15 close included — and only outside its embedded coverage does the
// clock-only weekday fallback apply. Policy blockers and eligibility gates
// must use this, not rpc.IsOptionRTH: that helper is holiday-blind by
// documented contract and is kept for display cadence only.
func optionSessionOpen(now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, now)
	if err == nil && session.State != marketcal.StateUnknown {
		return session.IsOpen
	}
	return gammaWeekdayOptionsRegular(now)
}

// gammaClassifySession classifies the option-data surface used by dealer
// gamma, not the underlying ETF quote session. The compute needs option OI,
// IV/model ticks, and classed SPX/SPXW contracts; outside the official regular
// U.S. listed-options session a non-force refresh is not expected to improve a
// good last-known snapshot reliably.
func gammaClassifySession(now time.Time) rpc.SessionClass {
	if optionSessionOpen(now) {
		return rpc.SessionRTH
	}
	return rpc.SessionClosed
}

func gammaWeekdayOptionsRegular(now time.Time) bool {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 15, 0, 0, ny)
	return !t.Before(open) && t.Before(closeT)
}

// gammaOperationalCadence separates process continuity from trading
// rankability. A prior-session result can be the expected last completed
// compute before the next options session while remaining context-only for
// regime confirmation.
func gammaOperationalCadence(env *rpc.GammaZeroSPXResult, now time.Time) string {
	if env == nil || env.Result == nil || env.Result.AsOf.IsZero() {
		return rpc.DataCadenceNoLastGood
	}
	completedDate, current, ok := lastCompletedOptionsSession(now)
	if !ok {
		return rpc.DataCadenceUnknown
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return rpc.DataCadenceUnknown
	}
	resultDate := env.Result.AsOf.In(ny).Format("2006-01-02")
	switch {
	case resultDate < completedDate:
		return rpc.DataCadenceMissedSession
	case resultDate > completedDate:
		return rpc.DataCadenceCurrent
	case current.IsOpen:
		return rpc.DataCadenceMissedSession
	default:
		return rpc.DataCadenceNotDue
	}
}

// gammaPublicationWindow bounds how long after the options open a
// last-completed-session gamma result may still be the newest that exists.
// A current-session compute takes about nine minutes; 30 minutes covers a slow
// open (contention, cold cache, pacing) including one retry, and the served
// result cannot confirm anything for the whole window, so the only cost of the
// bound is how long a hung or never-kicked compute stays invisible.
// Operator decision, 2026-07-31.
const gammaPublicationWindow = 30 * time.Minute

// gammaPublicationPending reports whether the served result is the immediately
// prior completed options session while a current-session compute is in flight
// inside the bounded window that opens with the session. It mirrors
// spx.PublicationPending: the typed in-flight marker is required, so a compute
// that never started — or one that hung past the deadline — is overdue rather
// than pending.
func gammaPublicationPending(env *rpc.GammaZeroSPXResult, now time.Time) bool {
	if env == nil || env.Result == nil || env.Result.AsOf.IsZero() || !env.Refreshing {
		return false
	}
	completedDate, current, ok := lastCompletedOptionsSession(now)
	if !ok || !current.IsOpen || current.Open.IsZero() {
		return false
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return false
	}
	if env.Result.AsOf.In(ny).Format("2006-01-02") != completedDate {
		return false
	}
	return !now.Before(current.Open) && now.Before(current.Open.Add(gammaPublicationWindow))
}

func lastCompletedOptionsSession(now time.Time) (string, marketcal.Session, bool) {
	return lastCompletedMarketSession(now, marketcal.MarketUSOptions)
}

func lastCompletedOptionsSessionWindow(now time.Time) (marketcal.Session, marketcal.Session, bool) {
	return lastCompletedMarketSessionWindow(now, marketcal.MarketUSOptions)
}

// gammaClosedSessionCacheStale is the single authority on whether a cached
// gamma compute served during a closed options session is genuinely stale: it
// predates the last completed session's open, so a newer session's evidence
// should exist. A weekend or holiday gap is a schedule, not staleness. While
// the calendar cannot vouch for the window, the 24h wall clock is the
// fallback bound.
func gammaClosedSessionCacheStale(asOf, now time.Time) bool {
	completed, _, ok := lastCompletedOptionsSessionWindow(now)
	return gammaClosedSessionCacheStaleAt(asOf, now, completed, ok && !completed.Open.IsZero())
}

func gammaClosedSessionCacheStaleAt(asOf, now time.Time, completed marketcal.Session, calendarVouched bool) bool {
	if !calendarVouched {
		return now.Sub(asOf) > gammaClosedSessionCacheMaxAge
	}
	return asOf.Before(completed.Open)
}

func lastCompletedMarketSession(now time.Time, market marketcal.Market) (string, marketcal.Session, bool) {
	completed, current, ok := lastCompletedMarketSessionWindow(now, market)
	return completed.Date, current, ok
}

// lastCompletedMarketSessionWindow is lastCompletedMarketSession with the
// completed session's own open/close window, for callers that must compare
// against the session boundary rather than the calendar date.
func lastCompletedMarketSessionWindow(now time.Time, market marketcal.Market) (marketcal.Session, marketcal.Session, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	current, err := cal.SessionAt(market, now)
	if err != nil || current.State == marketcal.StateUnknown {
		return marketcal.Session{}, current, false
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return marketcal.Session{}, current, false
	}
	local := now.In(ny)
	for offset := range 10 {
		day := local.AddDate(0, 0, -offset)
		at := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, ny)
		session, sessionErr := cal.SessionAt(market, at)
		if sessionErr != nil || session.State == marketcal.StateUnknown {
			return marketcal.Session{}, current, false
		}
		if (session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose) &&
			!session.Close.IsZero() && !session.Close.After(now) {
			return session, current, true
		}
	}
	return marketcal.Session{}, current, false
}

// SkewCurve is a quadratic fit of implied volatility against
// Why bother: the legacy sticky-IV recipe biases zero-gamma upward
type SkewCurve struct {
	A, B, C  float64
	nPoints  int
	ok       bool
	mLo, mHi float64
}

// IVAtMoneyness evaluates the curve at moneyness m = ln(K / S). Clamps
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
// given strike call IV and put IV must match (put-call parity), so
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
		r2 = 0
	}
	return r2, residualRMS
}

// gammaSkewDiagJournal retains its path codec only for legacy format tests.
// Once daemon authority is attached, beta calibration observations are no
// longer appended because no production reader consumes them.
//
// Lifecycle mirrors gammaZeroStore.Save: appended only on the
// successful, non-cancelled persist path in spawnJob; runtime attachment makes
// append a no-op and can never fail a compute.
type gammaSkewDiagJournal struct {
	// path is retained solely for explicit legacy-import and isolated
	// file-format tests. Production attaches the daemon authority before
	// the cache can run; once attached, append never reads or writes path.
	path      string
	authority *corestore.Store
}

// UseCoreStore switches the journal to daemon.db. There is deliberately no
// file fallback after attachment: a database error must remain visible to the
// daemon's authority-health latch instead of silently splitting history.
func (j *gammaSkewDiagJournal) UseCoreStore(store *corestore.Store) error {
	if j == nil {
		return errors.New("gamma skew diagnostics: nil journal")
	}
	if store == nil {
		return errors.New("gamma skew diagnostics: nil corestore")
	}
	j.authority = store
	return nil
}

// gammaSkewDiagDefaultPath resolves the journal's on-disk location in
// the same private state dir as the order journal and proposal
// outcomes ($XDG_STATE_HOME/ibkr/, default ~/.local/state/ibkr/).
func gammaSkewDiagDefaultPath() (string, error) {
	return defaultTradingStatePath("gamma-skew-diagnostics.jsonl")
}

// gammaSkewDiagLine is the v1 journal record. One line per slice: the
// combined node plus each per-index sub, so SPX and SPY fit
// distributions can be analysed separately. Rankability fields are
// computed on an annotated clone at append time — the served result is
// annotated lazily at serve time and must not be mutated here.
type gammaSkewDiagLine struct {
	V             int                        `json:"v"`
	TS            time.Time                  `json:"ts"`
	SessionKey    string                     `json:"session_key"`
	Session       string                     `json:"session"`
	Scope         string                     `json:"scope"`
	Slice         string                     `json:"slice"`
	AsOf          time.Time                  `json:"as_of"`
	MedianR2      float64                    `json:"median_r2"`
	MinR2         float64                    `json:"min_r2"`
	FitExpiries   int                        `json:"fit_expiries"`
	Expiries      map[string]rpc.SkewFitInfo `json:"expiries,omitempty"`
	PricedLegs    int                        `json:"priced_legs"`
	GEXLegs       int                        `json:"gex_legs"`
	OIObservedPct float64                    `json:"oi_observed_pct"`
	DerivedIVPct  float64                    `json:"derived_iv_pct"`
	Rankability   string                     `json:"rankability"`
	Reason        string                     `json:"reason,omitempty"`
	GammaSign     string                     `json:"gamma_sign,omitempty"`
	ZeroGamma     *float64                   `json:"zero_gamma,omitempty"`
	Warnings      []string                   `json:"warnings,omitempty"`
}

// append journals the slices of one successful compute. The whole
// batch is marshalled into a single buffer and issued as one Write on
// an O_APPEND descriptor so concurrent scope jobs cannot interleave
// partial lines.
func (j *gammaSkewDiagJournal) append(now time.Time, scope, sessionKey string, result *rpc.GammaZeroComputed) error {
	if j == nil || result == nil {
		return nil
	}
	// Quality is annotated lazily on serve-time clones; annotate a
	// clone here too. Annotating the raw combined result would find
	// nil sub-slice Quality and journal every combined line as
	// "blocked: SPX quality missing", silently poisoning the
	// calibration set.
	clone := cloneGammaComputed(result)
	annotateGammaQuality(clone, now)
	lines := gammaSkewDiagLines(now, scope, sessionKey, clone)
	if len(lines) == 0 {
		return nil
	}
	if j.authority != nil {
		return nil
	}
	return j.appendLegacy(lines)
}

const (
	gammaSkewDiagVersion         = 1
	gammaSkewDiagObservationKind = "gamma_skew_diagnostic.v1"
)

// appendLegacy is retained only for isolated codec tests. Runtime attaches
// corestore before diagnostics can be written.
func (j *gammaSkewDiagJournal) appendLegacy(lines []gammaSkewDiagLine) error {
	var buf []byte
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("encode skew diagnostics: %w", err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := ensurePrivateStateDir(j.path); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", j.path, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", j.path, err)
	}
	return f.Close()
}

func gammaSkewDiagLines(now time.Time, scope, sessionKey string, c *rpc.GammaZeroComputed) []gammaSkewDiagLine {
	if c == nil {
		return nil
	}
	lines := []gammaSkewDiagLine{gammaSkewDiagLineFor(now, scope, sessionKey, gammaQualityScope(c), c)}
	for _, key := range []string{"SPX", "SPY"} {
		if sub := c.PerIndex[key]; sub != nil {
			lines = append(lines, gammaSkewDiagLineFor(now, scope, sessionKey, key, sub))
		}
	}
	return lines
}

func gammaSkewDiagLineFor(now time.Time, scope, sessionKey, slice string, c *rpc.GammaZeroComputed) gammaSkewDiagLine {
	line := gammaSkewDiagLine{
		V:          gammaSkewDiagVersion,
		TS:         now,
		SessionKey: sessionKey,
		Scope:      scope,
		Slice:      slice,
		AsOf:       c.AsOf,
		Expiries:   c.SkewFitQuality,
		GammaSign:  c.GammaSign,
		ZeroGamma:  c.ZeroGamma,
		Warnings:   c.Warnings,
	}
	if q := c.Quality; q != nil {
		line.Session = q.Session
		line.MedianR2 = q.Coverage.MedianSkewRSquared
		line.MinR2 = q.Coverage.MinSkewRSquared
		line.FitExpiries = q.Coverage.SkewFitExpiries
		line.PricedLegs = q.Coverage.PricedLegs
		line.GEXLegs = q.Coverage.GEXLegs
		line.OIObservedPct = q.Coverage.OIObservedPct
		line.DerivedIVPct = q.Coverage.DerivedIVPct
		line.Rankability = q.Rankability
		line.Reason = q.RankabilityReason
	}
	return line
}

const gammaNotAdvice = "Market-structure context only; not a trade recommendation."

func hydrateGammaComputed(c *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	// Both cached payloads and child results are data-boundary input. Reduce
	// every warning surface to typed codes and rebuild the structured prose;
	// never preserve producer-supplied detail text across hydration.
	legacyCodes := make([]string, 0, len(c.WarningDetails))
	for _, detail := range c.WarningDetails {
		legacyCodes = append(legacyCodes, detail.Code)
	}
	c.Warnings = canonicalGammaWarningUnion(c.Warnings, legacyCodes)
	c.WarningDetails = nil
	for _, sub := range c.PerIndex {
		hydrateGammaComputed(sub)
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		c.WarningDetails = buildCombinedGammaWarningDetails(c)
	} else {
		c.WarningDetails = buildGammaWarningDetails(c)
	}
	c.Summary = buildGammaSummary(c)
	return c
}

// buildCombinedGammaWarningDetails projects the per-index warning surfaces to
// the combined envelope without pretending a SPY or SPX condition applies to
// both indices. The (scope, code) pair is the identity: duplicate copies from
// repeated hydration collapse, while the same code on SPY and SPX remains two
// distinct diagnostics.
func buildCombinedGammaWarningDetails(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}

	type warningKey struct {
		scope string
		code  string
	}
	seen := make(map[warningKey]struct{})
	childCodes := make(map[string]struct{})
	out := make([]rpc.GammaWarningDetail, 0, len(c.WarningDetails)+len(c.Warnings))
	appendDetail := func(d rpc.GammaWarningDetail) {
		if d.Code == "" {
			return
		}
		key := warningKey{scope: d.Scope, code: d.Code}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}

	for _, label := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[label]
		if sub == nil {
			continue
		}
		for _, code := range sub.Warnings {
			if code != "" {
				childCodes[code] = struct{}{}
			}
		}
		for _, d := range sub.WarningDetails {
			if d.Code != "" {
				childCodes[d.Code] = struct{}{}
			}
			appendDetail(d)
		}
	}

	// Preserve any combined-local structured warning that was attached by a
	// cache/fallback path. Child-derived copies lose to the freshly hydrated
	// child details above, which keeps repeated hydration deterministic.
	for _, d := range c.WarningDetails {
		appendDetail(d)
	}
	// Warnings is deliberately the code-only union for daemon-internal
	// compatibility. Only codes absent from both children are combined-local;
	// child codes already have their correctly scoped details above.
	for _, code := range c.Warnings {
		if code == "" {
			continue
		}
		if _, inherited := childCodes[code]; inherited {
			continue
		}
		appendDetail(gammaWarningDetail(c, code))
	}
	return out
}

func buildGammaSummary(c *rpc.GammaZeroComputed) *rpc.GammaZeroSummary {
	if c == nil {
		return nil
	}
	out := &rpc.GammaZeroSummary{
		NotAdvice:  gammaNotAdvice,
		Confidence: gammaResultConfidence(c),
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		out.PerIndex = make(map[string]rpc.GammaIndexSummary, len(c.PerIndex))
		var parts []string
		statuses := map[string]int{}
		regimes := map[string]int{}
		confidences := map[string]int{}
		for _, key := range []string{"SPX", "SPY"} {
			sub := c.PerIndex[key]
			if sub == nil {
				continue
			}
			item := buildGammaIndexSummary(sub, key)
			out.PerIndex[key] = item
			statuses[item.ZeroGammaStatus]++
			regimes[item.Regime]++
			confidences[item.Confidence]++
			parts = append(parts, gammaCombinedIndexStatement(key, item))
		}
		out.ZeroGammaStatus = combineSummaryStatus(statuses)
		out.Regime = combineSummaryRegime(c.RegimeAgreement, regimes)
		out.Confidence = combineSummaryConfidence(confidences)
		if c.Quality != nil && c.Quality.Rankability == rpc.GammaRankabilityRankable {
			out.Confidence = gammaResultConfidence(c)
		}
		if len(parts) == 0 {
			out.PrimaryStatement = "Zero-gamma: unavailable for SPY+SPX."
		} else {
			out.PrimaryStatement = "Zero-gamma: " + strings.Join(parts, "; ") + ". Price levels remain per-index because SPY and SPX use different price scales."
		}
		return out
	}
	item := buildGammaIndexSummary(c, gammaUnderlyingLabel(c))
	out.ZeroGammaStatus = item.ZeroGammaStatus
	out.Regime = item.Regime
	out.PrimaryStatement = "Zero-gamma: " + gammaIndexStatement(item) + "."
	return out
}

func buildGammaIndexSummary(c *rpc.GammaZeroComputed, label string) rpc.GammaIndexSummary {
	if label == "" {
		label = gammaUnderlyingLabel(c)
	}
	status, regime := gammaZeroStatusAndRegime(c)
	return rpc.GammaIndexSummary{
		Underlying:      label,
		SpotUnderlying:  c.SpotUnderlying,
		DataType:        c.DataType,
		ZeroGamma:       c.ZeroGamma,
		ZeroGammaStatus: status,
		Regime:          regime,
		SweepLowAbs:     c.SweepLowAbs,
		SweepHighAbs:    c.SweepHighAbs,
		LegCount:        c.LegCount,
		PricedLegCount:  c.PricedLegCount,
		GammaTotalAbs:   c.GammaTotalAbs,
		Confidence:      gammaResultConfidence(c),
		Interpretation:  gammaInterpretation(c, status, regime),
	}
}

func gammaCombinedIndexStatement(key string, s rpc.GammaIndexSummary) string {
	switch key {
	case "SPX":
		s.Underlying = "SPX canonical"
	case "SPY":
		s.Underlying = "SPY context"
	}
	return gammaIndexStatement(s)
}

func gammaIndexStatement(s rpc.GammaIndexSummary) string {
	label := s.Underlying
	if label == "" {
		label = "underlying"
	}
	switch s.ZeroGammaStatus {
	case "crossing":
		if s.ZeroGamma != nil {
			regime := strings.ReplaceAll(s.Regime, "_", "-")
			if regime != "" {
				return fmt.Sprintf("%s zero-gamma %s (%s)", label, formatGammaSummaryPrice(*s.ZeroGamma), regime)
			}
			return fmt.Sprintf("%s zero-gamma %s", label, formatGammaSummaryPrice(*s.ZeroGamma))
		}
	case "none_in_window":
		rangeText := gammaSummaryRange(s.SweepLowAbs, s.SweepHighAbs)
		regime := strings.ReplaceAll(s.Regime, "_", "-")
		if rangeText != "" && regime != "" {
			return fmt.Sprintf("%s stayed %s across %s", label, regime, rangeText)
		}
		if regime != "" {
			return fmt.Sprintf("%s stayed %s across the swept range", label, regime)
		}
		return fmt.Sprintf("%s had no crossing in the swept range", label)
	case "unavailable":
		if s.Interpretation != "" {
			return fmt.Sprintf("%s unavailable (%s)", label, s.Interpretation)
		}
		return fmt.Sprintf("%s unavailable", label)
	}
	return fmt.Sprintf("%s indeterminate", label)
}

func gammaZeroStatusAndRegime(c *rpc.GammaZeroComputed) (string, string) {
	if c == nil {
		return "unavailable", "unavailable"
	}
	if c.ZeroGamma != nil {
		return "crossing", rpc.GammaRegimeFromGap(c.GapPct)
	}
	if c.LegCount > 0 && c.GammaTotalAbs == 0 && gammaProfileAllZero(c.Profile) {
		return "unavailable", "unavailable"
	}
	switch c.GammaSign {
	case "positive":
		return "none_in_window", "long_gamma"
	case "negative":
		return "none_in_window", "short_gamma"
	case "no_data":
		return "unavailable", "unavailable"
	default:
		return "unavailable", "unavailable"
	}
}

func gammaInterpretation(c *rpc.GammaZeroComputed, status, regime string) string {
	switch status {
	case "crossing":
		if c.ZeroGamma != nil {
			if c.GapPct != nil {
				return fmt.Sprintf("signed gamma profile crosses zero at %s; spot is %+.1f%% from that level",
					formatGammaSummaryPrice(*c.ZeroGamma), *c.GapPct)
			}
			return "signed gamma profile crosses zero at " + formatGammaSummaryPrice(*c.ZeroGamma)
		}
	case "none_in_window":
		switch regime {
		case "long_gamma":
			return "no crossing; model stayed long-gamma across the swept range"
		case "short_gamma":
			return "no crossing; model stayed short-gamma across the swept range"
		}
	case "unavailable":
		if c != nil && c.LegCount > 0 && c.GammaTotalAbs == 0 {
			return "no usable gamma magnitude from landed legs"
		}
		return "no usable signed gamma profile"
	}
	return ""
}

func gammaResultConfidence(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "unavailable"
	}
	if c.Quality != nil {
		switch c.Quality.Rankability {
		case rpc.GammaRankabilityRankable:
			return "estimate"
		case rpc.GammaRankabilityContextOnly, rpc.GammaRankabilityBlocked:
			return "degraded"
		case rpc.GammaRankabilityUnavailable:
			return "unavailable"
		}
	}
	if c.LegCount > 0 && c.GammaTotalAbs == 0 && gammaProfileAllZero(c.Profile) {
		return "unavailable"
	}
	for _, w := range gammaWarningCodes(c) {
		switch {
		case w == "throttled", w == "all_iv_derived", w == "cache_stale_off_hours", w == "oi_missing":
			return "degraded"
		case strings.HasPrefix(w, "spy_unavailable:"):
			return "degraded"
		case strings.HasPrefix(w, "spx_unavailable:"):
			return "degraded"
		case strings.HasPrefix(w, "skew_fallback:"):
			return "degraded"
		}
	}
	return "estimate"
}

// gammaWarningCodes returns warnings that apply to this quality node. A
// combined envelope also projects its children's warnings for wire consumers;
// those inherited details are excluded here because each child already gates
// its own quality and SPY context must not re-gate the SPX-canonical result.
func gammaWarningCodes(c *rpc.GammaZeroComputed) []string {
	if c == nil {
		return nil
	}
	type warningKey struct {
		scope string
		code  string
	}
	childCodes := map[string]struct{}{}
	childDetails := map[warningKey]struct{}{}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		for _, sub := range c.PerIndex {
			if sub == nil {
				continue
			}
			for _, code := range sub.Warnings {
				if code != "" {
					childCodes[code] = struct{}{}
				}
			}
			for _, d := range sub.WarningDetails {
				if d.Code == "" {
					continue
				}
				childCodes[d.Code] = struct{}{}
				childDetails[warningKey{scope: d.Scope, code: d.Code}] = struct{}{}
			}
		}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, code := range c.Warnings {
		if code == "" {
			continue
		}
		if _, inherited := childCodes[code]; inherited {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	for _, d := range c.WarningDetails {
		if d.Code == "" {
			continue
		}
		if _, inherited := childDetails[warningKey{scope: d.Scope, code: d.Code}]; inherited {
			continue
		}
		if _, ok := seen[d.Code]; ok {
			continue
		}
		seen[d.Code] = struct{}{}
		out = append(out, d.Code)
	}
	return out
}

func combineSummaryStatus(counts map[string]int) string {
	if len(counts) == 0 {
		return "unavailable"
	}
	if len(counts) == 1 {
		for k := range counts {
			return k
		}
	}
	if counts["unavailable"] > 0 {
		return "mixed_degraded"
	}
	return "mixed"
}

func combineSummaryRegime(agreement string, regimes map[string]int) string {
	switch agreement {
	case "agree:long-gamma":
		return "long_gamma"
	case "agree:short-gamma":
		return "short_gamma"
	case "agree:transition-gamma":
		return "transition_gamma"
	case "disagree":
		return "mixed"
	}
	if len(regimes) == 1 {
		for k := range regimes {
			return k
		}
	}
	if len(regimes) == 0 {
		return "unavailable"
	}
	return "mixed"
}

func combineSummaryConfidence(counts map[string]int) string {
	switch {
	case len(counts) == 0:
		return "unavailable"
	case counts["unavailable"] > 0:
		return "unavailable"
	case counts["degraded"] > 0:
		return "degraded"
	default:
		return "estimate"
	}
}

func buildGammaWarningDetails(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}
	codes := gammaWarningCodes(c)
	if len(codes) == 0 {
		return c.WarningDetails
	}
	out := make([]rpc.GammaWarningDetail, 0, len(codes))
	for _, code := range codes {
		out = append(out, gammaWarningDetail(c, code))
	}
	return out
}

func gammaWarningDetail(c *rpc.GammaZeroComputed, code string) rpc.GammaWarningDetail {
	scope := gammaWarningScope(c, code)
	d := rpc.GammaWarningDetail{
		Code:     code,
		Scope:    scope,
		Severity: "info",
	}
	switch {
	case code == "no_crossing_in_window":
		d.Message = "No signed gamma-zero crossing was found in the swept range."
		d.Impact = "Use the regime label and swept range instead of a zero-gamma level."
	case code == "0dte_no_legs":
		d.Message = "No same-day expiry legs were included."
		d.Impact = "The 0DTE horizon is unavailable for this run."
	case code == "1to7_no_legs":
		d.Message = "No 1-7 DTE legs were included."
		d.Impact = "The weekly horizon is unavailable for this run."
	case code == "term_no_legs":
		d.Message = "No >7 DTE legs were included."
		d.Impact = "The term horizon is unavailable for this run."
	case code == "throttled":
		d.Severity = "data_quality"
		d.Message = "The gateway throttled part of the option fan-out."
		d.Impact = "Coverage may be incomplete; treat this slice as lower confidence."
		d.Action = "Retry later or during regular trading hours; avoid repeated forced runs."
	case code == "oi_missing":
		session := gammaWarningSession(c)
		if gammaOIMissingUnexpected(d.Scope, session) {
			d.Severity = "data_quality"
		}
		missing := gammaOIMissingCount(c.LegDiagnostics)
		if missing == 0 {
			missing = max(c.PricedLegCount-c.LegCount, 0)
		}
		d.Message = fmt.Sprintf("Open-interest ticks were missing for %d priced legs.", missing)
		d.Impact = fmt.Sprintf("%d priced legs contributed to IV/skew fitting; %d legs had observed OI and %d had positive OI for dealer GEX. Missing OI is unknown, not zero.", c.PricedLegCount, gammaOIObservedCount(c), c.LegCount)
		d.Action = gammaOIMissingAction(d.Scope, session)
	case code == "all_iv_derived":
		d.Severity = "data_quality"
		d.Message = "No gateway model IV ticks landed; all implied volatilities were back-solved."
		d.Impact = gammaIVSourceImpact(c)
		d.Action = "Treat gamma as non-voting until IBKR model-computation ticks resume; inspect gateway/farm notices and active market-data subscription pressure."
	case code == "strike_budget_capped":
		d.Severity = "methodology"
		d.Message = "The strike fan-out was capped to the nearest 80 listed strikes per expiry."
		d.Impact = "Farther out-of-money strikes inside the ±10% candidate window were skipped to keep the gateway request budget bounded."
	case code == "cache_stale_off_hours":
		d.Severity = "data_quality"
		d.Message = "The cached gamma result is older than 24 hours and markets are closed."
		d.Impact = "The daemon served the last persisted snapshot rather than recomputing against a closed market."
	case code == "unclassified_data_warning":
		d.Severity = "data_quality"
		d.Message = "An unclassified gamma data warning was received."
		d.Impact = "The affected gamma slice is not trusted for ranking until a typed warning is available."
		d.Action = "Inspect local daemon diagnostics and retry after the data source recovers."
	case strings.HasPrefix(code, "refresh_failed:"):
		d.Severity = "data_quality"
		summary := strings.TrimPrefix(code, "refresh_failed:")
		summary = strings.ReplaceAll(summary, "_", " ")
		d.Message = "The latest gamma refresh failed."
		d.Impact = "The daemon is serving an older cached gamma snapshot; do not treat it as a fresh market-structure read."
		if summary != "" {
			d.Action = "Inspect gateway/farm state and retry after resolving: " + summary + "."
		} else {
			d.Action = "Inspect gateway/farm state and retry after resolving the refresh failure."
		}
	case strings.HasPrefix(code, "spy_unavailable:"):
		d.Severity = "data_quality"
		d.Message, d.Impact, d.Action = spyUnavailableWarningText(strings.TrimPrefix(code, "spy_unavailable:"))
	case strings.HasPrefix(code, "spx_unavailable:"):
		d.Severity = "data_quality"
		d.Message, d.Impact, d.Action = spxUnavailableWarningText(strings.TrimPrefix(code, "spx_unavailable:"))
	case strings.HasPrefix(code, "spx_cache_fallback"):
		d.Severity = "info"
		d.Message, d.Impact, d.Action = spxCacheFallbackWarningText(strings.TrimPrefix(code, "spx_cache_fallback"))
	case strings.HasPrefix(code, "skew_fallback:"):
		d.Severity = "methodology"
		expiry := strings.TrimPrefix(code, "skew_fallback:")
		d.Scope = expiry
		d.Message = "Skew fit fell back to sticky-IV for expiry " + expiry + "."
		d.Impact = "That expiry used the simpler IV assumption during the sweep."
	default:
		// Warning codes are typed before reaching this renderer. Keep the
		// fallback generic so a corrupt cache or future producer cannot turn
		// arbitrary text into browser-visible copy.
		d.Code = "unclassified_data_warning"
		d.Severity = "data_quality"
		d.Message = "An unclassified gamma data warning was received."
		d.Impact = "The affected gamma slice is not trusted for ranking until a typed warning is available."
	}
	return d
}

func gammaIVSourceImpact(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "The result is more model-dependent because every priced leg used quote/close inversion."
	}
	denom := c.PricedLegCount
	if denom == 0 {
		denom = c.LegCount
	}
	var parts []string
	if c.DerivedLiveMidLegs > 0 {
		parts = append(parts, fmt.Sprintf("%d live bid/ask midpoint", c.DerivedLiveMidLegs))
	}
	if c.DerivedPrevCloseLegs > 0 {
		parts = append(parts, fmt.Sprintf("%d prior option close", c.DerivedPrevCloseLegs))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("The result is more model-dependent because %d/%d priced legs used quote/close inversion instead of IBKR model-computation ticks.", c.DerivedIVLegs, denom)
	}
	return fmt.Sprintf("The result is more model-dependent: %d/%d priced legs used quote/close inversion (%s) instead of IBKR model-computation ticks.", c.DerivedIVLegs, denom, strings.Join(parts, ", "))
}

func gammaOIObservedCount(c *rpc.GammaZeroComputed) int {
	if c == nil {
		return 0
	}
	if c.LegDiagnostics == nil {
		return c.LegCount
	}
	return max(c.LegDiagnostics.Total.OpenInterestObservedLegs, c.LegDiagnostics.Total.OpenInterestLegs)
}

func gammaWarningSession(c *rpc.GammaZeroComputed) rpc.SessionClass {
	asOf := time.Now()
	if c != nil && !c.AsOf.IsZero() {
		asOf = c.AsOf
	}
	return gammaClassifySession(asOf)
}

func gammaOIMissingUnexpected(scope string, session rpc.SessionClass) bool {
	scope = strings.ToUpper(strings.TrimSpace(scope))
	return scope == "SPX" || session == rpc.SessionRTH
}

func gammaOIMissingAction(scope string, session rpc.SessionClass) string {
	prefix := "The option request already asks IBKR for generic tick 101 (call/put open interest). "
	if strings.EqualFold(strings.TrimSpace(scope), "SPX") {
		if session == rpc.SessionRTH {
			return prefix + "This affected SPX during regular U.S. option hours, when OI should normally be available if TWS has it; check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
		}
		return prefix + "This affected SPX. SPX option OI should normally be stable across session phases; missing API OI is unknown, not zero. Check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
	}
	switch session {
	case rpc.SessionRTH:
		return prefix + "This happened during regular U.S. option hours, when OI should normally be available if TWS has it; check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
	case rpc.SessionPre:
		return prefix + "This affected SPY pre-market, outside regular U.S. option hours, so sparse SPY OI is expected for the regular option-data surface; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	case rpc.SessionPost:
		return prefix + "This affected SPY post-market, outside regular U.S. option hours, so sparse SPY OI is expected for the regular option-data surface; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	default:
		return prefix + "This affected SPY while the regular U.S. option-data surface is closed, so sparse SPY OI is expected; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	}
}

func spyUnavailableWarningText(reason string) (message, impact, action string) {
	switch reason {
	case "354":
		return "SPY option chain was skipped: missing OPRA option market-data entitlement (IBKR 354).",
			"Showing SPX only; SPY gamma is not included.",
			"Check the U.S. options data subscription in IBKR, or run --only=spx to request the SPX surface directly."
	case "200":
		return "SPY option chain was skipped: contract resolution was rejected (IBKR 200).",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later or run --only=spx if SPY is not available on this gateway."
	case "no_data":
		return "SPY option chain was skipped: no option data landed within the window.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET or run --only=spx."
	case "fetch_canceled", "context canceled", "context_canceled":
		return "SPY option-chain fetch was canceled before usable data landed.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spx."
	case "timeout", "context deadline exceeded":
		return "SPY option-chain fetch timed out before usable data landed.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spx."
	case "throttled":
		return "SPY option chain was skipped after gateway throttling.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later; avoid repeated forced runs."
	case "zero_magnitude":
		return "SPY option chain was skipped because landed legs produced zero usable gamma magnitude.",
			"Showing SPX only; SPY gamma is not included because the SPY slice was not reliable enough to classify.",
			"Retry during regular trading hours or run --only=spy --force for diagnostics."
	default:
		return "SPY option chain was skipped: " + reason + ".",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later or run --only=spx."
	}
}

func spxUnavailableWarningText(reason string) (message, impact, action string) {
	switch reason {
	case "354":
		return "SPX option chain was skipped: missing CBOE OPRA entitlement (IBKR 354).",
			"Showing SPY only; SPX gamma is not included.",
			"Subscribe to the required market data or run --only=spy to suppress this banner."
	case "200":
		return "SPX option chain was skipped: contract resolution was rejected (IBKR 200).",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later or run --only=spy if SPX is not available on this gateway."
	case "no_data":
		return "SPX option chain was skipped: no option data landed within the window.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during regular trading hours or run --only=spy."
	case "fetch_canceled", "context canceled", "context_canceled":
		return "SPX option-chain fetch was canceled before usable data landed.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spy."
	case "timeout", "context deadline exceeded":
		return "SPX option-chain fetch timed out before usable data landed.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spy."
	case "throttled":
		return "SPX option chain was skipped after gateway throttling.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later; avoid repeated forced runs."
	case "zero_magnitude":
		return "SPX option chain was skipped because landed legs produced zero usable gamma magnitude.",
			"Showing SPY only; the SPX slice was not reliable enough to classify.",
			"Retry during regular trading hours or run --only=spx --force for diagnostics."
	default:
		return "SPX option chain was skipped: " + reason + ".",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later or run --only=spy."
	}
}

func spxCacheFallbackWarningText(reason string) (message, impact, action string) {
	reason = strings.TrimPrefix(reason, ":")
	if reason == "" {
		reason = "previous_success"
	}
	switch reason {
	case "fetch_canceled", "context canceled", "context_canceled":
		message = "SPX live refresh was canceled; using the last successful cached SPX slice."
	case "timeout", "context deadline exceeded":
		message = "SPX live refresh timed out; using the last successful cached SPX slice."
	case "throttled":
		message = "SPX live refresh was throttled; using the last successful cached SPX slice."
	case "354":
		message = "SPX live refresh hit an entitlement error; using the last successful cached SPX slice."
	case "200":
		message = "SPX live refresh hit a contract-resolution error; using the last successful cached SPX slice."
	default:
		message = "SPX live refresh was unavailable; using the last successful cached SPX slice."
	}
	return message,
		"SPX is included from cache; quality.rankability shows whether the gamma read is fresh and covered enough to act as a market-structure signal.",
		"Refresh during 09:30-16:15 ET and inspect the SPX per-index as_of before treating it as a fresh market-structure read."
}

func gammaWarningScope(c *rpc.GammaZeroComputed, code string) string {
	if strings.HasPrefix(code, "spy_unavailable:") {
		return "SPY"
	}
	if strings.HasPrefix(code, "spx_unavailable:") || strings.HasPrefix(code, "spx_cache_fallback") {
		return "SPX"
	}
	return gammaUnderlyingLabel(c)
}

func gammaUnderlyingLabel(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX"
	case rpc.GammaZeroScopeCombined:
		return "SPY+SPX"
	default:
		return "SPY"
	}
}

func gammaSummaryRange(lo, hi float64) string {
	if lo <= 0 || hi <= 0 {
		return ""
	}
	return formatGammaSummaryPrice(lo) + "-" + formatGammaSummaryPrice(hi)
}

func formatGammaSummaryPrice(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
