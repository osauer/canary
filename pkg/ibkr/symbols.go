package ibkr

import (
	"regexp"
	"strings"
)

// Symbol classification helpers.
//
// IBKR requires correct contract hints (secType, exchange, primary) for
// subscription stability, especially for indices and off-hours routing.
// classifySymbol encodes minimal, stable hints we use consistently across
// reqMktData and reqContractData.
//
// FX pairs are accepted in two equivalent forms: dotted (USD.JPY — the
// canonical wire identifier, matching IBKR's own LocalSymbol convention)
// and slash (USD/JPY — the human-readable form). Both classify to
// CASH/IDEALPRO with the quote currency in the Currency field. Callers
// constructing a wire-side Contract must additionally call FxPair to lift
// the base currency into Contract.Symbol — the dotted/slash string is not
// a valid IBKR symbol field on its own.

// fxMajors is the G10 set we recognise as FX pairs. Keeping the list
// explicit avoids misclassifying stock tickers that happen to contain a
// dot or slash (e.g. BRK.B, "BTC/USD" on a crypto venue we don't route).
var fxMajors = map[string]struct{}{
	"USD": {}, "EUR": {}, "JPY": {}, "GBP": {}, "CHF": {},
	"AUD": {}, "NZD": {}, "CAD": {},
}

// FxPair parses an FX-pair symbol in either dotted (USD.JPY) or slash
// (USD/JPY) form. Returns the base currency, quote currency, and ok=true
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
// — the API canonical form uses a space ("BRK B", "BF B"). Returns the
// input unchanged for any ticker that doesn't match the convention.
//
// The rule matches the well-established US dual-class convention: 1–4
// uppercase letters (the base ticker) followed by a dot and a single
// uppercase letter (the share-class suffix, typically A or B). FX pairs
// don't match because their second leg is 3 letters; index probes like
// "BPSPX" / "MMFI" don't have a dot. Future-proof: when a new dual-class
// name joins the S&P 500 (rare but it happens — e.g. a re-class or a
// spin-off), the translation works without a table update.
//
// Why this lives next to classifySymbol: every code path that builds a
// Contract from a raw symbol — FetchContractDetails, FetchHistoricalDailyBars,
// prepareContract — needs the same translation to resolve dual-class
// names. Centralising the override prevents subtle drift between paths,
// which would silently exclude BRK.B (Berkshire Hathaway Class B) from
// breadth — a top-10 SPX member by weight whose exclusion materially
// distorts the S5FI compute.
func dualClassWireSymbol(input string) string {
	upper := strings.ToUpper(strings.TrimSpace(input))
	if dualClassPattern.MatchString(upper) {
		return strings.Replace(upper, ".", " ", 1)
	}
	return input
}

// dualClassPattern matches the US dual-class ticker convention: a 1–4
// letter base symbol, a dot, and a single-letter share class. Anchored
// to the full string so partial matches (e.g. embedded substrings) are
// rejected. Compiled once at init.
var dualClassPattern = regexp.MustCompile(`^[A-Z]{1,4}\.[A-Z]$`)

// classifySymbol returns (secType, exchange, currency, primaryExchangeHint)
// for common indices/ETFs/stocks/FX-pairs to keep contract requests and
// market data routing consistent across the codebase.
func classifySymbol(symbol string) (string, string, string, string) {
	// FX pairs route through IDEALPRO with the quote currency on the
	// Currency field; the base currency goes on Contract.Symbol (callers
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
	// denominator of the VIX term-structure signal (Indicator 1 of
	// the risk-regime dashboard).
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
	// S&P DJI; MMFI on TradingView; $SPXA50R on StockCharts) is NOT
	// catalogued in IBKR's contract database under any of the standard
	// names — verified via reqContractDetails probe against the CBOE
	// US Indexes subscription. IBKR's "CBOE US Indexes" feed covers
	// tradeable CBOE-listed indices (SPX, VIX, RUT, NDX, SKEW, VVIX,
	// …); S&P DJI's breadth statistics are calculated, not listed, so
	// they're a different data product that IBKR doesn't appear to
	// redistribute. The breadth.spx endpoint therefore needs either a
	// constituent-fan-out fallback (compute from 500 daily bars) or
	// the dashboard treats Indicator 5 as manual-entry. See
	// docs/docs/internals/regime-dashboard.md for the disposition.

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
	// futures contract, but no caller ever requested ES via classifySymbol
	// — the FUT override was dead code, and worse, it collided with
	// Eversource Energy (ticker ES), an S&P 500 stock whose breadth
	// fetcher needs STK/SMART. The futures override was removed in the
	// breadth-spx convergence fix; callers that want E-mini futures
	// must build a Contract with SecType=FUT explicitly rather than
	// rely on a symbol-string lookup.

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
		// discovery, but TradingClass is still the underlying symbol. Without
		// that hint SPY-style reqContractData calls can be rejected with code 200.
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
