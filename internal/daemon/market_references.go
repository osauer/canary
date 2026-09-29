package daemon

import "github.com/osauer/canary/v2/internal/rpc"

// marketReferences is shared by acquisition and required-feed coverage. A
// failed resolution must retain the reference instead of shrinking the catalogue.
// SPY and QQQ keep their own keys: they trade pre-market while the cash
// indices are frozen, and an ETF never stands in for its index.
func marketReferences() []rpc.MarketInstrument {
	var out []rpc.MarketInstrument
	for _, v := range []struct{ key, name, symbol, kind, exchange string }{
		{"sp500", "S&P 500", "SPX", "index", "CBOE"},
		{"dow", "Dow · DIA ETF", "DIA", "etf", "SMART"},
		{"nasdaq", "Nasdaq 100", "NDX", "index", "NASDAQ"},
		{"russell", "Russell 2000", "RUT", "index", "RUSSELL"},
		{"vix", "VIX", "VIX", "index", "CBOE"},
		{"gold", "Gold · GLD ETF", "GLD", "etf", "SMART"},
		{"spy", "S&P 500 · SPY ETF", "SPY", "etf", "SMART"},
		{"qqq", "Nasdaq 100 · QQQ ETF", "QQQ", "etf", "SMART"},
		{"sp500", "S&P 500 futures", "ES", "future", "CME"},
		{"dow", "Dow futures", "YM", "future", "CBOT"},
		{"nasdaq", "Nasdaq 100 futures", "NQ", "future", "CME"},
		{"russell", "Russell 2000 futures", "RTY", "future", "CME"},
		{"gold", "Gold futures", "GC", "future", "COMEX"},
	} {
		sec := "IND"
		if v.kind == "etf" {
			sec = "STK"
		}
		if v.kind == "future" {
			sec = "FUT"
		}
		out = append(out, rpc.MarketInstrument{Key: v.key, Name: v.name, Kind: v.kind, Quote: &rpc.Quote{Contract: rpc.ContractParams{Symbol: v.symbol, SecType: sec, Exchange: v.exchange, Currency: "USD"}}})
	}
	return out
}
