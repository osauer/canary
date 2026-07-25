package history

import (
	"database/sql"
	"fmt"
	"os"
)

// schemaVersion is the PRAGMA user_version this build writes and accepts.
// 0 means "fresh file, apply DDL"; versions 1 through 3 migrate in place
// via deltas (evidence rows are renamed with their table, never rewritten
// row by row); anything above schemaVersion means the file was written by
// a newer build and is deleted and recreated (the index is derived, so
// downgrade recovery is rebuild).
const schemaVersion = 4

// baseDDL creates the bookkeeping tables. None of them are evidence: their
// rows are updated in place (ingested-byte offsets, rotation state,
// statement-file records, derived equity days), so they carry no
// append-only triggers. `base` in ingest_sources is the cumulative byte
// count rotated out of the live journal; the stored `offset` is the
// logical-stream high-water mark (never decreases) and the physical resume
// point is `offset - base`. `base` is declared last so a v1 file migrated
// via ALTER TABLE has an identical column layout.
func baseDDL() []string {
	return []string{
		`CREATE TABLE ingest_sources (
  source     TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,
  genesis    TEXT,
  updated_at TEXT,
  base       INTEGER NOT NULL DEFAULT 0
)`,
	}
}

// bookkeepingV2DDL creates the schema-v2 bookkeeping tables: the rotation
// intent/audit log, the archive inventory, and the statement-derivation
// records. Split from baseDDL so the v1→v2 delta migration can apply
// exactly this set.
func bookkeepingV2DDL() []string {
	return []string{
		`CREATE TABLE rotation_log (
  id           INTEGER PRIMARY KEY,
  source       TEXT NOT NULL,
  started_at   TEXT NOT NULL,
  state        TEXT NOT NULL,
  cut_bytes    INTEGER NOT NULL,
  live_size    INTEGER NOT NULL,
  base_before  INTEGER NOT NULL,
  pre_genesis  TEXT,
  post_genesis TEXT,
  archives_json TEXT NOT NULL,
  finished_at  TEXT
)`,
		`CREATE TABLE archive_files (
  source     TEXT NOT NULL,
  name       TEXT NOT NULL,
  raw_bytes  INTEGER NOT NULL,
  gz_bytes   INTEGER NOT NULL,
  origin     TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (source, name)
)`,
		`CREATE TABLE statement_files (
  name        TEXT PRIMARY KEY,
  size        INTEGER NOT NULL,
  sha256      TEXT NOT NULL DEFAULT '',
  ingested_at TEXT NOT NULL,
  equity_days INTEGER NOT NULL
)`,
		`CREATE TABLE statement_equity_days (
  account_id     TEXT NOT NULL,
  day            TEXT NOT NULL,
  equity_base    REAL NOT NULL,
  source_stmt    TEXT NOT NULL,
  when_generated TEXT NOT NULL,
  PRIMARY KEY (account_id, day)
)`,
	}
}

// regimeDDL creates the regime-decision tables, their indexes, and the
// append-only triggers. Kept as one unit so the truncation-rebuild path
// re-applies exactly the DDL the migration applied.
func regimeDDL() []string {
	stmts := []string{
		`CREATE TABLE regime_decisions (
  id           INTEGER PRIMARY KEY,
  src_offset   INTEGER NOT NULL UNIQUE,
  at           TEXT NOT NULL,
  at_unix_ms   INTEGER NOT NULL,
  session_key  TEXT,
  fingerprint  TEXT,
  tape_session TEXT,
  stage        TEXT NOT NULL,
  severity     TEXT,
  readiness    TEXT,
  confidence   TEXT,
  verdict      TEXT,
  cluster_red_count          INTEGER,
  cluster_yellow_count       INTEGER,
  cluster_eligible_red_count INTEGER,
  raw_json     TEXT NOT NULL
)`,
		`CREATE INDEX regime_decisions_at ON regime_decisions(at_unix_ms)`,
		`CREATE TABLE regime_indicators (
  decision_id      INTEGER NOT NULL REFERENCES regime_decisions(id),
  indicator        TEXT NOT NULL,
  status           TEXT,
  band             TEXT,
  value            REAL,
  depth            REAL,
  streak_sessions  INTEGER,
  freshness        TEXT,
  eligible         INTEGER,
  latched          INTEGER NOT NULL DEFAULT 0,
  thresholds_label TEXT,
  PRIMARY KEY (decision_id, indicator)
) WITHOUT ROWID`,
		`CREATE INDEX regime_indicators_by_indicator ON regime_indicators(indicator, decision_id)`,
	}
	stmts = append(stmts, appendOnlyTriggers("regime_decisions")...)
	stmts = append(stmts, appendOnlyTriggers("regime_indicators")...)
	return stmts
}

// rulesDDL creates the rule-transition table, indexes, and append-only
// triggers, mirroring regimeDDL's rebuild-reusable shape.
func rulesDDL() []string {
	stmts := []string{
		`CREATE TABLE rule_transitions (
  id                 INTEGER PRIMARY KEY,
  src_offset         INTEGER NOT NULL UNIQUE,
  at                 TEXT NOT NULL,
  at_unix_ms         INTEGER NOT NULL,
  rule_id            TEXT NOT NULL,
  status             TEXT NOT NULL,
  was                TEXT,
  evidence           TEXT,
  policy_id          TEXT,
  policy_version     INTEGER,
  policy_fingerprint TEXT,
  raw_json           TEXT NOT NULL
)`,
		`CREATE INDEX rule_transitions_at   ON rule_transitions(at_unix_ms)`,
		`CREATE INDEX rule_transitions_rule ON rule_transitions(rule_id, at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("rule_transitions")...)
}

// capitalDDL mirrors capital-events.jsonl (declared capital-flow ledger).
func capitalDDL() []string {
	stmts := []string{
		`CREATE TABLE capital_events (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL, at_unix_ms INTEGER NOT NULL,
  type TEXT NOT NULL, amount_base REAL, effective_at TEXT,
  note TEXT, origin TEXT, report_id TEXT, coverage_to TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX capital_events_at ON capital_events(at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("capital_events")...)
}

// riskPolicyDDL mirrors risk-policy-journal.jsonl (governance audit trail).
func riskPolicyDDL() []string {
	stmts := []string{
		`CREATE TABLE risk_policy_events (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL, at_unix_ms INTEGER NOT NULL,
  kind TEXT NOT NULL, policy_id TEXT, policy_version INTEGER, policy_fingerprint TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX risk_policy_events_at   ON risk_policy_events(at_unix_ms)`,
		`CREATE INDEX risk_policy_events_kind ON risk_policy_events(kind, at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("risk_policy_events")...)
}

// proposalOutcomesDDL mirrors trade-proposal-outcomes.jsonl (measurement
// book for protection proposals).
func proposalOutcomesDDL() []string {
	stmts := []string{
		`CREATE TABLE proposal_outcomes (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL, at_unix_ms INTEGER NOT NULL,
  mark_date TEXT, state TEXT NOT NULL, proposal_key TEXT, revision TEXT, bucket TEXT,
  symbol TEXT, sec_type TEXT, action TEXT, quantity REAL,
  order_ref TEXT, preview_token_id TEXT, exec_id TEXT,
  policy_id TEXT, policy_version INTEGER, policy_fingerprint TEXT,
  baseline_price REAL, mark_price REAL, avg_fill_price REAL,
  execution_pnl REAL, benchmark_symbol TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX proposal_outcomes_at     ON proposal_outcomes(at_unix_ms)`,
		`CREATE INDEX proposal_outcomes_symbol ON proposal_outcomes(symbol, at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("proposal_outcomes")...)
}

// stressDDL mirrors canary-decisions.jsonl (portfolio-stress evidence).
func stressDDL() []string {
	stmts := []string{
		`CREATE TABLE stress_transitions (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL, at_unix_ms INTEGER NOT NULL,
  session_key TEXT, fingerprint TEXT, account TEXT, account_mode TEXT,
  action TEXT, severity TEXT, direction TEXT, market_stage TEXT,
  portfolio_alert_relevant INTEGER,
  input_health TEXT, summary TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX stress_transitions_at  ON stress_transitions(at_unix_ms)`,
		`CREATE INDEX stress_transitions_sev ON stress_transitions(severity, at_unix_ms)`,
	}
	return append(stmts, appendOnlyTriggers("stress_transitions")...)
}

// ordersDDL mirrors order-journal.jsonl. Deliberate deviation from the
// other sources: an unparseable or wrong-version line is stored verbatim
// with parse_ok = 0 instead of being skipped, because the legacy
// LoadEvents hard-fails on such lines and the indexed order-read path must
// reproduce that refusal exactly rather than silently diverge.
func ordersDDL() []string {
	stmts := []string{
		`CREATE TABLE order_events (
  id INTEGER PRIMARY KEY, src_offset INTEGER NOT NULL UNIQUE,
  at TEXT NOT NULL DEFAULT '', at_unix_ms INTEGER NOT NULL DEFAULT 0,
  parse_ok INTEGER NOT NULL DEFAULT 1,
  version INTEGER, type TEXT, order_ref TEXT, preview_token_id TEXT,
  reserved_order_id INTEGER, perm_id INTEGER,
  account TEXT, mode TEXT, status TEXT, send_state TEXT,
  raw_json TEXT NOT NULL
)`,
		`CREATE INDEX order_events_at       ON order_events(at_unix_ms, id)`,
		`CREATE INDEX order_events_token    ON order_events(preview_token_id) WHERE preview_token_id <> ''`,
		`CREATE INDEX order_events_reserved ON order_events(reserved_order_id) WHERE reserved_order_id > 0`,
		`CREATE INDEX order_events_bad      ON order_events(parse_ok) WHERE parse_ok = 0`,
	}
	return append(stmts, appendOnlyTriggers("order_events")...)
}

// evidenceV2DDL is every schema-v2 evidence table. Split out so the v1→v2
// delta migration applies exactly the set the fresh path applies.
func evidenceV2DDL() []string {
	var stmts []string
	stmts = append(stmts, capitalDDL()...)
	stmts = append(stmts, riskPolicyDDL()...)
	stmts = append(stmts, proposalOutcomesDDL()...)
	stmts = append(stmts, stressDDL()...)
	stmts = append(stmts, ordersDDL()...)
	return stmts
}

// appendOnlyTriggers returns UPDATE/DELETE ABORT triggers for one
// evidence-mirroring table. Rebuild uses DROP TABLE, which drops the
// triggers with the table.
func appendOnlyTriggers(table string) []string {
	return []string{
		fmt.Sprintf(`CREATE TRIGGER %s_no_update BEFORE UPDATE ON %s
  BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table),
		fmt.Sprintf(`CREATE TRIGGER %s_no_delete BEFORE DELETE ON %s
  BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table),
	}
}

// stressRenameV4DDL is the schema-v4 delta: the sensor's persisted names
// move from canary to stress without touching a single evidence row.
// ALTER TABLE ... RENAME TO carries the rows and SQLite rewrites the
// trigger bodies that reference the table; the derived index and trigger
// NAMES do not follow a rename, so they are dropped and recreated (both
// are derived from the table, so nothing is lost). The bookkeeping source
// value is rewritten in place, which keeps the ingest watermark, the
// archive inventory, and the rotation audit log attached to the source
// under its new name instead of stranding them and re-ingesting from
// offset 0.
func stressRenameV4DDL() []string {
	stmts := []string{
		`ALTER TABLE canary_transitions RENAME TO stress_transitions`,
		`DROP INDEX IF EXISTS canary_transitions_at`,
		`DROP INDEX IF EXISTS canary_transitions_sev`,
		`DROP TRIGGER IF EXISTS canary_transitions_no_update`,
		`DROP TRIGGER IF EXISTS canary_transitions_no_delete`,
		`CREATE INDEX stress_transitions_at  ON stress_transitions(at_unix_ms)`,
		`CREATE INDEX stress_transitions_sev ON stress_transitions(severity, at_unix_ms)`,
	}
	stmts = append(stmts, appendOnlyTriggers("stress_transitions")...)
	return append(stmts,
		`UPDATE ingest_sources SET source = 'stress' WHERE source = 'canary'`,
		`UPDATE archive_files  SET source = 'stress' WHERE source = 'canary'`,
		`UPDATE rotation_log   SET source = 'stress' WHERE source = 'canary'`,
	)
}

// openAndMigrate pre-creates the file 0600 (SQLite itself would create
// 0644; the WAL sidecars inherit the file's mode), opens the single
// serialized connection, and brings user_version to schemaVersion. Any
// error — unreadable file, garbage content, future user_version, failed
// DDL — is returned for the caller's delete-and-recreate recovery.
func openAndMigrate(path string) (*sql.DB, error) {
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pre-create: %w", err)
	}
	_ = f.Close()

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: transactions serialize in-process, so ingest batches
	// and read queries never see SQLITE_BUSY from each other.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrate brings the file to schemaVersion in one transaction. Each case is
// the whole distance from that version to current, not one link in a chain:
// user_version 0 applies the current DDL; 1 applies the schema-v2 delta
// (ALTER ingest_sources ADD base, create the new tables — already in their
// current shape, so no later delta applies); 2 adds statement file
// fingerprints and renames the canary sensor's tables to stress; 3 applies
// that rename alone. Existing evidence rows are untouched. Anything else is
// returned for delete-and-recreate recovery of last resort.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	var delta []string
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("user_version %d is newer than supported %d", version, schemaVersion)
	case version == 0:
		delta = append(delta, baseDDL()...)
		delta = append(delta, bookkeepingV2DDL()...)
		delta = append(delta, regimeDDL()...)
		delta = append(delta, rulesDDL()...)
		delta = append(delta, evidenceV2DDL()...)
	case version == 1:
		delta = append(delta, `ALTER TABLE ingest_sources ADD COLUMN base INTEGER NOT NULL DEFAULT 0`)
		delta = append(delta, bookkeepingV2DDL()...)
		delta = append(delta, evidenceV2DDL()...)
	case version == 2:
		delta = append(delta, `ALTER TABLE statement_files ADD COLUMN sha256 TEXT NOT NULL DEFAULT ''`)
		delta = append(delta, stressRenameV4DDL()...)
	case version == 3:
		delta = append(delta, stressRenameV4DDL()...)
	default:
		return fmt.Errorf("unexpected user_version %d", version)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range delta {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("apply DDL: %w", err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return tx.Commit()
}

// rebuildSource drops and recreates one source's tables (triggers and
// indexes go with them) and resets its bookkeeping row — the recovery for
// a truncated or replaced journal. For rotatable sources the archive_files
// rows are cleared too, so the next ingest re-streams the rotated archives
// before the live file. Runs in its own transaction so a crash mid-rebuild
// leaves either the old state or the clean empty state.
func (s *Store) rebuildSource(def sourceDef, now string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Children first: with foreign_keys ON, dropping the parent while a
	// referencing table exists would fail.
	for _, table := range def.dropTables {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	for _, stmt := range def.createDDL() {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("recreate %s tables: %w", def.name, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM archive_files WHERE source = ?`, def.name); err != nil {
		return fmt.Errorf("reset %s archive inventory: %w", def.name, err)
	}
	if _, err := tx.Exec(`INSERT INTO ingest_sources (source, path, offset, base, genesis, updated_at)
VALUES (?, ?, 0, 0, NULL, ?)
ON CONFLICT(source) DO UPDATE SET path = excluded.path, offset = 0, base = 0, genesis = NULL, updated_at = excluded.updated_at`,
		def.name, def.path, now); err != nil {
		return fmt.Errorf("reset %s bookkeeping: %w", def.name, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.setWatermark(def.name, 0)
	if def.name == sourceOrders {
		s.setOrdersParseBad(false)
	}
	return nil
}
