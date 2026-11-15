package chainrec

import (
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
)

// Slot is one session's run: it starts SnapshotLead before the regular US
// equity close (13:00 on an early-close day) and stops requesting at the
// close.
type Slot struct {
	SessionDate string
	Close       time.Time
	ScheduledAt time.Time
	Deadline    time.Time
	EarlyClose  bool
}

func slotFor(s marketcal.Session) (Slot, bool) {
	if (s.State != marketcal.StateRegular && s.State != marketcal.StateEarlyClose) || s.Close.IsZero() {
		return Slot{}, false
	}
	return Slot{
		SessionDate: s.Date,
		Close:       s.Close,
		ScheduledAt: s.Close.Add(-SnapshotLead),
		Deadline:    s.Close,
		EarlyClose:  s.State == marketcal.StateEarlyClose,
	}, true
}

// DueSlot returns the session slot a run may start at now: a trading session
// (never a weekend, holiday or a date outside calendar coverage) whose start
// has passed and whose latest start has not.
func DueSlot(cal *marketcal.Calendar, now time.Time) (Slot, bool) {
	if cal == nil {
		cal = marketcal.New()
	}
	sess, err := cal.SessionAt(marketcal.MarketUSEquity, now)
	if err != nil {
		return Slot{}, false
	}
	slot, ok := slotFor(sess)
	if !ok || now.Before(slot.ScheduledAt) || !now.Before(slot.Close.Add(-LatestStartBeforeClose)) {
		return Slot{}, false
	}
	return slot, true
}

// NextSlot returns the next slot whose latest start is after now.
func NextSlot(cal *marketcal.Calendar, now time.Time) (Slot, bool) {
	if cal == nil {
		cal = marketcal.New()
	}
	res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now, Days: 10})
	if err != nil {
		return Slot{}, false
	}
	for _, sess := range res.Sessions {
		if sess.State == marketcal.StateUnknown {
			return Slot{}, false
		}
		if slot, ok := slotFor(sess); ok && now.Before(slot.Close.Add(-LatestStartBeforeClose)) {
			return slot, true
		}
	}
	return Slot{}, false
}
