package rpc

import "strings"

func normalizeMarketLabel(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// UnderlyingMarketContract derives the existing market route from a held underlying group.
func UnderlyingMarketContract(group PositionGroup) (ContractParams, bool) {
	symbol := normalizeMarketLabel(group.Underlying)
	if symbol == "" && group.Stock != nil {
		symbol = normalizeMarketLabel(group.Stock.Symbol)
	}
	if symbol == "" && len(group.Options) > 0 {
		symbol = normalizeMarketLabel(group.Options[0].Symbol)
	}
	if symbol == "" {
		return ContractParams{}, false
	}

	if group.Stock != nil {
		contract := stockPositionQuoteContract(*group.Stock)
		contract.Symbol = symbol
		if contract.Currency == "" {
			contract.Currency = underlyingGroupCurrency(group)
		}
		return contract, true
	}

	contract := fallbackUnderlyingQuoteContract(symbol, underlyingGroupCurrency(group))
	return contract, true
}

func stockPositionQuoteContract(stock PositionView) ContractParams {
	contract := ContractParams{
		ConID:        stock.ConID,
		Symbol:       normalizeMarketLabel(stock.Symbol),
		SecType:      requestQuoteSecType(stock.SecType),
		Exchange:     strings.ToUpper(strings.TrimSpace(stock.Exchange)),
		Currency:     normalizeMarketLabel(stock.Currency),
		LocalSymbol:  strings.TrimSpace(stock.LocalSymbol),
		TradingClass: strings.TrimSpace(stock.TradingClass),
		Multiplier:   stock.Multiplier,
	}
	if contract.SecType == "" {
		contract.SecType = "STK"
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if contract.Exchange == "" && contract.ConID == 0 {
		contract.Exchange = "SMART"
	}
	return contract
}

func fallbackUnderlyingQuoteContract(symbol, currency string) ContractParams {
	contract := ContractParams{
		Symbol:   symbol,
		SecType:  "STK",
		Exchange: "SMART",
		Currency: normalizeMarketLabel(currency),
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if index, ok := indexUnderlyingContracts[symbol]; ok {
		return index
	}
	return contract
}

var indexUnderlyingContracts = map[string]ContractParams{
	"SPX": {Symbol: "SPX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
	"NDX": {Symbol: "NDX", SecType: "IND", Exchange: "NASDAQ", PrimaryExch: "NASDAQ", Currency: "USD"},
	"RUT": {Symbol: "RUT", SecType: "IND", Exchange: "RUSSELL", PrimaryExch: "RUSSELL", Currency: "USD"},
	"VIX": {Symbol: "VIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
}

func requestQuoteSecType(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "":
		return "STK"
	case "IND", "INDEX":
		return "IND"
	default:
		return ""
	}
}

func underlyingGroupCurrency(group PositionGroup) string {
	if group.Stock != nil {
		if ccy := normalizeMarketLabel(group.Stock.Currency); ccy != "" {
			return ccy
		}
	}
	for _, option := range group.Options {
		if ccy := normalizeMarketLabel(option.Currency); ccy != "" {
			return ccy
		}
	}
	return "USD"
}
