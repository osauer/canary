package history

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// v1BaseDDL is the phase-1 ingest_sources CREATE, verbatim, so the
// migration test exercises a real v1 file rather than whatever the
// current code would produce.
const v1BaseDDL = `CREATE TABLE ingest_sources (
  source     TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,
  genesis    TEXT,
  updated_at TEXT
)`

// buildV1Fixture creates a phase-1 database at path: v1 DDL (the regime
// and rules DDL are unchanged between v1 and the current schema, so the shared builders
// are the verbatim source), user_version 1, and seeded phase-1 rows.
func buildV1Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{v1BaseDDL}
	stmts = append(stmts, regimeDDL()...)
	stmts = append(stmts, rulesDDL()...)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply v1 DDL: %v", err)
		}
	}
	seeds := []string{
		`INSERT INTO ingest_sources (source, path, offset, genesis, updated_at) VALUES ('regime', '/j/regime.jsonl', 120, 'aa', '2026-07-01T00:00:00Z')`,
		`INSERT INTO regime_decisions (src_offset, at, at_unix_ms, stage, raw_json) VALUES (0, '2026-07-01T00:00:00Z', 1, 'calm', '{"v":1}')`,
		`INSERT INTO regime_indicators (decision_id, indicator, band) VALUES (1, 'vix_term', 'green')`,
		`INSERT INTO rule_transitions (src_offset, at, at_unix_ms, rule_id, status, raw_json) VALUES (0, '2026-07-01T00:00:00Z', 1, 'r1', 'pass', '{"version":1}')`,
		`PRAGMA user_version = 1`,
	}
	for _, stmt := range seeds {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v1: %v", err)
		}
	}
}

// schemaShape captures the comparable schema identity: the sqlite_master
// name set plus PRAGMA table_info per table.
func schemaShape(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	shape := map[string][]string{}
	rows, err := s.db.Query(`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatal(err)
		}
		shape["master"] = append(shape["master"], typ+":"+name)
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		info, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var cid int
			var name, ctype string
			var notNull, pk int
			var dflt sql.NullString
			if err := info.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			shape[table] = append(shape[table], fmt.Sprintf("%d|%s|%s|%d|%s|%d", cid, name, ctype, notNull, dflt.String, pk))
		}
		if err := info.Err(); err != nil {
			t.Fatal(err)
		}
		info.Close()
	}
	return shape
}

// TestMigrateV1ToCurrentSchemaEquality is the binding migration test: a migrated v1
// file and a fresh current-schema file have identical sqlite_master name sets and
// identical per-table column layouts, phase-1 rows survive, and base is 0.
func TestMigrateV1ToCurrentSchemaEquality(t *testing.T) {
	t.Parallel()
	migratedOpts := testOptions(t)
	buildV1Fixture(t, migratedOpts.DBPath)
	migrated := openTestStore(t, migratedOpts)
	if got := userVersion(t, migrated); got != schemaVersion {
		t.Fatalf("migrated user_version = %d, want %d", got, schemaVersion)
	}

	fresh := openTestStore(t, testOptions(t))
	freshShape := schemaShape(t, fresh)
	migratedShape := schemaShape(t, migrated)
	for _, key := range slices.Sorted(maps.Keys(freshShape)) {
		if !slices.Equal(freshShape[key], migratedShape[key]) {
			t.Errorf("schema %s differs:\n fresh    %v\n migrated %v", key, freshShape[key], migratedShape[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(migratedShape)) {
		if _, ok := freshShape[key]; !ok {
			t.Errorf("migrated has extra schema object %s", key)
		}
	}

	// Phase-1 rows intact, base defaulted to 0 (logical ≡ physical).
	var offset, base int64
	var genesis string
	if err := migrated.db.QueryRow(`SELECT offset, base, genesis FROM ingest_sources WHERE source = 'regime'`).Scan(&offset, &base, &genesis); err != nil {
		t.Fatalf("read migrated bookkeeping: %v", err)
	}
	if offset != 120 || base != 0 || genesis != "aa" {
		t.Fatalf("migrated bookkeeping = offset %d base %d genesis %q, want 120/0/aa", offset, base, genesis)
	}
	var stage string
	if err := migrated.db.QueryRow(`SELECT stage FROM regime_decisions WHERE src_offset = 0`).Scan(&stage); err != nil || stage != "calm" {
		t.Fatalf("phase-1 decision row did not survive: %q %v", stage, err)
	}
	var transitions int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM rule_transitions`).Scan(&transitions); err != nil || transitions != 1 {
		t.Fatalf("phase-1 transition rows = %d (%v), want 1", transitions, err)
	}
	// The new evidence tables exist and are empty.
	for _, table := range []string{"capital_events", "risk_policy_events", "proposal_outcomes", "stress_transitions", "order_events", "rotation_log", "archive_files", "statement_files", "statement_equity_days"} {
		var n int
		if err := migrated.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Errorf("migrated table %s missing: %v", table, err)
		} else if n != 0 {
			t.Errorf("migrated table %s has %d rows, want 0", table, n)
		}
	}
}

func TestMigrateV2AddsStatementFingerprint(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildV2Fixture(t, opts.DBPath)

	migrated := openTestStore(t, opts)
	if got := userVersion(t, migrated); got != schemaVersion {
		t.Fatalf("migrated user_version = %d, want %d", got, schemaVersion)
	}
	var name, fingerprint string
	var size int64
	if err := migrated.db.QueryRow(`SELECT name, size, sha256 FROM statement_files`).Scan(&name, &size, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if name != "flex-existing.xml" || size != 42 || fingerprint != "" {
		t.Fatalf("migrated statement row = %q/%d/%q, want flex-existing.xml/42/empty fingerprint", name, size, fingerprint)
	}
}

// v3StressDDL is the schema-v3 canary-named evidence DDL, verbatim, so the
// rename migration runs against a real pre-rename file rather than against
// whatever the current builders would produce.
func v3StressDDL() []string {
	stmts := []string{
		`CREATE TABLE canary_transitions (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL, at_unix_ms INTEGER NOT NULL,
  session_key TEXT, fingerprint TEXT, account TEXT, account_mode TEXT,
  action TEXT, severity TEXT, direction TEXT, market_stage TEXT,
  portfolio_alert_relevant INTEGER,
  input_health TEXT, summary TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX canary_transitions_at  ON canary_transitions(at_unix_ms)`,
		`CREATE INDEX canary_transitions_sev ON canary_transitions(severity, at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("canary_transitions")...)
}

// buildV3Fixture creates a pre-rename database at path: every v3 table
// (the shared builders are verbatim for the tables the rename does not
// touch), the canary-named evidence table, user_version 3, and the rows
// whose survival the rename must prove — one evidence row plus the three
// bookkeeping rows that carry the source value.
func buildV3Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stmts []string
	stmts = append(stmts, baseDDL()...)
	stmts = append(stmts, bookkeepingV2DDL()...)
	stmts = append(stmts, regimeDDL()...)
	stmts = append(stmts, rulesDDL()...)
	stmts = append(stmts, capitalDDL()...)
	stmts = append(stmts, riskPolicyDDL()...)
	stmts = append(stmts, proposalOutcomesDDL()...)
	stmts = append(stmts, v3StressDDL()...)
	stmts = append(stmts, ordersDDL()...)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply v3 DDL: %v", err)
		}
	}
	seeds := []string{
		`INSERT INTO ingest_sources (source, path, offset, genesis, updated_at, base)
VALUES ('canary', '/j/canary-decisions.jsonl', 4096, 'gen-canary', '2026-07-01T00:00:00Z', 1024)`,
		`INSERT INTO archive_files (source, name, raw_bytes, gz_bytes, origin, created_at)
VALUES ('canary', 'canary-decisions-2026-05.jsonl.gz', 1024, 256, 'rotation', '2026-06-01T00:00:00Z')`,
		`INSERT INTO rotation_log (id, source, started_at, state, cut_bytes, live_size, base_before, archives_json)
VALUES (1, 'canary', '2026-06-01T00:00:00Z', 'done', 1024, 5120, 0, '[]')`,
		`INSERT INTO canary_transitions
(src_offset, at, at_unix_ms, session_key, fingerprint, account, account_mode, action, severity, direction,
 market_stage, portfolio_alert_relevant, input_health, summary, raw_json)
VALUES (2048, '2026-07-05T13:30:00.75+02:00', 1783250100750, '2026-07-05', 'sha256:pre-rename', 'ACCT', 'paper',
 'defend', 'act', 'defensive', 'confirmed_stress', 1, 'ok', 'pre-rename summary line', '{"ts":"2026-07-05T13:30:00.75+02:00"}')`,
		`PRAGMA user_version = 3`,
	}
	for _, stmt := range seeds {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v3: %v", err)
		}
	}
}

// TestMigrateV3RenamesCanaryToStress is the binding rename test: a v3 file
// comes out with the evidence table renamed, every seeded value intact, the
// persisted source value rewritten, the schema shape identical to a fresh
// file (which is what forces the derived index and trigger NAMES to follow
// the table), and the append-only triggers still aborting UPDATE and DELETE
// under the new name.
func TestMigrateV3RenamesCanaryToStress(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildV3Fixture(t, opts.DBPath)
	migrated := openTestStore(t, opts)
	if got := userVersion(t, migrated); got != schemaVersion {
		t.Fatalf("migrated user_version = %d, want %d", got, schemaVersion)
	}

	// Schema identity: a migrated v3 file and a fresh file agree on every
	// sqlite_master name and every column layout.
	fresh := openTestStore(t, testOptions(t))
	freshShape := schemaShape(t, fresh)
	migratedShape := schemaShape(t, migrated)
	for _, key := range slices.Sorted(maps.Keys(freshShape)) {
		if !slices.Equal(freshShape[key], migratedShape[key]) {
			t.Errorf("schema %s differs:\n fresh    %v\n migrated %v", key, freshShape[key], migratedShape[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(migratedShape)) {
		if _, ok := freshShape[key]; !ok {
			t.Errorf("migrated has extra schema object %s", key)
		}
	}
	if _, err := migrated.db.Exec(`SELECT 1 FROM canary_transitions`); err == nil {
		t.Error("canary_transitions still exists after the rename")
	}

	// The evidence row survived the rename with every value intact.
	var (
		srcOffset, atMS, alertRelevant             int64
		at, sessionKey, fingerprint, account, mode string
		action, severity, direction, stage         string
		inputHealth, summary, rawJSON              string
	)
	if err := migrated.db.QueryRow(`SELECT src_offset, at, at_unix_ms, session_key, fingerprint, account, account_mode,
 action, severity, direction, market_stage, portfolio_alert_relevant, input_health, summary, raw_json
FROM stress_transitions`).Scan(&srcOffset, &at, &atMS, &sessionKey, &fingerprint, &account, &mode,
		&action, &severity, &direction, &stage, &alertRelevant, &inputHealth, &summary, &rawJSON); err != nil {
		t.Fatalf("read renamed evidence row: %v", err)
	}
	if srcOffset != 2048 || at != "2026-07-05T13:30:00.75+02:00" || atMS != 1783250100750 {
		t.Errorf("row identity changed: src_offset %d at %q at_unix_ms %d", srcOffset, at, atMS)
	}
	if sessionKey != "2026-07-05" || fingerprint != "sha256:pre-rename" || account != "ACCT" || mode != "paper" {
		t.Errorf("row provenance changed: %q %q %q %q", sessionKey, fingerprint, account, mode)
	}
	if action != "defend" || severity != "act" || direction != "defensive" || stage != "confirmed_stress" {
		t.Errorf("row verdict changed: %q %q %q %q", action, severity, direction, stage)
	}
	if alertRelevant != 1 || inputHealth != "ok" || summary != "pre-rename summary line" {
		t.Errorf("row disclosure changed: %d %q %q", alertRelevant, inputHealth, summary)
	}
	if rawJSON != `{"ts":"2026-07-05T13:30:00.75+02:00"}` {
		t.Errorf("raw evidence changed: %q", rawJSON)
	}

	// The persisted source value moved, carrying its watermark: the index
	// stays attached to the journal instead of re-ingesting from zero.
	var offset, base int64
	var genesis, path string
	if err := migrated.db.QueryRow(`SELECT offset, base, genesis, path FROM ingest_sources WHERE source = 'stress'`).
		Scan(&offset, &base, &genesis, &path); err != nil {
		t.Fatalf("read renamed bookkeeping: %v", err)
	}
	if offset != 4096 || base != 1024 || genesis != "gen-canary" || path != "/j/canary-decisions.jsonl" {
		t.Errorf("bookkeeping changed: offset %d base %d genesis %q path %q", offset, base, genesis, path)
	}
	for _, table := range []string{"ingest_sources", "archive_files", "rotation_log"} {
		var stale, renamed int
		if err := migrated.db.QueryRow(fmt.Sprintf(`SELECT
 (SELECT COUNT(*) FROM %s WHERE source = 'canary'),
 (SELECT COUNT(*) FROM %s WHERE source = 'stress')`, table, table)).Scan(&stale, &renamed); err != nil {
			t.Fatalf("count %s sources: %v", table, err)
		}
		if stale != 0 || renamed != 1 {
			t.Errorf("%s source rows = %d canary / %d stress, want 0/1", table, stale, renamed)
		}
	}

	// Append-only is still enforced, under the new table name.
	for _, stmt := range []string{
		`UPDATE stress_transitions SET summary = 'tampered'`,
		`DELETE FROM stress_transitions`,
	} {
		_, err := migrated.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "stress_transitions is append-only") {
			t.Errorf("%s: err = %v, want stress_transitions append-only abort", stmt, err)
		}
	}
	if got := countRows(t, migrated, "stress_transitions"); got != 1 {
		t.Fatalf("evidence rows after refused tampering = %d, want 1", got)
	}

	// Already-migrated files are left alone: reopening re-runs migrate,
	// which returns at the version check before any delta.
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, opts)
	if got := userVersion(t, reopened); got != schemaVersion {
		t.Fatalf("reopened user_version = %d, want %d", got, schemaVersion)
	}
	if got := countRows(t, reopened, "stress_transitions"); got != 1 {
		t.Fatalf("evidence rows after reopen = %d, want 1", got)
	}
	var reopenedSource string
	if err := reopened.db.QueryRow(`SELECT source FROM ingest_sources`).Scan(&reopenedSource); err != nil {
		t.Fatal(err)
	}
	if reopenedSource != "stress" {
		t.Fatalf("source after reopen = %q, want stress", reopenedSource)
	}
}

// buildV2Fixture creates a schema-v2 database at path: the v3 fixture wound
// back to the pre-fingerprint statement_files shape. A real v2 file already
// carries the whole evidence-table set, so the fixture must too — a
// statement_files-only fiction would not exercise the rename delta that a
// v2 file also receives.
func buildV2Fixture(t *testing.T, path string) {
	t.Helper()
	buildV3Fixture(t, path)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`DROP TABLE statement_files`,
		`CREATE TABLE statement_files (
  name TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  ingested_at TEXT NOT NULL,
  equity_days INTEGER NOT NULL
)`,
		`INSERT INTO statement_files (name, size, ingested_at, equity_days)
VALUES ('flex-existing.xml', 42, '2026-07-01T00:00:00Z', 3)`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("wind fixture back to v2: %v", err)
		}
	}
}

// TestMigrateV2RenamesCanaryToStress proves the older upgrade path lands in
// the same place: a v2 file gets the statement fingerprint column AND the
// rename in one transaction, because each migrate case is the whole
// distance to current rather than one link in a chain.
func TestMigrateV2RenamesCanaryToStress(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildV2Fixture(t, opts.DBPath)

	migrated := openTestStore(t, opts)
	if got := userVersion(t, migrated); got != schemaVersion {
		t.Fatalf("migrated user_version = %d, want %d", got, schemaVersion)
	}
	var fingerprint, summary, source string
	if err := migrated.db.QueryRow(`SELECT sha256 FROM statement_files`).Scan(&fingerprint); err != nil {
		t.Fatalf("v2 column delta did not apply: %v", err)
	}
	if err := migrated.db.QueryRow(`SELECT summary FROM stress_transitions`).Scan(&summary); err != nil {
		t.Fatalf("v2 rename delta did not apply: %v", err)
	}
	if err := migrated.db.QueryRow(`SELECT source FROM ingest_sources`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if fingerprint != "" || summary != "pre-rename summary line" || source != "stress" {
		t.Fatalf("v2 migration = fingerprint %q summary %q source %q", fingerprint, summary, source)
	}
}

// TestMigrateV1RowsIngestContinues proves a migrated file keeps its
// idempotency: the stored offset still governs, so re-ingest of a journal
// the v1 index already covered adds nothing.
func TestMigrateV1RowsIngestContinues(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	line := `{"v":1,"ts":"2026-07-01T00:00:00Z","stage":"calm"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, line)
	buildV1Fixture(t, opts.DBPath)
	// Point the fixture's bookkeeping at the real journal, fully ingested.
	db, err := sql.Open("sqlite", "file:"+opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Clean(opts.RegimeJournalPath)
	hash := lineHash([]byte(`{"v":1,"ts":"2026-07-01T00:00:00Z","stage":"calm"}`))
	if _, err := db.Exec(`UPDATE ingest_sources SET path = ?, offset = ?, genesis = ? WHERE source = 'regime'`, f, len(line), hash); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s := openTestStore(t, opts)
	s.ingestAll(t.Context())
	if got := countRows(t, s, "regime_decisions"); got != 1 {
		t.Fatalf("regime rows after migrated re-ingest = %d, want 1 (offset governs)", got)
	}
}
