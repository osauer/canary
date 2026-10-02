package rpc

import "time"

// SettledCashSchedule is TWS's settled-cash schedule for one native currency
// (account value SettledCashByDate) as the daemon judged it. Points are the
// currency's settled balance on each settlement date, ascending; the last is
// the balance once every pending trade has settled. Status admitted means the
// last point reconciled with the current trade-date cash, and Low, the lowest
// balance on the schedule, is the currency's settled cash: the most it can
// spend without a settled debit on any listed date. Held keeps it out and
// Reason says why.
type SettledCashSchedule struct {
	Status string   `json:"status"`
	Reason string   `json:"reason,omitempty"`
	Low    *float64 `json:"low,omitempty"`
	// Points come from the account total; SegmentPoints from the securities
	// segment's twin when TWS sent one. Low covers both.
	Points        []SettledCashPoint `json:"points,omitempty"`
	SegmentPoints []SettledCashPoint `json:"segment_points,omitempty"`
	// ReceivedAt is the local receipt of the latest schedule callback. TWS
	// resends a schedule only when it changes, so age alone is not staleness;
	// reconciliation with current trade-date cash is the freshness check.
	ReceivedAt time.Time `json:"received_at,omitzero"`
}

// SettledCashPoint is one dated settled balance. Date is YYYY-MM-DD.
type SettledCashPoint struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
}

// Settled-cash schedule statuses.
const (
	SettledCashScheduleAdmitted = "admitted"
	SettledCashScheduleHeld     = "held"
)
