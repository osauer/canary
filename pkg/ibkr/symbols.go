package ibkr

import (
	"regexp"
	"strings"
)

// Symbol classification helpers.
// IBKR requires correct contract hints (secType, exchange, primary) for
// canonical wire identifier, matching IBKR's own LocalSymbol convention)
// constructing a wire-side Contract must additionally call FxPair to lift
// a valid IBKR symbol field on its own.

// fxMajors is the G10 set we recognise as FX pairs. Keeping the list
var fxMajors = map[string]struct{}{
	"USD": {}, "EUR": {}, "JPY": {}, "GBP": {}, "CHF": {},
	"AUD": {}, "NZD": {}, "CAD": {},
}

// FxPair parses an FX-pair symbol in either dotted (USD.JPY) or slash
// only when both legs are in fxMajors. Case-insensitive; trims whitespace.
func FxPair(symbol string) (base, quote string, ok bool) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	var sep string
	switch {
	case strings.Count(s, ".") == 1:
		sep = "."
	case strings.Count(s, "/") == 1:
		sep = "/"
	default:
		return "", "", false
	}
	left, right, _ := strings.Cut(s, sep)
	if len(left) != 3 || len(right) != 3 {
		return "", "", false
	}
	if _, ok := fxMajors[left]; !ok {
		return "", "", false
	}
	if _, ok := fxMajors[right]; !ok {
		return "", "", false
	}
	return left, right, true
}

// dualClassWireSymbol translates an S&P ticker for a dual-class share
// into the wire-format symbol IBKR's TWS API expects. The S&P / index
// convention uses a dot ("BRK.B", "BF.B") but IBKR rejects that form
// with code 200 "No security definition has been found for the request"
func dualClassWireSymbol(input string) string {
	upper := strings.ToUpper(strings.TrimSpace(input))
	if dualClassPattern.MatchString(upper) {
		return strings.Replace(upper, ".", " ", 1)
	}
	return input
}

// dualClassPattern matches the US dual-class ticker convention: a 1–4
var dualClassPattern = regexp.MustCompile(`^[A-Z]{1,4}\.[A-Z]$`)

// classifySymbol returns (secType, exchange, currency, primaryExchangeHint)
func classifySymbol(symbol string) (string, string, string, string) {
	// FX pairs route through IDEALPRO with the quote currency on the
	// must apply FxPair when building the wire contract).
	if _, quote, ok := FxPair(symbol); ok {
		return "CASH", "IDEALPRO", quote, "IDEALPRO"
	}

	// Defaults
	secType := "STK"
	exchange := "SMART"
	currency := "USD"
	primary := ""

	switch symbol {
	// Broad indices
	case "VIX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	case "VVIX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	// VIX3M is the CBOE 3-month implied volatility index, the
	case "VIX3M":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	case "SPX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	case "NDX":
		secType = "IND"
		exchange = "NASDAQ"
		primary = "NASDAQ"
	case "DJI", "DJX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"

	// The S&P 500 stocks-above-50DMA breadth index (S5FI family on
	// catalogued in IBKR's contract database under any of the standard
	// US Indexes subscription. IBKR's "CBOE US Indexes" feed covers
	// they're a different data product that IBKR doesn't appear to

	// Common ETFs
	case "SPY", "QQQ", "IWM", "DIA", "GLD", "TLT", "HYG",
		"SMH", "XLK", "XLF", "XLI", "XLE", "XLV", "XLY", "XLP",
		"XLU", "XLB", "XLRE", "IBIT":
		secType = "STK"
		exchange = "SMART"
		primary = "ARCA" // Often better coverage OOH

	// Dollar index (ICE US)
	case "DXY":
		secType = "IND"
		exchange = "ICEUS"
		primary = "ICEUS"

	// "ES" historically mapped here to FUT/GLOBEX for the E-mini S&P
	// must build a Contract with SecType=FUT explicitly rather than

	default:
		// leave defaults
	}

	return secType, exchange, currency, primary
}

func optionUnderlyingPrimaryExchangeHint(symbol string) string {
	secType, _, _, primary := classifySymbol(strings.ToUpper(strings.TrimSpace(symbol)))
	if secType != "STK" {
		return ""
	}
	return primary
}

func contractDisplayHints(symbol, secType string) (string, string) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	switch secType {
	case "OPT":
		// Equity/ETF options usually leave LocalSymbol empty during contract
		return "", symbol
	case "IND":
		switch symbol {
		case "VIX", "VVIX", "VIX3M", "SPX", "NDX", "DJI", "DJX", "DXY":
			return symbol, symbol
		}
	case "CMDTY":
		return symbol, symbol
	}
	return "", ""
}
