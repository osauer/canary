package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// GetStateDocument returns the current verified document for scopeKey and kind.
// The boolean is false when no document exists; a digest mismatch is an error,
// never absence.
func (s *Store) GetStateDocument(ctx context.Context, scopeKey, kind string) (StateDocument, bool, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return StateDocument{}, false, err
	}
	if err := validateKey("state kind", kind, 128); err != nil {
		return StateDocument{}, false, err
	}
	var doc StateDocument
	var storedDigest []byte
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT revision, document_json, document_sha256, updated_at
FROM state_documents WHERE scope_key=? AND kind=?`, scopeKey, kind).Scan(&doc.Revision, &doc.JSON, &storedDigest, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return StateDocument{}, false, nil
	}
	if err != nil {
		return StateDocument{}, false, fmt.Errorf("get state document: %w", s.latchCorruptRead(err))
	}
	digest := sha256.Sum256(doc.JSON)
	if len(storedDigest) != sha256.Size || !bytes.Equal(storedDigest, digest[:]) {
		return StateDocument{}, false, errorsf("stored state document digest does not match content")
	}
	doc.ScopeKey = scopeKey
	doc.Kind = kind
	doc.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return StateDocument{}, false, fmt.Errorf("decode state timestamp: %w", err)
	}
	return doc, true, nil
}

// CompareAndSwapStateDocument commits one revision and advances the authority
// head atomically. A stale expected revision returns RevisionConflictError.
func (s *Store) CompareAndSwapStateDocument(ctx context.Context, update StateDocumentCAS) (StateDocument, error) {
	if err := validateStateCAS(update); err != nil {
		return StateDocument{}, err
	}
	var saved StateDocument
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		saved, err = compareAndSwapStateTx(ctx, tx, update, now)
		if err != nil {
			return err
		}
		if _, err := advanceHeadTx(ctx, tx, 0, now); err != nil {
			return fmt.Errorf("advance authority head: %w", err)
		}
		return nil
	})
	return saved, err
}

func compareAndSwapStateTx(ctx context.Context, tx *sql.Tx, update StateDocumentCAS, now time.Time) (StateDocument, error) {
	if update.ExpectedHead != nil {
		current, err := readAuthorityHead(ctx, tx)
		if err != nil {
			return StateDocument{}, err
		}
		if current != *update.ExpectedHead {
			return StateDocument{}, ErrAuthorityMismatch
		}
	}
	if !update.UpdatedAtNotBefore.IsZero() && now.Before(update.UpdatedAtNotBefore) {
		return StateDocument{}, fmt.Errorf("%w: state document commit clock precedes required floor", ErrRollback)
	}
	next := update.ExpectedRevision + 1
	digest := sha256.Sum256(update.JSON)
	var result sql.Result
	var err error
	if update.ExpectedRevision == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO state_documents(scope_key, kind, revision, document_json, document_sha256, updated_at)
VALUES (?, ?, 1, ?, ?, ?) ON CONFLICT(scope_key, kind) DO NOTHING`, update.ScopeKey, update.Kind, update.JSON, digest[:], formatTime(now))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE state_documents SET revision=?, document_json=?, document_sha256=?, updated_at=?
WHERE scope_key=? AND kind=? AND revision=?`, next, update.JSON, digest[:], formatTime(now), update.ScopeKey, update.Kind, update.ExpectedRevision)
	}
	if err != nil {
		return StateDocument{}, fmt.Errorf("write state document: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return StateDocument{}, err
	}
	if changed != 1 {
		var actual int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM state_documents WHERE scope_key=? AND kind=?`, update.ScopeKey, update.Kind).Scan(&actual)
		if errors.Is(err, sql.ErrNoRows) {
			return StateDocument{}, &RevisionConflictError{Expected: update.ExpectedRevision}
		}
		if err != nil {
			return StateDocument{}, err
		}
		return StateDocument{}, &RevisionConflictError{Expected: update.ExpectedRevision, Actual: actual, Exists: true}
	}
	return StateDocument{ScopeKey: update.ScopeKey, Kind: update.Kind, Revision: next, JSON: append([]byte(nil), update.JSON...), UpdatedAt: now}, nil
}

func validateStateCAS(update StateDocumentCAS) error {
	if err := validateStateCASCoordinates(update); err != nil {
		return err
	}
	if !json.Valid(update.JSON) {
		return errorsf("state document must be valid JSON")
	}
	return nil
}

func validateStateCASCoordinates(update StateDocumentCAS) error {
	if err := validateKey("scope key", update.ScopeKey, 512); err != nil {
		return err
	}
	if err := validateKey("state kind", update.Kind, 128); err != nil {
		return err
	}
	if update.ExpectedRevision < 0 {
		return errorsf("expected revision must not be negative")
	}
	return nil
}

func validateKey(label, value string, limit int) error {
	if value == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("corestore: %s is required", label)
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return fmt.Errorf("corestore: %s is invalid", label)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("corestore: %s is invalid", label)
		}
	}
	return nil
}
