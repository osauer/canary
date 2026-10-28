package mandate

import (
	"slices"
	"strings"
)

// BroadIndexInstrument is one instrument the index gap shock applies to: a
// fund or an index over hundreds of companies, which no single company's
// news can gap by a one-name stress (owner decision D4, 2026-10-14: "12%
// for broad index funds"; internal-docs/design/core-sleeve.md). A line
// matches by its ISIN where the daemon knows it (a contract IBKR listed
// under the ISIN, Input.LineISINs), else by its broker symbol and its
// currency together: a position row carries no ISIN, and one fund trades
// under other symbols on other venues.
type BroadIndexInstrument struct {
	Symbol     string
	Currencies []string
	ISIN       string
	Name       string
}

// BroadIndexInstruments is the fixed list. It is compiled, not policy: a
// line not on it keeps the one-name gap_shock_pct, the stricter stress, so
// an omission errs towards the larger gap and an addition needs a reviewed
// change. Index options match through their underlying symbol.
var BroadIndexInstruments = []BroadIndexInstrument{
	// US-listed S&P 500, Nasdaq-100, Russell 2000 and Dow ETFs and the
	// cash indices their options settle on.
	{Symbol: "SPY", Currencies: []string{"USD"}, Name: "SPDR S&P 500 ETF"},
	{Symbol: "SPX", Currencies: []string{"USD"}, Name: "S&P 500 index"},
	{Symbol: "SPXW", Currencies: []string{"USD"}, Name: "S&P 500 index (weekly options)"},
	{Symbol: "XSP", Currencies: []string{"USD"}, Name: "Mini S&P 500 index"},
	{Symbol: "QQQ", Currencies: []string{"USD"}, Name: "Invesco QQQ (Nasdaq-100)"},
	{Symbol: "NDX", Currencies: []string{"USD"}, Name: "Nasdaq-100 index"},
	{Symbol: "IWM", Currencies: []string{"USD"}, Name: "iShares Russell 2000 ETF"},
	{Symbol: "RUT", Currencies: []string{"USD"}, Name: "Russell 2000 index"},
	{Symbol: "DIA", Currencies: []string{"USD"}, Name: "SPDR Dow Jones Industrial Average ETF"},
	// UCITS S&P 500 funds.
	{Symbol: "IBCF", Currencies: []string{"EUR"}, ISIN: "IE00B3ZW0K18", Name: "iShares S&P 500 EUR Hedged UCITS ETF (Acc)"},
	{Symbol: "SXR8", Currencies: []string{"EUR"}, ISIN: "IE00B5BMR087", Name: "iShares Core S&P 500 UCITS ETF (Acc)"},
	{Symbol: "CSPX", Currencies: []string{"USD"}, ISIN: "IE00B5BMR087", Name: "iShares Core S&P 500 UCITS ETF (Acc)"},
	{Symbol: "VUSA", Currencies: []string{"EUR", "GBP"}, ISIN: "IE00B3XXRP09", Name: "Vanguard S&P 500 UCITS ETF (Dist)"},
	{Symbol: "VUAA", Currencies: []string{"EUR", "USD"}, ISIN: "IE00BFMXXD54", Name: "Vanguard S&P 500 UCITS ETF (Acc)"},
	// UCITS world funds.
	{Symbol: "EUNL", Currencies: []string{"EUR"}, ISIN: "IE00B4L5Y983", Name: "iShares Core MSCI World UCITS ETF (Acc)"},
	{Symbol: "IWDA", Currencies: []string{"EUR", "USD"}, ISIN: "IE00B4L5Y983", Name: "iShares Core MSCI World UCITS ETF (Acc)"},
	{Symbol: "SWDA", Currencies: []string{"GBP"}, ISIN: "IE00B4L5Y983", Name: "iShares Core MSCI World UCITS ETF (Acc)"},
	{Symbol: "VWCE", Currencies: []string{"EUR"}, ISIN: "IE00BK5BQT80", Name: "Vanguard FTSE All-World UCITS ETF (Acc)"},
	{Symbol: "VWRL", Currencies: []string{"EUR", "GBP"}, ISIN: "IE00B3RBWM25", Name: "Vanguard FTSE All-World UCITS ETF (Dist)"},
	{Symbol: "VGWL", Currencies: []string{"EUR"}, ISIN: "IE00B3RBWM25", Name: "Vanguard FTSE All-World UCITS ETF (Dist)"},
	// S&P 500 futures, under their own key (MicroSP500Future).
	{Symbol: MicroSP500Future, Currencies: []string{"USD"}, Name: "Micro E-mini S&P 500 futures"},
}

// MicroSP500Future is the underlying key of a Micro E-mini S&P 500 future
// line (the index hedge, owner decision 2026-10-28 13:17 CET). A future is a
// linear line on the index: a stock-kind leg at its contract multiplier,
// spot the future's price, moved by SPY's returns (ReturnsProxies) and
// stressed by the index gap. The key carries " FUT" so no share symbol can
// match it: ES, the larger contract's root, is also a listed company.
const MicroSP500Future = "MES FUT"

// IsBroadIndex reports whether a line of this symbol and currency is on
// the broad index list. Both must match: a symbol alone can name another
// company on another venue.
func IsBroadIndex(symbol, currency string) bool {
	symbol, currency = strings.ToUpper(strings.TrimSpace(symbol)), strings.ToUpper(strings.TrimSpace(currency))
	for _, b := range BroadIndexInstruments {
		if b.Symbol == symbol && slices.Contains(b.Currencies, currency) {
			return true
		}
	}
	return false
}

// IsBroadIndexLine reports whether a line is on the broad index list: by
// its ISIN when known, in any currency (one fund), else as IsBroadIndex.
func IsBroadIndexLine(symbol, currency, isin string) bool {
	if isin = strings.ToUpper(strings.TrimSpace(isin)); isin != "" {
		for _, b := range BroadIndexInstruments {
			if b.ISIN == isin {
				return true
			}
		}
	}
	return IsBroadIndex(symbol, currency)
}

// GapShockPctFor is the gap one underlying is stressed by: the index gap
// for a broad index when the owner wrote one, else the one-name gap.
func (p Policy) GapShockPctFor(broadIndex bool) float64 {
	if broadIndex && p.IndexGapShockPct > 0 {
		return p.IndexGapShockPct
	}
	return p.GapShockPct
}

// ReturnsProxy is a line the simulation moves with another series' daily
// returns instead of its own closes.
type ReturnsProxy struct {
	Symbol   string
	Currency string
	// ISIN matches the fund's other listings in Currency: a line IBKR
	// lists under it (Input.LineISINs) takes the proxy too.
	ISIN string
	// Series is the underlying whose decision-grade returns the line takes.
	Series string
	Why    string
}

// ReturnsProxies is the fixed list (core sleeve design review S5,
// 2026-10-14). IBCF holds the S&P 500 hedged to EUR at fund level, so in
// EUR it moves with the index's local-currency return and carries no USD
// leg; its own close at 17:30 CET (Xetra, and London's EUR line at 16:30
// London) is four and a half hours before the US close every other US
// line is measured at, which would understate its correlation with them.
// SPY's returns put it on the book's clock. Every EUR listing of the fund
// matches by ISIN.
var ReturnsProxies = []ReturnsProxy{
	{Symbol: "IBCF", Currency: "EUR", ISIN: "IE00B3ZW0K18", Series: IndexSymbol, Why: "EUR-hedged S&P 500 fund: SPY's local-currency returns, on the US close"},
	{Symbol: MicroSP500Future, Currency: "USD", Series: IndexSymbol, Why: "S&P 500 future: SPY's returns on the US close; its dated contracts roll, so no one contract's closes span the window"},
}

// ReturnsSeries names the series a line's daily returns are read from: its
// proxy's for a line on ReturnsProxies, else its own underlying's.
func ReturnsSeries(underlying, currency string) string {
	u, c := strings.ToUpper(strings.TrimSpace(underlying)), strings.ToUpper(strings.TrimSpace(currency))
	for _, p := range ReturnsProxies {
		if p.Symbol == u && p.Currency == c {
			return p.Series
		}
	}
	return u
}

// ReturnsSeriesFor is ReturnsSeries with the line's ISIN: a line IBKR lists
// under a proxied fund's ISIN, in that proxy's currency, takes the proxy
// whatever its broker symbol.
func ReturnsSeriesFor(underlying, currency, isin string) string {
	if i, c := strings.ToUpper(strings.TrimSpace(isin)), strings.ToUpper(strings.TrimSpace(currency)); i != "" {
		for _, p := range ReturnsProxies {
			if p.ISIN == i && p.Currency == c {
				return p.Series
			}
		}
	}
	return ReturnsSeries(underlying, currency)
}

// LegReturnsSeries is the series a leg's daily returns are read from.
func LegReturnsSeries(l Leg) string { return ReturnsSeriesFor(l.Underlying, l.Currency, l.ISIN) }
