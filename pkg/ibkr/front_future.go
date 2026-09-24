package ibkr

import (
	"context"
	"fmt"
	"time"
)

const (
	frontFutureCacheLimit = 64
	frontFutureCacheAge   = 6 * time.Hour
	frontFutureRetry      = 15 * time.Second
)

type frontFutureResolution struct {
	binding  ConnectorSessionBinding
	done     chan struct{}
	contract Contract
	err      error
	expires  time.Time
}

// The connector owns these identity-only flights. A canceled/short caller
// leaves at most the existing bounded contract-details wire request running;
// its terminal success can serve the next snapshot without another broker trip.
func (c *Connector) frontFuture(ctx context.Context, contract Contract, now time.Time, wait time.Duration, fetch func(Contract, time.Duration) ([]ContractDetailsLite, error)) (Contract, error) {
	if err := ctx.Err(); err != nil {
		return Contract{}, err
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return Contract{}, fmt.Errorf("broker session unavailable")
	}
	contract = normalizeMarketDataContract(contract)
	key := contractDetailsFlightKey(contract)
	c.frontFutureMu.Lock()
	if c.frontFutures == nil {
		c.frontFutures = make(map[string]*frontFutureResolution)
	}
	for k, entry := range c.frontFutures {
		if entry.binding != binding || !entry.expires.IsZero() && !now.Before(entry.expires) {
			delete(c.frontFutures, k)
		}
	}
	entry := c.frontFutures[key]
	if entry == nil {
		if len(c.frontFutures) >= frontFutureCacheLimit || c.frontFutureActive >= frontFutureCacheLimit {
			c.frontFutureMu.Unlock()
			return Contract{}, fmt.Errorf("front future resolution capacity reached")
		}
		entry = &frontFutureResolution{binding: binding, done: make(chan struct{})}
		c.frontFutures[key] = entry
		c.frontFutureActive++
		if fetch == nil {
			fetch = func(contract Contract, timeout time.Duration) ([]ContractDetailsLite, error) {
				return c.fetchFrontFutureDetails(binding, contract, timeout, func(budget time.Duration, observe func([]ContractDetailsLite)) ([]ContractDetailsLite, error) {
					return c.fetchContractDetailsForContractWire(contract, MarketDataKeyForContract(contract), budget, observe)
				})
			}
		}
		go c.acquireFrontFuture(entry, contract, now, fetch)
	}
	c.frontFutureMu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Contract{}, ctx.Err()
	case <-timer.C:
		return Contract{}, ErrContractDetailsTimeout
	case <-entry.done:
	}
	if err := ctx.Err(); err != nil {
		return Contract{}, err
	}
	if !c.SessionCurrent(binding) {
		return Contract{}, fmt.Errorf("broker session changed")
	}
	return entry.contract, entry.err
}

func (c *Connector) fetchFrontFutureDetails(binding ConnectorSessionBinding, contract Contract, timeout time.Duration, wire func(time.Duration, func([]ContractDetailsLite)) ([]ContractDetailsLite, error)) ([]ContractDetailsLite, error) {
	// Generic contract flights are not session keyed. Never join a request
	// started before reconnect while establishing this session's identity.
	key := fmt.Sprintf("front_future\x00%p\x00%d\x00%s", binding.connection, binding.epoch, contractDetailsFlightKey(contract))
	return c.coalesceContractDetails(key, timeout, func(budget time.Duration, observe func([]ContractDetailsLite)) ([]ContractDetailsLite, error) {
		if !c.SessionCurrent(binding) {
			return nil, fmt.Errorf("broker session changed")
		}
		return wire(budget, observe)
	})
}

func (c *Connector) acquireFrontFuture(entry *frontFutureResolution, contract Contract, now time.Time, fetch func(Contract, time.Duration) ([]ContractDetailsLite, error)) {
	started := time.Now()
	details, err := fetch(contract, contractDetailsSharedWireMinimum)
	completed := now.Add(time.Since(started))
	var resolved Contract
	if err == nil {
		resolved, err = selectFrontFuture(contract, details, completed)
	}
	if !c.SessionCurrent(entry.binding) {
		resolved, err = Contract{}, fmt.Errorf("broker session changed")
	}
	c.frontFutureMu.Lock()
	defer c.frontFutureMu.Unlock()
	c.frontFutureActive--
	entry.contract, entry.err = resolved, err
	entry.expires = completed.Add(frontFutureRetry)
	if err == nil {
		// Re-resolve on the next UTC date even if the six-hour lifetime spans
		// midnight. The selection excludes contracts on their expiry date.
		midnight := completed.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		entry.expires = completed.Add(frontFutureCacheAge)
		if midnight.Before(entry.expires) {
			entry.expires = midnight
		}
	}
	close(entry.done)
}
