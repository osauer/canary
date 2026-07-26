package daemon

import (
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// greeksCache memoises per-option model-computation Greeks so the
// positions handler doesn't churn a fresh option subscription for every
// held leg on every invocation. The keys are the OPRA-style option
// market-data keys (the same shape SubscribeOption returns), so callers
// in handlePositionsList build the same key from the position's contract
// fields and look up directly.
//
// TTL is tuned for actionability — Greeks shift slowly relative to spot
// (delta drifts on the order of minutes for liquid names), and a stale
// cached value is far better than nil for the portfolio-aggregate
// rendering. 60 s strikes the balance: short enough that an aggressive
// `watch -n 60 canary positions` re-warms each cycle, long enough that
// back-to-back invocations during a decision pause cost zero gateway
// round trips.
//
// Negative caching is essential. The model-computation tick (msg 21
// tickType 13) silently drops for far-OTM and illiquid OOH legs; we
// still want to remember "we tried and got nothing" so we don't re-
// poll a dead stream on the next call.
//
// Negative entries use a much shorter TTL than positive entries. A
// cold-daemon prewarm commonly fails to receive model ticks within
// the 2.5 s budget — the option-tick pipeline takes a few seconds to
// settle on a fresh connector. Under a single 60 s TTL, that one
// transient miss locked retries out for a full minute, well past the
// point the gateway started delivering ticks. A short negative TTL
// lets the next prewarm re-subscribe and capture the live values
// promptly, while still protecting the gateway from rapid re-poll
// loops within a few seconds.
type greeksCache struct {
	inner *ttlMap[string, greeksEntry]
}

type greeksEntry struct {
	value      ibkrlib.Greeks
	underlying float64 // model-computation underlying price, 0 if unavailable
	ok         bool    // false → negative cache: we tried and got nothing valid
}

const (
	// greeksTTL bounds positive entries — captured Greeks shift slowly
	// relative to spot (delta drifts on the order of minutes for liquid
	// names), and a stale cached value is far better than nil for the
	// portfolio aggregate. 60 s is short enough that an aggressive
	// `watch -n 60 canary positions` re-warms each cycle, long enough that
	// back-to-back invocations during a decision pause cost zero gateway
	// round trips.
	greeksTTL = 60 * time.Second

	// greeksNegativeTTL bounds ok=false entries. Held short so a single
	// cold-handshake miss doesn't lock out retries for a full minute —
	// see type-doc comment. Long enough to suppress a tight retry loop
	// from a misbehaving caller within the same RPC tick.
	greeksNegativeTTL = 10 * time.Second
)

func newGreeksCache() *greeksCache {
	return &greeksCache{
		inner: newTTLMap[string, greeksEntry](func(e greeksEntry, _ time.Time) time.Duration {
			if !e.ok {
				return greeksNegativeTTL
			}
			return greeksTTL
		}),
	}
}

func (c *greeksCache) get(key string, now time.Time) (greeksEntry, bool) {
	return c.inner.get(key, now)
}

func (c *greeksCache) put(key string, e greeksEntry, now time.Time) {
	c.inner.put(key, e, now)
}
