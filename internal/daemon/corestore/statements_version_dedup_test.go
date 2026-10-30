package corestore

import (
	"crypto/sha256"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedDuplicateStatementVersions writes, the way the pre-v9 refresh did, one
// version copy per report for rows whose content never changed. t2 is
// restated once, so it keeps two distinct versions.
func seedDuplicateStatementVersions(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	stamp := formatTime(time.Date(2026, time.October, 29, 6, 0, 0, 0, time.UTC))
	effective := formatTime(time.Date(2026, time.October, 27, 15, 0, 0, 0, time.UTC))
	files := []string{"flex-a.xml", "flex-b.xml", "flex-c.xml"}
	digest := func(file string) []byte { sum := sha256.Sum256([]byte(file)); return sum[:] }
	raw := func(s string) []byte { sum := sha256.Sum256([]byte(s)); return sum[:] }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, file := range files {
		exec(`INSERT INTO statement_file_versions(scope_key,file_key,sha256,size_bytes,status,recorded_at) VALUES('statements',?,?,1,'valid',?)`, file, digest(file), stamp)
	}
	for _, file := range files[1:] {
		exec(`INSERT INTO statement_files(scope_key,file_key,size_bytes,sha256,status,updated_at) VALUES('statements',?,1,?,'valid',?)`, file, digest(file), stamp)
	}
	record := func(key, file, payload string) {
		exec(`INSERT INTO statement_record_versions(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES('statements','trade',?,'DUEDGE',?,?,?,?,?,?,?)`, key, effective, file, digest(file), stamp, []byte(payload), raw(payload), stamp)
	}
	metadata := func(file, payload string) {
		exec(`INSERT INTO statement_metadata_versions(scope_key,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES('statements','statement/DUEDGE','DUEDGE',?,?,?,?,?,?,?)`, effective, file, digest(file), stamp, []byte(payload), raw(payload), stamp)
	}
	day := func(file string) {
		exec(`INSERT INTO statement_equity_day_versions(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES('statements','DUEDGE','2026-10-27','1',?,?,?,?,?,?)`, file, digest(file), stamp, []byte(`{"version":1}`), raw(`{"version":1}`), stamp)
	}
	for _, file := range files {
		record("t1", file, `{"p":1}`)
		metadata(file, `{"to":"2026-10-27"}`)
		day(file)
	}
	record("t2", "flex-a.xml", `{"p":2}`)
	record("t2", "flex-b.xml", `{"p":20}`)
	record("t2", "flex-c.xml", `{"p":20}`)
	exec(`INSERT INTO statement_records(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES('statements','trade','t1','DUEDGE',?,'flex-b.xml',?,?,CAST('{"p":1}' AS BLOB),?)`, effective, digest("flex-b.xml"), stamp, stamp)
	exec(`INSERT INTO statement_records(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES('statements','trade','t2','DUEDGE',?,'flex-c.xml',?,?,CAST('{"p":20}' AS BLOB),?)`, effective, digest("flex-c.xml"), stamp, stamp)
	exec(`INSERT INTO statement_metadata(scope_key,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES('statements','statement/DUEDGE','DUEDGE',?,'flex-c.xml',?,?,CAST('{"to":"2026-10-27"}' AS BLOB),?)`, effective, digest("flex-c.xml"), stamp, stamp)
	exec(`INSERT INTO statement_equity_days(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES('statements','DUEDGE','2026-10-27','1','flex-b.xml',?,?,CAST('{"version":1}' AS BLOB),?)`, digest("flex-b.xml"), stamp, stamp)
}

func createStatementAuthority(t *testing.T, version int, seed bool) (string, AuthorityHead) {
	t.Helper()
	dir := privateTempDir(t)
	path := filepath.Join(dir, "daemon.db")
	store, err := openWithPlan(t.Context(), Options{Path: path}, currentMigrationPlan()[:version])
	if err != nil {
		t.Fatalf("create v%d authority: %v", version, err)
	}
	if seed {
		seedDuplicateStatementVersions(t, store)
	}
	head, err := readAuthorityHead(t.Context(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, head
}

func prepareStatementDedupCandidate(t *testing.T, path string) UpgradeResult {
	t.Helper()
	dir := filepath.Dir(path)
	result, err := PrepareUpgrade(t.Context(), UpgradeOptions{
		SourcePath: path, BackupPath: filepath.Join(dir, "daemon.backup.db"),
		CandidatePath: filepath.Join(dir, "daemon.candidate.db"),
	})
	if err != nil {
		t.Fatalf("prepare v%d candidate: %v", statementVersionDedupMigrationVersion, err)
	}
	if result.Candidate.SchemaVersion != statementVersionDedupMigrationVersion || result.Candidate.Status != InspectionCurrent {
		t.Fatalf("candidate=%+v", result.Candidate)
	}
	return result
}

func TestStatementVersionDedupMigrationKeepsEarliestDistinctVersions(t *testing.T) {
	ctx := t.Context()
	path, head := createStatementAuthority(t, statementVersionDedupMigrationVersion-1, true)
	result := prepareStatementDedupCandidate(t, path)

	if len(result.Maintenance.StatementVersionDedups) != 1 {
		t.Fatalf("statement dedup summaries=%+v", result.Maintenance.StatementVersionDedups)
	}
	summary := result.Maintenance.StatementVersionDedups[0]
	if summary.MigrationVersion != statementVersionDedupMigrationVersion || summary.Selector != statementVersionDedupSelector ||
		summary.RemovedEquityDayRows != 2 || summary.RemovedRecordRows != 3 || summary.RemovedMetadataRows != 2 ||
		summary.RemovedPayloadBytes <= 0 || len(summary.OrderedDigestSHA256) != 64 {
		t.Fatalf("statement dedup summary=%+v", summary)
	}
	if !result.Maintenance.Compacted || !result.Maintenance.SourceBackupRetirementRequired {
		t.Fatalf("maintenance flags=%+v", result.Maintenance)
	}

	recomputed, err := RecomputeUpgradeMaintenance(ctx, RecomputeUpgradeMaintenanceOptions{
		SourcePath: result.Backup.Path, ExpectedSchemaVersion: statementVersionDedupMigrationVersion - 1, ExpectedHead: head,
	})
	if err != nil {
		t.Fatalf("recompute from source backup: %v", err)
	}
	if len(recomputed.StatementVersionDedups) != 1 || recomputed.StatementVersionDedups[0] != summary {
		t.Fatalf("recomputed=%+v want %+v", recomputed.StatementVersionDedups, summary)
	}

	candidate, err := Open(ctx, Options{Path: result.Candidate.Path})
	if err != nil {
		t.Fatalf("open candidate: %v", err)
	}
	defer candidate.Close()
	days, records, metas := statementVersionCounts(t, candidate)
	if days != 1 || records != 3 || metas != 1 {
		t.Fatalf("candidate versions days=%d records=%d metadata=%d want 1/3/1", days, records, metas)
	}
	rows, err := candidate.db.QueryContext(ctx, `SELECT record_key, statement_file_key, CAST(raw_json AS TEXT) FROM statement_record_versions ORDER BY record_version_id`)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for rows.Next() {
		var key, file, payload string
		if err := rows.Scan(&key, &file, &payload); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, key+" "+file+" "+payload)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{`t1 flex-a.xml {"p":1}`, `t2 flex-a.xml {"p":2}`, `t2 flex-b.xml {"p":20}`}
	if len(kept) != len(want) {
		t.Fatalf("kept versions=%q want %q", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept versions=%q want %q", kept, want)
		}
	}
	for _, check := range []struct{ label, query string }{
		{"record", `SELECT count(*) FROM statement_records c WHERE NOT EXISTS (SELECT 1 FROM statement_record_versions v
 WHERE v.scope_key=c.scope_key AND v.record_kind=c.record_kind AND v.record_key=c.record_key AND v.account_key=c.account_key AND v.effective_at=c.effective_at AND v.raw_json=c.raw_json)`},
		{"metadata", `SELECT count(*) FROM statement_metadata c WHERE NOT EXISTS (SELECT 1 FROM statement_metadata_versions v
 WHERE v.scope_key=c.scope_key AND v.record_key=c.record_key AND v.account_key=c.account_key AND v.effective_at=c.effective_at AND v.raw_json=c.raw_json)`},
		{"equity day", `SELECT count(*) FROM statement_equity_days c WHERE NOT EXISTS (SELECT 1 FROM statement_equity_day_versions v
 WHERE v.scope_key=c.scope_key AND v.account_key=c.account_key AND v.day=c.day AND v.equity_base_text=c.equity_base_text AND v.raw_json=c.raw_json)`},
	} {
		var orphans int
		if err := candidate.db.QueryRowContext(ctx, check.query).Scan(&orphans); err != nil {
			t.Fatalf("%s resolution: %v", check.label, err)
		}
		if orphans != 0 {
			t.Fatalf("%d current %s rows have no stored version after dedup", orphans, check.label)
		}
	}
	current, err := candidate.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil || len(current) != 3 {
		t.Fatalf("current records after dedup=%d err=%v", len(current), err)
	}

	// Running the dedup again removes nothing, and the append-only guards are
	// back in place.
	tx, err := candidate.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range statementVersionDedupStatements() {
		result, err := tx.ExecContext(ctx, statement)
		if err != nil {
			t.Fatalf("re-run %q: %v", statement, err)
		}
		if affected, _ := result.RowsAffected(); strings.HasPrefix(statement, "DELETE") && affected != 0 {
			t.Fatalf("re-run removed %d rows: %s", affected, statement)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := verifyStatementVersionDedupAbsent(ctx, candidate.db, statementVersionDedupSelector); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"statement_equity_day_versions", "statement_record_versions", "statement_metadata_versions"} {
		if _, err := candidate.db.ExecContext(ctx, `DELETE FROM `+table); err == nil {
			t.Fatalf("%s accepted a delete after the migration", table)
		}
	}
}

func TestStatementVersionDedupMigrationOnEmptyAndOldStores(t *testing.T) {
	ctx := t.Context()
	for _, version := range []int{6, statementVersionDedupMigrationVersion - 1} {
		path, head := createStatementAuthority(t, version, false)
		result := prepareStatementDedupCandidate(t, path)
		if len(result.Maintenance.StatementVersionDedups) != 1 {
			t.Fatalf("v%d statement dedup summaries=%+v", version, result.Maintenance.StatementVersionDedups)
		}
		summary := result.Maintenance.StatementVersionDedups[0]
		if summary.RemovedEquityDayRows != 0 || summary.RemovedRecordRows != 0 || summary.RemovedMetadataRows != 0 || summary.RemovedPayloadBytes != 0 {
			t.Fatalf("v%d empty store summary=%+v", version, summary)
		}
		if result.Maintenance.SourceBackupRetirementRequired {
			t.Fatalf("v%d empty store asked to retire its source backup", version)
		}
		recomputed, err := RecomputeUpgradeMaintenance(ctx, RecomputeUpgradeMaintenanceOptions{
			SourcePath: result.Backup.Path, ExpectedSchemaVersion: version, ExpectedHead: head,
		})
		if err != nil {
			t.Fatalf("v%d recompute: %v", version, err)
		}
		if len(recomputed.StatementVersionDedups) != 1 || recomputed.StatementVersionDedups[0] != summary {
			t.Fatalf("v%d recomputed=%+v want %+v", version, recomputed.StatementVersionDedups, summary)
		}
		candidate, err := Open(ctx, Options{Path: result.Candidate.Path})
		if err != nil {
			t.Fatalf("v%d open candidate: %v", version, err)
		}
		if err := candidate.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
