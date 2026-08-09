package spx

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
)

// refreshHourET and refreshMinuteET name the regular-session fallback
// either miss today's data or race the gateway.
const (
	refreshHourET   = 16
	refreshMinuteET = 35
)

const (
	refreshSettleDelay = 35 * time.Minute
	// publicationWindowDuration covers one normal full-universe HMDS pass.
	// without hiding a stuck or failed refresh for the rest of the evening.
	publicationWindowDuration = 90 * time.Minute
	calendarLookbackSessions  = 10
	calendarLookaheadDays     = 14
)

// belowThresholdRetryDelay is how long the scheduler waits before
// to give IBKR's per-account reqContractDetails bucket (~50 / 10 min,
const belowThresholdRetryDelay = 12 * time.Minute

// maxBelowThresholdRetries caps how many back-to-back retries the
// retry (IBKR's bucket math), 12 retries covers ~600 names — enough to
const maxBelowThresholdRetries = 15

// errorRetryDelay is the back-off applied when Refresh returns an
// completed-but-partial fan-out limited by IBKR's reqContractDetails
// bucket). Refresh errors are transport-level failures (gateway down,
const errorRetryDelay = 30 * time.Second

// nyLocation returns the America/New_York time.Location, falling back
// run at 16:35 UTC, which is mid-US-session) but never blocks the
func nyLocation() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}

// nextRefreshAt returns the next US-equity session close plus the
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
// completed session, which is the only daily-bar set the breadth cache
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
func PublicationDeadline(sessionKey string) (time.Time, bool) {
	refreshAt, ok := sessionRefreshAt(sessionKey)
	if !ok {
		return time.Time{}, false
	}
	return refreshAt.Add(publicationWindowDuration), true
}

// PublicationPending reports whether lastGoodSessionKey is the immediately
// not-due context only while this returns true; once the deadline passes (or
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
//
//	retry — letting accumulated windows + IBKR's refilled
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
	// only after refreshes that COMPLETED (no transport error) so a
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
// IBKR's per-account contract-details bucket has time to refill.
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
