package corestore

import (
	"crypto/sha256"
	"testing"
	"time"
)

// statementVersionCounts returns the rows in each statement version table.
func statementVersionCounts(t *testing.T, store *Store) (days, records, metadata int) {
	t.Helper()
	for _, count := range []struct {
		table string
		out   *int
	}{{"statement_equity_day_versions", &days}, {"statement_record_versions", &records}, {"statement_metadata_versions", &metadata}} {
		if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+count.table).Scan(count.out); err != nil {
			t.Fatal(err)
		}
	}
	return days, records, metadata
}

// The daily Flex report covers the year to date, so each new file repeats
// almost every row of the one before. A repeat with identical content is not
// a new version; only the first report that showed it is kept.
func TestStatementRefreshStoresOnlyFirstSeenVersions(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := t.Context()
	generated := time.Date(2026, time.October, 28, 6, 0, 0, 0, time.UTC)
	fileA := StatementFileRecord{FileKey: "flex-2026-10-28.xml", SHA256: sha256.Sum256([]byte("a")), Status: "valid"}
	fileB := StatementFileRecord{FileKey: "flex-2026-10-29.xml", SHA256: sha256.Sum256([]byte("b")), Status: "valid"}
	trade := func(key string, file StatementFileRecord, generated time.Time, raw string) StatementRecord {
		return StatementRecord{Kind: StatementRecordTrade, RecordKey: key, AccountKey: "DUEDGE", EffectiveAt: time.Date(2026, time.October, 27, 15, 0, 0, 0, time.UTC),
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(raw)}
	}
	day := func(file StatementFileRecord, generated time.Time) StatementEquityDayRecord {
		return StatementEquityDayRecord{AccountKey: "DUEDGE", Day: "2026-10-27", EquityBaseText: "1",
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(`{"version":1}`)}
	}
	metadata := func(file StatementFileRecord, generated time.Time, raw string) StatementRecord {
		return StatementRecord{Kind: StatementRecordMetadata, RecordKey: "statement/DUEDGE", AccountKey: "DUEDGE", EffectiveAt: generated,
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(raw)}
	}

	first := []StatementRecord{trade("t1", fileA, generated, `{"p":1}`), trade("t2", fileA, generated, `{"p":2}`), metadata(fileA, generated, `{"to":"2026-10-27"}`)}
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileA}, []StatementEquityDayRecord{day(fileA, generated)}, first, nil); err != nil {
		t.Fatal(err)
	}
	days, records, metas := statementVersionCounts(t, store)
	if days != 1 || records != 2 || metas != 1 {
		t.Fatalf("first refresh versions days=%d records=%d metadata=%d want 1/2/1", days, records, metas)
	}

	// The next report repeats t1 and t2 unchanged and the day unchanged. The
	// current winners stay on file A, but every typed row of both files is
	// offered as a version, newest file first, as the daemon projection does.
	next := generated.Add(24 * time.Hour)
	current := []StatementRecord{trade("t1", fileA, generated, `{"p":1}`), trade("t2", fileA, generated, `{"p":2}`), metadata(fileB, next, `{"to":"2026-10-28"}`)}
	versions := []StatementRecord{
		trade("t1", fileB, next, `{"p":1}`), trade("t2", fileB, next, `{"p":2}`), metadata(fileB, next, `{"to":"2026-10-28"}`),
		trade("t1", fileA, generated, `{"p":1}`), trade("t2", fileA, generated, `{"p":2}`), metadata(fileA, generated, `{"to":"2026-10-27"}`),
	}
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileA, fileB}, []StatementEquityDayRecord{day(fileA, generated)}, current, versions); err != nil {
		t.Fatal(err)
	}
	days, records, metas = statementVersionCounts(t, store)
	if days != 1 || records != 2 || metas != 2 {
		t.Fatalf("identical repeat versions days=%d records=%d metadata=%d want 1/2/2", days, records, metas)
	}

	// File A leaves retention: its rows' winners move to file B with the same
	// content, which is still not a new version.
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileB},
		[]StatementEquityDayRecord{day(fileB, next)}, []StatementRecord{trade("t1", fileB, next, `{"p":1}`), trade("t2", fileB, next, `{"p":2}`), metadata(fileB, next, `{"to":"2026-10-28"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	days, records, metas = statementVersionCounts(t, store)
	if days != 1 || records != 2 || metas != 2 {
		t.Fatalf("winner moved to a newer file versions days=%d records=%d metadata=%d want 1/2/2", days, records, metas)
	}
	var keptFile string
	if err := store.db.QueryRowContext(ctx, `SELECT statement_file_key FROM statement_record_versions WHERE record_key='t1'`).Scan(&keptFile); err != nil {
		t.Fatal(err)
	}
	if keptFile != fileA.FileKey {
		t.Fatalf("kept version came from %s want the first report %s", keptFile, fileA.FileKey)
	}

	// A restatement with different content is always a new version.
	fileC := StatementFileRecord{FileKey: "flex-2026-10-30.xml", SHA256: sha256.Sum256([]byte("c")), Status: "valid"}
	later := next.Add(24 * time.Hour)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{fileB, fileC},
		[]StatementEquityDayRecord{func() StatementEquityDayRecord { d := day(fileC, later); d.EquityBaseText = "2"; return d }()},
		[]StatementRecord{trade("t1", fileC, later, `{"p":10}`), trade("t2", fileB, next, `{"p":2}`), metadata(fileC, later, `{"to":"2026-10-29"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	days, records, metas = statementVersionCounts(t, store)
	if days != 2 || records != 3 || metas != 3 {
		t.Fatalf("restatement versions days=%d records=%d metadata=%d want 2/3/3", days, records, metas)
	}
}

// Within one refresh the first report that showed a row is the one kept,
// whatever order the caller offers the copies in.
func TestStatementRefreshKeepsEarliestReportCopy(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := t.Context()
	generated := time.Date(2026, time.October, 28, 6, 0, 0, 0, time.UTC)
	older := StatementFileRecord{FileKey: "flex-2026-10-28.xml", SHA256: sha256.Sum256([]byte("old")), Status: "valid"}
	newer := StatementFileRecord{FileKey: "flex-2026-10-29.xml", SHA256: sha256.Sum256([]byte("new")), Status: "valid"}
	trade := func(file StatementFileRecord, generated time.Time) StatementRecord {
		return StatementRecord{Kind: StatementRecordTrade, RecordKey: "t1", AccountKey: "DUEDGE", EffectiveAt: generated.Add(-time.Hour),
			StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256, GeneratedAt: generated, RawJSON: []byte(`{"p":1}`)}
	}
	newest := trade(newer, generated.Add(24*time.Hour))
	newest.EffectiveAt = generated.Add(-time.Hour)
	if err := store.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{older, newer}, nil,
		[]StatementRecord{trade(older, generated)}, []StatementRecord{newest, trade(older, generated)}); err != nil {
		t.Fatal(err)
	}
	var keptFile string
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT min(statement_file_key), count(*) FROM statement_record_versions`).Scan(&keptFile, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || keptFile != older.FileKey {
		t.Fatalf("versions=%d kept=%s want 1 from %s", rows, keptFile, older.FileKey)
	}
}
