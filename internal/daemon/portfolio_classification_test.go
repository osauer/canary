package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func syntheticStockBook(n int) *rpc.PositionsResult {
	p := &rpc.PositionsResult{}
	for i := range n {
		sym := fmt.Sprintf("ZZQ%02d", i)
		row := rpc.PositionView{ConID: 1000 + i, Symbol: sym, SecType: "STK", Currency: "USD", Exchange: "SMART", PrimaryExch: "NASDAQ", Quantity: 1, Mark: 10}
		p.Stocks = append(p.Stocks, row)
		stock := row
		p.ByUnderlying = append(p.ByUnderlying, rpc.PositionGroup{Underlying: sym, Stock: &stock})
	}
	return p
}

// A second snapshot in the same session must not ask the broker again; a
// new session must. A failed lookup is never remembered.
func TestClassificationCacheCannotRepeatALookupWithinASessionOrKeepOneAcrossSessions(t *testing.T) {
	cache := newClassificationCache[string]()
	var calls atomic.Int32
	fail := atomic.Bool{}
	lookup := func(_ context.Context, c ibkrlib.Contract) (ibkrlib.MarketClassification, error) {
		calls.Add(1)
		if fail.Load() {
			return ibkrlib.MarketClassification{}, errors.New("timeout")
		}
		return ibkrlib.MarketClassification{Industry: "Technology", Category: "Software"}, nil
	}
	book := syntheticStockBook(3)
	first := resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-a", lookup, nil))
	if calls.Load() != 3 || first["ZZQ00"].Sector != "Information Technology" {
		t.Fatalf("first pass calls=%d classes=%+v", calls.Load(), first)
	}
	resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-a", lookup, nil))
	if calls.Load() != 3 {
		t.Fatalf("second pass in the same session re-resolved: calls=%d", calls.Load())
	}
	fail.Store(true)
	third := resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-b", lookup, nil))
	if calls.Load() != 6 || third["ZZQ00"].Sector != "" {
		t.Fatalf("session change kept the old cache or a failure became a sector: calls=%d classes=%+v", calls.Load(), third)
	}
	fail.Store(false)
	fourth := resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-b", lookup, nil))
	if calls.Load() != 9 || fourth["ZZQ00"].Sector != "Information Technology" {
		t.Fatalf("failed lookups were remembered: calls=%d classes=%+v", calls.Load(), fourth)
	}
}

// A book larger than the old twelve-name bound classifies every name.
func TestClassificationCannotStopAtTwelveNames(t *testing.T) {
	book := syntheticStockBook(15)
	resolve := func(_ context.Context, c ibkrlib.Contract) (ibkrlib.MarketClassification, bool) {
		return ibkrlib.MarketClassification{Industry: "Financial", Category: "Banks"}, true
	}
	classes := resolveUnderlyingClassifications(context.Background(), book, resolve)
	if len(classes) != 15 {
		t.Fatalf("classes=%d", len(classes))
	}
	for sym, c := range classes {
		if c.Sector != "Financials" {
			t.Fatalf("%s unclassified: %+v", sym, c)
		}
	}
}

// The broker's own "no security definition" answer is a confirmed empty
// classification for the session, not a failure to retry on every snapshot.
func TestClassificationRemembersTheBrokersDefinitionVerdictForTheSession(t *testing.T) {
	cache := newClassificationCache[string]()
	var calls atomic.Int32
	lookup := func(_ context.Context, c ibkrlib.Contract) (ibkrlib.MarketClassification, error) {
		calls.Add(1)
		return ibkrlib.MarketClassification{}, fmt.Errorf("resolve: %w", ibkrlib.ErrContractNoDefinition)
	}
	book := syntheticStockBook(2)
	first := resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-a", lookup, nil))
	if calls.Load() != 2 || first["ZZQ00"].Sector != "" {
		t.Fatalf("first pass calls=%d classes=%+v", calls.Load(), first)
	}
	resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-a", lookup, nil))
	if calls.Load() != 2 {
		t.Fatalf("the verdict was asked for again within the session: calls=%d", calls.Load())
	}
	resolveUnderlyingClassifications(context.Background(), book, cachedClassificationResolver(cache, "session-b", lookup, nil))
	if calls.Load() != 4 {
		t.Fatalf("a new session must ask again: calls=%d", calls.Load())
	}
}
