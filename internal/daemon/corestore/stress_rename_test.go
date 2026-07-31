package corestore

import (
	"database/sql"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// migrationChecksumPins freezes the ledger identity of every shipped
// migration. A migration that has been applied to a real authority database can
// never be edited: migrate() compares these checksums against schema_migrations
// and refuses to open on drift, so an in-place edit would brick every existing
// daemon.db with "migration checksum drift at version N". Schema changes go in
// a new migration. If a change here looks necessary, it is not.
var migrationChecksumPins = map[int]string{
	1: "c24046b783d77f45b4609e23e677f581bca95af203ff5579803d7959259e811e",
	2: "055e15432884b5ec2e914440438cdb94863724464d319e50dbbd59803eefc056",
	3: "0d735df122fa40b41c2a74fe79560d5c4f865ff33b413a3bc69867bbb771419d",
}

func TestShippedMigrationChecksumsAreFrozen(t *testing.T) {
	plan := currentMigrationPlan()
	if err := validateMigrationPlan(plan); err != nil {
		t.Fatalf("shipped migration plan is invalid: %v", err)
	}
	if len(plan) != len(migrationChecksumPins) {
		t.Fatalf("plan has %d migrations but %d are pinned; pin the new one rather than editing an old one", len(plan), len(migrationChecksumPins))
	}
	for _, m := range plan {
		want, ok := migrationChecksumPins[m.version]
		if !ok {
			t.Fatalf("migration %d (%s) is not pinned", m.version, m.name)
		}
		if got := migrationChecksum(m); got != want {
			t.Fatalf("migration %d (%s) checksum drifted:\n got %s\nwant %s\nEditing an applied migration makes every existing authority database refuse to open. Add a new migration instead.", m.version, m.name, got, want)
		}
	}
}

// TestDestructiveGuardRejectsUnapprovedMigrations is the counterpart to
// migration 2's approval: the exception must stay attached to the statements a
// human signed off on, and must not become a way to run destructive SQL
// generally.
func TestDestructiveGuardRejectsUnapprovedMigrations(t *testing.T) {
	const dropEventLog = `DROP TABLE event_log`
	const dropTrigger = `DROP TRIGGER event_log_no_update`

	for _, tc := range []struct {
		name string
		m    migration
		want string
	}{{
		name: "no approval at all",
		m:    migration{version: 2, name: "unapproved", statements: []string{dropEventLog}},
		want: "contains destructive statement",
	}, {
		name: "approval does not cover every destructive statement",
		m: migration{
			version:    2,
			name:       "partially_approved",
			statements: []string{dropTrigger, dropEventLog, appendOnlyUpdateTrigger("event_log")},
			destructive: &destructiveApproval{
				reason:     "approved: unarm the update guard for one relabel",
				statements: []string{dropTrigger},
			},
		},
		want: "contains destructive statement",
	}, {
		name: "approval without a reason",
		m: migration{
			version:     2,
			name:        "reasonless",
			statements:  []string{dropTrigger},
			destructive: &destructiveApproval{statements: []string{dropTrigger}},
		},
		want: "without a reason",
	}, {
		name: "empty approval",
		m: migration{
			version:     2,
			name:        "empty_approval",
			statements:  []string{dropTrigger},
			destructive: &destructiveApproval{reason: "because"},
		},
		want: "approves no destructive statements",
	}, {
		name: "approval names a statement the migration does not run",
		m: migration{
			version:    2,
			name:       "absent",
			statements: []string{`CREATE TABLE probe(id INTEGER PRIMARY KEY) STRICT`},
			destructive: &destructiveApproval{
				reason:     "stale approval left behind after the statement was removed",
				statements: []string{dropEventLog},
			},
		},
		want: "does not run",
	}, {
		name: "approval names a statement that is not destructive",
		m: migration{
			version:    2,
			name:       "harmless",
			statements: []string{`CREATE TABLE probe(id INTEGER PRIMARY KEY) STRICT`},
			destructive: &destructiveApproval{
				reason:     "over-broad approval",
				statements: []string{`CREATE TABLE probe(id INTEGER PRIMARY KEY) STRICT`},
			},
		},
		want: "not destructive",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMigrationStatements(tc.m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateMigrationStatements error=%v, want one containing %q", err, tc.want)
			}
			plan := append(currentMigrationPlan(), tc.m)
			plan[len(plan)-1].version = len(plan)
			if err := validateMigrationPlan(plan); err == nil {
				t.Fatal("validateMigrationPlan accepted the rejected migration")
			}
		})
	}

	// Migration 2 genuinely needs its approval rather than carrying a decorative
	// one: strip the approval and the guard refuses the real statements.
	shipped := stressRenameMigration()
	if err := validateMigrationStatements(shipped); err != nil {
		t.Fatalf("shipped migration %d rejected with its own approval: %v", shipped.version, err)
	}
	unapproved := shipped
	unapproved.destructive = nil
	if err := validateMigrationStatements(unapproved); err == nil || !strings.Contains(err.Error(), "contains destructive statement") {
		t.Fatalf("migration %d without its approval error=%v", shipped.version, err)
	}

	// Every destructive form the guard knows about is still refused without an
	// approval, whatever an approved sibling statement in the same plan allows.
	for _, stmt := range []string{
		`DROP TABLE event_log`,
		`DROP INDEX event_log_scope_time`,
		`DELETE FROM event_log`,
		`REPLACE INTO event_log(event_seq) VALUES(1)`,
		`VACUUM`,
		`ALTER TABLE event_log DROP COLUMN origin`,
		`   drop   trigger event_log_no_delete`,
	} {
		m := migration{version: 2, name: "unapproved", statements: []string{stmt}}
		if err := validateMigrationStatements(m); err == nil {
			t.Errorf("guard accepted unapproved %q", stmt)
		}
	}
}

// TestStressRenameMigrationPreservesEvidence upgrades a populated v1 authority
// and proves the rename moved names only: no evidence row changes except the
// one relabelled audit column, and the append-only guards are armed again on
// the other side.
func TestStressRenameMigrationPreservesEvidence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(privateTempDir(t), "daemon.db")
	plan := currentMigrationPlan()

	seedV1Authority(t, path, plan)

	db := rawDB(t, path)
	defer db.Close()
	beforeTransitions := snapshotTable(t, db, "canary_transitions")
	beforeEvents := snapshotTable(t, db, "event_log")
	if len(beforeTransitions) != 2 || len(beforeEvents) != 3 {
		t.Fatalf("fixture rows: canary_transitions=%d event_log=%d", len(beforeTransitions), len(beforeEvents))
	}

	if _, err := migrate(ctx, db, plan, time.Now().UTC()); err != nil {
		t.Fatalf("apply stress rename: %v", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(plan) {
		t.Fatalf("user_version=%d err=%v want %d", version, err, len(plan))
	}
	if n := tableCount(t, db, "table", "canary_transitions"); n != 0 {
		t.Errorf("canary_transitions still exists after the rename")
	}
	if n := tableCount(t, db, "table", "stress_transitions"); n != 1 {
		t.Errorf("stress_transitions missing after the rename")
	}

	// Every column of every row carried across unchanged.
	afterTransitions := snapshotTable(t, db, "stress_transitions")
	if !reflect.DeepEqual(beforeTransitions, afterTransitions) {
		t.Fatalf("renamed rows changed:\nbefore=%v\nafter =%v", beforeTransitions, afterTransitions)
	}

	// event_log changed in exactly one column, on exactly the renamed type.
	wantEvents := make([]map[string]any, 0, len(beforeEvents))
	for _, row := range beforeEvents {
		copied := maps.Clone(row)
		if copied["event_type"] == "canary_decision" {
			copied["event_type"] = "stress_decision"
		}
		wantEvents = append(wantEvents, copied)
	}
	afterEvents := snapshotTable(t, db, "event_log")
	if !reflect.DeepEqual(wantEvents, afterEvents) {
		t.Fatalf("event_log changed beyond the relabelled event_type:\nwant=%v\ngot =%v", wantEvents, afterEvents)
	}
	counts := map[string]int{}
	for _, row := range afterEvents {
		counts[row["event_type"].(string)]++
	}
	if counts["stress_decision"] != 2 || counts["regime_decision"] != 1 || counts["canary_decision"] != 0 {
		t.Fatalf("event_type counts after migration: %v", counts)
	}

	// The append-only guards are armed again, under the new names.
	for _, table := range appendOnlyTables {
		if n := tableCount(t, db, "trigger", table+"_no_update"); n != 1 {
			t.Errorf("%s_no_update missing after migration", table)
		}
		if n := tableCount(t, db, "trigger", table+"_no_delete"); n != 1 {
			t.Errorf("%s_no_delete missing after migration", table)
		}
	}
	var armedTables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name LIKE '%\_no\_update' ESCAPE '\'`).Scan(&armedTables); err != nil {
		t.Fatal(err)
	}
	if armedTables != len(appendOnlyTables) {
		t.Errorf("append-only tables armed=%d want %d", armedTables, len(appendOnlyTables))
	}
	var strayTriggers int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name LIKE 'canary%'`).Scan(&strayTriggers); err != nil || strayTriggers != 0 {
		t.Errorf("canary-named triggers survived: %d err=%v", strayTriggers, err)
	}
	for _, stmt := range []string{
		`UPDATE stress_transitions SET action='tampered'`,
		`DELETE FROM stress_transitions`,
		`UPDATE event_log SET event_type='tampered'`,
		`DELETE FROM event_log`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("append-only guard did not re-arm: %q succeeded", stmt)
		} else if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("unexpected refusal for %q: %v", stmt, err)
		}
	}

	// Reopening a migrated authority applies nothing and rewrites nothing.
	if _, err := migrate(ctx, db, plan, time.Now().UTC()); err != nil {
		t.Fatalf("second migrate on a current database: %v", err)
	}
	if got := snapshotTable(t, db, "event_log"); !reflect.DeepEqual(wantEvents, got) {
		t.Fatal("re-running migrate rewrote event_log")
	}
	if got := snapshotTable(t, db, "stress_transitions"); !reflect.DeepEqual(beforeTransitions, got) {
		t.Fatal("re-running migrate rewrote stress_transitions")
	}
	ledger := snapshotTable(t, db, "schema_migrations")
	if len(ledger) != len(plan) {
		t.Fatalf("migration ledger rows=%d want %d", len(ledger), len(plan))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The migrated file is a valid current authority: this re-checks the full
	// schema-object manifest, the migration ledger, and the payload digests.
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open migrated authority: %v", err)
	}
	defer store.Close()
	events, err := store.LoadEvents(ctx, EventQuery{ScopeKey: "daemon", Type: "stress_decision"})
	if err != nil || len(events) != 2 {
		t.Fatalf("relabelled events are not readable: n=%d err=%v", len(events), err)
	}
	report, err := store.CheckIntegrity(ctx)
	if err != nil || !report.OK() {
		t.Fatalf("integrity after migration: %+v err=%v", report, err)
	}
}

// TestStressRenameUpgradesThroughOperatorPath drives the same rename through
// the path a daemon actually takes on first start after the upgrade: inspect,
// back up, build a candidate, and validate it, leaving the v1 source untouched.
func TestStressRenameUpgradesThroughOperatorPath(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon.db")
	plan := currentMigrationPlan()
	seedV1Authority(t, sourcePath, plan)

	inspection, err := inspectWithPlan(ctx, InspectOptions{Path: sourcePath}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != InspectionUpgradeRequired || inspection.SchemaVersion != 1 || inspection.TargetVersion != len(plan) {
		t.Fatalf("pre-upgrade inspection=%+v", inspection)
	}
	if _, err := openWithPlan(ctx, Options{Path: sourcePath}, plan); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("opening a v1 authority error=%v, want ErrUpgradeRequired", err)
	}

	result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
		SourcePath:    sourcePath,
		BackupPath:    filepath.Join(dir, "backup.db"),
		CandidatePath: filepath.Join(dir, "candidate.db"),
	}, plan)
	if err != nil {
		t.Fatalf("prepare upgrade: %v", err)
	}
	if result.Backup.SchemaVersion != 1 || result.Candidate.SchemaVersion != len(plan) {
		t.Fatalf("backup=%d candidate=%d", result.Backup.SchemaVersion, result.Candidate.SchemaVersion)
	}
	if !result.Candidate.Integrity.OK() {
		t.Fatalf("candidate integrity=%+v", result.Candidate.Integrity)
	}
	if result.Candidate.Head.LastEventSeq != result.Source.Head.LastEventSeq ||
		result.Candidate.Head.AuthorityEpoch != result.Source.Head.AuthorityEpoch {
		t.Fatalf("upgrade moved the authority head: source=%+v candidate=%+v", result.Source.Head, result.Candidate.Head)
	}

	candidate := rawDB(t, result.Candidate.Path)
	defer candidate.Close()
	if n := tableCount(t, candidate, "table", "stress_transitions"); n != 1 {
		t.Error("candidate has no stress_transitions table")
	}
	var relabelled int
	if err := candidate.QueryRow(`SELECT count(*) FROM event_log WHERE event_type='stress_decision'`).Scan(&relabelled); err != nil || relabelled != 2 {
		t.Fatalf("candidate relabelled rows=%d err=%v", relabelled, err)
	}

	// The source is still the untouched v1 authority.
	source := rawDB(t, sourcePath)
	defer source.Close()
	if n := tableCount(t, source, "table", "canary_transitions"); n != 1 {
		t.Error("prepare upgrade modified the source database")
	}
}

// seedV1Authority creates an authority at migration 1 holding two canary
// decision events with their projections and one unrelated event that the
// rename must not touch.
func seedV1Authority(t *testing.T, path string, plan []migration) {
	t.Helper()
	ctx := t.Context()
	store, err := openWithPlan(ctx, Options{Path: path}, plan[:1])
	if err != nil {
		t.Fatalf("open v1 authority: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	inputs := []EventInput{{
		ScopeKey: "daemon", EventKey: "canary_decision:1", Type: "canary_decision",
		Action: "record", Origin: "daemon_internal", OccurredAt: at,
		PayloadJSON: []byte(`{"action":"arm","severity":"amber"}`),
	}, {
		ScopeKey: "daemon", EventKey: "regime_decision:1", Type: "regime_decision",
		Action: "record", Origin: "daemon_internal", OccurredAt: at.Add(time.Minute),
		PayloadJSON: []byte(`{"stage":"calm"}`),
	}, {
		ScopeKey: "daemon", EventKey: "canary_decision:2", Type: "canary_decision",
		Action: "record", Origin: "daemon_internal", OccurredAt: at.Add(2 * time.Minute),
		PayloadJSON: []byte(`{"action":"clear","severity":"green"}`),
	}}
	receipts, err := store.AppendEvents(ctx, inputs)
	if err != nil {
		t.Fatalf("seed events: %v", err)
	}
	// The v1 projection table is written directly: the Go writer now targets
	// stress_transitions, which does not exist until migration 2.
	for i, receipt := range receipts {
		if inputs[i].Type != "canary_decision" {
			continue
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO canary_transitions(event_seq,scope_key,action,severity,direction,market_stage,input_health,portfolio_alert_relevant) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.EventSeq, "daemon", "arm", "amber", "deteriorating", "watch", "ok", 1); err != nil {
			t.Fatalf("seed canary_transitions: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// snapshotTable reads a whole table as column-keyed rows so a before/after
// comparison covers every column, including ones no assertion names.
func snapshotTable(t *testing.T, db *sql.DB, table string) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT * FROM `+table+` ORDER BY rowid`)
	if err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		cells := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if blob, ok := cells[i].([]byte); ok {
				row[name] = string(blob)
				continue
			}
			row[name] = cells[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableCount(t *testing.T, db *sql.DB, objectType, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type=? AND name=?`, objectType, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
