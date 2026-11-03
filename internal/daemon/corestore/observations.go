package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AppendObservation stores one immutable observation and advances the authority
// head.
func (s *Store) AppendObservation(ctx context.Context, input ObservationInput) (ObservationReceipt, error) {
	receipts, err := s.AppendObservations(ctx, []ObservationInput{input})
	if err != nil {
		return ObservationReceipt{}, err
	}
	return receipts[0], nil
}

// AppendObservations stores one batch atomically, preserving each payload's
// exact bytes alongside a digest.
func (s *Store) AppendObservations(ctx context.Context, inputs []ObservationInput) ([]ObservationReceipt, error) {
	if len(inputs) == 0 {
		return nil, errorsf("at least one observation is required")
	}
	for _, input := range inputs {
		if err := validateObservation(input); err != nil {
			return nil, err
		}
	}
	receipts := make([]ObservationReceipt, 0, len(inputs))
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		receipts, err = appendObservationsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		_, err = advanceHeadTx(ctx, tx, 0, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return receipts, nil
}

// CompareAndSwapStateDocumentWithObservations changes current state and
// appends its immutable observations under one commit and one head advance.
func (s *Store) CompareAndSwapStateDocumentWithObservations(ctx context.Context, update StateDocumentCAS, inputs []ObservationInput) (StateDocument, []ObservationReceipt, error) {
	if err := validateStateCAS(update); err != nil {
		return StateDocument{}, nil, err
	}
	if len(inputs) == 0 {
		return StateDocument{}, nil, errorsf("at least one observation is required")
	}
	for _, input := range inputs {
		if err := validateObservation(input); err != nil {
			return StateDocument{}, nil, err
		}
	}
	var saved StateDocument
	var receipts []ObservationReceipt
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		saved, err = compareAndSwapStateTx(ctx, tx, update, now)
		if err != nil {
			return err
		}
		receipts, err = appendObservationsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		_, err = advanceHeadTx(ctx, tx, 0, now)
		return err
	})
	return saved, receipts, err
}

// CompareAndSwapStateDocumentWithBoundObservations appends immutable
// observations, gives their uncommitted receipts to build, and publishes the
// resulting state document under one transaction and one head advance. This is
// the narrow path for state JSON that must name the exact observations created
// by the same commit. A build error or stale state revision rolls every
// observation back.
//
// build must be deterministic and must not call the Store. It receives the
// revision the document will have and a copy of the receipts in input order.
func (s *Store) CompareAndSwapStateDocumentWithBoundObservations(
	ctx context.Context,
	update StateDocumentCAS,
	inputs []ObservationInput,
	build func(nextRevision int64, receipts []ObservationReceipt) ([]byte, error),
) (StateDocument, []ObservationReceipt, error) {
	if err := validateStateCASCoordinates(update); err != nil {
		return StateDocument{}, nil, err
	}
	if len(update.JSON) != 0 {
		return StateDocument{}, nil, errorsf("bound state document JSON must be built from observation receipts")
	}
	if build == nil {
		return StateDocument{}, nil, errorsf("bound state document builder is required")
	}
	if len(inputs) == 0 {
		return StateDocument{}, nil, errorsf("at least one observation is required")
	}
	for _, input := range inputs {
		if err := validateObservation(input); err != nil {
			return StateDocument{}, nil, err
		}
	}

	var saved StateDocument
	var receipts []ObservationReceipt
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		receipts, err = appendObservationsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		nextRevision := update.ExpectedRevision + 1
		payload, err := build(nextRevision, append([]ObservationReceipt(nil), receipts...))
		if err != nil {
			return fmt.Errorf("build receipt-bound state document: %w", err)
		}
		if !json.Valid(payload) {
			return errorsf("receipt-bound state document must be valid JSON")
		}
		update.JSON = append([]byte(nil), payload...)
		saved, err = compareAndSwapStateTx(ctx, tx, update, now)
		if err != nil {
			return err
		}
		if saved.Revision != nextRevision {
			return errorsf("receipt-bound state revision mismatch")
		}
		_, err = advanceHeadTx(ctx, tx, 0, now)
		return err
	})
	if err != nil {
		return StateDocument{}, nil, err
	}
	return saved, receipts, nil
}

func appendObservationsTx(ctx context.Context, tx *sql.Tx, inputs []ObservationInput, now time.Time) ([]ObservationReceipt, error) {
	receipts := make([]ObservationReceipt, 0, len(inputs))
	for _, input := range inputs {
		digest := sha256.Sum256(input.Payload)
		var metadata any
		if len(input.MetadataJSON) > 0 {
			metadata = input.MetadataJSON
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO observations
(scope_key, source, kind, observed_at, observed_at_ms, recorded_at, content_type, payload, payload_sha256, metadata_json, decision_eligible)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.ScopeKey, input.Source, input.Kind,
			formatTime(input.ObservedAt), input.ObservedAt.UnixMilli(), formatTime(now), input.ContentType,
			input.Payload, digest[:], metadata, input.DecisionEligible)
		if err != nil {
			return nil, fmt.Errorf("append observation: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, ObservationReceipt{ID: id, PayloadSHA256: digest, RecordedAt: now})
	}
	return receipts, nil
}

// LatestObservation returns the newest retained observation regardless of its
// decision eligibility. The boolean is false when no row matches.
func (s *Store) LatestObservation(ctx context.Context, scopeKey, source, kind string) (Observation, bool, error) {
	return s.latestObservation(ctx, scopeKey, source, kind, nil)
}

// LatestDecisionEligibleObservation is the only observation read intended for
// a live decision path. Generic observation reads are research/inspection
// surfaces and may include quarantined legacy rows.
func (s *Store) LatestDecisionEligibleObservation(ctx context.Context, scopeKey, source, kind string) (Observation, bool, error) {
	eligible := true
	return s.latestObservation(ctx, scopeKey, source, kind, &eligible)
}

// LatestQuarantinedObservationForRecovery returns the newest explicitly
// decision-ineligible observation for a narrow startup-repair path. Callers
// must validate the full payload and preserve quarantine provenance before
// publishing any state derived from it. It is not a decision-history reader
// and must never be used as a fallback for ordinary live evaluation.
func (s *Store) LatestQuarantinedObservationForRecovery(ctx context.Context, scopeKey, source, kind string) (Observation, bool, error) {
	eligible := false
	return s.latestObservation(ctx, scopeKey, source, kind, &eligible)
}

func (s *Store) latestObservation(ctx context.Context, scopeKey, source, kind string, decisionEligible *bool) (Observation, bool, error) {
	query := ObservationQuery{ScopeKey: scopeKey, Source: source, Kind: kind, DecisionEligible: decisionEligible, Limit: 1}
	if err := validateObservationQuery(query); err != nil {
		return Observation{}, false, err
	}
	where := "scope_key=? AND source=? AND kind=?"
	args := []any{scopeKey, source, kind}
	if decisionEligible != nil {
		where += " AND decision_eligible=?"
		args = append(args, *decisionEligible)
	}
	var out Observation
	var observed, recorded string
	var digest []byte
	var eligible bool
	err := s.db.QueryRowContext(ctx, `SELECT observation_id,scope_key,source,kind,observed_at,recorded_at,content_type,payload,payload_sha256,metadata_json,decision_eligible
FROM observations WHERE `+where+` ORDER BY observed_at_ms DESC, observation_id DESC LIMIT 1`, args...).Scan(
		&out.ID, &out.ScopeKey, &out.Source, &out.Kind, &observed, &recorded, &out.ContentType, &out.Payload, &digest, &out.MetadataJSON, &eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, fmt.Errorf("latest observation: %w", s.latchCorruptRead(err))
	}
	if err := decodeObservation(&out, observed, recorded, digest); err != nil {
		return Observation{}, false, err
	}
	out.DecisionEligible = eligible
	return out, true, nil
}

// ExactDecisionEligibleObservation returns one immutable observation only
// when its receipt ID, authority coordinates, exact observation time, and
// decision-eligibility all match. It is the narrow live-decision reader for a
// state document that already names its evidence receipt; it never searches
// for a newest or nearby substitute.
func (s *Store) ExactDecisionEligibleObservation(ctx context.Context, receiptID int64, scopeKey, source, kind string, observedAt time.Time) (Observation, bool, error) {
	if receiptID <= 0 {
		return Observation{}, false, errorsf("observation receipt id is invalid")
	}
	for _, item := range []struct {
		label string
		value string
		limit int
	}{{"scope key", scopeKey, 512}, {"observation source", source, 128}, {"observation kind", kind, 128}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return Observation{}, false, err
		}
	}
	if observedAt.IsZero() {
		return Observation{}, false, errorsf("observation time is required")
	}
	var out Observation
	var observed, recorded string
	var digest []byte
	var eligible bool
	err := s.db.QueryRowContext(ctx, `SELECT observation_id,scope_key,source,kind,observed_at,recorded_at,content_type,payload,payload_sha256,metadata_json,decision_eligible
FROM observations WHERE observation_id=? AND scope_key=? AND source=? AND kind=? AND observed_at=? AND observed_at_ms=? AND decision_eligible=1`,
		receiptID, scopeKey, source, kind, formatTime(observedAt), observedAt.UnixMilli()).Scan(
		&out.ID, &out.ScopeKey, &out.Source, &out.Kind, &observed, &recorded, &out.ContentType, &out.Payload, &digest, &out.MetadataJSON, &eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, fmt.Errorf("exact decision-eligible observation: %w", s.latchCorruptRead(err))
	}
	if err := decodeObservation(&out, observed, recorded, digest); err != nil {
		return Observation{}, false, err
	}
	out.DecisionEligible = eligible
	return out, true, nil
}

// ListObservations returns matching observations in ascending observed-time and
// observation-ID order. A zero limit defaults to 1,000 rows.
func (s *Store) ListObservations(ctx context.Context, query ObservationQuery) ([]Observation, error) {
	if err := validateObservationQuery(query); err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit == 0 {
		limit = 1000
	}
	clauses := []string{"scope_key=?"}
	args := []any{query.ScopeKey}
	if query.Source != "" {
		clauses, args = append(clauses, "source=?"), append(args, query.Source)
	}
	if query.Kind != "" {
		clauses, args = append(clauses, "kind=?"), append(args, query.Kind)
	}
	if query.FromObservedAtMS != 0 {
		clauses, args = append(clauses, "observed_at_ms>=?"), append(args, query.FromObservedAtMS)
	}
	if query.ToObservedAtMS != 0 {
		clauses, args = append(clauses, "observed_at_ms<=?"), append(args, query.ToObservedAtMS)
	}
	if query.AfterObservationID != 0 {
		clauses, args = append(clauses, "observation_id>?"), append(args, query.AfterObservationID)
	}
	if query.DecisionEligible != nil {
		clauses, args = append(clauses, "decision_eligible=?"), append(args, *query.DecisionEligible)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT observation_id,scope_key,source,kind,observed_at,recorded_at,content_type,payload,payload_sha256,metadata_json,decision_eligible
FROM observations WHERE `+strings.Join(clauses, " AND ")+` ORDER BY observed_at_ms,observation_id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", s.latchCorruptRead(err))
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var item Observation
		var observed, recorded string
		var digest []byte
		var decisionEligible bool
		if err := rows.Scan(&item.ID, &item.ScopeKey, &item.Source, &item.Kind, &observed, &recorded, &item.ContentType, &item.Payload, &digest, &item.MetadataJSON, &decisionEligible); err != nil {
			return nil, err
		}
		if err := decodeObservation(&item, observed, recorded, digest); err != nil {
			return nil, err
		}
		item.DecisionEligible = decisionEligible
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, s.latchCorruptRead(err)
	}
	return out, nil
}

func decodeObservation(out *Observation, observed, recorded string, digest []byte) error {
	var err error
	out.ObservedAt, err = parseTime(observed)
	if err != nil {
		return fmt.Errorf("decode observation time: %w", err)
	}
	out.RecordedAt, err = parseTime(recorded)
	if err != nil {
		return fmt.Errorf("decode recorded time: %w", err)
	}
	if len(digest) != sha256.Size {
		return errorsf("stored observation digest is invalid")
	}
	copy(out.PayloadSHA256[:], digest)
	return nil
}

func validateObservationQuery(query ObservationQuery) error {
	if err := validateKey("scope key", query.ScopeKey, 512); err != nil {
		return err
	}
	if query.Source != "" {
		if err := validateKey("observation source", query.Source, 128); err != nil {
			return err
		}
	}
	if query.Kind != "" {
		if err := validateKey("observation kind", query.Kind, 128); err != nil {
			return err
		}
	}
	if query.Limit < 0 || query.Limit > 10000 {
		return errorsf("observation query limit is invalid")
	}
	return nil
}

func validateObservation(input ObservationInput) error {
	for _, item := range []struct {
		label string
		value string
		limit int
	}{{"scope key", input.ScopeKey, 512}, {"observation source", input.Source, 128}, {"observation kind", input.Kind, 128}, {"content type", input.ContentType, 128}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if input.ObservedAt.IsZero() {
		return errorsf("observation time is required")
	}
	if len(input.MetadataJSON) > 0 && !json.Valid(input.MetadataJSON) {
		return errorsf("observation metadata must be valid JSON")
	}
	return nil
}
