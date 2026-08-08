package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	orderJournalFileVersion = 1

	orderJournalEventPreviewed          = "previewed"
	orderJournalEventTokenConfirmed     = "token-confirmed"
	orderJournalEventSendAttempted      = "send-attempted"
	orderJournalEventSendCompleted      = "send-completed"
	orderJournalEventSendError          = "send-error"
	orderJournalEventBrokerError        = "broker-error"
	orderJournalEventBrokerAcknowledged = "broker-acknowledged"
	orderJournalEventStatusUpdated      = "status-updated"
	orderJournalEventModifyRequested    = "modify-requested"
	orderJournalEventCancelRequested    = "cancel-requested"
	orderJournalEventReconciledUnknown  = "reconciled-unknown"
	orderJournalEventReconciledAbsent   = "reconciled-absent"

	orderSendStateReserved           = "reserved"
	orderSendStateSendAttempted      = "send_attempted"
	orderSendStateBrokerAcknowledged = "broker_acknowledged"
	orderSendStateUncertainSend      = "uncertain_send"
	orderSendStateTerminal           = "terminal"

	legacyPurgeExecuteSource = "purge"
	legacyPurgeRestoreSource = "purge_restore"
)

var errOrderPreviewTokenAlreadyUsed = errors.New("preview token already used")

type orderJournalStore struct {
	Path string
	mu   sync.RWMutex
	// evidenceMu linearizes Order Integrity and Protection alert-registry commits
	// against every local mutation of the authoritative order-event frontier.
	// Broker-derived commits take the write side only after taking the broker
	// evidence barrier; ordinary journal writers take the read side.
	evidenceMu sync.RWMutex
	authority  *corestore.Store
}

type orderJournalEvent struct {
	Version         int                      `json:"version"`
	At              time.Time                `json:"at"`
	Type            string                   `json:"type"`
	OrderRef        string                   `json:"order_ref,omitempty"`
	PreviewTokenID  string                   `json:"preview_token_id,omitempty"`
	ReservedOrderID int                      `json:"reserved_order_id,omitempty"`
	ClientID        int                      `json:"client_id,omitempty"`
	PermID          int                      `json:"perm_id,omitempty"`
	Account         string                   `json:"account,omitempty"`
	Endpoint        string                   `json:"endpoint,omitempty"`
	Mode            string                   `json:"mode,omitempty"`
	Source          string                   `json:"source,omitempty"`
	Origin          string                   `json:"origin,omitempty"`
	PurgeID         string                   `json:"purge_id,omitempty"`
	LegID           string                   `json:"leg_id,omitempty"`
	BypassPreview   bool                     `json:"bypass_preview,omitempty"`
	Symbol          string                   `json:"symbol,omitempty"`
	SecType         string                   `json:"sec_type,omitempty"`
	ConID           int                      `json:"con_id,omitempty"`
	Exchange        string                   `json:"exchange,omitempty"`
	PrimaryExch     string                   `json:"primary_exch,omitempty"`
	Currency        string                   `json:"currency,omitempty"`
	LocalSymbol     string                   `json:"local_symbol,omitempty"`
	TradingClass    string                   `json:"trading_class,omitempty"`
	Expiry          string                   `json:"expiry,omitempty"`
	Strike          float64                  `json:"strike,omitempty"`
	Right           string                   `json:"right,omitempty"`
	Multiplier      int                      `json:"multiplier,omitempty"`
	Action          string                   `json:"action,omitempty"`
	OrderType       string                   `json:"order_type,omitempty"`
	TIF             string                   `json:"tif,omitempty"`
	TriggerMethod   int                      `json:"trigger_method,omitempty"`
	OutsideRTH      bool                     `json:"outside_rth,omitempty"`
	Quantity        float64                  `json:"quantity,omitempty"`
	LimitPrice      float64                  `json:"limit_price,omitempty"`
	Trail           *rpc.OrderTrailSpec      `json:"trail,omitempty"`
	OpenClose       string                   `json:"open_close,omitempty"`
	Status          string                   `json:"status,omitempty"`
	Filled          float64                  `json:"filled,omitempty"`
	Remaining       float64                  `json:"remaining,omitempty"`
	AvgFillPrice    float64                  `json:"avg_fill_price,omitempty"`
	LastFillPrice   float64                  `json:"last_fill_price,omitempty"`
	WhyHeld         string                   `json:"why_held,omitempty"`
	MktCapPrice     float64                  `json:"mkt_cap_price,omitempty"`
	ExecID          string                   `json:"exec_id,omitempty"`
	ExecTime        string                   `json:"exec_time,omitempty"`
	ErrorCode       int                      `json:"error_code,omitempty"`
	SendState       string                   `json:"send_state,omitempty"`
	AttemptID       string                   `json:"attempt_id,omitempty"`
	ActionKind      corestore.ActionKind     `json:"action_kind,omitempty"`
	TransmitOrigin  corestore.TransmitOrigin `json:"transmit_origin,omitempty"`
	SendDisposition ibkrlib.SendDisposition  `json:"send_disposition,omitempty"`
	Message         string                   `json:"message,omitempty"`

	// clientIDPresent records whether a decoded legacy JSON object actually
	// carried client_id. Zero is a valid IBKR client ID, so the numeric Go
	// value alone cannot distinguish an explicit zero from an omitted field.
	// It is importer metadata only and never becomes part of the event JSON.
	clientIDPresent bool `json:"-"`
}

type orderJournalSummary struct {
	OpenOrders int
	LastEvent  string
}

// legacyOrderRoute is the complete broker identity carried forward during the
// one-time JSONL import. A broker order ID or local reference has no authority
// outside this four-dimensional route.
type legacyOrderRoute struct {
	Endpoint      string
	ClientID      int
	ClientIDKnown bool
	Account       string
	Mode          string
}

type legacyConsumedPreviewToken struct {
	TokenID string
	Route   legacyOrderRoute
	At      time.Time
	Type    string
}

type legacyScopedOrderIDFloor struct {
	Route legacyOrderRoute
	Floor int
}

// legacyOrderImportSelection is deliberately narrower than the old journal.
// Events retains complete chains only where omitting history could weaken a
// recovery decision. ConsumedTokens and GlobalOrderIDFloor are computed over
// every valid input row, including safely omitted terminal chains.
type legacyOrderImportSelection struct {
	SourceFingerprint    string
	Events               []orderJournalEvent
	SourceEvents         []orderJournalEvent
	ConsumedTokens       []legacyConsumedPreviewToken
	GlobalOrderIDFloor   int
	ScopedOrderIDFloors  map[string]legacyScopedOrderIDFloor
	ReconciliationEvents int
}

type legacyOrderImportParity struct {
	SourceFingerprint    string
	RetainedEventCount   int
	RetainedChainCount   int
	ConsumedTokenCount   int
	ConsumedTokenRoutes  map[string]legacyOrderRoute
	GlobalOrderIDFloor   int
	ScopedOrderIDFloors  map[string]int
	ReconciliationEvents int
}

type legacyTradingAuthorityParity struct{ Orders legacyOrderImportParity }

// initializeFreshTradingAuthority establishes deterministic empty order safety
// state without consulting legacy paths. The corestore operation is atomic and
// refuses partial or nonempty trading authority.
func initializeFreshTradingAuthority(ctx context.Context, store *corestore.Store) error {
	if store == nil {
		return fmt.Errorf("trading authority is unavailable")
	}
	if err := store.InitializeFreshOrderAuthority(ctx); err != nil {
		return fmt.Errorf("initialize fresh trading authority: %w", err)
	}
	return nil
}

func defaultOrderJournalPath() (string, error) {
	return defaultTradingStatePath("order-journal.jsonl")
}

func newOrderJournalStore(path string) *orderJournalStore {
	return &orderJournalStore{Path: path}
}

// UseCoreStore switches every live order read and write to daemon.db. Path is
// retained only for the explicit legacy importer; it is never a fallback.
func (s *orderJournalStore) UseCoreStore(store *corestore.Store) error {
	if s == nil || store == nil {
		return fmt.Errorf("order journal authority is unavailable")
	}
	if !store.Health().Ready {
		return fmt.Errorf("order journal authority is blocked")
	}
	s.evidenceMu.RLock()
	s.mu.Lock()
	s.authority = store
	s.mu.Unlock()
	s.evidenceMu.RUnlock()
	return nil
}

func (s *orderJournalStore) coreStore() (*corestore.Store, error) {
	if s == nil {
		return nil, fmt.Errorf("order journal authority is unavailable")
	}
	s.mu.RLock()
	store := s.authority
	s.mu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("order journal authority is unavailable")
	}
	if !store.Health().Ready {
		return nil, fmt.Errorf("order journal authority is blocked")
	}
	return store, nil
}

// attachCoreOrderAuthority is called by the Start winner after cutover and
// before RPC serving or broker connection. It also rotates token verification
// into daemon.db's authority epoch/signer generation.
func (s *Server) attachCoreOrderAuthority(ctx context.Context, store *corestore.Store) error {
	if s == nil || store == nil || s.orderJournal == nil || s.orderTokens == nil || !s.tradingReadiness.attachedTo(store) {
		return fmt.Errorf("order authority adapters are unavailable")
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		return fmt.Errorf("read order authority head: %w", err)
	}
	if err := s.orderTokens.bindAuthority(head.AuthorityEpoch, head.SignerGeneration); err != nil {
		return err
	}
	if err := s.orderJournal.UseCoreStore(store); err != nil {
		return err
	}
	return nil
}

func (s *orderJournalStore) Append(ev orderJournalEvent) error {
	return s.AppendAll([]orderJournalEvent{ev})
}

func (s *orderJournalStore) AppendAll(events []orderJournalEvent) error {
	return s.appendAllAtHead(events, -1)
}

func (s *orderJournalStore) AppendAllAtHead(events []orderJournalEvent, expectedLastEventSeq int64) error {
	if expectedLastEventSeq < 0 {
		return fmt.Errorf("expected order event head must not be negative")
	}
	return s.appendAllAtHead(events, expectedLastEventSeq)
}

func (s *orderJournalStore) appendAllAtHead(events []orderJournalEvent, expectedLastEventSeq int64) error {
	if len(events) == 0 {
		return nil
	}
	return s.withEvidenceMutation(func() error {
		return s.appendAllAtHeadLocked(events, expectedLastEventSeq)
	})
}

func (s *orderJournalStore) appendAllAtHeadLocked(events []orderJournalEvent, expectedLastEventSeq int64) error {
	store, err := s.coreStore()
	if err != nil {
		return err
	}
	records := make([]corestore.OrderEventRecord, 0, len(events))
	for _, ev := range events {
		record, _, err := coreOrderEventRecord(ev, "", "")
		if err != nil {
			return err
		}
		records = append(records, record)
	}
	var appendErr error
	if expectedLastEventSeq >= 0 {
		_, appendErr = store.AppendOrderEventsAtHead(context.Background(), expectedLastEventSeq, records)
	} else {
		_, appendErr = store.AppendOrderEvents(context.Background(), records)
	}
	if appendErr != nil {
		return fmt.Errorf("append authoritative order event: %w", appendErr)
	}
	return nil
}

func (s *orderJournalStore) AuthorityHead() (corestore.AuthorityHead, error) {
	store, err := s.coreStore()
	if err != nil {
		return corestore.AuthorityHead{}, err
	}
	return store.AuthorityHead(context.Background())
}

func (s *orderJournalStore) LoadEvents(limit int) ([]orderJournalEvent, error) {
	store, err := s.coreStore()
	if err != nil {
		return nil, err
	}
	var records []corestore.OrderEventRecord
	var after int64
	for {
		page, err := store.LoadOrderEvents(context.Background(), corestore.OrderQuery{AfterEventSeq: after, Limit: 10_000})
		if err != nil {
			return nil, fmt.Errorf("load authoritative order events: %w", err)
		}
		records = append(records, page...)
		if len(page) < 10_000 {
			break
		}
		after = page[len(page)-1].EventSeq
	}
	events := make([]orderJournalEvent, 0, len(records))
	for _, record := range records {
		ev, err := decodeCoreOrderEvent(record)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if limit > 0 && len(events) > limit {
		events = append([]orderJournalEvent(nil), events[len(events)-limit:]...)
	}
	return events, nil
}

// StagePreTransmit atomically consumes the global preview-token tombstone (if
// present), binds the complete route, advances global and scoped floors, and
// appends every ordered intent event before the caller may touch the broker.
func (s *orderJournalStore) StagePreTransmit(tokenID, authorityEpoch string, signerGeneration int64, requestedFloor int, action corestore.ActionKind, origin corestore.TransmitOrigin, events []orderJournalEvent) error {
	return s.withEvidenceMutation(func() error {
		_, err := s.stagePreTransmitLocked(tokenID, authorityEpoch, signerGeneration, requestedFloor, action, origin, events, nil)
		return err
	})
}

// StageModifyPreTransmit atomically requires the exact per-order frontier that
// validation observed and returns the modify event's durable sequence. The
// caller carries that sequence to the first-byte guard; any cancel or broker
// lifecycle event committed in between invalidates the pending modify.
func (s *orderJournalStore) StageModifyPreTransmit(tokenID, authorityEpoch string, signerGeneration int64, requestedFloor int, origin corestore.TransmitOrigin, expectedOrderEventSeq int64, events []orderJournalEvent) (int64, error) {
	if s == nil {
		return 0, fmt.Errorf("order journal evidence mutation is unavailable")
	}
	s.evidenceMu.RLock()
	defer s.evidenceMu.RUnlock()
	result, err := s.stagePreTransmitLocked(tokenID, authorityEpoch, signerGeneration, requestedFloor, corestore.ActionModify, origin, events, &expectedOrderEventSeq)
	if err != nil {
		return 0, err
	}
	if len(result.EventSeqs) == 0 {
		return 0, fmt.Errorf("modify pre-transmit stage returned no durable event sequence")
	}
	return result.EventSeqs[len(result.EventSeqs)-1], nil
}

func (s *orderJournalStore) stagePreTransmitLocked(tokenID, authorityEpoch string, signerGeneration int64, requestedFloor int, action corestore.ActionKind, origin corestore.TransmitOrigin, events []orderJournalEvent, expectedOrderEventSeq *int64) (corestore.PreTransmitResult, error) {
	store, err := s.coreStore()
	if err != nil {
		return corestore.PreTransmitResult{}, err
	}
	if len(events) == 0 {
		return corestore.PreTransmitResult{}, fmt.Errorf("pre-transmit order events are required")
	}
	records := make([]corestore.OrderEventRecord, 0, len(events))
	var scope corestore.BrokerScope
	reserved := requestedFloor
	for _, ev := range events {
		record, normalized, err := coreOrderEventRecord(ev, action, origin)
		if err != nil {
			return corestore.PreTransmitResult{}, err
		}
		if scope == (corestore.BrokerScope{}) {
			scope = record.Scope
		} else if record.Scope != scope {
			return corestore.PreTransmitResult{}, corestore.ErrBrokerScopeCollision
		}
		if normalized.ReservedOrderID > reserved {
			reserved = normalized.ReservedOrderID
		}
		records = append(records, record)
	}
	request := corestore.PreTransmitRequest{
		Scope: scope, AuthorityEpoch: authorityEpoch, SignerGeneration: signerGeneration,
		RequestedOrderIDFloor: int64(requestedFloor), ReservedOrderID: int64(reserved),
		ExpectedOrderEventSeq: expectedOrderEventSeq, Action: action, Origin: origin, Events: records,
	}
	if tokenID != "" {
		request.TokenDigest = corestore.HashPreviewTokenID(tokenID)
	}
	result, err := store.StagePreTransmit(context.Background(), request)
	if err != nil {
		if errors.Is(err, corestore.ErrPreviewTokenConsumed) {
			return corestore.PreTransmitResult{}, fmt.Errorf("%w: %s was already consumed", errOrderPreviewTokenAlreadyUsed, tokenID)
		}
		return corestore.PreTransmitResult{}, fmt.Errorf("stage authoritative pre-transmit intent: %w", err)
	}
	return result, nil
}

func (s *orderJournalStore) LatestOrderEventSeq(ev orderJournalEvent) (int64, error) {
	store, err := s.coreStore()
	if err != nil {
		return 0, err
	}
	scope, err := coreBrokerScopeFromEvent(ev)
	if err != nil {
		return 0, err
	}
	return store.LatestOrderEventSeq(context.Background(), scope, int64(ev.ReservedOrderID))
}

func (s *orderJournalStore) withEvidenceMutation(mutate func() error) error {
	if s == nil || mutate == nil {
		return fmt.Errorf("order journal evidence mutation is unavailable")
	}
	s.evidenceMu.RLock()
	defer s.evidenceMu.RUnlock()
	return mutate()
}

// WithStableAuthorityHead executes commit while all local order-journal
// mutations are excluded. False with nil error means a journal event landed
// after the caller's stable read, so commit was not called.
func (s *orderJournalStore) WithStableAuthorityHead(expectedLastEventSeq int64, commit func() error) (bool, error) {
	if s == nil || expectedLastEventSeq < 0 || commit == nil {
		return false, nil
	}
	s.evidenceMu.Lock()
	defer s.evidenceMu.Unlock()
	head, err := s.AuthorityHead()
	if err != nil {
		return false, err
	}
	if head.LastEventSeq != expectedLastEventSeq {
		return false, nil
	}
	if err := commit(); err != nil {
		return false, err
	}
	return true, nil
}

func coreOrderEventRecord(ev orderJournalEvent, action corestore.ActionKind, origin corestore.TransmitOrigin) (corestore.OrderEventRecord, orderJournalEvent, error) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	} else {
		ev.At = ev.At.UTC()
	}
	if ev.Version == 0 {
		ev.Version = orderJournalFileVersion
	}
	ev.Endpoint = strings.TrimSpace(ev.Endpoint)
	ev.Account = strings.ToUpper(strings.TrimSpace(ev.Account))
	ev.Mode = strings.ToLower(strings.TrimSpace(ev.Mode))
	if err := validateOrderJournalEvent(ev); err != nil {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, err
	}
	scope, err := coreBrokerScopeFromEvent(ev)
	if err != nil {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, err
	}
	if action == "" {
		action = ev.ActionKind
	}
	if action == "" {
		action = coreOrderActionForEvent(ev)
	}
	if action == "" {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, fmt.Errorf("order event %q requires an explicit broker action kind", ev.Type)
	}
	if ev.ActionKind != "" && ev.ActionKind != action {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, fmt.Errorf("order event action %q does not match durable action %q", ev.ActionKind, action)
	}
	ev.ActionKind = action
	if origin == "" {
		origin = ev.TransmitOrigin
	}
	if origin == "" {
		origin = coreOrderOrigin(ev.Origin)
	}
	if ev.TransmitOrigin != "" && ev.TransmitOrigin != origin {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, fmt.Errorf("order event origin %q does not match durable origin %q", ev.TransmitOrigin, origin)
	}
	ev.TransmitOrigin = origin
	raw, err := json.Marshal(ev)
	if err != nil {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, fmt.Errorf("marshal order event: %w", err)
	}
	eventID, err := randomTokenID()
	if err != nil {
		return corestore.OrderEventRecord{}, orderJournalEvent{}, fmt.Errorf("generate order event key: %w", err)
	}
	return corestore.OrderEventRecord{
		Scope: scope, EventKey: "order-" + eventID, AtMS: ev.At.UnixMilli(), Type: ev.Type,
		Action: action, Origin: origin, OrderRef: ev.OrderRef, PreviewTokenID: ev.PreviewTokenID,
		ReservedOrderID: int64(ev.ReservedOrderID), PermID: int64(ev.PermID), Status: ev.Status, RawJSON: raw,
	}, ev, nil
}

func coreBrokerScopeFromEvent(ev orderJournalEvent) (corestore.BrokerScope, error) {
	if strings.TrimSpace(ev.Endpoint) == "" || strings.TrimSpace(ev.Account) == "" || ev.ClientID < 0 {
		return corestore.BrokerScope{}, fmt.Errorf("order event requires complete endpoint/client/account/mode identity")
	}
	mode := strings.ToLower(strings.TrimSpace(ev.Mode))
	if mode != "paper" && mode != "live" {
		return corestore.BrokerScope{}, fmt.Errorf("order event broker mode %q is invalid", ev.Mode)
	}
	route := orderJournalRouteIdentity(ev.Endpoint, ev.ClientID, ev.Account, mode)
	digest := sha256.Sum256([]byte(route))
	return corestore.BrokerScope{
		ScopeKey: "broker-" + hex.EncodeToString(digest[:]), Endpoint: strings.TrimSpace(ev.Endpoint),
		ClientID: ev.ClientID, Account: strings.ToUpper(strings.TrimSpace(ev.Account)), Mode: mode,
	}, nil
}

func coreOrderActionForEvent(ev orderJournalEvent) corestore.ActionKind {
	if ev.ActionKind != "" {
		return ev.ActionKind
	}
	switch ev.Source {
	case legacyPurgeExecuteSource:
		return corestore.ActionPurge
	case legacyPurgeRestoreSource:
		return corestore.ActionRestore
	case "opportunity":
		return corestore.ActionExercise
	}
	switch ev.Type {
	case orderJournalEventModifyRequested:
		return corestore.ActionModify
	case orderJournalEventCancelRequested:
		return corestore.ActionCancel
	case orderJournalEventSendCompleted, orderJournalEventSendError:
		// A local send outcome is meaningless without its exact staged action.
		// Legacy import supplies its historical place-only interpretation at the
		// importer boundary; normal append must never infer it here.
		return ""
	default:
		return corestore.ActionPlace
	}
}

func coreOrderOrigin(origin string) corestore.TransmitOrigin {
	if strings.TrimSpace(origin) == "" {
		return corestore.OriginDaemon
	}
	switch normalizedWriteOrigin(origin) {
	case rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice:
		return corestore.OriginHumanCLI
	case rpc.OrderOriginAgent:
		return corestore.OriginAgentCLI
	default:
		return corestore.OriginDaemon
	}
}

func decodeCoreOrderEvent(record corestore.OrderEventRecord) (orderJournalEvent, error) {
	ev, err := decodeOrderJournalLine(record.RawJSON)
	if err != nil {
		return orderJournalEvent{}, fmt.Errorf("decode authoritative order event seq %d: %w", record.EventSeq, err)
	}
	want, err := coreBrokerScopeFromEvent(ev)
	if err != nil {
		return orderJournalEvent{}, fmt.Errorf("validate authoritative order event seq %d: %w", record.EventSeq, err)
	}
	if ev.ActionKind == "" {
		// Rows written before per-attempt payload provenance still have immutable
		// event_log.action_kind. Hydrate only that legacy omission; typed rows must
		// match exactly below.
		ev.ActionKind = record.Action
	} else if ev.ActionKind != record.Action {
		return orderJournalEvent{}, fmt.Errorf("authoritative order event seq %d action projection does not match payload", record.EventSeq)
	}
	if ev.TransmitOrigin == "" {
		// Rows written before payload provenance carried the immutable event-log
		// origin only. Hydrate that legacy omission; typed rows must match exactly.
		ev.TransmitOrigin = record.Origin
	} else if ev.TransmitOrigin != record.Origin {
		return orderJournalEvent{}, fmt.Errorf("authoritative order event seq %d origin projection does not match payload", record.EventSeq)
	}
	if want != record.Scope || ev.Type != record.Type || ev.OrderRef != record.OrderRef || ev.PreviewTokenID != record.PreviewTokenID || int64(ev.ReservedOrderID) != record.ReservedOrderID || int64(ev.PermID) != record.PermID || ev.Status != record.Status {
		return orderJournalEvent{}, fmt.Errorf("authoritative order event seq %d projection does not match payload", record.EventSeq)
	}
	return ev, nil
}

func decodeOrderJournalLine(raw []byte) (orderJournalEvent, error) {
	var ev orderJournalEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return orderJournalEvent{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return orderJournalEvent{}, err
	}
	if rawClientID, present := fields["client_id"]; present {
		var clientID *int
		if err := json.Unmarshal(rawClientID, &clientID); err != nil || clientID == nil {
			return orderJournalEvent{}, fmt.Errorf("client_id must be an integer")
		}
		ev.ClientID = *clientID
		ev.clientIDPresent = true
	}
	if ev.Version != orderJournalFileVersion {
		return orderJournalEvent{}, fmt.Errorf("unsupported order journal version %d", ev.Version)
	}
	if err := validateOrderJournalEvent(ev); err != nil {
		return orderJournalEvent{}, err
	}
	return ev, nil
}

// loadLegacyOrderImportSelection is the only post-cutover reader for the old
// order journal. It uses the same bounded decoder as the former runtime scan,
// accepts a valid unterminated final line, and never becomes a live fallback.
func loadLegacyOrderImportSelection(path string) (legacyOrderImportSelection, error) {
	var selection legacyOrderImportSelection
	selection.ScopedOrderIDFloors = map[string]legacyScopedOrderIDFloor{}
	if strings.TrimSpace(path) == "" {
		return selection, fmt.Errorf("legacy order journal path is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			empty := sha256.Sum256(nil)
			selection.SourceFingerprint = "sha256:" + hex.EncodeToString(empty[:])
			return selection, nil
		}
		return selection, fmt.Errorf("open legacy order journal %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(f, hash))
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	events := make([]orderJournalEvent, 0)
	line := 0
	consumedByID := map[string]legacyConsumedPreviewToken{}
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		ev, err := decodeOrderJournalLine([]byte(raw))
		if err != nil {
			return legacyOrderImportSelection{}, fmt.Errorf("parse legacy order journal line %d: %w", line, err)
		}
		events = append(events, ev)
		if ev.ReservedOrderID > selection.GlobalOrderIDFloor {
			selection.GlobalOrderIDFloor = ev.ReservedOrderID
		}
		if ev.ReservedOrderID > 0 {
			route := legacyOrderRouteFromEvent(ev)
			if route.complete() {
				key := route.key()
				floor := selection.ScopedOrderIDFloors[key]
				if ev.ReservedOrderID > floor.Floor {
					selection.ScopedOrderIDFloors[key] = legacyScopedOrderIDFloor{Route: route, Floor: ev.ReservedOrderID}
				}
			}
		}
		if ev.PreviewTokenID != "" && orderJournalEventConsumesPreviewToken(ev) {
			route := legacyOrderRouteFromEvent(ev)
			if !route.complete() {
				return legacyOrderImportSelection{}, fmt.Errorf("legacy consumed preview token on line %d lacks complete endpoint/client/account/mode identity", line)
			}
			next := legacyConsumedPreviewToken{TokenID: ev.PreviewTokenID, Route: route, At: ev.At, Type: ev.Type}
			if prior, ok := consumedByID[ev.PreviewTokenID]; ok {
				if prior.Route.key() != route.key() {
					return legacyOrderImportSelection{}, fmt.Errorf("legacy preview token %q was consumed in multiple broker routes", ev.PreviewTokenID)
				}
			} else {
				consumedByID[ev.PreviewTokenID] = next
				selection.ConsumedTokens = append(selection.ConsumedTokens, next)
			}
		}
		if ev.Type == orderJournalEventReconciledUnknown || ev.Type == orderJournalEventReconciledAbsent {
			selection.ReconciliationEvents++
		}
	}
	if err := scanner.Err(); err != nil {
		return legacyOrderImportSelection{}, fmt.Errorf("scan legacy order journal: %w", err)
	}
	selection.SourceFingerprint = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	selection.SourceEvents = append([]orderJournalEvent(nil), events...)

	// Partition before aliasing so an omitted client_id cannot collide with
	// an explicitly journaled client_id=0. Legacy route fields are evidence;
	// later rows and the currently configured daemon scope may not fill them.
	partitions := make(map[string][]orderJournalEvent)
	for _, ev := range events {
		partition := legacyOrderRoutePartitionKey(ev)
		partitions[partition] = append(partitions[partition], ev)
	}
	aliasesByPartition := make(map[string]map[string]string, len(partitions))
	chains := make(map[string][]orderJournalEvent)
	for partition, partitionEvents := range partitions {
		aliases := orderJournalKeyAliases(partitionEvents)
		aliasesByPartition[partition] = aliases
		for _, ev := range partitionEvents {
			if key := orderJournalCanonicalKey(ev, aliases); key != "" {
				chainKey := partition + "\x1e" + key
				chains[chainKey] = append(chains[chainKey], ev)
			}
		}
	}
	keep := make(map[string]bool, len(chains))
	for key, chain := range chains {
		if !legacyOrderChainBrokerProvenTerminal(chain) {
			keep[key] = true
		}
	}
	for _, ev := range events {
		partition := legacyOrderRoutePartitionKey(ev)
		key := orderJournalCanonicalKey(ev, aliasesByPartition[partition])
		if key != "" {
			key = partition + "\x1e" + key
		}
		if key == "" || !keep[key] {
			continue
		}
		if !legacyOrderRouteFromEvent(ev).complete() {
			return legacyOrderImportSelection{}, fmt.Errorf("retained legacy order event %q lacks complete endpoint/client/account/mode identity", orderJournalEventLabel(ev))
		}
		selection.Events = append(selection.Events, ev)
	}
	return selection, nil
}

// importLegacyOrderAuthority is the cutover's single fail-closed order call.
// It selects through the canonical decoder/fold, commits chains/tombstones/
// floors in one corestore transaction, then verifies event and floor parity.
func importLegacyOrderAuthority(ctx context.Context, store *corestore.Store, path string) (legacyOrderImportParity, error) {
	selection, err := loadLegacyOrderImportSelection(path)
	if err != nil {
		return legacyOrderImportParity{}, err
	}
	return importSelectedLegacyOrderAuthority(ctx, store, selection)
}

// importLegacyTradingAuthority is startup's first-cutover entrypoint. Retired
// purge-ledger files are sealed into the cutover backup but are no longer live
// authority; their order events remain preserved through the order journal.
func importLegacyTradingAuthority(ctx context.Context, store *corestore.Store, orderPath string) (legacyTradingAuthorityParity, error) {
	var parity legacyTradingAuthorityParity
	selection, err := loadLegacyOrderImportSelection(orderPath)
	if err != nil {
		return parity, err
	}
	parity.Orders, err = importSelectedLegacyOrderAuthority(ctx, store, selection)
	if err != nil {
		return parity, err
	}
	return parity, nil
}

func importSelectedLegacyOrderAuthority(ctx context.Context, store *corestore.Store, selection legacyOrderImportSelection) (legacyOrderImportParity, error) {
	var parity legacyOrderImportParity
	if store == nil {
		return parity, fmt.Errorf("order authority is unavailable")
	}
	if strings.TrimSpace(selection.SourceFingerprint) == "" {
		return parity, fmt.Errorf("legacy order source fingerprint is missing")
	}
	input := corestore.LegacyOrderImport{
		SourceFingerprint: selection.SourceFingerprint,
		GlobalFloor:       int64(selection.GlobalOrderIDFloor),
	}
	expectedPayloads := make(map[[sha256.Size]byte]int)
	chains := map[string]bool{}
	selectedAliases := orderJournalKeyAliases(selection.Events)
	for _, ev := range selection.Events {
		if ev.At.IsZero() {
			return parity, fmt.Errorf("retained legacy order event %q has no event time", orderJournalEventLabel(ev))
		}
		action := coreOrderActionForEvent(ev)
		if action == "" && ev.Type == orderJournalEventSendError && ev.AttemptID == "" && ev.SendDisposition == "" {
			// Historical JSONL only emitted generic send-error after place. This is
			// a one-time import compatibility rule, not runtime action inference.
			action = corestore.ActionPlace
		}
		record, normalized, err := coreOrderEventRecord(ev, action, "")
		if err != nil {
			return parity, fmt.Errorf("prepare retained legacy order event: %w", err)
		}
		input.Events = append(input.Events, record)
		expectedPayloads[sha256.Sum256(record.RawJSON)]++
		chains[orderJournalCanonicalKey(normalized, selectedAliases)] = true
	}
	for _, token := range selection.ConsumedTokens {
		scope, err := coreBrokerScopeFromLegacyRoute(token.Route)
		if err != nil {
			return parity, err
		}
		if token.At.IsZero() {
			return parity, fmt.Errorf("legacy preview token %q has no consumption time", token.TokenID)
		}
		input.ConsumedTokens = append(input.ConsumedTokens, corestore.LegacyConsumedToken{
			Scope: scope, PreviewTokenID: token.TokenID, ConsumedAt: token.At,
		})
	}
	for _, floor := range selection.ScopedOrderIDFloors {
		scope, err := coreBrokerScopeFromLegacyRoute(floor.Route)
		if err != nil {
			return parity, err
		}
		input.ScopedFloors = append(input.ScopedFloors, corestore.LegacyOrderFloor{Scope: scope, Floor: int64(floor.Floor)})
	}
	if _, err := store.ImportLegacyOrderAuthority(ctx, input); err != nil {
		return parity, fmt.Errorf("import legacy order authority: %w", err)
	}

	global, err := store.GlobalOrderIDFloor(ctx)
	if err != nil {
		return parity, fmt.Errorf("verify imported global order id floor: %w", err)
	}
	if global < int64(selection.GlobalOrderIDFloor) {
		return parity, fmt.Errorf("imported global order id floor %d is below legacy floor %d", global, selection.GlobalOrderIDFloor)
	}
	for _, floor := range input.ScopedFloors {
		got, err := store.ScopedOrderIDFloor(ctx, floor.Scope.ScopeKey)
		if err != nil {
			return parity, fmt.Errorf("verify imported scoped order id floor: %w", err)
		}
		if got < floor.Floor || got < global {
			return parity, fmt.Errorf("imported scoped order id floor %d is below required floor", got)
		}
	}
	if err := verifyLegacyOrderPayloads(ctx, store, expectedPayloads); err != nil {
		return parity, err
	}

	parity = legacyOrderImportParity{
		SourceFingerprint: selection.SourceFingerprint, RetainedEventCount: len(selection.Events), RetainedChainCount: len(chains),
		ConsumedTokenCount: len(selection.ConsumedTokens), ConsumedTokenRoutes: map[string]legacyOrderRoute{},
		GlobalOrderIDFloor: selection.GlobalOrderIDFloor, ScopedOrderIDFloors: map[string]int{},
		ReconciliationEvents: selection.ReconciliationEvents,
	}
	for _, token := range selection.ConsumedTokens {
		parity.ConsumedTokenRoutes[token.TokenID] = token.Route
	}
	for key, floor := range selection.ScopedOrderIDFloors {
		parity.ScopedOrderIDFloors[key] = floor.Floor
	}
	return parity, nil
}

func coreBrokerScopeFromLegacyRoute(route legacyOrderRoute) (corestore.BrokerScope, error) {
	return coreBrokerScopeFromEvent(orderJournalEvent{
		Endpoint: route.Endpoint, ClientID: route.ClientID, Account: route.Account, Mode: route.Mode,
	})
}

func verifyLegacyOrderPayloads(ctx context.Context, store *corestore.Store, expected map[[sha256.Size]byte]int) error {
	if len(expected) == 0 {
		return nil
	}
	actual := make(map[[sha256.Size]byte]int)
	var after int64
	for {
		page, err := store.LoadOrderEvents(ctx, corestore.OrderQuery{AfterEventSeq: after, Limit: 10_000})
		if err != nil {
			return fmt.Errorf("verify imported order events: %w", err)
		}
		for _, record := range page {
			actual[sha256.Sum256(record.RawJSON)]++
		}
		if len(page) < 10_000 {
			break
		}
		after = page[len(page)-1].EventSeq
	}
	for digest, count := range expected {
		if actual[digest] < count {
			return fmt.Errorf("legacy order event payload parity failed")
		}
	}
	return nil
}

func legacyOrderRouteFromEvent(ev orderJournalEvent) legacyOrderRoute {
	return legacyOrderRoute{
		Endpoint:      strings.TrimSpace(ev.Endpoint),
		ClientID:      ev.ClientID,
		ClientIDKnown: ev.clientIDPresent,
		Account:       strings.ToUpper(strings.TrimSpace(ev.Account)),
		Mode:          strings.ToLower(strings.TrimSpace(ev.Mode)),
	}
}

func (r legacyOrderRoute) complete() bool {
	return r.Endpoint != "" && r.ClientIDKnown && r.ClientID >= 0 && r.Account != "" && (r.Mode == "paper" || r.Mode == "live")
}

func (r legacyOrderRoute) key() string {
	return orderJournalRouteIdentity(r.Endpoint, r.ClientID, r.Account, r.Mode)
}

func legacyOrderRoutePartitionKey(ev orderJournalEvent) string {
	route := legacyOrderRouteFromEvent(ev)
	client := "missing"
	if route.ClientIDKnown {
		client = "known:" + strconv.Itoa(route.ClientID)
	}
	return route.Endpoint + "\x1f" + client + "\x1f" + route.Account + "\x1f" + route.Mode
}

// Only an explicit terminal broker callback or an allowlisted typed broker
// error permits omission. Reconciliation remains retained: an absent-open-order
// proof closes working authority but does not establish the final fill/cancel
// outcome needed for a complete audit.
func legacyOrderChainBrokerProvenTerminal(chain []orderJournalEvent) bool {
	if len(chain) == 0 {
		return false
	}
	last := chain[len(chain)-1]
	if last.Type == orderJournalEventReconciledUnknown || last.Type == orderJournalEventReconciledAbsent {
		return false
	}
	if last.Type == orderJournalEventBrokerError {
		// ErrorCode was not persisted by older binaries. A code-less legacy
		// broker error is therefore uncertain even when its Status or Message
		// looks terminal; retaining it is the only fail-closed migration.
		if last.ErrorCode == 0 {
			return false
		}
		return orderLifecycleStatusIsTerminal(mapBrokerErrorLifecycleStatus(last.ErrorCode, last.Status))
	}
	if last.Type != orderJournalEventStatusUpdated && last.Type != orderJournalEventBrokerAcknowledged {
		return false
	}
	return orderLifecycleStatusIsTerminal(mapOrderJournalLifecycleStatus(last))
}

func (s *orderJournalStore) Summary() (orderJournalSummary, error) {
	events, err := s.LoadEvents(0)
	if err != nil {
		return orderJournalSummary{}, err
	}
	var last orderJournalEvent
	for _, ev := range events {
		last = ev
	}
	var summary orderJournalSummary
	for _, view := range buildOrderViews(events) {
		if view.Open {
			summary.OpenOrders++
		}
	}
	if !last.At.IsZero() {
		summary.LastEvent = fmt.Sprintf("%s %s at %s", last.Type, orderJournalEventLabel(last), last.At.Format(time.RFC3339))
	}
	return summary, nil
}

func validateOrderJournalEvent(ev orderJournalEvent) error {
	if ev.Type == "" {
		return fmt.Errorf("order journal event type is required")
	}
	if ev.Version != orderJournalFileVersion {
		return fmt.Errorf("unsupported order journal version %d", ev.Version)
	}
	if len(ev.AttemptID) > 128 || strings.TrimSpace(ev.AttemptID) != ev.AttemptID {
		return fmt.Errorf("order journal attempt id is invalid")
	}
	if ev.ActionKind != "" && !validOrderActionKind(ev.ActionKind) {
		return fmt.Errorf("order journal action kind %q is invalid", ev.ActionKind)
	}
	if ev.TransmitOrigin != "" && !validOrderTransmitOrigin(ev.TransmitOrigin) {
		return fmt.Errorf("order journal transmit origin %q is invalid", ev.TransmitOrigin)
	}
	if ev.AttemptID != "" && ev.ActionKind == "" {
		return fmt.Errorf("order journal attempt %q requires an action kind", ev.AttemptID)
	}
	if ev.SendDisposition != "" {
		if ev.Type != orderJournalEventSendError || ev.AttemptID == "" || !validOrderSendDisposition(ev.SendDisposition) {
			return fmt.Errorf("order journal send disposition is invalid")
		}
	}
	if (ev.Type == orderJournalEventSendCompleted || ev.Type == orderJournalEventSendError) && ev.AttemptID != "" && ev.ActionKind == "" {
		return fmt.Errorf("typed order send outcome requires an action kind")
	}
	return nil
}

func validOrderActionKind(action corestore.ActionKind) bool {
	switch action {
	case corestore.ActionPlace, corestore.ActionModify, corestore.ActionCancel,
		corestore.ActionPurge, corestore.ActionRestore, corestore.ActionExercise,
		corestore.ActionSmokeCleanup:
		return true
	default:
		return false
	}
}

func validOrderTransmitOrigin(origin corestore.TransmitOrigin) bool {
	switch origin {
	case corestore.OriginAgentCLI, corestore.OriginHumanCLI, corestore.OriginDaemon:
		return true
	default:
		return false
	}
}

func validOrderSendDisposition(disposition ibkrlib.SendDisposition) bool {
	switch disposition {
	case ibkrlib.SendDispositionDefinitelyUnsent,
		ibkrlib.SendDispositionMayHaveWritten,
		ibkrlib.SendDispositionUnknown:
		return true
	default:
		return false
	}
}

func orderJournalEventConsumesPreviewToken(ev orderJournalEvent) bool {
	switch ev.Type {
	case orderJournalEventTokenConfirmed,
		orderJournalEventSendAttempted,
		orderJournalEventSendCompleted,
		orderJournalEventSendError,
		orderJournalEventBrokerAcknowledged,
		orderJournalEventModifyRequested,
		orderJournalEventCancelRequested:
		return true
	default:
		return false
	}
}

func orderJournalEventLabel(ev orderJournalEvent) string {
	if ev.OrderRef != "" {
		return ev.OrderRef
	}
	if ev.ReservedOrderID != 0 {
		return "order:" + strconv.Itoa(ev.ReservedOrderID)
	}
	if ev.PermID != 0 {
		return "perm:" + strconv.Itoa(ev.PermID)
	}
	return "unknown-order"
}

func maxReservedBrokerOrderID(store *orderJournalStore) (int, error) {
	if store == nil {
		return 0, nil
	}
	events, err := store.LoadEvents(0)
	if err != nil {
		return 0, err
	}
	var maxID int
	for _, ev := range events {
		if ev.ReservedOrderID > maxID {
			maxID = ev.ReservedOrderID
		}
	}
	return maxID, nil
}
