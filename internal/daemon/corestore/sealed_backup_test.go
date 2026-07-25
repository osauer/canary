package corestore

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup sealed at an older schema version must stay verifiable after later
// migrations ship. VerifyBackup demands equality with the current version,
// which is right for a backup taken now and wrong for a frozen artifact: once
// any sealed backup existed, the first new migration made every daemon start
// fail with "schema version 1 does not match expected N". VerifySealedBackup
// checks the version the artifact actually carries, and still proves it
// genuine by requiring its migration ledger to be a checksum-matching prefix
// of the current plan.
func TestVerifySealedBackupAcceptsAnOlderFrozenVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	plan := currentMigrationPlan()
	if len(plan) < 2 {
		t.Skip("needs at least two shipped migrations to have an older version to freeze")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("tighten authority dir: %v", err)
	}
	source := filepath.Join(dir, "daemon.db")
	seedV1Authority(t, source, plan)

	store, err := openWithPlan(ctx, Options{Path: source}, plan[:1])
	if err != nil {
		t.Fatalf("reopen v1 authority: %v", err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sealed := filepath.Join(dir, "sealed-v1.db")
	copyFileForTest(t, source, sealed)

	info, err := VerifySealedBackup(ctx, sealed, head)
	if err != nil {
		t.Fatalf("sealed backup at an older version must verify: %v", err)
	}
	if info.SchemaVersion != 1 {
		t.Fatalf("sealed backup schema version = %d, want 1", info.SchemaVersion)
	}

	// The regression itself: the exact-version check is what broke startup.
	// If this ever stops failing, VerifyBackup changed meaning and the caller
	// in ensureCutoverBackup should be revisited.
	if _, err := VerifyBackup(ctx, sealed, head); err == nil {
		t.Fatal("VerifyBackup must still require the current version; sealed artifacts use VerifySealedBackup")
	} else if !strings.Contains(err.Error(), "does not match expected") {
		t.Fatalf("unexpected VerifyBackup failure: %v", err)
	}
}

// A version outside the known plan is not a stale artifact, it is a file this
// build cannot vouch for, and it must be refused rather than trusted.
func TestVerifySealedBackupRejectsAnUnknownVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	plan := currentMigrationPlan()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("tighten authority dir: %v", err)
	}
	source := filepath.Join(dir, "daemon.db")
	seedV1Authority(t, source, plan)

	store, err := openWithPlan(ctx, Options{Path: source}, plan[:1])
	if err != nil {
		t.Fatalf("reopen v1 authority: %v", err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sealed := filepath.Join(dir, "sealed-future.db")
	copyFileForTest(t, source, sealed)

	db, err := sql.Open("sqlite", sqliteDSN(sealed, defaultBusyTimeout, false))
	if err != nil {
		t.Fatalf("open sealed backup: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sealed backup: %v", err)
	}

	if _, err := VerifySealedBackup(ctx, sealed, head); err == nil {
		t.Fatal("a version outside the known migration plan must be refused")
	} else if !strings.Contains(err.Error(), "outside the known migration plan") {
		t.Fatalf("unexpected failure: %v", err)
	}
}

func copyFileForTest(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
