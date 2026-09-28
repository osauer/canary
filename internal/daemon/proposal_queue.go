package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Queued authorisations (owner decisions #26–#31, 2026-09-28; see
// internal-docs/design/queued-authorisations.md).
//
// A queued authorisation carries one owner-signed reduction to the next
// regular session. Queue prepare fixes the terms from the row the owner
// reviewed: one exact contract and side, a maximum quantity, a bounded limit
// and a one-hour send window that opens 15 minutes after the options open
// (5 after a stock open). It returns them with their digest and a private
// reference, and sends nothing. Desk arms the record with that reference once
// the owner has signed the digest. Inside the window the daemon sends the
// order itself after every gate a manual submit passes, or it holds, cancels
// or expires the record (proposal_queue_executor.go). The clock only chooses
// when to send; the owner's signature is the authority, and an outage means
// no trade.

const (
	queuedStateKind       = "trade_proposals_queued"
	queuedCoreEventType   = "trade_proposal_queued_event"
	queuedDocumentVersion = 1
	queuedTermsVersion    = 1
	queuedReferencePrefix = "canaryqa1"

	// queuedArmWindow is how long a prepared record waits for its arm.
	queuedArmWindow = 10 * time.Minute
	// queuedSendWindow follows not_before (owner decision #28: one hour,
	// with a DAY order).
	queuedSendWindow = time.Hour
	// queuedConcession sets the limit halfway from the mid toward the bid
	// (owner decision #29).
	queuedConcession = 0.5
	// queuedWorstMove places the default worst price 25% against the order
	// from the reference mark.
	queuedWorstMove = 0.25
	// queuedLateAfter flags a send this long after not_before as late.
	queuedLateAfter = 2 * time.Minute
	// queuedRetention keeps final records this long for list and audit.
	queuedRetention = 7 * 24 * time.Hour
	// queuedSubmitTimeout bounds the quote and WhatIf wait of one send.
	queuedSubmitTimeout = 5 * time.Second
	// Bounds on the audit fields an arm carries.
	queuedEnvelopeMaxBytes = 4096
	queuedIdentityMaxBytes = 128
	queuedReasonMaxRunes   = 300
)

// Journaled transitions (design: Events). "signed" is Desk's event: Canary
// receives the owner's signature with the arm.
const (
	queuedEventPrepared        = "queued_auth.prepared"
	queuedEventArmed           = "queued_auth.armed"
	queuedEventHeld            = "queued_auth.held"
	queuedEventResumed         = "queued_auth.resumed"
	queuedEventSending         = "queued_auth.sending"
	queuedEventSent            = "queued_auth.sent"
	queuedEventFilled          = "queued_auth.filled"
	queuedEventPartiallyFilled = "queued_auth.partially_filled"
	queuedEventExpiredUnfilled = "queued_auth.expired_unfilled"
	queuedEventCancelled       = "queued_auth.cancelled"
	queuedEventExpired         = "queued_auth.expired"
	queuedEventFailed          = "queued_auth.failed"
	queuedEventRecovered       = "queued_auth.recovered"
	// queuedEventCancelRequested journals an owner cancel that arrived while
	// the order was being placed; the record ends cancelled if the attempt
	// proves unsent.
	queuedEventCancelRequested = "queued_auth.cancel_requested"
)

// Why a record was cancelled, expired or failed. A blocker the executor does
// not wait out cancels the record under the blocker's own code.
const (
	queuedReasonOwnerCancelled    = "owner_cancelled"
	queuedReasonSuperseded        = "superseded"
	queuedReasonNotArmed          = "not_armed"
	queuedReasonWindowEnded       = "window_ended"
	queuedReasonPositionChanged   = "position_changed"
	queuedReasonRowGone           = "row_gone"
	queuedReasonRowTermsChanged   = "row_terms_changed"
	queuedReasonPolicyChanged     = "policy_changed"
	queuedReasonAccountChanged    = "account_changed"
	queuedReasonWhatIfRefused     = "whatif_refused"
	queuedReasonSendRefused       = "send_refused"
	queuedReasonSendUnclear       = "send_outcome_unclear"
	queuedReasonRestartBeforeSend = "restart_before_send"
	// queuedReasonRecordUnreadable ends a record this build cannot fully
	// read: a field, version, style or state it does not know, or terms that
	// no longer hash to their digest.
	queuedReasonRecordUnreadable = "record_unreadable"
)

// What a held record waits for, beside the readiness codes it shares:
// market_closed, trading_frozen, broker_unavailable, halted, quote_unusable
// and spread_too_wide.
const (
	queuedHoldWorstPrice = "worst_price"
	queuedHoldHandOrder  = "hand_order_working"
)

// queuedAuthRecord is one durable queued authorisation, keyed by queue ID.
// Its terms are immutable once prepared. Timestamps are UTC.
type queuedAuthRecord struct {
	Version       int                 `json:"version"`
	ReferenceHash [sha256.Size]byte   `json:"reference_hash"`
	Terms         rpc.QueuedAuthTerms `json:"terms"`
	TermsDigest   string              `json:"terms_digest"`
	// ContractSide is sameContractSide of the queued row, the key of the
	// one-intent guard.
	ContractSide string    `json:"contract_side"`
	State        string    `json:"state"`
	Seq          int64     `json:"seq"`
	CreatedAt    time.Time `json:"created_at"`

	ArmedAt      time.Time `json:"armed_at,omitzero"`
	DeskActionID string    `json:"desk_action_id,omitempty"`
	Credential   string    `json:"credential,omitempty"`
	ArmOrigin    string    `json:"arm_origin,omitempty"`
	// Envelope is the owner's signature envelope, kept for audit only.
	Envelope string `json:"envelope,omitempty"`
	// SettlingBaseline marks the hand orders already working when the record
	// was armed (automaticOrderMark); a hand order placed or modified since
	// holds the send.
	SettlingBaseline      []string `json:"settling_baseline,omitempty"`
	SettlingBaselineKnown bool     `json:"settling_baseline_known,omitempty"`

	HeldAt     time.Time `json:"held_at,omitzero"`
	HoldCode   string    `json:"hold_code,omitempty"`
	HoldReason string    `json:"hold_reason,omitempty"`

	// PreviewTokenID is persisted with the sending state, before the broker
	// call, so a restart finds the journaled attempt for this record.
	SendingAt      time.Time            `json:"sending_at,omitzero"`
	PreviewTokenID string               `json:"preview_token_id,omitempty"`
	OrderRef       string               `json:"order_ref,omitempty"`
	QuantitySent   int                  `json:"quantity_sent,omitempty"`
	LimitPrice     float64              `json:"limit_price,omitempty"`
	SendQuote      *rpc.QueuedAuthQuote `json:"send_quote,omitempty"`
	Late           bool                 `json:"late,omitempty"`
	SentAt         time.Time            `json:"sent_at,omitzero"`
	PermID         int                  `json:"perm_id,omitempty"`
	FilledQuantity float64              `json:"filled_quantity,omitempty"`
	AvgFillPrice   float64              `json:"avg_fill_price,omitempty"`

	ResolvedAt   time.Time `json:"resolved_at,omitzero"`
	ReasonCode   string    `json:"reason_code,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	CancelOrigin string    `json:"cancel_origin,omitempty"`
	// CancelRequested is an owner cancel that arrived while the order was
	// being placed: an attempt that proves unsent ends cancelled.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// problem names why this build cannot execute the record as loaded; it
	// is never persisted, and the executor ends such a record first.
	problem string
}

// queuedKnownStates are the states this build reads.
var queuedKnownStates = []string{rpc.QueuedAuthPrepared, rpc.QueuedAuthArmed, rpc.QueuedAuthHeld, rpc.QueuedAuthSending,
	rpc.QueuedAuthSent, rpc.QueuedAuthFilled, rpc.QueuedAuthPartiallyFilled, rpc.QueuedAuthExpiredUnfilled,
	rpc.QueuedAuthCancelled, rpc.QueuedAuthExpired, rpc.QueuedAuthFailed}

// queuedRecordProblem names why this build cannot execute a record, or "":
// a version, state, style or TIF it does not know, or terms that no longer
// hash to their digest.
func queuedRecordProblem(rec queuedAuthRecord) string {
	switch {
	case rec.problem != "":
		return rec.problem
	case rec.Version != queuedDocumentVersion || rec.Terms.Version != queuedTermsVersion:
		return "the stored record has a version this build of Canary does not know"
	case !slices.Contains(queuedKnownStates, rec.State):
		return "the stored record is in a state this build of Canary does not know"
	case rec.Terms.Style != rpc.QueuedAuthStyleBoundedLimit || rec.Terms.TIF != rpc.OrderTIFDay:
		return "the stored record has an order style this build of Canary does not execute"
	}
	if digest, err := queuedTermsDigest(rec.Terms); err != nil || digest != rec.TermsDigest {
		return "the stored terms no longer match their digest"
	}
	return ""
}

// decodeQueuedRecord reads one stored record strictly. A record with a field
// this build does not know still loads, with the problem named, so the
// executor ends it instead of sending terms it cannot fully read.
func decodeQueuedRecord(raw json.RawMessage) (queuedAuthRecord, error) {
	var rec queuedAuthRecord
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err == nil {
		rec.problem = queuedRecordProblem(rec)
		return rec, nil
	}
	var loose queuedAuthRecord
	if err := json.Unmarshal(raw, &loose); err != nil {
		return loose, err
	}
	loose.problem = "the stored record carries fields this build of Canary does not know"
	return loose, nil
}

func (r queuedAuthRecord) scope() brokerStateScope {
	return brokerStateScope{Account: r.Terms.AccountID, Mode: r.Terms.AccountMode}
}

// waiting reports a record that may still send: armed, or held inside or
// before its window.
func (r queuedAuthRecord) waiting() bool {
	return r.State == rpc.QueuedAuthArmed || r.State == rpc.QueuedAuthHeld
}

// liveIntent reports an authorised intent that has not reached the broker.
func (r queuedAuthRecord) liveIntent() bool {
	return r.waiting() || r.State == rpc.QueuedAuthSending
}

// final reports a record with no transition left.
func (r queuedAuthRecord) final() bool {
	switch r.State {
	case rpc.QueuedAuthFilled, rpc.QueuedAuthPartiallyFilled, rpc.QueuedAuthExpiredUnfilled,
		rpc.QueuedAuthCancelled, rpc.QueuedAuthExpired, rpc.QueuedAuthFailed:
		return true
	}
	return false
}

func (r queuedAuthRecord) view() rpc.QueuedAuth {
	out := rpc.QueuedAuth{
		Terms: r.Terms, TermsDigest: r.TermsDigest, State: r.State, CreatedAt: r.CreatedAt, ArmedAt: r.ArmedAt,
		DeskActionID: r.DeskActionID, Credential: r.Credential, HeldAt: r.HeldAt, HoldCode: r.HoldCode, HoldReason: r.HoldReason,
		SendingAt: r.SendingAt, SentAt: r.SentAt, Late: r.Late, PreviewTokenID: r.PreviewTokenID, OrderRef: r.OrderRef,
		PermID: r.PermID, QuantitySent: r.QuantitySent, LimitPrice: r.LimitPrice, FilledQuantity: r.FilledQuantity,
		AvgFillPrice: r.AvgFillPrice, ResolvedAt: r.ResolvedAt, ReasonCode: r.ReasonCode, Reason: r.Reason, CancelOrigin: r.CancelOrigin,
		CancelRequested: r.CancelRequested,
	}
	if r.SendQuote != nil {
		q := *r.SendQuote
		out.SendQuote = &q
	}
	return out
}

// queuedAuthEvent is one journaled transition. It never carries the queued
// reference, its hash or the signature envelope.
type queuedAuthEvent struct {
	Version         int                  `json:"version"`
	Type            string               `json:"type"`
	At              time.Time            `json:"at"`
	CorrelationID   string               `json:"correlation_id"`
	QueueID         string               `json:"queue_id"`
	DeskActionID    string               `json:"desk_action_id,omitempty"`
	Seq             int64                `json:"seq"`
	StateFrom       string               `json:"state_from,omitempty"`
	StateTo         string               `json:"state_to"`
	ReasonCode      string               `json:"reason_code,omitempty"`
	Reason          string               `json:"reason,omitempty"`
	Origin          string               `json:"origin,omitempty"`
	Credential      string               `json:"credential,omitempty"`
	Key             string               `json:"key"`
	RevisionAtQueue string               `json:"revision_at_queue,omitempty"`
	RowTermsDigest  string               `json:"row_terms_digest,omitempty"`
	TermsDigest     string               `json:"terms_digest,omitempty"`
	Bucket          string               `json:"bucket,omitempty"`
	ConID           int                  `json:"con_id,omitempty"`
	Side            string               `json:"side,omitempty"`
	MaxQty          int                  `json:"max_qty,omitempty"`
	QtySent         int                  `json:"qty_sent,omitempty"`
	WorstPrice      float64              `json:"worst_price,omitempty"`
	Limit           float64              `json:"limit,omitempty"`
	NotBefore       time.Time            `json:"not_before,omitzero"`
	NotAfter        time.Time            `json:"not_after,omitzero"`
	Quote           *rpc.QueuedAuthQuote `json:"quote,omitempty"`
	PreviewTokenID  string               `json:"preview_token_id,omitempty"`
	OrderRef        string               `json:"order_ref,omitempty"`
	PermID          int                  `json:"perm_id,omitempty"`
	FillQty         float64              `json:"fill_qty,omitempty"`
	AvgPrice        float64              `json:"avg_price,omitempty"`
	Late            bool                 `json:"late,omitempty"`
	DaemonStartedAt time.Time            `json:"daemon_started_at,omitzero"`
	ExecutorTickAt  time.Time            `json:"executor_tick_at,omitzero"`
}

// transition moves r to state and returns the event that journals it.
func (r *queuedAuthRecord) transition(eventType, state string, at time.Time) queuedAuthEvent {
	from := r.State
	r.State = state
	return r.note(eventType, from, at)
}

// note journals an event on r without a state change of its own; from is
// the state the event leaves.
func (r *queuedAuthRecord) note(eventType, from string, at time.Time) queuedAuthEvent {
	r.Seq++
	t := r.Terms
	return queuedAuthEvent{
		Version: queuedDocumentVersion, Type: eventType, At: at.UTC(), CorrelationID: t.QueueID, QueueID: t.QueueID,
		DeskActionID: r.DeskActionID, Seq: r.Seq, StateFrom: from, StateTo: r.State, Key: t.Key,
		RevisionAtQueue: t.RevisionAtQueue, RowTermsDigest: t.RowTermsDigest, TermsDigest: r.TermsDigest, Bucket: t.Bucket,
		ConID: t.Contract.ConID, Side: t.Action, MaxQty: t.MaxQuantity, WorstPrice: t.WorstPrice, NotBefore: t.NotBefore, NotAfter: t.NotAfter,
		OrderRef: r.OrderRef, PreviewTokenID: r.PreviewTokenID,
	}
}

// resolve ends r in a final state with its reason.
func (r *queuedAuthRecord) resolve(eventType, state, code, reason string, at time.Time) queuedAuthEvent {
	r.ResolvedAt, r.ReasonCode, r.Reason = at.UTC(), code, boundedQueuedReason(reason)
	r.HeldAt, r.HoldCode, r.HoldReason = time.Time{}, "", ""
	ev := r.transition(eventType, state, at)
	ev.ReasonCode, ev.Reason = code, r.Reason
	return ev
}

func boundedQueuedReason(s string) string {
	s = decisionAccountPattern.ReplaceAllString(strings.Join(strings.Fields(s), " "), "[account]")
	if utf8.RuneCountInString(s) <= queuedReasonMaxRunes {
		return s
	}
	return string([]rune(s)[:queuedReasonMaxRunes-1]) + "…"
}

type queuedAuthDocument struct {
	Version int                `json:"version"`
	Records []queuedAuthRecord `json:"records"`
}

// queuedAuthStore owns the records in daemon.db. Every mutation is a
// read-modify-write under mu followed by a compare-and-swap of the whole
// document with its events in one transaction, so an arm, a cancel and the
// executor never overwrite each other and a crash leaves either the old or
// the new document.
type queuedAuthStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	records  map[string]*queuedAuthRecord
}

func (s *queuedAuthStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return errors.New("queued authorisation store is not attached")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, queuedStateKind)
	if err != nil {
		return err
	}
	records := map[string]*queuedAuthRecord{}
	var revision int64
	if ok {
		var parsed struct {
			Records []json.RawMessage `json:"records"`
		}
		if err := json.Unmarshal(doc.JSON, &parsed); err != nil {
			return fmt.Errorf("decode queued authorisation state: %w", err)
		}
		for _, raw := range parsed.Records {
			rec, err := decodeQueuedRecord(raw)
			if err != nil || strings.TrimSpace(rec.Terms.QueueID) == "" || strings.TrimSpace(rec.Terms.Key) == "" {
				return errors.New("queued authorisation state is malformed")
			}
			records[rec.Terms.QueueID] = &rec
		}
		revision = doc.Revision
	}
	s.mu.Lock()
	s.core, s.records, s.revision = core, records, revision
	s.mu.Unlock()
	return nil
}

func (s *queuedAuthStore) attached() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.core != nil
}

// list returns copies of every record, oldest first.
func (s *queuedAuthStore) list() []queuedAuthRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]queuedAuthRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, *rec)
	}
	slices.SortFunc(out, compareQueuedRecords)
	return out
}

func compareQueuedRecords(a, b queuedAuthRecord) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.Terms.QueueID, b.Terms.QueueID)
}

func (s *queuedAuthStore) get(id string) (queuedAuthRecord, bool) {
	if s == nil {
		return queuedAuthRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return queuedAuthRecord{}, false
	}
	return *rec, true
}

// update runs mutate against a copy of the records under the store lock and
// persists the result with its events atomically. mutate returns the events
// to append; none means nothing changed and nothing is written. Final records
// older than queuedRetention at now are dropped with the write.
func (s *queuedAuthStore) update(ctx context.Context, now time.Time, mutate func(records map[string]*queuedAuthRecord) []queuedAuthEvent) error {
	if s == nil {
		return errors.New("queued authorisation store is not attached")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("queued authorisation store is not attached")
	}
	working := make(map[string]*queuedAuthRecord, len(s.records))
	for k, rec := range s.records {
		copied := *rec
		copied.SettlingBaseline = slices.Clone(rec.SettlingBaseline)
		if rec.SendQuote != nil {
			q := *rec.SendQuote
			copied.SendQuote = &q
		}
		working[k] = &copied
	}
	events := mutate(working)
	if len(events) == 0 {
		return nil
	}
	doc := queuedAuthDocument{Version: queuedDocumentVersion}
	for id, rec := range working {
		if rec.final() && !rec.ResolvedAt.IsZero() && now.Sub(rec.ResolvedAt) > queuedRetention {
			delete(working, id)
			continue
		}
		doc.Records = append(doc.Records, *rec)
	}
	slices.SortFunc(doc.Records, compareQueuedRecords)
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	inputs := make([]corestore.EventInput, 0, len(events))
	for i, ev := range events {
		if ev.At.IsZero() {
			ev.At = now.UTC()
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		eventKey, err := coreStoreEventKey(ctx, s.core, "proposal-queued", ev.At, payload, i)
		if err != nil {
			return err
		}
		inputs = append(inputs, corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: queuedCoreEventType, Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: ev.At, PayloadJSON: payload})
	}
	saved, _, err := s.core.CompareAndSwapStateDocumentWithEvents(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: queuedStateKind, ExpectedRevision: s.revision, JSON: raw,
	}, inputs)
	if err != nil {
		return fmt.Errorf("persist queued authorisation state: %w", err)
	}
	s.revision = saved.Revision
	s.records = working
	return nil
}

// mintQueuedReference returns a new queue ID and the private reference that
// names it; only the reference's hash is stored.
func mintQueuedReference() (id, reference string, err error) {
	if id, err = randomTokenID(); err != nil {
		return "", "", err
	}
	secret, err := randomTokenID()
	if err != nil {
		return "", "", err
	}
	return id, queuedReferencePrefix + "." + id + "." + secret, nil
}

func parseQueuedReference(reference string) (string, error) {
	return parseReferenceID(reference, queuedReferencePrefix, "invalid queued authorisation reference")
}

// parseReferenceID returns the ID of a prefix.id.secret reference whose two
// parts are canonical 16-byte base64url values.
func parseReferenceID(reference, prefix, invalid string) (string, error) {
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != prefix {
		return "", errors.New(invalid)
	}
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(raw) != 16 || base64.RawURLEncoding.EncodeToString(raw) != part {
			return "", errors.New(invalid)
		}
	}
	return parts[1], nil
}

// queuedTermsDigest is the lowercase hex SHA-256 of the terms' JSON encoding.
// Every time in the terms is UTC, so a stored record re-encodes to the same
// bytes.
func queuedTermsDigest(t rpc.QueuedAuthTerms) (string, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// queuedRowTermsDigest binds the row a send must still find under the key:
// its bucket, exact contract and side, position effect and order form.
func queuedRowTermsDigest(p rpc.TradeProposal) string {
	orderType := strings.ToUpper(strings.TrimSpace(p.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	raw, _ := json.Marshal(struct {
		Bucket         string `json:"bucket"`
		ContractSide   string `json:"contract_side"`
		PositionEffect string `json:"position_effect"`
		OrderType      string `json:"order_type"`
		TIF            string `json:"tif"`
	}{p.Bucket, sameContractSide(p), strings.ToLower(strings.TrimSpace(p.PositionEffect)), orderType, proposalTIF(p)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// queuedQueueableBlockers refuses a row a queue cannot carry: only
// single-contract governor, theta and issuer-trim reductions that close or
// reduce with a DAY limit. Loss exits need a live bid first.
func queuedQueueableBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	block := func(code, message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: code, Message: message, Action: "Review the proposal when the market is open instead."}}
	}
	orderType := strings.ToUpper(strings.TrimSpace(p.OrderType))
	switch {
	case p.Shadow:
		return shadowProposalBlockers(p)
	case !slices.Contains(readinessQueueBuckets, p.Bucket) || !proposalIsReduction(p):
		return block("queue_bucket_not_queueable", "only governor, theta and issuer-trim reductions can be queued for the open; exits need a live bid first")
	case p.Contract.ConID <= 0 || sameContractSide(p) == "":
		return block("queue_contract_inexact", "a queued authorisation needs the row's exact broker contract id")
	case !proposalCloseReduceEffect(p.PositionEffect):
		return block("proposal_effect_not_close_reduce", fmt.Sprintf("proposal effect %q is not close/reduce", p.PositionEffect))
	case orderType != "" && orderType != rpc.OrderTypeLMT:
		return block("queue_order_type", fmt.Sprintf("order type %q cannot be queued; only limit orders can", p.OrderType))
	case proposalTIF(p) != rpc.OrderTIFDay:
		return block("queue_order_type", "only DAY orders can be queued")
	case p.Quantity <= 0:
		return block("queue_quantity", "the row asks for no quantity")
	}
	return nil
}

// queuedNotOfferedBlocker explains why readiness does not offer a queue.
func queuedNotOfferedBlocker(r *rpc.TradeProposalReadiness) rpc.TradingBlocker {
	b := rpc.TradingBlocker{Code: "queue_not_offered", Message: "a queue is offered only while the market is closed or in its opening window"}
	if r == nil {
		return b
	}
	switch r.Code {
	case rpc.ReadinessReady:
		b.Message, b.Action = "the market is open past its opening window; nothing needs queueing", "Send the order now instead."
	case rpc.ReadinessTradingFrozen:
		b.Message = "trading is frozen; nothing can be queued while it is"
	case rpc.ReadinessBrokerUnavailable:
		b.Message = "the broker link is not ready; nothing can be queued"
	default:
		if msg := strings.TrimSpace(r.Message); msg != "" {
			b.Message += ": " + msg
		}
	}
	return b
}

// queuedTerms fixes the terms of a queued authorisation for prop: the send
// window in the session the calendar dates next, the bounded limit and the
// worst price from the row's mark.
func (e *proposalEngine) queuedTerms(prop rpc.TradeProposal, policy protectionPolicy, status rpc.ProtectionPolicyStatus, quantity int, scope brokerStateScope, now time.Time) (rpc.QueuedAuthTerms, []rpc.TradingBlocker) {
	unknown := func(message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: "queue_session_unknown", Message: message, Action: "Review the proposal when the market is open instead."}}
	}
	market, ok := quoteSessionMarketForContract(prop.Contract)
	if !ok {
		return rpc.QueuedAuthTerms{}, unknown("this contract's market has no embedded trading calendar, so no send window can be dated")
	}
	session, ok := e.server.previewSession(market, now)
	if !ok || session.State == marketcal.StateUnknown {
		return rpc.QueuedAuthTerms{}, unknown("the trading calendar does not cover today for this market")
	}
	var opens time.Time
	switch {
	case session.IsOpen && !session.Open.IsZero():
		opens = session.Open
	case session.NextOpen != nil:
		opens = *session.NextOpen
	default:
		return rpc.QueuedAuthTerms{}, unknown("the trading calendar dates no next regular open for this market")
	}
	notBefore := opens.Add(readinessOpeningOffset(market)).UTC()
	day, ok := e.server.previewSession(market, notBefore)
	if !ok || !day.IsOpen {
		return rpc.QueuedAuthTerms{}, unknown("the trading calendar does not show the regular session open at the send time")
	}
	notAfter := notBefore.Add(queuedSendWindow)
	if !day.Close.IsZero() && day.Close.Before(notAfter) {
		notAfter = day.Close.UTC()
	}
	if !notAfter.After(notBefore) || !notAfter.After(now) {
		return rpc.QueuedAuthTerms{}, unknown("the send window would be empty")
	}
	if prop.LimitPrice == nil || !positiveFinite(*prop.LimitPrice) {
		return rpc.QueuedAuthTerms{}, []rpc.TradingBlocker{{Code: "queue_reference_mark_missing", Message: "the row carries no position mark, so no worst price can be set", Action: "Refresh proposals once the position has a mark."}}
	}
	mark := *prop.LimitPrice
	maxQty := prop.Quantity
	if quantity > 0 {
		maxQty = min(quantity, prop.Quantity)
	}
	action := strings.ToUpper(strings.TrimSpace(prop.Action))
	return rpc.QueuedAuthTerms{
		Version: queuedTermsVersion, AccountID: scope.Account, AccountMode: scope.Mode, Key: prop.Key, Bucket: prop.Bucket,
		RevisionAtQueue: prop.Revision, Contract: prop.Contract, Action: action, PositionEffect: prop.PositionEffect,
		PositionQuantity: prop.PositionQuantity, MaxQuantity: maxQty, Style: rpc.QueuedAuthStyleBoundedLimit,
		Concession: queuedConcession, WorstPrice: queuedDefaultWorstPrice(action, mark, prop.Contract), ReferenceMark: mark,
		ReferenceMarkAt: prop.CreatedAt.UTC(), MaxSpreadPctOfMid: queuedSpreadLimit(policy, prop), Currency: prop.Contract.Currency,
		Market: string(market), SessionDate: day.Date, NotBefore: notBefore, NotAfter: notAfter, TIF: rpc.OrderTIFDay,
		ArmDeadline: now.Add(queuedArmWindow).UTC(), RowTermsDigest: queuedRowTermsDigest(prop),
		PolicyFingerprint: status.EffectiveFingerprint, RulebookFingerprint: prop.SourceFingerprints.EffectiveRulebook,
	}, nil
}

// queuedDefaultWorstPrice moves the reference mark 25% against the order and
// rounds inward on the tick grid: a sell's floor up, a buy's ceiling down.
func queuedDefaultWorstPrice(action string, mark float64, contract rpc.ContractParams) float64 {
	if action == rpc.OrderActionBuy {
		worst := mark * (1 + queuedWorstMove)
		return floorPriceToTick(worst, queuedPriceTick(contract, worst))
	}
	worst := mark * (1 - queuedWorstMove)
	return ceilPriceToTick(worst, queuedPriceTick(contract, worst))
}

// queuedPriceTick is the grid a queued price lands on before a live quote
// exists: an option's nickel grid from $3.00, else the patient-limit grid,
// never finer than the broker's minimum tick.
func queuedPriceTick(contract rpc.ContractParams, price float64) float64 {
	tick := priceTick(price)
	if strings.EqualFold(strings.TrimSpace(contract.SecType), "OPT") {
		tick = 0.01
		if price >= optionPennyBandCeiling {
			tick = optionCoarseTick
		}
	}
	return max(tick, contract.MinTick)
}

// queuedSpreadLimit is the policy's spread limit for the row: theta
// hygiene's own for its options, the option trail's for other options (25%),
// the stock trail's for stocks and ETFs (2%).
func queuedSpreadLimit(policy protectionPolicy, prop rpc.TradeProposal) float64 {
	if strings.EqualFold(strings.TrimSpace(prop.Contract.SecType), "OPT") {
		if prop.Bucket == rpc.TradeProposalBucketThetaHygiene && policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid > 0 {
			return policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid
		}
		if v := policy.Buckets.TrailingStop.Options.MaxSpreadPctOfMid; v > 0 {
			return v
		}
		return 25
	}
	if v := policy.Buckets.TrailingStop.StockETF.MaxSpreadPctOfMid; v > 0 {
		return v
	}
	return 2
}

// automaticIntentBlockers refuses a queue while the pre-authorised scheduler
// may place an order for the same row or the same contract and side.
func (e *proposalEngine) automaticIntentBlockers(prop rpc.TradeProposal, scope brokerStateScope) []rpc.TradingBlocker {
	return e.automaticIntentBlockersFor(prop.Key, sameContractSide(prop), scope)
}

// automaticIntentBlockersFor is automaticIntentBlockers for a key and a
// contract side; arm re-checks it for the record it arms. It reads the
// automatic store, so it is never called inside a queued-store update.
func (e *proposalEngine) automaticIntentBlockersFor(key, side string, scope brokerStateScope) []rpc.TradingBlocker {
	if e == nil || !e.automatic.attached() {
		return nil
	}
	served := map[string]rpc.TradeProposal{}
	for _, row := range e.Snapshot(false).Proposals {
		served[row.Key] = row
	}
	for _, rec := range e.automatic.list() {
		if !(rec.waiting() || rec.State == rpc.TradeProposalAutomaticSubmitting) || !sameBrokerScope(brokerStateScope{Account: rec.AccountID, Mode: rec.AccountMode}, scope) {
			continue
		}
		row, ok := served[rec.Key]
		if rec.Key == key || ok && side != "" && sameContractSide(row) == side {
			return []rpc.TradingBlocker{{Code: "automatic_submission_pending", Message: "Canary's pre-authorised protection may place an order for this contract; nothing is queued beside it",
				Action: "Veto the automatic submission first if you want to queue this row instead."}}
		}
	}
	return nil
}

// queuedIntentExistsCode refuses a second intent beside a live queued
// authorisation for the same contract and side.
const queuedIntentExistsCode = "queued_intent_exists"

func queuedIntentExistsBlocker(key string) []rpc.TradingBlocker {
	return []rpc.TradingBlocker{{Code: queuedIntentExistsCode, Message: fmt.Sprintf("an armed queued authorisation (%s) already covers this contract and side", key),
		Action: "Cancel the queued authorisation first to queue a different one."}}
}

// queuedOrderWorkingBlocker refuses a queue while a queued order for the same
// contract and side still works at the broker.
func queuedOrderWorkingBlocker(key string) []rpc.TradingBlocker {
	return []rpc.TradingBlocker{{Code: "queued_order_working", Message: fmt.Sprintf("a queued order (%s) for this contract and side is working at the broker", key),
		Action: "Wait until it fills or ends, or cancel it at the broker first."}}
}

// QueuePrepare fixes the terms of a queued authorisation for one row and
// returns them with a private reference. It sends nothing and reads no quote.
func (e *proposalEngine) QueuePrepare(ctx context.Context, p rpc.TradeProposalQueuePrepareParams) (out rpc.TradeProposalQueuePrepareResult, err error) {
	started := time.Now()
	now := e.clock()
	out.AsOf = now
	defer func() {
		d := proposalDecision{event: "queue_prepare", key: p.Key, rev: p.Revision, accepted: out.Accepted, accept: decisionQueued,
			blockers: out.Blockers, err: err, readiness: out.Readiness, mode: e.decisionMode(nil), started: started}
		if out.Proposal != nil {
			d.prop = *out.Proposal
		}
		if out.Queue != nil {
			d.queue = out.Queue.Terms.QueueID
		}
		e.recordDecision(d)
	}()
	if p.Quantity < 0 {
		return out, errBadRequest("queue prepare quantity must not be negative")
	}
	if !e.queued.attached() {
		out.Blockers = preparedBlocker("queue_unavailable", "Queued authorisations are unavailable: the daemon state store is not attached.")
		return out, nil
	}
	if !e.server.cfg.AutoTrade.WithDefaults().FastPathEnabledResolved() {
		out.Blockers = preparedBlocker("fast_path_disabled", "Proposal submit is disabled by [auto_trade].fast_path_enabled, so nothing can be queued.")
		return out, nil
	}
	prop, blockers, err := e.resolveProposal(ctx, p.Key, p.Revision)
	if prop.Key != "" {
		out.Proposal = &prop
	}
	if err != nil || len(blockers) > 0 {
		out.Blockers = blockers
		out.Readiness = e.refusalReadiness(prop, blockers, err)
		return out, err
	}
	if blockers := queuedQueueableBlockers(prop); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	// A row a queue already covers names that queue, not the session.
	if q := prop.Queued; q != nil {
		out.Blockers = queuedIntentExistsBlocker(q.Key)
		if q.State == rpc.QueuedAuthSent {
			out.Blockers = queuedOrderWorkingBlocker(q.Key)
		}
		return out, nil
	}
	out.Readiness = e.classifyReadiness(prop, nil, true, readinessSessions{})
	if out.Readiness == nil || !out.Readiness.Queueable {
		out.Blockers = []rpc.TradingBlocker{queuedNotOfferedBlocker(out.Readiness)}
		return out, nil
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		out.Blockers = []rpc.TradingBlocker{proposalScopeUnscopedBlocker(scope)}
		return out, nil
	}
	if e.server.protectionPolicies == nil {
		out.Blockers = preparedBlocker("policy_unavailable", "No protection policy is loaded, so the queue's spread limit cannot be set.")
		return out, nil
	}
	policy, status := e.server.protectionPolicies.Active()
	terms, blockers := e.queuedTerms(prop, policy, status, p.Quantity, scope, now)
	if len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	if blockers := e.automaticIntentBlockers(prop, scope); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	id, reference, err := mintQueuedReference()
	if err != nil {
		return out, err
	}
	terms.QueueID = id
	digest, err := queuedTermsDigest(terms)
	if err != nil {
		return out, err
	}
	rec := &queuedAuthRecord{Version: queuedDocumentVersion, ReferenceHash: sha256.Sum256([]byte(reference)), Terms: terms, TermsDigest: digest,
		ContractSide: sameContractSide(prop), CreatedAt: now.UTC()}
	var refused []rpc.TradingBlocker
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, other := range records {
			if other.ContractSide != rec.ContractSide || !sameBrokerScope(other.scope(), scope) {
				continue
			}
			if other.liveIntent() {
				refused = queuedIntentExistsBlocker(other.Terms.Key)
				return nil
			}
			if other.State == rpc.QueuedAuthSent {
				refused = queuedOrderWorkingBlocker(other.Terms.Key)
				return nil
			}
			if other.State == rpc.QueuedAuthPrepared {
				// A second sheet for one contract replaces the unarmed one,
				// which never carried authority.
				events = append(events, other.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonSuperseded, "a newer queue preparation for this contract replaced it before it was armed", now))
			}
		}
		records[id] = rec
		return append(events, rec.transition(queuedEventPrepared, rpc.QueuedAuthPrepared, now))
	})
	if err != nil {
		out.Blockers = preparedBlocker("queue_not_persisted", "The queued authorisation could not be stored; nothing was queued.")
		return out, err
	}
	if len(refused) > 0 {
		out.Blockers = refused
		return out, nil
	}
	view := rec.view()
	out.Accepted, out.QueuedRef, out.Queue = true, reference, &view
	return out, nil
}

// loadQueuedByReference resolves a private reference to its record.
func (e *proposalEngine) loadQueuedByReference(reference string) (queuedAuthRecord, bool) {
	id, err := parseQueuedReference(strings.TrimSpace(reference))
	if err != nil {
		return queuedAuthRecord{}, false
	}
	rec, ok := e.queued.get(id)
	if !ok {
		return queuedAuthRecord{}, false
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(reference)))
	if subtle.ConstantTimeCompare(rec.ReferenceHash[:], digest[:]) != 1 {
		return queuedAuthRecord{}, false
	}
	return rec, true
}

// QueueArm arms a prepared record once the owner has signed its terms. It
// sends nothing; the executor sends inside the window after every gate.
func (e *proposalEngine) QueueArm(ctx context.Context, p rpc.TradeProposalQueueArmParams) (out rpc.TradeProposalQueueResult, err error) {
	started := time.Now()
	now := e.clock()
	out.AsOf = now
	var armed queuedAuthRecord
	defer func() {
		d := proposalDecision{event: "queue_arm", accepted: out.Accepted, accept: decisionArmed, blockers: out.Blockers, err: err, mode: e.decisionMode(nil), started: started}
		if out.Queue != nil {
			d.queue, d.key, d.prop.Bucket = out.Queue.Terms.QueueID, out.Queue.Terms.Key, out.Queue.Terms.Bucket
		}
		e.recordDecision(d)
	}()
	block := func(code, message string) (rpc.TradeProposalQueueResult, error) {
		out.Blockers = preparedBlocker(code, message)
		return out, nil
	}
	if !e.queued.attached() {
		return block("queue_unavailable", "Queued authorisations are unavailable: the daemon state store is not attached.")
	}
	if len(p.Envelope) > queuedEnvelopeMaxBytes || len(p.DeskActionID) > queuedIdentityMaxBytes || len(p.Credential) > queuedIdentityMaxBytes {
		return out, errBadRequest("queue arm audit fields exceed their bounds")
	}
	rec, ok := e.loadQueuedByReference(p.QueuedRef)
	if !ok {
		return block("queued_reference_unavailable", "The queued authorisation reference cannot be resolved safely.")
	}
	view := rec.view()
	out.Queue = &view
	if problem := queuedRecordProblem(rec); problem != "" {
		return block("queued_record_invalid", "This queued authorisation cannot be armed: "+problem+"; nothing was armed.")
	}
	signed := strings.ToLower(strings.TrimSpace(p.TermsDigest))
	if subtle.ConstantTimeCompare([]byte(signed), []byte(rec.TermsDigest)) != 1 {
		return block("queued_terms_mismatch", "The signed terms digest does not match the prepared terms; nothing was armed.")
	}
	if rec.State != rpc.QueuedAuthPrepared {
		return block("queued_not_prepared", "This queued authorisation is already "+rec.State+" and cannot be armed again.")
	}
	if !now.Before(rec.Terms.ArmDeadline) {
		e.expireUnarmed(ctx, rec.Terms.QueueID, now)
		if current, ok := e.queued.get(rec.Terms.QueueID); ok {
			view := current.view()
			out.Queue = &view
		}
		return block("queued_arm_deadline_passed", "The owner's signature arrived after the ten-minute arm deadline; prepare the queue again.")
	}
	if !e.server.cfg.AutoTrade.WithDefaults().FastPathEnabledResolved() {
		return block("fast_path_disabled", "Proposal submit is disabled by [auto_trade].fast_path_enabled, so nothing can be armed.")
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) || !sameBrokerScope(scope, rec.scope()) {
		return block("queued_scope_mismatch", "The connected account or paper/live mode differs from the one the queue was prepared for.")
	}
	// The pre-authorised scheduler may have created a record for this row or
	// its contract and side since prepare. Checked before the queued-store
	// update, never inside it (lock order); the submit gates refuse the
	// scheduler's send while this record is live.
	if blockers := e.automaticIntentBlockersFor(rec.Terms.Key, rec.ContractSide, scope); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	// The settling baseline: hand orders working now never hold the send;
	// one placed or modified after the arm does.
	book := e.automaticSettlingBook(ctx, scope, false)
	var refused []rpc.TradingBlocker
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[rec.Terms.QueueID]
		if r == nil || r.State != rpc.QueuedAuthPrepared || !now.Before(r.Terms.ArmDeadline) {
			refused = preparedBlocker("queued_not_prepared", "The queued authorisation changed while it was being armed; nothing was armed.")
			return nil
		}
		for _, other := range records {
			if other == r || other.ContractSide != r.ContractSide || !sameBrokerScope(other.scope(), r.scope()) {
				continue
			}
			switch {
			case other.liveIntent():
				refused = queuedIntentExistsBlocker(other.Terms.Key)
				return nil
			case other.State == rpc.QueuedAuthSent:
				refused = queuedOrderWorkingBlocker(other.Terms.Key)
				return nil
			}
		}
		r.ArmedAt, r.DeskActionID, r.Credential, r.Envelope = now.UTC(), strings.TrimSpace(p.DeskActionID), strings.TrimSpace(p.Credential), p.Envelope
		r.ArmOrigin = normalizedWriteOrigin(p.Origin)
		r.SettlingBaseline, r.SettlingBaselineKnown = book.marks(), book.ok
		ev := r.transition(queuedEventArmed, rpc.QueuedAuthArmed, now)
		ev.Origin, ev.Credential = r.ArmOrigin, r.Credential
		armed = *r
		return []queuedAuthEvent{ev}
	})
	if err != nil {
		return block("queue_not_persisted", "The arm could not be stored; nothing was armed.")
	}
	if len(refused) > 0 {
		out.Blockers = refused
		return out, nil
	}
	view = armed.view()
	out.Accepted, out.Queue = true, &view
	out.Message = fmt.Sprintf("armed: sends from %s until %s if every gate still passes", armed.Terms.NotBefore.Format(time.RFC3339), armed.Terms.NotAfter.Format(time.RFC3339))
	if e.server != nil {
		e.server.infof("queued authorisation %s armed for %s; sends from %s", armed.Terms.QueueID, armed.Terms.Key, armed.Terms.NotBefore.Format(time.RFC3339))
	}
	e.Kick()
	return out, nil
}

// expireUnarmed expires a prepared record whose arm deadline has passed.
func (e *proposalEngine) expireUnarmed(ctx context.Context, id string, now time.Time) {
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[id]
		if r == nil || r.State != rpc.QueuedAuthPrepared || now.Before(r.Terms.ArmDeadline) {
			return nil
		}
		return []queuedAuthEvent{r.resolve(queuedEventExpired, rpc.QueuedAuthExpired, queuedReasonNotArmed, "the owner's signature did not arrive within ten minutes", now)}
	})
	if err != nil && e.server != nil {
		e.server.warnf("queued authorisation %s: record expiry: %v", id, err)
	}
}

// QueueCancel withdraws one record, or with All every record not yet sent.
// It only removes authority, so any origin may ask; the origin is recorded.
func (e *proposalEngine) QueueCancel(ctx context.Context, p rpc.TradeProposalQueueCancelParams) (out rpc.TradeProposalQueueResult, err error) {
	started := time.Now()
	now := e.clock()
	out.AsOf = now
	id := strings.TrimSpace(p.QueueID)
	defer func() {
		d := proposalDecision{event: "queue_cancel", accepted: out.Accepted, accept: decisionCancelled, blockers: out.Blockers, err: err, queue: id, mode: e.decisionMode(nil), started: started}
		if out.Queue != nil {
			d.key, d.prop.Bucket = out.Queue.Terms.Key, out.Queue.Terms.Bucket
		}
		e.recordDecision(d)
	}()
	if p.All == (id != "") {
		return out, errBadRequest("queue cancel needs exactly one of queue_id or all")
	}
	if !e.queued.attached() {
		out.Blockers = preparedBlocker("queue_unavailable", "Queued authorisations are unavailable: the daemon state store is not attached.")
		return out, nil
	}
	origin := normalizedWriteOrigin(p.Origin)
	reason := nonEmptyString(boundedQueuedReason(strings.TrimSpace(p.Reason)), "cancelled by the owner")
	var cancelled, inFlight []queuedAuthRecord
	var single *queuedAuthRecord
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, r := range records {
			if !p.All && r.Terms.QueueID != id {
				continue
			}
			if !p.All {
				copied := *r
				single = &copied
			}
			if p.PreparedOnly && r.State != rpc.QueuedAuthPrepared {
				continue
			}
			switch r.State {
			case rpc.QueuedAuthSending:
				// The order is being placed now. The request is kept: an
				// attempt that proves unsent ends cancelled, never waits again.
				if !r.CancelRequested {
					r.CancelRequested, r.CancelOrigin = true, origin
					ev := r.note(queuedEventCancelRequested, r.State, now)
					ev.Origin, ev.Reason = origin, reason
					events = append(events, ev)
				}
				inFlight = append(inFlight, *r)
				if !p.All {
					copied := *r
					single = &copied
				}
				continue
			case rpc.QueuedAuthSent:
				inFlight = append(inFlight, *r)
				continue
			}
			if r.State != rpc.QueuedAuthPrepared && !r.waiting() {
				continue
			}
			r.CancelOrigin = origin
			ev := r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonOwnerCancelled, reason, now)
			ev.Origin = origin
			events = append(events, ev)
			cancelled = append(cancelled, *r)
			if !p.All {
				copied := *r
				single = &copied
			}
		}
		return events
	})
	if err != nil {
		out.Blockers = preparedBlocker("queue_not_persisted", "The cancel could not be stored; the queued authorisation may still send. Retry the cancel.")
		return out, nil
	}
	slices.SortFunc(cancelled, compareQueuedRecords)
	slices.SortFunc(inFlight, compareQueuedRecords)
	for _, r := range cancelled {
		out.Queues = append(out.Queues, r.view())
	}
	for _, r := range inFlight {
		out.InFlight = append(out.InFlight, r.view())
	}
	if p.All {
		out.Accepted = true
		out.Message = fmt.Sprintf("cancelled %d queued authorisation(s) not yet sent", len(cancelled))
		if len(inFlight) > 0 {
			out.Message += fmt.Sprintf("; %d could not be cancelled here: being placed now (the cancel is kept, so an unsent attempt ends cancelled) or sent and working at the broker (cancel those with `canary order cancel`)", len(inFlight))
		}
		return out, nil
	}
	if single == nil {
		out.Blockers = preparedBlocker("queued_not_found", "No queued authorisation has this ID.")
		return out, nil
	}
	view := single.view()
	out.Queue = &view
	switch {
	case len(cancelled) == 1:
		out.Accepted, out.Message = true, "cancelled; nothing will be sent"
	case p.PreparedOnly && !single.final():
		out.Blockers = preparedBlocker("queued_not_prepared", "It was confirmed meanwhile, so it stays queued; cancel it from the queued list to withdraw it.")
	case single.State == rpc.QueuedAuthSending:
		out.Blockers = preparedBlocker("queued_sending", "The order is being placed now. The cancel is kept: if the attempt did not reach the broker, the record ends cancelled and is never retried; if it did, cancel the order at the broker once it is journaled.")
	case single.State == rpc.QueuedAuthSent:
		out.Blockers = preparedBlocker("queued_sent", fmt.Sprintf("The order is at the broker; cancel it with `canary order cancel %s`.", single.OrderRef))
	default:
		out.Accepted, out.Message = true, "already "+single.State+"; nothing will be sent"
	}
	return out, nil
}

// decorateQueued names, on each served row, the queued authorisation for its
// exact contract and side in the snapshot's account and mode: armed, held or
// sending, or sent until its order resolves. It is read at serve time, like
// decorateAutomatic.
func (e *proposalEngine) decorateQueued(snap *rpc.TradeProposalSnapshot) {
	if e == nil || snap == nil || !e.queued.attached() {
		return
	}
	scope := brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}
	if !brokerScopeConcrete(scope) {
		return
	}
	marks := map[string]rpc.TradeProposalQueued{}
	for _, rec := range e.queued.list() {
		live := rec.liveIntent()
		if !live && rec.State != rpc.QueuedAuthSent || rec.ContractSide == "" || !sameBrokerScope(rec.scope(), scope) {
			continue
		}
		if prior, ok := marks[rec.ContractSide]; ok && prior.State != rpc.QueuedAuthSent && !live {
			continue
		}
		marks[rec.ContractSide] = rpc.TradeProposalQueued{QueueID: rec.Terms.QueueID, Key: rec.Terms.Key, State: rec.State,
			NotBefore: rec.Terms.NotBefore, NotAfter: rec.Terms.NotAfter}
	}
	if len(marks) == 0 {
		return
	}
	// A row a live queue covers is queued for the open, never ready to act:
	// the queued row itself leaves Actionable, the rows it holds back leave
	// the blocked remainder, and a covered row stays counted as covered.
	for i := range snap.Proposals {
		p := &snap.Proposals[i]
		mark, ok := marks[sameContractSide(*p)]
		if !ok {
			continue
		}
		p.Queued = &mark
		if mark.State == rpc.QueuedAuthSent || p.CoveredBy != "" {
			continue
		}
		if p.AutomaticEligible() {
			snap.Counts.Actionable--
		}
		snap.Counts.Queued++
	}
}

// QueueList lists the retained records, newest first, without references.
func (e *proposalEngine) QueueList(p rpc.TradeProposalQueueListParams) rpc.TradeProposalQueueListResult {
	out := rpc.TradeProposalQueueListResult{Queues: []rpc.QueuedAuth{}, AsOf: e.clock()}
	if scope := e.currentScope(); brokerScopeConcrete(scope) {
		out.AccountID, out.AccountMode = scope.Account, scope.Mode
	}
	records := e.queued.list()
	slices.Reverse(records)
	for _, r := range records {
		if p.LiveOnly && r.final() {
			continue
		}
		out.Queues = append(out.Queues, r.view())
	}
	return out
}

// QueueStatus reads one record by ID.
func (e *proposalEngine) QueueStatus(p rpc.TradeProposalQueueStatusParams) rpc.TradeProposalQueueResult {
	out := rpc.TradeProposalQueueResult{AsOf: e.clock()}
	rec, ok := e.queued.get(strings.TrimSpace(p.QueueID))
	if !ok {
		out.Blockers = preparedBlocker("queued_not_found", "No queued authorisation has this ID.")
		return out
	}
	view := rec.view()
	out.Accepted, out.Queue = true, &view
	return out
}

// queuedLimitInside reports whether limit is on the owner's side of worst.
func queuedLimitInside(action string, limit, worst float64) bool {
	if !positiveFinite(limit) || !positiveFinite(worst) {
		return false
	}
	if action == rpc.OrderActionBuy {
		return limit <= worst+1e-9
	}
	return limit+1e-9 >= worst
}

func queuedSendQuote(q rpc.OrderQuoteSnapshot) *rpc.QueuedAuthQuote {
	out := &rpc.QueuedAuthQuote{Bid: cloneFloat64Ptr(q.Bid), Ask: cloneFloat64Ptr(q.Ask), DataType: q.DataType, AsOf: q.AsOf.UTC()}
	if q.Bid != nil && q.Ask != nil {
		if mid := (*q.Bid + *q.Ask) / 2; mid > 0 && !math.IsNaN(mid) {
			out.SpreadPct = new((*q.Ask - *q.Bid) / mid * 100)
		}
	}
	return out
}

func (s *Server) handleTradeProposalsQueuePrepare(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalQueuePrepareResult, error) {
	var p rpc.TradeProposalQueuePrepareParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalQueuePrepareResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.QueuePrepare(ctx, p)
	return &out, err
}

func (s *Server) handleTradeProposalsQueueArm(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalQueueResult, error) {
	var p rpc.TradeProposalQueueArmParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalQueueResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.QueueArm(ctx, p)
	return &out, err
}

func (s *Server) handleTradeProposalsQueueCancel(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalQueueResult, error) {
	var p rpc.TradeProposalQueueCancelParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalQueueResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.QueueCancel(ctx, p)
	return &out, err
}

func (s *Server) handleTradeProposalsQueueList(req *rpc.Request) (*rpc.TradeProposalQueueListResult, error) {
	var p rpc.TradeProposalQueueListParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalQueueListResult{Queues: []rpc.QueuedAuth{}, AsOf: s.orderNow()}, nil
	}
	out := s.tradeProposals.QueueList(p)
	return &out, nil
}

func (s *Server) handleTradeProposalsQueueStatus(req *rpc.Request) (*rpc.TradeProposalQueueResult, error) {
	var p rpc.TradeProposalQueueStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalQueueResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out := s.tradeProposals.QueueStatus(p)
	return &out, nil
}
