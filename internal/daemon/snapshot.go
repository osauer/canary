package daemon

import (
	"context"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// snapshotTicks is the raw tick set one brief subscribe collected inside its
// time however old the value is. See [ibkrlib.Subscription.LastPriceTickAt].
type snapshotTicks struct {
	bid, ask, last, mark, closePx float64
	dataType                      string
	observedAt                    time.Time
}

// snapshotQuote is a resolved snapshot price: the last → mid → bid → ask →
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
// ticks 100/101/104 only (option vol / OI / HV — irrelevant here) and
func briefSnapshotPriceWith52WHigh(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotQuote {
	if c == nil {
		return snapshotQuote{}
	}
	var out snapshotQuote
	sym := normSym(symbol)
	// 165 (Misc Stats) is the only addition over briefSnapshotFull's
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
		if out.dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || out.prevClose > 0) {
			out.dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Done only when both the price triple is summarised AND
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
// VIX and SPX emit it as their only price — they don't trade so there
// indices like VIX3M routinely send only close pre-open. On timeout the
// may be non-zero even when the live-tick predicate never fired;
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
		out.bid, out.ask, out.last, out.mark, out.closePx = d.Bid, d.Ask, d.Last, d.MarkPrice, d.Close
		out.observedAt = d.LastPriceTickAt
		if out.dataType == "" && (out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0 || out.closePx > 0) {
			// Capture data-type while the subscription is still live;
			out.dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		// Only a true live tick terminates the wait; close alone keeps
		return out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0
	})
	return out
}
