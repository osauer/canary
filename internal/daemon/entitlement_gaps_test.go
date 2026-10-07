package daemon

import (
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
