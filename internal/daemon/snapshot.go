package daemon

import (
	"context"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// briefSnapshotPriceWithClose wraps briefSnapshotFull and returns the
// price (last → mid → bid → ask → mark → close), the previous regular-
// session close (tick 9), and the gateway data-type. Same price-fallback
// ladder as briefSnapshotPrice — adds the close as a separate return so
// renderers can show day-over-day change without a second subscribe.
//
// Used by the regime VIX fetcher so the dashboard header can carry
// "VIX 18.4 −1.2%" alongside the term-structure ratio.
func briefSnapshotPriceWithClose(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) (price, prevClose float64, dataType string) {
	bid, ask, last, mark, closePx, dt := briefSnapshotFull(ctx, c, symbol, timeout, warnf)
	if dt == "" {
		dt = "live"
	}
	switch {
	case last > 0:
		price = last
	case bid > 0 && ask > 0:
		price = (bid + ask) / 2
	case bid > 0:
		price = bid
	case ask > 0:
		price = ask
	case mark > 0:
		price = mark
	case closePx > 0:
		price = closePx
	default:
		return 0, 0, ""
	}
	return price, closePx, dt
}

// briefSnapshotPriceWith52WHigh subscribes to a symbol with generic
// tick 165 (Misc Stats) and waits for both the price triple AND the
// Week52High field to land before returning. Either field may still
// come back zero — partial results are honest; callers gate on what
// they got.
//
// Why a separate helper: the default briefSnapshotPrice path requests
// ticks 100/101/104 only (option vol / OI / HV — irrelevant here) and
// returns on the FIRST price tick, which is too fast for the
// Misc-Stats tick (165 = Week-range highs/lows) to arrive in the same
// subscribe window. The regime HYG/SPY indicator needs SPY's 52w high
// to evaluate the spec's yellow-band trigger ("HYG breaks 50dma while
// SPY near highs"); without it the indicator drops to a 2-state
// signal. Two sequential subscribes (price-only + Misc) would also
// double the gateway-slot footprint and add a second
// contract-resolution round-trip; one combined call is cheaper.
//
// Returns (price, prevClose, week52High, dataType). Price uses the same
// last→mid→bid→ask→mark→close priority as briefSnapshotPriceWithClose.
// PrevClose carries tick 9 (previous regular-session close) when it
// lands in the same subscribe window — the regime HYG/SPY indicator
// uses it to populate the dashboard's SPY day-change header.
func briefSnapshotPriceWith52WHigh(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) (price, prevClose, week52High float64, dataType string) {
	if c == nil {
		return 0, 0, 0, ""
	}
	sym := normSym(symbol)
	// 165 (Misc Stats) is the only addition over briefSnapshotFull's
	// list; the others are kept for API consistency with the
	// established subscribe pattern.
	if err := c.SubscribeMarketData(ctx, sym, []string{"100", "101", "104", "165"}); err != nil && warnf != nil {
		warnf("snapshot: SubscribeMarketData %s failed: %v", sym, err)
	}
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	var bid, ask, last, mark float64
	_ = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		if d.Bid > 0 {
			bid = d.Bid
		}
		if d.Ask > 0 {
			ask = d.Ask
		}
		if d.Last > 0 {
			last = d.Last
		}
		if d.MarkPrice > 0 {
			mark = d.MarkPrice
		}
		if d.Close > 0 {
			prevClose = d.Close
		}
		if d.Week52High > 0 {
			week52High = d.Week52High
		}
		// Capture dataType while the subscription is still live; once
		// UnsubscribeMarketData fires (defer above), the connector's
		// symbol→reqID mapping is gone and the type would always read
		// "unknown".
		if dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || prevClose > 0) {
			dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Done only when both the price triple is summarised AND
		// Week52High has arrived. On timeout, pollMarketData returns
		// DeadlineExceeded and the caller gets whatever did land
		// (price may be set even if week52High didn't).
		return (last > 0 || (bid > 0 && ask > 0) || mark > 0) && week52High > 0
	})

	switch {
	case last > 0:
		price = last
	case bid > 0 && ask > 0:
		price = (bid + ask) / 2
	case bid > 0:
		price = bid
	case ask > 0:
		price = ask
	case mark > 0:
		price = mark
	case prevClose > 0:
		price = prevClose
	}
	return price, prevClose, week52High, dataType
}

// briefSnapshotFull subscribes to a symbol, polls until a live tick
// (bid/ask/last/mark) lands, and returns the raw quintuple
// (bid, ask, last, mark, close) plus the gateway's data-type name (live,
// frozen, delayed, delayed-frozen, or "" when nothing arrived). The
// data type is captured while the subscription is still live — once
// UnsubscribeMarketData fires (defer), the connector's symbol→reqID
// mapping is gone and the type would always read "unknown".
//
// Mark price (tick 37) is treated as a live tick because indices like
// VIX and SPX emit it as their only price — they don't trade so there
// is no bid/ask/last.
//
// Close (tick 9, the prior regular-session close) is captured on every
// poll iteration but does NOT terminate the wait. It is a backstop for
// instruments that emit no live tick within the budget — thin CBOE
// indices like VIX3M routinely send only close pre-open. On timeout the
// values from the last poll iteration are returned, which means close
// may be non-zero even when the live-tick predicate never fired;
// callers fall back to it as a last resort. The data-type field is
// populated regardless of which ticks landed so the renderer can
// truthfully label the row "frozen" instead of pretending it's live.
func briefSnapshotFull(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) (bid, ask, last, mark, closePx float64, dataType string) {
	if c == nil {
		return 0, 0, 0, 0, 0, ""
	}
	sym := normSym(symbol)
	if err := c.SubscribeMarketData(ctx, sym, []string{"100", "101", "104"}); err != nil && warnf != nil {
		warnf("snapshot: SubscribeMarketData %s failed: %v", sym, err)
	}
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	return briefSnapshotFullHeld(ctx, c, sym, timeout)
}

func briefSnapshotFullHeld(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) (bid, ask, last, mark, closePx float64, dataType string) {
	if c == nil {
		return 0, 0, 0, 0, 0, ""
	}
	sym := normSym(symbol)
	_ = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		// Capture every tick we've seen so far; on timeout the final
		// iteration's values are what the caller observes.
		bid, ask, last, mark, closePx = d.Bid, d.Ask, d.Last, d.MarkPrice, d.Close
		if dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || closePx > 0) {
			// Capture data-type while the subscription is still live;
			// once UnsubscribeMarketData fires (defer above), the
			// connector's symbol→reqID mapping is gone and the type
			// would always read "unknown".
			dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Only a true live tick terminates the wait; close alone keeps
		// us polling so a slow bid/ask still wins if it lands in time.
		return bid > 0 || ask > 0 || last > 0 || mark > 0
	})
	return bid, ask, last, mark, closePx, dataType
}
