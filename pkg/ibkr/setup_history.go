package ibkr

import (
	"context"
	"fmt"
	"math"
	"time"
)

// FetchSetupBars reads at most seven days of exact-contract, five-minute RTH
// TRADES bars. End is explicit for bounded historical reconstruction; the
// shared historical client still owns pacing, registration and cancellation.
func (c *Connector) FetchSetupBars(ctx context.Context, contract Contract, start, end time.Time, timeout time.Duration) ([]HistoricalBar, error) {
	if err := validateSetupHistoryWindow(contract, start, end); err != nil {
		return nil, err
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return nil, fmt.Errorf("broker session unavailable")
	}
	if _, terminal := c.reviewedTerminalReason(MarketDataKeyForContract(normalizeMarketDataContract(contract))); terminal {
		return nil, ErrSymbolInactive
	}
	days := int(math.Ceil(end.Sub(start).Hours() / 24))
	bars, err := c.fetchHistoricalWithContractOptions(ctx, contract.Symbol, contract, days, timeout, "TRADES", historicalRequestOptions{endDateTime: end.UTC().Format("20060102-15:04:05"), formatDate: 2, chartBarSize: "5 mins", strictDaily: true, waitForEnd: true, maxBars: ChartMaxBars})
	if !c.HistoricalSessionCurrent(binding) {
		return nil, fmt.Errorf("broker session changed during setup history")
	}
	if err != nil {
		return nil, err
	}
	out := make([]HistoricalBar, 0, len(bars))
	for _, bar := range bars {
		if !bar.Time.Before(start) && bar.Time.Before(end) {
			out = append(out, bar)
		}
	}
	return out, nil
}

func validateSetupHistoryWindow(contract Contract, start, end time.Time) error {
	if contract.ConID <= 0 || contract.SecType != "STK" || contract.Currency != "USD" {
		return fmt.Errorf("setup history requires an exact USD stock contract")
	}
	if start.IsZero() || end.IsZero() || !end.After(start) || end.Sub(start) > 7*24*time.Hour || end.After(time.Now()) {
		return fmt.Errorf("setup history window must be past, positive and at most seven days")
	}
	return nil
}
