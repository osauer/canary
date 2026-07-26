package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// RegimeQuery filters RegimeHistory. Since (inclusive) and Until
// (exclusive) are resolved UTC instants — the RPC layer owns the
// YYYY-MM-DD / RFC3339 grammar and defaults. Empty Stage matches every
// lifecycle stage. Limit caps returned rows, newest first.
type RegimeQuery struct {
	Since time.Time
	Until time.Time
	Stage string
	Limit int
}

// RulesQuery filters RulesHistory; Rule filters on the journal's rule id
// (for example single_name_exposure). Same boundary semantics as
// RegimeQuery.
type RulesQuery struct {
	Since time.Time
	Until time.Time
	Rule  string
	Limit int
}

// StressQuery filters StressHistory; Severity and Action filter on the
// journal's exact words. Same boundary semantics as RegimeQuery.
type StressQuery struct {
	Since    time.Time
	Until    time.Time
	Severity string
	Action   string
	Limit    int
}

// EquityQuery filters EquityDays on the derived statement-equity series.
type EquityQuery struct {
	Since time.Time
	Until time.Time
	Limit int
}

// RegimeHistory returns indexed regime decisions in [Since, Until),
// newest first, plus the total match count before the limit cut. Free
// text (verdicts) is returned as data for rendering, never parsed into
// authority.
func (s *Store) RegimeHistory(q RegimeQuery) ([]rpc.RegimeHistoryEntry, int, error) {
	where := " WHERE at_unix_ms >= ? AND at_unix_ms < ?"
	args := []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	if q.Stage != "" {
		where += " AND stage = ?"
		args = append(args, q.Stage)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var total int
	if err := tx.QueryRow("SELECT COUNT(*) FROM regime_decisions"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(`SELECT at, at_unix_ms, session_key, tape_session, stage, severity, readiness, confidence, verdict,
 cluster_red_count, cluster_yellow_count, cluster_eligible_red_count, fingerprint
FROM regime_decisions`+where+" ORDER BY at_unix_ms DESC, id DESC LIMIT ?", append(args, q.Limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var entries []rpc.RegimeHistoryEntry
	for rows.Next() {
		var at sql.NullString
		var atMS int64
		var sessionKey, tapeSession, stage, severity, readiness, confidence, verdict, fingerprint sql.NullString
		var red, yellow, eligibleRed sql.NullInt64
		if err := rows.Scan(&at, &atMS, &sessionKey, &tapeSession, &stage, &severity, &readiness, &confidence, &verdict,
			&red, &yellow, &eligibleRed, &fingerprint); err != nil {
			return nil, 0, err
		}
		entries = append(entries, rpc.RegimeHistoryEntry{
			At:                 parseJournalTime(at.String, atMS),
			SessionKey:         sessionKey.String,
			TapeSession:        tapeSession.String,
			Stage:              stage.String,
			Severity:           severity.String,
			Readiness:          readiness.String,
			Confidence:         confidence.String,
			Verdict:            verdict.String,
			ClusterRed:         int(red.Int64),
			ClusterYellow:      int(yellow.Int64),
			ClusterEligibleRed: int(eligibleRed.Int64),
			Fingerprint:        fingerprint.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, tx.Commit()
}

// RulesHistory returns indexed rule transitions in [Since, Until), newest
// first, plus the total match count before the limit cut.
func (s *Store) RulesHistory(q RulesQuery) ([]rpc.RuleTransitionEntry, int, error) {
	where := " WHERE at_unix_ms >= ? AND at_unix_ms < ?"
	args := []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	if q.Rule != "" {
		where += " AND rule_id = ?"
		args = append(args, q.Rule)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var total int
	if err := tx.QueryRow("SELECT COUNT(*) FROM rule_transitions"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(`SELECT at, at_unix_ms, rule_id, status, was, evidence, policy_id, policy_version, policy_fingerprint
FROM rule_transitions`+where+" ORDER BY at_unix_ms DESC, id DESC LIMIT ?", append(args, q.Limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var entries []rpc.RuleTransitionEntry
	for rows.Next() {
		var at sql.NullString
		var atMS int64
		var ruleID, status, was, evidence, policyID, policyFingerprint sql.NullString
		var policyVersion sql.NullInt64
		if err := rows.Scan(&at, &atMS, &ruleID, &status, &was, &evidence, &policyID, &policyVersion, &policyFingerprint); err != nil {
			return nil, 0, err
		}
		entries = append(entries, rpc.RuleTransitionEntry{
			At:                parseJournalTime(at.String, atMS),
			Rule:              ruleID.String,
			Status:            status.String,
			Was:               was.String,
			Evidence:          evidence.String,
			PolicyID:          policyID.String,
			PolicyVersion:     int(policyVersion.Int64),
			PolicyFingerprint: policyFingerprint.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, tx.Commit()
}

// StressHistory returns indexed stress decisions in [Since, Until),
// newest first, plus the total match count before the limit cut. Summary
// text is journal evidence for display, never parsed into authority.
func (s *Store) StressHistory(q StressQuery) ([]rpc.StressHistoryEntry, int, error) {
	where := " WHERE at_unix_ms >= ? AND at_unix_ms < ?"
	args := []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	if q.Severity != "" {
		where += " AND severity = ?"
		args = append(args, q.Severity)
	}
	if q.Action != "" {
		where += " AND action = ?"
		args = append(args, q.Action)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var total int
	if err := tx.QueryRow("SELECT COUNT(*) FROM stress_transitions"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(`SELECT at, at_unix_ms, session_key, fingerprint, account, account_mode, action, severity, direction,
 market_stage, portfolio_alert_relevant, input_health, summary
FROM stress_transitions`+where+" ORDER BY at_unix_ms DESC, id DESC LIMIT ?", append(args, q.Limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var entries []rpc.StressHistoryEntry
	for rows.Next() {
		var at sql.NullString
		var atMS int64
		var sessionKey, fingerprint, account, accountMode, action, severity, direction, stage, inputHealth, summary sql.NullString
		var alertRelevant sql.NullInt64
		if err := rows.Scan(&at, &atMS, &sessionKey, &fingerprint, &account, &accountMode, &action, &severity, &direction,
			&stage, &alertRelevant, &inputHealth, &summary); err != nil {
			return nil, 0, err
		}
		entry := rpc.StressHistoryEntry{
			At:          parseJournalTime(at.String, atMS),
			SessionKey:  sessionKey.String,
			Fingerprint: fingerprint.String,
			Account:     account.String,
			AccountMode: accountMode.String,
			Action:      action.String,
			Severity:    severity.String,
			Direction:   direction.String,
			MarketStage: stage.String,
			InputHealth: inputHealth.String,
			Summary:     summary.String,
		}
		if alertRelevant.Valid {
			entry.PortfolioAlertRelevant = new(alertRelevant.Int64 != 0)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, tx.Commit()
}

// EquityDays returns statement-derived equity days whose UTC day falls in
// [Since, Until), newest day first, plus the total match count before the
// limit cut.
func (s *Store) EquityDays(q EquityQuery) ([]rpc.EquityDayEntry, int, error) {
	sinceDay := q.Since.UTC().Format("2006-01-02")
	untilDay := q.Until.UTC().Add(-time.Nanosecond).Format("2006-01-02")
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var total int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM statement_equity_days WHERE day >= ? AND day <= ?`, sinceDay, untilDay).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(`SELECT account_id, day, equity_base, source_stmt, when_generated
FROM statement_equity_days WHERE day >= ? AND day <= ? ORDER BY day DESC, account_id LIMIT ?`, sinceDay, untilDay, q.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var entries []rpc.EquityDayEntry
	for rows.Next() {
		var accountID, day, sourceStmt, whenGenerated string
		var equity float64
		if err := rows.Scan(&accountID, &day, &equity, &sourceStmt, &whenGenerated); err != nil {
			return nil, 0, err
		}
		entry := rpc.EquityDayEntry{
			Day:        day,
			AccountID:  accountID,
			EquityBase: equity,
			SourceStmt: sourceStmt,
		}
		if t, perr := time.Parse(time.RFC3339, whenGenerated); perr == nil {
			entry.WhenGenerated = t
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, tx.Commit()
}

// CapitalEvents returns declared capital events in [since, until), newest
// first, capped at limit; truncated reports whether more matched.
func (s *Store) CapitalEvents(since, until time.Time, limit int) ([]rpc.CapitalEventEntry, bool, error) {
	rows, err := s.db.Query(`SELECT at, at_unix_ms, type, amount_base, effective_at, note, origin, report_id
FROM capital_events WHERE at_unix_ms >= ? AND at_unix_ms < ? ORDER BY at_unix_ms DESC, id DESC LIMIT ?`,
		since.UnixMilli(), until.UnixMilli(), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var entries []rpc.CapitalEventEntry
	for rows.Next() {
		var at, typ, effectiveAt, note, origin, reportID sql.NullString
		var atMS int64
		var amount sql.NullFloat64
		if err := rows.Scan(&at, &atMS, &typ, &amount, &effectiveAt, &note, &origin, &reportID); err != nil {
			return nil, false, err
		}
		entry := rpc.CapitalEventEntry{
			At:       parseJournalTime(at.String, atMS),
			Type:     typ.String,
			Note:     note.String,
			Origin:   origin.String,
			ReportID: reportID.String,
		}
		if amount.Valid {
			entry.AmountBase = amount.Float64
		}
		if effectiveAt.Valid {
			if t, perr := time.Parse(time.RFC3339Nano, effectiveAt.String); perr == nil {
				entry.EffectiveAt = t
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	return entries, truncated, nil
}

// OrdersFresh reports whether order_events provably mirrors the entire
// order journal at this instant: the on-disk size equals the committed
// logical watermark (base is permanently 0 for orders) and no parse-marker
// rows were recorded. The live path must also still name the exact file
// generation whose identity and genesis were validated by the ingest
// pass; equal-size replacement is therefore stale. It never touches
// SQLite, so hot order paths cannot block on an ingest transaction.
func (s *Store) OrdersFresh() bool {
	if s == nil || s.opts.OrderJournalPath == "" || s.opts.ValidateOrderLine == nil {
		return false
	}
	f, err := os.Open(s.opts.OrderJournalPath)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	firstHash, firstOK := firstLineHash(f)
	changeIdentity, changeIdentityOK := orderChangeIdentityFor(st)
	wm, watermarkOK, bad, generation, generationOK := s.ordersFreshState()
	if !watermarkOK || bad || !generationOK || generation.info == nil ||
		!changeIdentityOK || !generation.changeIdentityOK {
		return false
	}
	return st.Size() == wm &&
		changeIdentity == generation.changeIdentity &&
		st.Size() == generation.info.Size() &&
		st.ModTime().Equal(generation.info.ModTime()) &&
		os.SameFile(st, generation.info) &&
		firstOK == generation.genesisOK && firstHash == generation.genesis
}

// OrderEventLines returns raw order-journal lines from the index in
// journal order (by id), optionally pruned on the widened at_unix_ms
// range the caller supplies. It re-verifies the no-parse-marker invariant
// inside the same transaction; any parse-marker row is an error so the
// caller falls back to the legacy scan, which fails loudly on the same
// line.
func (s *Store) OrderEventLines(sinceMS, untilMS *int64) ([][]byte, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ordersParseMarkerCheck(tx); err != nil {
		return nil, err
	}
	query := `SELECT raw_json FROM order_events`
	var args []any
	if sinceMS != nil && untilMS != nil {
		query += ` WHERE at_unix_ms BETWEEN ? AND ?`
		args = append(args, *sinceMS, *untilMS)
	}
	query += ` ORDER BY id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := [][]byte{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		lines = append(lines, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lines, tx.Commit()
}

// OrderEventsForToken returns raw order-journal lines carrying the given
// preview token id, in journal order, with the same parse-marker
// verification as OrderEventLines. The context bounds the query so an
// in-flight backfill can only cause a fallback, never a stall.
func (s *Store) OrderEventsForToken(ctx context.Context, tokenID string) ([][]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ordersParseMarkerCheck(tx); err != nil {
		return nil, err
	}
	// The `<> ''` repeats order_events_token's partial-index predicate so
	// SQLite can use it; without it the plan degrades to a full scan of a
	// never-rotated table, inside the order-journal lock.
	rows, err := tx.QueryContext(ctx, `SELECT raw_json FROM order_events WHERE preview_token_id = ? AND preview_token_id <> '' ORDER BY id`, tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := [][]byte{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		lines = append(lines, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lines, tx.Commit()
}

// MaxReservedOrderID returns the highest reserved broker order id in
// order_events (0 when none), with the parse-marker verification.
func (s *Store) MaxReservedOrderID() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ordersParseMarkerCheck(tx); err != nil {
		return 0, err
	}
	var maxID int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(reserved_order_id), 0) FROM order_events WHERE reserved_order_id > 0`).Scan(&maxID); err != nil {
		return 0, err
	}
	return maxID, tx.Commit()
}

func ordersParseMarkerCheck(tx *sql.Tx) error {
	var bad bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM order_events WHERE parse_ok = 0)`).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return errors.New("order_events contains parse-marker rows; serve from the journal scan")
	}
	return nil
}

// Health reports index freshness for one journal-backed source: the
// bookkeeping row's ingested-byte watermark (physical live-file bytes,
// i.e. offset - base, so the comparison against the on-disk size stays
// exact after rotation) and last ingest time against the journal's
// current size. Surfaces disclose the gap instead of presenting the index
// as silently fresh.
func (s *Store) Health(source string) (rpc.HistoryIndexHealth, error) {
	var h rpc.HistoryIndexHealth
	var journalPath string
	switch source {
	case sourceRegime:
		journalPath = s.opts.RegimeJournalPath
	case sourceRules:
		journalPath = s.opts.RulesJournalPath
	case sourceStress:
		journalPath = s.opts.StressJournalPath
	case sourceCapital:
		journalPath = s.opts.CapitalJournalPath
	case sourceRiskPolicy:
		journalPath = s.opts.RiskPolicyJournalPath
	case sourceProposalOutcomes:
		journalPath = s.opts.ProposalOutcomesPath
	case sourceOrders:
		journalPath = s.opts.OrderJournalPath
	default:
		return h, fmt.Errorf("history: unknown source %q", source)
	}
	var offset, base int64
	var updated sql.NullString
	err := s.db.QueryRow(`SELECT offset, base, updated_at FROM ingest_sources WHERE source = ?`, source).Scan(&offset, &base, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	h.IngestedBytes = offset - base
	if updated.Valid {
		if t, perr := time.Parse(time.RFC3339Nano, updated.String); perr == nil {
			h.LastIngestAt = t
		}
	}
	if st, err := os.Stat(journalPath); err == nil {
		h.JournalBytes = st.Size()
	}
	return h, nil
}

// StatementsHealth reports the statement file-set variant of index
// health: summed on-disk XML bytes against summed recorded bytes, with
// the newest ingest time. A statement retained but not yet parsed shows
// as a byte gap.
func (s *Store) StatementsHealth() (rpc.HistoryIndexHealth, error) {
	var h rpc.HistoryIndexHealth
	rows, err := s.db.Query(`SELECT size, ingested_at FROM statement_files`)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var size int64
		var ingestedAt string
		if err := rows.Scan(&size, &ingestedAt); err != nil {
			return h, err
		}
		h.IngestedBytes += size
		if t, perr := time.Parse(time.RFC3339Nano, ingestedAt); perr == nil && t.After(h.LastIngestAt) {
			h.LastIngestAt = t
		}
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	if entries, err := os.ReadDir(s.opts.StatementsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
				continue
			}
			if info, err := e.Info(); err == nil {
				h.JournalBytes += info.Size()
			}
		}
	}
	return h, nil
}

// parseJournalTime prefers the verbatim journal timestamp (original UTC
// offset preserved); the canonical unix-ms column is the fallback for a
// row whose stored text no longer parses.
func parseJournalTime(raw string, unixMS int64) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	return time.UnixMilli(unixMS).UTC()
}
