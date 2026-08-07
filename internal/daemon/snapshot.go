package daemon

import (
	"context"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// snapshotTicks is the raw tick set one brief subscribe collected inside its
// budget, plus the gateway data-type and the price-tick observation instant.
//
// observedAt is when this process last accepted a positive price tick on the
// subscription, zero when none ever arrived. It is an arrival instant, not the
// instant the value was struck: under frozen mode the gateway re-sends the last
// known value on request, so a frozen quote's observedAt is essentially read
// time however old the value is. See [ibkrlib.Subscription.LastPriceTickAt].
type snapshotTicks struct {
	bid, ask, last, mark, closePx float64
	dataType                      string
	observedAt                    time.Time
}

// snapshotQuote is a resolved snapshot price: the last → mid → bid → ask →
// mark → close ladder applied to a [snapshotTicks], carrying the same
// provenance. week52High is populated only by briefSnapshotPriceWith52WHigh;
// the plain price path never requests tick 165.
type snapshotQuote struct {
	price, prevClose, week52High float64
	dataType                     string
	observedAt                   time.Time
}

// briefSnapshotPriceWithClose wraps briefSnapshotFull and returns the
// price (last → mid → bid → ask → mark → close), the previous regular-
// session close (tick 9), the gateway data-type, and the tick observation
// instant. Same price-fallback ladder as briefSnapshotPrice — adds the close
// as a separate field so renderers can show day-over-day change without a
// second subscribe.
//
// Used by the regime VIX fetcher so the dashboard header can carry
// "VIX 18.4 −1.2%" alongside the term-structure ratio.
func briefSnapshotPriceWithClose(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotQuote {
	t := briefSnapshotFull(ctx, c, symbol, timeout, warnf)
	if t.dataType == "" {
		t.dataType = "live"
	}
	var price float64
	switch {
	case t.last > 0:
		price = t.last
	case t.bid > 0 && t.ask > 0:
		price = (t.bid + t.ask) / 2
	case t.bid > 0:
		price = t.bid
	case t.ask > 0:
		price = t.ask
	case t.mark > 0:
		price = t.mark
	case t.closePx > 0:
		price = t.closePx
	default:
		return snapshotQuote{}
	}
	return snapshotQuote{price: price, prevClose: t.closePx, dataType: t.dataType, observedAt: t.observedAt}
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
// Price uses the same last→mid→bid→ask→mark→close priority as
// briefSnapshotPriceWithClose. PrevClose carries tick 9 (previous
// regular-session close) when it lands in the same subscribe window — the
// regime HYG/SPY indicator uses it to populate the dashboard's SPY day-change
// header. Partial results are returned as they landed, so week52High can be
// set while price is zero and vice versa.
func briefSnapshotPriceWith52WHigh(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotQuote {
	if c == nil {
		return snapshotQuote{}
	}
	var out snapshotQuote
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
			out.prevClose = d.Close
		}
		if d.Week52High > 0 {
			out.week52High = d.Week52High
		}
		out.observedAt = d.LastPriceTickAt
		// Capture dataType while the subscription is still live; once
		// UnsubscribeMarketData fires (defer above), the connector's
		// symbol→reqID mapping is gone and the type would always read
		// "unknown".
		if out.dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || out.prevClose > 0) {
			out.dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Done only when both the price triple is summarised AND
		// Week52High has arrived. On timeout, pollMarketData returns
		// DeadlineExceeded and the caller gets whatever did land
		// (price may be set even if week52High didn't).
		return (last > 0 || (bid > 0 && ask > 0) || mark > 0) && out.week52High > 0
	})

	switch {
	case last > 0:
		out.price = last
	case bid > 0 && ask > 0:
		out.price = (bid + ask) / 2
	case bid > 0:
		out.price = bid
	case ask > 0:
		out.price = ask
	case mark > 0:
		out.price = mark
	case out.prevClose > 0:
		out.price = out.prevClose
	}
	return out
}

// briefSnapshotFull subscribes to a symbol, polls until a live tick
// (bid/ask/last/mark) lands, and returns the raw quintuple
// (bid, ask, last, mark, close) plus the gateway's data-type name (live,
// frozen, delayed, delayed-frozen, or "" when nothing arrived) and the tick
// observation instant. The data type is captured while the subscription is
// still live — once
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
func briefSnapshotFull(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotTicks {
	if c == nil {
		return snapshotTicks{}
	}
	sym := normSym(symbol)
	if err := c.SubscribeMarketData(ctx, sym, []string{"100", "101", "104"}); err != nil && warnf != nil {
		warnf("snapshot: SubscribeMarketData %s failed: %v", sym, err)
	}
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	return briefSnapshotFullHeld(ctx, c, sym, timeout)
}

func briefSnapshotFullHeld(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) snapshotTicks {
	if c == nil {
		return snapshotTicks{}
	}
	sym := normSym(symbol)
	var out snapshotTicks
	_ = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		// Capture every tick we've seen so far; on timeout the final
		// iteration's values are what the caller observes.
		out.bid, out.ask, out.last, out.mark, out.closePx = d.Bid, d.Ask, d.Last, d.MarkPrice, d.Close
		out.observedAt = d.LastPriceTickAt
		if out.dataType == "" && (out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0 || out.closePx > 0) {
			// Capture data-type while the subscription is still live;
			// once UnsubscribeMarketData fires (defer above), the
			// connector's symbol→reqID mapping is gone and the type
			// would always read "unknown".
			out.dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Only a true live tick terminates the wait; close alone keeps
		// us polling so a slow bid/ask still wins if it lands in time.
		return out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0
	})
	return out
}
