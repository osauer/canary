package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func statementDedupTestSummary(removed int64) corestore.StatementVersionDedupSummary {
	summary := corestore.StatementVersionDedupSummary{
		MigrationVersion:    statementVersionDedupMigrationVersion,
		MigrationName:       statementVersionDedupMigrationName,
		Selector:            statementVersionDedupReceiptSelector,
		RemovedRecordRows:   removed,
		OrderedDigestSHA256: strings.Repeat("a", 64),
	}
	if removed > 0 {
		summary.RemovedPayloadBytes = 10 * removed
	}
	return summary
}

// The v9 statement-version dedup binds into the upgrade manifest and receipt
// like the earlier repairs, and an upgrade spanning several maintenance
// migrations carries one summary for each.
func TestCoreSchemaStatementDedupMaintenanceBindsToItsMigration(t *testing.T) {
	removing := corestore.UpgradeResult{Maintenance: corestore.UpgradeMaintenanceResult{
		StatementVersionDedups:         []corestore.StatementVersionDedupSummary{statementDedupTestSummary(3)},
		Compacted:                      true,
		SourceBackupRetirementRequired: true,
	}}
	bound, err := coreSchemaUpgradeMaintenanceFromResult(removing)
	if err != nil || bound == nil || bound.StatementVersionDedup == nil || !bound.RetireSourceBackup {
		t.Fatalf("bound=%+v err=%v", bound, err)
	}
	if !coreSchemaUpgradeMaintenanceMatchesTarget(*bound, 8, 9) {
		t.Fatal("v8 to v9 dedup did not match its migration")
	}
	if coreSchemaUpgradeMaintenanceMatchesTarget(*bound, 5, 9) {
		t.Fatal("v5 to v9 accepted maintenance without the v6 operational prune summary")
	}
	if coreSchemaUpgradeMaintenanceMatchesTarget(*bound, 8, 8) {
		t.Fatal("dedup summary matched an upgrade that does not run migration 9")
	}

	prune := corestore.OperationalPruneSummary{
		MigrationVersion: betaOperationalPruneMigrationVersion, MigrationName: betaOperationalPruneMigrationName,
		Selector: betaOperationalPruneReceiptSelector, ObservationDigestSHA256: strings.Repeat("b", 64), EventDigestSHA256: strings.Repeat("c", 64),
	}
	spanning := coreSchemaUpgradeMaintenance{OperationalPrune: &prune, StatementVersionDedup: bound.StatementVersionDedup, Compacted: true, RetireSourceBackup: true}
	if err := validateCoreSchemaUpgradeMaintenance(spanning); err != nil {
		t.Fatal(err)
	}
	if !coreSchemaUpgradeMaintenanceMatchesTarget(spanning, 5, 9) {
		t.Fatal("v5 to v9 did not accept both pending repairs")
	}
	if coreSchemaUpgradeMaintenanceMatchesTarget(coreSchemaUpgradeMaintenance{OperationalPrune: &prune, Compacted: true}, 5, 9) {
		t.Fatal("v5 to v9 accepted maintenance without the v9 dedup summary")
	}

	empty, err := coreSchemaUpgradeMaintenanceFromResult(corestore.UpgradeResult{Maintenance: corestore.UpgradeMaintenanceResult{
		StatementVersionDedups: []corestore.StatementVersionDedupSummary{statementDedupTestSummary(0)}, Compacted: true,
	}})
	if err != nil || empty == nil || empty.RetireSourceBackup {
		t.Fatalf("empty store dedup bound=%+v err=%v", empty, err)
	}
	wrong := statementDedupTestSummary(3)
	wrong.Selector.Predicate = "other"
	if _, err := coreSchemaUpgradeMaintenanceFromResult(corestore.UpgradeResult{Maintenance: corestore.UpgradeMaintenanceResult{
		StatementVersionDedups: []corestore.StatementVersionDedupSummary{wrong}, Compacted: true, SourceBackupRetirementRequired: true,
	}}); err == nil {
		t.Fatal("accepted a dedup summary with an unknown selector")
	}

	head := corestore.AuthorityHead{AuthorityEpoch: strings.Repeat("e", 32), HeadGeneration: 2, LastEventSeq: 1, SignerGeneration: 1}
	candidate := head
	candidate.HeadGeneration++
	manifest := coreSchemaUpgradeManifest{
		UpgradeID: strings.Repeat("1", 24), SourceVersion: 8, TargetVersion: 9, SourceHead: head, CandidateHead: &candidate,
		BackupSHA256: strings.Repeat("d", 64), BackupBytes: 100, TargetBackupSHA256: strings.Repeat("f", 64), TargetBackupBytes: 50,
		Maintenance: bound,
	}
	receipt := coreSchemaMaintenanceReceiptFromManifest(manifest)
	if receipt.Version != coreSchemaMaintenanceReceiptVersion || receipt.StatementVersionDedup == nil {
		t.Fatalf("v9 receipt=%+v", receipt)
	}
	path := filepath.Join(t.TempDir(), "backups", "receipt.json")
	if err := writeCoreSchemaMaintenanceReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := loadCoreSchemaMaintenanceReceipt(path)
	if err != nil || !ok || !equalCoreSchemaMaintenanceReceipt(loaded, receipt) {
		t.Fatalf("loaded receipt=%+v ok=%v err=%v", loaded, ok, err)
	}
	altered := receipt
	alteredDedup := *receipt.StatementVersionDedup
	alteredDedup.RemovedRecordRows++
	altered.StatementVersionDedup = &alteredDedup
	if equalCoreSchemaMaintenanceReceipt(altered, receipt) {
		t.Fatal("receipt equality ignored the dedup summary")
	}

	// A v6 receipt written before the dedup existed keeps its version.
	manifest.SourceVersion, manifest.TargetVersion = 5, 6
	manifest.Maintenance = &coreSchemaUpgradeMaintenance{OperationalPrune: &prune, Compacted: true, RetireSourceBackup: true}
	if got := coreSchemaMaintenanceReceiptFromManifest(manifest).Version; got != coreSchemaMaintenanceReceiptOperationVersion {
		t.Fatalf("v6 receipt version=%d want %d", got, coreSchemaMaintenanceReceiptOperationVersion)
	}
}
