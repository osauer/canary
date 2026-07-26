package spx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func openBreadthTestCoreStore(t *testing.T) *corestore.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure test corestore dir: %v", err)
	}
	store, err := corestore.Open(context.Background(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open test corestore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close test corestore: %v", err)
		}
	})
	return store
}

func TestStoreSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if got, err := s.LoadSnapshot(); err != nil || got != nil {
		t.Fatalf("cold load: want (nil, nil), got (%v, %v)", got, err)
	}

	want := Snapshot{
		Value:       58.7,
		AsOf:        time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey:  "2026-05-16",
		Method:      methodConstituentFanout,
		MemberCount: 503,
		Coverage:    501,
		Excluded:    []ExcludedMember{{Symbol: "NEW", Reason: "thin_history(3)"}},
	}
	if err := s.SaveSnapshot(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("load returned nil after save")
	}
	if got.Value != want.Value || got.SessionKey != want.SessionKey || got.Coverage != want.Coverage {
		t.Errorf("snapshot roundtrip mismatch:\n  want %+v\n  got  %+v", want, *got)
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Symbol != "NEW" {
		t.Errorf("excluded list lost in round-trip: got %+v", got.Excluded)
	}
}

func TestStoreUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openBreadthTestCoreStore(t)
	store := NewStore(legacyDir)
	if err := store.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	now := time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC)
	want := Snapshot{
		Value: 58.7, AsOf: now, SessionKey: "2026-05-16",
		Method: methodConstituentFanout, MemberCount: 503, Coverage: 501,
	}
	if err := store.SaveSnapshot(want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := store.SaveWindows(map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: []float64{100, 101}, LastBarAt: "2026-05-16"},
	}, now); err != nil {
		t.Fatalf("SaveWindows: %v", err)
	}
	if err := store.SaveHistory([]HistoryPoint{{Date: "2026-05-16", PctAbove50DMA: 58.7}}); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	entries, err := os.ReadDir(legacyDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy breadth cache was written: entries=%v err=%v", entries, err)
	}

	restarted := NewStore(legacyDir)
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	got, err := restarted.LoadSnapshot()
	if err != nil || got == nil || got.Value != want.Value || got.Method != want.Method {
		t.Fatalf("SQLite snapshot round trip: got=%+v err=%v", got, err)
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: breadthAuthorityScope, Source: breadthSource,
	})
	if err != nil || len(observations) != 3 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
	for _, observation := range observations {
		if !observation.DecisionEligible {
			t.Fatal("current breadth observation is not decision-eligible")
		}
	}
}

func TestWindowCheckpointReplacesStateWithoutObservation(t *testing.T) {
	authority := openBreadthTestCoreStore(t)
	store := NewStore(t.TempDir())
	if err := store.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	windows := map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: []float64{100, 101}, LastBarAt: "2026-05-18"},
	}
	if err := store.checkpointWindows(windows, now); err != nil {
		t.Fatalf("checkpointWindows: %v", err)
	}
	loaded, err := store.LoadWindows()
	if err != nil || loaded["AAPL"].LastBarAt != "2026-05-18" {
		t.Fatalf("checkpoint load=%+v err=%v", loaded, err)
	}
	query := corestore.ObservationQuery{
		ScopeKey: breadthAuthorityScope, Source: breadthSource, Kind: breadthWindowsObservationKind,
	}
	observations, err := authority.ListObservations(context.Background(), query)
	if err != nil {
		t.Fatalf("list checkpoint observations: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("checkpoint appended %d immutable observations, want zero", len(observations))
	}
	if err := store.SaveWindows(windows, now); err != nil {
		t.Fatalf("SaveWindows: %v", err)
	}
	observations, err = authority.ListObservations(context.Background(), query)
	if err != nil || len(observations) != 1 {
		t.Fatalf("canonical observations=%d err=%v, want one", len(observations), err)
	}
}

func TestEngineUseCoreStoreReplacesPreloadedLegacyProjection(t *testing.T) {
	legacyDir := t.TempDir()
	legacy := NewStore(legacyDir)
	legacySnapshot := Snapshot{
		Value: 99, AsOf: time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey: "2026-05-16", Method: methodConstituentFanout,
	}
	if err := legacy.SaveSnapshot(legacySnapshot); err != nil {
		t.Fatalf("seed legacy snapshot: %v", err)
	}
	engine := New(legacy, &FakeBarFetcher{}, Options{Members: []string{"AAPL"}})
	if got, ok := engine.Get(); !ok || got.Value != legacySnapshot.Value {
		t.Fatalf("engine did not preload legacy fixture: got=%+v ok=%v", got, ok)
	}

	authority := openBreadthTestCoreStore(t)
	if err := engine.UseCoreStore(authority); err != nil {
		t.Fatalf("Engine.UseCoreStore: %v", err)
	}
	if got, ok := engine.Get(); ok || got != nil {
		t.Fatalf("empty SQLite authority did not replace legacy projection: got=%+v ok=%v", got, ok)
	}
}

func TestEngineDeferredStoreLoadDoesNotReadLegacyProjection(t *testing.T) {
	legacyDir := t.TempDir()
	legacy := NewStore(legacyDir)
	legacySnapshot := Snapshot{
		Value: 99, AsOf: time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey: "2026-05-16", Method: methodConstituentFanout,
	}
	if err := legacy.SaveSnapshot(legacySnapshot); err != nil {
		t.Fatal(err)
	}
	engine := New(legacy, &FakeBarFetcher{}, Options{
		Members: []string{"AAPL"}, DeferStoreLoad: true,
	})
	if got, ok := engine.Get(); ok || got != nil {
		t.Fatalf("deferred construction read legacy snapshot: got=%+v ok=%v", got, ok)
	}
	authority := openBreadthTestCoreStore(t)
	if err := engine.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	if got, ok := engine.Get(); ok || got != nil {
		t.Fatalf("empty SQLite authority did not stay cold: got=%+v ok=%v", got, ok)
	}
}

// TestLoadSnapshotMethodGate pins the version gate. A snapshot.json
// written by an older methodology must be treated as no-cache so the
// next refresh tick rebuilds with the current schema. Without this gate
// a v1 file (Value-only payload) decoded silently into a v2 struct with
// the new fields zeroed, and the engine reported state=ready with a
// phantom "0% above 50-DMA / 0 new highs" reading.
func TestLoadSnapshotMethodGate(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	stale := Snapshot{
		Value:       58.7,
		AsOf:        time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey:  "2026-05-16",
		Method:      "constituent-fanout-50dma", // the pre-v2 token
		MemberCount: 503,
		Coverage:    501,
	}
	if err := s.SaveSnapshot(stale); err != nil {
		t.Fatalf("save stale-method snapshot: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Fatalf("stale-method snapshot returned non-nil; the version gate failed open. Got: %+v", *got)
	}
}

func TestStoreWindowsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	closes := make([]float64, WindowSize)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	want := map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: closes, LastBarAt: "2026-05-16"},
		"MSFT": {Symbol: "MSFT", Closes: closes, LastBarAt: "2026-05-16"},
	}
	if err := s.SaveWindows(want, time.Now()); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadWindows()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d windows, want 2", len(got))
	}
	if len(got["AAPL"].Closes) != WindowSize {
		t.Errorf("AAPL closes: want %d entries, got %d", WindowSize, len(got["AAPL"].Closes))
	}
}

// TestStoreCorruptionRecovery covers the daemon-crash-mid-write case:
// a half-written file should not poison startup. We don't claim
// recovery from arbitrary corruption — we just need the error to be
// surfaced so the engine can cold-rebuild rather than silently use
// stale state.
func TestStoreCorruptionRecovery(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("not json {"), 0o644); err != nil {
		t.Fatalf("seed corruption: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err == nil {
		t.Errorf("expected decode error for corrupt file, got %+v", got)
	}
	if !strings.Contains(err.Error(), "decode snapshot") {
		t.Errorf("error should mention decode failure, got %v", err)
	}
}

// TestStoreUnknownVersionTriggersColdStart pins the schema-bump
// behaviour: an on-disk file with a Version field the engine doesn't
// recognise must be treated as no-cache, not as an error or as
// data-to-load. Forward-compat: a future binary can write Version=2,
// an older binary cold-rebuilds instead of corrupting its view.
func TestStoreUnknownVersionTriggersColdStart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	future := WindowSet{Version: 99, AsOf: time.Now(), Windows: map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: []float64{1, 2, 3}},
	}}
	if err := s.writeAtomic("windows.json", future); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.LoadWindows()
	if err != nil {
		t.Errorf("unknown version should not error: got %v", err)
	}
	if got != nil {
		t.Errorf("unknown version should yield no-cache (nil map), got %v", got)
	}
}

// TestStoreAtomicWriteCleanupOnDecodeFailure pins that a failed save
// doesn't leave a partial snapshot.json visible. Since SaveSnapshot
// encodes-then-renames, a successful temp write followed by a rename
// failure should still result in no target file. Hard to simulate
// rename failure portably; this test instead just confirms the temp
// pattern doesn't litter the dir on the success path.
func TestStoreAtomicWriteNoLitter(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := range 5 {
		snap := Snapshot{Value: float64(i), Method: methodConstituentFanout}
		if err := s.SaveSnapshot(snap); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	// Only snapshot.json should remain — no stray .tmp.* files from
	// the atomic-write path.
	for _, e := range entries {
		name := e.Name()
		if name == "snapshot.json" {
			continue
		}
		if strings.HasPrefix(name, "snapshot.json.tmp") {
			t.Errorf("stale tempfile left behind: %s", name)
		}
	}
}

func TestDefaultDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/test-xdg")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("default dir: %v", err)
	}
	if got != "/tmp/test-xdg/ibkr/breadth-spx" {
		t.Errorf("XDG dir: want /tmp/test-xdg/ibkr/breadth-spx, got %q", got)
	}
}

func TestDefaultDirFallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/tmp/test-home")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("default dir: %v", err)
	}
	if got != "/tmp/test-home/.cache/ibkr/breadth-spx" {
		t.Errorf("HOME fallback: want /tmp/test-home/.cache/ibkr/breadth-spx, got %q", got)
	}
}
