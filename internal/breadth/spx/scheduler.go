package spx

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
)

// refreshHourET and refreshMinuteET name the regular-session fallback
// wakeup time (16:35 America/New_York). Calendar-backed scheduling
// uses session close + refreshSettleDelay, so known early closes wake
// earlier. The 35-minute pad gives the gateway time to settle late
// prints and busted trades before we sample the daily-bar feed. S&P
// DJI publishes S5FI post-close as well, so an earlier wake would
// either miss today's data or race the gateway.
const (
	refreshHourET   = 16
	refreshMinuteET = 35
)

const (
	refreshSettleDelay = 35 * time.Minute
	// publicationWindowDuration covers one normal full-universe HMDS pass.
	// The shared historical-data limiter permits an initial 60 requests and
	// then refills at 0.1 request/second, so the remaining 443 names take about
	// 74 minutes. Ninety minutes leaves a bounded margin for request latency
	// without hiding a stuck or failed refresh for the rest of the evening.
	publicationWindowDuration = 90 * time.Minute
	calendarLookbackSessions  = 10
	calendarLookaheadDays     = 14
)

// belowThresholdRetryDelay is how long the scheduler waits before
// retrying a refresh that finished below MinCoverageFraction. Sized
// to give IBKR's per-account reqContractDetails bucket (~50 / 10 min,
// observed) time to refill — the dominant bottleneck during a 503-name
// cold-start fan-out. A previous refresh that resolved ~50 contracts
// drained the bucket; waiting 12 min lets the next attempt land another
// ~50 successful resolutions on top of the windows already persisted.
const belowThresholdRetryDelay = 12 * time.Minute

// maxBelowThresholdRetries caps how many back-to-back retries the
// scheduler performs before falling through to the daily cadence.
// With belowThresholdRetryDelay = 12 min and ~50 new resolutions per
// retry (IBKR's bucket math), 12 retries covers ~600 names — enough to
// converge from a cold start for the S&P 500. The cap exists so a
// genuinely broken gateway doesn't keep us in a tight retry loop
// indefinitely: after the limit, we fall through to the daily 16:35 ET
// wake-up and the operator can investigate.
const maxBelowThresholdRetries = 15

// errorRetryDelay is the back-off applied when Refresh returns an
// error — distinct from belowThresholdRetryDelay (which assumes a
// completed-but-partial fan-out limited by IBKR's reqContractDetails
// bucket). Refresh errors are transport-level failures (gateway down,
// bulk-connector not yet ready, ctx cancellation upstream); they
// resolve in seconds, not minutes, so a short fixed back-off is
// right. 30 s is long enough not to retry-storm a recovering gateway
// and short enough that a one-shot blip clears within one user-visible
// poll cycle.
const errorRetryDelay = 30 * time.Second

// nyLocation returns the America/New_York time.Location, falling back
// to UTC if the zoneinfo database isn't available on this host. The
// fallback degrades cadence (a daemon on a UTC-only container would
// run at 16:35 UTC, which is mid-US-session) but never blocks the
// scheduler from running.
func nyLocation() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}

// nextRefreshAt returns the next US-equity session close plus the
// settlement pad. Weekends, holidays, and known early closes come
// from the embedded official calendar so the scheduler does not wake
// every closed day just to discover there are no new bars.
func nextRefreshAt(now time.Time) time.Time {
	cal := marketcal.NewWithClock(func() time.Time { return now })
	res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now, Days: calendarLookaheadDays})
	if err == nil {
		for _, session := range res.Sessions {
			if !isBreadthSession(session) {
				continue
			}
			refreshAt := breadthRefreshAt(session)
			if refreshAt.After(now) {
				return refreshAt
			}
		}
	}
	return fallbackNextRefreshAt(now)
}

func fallbackNextRefreshAt(now time.Time) time.Time {
	loc := nyLocation()
	localNow := now.In(loc)
	candidate := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		refreshHourET, refreshMinuteET, 0, 0, loc,
	)
	if !candidate.After(localNow) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for candidate.Weekday() == time.Saturday || candidate.Weekday() == time.Sunday {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// CompletedSessionKey returns the latest US-equity session whose close
// plus the breadth settlement pad has passed at now. During weekends,
// holidays, and pre-close trading hours this stays on the previous
// completed session, which is the only daily-bar set the breadth cache
// can publish without racing partial data.
func CompletedSessionKey(now time.Time) string {
	if key, ok := completedSessionKeyFromCalendar(now); ok {
		return key
	}
	return fallbackCompletedSessionKey(now)
}

func completedSessionKeyFromCalendar(now time.Time) (string, bool) {
	loc := nyLocation()
	localNow := now.In(loc)
	cal := marketcal.NewWithClock(func() time.Time { return now })
	for daysBack := range calendarLookbackSessions {
		day := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 12, 0, 0, 0, loc).AddDate(0, 0, -daysBack)
		res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: day.Format("2006-01-02"), Days: 1})
		if err != nil {
			return "", false
		}
		if !isBreadthSession(res.Session) {
			continue
		}
		if !breadthRefreshAt(res.Session).After(now) {
			return res.Session.Date, true
		}
	}
	return "", false
}

func fallbackCompletedSessionKey(now time.Time) string {
	loc := nyLocation()
	localNow := now.In(loc)
	candidate := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		refreshHourET, refreshMinuteET, 0, 0, loc,
	)
	if candidate.After(localNow) {
		candidate = candidate.AddDate(0, 0, -1)
	}
	for candidate.Weekday() == time.Saturday || candidate.Weekday() == time.Sunday {
		candidate = candidate.AddDate(0, 0, -1)
	}
	return candidate.Format("2006-01-02")
}

func sessionRefreshAt(sessionKey string) (time.Time, bool) {
	cal := marketcal.New()
	res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: sessionKey, Days: 1})
	if err == nil && isBreadthSession(res.Session) {
		return breadthRefreshAt(res.Session), true
	}

	loc := nyLocation()
	day, err := time.ParseInLocation("2006-01-02", sessionKey, loc)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(day.Year(), day.Month(), day.Day(), refreshHourET, refreshMinuteET, 0, 0, loc), true
}

// PublicationDeadline returns the bounded deadline for publishing one
// session's breadth snapshot. The start follows the official session close
// (including known early closes) plus the normal settlement delay; the end is
// sized for one normally paced full-universe HMDS pass.
func PublicationDeadline(sessionKey string) (time.Time, bool) {
	refreshAt, ok := sessionRefreshAt(sessionKey)
	if !ok {
		return time.Time{}, false
	}
	return refreshAt.Add(publicationWindowDuration), true
}

// PublicationPending reports whether lastGoodSessionKey is the immediately
// prior session and an active refresh is still inside the current session's
// bounded publication window. Callers may keep that prior last-good as typed
// not-due context only while this returns true; once the deadline passes (or
// the engine is no longer active), the older session is overdue.
func PublicationPending(lastGoodSessionKey string, refreshActive bool, now time.Time) bool {
	if lastGoodSessionKey == "" || !refreshActive {
		return false
	}
	targetSession := CompletedSessionKey(now)
	if targetSession == "" || targetSession == lastGoodSessionKey {
		return false
	}
	refreshAt, ok := sessionRefreshAt(targetSession)
	if !ok || now.Before(refreshAt) || !now.Before(refreshAt.Add(publicationWindowDuration)) {
		return false
	}
	previousSession := CompletedSessionKey(refreshAt.Add(-time.Nanosecond))
	return lastGoodSessionKey == previousSession
}

func breadthRefreshAt(session marketcal.Session) time.Time {
	return session.Close.Add(refreshSettleDelay)
}

func isBreadthSession(session marketcal.Session) bool {
	return session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose
}

// shouldRefreshOnStartup reports whether the engine should run a
// catch-up Refresh as soon as Run() starts, before settling into the
// daily cadence. The conditions are:
//
//  1. No snapshot has ever been computed (cold install). Always run.
//  2. The cached snapshot is for an older completed US-equity session.
//     We missed at least one tradable post-close refresh while the
//     daemon was down — run now rather than wait for the next close.
//  3. The cached snapshot has the current session key but its AsOf
//     predates that session's close-plus-pad. This catches the rare
//     pre-close partial snapshot that would otherwise look current
//     by date alone.
//
// When none of these hold, the scheduler sleeps until the next
// tradable close plus settlement pad.
func shouldRefreshOnStartup(snap *Snapshot, now time.Time) bool {
	if snap == nil {
		return true
	}
	targetSession := CompletedSessionKey(now)
	if snap.SessionKey != targetSession {
		return true
	}
	refreshAt, ok := sessionRefreshAt(targetSession)
	return ok && snap.AsOf.Before(refreshAt)
}

// Run starts the engine's scheduler. Returns when ctx is cancelled.
// Designed to be called once from the daemon's startup sequence inside
// a goroutine; multiple concurrent Run loops on the same Engine would
// compete on the refresh mutex without crashing, but the caller
// shouldn't do that.
//
// Lifecycle:
//   - On entry, check shouldRefreshOnStartup. If true, call Refresh
//     immediately so handleBreadthSPX has data ASAP.
//   - After each refresh, check coverage. If it converged (≥ threshold),
//     sleep until nextRefreshAt (daily cadence). If it didn't and
//     we're under the retry limit, sleep belowThresholdRetryDelay and
//     retry — letting accumulated windows + IBKR's refilled
//     reqContractDetails bucket push coverage higher on the next pass.
//   - On ctx.Done at any point: return cleanly. An in-flight Refresh
//     is cancelled via its context.
//
// Errors from Refresh are logged but do not stop the loop — the
// engine retries on the next tick. A transient gateway disconnect
// shouldn't take the daily cadence offline.
func (e *Engine) Run(ctx context.Context) {
	defer e.setRetryPending(false)

	retries := 0
	lastErrored := false
	doRefresh := func(reason string) error {
		err := e.Refresh(ctx)
		if err != nil {
			e.warnf("breadth: %s refresh: %v", reason, err)
		}
		return err
	}
	// updateRetryState reads the post-refresh coverage signal and
	// adjusts the retry counter for the next loop iteration. Called
	// only after refreshes that COMPLETED (no transport error) so a
	// below-threshold result triggers the short retry cadence —
	// otherwise the bootstrap's below-threshold outcome would sit idle
	// until the next 16:35 ET tick, defeating the retry mechanism.
	// Refresh errors take the errorRetryDelay path instead.
	updateRetryState := func() {
		cov, mc := e.LastRefreshCoverage()
		converged := mc > 0 && cov >= int(MinCoverageFraction*float64(mc))
		switch {
		case converged:
			retries = 0
			e.setRetryPending(false)
		case retries < maxBelowThresholdRetries:
			retries++
			e.setRetryPending(true)
		default:
			// Burned through the retry budget without converging.
			// Reset and fall back to the daily cadence — the
			// operator should investigate (the warnf in finalise
			// already logged each below-threshold result).
			//
			// Say so explicitly: every below-threshold pass logs an
			// identical line, so without this the transition from
			// "retrying every 12 min" to "idle until tomorrow" is
			// invisible — the log simply stops, which reads like a
			// crashed scheduler rather than a documented give-up.
			e.warnf("breadth: %d consecutive refreshes stayed below the coverage threshold (last pass %d/%d); leaving the %s retry cadence and waiting for the next daily tick",
				maxBelowThresholdRetries, cov, mc, belowThresholdRetryDelay)
			retries = 0
			e.setRetryPending(false)
		}
	}

	if cur, _ := e.Get(); shouldRefreshOnStartup(cur, e.clock()) {
		lastErrored = doRefresh("bootstrap") != nil
		if ctx.Err() != nil {
			return
		}
		if !lastErrored {
			updateRetryState()
		}
	}

	for {
		wait := e.nextWait(retries, lastErrored)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		lastErrored = doRefresh("scheduled") != nil
		if ctx.Err() != nil {
			return
		}
		if !lastErrored {
			updateRetryState()
		} else {
			e.setRetryPending(false)
		}
	}
}

// nextWait returns how long Run should sleep before the next refresh.
// Priority: a transport error gets the short errorRetryDelay (a
// recovering gateway clears in seconds). Below-threshold coverage
// (refresh completed but partial) gets belowThresholdRetryDelay so
// IBKR's per-account contract-details bucket has time to refill.
// Otherwise we're in the steady-state daily cadence and wait until
// 16:35 ET.
//
// The error path and coverage path are deliberately distinct: a startup-time
// gateway hiccup must not consume the coverage retry budget.
func (e *Engine) nextWait(retries int, lastErrored bool) time.Duration {
	if lastErrored {
		return errorRetryDelay
	}
	if retries > 0 {
		return belowThresholdRetryDelay
	}
	next := nextRefreshAt(e.clock())
	return max(next.Sub(e.clock()), 0)
}
