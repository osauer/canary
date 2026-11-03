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
	// JPX publishes the next year's market holidays once the National
	// Astronomical Observatory announces the equinox dates each February; on
	// 2026-11-03 its page (updated 2026-02-06) listed 2026 and 2027 only, so
	// 2028 waits for that publication.
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
	// 2027 checked on 2026-11-03 against SEHK circular CT/077/26 of
	// 2026-06-01, the securities market holiday schedule for 2027 (sourceURL);
	// 2026 follows circular CT/075/25:
	// https://www.hkex.com.hk/-/media/HKEX-Market/Services/Circulars-and-Notices/Participant-and-Members-Circulars/SEHK/2025/ce_SEHK_CT_075_2025.pdf.
	// Holidays on a Saturday (Labour Day and Christmas Day 2027) do not move.
	specs[MarketHKHKEX] = calendarSpec{
		market: MarketHKHKEX, label: "Hong Kong cash equities", timezone: "Asia/Hong_Kong",
		open: hm{9, 30}, close: hm{16, 0}, breaks: []sessionBreak{{hm{12, 0}, hm{13, 0}}},
		coverageStart: "2026-01-01", coverageEnd: "2027-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.hkex.com.hk/Services/Circulars-and-Notices/Participant-and-Members-Circulars/SEHK/2026/SEHK_CT_077_2026Jun01162846?sc_lang=en",
		notes:     "Ordinary cash-equity continuous sessions only. Closing auctions (random end), extended-morning eligible products, Stock Connect and derivatives are excluded. Hours: https://www.hkex.com.hk/Services/Trading-hours-and-Severe-Weather-Arrangements/Trading-Hours/Securities-Market.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-02-17": "Lunar New Year", "2026-02-18": "Lunar New Year",
			"2026-02-19": "Lunar New Year", "2026-04-03": "Good Friday", "2026-04-06": "Ching Ming observed",
			"2026-04-07": "Easter Monday observed", "2026-05-01": "Labour Day", "2026-05-25": "Buddha's Birthday observed",
			"2026-06-19": "Tuen Ng Festival", "2026-07-01": "Establishment Day", "2026-10-01": "National Day",
			"2026-10-19": "Chung Yeung observed", "2026-12-25": "Christmas Day",
			"2027-01-01": "New Year's Day", "2027-02-08": "Lunar New Year", "2027-02-09": "Lunar New Year observed",
			"2027-03-26": "Good Friday", "2027-03-29": "Easter Monday", "2027-04-05": "Ching Ming Festival",
			"2027-05-13": "Buddha's Birthday", "2027-06-09": "Tuen Ng Festival", "2027-07-01": "Establishment Day",
			"2027-09-16": "Day after Mid-Autumn Festival", "2027-10-01": "National Day", "2027-10-08": "Chung Yeung Festival",
			"2027-12-27": "First weekday after Christmas",
		},
		earlyCloses: map[string]dayOverride{
			"2026-02-16": {"Lunar New Year early close", hm{12, 0}},
			"2026-12-24": {"Christmas early close", hm{12, 0}},
			"2026-12-31": {"New Year early close", hm{12, 0}},
			"2027-02-05": {"Lunar New Year early close", hm{12, 0}},
			"2027-12-24": {"Christmas early close", hm{12, 0}},
			"2027-12-31": {"New Year early close", hm{12, 0}},
		},
	}
	// Checked against the linked publications on 2026-11-03. SGX closes on
	// Singapore's gazetted public holidays (a Sunday holiday moves to the
	// Monday; a Saturday one does not move) and trades the morning only on the
	// eves of Chinese New Year, Christmas and New Year when those fall on a
	// trading day. 2027's Hari Raya dates follow the published list and can
	// move with the moon sighting.
	specs[MarketSGSGX] = calendarSpec{
		market: MarketSGSGX, label: "Singapore securities", timezone: "Asia/Singapore",
		open: hm{9, 0}, close: hm{17, 0}, breaks: []sessionBreak{{hm{12, 0}, hm{13, 0}}},
		coverageStart: "2026-01-01", coverageEnd: "2027-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.mom.gov.sg/employment-practices/public-holidays",
		notes:     "SGX securities market continuous sessions only; the pre-open, closing routine and trade-at-close phases are excluded. Holidays: Singapore's gazetted public holidays as SGX observes them. Half days end at 12:00 with no afternoon session. Hours: https://www.sgx.com/securities/trading.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-02-17": "Chinese New Year", "2026-02-18": "Chinese New Year",
			"2026-04-03": "Good Friday", "2026-05-01": "Labour Day", "2026-05-27": "Hari Raya Haji",
			"2026-06-01": "Vesak Day observed", "2026-08-10": "National Day observed", "2026-11-09": "Deepavali observed",
			"2026-12-25": "Christmas Day",
			"2027-01-01": "New Year's Day", "2027-02-08": "Chinese New Year observed", "2027-03-10": "Hari Raya Puasa",
			"2027-03-26": "Good Friday", "2027-05-17": "Hari Raya Haji", "2027-05-20": "Vesak Day",
			"2027-08-09": "National Day", "2027-10-28": "Deepavali",
		},
		earlyCloses: map[string]dayOverride{
			"2026-02-16": {"Chinese New Year eve half day", hm{12, 0}},
			"2026-12-24": {"Christmas eve half day", hm{12, 0}},
			"2026-12-31": {"New Year eve half day", hm{12, 0}},
			"2027-02-05": {"Chinese New Year eve half day", hm{12, 0}},
			"2027-12-24": {"Christmas eve half day", hm{12, 0}},
			"2027-12-31": {"New Year eve half day", hm{12, 0}},
		},
	}
	// Checked against the ASX cash-market trading calendar on 2026-11-03.
	// ANZAC Day on a weekend is not substituted (2026-04-25 is a Saturday;
	// the 2027-04-26 substitute is a normal trading day).
	specs[MarketAUASX] = calendarSpec{
		market: MarketAUASX, label: "ASX cash equities", timezone: "Australia/Sydney",
		open: hm{10, 0}, close: hm{16, 0},
		coverageStart: "2026-01-01", coverageEnd: "2027-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.asx.com.au/markets/market-resources/trading-hours-calendar/cash-market-trading-hours/trading-calendar",
		notes:     "ASX normal trading only. The staggered open (10:00 to 10:09 by security group), the pre-CSPA phase and the closing single price auction from 16:10 are excluded. Early closes end normal trading at 14:10. Hours: https://www.asx.com.au/markets/market-resources/trading-hours-calendar/cash-market-trading-hours.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-01-26": "Australia Day", "2026-04-03": "Good Friday",
			"2026-04-06": "Easter Monday", "2026-06-08": "King's Birthday", "2026-12-25": "Christmas Day",
			"2026-12-28": "Boxing Day observed",
			"2027-01-01": "New Year's Day", "2027-01-26": "Australia Day", "2027-03-26": "Good Friday",
			"2027-03-29": "Easter Monday", "2027-06-14": "King's Birthday", "2027-12-27": "Christmas Day observed",
			"2027-12-28": "Boxing Day observed",
		},
		earlyCloses: map[string]dayOverride{
			"2026-12-24": {"Christmas early close", hm{14, 10}}, "2026-12-31": {"New Year early close", hm{14, 10}},
			"2027-12-24": {"Christmas early close", hm{14, 10}}, "2027-12-31": {"New Year early close", hm{14, 10}},
		},
	}
	// Checked against the Shanghai Stock Exchange notice on 2026-11-03
	// (上证公告〔2025〕45号; Shenzhen follows the same State Council
	// arrangement). Make-up working Saturdays and Sundays stay closed. The
	// 2027 arrangement is published late in 2026; coverage ends with 2026
	// until it is embedded.
	specs[MarketCNAShares] = calendarSpec{
		market: MarketCNAShares, label: "China A-shares (SSE/SZSE)", timezone: "Asia/Shanghai",
		open: hm{9, 30}, close: hm{15, 0}, breaks: []sessionBreak{{hm{11, 30}, hm{13, 0}}},
		coverageStart: "2026-01-01", coverageEnd: "2026-12-31",
		source:    "official_exchange_calendar",
		sourceURL: "https://www.sse.com.cn/disclosure/announcement/general/c/c_20251222_10802507.shtml",
		notes:     "Shanghai and Shenzhen A-share sessions; the afternoon window ends with the 14:57 to 15:00 closing call auction. Northbound Stock Connect, which is how IBKR routes A-shares (SEHKNTL, SEHKSZSE), trades only on days both this calendar and Hong Kong are open and skips some settlement-constrained days; consult the HKEX Stock Connect calendar before relying on a date. Hours: https://english.sse.com.cn/start/trading/schedule/.",
		holidays: map[string]string{
			"2026-01-01": "New Year's Day", "2026-01-02": "New Year's Day holiday",
			"2026-02-16": "Spring Festival", "2026-02-17": "Spring Festival", "2026-02-18": "Spring Festival",
			"2026-02-19": "Spring Festival", "2026-02-20": "Spring Festival", "2026-02-23": "Spring Festival",
			"2026-04-06": "Qingming Festival", "2026-05-01": "Labour Day", "2026-05-04": "Labour Day holiday",
			"2026-05-05": "Labour Day holiday", "2026-06-19": "Dragon Boat Festival", "2026-09-25": "Mid-Autumn Festival",
			"2026-10-01": "National Day", "2026-10-02": "National Day holiday", "2026-10-05": "National Day holiday",
			"2026-10-06": "National Day holiday", "2026-10-07": "National Day holiday",
		},
	}
}
