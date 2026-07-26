package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRegimeDecisionConsumersRequireCompletedProjection(t *testing.T) {
	t.Run("cold authority is unavailable rather than projection pending", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		daemonContext, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
		cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
		server := &Server{regimeSnapshots: cache}

		quality := server.statusDataQuality()
		if len(quality) != 1 {
			t.Fatalf("status quality=%+v, want one cold-authority warning", quality)
		}
		warning := quality[0]
		if warning.Surface != "regime" || warning.Status != "unavailable" || warning.Summary != "unavailable: no completed regime snapshot" {
			t.Fatalf("cold authority warning=%+v", warning)
		}
		if len(warning.PartialClusters) != 1 || warning.PartialClusters[0] != "authority" {
			t.Fatalf("cold authority clusters=%+v", warning.PartialClusters)
		}
		if pending, revision := cache.projectionFailure(); pending || revision != 0 {
			t.Fatalf("cold authority reported projection pending=%v revision=%d", pending, revision)
		}
		if cache.refreshing() {
			t.Fatal("cold status read started market-data refresh")
		}
	})

	t.Run("no pending revision reads last-good without repair authority", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		cache := projectionRecoveryPersistSnapshot(t, store, regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "projected"), 1)
		server := &Server{regimeSnapshots: cache}

		snapshot, err := server.briefRegimeSnapshotContext(t.Context())
		if err != nil || snapshot == nil || snapshot.Summary.Label != "projected" {
			t.Fatalf("brief snapshot=%+v err=%v, want projected last-good", snapshot, err)
		}
		if quality := server.statusDataQuality(); len(quality) != 0 {
			t.Fatalf("status quality=%+v, want no projection warning", quality)
		}
		if pending, revision := cache.projectionFailure(); pending || revision != 0 {
			t.Fatalf("projection pending=%v revision=%d, want false/0", pending, revision)
		}
		if cache.refreshing() {
			t.Fatal("non-triggering consumer read started market-data refresh")
		}
	})

	t.Run("stale last-good authority is explicit in brief and status", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		cache := projectionRecoveryPersistSnapshot(t, store, regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "stale authority"), 1)
		cache.mu.Lock()
		cache.now = func() time.Time { return cache.lastSuccessAt.Add(2 * time.Minute) }
		cache.mu.Unlock()
		server := &Server{regimeSnapshots: cache}

		snapshot, err := server.currentDecisionReadyRegimeSnapshot(t.Context())
		if err != nil || snapshot == nil || snapshot.AuthorityHealth == nil || snapshot.AuthorityHealth.Status != rpc.RegimeAuthorityStale {
			t.Fatalf("stale snapshot=%+v err=%v", snapshot, err)
		}
		market, _ := composeBriefMarket(cache.now(), nil, nil, snapshot, nil, nil, nil, nil, nil, nil, nil, nil, false)
		if market.Regime.Status != rpc.BriefStatusDegraded || market.Regime.Detail != "daemon Regime verdict is retained stale last-good context" {
			t.Fatalf("brief Regime row=%+v, want explicit stale last-good", market.Regime)
		}
		quality := server.statusDataQuality()
		if len(quality) != 1 || quality[0].Surface != "regime" || quality[0].Status != "stale" {
			t.Fatalf("status quality=%+v, want stale Regime authority", quality)
		}
		if !strings.Contains(quality[0].Summary, "authority: stale last-good") || len(quality[0].StaleClusters) != 1 || quality[0].StaleClusters[0] != "authority" {
			t.Fatalf("status stale authority detail=%+v", quality[0])
		}
		if cache.refreshing() {
			t.Fatal("brief/status stale reads started market-data refresh")
		}
	})

	t.Run("brief repairs the exact pending revision", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		cache := projectionRecoveryPersistSnapshot(t, store, regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "brief repair"), 1)
		markRegimeProjectionPendingForConsumerTest(cache)
		server := regimeProjectionConsumerTestServer(cache, store)

		snapshot, err := server.briefRegimeSnapshotContext(t.Context())
		if err != nil || snapshot == nil || snapshot.Summary.Label != "brief repair" {
			t.Fatalf("brief snapshot=%+v err=%v, want repaired last-good", snapshot, err)
		}
		assertRegimeConsumerProjectionRepaired(t, server, cache, 1)
		if snapshot.AuthorityHealth == nil || snapshot.AuthorityHealth.FailureCode != rpc.RegimeAuthorityFailureNone {
			t.Fatalf("brief authority health=%+v, want repaired authority", snapshot.AuthorityHealth)
		}
	})

	t.Run("status repairs the exact pending revision", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		cache := projectionRecoveryPersistSnapshot(t, store, regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "status repair"), 1)
		markRegimeProjectionPendingForConsumerTest(cache)
		server := regimeProjectionConsumerTestServer(cache, store)

		if quality := server.statusDataQuality(); len(quality) != 0 {
			t.Fatalf("status quality=%+v, want repaired healthy snapshot", quality)
		}
		assertRegimeConsumerProjectionRepaired(t, server, cache, 1)
		if cache.refreshing() {
			t.Fatal("status repair started market-data refresh")
		}
	})

	t.Run("failed repair withholds brief and is explicit in status", func(t *testing.T) {
		store := openRegimeSnapshotTestStore(t)
		snapshot := regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "must be withheld")
		snapshot.VIXTermStructure.Status = rpc.RegimeStatusStale
		snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
		cache := projectionRecoveryPersistSnapshot(t, store, snapshot, 1)
		markRegimeProjectionPendingForConsumerTest(cache)
		server := regimeProjectionConsumerTestServer(cache, store)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		server.serverCtx = cancelled

		got, err := server.briefRegimeSnapshotContext(cancelled)
		if err == nil || got != nil {
			t.Fatalf("brief snapshot=%+v err=%v, want withheld pending projection", got, err)
		}
		quality := server.statusDataQuality()
		if len(quality) != 1 {
			t.Fatalf("status quality=%+v, want one projection warning", quality)
		}
		warning := quality[0]
		if warning.Surface != "regime" || warning.Status != "partial" || warning.Summary != "partial: snapshot projection repair pending" {
			t.Fatalf("status projection warning=%+v", warning)
		}
		if len(warning.PartialClusters) != 1 || warning.PartialClusters[0] != "projection" || len(warning.StaleClusters) != 0 {
			t.Fatalf("status leaked pending snapshot quality=%+v", warning)
		}
		if !warning.AsOf.Equal(snapshot.AsOf) {
			t.Fatalf("status projection as_of=%s, want %s", warning.AsOf, snapshot.AsOf)
		}
		if pending, revision := cache.projectionFailure(); !pending || revision != 1 {
			t.Fatalf("projection pending=%v revision=%d, want true/1 after failed repair", pending, revision)
		}
		if cache.refreshing() {
			t.Fatal("failed consumer repair started market-data refresh")
		}
	})
}

func regimeProjectionConsumerTestServer(cache *regimeSnapshotCache, store *corestore.Store) *Server {
	return &Server{
		regimeSnapshots:        cache,
		coreStore:              store,
		rulesRegimeStageLoaded: true,
		logger:                 NewLogger(&bytes.Buffer{}, "error"),
	}
}

func markRegimeProjectionPendingForConsumerTest(cache *regimeSnapshotCache) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.projectionPending = true
	cache.projectionRevision = cache.revision
	cache.failureCode = rpc.RegimeAuthorityFailurePublishFailed
}

func assertRegimeConsumerProjectionRepaired(t *testing.T, server *Server, cache *regimeSnapshotCache, wantRevision int64) {
	t.Helper()
	if pending, revision := cache.projectionFailure(); pending || revision != 0 {
		t.Fatalf("projection pending=%v revision=%d, want false/0", pending, revision)
	}
	receipt, ok, err := server.loadRegimeProjectionReceipt(t.Context())
	if err != nil || !ok || receipt.SnapshotRevision != wantRevision {
		t.Fatalf("projection receipt=%+v ok=%v err=%v, want revision %d", receipt, ok, err, wantRevision)
	}
}
