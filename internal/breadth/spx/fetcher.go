package spx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoDefinition is returned by a BarFetcher when the broker has no
// contract definition for the symbol. The engine leaves such a name out of
// the fetch plan until the next completed session: the answer does not
// change within one, and asking again on every retry pass only repeats it.
var ErrNoDefinition = errors.New("no security definition (skipped until the next session)")

// Bar is the engine's view of one daily price bar — just the date
// and close, since 50-DMA breadth needs nothing else. Decoupling
// from ibkr.HistoricalBar (which carries open/high/low/volume the
// engine doesn't use) keeps the spx package free of any gateway
// dependency, so tests can fake the fetcher without importing the
// connector.
type Bar struct {
	Date  string // "YYYY-MM-DD"
	Close float64
}

// BarFetcher is the daily-bar source the engine pulls from. The
// production implementation in internal/daemon/breadth_fetcher.go
// wraps *ibkr.Connector.FetchHistoricalDailyBars; unit tests use
// FakeBarFetcher below.
//
// Contract:
//   - FetchDaily returns bars in chronological order, oldest first.
//   - lookbackDays is a calendar-day duration, not a bar count.
//     Weekends, holidays and listing dates reduce the returned bars.
//   - Errors per-symbol are non-fatal to the engine: a refresh that
//     loses some names still returns a partial result rather than
//     failing the whole call.
//   - Cancellation honours the supplied context. A long-running
//     fetch must bail when ctx.Done() fires.
type BarFetcher interface {
	FetchDaily(ctx context.Context, symbol string, lookbackDays int) ([]Bar, error)
}

// FakeBarFetcher is a test-only BarFetcher. Routes calls to a canned
// map of bars per symbol; missing symbols return an error so tests
// can assert engine behaviour when individual fetches fail. Safe for
// concurrent use — refresh tests fan calls out across workers.
type FakeBarFetcher struct {
	mu sync.Mutex
	// Bars is the canned response per symbol. The fetcher trims to
	// the requested lookback length so test inputs can be a single
	// long series shared across cases.
	Bars map[string][]Bar
	// Errors injects per-symbol failures (e.g. simulate a gateway
	// throttle). When a symbol is in both Bars and Errors, Errors
	// wins.
	Errors map[string]error
	// Latency makes FetchDaily sleep before returning. Used to test
	// the worker-pool concurrency and context cancellation paths.
	Latency time.Duration
	// Calls records every (symbol, lookbackDays) pair the engine
	// has invoked, so tests can assert what the refresh planner
	// asked for without instrumenting the engine.
	Calls []FakeCall
}

// FakeCall is one (symbol, lookbackDays) the engine asked for.
type FakeCall struct {
	Symbol       string
	LookbackDays int
}

// FetchDaily satisfies the BarFetcher interface. ctx-aware: returns
// early if ctx is cancelled during the simulated Latency.
func (f *FakeBarFetcher) FetchDaily(ctx context.Context, symbol string, lookbackDays int) ([]Bar, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Symbol: symbol, LookbackDays: lookbackDays})
	bars := f.Bars[symbol]
	err := f.Errors[symbol]
	latency := f.Latency
	f.mu.Unlock()

	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if bars == nil {
		return nil, fmt.Errorf("fake: no canned bars for %s", symbol)
	}
	// Trim by calendar duration relative to the latest canned observation.
	if lookbackDays > 0 && len(bars) > 0 {
		if latest, err := time.Parse("2006-01-02", bars[len(bars)-1].Date); err == nil {
			cutoff := latest.AddDate(0, 0, -lookbackDays).Format("2006-01-02")
			for len(bars) > 0 && bars[0].Date < cutoff {
				bars = bars[1:]
			}
		}
	}

	return append([]Bar(nil), bars...), nil
}

// CallCount returns the number of recorded fetch attempts. Convenience
// for test assertions on planner behaviour ("expected exactly N
// fetches for a cold start, got M").
func (f *FakeBarFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}
