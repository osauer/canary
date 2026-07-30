package rpc

// A data-quality signal claims that reality is unobserved. Where reality is
// observed to be nothing — a cancelled equity, a defunct issuer — that is a
// fact, not a gap. QuoteExpectationNone carries that distinction across every
// surface so each consumer does not have to re-derive it from warning codes.
// Only the broker's terminal non-reporting verdict may mint it; numeric zeros
// in account rows are a data-quality warning, never expectation authority.
const (
	QuoteExpectationNone = "none"

	QuoteExpectationReasonTerminal = "terminal_non_reporting"
)

// ExpectsMarketData reports whether a quote, mark, or market-event flag should
// exist for this position. Absence is a defect only when this returns true.
func ExpectsMarketData(p PositionView) bool {
	return p.QuoteExpectation != QuoteExpectationNone
}

// ExpectsMarketDataGroup reports whether an underlying group should be
// subscribed for market data. Only a stock-only group whose stock expects no
// data is skipped: an option leg on a defunct underlying still needs its own
// quote, and the group's other rows are unaffected.
func ExpectsMarketDataGroup(g PositionGroup) bool {
	if len(g.Options) > 0 {
		return true
	}
	if g.Stock == nil {
		return true
	}
	return ExpectsMarketData(*g.Stock)
}
