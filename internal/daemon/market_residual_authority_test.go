package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func nasdaqTestPayload(t testing.TB, data any, rCode int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": data, "status": map[string]any{"rCode": rCode}})
	if err != nil {
		t.Fatal("marshal synthetic Nasdaq payload")
	}
	return body
}

func TestServerNewDoesNotReadLegacyFXOrEarningsBeforeLock(t *testing.T) {
	cacheRoot := t.TempDir()
	stateRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	now := time.Now().UTC()
	cacheDir, err := fxRateStoreDefaultDir()
	if err != nil {
		t.Fatalf("resolve cache directory: %v", err)
	}
	if err := newFXRateStore(cacheDir).save(map[string]fxCachedRate{
		"EUR/USD": {rate: 9.99, at: now},
	}); err != nil {
		t.Fatalf("seed legacy FX sentinel: %v", err)
	}
	if err := (&earningsStore{dir: cacheDir}).save(map[string]earningsEntry{
		"BUGGY": {Date: now.AddDate(0, 0, 7).Format("2006-01-02"), ObservedAt: now},
	}); err != nil {
		t.Fatalf("seed legacy earnings sentinel: %v", err)
	}
	breadthDir, err := spx.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := spx.NewStore(breadthDir).SaveSnapshot(spx.Snapshot{
		Value: 99, AsOf: now, SessionKey: now.Format(time.DateOnly),
		Method: spx.MethodConstituentFanout, MemberCount: 500, Coverage: 500,
	}); err != nil {
		t.Fatalf("seed legacy breadth sentinel: %v", err)
	}
	var logs bytes.Buffer
	srv := New(Options{
		Config: &config.Resolved{}, SocketPath: filepath.Join(t.TempDir(), "daemon.sock"),
		Version: "test", Logger: NewLogger(&logs, "warn"),
		StateDatabasePath: filepath.Join(stateRoot, "ibkr", "daemon.db"),
	})
	if rate, _, ok := srv.fxRates.get("EUR", "USD", fxCacheTTL); ok {
		t.Fatalf("Server.New hydrated pre-lock FX sentinel: rate=%v", rate)
	}
	if entry, _, ok := srv.earnings.get("BUGGY"); ok {
		t.Fatalf("Server.New hydrated pre-lock earnings sentinel: %+v", entry)
	}
	if snapshot, ok := srv.breadth.Get(); ok || snapshot != nil {
		t.Fatalf("Server.New hydrated pre-lock breadth sentinel: %+v", snapshot)
	}
	if srv.fxRates.store == nil || srv.earnings.store == nil {
		t.Fatal("Server.New failed to retain cold legacy codecs for cutover path discovery")
	}
}

func TestContractCacheSQLiteColdStartContinuityAndNoLegacyWrite(t *testing.T) {
	legacyDir := t.TempDir()
	legacy := ibkrlib.NewContractStore(legacyDir)
	if err := legacy.Save(map[string]ibkrlib.ContractDetailsLite{
		"BUGGY": {Symbol: "BUGGY", ConID: 111},
	}, nil, "legacy"); err != nil {
		t.Fatalf("seed legacy contract cache: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "contracts.json")
	legacyBytes, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read legacy contract cache: %v", err)
	}

	authority := openMarketTestCoreStore(t)
	store := ibkrlib.NewContractStore(legacyDir)
	if err := store.UseAuthority(coreContractCacheAuthority{store: authority}); err != nil {
		t.Fatalf("UseAuthority: %v", err)
	}
	if got, _, err := store.Load(); err != nil || got != nil {
		t.Fatalf("empty SQLite authority hydrated legacy contracts: got=%v err=%v", got, err)
	}
	want := map[string]ibkrlib.ContractDetailsLite{
		"AAPL": {Symbol: "AAPL", ConID: 265598, Exchange: "SMART"},
	}
	if err := store.Save(want, nil, "current"); err != nil {
		t.Fatalf("save SQLite contract cache: %v", err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Equal(after, legacyBytes) {
		t.Fatalf("legacy contracts.json changed after attachment: err=%v", err)
	}

	restarted := ibkrlib.NewContractStore(legacyDir)
	if err := restarted.UseAuthority(coreContractCacheAuthority{store: authority}); err != nil {
		t.Fatalf("restart UseAuthority: %v", err)
	}
	got, hash, err := restarted.Load()
	if err != nil || got["AAPL"].ConID != 265598 || hash != "current" {
		t.Fatalf("SQLite restart load: got=%v hash=%q err=%v", got, hash, err)
	}
	if _, ok, err := authority.GetStateDocument(context.Background(), contractAuthorityScope, contractStateKind); err != nil || !ok {
		t.Fatalf("contract cache state document missing: ok=%v err=%v", ok, err)
	}
	// The contract cache is a refetchable cache, not evidence. Appending it to
	// the immutable ledger once a minute is what put 5.1 GB of unread snapshots
	// in daemon.db and made every boot re-hash them before opening the socket.
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{ScopeKey: contractAuthorityScope})
	if err != nil {
		t.Fatalf("list contract-scope observations: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("contract cache wrote %d observations; it must publish state only", len(observations))
	}
}

func TestFXRateSQLiteReplacesLegacyAndSurvivesRestart(t *testing.T) {
	legacyDir := t.TempDir()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	legacyStore := newFXRateStore(legacyDir)
	if err := legacyStore.save(map[string]fxCachedRate{"EUR/USD": {rate: 9.99, at: base}}); err != nil {
		t.Fatalf("seed legacy FX: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, fxRateStoreFilename)
	legacyBytes, _ := os.ReadFile(legacyPath)
	cache := newFXRateCacheWithStore(newFXRateStore(legacyDir), func() time.Time { return base }, nil)
	if rate, _, ok := cache.get("EUR", "USD", fxCacheTTL); !ok || rate != 9.99 {
		t.Fatalf("test setup did not preload legacy FX: rate=%v ok=%v", rate, ok)
	}
	authority := openMarketTestCoreStore(t)
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	if _, _, ok := cache.get("EUR", "USD", fxCacheTTL); ok {
		t.Fatal("empty SQLite authority retained legacy FX current state")
	}
	cache.put("EUR", "USD", 0.88)
	after, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Equal(after, legacyBytes) {
		t.Fatalf("legacy fx-rates.json changed after attachment: err=%v", err)
	}

	restarted := newFXRateCache()
	restarted.now = func() time.Time { return base.Add(time.Hour) }
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	if rate, _, ok := restarted.get("EUR", "USD", fxCacheTTL); !ok || rate != 0.88 {
		t.Fatalf("SQLite FX restart: rate=%v ok=%v", rate, ok)
	}
	assertStateWithoutObservation(t, authority, fxAuthorityScope, fxStateKind, fxObservationSource, fxObservationKind)
}

func TestEarningsSQLiteReplacesLegacyAndSurvivesRestart(t *testing.T) {
	legacyDir := t.TempDir()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	legacyEntry := earningsEntry{Date: "2026-08-01", ObservedAt: base}
	if err := (&earningsStore{dir: legacyDir}).save(map[string]earningsEntry{"BUGGY": legacyEntry}); err != nil {
		t.Fatalf("seed legacy earnings: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, earningsStoreFilename)
	legacyBytes, _ := os.ReadFile(legacyPath)
	cache := newEarningsCache(legacyDir, nil)
	cache.clock = func() time.Time { return base }
	authority := openMarketTestCoreStore(t)
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	if _, _, ok := cache.get("BUGGY"); ok {
		t.Fatal("empty SQLite authority retained legacy earnings current state")
	}
	cache.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"}, http.StatusOK)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	cache.refreshOne(context.Background(), "TESTQ")
	after, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Equal(after, legacyBytes) {
		t.Fatalf("legacy earnings-dates.json changed after attachment: err=%v", err)
	}

	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return base.Add(time.Hour) }
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	entry, _, ok := restarted.get("TESTQ")
	if !ok || entry.Date != "2026-07-30" || entry.TimeOfDay != "" || entry.Estimated {
		t.Fatal("SQLite earnings restart did not retain only the typed date")
	}
	assertStateWithoutObservation(t, authority, earningsAuthorityScope, earningsStateKind, earningsObservationSource, earningsProviderObservationKind)
}

func TestResidualAuthoritiesRejectMalformedRows(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		authority := openMarketTestCoreStore(t)
		writeMalformedMarketState(t, authority, contractAuthorityScope, contractStateKind, []byte(`{"version":3,"as_of":"2026-07-20T12:00:00Z","contracts":{"AAPL":{"ConID":0}}}`))
		if err := ibkrlib.NewContractStore(t.TempDir()).UseAuthority(coreContractCacheAuthority{store: authority}); err == nil {
			t.Fatal("malformed contract row attached")
		}
	})
	t.Run("fx", func(t *testing.T) {
		authority := openMarketTestCoreStore(t)
		writeMalformedMarketState(t, authority, fxAuthorityScope, fxStateKind, []byte(`{"version":1,"rates":{"EUR/USD":{"rate":-1,"at":"2026-07-20T12:00:00Z"}}}`))
		cache := newFXRateCache()
		cache.now = func() time.Time { return time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC) }
		if err := cache.UseCoreStore(authority); err == nil {
			t.Fatal("malformed FX row attached")
		}
	})
	t.Run("earnings", func(t *testing.T) {
		authority := openMarketTestCoreStore(t)
		writeMalformedMarketState(t, authority, earningsAuthorityScope, earningsStateKind, []byte(`{"version":1,"entries":{"AAPL":{"date":"not-a-date","observed_at":"2026-07-20T12:00:00Z"}}}`))
		cache := newEarningsCacheMemory(nil)
		cache.clock = func() time.Time { return time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC) }
		if err := cache.UseCoreStore(authority); err == nil {
			t.Fatal("malformed earnings row attached")
		}
	})
}

func writeMalformedMarketState(t *testing.T, store *corestore.Store, scope, kind string, payload []byte) {
	t.Helper()
	if _, err := store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
		ScopeKey: scope, Kind: kind, JSON: payload,
	}); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}
}

func assertStateWithoutObservation(t *testing.T, store *corestore.Store, scope, stateKind, source, observationKind string) {
	t.Helper()
	if _, ok, err := store.GetStateDocument(context.Background(), scope, stateKind); err != nil || !ok {
		t.Fatalf("state document missing: ok=%v err=%v", ok, err)
	}
	observations, err := store.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: scope, Source: source, Kind: observationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("unexpected observation count=%d err=%v", len(observations), err)
	}
}
