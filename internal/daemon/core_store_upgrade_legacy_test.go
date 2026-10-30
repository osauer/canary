package daemon

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

// A complete schema-4 authority can skip releases and upgrade directly to
// schema 8. The pending maintenance runs at versions 5 and 6, followed by
// additive migrations; the final schema need not itself be a maintenance
// version. Zero removed rows still require both typed maintenance summaries.
func TestCoreSchemaUpgradeAcceptsMaintenanceBeforeAdditiveMigrations(t *testing.T) {
	dir := t.TempDir()
	artifacts := coreSchemaUpgradeArtifacts{
		source: filepath.Join(dir, "daemon.db"), backup: filepath.Join(dir, "source.db"),
		candidate: filepath.Join(dir, "candidate.db"),
	}
	head := corestore.AuthorityHead{AuthorityEpoch: "ffeeddccbbaa99887766554433221100", HeadGeneration: 12, LastEventSeq: 91, SignerGeneration: 4}
	candidateHead := head
	candidateHead.HeadGeneration++
	manifest := coreSchemaUpgradeManifest{
		Version: coreSchemaUpgradeManifestVersion, Status: coreSchemaUpgradePreparing,
		SourceVersion: 4, TargetVersion: 8, SourceHead: head,
		HeadTransition: corestore.UpgradeHeadTransitionAdvanceOnce,
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(nil))
	result := corestore.UpgradeResult{
		Source: corestore.Inspection{Path: artifacts.source, SchemaVersion: 4, TargetVersion: 8, Head: head},
		Backup: corestore.BackupInfo{Path: artifacts.backup, SchemaVersion: 4, Head: head},
		Candidate: corestore.Inspection{Path: artifacts.candidate, Status: corestore.InspectionCurrent,
			SchemaVersion: 8, Head: candidateHead},
		HeadTransition: corestore.UpgradeHeadTransitionAdvanceOnce,
		Maintenance: corestore.UpgradeMaintenanceResult{
			EventDiscards: []corestore.EventDiscardSummary{{
				MigrationVersion: alertEpisodePruneMigrationVersion, MigrationName: alertEpisodePruneMigrationName,
				Selector: alertEpisodePruneSelector, OrderedDigestSHA256: digest,
			}},
			OperationalPrunes: []corestore.OperationalPruneSummary{{
				MigrationVersion: betaOperationalPruneMigrationVersion, MigrationName: betaOperationalPruneMigrationName,
				Selector: betaOperationalPruneReceiptSelector, ObservationDigestSHA256: digest, EventDigestSHA256: digest,
			}},
			Compacted: true,
		},
	}
	if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err != nil {
		t.Fatalf("supported schema-4 upgrade through maintenance to schema 8 was refused: %v", err)
	}
	result.Maintenance.EventDiscards = nil
	if err := validatePreparedCoreSchemaUpgrade(manifest, artifacts, result); err == nil {
		t.Fatal("accepted an upgrade without evidence for the pending event-prune migration")
	}
}
