package ibkr

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TWS reports settled cash for a margin account only as a dated schedule per
// native currency, on the full account-value feed (reqAccountUpdates, and
// reqAccountUpdatesMulti with ledgerAndNLV=false); the lightweight ledger feed
// and reqAccountSummary carry neither it nor a plain SettledCash. The value
// lists each settlement date with the currency's settled balance on that date,
// "20261002:1234.56;20261005:2345.67": the first point is today's settled cash
// and the last is the balance once every pending trade has settled, which is
// the trade-date cash. The "-S" twin is the securities segment's schedule.
const (
	settledCashScheduleKey        = "SettledCashByDate"
	settledCashScheduleSegmentKey = "SettledCashByDate-S"
	// maxSettledCashSchedulePoints bounds one parsed schedule. Settlement
	// cycles span a few business days, so a longer list is not a schedule.
	maxSettledCashSchedulePoints = 16
	// maxSettledCashScheduleCells bounds the per-subscription receipt.
	maxSettledCashScheduleCells = 32
)

var errSettledCashScheduleInvalid = errors.New("invalid settled-cash schedule")

// SettledCashPoint is one dated settled balance in the schedule's currency.
// Date is the broker's settlement date at UTC midnight.
type SettledCashPoint struct {
	Date   time.Time
	Amount float64
}

// SettledCashSchedule is one native currency's settled-cash schedule from the
// bound account stream. Points come from the account total; SegmentPoints from
// the securities-segment twin when TWS sent one. Status is observed or
// invalid; an invalid schedule keeps no points and names why in Reason.
type SettledCashSchedule struct {
	Currency      string
	Points        []SettledCashPoint
	SegmentPoints []SettledCashPoint
	// ReceivedAt is the oldest receipt among included total/segment components.
	// A missing component timestamp remains unknown rather than being refreshed
	// by another component's newer callback.
	ReceivedAt time.Time
	Status     string
	Reason     string
}

// SettledCashScheduleCapture is the schedule receipt of the already-running
// account subscription. StreamStatus uses AccountStreamObservation's status
// vocabulary; only initial_complete describes a fully downloaded stream.
type SettledCashScheduleCapture struct {
	StreamStatus string
	Truncated    bool
	Schedules    map[string]SettledCashSchedule
}

type settledCashScheduleCell struct {
	value      string
	receivedAt time.Time
}

// parseSettledCashSchedule reads one SettledCashByDate value strictly: every
// point is an exact YYYYMMDD calendar date and a finite amount other than
// IBKR's unset sentinel, dates strictly ascend, and nothing else is accepted.
func parseSettledCashSchedule(value string) ([]SettledCashPoint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("%w: empty", errSettledCashScheduleInvalid)
	}
	var points []SettledCashPoint
	for part := range strings.SplitSeq(value, ";") {
		if len(points) == maxSettledCashSchedulePoints {
			return nil, fmt.Errorf("%w: more than %d points", errSettledCashScheduleInvalid, maxSettledCashSchedulePoints)
		}
		dateText, amountText, ok := strings.Cut(part, ":")
		if !ok || len(dateText) != 8 {
			return nil, fmt.Errorf("%w: a point is not date:amount", errSettledCashScheduleInvalid)
		}
		date, err := time.Parse("20060102", dateText)
		if err != nil || date.Format("20060102") != dateText {
			return nil, fmt.Errorf("%w: a date is not YYYYMMDD", errSettledCashScheduleInvalid)
		}
		amount, err := strconv.ParseFloat(amountText, 64)
		if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || math.Abs(amount) == math.MaxFloat64 {
			return nil, fmt.Errorf("%w: an amount is not a finite number", errSettledCashScheduleInvalid)
		}
		if n := len(points); n > 0 && !date.After(points[n-1].Date) {
			return nil, fmt.Errorf("%w: dates do not strictly ascend", errSettledCashScheduleInvalid)
		}
		points = append(points, SettledCashPoint{Date: date, Amount: amount})
	}
	return points, nil
}

// observeSettledCashScheduleLocked keeps the latest raw schedule per key and
// native currency. The caller holds accountMu and has bound the row to the
// subscription's account and epoch. A BASE row is the base-currency total of
// every currency and is never kept.
func (r *accountStreamReceipt) observeSettledCashScheduleLocked(field, value, currency string, at time.Time) {
	if field != settledCashScheduleKey && field != settledCashScheduleSegmentKey || !concreteAccountSummaryLedgerCurrency(currency) {
		return
	}
	id := field + "\x00" + currency
	if _, exists := r.schedules[id]; !exists && len(r.schedules) >= maxSettledCashScheduleCells {
		r.schedulesTruncated = true
		return
	}
	if r.schedules == nil {
		r.schedules = map[string]settledCashScheduleCell{}
	}
	r.schedules[id] = settledCashScheduleCell{value: value, receivedAt: at}
}

func (r *accountStreamReceipt) settledCashSchedules() map[string]SettledCashSchedule {
	out := map[string]SettledCashSchedule{}
	for _, id := range slices.Sorted(maps.Keys(r.schedules)) {
		field, currency, _ := strings.Cut(id, "\x00")
		cell := r.schedules[id]
		s, exists := out[currency]
		s.Currency = currency
		if s.Status == "" {
			s.Status = "observed"
		}
		if !exists || cell.receivedAt.Before(s.ReceivedAt) {
			s.ReceivedAt = cell.receivedAt
		}
		points, err := parseSettledCashSchedule(cell.value)
		switch {
		case s.Status != "observed":
		case err != nil:
			s.Points, s.SegmentPoints, s.Status, s.Reason = nil, nil, "invalid", field+": "+err.Error()
		case field == settledCashScheduleKey:
			s.Points = points
		default:
			s.SegmentPoints = points
		}
		out[currency] = s
	}
	for currency, s := range out {
		if s.Status == "observed" && len(s.Points) == 0 {
			s.SegmentPoints, s.Status, s.Reason = nil, "invalid", "only the securities-segment schedule arrived"
			out[currency] = s
		}
	}
	return out
}

// CaptureSettledCashSchedulesForSession reads the schedules the already-running
// account subscription delivered within the caller's original binding, under
// the same barriers and scope checks as CaptureAccountStreamObservationForSession.
// It sends no request. Unlike that diagnostic it carries amounts, so it feeds
// the daemon's cash admission and is never logged.
func (c *Connector) CaptureSettledCashSchedulesForSession(binding ConnectorSessionBinding, expected string) *SettledCashScheduleCapture {
	if c == nil || !accountCodeConcrete(expected) || binding.connector != c || binding.connection == nil {
		return nil
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	binding.connection.inboundEpochMu.Lock()
	defer binding.connection.inboundEpochMu.Unlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(binding) {
		return nil
	}
	conn := binding.connection
	conn.portfolioHealthMu.RLock()
	health := conn.portfolioHealth
	conn.portfolioHealthMu.RUnlock()
	conn.accountMu.RLock()
	defer conn.accountMu.RUnlock()
	r := conn.accountStreamReceipt
	out := &SettledCashScheduleCapture{StreamStatus: "no_subscription", Schedules: map[string]SettledCashSchedule{}}
	if r.requestedAt.IsZero() {
		return out
	}
	if !accountStreamReceiptBound(r, health, binding, expected) {
		out.StreamStatus = "scope_or_generation_changed"
		return out
	}
	out.StreamStatus = accountStreamReceiptStatus(health, r.accountReady)
	out.Truncated = r.schedulesTruncated
	out.Schedules = r.settledCashSchedules()
	return out
}
