package ibkr

import (
	"context"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// pollCadence is how often a brief look reads the line, as the daemon's
// short-lived snapshot reads always have.
const pollCadence = 75 * time.Millisecond

// BriefQuote takes one brief look at a line by symbol: the default route the
// connector resolves for in.Symbol, whatever in.SecType says, as the daemon's
// brief snapshots always have.
func (a *Adapter) BriefQuote(ctx context.Context, in broker.Instrument, fields []string, timeout time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error) {
	if a == nil || a.conn == nil {
		return broker.BriefQuote{}, ibkrlib.ErrIBKRUnavailable
	}
	return BriefQuote(ctx, a.conn, in.Symbol, fields, timeout, done)
}

// BriefQuote subscribes c to symbol, reads the cached line every pollCadence
// until done holds, timeout passes, ctx ends or the gateway rejects the
// line, and unsubscribes. A failed subscribe is returned after the reads,
// which still run: a line another reader holds may already be cached.
func BriefQuote(ctx context.Context, c *ibkrlib.Connector, symbol string, fields []string, timeout time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error) {
	if c == nil {
		return broker.BriefQuote{}, ibkrlib.ErrIBKRUnavailable
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	subscribeErr := c.SubscribeMarketData(ctx, sym, fields)
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	var seen broker.BriefQuote
	read := func() bool {
		d, ok := c.MarketDataSnapshot()[sym]
		if !ok {
			return false
		}
		seen = broker.BriefQuote{
			Bid: d.Bid, Ask: d.Ask, Last: d.Last, Mark: d.MarkPrice, Close: d.Close, Week52High: d.Week52High,
			FeedType:   c.MarketDataTypeForSymbol(sym),
			ObservedAt: d.LastPriceTickAt,
		}
		return done(seen)
	}
	deadline := time.Now().Add(timeout)
	rejected := c.SubscriptionRejectCh(sym)
	if read() {
		return seen, subscribeErr
	}
	poll := time.NewTicker(pollCadence)
	defer poll.Stop()
	for !time.Now().After(deadline) {
		select {
		case <-ctx.Done():
			return seen, subscribeErr
		case <-rejected:
			return seen, subscribeErr
		case <-poll.C:
		}
		if read() {
			return seen, subscribeErr
		}
	}
	return seen, subscribeErr
}

// DailyBars reads daily bars by symbol through the connector's default route,
// the history the regime has always read.
func (a *Adapter) DailyBars(ctx context.Context, in broker.Instrument, days int) ([]broker.Bar, error) {
	if a == nil || a.conn == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	rows, err := a.conn.FetchHistoricalDailyBars(ctx, in.Symbol, days, 0)
	return HistoricalBars(rows), err
}

// HistoricalBars maps the connector's daily bars onto neutral bars, field
// for field; [ToHistoricalBars] is its inverse.
func HistoricalBars(rows []ibkrlib.HistoricalBar) []broker.Bar {
	if rows == nil {
		return nil
	}
	out := make([]broker.Bar, len(rows))
	for i, r := range rows {
		out[i] = broker.Bar{Time: r.Time, Date: r.Date, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume, Average: r.Average, BarCount: r.BarCount}
	}
	return out
}

// ToHistoricalBars maps neutral bars back onto the connector's daily bars,
// for the code that still reads that shape.
func ToHistoricalBars(bars []broker.Bar) []ibkrlib.HistoricalBar {
	if bars == nil {
		return nil
	}
	out := make([]ibkrlib.HistoricalBar, len(bars))
	for i, b := range bars {
		out[i] = ibkrlib.HistoricalBar{Time: b.Time, Date: b.Date, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Average: b.Average, BarCount: b.BarCount}
	}
	return out
}
