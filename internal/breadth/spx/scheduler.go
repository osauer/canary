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
// retrying a refresh that finished below MinCoverageFraction. Sized
// to give IBKR's per-account reqContractDetails bucket (~50 / 10 min,
// observed) time to refill — the dominant bottleneck during a 503-name
// cold-start fan-out. A previous refresh that resolved ~50 contracts
// drained the bucket; waiting 12 min lets the next attempt land another
// ~50 successful resolutions on top of the windows already persisted.
//
// The cadence holds until convergence or the next daily tick — a
// calendar bound, not a counter. A counter budget once expired ~3 h
// after the close; an HMDS outage that cleared later the same evening
// then stayed unpublished until the NEXT day's tick even though the
// data was one converged pass away. The health gate keeps a dead
// transport from burning fan-outs during that window, so the only cost
// of an unconverged evening is one paced attempt per delay.
const belowThresholdRetryDelay = 12 * time.Minute

// transportRetryDelay paces attempts while Refresh reports the transport
// path unusable (bulk lane down, historical farm broken). Each attempt is
// refused by the health gate before any per-symbol fetch, so this is a
// cheap in-memory poll, not gateway traffic — 60 s bounds how stale the
// gate's answer can get when a farm recovers without a reconnect
// handshake (IBKR sends farm-OK notices on existing connections). A
// rebuild of the lane also Kicks the scheduler directly, so
// reconnect-shaped recoveries do not wait even this long.
const transportRetryDelay = 60 * time.Second

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
//
//	retry — letting accumulated windows + IBKR's refilled
func (e *Engine) Run(ctx context.Context) {
	defer e.setRetryPending(false)

	retrying := false
	lastErrored := false
	lastTransportWarn := ""
	doRefresh := func(reason string) error {
		err := e.Refresh(ctx)
		if err != nil {
			// Transport waits poll every transportRetryDelay; warn only
			// when the reason changes so an hours-long farm outage reads
			// as an episode in the log, not a line per poll.
			if msg := err.Error(); msg != lastTransportWarn {
				e.warnf("breadth: %s refresh: %v", reason, err)
				lastTransportWarn = msg
			}
		} else {
			lastTransportWarn = ""
		}
		return err
	}
	// updateRetryState reads the post-refresh coverage signal. Called
	// only after refreshes that COMPLETED (no transport error) so a
	// below-threshold result triggers the short retry cadence —
	// otherwise the bootstrap's below-threshold outcome would sit idle
	// until the next 16:35 ET tick, defeating the retry mechanism.
	// Refresh errors take the transportRetryDelay path instead.
	updateRetryState := func() {
		cov, mc := e.LastRefreshCoverage()
		if mc > 0 && cov >= int(MinCoverageFraction*float64(mc)) {
			retrying = false
			e.setRetryPending(false)
			return
		}
		retrying = true
		e.setRetryPending(true)
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
		wait := e.nextWait(retrying, lastErrored)
		e.setNextAttempt(e.clock().Add(wait))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-e.kick:
			// The lane was just rebuilt; waiting out a transport or
			// retry delay would only defer recovery. A kick during the
			// healthy daily-tick sleep costs one no-op recompute.
			timer.Stop()
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
// gateway hiccup must not consume the coverage retry cadence.
func (e *Engine) nextWait(retrying, lastErrored bool) time.Duration {
	if lastErrored {
		return transportRetryDelay
	}
	if retrying {
		return belowThresholdRetryDelay
	}
	next := nextRefreshAt(e.clock())
	return max(next.Sub(e.clock()), 0)
}
