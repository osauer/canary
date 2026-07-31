package corestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPublicUpgradeAPIsHonorFrozenTargetVersion(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	plan := currentMigrationPlan()
	sourcePath := filepath.Join(dir, "daemon-v1.db")
	backupPath := filepath.Join(dir, "daemon-v1-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v2-candidate.db")
	targetBackupPath := filepath.Join(dir, "daemon-v2-backup.db")

	store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:1])
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

	inspection, err := Inspect(ctx, InspectOptions{
		Path: sourcePath, MinimumHead: &sourceHead, TargetVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.SchemaVersion != 1 ||
		inspection.TargetVersion != 2 ||
		inspection.Status != InspectionUpgradeRequired ||
		inspection.HeadTransition != UpgradeHeadTransitionAdvanceOnce {
		t.Fatalf("frozen v2 inspection=%+v", inspection)
	}
	currentInspection, err := Inspect(ctx, InspectOptions{Path: sourcePath, MinimumHead: &sourceHead})
	if err != nil {
		t.Fatal(err)
	}
	if currentInspection.TargetVersion != len(plan) {
		t.Fatalf("zero TargetVersion selected %d, want current %d", currentInspection.TargetVersion, len(plan))
	}

	result, err := PrepareUpgrade(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath,
		MinimumHead: &sourceHead, TargetVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHead, err := expectedUpgradeHead(sourceHead, UpgradeHeadTransitionAdvanceOnce)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.SchemaVersion != 1 ||
		result.Backup.SchemaVersion != 1 ||
		result.Candidate.SchemaVersion != 2 ||
		result.Candidate.TargetVersion != 2 ||
		result.Candidate.Status != InspectionCurrent ||
		result.Candidate.Head != wantHead ||
		result.HeadTransition != UpgradeHeadTransitionAdvanceOnce {
		t.Fatalf("frozen v1->v2 result=%+v", result)
	}
	next, err := Inspect(ctx, InspectOptions{
		Path: candidatePath, MinimumHead: &wantHead, TargetVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.SchemaVersion != 2 ||
		next.TargetVersion != 3 ||
		next.Status != InspectionUpgradeRequired ||
		next.HeadTransition != UpgradeHeadTransitionAdvanceOnce {
		t.Fatalf("frozen v3 follow-up inspection=%+v", next)
	}
	targetBackup, err := PrepareUpgradeTargetBackup(ctx, UpgradeTargetBackupOptions{
		SourcePath: candidatePath, BackupPath: targetBackupPath,
		ExpectedSchemaVersion: 2, ExpectedHead: wantHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if targetBackup.SchemaVersion != 2 || targetBackup.Head != wantHead {
		t.Fatalf("historical target backup=%+v", targetBackup)
	}
	for _, target := range []int{-1, len(plan) + 1} {
		if _, err := Inspect(ctx, InspectOptions{Path: sourcePath, TargetVersion: target}); err == nil {
			t.Errorf("unsupported inspection target %d was accepted", target)
		}
		if _, err := PrepareUpgrade(ctx, UpgradeOptions{
			SourcePath: sourcePath, BackupPath: filepath.Join(dir, "unused-backup.db"),
			CandidatePath: filepath.Join(dir, "unused-candidate.db"), TargetVersion: target,
		}); err == nil {
			t.Errorf("unsupported upgrade target %d was accepted", target)
		}
	}
}

func TestRecomputeUpgradeMaintenanceMatchesPreparationWithoutMutation(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	plan := currentMigrationPlan()
	sourcePath := filepath.Join(dir, "daemon-v3.db")
	backupPath := filepath.Join(dir, "daemon-v3-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v4-candidate.db")

	store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:3])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendObservations(ctx, []ObservationInput{
		pruneObservationInput(
			"market/contracts",
			"ibkr.tws.contract_details",
			"contract_cache.snapshot.v3",
			bytes.Repeat([]byte("discard"), 128),
			0,
		),
		pruneObservationInput(
			"market/contracts",
			"ibkr.tws.contract_details",
			"contract_cache.snapshot.v3.near",
			[]byte("preserve"),
			1,
		),
	}); err != nil {
		t.Fatal(err)
	}
	sourceHead, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareUpgrade(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath,
		MinimumHead: &sourceHead, TargetVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSidecars, err := snapshotReadOnlySidecars(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	recomputed, err := RecomputeUpgradeMaintenance(ctx, RecomputeUpgradeMaintenanceOptions{
		SourcePath: backupPath, ExpectedSchemaVersion: 3,
		TargetVersion: 4, ExpectedHead: sourceHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recomputed, prepared.Maintenance) {
		t.Fatalf("recomputed maintenance=%+v\nprepared maintenance=%+v", recomputed, prepared.Maintenance)
	}
	afterBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	afterSidecars, err := snapshotReadOnlySidecars(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) || !reflect.DeepEqual(beforeSidecars, afterSidecars) {
		t.Fatalf("read-only recomputation changed source backup: bytes_equal=%t sidecars_before=%v sidecars_after=%v",
			bytes.Equal(beforeBytes, afterBytes), beforeSidecars, afterSidecars)
	}

	wrongHead := sourceHead
	wrongHead.HeadGeneration++
	if _, err := RecomputeUpgradeMaintenance(ctx, RecomputeUpgradeMaintenanceOptions{
		SourcePath: backupPath, ExpectedSchemaVersion: 3,
		TargetVersion: 4, ExpectedHead: wrongHead,
	}); !errors.Is(err, ErrRollback) {
		t.Fatalf("wrong maintenance head error=%v, want ErrRollback", err)
	}
	if _, err := RecomputeUpgradeMaintenance(ctx, RecomputeUpgradeMaintenanceOptions{
		SourcePath: backupPath, ExpectedSchemaVersion: 2,
		TargetVersion: 4, ExpectedHead: sourceHead,
	}); !errors.Is(err, ErrRollback) {
		t.Fatalf("wrong maintenance schema error=%v, want ErrRollback", err)
	}
}

func TestResetUnboundUpgradeArtifactsPreservesSourceAndRebuildsFromOnlySource(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	plan := currentMigrationPlan()
	sourcePath := filepath.Join(dir, "daemon-v3.db")
	backupPath := filepath.Join(dir, "daemon-v3-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v4-candidate.db")

	store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:3])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendObservations(ctx, []ObservationInput{
		pruneObservationInput(
			"market/contracts",
			"ibkr.tws.contract_details",
			"contract_cache.snapshot.v3",
			bytes.Repeat([]byte("bloated"), 128*1024),
			0,
		),
	}); err != nil {
		t.Fatal(err)
	}
	sourceHead, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	opts := UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath,
		MinimumHead: &sourceHead, TargetVersion: 4,
	}
	first, err := PrepareUpgrade(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareUpgrade(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath,
		MinimumHead: &sourceHead, TargetVersion: 4, ResetUnboundArtifacts: true,
	}); err == nil {
		t.Fatal("unbound reset without ReplaceCandidate was accepted")
	}
	for _, path := range []string{
		upgradeWorkPath(candidatePath),
		upgradeCompactPath(candidatePath),
		upgradeBackupPreparingPath(backupPath),
		candidatePath + "-wal",
	} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetUnboundUpgradeArtifacts(ctx, first.Source, backupPath, candidatePath, plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		backupPath,
		upgradeBackupPreparingPath(backupPath),
		candidatePath,
		candidatePath + "-wal",
		upgradeWorkPath(candidatePath),
		upgradeCompactPath(candidatePath),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unbound artifact survived reset %s: %v", path, err)
		}
	}
	afterResetBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceBytes, afterResetBytes) {
		t.Fatal("unbound reset changed source database bytes")
	}
	sourceAfterReset, err := Inspect(ctx, InspectOptions{
		Path: sourcePath, MinimumHead: &sourceHead, TargetVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterReset.SchemaVersion != 3 || sourceAfterReset.Head != sourceHead {
		t.Fatalf("source changed during reset: %+v", sourceAfterReset)
	}

	opts.ReplaceCandidate = true
	opts.ResetUnboundArtifacts = true
	rebuilt, err := PrepareUpgrade(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Candidate.SchemaVersion != 4 || rebuilt.Candidate.Head != sourceHead {
		t.Fatalf("rebuilt candidate=%+v", rebuilt.Candidate)
	}

	// A crash can leave only an unbound source backup. Corrupting that copy
	// proves the reset removes it before the normal backup reuse path can inspect
	// or adopt it; preparation then succeeds by rebuilding from the exact source.
	if err := os.Remove(candidatePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte("invalid unbound backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareUpgrade(ctx, opts); err != nil {
		t.Fatalf("preparing reset did not discard unbound backup before rebuild: %v", err)
	}
	finalSourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceBytes, finalSourceBytes) {
		t.Fatal("preparing reset or rebuild changed source database bytes")
	}
}
