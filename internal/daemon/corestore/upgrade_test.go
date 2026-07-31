package corestore

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrepareUpgradeBuildsIndependentAtomicCandidate(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon.db")
	backupPath := filepath.Join(dir, "daemon-v1-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v2-candidate.db")

	store, err := Open(ctx, Options{Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
		ScopeKey: "safety", Kind: "guardrails", JSON: []byte(`{"freeze":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvents(ctx, []EventInput{{
		ScopeKey: "market", EventKey: "upgrade-event", Type: "test.upgrade", Action: "observe", Origin: "daemon",
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(), PayloadJSON: []byte(`{"version":1}`),
	}}); err != nil {
		t.Fatal(err)
	}
	beforeSigner, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sourceHead, err := store.AdvanceSignerGeneration(ctx, beforeSigner.SignerGeneration, beforeSigner.SignerGeneration+1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBytesBefore, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntriesBefore := directoryNames(t, dir)

	plan := syntheticNextPlan()
	current := len(plan) - 1
	inspection1, err := inspectWithPlan(ctx, InspectOptions{Path: sourcePath, MinimumHead: &sourceHead}, plan)
	if err != nil {
		t.Fatal(err)
	}
	inspection2, err := inspectWithPlan(ctx, InspectOptions{Path: sourcePath, MinimumHead: &sourceHead}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inspection1, inspection2) || inspection1.Status != InspectionUpgradeRequired || inspection1.SchemaVersion != current || inspection1.TargetVersion != current+1 {
		t.Fatalf("idempotent inspection mismatch: first=%+v second=%+v", inspection1, inspection2)
	}

	result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath, MinimumHead: &sourceHead,
	}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Head != sourceHead || result.Source.SchemaVersion != current || result.Source.Status != InspectionUpgradeRequired {
		t.Fatalf("source inspection=%+v", result.Source)
	}
	if result.Backup.Head != sourceHead || result.Backup.SchemaVersion != current || !result.Backup.Integrity.OK() {
		t.Fatalf("backup=%+v", result.Backup)
	}
	wantCandidateHead := sourceHead
	wantCandidateHead.HeadGeneration++
	if result.Candidate.Head != wantCandidateHead || result.Candidate.SchemaVersion != current+1 || result.Candidate.Status != InspectionCurrent || !result.Candidate.Integrity.OK() {
		t.Fatalf("candidate=%+v want head=%+v", result.Candidate, wantCandidateHead)
	}
	if result.Candidate.Head.AuthorityEpoch != sourceHead.AuthorityEpoch ||
		result.Candidate.Head.LastEventSeq != sourceHead.LastEventSeq ||
		result.Candidate.Head.SignerGeneration != sourceHead.SignerGeneration {
		t.Fatalf("candidate changed preserved authority fields: source=%+v candidate=%+v", sourceHead, result.Candidate.Head)
	}

	sourceBytesAfter, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceBytesBefore, sourceBytesAfter) {
		t.Fatal("PrepareUpgrade changed source main-file bytes")
	}
	if got, err := inspectWithPlan(ctx, InspectOptions{Path: sourcePath, MinimumHead: &sourceHead}, plan); err != nil || got.Head != sourceHead || got.SchemaVersion != current {
		t.Fatalf("source changed after prepare: inspection=%+v err=%v", got, err)
	}

	backup, err := verifyBackupWithPlan(ctx, backupPath, sourceHead, current, plan)
	if err != nil || backup.Head != sourceHead {
		t.Fatalf("verify old backup=%+v err=%v", backup, err)
	}
	candidate, err := inspectWithPlan(ctx, InspectOptions{Path: candidatePath, MinimumHead: &wantCandidateHead}, plan)
	if err != nil || candidate.Head != wantCandidateHead {
		t.Fatalf("verify candidate=%+v err=%v", candidate, err)
	}
	assertUpgradeTableExists(t, candidatePath, "upgrade_probe")
	assertStandaloneUpgradeArtifacts(t, dir, backupPath, candidatePath)
	for _, path := range []string{backupPath, candidatePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o want 600", path, info.Mode().Perm())
		}
	}

	for _, beforeName := range sourceEntriesBefore {
		if strings.HasPrefix(beforeName, ".daemon.db.tmp-") {
			t.Fatalf("unexpected source temp before prepare %q", beforeName)
		}
	}
}

func TestPrepareUpgradeReusesExactBackupAndReplacesOnlyExplicitCandidate(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon.db")
	backupPath := filepath.Join(dir, "backup.db")
	candidatePath := filepath.Join(dir, "candidate.db")
	store, err := Open(ctx, Options{Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "x", Kind: "y", JSON: []byte(`{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	plan := syntheticNextPlan()
	opts := UpgradeOptions{SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath, MinimumHead: &head}
	first, err := prepareUpgradeWithPlan(ctx, opts, plan)
	if err != nil {
		t.Fatal(err)
	}
	backupBefore, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareUpgradeWithPlan(ctx, opts, plan); err == nil || !strings.Contains(err.Error(), "candidate destination already exists") {
		t.Fatalf("candidate overwrite refusal error=%v", err)
	}
	opts.ReplaceCandidate = true
	second, err := prepareUpgradeWithPlan(ctx, opts, plan)
	if err != nil {
		t.Fatal(err)
	}
	backupAfter, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupBefore, backupAfter) {
		t.Fatal("existing exact backup was overwritten")
	}
	if first.Backup.Head != second.Backup.Head || first.Candidate.Head != second.Candidate.Head {
		t.Fatalf("resume changed logical artifacts: first=%+v second=%+v", first, second)
	}
	assertStandaloneUpgradeArtifacts(t, dir, backupPath, candidatePath)
}

func TestSyntheticUpgradeMatchesFreshTargetSchema(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	plan := syntheticNextPlan()
	sourcePath := filepath.Join(dir, "source.db")
	backupPath := filepath.Join(dir, "backup.db")
	candidatePath := filepath.Join(dir, "candidate.db")
	freshPath := filepath.Join(dir, "fresh.db")

	source, err := Open(ctx, Options{Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	head, err := source.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath}, plan); err != nil {
		t.Fatal(err)
	}
	fresh, err := openWithPlan(ctx, Options{Path: freshPath}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{candidatePath, freshPath} {
		got, err := inspectWithPlan(ctx, InspectOptions{Path: path}, plan)
		if err != nil || got.SchemaVersion != len(plan) || got.Status != InspectionCurrent {
			t.Fatalf("target inspection path=%s got=%+v err=%v", path, got, err)
		}
	}
	candidateManifest := schemaManifestForPath(t, candidatePath)
	freshManifest := schemaManifestForPath(t, freshPath)
	if !reflect.DeepEqual(candidateManifest, freshManifest) {
		t.Fatalf("migrated and fresh target manifests differ\nmigrated=%+v\nfresh=%+v", candidateManifest, freshManifest)
	}
	verifiedBackup, err := verifyBackupWithPlan(ctx, backupPath, head, len(plan)-1, plan)
	if err != nil || verifiedBackup.SchemaVersion != len(plan)-1 {
		t.Fatalf("backup version changed: backup=%+v err=%v", verifiedBackup, err)
	}
}

func TestAllPendingMigrationsRollBackTogether(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(privateTempDir(t), "daemon.db")
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	plan := currentMigrationPlan()
	current := len(plan)
	plan = append(plan,
		migration{version: current + 1, name: "first_pending", statements: []string{`CREATE TABLE first_pending(id INTEGER PRIMARY KEY) STRICT`}},
		migration{version: current + 2, name: "later_failure", statements: []string{`CREATE TABLE second_pending(id INTEGER PRIMARY KEY) STRICT`, `this is not valid sql`}},
	)
	db := rawDB(t, path)
	if _, _, err := migratePendingAtomically(ctx, db, plan, time.Now().UTC()); err == nil {
		t.Fatal("failing all-pending migration succeeded")
	}
	var version, ledgerRows, firstTable, secondTable int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='first_pending'`).Scan(&firstTable); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='second_pending'`).Scan(&secondTable); err != nil {
		t.Fatal(err)
	}
	afterHead, err := readAuthorityHead(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != current || ledgerRows != current || firstTable != 0 || secondTable != 0 || afterHead != head {
		t.Fatalf("partial upgrade survived: version=%d ledger=%d first=%d second=%d before=%+v after=%+v", version, ledgerRows, firstTable, secondTable, head, afterHead)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectionAndOpenRefuseFutureTamperAndWrongHeadWithoutWrites(t *testing.T) {
	t.Run("older normal open is read-only and distinguishable", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = openWithPlan(t.Context(), Options{Path: path}, syntheticNextPlan())
		if !errors.Is(err, ErrUpgradeRequired) {
			t.Fatalf("older open error=%v, want ErrUpgradeRequired", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("upgrade-required Open changed old source bytes")
		}
		assertNoSQLiteSidecars(t, path)
	})

	t.Run("future", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		db := rawDB(t, path)
		if _, err := db.Exec(`PRAGMA user_version=99`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectWithPlan(t.Context(), InspectOptions{Path: path}, syntheticNextPlan()); err == nil || !strings.Contains(err.Error(), "future schema version") {
			t.Fatalf("future inspection error=%v", err)
		}
	})

	t.Run("ledger tamper", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		db := rawDB(t, path)
		if _, err := db.Exec(`DROP TRIGGER schema_migrations_no_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE schema_migrations SET checksum='tampered' WHERE version=1`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectWithPlan(t.Context(), InspectOptions{Path: path}, syntheticNextPlan()); err == nil || !strings.Contains(err.Error(), "checksum drift") {
			t.Fatalf("tampered inspection error=%v", err)
		}
	})

	t.Run("wrong minimum head", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		head.HeadGeneration++
		if _, err := inspectWithPlan(t.Context(), InspectOptions{Path: path, MinimumHead: &head}, syntheticNextPlan()); !errors.Is(err, ErrRollback) {
			t.Fatalf("wrong-head inspection error=%v", err)
		}
	})
}

func TestQuiesceForReplacementRecoversCommittedWALAndRejectsUnsafeSidecars(t *testing.T) {
	t.Run("committed WAL", func(t *testing.T) {
		dir := privateTempDir(t)
		livePath := filepath.Join(dir, "live.db")
		crashPath := filepath.Join(dir, "crash.db")
		store, err := Open(t.Context(), Options{Path: livePath})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "x", Kind: "wal", JSON: []byte(`{"committed":true}`)}); err != nil {
			t.Fatal(err)
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		copyTestFile(t, livePath, crashPath)
		copyTestFile(t, livePath+"-wal", crashPath+"-wal")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		inspection, err := QuiesceForReplacement(t.Context(), QuiesceOptions{Path: crashPath, ExpectedSchemaVersion: len(migrations), ExpectedHead: head})
		if err != nil {
			t.Fatal(err)
		}
		if inspection.Head != head || inspection.SchemaVersion != len(migrations) {
			t.Fatalf("quiesced inspection=%+v", inspection)
		}
		assertNoSQLiteSidecars(t, crashPath)
		if _, err := Inspect(t.Context(), InspectOptions{Path: crashPath, MinimumHead: &head}); err != nil {
			t.Fatalf("reopen quiesced authority: %v", err)
		}
	})

	t.Run("symlink sidecar", func(t *testing.T) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "unrelated")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+"-wal"); err != nil {
			t.Fatal(err)
		}
		if _, err := QuiesceForReplacement(t.Context(), QuiesceOptions{Path: path, ExpectedSchemaVersion: 1, ExpectedHead: head}); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("unsafe sidecar error=%v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "keep" {
			t.Fatalf("symlink target changed: %q err=%v", got, err)
		}
	})
}

func TestReplaceCandidateRejectsUnsafeResidualSidecar(t *testing.T) {
	dir := privateTempDir(t)
	candidate := filepath.Join(dir, "candidate.db")
	target := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, candidate+"-wal"); err != nil {
		t.Fatal(err)
	}
	if err := prepareCandidateDestination(candidate, true, false); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("unsafe candidate cleanup error=%v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "keep" {
		t.Fatalf("candidate symlink target changed: %q err=%v", got, err)
	}
}

// syntheticNextPlan appends one probe migration on top of the shipped plan,
// so upgrade tests exercise a real pending upgrade without pinning themselves
// to whatever the current version happens to be.
func syntheticNextPlan() []migration {
	plan := currentMigrationPlan()
	return append(plan, migration{
		version: len(plan) + 1,
		name:    "synthetic_upgrade_probe",
		statements: []string{
			`CREATE TABLE upgrade_probe(id INTEGER PRIMARY KEY, value TEXT NOT NULL) STRICT`,
			`CREATE INDEX upgrade_probe_value ON upgrade_probe(value)`,
		},
	})
}

func schemaManifestForPath(t *testing.T, path string) []schemaObject {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	manifest, err := readSchemaManifest(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertUpgradeTableExists(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
		t.Fatalf("table %s count=%d err=%v", table, count, err)
	}
}

func assertStandaloneUpgradeArtifacts(t *testing.T, dir string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		assertSQLiteHeaderVersions(t, path, 1, 1)
		assertNoSQLiteSidecars(t, path)
	}
	for _, name := range directoryNames(t, dir) {
		if strings.Contains(name, ".tmp-") || strings.HasSuffix(name, ".preparing") ||
			strings.HasSuffix(name, ".maintenance-work") || strings.HasSuffix(name, ".maintenance-compact") {
			t.Fatalf("unexpected upgrade temporary artifact %q", name)
		}
	}
}

func assertNoSQLiteSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected SQLite sidecar %s error=%v", path+suffix, err)
		}
	}
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}
