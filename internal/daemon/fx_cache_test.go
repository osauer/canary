package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func testFXCacheAt(now *time.Time) *fxRateCache {
	c := newFXRateCache()
	c.now = func() time.Time { return *now }
	return c
}

func TestFXRateCacheFreshWindowAndTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cache := testFXCacheAt(&now)
	cache.put("EUR", "USD", 0.88)
	if rate, _, ok := cache.get("EUR", "USD", fxCacheFreshWindow); !ok || rate != 0.88 {
		t.Fatalf("fresh get = %v, %v; want 0.88, true", rate, ok)
	}
	now = now.Add(fxCacheFreshWindow + time.Minute)
	if _, _, ok := cache.get("EUR", "USD", fxCacheFreshWindow); ok {
		t.Fatal("fresh window lapsed; get must miss")
	}
	if rate, age, ok := cache.get("EUR", "USD", fxCacheTTL); !ok || rate != 0.88 || age <= fxCacheFreshWindow {
		t.Fatalf("TTL get = %v age=%v ok=%v; want 0.88 with age past fresh window", rate, age, ok)
	}
	now = now.Add(fxCacheTTL)
	if _, _, ok := cache.get("EUR", "USD", fxCacheTTL); ok {
		t.Fatal("TTL exceeded; get must miss")
	}
}

func TestFXRateStoreLoadRespectsTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	store := newFXRateStore(t.TempDir())
	if err := store.save(map[string]fxCachedRate{
		"EUR/USD": {rate: 0.88, at: now.Add(-time.Hour)},
		"EUR/JPY": {rate: 0.0061, at: now.Add(-fxCacheTTL - time.Minute)},
		"EUR/BAD": {rate: 0, at: now}, // corruption defense: non-positive rates never load
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.load(now)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded = %v; want only the within-TTL pair", loaded)
	}
	if got := loaded["EUR/USD"]; got.rate != 0.88 || !got.at.Equal(now.Add(-time.Hour)) {
		t.Fatalf("EUR/USD = %+v; want rate 0.88 with original timestamp", got)
	}
}

// A daemon restart during IBKR's nightly reset window (or a weekend)
// must serve last-known-good rates from disk, not start cold with nil
// *_base fields until the first successful live resolution.
func TestFXRateCachePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	first := newFXRateCacheWithStore(newFXRateStore(dir), func() time.Time { return base }, nil)
	first.put("EUR", "USD", 0.88)

	// Restart an hour later: past the fresh window (no quote-free serve)
	// but well within TTL — the persisted rate must be available as
	// last-known-good.
	restarted := newFXRateCacheWithStore(newFXRateStore(dir), func() time.Time { return base.Add(time.Hour) }, nil)
	if _, _, ok := restarted.get("EUR", "USD", fxCacheFreshWindow); ok {
		t.Fatal("hour-old persisted rate must not count as fresh")
	}
	if rate, _, ok := restarted.get("EUR", "USD", fxCacheTTL); !ok || rate != 0.88 {
		t.Fatalf("restarted TTL get = %v, %v; want persisted 0.88", rate, ok)
	}

	// Restart past the TTL: the persisted rate must not resurrect.
	expired := newFXRateCacheWithStore(newFXRateStore(dir), func() time.Time { return base.Add(fxCacheTTL + time.Hour) }, nil)
	if _, _, ok := expired.get("EUR", "USD", fxCacheTTL); ok {
		t.Fatal("TTL-expired persisted rate must not survive a restart")
	}
}

func TestCachedFXResolverServesLastKnownGoodOnLiveFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cache := testFXCacheAt(&now)
	s := &Server{fxRates: cache}
	// nil connector makes resolveBasePerCurrencyFXRate fail — this stands
	// in for the observed transient FX snapshot-quote timeouts.
	resolver := s.cachedFXResolver(nil)
	ctx := context.Background()
	if _, ok := resolver(ctx, "EUR", "USD", time.Millisecond); ok {
		t.Fatal("cold cache with failing live resolution must not resolve")
	}
	cache.put("EUR", "USD", 0.88)
	if rate, ok := resolver(ctx, "EUR", "USD", time.Millisecond); !ok || rate != 0.88 {
		t.Fatalf("fresh-window resolve = %v, %v; want cached 0.88", rate, ok)
	}
	now = now.Add(time.Hour)
	if rate, ok := resolver(ctx, "EUR", "USD", time.Millisecond); !ok || rate != 0.88 {
		t.Fatalf("stale-window fallback = %v, %v; want cached 0.88", rate, ok)
	}
	// The fallback above marked the pair degraded; a repeat must dedupe so
	// the WARN logs once per episode, not once per poll.
	if cache.markDegraded("EUR", "USD", true) {
		t.Fatal("degraded transition should already be recorded")
	}
	now = now.Add(fxCacheTTL)
	if _, ok := resolver(ctx, "EUR", "USD", time.Millisecond); ok {
		t.Fatal("cache older than TTL must not serve")
	}
}

func TestRepairCachedBackfillsSuspiciousRateFromHarvest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cache := testFXCacheAt(&now)
	s := &Server{fxRates: cache}
	ctx := context.Background()
	good := map[string]ibkrlib.CurrencyLedger{
		"EUR": {ExchangeRate: 1},
		"USD": {ExchangeRate: 0.88},
	}
	repaired := s.repairCurrencyLedgerFXRatesCached(ctx, nil, good, "EUR")
	if repaired["USD"].ExchangeRate != 0.88 {
		t.Fatalf("healthy ledger rate = %v; want 0.88 kept", repaired["USD"].ExchangeRate)
	}
	// Next poll: the streaming ledger degrades to the fake unit rate and
	// live quote resolution fails (nil connector). Before the cache this
	// zeroed the rate and stripped every *_base field for the response.
	now = now.Add(time.Minute)
	bad := map[string]ibkrlib.CurrencyLedger{
		"EUR": {ExchangeRate: 1},
		"USD": {ExchangeRate: 1},
	}
	repaired = s.repairCurrencyLedgerFXRatesCached(ctx, nil, bad, "EUR")
	if repaired["USD"].ExchangeRate != 0.88 {
		t.Fatalf("suspicious rate backfill = %v; want harvested 0.88", repaired["USD"].ExchangeRate)
	}
}

// The positions fingerprint hashes base-derived exposure; before the cache
// a single failed FX resolution emptied ExposureBase, flipped the
// fingerprint, and staled proposal revisions mid-confirm. Assert the
// fingerprint is identical between a healthy poll and a cache-served poll.
func TestPositionsFingerprintStableWhenFXServedFromCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cache := testFXCacheAt(&now)
	s := &Server{fxRates: cache}
	ctx := context.Background()
	nlv := 100000.0
	build := func(ledger map[string]ibkrlib.CurrencyLedger) rpc.Fingerprint {
		stocks := []rpc.PositionView{{
			Symbol: "MSFT", SecType: "STOCK", Quantity: 100, Mark: 400,
			MarketValue: 40000, Currency: "USD", Multiplier: 1,
		}}
		var options []rpc.PositionView
		fillFXRates(stocks, ledger, "EUR")
		fillBaseValues(stocks, "EUR")
		res := &rpc.PositionsResult{Stocks: stocks, Options: options}
		res.ByUnderlying = groupByUnderlying(stocks, options, "EUR", &nlv)
		res.Portfolio = buildPortfolioAggregatesWithBase(stocks, options, "EUR")
		addPortfolioBaseContext(res.Portfolio, res.ByUnderlying, "EUR", &nlv)
		return rpc.BuildPositionsFingerprint(res, nlv)
	}
	healthy := build(s.repairCurrencyLedgerFXRatesCached(ctx, nil, map[string]ibkrlib.CurrencyLedger{
		"EUR": {ExchangeRate: 1}, "USD": {ExchangeRate: 0.88},
	}, "EUR"))
	now = now.Add(time.Minute)
	cacheServed := build(s.repairCurrencyLedgerFXRatesCached(ctx, nil, map[string]ibkrlib.CurrencyLedger{
		"EUR": {ExchangeRate: 1}, "USD": {ExchangeRate: 1},
	}, "EUR"))
	if healthy.Key == "" || healthy.Key != cacheServed.Key {
		t.Fatalf("fingerprint flipped across cache-served poll: %q vs %q", healthy.Key, cacheServed.Key)
	}
}
