package ibkr

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// optionHistoryMaxDays is the span of one option read. IBKR serves hourly
// bars a month at a time, which covers every horizon Edge scores (20
// sessions) for a contract read at least once in that month.
const optionHistoryMaxDays = 30

// FetchOptionSessionCloses reads, for one exact option contract, the bid/ask
// midpoint at the end of each regular session: hourly MIDPOINT bars of
// regular hours, each New York day's last one kept as that day's bar.
//
// Why this shape (IBKR's historical-data limitations, and live answers on
// 2026-11-19): end-of-day data is not offered for options, so a "1 day"
// request is refused ("No data of type EODChart is available for the
// exchange 'BEST' and the security type 'Option'"); intraday bars are, as
// TRADES, MIDPOINT, BID, ASK or BID_ASK. Hourly TRADES bars went unanswered
// for 45 seconds on each of three contracts: a thin contract has few prints
// for the history service to find. A quote exists in every session of a live
// contract, and a closing midpoint is the same kind of price as the
// statement mark it stands beside. It is not a traded price, and callers
// must not present it as one. IBKR serves these bars only while the contract
// is alive.
//
// The contract id is the whole identity: the request carries no symbol, and
// nothing on this path consults the symbol-keyed contract cache or resolves
// contract details, so a cached listing of the underlying can never stand in
// for the option. The shared historical client still owns pacing,
// registration and cancellation, and the request is registered under the
// contract id, so a pacing backoff is kept per option and never against a
// stock.
func (c *Connector) FetchOptionSessionCloses(ctx context.Context, contract Contract, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contract = normalizeMarketDataContract(contract)
	if contract.ConID <= 0 || contract.SecType != "OPT" {
		return nil, fmt.Errorf("option history requires an exact option contract ID")
	}
	if !c.IsReady() {
		return nil, fmt.Errorf("IBKR connection not ready: %w", ErrIBKRUnavailable)
	}
	if lookbackDays <= 0 || lookbackDays > optionHistoryMaxDays {
		lookbackDays = optionHistoryMaxDays
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	exact := Contract{ConID: contract.ConID, SecType: "OPT", Exchange: "SMART", Currency: contract.Currency}
	hourly, err := c.fetchHistoricalWithContractOptions(ctx, fmt.Sprintf("OPT %d", contract.ConID), exact, lookbackDays, timeout, "MIDPOINT",
		historicalRequestOptions{formatDate: 2, chartBarSize: "1 hour", strictDaily: true, waitForEnd: true, maxBars: ChartMaxBars})
	if err != nil {
		return nil, err
	}
	return lastBarOfEachNewYorkDay(hourly), nil
}

// lastBarOfEachNewYorkDay folds intraday bars into one bar per New York
// day, the day's last, dated at that day.
func lastBarOfEachNewYorkDay(bars []HistoricalBar) []HistoricalBar {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.FixedZone("EST", -5*60*60)
	}
	last := map[string]HistoricalBar{}
	for _, bar := range bars {
		if bar.Time.IsZero() || bar.Close <= 0 {
			continue
		}
		day := bar.Time.In(ny).Format("20060102")
		if kept, ok := last[day]; !ok || bar.Time.After(kept.Time) {
			last[day] = bar
		}
	}
	out := make([]HistoricalBar, 0, len(last))
	for _, day := range slices.Sorted(func(yield func(string) bool) {
		for day := range last {
			if !yield(day) {
				return
			}
		}
	}) {
		bar := last[day]
		at, _ := time.Parse("20060102", day)
		bar.Date, bar.Time = day, at
		out = append(out, bar)
	}
	return out
}
