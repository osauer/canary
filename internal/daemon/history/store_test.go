package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testOptions returns Options rooted in a fresh temp dir; the journal
// paths point at (possibly not yet existing) files inside it.
func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		DBPath:                filepath.Join(dir, "history.db"),
		RegimeJournalPath:     filepath.Join(dir, "regime-decisions.jsonl"),
		RulesJournalPath:      filepath.Join(dir, "rules-decisions.jsonl"),
		StressJournalPath:     filepath.Join(dir, "canary-decisions.jsonl"),
		CapitalJournalPath:    filepath.Join(dir, "capital-events.jsonl"),
		RiskPolicyJournalPath: filepath.Join(dir, "risk-policy-journal.jsonl"),
		ProposalOutcomesPath:  filepath.Join(dir, "trade-proposal-outcomes.jsonl"),
		OrderJournalPath:      filepath.Join(dir, "order-journal.jsonl"),
		ValidateOrderLine:     validateTestOrderLine,
		StatementsDir:         filepath.Join(dir, "statements"),
		RotatedDir:            filepath.Join(dir, "rotated"),
		Logf:                  t.Logf,
	}
}

func validateTestOrderLine(raw []byte) error {
	var line struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return err
	}
	if line.Version != 1 {
		return fmt.Errorf("unsupported version %d", line.Version)
	}
	return nil
}

func openTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func TestOpenCreatesPrivateMigratedFile(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	s := openTestStore(t, opts)
	if got := userVersion(t, s); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	st, err := os.Stat(opts.DBPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("db file mode = %o, want 0600", got)
	}
}

func TestOpenReopenIsNoOp(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	s := openTestStore(t, opts)
	if _, err := s.db.Exec(`INSERT INTO ingest_sources (source, path, offset) VALUES ('regime', 'x', 42)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openTestStore(t, opts)
	if got := userVersion(t, s2); got != schemaVersion {
		t.Fatalf("user_version after reopen = %d, want %d", got, schemaVersion)
	}
	var offset int64
	if err := s2.db.QueryRow(`SELECT offset FROM ingest_sources WHERE source = 'regime'`).Scan(&offset); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	if offset != 42 {
		t.Fatalf("reopen lost data: offset = %d, want 42", offset)
	}
}

func TestOpenFutureVersionRecreates(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	s := openTestStore(t, opts)
	if _, err := s.db.Exec(`INSERT INTO ingest_sources (source, path, offset) VALUES ('regime', 'x', 7)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openTestStore(t, opts)
	if got := userVersion(t, s2); got != schemaVersion {
		t.Fatalf("user_version after recreate = %d, want %d", got, schemaVersion)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM ingest_sources`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("recreated db still has %d bookkeeping rows, want 0", n)
	}
}

func TestOpenCorruptFileRecreates(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	if err := os.WriteFile(opts.DBPath, []byte("this is not a sqlite database at all"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	s := openTestStore(t, opts)
	if got := userVersion(t, s); got != schemaVersion {
		t.Fatalf("user_version after corrupt-recreate = %d, want %d", got, schemaVersion)
	}
}

func TestAppendOnlyTriggers(t *testing.T) {
	t.Parallel()
	s := openTestStore(t, testOptions(t))
	if _, err := s.db.Exec(`INSERT INTO regime_decisions (src_offset, at, at_unix_ms, stage, raw_json) VALUES (0, 't', 1, 'calm', '{}')`); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO regime_indicators (decision_id, indicator) VALUES (1, 'vix_term')`); err != nil {
		t.Fatalf("insert indicator: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO rule_transitions (src_offset, at, at_unix_ms, rule_id, status, raw_json) VALUES (0, 't', 1, 'r', 'pass', '{}')`); err != nil {
		t.Fatalf("insert transition: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE regime_decisions SET stage = 'x'`,
		`DELETE FROM regime_decisions`,
		`UPDATE regime_indicators SET indicator = 'x'`,
		`DELETE FROM regime_indicators`,
		`UPDATE rule_transitions SET status = 'x'`,
		`DELETE FROM rule_transitions`,
	} {
		_, err := s.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only abort", stmt, err)
		}
	}
}
