package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetEvent retrieves one immutable receipt by its unique scope/key and verifies
// its stored content digest. A corrupt receipt is never treated as absent.
func (s *Store) GetEvent(ctx context.Context, scope, key string) (EventRecord, bool, error) {
	if err := validateKey("scope key", scope, 512); err != nil {
		return EventRecord{}, false, err
	}
	if err := validateKey("event key", key, 512); err != nil {
		return EventRecord{}, false, err
	}
	var out EventRecord
	var digest []byte
	var occurred, recorded string
	err := s.db.QueryRowContext(ctx, `SELECT event_seq,scope_key,event_key,event_type,action_kind,origin,occurred_at,recorded_at,payload_json,payload_sha256 FROM event_log WHERE scope_key=? AND event_key=?`, scope, key).Scan(&out.EventSeq, &out.ScopeKey, &out.EventKey, &out.Type, &out.Action, &out.Origin, &occurred, &recorded, &out.PayloadJSON, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return EventRecord{}, false, nil
	}
	if err != nil {
		return EventRecord{}, false, err
	}
	want := sha256.Sum256(out.PayloadJSON)
	if !bytes.Equal(digest, want[:]) {
		return EventRecord{}, false, errorsf("stored event digest does not match content")
	}
	if out.OccurredAt, err = parseTime(occurred); err != nil {
		return EventRecord{}, false, err
	}
	if out.RecordedAt, err = parseTime(recorded); err != nil {
		return EventRecord{}, false, err
	}
	return out, true, nil
}

// AppendEventsAtStateRevision records a semantic no-op receipt only while the
// named state revision still matches. It advances the audit head without
// rewriting or upgrading the state document. Revision zero fences the absence
// of an initial document; it never replaces an existing revision.
func (s *Store) AppendEventsAtStateRevision(ctx context.Context, update StateDocumentCAS, inputs []EventInput) ([]EventReceipt, error) {
	if err := validateStateCASCoordinates(update); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, errorsf("at least one event is required")
	}
	for _, in := range inputs {
		if err := validateEventInput(in); err != nil {
			return nil, err
		}
	}
	var receipts []EventReceipt
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		var revision int64
		var raw, digest []byte
		err := tx.QueryRowContext(ctx, `SELECT revision,document_json,document_sha256 FROM state_documents WHERE scope_key=? AND kind=?`, update.ScopeKey, update.Kind).Scan(&revision, &raw, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			if update.ExpectedRevision != 0 {
				return &RevisionConflictError{Expected: update.ExpectedRevision}
			}
		} else {
			if err != nil {
				return err
			}
			if revision != update.ExpectedRevision {
				return &RevisionConflictError{Expected: update.ExpectedRevision, Actual: revision, Exists: true}
			}
			want := sha256.Sum256(raw)
			if !bytes.Equal(digest, want[:]) {
				return errorsf("stored state document digest does not match content")
			}
		}
		now := time.Now().UTC()
		receipts, err = appendEventsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		head, err := advanceHeadTx(ctx, tx, receipts[len(receipts)-1].EventSeq, now)
		if err != nil {
			return fmt.Errorf("advance receipt authority head: %w", err)
		}
		for i := range receipts {
			receipts[i].Head = head
		}
		return nil
	})
	return receipts, err
}

// WithAcceptedStateDocument reads a verified document and publishes its view
// while excluding all critical writers. A latched authority never invokes
// accept. The callback must not call mutation methods on this Store.
func (s *Store) WithAcceptedStateDocument(ctx context.Context, scope, kind string, accept func(StateDocument, bool) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Health().Ready {
		return ErrBlocked
	}
	doc, found, err := s.GetStateDocument(ctx, scope, kind)
	if err != nil {
		return err
	}
	return accept(doc, found)
}
