package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// addContractCache keeps the broker's answers to the lookups option
// discovery (add market, strategy spreads) repeats: an exact contract's
// resolution and an underlying's option chain. On 2026-11-24 each expiry
// spent 1.2 to 3.4 s looking up contracts, and the Add sheet repeats the same
// lookups on every open and every refresh. Identities only: no price is kept,
// and every quote is read fresh with its own clock.
//
// What invalidates an entry:
//   - the New York trading date changing: a resolution carries the contract's
//     dated trading and liquid hours (the class close Add reads) and the
//     expiries roll;
//   - the gateway connector being replaced;
//   - for an option chain, and for the broker's code-200 "no definition"
//     verdict on a strike, addListingFor: exchanges list new strikes during
//     the day when the price moves, so a chain or an absent strike is not
//     kept for the whole day. A timeout or any other failure is never kept.
//
// The order path (previews, call plans, strategy orders) resolves through the
// broker every time and never reads this cache.
type addContractCache struct {
	mu        sync.Mutex
	connector *ibkr.Connector
	day       string
	resolved  map[addContractKey]addResolution
	chains    map[addContractKey]addChain
}

// addListingFor is how long an option chain, or a strike the broker said
// does not exist, is kept.
const addListingFor = 15 * time.Minute

// addCacheMaxEntries bounds one day's resolutions; past it the day starts
// over. Discovery resolves at most a few dozen strikes a symbol.
const addCacheMaxEntries = 4096

type addContractKey struct {
	conID                                       int
	symbol, secType, exchange, currency, expiry string
	right, tradingClass, primaryExch            string
	strike                                      float64
	multiplier                                  int
}

type addResolution struct {
	exact  ibkr.ResolvedOrderContract
	absent bool
	at     time.Time
}

type addChain struct {
	listing map[string][]ibkr.ExpiryClassedStrikes
	at      time.Time
}

func addContractKeyOf(c ibkr.Contract) addContractKey {
	return addContractKey{conID: c.ConID, symbol: c.Symbol, secType: c.SecType, exchange: c.Exchange, currency: c.Currency, expiry: c.Expiry,
		right: c.Right, tradingClass: c.TradingClass, primaryExch: c.PrimaryExch, strike: c.Strike, multiplier: c.Multiplier}
}

// currentLocked starts the cache over when the connector or the New York
// date changed, or the day's entries passed their bound. The caller holds mu.
func (a *addContractCache) currentLocked(c *ibkr.Connector, now time.Time) {
	day := addTradingDate(now)
	if a.connector != c || a.day != day || len(a.resolved) > addCacheMaxEntries {
		a.connector, a.day = c, day
		a.resolved = map[addContractKey]addResolution{}
		a.chains = map[addContractKey]addChain{}
	}
}

func addTradingDate(now time.Time) string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return now.UTC().Format("20060102")
	}
	return now.In(loc).Format("20060102")
}

// addResolveContract is c.ResolveOrderContractForSession for discovery,
// answered from the day's cache when the same contract was resolved before.
// A cached answer is not bound to binding's session; callers check the
// session is current before they return, as they do for a fresh one.
func (s *Server) addResolveContract(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, requested ibkr.Contract, timeout time.Duration) (ibkr.ResolvedOrderContract, error) {
	key := addContractKeyOf(requested)
	now := time.Now()
	a := &s.addContracts
	a.mu.Lock()
	a.currentLocked(c, now)
	if r, ok := a.resolved[key]; ok && (!r.absent || now.Sub(r.at) < addListingFor) {
		a.mu.Unlock()
		if r.absent {
			return ibkr.ResolvedOrderContract{}, ibkr.ErrContractNoDefinition
		}
		return r.exact, nil
	}
	a.mu.Unlock()
	exact, err := c.ResolveOrderContractForSession(ctx, binding, requested, timeout)
	if err != nil && !errors.Is(err, ibkr.ErrContractNoDefinition) {
		return exact, err
	}
	a.mu.Lock()
	a.currentLocked(c, now)
	a.resolved[key] = addResolution{exact: exact, absent: err != nil, at: now}
	a.mu.Unlock()
	return exact, err
}

// addOptionChain is c.FetchSetupOptionStrikes for discovery, kept for
// addListingFor. The listing is shared: callers read it and never change it.
func (s *Server) addOptionChain(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, timeout time.Duration) (map[string][]ibkr.ExpiryClassedStrikes, error) {
	key := addContractKeyOf(under)
	now := time.Now()
	a := &s.addContracts
	a.mu.Lock()
	a.currentLocked(c, now)
	if chain, ok := a.chains[key]; ok && now.Sub(chain.at) < addListingFor {
		a.mu.Unlock()
		return chain.listing, nil
	}
	a.mu.Unlock()
	listing, err := c.FetchSetupOptionStrikes(ctx, binding, under, timeout)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.currentLocked(c, now)
	a.chains[key] = addChain{listing: listing, at: now}
	a.mu.Unlock()
	return listing, nil
}
