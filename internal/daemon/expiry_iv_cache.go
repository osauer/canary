package daemon

import "time"

// expiryIVCache memoises per-(symbol, expiry) ATM implied volatility lookups
// so repeated `canary chain SYM` calls within the TTL skip the per-expiry
// market-data subscribe cycle. The cache lives on the daemon (single
// process) and survives across CLI invocations — that's the whole point:
// the first call pays the gateway round-trip cost; the next ten do not.
//
// TTL varies with market phase: short during regular trading hours when
// IV moves intraday, long outside RTH when nothing is recomputing. The
// dividing line is intentionally generous (3am – 9pm ET catches all four
// US sessions plus a buffer) so we don't have to reason about overnight
// futures vs equities.
type expiryIVCache struct {
	inner *ttlMap[expiryIVKey, expiryIVEntry]
}

type expiryIVKey struct {
	symbol string // upper-cased
	expiry string // YYYY-MM-DD
}

type expiryIVEntry struct {
	iv      float64 // 0 when status != "ok"
	status  string  // "ok" | "timeout" | "unavailable"
	source  string  // "live_model" | "unavailable"
	quality string  // "live_model" | "unavailable"
	asOf    time.Time
}

func newExpiryIVCache() *expiryIVCache {
	return &expiryIVCache{
		inner: newTTLMap[expiryIVKey, expiryIVEntry](func(_ expiryIVEntry, now time.Time) time.Duration {
			return expiryIVTTL(now)
		}),
	}
}

// get returns (entry, true) when a non-stale entry exists for the key.
// Staleness is decided against now per expiryIVTTL — callers don't have
// to thread their own clock in (tests inject via testNow if needed).
func (c *expiryIVCache) get(symbol, expiry string, now time.Time) (expiryIVEntry, bool) {
	return c.inner.get(expiryIVKey{symbol: symbol, expiry: expiry}, now)
}

// put records the IV result. Negative-cache "timeout" and "unavailable"
// entries get the same TTL as successful fills — without that, a single
// dead expiry would be re-fetched on every chain refresh and chew through
// the gateway's market-data slot budget.
func (c *expiryIVCache) put(symbol, expiry string, e expiryIVEntry, now time.Time) {
	c.inner.put(expiryIVKey{symbol: symbol, expiry: expiry}, e, now)
}

// expiryIVTTL picks the freshness budget for a cached IV based on whether
// now falls in a US-equities-active window. We don't have a market phase
// per symbol on the daemon side — every option here is on a US underlying,
// so wall-clock America/New_York is the right proxy. Errors loading the
// zone (rare; would mean the system has no tzdata) fall back to UTC and
// the conservative 60s TTL.
//
// 9:30am – 4pm ET, Mon–Fri  → 60 s   (IV moves intraday, freshness matters)
// any other hour            →  4 h   (IV is approximately static; caching
//
//	hard avoids burning slots overnight)
var expiryIVNYZone = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func expiryIVTTL(now time.Time) time.Duration {
	local := now.In(expiryIVNYZone)
	wd := local.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return 4 * time.Hour
	}
	hour := local.Hour()
	min := local.Minute()
	// 9:30 – 16:00 ET ≡ minutes-since-midnight in [570, 960).
	mins := hour*60 + min
	if mins >= 570 && mins < 960 {
		return 60 * time.Second
	}
	return 4 * time.Hour
}
