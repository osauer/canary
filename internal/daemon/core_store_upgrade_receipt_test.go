package daemon

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestCoreSchemaMaintenanceUpgradeResumesEveryDurableBoundaryWithoutRestampingHead(t *testing.T) {
	phases := []string{
		coreSchemaPhaseIntent,
		coreSchemaPhaseCandidate,
		coreSchemaPhaseWatermark,
		coreSchemaPhaseQuiesced,
		coreSchemaPhaseRenamed,
		coreSchemaPhaseSynced,
		coreSchemaPhaseVerified,
		coreSchemaPhaseTarget,
		coreSchemaPhaseReceipt,
		coreSchemaPhaseRetired,
		coreSchemaPhaseRetireSync,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			databasePath, source := newFakeMaintenanceSchemaAuthority(t, 2)
			watermarkPath := databasePath + ".head"
			fixedModTime := time.Unix(1_700_000_000, 123_000_000)
			if err := os.Chtimes(watermarkPath, fixedModTime, fixedModTime); err != nil {
				t.Fatal(err)
			}
			watermarkBefore, err := os.ReadFile(watermarkPath)
			if err != nil {
				t.Fatal(err)
			}
			watermarkInfoBefore, err := os.Stat(watermarkPath)
			if err != nil {
				t.Fatal(err)
			}

			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(reached string) error {
				if reached == phase {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatalf("maintenance upgrade did not stop after %s", phase)
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists {
				t.Fatalf("durable maintenance manifest after %s: exists=%v err=%v", phase, exists, err)
			}
			artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			if err != nil {
				t.Fatal(err)
			}

			resumedMinimum, err := loadAuthorityWatermark(watermarkPath)
			if err != nil || resumedMinimum == nil {
				t.Fatalf("load preserved watermark: head=%+v err=%v", resumedMinimum, err)
			}
			gotHead, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, resumedMinimum, time.Now(), fakeCoreSchemaUpgradeOps())
			if err != nil {
				t.Fatalf("resume maintenance after %s: %v", phase, err)
			}
			if gotHead == nil || *gotHead != source.Head {
				t.Fatalf("maintenance head=%+v want preserved %+v", gotHead, source.Head)
			}
			published := readFakeSchemaFile(t, databasePath)
			if published.Version != contractCachePruneMigrationVersion ||
				published.Head != source.Head ||
				published.Evidence != source.Evidence {
				t.Fatalf("published maintenance authority=%+v", published)
			}
			watermarkAfter, err := os.ReadFile(watermarkPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(watermarkBefore, watermarkAfter) {
				t.Fatal("head-preserving maintenance rewrote watermark bytes")
			}
			info, err := os.Stat(watermarkPath)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(fixedModTime) {
				t.Fatalf("head-preserving maintenance restamped watermark mtime: got %s want %s", info.ModTime(), fixedModTime)
			}
			if !os.SameFile(watermarkInfoBefore, info) {
				t.Fatal("head-preserving maintenance replaced watermark inode")
			}
			if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
				t.Fatalf("maintenance manifest pending=%v err=%v", pending, err)
			}
			if _, err := os.Lstat(artifacts.backup); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("oversized source backup was not retired: %v", err)
			}
			if _, err := os.Stat(artifacts.targetBackup); err != nil {
				t.Fatalf("compact target backup missing: %v", err)
			}
			receipt, ok, err := loadCoreSchemaMaintenanceReceipt(artifacts.receipt)
			if err != nil || !ok {
				t.Fatalf("maintenance receipt: ok=%v err=%v", ok, err)
			}
			if receipt.UpgradeID != manifest.UpgradeID ||
				receipt.Discard.MigrationVersion != contractCachePruneMigrationVersion ||
				receipt.Discard.Selector != contractCachePruneSelector ||
				receipt.Discard.RemovedRows != 2 ||
				receipt.Discard.PayloadBytes != 200 ||
				receipt.Source.SchemaVersion != contractCachePruneMigrationVersion-1 ||
				receipt.Source.Head != source.Head ||
				receipt.Target.SchemaVersion != contractCachePruneMigrationVersion ||
				receipt.Target.Head != source.Head {
				t.Fatalf("maintenance receipt does not bind exact repair: %+v", receipt)
			}
			targetDigest, targetBytes, err := hashPrivateUpgradeArtifact(artifacts.targetBackup)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Target.SHA256 != targetDigest || receipt.Target.Bytes != targetBytes {
				t.Fatalf("maintenance receipt target fingerprint=%+v want %s/%d", receipt.Target, targetDigest, targetBytes)
			}
			if err := requireIndependentUpgradeArtifacts(databasePath, artifacts.targetBackup); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoreSchemaMaintenanceZeroRowsRetainsSourceBackup(t *testing.T) {
	databasePath, source := newFakeMaintenanceSchemaAuthority(t, 0)
	minimum := source.Head
	gotHead, err := ensureCoreStoreSchemaCurrentWithOps(
		t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotHead == nil || *gotHead != source.Head {
		t.Fatalf("zero-row maintenance changed head: got=%+v want=%+v", gotHead, source.Head)
	}
	_, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
	if err != nil || exists {
		t.Fatalf("completed zero-row manifest exists=%v err=%v", exists, err)
	}

	// The completed manifest is gone, so locate only this test's private backup.
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(databasePath), "backups", "*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("zero-row maintenance backups=%v, want retained source backup only", backups)
	}
	if _, err := fakeInspectSchema(t.Context(), corestore.InspectOptions{Path: backups[0], MinimumHead: &source.Head}); err != nil {
		t.Fatalf("retained source backup is invalid: %v", err)
	}
	receipts, err := filepath.Glob(filepath.Join(filepath.Dir(databasePath), "backups", "*.maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("zero-row maintenance created retirement receipt: %v", receipts)
	}
}

func TestCoreSchemaMaintenanceFailsClosedWithoutReceiptAfterPublication(t *testing.T) {
	databasePath, source := newFakeMaintenanceSchemaAuthority(t, 1)
	minimum := source.Head
	ops := fakeCoreSchemaUpgradeOps()
	ops.after = func(phase string) error {
		if phase == coreSchemaPhaseVerified {
			return errors.New("injected crash")
		}
		return nil
	}
	if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
		t.Fatal("maintenance did not stop after publication verification")
	}
	manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
	if err != nil || !exists {
		t.Fatal(err)
	}
	artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifacts.backup); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
		t.Fatal("missing source backup without receipt was accepted")
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err != nil || !pending {
		t.Fatalf("failed-closed maintenance manifest pending=%v err=%v", pending, err)
	}
}

func TestCoreSchemaMaintenanceRejectsRetirementClaimNotProvedBySourceBackup(t *testing.T) {
	for _, test := range []struct {
		name   string
		rows   int64
		tamper func(*coreSchemaUpgradeMaintenance)
	}{
		{
			name: "zero rows promoted to retirement",
			rows: 0,
			tamper: func(maintenance *coreSchemaUpgradeMaintenance) {
				maintenance.Discard.RemovedRows = 1
				maintenance.RetireSourceBackup = true
			},
		},
		{
			name: "removed row count",
			rows: 1,
			tamper: func(maintenance *coreSchemaUpgradeMaintenance) {
				maintenance.Discard.RemovedRows++
			},
		},
		{
			name: "payload bytes",
			rows: 1,
			tamper: func(maintenance *coreSchemaUpgradeMaintenance) {
				maintenance.Discard.PayloadBytes++
			},
		},
		{
			name: "removed set digest",
			rows: 1,
			tamper: func(maintenance *coreSchemaUpgradeMaintenance) {
				maintenance.Discard.OrderedDigestSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			databasePath, source := newFakeMaintenanceSchemaAuthority(t, test.rows)
			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(phase string) error {
				if phase == coreSchemaPhaseVerified {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatal("maintenance did not stop after publication verification")
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists || manifest.Maintenance == nil {
				t.Fatalf("load maintenance manifest: exists=%v manifest=%+v err=%v", exists, manifest, err)
			}
			artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			if err != nil {
				t.Fatal(err)
			}

			// These edits remain structurally valid and leave every database
			// fingerprint unchanged. Only recomputation from the immutable
			// source backup can reject the false maintenance claim.
			test.tamper(manifest.Maintenance)
			if err := writeCoreSchemaUpgradeManifest(databasePath, manifest); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
				t.Fatal("unproved maintenance summary was accepted")
			}
			if _, err := os.Stat(artifacts.backup); err != nil {
				t.Fatalf("source backup was not retained after proof mismatch: %v", err)
			}
			if _, err := os.Lstat(artifacts.targetBackup); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("target backup created before source proof: %v", err)
			}
			if _, err := os.Lstat(artifacts.receipt); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("receipt created before source proof: %v", err)
			}
		})
	}
}

func TestCoreSchemaMaintenanceReadyCandidateNeverRebuildsWithPreservedWatermark(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, string)
	}{
		{
			name: "missing",
			tamper: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed",
			tamper: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"changed":true}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			databasePath, source := newFakeMaintenanceSchemaAuthority(t, 1)
			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(phase string) error {
				if phase == coreSchemaPhaseCandidate {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatal("maintenance did not stop at ready candidate")
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
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
				t.Fatal("ready preserved-head candidate was silently rebuilt")
			}
			live := readFakeSchemaFile(t, databasePath)
			if live.Version != contractCachePruneMigrationVersion-1 || live.Head != source.Head {
				t.Fatalf("failed-closed candidate recovery changed source: %+v", live)
			}
		})
	}
}

func TestCoreSchemaMaintenanceRejectsTamperedReceiptAndMissingTarget(t *testing.T) {
	for _, test := range []struct {
		name       string
		crashPhase string
		tamper     func(*testing.T, coreSchemaUpgradeArtifacts)
	}{
		{
			name:       "tampered receipt",
			crashPhase: coreSchemaPhaseReceipt,
			tamper: func(t *testing.T, artifacts coreSchemaUpgradeArtifacts) {
				if err := os.WriteFile(artifacts.receipt, []byte(`{"changed":true}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "missing target after retirement",
			crashPhase: coreSchemaPhaseRetired,
			tamper: func(t *testing.T, artifacts coreSchemaUpgradeArtifacts) {
				if err := os.Remove(artifacts.targetBackup); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			databasePath, source := newFakeMaintenanceSchemaAuthority(t, 1)
			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(phase string) error {
				if phase == test.crashPhase {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatalf("maintenance did not stop after %s", test.crashPhase)
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists {
				t.Fatal(err)
			}
			artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, artifacts)
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
				t.Fatal("tampered maintenance recovery proof was accepted")
			}
			if pending, err := coreSchemaUpgradePending(databasePath); err != nil || !pending {
				t.Fatalf("failed-closed maintenance manifest pending=%v err=%v", pending, err)
			}
		})
	}
}

func TestCoreSchemaUpgradeLegacyManifestChainsThroughCurrentTarget(t *testing.T) {
	databasePath, source := newFakeSchemaAuthority(t)
	manifest := coreSchemaUpgradeManifest{
		Version:       coreSchemaUpgradeLegacyManifestVersion,
		UpgradeID:     "00112233445566778899aabb",
		Status:        coreSchemaUpgradePreparing,
		CreatedAt:     time.Now().UTC(),
		SourceVersion: source.Version,
		TargetVersion: 2,
		SourceHead:    source.Head,
	}
	if err := writeCoreSchemaUpgradeManifest(databasePath, manifest); err != nil {
		t.Fatal(err)
	}
	minimum := source.Head
	got, err := ensureCoreStoreSchemaCurrentWithOps(
		t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps(),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := source.Head
	want.HeadGeneration += 2
	if got == nil || *got != want {
		t.Fatalf("legacy manifest chained head=%+v want %+v", got, want)
	}
	watermark, err := loadAuthorityWatermark(databasePath + ".head")
	if err != nil || watermark == nil || *watermark != want {
		t.Fatalf("legacy chained watermark=%+v err=%v", watermark, err)
	}
	published := readFakeSchemaFile(t, databasePath)
	if published.Version != contractCachePruneMigrationVersion {
		t.Fatalf("legacy manifest stopped at schema %d", published.Version)
	}
}

func TestCoreSchemaUpgradeLegacyManifestRealPlanChainsBeforeAndAfterPublication(t *testing.T) {
	for _, crashPhase := range []string{coreSchemaPhaseCandidate, coreSchemaPhaseRenamed} {
		t.Run(crashPhase, func(t *testing.T) {
			fixture := historicalUpgradeFixtureByID(t, "v2.3.0-schema-v1-authority")
			root := privateTestDir(t)
			materializeHistoricalUpgradeFixture(t, fixture, root)
			databasePath := filepath.Join(root, "daemon.db")
			minimum, err := loadAuthorityWatermark(databasePath + ".head")
			if err != nil || minimum == nil {
				t.Fatalf("load historical source watermark=%+v err=%v", minimum, err)
			}
			source, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
				Path: databasePath, MinimumHead: minimum,
			})
			if err != nil {
				t.Fatal(err)
			}
			legacy := coreSchemaUpgradeManifest{
				Version:       coreSchemaUpgradeLegacyManifestVersion,
				UpgradeID:     "1234567890abcdef12345678",
				Status:        coreSchemaUpgradePreparing,
				CreatedAt:     time.Now().UTC(),
				SourceVersion: source.SchemaVersion,
				TargetVersion: 2,
				SourceHead:    source.Head,
			}
			if err := writeCoreSchemaUpgradeManifest(databasePath, legacy); err != nil {
				t.Fatal(err)
			}

			ops := productionCoreSchemaUpgradeOps()
			ops.after = func(phase string) error {
				if phase == crashPhase {
					return errors.New("injected legacy-manifest crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(
				t.Context(), databasePath, minimum, time.Now(), ops,
			); err == nil {
				t.Fatalf("legacy real-plan upgrade did not stop after %s", crashPhase)
			}
			pending, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists || pending.TargetVersion != 2 {
				t.Fatalf("legacy manifest after %s: exists=%v manifest=%+v err=%v", crashPhase, exists, pending, err)
			}
			resumeMinimum, err := loadAuthorityWatermark(databasePath + ".head")
			if err != nil || resumeMinimum == nil {
				t.Fatalf("load legacy resume watermark=%+v err=%v", resumeMinimum, err)
			}
			if crashPhase == coreSchemaPhaseRenamed {
				mid, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
					Path: databasePath, MinimumHead: resumeMinimum,
				})
				if err != nil {
					t.Fatal(err)
				}
				if mid.SchemaVersion != 2 || mid.Status != corestore.InspectionUpgradeRequired {
					t.Fatalf("published legacy target was not a real schema-2 prefix: %+v", mid)
				}
			}

			finalHead, err := ensureCoreStoreSchemaCurrentWithOps(
				t.Context(), databasePath, resumeMinimum, time.Now(), productionCoreSchemaUpgradeOps(),
			)
			if err != nil {
				t.Fatalf("resume legacy real-plan upgrade after %s: %v", crashPhase, err)
			}
			wantHead := source.Head
			wantHead.HeadGeneration += 2
			if finalHead == nil || *finalHead != wantHead {
				t.Fatalf("legacy chained head=%+v want %+v", finalHead, wantHead)
			}
			current, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
				Path: databasePath, MinimumHead: finalHead,
			})
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != corestore.InspectionCurrent ||
				current.SchemaVersion != contractCachePruneMigrationVersion {
				t.Fatalf("legacy chain did not reach current v4: %+v", current)
			}
			if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
				t.Fatalf("legacy chain manifest pending=%v err=%v", pending, err)
			}
		})
	}
}

func TestCoreSchemaUpgradePreparingIntentResetsUnboundRealPlanArtifacts(t *testing.T) {
	fixture := historicalUpgradeFixtureByID(t, "v2.5.4-schema-v3-authority")
	root := privateTestDir(t)
	materializeHistoricalUpgradeFixture(t, fixture, root)
	databasePath := filepath.Join(root, "daemon.db")
	minimum, err := loadAuthorityWatermark(databasePath + ".head")
	if err != nil || minimum == nil {
		t.Fatalf("load historical v3 watermark=%+v err=%v", minimum, err)
	}
	source, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: databasePath, MinimumHead: minimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := coreSchemaUpgradeManifest{
		Version:        coreSchemaUpgradeManifestVersion,
		UpgradeID:      "abcdef1234567890abcdef12",
		Status:         coreSchemaUpgradePreparing,
		CreatedAt:      time.Now().UTC(),
		SourceVersion:  source.SchemaVersion,
		TargetVersion:  source.TargetVersion,
		SourceHead:     source.Head,
		HeadTransition: source.HeadTransition,
	}
	if err := writeCoreSchemaUpgradeManifest(databasePath, manifest); err != nil {
		t.Fatal(err)
	}
	artifacts, err := coreSchemaUpgradeArtifactPaths(databasePath, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corestore.PrepareUpgrade(t.Context(), corestore.UpgradeOptions{
		SourcePath:       databasePath,
		BackupPath:       artifacts.backup,
		CandidatePath:    artifacts.candidate,
		TargetVersion:    manifest.TargetVersion,
		MinimumHead:      &source.Head,
		ReplaceCandidate: true,
	}); err != nil {
		t.Fatalf("seed unbound prepared artifacts: %v", err)
	}
	unboundCandidate, err := os.Stat(artifacts.candidate)
	if err != nil {
		t.Fatal(err)
	}

	ops := productionCoreSchemaUpgradeOps()
	productionPrepare := ops.prepare
	var sawReset bool
	ops.prepare = func(ctx context.Context, options corestore.UpgradeOptions) (corestore.UpgradeResult, error) {
		sawReset = options.ResetUnboundArtifacts
		return productionPrepare(ctx, options)
	}
	finalHead, err := ensureCoreStoreSchemaCurrentWithOps(
		t.Context(), databasePath, minimum, time.Now(), ops,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !sawReset {
		t.Fatal("preparing resume did not request safe reset of unbound artifacts")
	}
	if finalHead == nil || *finalHead != source.Head {
		t.Fatalf("preparing resume changed maintenance head: got=%+v want=%+v", finalHead, source.Head)
	}
	published, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(unboundCandidate, published) {
		t.Fatal("unbound preparing candidate was adopted instead of safely rebuilt")
	}
}

func newFakeMaintenanceSchemaAuthority(t *testing.T, rows int64) (string, fakeSchemaFile) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "daemon.db")
	source := fakeSchemaFile{
		Version:       contractCachePruneMigrationVersion - 1,
		TargetVersion: contractCachePruneMigrationVersion,
		Head: corestore.AuthorityHead{
			AuthorityEpoch:   "ffeeddccbbaa99887766554433221100",
			HeadGeneration:   12,
			LastEventSeq:     91,
			SignerGeneration: 4,
		},
		Evidence:        "preserved-state-and-near-match-evidence",
		MaintenanceRows: rows,
	}
	writeFakeSchemaFile(t, path, source)
	if err := writeAuthorityWatermark(path+".head", source.Head); err != nil {
		t.Fatal(err)
	}
	return path, source
}
