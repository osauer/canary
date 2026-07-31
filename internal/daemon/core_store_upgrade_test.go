package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestCoreSchemaUpgradeResumesEveryDurableBoundary(t *testing.T) {
	phases := []string{
		coreSchemaPhaseIntent,
		coreSchemaPhaseCandidate,
		coreSchemaPhaseWatermark,
		coreSchemaPhaseQuiesced,
		coreSchemaPhaseRenamed,
		coreSchemaPhaseSynced,
		coreSchemaPhaseVerified,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			databasePath, source := newFakeSchemaAuthority(t)
			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(reached string) error {
				if reached == phase {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatalf("upgrade did not stop after %s", phase)
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists {
				t.Fatalf("durable manifest after %s: exists=%v err=%v", phase, exists, err)
			}
			artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			if err != nil {
				t.Fatal(err)
			}

			resumedMinimum, err := loadAuthorityWatermark(databasePath + ".head")
			if err != nil || resumedMinimum == nil {
				t.Fatalf("load resume watermark: head=%+v err=%v", resumedMinimum, err)
			}
			gotHead, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, resumedMinimum, time.Now(), fakeCoreSchemaUpgradeOps())
			if err != nil {
				t.Fatalf("resume after %s: %v", phase, err)
			}
			wantHead := source.Head
			wantHead.HeadGeneration++
			if gotHead == nil || *gotHead != wantHead {
				t.Fatalf("resumed head=%+v want %+v", gotHead, wantHead)
			}
			published := readFakeSchemaFile(t, databasePath)
			if published.Version != contractCachePruneMigrationVersion || published.Head != wantHead || published.Evidence != source.Evidence {
				t.Fatalf("published authority=%+v", published)
			}
			watermark, err := loadAuthorityWatermark(databasePath + ".head")
			if err != nil || watermark == nil || *watermark != wantHead {
				t.Fatalf("published watermark=%+v err=%v", watermark, err)
			}
			if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
				t.Fatalf("upgrade manifest pending=%v err=%v", pending, err)
			}
			if _, err := os.Stat(artifacts.backup); err != nil {
				t.Fatalf("retained pre-upgrade backup: %v", err)
			}
			if _, err := os.Lstat(artifacts.candidate); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("published candidate still exists: %v", err)
			}
		})
	}
}

func TestCoreSchemaUpgradePrepareFailureLeavesSourceAndWatermark(t *testing.T) {
	databasePath, source := newFakeSchemaAuthority(t)
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	minimum := source.Head
	ops := fakeCoreSchemaUpgradeOps()
	ops.prepare = func(context.Context, corestore.UpgradeOptions) (corestore.UpgradeResult, error) {
		return corestore.UpgradeResult{}, errors.New("backup unavailable")
	}
	if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
		t.Fatal("upgrade unexpectedly survived backup failure")
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("prepublication failure changed source database bytes")
	}
	watermark, err := loadAuthorityWatermark(databasePath + ".head")
	if err != nil || watermark == nil || *watermark != source.Head {
		t.Fatalf("prepublication watermark=%+v err=%v", watermark, err)
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err != nil || !pending {
		t.Fatalf("failed preparation manifest pending=%v err=%v", pending, err)
	}
}

func TestCoreSchemaUpgradeArmedCandidateNeverFallsBack(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, string)
	}{
		{name: "missing", tamper: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "changed", tamper: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"changed":true}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			databasePath, source := newFakeSchemaAuthority(t)
			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(phase string) error {
				if phase == coreSchemaPhaseWatermark {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatal("upgrade did not stop after arming watermark")
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists {
				t.Fatal(err)
			}
			artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, artifacts.candidate)
			armed, err := loadAuthorityWatermark(databasePath + ".head")
			if err != nil || armed == nil {
				t.Fatal(err)
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, armed, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
				t.Fatal("armed upgrade silently rebuilt or restored a missing/changed candidate")
			}
			live := readFakeSchemaFile(t, databasePath)
			if live.Version != 1 || live.Head != source.Head {
				t.Fatalf("failed closed upgrade changed source: %+v", live)
			}
		})
	}
}

func TestCoreSchemaUpgradeManifestStrictAndPrivate(t *testing.T) {
	databasePath, _ := newFakeSchemaAuthority(t)
	manifestPath := coreSchemaUpgradeManifestPath(databasePath)
	if err := os.WriteFile(manifestPath, []byte(`{"version":1,"unknown":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCoreSchemaUpgradeManifest(databasePath); err == nil {
		t.Fatal("manifest with unknown fields decoded")
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(databasePath), "manifest-target")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Fatal(err)
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err == nil || pending {
		t.Fatalf("symlink manifest pending=%v err=%v", pending, err)
	}
}

type fakeSchemaFile struct {
	Version         int                     `json:"version"`
	TargetVersion   int                     `json:"target_version,omitempty"`
	Head            corestore.AuthorityHead `json:"head"`
	Evidence        string                  `json:"evidence"`
	MaintenanceRows int64                   `json:"maintenance_rows,omitempty"`
}

func newFakeSchemaAuthority(t *testing.T) (string, fakeSchemaFile) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "daemon.db")
	source := fakeSchemaFile{
		Version:       1,
		TargetVersion: contractCachePruneMigrationVersion,
		Head: corestore.AuthorityHead{
			AuthorityEpoch:   "00112233445566778899aabbccddeeff",
			HeadGeneration:   7,
			LastEventSeq:     41,
			SignerGeneration: 3,
		},
		Evidence: "immutable-order-and-market-evidence",
	}
	writeFakeSchemaFile(t, path, source)
	if err := writeAuthorityWatermark(path+".head", source.Head); err != nil {
		t.Fatal(err)
	}
	return path, source
}

func fakeCoreSchemaUpgradeOps() coreSchemaUpgradeOps {
	return coreSchemaUpgradeOps{
		inspect:      fakeInspectSchema,
		prepare:      fakePrepareSchemaUpgrade,
		recompute:    fakeRecomputeSchemaUpgradeMaintenance,
		targetBackup: fakePrepareSchemaTargetBackup,
		quiesce: func(ctx context.Context, opts corestore.QuiesceOptions) (corestore.Inspection, error) {
			inspection, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.Path, MinimumHead: &opts.ExpectedHead})
			if err != nil {
				return corestore.Inspection{}, err
			}
			if inspection.SchemaVersion != opts.ExpectedSchemaVersion || inspection.Head != opts.ExpectedHead {
				return corestore.Inspection{}, fmt.Errorf("fake quiesce identity mismatch")
			}
			return inspection, nil
		},
	}
}

func fakeRecomputeSchemaUpgradeMaintenance(
	ctx context.Context,
	opts corestore.RecomputeUpgradeMaintenanceOptions,
) (corestore.UpgradeMaintenanceResult, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.SourcePath, MinimumHead: &opts.ExpectedHead, TargetVersion: opts.TargetVersion,
	})
	if err != nil {
		return corestore.UpgradeMaintenanceResult{}, err
	}
	if source.SchemaVersion != opts.ExpectedSchemaVersion || source.Head != opts.ExpectedHead {
		return corestore.UpgradeMaintenanceResult{}, fmt.Errorf("fake maintenance-proof source mismatch")
	}
	if opts.TargetVersion != contractCachePruneMigrationVersion {
		return corestore.UpgradeMaintenanceResult{}, nil
	}
	file, err := readFakeSchemaFileE(opts.SourcePath)
	if err != nil {
		return corestore.UpgradeMaintenanceResult{}, err
	}
	return corestore.UpgradeMaintenanceResult{
		Discards: []corestore.ObservationDiscardSummary{{
			MigrationVersion:    contractCachePruneMigrationVersion,
			MigrationName:       contractCachePruneMigrationName,
			Selector:            contractCachePruneSelector,
			RemovedRows:         file.MaintenanceRows,
			PayloadBytes:        file.MaintenanceRows * 100,
			OrderedDigestSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		}},
		Compacted:                      true,
		SourceBackupRetirementRequired: file.MaintenanceRows > 0,
	}, nil
}

func fakeInspectSchema(_ context.Context, opts corestore.InspectOptions) (corestore.Inspection, error) {
	info, err := os.Lstat(opts.Path)
	if err != nil {
		return corestore.Inspection{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return corestore.Inspection{}, fmt.Errorf("fake authority is not regular")
	}
	raw, err := os.ReadFile(opts.Path)
	if err != nil {
		return corestore.Inspection{}, err
	}
	var file fakeSchemaFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return corestore.Inspection{}, err
	}
	targetVersion := file.TargetVersion
	if targetVersion == 0 {
		targetVersion = contractCachePruneMigrationVersion
	}
	if opts.TargetVersion != 0 {
		if opts.TargetVersion > targetVersion {
			return corestore.Inspection{}, fmt.Errorf("unsupported fake target version %d", opts.TargetVersion)
		}
		targetVersion = opts.TargetVersion
	}
	if file.Version < 1 || file.Version > targetVersion {
		return corestore.Inspection{}, fmt.Errorf("unsupported fake schema version %d", file.Version)
	}
	if opts.MinimumHead != nil {
		minimum := *opts.MinimumHead
		if file.Head.AuthorityEpoch != minimum.AuthorityEpoch || file.Head.HeadGeneration < minimum.HeadGeneration || file.Head.LastEventSeq < minimum.LastEventSeq || file.Head.SignerGeneration < minimum.SignerGeneration {
			return corestore.Inspection{}, corestore.ErrRollback
		}
	}
	status := corestore.InspectionUpgradeRequired
	if file.Version == targetVersion {
		status = corestore.InspectionCurrent
	}
	var transition corestore.UpgradeHeadTransition
	if status == corestore.InspectionUpgradeRequired {
		transition = corestore.UpgradeHeadTransitionAdvanceOnce
		if file.Version == 3 && targetVersion == 4 {
			transition = corestore.UpgradeHeadTransitionPreserve
		}
	}
	return corestore.Inspection{
		Path: opts.Path, SchemaVersion: file.Version, TargetVersion: targetVersion,
		Status: status, Head: file.Head,
		Integrity:      corestore.IntegrityReport{QuickCheckResults: []string{"ok"}},
		HeadTransition: transition,
	}, nil
}

func fakePrepareSchemaUpgrade(ctx context.Context, opts corestore.UpgradeOptions) (corestore.UpgradeResult, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.SourcePath, MinimumHead: opts.MinimumHead, TargetVersion: opts.TargetVersion,
	})
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	if source.Status != corestore.InspectionUpgradeRequired {
		return corestore.UpgradeResult{}, fmt.Errorf("fake source is already current")
	}
	if opts.ResetUnboundArtifacts {
		for _, path := range []string{opts.CandidatePath, opts.BackupPath} {
			if info, err := os.Lstat(path); err == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return corestore.UpgradeResult{}, fmt.Errorf("fake unbound artifact is invalid")
				}
				if err := os.Remove(path); err != nil {
					return corestore.UpgradeResult{}, err
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return corestore.UpgradeResult{}, err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.BackupPath), 0o700); err != nil {
		return corestore.UpgradeResult{}, err
	}
	if _, err := os.Lstat(opts.BackupPath); errors.Is(err, fs.ErrNotExist) {
		file, readErr := readFakeSchemaFileE(opts.SourcePath)
		if readErr != nil {
			return corestore.UpgradeResult{}, readErr
		}
		if err := writeFakeSchemaFileE(opts.BackupPath, file); err != nil {
			return corestore.UpgradeResult{}, err
		}
	} else if err != nil {
		return corestore.UpgradeResult{}, err
	}
	backupInspection, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.BackupPath, MinimumHead: &source.Head, TargetVersion: opts.TargetVersion,
	})
	if err != nil || backupInspection.SchemaVersion != source.SchemaVersion || backupInspection.Head != source.Head {
		return corestore.UpgradeResult{}, fmt.Errorf("fake backup mismatch: %w", err)
	}
	if info, err := os.Lstat(opts.CandidatePath); err == nil {
		if !opts.ReplaceCandidate || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return corestore.UpgradeResult{}, fmt.Errorf("fake candidate cannot be replaced")
		}
		if err := os.Remove(opts.CandidatePath); err != nil {
			return corestore.UpgradeResult{}, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return corestore.UpgradeResult{}, err
	}
	sourceFile, err := readFakeSchemaFileE(opts.SourcePath)
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	targetVersion := sourceFile.TargetVersion
	if targetVersion == 0 {
		targetVersion = contractCachePruneMigrationVersion
	}
	if opts.TargetVersion != 0 {
		targetVersion = opts.TargetVersion
	}
	transition := corestore.UpgradeHeadTransitionAdvanceOnce
	if sourceFile.Version == 3 && targetVersion == 4 {
		transition = corestore.UpgradeHeadTransitionPreserve
	} else {
		sourceFile.Head.HeadGeneration++
	}
	sourceFile.Version = targetVersion
	if err := writeFakeSchemaFileE(opts.CandidatePath, sourceFile); err != nil {
		return corestore.UpgradeResult{}, err
	}
	candidate, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.CandidatePath, MinimumHead: &sourceFile.Head, TargetVersion: targetVersion,
	})
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	result := corestore.UpgradeResult{
		Source:         source,
		Backup:         corestore.BackupInfo{Path: opts.BackupPath, SchemaVersion: source.SchemaVersion, Head: source.Head, Integrity: source.Integrity},
		Candidate:      candidate,
		HeadTransition: transition,
	}
	if targetVersion == contractCachePruneMigrationVersion {
		result.Maintenance = corestore.UpgradeMaintenanceResult{
			Discards: []corestore.ObservationDiscardSummary{{
				MigrationVersion:    contractCachePruneMigrationVersion,
				MigrationName:       contractCachePruneMigrationName,
				Selector:            contractCachePruneSelector,
				RemovedRows:         sourceFile.MaintenanceRows,
				PayloadBytes:        sourceFile.MaintenanceRows * 100,
				OrderedDigestSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			}},
			Compacted:                      true,
			SourceBackupRetirementRequired: sourceFile.MaintenanceRows > 0,
		}
	}
	return result, nil
}

func fakePrepareSchemaTargetBackup(ctx context.Context, opts corestore.UpgradeTargetBackupOptions) (corestore.BackupInfo, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.SourcePath, MinimumHead: &opts.ExpectedHead})
	if err != nil {
		return corestore.BackupInfo{}, err
	}
	if source.Status != corestore.InspectionCurrent ||
		source.SchemaVersion != opts.ExpectedSchemaVersion ||
		source.Head != opts.ExpectedHead {
		return corestore.BackupInfo{}, fmt.Errorf("fake target-backup source mismatch")
	}
	if _, err := os.Lstat(opts.BackupPath); errors.Is(err, fs.ErrNotExist) {
		file, readErr := readFakeSchemaFileE(opts.SourcePath)
		if readErr != nil {
			return corestore.BackupInfo{}, readErr
		}
		if err := writeFakeSchemaFileE(opts.BackupPath, file); err != nil {
			return corestore.BackupInfo{}, err
		}
	} else if err != nil {
		return corestore.BackupInfo{}, err
	}
	backup, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.BackupPath, MinimumHead: &opts.ExpectedHead})
	if err != nil {
		return corestore.BackupInfo{}, err
	}
	if backup.Status != corestore.InspectionCurrent ||
		backup.SchemaVersion != opts.ExpectedSchemaVersion ||
		backup.Head != opts.ExpectedHead {
		return corestore.BackupInfo{}, fmt.Errorf("fake target backup mismatch")
	}
	return corestore.BackupInfo{
		Path:          opts.BackupPath,
		SchemaVersion: backup.SchemaVersion,
		Head:          backup.Head,
		Integrity:     backup.Integrity,
	}, nil
}

func writeFakeSchemaFile(t *testing.T, path string, file fakeSchemaFile) {
	t.Helper()
	if err := writeFakeSchemaFileE(path, file); err != nil {
		t.Fatal(err)
	}
}

func writeFakeSchemaFileE(path string, file fakeSchemaFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func readFakeSchemaFile(t *testing.T, path string) fakeSchemaFile {
	t.Helper()
	file, err := readFakeSchemaFileE(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func readFakeSchemaFileE(path string) (fakeSchemaFile, error) {
	var file fakeSchemaFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return file, err
	}
	return file, nil
}
