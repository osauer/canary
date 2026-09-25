package daemon

import (
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimePublicationPending is an official daily close its publisher has not
// released although Canary read the publisher's file: an expected upstream
// delay, not a fault in Canary, the broker or the network. OfficialDate is the
// newest close the file holds; Since is when that gap began to hold the row
// out of its schedule.
type regimePublicationPending struct {
	Publisher    string
	Series       string
	OfficialDate string
	Since        time.Time
	// Verdict names what the gap did to the row: "unverified" for the VIX3M
	// cross-check, "overdue" for VVIX's age budget.
	Verdict string
}

// receiving is the data-health state text for the gap.
func (p regimePublicationPending) receiving() string {
	return fmt.Sprintf("%s publication pending · last official close %s · %s since %s", p.Publisher, p.OfficialDate, p.Verdict, p.Since.UTC().Format("2006-01-02 15:04Z"))
}

// vix3mPublicationPending reports an unverified VIX3M cross-check caused by
// Cboe's file lagging, not by a failed read: the row carries the file's date
// only when the read succeeded, and that date is more than one session behind
// the last completed publication window.
func vix3mPublicationPending(row rpc.RegimeVIXTerm, now time.Time) (regimePublicationPending, bool) {
	if row.VIX3MCrossCheck != rpc.VIX3MCrossCheckUnverified || row.VIX3MOfficialDate == "" {
		return regimePublicationPending{}, false
	}
	since, ok := vix3mUnverifiedSince(row.VIX3MOfficialDate, now)
	if !ok || since.After(now) {
		return regimePublicationPending{}, false
	}
	return regimePublicationPending{Publisher: "Cboe", Series: "VIX3M", OfficialDate: row.VIX3MOfficialDate, Since: since, Verdict: "unverified"}, true
}

// vix3mUnverifiedSince is when a VIX3M file ending at officialDate stopped
// vouching for the off-window leg: the end of the publication window of the
// second options session after that date, from which the newest close is more
// than one session behind (see resolveVIX3MOffWindow).
func vix3mUnverifiedSince(officialDate string, now time.Time) (time.Time, bool) {
	session, ok := optionsSessionAfter(officialDate, 2, now)
	if !ok {
		return time.Time{}, false
	}
	_, end := vix3mWindow(session)
	return end, true
}

// vixTermPublicationPending reports a VIX term row held overdue by Cboe's
// VIX3M publication gap alone: vouched for, it would read not_due. A row
// overdue for any other reason as well does not name the gap as its cause.
func vixTermPublicationPending(res *rpc.RegimeSnapshotResult, nowNY time.Time) (regimePublicationPending, bool) {
	if res == nil {
		return regimePublicationPending{}, false
	}
	pending, ok := vix3mPublicationPending(res.VIXTermStructure, nowNY)
	if !ok || vixTermCadenceClass(res, nowNY) != rpc.RegimeFreshnessOverdue {
		return regimePublicationPending{}, false
	}
	vouched := *res
	vouched.VIXTermStructure.VIX3MCrossCheck = rpc.VIX3MCrossCheckPendingPublication
	if vixTermCadenceClass(&vouched, nowNY) != rpc.RegimeFreshnessNotDue {
		return regimePublicationPending{}, false
	}
	return pending, true
}

// volOfVolPublicationPending reports a VVIX row held overdue by Cboe's file
// lagging: the read succeeded (status ok, or stale once a week old) and
// carries a valid close, which is simply older than its budget allows.
func volOfVolPublicationPending(res *rpc.RegimeSnapshotResult, nowNY time.Time) (regimePublicationPending, bool) {
	if res == nil {
		return regimePublicationPending{}, false
	}
	row := res.VolOfVol
	if row.Status != rpc.RegimeStatusOK && row.Status != rpc.RegimeStatusStale || row.Last == nil || *row.Last <= 0 || math.IsNaN(*row.Last) || math.IsInf(*row.Last, 0) {
		return regimePublicationPending{}, false
	}
	date, err := time.Parse("2006-01-02", row.AsOfDate)
	if err != nil || date.After(nowNY) || volOfVolCadenceClass(res, nowNY) != rpc.RegimeFreshnessOverdue {
		return regimePublicationPending{}, false
	}
	// Overdue once past the age budget and once the next session completed,
	// which otherwise keeps a holiday-weekend close not_due.
	since := date.Add(volOfVolMaxAgeDays * 24 * time.Hour)
	if next, ok := optionsSessionAfter(row.AsOfDate, 1, nowNY); ok && next.Close.After(since) {
		since = next.Close
	}
	if since.After(nowNY) {
		return regimePublicationPending{}, false
	}
	return regimePublicationPending{Publisher: "Cboe", Series: "VVIX", OfficialDate: row.AsOfDate, Since: since, Verdict: "overdue"}, true
}

// optionsSessionAfter is the nth regular US options session after date.
func optionsSessionAfter(date string, n int, now time.Time) (marketcal.Session, bool) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return marketcal.Session{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", date, ny)
	if err != nil {
		return marketcal.Session{}, false
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	for offset := 1; offset <= 14; offset++ {
		at := time.Date(day.Year(), day.Month(), day.Day()+offset, 12, 0, 0, 0, ny)
		session, err := cal.SessionAt(marketcal.MarketUSOptions, at)
		if err != nil || session.State == marketcal.StateUnknown {
			return marketcal.Session{}, false
		}
		if (session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose) && !session.Close.IsZero() {
			if n--; n == 0 {
				return session, true
			}
		}
	}
	return marketcal.Session{}, false
}
