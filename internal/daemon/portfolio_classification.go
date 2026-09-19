package daemon

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// classificationKey identifies one routed stock contract for the purpose
// of remembering the broker's business classification of it.
type classificationKey struct {
	Symbol, SecType, Currency, Exchange, PrimaryExch string
}

func classificationKeyOf(c ibkrlib.Contract) classificationKey {
	return classificationKey{
		Symbol: strings.ToUpper(c.Symbol), SecType: strings.ToUpper(c.SecType), Currency: strings.ToUpper(c.Currency),
		Exchange: strings.ToUpper(c.Exchange), PrimaryExch: strings.ToUpper(c.PrimaryExch),
	}
}

// classificationCache remembers broker classifications for exactly one
// connector session. A different binding empties it: contract details are
// stable within a session, and re-resolving them on every snapshot is what
// bounded the old lookup to twelve names. Confirmed-empty answers are cached
// too; transport failures and timeouts are not stored, so the next call
// tries again. The binding type is a parameter so tests can drive session
// changes without a live connector.
type classificationCache[B comparable] struct {
	mu      sync.Mutex
	binding B
	entries map[classificationKey]ibkrlib.MarketClassification
}

func newClassificationCache[B comparable]() *classificationCache[B] {
	return &classificationCache[B]{entries: map[classificationKey]ibkrlib.MarketClassification{}}
}

func (c *classificationCache[B]) rebind(binding B) {
	if c.binding != binding {
		c.binding = binding
		c.entries = map[classificationKey]ibkrlib.MarketClassification{}
	}
}

func (c *classificationCache[B]) get(binding B, key classificationKey) (ibkrlib.MarketClassification, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebind(binding)
	mc, ok := c.entries[key]
	return mc, ok
}

func (c *classificationCache[B]) put(binding B, key classificationKey, mc ibkrlib.MarketClassification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebind(binding)
	c.entries[key] = mc
}

// classificationResolver answers the broker's classification for one
// routed contract, or false when the answer is unavailable this call.
type classificationResolver func(ctx context.Context, contract ibkrlib.Contract) (ibkrlib.MarketClassification, bool)

// Lookup bounds. Four workers keep the contract-details lane shared with
// interactive calls; the phase deadline keeps a large book inside the RPC
// budget on its first, uncached snapshot. Names left unresolved by the
// deadline are unclassified for this call and resolve on the next one.
const (
	classificationWorkers       = 4
	classificationLookupTimeout = 3 * time.Second
	classificationPhaseDeadline = 20 * time.Second
)

// resolveUnderlyingClassifications classifies every underlying group in
// the book. The free sources decide S&P names and the embedded funds; the
// resolver is asked only where they are silent and the group is unambiguous
// and expected to quote.
func resolveUnderlyingClassifications(ctx context.Context, p *rpc.PositionsResult, resolve classificationResolver) map[string]underlyingClassification {
	broker := map[string]ibkrlib.MarketClassification{}
	var mu sync.Mutex
	if resolve != nil {
		phase, cancel := context.WithTimeout(ctx, classificationPhaseDeadline)
		defer cancel()
		var wg sync.WaitGroup
		sem := make(chan struct{}, classificationWorkers)
		for _, g := range p.ByUnderlying {
			if classificationSettledWithoutBroker(g.Underlying) || !portfolioClassificationUnambiguous(g.Underlying, p) {
				continue
			}
			contract, ok := rpc.UnderlyingMarketContract(g)
			if !ok || !rpc.ExpectsMarketDataGroup(g) {
				continue
			}
			wg.Go(func() {
				select {
				case sem <- struct{}{}:
				case <-phase.Done():
					return
				}
				defer func() { <-sem }()
				route, _, _, err := normaliseStockQuoteContract(contract)
				if err != nil {
					return
				}
				mc, ok := resolve(phase, route)
				if !ok {
					return
				}
				mu.Lock()
				broker[strings.ToUpper(g.Underlying)] = mc
				mu.Unlock()
			})
		}
		wg.Wait()
	}
	classes := make(map[string]underlyingClassification, len(p.ByUnderlying))
	for _, g := range p.ByUnderlying {
		key := strings.ToUpper(g.Underlying)
		classes[key] = classifyUnderlying(key, broker[key])
	}
	return classes
}

// brokerClassificationResolver wraps the connector's contract-details read
// in the session cache.
func (s *Server) brokerClassificationResolver(c *ibkrlib.Connector, binding ibkrlib.ConnectorSessionBinding) classificationResolver {
	return cachedClassificationResolver(s.classifications, binding, func(ctx context.Context, contract ibkrlib.Contract) (ibkrlib.MarketClassification, error) {
		return c.MarketClassification(ctx, contract, classificationLookupTimeout)
	}, s.logger)
}

// cachedClassificationResolver answers from the cache for the given
// session binding and asks lookup otherwise. A confirmed answer, including
// an empty one, is remembered for the session; an error is not.
func cachedClassificationResolver[B comparable](cache *classificationCache[B], binding B, lookup func(context.Context, ibkrlib.Contract) (ibkrlib.MarketClassification, error), logger *Logger) classificationResolver {
	return func(ctx context.Context, contract ibkrlib.Contract) (ibkrlib.MarketClassification, bool) {
		key := classificationKeyOf(contract)
		if mc, ok := cache.get(binding, key); ok {
			return mc, true
		}
		mc, err := lookup(ctx, contract)
		if err != nil {
			return ibkrlib.MarketClassification{}, false
		}
		if _, mapped := gicsFromIBKR(mc.Industry, mc.Category); !mapped && !isFundStockType(mc.StockType) && mc.Industry != "" && logger != nil {
			// A broker pairing the map does not know is worth a line: it is
			// how the map grows. No symbol: the pairing is not a holding.
			logger.Warnf("portfolio classification: broker industry %q category %q stock type %q has no GICS sector", mc.Industry, mc.Category, mc.StockType)
		}
		cache.put(binding, key, mc)
		return mc, true
	}
}
