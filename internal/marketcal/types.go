// Package marketcal queries embedded official exchange-session calendars
// without consulting broker contract metadata or the network.
//
// Coverage is finite and disclosed on every result. Dates outside the embedded
// range return StateUnknown rather than being inferred from weekdays.
package marketcal

import "time"

// Market is the supported exchange/session calendar identifier.
type Market string

const (
	// MarketUSEquity is the regular U.S. cash-equity / ETF session.
	MarketUSEquity Market = "us_equity"
	// MarketUSOptions is the regular U.S. listed-options session. It models the
	// regular Cboe-style session, not SPX/VIX
	// global/curb trading or per-class late-close exceptions.
	MarketUSOptions Market = "us_options"
	// MarketDEXetra is Deutsche Boerse Xetra's cash-equity session.
	MarketDEXetra Market = "de_xetra"
)

// AllMarkets returns every supported market calendar. Consumers that reason
// about "any supported market" (session-open unions, coverage checks) must
// iterate this instead of naming markets, so adding a calendar extends them
// automatically.
func AllMarkets() []Market {
	return []Market{MarketUSEquity, MarketUSOptions, MarketDEXetra}
}

// State classifies a market at an instant or a calendar date.
type State string

// Calendar states. StateUnknown is reserved for dates outside verified
// embedded coverage.
const (
	StateRegular    State = "regular"
	StateClosed     State = "closed"
	StateHoliday    State = "holiday"
	StateEarlyClose State = "early_close"
	StateUnknown    State = "unknown"
)

// Session is one market's trading-session context for a date or instant.
type Session struct {
	Market        Market
	Label         string
	Date          string
	Timezone      string
	State         State
	IsOpen        bool
	Reason        string
	Open          time.Time
	Close         time.Time
	NextOpen      *time.Time
	NextClose     *time.Time
	Source        string
	SourceURL     string
	CoverageStart string
	CoverageEnd   string
	Notes         string
}

// Query requests calendar context. Date and At are mutually complementary:
// At wins when non-zero; otherwise Date is interpreted in the market's local
// timezone at noon so the result describes that date without boundary noise.
type Query struct {
	Market Market
	Date   string
	At     time.Time
	Days   int
}

// Result is the calendar response: the current/day session plus a forward
// list of sessions when Days > 0.
type Result struct {
	Market        Market
	Label         string
	Timezone      string
	AsOf          time.Time
	CoverageStart string
	CoverageEnd   string
	Source        string
	SourceURL     string
	Session       Session
	Sessions      []Session
}
