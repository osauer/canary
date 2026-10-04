package corestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	setupCoverageKind  = "setups_coverage.session.v1"
	setupCoverageScope = "setups/coverage/"
)

// SetupCoverageSessions bounds retained setup coverage documents to the most
// recent market sessions.
const SetupCoverageSessions = 30

// LoadSetupCoverage returns one session's digest-verified setup coverage
// document. Coverage is an operational instrument, never decision evidence.
func (s *Store) LoadSetupCoverage(ctx context.Context, session string) (StateDocument, bool, error) {
	if err := validateSetupCoverageSession(session); err != nil {
		return StateDocument{}, false, err
	}
	return s.GetStateDocument(ctx, setupCoverageScope+session, setupCoverageKind)
}

// SaveSetupCoverage publishes one session's coverage document and deletes
// documents beyond the newest SetupCoverageSessions sessions. The caller
// serializes read/merge/write.
func (s *Store) SaveSetupCoverage(ctx context.Context, session string, payload []byte) (StateDocument, error) {
	if err := validateSetupCoverageSession(session); err != nil {
		return StateDocument{}, err
	}
	if len(payload) > 1<<20 || !json.Valid(payload) {
		return StateDocument{}, fmt.Errorf("invalid or oversized setup coverage document")
	}
	var saved StateDocument
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		scope := setupCoverageScope + session
		var revision int64
		err := tx.QueryRowContext(ctx, "SELECT revision FROM state_documents WHERE scope_key=? AND kind=?", scope, setupCoverageKind).Scan(&revision)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		saved, err = compareAndSwapStateTx(ctx, tx, StateDocumentCAS{ScopeKey: scope, Kind: setupCoverageKind, ExpectedRevision: revision, JSON: payload}, now)
		if err != nil {
			return err
		}
		// Session dates order lexically, so the newest scopes sort last.
		if _, err = tx.ExecContext(ctx, `DELETE FROM state_documents WHERE kind=? AND scope_key NOT IN
(SELECT scope_key FROM state_documents WHERE kind=? ORDER BY scope_key DESC LIMIT ?)`, setupCoverageKind, setupCoverageKind, SetupCoverageSessions); err != nil {
			return err
		}
		_, err = advanceHeadTx(ctx, tx, 0, now)
		return err
	})
	return saved, err
}

// ListSetupCoverageSessions returns the retained session dates, newest first.
func (s *Store) ListSetupCoverageSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT scope_key FROM state_documents WHERE kind=? ORDER BY scope_key DESC LIMIT ?", setupCoverageKind, SetupCoverageSessions)
	if err != nil {
		return nil, fmt.Errorf("list setup coverage: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		if session, ok := strings.CutPrefix(scope, setupCoverageScope); ok && validateSetupCoverageSession(session) == nil {
			out = append(out, session)
		}
	}
	return out, rows.Err()
}

func validateSetupCoverageSession(session string) error {
	d, err := time.Parse(time.DateOnly, session)
	if err != nil || d.Format(time.DateOnly) != session {
		return fmt.Errorf("invalid setup coverage session %q", session)
	}
	return nil
}
