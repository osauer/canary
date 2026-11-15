package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
)

// Most daemon tests need an empty, current authority store, not the act of
// creating one. Creating one runs every migration under synchronous=FULL and
// F_FULLFSYNC, which on macOS flushes the drive cache each time: about 0.2 s
// a store, repeated in hundreds of tests that assert nothing about it.
// Creation, migration and recovery stay proven by corestore's own tests and
// by the daemon tests that call corestore.Open on a fresh path directly.
//
// openTestCoreStore migrates once per test process, keeps the bytes, and
// writes a private copy for each new path, which corestore.Open then validates
// in full like any existing database. No writable store is shared. Every
// copy carries the template's authority epoch, so a test that needs two
// stores with different authority must open fresh ones.
var coreStoreTemplate struct {
	once  sync.Once
	bytes []byte
	err   error
}

func openTestCoreStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	coreStoreTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "canary-corestore-template-")
		if err != nil {
			coreStoreTemplate.err = err
			return
		}
		defer os.RemoveAll(dir)
		db := filepath.Join(dir, "template.db")
		store, err := corestore.Open(context.Background(), corestore.Options{Path: db})
		if err != nil {
			coreStoreTemplate.err = err
			return
		}
		if err := store.Close(); err != nil {
			coreStoreTemplate.err = err
			return
		}
		if _, err := os.Stat(db + "-wal"); err == nil {
			coreStoreTemplate.err = os.ErrExist // a closed store must leave no write-ahead log
			return
		}
		coreStoreTemplate.bytes, coreStoreTemplate.err = os.ReadFile(db)
	})
	if coreStoreTemplate.err != nil {
		t.Fatalf("migrate the test authority template: %v", coreStoreTemplate.err)
	}
	// A path that already holds a store is a restart: reopen what is there.
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, coreStoreTemplate.bytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("open test authority copy: %v", err)
	}
	return store
}
