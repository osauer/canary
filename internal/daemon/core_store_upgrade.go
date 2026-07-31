package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	coreSchemaUpgradeManifestVersion       = 2
	coreSchemaUpgradeLegacyManifestVersion = 1
	coreSchemaUpgradePreparing             = "preparing"
	coreSchemaUpgradeReady                 = "candidate_ready"

	coreSchemaPhaseIntent     = "intent_durable"
	coreSchemaPhaseCandidate  = "candidate_ready"
	coreSchemaPhaseWatermark  = "watermark_armed"
	coreSchemaPhaseQuiesced   = "source_quiesced"
	coreSchemaPhaseRenamed    = "candidate_renamed"
	coreSchemaPhaseSynced     = "publication_synced"
	coreSchemaPhaseVerified   = "publication_verified"
	coreSchemaPhaseTarget     = "target_backup_ready"
	coreSchemaPhaseReceipt    = "maintenance_receipt_durable"
	coreSchemaPhaseRetired    = "source_backup_retired"
	coreSchemaPhaseRetireSync = "source_backup_retirement_synced"
)

// coreSchemaUpgradeManifest is transient crash-recovery coordination. It
// contains no business state and is removed only after the upgraded authority
// has been reopened and fully validated. Artifact paths are derived from the
// validated ID and versions rather than trusted from JSON.
type coreSchemaUpgradeManifest struct {
	Version            int                             `json:"version"`
	UpgradeID          string                          `json:"upgrade_id"`
	Status             string                          `json:"status"`
	CreatedAt          time.Time                       `json:"created_at"`
	SourceVersion      int                             `json:"source_version"`
	TargetVersion      int                             `json:"target_version"`
	SourceHead         corestore.AuthorityHead         `json:"source_head"`
	CandidateHead      *corestore.AuthorityHead        `json:"candidate_head,omitempty"`
	HeadTransition     corestore.UpgradeHeadTransition `json:"head_transition,omitempty"`
	BackupSHA256       string                          `json:"backup_sha256,omitempty"`
	BackupBytes        int64                           `json:"backup_bytes,omitempty"`
	CandidateSHA256    string                          `json:"candidate_sha256,omitempty"`
	CandidateBytes     int64                           `json:"candidate_bytes,omitempty"`
	TargetBackupSHA256 string                          `json:"target_backup_sha256,omitempty"`
	TargetBackupBytes  int64                           `json:"target_backup_bytes,omitempty"`
	Maintenance        *coreSchemaUpgradeMaintenance   `json:"maintenance,omitempty"`
}

type coreSchemaUpgradeArtifacts struct {
	source       string
	backup       string
	candidate    string
	targetBackup string
	receipt      string
}

type coreSchemaUpgradeOps struct {
	inspect        func(context.Context, corestore.InspectOptions) (corestore.Inspection, error)
	prepare        func(context.Context, corestore.UpgradeOptions) (corestore.UpgradeResult, error)
	recompute      func(context.Context, corestore.RecomputeUpgradeMaintenanceOptions) (corestore.UpgradeMaintenanceResult, error)
	targetBackup   func(context.Context, corestore.UpgradeTargetBackupOptions) (corestore.BackupInfo, error)
	quiesce        func(context.Context, corestore.QuiesceOptions) (corestore.Inspection, error)
	availableBytes func(string) (uint64, error)
	after          func(string) error
}

func productionCoreSchemaUpgradeOps() coreSchemaUpgradeOps {
	return coreSchemaUpgradeOps{
		inspect:      corestore.Inspect,
		prepare:      corestore.PrepareUpgrade,
		recompute:    corestore.RecomputeUpgradeMaintenance,
		targetBackup: corestore.PrepareUpgradeTargetBackup,
		quiesce:      corestore.QuiesceForReplacement,
	}
}

func (o coreSchemaUpgradeOps) validate() error {
	if o.inspect == nil || o.prepare == nil || o.recompute == nil || o.targetBackup == nil || o.quiesce == nil {
		return fmt.Errorf("schema upgrade operations are incomplete")
	}
	return nil
}

func (o coreSchemaUpgradeOps) reached(phase string) error {
	if o.after == nil {
		return nil
	}
	if err := o.after(phase); err != nil {
		return fmt.Errorf("schema upgrade stopped after %s: %w", phase, err)
	}
	return nil
}

func coreSchemaUpgradeManifestPath(databasePath string) string {
	return databasePath + ".upgrade.json"
}

func coreSchemaUpgradePending(databasePath string) (bool, error) {
	path := coreSchemaUpgradeManifestPath(databasePath)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect schema upgrade manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("schema upgrade manifest must be a private regular file")
	}
	return true, nil
}

func ensureCoreStoreSchemaCurrent(ctx context.Context, databasePath string, minimum *corestore.AuthorityHead, now time.Time) (*corestore.AuthorityHead, error) {
	return ensureCoreStoreSchemaCurrentWithOps(ctx, databasePath, minimum, now, productionCoreSchemaUpgradeOps())
}

func ensureCoreStoreSchemaCurrentWithOps(
	ctx context.Context,
	databasePath string,
	minimum *corestore.AuthorityHead,
	now time.Time,
	ops coreSchemaUpgradeOps,
) (*corestore.AuthorityHead, error) {
	if err := ops.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("schema upgrade database path is empty")
	}
	path, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve schema upgrade database path: %w", err)
	}
	if minimum == nil {
		return nil, fmt.Errorf("schema upgrade requires the existing anti-rollback watermark")
	}

	manifest, exists, err := loadCoreSchemaUpgradeManifest(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		inspection, err := ops.inspect(ctx, corestore.InspectOptions{Path: path, MinimumHead: minimum})
		if err != nil {
			return nil, fmt.Errorf("inspect daemon schema before upgrade: %w", err)
		}
		if inspection.Status == corestore.InspectionCurrent {
			head := inspection.Head
			return &head, nil
		}
		if inspection.Status != corestore.InspectionUpgradeRequired || inspection.SchemaVersion >= inspection.TargetVersion {
			return nil, fmt.Errorf("daemon schema inspection returned invalid upgrade state")
		}
		expectedTransition, err := corestore.ExpectedUpgradeHeadTransition(inspection.SchemaVersion, inspection.TargetVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve daemon schema upgrade head transition: %w", err)
		}
		if inspection.HeadTransition != expectedTransition {
			return nil, fmt.Errorf("daemon schema inspection returned inconsistent authority-head transition")
		}
		if err := ensureCoreSchemaUpgradeSpace(path, "candidate preparation", ops.availableBytes); err != nil {
			return nil, err
		}
		// Close the ordinary commit-observer crash window before binding the
		// upgrade intent. From this point the manifest requires exact equality.
		if inspection.Head != *minimum {
			if err := writeAuthorityWatermark(path+".head", inspection.Head); err != nil {
				return nil, fmt.Errorf("synchronize pre-upgrade authority watermark: %w", err)
			}
			minimum = authorityHeadPointer(inspection.Head)
		}
		id, err := newCoreSchemaUpgradeID()
		if err != nil {
			return nil, err
		}
		manifest = coreSchemaUpgradeManifest{
			Version:        coreSchemaUpgradeManifestVersion,
			UpgradeID:      id,
			Status:         coreSchemaUpgradePreparing,
			CreatedAt:      now.UTC(),
			SourceVersion:  inspection.SchemaVersion,
			TargetVersion:  inspection.TargetVersion,
			SourceHead:     inspection.Head,
			HeadTransition: expectedTransition,
		}
		if err := writeCoreSchemaUpgradeManifest(path, manifest); err != nil {
			return nil, err
		}
		if err := ops.reached(coreSchemaPhaseIntent); err != nil {
			return nil, err
		}
	}

	artifacts, err := coreSchemaUpgradeArtifactPaths(path, manifest)
	if err != nil {
		return nil, err
	}
	live, err := ops.inspect(ctx, corestore.InspectOptions{Path: path})
	if err != nil {
		return nil, fmt.Errorf("inspect live authority while resuming schema upgrade: %w", err)
	}
	currentTargetVersion := live.TargetVersion

	if manifest.CandidateHead != nil && live.SchemaVersion == manifest.TargetVersion && live.Head == *manifest.CandidateHead {
		published, err := inspectCoreSchemaUpgradeAtManifestTarget(ctx, path, manifest, ops)
		if err != nil {
			return nil, err
		}
		return finalizePublishedCoreSchemaUpgrade(
			ctx, path, minimum, manifest, artifacts, published,
			currentTargetVersion > manifest.TargetVersion, now, ops,
		)
	}
	if live.SchemaVersion != manifest.SourceVersion || live.Head != manifest.SourceHead {
		return nil, fmt.Errorf("schema upgrade live authority does not match recorded source or candidate")
	}
	if live.TargetVersion < manifest.TargetVersion || live.Status != corestore.InspectionUpgradeRequired {
		return nil, fmt.Errorf("schema upgrade source is not upgradeable to its recorded target")
	}

	if manifest.Status == coreSchemaUpgradePreparing {
		if *minimum != manifest.SourceHead {
			return nil, fmt.Errorf("preparing schema upgrade watermark does not match source head")
		}
		// The full two-footprint preflight is intentionally only before durable
		// intent. A resume may already own validated candidate/backup bytes that
		// consume that allowance; PrepareUpgrade removes all unbound artifacts,
		// rebuilds from the exact source, and reports any genuinely new ENOSPC
		// without demanding the space twice.
		result, err := ops.prepare(ctx, corestore.UpgradeOptions{
			SourcePath:            path,
			BackupPath:            artifacts.backup,
			CandidatePath:         artifacts.candidate,
			TargetVersion:         manifest.TargetVersion,
			MinimumHead:           authorityHeadPointer(manifest.SourceHead),
			ReplaceCandidate:      true,
			ResetUnboundArtifacts: true,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"prepare daemon schema upgrade: %w",
				coreSchemaUpgradeNoSpaceError(path, "candidate preparation", ops.availableBytes, err),
			)
		}
		if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err != nil {
			return nil, err
		}
		maintenance, err := coreSchemaUpgradeMaintenanceFromResult(result)
		if err != nil {
			return nil, err
		}
		backupDigest, backupBytes, err := hashPrivateUpgradeArtifact(artifacts.backup)
		if err != nil {
			return nil, fmt.Errorf("fingerprint schema upgrade backup: %w", err)
		}
		candidateDigest, candidateBytes, err := hashPrivateUpgradeArtifact(artifacts.candidate)
		if err != nil {
			return nil, fmt.Errorf("fingerprint schema upgrade candidate: %w", err)
		}
		candidateHead := result.Candidate.Head
		manifest.Status = coreSchemaUpgradeReady
		manifest.CandidateHead = &candidateHead
		if manifest.Version >= coreSchemaUpgradeManifestVersion {
			manifest.HeadTransition = result.HeadTransition
		}
		manifest.BackupSHA256 = backupDigest
		manifest.BackupBytes = backupBytes
		manifest.CandidateSHA256 = candidateDigest
		manifest.CandidateBytes = candidateBytes
		manifest.Maintenance = maintenance
		if err := writeCoreSchemaUpgradeManifest(path, manifest); err != nil {
			return nil, err
		}
		if err := ops.reached(coreSchemaPhaseCandidate); err != nil {
			return nil, err
		}
	}
	if manifest.Status != coreSchemaUpgradeReady || manifest.CandidateHead == nil {
		return nil, fmt.Errorf("schema upgrade manifest is not publishable")
	}

	if err := verifyCoreSchemaUpgradeArtifacts(ctx, manifest, artifacts, ops); err != nil {
		// Before the watermark is armed, a missing unpublished candidate can
		// be rebuilt from the immutable verified backup. Changed artifacts are
		// never accepted or overwritten here.
		if coreSchemaUpgradeHeadTransition(manifest) == corestore.UpgradeHeadTransitionPreserve {
			return nil, err
		}
		candidateMissing, missingErr := upgradeArtifactMissing(artifacts.candidate)
		if missingErr != nil {
			return nil, missingErr
		}
		if *minimum != manifest.SourceHead || !candidateMissing {
			return nil, err
		}
		result, prepareErr := ops.prepare(ctx, corestore.UpgradeOptions{
			SourcePath:       path,
			BackupPath:       artifacts.backup,
			CandidatePath:    artifacts.candidate,
			TargetVersion:    manifest.TargetVersion,
			MinimumHead:      authorityHeadPointer(manifest.SourceHead),
			ReplaceCandidate: true,
		})
		if prepareErr != nil {
			return nil, fmt.Errorf(
				"rebuild unpublished schema upgrade candidate: %w",
				coreSchemaUpgradeNoSpaceError(path, "candidate rebuild", ops.availableBytes, prepareErr),
			)
		}
		if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err != nil {
			return nil, err
		}
		maintenance, maintenanceErr := coreSchemaUpgradeMaintenanceFromResult(result)
		if maintenanceErr != nil {
			return nil, maintenanceErr
		}
		if !equalCoreSchemaUpgradeMaintenance(manifest.Maintenance, maintenance) || result.HeadTransition != coreSchemaUpgradeHeadTransition(manifest) {
			return nil, fmt.Errorf("rebuilt schema upgrade changed immutable maintenance metadata")
		}
		candidateDigest, candidateBytes, hashErr := hashPrivateUpgradeArtifact(artifacts.candidate)
		if hashErr != nil {
			return nil, hashErr
		}
		manifest.CandidateSHA256 = candidateDigest
		manifest.CandidateBytes = candidateBytes
		if err := writeCoreSchemaUpgradeManifest(path, manifest); err != nil {
			return nil, err
		}
		if err := verifyCoreSchemaUpgradeArtifacts(ctx, manifest, artifacts, ops); err != nil {
			return nil, err
		}
	}

	candidateHead := *manifest.CandidateHead
	if coreSchemaUpgradeHeadTransition(manifest) == corestore.UpgradeHeadTransitionPreserve {
		if candidateHead != manifest.SourceHead || *minimum != manifest.SourceHead {
			return nil, fmt.Errorf("head-preserving schema upgrade changed authority or watermark identity")
		}
		// No watermark bytes are written for maintenance-only publication.
		// This phase is still the durable publication-authorization boundary
		// used by crash tests and recovery sequencing.
		if err := ops.reached(coreSchemaPhaseWatermark); err != nil {
			return nil, err
		}
	} else {
		switch *minimum {
		case manifest.SourceHead:
			if err := writeAuthorityWatermark(path+".head", candidateHead); err != nil {
				return nil, fmt.Errorf("arm upgraded authority watermark: %w", err)
			}
			minimum = authorityHeadPointer(candidateHead)
			if err := ops.reached(coreSchemaPhaseWatermark); err != nil {
				return nil, err
			}
		case candidateHead:
			// Crash recovery after the watermark was armed.
		default:
			return nil, fmt.Errorf("schema upgrade watermark matches neither source nor candidate head")
		}
	}

	if err := verifyCoreSchemaUpgradeArtifacts(ctx, manifest, artifacts, ops); err != nil {
		return nil, fmt.Errorf("reverify armed schema upgrade artifacts: %w", err)
	}
	quiesced, err := ops.quiesce(ctx, corestore.QuiesceOptions{
		Path: path, ExpectedSchemaVersion: manifest.SourceVersion, ExpectedHead: manifest.SourceHead,
	})
	if err != nil {
		return nil, fmt.Errorf("quiesce source authority for schema publication: %w", err)
	}
	if quiesced.SchemaVersion != manifest.SourceVersion || quiesced.Head != manifest.SourceHead {
		return nil, fmt.Errorf("quiesced schema upgrade source identity changed")
	}
	if err := ops.reached(coreSchemaPhaseQuiesced); err != nil {
		return nil, err
	}
	if err := verifyCoreSchemaUpgradeArtifacts(ctx, manifest, artifacts, ops); err != nil {
		return nil, fmt.Errorf("reverify schema candidate after source checkpoint: %w", err)
	}
	if err := os.Rename(artifacts.candidate, path); err != nil {
		return nil, fmt.Errorf("publish upgraded daemon authority: %w", err)
	}
	if err := ops.reached(coreSchemaPhaseRenamed); err != nil {
		return nil, err
	}
	if err := syncPrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("sync upgraded authority publication: %w", err)
	}
	if err := ops.reached(coreSchemaPhaseSynced); err != nil {
		return nil, err
	}
	published, err := inspectCoreSchemaUpgradeAtManifestTarget(ctx, path, manifest, ops)
	if err != nil {
		return nil, fmt.Errorf("verify published schema upgrade: %w", err)
	}
	if published.Status != corestore.InspectionCurrent || published.SchemaVersion != manifest.TargetVersion || published.Head != candidateHead {
		return nil, fmt.Errorf("published schema upgrade identity is invalid")
	}
	return finalizePublishedCoreSchemaUpgrade(
		ctx, path, minimum, manifest, artifacts, published,
		currentTargetVersion > manifest.TargetVersion, now, ops,
	)
}

func finalizePublishedCoreSchemaUpgrade(
	ctx context.Context,
	databasePath string,
	minimum *corestore.AuthorityHead,
	manifest coreSchemaUpgradeManifest,
	artifacts coreSchemaUpgradeArtifacts,
	live corestore.Inspection,
	continueToCurrent bool,
	now time.Time,
	ops coreSchemaUpgradeOps,
) (*corestore.AuthorityHead, error) {
	if manifest.Status != coreSchemaUpgradeReady || manifest.CandidateHead == nil || *minimum != *manifest.CandidateHead {
		return nil, fmt.Errorf("published schema upgrade is not bound to its ready manifest and watermark")
	}
	if live.Status != corestore.InspectionCurrent || live.TargetVersion != manifest.TargetVersion {
		return nil, fmt.Errorf("published schema upgrade is not current")
	}
	liveDigest, liveBytes, err := hashPrivateUpgradeArtifact(databasePath)
	if err != nil {
		return nil, fmt.Errorf("verify published schema upgrade bytes: %w", err)
	}
	if liveDigest != manifest.CandidateSHA256 || liveBytes != manifest.CandidateBytes {
		return nil, fmt.Errorf("published schema upgrade fingerprint changed")
	}
	if err := syncPrivateDirectory(filepath.Dir(databasePath)); err != nil {
		return nil, err
	}
	if err := ops.reached(coreSchemaPhaseVerified); err != nil {
		return nil, err
	}
	if manifest.Maintenance == nil {
		if err := verifyCoreSchemaUpgradeSourceBackup(ctx, manifest, artifacts, ops); err != nil {
			return nil, err
		}
	} else if err := finalizeCoreSchemaUpgradeMaintenance(ctx, databasePath, &manifest, artifacts, live, ops); err != nil {
		return nil, err
	}
	if err := removeCoreSchemaUpgradeManifest(databasePath); err != nil {
		return nil, err
	}
	head := *manifest.CandidateHead
	if continueToCurrent {
		return ensureCoreStoreSchemaCurrentWithOps(ctx, databasePath, &head, now, ops)
	}
	return &head, nil
}

func inspectCoreSchemaUpgradeAtManifestTarget(
	ctx context.Context,
	databasePath string,
	manifest coreSchemaUpgradeManifest,
	ops coreSchemaUpgradeOps,
) (corestore.Inspection, error) {
	if manifest.CandidateHead == nil {
		return corestore.Inspection{}, fmt.Errorf("schema upgrade candidate head is missing")
	}
	return ops.inspect(ctx, corestore.InspectOptions{
		Path:          databasePath,
		MinimumHead:   manifest.CandidateHead,
		TargetVersion: manifest.TargetVersion,
	})
}

func verifyCoreSchemaUpgradeArtifacts(ctx context.Context, manifest coreSchemaUpgradeManifest, artifacts coreSchemaUpgradeArtifacts, ops coreSchemaUpgradeOps) error {
	if err := verifyCoreSchemaUpgradeSourceBackup(ctx, manifest, artifacts, ops); err != nil {
		return err
	}
	candidate, err := ops.inspect(ctx, corestore.InspectOptions{
		Path: artifacts.candidate, MinimumHead: manifest.CandidateHead, TargetVersion: manifest.TargetVersion,
	})
	if err != nil {
		return fmt.Errorf("inspect schema upgrade candidate: %w", err)
	}
	if candidate.Status != corestore.InspectionCurrent || candidate.SchemaVersion != manifest.TargetVersion || candidate.Head != *manifest.CandidateHead {
		return fmt.Errorf("schema upgrade candidate identity changed")
	}
	candidateDigest, candidateBytes, err := hashPrivateUpgradeArtifact(artifacts.candidate)
	if err != nil {
		return fmt.Errorf("hash schema upgrade candidate: %w", err)
	}
	if candidateDigest != manifest.CandidateSHA256 || candidateBytes != manifest.CandidateBytes {
		return fmt.Errorf("schema upgrade candidate fingerprint changed")
	}
	return nil
}

func verifyCoreSchemaUpgradeSourceBackup(ctx context.Context, manifest coreSchemaUpgradeManifest, artifacts coreSchemaUpgradeArtifacts, ops coreSchemaUpgradeOps) error {
	backup, err := ops.inspect(ctx, corestore.InspectOptions{
		Path: artifacts.backup, MinimumHead: authorityHeadPointer(manifest.SourceHead), TargetVersion: manifest.TargetVersion,
	})
	if err != nil {
		return fmt.Errorf("inspect schema upgrade backup: %w", err)
	}
	if backup.SchemaVersion != manifest.SourceVersion || backup.Head != manifest.SourceHead {
		return fmt.Errorf("schema upgrade backup identity changed")
	}
	backupDigest, backupBytes, err := hashPrivateUpgradeArtifact(artifacts.backup)
	if err != nil {
		return fmt.Errorf("hash schema upgrade backup: %w", err)
	}
	if backupDigest != manifest.BackupSHA256 || backupBytes != manifest.BackupBytes {
		return fmt.Errorf("schema upgrade backup fingerprint changed")
	}
	return nil
}

func verifyCoreSchemaUpgradeTargetBackup(ctx context.Context, manifest coreSchemaUpgradeManifest, artifacts coreSchemaUpgradeArtifacts, ops coreSchemaUpgradeOps) error {
	target, err := ops.inspect(ctx, corestore.InspectOptions{
		Path: artifacts.targetBackup, MinimumHead: manifest.CandidateHead, TargetVersion: manifest.TargetVersion,
	})
	if err != nil {
		return fmt.Errorf("inspect schema upgrade target backup: %w", err)
	}
	if target.Status != corestore.InspectionCurrent || target.SchemaVersion != manifest.TargetVersion || target.Head != *manifest.CandidateHead {
		return fmt.Errorf("schema upgrade target backup identity changed")
	}
	digest, size, err := hashPrivateUpgradeArtifact(artifacts.targetBackup)
	if err != nil {
		return fmt.Errorf("hash schema upgrade target backup: %w", err)
	}
	if digest != manifest.TargetBackupSHA256 || size != manifest.TargetBackupBytes {
		return fmt.Errorf("schema upgrade target backup fingerprint changed")
	}
	return nil
}

func validatePreparedCoreSchemaUpgrade(manifest coreSchemaUpgradeManifest, artifacts coreSchemaUpgradeArtifacts, result corestore.UpgradeResult) error {
	if result.Source.SchemaVersion != manifest.SourceVersion || result.Source.TargetVersion != manifest.TargetVersion || result.Source.Head != manifest.SourceHead {
		return fmt.Errorf("prepared schema upgrade source identity changed")
	}
	if result.Source.Path != artifacts.source {
		return fmt.Errorf("prepared schema upgrade source path is invalid")
	}
	if result.Backup.SchemaVersion != manifest.SourceVersion || result.Backup.Head != manifest.SourceHead || result.Backup.Path != artifacts.backup {
		return fmt.Errorf("prepared schema upgrade backup identity changed")
	}
	if result.HeadTransition != corestore.UpgradeHeadTransitionAdvanceOnce && result.HeadTransition != corestore.UpgradeHeadTransitionPreserve {
		return fmt.Errorf("prepared schema upgrade has invalid authority-head transition")
	}
	if manifest.Version == coreSchemaUpgradeManifestVersion && result.HeadTransition != manifest.HeadTransition {
		return fmt.Errorf("prepared schema upgrade changed the intent-bound authority-head transition")
	}
	if manifest.Version == coreSchemaUpgradeLegacyManifestVersion {
		if result.HeadTransition != corestore.UpgradeHeadTransitionAdvanceOnce {
			return fmt.Errorf("legacy schema upgrade manifest did not advance authority head once")
		}
	}
	expectedTransition, err := corestore.ExpectedUpgradeHeadTransition(manifest.SourceVersion, manifest.TargetVersion)
	if err != nil {
		return fmt.Errorf("validate prepared schema upgrade head transition: %w", err)
	}
	if result.HeadTransition != expectedTransition {
		return fmt.Errorf("prepared schema upgrade head transition disagrees with the immutable migration plan")
	}
	want := coreSchemaUpgradeExpectedCandidateHead(manifest.SourceHead, result.HeadTransition)
	if result.Candidate.Status != corestore.InspectionCurrent || result.Candidate.SchemaVersion != manifest.TargetVersion || result.Candidate.Head != want || result.Candidate.Path != artifacts.candidate {
		return fmt.Errorf("prepared schema upgrade candidate did not preserve authority continuity")
	}
	maintenance, err := coreSchemaUpgradeMaintenanceFromResult(result)
	if err != nil {
		return err
	}
	if result.HeadTransition == corestore.UpgradeHeadTransitionPreserve {
		if maintenance == nil {
			return fmt.Errorf("prepared head-preserving upgrade lacks exact v4 maintenance authority")
		}
	}
	if maintenance != nil && manifest.TargetVersion != contractCachePruneMigrationVersion {
		return fmt.Errorf("prepared contract-cache maintenance has invalid target version")
	}
	if manifest.Status == coreSchemaUpgradeReady {
		if result.HeadTransition != coreSchemaUpgradeHeadTransition(manifest) || !equalCoreSchemaUpgradeMaintenance(manifest.Maintenance, maintenance) {
			return fmt.Errorf("prepared schema upgrade changed ready manifest semantics")
		}
	}
	if maintenance == nil {
		if result.TargetBackup != nil {
			return fmt.Errorf("ordinary schema upgrade unexpectedly produced a target backup")
		}
		return nil
	}
	if result.TargetBackup != nil {
		return fmt.Errorf("schema preparation created target backup before publication")
	}
	return nil
}

func coreSchemaUpgradeArtifactPaths(databasePath string, manifest coreSchemaUpgradeManifest) (coreSchemaUpgradeArtifacts, error) {
	if err := validateCoreSchemaUpgradeManifest(manifest); err != nil {
		return coreSchemaUpgradeArtifacts{}, err
	}
	parent := filepath.Dir(databasePath)
	base := filepath.Base(databasePath)
	label := fmt.Sprintf("%s-schema-v%d-to-v%d-%s", base, manifest.SourceVersion, manifest.TargetVersion, manifest.UpgradeID)
	return coreSchemaUpgradeArtifacts{
		source:       databasePath,
		backup:       filepath.Join(parent, "backups", label+".db"),
		candidate:    filepath.Join(parent, "."+label+".candidate"),
		targetBackup: filepath.Join(parent, "backups", label+".target.db"),
		receipt:      filepath.Join(parent, "backups", label+".maintenance.json"),
	}, nil
}

func loadCoreSchemaUpgradeManifest(databasePath string) (coreSchemaUpgradeManifest, bool, error) {
	var manifest coreSchemaUpgradeManifest
	path := coreSchemaUpgradeManifestPath(databasePath)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return manifest, false, nil
	}
	if err != nil {
		return manifest, false, fmt.Errorf("inspect schema upgrade manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return manifest, false, fmt.Errorf("schema upgrade manifest must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest, false, fmt.Errorf("read schema upgrade manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, false, fmt.Errorf("decode schema upgrade manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest, false, fmt.Errorf("decode schema upgrade manifest: trailing content")
	}
	if err := validateCoreSchemaUpgradeManifest(manifest); err != nil {
		return manifest, false, err
	}
	return manifest, true, nil
}

func writeCoreSchemaUpgradeManifest(databasePath string, manifest coreSchemaUpgradeManifest) error {
	if err := validateCoreSchemaUpgradeManifest(manifest); err != nil {
		return err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode schema upgrade manifest: %w", err)
	}
	path := coreSchemaUpgradeManifestPath(databasePath)
	if err := writePrivateStateAtomic(path, append(raw, '\n')); err != nil {
		return fmt.Errorf("write schema upgrade manifest: %w", err)
	}
	if err := syncPrivateFile(path); err != nil {
		return fmt.Errorf("sync schema upgrade manifest: %w", err)
	}
	if err := syncPrivateDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync schema upgrade manifest directory: %w", err)
	}
	return nil
}

func removeCoreSchemaUpgradeManifest(databasePath string) error {
	path := coreSchemaUpgradeManifestPath(databasePath)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to remove invalid schema upgrade manifest")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove completed schema upgrade manifest: %w", err)
	}
	return syncPrivateDirectory(filepath.Dir(path))
}

func validateCoreSchemaUpgradeManifest(manifest coreSchemaUpgradeManifest) error {
	if (manifest.Version != coreSchemaUpgradeLegacyManifestVersion && manifest.Version != coreSchemaUpgradeManifestVersion) ||
		!validCoreSchemaUpgradeID(manifest.UpgradeID) ||
		manifest.CreatedAt.IsZero() {
		return fmt.Errorf("schema upgrade manifest identity is invalid")
	}
	if manifest.SourceVersion < 1 || manifest.TargetVersion <= manifest.SourceVersion || !validAuthorityHead(manifest.SourceHead) {
		return fmt.Errorf("schema upgrade manifest source is invalid")
	}
	transition := coreSchemaUpgradeHeadTransition(manifest)
	if transition != corestore.UpgradeHeadTransitionAdvanceOnce && transition != corestore.UpgradeHeadTransitionPreserve {
		return fmt.Errorf("schema upgrade manifest authority-head transition is invalid")
	}
	if manifest.Version == coreSchemaUpgradeLegacyManifestVersion {
		if manifest.HeadTransition != "" || manifest.Maintenance != nil ||
			manifest.TargetBackupSHA256 != "" || manifest.TargetBackupBytes != 0 {
			return fmt.Errorf("legacy schema upgrade manifest contains unsupported maintenance metadata")
		}
		expectedTransition, err := corestore.ExpectedUpgradeHeadTransition(manifest.SourceVersion, manifest.TargetVersion)
		if err != nil {
			return fmt.Errorf("validate legacy schema upgrade manifest head transition: %w", err)
		}
		if expectedTransition != corestore.UpgradeHeadTransitionAdvanceOnce {
			return fmt.Errorf("legacy schema upgrade manifest does not describe an advance-once plan")
		}
	} else if manifest.HeadTransition == "" {
		return fmt.Errorf("schema upgrade manifest does not bind its authority-head transition")
	} else {
		expectedTransition, err := corestore.ExpectedUpgradeHeadTransition(manifest.SourceVersion, manifest.TargetVersion)
		if err != nil {
			return fmt.Errorf("validate schema upgrade manifest head transition: %w", err)
		}
		if transition != expectedTransition {
			return fmt.Errorf("schema upgrade manifest head transition disagrees with the immutable migration plan")
		}
	}
	switch manifest.Status {
	case coreSchemaUpgradePreparing:
		if manifest.CandidateHead != nil ||
			manifest.BackupSHA256 != "" || manifest.BackupBytes != 0 ||
			manifest.CandidateSHA256 != "" || manifest.CandidateBytes != 0 ||
			manifest.TargetBackupSHA256 != "" || manifest.TargetBackupBytes != 0 ||
			manifest.Maintenance != nil {
			return fmt.Errorf("preparing schema upgrade manifest contains ready artifacts")
		}
	case coreSchemaUpgradeReady:
		if manifest.CandidateHead == nil || !validAuthorityHead(*manifest.CandidateHead) || !validSHA256Hex(manifest.BackupSHA256) || manifest.BackupBytes <= 0 || !validSHA256Hex(manifest.CandidateSHA256) || manifest.CandidateBytes <= 0 {
			return fmt.Errorf("ready schema upgrade manifest is incomplete")
		}
		want := coreSchemaUpgradeExpectedCandidateHead(manifest.SourceHead, transition)
		if *manifest.CandidateHead != want {
			return fmt.Errorf("schema upgrade manifest candidate head breaks authority continuity")
		}
		if manifest.Maintenance != nil {
			if err := validateCoreSchemaUpgradeMaintenance(*manifest.Maintenance); err != nil {
				return err
			}
		}
		if transition == corestore.UpgradeHeadTransitionPreserve {
			if manifest.Version != coreSchemaUpgradeManifestVersion ||
				manifest.Maintenance == nil {
				return fmt.Errorf("head-preserving schema upgrade lacks exact v4 maintenance authority")
			}
		}
		if manifest.Maintenance != nil && manifest.TargetVersion != contractCachePruneMigrationVersion {
			return fmt.Errorf("contract-cache maintenance manifest has invalid target version")
		}
		hasTargetFingerprint := manifest.TargetBackupSHA256 != "" || manifest.TargetBackupBytes != 0
		if coreSchemaUpgradeRetiresSourceBackup(manifest) {
			if hasTargetFingerprint &&
				(!validSHA256Hex(manifest.TargetBackupSHA256) || manifest.TargetBackupBytes <= 0) {
				return fmt.Errorf("schema upgrade target-backup fingerprint is incomplete")
			}
		} else if hasTargetFingerprint {
			return fmt.Errorf("schema upgrade without retirement contains target-backup metadata")
		}
	default:
		return fmt.Errorf("schema upgrade manifest status %q is invalid", manifest.Status)
	}
	return nil
}

func hashPrivateUpgradeArtifact(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", 0, fmt.Errorf("upgrade artifact must be a private regular file")
	}
	return hashRegularFile(path, info)
}

func upgradeArtifactMissing(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect upgrade artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("upgrade artifact must be a regular file")
	}
	return false, nil
}

func newCoreSchemaUpgradeID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate schema upgrade id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validCoreSchemaUpgradeID(value string) bool {
	if len(value) != 24 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validAuthorityHead(head corestore.AuthorityHead) bool {
	return strings.TrimSpace(head.AuthorityEpoch) != "" && head.HeadGeneration >= 0 && head.LastEventSeq >= 0 && head.SignerGeneration >= 1
}

func authorityHeadPointer(head corestore.AuthorityHead) *corestore.AuthorityHead {
	copy := head
	return &copy
}
