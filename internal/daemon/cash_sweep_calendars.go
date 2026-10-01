package daemon

import (
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Payment calendars are distinct from stock exchanges and bond trading hours.
// Reviewed 2026-10-01 against:
// https://www.frbservices.org/about/holiday-schedules/
// https://www.ecb.europa.eu/ecb/contacts/working-hours/html/index.en.html
// Coverage is deliberately bounded to published 2026–2028 dates. Federal
// Reserve Saturday holidays do NOT close the preceding Friday.
var cashSweepPaymentHolidays = map[string]map[int][]string{
	"USD": {
		2026: {"01-01", "01-19", "02-16", "05-25", "06-19", "09-07", "10-12", "11-11", "11-26", "12-25"},
		2027: {"01-01", "01-18", "02-15", "05-31", "07-05", "09-06", "10-11", "11-11", "11-25"},
		2028: {"01-17", "02-21", "05-29", "06-19", "07-04", "09-04", "10-09", "11-23", "12-25"},
	},
	"EUR": {
		2026: {"01-01", "04-03", "04-06", "05-01", "12-25", "12-26"},
		2027: {"01-01", "03-26", "03-29", "05-01", "12-25", "12-26"},
		2028: {"01-01", "04-14", "04-17", "05-01", "12-25", "12-26"},
	},
}

func cashSweepPaymentDay(currency string, day time.Time) (bool, bool) {
	holidays, known := cashSweepPaymentHolidays[currency][day.Year()]
	if !known || day.IsZero() {
		return false, false
	}
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !slices.Contains(holidays, day.Format("01-02")), true
}

// Lag and route are broker facts requiring dated owner commissioning, not
// inferred from the payment calendar. No T+1 weekday fallback is authoritative.
func cashSweepSettlementDate(currency, exchange string, days *int, validThrough string, at time.Time) (time.Time, string) {
	valid, err := time.Parse(time.DateOnly, validThrough)
	if at.IsZero() || days == nil || *days < 1 || *days > 5 || exchange == "" || err != nil || cashSweepDay(at).After(valid) {
		return time.Time{}, "the route's settlement lag, exchange or dated review is unavailable"
	}
	zone := ""
	switch currency {
	case "USD":
		zone = "America/New_York"
	case "EUR":
		zone = "Europe/Berlin"
	default:
		return time.Time{}, "no verified payment calendar for this currency"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, "the settlement timezone is unavailable"
	}
	local := at.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	if _, known := cashSweepPaymentDay(currency, day); !known {
		return time.Time{}, "the payment calendar is outside published coverage"
	}
	for left := *days; left > 0; {
		day = day.AddDate(0, 0, 1)
		open, known := cashSweepPaymentDay(currency, day)
		if !known {
			return time.Time{}, "the settlement date leaves published calendar coverage"
		}
		if open {
			left--
		}
	}
	return day, ""
}

func cashSweepCalendarBlockers(s *rpc.TradeProposalCashSweep, exchange string, at time.Time) []rpc.TradingBlocker {
	if s == nil || !cashSweepIsBill(s.Instrument) {
		return nil
	}
	_, reason := cashSweepSettlementDate(s.Currency, s.SettlementExchange, s.SettlementDays, s.SettlementValidThrough, at)
	if exchange != s.SettlementExchange {
		reason = "the exact broker exchange does not match the reviewed settlement route"
	}
	if reason == "" {
		return nil
	}
	return []rpc.TradingBlocker{{Code: "cash_sweep_settlement_calendar_unknown", Message: fmt.Sprintf("%s bill route is held: %s.", s.Currency, reason), Action: "Commission the exact route's settlement lag and review validity before activating it."}}
}
