package corestore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestContractCacheObservationPruneIsExactCompactAndHeadPreserving(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon.db")
	backupPath := filepath.Join(dir, "daemon-v3-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v4-candidate.db")
	targetBackupPath := filepath.Join(dir, "daemon-v4-backup.db")
	plan := currentMigrationPlan()[:contractCachePruneMigrationVersionForTest]
	if len(plan) != 4 {
		t.Fatalf("test fixture expects schema 4, got %d", len(plan))
	}

	store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:3])
	if err != nil {
		t.Fatal(err)
	}
	currentState := []byte(`{"contracts":[{"conid":123,"route":"SMART"}],"format":"current"}`)
	if _, err := store.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
		ScopeKey: "market/contracts",
		Kind:     "contract_cache.current.v3",
		JSON:     currentState,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvents(ctx, []EventInput{{
		ScopeKey: "daemon", EventKey: "prune-control", Type: "test.control", Action: "record", Origin: "test",
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(), PayloadJSON: []byte(`{"keep":true}`),
	}}); err != nil {
		t.Fatal(err)
	}
	exactPayloadA := bytes.Repeat([]byte("a"), 512*1024)
	exactPayloadB := bytes.Repeat([]byte("b"), 512*1024)
	inputs := []ObservationInput{
		pruneObservationInput("market/contracts", "ibkr.tws.contract_details", "contract_cache.snapshot.v3", exactPayloadA, 0),
		pruneObservationInput("market/contracts/near", "ibkr.tws.contract_details", "contract_cache.snapshot.v3", []byte("near-scope"), 1),
		pruneObservationInput("market/contracts", "ibkr.tws.contract_details", "contract_cache.snapshot.v3", exactPayloadB, 2),
		pruneObservationInput("market/contracts", "ibkr.tws.contract_details.near", "contract_cache.snapshot.v3", []byte("near-source"), 3),
		pruneObservationInput("market/contracts", "ibkr.tws.contract_details", "contract_cache.snapshot.v3.near", []byte("near-kind"), 4),
		pruneObservationInput("unrelated", "unrelated", "unrelated", []byte("unrelated"), 5),
	}
	receipts, err := store.AppendObservations(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	sourceHead, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	sourceDB := openReadOnlyTestDB(t, sourcePath)
	beforeMeta := snapshotTable(t, sourceDB, "store_meta")
	beforeEvents := snapshotTable(t, sourceDB, "event_log")
	beforeState := snapshotTable(t, sourceDB, "state_documents")
	beforeObservations := snapshotTable(t, sourceDB, "observations")
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}

	inspection, err := inspectWithPlan(ctx, InspectOptions{Path: sourcePath, MinimumHead: &sourceHead}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != InspectionUpgradeRequired || inspection.HeadTransition != UpgradeHeadTransitionPreserve {
		t.Fatalf("v3 inspection transition=%q status=%q", inspection.HeadTransition, inspection.Status)
	}
	transition, err := ExpectedUpgradeHeadTransition(3, 4)
	if err != nil || transition != UpgradeHeadTransitionPreserve {
		t.Fatalf("v3->v4 transition=%q err=%v", transition, err)
	}

	result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath, MinimumHead: &sourceHead,
	}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.HeadTransition != UpgradeHeadTransitionPreserve || result.Candidate.Head != sourceHead {
		t.Fatalf("maintenance head transition=%q source=%+v candidate=%+v", result.HeadTransition, sourceHead, result.Candidate.Head)
	}
	if result.Source.Head != sourceHead || result.Backup.Head != sourceHead || result.Backup.SchemaVersion != 3 {
		t.Fatalf("source or old backup changed: source=%+v backup=%+v", result.Source, result.Backup)
	}
	if result.TargetBackup != nil {
		t.Fatalf("PrepareUpgrade created a target backup before publication: %+v", result.TargetBackup)
	}
	if !result.Maintenance.Compacted || !result.Maintenance.SourceBackupRetirementRequired || len(result.Maintenance.Discards) != 1 {
		t.Fatalf("maintenance result=%+v", result.Maintenance)
	}
	summary := result.Maintenance.Discards[0]
	wantSelector := ObservationDiscardSelector{
		ScopeKey: "market/contracts", Source: "ibkr.tws.contract_details", Kind: "contract_cache.snapshot.v3",
	}
	wantDigest := expectedDiscardDigest(wantSelector, []discardDigestRow{
		{id: receipts[0].ID, digest: receipts[0].PayloadSHA256},
		{id: receipts[2].ID, digest: receipts[2].PayloadSHA256},
	})
	if summary.MigrationVersion != 4 || summary.MigrationName != "contract_cache_observation_prune" || summary.Selector != wantSelector ||
		summary.RemovedRows != 2 || summary.PayloadBytes != int64(len(exactPayloadA)+len(exactPayloadB)) || summary.OrderedDigestSHA256 != wantDigest {
		t.Fatalf("discard summary=%+v want rows=2 bytes=%d digest=%s", summary, len(exactPayloadA)+len(exactPayloadB), wantDigest)
	}

	candidateDB := openReadOnlyTestDB(t, candidatePath)
	if got := snapshotTable(t, candidateDB, "store_meta"); !reflect.DeepEqual(got, beforeMeta) {
		t.Fatalf("store_meta changed in maintenance-only migration:\nbefore=%v\nafter =%v", beforeMeta, got)
	}
	if got := snapshotTable(t, candidateDB, "event_log"); !reflect.DeepEqual(got, beforeEvents) {
		t.Fatalf("event_log changed:\nbefore=%v\nafter =%v", beforeEvents, got)
	}
	if got := snapshotTable(t, candidateDB, "state_documents"); !reflect.DeepEqual(got, beforeState) {
		t.Fatalf("current state changed:\nbefore=%v\nafter =%v", beforeState, got)
	}
	wantObservations := observationsWithoutExactDiscard(beforeObservations, wantSelector)
	if got := snapshotTable(t, candidateDB, "observations"); !reflect.DeepEqual(got, wantObservations) {
		t.Fatalf("observations changed beyond exact discard:\nwant=%v\ngot =%v", wantObservations, got)
	}
	var triggerSQL string
	if err := candidateDB.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='observations_no_delete'`).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	if triggerSQL != appendOnlyDeleteTrigger("observations") {
		t.Fatalf("delete trigger=%q want %q", triggerSQL, appendOnlyDeleteTrigger("observations"))
	}
	if err := candidateDB.Close(); err != nil {
		t.Fatal(err)
	}

	guardedDB := rawDB(t, candidatePath)
	if _, err := guardedDB.Exec(`DELETE FROM observations WHERE scope_key='unrelated'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("restored delete guard error=%v", err)
	}
	if err := guardedDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := normalizeBackupJournal(ctx, candidatePath); err != nil {
		t.Fatal(err)
	}
	if err := removeBackupSidecars(candidatePath); err != nil {
		t.Fatal(err)
	}

	sourceInfo, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	candidateInfo, err := os.Stat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if candidateInfo.Size() >= sourceInfo.Size() {
		t.Fatalf("compaction did not shrink candidate: candidate=%d old-backup=%d", candidateInfo.Size(), sourceInfo.Size())
	}

	targetBackup, err := PrepareUpgradeTargetBackup(ctx, UpgradeTargetBackupOptions{
		SourcePath: candidatePath, BackupPath: targetBackupPath,
		ExpectedSchemaVersion: 4, ExpectedHead: sourceHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if targetBackup.SchemaVersion != 4 || targetBackup.Head != sourceHead || !targetBackup.Integrity.OK() {
		t.Fatalf("target backup=%+v", targetBackup)
	}
	targetInfo, err := os.Stat(targetBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(candidateInfo, targetInfo) {
		t.Fatal("target backup is a hard link to the candidate")
	}
	targetBytes, err := os.ReadFile(targetBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upgradeBackupPreparingPath(targetBackupPath), []byte("interrupted target copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	reusedTarget, err := PrepareUpgradeTargetBackup(ctx, UpgradeTargetBackupOptions{
		SourcePath: candidatePath, BackupPath: targetBackupPath,
		ExpectedSchemaVersion: 4, ExpectedHead: sourceHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	reusedBytes, err := os.ReadFile(targetBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if reusedTarget.Head != targetBackup.Head || !bytes.Equal(targetBytes, reusedBytes) {
		t.Fatal("reusing target backup overwrote or changed the verified artifact")
	}
	if _, err := os.Lstat(upgradeBackupPreparingPath(targetBackupPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target backup preparing artifact survived reuse: %v", err)
	}
	assertStandaloneUpgradeArtifacts(t, dir, backupPath, candidatePath, targetBackupPath)
}

func TestExpectedUpgradeHeadTransitionSupportsFrozenPlanPrefixes(t *testing.T) {
	for _, tc := range []struct {
		source int
		target int
		want   UpgradeHeadTransition
	}{{1, 2, UpgradeHeadTransitionAdvanceOnce}, {1, 3, UpgradeHeadTransitionAdvanceOnce}, {2, 3, UpgradeHeadTransitionAdvanceOnce}, {3, 4, UpgradeHeadTransitionPreserve}, {3, 5, UpgradeHeadTransitionAdvanceOnce}, {4, 5, UpgradeHeadTransitionAdvanceOnce}} {
		got, err := ExpectedUpgradeHeadTransition(tc.source, tc.target)
		if err != nil || got != tc.want {
			t.Errorf("transition %d->%d = %q err=%v want %q", tc.source, tc.target, got, err, tc.want)
		}
	}
	for _, versions := range [][2]int{{0, 2}, {1, 1}, {1, 99}} {
		if _, err := ExpectedUpgradeHeadTransition(versions[0], versions[1]); err == nil {
			t.Errorf("unsupported transition %d->%d was accepted", versions[0], versions[1])
		}
	}
}

func TestUpgradeHeadTransitionMatrixAndZeroRowMaintenance(t *testing.T) {
	plan := currentMigrationPlan()
	for _, tc := range []struct {
		version int
		want    UpgradeHeadTransition
	}{{1, UpgradeHeadTransitionAdvanceOnce}, {2, UpgradeHeadTransitionAdvanceOnce}, {3, UpgradeHeadTransitionAdvanceOnce}, {4, UpgradeHeadTransitionAdvanceOnce}} {
		t.Run(string(rune('0'+tc.version)), func(t *testing.T) {
			dir := privateTempDir(t)
			sourcePath := filepath.Join(dir, "daemon.db")
			store, err := openWithPlan(t.Context(), Options{Path: sourcePath}, plan[:tc.version])
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
			inspection, err := inspectWithPlan(t.Context(), InspectOptions{Path: sourcePath}, plan)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.HeadTransition != tc.want {
				t.Fatalf("inspection transition=%q want %q", inspection.HeadTransition, tc.want)
			}
			if got, err := ExpectedUpgradeHeadTransition(tc.version, len(plan)); err != nil || got != tc.want {
				t.Fatalf("expected transition=%q err=%v want %q", got, err, tc.want)
			}
			result, err := prepareUpgradeWithPlan(t.Context(), UpgradeOptions{
				SourcePath:    sourcePath,
				BackupPath:    filepath.Join(dir, "backup.db"),
				CandidatePath: filepath.Join(dir, "candidate.db"),
			}, plan)
			if err != nil {
				t.Fatal(err)
			}
			wantHead, err := expectedUpgradeHead(head, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if result.HeadTransition != tc.want || result.Candidate.Head != wantHead {
				t.Fatalf("result transition=%q candidate=%+v want %q %+v", result.HeadTransition, result.Candidate.Head, tc.want, wantHead)
			}
			wantObservationDiscards := 0
			if tc.version < contractCachePruneMigrationVersionForTest {
				wantObservationDiscards = 1
			}
			if len(result.Maintenance.Discards) != wantObservationDiscards || len(result.Maintenance.EventDiscards) != 1 ||
				result.Maintenance.EventDiscards[0].RemovedRows != 0 ||
				result.Maintenance.SourceBackupRetirementRequired || result.TargetBackup != nil || !result.Maintenance.Compacted {
				t.Fatalf("zero-row maintenance=%+v target=%+v", result.Maintenance, result.TargetBackup)
			}
		})
	}
}

const contractCachePruneMigrationVersionForTest = 4

func TestMaintenanceUpgradeRebuildsOnlyNamedCrashArtifacts(t *testing.T) {
	dir := privateTempDir(t)
	plan := currentMigrationPlan()
	sourcePath := filepath.Join(dir, "daemon.db")
	backupPath := filepath.Join(dir, "backup.db")
	candidatePath := filepath.Join(dir, "candidate.db")
	store, err := openWithPlan(t.Context(), Options{Path: sourcePath}, plan[:3])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	opts := UpgradeOptions{SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath}
	if _, err := prepareUpgradeWithPlan(t.Context(), opts, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(candidatePath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		upgradeWorkPath(candidatePath),
		upgradeCompactPath(candidatePath),
		upgradeBackupPreparingPath(backupPath),
	} {
		if err := os.WriteFile(path, []byte("interrupted unpublished artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts.ReplaceCandidate = true
	if _, err := prepareUpgradeWithPlan(t.Context(), opts, plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		upgradeWorkPath(candidatePath),
		upgradeCompactPath(candidatePath),
		upgradeBackupPreparingPath(backupPath),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("named crash artifact survived %s: %v", path, err)
		}
	}
}

func TestCheckpointUpgradeWorkingSnapshotTruncatesDeleteWALBeforeVacuum(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "working.db")
	if err := createPrivateEmptyUpgradeArtifact(path, "test working database"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE probe(payload BLOB NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe(payload) VALUES(zeroblob(1048576))`); err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.Stat(path + "-wal")
	if err != nil || walBefore.Size() == 0 {
		t.Fatalf("fixture WAL size=%d err=%v", fileSize(walBefore), err)
	}
	if err := checkpointUpgradeWorkingSnapshot(t.Context(), db, path); err != nil {
		t.Fatal(err)
	}
	if walAfter, err := os.Stat(path + "-wal"); err == nil && walAfter.Size() != 0 {
		t.Fatalf("checkpoint left %d WAL bytes before VACUUM INTO", walAfter.Size())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContractCachePruneMaintenanceMetadataCannotWiden(t *testing.T) {
	valid := contractCacheObservationPrune()
	if err := validateMigrationStatements(valid); err != nil {
		t.Fatalf("valid maintenance rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*migration)
	}{
		{"selector", func(m *migration) { m.maintenance.ObservationDiscard.Kind += ".near" }},
		{"extra statement", func(m *migration) { m.statements = append(m.statements, `SELECT 1`) }},
		{"head default", func(m *migration) { m.maintenance.PreserveAuthorityHead = false }},
		{"missing approval", func(m *migration) { m.destructive.statements = m.destructive.statements[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cloneMigrationPlan([]migration{valid})[0]
			tc.mutate(&m)
			if err := validateMigrationStatements(m); err == nil {
				t.Fatal("widened maintenance metadata was accepted")
			}
		})
	}
}

func pruneObservationInput(scope, source, kind string, payload []byte, minute int) ObservationInput {
	return ObservationInput{
		ScopeKey: scope, Source: source, Kind: kind,
		ObservedAt:  time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(minute) * time.Minute),
		ContentType: "application/octet-stream", Payload: payload,
	}
}

type discardDigestRow struct {
	id     int64
	digest [sha256.Size]byte
}

func expectedDiscardDigest(selector ObservationDiscardSelector, rows []discardDigestRow) string {
	h := sha256.New()
	h.Write([]byte("canary.observation-discard.v1\x00"))
	for _, value := range []string{selector.ScopeKey, selector.Source, selector.Kind} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	for _, row := range rows {
		var identity [8]byte
		binary.BigEndian.PutUint64(identity[:], uint64(row.id))
		h.Write(identity[:])
		h.Write(row.digest[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func observationsWithoutExactDiscard(rows []map[string]any, selector ObservationDiscardSelector) []map[string]any {
	kept := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row["scope_key"] == selector.ScopeKey && row["source"] == selector.Source && row["kind"] == selector.Kind {
			continue
		}
		kept = append(kept, row)
	}
	return kept
}

func openReadOnlyTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(t.Context()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
