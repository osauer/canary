package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func stockRow(symbol, venue, currency string, base float64) rpc.PositionView {
	return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeStock, PrimaryExch: venue, Currency: currency, Quantity: 100, MarketValue: base, MarketValueBase: new(base)}
}

// The brief's session follows the book: a Hong Kong or Singapore account
// reads its own exchange, a US-weighted book keeps the US session, and
// bonds, cash and an empty book never move it off the US default.
func TestBriefSessionMarketFollowsTheBooksVenue(t *testing.T) {
	hk := stockRow("700", "SEHK", "HKD", 120000)
	sg := stockRow("D05", "SGX", "SGD", 90000)
	us := stockRow("SPY", "ARCA", "USD", 50000)
	jpOption := rpc.PositionView{Symbol: "7203", SecType: rpc.SecTypeOption, Currency: "JPY", Quantity: 1, MarketValue: 300000}
	bill := rpc.PositionView{Symbol: "B 0 12/15/26", SecType: "BOND", Currency: "HKD", Quantity: 1000, MarketValue: 980000, MarketValueBase: new(980000.0)}
	for name, tc := range map[string]struct {
		pos  *rpc.PositionsResult
		want marketcal.Market
	}{
		"nil book":                 {nil, marketcal.MarketUSEquity},
		"empty book":               {&rpc.PositionsResult{}, marketcal.MarketUSEquity},
		"hong kong only":           {&rpc.PositionsResult{Stocks: []rpc.PositionView{hk}}, marketcal.MarketHKHKEX},
		"singapore outweighs hk":   {&rpc.PositionsResult{Stocks: []rpc.PositionView{hk, sg, sg}}, marketcal.MarketSGSGX},
		"us plurality keeps us":    {&rpc.PositionsResult{Stocks: []rpc.PositionView{us, us, us, hk}}, marketcal.MarketUSEquity},
		"tokyo option by currency": {&rpc.PositionsResult{Options: []rpc.PositionView{jpOption}}, marketcal.MarketJPTSE},
		"bills do not vote":        {&rpc.PositionsResult{Stocks: []rpc.PositionView{bill, us}}, marketcal.MarketUSEquity},
		"asx and a-shares":         {&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("BHP", "ASX", "AUD", 10), stockRow("600519", "SEHKNTL", "CNH", 20)}}, marketcal.MarketCNAShares},
		"toronto is not tokyo":     {&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("RY", "TSE", "CAD", 10)}}, marketcal.MarketUSEquity},
	} {
		t.Run(name, func(t *testing.T) {
			if got := briefSessionMarket(tc.pos); got != tc.want {
				t.Fatalf("briefSessionMarket = %s, want %s", got, tc.want)
			}
		})
	}
}

// At 10:00 in Hong Kong on a trading day the brief's session row reads the
// Hong Kong morning session as open, while the US calendar it used to consult
// reads closed; the same instant is the Singapore pre-open and the Tokyo
// morning window.
func TestBriefCalendarAtAnAsianMorning(t *testing.T) {
	hongKong, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 4, 10, 0, 0, 0, hongKong)
	s := &Server{}
	for _, tc := range []struct {
		pos  *rpc.PositionsResult
		open bool
		want marketcal.Market
	}{
		{&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("700", "SEHK", "HKD", 1)}}, true, marketcal.MarketHKHKEX},
		{&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("D05", "SGX", "SGD", 1)}}, true, marketcal.MarketSGSGX},
		{&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("7203", "TSEJ", "JPY", 1)}}, true, marketcal.MarketJPTSE},
		{&rpc.PositionsResult{Stocks: []rpc.PositionView{stockRow("SPY", "ARCA", "USD", 1)}}, false, marketcal.MarketUSEquity},
	} {
		market := briefSessionMarket(tc.pos)
		if market != tc.want {
			t.Fatalf("market = %s, want %s", market, tc.want)
		}
		cal, err := s.handleMarketCalendar(&rpc.Request{Params: briefJSON(rpc.MarketCalendarParams{Market: string(market), At: at, Days: 1})})
		if err != nil {
			t.Fatal(err)
		}
		section := composeBriefCalendar(cal, nil, nil, nil, nil, cal.Session.IsOpen)
		if section.Session.Market != string(tc.want) || section.Session.IsOpen != tc.open || section.Session.Status != rpc.BriefStatusOK {
			t.Fatalf("%s session row = %+v, want market %s open %t", market, section.Session, tc.want, tc.open)
		}
	}
}
