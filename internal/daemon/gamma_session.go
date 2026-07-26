package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaClassifySession classifies the option-data surface used by dealer
// gamma, not the underlying ETF quote session. The compute needs option OI,
// IV/model ticks, and classed SPX/SPXW contracts; outside the official regular
// U.S. listed-options session a non-force refresh is not expected to improve a
// good last-known snapshot reliably.
func gammaClassifySession(now time.Time) rpc.SessionClass {
	if now.IsZero() {
		now = time.Now()
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, now)
	if err == nil {
		if session.IsOpen {
			return rpc.SessionRTH
		}
		if session.State != marketcal.StateUnknown {
			return rpc.SessionClosed
		}
	}
	if gammaWeekdayOptionsRegular(now) {
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

func lastCompletedOptionsSession(now time.Time) (string, marketcal.Session, bool) {
	return lastCompletedMarketSession(now, marketcal.MarketUSOptions)
}

func lastCompletedMarketSession(now time.Time, market marketcal.Market) (string, marketcal.Session, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	current, err := cal.SessionAt(market, now)
	if err != nil || current.State == marketcal.StateUnknown {
		return "", current, false
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return "", current, false
	}
	local := now.In(ny)
	for offset := range 10 {
		day := local.AddDate(0, 0, -offset)
		at := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, ny)
		session, sessionErr := cal.SessionAt(market, at)
		if sessionErr != nil || session.State == marketcal.StateUnknown {
			return "", current, false
		}
		if (session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose) &&
			!session.Close.IsZero() && !session.Close.After(now) {
			return session.Date, current, true
		}
	}
	return "", current, false
}
