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
	coreSchemaUpgradeManifestVersion = 1
	coreSchemaUpgradePreparing       = "preparing"
	coreSchemaUpgradeReady           = "candidate_ready"

	coreSchemaPhaseIntent    = "intent_durable"
	coreSchemaPhaseCandidate = "candidate_ready"
	coreSchemaPhaseWatermark = "watermark_armed"
	coreSchemaPhaseQuiesced  = "source_quiesced"
	coreSchemaPhaseRenamed   = "candidate_renamed"
	coreSchemaPhaseSynced    = "publication_synced"
	coreSchemaPhaseVerified  = "publication_verified"
)

// coreSchemaUpgradeManifest is transient crash-recovery coordination. It
// contains no business state and is removed only after the upgraded authority
// has been reopened and fully validated. Artifact paths are derived from the
// validated ID and versions rather than trusted from JSON.
type coreSchemaUpgradeManifest struct {
	Version         int                      `json:"version"`
	UpgradeID       string                   `json:"upgrade_id"`
	Status          string                   `json:"status"`
	CreatedAt       time.Time                `json:"created_at"`
	SourceVersion   int                      `json:"source_version"`
	TargetVersion   int                      `json:"target_version"`
	SourceHead      corestore.AuthorityHead  `json:"source_head"`
	CandidateHead   *corestore.AuthorityHead `json:"candidate_head,omitempty"`
	BackupSHA256    string                   `json:"backup_sha256,omitempty"`
	BackupBytes     int64                    `json:"backup_bytes,omitempty"`
	CandidateSHA256 string                   `json:"candidate_sha256,omitempty"`
	CandidateBytes  int64                    `json:"candidate_bytes,omitempty"`
}

type coreSchemaUpgradeArtifacts struct {
	source    string
	backup    string
	candidate string
}

type coreSchemaUpgradeOps struct {
	inspect func(context.Context, corestore.InspectOptions) (corestore.Inspection, error)
	prepare func(context.Context, corestore.UpgradeOptions) (corestore.UpgradeResult, error)
	quiesce func(context.Context, corestore.QuiesceOptions) (corestore.Inspection, error)
	after   func(string) error
}

func productionCoreSchemaUpgradeOps() coreSchemaUpgradeOps {
	return coreSchemaUpgradeOps{
		inspect: corestore.Inspect,
		prepare: corestore.PrepareUpgrade,
		quiesce: corestore.QuiesceForReplacement,
	}
}

func (o coreSchemaUpgradeOps) validate() error {
	if o.inspect == nil || o.prepare == nil || o.quiesce == nil {
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
			Version:       coreSchemaUpgradeManifestVersion,
			UpgradeID:     id,
			Status:        coreSchemaUpgradePreparing,
			CreatedAt:     now.UTC(),
			SourceVersion: inspection.SchemaVersion,
			TargetVersion: inspection.TargetVersion,
			SourceHead:    inspection.Head,
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

	if manifest.CandidateHead != nil && live.SchemaVersion == manifest.TargetVersion && live.Head == *manifest.CandidateHead {
		return finalizePublishedCoreSchemaUpgrade(ctx, path, minimum, manifest, artifacts, live, ops)
	}
	if live.SchemaVersion != manifest.SourceVersion || live.Head != manifest.SourceHead {
		return nil, fmt.Errorf("schema upgrade live authority does not match recorded source or candidate")
	}
	if live.TargetVersion != manifest.TargetVersion || live.Status != corestore.InspectionUpgradeRequired {
		return nil, fmt.Errorf("schema upgrade target changed or source is not upgradeable")
	}

	if manifest.Status == coreSchemaUpgradePreparing {
		if *minimum != manifest.SourceHead {
			return nil, fmt.Errorf("preparing schema upgrade watermark does not match source head")
		}
		result, err := ops.prepare(ctx, corestore.UpgradeOptions{
			SourcePath:       path,
			BackupPath:       artifacts.backup,
			CandidatePath:    artifacts.candidate,
			MinimumHead:      authorityHeadPointer(manifest.SourceHead),
			ReplaceCandidate: true,
		})
		if err != nil {
			return nil, fmt.Errorf("prepare daemon schema upgrade: %w", err)
		}
		if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err != nil {
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
		manifest.BackupSHA256 = backupDigest
		manifest.BackupBytes = backupBytes
		manifest.CandidateSHA256 = candidateDigest
		manifest.CandidateBytes = candidateBytes
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
			MinimumHead:      authorityHeadPointer(manifest.SourceHead),
			ReplaceCandidate: true,
		})
		if prepareErr != nil {
			return nil, fmt.Errorf("rebuild unpublished schema upgrade candidate: %w", prepareErr)
		}
		if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err != nil {
			return nil, err
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
	switch *minimum {
	case manifest.SourceHead:
		if err := writeAuthorityWatermark(path+".head", candidateHead); err != nil {
			return nil, fmt.Errorf("arm upgraded authority watermark: %w", err)
		}
		if err := ops.reached(coreSchemaPhaseWatermark); err != nil {
			return nil, err
		}
	case candidateHead:
		// Crash recovery after the watermark was armed.
	default:
		return nil, fmt.Errorf("schema upgrade watermark matches neither source nor candidate head")
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
	published, err := ops.inspect(ctx, corestore.InspectOptions{Path: path, MinimumHead: authorityHeadPointer(candidateHead)})
	if err != nil {
		return nil, fmt.Errorf("verify published schema upgrade: %w", err)
	}
	if published.Status != corestore.InspectionCurrent || published.SchemaVersion != manifest.TargetVersion || published.Head != candidateHead {
		return nil, fmt.Errorf("published schema upgrade identity is invalid")
	}
	if err := ops.reached(coreSchemaPhaseVerified); err != nil {
		return nil, err
	}
	if err := removeCoreSchemaUpgradeManifest(path); err != nil {
		return nil, err
	}
	return authorityHeadPointer(candidateHead), nil
}

func finalizePublishedCoreSchemaUpgrade(
	ctx context.Context,
	databasePath string,
	minimum *corestore.AuthorityHead,
	manifest coreSchemaUpgradeManifest,
	artifacts coreSchemaUpgradeArtifacts,
	live corestore.Inspection,
	ops coreSchemaUpgradeOps,
) (*corestore.AuthorityHead, error) {
	if manifest.Status != coreSchemaUpgradeReady || manifest.CandidateHead == nil || *minimum != *manifest.CandidateHead {
		return nil, fmt.Errorf("published schema upgrade is not bound to its ready manifest and watermark")
	}
	if live.Status != corestore.InspectionCurrent || live.TargetVersion != manifest.TargetVersion {
		return nil, fmt.Errorf("published schema upgrade is not current")
	}
	backupDigest, backupBytes, err := hashPrivateUpgradeArtifact(artifacts.backup)
	if err != nil {
		return nil, fmt.Errorf("verify published schema upgrade backup: %w", err)
	}
	if backupDigest != manifest.BackupSHA256 || backupBytes != manifest.BackupBytes {
		return nil, fmt.Errorf("published schema upgrade backup fingerprint changed")
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
	if err := removeCoreSchemaUpgradeManifest(databasePath); err != nil {
		return nil, err
	}
	head := *manifest.CandidateHead
	return &head, nil
}

func verifyCoreSchemaUpgradeArtifacts(ctx context.Context, manifest coreSchemaUpgradeManifest, artifacts coreSchemaUpgradeArtifacts, ops coreSchemaUpgradeOps) error {
	backup, err := ops.inspect(ctx, corestore.InspectOptions{Path: artifacts.backup, MinimumHead: authorityHeadPointer(manifest.SourceHead)})
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
	candidate, err := ops.inspect(ctx, corestore.InspectOptions{Path: artifacts.candidate, MinimumHead: manifest.CandidateHead})
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
	want := manifest.SourceHead
	want.HeadGeneration++
	if result.Candidate.Status != corestore.InspectionCurrent || result.Candidate.SchemaVersion != manifest.TargetVersion || result.Candidate.Head != want || result.Candidate.Path != artifacts.candidate {
		return fmt.Errorf("prepared schema upgrade candidate did not preserve authority continuity")
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
		source:    databasePath,
		backup:    filepath.Join(parent, "backups", label+".db"),
		candidate: filepath.Join(parent, "."+label+".candidate"),
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
	if manifest.Version != coreSchemaUpgradeManifestVersion || !validCoreSchemaUpgradeID(manifest.UpgradeID) || manifest.CreatedAt.IsZero() {
		return fmt.Errorf("schema upgrade manifest identity is invalid")
	}
	if manifest.SourceVersion < 1 || manifest.TargetVersion <= manifest.SourceVersion || !validAuthorityHead(manifest.SourceHead) {
		return fmt.Errorf("schema upgrade manifest source is invalid")
	}
	switch manifest.Status {
	case coreSchemaUpgradePreparing:
		if manifest.CandidateHead != nil || manifest.BackupSHA256 != "" || manifest.BackupBytes != 0 || manifest.CandidateSHA256 != "" || manifest.CandidateBytes != 0 {
			return fmt.Errorf("preparing schema upgrade manifest contains ready artifacts")
		}
	case coreSchemaUpgradeReady:
		if manifest.CandidateHead == nil || !validAuthorityHead(*manifest.CandidateHead) || !validSHA256Hex(manifest.BackupSHA256) || manifest.BackupBytes <= 0 || !validSHA256Hex(manifest.CandidateSHA256) || manifest.CandidateBytes <= 0 {
			return fmt.Errorf("ready schema upgrade manifest is incomplete")
		}
		want := manifest.SourceHead
		want.HeadGeneration++
		if *manifest.CandidateHead != want {
			return fmt.Errorf("schema upgrade manifest candidate head breaks authority continuity")
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
