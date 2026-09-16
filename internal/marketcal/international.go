package marketcal

// These finite tables were checked against the linked exchange publications on
// 2026-09-14. Ordinary cash sessions only: auctions, special products and
// unscheduled halts are outside this calendar's contract.
func init() {
	specs[MarketUKLSE] = calendarSpec{
		market: MarketUKLSE, label: "London SETS", timezone: "Europe/London",
		open: hm{8, 0}, close: hm{16, 30},
		coverageStart: "2026-01-01", coverageEnd: "2028-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.londonstockexchange.com/trade/trading-access/business-days",
		notes:     "SETS ordinary cash session. Closing auctions and other trading services are excluded. England and Wales bank holidays apply; earlier 2026 dates verified against https://www.gov.uk/bank-holidays. Hours: Millennium Exchange business parameters.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-04-03": "Good Friday", "2026-04-06": "Easter Monday",
			"2026-05-04": "Early May Bank Holiday", "2026-05-25": "Spring Bank Holiday", "2026-08-31": "Summer Bank Holiday",
			"2026-12-25": "Christmas Day", "2026-12-28": "Boxing Day observed",
			"2027-01-01": "New Year's Day", "2027-03-26": "Good Friday", "2027-03-29": "Easter Monday",
			"2027-05-03": "Early May Bank Holiday", "2027-05-31": "Spring Bank Holiday", "2027-08-30": "Summer Bank Holiday",
			"2027-12-27": "Christmas Day observed", "2027-12-28": "Boxing Day observed",
			"2028-01-03": "New Year's Day observed", "2028-04-14": "Good Friday", "2028-04-17": "Easter Monday",
			"2028-05-01": "Early May Bank Holiday", "2028-05-29": "Spring Bank Holiday", "2028-08-28": "Summer Bank Holiday",
			"2028-12-25": "Christmas Day", "2028-12-26": "Boxing Day",
		},
		earlyCloses: map[string]dayOverride{
			"2026-12-24": {"Christmas early close", hm{12, 30}}, "2026-12-31": {"New Year early close", hm{12, 30}},
			"2027-12-24": {"Christmas early close", hm{12, 30}}, "2027-12-31": {"New Year early close", hm{12, 30}},
			"2028-12-22": {"Christmas early close", hm{12, 30}}, "2028-12-29": {"New Year early close", hm{12, 30}},
		},
	}
	specs[MarketJPTSE] = calendarSpec{
		market: MarketJPTSE, label: "Tokyo cash equities", timezone: "Asia/Tokyo",
		open: hm{9, 0}, close: hm{15, 30}, breaks: []sessionBreak{{hm{11, 30}, hm{12, 30}}},
		coverageStart: "2026-01-01", coverageEnd: "2027-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.jpx.co.jp/english/corporate/about-jpx/calendar/",
		notes:     "Tokyo cash equities; derivatives holiday trading is excluded. Scheduled lunch is not an open session. Hours: https://www.jpx.co.jp/english/equities/trading/domestic/01.html.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-01-02": "Market Holiday", "2026-01-03": "Market Holiday",
			"2026-01-12": "Coming of Age Day", "2026-02-11": "National Foundation Day", "2026-02-23": "Emperor's Birthday",
			"2026-03-20": "Vernal Equinox", "2026-04-29": "Showa Day", "2026-05-03": "Constitution Memorial Day",
			"2026-05-04": "Greenery Day", "2026-05-05": "Children's Day", "2026-05-06": "Constitution Memorial Day observed",
			"2026-07-20": "Marine Day", "2026-08-11": "Mountain Day", "2026-09-21": "Respect for the Aged Day",
			"2026-09-22": "National holiday", "2026-09-23": "Autumnal Equinox", "2026-10-12": "Sports Day",
			"2026-11-03": "Culture Day", "2026-11-23": "Labor Thanksgiving Day", "2026-12-31": "Market Holiday",
			"2027-01-01": "New Year's Day", "2027-01-02": "Market Holiday", "2027-01-03": "Market Holiday",
			"2027-01-11": "Coming of Age Day", "2027-02-11": "National Foundation Day", "2027-02-23": "Emperor's Birthday",
			"2027-03-21": "Vernal Equinox", "2027-03-22": "Vernal Equinox observed", "2027-04-29": "Showa Day",
			"2027-05-03": "Constitution Memorial Day", "2027-05-04": "Greenery Day", "2027-05-05": "Children's Day",
			"2027-07-19": "Marine Day", "2027-08-11": "Mountain Day", "2027-09-20": "Respect for the Aged Day",
			"2027-09-23": "Autumnal Equinox", "2027-10-11": "Sports Day", "2027-11-03": "Culture Day",
			"2027-11-23": "Labor Thanksgiving Day", "2027-12-31": "Market Holiday",
		},
	}
	specs[MarketHKHKEX] = calendarSpec{
		market: MarketHKHKEX, label: "Hong Kong cash equities", timezone: "Asia/Hong_Kong",
		open: hm{9, 30}, close: hm{16, 0}, breaks: []sessionBreak{{hm{12, 0}, hm{13, 0}}},
		coverageStart: "2026-01-01", coverageEnd: "2026-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.hkex.com.hk/-/media/HKEX-Market/Services/Circulars-and-Notices/Participant-and-Members-Circulars/SEHK/2025/ce_SEHK_CT_075_2025.pdf",
		notes:     "Ordinary cash-equity continuous sessions only. Closing auctions (random end), extended-morning eligible products, Stock Connect and derivatives are excluded. Hours: https://www.hkex.com.hk/Services/Trading-hours-and-Severe-Weather-Arrangements/Trading-Hours/Securities-Market.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-02-17": "Lunar New Year", "2026-02-18": "Lunar New Year",
			"2026-02-19": "Lunar New Year", "2026-04-03": "Good Friday", "2026-04-06": "Ching Ming observed",
			"2026-04-07": "Easter Monday observed", "2026-05-01": "Labour Day", "2026-05-25": "Buddha's Birthday observed",
			"2026-06-19": "Tuen Ng Festival", "2026-07-01": "Establishment Day", "2026-10-01": "National Day",
			"2026-10-19": "Chung Yeung observed", "2026-12-25": "Christmas Day",
		},
		earlyCloses: map[string]dayOverride{
			"2026-02-16": {"Lunar New Year early close", hm{12, 0}},
			"2026-12-24": {"Christmas early close", hm{12, 0}},
			"2026-12-31": {"New Year early close", hm{12, 0}},
		},
	}
}
