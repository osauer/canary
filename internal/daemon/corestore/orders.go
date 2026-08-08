package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const freshOrderAuthorityFingerprint = "fresh-empty-authority-v1"

// InitializeFreshOrderAuthority atomically establishes an empty order-safety
// epoch. It is only for a newly created authority: any existing order event,
// token tombstone, broker scope, order-ID floor, or import marker is a conflict.
// Unrelated daemon state may already exist in the same database.
func (s *Store) InitializeFreshOrderAuthority(ctx context.Context) error {
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		checks := []struct {
			label string
			query string
			args  []any
		}{
			{label: "order events", query: `SELECT EXISTS(SELECT 1 FROM order_events)`},
			{label: "preview-token tombstones", query: `SELECT EXISTS(SELECT 1 FROM consumed_preview_tokens)`},
			{label: "order-ID floors", query: `SELECT EXISTS(SELECT 1 FROM order_id_floors)`},
			{label: "broker scopes", query: `SELECT EXISTS(SELECT 1 FROM broker_scopes)`},
			{label: "order import marker", query: `SELECT EXISTS(SELECT 1 FROM legacy_imports WHERE scope_key='authority' AND source_kind='orders')`},
		}
		for _, check := range checks {
			var exists bool
			if err := tx.QueryRowContext(ctx, check.query, check.args...).Scan(&exists); err != nil {
				return fmt.Errorf("inspect fresh %s: %w", check.label, err)
			}
			if exists {
				return fmt.Errorf("%w: found %s", ErrFreshAuthorityConflict, check.label)
			}
		}

		now := time.Now().UTC()
		stamp := formatTime(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_id_floors(floor_scope,scope_key,floor,updated_at) VALUES('global','',0,?)`, stamp); err != nil {
			return fmt.Errorf("initialize global order-ID floor: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy_imports
(scope_key,source_kind,source_fingerprint,status,imported_through,details_json,created_at,updated_at)
VALUES('authority','orders',?,'complete','0',?,?,?)`, freshOrderAuthorityFingerprint, []byte(`{"mode":"fresh","version":1}`), stamp, stamp); err != nil {
			return fmt.Errorf("initialize fresh order marker: %w", err)
		}
		if _, err := advanceHeadTx(ctx, tx, 0, now); err != nil {
			return fmt.Errorf("advance fresh order authority head: %w", err)
		}
		return nil
	})
	return err
}

// StagePreTransmit atomically binds the broker scope, validates and consumes an
// optional preview-token digest, advances conservative order-ID floors, appends
// pre-transmit evidence, and advances the authority head. Success is durable
// staging evidence; the caller still owns the guarded broker transmission.
func (s *Store) StagePreTransmit(ctx context.Context, request PreTransmitRequest) (PreTransmitResult, error) {
	if err := validateBrokerScope(request.Scope); err != nil {
		return PreTransmitResult{}, err
	}
	if !validAction(request.Action) || !validOrigin(request.Origin) {
		return PreTransmitResult{}, errorsf("invalid pre-transmit provenance")
	}
	if request.RequestedOrderIDFloor < 0 || request.ReservedOrderID < 0 {
		return PreTransmitResult{}, errorsf("order id floor must not be negative")
	}
	if len(request.Events) == 0 {
		return PreTransmitResult{}, errorsf("at least one pre-transmit event is required")
	}
	hasToken := !zeroDigest(request.TokenDigest)
	if hasToken && (request.AuthorityEpoch == "" || request.SignerGeneration < 1) {
		return PreTransmitResult{}, errorsf("token authority identity is required")
	}
	events := make([]OrderEventRecord, len(request.Events))
	copy(events, request.Events)
	matchingTokenID := !hasToken
	for i := range events {
		if scopeZero(events[i].Scope) {
			events[i].Scope = request.Scope
		} else if !sameBrokerScope(events[i].Scope, request.Scope) {
			return PreTransmitResult{}, ErrBrokerScopeCollision
		}
		if events[i].Action == "" {
			events[i].Action = request.Action
		}
		if events[i].Origin == "" {
			events[i].Origin = request.Origin
		}
		if events[i].Action != request.Action || events[i].Origin != request.Origin {
			return PreTransmitResult{}, errorsf("event provenance does not match staged action")
		}
		if hasToken && events[i].PreviewTokenID != "" {
			if HashPreviewTokenID(events[i].PreviewTokenID) != request.TokenDigest {
				return PreTransmitResult{}, errorsf("preview token identifier does not match digest")
			}
			matchingTokenID = true
		}
		if err := validateOrderEvent(events[i]); err != nil {
			return PreTransmitResult{}, err
		}
	}
	if !matchingTokenID {
		return PreTransmitResult{}, errorsf("canonical preview token identifier is required")
	}

	var result PreTransmitResult
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if err := bindBrokerScopeTx(ctx, tx, request.Scope, now); err != nil {
			return err
		}
		before, err := readAuthorityHead(ctx, tx)
		if err != nil {
			return err
		}
		if hasToken {
			if before.AuthorityEpoch != request.AuthorityEpoch || before.SignerGeneration != request.SignerGeneration {
				return ErrAuthorityMismatch
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO consumed_preview_tokens
(token_digest, scope_key, authority_epoch, signer_generation, head_generation, consumed_at)
VALUES (?, ?, ?, ?, ?, ?)`, request.TokenDigest[:], request.Scope.ScopeKey, before.AuthorityEpoch,
				before.SignerGeneration, before.HeadGeneration+1, formatTime(now))
			if err != nil {
				if isConstraint(err) {
					return ErrPreviewTokenConsumed
				}
				return fmt.Errorf("consume preview token: %w", err)
			}
		}
		if request.ExpectedOrderEventSeq != nil {
			var (
				actual       int64
				latestType   string
				latestStatus sql.NullString
			)
			err := tx.QueryRowContext(ctx, `SELECT event_seq,type,status FROM order_events WHERE scope_key=? AND reserved_order_id=? ORDER BY event_seq DESC LIMIT 1`, request.Scope.ScopeKey, request.ReservedOrderID).Scan(&actual, &latestType, &latestStatus)
			exists := true
			if errors.Is(err, sql.ErrNoRows) {
				actual = 0
				exists = false
			} else if err != nil {
				return fmt.Errorf("read per-order event frontier: %w", err)
			}
			if actual != *request.ExpectedOrderEventSeq {
				return &RevisionConflictError{Expected: *request.ExpectedOrderEventSeq, Actual: actual, Exists: exists}
			}
			if request.Action == ActionModify {
				if !exists || latestType == "cancel-requested" || terminalOrderStatus(latestStatus.String) {
					return ErrOrderNotModifiable
				}
				var lastTerminal sql.NullInt64
				if err := tx.QueryRowContext(ctx, `SELECT
MAX(CASE WHEN lower(COALESCE(status,'')) IN ('filled','cancelled','canceled','apicancelled','inactive','rejected') THEN event_seq END)
FROM order_events WHERE scope_key=? AND reserved_order_id=?`, request.Scope.ScopeKey, request.ReservedOrderID).Scan(&lastTerminal); err != nil {
					return fmt.Errorf("read unresolved cancel frontier: %w", err)
				}
				unresolvedCancel, err := hasUnresolvedCancelAttemptTx(ctx, tx, request.Scope.ScopeKey, request.ReservedOrderID, lastTerminal.Int64)
				if err != nil {
					return err
				}
				if unresolvedCancel {
					return ErrOrderNotModifiable
				}
			}
		}
		if request.Action == ActionPlace {
			global, err := readFloor(ctx, tx, "global", "")
			if err != nil {
				return err
			}
			if request.ReservedOrderID <= global {
				return ErrOrderIDFloor
			}
		}
		requestedFloor := max(request.ReservedOrderID, request.RequestedOrderIDFloor)
		effective, err := advanceOrderIDFloorsTx(ctx, tx, request.Scope.ScopeKey, requestedFloor, now)
		if err != nil {
			return err
		}
		seqs, err := appendOrderEventsTx(ctx, tx, events, digestBytes(request.TokenDigest, hasToken), now)
		if err != nil {
			return err
		}
		head, err := advanceHeadTx(ctx, tx, seqs[len(seqs)-1], now)
		if err != nil {
			return err
		}
		result = PreTransmitResult{EffectiveOrderIDFloor: effective, EventSeqs: seqs, Head: head}
		return nil
	})
	return result, err
}

func terminalOrderStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "filled", "cancelled", "canceled", "apicancelled", "inactive", "rejected":
		return true
	default:
		return false
	}
}

type cancelAttemptAdmissionPayload struct {
	Type            string     `json:"type"`
	AttemptID       string     `json:"attempt_id"`
	ActionKind      ActionKind `json:"action_kind"`
	SendDisposition string     `json:"send_disposition"`
}

type cancelAttemptAdmissionState struct {
	action           ActionKind
	outcomeCount     int
	definitelyUnsent bool
}

func hasUnresolvedCancelAttemptTx(ctx context.Context, tx *sql.Tx, scopeKey string, reservedOrderID, afterEventSeq int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT oe.type,el.action_kind,el.payload_json
FROM order_events oe JOIN event_log el ON el.event_seq=oe.event_seq
WHERE oe.scope_key=? AND oe.reserved_order_id=? AND oe.event_seq>?
AND oe.type IN ('cancel-requested','send-completed','send-error')
ORDER BY oe.event_seq`, scopeKey, reservedOrderID, afterEventSeq)
	if err != nil {
		return false, fmt.Errorf("read cancel attempt evidence: %w", err)
	}
	defer rows.Close()

	attempts := make(map[string]*cancelAttemptAdmissionState)
	for rows.Next() {
		var (
			eventType string
			action    string
			rawJSON   []byte
		)
		if err := rows.Scan(&eventType, &action, &rawJSON); err != nil {
			return false, fmt.Errorf("scan cancel attempt evidence: %w", err)
		}
		var payload cancelAttemptAdmissionPayload
		payloadOK := json.Unmarshal(rawJSON, &payload) == nil &&
			payload.Type == eventType && payload.AttemptID != "" &&
			strings.TrimSpace(payload.AttemptID) == payload.AttemptID &&
			string(payload.ActionKind) == action

		switch eventType {
		case "cancel-requested":
			cancelAction := payload.ActionKind == ActionCancel || payload.ActionKind == ActionSmokeCleanup
			if !payloadOK || !cancelAction {
				return true, nil
			}
			if _, duplicate := attempts[payload.AttemptID]; duplicate {
				return true, nil
			}
			attempts[payload.AttemptID] = &cancelAttemptAdmissionState{action: payload.ActionKind}
		case "send-completed", "send-error":
			attempt := attempts[payload.AttemptID]
			if attempt == nil {
				continue
			}
			attempt.outcomeCount++
			attempt.definitelyUnsent = attempt.outcomeCount == 1 && payloadOK &&
				eventType == "send-error" && payload.ActionKind == attempt.action &&
				payload.SendDisposition == "definitely_unsent"
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read cancel attempt evidence: %w", err)
	}
	for _, attempt := range attempts {
		if !attempt.definitelyUnsent {
			return true, nil
		}
	}
	return false, nil
}

// LatestOrderEventSeq returns the exact durable frontier for one broker order.
// Zero means no event exists for the scope/order pair.
func (s *Store) LatestOrderEventSeq(ctx context.Context, scope BrokerScope, reservedOrderID int64) (int64, error) {
	if err := validateBrokerScope(scope); err != nil {
		return 0, err
	}
	if reservedOrderID <= 0 {
		return 0, errorsf("reserved order id must be positive")
	}
	var latest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(event_seq) FROM order_events WHERE scope_key=? AND reserved_order_id=?`, scope.ScopeKey, reservedOrderID).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read per-order event frontier: %w", err)
	}
	if !latest.Valid {
		return 0, nil
	}
	return latest.Int64, nil
}

// AppendOrderEvents appends normal lifecycle events atomically and returns
// their stable event_seq values in input order.
func (s *Store) AppendOrderEvents(ctx context.Context, events []OrderEventRecord) ([]int64, error) {
	return s.appendOrderEventsAtHead(ctx, -1, events)
}

// AppendOrderEventsAtHead appends events only when the authoritative order
// event frontier still equals expectedLastEventSeq. It is the reconciliation
// CAS boundary: a journal write after the caller's reload makes absence
// evidence stale instead of allowing it to close a newer row.
func (s *Store) AppendOrderEventsAtHead(ctx context.Context, expectedLastEventSeq int64, events []OrderEventRecord) ([]int64, error) {
	if expectedLastEventSeq < 0 {
		return nil, errorsf("expected order event head must not be negative")
	}
	return s.appendOrderEventsAtHead(ctx, expectedLastEventSeq, events)
}

func (s *Store) appendOrderEventsAtHead(ctx context.Context, expectedLastEventSeq int64, events []OrderEventRecord) ([]int64, error) {
	if len(events) == 0 {
		return nil, errorsf("at least one order event is required")
	}
	for _, event := range events {
		if err := validateBrokerScope(event.Scope); err != nil {
			return nil, err
		}
		if err := validateOrderEvent(event); err != nil {
			return nil, err
		}
	}
	var seqs []int64
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		if expectedLastEventSeq >= 0 {
			head, err := readAuthorityHead(ctx, tx)
			if err != nil {
				return err
			}
			if head.LastEventSeq != expectedLastEventSeq {
				return &RevisionConflictError{Expected: expectedLastEventSeq, Actual: head.LastEventSeq, Exists: true}
			}
		}
		now := time.Now().UTC()
		seen := map[string]BrokerScope{}
		for _, event := range events {
			if prior, ok := seen[event.Scope.ScopeKey]; ok && !sameBrokerScope(prior, event.Scope) {
				return ErrBrokerScopeCollision
			}
			seen[event.Scope.ScopeKey] = event.Scope
		}
		for _, scope := range seen {
			if err := bindBrokerScopeTx(ctx, tx, scope, now); err != nil {
				return err
			}
		}
		var err error
		seqs, err = appendOrderEventsTx(ctx, tx, events, nil, now)
		if err != nil {
			return err
		}
		_, err = advanceHeadTx(ctx, tx, seqs[len(seqs)-1], now)
		return err
	})
	return seqs, err
}

// ImportLegacyOrderAuthority performs the one-time cutover transaction. It
// imports token tombstones and conservative floors independently of the
// selected full-chain events, so omitted terminal chains never require
// fabricated user-visible events. SourceFingerprint makes retries idempotent.
func (s *Store) ImportLegacyOrderAuthority(ctx context.Context, input LegacyOrderImport) (LegacyOrderImportResult, error) {
	if err := validateKey("legacy source fingerprint", input.SourceFingerprint, 256); err != nil {
		return LegacyOrderImportResult{}, err
	}
	if input.GlobalFloor < 0 {
		return LegacyOrderImportResult{}, errorsf("legacy global floor must not be negative")
	}
	for _, item := range input.ScopedFloors {
		if err := validateBrokerScope(item.Scope); err != nil {
			return LegacyOrderImportResult{}, err
		}
		if item.Floor < 0 {
			return LegacyOrderImportResult{}, errorsf("legacy scoped floor must not be negative")
		}
	}
	for _, item := range input.ConsumedTokens {
		if err := validateBrokerScope(item.Scope); err != nil {
			return LegacyOrderImportResult{}, err
		}
		if err := validateKey("preview token identifier", item.PreviewTokenID, 512); err != nil {
			return LegacyOrderImportResult{}, err
		}
		if item.ConsumedAt.IsZero() {
			return LegacyOrderImportResult{}, errorsf("legacy token consumption time is required")
		}
	}
	for _, event := range input.Events {
		if err := validateBrokerScope(event.Scope); err != nil {
			return LegacyOrderImportResult{}, err
		}
		if err := validateOrderEvent(event); err != nil {
			return LegacyOrderImportResult{}, err
		}
	}
	var out LegacyOrderImportResult
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var importedFingerprint string
		err := tx.QueryRowContext(ctx, `SELECT source_fingerprint FROM legacy_imports WHERE scope_key='authority' AND source_kind='orders' AND status='complete'`).Scan(&importedFingerprint)
		if err == nil {
			if importedFingerprint != input.SourceFingerprint {
				return ErrLegacyImportConflict
			}
			out.Head, err = readAuthorityHead(ctx, tx)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		scopes := map[string]BrokerScope{}
		for _, item := range input.ScopedFloors {
			scopes[item.Scope.ScopeKey] = item.Scope
		}
		for _, item := range input.ConsumedTokens {
			if prior, ok := scopes[item.Scope.ScopeKey]; ok && !sameBrokerScope(prior, item.Scope) {
				return ErrBrokerScopeCollision
			}
			scopes[item.Scope.ScopeKey] = item.Scope
		}
		for _, event := range input.Events {
			if prior, ok := scopes[event.Scope.ScopeKey]; ok && !sameBrokerScope(prior, event.Scope) {
				return ErrBrokerScopeCollision
			}
			scopes[event.Scope.ScopeKey] = event.Scope
		}
		for _, scope := range scopes {
			if err := bindBrokerScopeTx(ctx, tx, scope, now); err != nil {
				return err
			}
		}
		head, err := readAuthorityHead(ctx, tx)
		if err != nil {
			return err
		}
		for _, item := range input.ConsumedTokens {
			digest := HashPreviewTokenID(item.PreviewTokenID)
			result, err := tx.ExecContext(ctx, `INSERT INTO consumed_preview_tokens(token_digest,scope_key,authority_epoch,signer_generation,head_generation,consumed_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(token_digest) DO NOTHING`, digest[:], item.Scope.ScopeKey, head.AuthorityEpoch, head.SignerGeneration, head.HeadGeneration+1, formatTime(item.ConsumedAt))
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				var scopeKey string
				if err := tx.QueryRowContext(ctx, `SELECT scope_key FROM consumed_preview_tokens WHERE token_digest=?`, digest[:]).Scan(&scopeKey); err != nil {
					return err
				}
				if scopeKey != item.Scope.ScopeKey {
					return ErrBrokerScopeCollision
				}
			}
		}
		stamp := formatTime(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_id_floors(floor_scope,scope_key,floor,updated_at) VALUES('global','',?,?)
ON CONFLICT(floor_scope,scope_key) DO UPDATE SET floor=MAX(order_id_floors.floor,excluded.floor),updated_at=excluded.updated_at`, input.GlobalFloor, stamp); err != nil {
			return err
		}
		for _, item := range input.ScopedFloors {
			if _, err := advanceOrderIDFloorsTx(ctx, tx, item.Scope.ScopeKey, item.Floor, now); err != nil {
				return err
			}
		}
		if len(input.Events) > 0 {
			out.EventSeqs, err = appendOrderEventsTx(ctx, tx, input.Events, nil, now)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy_imports(scope_key,source_kind,source_fingerprint,status,imported_through,details_json,created_at,updated_at)
VALUES('authority','orders',?,'complete',?,NULL,?,?)`, input.SourceFingerprint, fmt.Sprint(len(input.Events)), stamp, stamp); err != nil {
			return err
		}
		last := int64(0)
		if len(out.EventSeqs) > 0 {
			last = out.EventSeqs[len(out.EventSeqs)-1]
		}
		out.Head, err = advanceHeadTx(ctx, tx, last, now)
		if err != nil {
			return err
		}
		out.Imported = true
		return nil
	})
	return out, err
}

// CommitLifecycle appends exact-route lifecycle events and optionally performs
// a versioned state mutation in the same transaction. The state scope may be
// global and need not equal the broker scope.
func (s *Store) CommitLifecycle(ctx context.Context, commit LifecycleCommit) (LifecycleResult, error) {
	if err := validateBrokerScope(commit.Scope); err != nil {
		return LifecycleResult{}, err
	}
	if len(commit.Events) == 0 {
		return LifecycleResult{}, errorsf("at least one lifecycle event is required")
	}
	events := make([]OrderEventRecord, len(commit.Events))
	copy(events, commit.Events)
	for i := range events {
		if scopeZero(events[i].Scope) {
			events[i].Scope = commit.Scope
		}
		if !sameBrokerScope(events[i].Scope, commit.Scope) {
			return LifecycleResult{}, ErrBrokerScopeCollision
		}
		if err := validateOrderEvent(events[i]); err != nil {
			return LifecycleResult{}, err
		}
	}
	if commit.State != nil {
		if err := validateStateCAS(*commit.State); err != nil {
			return LifecycleResult{}, err
		}
	}
	var out LifecycleResult
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if err := bindBrokerScopeTx(ctx, tx, commit.Scope, now); err != nil {
			return err
		}
		seqs, err := appendOrderEventsTx(ctx, tx, events, nil, now)
		if err != nil {
			return err
		}
		if commit.State != nil {
			saved, err := compareAndSwapStateTx(ctx, tx, *commit.State, now)
			if err != nil {
				return err
			}
			out.State = &saved
		}
		head, err := advanceHeadTx(ctx, tx, seqs[len(seqs)-1], now)
		if err != nil {
			return err
		}
		out.EventSeqs, out.Head = seqs, head
		return nil
	})
	return out, err
}

// LoadOrderEvents returns matching order events in ascending event-sequence
// order. A zero limit defaults to 1,000 rows.
func (s *Store) LoadOrderEvents(ctx context.Context, query OrderQuery) ([]OrderEventRecord, error) {
	if query.ScopeKey != "" {
		if err := validateKey("scope key", query.ScopeKey, 512); err != nil {
			return nil, err
		}
	}
	if query.Limit < 0 || query.Limit > 10_000 {
		return nil, errorsf("order query limit is invalid")
	}
	limit := query.Limit
	if limit == 0 {
		limit = 1000
	}
	clauses := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) { clauses, args = append(clauses, clause), append(args, value) }
	if query.ScopeKey != "" {
		add("oe.scope_key=?", query.ScopeKey)
	}
	if query.FromAtMS != 0 {
		add("el.occurred_at_ms>=?", query.FromAtMS)
	}
	if query.ToAtMS != 0 {
		add("el.occurred_at_ms<=?", query.ToAtMS)
	}
	if query.AfterEventSeq != 0 {
		add("oe.event_seq>?", query.AfterEventSeq)
	}
	if query.OrderRef != "" {
		add("oe.order_ref=?", query.OrderRef)
	}
	if query.ReservedOrderID != nil {
		add("oe.reserved_order_id=?", *query.ReservedOrderID)
	}
	if query.PermID != nil {
		add("oe.perm_id=?", *query.PermID)
	}
	if query.PreviewTokenID != "" {
		add("oe.preview_token_id=?", query.PreviewTokenID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT oe.event_seq, oe.scope_key, bs.endpoint, bs.client_id, bs.account, bs.mode,
el.event_key, el.occurred_at_ms, oe.type, el.action_kind, el.origin, oe.order_ref, oe.preview_token_id,
oe.reserved_order_id, oe.perm_id, oe.status, el.payload_json
FROM order_events oe JOIN event_log el ON el.event_seq=oe.event_seq JOIN broker_scopes bs ON bs.scope_key=oe.scope_key
WHERE `+strings.Join(clauses, " AND ")+` ORDER BY oe.event_seq LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("load order events: %w", err)
	}
	defer rows.Close()
	var out []OrderEventRecord
	for rows.Next() {
		var event OrderEventRecord
		var orderRef, tokenID, status sql.NullString
		var reserved, perm sql.NullInt64
		if err := rows.Scan(&event.EventSeq, &event.Scope.ScopeKey, &event.Scope.Endpoint, &event.Scope.ClientID, &event.Scope.Account, &event.Scope.Mode,
			&event.EventKey, &event.AtMS, &event.Type, &event.Action, &event.Origin, &orderRef, &tokenID, &reserved, &perm, &status, &event.RawJSON); err != nil {
			return nil, fmt.Errorf("scan order event: %w", err)
		}
		event.OrderRef, event.PreviewTokenID, event.Status = orderRef.String, tokenID.String, status.String
		event.ReservedOrderID, event.PermID = reserved.Int64, perm.Int64
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load order events: %w", err)
	}
	return out, nil
}

// GlobalOrderIDFloor returns the greatest order ID reserved across all broker
// scopes, or zero when no floor exists.
func (s *Store) GlobalOrderIDFloor(ctx context.Context) (int64, error) {
	return readFloor(ctx, s.db, "global", "")
}

// ScopedOrderIDFloor returns the greater of the global and named broker-scope
// floors, or zero when neither exists.
func (s *Store) ScopedOrderIDFloor(ctx context.Context, scopeKey string) (int64, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return 0, err
	}
	global, err := readFloor(ctx, s.db, "global", "")
	if err != nil {
		return 0, err
	}
	scoped, err := readFloor(ctx, s.db, "broker", scopeKey)
	if err != nil {
		return 0, err
	}
	if global > scoped {
		return global, nil
	}
	return scoped, nil
}

func readFloor(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, kind, key string) (int64, error) {
	var floor int64
	err := q.QueryRowContext(ctx, `SELECT floor FROM order_id_floors WHERE floor_scope=? AND scope_key=?`, kind, key).Scan(&floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return floor, err
}

func advanceOrderIDFloorsTx(ctx context.Context, tx *sql.Tx, scopeKey string, requested int64, now time.Time) (int64, error) {
	stamp := formatTime(now)
	for _, item := range []struct{ kind, key string }{{"global", ""}, {"broker", scopeKey}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_id_floors(floor_scope, scope_key, floor, updated_at)
VALUES (?, ?, ?, ?) ON CONFLICT(floor_scope, scope_key) DO UPDATE SET
floor=MAX(order_id_floors.floor, excluded.floor), updated_at=excluded.updated_at`, item.kind, item.key, requested, stamp); err != nil {
			return 0, fmt.Errorf("advance order id floor: %w", err)
		}
	}
	global, err := readFloor(ctx, tx, "global", "")
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE order_id_floors SET floor=MAX(floor, ?), updated_at=? WHERE floor_scope='broker' AND scope_key=?`, global, stamp, scopeKey); err != nil {
		return 0, err
	}
	return global, nil
}

func bindBrokerScopeTx(ctx context.Context, tx *sql.Tx, scope BrokerScope, now time.Time) error {
	digest := brokerScopeDigest(scope)
	result, err := tx.ExecContext(ctx, `INSERT INTO broker_scopes(scope_key, endpoint, client_id, account, mode, binding_sha256, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, scope.ScopeKey, scope.Endpoint, scope.ClientID, scope.Account, scope.Mode, digest[:], formatTime(now))
	if err != nil {
		return fmt.Errorf("bind broker scope: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var endpoint, account, mode string
	var clientID int
	err = tx.QueryRowContext(ctx, `SELECT endpoint, client_id, account, mode FROM broker_scopes WHERE scope_key=?`, scope.ScopeKey).Scan(&endpoint, &clientID, &account, &mode)
	if err == nil && endpoint == scope.Endpoint && clientID == scope.ClientID && account == scope.Account && mode == scope.Mode {
		return nil
	}
	return ErrBrokerScopeCollision
}

func brokerScopeDigest(scope BrokerScope) [sha256.Size]byte {
	h := sha256.New()
	for _, value := range []string{scope.Endpoint, fmt.Sprint(scope.ClientID), scope.Account, scope.Mode} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(value)))
		h.Write([]byte(value))
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func appendOrderEventsTx(ctx context.Context, tx *sql.Tx, events []OrderEventRecord, tokenDigest []byte, now time.Time) ([]int64, error) {
	seqs := make([]int64, 0, len(events))
	for ordinal, event := range events {
		digest := sha256.Sum256(event.RawJSON)
		result, err := tx.ExecContext(ctx, `INSERT INTO event_log
(scope_key,event_key,event_type,action_kind,origin,occurred_at,occurred_at_ms,recorded_at,payload_json,payload_sha256)
VALUES (?,?,?,?,?,?,?,?,?,?)`, event.Scope.ScopeKey, event.EventKey, event.Type, event.Action, event.Origin,
			formatTime(time.UnixMilli(event.AtMS)), event.AtMS, formatTime(now), event.RawJSON, digest[:])
		if err != nil {
			return nil, fmt.Errorf("append event log: %w", err)
		}
		seq, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO order_events
(event_seq,scope_key,batch_ordinal,type,order_ref,preview_token_id,reserved_order_id,perm_id,status,token_digest)
VALUES (?,?,?,?,?,?,?,?,?,?)`, seq, event.Scope.ScopeKey, ordinal, event.Type, nullableString(event.OrderRef), nullableString(event.PreviewTokenID),
			nullablePositive(event.ReservedOrderID), nullablePositive(event.PermID), nullableString(event.Status), tokenDigest)
		if err != nil {
			return nil, fmt.Errorf("append order projection: %w", err)
		}
		seqs = append(seqs, seq)
	}
	return seqs, nil
}

func validateOrderEvent(event OrderEventRecord) error {
	if event.EventSeq != 0 {
		return errorsf("new order event must not set event sequence")
	}
	if err := validateKey("event key", event.EventKey, 512); err != nil {
		return err
	}
	if err := validateKey("event type", event.Type, 128); err != nil {
		return err
	}
	if event.AtMS <= 0 {
		return errorsf("event time is required")
	}
	if !validAction(event.Action) || !validOrigin(event.Origin) {
		return errorsf("invalid event provenance")
	}
	if !json.Valid(event.RawJSON) {
		return errorsf("order event payload must be valid JSON")
	}
	if event.ReservedOrderID < 0 || event.PermID < 0 {
		return errorsf("order identifiers must not be negative")
	}
	return nil
}

func validateBrokerScope(scope BrokerScope) error {
	for _, item := range []struct {
		label, value string
		limit        int
	}{{"scope key", scope.ScopeKey, 512}, {"broker endpoint", scope.Endpoint, 512}, {"broker account", scope.Account, 256}, {"broker mode", scope.Mode, 32}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if scope.ClientID < 0 {
		return errorsf("broker client id must not be negative")
	}
	if scope.Mode != "paper" && scope.Mode != "live" {
		return errorsf("broker mode is invalid")
	}
	return nil
}

func validAction(action ActionKind) bool {
	switch action {
	case ActionPlace, ActionModify, ActionCancel, ActionPurge, ActionRestore, ActionExercise, ActionSmokeCleanup:
		return true
	}
	return false
}

func validOrigin(origin TransmitOrigin) bool {
	switch origin {
	case OriginAgentCLI, OriginHumanCLI, OriginDaemon:
		return true
	}
	return false
}

func sameBrokerScope(a, b BrokerScope) bool { return a == b }
func scopeZero(s BrokerScope) bool          { return s == (BrokerScope{}) }
func zeroDigest(d PreviewTokenDigest) bool  { return d == (PreviewTokenDigest{}) }
func digestBytes(d PreviewTokenDigest, present bool) []byte {
	if !present {
		return nil
	}
	return d[:]
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func nullablePositive(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func isConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19
}
