package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRecordIsAtomicAndFailsClosedOnCorruption(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "success.json")
	record := cacheRecord{
		Key: "exact-key", Tree: "tree", Targets: []string{"app-check"},
		PassedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}
	if err := writeCacheRecord(path, record); err != nil {
		t.Fatal(err)
	}
	if !cacheHit(path, "exact-key") {
		t.Fatal("fresh exact-key cache record missed")
	}
	if cacheHit(path, "different-key") {
		t.Fatal("cache record matched a different key")
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cacheHit(path, "exact-key") {
		t.Fatal("corrupt cache record was accepted")
	}
}
