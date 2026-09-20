package spx

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func newSectorsTestAuthority(t *testing.T) (string, *corestore.Store) {
	t.Helper()
	// The store refuses a database whose directory others can read; the
	// per-test temp directory is created 0755, so use a private child.
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(context.Background(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	path := filepath.Join(dir, MembersFilename)
	if err := UseCoreMembersStore(path, store); err != nil {
		t.Fatal(err)
	}
	return path, store
}

// withBaselineSectors starts the test from the release baseline and puts the
// registry back afterwards; the registry is package state.
func withBaselineSectors(t *testing.T) {
	t.Helper()
	before, asOf := SectorList()
	t.Cleanup(func() { resetSectorsForTest(before, asOf) })
	resetSectorsForTest(maps.Clone(sp500Sectors), sp500AsOf)
}

func resetSectorsForTest(sectors map[string]string, asOf time.Time) {
	sectorRegistry.Lock()
	sectorRegistry.sectors = sectors
	sectorRegistry.asOf = asOf
	sectorRegistry.Unlock()
}

// The sector map parsed from the daily page is kept beside the member list
// and comes back after a restart when it is newer than the release baseline.
func TestSectorsPersistAndRestoreAcrossRestart(t *testing.T) {
	withBaselineSectors(t)
	path, _ := newSectorsTestAuthority(t)
	parsed := maps.Clone(sp500Sectors)
	parsed["AAPL"] = "Test Sector"
	observed := sp500AsOf.Add(48 * time.Hour)
	if err := SaveSectors(path, parsed, observed); err != nil {
		t.Fatal(err)
	}
	got, asOf, ok := LoadSectors(path)
	if !ok || !asOf.Equal(observed) || got["AAPL"] != "Test Sector" || len(got) != len(parsed) {
		t.Fatalf("load: ok=%t asOf=%s AAPL=%q n=%d", ok, asOf, got["AAPL"], len(got))
	}
	resetSectorsForTest(maps.Clone(sp500Sectors), sp500AsOf) // a restart begins from the baseline
	if restoredAsOf, ok := RestoreSectors(path); !ok || !restoredAsOf.Equal(observed) {
		t.Fatalf("restore: ok=%t asOf=%s", ok, restoredAsOf)
	}
	if s, _ := SectorOf("AAPL"); s != "Test Sector" {
		t.Fatalf("restored map not in use: AAPL=%q", s)
	}
	if !SectorsAsOf().Equal(observed) {
		t.Fatalf("SectorsAsOf = %s, want %s", SectorsAsOf(), observed)
	}
}

// A release regenerates the baseline; a document an older binary wrote
// before that release must not replace it.
func TestSectorsOlderThanTheBaselineAreIgnored(t *testing.T) {
	withBaselineSectors(t)
	path, _ := newSectorsTestAuthority(t)
	stale := maps.Clone(sp500Sectors)
	stale["AAPL"] = "Stale Sector"
	if err := SaveSectors(path, stale, sp500AsOf.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := RestoreSectors(path); ok {
		t.Fatal("a document older than the baseline was restored")
	}
	if s, _ := SectorOf("AAPL"); s == "Stale Sector" {
		t.Fatal("the stale map replaced the baseline")
	}
}

// Without a binding nothing is persisted and nothing fails: the baseline is
// the floor.
func TestSectorsWithoutAnAuthorityAreNotPersisted(t *testing.T) {
	withBaselineSectors(t)
	path := filepath.Join(t.TempDir(), MembersFilename)
	if err := SaveSectors(path, maps.Clone(sp500Sectors), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := LoadSectors(path); ok {
		t.Fatal("loaded sectors without an authority")
	}
	if _, ok := RestoreSectors(path); ok {
		t.Fatal("restored sectors without an authority")
	}
}

// A document that fails validation is ignored rather than blocking the
// binding or blanking the map: wrong version, too few entries, or junk.
func TestSectorsRejectAnUnusableDocument(t *testing.T) {
	withBaselineSectors(t)
	path, store := newSectorsTestAuthority(t)
	tiny := `{"version":1,"as_of":"2026-09-20T00:00:00Z","source":"wikipedia","url":"` + WikipediaURL + `","count":1,"sectors":{"AAPL":"Information Technology"}}`
	for _, payload := range []string{`{"version":2}`, tiny, `[]`} {
		doc, ok, err := store.GetStateDocument(context.Background(), membersAuthorityScope, sectorsStateKind)
		if err != nil {
			t.Fatal(err)
		}
		var rev int64
		if ok {
			rev = doc.Revision
		}
		if _, err := store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{ScopeKey: membersAuthorityScope, Kind: sectorsStateKind, ExpectedRevision: rev, JSON: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := LoadSectors(path); ok {
			t.Fatalf("accepted unusable document %s", payload)
		}
		if _, ok := RestoreSectors(path); ok {
			t.Fatalf("restored unusable document %s", payload)
		}
		if err := UseCoreMembersStore(path, store); err != nil {
			t.Fatalf("binding refused over a sectors document: %v", err)
		}
	}
	if s, _ := SectorOf("AAPL"); s != sp500Sectors["AAPL"] {
		t.Fatalf("baseline changed: %q", s)
	}
}
