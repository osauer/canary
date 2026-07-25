package history

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"
)

const (
	// Source names key ingest_sources rows, Health lookups, and rotation.
	sourceRegime           = "regime"
	sourceRules            = "rules"
	sourceStress           = "stress"
	sourceCapital          = "capital"
	sourceRiskPolicy       = "risk_policy"
	sourceProposalOutcomes = "proposal_outcomes"
	sourceOrders           = "orders"

	// ingestBatchLines is how many journal lines land per transaction; the
	// bookkeeping offset advances in the same commit as its rows, which is
	// the entire idempotency mechanism.
	ingestBatchLines = 2000

	// scanBufInitial/scanBufMax bound per-line memory: one line at a time,
	// O(largest line), hard-capped. A line beyond the cap halts its source
	// at the current offset without advancing.
	scanBufInitial = 64 * 1024
	scanBufMax     = 16 * 1024 * 1024
)

// sourceDef binds one journal to its tables: where it lives, what to drop
// and recreate on a truncation rebuild, and how a complete line becomes
// rows. One ingest code path serves backfill, tail-ingest, and crash
// reconcile — they differ only in the stored offset. insertLine's replay
// flag switches the src_offset key to INSERT OR IGNORE for idempotent
// archive-backfill resume; live-file ingest keeps the plain-INSERT loud
// backstop.
type sourceDef struct {
	name       string
	path       string
	dropTables []string // children first: foreign_keys=ON forbids dropping a referenced parent
	createDDL  func() []string
	insertLine func(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error
	// rotatable marks the journal as covered by the rotation engine:
	// archives under rotated/ participate in rebuild/backfill, and the
	// maintenance loop may rotate its fully-ingested prefix.
	rotatable bool
	// tsField is the journal timestamp key the rotation cut parses ("ts"
	// for regime/stress, "at" for rules). Empty for non-rotatable sources.
	tsField string
}

func (s *Store) sources() []sourceDef {
	return []sourceDef{
		{
			name:       sourceRegime,
			path:       s.opts.RegimeJournalPath,
			dropTables: []string{"regime_indicators", "regime_decisions"},
			createDDL:  regimeDDL,
			insertLine: insertRegimeLine,
			rotatable:  true,
			tsField:    "ts",
		},
		{
			name:       sourceRules,
			path:       s.opts.RulesJournalPath,
			dropTables: []string{"rule_transitions"},
			createDDL:  rulesDDL,
			insertLine: insertRulesLine,
			rotatable:  true,
			tsField:    "at",
		},
		{
			name:       sourceStress,
			path:       s.opts.StressJournalPath,
			dropTables: []string{"stress_transitions"},
			createDDL:  stressDDL,
			insertLine: insertStressLine,
			rotatable:  true,
			tsField:    "ts",
		},
		{
			name:       sourceCapital,
			path:       s.opts.CapitalJournalPath,
			dropTables: []string{"capital_events"},
			createDDL:  capitalDDL,
			insertLine: insertCapitalLine,
		},
		{
			name:       sourceRiskPolicy,
			path:       s.opts.RiskPolicyJournalPath,
			dropTables: []string{"risk_policy_events"},
			createDDL:  riskPolicyDDL,
			insertLine: insertRiskPolicyLine,
		},
		{
			name:       sourceProposalOutcomes,
			path:       s.opts.ProposalOutcomesPath,
			dropTables: []string{"proposal_outcomes"},
			createDDL:  proposalOutcomesDDL,
			insertLine: insertProposalOutcomeLine,
		},
		{
			name:       sourceOrders,
			path:       s.opts.OrderJournalPath,
			dropTables: []string{"order_events"},
			createDDL:  ordersDDL,
			insertLine: s.insertOrderLine,
		},
	}
}

func (s *Store) sourceByName(name string) (sourceDef, bool) {
	for _, def := range s.sources() {
		if def.name == name {
			return def, true
		}
	}
	return sourceDef{}, false
}

// ingestAll advances every source to its journal's last complete line and
// refreshes the statement-derived equity days. Errors are logged and
// swallowed — the index degrades, journaling never does. Serialized with
// rotation via ingestMu.
func (s *Store) ingestAll(ctx context.Context) {
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	for _, def := range s.sources() {
		if def.path == "" {
			continue
		}
		if err := s.ingestSource(ctx, def); err != nil && !errors.Is(err, context.Canceled) {
			s.warnf("history: ingest %s: %v", def.name, err)
		}
	}
	if s.opts.StatementsDir != "" {
		if err := s.ingestStatements(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.warnf("history: ingest statements: %v", err)
		}
	}
}

// ingestSource is the single ingest code path for one journal. Rotatable
// sources first stream any rotated archives the DB does not contain yet
// (fresh DB, post-rebuild); then the live file is scanned:
//
//  1. read (offset, base, genesis) bookkeeping, creating the row on first
//     sight; `offset` is the logical-stream high-water mark and
//     `offset - base` the physical resume point in the live file;
//  2. a missing journal or size == offset-base is a clean no-op;
//  3. size < offset-base or a changed first-line hash means the journal
//     was truncated or replaced → drop and rebuild this source from
//     offset 0 (re-streaming archives first);
//  4. scan complete lines from the physical resume point (a trailing line
//     without '\n' is left for next time; the offset never advances past
//     it), inserting rows keyed by the line's starting logical offset; a
//     complete but unparseable line is logged, counted, and skipped
//     (orders: stored with parse_ok = 0 instead);
//  5. every ingestBatchLines lines the offset advances and the batch
//     commits atomically with its rows; the final partial batch commits
//     the same way.
//
// Callers hold ingestMu.
func (s *Store) ingestSource(ctx context.Context, def sourceDef) error {
	for range 2 {
		rebuilt, err := s.ingestSourcePass(ctx, def)
		if err != nil || !rebuilt {
			return err
		}
	}
	return fmt.Errorf("%s journal changed identity twice in one pass", def.name)
}

func (s *Store) ingestSourcePass(ctx context.Context, def sourceDef) (rebuilt bool, err error) {
	if def.rotatable && s.opts.RotatedDir != "" {
		if err := s.backfillArchives(ctx, def); err != nil {
			if !errors.Is(err, errArchiveBoundaryConflict) {
				return false, err
			}
			// A rotation swapped the live file but died before recording
			// the archive. Rebuilding re-streams archives then the live
			// file, which is exactly the recovery this state needs.
			s.warnf("history: %s has an archive the index has not accounted for (%v); dropping and rebuilding its index tables from offset 0", def.name, err)
			if rerr := s.rebuildSource(def, nowUTC()); rerr != nil {
				return false, fmt.Errorf("rebuild after archive boundary conflict: %w", rerr)
			}
			return true, nil
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	// tx is rebound across batches; a deferred rollback on the latest tx is
	// a no-op (ErrTxDone) after its commit.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT OR IGNORE INTO ingest_sources (source, path, offset) VALUES (?, ?, 0)`,
		def.name, def.path); err != nil {
		return false, err
	}
	var offset, base int64
	var genesis sql.NullString
	if err := tx.QueryRow(`SELECT offset, base, genesis FROM ingest_sources WHERE source = ?`, def.name).Scan(&offset, &base, &genesis); err != nil {
		return false, err
	}

	f, err := os.Open(def.path)
	if err != nil {
		if os.IsNotExist(err) {
			if cerr := tx.Commit(); cerr != nil {
				return false, cerr
			}
			s.setWatermark(def.name, offset)
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()
	physical := offset - base
	// Identity must be checked before the equal-size no-op. Otherwise an
	// atomic replacement with the same byte length leaves stale indexed
	// rows looking current.
	firstHash, firstOK := firstLineHash(f)
	if size < physical || (genesis.String != "" && (!firstOK || firstHash != genesis.String)) {
		_ = tx.Rollback()
		if def.name == sourceOrders {
			s.clearOrderGeneration()
		}
		s.warnf("history: %s journal %s was truncated or replaced (size %d, ingested %d past %d rotated); dropping and rebuilding its index tables from offset 0",
			def.name, def.path, size, physical, base)
		if err := s.rebuildSource(def, nowUTC()); err != nil {
			return false, fmt.Errorf("rebuild after truncation: %w", err)
		}
		return true, nil
	}
	if size == physical {
		if def.name == sourceOrders && !s.orderGenerationMatches(st, firstHash, firstOK) {
			matches, matchErr := orderIndexMatchesJournal(tx, f)
			if matchErr != nil {
				return false, fmt.Errorf("verify equal-size orders journal: %w", matchErr)
			}
			if !matches {
				_ = tx.Rollback()
				s.clearOrderGeneration()
				s.warnf("history: orders journal %s changed content without changing size; dropping and rebuilding its index tables from offset 0", def.path)
				if err := s.rebuildSource(def, nowUTC()); err != nil {
					return false, fmt.Errorf("rebuild after equal-size replacement: %w", err)
				}
				return true, nil
			}
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		s.setWatermark(def.name, offset)
		if def.name == sourceOrders {
			s.setOrderGeneration(st, firstHash, firstOK)
		}
		return false, nil
	}

	if _, err := f.Seek(physical, io.SeekStart); err != nil {
		return false, err
	}
	sc := newLineScanner(f)

	// genesis is set once, from the live file's first complete line, in the
	// same transaction as the first ingested batch.
	newGenesis := genesis.String
	if newGenesis == "" && firstOK {
		newGenesis = firstHash
	}

	lineStart := offset // logical
	linesInBatch := 0
	skipped := 0
	flush := func() error {
		if _, err := tx.Exec(`UPDATE ingest_sources SET offset = ?, genesis = ?, updated_at = ? WHERE source = ?`,
			lineStart, nullableString(newGenesis), nowUTC(), def.name); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.setWatermark(def.name, lineStart)
		return nil
	}
	for sc.Scan() {
		line := sc.Bytes()
		if err := def.insertLine(tx, lineStart, line, false); err != nil {
			parseErr, ok := errors.AsType[*lineParseError](err)
			if !ok {
				return false, fmt.Errorf("insert line at offset %d: %w", lineStart, err)
			}
			skipped++
			s.warnf("history: %s journal line at offset %d is unparseable and was skipped: %v", def.name, lineStart, parseErr.err)
		}
		lineStart += int64(len(line)) + 1
		linesInBatch++
		if linesInBatch >= ingestBatchLines {
			if err := flush(); err != nil {
				return false, err
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if tx, err = s.db.Begin(); err != nil {
				return false, err
			}
			linesInBatch = 0
		}
	}
	scanErr := sc.Err()
	if err := flush(); err != nil {
		return false, err
	}
	if scanErr != nil {
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return false, fmt.Errorf("line at offset %d exceeds the %d-byte cap; source halted at that offset", lineStart, scanBufMax)
		}
		return false, scanErr
	}
	if def.name == sourceOrders {
		// Capture the generation after scanning. If the file grew beyond the
		// committed offset, OrdersFresh's size comparison still rejects it;
		// if the path was replaced, os.SameFile rejects it.
		if finalInfo, statErr := f.Stat(); statErr == nil && sameOrderFileGeneration(st, finalInfo) {
			s.setOrderGeneration(finalInfo, firstHash, firstOK)
		} else {
			// A concurrent append or rewrite means this pass did not observe
			// one stable generation. The next kick/no-op pass performs an
			// exact DB/file comparison before enabling indexed reads.
			s.clearOrderGeneration()
		}
	}
	if skipped > 0 {
		s.warnf("history: %s ingest skipped %d unparseable line(s); offsets advanced past them", def.name, skipped)
	}
	return false, nil
}

// orderIndexMatchesJournal performs the cold-start/equal-size generation
// proof. A persisted byte count and first-line hash cannot distinguish a
// replacement that preserves both, so when no validated in-memory file
// identity exists we compare every indexed raw line with the evidence
// file before allowing the fast path. This is a startup/replacement cost,
// not a hot-read cost.
func orderIndexMatchesJournal(tx *sql.Tx, f *os.File) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	rows, err := tx.Query(`SELECT raw_json FROM order_events ORDER BY src_offset`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	sc := newLineScanner(f)
	for rows.Next() {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return false, err
			}
			return false, nil
		}
		var indexed []byte
		if err := rows.Scan(&indexed); err != nil {
			return false, err
		}
		if !bytes.Equal(indexed, sc.Bytes()) {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if sc.Scan() {
		return false, nil
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// scanCompleteLines is a bufio.SplitFunc that emits only newline-terminated
// lines (without the '\n'). A torn trailing line is deliberately never
// emitted: at EOF the scanner just stops, the leftover bytes stay
// unconsumed, and the stored offset holds at the last complete line — a
// mid-write crash is indistinguishable from a slow writer and both are
// safe.
func scanCompleteLines(data []byte, _ bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	return 0, nil, nil
}

// lineParseError marks a complete journal line the parser could not map to
// rows. The ingester logs, counts, and skips it — the offset still
// advances, so one bad line cannot wedge its source.
type lineParseError struct{ err error }

func (e *lineParseError) Error() string { return e.err.Error() }

// firstLineHash returns the hex SHA-256 of the journal's first complete
// line (without its '\n'), reading at most scanBufMax bytes. false when no
// complete first line exists yet. Used as the genesis marker that detects
// a replaced journal whose size happens to exceed the stored offset.
func firstLineHash(f *os.File) (string, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	h := sha256.New()
	buf := make([]byte, scanBufInitial)
	total := 0
	for total <= scanBufMax {
		n, err := f.Read(buf)
		if n > 0 {
			if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
				h.Write(buf[:i])
				return hex.EncodeToString(h.Sum(nil)), true
			}
			h.Write(buf[:n])
			total += n
		}
		if err != nil {
			return "", false
		}
	}
	return "", false
}

// regimeLine is this package's own minimal decode of one
// regime-decisions.jsonl line. It deliberately does NOT import the
// daemon's writer structs (import cycle); drift between writer and this
// parser is pinned by round-trip tests in internal/daemon.
type regimeLine struct {
	TS          string `json:"ts"`
	SessionKey  string `json:"session_key"`
	Fingerprint string `json:"fingerprint"`
	TapeSession string `json:"tape_session"`
	Stage       string `json:"stage"`
	Severity    string `json:"severity"`
	Readiness   string `json:"readiness"`
	Confidence  string `json:"confidence"`
	Verdict     string `json:"verdict"`
	Composite   struct {
		ClusterRedCount         int `json:"cluster_red_count"`
		ClusterYellowCount      int `json:"cluster_yellow_count"`
		ClusterEligibleRedCount int `json:"cluster_eligible_red_count"`
	} `json:"composite"`
	Indicators map[string]regimeLineIndicator `json:"indicators"`
}

type regimeLineIndicator struct {
	Status          string   `json:"status"`
	Band            string   `json:"band"`
	Value           *float64 `json:"value"`
	Depth           *float64 `json:"depth"`
	StreakSessions  *int     `json:"streak_sessions"`
	Freshness       string   `json:"freshness"`
	Eligible        *bool    `json:"eligible"`
	Latched         bool     `json:"latched"`
	ThresholdsLabel string   `json:"thresholds_label"`
}

// rulesLine is the minimal decode of one rules-decisions.jsonl line (the
// writer emits a flat map; see internal/daemon/rulebook.go).
type rulesLine struct {
	At                string `json:"at"`
	Rule              string `json:"rule"`
	Status            string `json:"status"`
	Was               string `json:"was"`
	Evidence          string `json:"evidence"`
	PolicyID          string `json:"policy_id"`
	PolicyVersion     *int   `json:"policy_version"`
	PolicyFingerprint string `json:"policy_fingerprint"`
}

// insertRegimeLine maps one complete regime journal line to its decision
// row plus up to one indicator row per journal indicator. raw_json keeps
// the line verbatim so future calibration questions are json_extract()
// queries, not schema migrations.
func insertRegimeLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l regimeLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.TS)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("ts %q: %w", l.TS, err)}
	}
	res, err := tx.Exec(insertVerb(replay)+` INTO regime_decisions
(src_offset, at, at_unix_ms, session_key, fingerprint, tape_session, stage, severity, readiness, confidence, verdict,
 cluster_red_count, cluster_yellow_count, cluster_eligible_red_count, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.TS, at.UnixMilli(), l.SessionKey, l.Fingerprint, l.TapeSession, l.Stage, l.Severity,
		l.Readiness, l.Confidence, l.Verdict,
		l.Composite.ClusterRedCount, l.Composite.ClusterYellowCount, l.Composite.ClusterEligibleRedCount,
		string(line))
	if err != nil {
		return err
	}
	if replay {
		// An ignored replay insert means this line (and its indicator rows,
		// committed in the same original batch) is already present.
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(l.Indicators)) {
		ind := l.Indicators[name]
		if _, err := tx.Exec(`INSERT INTO regime_indicators
(decision_id, indicator, status, band, value, depth, streak_sessions, freshness, eligible, latched, thresholds_label)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, name, ind.Status, ind.Band, ind.Value, ind.Depth, ind.StreakSessions,
			ind.Freshness, ind.Eligible, ind.Latched, ind.ThresholdsLabel); err != nil {
			return err
		}
	}
	return nil
}

// insertRulesLine maps one complete rules journal line to its transition
// row, raw line kept verbatim.
func insertRulesLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l rulesLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.At)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("at %q: %w", l.At, err)}
	}
	_, err = tx.Exec(insertVerb(replay)+` INTO rule_transitions
(src_offset, at, at_unix_ms, rule_id, status, was, evidence, policy_id, policy_version, policy_fingerprint, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.At, at.UnixMilli(), l.Rule, l.Status, l.Was, l.Evidence,
		l.PolicyID, l.PolicyVersion, l.PolicyFingerprint, string(line))
	return err
}

// stressLine is the minimal decode of one canary-decisions.jsonl line
// (writer: internal/daemon/canary_decisions.go; drift pinned by daemon
// round-trip tests).
type stressLine struct {
	TS                     string `json:"ts"`
	SessionKey             string `json:"session_key"`
	Fingerprint            string `json:"fingerprint"`
	Account                string `json:"account"`
	AccountMode            string `json:"account_mode"`
	Action                 string `json:"action"`
	Severity               string `json:"severity"`
	Direction              string `json:"direction"`
	PortfolioAlertRelevant *bool  `json:"portfolio_alert_relevant"`
	InputHealth            string `json:"input_health"`
	Summary                string `json:"summary"`
	Market                 struct {
		RegimePosture struct {
			Stage string `json:"stage"`
		} `json:"regime_posture"`
	} `json:"market"`
}

func insertStressLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l stressLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.TS)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("ts %q: %w", l.TS, err)}
	}
	_, err = tx.Exec(insertVerb(replay)+` INTO stress_transitions
(src_offset, at, at_unix_ms, session_key, fingerprint, account, account_mode, action, severity, direction, market_stage,
 portfolio_alert_relevant, input_health, summary, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.TS, at.UnixMilli(), l.SessionKey, l.Fingerprint, l.Account, l.AccountMode,
		l.Action, l.Severity, l.Direction, l.Market.RegimePosture.Stage,
		nullableBoolInt(l.PortfolioAlertRelevant), l.InputHealth, l.Summary, string(line))
	return err
}

// capitalLine is the minimal decode of one capital-events.jsonl line
// (writer: capitalEventV1 in internal/daemon/risk_capital_state.go).
type capitalLine struct {
	At          string   `json:"at"`
	Type        string   `json:"type"`
	AmountBase  *float64 `json:"amount_base"`
	EffectiveAt string   `json:"effective_at"`
	Note        string   `json:"note"`
	Origin      string   `json:"origin"`
	ReportID    string   `json:"report_id"`
	CoverageTo  string   `json:"coverage_to"`
}

func insertCapitalLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l capitalLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.At)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("at %q: %w", l.At, err)}
	}
	_, err = tx.Exec(insertVerb(replay)+` INTO capital_events
(src_offset, at, at_unix_ms, type, amount_base, effective_at, note, origin, report_id, coverage_to, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.At, at.UnixMilli(), l.Type, l.AmountBase, nullableString(l.EffectiveAt),
		l.Note, l.Origin, l.ReportID, nullableString(l.CoverageTo), string(line))
	return err
}

// riskPolicyLine is the minimal decode of one risk-policy-journal.jsonl
// line: a flat map with a "kind" discriminator; policy_fingerprint is
// always a string (constitution fingerprint key).
type riskPolicyLine struct {
	At                string `json:"at"`
	Kind              string `json:"kind"`
	PolicyID          string `json:"policy_id"`
	PolicyVersion     *int   `json:"policy_version"`
	PolicyFingerprint string `json:"policy_fingerprint"`
}

func insertRiskPolicyLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l riskPolicyLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.At)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("at %q: %w", l.At, err)}
	}
	_, err = tx.Exec(insertVerb(replay)+` INTO risk_policy_events
(src_offset, at, at_unix_ms, kind, policy_id, policy_version, policy_fingerprint, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.At, at.UnixMilli(), l.Kind, l.PolicyID, l.PolicyVersion, l.PolicyFingerprint, string(line))
	return err
}

// proposalOutcomeLine is the minimal decode of one
// trade-proposal-outcomes.jsonl line (writer: proposalOutcomeMark). The
// policy fingerprint is the {version,key} object's key.
type proposalOutcomeLine struct {
	At                string  `json:"at"`
	MarkDate          string  `json:"mark_date"`
	State             string  `json:"state"`
	ProposalKey       string  `json:"proposal_key"`
	Revision          string  `json:"revision"`
	Bucket            string  `json:"bucket"`
	Symbol            string  `json:"symbol"`
	SecType           string  `json:"sec_type"`
	Action            string  `json:"action"`
	Quantity          float64 `json:"quantity"`
	OrderRef          string  `json:"order_ref"`
	PreviewTokenID    string  `json:"preview_token_id"`
	ExecID            string  `json:"exec_id"`
	PolicyID          string  `json:"policy_id"`
	PolicyVersion     *int    `json:"policy_version"`
	PolicyFingerprint struct {
		Key string `json:"key"`
	} `json:"policy_fingerprint"`
	BaselinePrice   float64 `json:"baseline_price"`
	MarkPrice       float64 `json:"mark_price"`
	AvgFillPrice    float64 `json:"avg_fill_price"`
	ExecutionPnL    float64 `json:"execution_pnl"`
	BenchmarkSymbol string  `json:"benchmark_symbol"`
}

func insertProposalOutcomeLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l proposalOutcomeLine
	if err := json.Unmarshal(line, &l); err != nil {
		return &lineParseError{err: err}
	}
	at, err := time.Parse(time.RFC3339Nano, l.At)
	if err != nil {
		return &lineParseError{err: fmt.Errorf("at %q: %w", l.At, err)}
	}
	_, err = tx.Exec(insertVerb(replay)+` INTO proposal_outcomes
(src_offset, at, at_unix_ms, mark_date, state, proposal_key, revision, bucket, symbol, sec_type, action, quantity,
 order_ref, preview_token_id, exec_id, policy_id, policy_version, policy_fingerprint,
 baseline_price, mark_price, avg_fill_price, execution_pnl, benchmark_symbol, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, l.At, at.UnixMilli(), l.MarkDate, l.State, l.ProposalKey, l.Revision, l.Bucket,
		l.Symbol, l.SecType, l.Action, l.Quantity, l.OrderRef, l.PreviewTokenID, l.ExecID,
		l.PolicyID, l.PolicyVersion, l.PolicyFingerprint.Key,
		l.BaselinePrice, l.MarkPrice, l.AvgFillPrice, l.ExecutionPnL, l.BenchmarkSymbol, string(line))
	return err
}

// orderLine is the minimal decode of one order-journal.jsonl line (writer:
// orderJournalEvent in internal/daemon/order_journal.go).
type orderLine struct {
	Version         int    `json:"version"`
	At              string `json:"at"`
	Type            string `json:"type"`
	OrderRef        string `json:"order_ref"`
	PreviewTokenID  string `json:"preview_token_id"`
	ReservedOrderID int    `json:"reserved_order_id"`
	PermID          int    `json:"perm_id"`
	Account         string `json:"account"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	SendState       string `json:"send_state"`
}

// insertOrderLine stores every complete order-journal line. A JSON-bad or
// wrong-version line is stored verbatim with parse_ok = 0 and zero-value
// columns — never skipped — so the indexed order-read path can refuse to
// serve exactly when the legacy scan would hard-fail (D2/D10).
func (s *Store) insertOrderLine(tx *sql.Tx, srcOffset int64, line []byte, replay bool) error {
	var l orderLine
	parseOK := true
	if s.opts.ValidateOrderLine == nil || s.opts.ValidateOrderLine(line) != nil {
		parseOK = false
	}
	if err := json.Unmarshal(line, &l); err != nil || l.Version != 1 {
		parseOK = false
	}
	if !parseOK {
		l = orderLine{Version: l.Version}
	}
	var atText string
	var atMS int64
	if parseOK {
		if at, err := time.Parse(time.RFC3339Nano, l.At); err == nil {
			atText = l.At
			atMS = at.UnixMilli()
		}
	}
	_, err := tx.Exec(insertVerb(replay)+` INTO order_events
(src_offset, at, at_unix_ms, parse_ok, version, type, order_ref, preview_token_id, reserved_order_id, perm_id,
 account, mode, status, send_state, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srcOffset, atText, atMS, boolInt(parseOK), l.Version, l.Type, l.OrderRef, l.PreviewTokenID,
		l.ReservedOrderID, l.PermID, l.Account, l.Mode, l.Status, l.SendState, string(line))
	if err == nil && !parseOK {
		s.setOrdersParseBad(true)
		s.warnf("history: orders journal line at offset %d is unparseable or wrong-version; stored with parse_ok=0 (indexed order reads disabled until resolved)", srcOffset)
	}
	return err
}

func insertVerb(replay bool) string {
	if replay {
		return "INSERT OR IGNORE"
	}
	return "INSERT"
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableBoolInt(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
