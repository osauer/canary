package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

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
