package daemon

import (
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// streakIndicator is the per-indicator surface populateStreaks iterates.
// Each implementation is a zero-state struct — pure dispatch, no fields.
// Variations between indicators (status gate, classifier inputs, value
// extraction, slot to attach the streak to) are encapsulated here so
// populateStreaks itself is one loop.
//
// The confirmation-policy methods (displayBand, depth, fresh, exitHoldsRed)
// implement internal-docs/design/regime-calibration.md: classification + hysteresis
// run HERE, once, daemon-side; every downstream consumer reads the served
// post-hysteresis band and eligibility verdict.
type streakIndicator interface {
	key() string
	// bandAndValue inspects res and returns the band/value the streak
	// counter should tick with. Returns ("", 0) to freeze the counter —
	// status not usable or required fields missing.
	bandAndValue(res *rpc.RegimeSnapshotResult) (band string, value float64)
	// attachStreak writes s into the indicator's slot in res.
	attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo)
	// displayBand is the band shown on the row's meta. Usually identical
	// to bandAndValue's band; gamma diverges on stale (band stays visible
	// for awareness while the streak freezes and the cluster unranks).
	displayBand(res *rpc.RegimeSnapshotResult) string
	// depth extracts the eligibility depth metric in the indicator's gate
	// units (rpc.RegimeGateFor). Nil when the indicator has none — the
	// band threshold itself is the depth gate.
	depth(res *rpc.RegimeSnapshotResult) *float64
	// fresh is the cadence-relative freshness verdict: no newer
	// observation should exist under the indicator's native cadence.
	fresh(res *rpc.RegimeSnapshotResult, nowNY time.Time) bool
	// exitHoldsRed reports whether the red-exit hysteresis threshold
	// still holds — consulted only when the previous tick was red and the
	// fresh classification left red, to prevent boundary flapping.
	exitHoldsRed(res *rpc.RegimeSnapshotResult) bool
}

var streakIndicators = []streakIndicator{
	vixTermStreaks{}, volOfVolStreaks{},
	hygSpyStreaks{}, creditSpreadsStreaks{}, fundingStressStreaks{}, usdJpyStreaks{},
	gammaZeroStreaks{}, breadthStreaks{},
}

// vixTermStreaks — VIX/VIX3M term-structure ratio.
type vixTermStreaks struct{}

func (vixTermStreaks) key() string { return StreakKeyVIXTerm }

func (vixTermStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.VIXTermStructure.Status != rpc.RegimeStatusOK && res.VIXTermStructure.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyVIXTermBand(res.VIXTermStructure.Ratio)
	var value float64
	if res.VIXTermStructure.Ratio != nil {
		value = *res.VIXTermStructure.Ratio
	}
	return band, value
}

func (vixTermStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.VIXTermStructure.Streak = s
}

// volOfVolStreaks — VVIX level.
type volOfVolStreaks struct{}

func (volOfVolStreaks) key() string { return StreakKeyVolOfVol }

func (volOfVolStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.VolOfVol.Status != rpc.RegimeStatusOK && res.VolOfVol.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyVolOfVolBand(res.VolOfVol.Last)
	var value float64
	if res.VolOfVol.Last != nil {
		value = *res.VolOfVol.Last
	}
	return band, value
}

func (volOfVolStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.VolOfVol.Streak = s
}

// hygSpyStreaks — HYG vs SPY divergence band.
type hygSpyStreaks struct{}

func (hygSpyStreaks) key() string { return StreakKeyHYGSPY }

func (hygSpyStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.HYGSPYDivergence.Status != rpc.RegimeStatusOK && res.HYGSPYDivergence.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyHYGSPYBand(res.HYGSPYDivergence)
	var value float64
	if res.HYGSPYDivergence.HYGPrice != nil {
		value = *res.HYGSPYDivergence.HYGPrice
	}
	return band, value
}

func (hygSpyStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.HYGSPYDivergence.Streak = s
}

// creditSpreadsStreaks — official HY OAS stress band.
type creditSpreadsStreaks struct{}

func (creditSpreadsStreaks) key() string { return StreakKeyCredit }

func (creditSpreadsStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.CreditSpreads.Status != rpc.RegimeStatusOK && res.CreditSpreads.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyCreditSpreadsBand(res.CreditSpreads)
	var value float64
	if res.CreditSpreads.HYOAS != nil {
		value = *res.CreditSpreads.HYOAS
	}
	return band, value
}

func (creditSpreadsStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.CreditSpreads.Streak = s
}

// fundingStressStreaks — CP/T-bill spread in basis points.
type fundingStressStreaks struct{}

func (fundingStressStreaks) key() string { return StreakKeyFunding }

func (fundingStressStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.FundingStress.Status != rpc.RegimeStatusOK && res.FundingStress.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyFundingStressBand(res.FundingStress.SpreadBps)
	var value float64
	if res.FundingStress.SpreadBps != nil {
		value = *res.FundingStress.SpreadBps
	}
	return band, value
}

func (fundingStressStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.FundingStress.Streak = s
}

// usdJpyStreaks — USD/JPY weekly-change band.
type usdJpyStreaks struct{}

func (usdJpyStreaks) key() string { return StreakKeyUSDJPY }

func (usdJpyStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.USDJPY.Status != rpc.RegimeStatusOK && res.USDJPY.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyUSDJPYBand(res.USDJPY.WeeklyChange)
	var value float64
	if res.USDJPY.WeeklyChange != nil {
		value = *res.USDJPY.WeeklyChange
	}
	return band, value
}

func (usdJpyStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.USDJPY.Streak = s
}

// gammaZeroStreaks gates on OK-only because the gamma envelope's Stale
// path doesn't carry a Result pointer; the nested-pointer check is
// meaningful and must precede classifier invocation.
type gammaZeroStreaks struct{}

func (gammaZeroStreaks) key() string { return StreakKeyGammaZero }

func (gammaZeroStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.GammaZero.Status != rpc.RegimeStatusOK || res.GammaZero.Envelope.Result == nil {
		return "", 0
	}
	c := res.GammaZero.Envelope.Result
	return classifyGammaComputedBand(c), gammaComputedStreakValue(c)
}

func (gammaZeroStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.GammaZero.Streak = s
}

// breadthStreaks — S&P 500 breadth pct-above-50DMA. Additionally gates
// on Envelope.State == BreadthStateReady; value is a plain float64
// (not a pointer) so no nil check is needed.
type breadthStreaks struct{}

func (breadthStreaks) key() string { return StreakKeyBreadth }

func (breadthStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if (res.Breadth.Status != rpc.RegimeStatusOK && res.Breadth.Status != rpc.RegimeStatusStale) || res.Breadth.Envelope.State != rpc.BreadthStateReady {
		return "", 0
	}
	value := res.Breadth.Envelope.PctAbove50DMA
	band := classifyBreadthBand(value)
	return band, value
}

func (breadthStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.Breadth.Streak = s
}

// ---------------------------------------------------------------------------
// Confirmation-policy methods (eligibility depth, cadence freshness,
// red-exit hysteresis, display band). Gate values live in
// internal/rpc/regime_policy.go; exit thresholds here mirror the design
// doc's per-indicator table.

func (v vixTermStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := v.bandAndValue(res)
	return band
}

func (vixTermStreaks) depth(res *rpc.RegimeSnapshotResult) *float64 {
	return res.VIXTermStructure.Ratio
}

// VIX freshness: live rows are fresh at any hour. Frozen rows remain
// confirmation-ineligible. VIX3M is disseminated only during Cboe regular
// trading hours (approximately 09:31-16:15 ET), so a prior close is not due
// before 09:31 ET or on an official closed date; it becomes overdue once a
// newer VIX3M observation should exist.
func (vixTermStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.VIXTermStructure.Status == rpc.RegimeStatusOK
}

// Cboe keeps publishing VIX3M for a quarter hour past the equity close, so the
// dissemination window runs 09:31 to close+15m.
const vix3mDisseminationTail = 15 * time.Minute

// vix3mCrossSourceTolerance is how far the broker's VIX3M may sit from Cboe's
// published close for the same session before the leg is called stuck, in index
// points (Cboe quotes the file to two decimals). Heuristic and operator-owned,
// like the band thresholds: sized to clear any rounding or mark-versus-
// settlement difference while still catching a leg serving another session's
// value, which normally moves VIX3M by several tenths of a point.
const vix3mCrossSourceTolerance = 0.25

// vix3mWindow is the single definition of one session's VIX3M publication
// window. Every VIX3M schedule question — cadence class, honest age, carry
// bound — resolves through it.
func vix3mWindow(session marketcal.Session) (start, end time.Time) {
	open := session.Open
	start = time.Date(open.Year(), open.Month(), open.Day(), 9, 31, 0, 0, open.Location())
	return start, session.Close.Add(vix3mDisseminationTail)
}

// vix3mDisseminating reports whether VIX3M is being published at nowNY.
func vix3mDisseminating(nowNY time.Time) bool {
	cal := marketcal.NewWithClock(func() time.Time { return nowNY })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, nowNY)
	if err != nil || (session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose) {
		return false
	}
	start, end := vix3mWindow(session)
	return !nowNY.Before(start) && nowNY.Before(end)
}

// vix3mLastDisseminationWindow is the most recently completed publication
// window. A frozen leg's value comes from it, and a carried value observed
// before its start means a dead subscription rather than a slow one.
func vix3mLastDisseminationWindow(now time.Time) (start, end time.Time, ok bool) {
	date, _, found := lastCompletedOptionsSession(now)
	if !found {
		return time.Time{}, time.Time{}, false
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", date, ny)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, day.Add(12*time.Hour))
	if err != nil || session.Close.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	start, end = vix3mWindow(session)
	return start, end, true
}

// In-session VIX3M carry tolerance. A carried leg distorts the printed ratio
// by the VIX3M drift since its observation, which a VIX move bounds from above
// because VIX3M is the slower leg: 1% keeps a true 1.02 ratio printing near
// 1.03 rather than the 1.05 that would read as backwardation. The 15-minute
// ceiling is three regime polls, past which a thin index that keeps missing is
// a gap rather than a slow tick. Operator decision, 2026-07-31.
const (
	vix3mCarryMaxVIXMovePct = 1.0
	vix3mCarryMaxAge        = 15 * time.Minute
)

// vix3mCurrentWindow returns the publication window nowNY sits inside.
func vix3mCurrentWindow(nowNY time.Time) (start, end time.Time, ok bool) {
	cal := marketcal.NewWithClock(func() time.Time { return nowNY })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, nowNY)
	if err != nil || (session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose) {
		return time.Time{}, time.Time{}, false
	}
	start, end = vix3mWindow(session)
	if nowNY.Before(start) || !nowNY.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// vix3mCarryWithinTolerance reports whether a VIX3M leg observed earlier in the
// current publication window may still stand in for a missed poll. It is the
// single authority for that question: the carry site applies it before keeping
// a value, and the cadence classifier applies it again before typing the row,
// so a row assembled anywhere else cannot slip past the gate.
func vix3mCarryWithinTolerance(row rpc.RegimeVIXTerm, nowNY time.Time) bool {
	if row.VIX == nil || row.VIX3M == nil || row.VIX3MQuality == nil || row.VIX3MAnchorVIX == nil {
		return false
	}
	anchor := *row.VIX3MAnchorVIX
	if anchor <= 0 || *row.VIX <= 0 || *row.VIX3M <= 0 {
		return false
	}
	observed := row.VIX3MQuality.AsOf
	start, _, ok := vix3mCurrentWindow(nowNY)
	if !ok || observed.IsZero() || observed.Before(start) || observed.After(nowNY) {
		return false
	}
	if nowNY.Sub(observed) > vix3mCarryMaxAge {
		return false
	}
	return math.Abs(*row.VIX-anchor)/anchor*100 <= vix3mCarryMaxVIXMovePct
}

// vix3mTickQuality stamps a live leg when its tick arrived and a frozen leg at
// the end of the window that produced it. A frozen quote's arrival instant is
// essentially read time — the gateway re-sends the last known value on
// request — so only the window end shows a frozen leg's true vintage.
func vix3mTickQuality(observedAt, now time.Time, dataType string) *rpc.Quality {
	q := firmTickQuality(observedAt, now, dataType, "VIX3M tick (thin CBOE; off-hours typically frozen)")
	if rpc.IsLiveDataType(dataType) {
		return q
	}
	if _, end, ok := vix3mLastDisseminationWindow(now); ok && end.Before(now) {
		q.AsOf = end
	}
	return q
}

func vixTermCadenceClass(res *rpc.RegimeSnapshotResult, nowNY time.Time) string {
	if res == nil || nowNY.IsZero() {
		return rpc.RegimeFreshnessOverdue
	}
	row := res.VIXTermStructure
	vixClass, vixOK := regimeTickQualityClass(row.VIXQuality, nowNY)
	vix3mClass, vix3mOK := regimeTickQualityClass(row.VIX3MQuality, nowNY)
	// Only usable row states with both typed legs can participate.
	if (row.Status != rpc.RegimeStatusOK && row.Status != rpc.RegimeStatusStale) || !vixOK || !vix3mOK {
		return rpc.RegimeFreshnessOverdue
	}
	cal := marketcal.NewWithClock(func() time.Time { return nowNY })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, nowNY)
	if err != nil || session.State == marketcal.StateUnknown {
		return rpc.RegimeFreshnessOverdue
	}
	switch session.State {
	case marketcal.StateClosed, marketcal.StateHoliday:
		// No dissemination window today; the tail rule decides.
	case marketcal.StateRegular, marketcal.StateEarlyClose:
		local := nowNY.In(session.Open.Location())
		vix3mStart, vix3mEnd := vix3mWindow(session)
		if !local.Before(vix3mStart) && local.Before(vix3mEnd) {
			if row.Status == rpc.RegimeStatusOK && vixClass == rpc.FreshnessLive && vix3mClass == rpc.FreshnessLive {
				return rpc.RegimeFreshnessFresh
			}
			// A missed poll inside the window is a failed refresh: stale while
			// the print is from this window and inside the carry tolerance,
			// overdue past it. Never not_due — the window is open.
			if vixClass == rpc.FreshnessLive && vix3mCarryWithinTolerance(row, local) {
				return rpc.RegimeFreshnessStale
			}
			return rpc.RegimeFreshnessOverdue
		}
	default:
		return rpc.RegimeFreshnessOverdue
	}
	// Outside VIX3M's dissemination window no newer observation can exist, so
	// the row is context, never a defect. Neither leg's class is consulted
	// here: IBKR reports the subscription's mode, not an observation's age,
	// and it flips both Cboe index legs between live and frozen off-window
	// without anything going missing. Confirmable freshness is unreachable
	// outside the window regardless, so an off-window "live" label still
	// cannot become confirming evidence. A genuinely absent leg is already
	// overdue above, via the row status and typed-quality checks.
	//
	// What the window alone cannot say is how old the value behind a frozen leg
	// really is. not_due exempts the row from every age bound, so it is granted
	// only once Cboe's dated close has established the leg's vintage; an
	// uncorroborated or contradicted leg is overdue.
	if !rpc.VIX3MCrossCheckVouches(row.VIX3MCrossCheck) {
		return rpc.RegimeFreshnessOverdue
	}
	return rpc.RegimeFreshnessNotDue
}

func regimeTickQualityClass(quality *rpc.Quality, now time.Time) (string, bool) {
	if quality == nil || quality.AsOf.IsZero() || quality.AsOf.After(now.Add(time.Minute)) {
		return "", false
	}
	class := strings.ToLower(strings.TrimSpace(quality.FreshnessClass))
	switch class {
	// Derived covers the official close serving as the off-window VIX3M leg.
	// It is a typed dated observation, and since only a live class reaches
	// confirmable freshness inside the window, accepting it here cannot let
	// end-of-day evidence confirm anything.
	case rpc.FreshnessLive, rpc.FreshnessFrozen, rpc.FreshnessDerived:
		return class, true
	default:
		return "", false
	}
}

func gammaCadenceClass(res *rpc.RegimeSnapshotResult, now time.Time) string {
	if res == nil || res.GammaZero.Envelope.Result == nil {
		return rpc.RegimeFreshnessOverdue
	}
	served := res.GammaZero.Status == rpc.RegimeStatusOK || res.GammaZero.Status == rpc.RegimeStatusStale
	switch gammaOperationalCadence(&res.GammaZero.Envelope, now) {
	case rpc.DataCadenceCurrent:
		if res.GammaZero.Status == rpc.RegimeStatusOK {
			return rpc.RegimeFreshnessFresh
		}
	case rpc.DataCadenceNotDue:
		if served {
			return rpc.RegimeFreshnessNotDue
		}
	case rpc.DataCadenceMissedSession:
		// The session's first compute is in flight inside its bounded window,
		// so the last completed session's result is still the newest that
		// exists. Non-confirming context, and overdue the moment the typed
		// in-flight marker or the window goes away.
		if served && gammaPublicationPending(&res.GammaZero.Envelope, now) {
			return rpc.RegimeFreshnessPending
		}
	}
	return rpc.RegimeFreshnessOverdue
}

// breadthCadenceClass keeps the raw row honest (the served snapshot is stale
// once CompletedSessionKey rolls) while identifying the one expected gap: the
// immediately prior last-good is typed pending while an active HMDS refresh is
// still inside its bounded, calendar-based publication window. Pending rather
// than not_due since the session's own observation is genuinely due — it is
// being computed — which is the same state gamma reaches at the options open.
func breadthCadenceClass(res *rpc.RegimeSnapshotResult, now time.Time) string {
	if res == nil {
		return rpc.RegimeFreshnessOverdue
	}
	row := res.Breadth
	if row.Envelope.State != rpc.BreadthStateReady {
		return rpc.RegimeFreshnessOverdue
	}
	if spx.PublicationPending(row.Envelope.SessionKey, row.Envelope.Refreshing, now) &&
		(row.Status == rpc.RegimeStatusOK || row.Status == rpc.RegimeStatusStale) {
		return rpc.RegimeFreshnessPending
	}
	if row.Status == rpc.RegimeStatusOK {
		return rpc.RegimeFreshnessFresh
	}
	return rpc.RegimeFreshnessOverdue
}

// idealproTrading reports whether IBKR's IDEALPRO FX session is trading at
// nowET. IDEALPRO runs one continuous weekly session rather than daily ones:
// the broker publishes 17:15-to-next-day-17:00 blocks with Saturday CLOSED
// (USD.JPY contract-details tradingHours, timezone US/Eastern), so FX is shut
// for a 15-minute daily changeover and from Friday 17:00 until Sunday 17:15.
// Holiday closures are not modelled — on one the row reads overdue, which is
// the conservative direction and no worse than treating every gap that way.
func idealproTrading(nowET time.Time) bool {
	const (
		sessionEnd   = 17 * 60    // 17:00 ET
		sessionStart = 17*60 + 15 // 17:15 ET
	)
	mins := nowET.Hour()*60 + nowET.Minute()
	switch nowET.Weekday() {
	case time.Saturday:
		return false
	case time.Sunday:
		return mins >= sessionStart
	case time.Friday:
		return mins < sessionEnd
	default:
		return mins < sessionEnd || mins >= sessionStart
	}
}

// USD/JPY freshness: the cadence question for a continuous market is simply
// whether it is trading. While IDEALPRO is open only a live tick is current,
// so a frozen one is a real gap. While it is shut the last tick — or the HMDS
// midpoint close the row falls back to — is the newest observation that can
// exist, exactly like an equity row off-hours.
func usdJpyCadenceClass(res *rpc.RegimeSnapshotResult, nowNY time.Time) string {
	if res == nil || nowNY.IsZero() {
		return rpc.RegimeFreshnessOverdue
	}
	status := res.USDJPY.Status
	if status != rpc.RegimeStatusOK && status != rpc.RegimeStatusStale {
		return rpc.RegimeFreshnessOverdue
	}
	if !idealproTrading(nowNY) {
		return rpc.RegimeFreshnessNotDue
	}
	if status == rpc.RegimeStatusOK {
		return rpc.RegimeFreshnessFresh
	}
	return rpc.RegimeFreshnessOverdue
}

// Exit hysteresis: leave red only when the ratio falls below 0.98.
func (vixTermStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	return res.VIXTermStructure.Ratio != nil && *res.VIXTermStructure.Ratio >= 0.98
}

func (v volOfVolStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := v.bandAndValue(res)
	return band
}

func (volOfVolStreaks) depth(res *rpc.RegimeSnapshotResult) *float64 {
	return res.VolOfVol.Last
}

// VVIX freshness: the official daily close, allowing weekend + publication
// lag. Beyond ~4 calendar days a newer close must exist.
func (volOfVolStreaks) fresh(res *rpc.RegimeSnapshotResult, nowNY time.Time) bool {
	if res.VolOfVol.Status != rpc.RegimeStatusOK {
		return false
	}
	return officialDateWithinDays(res.VolOfVol.AsOfDate, nowNY, 4)
}

// Exit hysteresis: leave red below 105.
func (volOfVolStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	return res.VolOfVol.Last != nil && *res.VolOfVol.Last >= 105
}

func (h hygSpyStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := h.bandAndValue(res)
	return band
}

// Depth in percent below the 50DMA: (dma − price) / dma × 100.
func (hygSpyStreaks) depth(res *rpc.RegimeSnapshotResult) *float64 {
	r := res.HYGSPYDivergence
	if r.HYGPrice == nil || r.HYG50DMA == nil || *r.HYG50DMA <= 0 {
		return nil
	}
	d := (*r.HYG50DMA - *r.HYGPrice) / *r.HYG50DMA * 100
	return &d
}

// HYG freshness: an RTH tick or the latest official close (the off-hours
// banding input) is the newest possible observation — both land status ok.
func (hygSpyStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.HYGSPYDivergence.Status == rpc.RegimeStatusOK
}

// Exit hysteresis: leave red only after HYG closes back above its 50DMA —
// SPY drifting off the near-high line alone does not end a credit break.
func (hygSpyStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	r := res.HYGSPYDivergence
	return r.HYGPrice != nil && r.HYG50DMA != nil && *r.HYGPrice < *r.HYG50DMA
}

func (c creditSpreadsStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := c.bandAndValue(res)
	return band
}

// Official series red levels are already deep — no separate depth metric.
func (creditSpreadsStreaks) depth(_ *rpc.RegimeSnapshotResult) *float64 { return nil }

func (creditSpreadsStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.CreditSpreads.Status == rpc.RegimeStatusOK
}

// Exit hysteresis: leave red when HY OAS < 5.25 and the 20-obs widening
// < 0.85 pp.
func (creditSpreadsStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	r := res.CreditSpreads
	if r.HYOAS != nil && *r.HYOAS >= 5.25 {
		return true
	}
	return r.HY20DChange != nil && *r.HY20DChange >= 0.85
}

func (f fundingStressStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := f.bandAndValue(res)
	return band
}

func (fundingStressStreaks) depth(_ *rpc.RegimeSnapshotResult) *float64 { return nil }

func (fundingStressStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.FundingStress.Status == rpc.RegimeStatusOK
}

// Exit hysteresis: leave red below 65 bp.
func (fundingStressStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	return res.FundingStress.SpreadBps != nil && *res.FundingStress.SpreadBps >= 65
}

func (u usdJpyStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := u.bandAndValue(res)
	return band
}

// Speed is the depth for the carry proxy — the 2%/week red band is the gate.
func (usdJpyStreaks) depth(_ *rpc.RegimeSnapshotResult) *float64 { return nil }

func (usdJpyStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.USDJPY.Status == rpc.RegimeStatusOK
}

// Exit hysteresis: leave red when the weekly yen move falls below 1.5%.
func (usdJpyStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	if res.USDJPY.WeeklyChange == nil {
		return false
	}
	return -*res.USDJPY.WeeklyChange >= 1.5
}

// gamma's display band stays visible on STALE rows (prior-trading-date
// cache): the red is awareness evidence even though the streak freezes, the
// cluster unranks, and eligibility reports data_overdue.
func (gammaZeroStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	if res.GammaZero.Status != rpc.RegimeStatusOK && res.GammaZero.Status != rpc.RegimeStatusStale {
		return ""
	}
	if res.GammaZero.Envelope.Result == nil {
		return ""
	}
	return classifyGammaComputedBand(res.GammaZero.Envelope.Result)
}

// Depth in percent below gamma-zero (−gap); see rpc.RegimeGammaDepth.
func (gammaZeroStreaks) depth(res *rpc.RegimeSnapshotResult) *float64 {
	return rpc.RegimeGammaDepth(res.GammaZero.Envelope.Result)
}

// Gamma freshness: fetchRegimeGamma already downgrades prior-trading-date
// computes to status stale, so status ok ⇔ cadence-fresh.
func (gammaZeroStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.GammaZero.Status == rpc.RegimeStatusOK && res.GammaZero.Envelope.Result != nil
}

// Exit hysteresis: leave red when spot clears +0.5% above gamma-zero.
func (gammaZeroStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	d := rpc.RegimeGammaDepth(res.GammaZero.Envelope.Result)
	return d != nil && *d >= -0.5
}

func (b breadthStreaks) displayBand(res *rpc.RegimeSnapshotResult) string {
	band, _ := b.bandAndValue(res)
	return band
}

// Depth in points below the 40% band floor.
func (breadthStreaks) depth(res *rpc.RegimeSnapshotResult) *float64 {
	if res.Breadth.Envelope.State != rpc.BreadthStateReady {
		return nil
	}
	d := 40 - res.Breadth.Envelope.PctAbove50DMA
	return &d
}

// Breadth freshness: the post-close compute of the last completed session
// is inherently the newest possible observation; the session-key staleness
// check already runs in fetchRegimeBreadth, so status ok ⇔ fresh.
func (breadthStreaks) fresh(res *rpc.RegimeSnapshotResult, _ time.Time) bool {
	return res.Breadth.Status == rpc.RegimeStatusOK
}

// Exit hysteresis: leave red above 45% of members over their 50DMA.
func (breadthStreaks) exitHoldsRed(res *rpc.RegimeSnapshotResult) bool {
	return res.Breadth.Envelope.State == rpc.BreadthStateReady && res.Breadth.Envelope.PctAbove50DMA < 45
}

// officialDateWithinDays reports whether a YYYY-MM-DD observation date is
// within n calendar days of nowNY. Unparseable/empty dates are not fresh.
func officialDateWithinDays(date string, nowNY time.Time, n int) bool {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return false
	}
	return nowNY.Sub(d) <= time.Duration(n)*24*time.Hour
}
