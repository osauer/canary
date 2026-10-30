package corestore

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStatementRecordMigrationV6ToV7Candidate(t *testing.T) {
	ctx := t.Context()
	plan := currentMigrationPlan()
	if len(plan) != 9 {
		t.Fatalf("current schema version=%d want 9", len(plan))
	}
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon-v6.db")
	old, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:6])
	if err != nil {
		t.Fatalf("create v6 authority: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close v6 authority: %v", err)
	}

	if _, err := Open(ctx, Options{Path: sourcePath}); err == nil {
		t.Fatal("current store opened v6 authority without an explicit upgrade")
	} else {
		var upgrade *UpgradeRequiredError
		if !errors.As(err, &upgrade) || upgrade.CurrentVersion != 6 || upgrade.TargetVersion != 9 {
			t.Fatalf("open v6 error=%v", err)
		}
	}

	result, err := PrepareUpgrade(ctx, UpgradeOptions{
		SourcePath:    sourcePath,
		BackupPath:    filepath.Join(dir, "daemon-v6.backup.db"),
		CandidatePath: filepath.Join(dir, "daemon-v7.candidate.db"),
		TargetVersion: 7,
	})
	if err != nil {
		t.Fatalf("prepare v7 candidate: %v", err)
	}
	if result.Source.SchemaVersion != 6 || result.Candidate.SchemaVersion != 7 || result.Candidate.Status != InspectionCurrent {
		t.Fatalf("upgrade result source=%+v candidate=%+v", result.Source, result.Candidate)
	}

	candidate, err := openWithPlan(ctx, Options{Path: result.Candidate.Path}, plan[:7])
	if err != nil {
		t.Fatalf("open v7 candidate: %v", err)
	}
	defer candidate.Close()
	for _, table := range []string{"statement_records", "statement_record_versions"} {
		var count int
		if err := candidate.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("candidate table %s count=%d want 1", table, count)
		}
	}
}

func TestStatementMetadataMigrationV7ToV8Candidate(t *testing.T) {
	ctx := t.Context()
	plan := currentMigrationPlan()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon-v7.db")
	old, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:7])
	if err != nil {
		t.Fatalf("create v7 authority: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close v7 authority: %v", err)
	}

	result, err := PrepareUpgrade(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: filepath.Join(dir, "daemon-v7.backup.db"),
		CandidatePath: filepath.Join(dir, "daemon-v8.candidate.db"), TargetVersion: 8,
	})
	if err != nil {
		t.Fatalf("prepare v8 candidate: %v", err)
	}
	if result.Source.SchemaVersion != 7 || result.Candidate.SchemaVersion != 8 || result.Candidate.Status != InspectionCurrent {
		t.Fatalf("upgrade result source=%+v candidate=%+v", result.Source, result.Candidate)
	}
	candidate, err := openWithPlan(ctx, Options{Path: result.Candidate.Path}, plan[:8])
	if err != nil {
		t.Fatalf("open v8 candidate: %v", err)
	}
	defer candidate.Close()
	for _, table := range []string{"statement_metadata", "statement_metadata_versions"} {
		var count int
		if err := candidate.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("candidate table %s count=%d want 1", table, count)
		}
	}
}

func TestStatementRecordProjectionRetainsRestatementsAndRetractsCurrentRows(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := t.Context()
	generated := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)

	file := StatementFileRecord{
		FileKey: "flex-2026-08-24.xml",
		SHA256:  sha256.Sum256([]byte("statement-v1")),
		Status:  "valid",
	}
	record := StatementRecord{
		Kind:                StatementRecordTrade,
		RecordKey:           "trade_opaque_1",
		AccountKey:          "DUEDGE",
		EffectiveAt:         generated.Add(-time.Hour),
		StatementFileKey:    file.FileKey,
		StatementFileSHA256: file.SHA256,
		GeneratedAt:         generated,
		RawJSON:             []byte(`{"price":100}`),
	}
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, nil, []StatementRecord{record}, nil); err != nil {
		t.Fatal(err)
	}

	file.SHA256 = sha256.Sum256([]byte("statement-v2"))
	record.StatementFileSHA256 = file.SHA256
	record.GeneratedAt = generated.Add(time.Hour)
	record.RawJSON = []byte(`{"price":101}`)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, nil, []StatementRecord{record}, nil); err != nil {
		t.Fatal(err)
	}

	current, err := store.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || string(current[0].RawJSON) != `{"price":101}` || current[0].StatementFileSHA256 != file.SHA256 {
		t.Fatalf("current restatement winner = %+v", current)
	}
	var versions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM statement_record_versions WHERE scope_key='statements'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("record versions=%d want 2", versions)
	}

	bad := record
	bad.RawJSON = []byte(`not json`)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, nil, []StatementRecord{bad}, nil); err == nil {
		t.Fatal("invalid candidate projection was accepted")
	}
	current, err = store.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || string(current[0].RawJSON) != `{"price":101}` {
		t.Fatalf("failed candidate changed current projection: %+v", current)
	}

	if err := store.ReplaceStatementProjection(ctx, "statements", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	current, err = store.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("removed source left %d current records", len(current))
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM statement_record_versions WHERE scope_key='statements'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("retraction changed immutable versions: %d", versions)
	}
}

func TestStatementMetadataProjectionIsCurrentAndVersioned(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := t.Context()
	generated := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	file := StatementFileRecord{FileKey: "flex.xml", SHA256: sha256.Sum256([]byte("v1")), Status: "valid"}
	metadata := StatementRecord{
		Kind: StatementRecordMetadata, RecordKey: "period_opaque", AccountKey: "DUEDGE", EffectiveAt: generated,
		StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated,
		RawJSON: []byte(`{"version":3,"from_date":"2026-07-01T00:00:00Z","to_date":"2026-08-24T00:00:00Z"}`),
	}
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, nil, []StatementRecord{metadata}, nil); err != nil {
		t.Fatal(err)
	}
	file.SHA256 = sha256.Sum256([]byte("v2"))
	metadata.StatementFileSHA256 = file.SHA256
	metadata.GeneratedAt = generated.Add(time.Hour)
	metadata.RawJSON = []byte(`{"version":3,"from_date":"2026-07-01T00:00:00Z","to_date":"2026-08-24T00:00:00Z","coverage":[]}`)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, nil, []StatementRecord{metadata}, nil); err != nil {
		t.Fatal(err)
	}
	current, err := store.LoadStatementRecords(ctx, "statements", []string{StatementRecordMetadata}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || string(current[0].RawJSON) != string(metadata.RawJSON) {
		t.Fatalf("current metadata winner=%+v", current)
	}
	var versions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM statement_metadata_versions WHERE scope_key='statements'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("metadata versions=%d want 2", versions)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM statement_metadata_versions`); err == nil {
		t.Fatal("append-only metadata versions accepted delete")
	}
}

func TestStatementRecordVersionsAreAppendOnly(t *testing.T) {
	store, _ := openTestStore(t)
	generated := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	file := StatementFileRecord{FileKey: "flex.xml", SHA256: sha256.Sum256([]byte("statement")), Status: "valid"}
	record := StatementRecord{
		Kind: StatementRecordTrade, RecordKey: "trade_opaque_1", AccountKey: "DUEDGE",
		EffectiveAt: generated, StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256,
		GeneratedAt: generated, RawJSON: []byte(`{"price":100}`),
	}
	if err := store.ReplaceStatementProjection(t.Context(), "statements", []StatementFileRecord{file}, nil, []StatementRecord{record}, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE statement_record_versions SET record_key='changed'`,
		`DELETE FROM statement_record_versions`,
	} {
		if _, err := store.db.ExecContext(t.Context(), query); err == nil {
			t.Fatalf("append-only table accepted %q", query)
		}
	}
}

// A refresh rewrites only the current rows it changes. Unchanged winners keep
// their row and stamp, so a daily Flex file no longer rewrites the whole
// projection in one transaction; the visible result is the same as a full
// replacement, including a restated file and a retracted row.
func TestStatementProjectionRefreshLeavesUnchangedRowsInPlace(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := t.Context()
	generated := time.Date(2026, time.October, 29, 6, 0, 0, 0, time.UTC)
	fileA := StatementFileRecord{FileKey: "flex-a.xml", SHA256: sha256.Sum256([]byte("a-v1")), Status: "valid"}
	fileB := StatementFileRecord{FileKey: "flex-b.xml", SHA256: sha256.Sum256([]byte("b-v1")), Status: "valid"}
	trade := func(key string, file StatementFileRecord, raw string) StatementRecord {
		return StatementRecord{Kind: StatementRecordTrade, RecordKey: key, AccountKey: "DUEDGE", EffectiveAt: generated.Add(-time.Hour),
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(raw)}
	}
	day := func(account string, file StatementFileRecord, equity string) StatementEquityDayRecord {
		return StatementEquityDayRecord{AccountKey: account, Day: "2026-10-28", EquityBaseText: equity,
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(`{"version":1}`)}
	}
	first := []StatementRecord{trade("t1", fileA, `{"p":1}`), trade("t2", fileA, `{"p":2}`), trade("t3", fileA, `{"p":3}`), trade("t4", fileB, `{"p":4}`), trade("t6", fileA, `{"p":6}`)}
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileA, fileB}, []StatementEquityDayRecord{day("DUA", fileA, "1"), day("DUB", fileB, "2")}, first, nil); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var stampBefore string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM statement_records WHERE record_key='t1'`).Scan(&stampBefore); err != nil {
		t.Fatal(err)
	}

	fileB.SHA256 = sha256.Sum256([]byte("b-v2"))
	fileC := StatementFileRecord{FileKey: "flex-c.xml", SHA256: sha256.Sum256([]byte("c-v1")), Status: "valid"}
	second := []StatementRecord{trade("t1", fileA, `{"p":1}`), trade("t2", fileA, `{"p":20}`), trade("t4", fileB, `{"p":4}`), trade("t5", fileA, `{"p":5}`), trade("t6", fileC, `{"p":6}`)}
	time.Sleep(2 * time.Millisecond)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileA, fileB, fileC}, []StatementEquityDayRecord{day("DUB", fileB, "3"), day("DUA", fileA, "1")}, second, nil); err != nil {
		t.Fatal(err)
	}

	after, err := store.LoadStatementRecords(ctx, "statements", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]StatementRecord{}
	for _, record := range after {
		got[record.RecordKey] = record
	}
	if len(after) != 5 || got["t3"].RecordKey != "" || string(got["t2"].RawJSON) != `{"p":20}` || string(got["t5"].RawJSON) != `{"p":5}` || got["t4"].StatementFileSHA256 != fileB.SHA256 {
		t.Fatalf("current records after refresh = %+v", after)
	}
	ids := map[string]int64{}
	for _, record := range before {
		ids[record.RecordKey] = record.ID
	}
	if got["t1"].ID != ids["t1"] {
		t.Fatalf("unchanged record was rewritten: id %d -> %d", ids["t1"], got["t1"].ID)
	}
	if got["t6"].ID != ids["t6"] || got["t6"].StatementFileKey != fileC.FileKey || got["t6"].StatementFileSHA256 != fileC.SHA256 {
		t.Fatalf("record moving to a newer file was not updated in place: before id %d, after %+v", ids["t6"], got["t6"])
	}
	var stampAfter string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM statement_records WHERE record_key='t1'`).Scan(&stampAfter); err != nil {
		t.Fatal(err)
	}
	if stampAfter != stampBefore {
		t.Fatalf("unchanged record stamp moved: %s -> %s", stampBefore, stampAfter)
	}
	days, err := store.LoadStatementEquityDays(ctx, "statements", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].AccountKey != "DUA" || days[1].AccountKey != "DUB" || days[1].EquityBaseText != "3" || days[1].StatementFileSHA256 != fileB.SHA256 {
		t.Fatalf("current equity days after refresh = %+v", days)
	}
	files, err := store.LoadStatementFiles(ctx, "statements")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[1].SHA256 != fileB.SHA256 {
		t.Fatalf("current files after refresh = %+v", files)
	}
}
