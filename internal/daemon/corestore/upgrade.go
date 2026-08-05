package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type upgradePlanEffect struct {
	headTransition     UpgradeHeadTransition
	compactCandidate   bool
	retireSourceBackup bool
}

type pendingMigrationExecution struct {
	before         AuthorityHead
	after          AuthorityHead
	headTransition UpgradeHeadTransition
	maintenance    UpgradeMaintenanceResult
}

type upgradeCandidateBuild struct {
	inspection     Inspection
	headTransition UpgradeHeadTransition
	maintenance    UpgradeMaintenanceResult
}

// Inspect validates an existing authority without migrating, repairing, or
// opening it for writes. A supported older version is reported as
// InspectionUpgradeRequired rather than treated as corruption.
func Inspect(ctx context.Context, opts InspectOptions) (Inspection, error) {
	plan, err := migrationPlanForTarget(opts.TargetVersion)
	if err != nil {
		return Inspection{}, err
	}
	return inspectWithPlan(ctx, opts, plan)
}

func migrationPlanForTarget(targetVersion int) ([]migration, error) {
	plan := currentMigrationPlan()
	if targetVersion == 0 {
		return plan, nil
	}
	if targetVersion < 1 || targetVersion > len(plan) {
		return nil, fmt.Errorf("corestore: unsupported target schema version %d", targetVersion)
	}
	return plan[:targetVersion], nil
}

func readSchemaVersionOnly(ctx context.Context, path string, busy time.Duration) (int, error) {
	before, err := snapshotReadOnlySidecars(path)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, busy, true))
	if err != nil {
		return 0, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return 0, errors.Join(err, cleanupNewReadOnlySidecars(path, before))
	}
	var version int
	queryErr := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	closeErr := db.Close()
	cleanupErr := cleanupNewReadOnlySidecars(path, before)
	if queryErr != nil {
		return 0, queryErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if cleanupErr != nil {
		return 0, cleanupErr
	}
	return version, nil
}

func inspectWithPlan(ctx context.Context, opts InspectOptions, plan []migration) (Inspection, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return Inspection{}, err
	}
	path, _, err := existingRegularPath(opts.Path, "authority")
	if err != nil {
		return Inspection{}, err
	}
	before, err := snapshotReadOnlySidecars(path)
	if err != nil {
		return Inspection{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		return Inspection{}, fmt.Errorf("open authority for inspection: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("open authority for inspection: %w", errors.Join(err, cleanupNewReadOnlySidecars(path, before)))
	}
	inspection, inspectErr := inspectDBWithPlan(ctx, db, path, opts.MinimumHead, plan)
	closeErr := db.Close()
	cleanupErr := cleanupNewReadOnlySidecars(path, before)
	if inspectErr != nil {
		return Inspection{}, inspectErr
	}
	if closeErr != nil {
		return Inspection{}, fmt.Errorf("close inspected authority: %w", closeErr)
	}
	if cleanupErr != nil {
		return Inspection{}, cleanupErr
	}
	return inspection, nil
}

type readOnlySidecarSnapshot map[string]bool

func snapshotReadOnlySidecars(path string) (readOnlySidecarSnapshot, error) {
	snapshot := make(readOnlySidecarSnapshot, 3)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect authority%s: %w", suffix, err)
		}
		if err := validatePrivateRegularInfo(info, "authority"+suffix); err != nil {
			return nil, err
		}
		snapshot[suffix] = true
	}
	return snapshot, nil
}

func cleanupNewReadOnlySidecars(path string, before readOnlySidecarSnapshot) error {
	removed := false
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if before[suffix] {
			continue
		}
		candidate := path + suffix
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect read-only authority%s: %w", suffix, err)
		}
		if err := validatePrivateRegularInfo(info, "read-only authority"+suffix); err != nil {
			return err
		}
		if suffix != "-shm" && info.Size() != 0 {
			return fmt.Errorf("corestore: read-only inspection created non-empty %s sidecar", suffix)
		}
		if err := os.Remove(candidate); err != nil {
			return fmt.Errorf("remove read-only authority%s: %w", suffix, err)
		}
		removed = true
	}
	if removed {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

func inspectDBWithPlan(ctx context.Context, db *sql.DB, path string, minimum *AuthorityHead, plan []migration) (Inspection, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return Inspection{}, err
	}
	target := len(plan)
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return Inspection{}, fmt.Errorf("read schema version: %w", err)
	}
	if version > target {
		return Inspection{}, fmt.Errorf("future schema version %d exceeds supported %d", version, target)
	}
	if version < 1 {
		return Inspection{}, errorsf("unmanaged or incomplete authority database")
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, version, plan); err != nil {
		return Inspection{}, fmt.Errorf("validate authority schema version %d: %w", version, err)
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect authority integrity: %w", err)
	}
	if !report.OK() {
		return Inspection{}, integrityFailure(report)
	}
	head, err := readAuthorityHead(ctx, db)
	if err != nil {
		return Inspection{}, fmt.Errorf("read authority head: %w", err)
	}
	if minimum != nil {
		if err := requireMinimumHead(head, *minimum); err != nil {
			return Inspection{}, err
		}
	}
	status := InspectionCurrent
	var transition UpgradeHeadTransition
	if version < target {
		status = InspectionUpgradeRequired
		effect, effectErr := pendingUpgradePlanEffect(plan, version)
		if effectErr != nil {
			return Inspection{}, effectErr
		}
		transition = effect.headTransition
	}
	return Inspection{
		Path: path, SchemaVersion: version, TargetVersion: target,
		Status: status, Head: head, Integrity: report, HeadTransition: transition,
	}, nil
}

// PrepareUpgrade creates an immutable exact-head backup and an independently
// validated target-version candidate. It never changes SourcePath and never
// publishes CandidatePath over an existing file unless ReplaceCandidate is
// explicitly set for crash recovery.
func PrepareUpgrade(ctx context.Context, opts UpgradeOptions) (UpgradeResult, error) {
	plan, err := migrationPlanForTarget(opts.TargetVersion)
	if err != nil {
		return UpgradeResult{}, err
	}
	return prepareUpgradeWithPlan(ctx, opts, plan)
}

// ExpectedUpgradeHeadTransition returns the frozen head effect for one
// supported source-to-target plan prefix. Coordinators persist this typed value in
// upgrade intent rather than inferring it from version numbers or legacy
// defaults.
func ExpectedUpgradeHeadTransition(sourceVersion, targetVersion int) (UpgradeHeadTransition, error) {
	plan := currentMigrationPlan()
	if targetVersion < 2 || targetVersion > len(plan) {
		return "", fmt.Errorf("corestore: unsupported upgrade target version %d", targetVersion)
	}
	effect, err := pendingUpgradePlanEffect(plan[:targetVersion], sourceVersion)
	if err != nil {
		return "", err
	}
	return effect.headTransition, nil
}

// PrepareUpgradeTargetBackup creates or reuses an independently copied,
// verified backup of one exact published target. It is deliberately separate
// from PrepareUpgrade: calling it only after candidate publication prevents a
// large source backup, candidate, and target backup from consuming space at the
// same time while the bloated source file is still live.
func PrepareUpgradeTargetBackup(ctx context.Context, opts UpgradeTargetBackupOptions) (BackupInfo, error) {
	plan, err := migrationPlanForTarget(opts.ExpectedSchemaVersion)
	if err != nil {
		return BackupInfo{}, err
	}
	source, err := inspectWithPlan(ctx, InspectOptions{Path: opts.SourcePath, MinimumHead: &opts.ExpectedHead}, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if source.Status != InspectionCurrent || source.SchemaVersion != opts.ExpectedSchemaVersion || source.Head != opts.ExpectedHead {
		return BackupInfo{}, fmt.Errorf("%w: target-backup source is not the exact published target", ErrRollback)
	}
	destination, err := destinationPath(opts.BackupPath, "upgrade target backup")
	if err != nil {
		return BackupInfo{}, err
	}
	if err := requireDistinctUpgradePaths(source.Path, destination, upgradeBackupPreparingPath(destination)); err != nil {
		return BackupInfo{}, err
	}
	backup, err := reuseOrCreateUpgradeBackup(ctx, source, destination, plan)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("prepare upgrade target backup: %w", err)
	}
	sourceInfo, err := os.Stat(source.Path)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("inspect target-backup source: %w", err)
	}
	backupInfo, err := os.Stat(backup.Path)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("inspect target backup: %w", err)
	}
	if os.SameFile(sourceInfo, backupInfo) {
		return BackupInfo{}, errorsf("upgrade target backup must be an independent copy")
	}
	return backup, nil
}

// RecomputeUpgradeMaintenance validates one exact old-version authority and
// recomputes the deterministic maintenance evidence for a frozen target plan.
// It opens the source read-only, creates no backup or candidate, and changes no
// authority bytes. Coordinators use it against the retained exact source
// backup before retiring that backup.
func RecomputeUpgradeMaintenance(ctx context.Context, opts RecomputeUpgradeMaintenanceOptions) (result UpgradeMaintenanceResult, retErr error) {
	plan, err := migrationPlanForTarget(opts.TargetVersion)
	if err != nil {
		return UpgradeMaintenanceResult{}, err
	}
	if opts.ExpectedSchemaVersion < 1 || opts.ExpectedSchemaVersion >= len(plan) {
		return UpgradeMaintenanceResult{}, fmt.Errorf(
			"corestore: maintenance source schema version %d has no pending migration to target %d",
			opts.ExpectedSchemaVersion,
			len(plan),
		)
	}
	path, _, err := existingRegularPath(opts.SourcePath, "upgrade maintenance source")
	if err != nil {
		return UpgradeMaintenanceResult{}, err
	}
	beforeSidecars, err := snapshotReadOnlySidecars(path)
	if err != nil {
		return UpgradeMaintenanceResult{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("open upgrade maintenance source: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		closeErr := db.Close()
		cleanupErr := cleanupNewReadOnlySidecars(path, beforeSidecars)
		if closeErr != nil {
			closeErr = fmt.Errorf("close upgrade maintenance source: %w", closeErr)
		}
		retErr = errors.Join(retErr, closeErr, cleanupErr)
	}()
	if err := db.PingContext(ctx); err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("open upgrade maintenance source: %w", err)
	}
	exact := opts.ExpectedHead
	inspection, err := inspectDBWithPlan(ctx, db, path, &exact, plan)
	if err != nil {
		return UpgradeMaintenanceResult{}, err
	}
	if inspection.Status != InspectionUpgradeRequired ||
		inspection.SchemaVersion != opts.ExpectedSchemaVersion ||
		inspection.Head != opts.ExpectedHead {
		return UpgradeMaintenanceResult{}, fmt.Errorf(
			"%w: maintenance source is not the exact expected version and head",
			ErrRollback,
		)
	}
	effect, err := pendingUpgradePlanEffect(plan, inspection.SchemaVersion)
	if err != nil {
		return UpgradeMaintenanceResult{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("begin upgrade maintenance inspection: %w", err)
	}
	defer tx.Rollback()
	var snapshotVersion int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&snapshotVersion); err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("read upgrade maintenance source version: %w", err)
	}
	snapshotHead, err := readAuthorityHead(ctx, tx)
	if err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("read upgrade maintenance source head: %w", err)
	}
	if snapshotVersion != opts.ExpectedSchemaVersion || snapshotHead != opts.ExpectedHead {
		return UpgradeMaintenanceResult{}, fmt.Errorf(
			"%w: maintenance source changed before evidence recomputation",
			ErrRollback,
		)
	}
	result.Compacted = effect.compactCandidate
	for _, m := range plan[inspection.SchemaVersion:] {
		if m.maintenance == nil {
			continue
		}
		if m.maintenance.ObservationDiscard != nil {
			summary, err := summarizeObservationDiscard(ctx, tx, m)
			if err != nil {
				return UpgradeMaintenanceResult{}, fmt.Errorf("recompute migration %d observation discard: %w", m.version, err)
			}
			result.Discards = append(result.Discards, summary)
			if effect.retireSourceBackup && summary.RemovedRows > 0 {
				result.SourceBackupRetirementRequired = true
			}
		}
		if m.maintenance.EventDiscard != nil {
			summary, err := summarizeEventDiscard(ctx, tx, m)
			if err != nil {
				return UpgradeMaintenanceResult{}, fmt.Errorf("recompute migration %d event discard: %w", m.version, err)
			}
			result.EventDiscards = append(result.EventDiscards, summary)
			if effect.retireSourceBackup && summary.RemovedRows > 0 {
				result.SourceBackupRetirementRequired = true
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return UpgradeMaintenanceResult{}, fmt.Errorf("finish upgrade maintenance inspection: %w", err)
	}
	return result, nil
}

// QuiesceForReplacement checkpoints the exact validated old authority and
// removes only disposable SQLite sidecars. The caller must hold the daemon's
// state-root persistence lock and have closed every Store handle for this path
// for the full call through atomic replacement. It is intentionally separate
// from PrepareUpgrade because this is the one step that may change SourcePath's
// physical representation, and belongs immediately before atomic publication.
func QuiesceForReplacement(ctx context.Context, opts QuiesceOptions) (Inspection, error) {
	plan := currentMigrationPlan()
	if opts.ExpectedSchemaVersion < 1 || opts.ExpectedSchemaVersion > len(plan) {
		return Inspection{}, errorsf("unsupported expected schema version for replacement")
	}
	path, _, err := existingRegularPath(opts.Path, "replacement source")
	if err != nil {
		return Inspection{}, err
	}
	if err := validateReplacementSidecarTypes(path); err != nil {
		return Inspection{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, false))
	if err != nil {
		return Inspection{}, fmt.Errorf("open replacement source: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("open replacement source: %w", err)
	}
	expected := opts.ExpectedHead
	before, err := inspectDBWithPlan(ctx, db, path, &expected, plan)
	if err != nil {
		_ = db.Close()
		return Inspection{}, err
	}
	if before.SchemaVersion != opts.ExpectedSchemaVersion || before.Head != opts.ExpectedHead {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("%w: replacement source does not match expected version and head", ErrRollback)
	}
	var checkpoint CheckpointResult
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&checkpoint.Busy, &checkpoint.LogFrames, &checkpoint.CheckpointedFrames); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("checkpoint replacement source: %w", err)
	}
	if checkpoint.Busy != 0 {
		_ = db.Close()
		return Inspection{}, ErrCheckpointBusy
	}
	if err := db.Close(); err != nil {
		return Inspection{}, fmt.Errorf("close replacement source: %w", err)
	}
	if err := removeQuiescedSidecars(path); err != nil {
		return Inspection{}, err
	}
	if err := syncFile(path); err != nil {
		return Inspection{}, fmt.Errorf("sync replacement source: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return Inspection{}, err
	}
	after, err := inspectWithPlan(ctx, InspectOptions{Path: path, MinimumHead: &expected}, plan)
	if err != nil {
		return Inspection{}, err
	}
	if after.SchemaVersion != opts.ExpectedSchemaVersion || after.Head != opts.ExpectedHead {
		return Inspection{}, fmt.Errorf("%w: replacement source changed during checkpoint", ErrRollback)
	}
	return after, nil
}

func removeQuiescedSidecars(path string) error {
	walPath := path + "-wal"
	if info, err := os.Lstat(walPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errorsf("replacement WAL sidecar must be a regular file, not a symbolic link")
		}
		if err := validatePrivateRegularInfo(info, "replacement WAL sidecar"); err != nil {
			return err
		}
		if info.Size() != 0 {
			return errorsf("replacement WAL sidecar is not empty after checkpoint")
		}
		if err := os.Remove(walPath); err != nil {
			return fmt.Errorf("remove empty replacement WAL: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement WAL: %w", err)
	}
	shmPath := path + "-shm"
	if info, err := os.Lstat(shmPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errorsf("replacement SHM sidecar must be a regular file, not a symbolic link")
		}
		if err := validatePrivateRegularInfo(info, "replacement SHM sidecar"); err != nil {
			return err
		}
		if err := os.Remove(shmPath); err != nil {
			return fmt.Errorf("remove disposable replacement SHM: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement SHM: %w", err)
	}
	journalPath := path + "-journal"
	if info, err := os.Lstat(journalPath); err == nil {
		if err := validatePrivateRegularInfo(info, "replacement rollback journal"); err != nil {
			return err
		}
		if info.Size() != 0 {
			return errorsf("replacement rollback journal is not empty")
		}
		if err := os.Remove(journalPath); err != nil {
			return fmt.Errorf("remove empty replacement rollback journal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement rollback journal: %w", err)
	}
	return nil
}

func validateReplacementSidecarTypes(path string) error {
	for _, item := range []struct {
		suffix string
		label  string
	}{{"-wal", "WAL"}, {"-shm", "SHM"}, {"-journal", "rollback journal"}} {
		info, err := os.Lstat(path + item.suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect replacement %s: %w", item.label, err)
		}
		if err := validatePrivateRegularInfo(info, "replacement "+item.label+" sidecar"); err != nil {
			return err
		}
	}
	return nil
}

func prepareUpgradeWithPlan(ctx context.Context, opts UpgradeOptions, plan []migration) (UpgradeResult, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return UpgradeResult{}, err
	}
	if opts.ResetUnboundArtifacts && !opts.ReplaceCandidate {
		return UpgradeResult{}, errorsf("resetting unbound upgrade artifacts requires ReplaceCandidate")
	}
	source, err := inspectWithPlan(ctx, InspectOptions{Path: opts.SourcePath, MinimumHead: opts.MinimumHead}, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	if source.Status != InspectionUpgradeRequired {
		return UpgradeResult{}, fmt.Errorf("corestore: database schema is already current at version %d", source.SchemaVersion)
	}
	effect, err := pendingUpgradePlanEffect(plan, source.SchemaVersion)
	if err != nil {
		return UpgradeResult{}, err
	}

	backupPath, err := destinationPath(opts.BackupPath, "upgrade backup")
	if err != nil {
		return UpgradeResult{}, err
	}
	candidatePath, err := destinationPath(opts.CandidatePath, "upgrade candidate")
	if err != nil {
		return UpgradeResult{}, err
	}
	paths := []string{
		source.Path,
		backupPath,
		upgradeBackupPreparingPath(backupPath),
		candidatePath,
		upgradeWorkPath(candidatePath),
		upgradeCompactPath(candidatePath),
	}
	if err := requireDistinctUpgradePaths(paths...); err != nil {
		return UpgradeResult{}, err
	}
	if filepath.Dir(candidatePath) != filepath.Dir(source.Path) {
		return UpgradeResult{}, errorsf("upgrade candidate must be in the authority directory for atomic publication")
	}

	if opts.ResetUnboundArtifacts {
		if err := resetUnboundUpgradeArtifacts(ctx, source, backupPath, candidatePath, plan); err != nil {
			return UpgradeResult{}, err
		}
	}
	if err := prepareCandidateDestination(candidatePath, opts.ReplaceCandidate, effect.compactCandidate); err != nil {
		return UpgradeResult{}, err
	}

	var backup BackupInfo
	var candidate upgradeCandidateBuild
	if effect.compactCandidate {
		// The working snapshot and its potentially source-sized delete WAL must be
		// gone before the immutable old-head backup is created. This keeps the new
		// preparation peak within the controller's two-source-footprint preflight.
		candidate, err = buildCompactUpgradeCandidate(ctx, source, candidatePath, plan, effect)
		if err != nil {
			return UpgradeResult{}, err
		}
		backup, err = reuseOrCreateUpgradeBackup(ctx, source, backupPath, plan)
		if err != nil {
			return UpgradeResult{}, err
		}
	} else {
		backup, err = reuseOrCreateUpgradeBackup(ctx, source, backupPath, plan)
		if err != nil {
			return UpgradeResult{}, err
		}
		candidate, err = buildUpgradeCandidateFromBackup(ctx, source, backup, candidatePath, plan, effect)
		if err != nil {
			return UpgradeResult{}, err
		}
	}

	finalSource, err := inspectWithPlan(ctx, InspectOptions{Path: source.Path, MinimumHead: &source.Head}, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	if finalSource.SchemaVersion != source.SchemaVersion || finalSource.Head != source.Head {
		return UpgradeResult{}, fmt.Errorf("%w: upgrade source changed while preparing candidate", ErrRollback)
	}
	if err := requireIndependentUpgradeArtifacts(finalSource, backup, candidate.inspection, nil); err != nil {
		return UpgradeResult{}, err
	}
	return UpgradeResult{
		Source:         finalSource,
		Backup:         backup,
		Candidate:      candidate.inspection,
		TargetBackup:   nil,
		Maintenance:    candidate.maintenance,
		HeadTransition: candidate.headTransition,
	}, nil
}

func pendingUpgradePlanEffect(plan []migration, sourceVersion int) (upgradePlanEffect, error) {
	if sourceVersion < 1 || sourceVersion >= len(plan) {
		return upgradePlanEffect{}, fmt.Errorf("corestore: no supported pending migrations from version %d", sourceVersion)
	}
	effect := upgradePlanEffect{headTransition: UpgradeHeadTransitionPreserve}
	for _, m := range plan[sourceVersion:] {
		// Advancing is the fail-safe default. Preservation is allowed only when
		// every pending migration carries the frozen reviewed maintenance marker.
		if m.maintenance == nil || !m.maintenance.PreserveAuthorityHead {
			effect.headTransition = UpgradeHeadTransitionAdvanceOnce
		}
		if m.maintenance == nil {
			continue
		}
		effect.compactCandidate = effect.compactCandidate || m.maintenance.CompactCandidate
		effect.retireSourceBackup = effect.retireSourceBackup || m.maintenance.RetireSourceBackup
	}
	return effect, nil
}

func expectedUpgradeHead(source AuthorityHead, transition UpgradeHeadTransition) (AuthorityHead, error) {
	switch transition {
	case UpgradeHeadTransitionPreserve:
		return source, nil
	case UpgradeHeadTransitionAdvanceOnce:
		source.HeadGeneration++
		return source, nil
	default:
		return AuthorityHead{}, fmt.Errorf("corestore: unknown upgrade head transition %q", transition)
	}
}

func requireDistinctUpgradePaths(paths ...string) error {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			return errorsf("upgrade source, backup, candidate, and target-backup paths must differ")
		}
		seen[path] = struct{}{}
	}
	return nil
}

func requireIndependentUpgradeArtifacts(source Inspection, backup BackupInfo, candidate Inspection, target *BackupInfo) error {
	artifacts := []struct {
		path  string
		label string
	}{{source.Path, "source"}, {backup.Path, "source backup"}, {candidate.Path, "candidate"}}
	if target != nil {
		artifacts = append(artifacts, struct {
			path  string
			label string
		}{target.Path, "target backup"})
	}
	for i := range artifacts {
		left, err := os.Stat(artifacts[i].path)
		if err != nil {
			return fmt.Errorf("inspect upgrade %s: %w", artifacts[i].label, err)
		}
		for j := range i {
			right, err := os.Stat(artifacts[j].path)
			if err != nil {
				return fmt.Errorf("inspect upgrade %s: %w", artifacts[j].label, err)
			}
			if os.SameFile(left, right) {
				return fmt.Errorf("corestore: upgrade %s must be independent from %s", artifacts[i].label, artifacts[j].label)
			}
		}
	}
	return nil
}

func reuseOrCreateUpgradeBackup(ctx context.Context, source Inspection, destination string, plan []migration) (BackupInfo, error) {
	info, err := os.Lstat(destination)
	switch {
	case err == nil:
		if err := validatePrivateRegularInfo(info, "upgrade backup"); err != nil {
			return BackupInfo{}, err
		}
		if err := requireNoSQLiteSidecars(destination, "upgrade backup"); err != nil {
			return BackupInfo{}, err
		}
		sourceInfo, statErr := os.Stat(source.Path)
		if statErr != nil {
			return BackupInfo{}, fmt.Errorf("inspect upgrade source: %w", statErr)
		}
		if os.SameFile(sourceInfo, info) {
			return BackupInfo{}, errorsf("upgrade backup must be independent from source")
		}
		backup, verifyErr := verifyBackupWithPlan(ctx, destination, source.Head, source.SchemaVersion, plan)
		if verifyErr != nil {
			return BackupInfo{}, fmt.Errorf("reuse upgrade backup: %w", verifyErr)
		}
		if backup.Head != source.Head || backup.SchemaVersion != source.SchemaVersion {
			return BackupInfo{}, fmt.Errorf("%w: upgrade backup is not at exact source version and head", ErrRollback)
		}
		if err := syncFile(destination); err != nil {
			return BackupInfo{}, fmt.Errorf("sync reused upgrade backup: %w", err)
		}
		if err := syncDir(filepath.Dir(destination)); err != nil {
			return BackupInfo{}, fmt.Errorf("sync reused upgrade backup directory: %w", err)
		}
		if err := removeUnpublishedSQLiteArtifact(upgradeBackupPreparingPath(destination), "upgrade backup preparing copy"); err != nil {
			return BackupInfo{}, err
		}
		return backup, nil
	case !errors.Is(err, os.ErrNotExist):
		return BackupInfo{}, fmt.Errorf("inspect upgrade backup: %w", err)
	}
	return createUpgradeBackup(ctx, source, destination, plan)
}

func requireNoSQLiteSidecars(path, label string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s%s: %w", label, suffix, err)
		}
		if err := validatePrivateRegularInfo(info, label+suffix); err != nil {
			return err
		}
		return fmt.Errorf("corestore: %s is not standalone: unexpected %s sidecar", label, suffix)
	}
	return nil
}

func createUpgradeBackup(ctx context.Context, source Inspection, destination string, plan []migration) (BackupInfo, error) {
	preparingPath := upgradeBackupPreparingPath(destination)
	if err := prepareUnpublishedSQLiteArtifact(preparingPath, "upgrade backup preparing copy", true); err != nil {
		return BackupInfo{}, err
	}
	if err := createPrivateEmptyUpgradeArtifact(preparingPath, "upgrade backup preparing copy"); err != nil {
		return BackupInfo{}, err
	}
	defer cleanupBackupTemp(preparingPath)
	db, err := sql.Open("sqlite", sqliteDSN(source.Path, defaultBusyTimeout, true))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("open upgrade source: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return BackupInfo{}, fmt.Errorf("open upgrade source: %w", err)
	}
	exact := source.Head
	before, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if before.SchemaVersion != source.SchemaVersion || before.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade source changed before backup", ErrRollback)
	}

	if err := runOnlineBackupDB(ctx, db, preparingPath); err != nil {
		return BackupInfo{}, fmt.Errorf("snapshot upgrade source: %w", err)
	}
	after, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if after.SchemaVersion != source.SchemaVersion || after.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade source changed during backup", ErrRollback)
	}
	if err := normalizeBackupJournal(ctx, preparingPath); err != nil {
		return BackupInfo{}, err
	}
	if err := removeBackupSidecars(preparingPath); err != nil {
		return BackupInfo{}, err
	}
	if err := syncFile(preparingPath); err != nil {
		return BackupInfo{}, err
	}
	backup, err := verifyBackupWithPlan(ctx, preparingPath, source.Head, source.SchemaVersion, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if backup.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade backup is not at exact source head", ErrRollback)
	}
	if err := os.Link(preparingPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupInfo{}, errorsf("upgrade backup destination already exists")
		}
		return BackupInfo{}, fmt.Errorf("publish upgrade backup: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return BackupInfo{}, err
	}
	if err := removeUnpublishedSQLiteArtifact(preparingPath, "upgrade backup preparing copy"); err != nil {
		return BackupInfo{}, err
	}
	backup.Path = destination
	return backup, nil
}

func upgradeBackupPreparingPath(destination string) string { return destination + ".preparing" }

func prepareCandidateDestination(path string, replace, compact bool) error {
	if err := prepareUnpublishedSQLiteArtifact(path, "upgrade candidate", replace); err != nil {
		return err
	}
	if err := prepareUnpublishedSQLiteArtifact(upgradeWorkPath(path), "upgrade working snapshot", replace); err != nil {
		return err
	}
	if compact {
		if err := prepareUnpublishedSQLiteArtifact(upgradeCompactPath(path), "upgrade compact output", replace); err != nil {
			return err
		}
	}
	return nil
}

func upgradeWorkPath(candidate string) string    { return candidate + ".maintenance-work" }
func upgradeCompactPath(candidate string) string { return candidate + ".maintenance-compact" }

type unpublishedSQLiteArtifact struct {
	path  string
	label string
}

func inspectUnpublishedSQLiteArtifact(path, label string) ([]unpublishedSQLiteArtifact, error) {
	var existing []string
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		candidate := path + suffix
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s%s: %w", label, suffix, err)
		}
		if err := validatePrivateRegularInfo(info, label+suffix); err != nil {
			return nil, err
		}
		existing = append(existing, candidate)
	}
	artifacts := make([]unpublishedSQLiteArtifact, 0, len(existing))
	for _, candidate := range existing {
		artifacts = append(artifacts, unpublishedSQLiteArtifact{path: candidate, label: label})
	}
	return artifacts, nil
}

func prepareUnpublishedSQLiteArtifact(path, label string, replace bool) error {
	existing, err := inspectUnpublishedSQLiteArtifact(path, label)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	if !replace {
		return fmt.Errorf("corestore: %s destination already exists", label)
	}
	for _, artifact := range slices.Backward(existing) {
		if err := os.Remove(artifact.path); err != nil {
			return fmt.Errorf("remove unpublished %s: %w", label, err)
		}
	}
	return syncDir(filepath.Dir(path))
}

func removeUnpublishedSQLiteArtifact(path, label string) error {
	return prepareUnpublishedSQLiteArtifact(path, label, true)
}

// resetUnboundUpgradeArtifacts is the explicit preparing-state recovery path.
// A candidate without a durable outer fingerprint is not trusted or adopted.
// The exact live source is revalidated first, then every deterministic
// unpublished candidate and source-backup artifact is type-checked before any
// removal. Directory syncs make the reset durable before preparation allocates
// another working snapshot.
func resetUnboundUpgradeArtifacts(
	ctx context.Context,
	source Inspection,
	backupPath, candidatePath string,
	plan []migration,
) error {
	exact := source.Head
	revalidated, err := inspectWithPlan(ctx, InspectOptions{Path: source.Path, MinimumHead: &exact}, plan)
	if err != nil {
		return err
	}
	if revalidated.Status != InspectionUpgradeRequired ||
		revalidated.SchemaVersion != source.SchemaVersion ||
		revalidated.Head != source.Head {
		return fmt.Errorf("%w: upgrade source changed before resetting unbound artifacts", ErrRollback)
	}
	targets := []struct {
		path  string
		label string
	}{
		{candidatePath, "unbound upgrade candidate"},
		{upgradeWorkPath(candidatePath), "unbound upgrade working snapshot"},
		{upgradeCompactPath(candidatePath), "unbound upgrade compact output"},
		{backupPath, "unbound upgrade source backup"},
		{upgradeBackupPreparingPath(backupPath), "unbound upgrade backup preparing copy"},
	}
	var artifacts []unpublishedSQLiteArtifact
	var directories []string
	seenDirectories := make(map[string]struct{})
	for _, target := range targets {
		found, err := inspectUnpublishedSQLiteArtifact(target.path, target.label)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, found...)
		dir := filepath.Dir(target.path)
		if _, ok := seenDirectories[dir]; !ok {
			seenDirectories[dir] = struct{}{}
			directories = append(directories, dir)
		}
	}
	for _, artifact := range slices.Backward(artifacts) {
		if err := os.Remove(artifact.path); err != nil {
			return fmt.Errorf("remove %s: %w", artifact.label, err)
		}
	}
	for _, dir := range directories {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func buildUpgradeCandidateFromBackup(
	ctx context.Context,
	source Inspection,
	backup BackupInfo,
	destination string,
	plan []migration,
	effect upgradePlanEffect,
) (upgradeCandidateBuild, error) {
	workPath := upgradeWorkPath(destination)
	if err := copyUpgradeArtifact(backup.Path, workPath); err != nil {
		return upgradeCandidateBuild{}, err
	}
	return migrateAndPublishUpgradeCandidate(ctx, source, workPath, "", destination, plan, effect)
}

func buildCompactUpgradeCandidate(
	ctx context.Context,
	source Inspection,
	destination string,
	plan []migration,
	effect upgradePlanEffect,
) (upgradeCandidateBuild, error) {
	workPath := upgradeWorkPath(destination)
	if err := createDisposableUpgradeSnapshot(ctx, source, workPath, plan); err != nil {
		return upgradeCandidateBuild{}, err
	}
	return migrateAndPublishUpgradeCandidate(ctx, source, workPath, upgradeCompactPath(destination), destination, plan, effect)
}

func copyUpgradeArtifact(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open upgrade backup: %w", err)
	}
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = src.Close()
		return fmt.Errorf("create upgrade working snapshot: %w", err)
	}
	_, copyErr := io.Copy(dst, src)
	closeSourceErr := src.Close()
	syncErr := dst.Sync()
	closeDestinationErr := dst.Close()
	if copyErr != nil {
		return fmt.Errorf("copy upgrade backup: %w", copyErr)
	}
	if closeSourceErr != nil {
		return fmt.Errorf("close upgrade backup: %w", closeSourceErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync upgrade working snapshot: %w", syncErr)
	}
	if closeDestinationErr != nil {
		return fmt.Errorf("close upgrade working snapshot: %w", closeDestinationErr)
	}
	return nil
}

func createDisposableUpgradeSnapshot(ctx context.Context, source Inspection, destination string, plan []migration) error {
	if err := createPrivateEmptyUpgradeArtifact(destination, "upgrade working snapshot"); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteDSN(source.Path, defaultBusyTimeout, true))
	if err != nil {
		return fmt.Errorf("open upgrade source for working snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("open upgrade source for working snapshot: %w", err)
	}
	exact := source.Head
	before, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return err
	}
	if before.SchemaVersion != source.SchemaVersion || before.Head != source.Head {
		return fmt.Errorf("%w: upgrade source changed before working snapshot", ErrRollback)
	}
	if err := runOnlineBackupDB(ctx, db, destination); err != nil {
		return fmt.Errorf("snapshot upgrade working source: %w", err)
	}
	after, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return err
	}
	if after.SchemaVersion != source.SchemaVersion || after.Head != source.Head {
		return fmt.Errorf("%w: upgrade source changed during working snapshot", ErrRollback)
	}
	return nil
}

func migrateAndPublishUpgradeCandidate(
	ctx context.Context,
	source Inspection,
	workPath, compactPath, destination string,
	plan []migration,
	effect upgradePlanEffect,
) (upgradeCandidateBuild, error) {
	defer cleanupBackupTemp(workPath)
	if compactPath != "" {
		defer cleanupBackupTemp(compactPath)
	}
	db, err := sql.Open("sqlite", sqliteDSN(workPath, defaultBusyTimeout, false))
	if err != nil {
		return upgradeCandidateBuild{}, fmt.Errorf("open upgrade working snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return upgradeCandidateBuild{}, fmt.Errorf("open upgrade working snapshot: %w", err)
	}
	if err := verifyPragmas(ctx, db, defaultBusyTimeout); err != nil {
		_ = db.Close()
		return upgradeCandidateBuild{}, err
	}
	execution, migrationErr := migratePendingAtomicallyDetailed(ctx, db, plan, time.Now().UTC())
	if migrationErr == nil {
		migrationErr = validateUpgradedCandidate(ctx, db, source, execution, plan)
	}
	if migrationErr == nil && compactPath != "" {
		migrationErr = checkpointUpgradeWorkingSnapshot(ctx, db, workPath)
	}
	if migrationErr == nil && compactPath != "" {
		if err := createPrivateEmptyUpgradeArtifact(compactPath, "upgrade compact output"); err != nil {
			migrationErr = err
		}
	}
	if migrationErr == nil && compactPath != "" {
		if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, compactPath); err != nil {
			migrationErr = fmt.Errorf("compact upgraded candidate: %w", err)
		}
	}
	closeErr := db.Close()
	if migrationErr != nil {
		return upgradeCandidateBuild{}, migrationErr
	}
	if closeErr != nil {
		return upgradeCandidateBuild{}, fmt.Errorf("close upgrade working snapshot: %w", closeErr)
	}

	outputPath := workPath
	if compactPath != "" {
		outputPath = compactPath
		execution.maintenance.Compacted = true
	}
	if execution.headTransition != effect.headTransition {
		return upgradeCandidateBuild{}, errorsf("upgrade head transition changed during candidate build")
	}
	if err := normalizeBackupJournal(ctx, outputPath); err != nil {
		return upgradeCandidateBuild{}, err
	}
	if err := removeBackupSidecars(outputPath); err != nil {
		return upgradeCandidateBuild{}, err
	}
	if err := syncFile(outputPath); err != nil {
		return upgradeCandidateBuild{}, err
	}
	expectedHead, err := expectedUpgradeHead(source.Head, execution.headTransition)
	if err != nil {
		return upgradeCandidateBuild{}, err
	}
	verified, err := inspectWithPlan(ctx, InspectOptions{Path: outputPath, MinimumHead: &expectedHead}, plan)
	if err != nil {
		return upgradeCandidateBuild{}, err
	}
	if verified.Status != InspectionCurrent || verified.Head != expectedHead {
		return upgradeCandidateBuild{}, fmt.Errorf("%w: upgraded candidate has unexpected version or head", ErrRollback)
	}
	if err := verifyMaintenanceDiscardsAbsent(ctx, outputPath, execution.maintenance.Discards, execution.maintenance.EventDiscards); err != nil {
		return upgradeCandidateBuild{}, err
	}
	if err := os.Link(outputPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return upgradeCandidateBuild{}, errorsf("upgrade candidate destination already exists")
		}
		return upgradeCandidateBuild{}, fmt.Errorf("publish upgrade candidate: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return upgradeCandidateBuild{}, err
	}
	// Remove both deterministic unpublished build names before returning. In the
	// maintenance path this is the space boundary: only now may the old-head
	// backup be created.
	if err := removeUnpublishedSQLiteArtifact(workPath, "upgrade working snapshot"); err != nil {
		return upgradeCandidateBuild{}, err
	}
	if compactPath != "" {
		if err := removeUnpublishedSQLiteArtifact(compactPath, "upgrade compact output"); err != nil {
			return upgradeCandidateBuild{}, err
		}
	}
	verified.Path = destination
	return upgradeCandidateBuild{
		inspection:     verified,
		headTransition: execution.headTransition,
		maintenance:    execution.maintenance,
	}, nil
}

func createPrivateEmptyUpgradeArtifact(path, label string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure %s: %w", label, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close empty %s: %w", label, err)
	}
	return nil
}

func checkpointUpgradeWorkingSnapshot(ctx context.Context, db *sql.DB, path string) error {
	var checkpoint CheckpointResult
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&checkpoint.Busy,
		&checkpoint.LogFrames,
		&checkpoint.CheckpointedFrames,
	); err != nil {
		return fmt.Errorf("checkpoint upgraded working snapshot: %w", err)
	}
	if checkpoint.Busy != 0 {
		return fmt.Errorf("checkpoint upgraded working snapshot: %w", ErrCheckpointBusy)
	}
	walInfo, err := os.Stat(path + "-wal")
	if err == nil && walInfo.Size() != 0 {
		return errorsf("upgraded working snapshot WAL was not truncated")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect upgraded working snapshot WAL: %w", err)
	}
	return nil
}

func migratePendingAtomically(ctx context.Context, db *sql.DB, plan []migration, now time.Time) (AuthorityHead, AuthorityHead, error) {
	execution, err := migratePendingAtomicallyDetailed(ctx, db, plan, now)
	return execution.before, execution.after, err
}

func migratePendingAtomicallyDetailed(ctx context.Context, db *sql.DB, plan []migration, now time.Time) (pendingMigrationExecution, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return pendingMigrationExecution{}, err
	}
	var version, appID int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return pendingMigrationExecution{}, err
	}
	effect, err := pendingUpgradePlanEffect(plan, version)
	if err != nil {
		return pendingMigrationExecution{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&appID); err != nil {
		return pendingMigrationExecution{}, err
	}
	if appID != applicationID {
		return pendingMigrationExecution{}, errorsf("application identity mismatch")
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, version, plan); err != nil {
		return pendingMigrationExecution{}, err
	}
	before, err := readAuthorityHead(ctx, db)
	if err != nil {
		return pendingMigrationExecution{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return pendingMigrationExecution{}, fmt.Errorf("begin atomic schema upgrade: %w", err)
	}
	fail := func(err error) (pendingMigrationExecution, error) {
		_ = tx.Rollback()
		return pendingMigrationExecution{}, err
	}
	maintenance := UpgradeMaintenanceResult{}
	stamp := formatTime(now)
	for next := version + 1; next <= len(plan); next++ {
		m := plan[next-1]
		var observationSummary *ObservationDiscardSummary
		var eventSummary *EventDiscardSummary
		if m.maintenance != nil && m.maintenance.ObservationDiscard != nil {
			computed, err := summarizeObservationDiscard(ctx, tx, m)
			if err != nil {
				return fail(fmt.Errorf("summarize migration %d observation discard: %w", next, err))
			}
			observationSummary = &computed
		}
		if m.maintenance != nil && m.maintenance.EventDiscard != nil {
			computed, err := summarizeEventDiscard(ctx, tx, m)
			if err != nil {
				return fail(fmt.Errorf("summarize migration %d event discard: %w", next, err))
			}
			eventSummary = &computed
		}
		for _, stmt := range m.statements {
			result, err := tx.ExecContext(ctx, stmt)
			if err != nil {
				return fail(fmt.Errorf("apply migration %d: %w", next, err))
			}
			if observationSummary != nil && stmt == observationDiscardDeleteStatement(observationSummary.Selector) {
				affected, err := result.RowsAffected()
				if err != nil {
					return fail(fmt.Errorf("count migration %d discarded rows: %w", next, err))
				}
				if affected != observationSummary.RemovedRows {
					return fail(fmt.Errorf("migration %d discarded %d observation rows after summarizing %d", next, affected, observationSummary.RemovedRows))
				}
			}
			if eventSummary != nil && stmt == eventDiscardDeleteStatement(eventSummary.Selector) {
				affected, err := result.RowsAffected()
				if err != nil {
					return fail(fmt.Errorf("count migration %d discarded event rows: %w", next, err))
				}
				if affected != eventSummary.RemovedRows {
					return fail(fmt.Errorf("migration %d discarded %d event rows after summarizing %d", next, affected, eventSummary.RemovedRows))
				}
			}
		}
		if observationSummary != nil {
			if err := verifyObservationDiscardAbsent(ctx, tx, observationSummary.Selector); err != nil {
				return fail(fmt.Errorf("verify migration %d observation discard: %w", next, err))
			}
			maintenance.Discards = append(maintenance.Discards, *observationSummary)
			if effect.retireSourceBackup && observationSummary.RemovedRows > 0 {
				maintenance.SourceBackupRetirementRequired = true
			}
		}
		if eventSummary != nil {
			if err := verifyEventDiscardAbsent(ctx, tx, eventSummary.Selector); err != nil {
				return fail(fmt.Errorf("verify migration %d event discard: %w", next, err))
			}
			maintenance.EventDiscards = append(maintenance.EventDiscards, *eventSummary)
			if effect.retireSourceBackup && eventSummary.RemovedRows > 0 {
				maintenance.SourceBackupRetirementRequired = true
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES (?,?,?,?)`, m.version, m.name, migrationChecksum(m), stamp); err != nil {
			return fail(fmt.Errorf("record migration %d: %w", next, err))
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA application_id = %d`, applicationID)); err != nil {
		return fail(fmt.Errorf("stamp application identity: %w", err))
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(plan))); err != nil {
		return fail(fmt.Errorf("stamp schema version: %w", err))
	}
	if effect.headTransition == UpgradeHeadTransitionAdvanceOnce {
		if _, err := tx.ExecContext(ctx, `UPDATE store_meta SET head_generation=head_generation+1, updated_at=? WHERE singleton=1`, stamp); err != nil {
			return fail(fmt.Errorf("advance upgrade authority head: %w", err))
		}
	}
	after, err := readAuthorityHead(ctx, tx)
	if err != nil {
		return fail(fmt.Errorf("read upgraded authority head: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return pendingMigrationExecution{}, fmt.Errorf("commit atomic schema upgrade: %w", err)
	}
	return pendingMigrationExecution{
		before:         before,
		after:          after,
		headTransition: effect.headTransition,
		maintenance:    maintenance,
	}, nil
}

type contextQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func summarizeObservationDiscard(ctx context.Context, q contextQueryer, m migration) (ObservationDiscardSummary, error) {
	selector := *m.maintenance.ObservationDiscard
	summary := ObservationDiscardSummary{
		MigrationVersion: m.version,
		MigrationName:    m.name,
		Selector:         selector,
	}
	h := sha256.New()
	h.Write([]byte("canary.observation-discard.v1\x00"))
	for _, value := range []string{selector.ScopeKey, selector.Source, selector.Kind} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	rows, err := q.QueryContext(ctx, `SELECT observation_id,length(payload),payload_sha256
FROM observations
WHERE scope_key=? AND source=? AND kind=?
ORDER BY observation_id`, selector.ScopeKey, selector.Source, selector.Kind)
	if err != nil {
		return ObservationDiscardSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var observationID, payloadBytes int64
		var payloadDigest []byte
		if err := rows.Scan(&observationID, &payloadBytes, &payloadDigest); err != nil {
			return ObservationDiscardSummary{}, err
		}
		if observationID < 1 || payloadBytes < 0 || len(payloadDigest) != sha256.Size {
			return ObservationDiscardSummary{}, errorsf("invalid observation identity, size, or payload digest while summarizing discard")
		}
		var identity [8]byte
		binary.BigEndian.PutUint64(identity[:], uint64(observationID))
		h.Write(identity[:])
		h.Write(payloadDigest)
		if summary.RemovedRows == math.MaxInt64 || payloadBytes > math.MaxInt64-summary.PayloadBytes {
			return ObservationDiscardSummary{}, errorsf("observation discard summary counter overflow")
		}
		summary.RemovedRows++
		summary.PayloadBytes += payloadBytes
	}
	if err := rows.Err(); err != nil {
		return ObservationDiscardSummary{}, err
	}
	summary.OrderedDigestSHA256 = hex.EncodeToString(h.Sum(nil))
	return summary, nil
}

func summarizeEventDiscard(ctx context.Context, q contextQueryer, m migration) (EventDiscardSummary, error) {
	selector := *m.maintenance.EventDiscard
	summary := EventDiscardSummary{
		MigrationVersion: m.version,
		MigrationName:    m.name,
		Selector:         selector,
	}
	h := sha256.New()
	h.Write([]byte("canary.event-discard.v1\x00"))
	for _, value := range []string{selector.ScopeKey, selector.EventType, selector.Predicate} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	rows, err := q.QueryContext(ctx, eventDiscardSummaryStatement(selector))
	if err != nil {
		return EventDiscardSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var eventSeq, payloadBytes int64
		var payloadDigest []byte
		if err := rows.Scan(&eventSeq, &payloadBytes, &payloadDigest); err != nil {
			return EventDiscardSummary{}, err
		}
		if eventSeq < 1 || payloadBytes < 0 || len(payloadDigest) != sha256.Size {
			return EventDiscardSummary{}, errorsf("invalid event identity, size, or payload digest while summarizing discard")
		}
		var identity [8]byte
		binary.BigEndian.PutUint64(identity[:], uint64(eventSeq))
		h.Write(identity[:])
		h.Write(payloadDigest)
		if summary.RemovedRows == math.MaxInt64 || payloadBytes > math.MaxInt64-summary.PayloadBytes {
			return EventDiscardSummary{}, errorsf("event discard summary counter overflow")
		}
		summary.RemovedRows++
		summary.PayloadBytes += payloadBytes
	}
	if err := rows.Err(); err != nil {
		return EventDiscardSummary{}, err
	}
	summary.OrderedDigestSHA256 = hex.EncodeToString(h.Sum(nil))
	return summary, nil
}

type contextQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func verifyObservationDiscardAbsent(ctx context.Context, q contextQueryRower, selector ObservationDiscardSelector) error {
	var remaining int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM observations WHERE scope_key=? AND source=? AND kind=?`,
		selector.ScopeKey, selector.Source, selector.Kind).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("corestore: exact observation discard selector still matches %d rows", remaining)
	}
	return nil
}

func verifyEventDiscardAbsent(ctx context.Context, q contextQueryRower, selector EventDiscardSelector) error {
	var remaining int64
	if err := q.QueryRowContext(ctx, eventDiscardCountStatement(selector)).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("corestore: exact event discard selector still matches %d rows", remaining)
	}
	return nil
}

func verifyMaintenanceDiscardsAbsent(ctx context.Context, path string, observationSummaries []ObservationDiscardSummary, eventSummaries []EventDiscardSummary) error {
	if len(observationSummaries) == 0 && len(eventSummaries) == 0 {
		return nil
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		return fmt.Errorf("open compact candidate for discard verification: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, summary := range observationSummaries {
		if err := verifyObservationDiscardAbsent(ctx, db, summary.Selector); err != nil {
			return err
		}
	}
	for _, summary := range eventSummaries {
		if err := verifyEventDiscardAbsent(ctx, db, summary.Selector); err != nil {
			return err
		}
	}
	return nil
}

func validateUpgradedCandidate(ctx context.Context, db *sql.DB, source Inspection, execution pendingMigrationExecution, plan []migration) error {
	if execution.before != source.Head {
		return fmt.Errorf("%w: candidate did not start at exact source head", ErrRollback)
	}
	want, err := expectedUpgradeHead(execution.before, execution.headTransition)
	if err != nil {
		return err
	}
	if execution.after != want {
		return fmt.Errorf("%w: schema upgrade did not perform its declared head transition", ErrRollback)
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, len(plan), plan); err != nil {
		return fmt.Errorf("validate upgraded schema: %w", err)
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return fmt.Errorf("validate upgraded integrity: %w", err)
	}
	if !report.OK() {
		return integrityFailure(report)
	}
	got, err := readAuthorityHead(ctx, db)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: upgraded candidate head changed during validation", ErrRollback)
	}
	for _, summary := range execution.maintenance.Discards {
		if err := verifyObservationDiscardAbsent(ctx, db, summary.Selector); err != nil {
			return err
		}
	}
	for _, summary := range execution.maintenance.EventDiscards {
		if err := verifyEventDiscardAbsent(ctx, db, summary.Selector); err != nil {
			return err
		}
	}
	return nil
}

func existingRegularPath(path, label string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, fmt.Errorf("corestore: %s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("inspect %s path: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("corestore: %s path must be a regular file, not a symbolic link", label)
	}
	if err := validatePrivateRegularInfo(info, label); err != nil {
		return "", nil, err
	}
	if err := ensurePrivateParent(filepath.Dir(abs)); err != nil {
		return "", nil, err
	}
	return abs, info, nil
}

func validatePrivateRegularInfo(info os.FileInfo, label string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("corestore: %s must be a regular file, not a symbolic link", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("corestore: %s must not be group or world accessible", label)
	}
	return nil
}

func destinationPath(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("corestore: %s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	if err := ensurePrivateParent(filepath.Dir(abs)); err != nil {
		return "", err
	}
	return abs, nil
}
