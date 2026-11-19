package ibkr

import (
	"context"
	"fmt"
	"time"
)

// optionHistoryMaxDays caps one option read at a year of daily bars: a
// request of 365 days or fewer keeps IBKR's day-count duration.
const optionHistoryMaxDays = 365

// FetchOptionDailyTradeBars reads the daily TRADES bars of one exact option
// contract. The contract id is the whole identity: the request carries no
// symbol, and nothing on this path consults the symbol-keyed contract cache
// or resolves contract details, so a cached listing of the underlying can
// never stand in for the option. A session with no trade has no bar; nothing
// falls back to midpoint data. IBKR serves these bars only while the
// contract is alive.
//
// The shared historical client still owns pacing, registration and
// cancellation. The label the request is registered under is the contract
// id, so a pacing backoff is kept per option and never against a stock.
func (c *Connector) FetchOptionDailyTradeBars(ctx context.Context, contract Contract, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
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
	return c.fetchHistoricalWithContractOptions(ctx, fmt.Sprintf("OPT %d", contract.ConID), exact, lookbackDays, timeout, "TRADES", historicalRequestOptions{})
}
