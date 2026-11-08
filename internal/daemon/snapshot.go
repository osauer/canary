package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	brokerIBKR "github.com/osauer/canary/v2/internal/broker/ibkr"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// snapshotTicks is the raw tick set one brief subscribe collected inside its
// time however old the value is. See [ibkrlib.Subscription.LastPriceTickAt].
type snapshotTicks struct {
	bid, ask, last, mark, closePx float64
	dataType                      string
	observedAt                    time.Time
	// unsupplied is a look the broker refused because it does not supply
	// the instrument at all.
	unsupplied bool
}

// snapshotQuote is a resolved snapshot price: the last → mid → bid → ask →
// provenance. week52High is populated only by briefWith52WHigh;
// the plain price path never requests tick 165.
type snapshotQuote struct {
	price, prevClose, week52High float64
	dataType                     string
	observedAt                   time.Time
	// unsupplied is a look the broker refused because it does not supply
	// the instrument at all.
	unsupplied bool
}

// briefLook takes one brief look at a quote line by symbol: the broker
// connection's [broker.Snapshots], or the IBKR connector's own for callers
// that hold one.
type briefLook func(ctx context.Context, symbol string, fields []string, timeout time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error)

// connectorLook is the brief look on an IBKR connector, the routine the
// IBKR broker connection serves.
func connectorLook(c *ibkrlib.Connector) briefLook {
	return func(ctx context.Context, symbol string, fields []string, timeout time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error) {
		return brokerIBKR.BriefQuote(ctx, c, symbol, fields, timeout, done)
	}
}

// warnLook reports a failed look, except an instrument the broker does not
// supply: that is a gap the caller names, not a fault.
func warnLook(warnf func(string, ...any), symbol string, err error) {
	if err != nil && warnf != nil && !errors.Is(err, broker.ErrNotSupplied) {
		warnf("snapshot: SubscribeMarketData %s failed: %v", normSym(symbol), err)
	}
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
	if c == nil {
		return snapshotQuote{}
	}
	return priceWithClose(briefTicks(ctx, connectorLook(c), symbol, timeout, warnf))
}

// priceWithClose resolves a look's ticks to a price by the ladder above.
func priceWithClose(t snapshotTicks) snapshotQuote {
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
		return snapshotQuote{unsupplied: t.unsupplied}
	}
	return snapshotQuote{price: price, prevClose: t.closePx, dataType: t.dataType, observedAt: t.observedAt}
}

// briefWith52WHigh asks for generic ticks 100/101/104 (option vol / OI / HV,
// irrelevant here) and 165 (Misc Stats) for the 52-week high, and keeps each
// tick's last positive value across reads.
func briefWith52WHigh(ctx context.Context, look briefLook, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotQuote {
	var out snapshotQuote
	var bid, ask, last, mark float64
	// 165 (Misc Stats) is the only addition over briefSnapshotFull's
	_, err := look(ctx, symbol, []string{"100", "101", "104", "165"}, timeout, func(d broker.BriefQuote) bool {
		if d.Bid > 0 {
			bid = d.Bid
		}
		if d.Ask > 0 {
			ask = d.Ask
		}
		if d.Last > 0 {
			last = d.Last
		}
		if d.Mark > 0 {
			mark = d.Mark
		}
		if d.Close > 0 {
			out.prevClose = d.Close
		}
		if d.Week52High > 0 {
			out.week52High = d.Week52High
		}
		out.observedAt = d.ObservedAt
		// Capture dataType while the subscription is still live; once
		if out.dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || out.prevClose > 0) {
			out.dataType = marketDataTypeName(d.FeedType)
		}
		// Done only when both the price triple is summarised AND
		return (last > 0 || (bid > 0 && ask > 0) || mark > 0) && out.week52High > 0
	})
	warnLook(warnf, symbol, err)
	out.unsupplied = errors.Is(err, broker.ErrNotSupplied)

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
	return briefTicks(ctx, connectorLook(c), symbol, timeout, warnf)
}

// briefTicks is briefSnapshotFull over any brief look: each read replaces the
// ticks, and only a live tick ends the wait.
func briefTicks(ctx context.Context, look briefLook, symbol string, timeout time.Duration, warnf func(string, ...any)) snapshotTicks {
	var out snapshotTicks
	_, err := look(ctx, symbol, []string{"100", "101", "104"}, timeout, func(d broker.BriefQuote) bool {
		// Capture every tick we've seen so far; on timeout the final
		out.bid, out.ask, out.last, out.mark, out.closePx = d.Bid, d.Ask, d.Last, d.Mark, d.Close
		out.observedAt = d.ObservedAt
		if out.dataType == "" && (out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0 || out.closePx > 0) {
			// Capture data-type while the subscription is still live;
			out.dataType = marketDataTypeName(d.FeedType)
		}
		// Only a true live tick terminates the wait; close alone keeps
		return out.bid > 0 || out.ask > 0 || out.last > 0 || out.mark > 0
	})
	warnLook(warnf, symbol, err)
	out.unsupplied = errors.Is(err, broker.ErrNotSupplied)
	return out
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
