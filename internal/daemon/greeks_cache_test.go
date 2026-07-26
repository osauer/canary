package daemon

import (
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestGreeksCacheRoundTrip: put then get within the TTL returns the
// stored value; the second-level cache key drives the renderer's
// nil-vs-value decision, so the round-trip semantics must be precise.
func TestGreeksCacheRoundTrip(t *testing.T) {
	c := newGreeksCache()
	now := time.Now()
	g := ibkrlib.Greeks{Delta: 0.5, Theta: -0.1}
	c.put("AAPL_260117C200", greeksEntry{value: g, ok: true}, now)

	got, ok := c.get("AAPL_260117C200", now)
	if !ok {
		t.Fatalf("expected cache hit, got miss")
	}
	if got.value.Delta != 0.5 || got.value.Theta != -0.1 {
		t.Errorf("value mismatch: %+v", got.value)
	}
	if !got.ok {
		t.Errorf("ok bit lost across roundtrip")
	}
}

// TestGreeksCacheTTLExpiry: an entry older than TTL must miss so the
// caller re-fetches. The TTL is deliberately short relative to a
// session so back-to-back invocations are free but mid-session drift
// still refreshes.
func TestGreeksCacheTTLExpiry(t *testing.T) {
	c := newGreeksCache()
	stale := time.Now().Add(-2 * greeksTTL)
	c.put("AAPL_260117C200", greeksEntry{value: ibkrlib.Greeks{Delta: 0.5}, ok: true}, stale)

	if _, ok := c.get("AAPL_260117C200", time.Now()); ok {
		t.Fatalf("expected stale entry to miss, got hit")
	}
}

// TestGreeksCacheNegativeEntry: an ok=false entry IS a cache hit (we
// remember we tried) — the renderer treats it as "tried and got
// nothing" rather than "haven't tried yet". This is what keeps the
// next positions call from re-polling a dead leg every time.
func TestGreeksCacheNegativeEntry(t *testing.T) {
	c := newGreeksCache()
	now := time.Now()
	c.put("AAPL_260117C200", greeksEntry{ok: false}, now)
	got, ok := c.get("AAPL_260117C200", now)
	if !ok {
		t.Fatalf("negative cache entry should hit within TTL")
	}
	if got.ok {
		t.Errorf("ok bit should be false on negative entry")
	}
}

// TestGreeksCacheNegativeTTLShorterThanPositive: a negative entry past
// the short negative TTL must miss so the caller re-subscribes, even
// though a positive entry at the same age would still hit. Without
// this asymmetry, a cold-daemon prewarm miss would lock retries out
// for the full positive TTL — well past the point the connector has
// warmed and the gateway is delivering model ticks again.
func TestGreeksCacheNegativeTTLShorterThanPositive(t *testing.T) {
	if greeksNegativeTTL >= greeksTTL {
		t.Fatalf("negative TTL (%v) must be strictly shorter than positive TTL (%v) — the asymmetry is the point", greeksNegativeTTL, greeksTTL)
	}
	c := newGreeksCache()
	stale := time.Now().Add(-(greeksNegativeTTL + time.Second))

	c.put("AAPL_260117C200", greeksEntry{ok: false}, stale)
	if _, ok := c.get("AAPL_260117C200", time.Now()); ok {
		t.Errorf("negative entry past negativeTTL should miss so retries unblock")
	}

	// Same age, but positive — must still hit. Pins the asymmetry.
	c.put("AAPL_260117P200", greeksEntry{
		value: ibkrlib.Greeks{Delta: 0.5},
		ok:    true,
	}, stale)
	if _, ok := c.get("AAPL_260117P200", time.Now()); !ok {
		t.Errorf("positive entry within positiveTTL must still hit at the same age — positive cache is load-bearing for back-to-back calls")
	}
}
