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

// cashSweepEUROneDayFrom is the first trade date on which EU venue trades
// settle T+1: Regulation (EU) 2025/2075 amends CSDR from 11 October 2027.
// Reviewed 2026-10-02 against:
// https://www.amf-france.org/en/news-publications/depth/settlement-cycle-t1
// The route decides only the bill-order calendar check and the redemption
// maturity check; settled cash itself comes from the broker's schedule.
var cashSweepEUROneDayFrom = time.Date(2027, 10, 11, 0, 0, 0, 0, time.UTC)

// cashSweepDefaultRoute is Canary's maintained settlement route for a
// currency's bills on one trade day (the payment jurisdiction's date): the
// broker exchange the sweep's bill orders use and the market's settlement lag
// in payment business days. US Treasury bills settle T+1; EUR government bills
// settle T+2 under CSDR and T+1 for trades from 11 October 2027. Like the
// payment calendars it is kept current by Canary releases, so no owner file
// needs a date for it. A currency without a payment calendar has no route.
func cashSweepDefaultRoute(currency string, day time.Time) (exchange string, days int, ok bool) {
	switch currency {
	case "USD":
		return "SMART", 1, true
	case "EUR":
		if day.Before(cashSweepEUROneDayFrom) {
			return "SMART", 2, true
		}
		return "SMART", 1, true
	}
	return "", 0, false
}

// cashSweepRoute is the settlement route in force for one currency's bill
// orders: the owner's settlement_exchange and settlement_days where the
// currency table writes them, else Canary's maintained default.
// settlement_valid_through is only ever the owner's optional end date.
type cashSweepRoute struct {
	exchange     string
	days         *int
	validThrough string
	source       string
}

func cashSweepRouteFor(currency string, cfg protectionCashSweepCurrency, at time.Time) cashSweepRoute {
	r := cashSweepRoute{exchange: cfg.SettlementExchange, days: cfg.SettlementDays, validThrough: string(cfg.SettlementValidThrough), source: rpc.CashSweepSettlementSourcePolicy}
	if r.exchange != "" && r.days != nil || at.IsZero() {
		return r
	}
	day, reason := cashSweepTradeDay(currency, at)
	if reason != "" {
		return r
	}
	exchange, days, ok := cashSweepDefaultRoute(currency, day)
	if !ok {
		return r
	}
	if r.exchange == "" {
		r.exchange = exchange
	}
	if r.days == nil {
		r.days, r.source = new(days), rpc.CashSweepSettlementSourceDefault
	}
	return r
}

// cashSweepTradeDay is at's date in the currency's payment jurisdiction.
func cashSweepTradeDay(currency string, at time.Time) (time.Time, string) {
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
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC), ""
}

// cashSweepSettlementDate counts a route's lag on the payment calendar. The
// lag and exchange are the route's, never inferred from the calendar; an empty
// validThrough means no owner end date.
func cashSweepSettlementDate(currency, exchange string, days *int, validThrough string, at time.Time) (time.Time, string) {
	if at.IsZero() || days == nil || *days < 1 || *days > 5 || exchange == "" {
		return time.Time{}, "the route's settlement lag or exchange is unavailable"
	}
	if validThrough != "" {
		valid, err := time.Parse(time.DateOnly, validThrough)
		if err != nil || cashSweepDay(at).After(valid) {
			return time.Time{}, "the policy's settlement_valid_through has passed"
		}
	}
	day, reason := cashSweepTradeDay(currency, at)
	if reason != "" {
		return time.Time{}, reason
	}
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
		reason = "the bill order's exchange does not match the settlement route"
	}
	if reason == "" {
		return nil
	}
	return []rpc.TradingBlocker{{Code: "cash_sweep_settlement_calendar_unknown", Message: fmt.Sprintf("%s bill route is held: %s.", s.Currency, reason),
		Action: "USD and EUR bills on SMART follow Canary's maintained route; correct or remove this currency's settlement_* override, or update Canary for a date or currency its calendars do not cover."}}
}
