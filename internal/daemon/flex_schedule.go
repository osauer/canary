package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
)

// Flex statement schedule: which completed New York date the daily broker
// check targets, and when the window for that check opens.
//
// IBKR publishes a securities statement for each US session day shortly
// after New York midnight. A weekend or exchange-holiday date carries no
// statement, and asking for one is not a safe no-op: since the query was
// re-created in late August 2026, every Sunday run that requested a range
// ending on the Saturday came back with the undocumented Flex code 1025,
// while Friday- and Sunday-ended ranges kept landing. The target therefore
// advances only across session days. A non-session day presents the same
// target as the previous check, so the scheduler reports it current without
// a broker call and names the next window instead.

const (
	flexScheduleZone  = "Europe/Berlin"
	flexReportingZone = "America/New_York"
	// IBKR says securities statements are available around midnight New
	// York. 06:30 Berlin is 00:30 there, or 01:30 during the weeks when
	// only one side observes daylight saving, so the date has always closed.
	flexMorningHour   = 6
	flexMorningMinute = 30
	// flexSessionLookbackDays bounds the walk back to the latest session
	// day. The longest closure the embedded calendar models is a holiday
	// beside a weekend, so ten days is generous.
	flexSessionLookbackDays = 10
)

// flexDailyWindow names the daily broker check at now: the latest completed
// statement date and the local morning window in which its first attempt
// may run. The check is evaluated every calendar day; the target only moves
// on session days, so an unchanged target means nothing new is due.
func flexDailyWindow(now time.Time) (targetDate, firstAttempt time.Time) {
	return latestCompletedFlexDate(now), flexMorningWindow(now.In(flexScheduleLocation()))
}

// latestCompletedFlexDate returns the most recent New York calendar date
// that has closed and carried a US equity session, as a UTC midnight date.
// IBKR documents its reporting window in Eastern time, so a Canary host must
// not derive this date from its own local calendar. A date the embedded
// calendar does not cover falls back to the plain completed date rather than
// blocking the check.
func latestCompletedFlexDate(now time.Time) time.Time {
	completed := completedFlexReportingDate(now)
	for offset := range flexSessionLookbackDays {
		day := completed.AddDate(0, 0, -offset)
		sessionDay, vouched := flexStatementDay(day)
		if !vouched {
			return completed
		}
		if sessionDay {
			return day
		}
	}
	return completed
}

// nextFlexDailyWindow returns when the check after target first runs: the
// morning window on the day after the next session day. It is zero when the
// embedded calendar cannot vouch for the days that follow target.
func nextFlexDailyWindow(target time.Time) time.Time {
	target = dateOnlyUTC(target)
	if target.IsZero() {
		return time.Time{}
	}
	for offset := range flexSessionLookbackDays {
		day := target.AddDate(0, 0, offset+1)
		sessionDay, vouched := flexStatementDay(day)
		if !vouched {
			return time.Time{}
		}
		if sessionDay {
			return flexMorningWindow(day.AddDate(0, 0, 1))
		}
	}
	return time.Time{}
}

// completedFlexReportingDate is the New York calendar date before now, as a
// UTC midnight date, whether or not it carried a session.
func completedFlexReportingDate(now time.Time) time.Time {
	completed := now.In(flexReportingLocation()).AddDate(0, 0, -1)
	return time.Date(completed.Year(), completed.Month(), completed.Day(), 0, 0, 0, 0, time.UTC)
}

// flexStatementDay reports whether IBKR publishes a securities statement for
// the New York calendar date, which is exactly on US equity session days
// including early closes. vouched is false when the date lies outside the
// embedded official calendar's coverage.
func flexStatementDay(date time.Time) (sessionDay, vouched bool) {
	noon := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, flexReportingLocation())
	session, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, noon)
	if err != nil {
		return false, false
	}
	switch session.State {
	case marketcal.StateRegular, marketcal.StateEarlyClose:
		return true, true
	case marketcal.StateClosed, marketcal.StateHoliday:
		return false, true
	default:
		return false, false
	}
}

// flexMorningWindow is the Berlin morning window on day's calendar date, as
// UTC. Only day's year, month, and day fields are read.
func flexMorningWindow(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), flexMorningHour, flexMorningMinute, 0, 0, flexScheduleLocation()).UTC()
}

func flexScheduleLocation() *time.Location {
	if location, err := time.LoadLocation(flexScheduleZone); err == nil {
		return location
	}
	return time.FixedZone("CET", 60*60)
}

func flexReportingLocation() *time.Location {
	if location, err := time.LoadLocation(flexReportingZone); err == nil {
		return location
	}
	return time.FixedZone("EST", -5*60*60)
}
