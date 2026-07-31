package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	coreSchemaMaintenanceReceiptVersion = 1
	contractCachePruneMigrationVersion  = 4
	contractCachePruneMigrationName     = "contract_cache_observation_prune"
)

var contractCachePruneSelector = corestore.ObservationDiscardSelector{
	ScopeKey: "market/contracts",
	Source:   "ibkr.tws.contract_details",
	Kind:     "contract_cache.snapshot.v3",
}

// coreSchemaUpgradeMaintenance is the frozen, typed maintenance result bound
// into the transient manifest before publication. It is deliberately singular:
// this release authorizes one exact derived-cache discard, not a retention API.
type coreSchemaUpgradeMaintenance struct {
	Discard            corestore.ObservationDiscardSummary `json:"discard"`
	Compacted          bool                                `json:"compacted"`
	RetireSourceBackup bool                                `json:"retire_source_backup"`
}

type coreSchemaMaintenanceArtifactReceipt struct {
	SchemaVersion int                     `json:"schema_version"`
	Head          corestore.AuthorityHead `json:"head"`
	SHA256        string                  `json:"sha256"`
	Bytes         int64                   `json:"bytes"`
}

type coreSchemaMaintenanceReceipt struct {
	Version   int                                  `json:"version"`
	UpgradeID string                               `json:"upgrade_id"`
	Discard   corestore.ObservationDiscardSummary  `json:"discard"`
	Source    coreSchemaMaintenanceArtifactReceipt `json:"source"`
	Target    coreSchemaMaintenanceArtifactReceipt `json:"target"`
}

func coreSchemaUpgradeMaintenanceFromResult(result corestore.UpgradeResult) (*coreSchemaUpgradeMaintenance, error) {
	maintenance := result.Maintenance
	if result.TargetBackup != nil {
		return nil, fmt.Errorf("schema preparation created a target backup before publication")
	}
	if len(maintenance.Discards) == 0 {
		if maintenance.Compacted || maintenance.SourceBackupRetirementRequired {
			return nil, fmt.Errorf("schema upgrade maintenance flags have no typed discard summary")
		}
		return nil, nil
	}
	if len(maintenance.Discards) != 1 {
		return nil, fmt.Errorf("schema upgrade maintenance must contain exactly one discard summary")
	}
	bound := &coreSchemaUpgradeMaintenance{
		Discard:            maintenance.Discards[0],
		Compacted:          maintenance.Compacted,
		RetireSourceBackup: maintenance.SourceBackupRetirementRequired,
	}
	if err := validateCoreSchemaUpgradeMaintenance(*bound); err != nil {
		return nil, err
	}
	return bound, nil
}

func validateCoreSchemaUpgradeMaintenance(maintenance coreSchemaUpgradeMaintenance) error {
	discard := maintenance.Discard
	if discard.MigrationVersion != contractCachePruneMigrationVersion ||
		discard.MigrationName != contractCachePruneMigrationName ||
		discard.Selector != contractCachePruneSelector {
		return fmt.Errorf("schema upgrade maintenance is not the authorized contract-cache discard")
	}
	if discard.RemovedRows < 0 || discard.PayloadBytes < 0 || !validSHA256Hex(discard.OrderedDigestSHA256) {
		return fmt.Errorf("schema upgrade maintenance discard summary is invalid")
	}
	if !maintenance.Compacted {
		return fmt.Errorf("maintenance discard did not compact the candidate")
	}
	if discard.RemovedRows == 0 {
		if discard.PayloadBytes != 0 {
			return fmt.Errorf("zero-row maintenance discard reports payload bytes")
		}
		if maintenance.RetireSourceBackup {
			return fmt.Errorf("zero-row maintenance cannot retire the source backup")
		}
		return nil
	}
	if !maintenance.RetireSourceBackup {
		return fmt.Errorf("non-empty maintenance discard did not authorize source-backup retirement")
	}
	return nil
}

func equalCoreSchemaUpgradeMaintenance(left, right *coreSchemaUpgradeMaintenance) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func coreSchemaUpgradeRetiresSourceBackup(manifest coreSchemaUpgradeManifest) bool {
	return manifest.Maintenance != nil && manifest.Maintenance.RetireSourceBackup
}

func coreSchemaUpgradeHeadTransition(manifest coreSchemaUpgradeManifest) corestore.UpgradeHeadTransition {
	if manifest.HeadTransition == "" && manifest.Version == coreSchemaUpgradeLegacyManifestVersion {
		return corestore.UpgradeHeadTransitionAdvanceOnce
	}
	return manifest.HeadTransition
}

func coreSchemaUpgradeExpectedCandidateHead(source corestore.AuthorityHead, transition corestore.UpgradeHeadTransition) corestore.AuthorityHead {
	if transition == corestore.UpgradeHeadTransitionAdvanceOnce {
		source.HeadGeneration++
	}
	return source
}

func verifyCoreSchemaUpgradeMaintenanceProof(
	ctx context.Context,
	manifest coreSchemaUpgradeManifest,
	artifacts coreSchemaUpgradeArtifacts,
	ops coreSchemaUpgradeOps,
) error {
	if manifest.Maintenance == nil {
		return fmt.Errorf("schema upgrade maintenance proof has no manifest claim")
	}
	if err := verifyCoreSchemaUpgradeSourceBackup(ctx, manifest, artifacts, ops); err != nil {
		return err
	}
	recomputed, err := ops.recompute(ctx, corestore.RecomputeUpgradeMaintenanceOptions{
		SourcePath:            artifacts.backup,
		ExpectedSchemaVersion: manifest.SourceVersion,
		TargetVersion:         manifest.TargetVersion,
		ExpectedHead:          manifest.SourceHead,
	})
	if err != nil {
		return fmt.Errorf("recompute schema maintenance from immutable source backup: %w", err)
	}
	if len(recomputed.Discards) != 1 {
		return fmt.Errorf("recomputed schema maintenance did not contain exactly one discard")
	}
	proof := coreSchemaUpgradeMaintenance{
		Discard:            recomputed.Discards[0],
		Compacted:          recomputed.Compacted,
		RetireSourceBackup: recomputed.SourceBackupRetirementRequired,
	}
	if err := validateCoreSchemaUpgradeMaintenance(proof); err != nil {
		return fmt.Errorf("validate recomputed schema maintenance: %w", err)
	}
	if !equalCoreSchemaUpgradeMaintenance(manifest.Maintenance, &proof) {
		return fmt.Errorf("schema maintenance manifest disagrees with immutable source-backup proof")
	}
	return nil
}

func finalizeCoreSchemaUpgradeMaintenance(
	ctx context.Context,
	databasePath string,
	manifest *coreSchemaUpgradeManifest,
	artifacts coreSchemaUpgradeArtifacts,
	live corestore.Inspection,
	ops coreSchemaUpgradeOps,
) error {
	if manifest.Maintenance == nil {
		return fmt.Errorf("schema upgrade maintenance metadata is missing")
	}
	backupMissing, err := upgradeArtifactMissing(artifacts.backup)
	if err != nil {
		return err
	}
	if !manifest.Maintenance.RetireSourceBackup {
		if backupMissing {
			return fmt.Errorf("non-retiring schema maintenance source backup is missing")
		}
		return verifyCoreSchemaUpgradeMaintenanceProof(ctx, *manifest, artifacts, ops)
	}
	if manifest.Maintenance.Discard.RemovedRows <= 0 {
		return fmt.Errorf("source-backup retirement lacks a non-empty discard summary")
	}

	if !backupMissing {
		if err := verifyCoreSchemaUpgradeMaintenanceProof(ctx, *manifest, artifacts, ops); err != nil {
			return err
		}
	}
	if err := ensureCoreSchemaUpgradeTargetBackup(ctx, databasePath, manifest, artifacts, live, ops); err != nil {
		return err
	}
	expected := coreSchemaMaintenanceReceiptFromManifest(*manifest)
	receipt, receiptExists, err := loadCoreSchemaMaintenanceReceipt(artifacts.receipt)
	if err != nil {
		return err
	}
	if receiptExists && receipt != expected {
		return fmt.Errorf("schema maintenance receipt does not match the upgrade manifest")
	}
	if receiptExists {
		if err := syncPrivateFile(artifacts.receipt); err != nil {
			return fmt.Errorf("sync existing schema maintenance receipt: %w", err)
		}
		if err := syncPrivateDirectory(filepath.Dir(artifacts.receipt)); err != nil {
			return fmt.Errorf("sync existing schema maintenance receipt directory: %w", err)
		}
	}

	if backupMissing {
		if !receiptExists {
			return fmt.Errorf("schema upgrade source backup is missing without a valid maintenance receipt")
		}
		if err := syncPrivateDirectory(filepath.Dir(artifacts.backup)); err != nil {
			return fmt.Errorf("sync resumed source-backup retirement: %w", err)
		}
		if err := ops.reached(coreSchemaPhaseRetireSync); err != nil {
			return err
		}
		return nil
	}

	if err := requireIndependentUpgradeArtifacts(databasePath, artifacts.backup, artifacts.targetBackup); err != nil {
		return err
	}
	if !receiptExists {
		if err := writeCoreSchemaMaintenanceReceipt(artifacts.receipt, expected); err != nil {
			return err
		}
		receiptExists = true
		if err := ops.reached(coreSchemaPhaseReceipt); err != nil {
			return err
		}
	}
	if !receiptExists {
		return fmt.Errorf("schema maintenance receipt was not made durable")
	}

	// Reprove the published authority, both recovery artifacts, and the exact
	// immutable receipt immediately before retiring the only large old-head
	// artifact.
	liveDigest, liveBytes, err := hashPrivateUpgradeArtifact(databasePath)
	if err != nil {
		return fmt.Errorf("reverify published authority before backup retirement: %w", err)
	}
	if liveDigest != manifest.CandidateSHA256 || liveBytes != manifest.CandidateBytes {
		return fmt.Errorf("published authority changed before source-backup retirement")
	}
	if err := verifyCoreSchemaUpgradeSourceBackup(ctx, *manifest, artifacts, ops); err != nil {
		return err
	}
	if err := verifyCoreSchemaUpgradeTargetBackup(ctx, *manifest, artifacts, ops); err != nil {
		return err
	}
	if err := os.Remove(artifacts.backup); err != nil {
		return fmt.Errorf("retire schema upgrade source backup: %w", err)
	}
	if err := ops.reached(coreSchemaPhaseRetired); err != nil {
		return err
	}
	if err := syncPrivateDirectory(filepath.Dir(artifacts.backup)); err != nil {
		return fmt.Errorf("sync schema upgrade source-backup retirement: %w", err)
	}
	if err := ops.reached(coreSchemaPhaseRetireSync); err != nil {
		return err
	}
	return nil
}

func ensureCoreSchemaUpgradeTargetBackup(
	ctx context.Context,
	databasePath string,
	manifest *coreSchemaUpgradeManifest,
	artifacts coreSchemaUpgradeArtifacts,
	live corestore.Inspection,
	ops coreSchemaUpgradeOps,
) error {
	if manifest.TargetBackupSHA256 != "" || manifest.TargetBackupBytes != 0 {
		if !validSHA256Hex(manifest.TargetBackupSHA256) || manifest.TargetBackupBytes <= 0 {
			return fmt.Errorf("schema upgrade target-backup fingerprint is incomplete")
		}
		if err := verifyCoreSchemaUpgradeTargetBackup(ctx, *manifest, artifacts, ops); err != nil {
			return err
		}
		return requireIndependentUpgradeArtifacts(databasePath, artifacts.targetBackup)
	}
	backup, err := ops.targetBackup(ctx, corestore.UpgradeTargetBackupOptions{
		SourcePath:            databasePath,
		BackupPath:            artifacts.targetBackup,
		ExpectedSchemaVersion: manifest.TargetVersion,
		ExpectedHead:          *manifest.CandidateHead,
	})
	if err != nil {
		return fmt.Errorf("create compact target-head backup; published authority remains valid and source backup is retained: %w", err)
	}
	if backup.Path != artifacts.targetBackup ||
		backup.SchemaVersion != manifest.TargetVersion ||
		backup.Head != *manifest.CandidateHead {
		return fmt.Errorf("created target-head backup has invalid identity")
	}
	if live.SchemaVersion != backup.SchemaVersion || live.Head != backup.Head {
		return fmt.Errorf("target-head backup does not match published authority")
	}
	if err := requireIndependentUpgradeArtifacts(databasePath, artifacts.targetBackup); err != nil {
		return err
	}
	digest, size, err := hashPrivateUpgradeArtifact(artifacts.targetBackup)
	if err != nil {
		return fmt.Errorf("fingerprint compact target-head backup: %w", err)
	}
	manifest.TargetBackupSHA256 = digest
	manifest.TargetBackupBytes = size
	if err := writeCoreSchemaUpgradeManifest(databasePath, *manifest); err != nil {
		return err
	}
	if err := ops.reached(coreSchemaPhaseTarget); err != nil {
		return err
	}
	return verifyCoreSchemaUpgradeTargetBackup(ctx, *manifest, artifacts, ops)
}

func coreSchemaMaintenanceReceiptFromManifest(manifest coreSchemaUpgradeManifest) coreSchemaMaintenanceReceipt {
	return coreSchemaMaintenanceReceipt{
		Version:   coreSchemaMaintenanceReceiptVersion,
		UpgradeID: manifest.UpgradeID,
		Discard:   manifest.Maintenance.Discard,
		Source: coreSchemaMaintenanceArtifactReceipt{
			SchemaVersion: manifest.SourceVersion,
			Head:          manifest.SourceHead,
			SHA256:        manifest.BackupSHA256,
			Bytes:         manifest.BackupBytes,
		},
		Target: coreSchemaMaintenanceArtifactReceipt{
			SchemaVersion: manifest.TargetVersion,
			Head:          *manifest.CandidateHead,
			SHA256:        manifest.TargetBackupSHA256,
			Bytes:         manifest.TargetBackupBytes,
		},
	}
}

func validateCoreSchemaMaintenanceReceipt(receipt coreSchemaMaintenanceReceipt) error {
	if receipt.Version != coreSchemaMaintenanceReceiptVersion ||
		!validCoreSchemaUpgradeID(receipt.UpgradeID) ||
		!validAuthorityHead(receipt.Source.Head) ||
		!validAuthorityHead(receipt.Target.Head) ||
		receipt.Source.SchemaVersion < 1 ||
		receipt.Target.SchemaVersion <= receipt.Source.SchemaVersion ||
		!validSHA256Hex(receipt.Source.SHA256) ||
		!validSHA256Hex(receipt.Target.SHA256) ||
		receipt.Source.Bytes <= 0 ||
		receipt.Target.Bytes <= 0 {
		return fmt.Errorf("schema maintenance receipt identity is invalid")
	}
	return validateCoreSchemaUpgradeMaintenance(coreSchemaUpgradeMaintenance{
		Discard:            receipt.Discard,
		Compacted:          true,
		RetireSourceBackup: true,
	})
}

func loadCoreSchemaMaintenanceReceipt(path string) (coreSchemaMaintenanceReceipt, bool, error) {
	var receipt coreSchemaMaintenanceReceipt
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, fmt.Errorf("inspect schema maintenance receipt: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return receipt, false, fmt.Errorf("schema maintenance receipt must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return receipt, false, fmt.Errorf("read schema maintenance receipt: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, false, fmt.Errorf("decode schema maintenance receipt: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, false, fmt.Errorf("decode schema maintenance receipt: trailing content")
	}
	if err := validateCoreSchemaMaintenanceReceipt(receipt); err != nil {
		return receipt, false, err
	}
	return receipt, true, nil
}

func writeCoreSchemaMaintenanceReceipt(path string, receipt coreSchemaMaintenanceReceipt) error {
	if err := validateCoreSchemaMaintenanceReceipt(receipt); err != nil {
		return err
	}
	if existing, ok, err := loadCoreSchemaMaintenanceReceipt(path); err != nil {
		return err
	} else if ok {
		if existing != receipt {
			return fmt.Errorf("refuse to replace a different schema maintenance receipt")
		}
		return nil
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode schema maintenance receipt: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create schema maintenance receipt temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(temp, bytes.NewReader(append(raw, '\n'))); err != nil {
		return fmt.Errorf("write schema maintenance receipt: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync schema maintenance receipt temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close schema maintenance receipt temporary file: %w", err)
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("schema maintenance receipt destination already exists")
		}
		return fmt.Errorf("publish schema maintenance receipt without clobber: %w", err)
	}
	if err := syncPrivateDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync schema maintenance receipt publication: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove schema maintenance receipt temporary link: %w", err)
	}
	if err := syncPrivateDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync schema maintenance receipt directory: %w", err)
	}
	loaded, ok, err := loadCoreSchemaMaintenanceReceipt(path)
	if err != nil || !ok || loaded != receipt {
		return fmt.Errorf("verify durable schema maintenance receipt: %w", err)
	}
	return nil
}

func requireIndependentUpgradeArtifacts(paths ...string) error {
	infos := make([]fs.FileInfo, len(paths))
	for index, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect upgrade artifact independence: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("upgrade artifact must be a regular file for independence verification")
		}
		infos[index] = info
	}
	for left := range infos {
		for right := left + 1; right < len(infos); right++ {
			if os.SameFile(infos[left], infos[right]) {
				return fmt.Errorf("schema upgrade recovery artifacts must have independent inodes")
			}
		}
	}
	return nil
}
