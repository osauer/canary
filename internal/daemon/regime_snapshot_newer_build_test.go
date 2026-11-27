package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
)

// A snapshot a later build stored, with fields this build does not know at
// the top level and inside a row, loads at start and serves: without that
// the build before a release cannot start on the release's store. A document
// that is damaged or not a regime snapshot is still refused.
func TestSnapshotCacheLoadsASnapshotALaterBuildStored(t *testing.T) {
	at := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)
	raw, _, err := encodeRegimeSnapshotDocument(regimeSnapshotCacheFixture(at, "later-build"))
	if err != nil {
		t.Fatal(err)
	}
	row := []byte(`"hyg_spy_divergence":{`)
	if !bytes.Contains(raw, row) {
		t.Fatalf("fixture has no hyg_spy_divergence object: %s", raw)
	}
	later := bytes.Replace(raw, row, []byte(`"hyg_spy_divergence":{"hyg_close":79.5,"close_session":"2026-11-24","later_object":{"a":[1,2]},`), 1)
	later = append([]byte(`{"later_top_level":true,`), later[1:]...)

	load := func(t *testing.T, document []byte) (*regimeSnapshotCache, error) {
		t.Helper()
		store := openRegimeSnapshotTestStore(t)
		saved, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, ExpectedRevision: 0, JSON: document,
		})
		if err != nil {
			t.Fatalf("persist snapshot: %v", err)
		}
		daemonContext, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
			FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: time.Minute,
			Now: func() time.Time { return saved.UpdatedAt.Add(time.Second) },
		})
	}

	cache, err := load(t, later)
	if err != nil {
		t.Fatalf("a later build's snapshot refused the start: %v", err)
	}
	cache.mu.Lock()
	view, served := cache.viewStateLocked(cache.now())
	cache.mu.Unlock()
	view, err = materializeRegimeSnapshotCacheView(view, served)
	if err != nil || view.Snapshot == nil {
		t.Fatalf("a later build's snapshot loaded but does not serve: %v", err)
	}
	if view.Revision != 1 || view.Snapshot.Fingerprint != cache.fingerprint || !view.Snapshot.AsOf.Equal(at) {
		t.Fatalf("served revision %d as of %s, want the stored snapshot (revision 1 as of %s)", view.Revision, view.Snapshot.AsOf, at)
	}

	for name, damaged := range map[string][]byte{
		"a known field of another type": bytes.Replace(later, []byte(`"as_of":"`), []byte(`"as_of":7,"was_as_of":"`), 1),
		"a row without its status": editRegimeSnapshotJSON(t, later, func(d map[string]any) {
			d["hyg_spy_divergence"].(map[string]any)["status"] = ""
		}),
		"a state its fingerprint disowns": editRegimeSnapshotJSON(t, later, func(d map[string]any) {
			d["fingerprint"].(map[string]any)["key"] = "0000"
		}),
		"another document's shape entirely": []byte(`{"version":"v1","entries":{}}`),
	} {
		if bytes.Equal(damaged, later) {
			t.Fatalf("%s: the damage did not apply", name)
		}
		if _, err := load(t, damaged); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// editRegimeSnapshotJSON returns raw with edit applied to its decoded object.
func editRegimeSnapshotJSON(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	edit(document)
	out, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
