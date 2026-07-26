package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// breadthFetcher adapts the daemon's gateway connector to the
// spx.BarFetcher interface. Lives in the daemon package — not in spx
// — so the engine package stays free of any pkg/ibkr import.
//
// The connector is supplied via a thunk (connector accessor) rather
// than captured at construction. The daemon may reconnect or swap
// connectors after a gateway disconnect; reading the live pointer on
// every call keeps the adapter pointing at whichever connector is
// currently authoritative without re-instantiating it.
type breadthFetcher struct {
	getConn func() *ibkrlib.Connector
	// defaultTimeout is the per-request wait when ctx has no deadline.
	// Note this budget does NOT cover the rate-limiter queue wait —
	// historical requests can sit up to ~12 min inside rl.Submit while
	// waiting on the historicalRate bucket (60 tokens, 0.1/sec refill),
	// and that wait is bounded by submitTimeout(RequestTypeHistorical)
	// in pkg/ibkr/ratelimiter.go. defaultTimeout governs only the
	// post-send segments: graceWindow for awaitContractDetail and the
	// response-wait timer in fetchHistoricalWithContract. Daily bars
	// typically return within ~2 s once on the wire; 2 min gives
	// generous headroom for the concurrent-fan-out case (the rate
	// limiter's per-request dispatcher means ~50 historical responses
	// can be in flight at once and IBKR is slower under that load)
	// while still surfacing a genuine stall in well under one refresh
	// tick. The previous 30 s was tight under the new concurrency.
	defaultTimeout time.Duration
}

// newBreadthFetcher returns a spx.BarFetcher backed by getConn. nil
// from getConn maps to a "gateway unavailable" error from FetchDaily.
func newBreadthFetcher(getConn func() *ibkrlib.Connector) *breadthFetcher {
	return &breadthFetcher{
		getConn:        getConn,
		defaultTimeout: 2 * time.Minute,
	}
}

// FetchDaily satisfies spx.BarFetcher. ctx propagates into the connector's
// paced send and response wait; the connector clamps the effective budget
// to ctx's deadline itself (historicalTimeoutWithinContext), so
// defaultTimeout only rules when ctx carries no deadline.
func (f *breadthFetcher) FetchDaily(ctx context.Context, symbol string, lookbackDays int) ([]spx.Bar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := f.getConn()
	if c == nil {
		return nil, fmt.Errorf("breadth fetcher: no gateway connector")
	}
	raw, err := c.FetchHistoricalDailyBars(ctx, symbol, lookbackDays, f.defaultTimeout)
	if err != nil {
		return nil, err
	}
	out := make([]spx.Bar, 0, len(raw))
	for _, b := range raw {
		date := b.Date
		if !b.Time.IsZero() {
			date = b.Time.Format("2006-01-02")
		}
		// Skip bars with no parseable date — the engine's window
		// merge relies on date strings as monotonic keys, and an
		// empty date would silently collapse multiple bars into one
		// same-day overwrite.
		if date == "" {
			continue
		}
		out = append(out, spx.Bar{Date: date, Close: b.Close})
	}
	return out, nil
}
