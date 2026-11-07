package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A lost write's WAL is gone once the next open folds it into the database, so
// the latch keeps the files as it found them, and a missing one is no failure.
func TestLostWriteEvidenceKeepsTheFilesAsTheLatchFoundThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.db")
	for suffix, body := range map[string]string{"": "db", "-wal": "wal", ".head": "head"} {
		if err := os.WriteFile(path+suffix, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kept, err := preserveAuthorityEvidence(context.Background(), path, time.Date(2026, 11, 6, 16, 41, 25, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "damaged", "lost-write-20261106T164125Z"); kept != want {
		t.Fatalf("kept in %s, want %s", kept, want)
	}
	for name, body := range map[string]string{"daemon.db": "db", "daemon.db-wal": "wal", "daemon.db.head": "head"} {
		got, err := os.ReadFile(filepath.Join(kept, name))
		if err != nil || string(got) != body {
			t.Fatalf("%s = %q %v", name, got, err)
		}
	}
	info, err := os.Stat(kept)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("evidence directory mode %v %v", info.Mode(), err)
	}
}
