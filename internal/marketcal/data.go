package marketcal

type hm struct {
	h int
	m int
}

type dayOverride struct {
	reason string
	close  hm
}

type sessionBreak struct {
	start hm
	end   hm
}

type calendarSpec struct {
	market        Market
	label         string
	timezone      string
	open          hm
	close         hm
	breaks        []sessionBreak
	coverageStart string
	coverageEnd   string
	source        string
	sourceURL     string
	notes         string
	holidays      map[string]string
	earlyCloses   map[string]dayOverride
}

var specs = map[Market]calendarSpec{
	MarketUSEquity: {
		market:        MarketUSEquity,
		label:         "US equities",
		timezone:      "America/New_York",
		open:          hm{h: 9, m: 30},
		close:         hm{h: 16, m: 0},
		coverageStart: "2026-01-01",
		coverageEnd:   "2028-12-31",
		source:        "official_exchange_calendar",
		sourceURL:     "https://www.nyse.com/markets/hours-calendars",
		notes:         "Official NYSE/Nasdaq cash-equity holidays and early closes; other U.S. products may differ.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day",
			"2026-01-19": "Martin Luther King Jr. Day",
			"2026-02-16": "Washington's Birthday",
			"2026-04-03": "Good Friday",
			"2026-05-25": "Memorial Day",
			"2026-06-19": "Juneteenth National Independence Day",
			"2026-07-03": "Independence Day observed",
			"2026-09-07": "Labor Day",
			"2026-11-26": "Thanksgiving Day",
			"2026-12-25": "Christmas Day",
			"2027-01-01": "New Year's Day",
			"2027-01-18": "Martin Luther King Jr. Day",
			"2027-02-15": "Washington's Birthday",
			"2027-03-26": "Good Friday",
			"2027-05-31": "Memorial Day",
			"2027-06-18": "Juneteenth National Independence Day observed",
			"2027-07-05": "Independence Day observed",
			"2027-09-06": "Labor Day",
			"2027-11-25": "Thanksgiving Day",
			"2027-12-24": "Christmas Day observed",
			"2028-01-17": "Martin Luther King Jr. Day",
			"2028-02-21": "Washington's Birthday",
			"2028-04-14": "Good Friday",
			"2028-05-29": "Memorial Day",
			"2028-06-19": "Juneteenth National Independence Day",
			"2028-07-04": "Independence Day",
			"2028-09-04": "Labor Day",
			"2028-11-23": "Thanksgiving Day",
			"2028-12-25": "Christmas Day",
		},
		earlyCloses: map[string]dayOverride{
			"2026-11-27": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
			"2026-12-24": {reason: "Christmas early close", close: hm{h: 13, m: 0}},
			"2027-11-26": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
			"2028-07-03": {reason: "Independence Day early close", close: hm{h: 13, m: 0}},
			"2028-11-24": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
		},
	},
	MarketUSOptions: {
		market:        MarketUSOptions,
		label:         "US listed options",
		timezone:      "America/New_York",
		open:          hm{h: 9, m: 30},
		close:         hm{h: 16, m: 15},
		coverageStart: "2026-01-01",
		coverageEnd:   "2028-12-31",
		source:        "official_exchange_calendar",
		sourceURL:     "https://www.cboe.com/about/hours/us-options",
		notes:         "Models the regular U.S. listed-options session; SPX/VIX global hours, curb trading, and per-class exceptions are not modeled in v1.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day",
			"2026-01-19": "Martin Luther King Jr. Day",
			"2026-02-16": "Presidents' Day",
			"2026-04-03": "Good Friday",
			"2026-05-25": "Memorial Day",
			"2026-06-19": "Juneteenth Holiday",
			"2026-07-03": "Independence Day observed",
			"2026-09-07": "Labor Day",
			"2026-11-26": "Thanksgiving Day",
			"2026-12-25": "Christmas Day",
			"2027-01-01": "New Year's Day",
			"2027-01-18": "Martin Luther King Jr. Day",
			"2027-02-15": "Presidents' Day",
			"2027-03-26": "Good Friday",
			"2027-05-31": "Memorial Day",
			"2027-06-18": "Juneteenth Holiday observed",
			"2027-07-05": "Independence Day observed",
			"2027-09-06": "Labor Day",
			"2027-11-25": "Thanksgiving Day",
			"2027-12-24": "Christmas Day observed",
			"2028-01-17": "Martin Luther King Jr. Day",
			"2028-02-21": "Presidents' Day",
			"2028-04-14": "Good Friday",
			"2028-05-29": "Memorial Day",
			"2028-06-19": "Juneteenth Holiday",
			"2028-07-04": "Independence Day",
			"2028-09-04": "Labor Day",
			"2028-11-23": "Thanksgiving Day",
			"2028-12-25": "Christmas Day",
		},
		earlyCloses: map[string]dayOverride{
			"2026-11-27": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
			"2026-12-24": {reason: "Christmas early close", close: hm{h: 13, m: 0}},
			"2027-11-26": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
			"2028-07-03": {reason: "Independence Day early close", close: hm{h: 13, m: 0}},
			"2028-11-24": {reason: "Thanksgiving early close", close: hm{h: 13, m: 0}},
		},
	},
	MarketDEXetra: {
		market:        MarketDEXetra,
		label:         "Xetra",
		timezone:      "Europe/Berlin",
		open:          hm{h: 9, m: 0},
		close:         hm{h: 17, m: 30},
		coverageStart: "2026-01-01",
		coverageEnd:   "2028-12-31",
		source:        "official_exchange_calendar",
		sourceURL:     "https://www.cashmarket.deutsche-boerse.com/cash-en/trading/trading-calendar-and-trading-hours",
		notes:         "Official Deutsche Boerse Xetra cash-equity calendar; Frankfurt floor trading and Eurex derivatives are not modeled in v1.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day",
			"2026-04-03": "Good Friday",
			"2026-04-06": "Easter Monday",
			"2026-05-01": "Labour Day",
			"2026-12-24": "Christmas Eve",
			"2026-12-25": "Christmas Day",
			"2026-12-31": "New Year's Eve",
			"2027-01-01": "New Year's Day",
			"2027-03-26": "Good Friday",
			"2027-03-29": "Easter Monday",
			"2027-12-24": "Christmas Eve",
			"2027-12-31": "New Year's Eve",
			"2028-04-14": "Good Friday",
			"2028-04-17": "Easter Monday",
			"2028-05-01": "Labour Day",
			"2028-12-25": "Christmas Day",
			"2028-12-26": "Boxing Day",
		},
		earlyCloses: map[string]dayOverride{},
	},
}
