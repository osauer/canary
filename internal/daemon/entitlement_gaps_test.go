package daemon

import (
	"encoding/json"
	"testing"
	"time"
)

// A warned entitlement gap outlives a daemon restart: the retiring
// connector's gaps are recorded and seed the next process's first connector.
func TestEntitlementGapsSurviveARestart(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	t.Cleanup(func() { _ = store.Close() })
	first := time.Date(2026, 10, 6, 13, 45, 11, 0, time.UTC)

	before := &Server{coreStore: store}
	before.marketDataMemory = before.marketDataMemory.WithEntitlementGaps(map[string]time.Time{"NDX": first})
	before.persistEntitlementGaps(before.marketDataMemory)

	after := &Server{coreStore: store}
	after.loadEntitlementGaps()
	if got := after.marketDataMemory.EntitlementGaps(); !got["NDX"].Equal(first) || len(got) != 1 {
		t.Fatalf("restored gaps = %v", got)
	}
	// No store: nothing to load, nothing to record, no panic.
	bare := &Server{}
	bare.loadEntitlementGaps()
	bare.persistEntitlementGaps(before.marketDataMemory)
}

// A document recorded before exact-session keys were normalised holds one
// key per order preview; loading merges them into the instrument's gap at the
// earliest warning, so the instrument stays INFO and is recorded once.
func TestEntitlementGapsMergeExactSessionKeysOnLoad(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	t.Cleanup(func() { _ = store.Close() })
	first := time.Date(2026, 10, 7, 9, 12, 0, 0, time.UTC)
	instrument := "SYNTH|STK|SMART||USD|||CONID:4242"
	raw, err := json.Marshal(entitlementGapDoc{Version: 1, Gaps: map[string]time.Time{
		instrument + "|EXACT:7": first.Add(time.Hour),
		instrument + "|EXACT:3": first,
		"SYNTHIDX":              first,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveMarketDocument(t.Context(), store, entitlementGapScope, entitlementGapKind, raw); err != nil {
		t.Fatal(err)
	}
	s := &Server{coreStore: store}
	s.loadEntitlementGaps()
	got := s.marketDataMemory.EntitlementGaps()
	if len(got) != 2 || !got[instrument].Equal(first) || !got["SYNTHIDX"].Equal(first) {
		t.Fatalf("loaded gaps = %v, want %s and SYNTHIDX at %s", got, instrument, first)
	}
}
