package daemon

import (
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// briefSessionMarket is the exchange calendar the brief reads its session
// from: the venue holding the largest share of the book's stock and option
// value among the calendars Canary embeds. An account whose holdings sit on
// SEHK, SGX, TSEJ or ASX gets that exchange's open, lunch and close; an
// account with no such holdings, or with most of its value on US venues,
// keeps the US cash session, so the US path is unchanged. Bonds and cash
// rows carry no exchange session and are left out.
func briefSessionMarket(pos *rpc.PositionsResult) marketcal.Market {
	if pos == nil {
		return marketcal.MarketUSEquity
	}
	weights := map[marketcal.Market]float64{}
	for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
		for _, p := range rows {
			if !rpc.PositionQuotesAsStock(p) && !strings.EqualFold(p.SecType, rpc.SecTypeOption) && !strings.EqualFold(p.SecType, "OPT") {
				continue
			}
			weights[briefVenueMarket(p.PrimaryExch, p.Exchange, p.Currency)] += positionWeight(p)
		}
	}
	best, bestWeight := marketcal.MarketUSEquity, weights[marketcal.MarketUSEquity]
	for _, market := range marketcal.AllMarkets() {
		if weight := weights[market]; weight > bestWeight {
			best, bestWeight = market, weight
		}
	}
	return best
}

// positionWeight is the row's absolute value in base currency; the contract
// currency value stands in while the base conversion is unavailable, and a
// row without any value still counts once.
func positionWeight(p rpc.PositionView) float64 {
	if p.MarketValueBase != nil && *p.MarketValueBase != 0 {
		return math.Abs(*p.MarketValueBase)
	}
	if p.MarketValue != 0 {
		return math.Abs(p.MarketValue)
	}
	return 1
}

// briefVenueMarket is venueMarket with the contract currency as the fallback
// for a row IBKR reports without a listing venue, as option rows often are.
// Tokyo is TSEJ in IBKR's naming; TSE is Toronto and stays on the US default
// like every venue without a calendar.
func briefVenueMarket(primaryExch, exchange, currency string) marketcal.Market {
	for _, venue := range []string{primaryExch, exchange} {
		if market, ok := venueMarket(venue); ok {
			return market
		}
	}
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "HKD":
		return marketcal.MarketHKHKEX
	case "SGD":
		return marketcal.MarketSGSGX
	case "JPY":
		return marketcal.MarketJPTSE
	case "AUD":
		return marketcal.MarketAUASX
	case "CNH", "CNY":
		return marketcal.MarketCNAShares
	}
	return marketcal.MarketUSEquity
}
