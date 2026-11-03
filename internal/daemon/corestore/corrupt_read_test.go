package corestore

import (
	"os"
	"testing"
)

// corruptStateDocumentsRoot damages the on-disk root page of state_documents
// under an open store and drops the connection's page cache, so the next read
// meets the damage the way a live daemon met a cross-linked page on
// 2026-11-03.
func corruptStateDocumentsRoot(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var root, pageSize int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE name='state_documents'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Byte 0 of a b-tree page is its type; 0x00 is never valid.
	if _, err := f.WriteAt([]byte{0x00}, (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA shrink_memory`); err != nil {
		t.Fatal(err)
	}
}

// A read that finds the authority damaged is the same evidence as a failed
// mutation: the store must latch fail-closed and report it, not hand the error
// to a caller that may only mark its own feature unavailable while later
// writes keep landing in the damaged tree.
func TestCorruptReadLatchesTheStore(t *testing.T) {
	f := openRecoveryFixture(t)
	for _, kind := range []string{"victim", "neighbour"} {
		if err := f.mutate(t, kind); err != nil {
			t.Fatal(err)
		}
	}
	corruptStateDocumentsRoot(t, f.store)

	if _, _, err := f.store.GetStateDocument(t.Context(), "test", "victim"); err == nil {
		t.Fatal("read of a damaged page succeeded")
	}
	if got := f.store.Health(); got.Ready || got.Code != "corrupt" {
		t.Fatalf("health after a corrupt read = %+v, want latched corrupt", got)
	}
	seen := f.recorder.transitions(t, 1)
	if seen[0].health.Code != "corrupt" || seen[0].cause == nil {
		t.Fatalf("health observer saw %+v, want the corrupt latch with its cause", seen[0])
	}
	if err := f.mutate(t, "after"); err == nil {
		t.Fatal("a mutation was accepted after the store latched corrupt")
	}
}
