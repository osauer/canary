package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	proposalStateKind        = "trade_proposals_current"
	proposalCoreEventType    = "trade_proposal_event"
	opportunityStateKind     = "opportunities_current"
	opportunityCoreEventType = "opportunity_event"
)

type proposalStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	events   []proposalEvent
}

type opportunityStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	events   []opportunityEvent
}

// initializeCleanProposalOpportunityAuthority establishes the new semantic
// epoch for proposals and opportunities in an unpublished daemon.db. These
// derived surfaces intentionally do not import their legacy JSON snapshots or
// JSONL histories: the first authoritative generation starts empty.
//
// The caller publishes the database only after this initializer and the
// database integrity/backup checks succeed. Existing rows are accepted only
// when they are the same clean empty epoch, which makes a retried cutover safe
// without permitting a partial or already-live database to masquerade as new.
func initializeCleanProposalOpportunityAuthority(ctx context.Context, core *corestore.Store) error {
	if core == nil {
		return fmt.Errorf("proposal/opportunity SQLite authority is unavailable")
	}
	proposalDoc, proposalExists, err := core.GetStateDocument(ctx, daemonStateScope, proposalStateKind)
	if err != nil {
		return fmt.Errorf("inspect clean proposal authority: %w", err)
	}
	opportunityDoc, opportunityExists, err := core.GetStateDocument(ctx, daemonStateScope, opportunityStateKind)
	if err != nil {
		return fmt.Errorf("inspect clean opportunity authority: %w", err)
	}
	if err := validateCleanProposalDocument(proposalDoc, proposalExists); err != nil {
		return err
	}
	if err := validateCleanOpportunityDocument(opportunityDoc, opportunityExists); err != nil {
		return err
	}
	if events, err := loadAllCoreEvents(ctx, core, proposalCoreEventType); err != nil {
		return fmt.Errorf("inspect clean proposal events: %w", err)
	} else if len(events) != 0 {
		return fmt.Errorf("clean proposal authority already has %d events", len(events))
	}
	if events, err := loadAllCoreEvents(ctx, core, opportunityCoreEventType); err != nil {
		return fmt.Errorf("inspect clean opportunity events: %w", err)
	} else if len(events) != 0 {
		return fmt.Errorf("clean opportunity authority already has %d events", len(events))
	}
	now := time.Now().UTC()
	if !proposalExists {
		if err := writeCleanAuthorityDocument(ctx, core, proposalStateKind, emptyProposalSnapshot(now)); err != nil {
			return err
		}
	}
	if !opportunityExists {
		if err := writeCleanAuthorityDocument(ctx, core, opportunityStateKind, emptyOpportunitySnapshot(now)); err != nil {
			return err
		}
	}
	return nil
}

func validateCleanProposalDocument(doc corestore.StateDocument, exists bool) error {
	if !exists {
		return nil
	}
	var snap rpc.TradeProposalSnapshot
	if err := json.Unmarshal(doc.JSON, &snap); err != nil {
		return fmt.Errorf("decode clean proposal state: %w", err)
	}
	if !proposalSnapshotClean(snap) {
		return errors.New("clean proposal authority contains a non-empty or malformed current snapshot")
	}
	return nil
}

func validateCleanOpportunityDocument(doc corestore.StateDocument, exists bool) error {
	if !exists {
		return nil
	}
	var snap rpc.OpportunitySnapshot
	if err := json.Unmarshal(doc.JSON, &snap); err != nil {
		return fmt.Errorf("decode clean opportunity state: %w", err)
	}
	if !opportunitySnapshotClean(snap) {
		return errors.New("clean opportunity authority contains a non-empty or malformed current snapshot")
	}
	return nil
}

func writeCleanAuthorityDocument(ctx context.Context, core *corestore.Store, kind string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope,
		Kind:     kind,
		JSON:     raw,
	}); err != nil {
		return fmt.Errorf("initialize clean %s: %w", kind, err)
	}
	return nil
}

// attachProposalOpportunityAuthority is the explicit post-publication
// attachment. Server.Start calls it after daemon.db has passed publication and
// opened, but before RPC or broker activity. It never consults legacy files.
func (s *Server) attachProposalOpportunityAuthority(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return fmt.Errorf("proposal/opportunity SQLite authority is unavailable")
	}
	if s.tradeProposals == nil {
		s.installProposalEngine()
	}
	if s.opportunities == nil {
		s.installOpportunityEngine()
	}
	if err := s.tradeProposals.bindCore(ctx, core); err != nil {
		return fmt.Errorf("attach proposal authority: %w", err)
	}
	if err := s.opportunities.bindCore(ctx, core); err != nil {
		return fmt.Errorf("attach opportunity authority: %w", err)
	}
	return nil
}

func (e *proposalEngine) bindCore(ctx context.Context, core *corestore.Store) error {
	snap, events, err := e.store.bindCore(ctx, core)
	if err != nil {
		return err
	}
	ignored := map[string]struct{}{}
	for _, ev := range events {
		if ev.Type == "ignored" && ev.Key != "" {
			scope := brokerStateScope{Account: ev.AccountID, Mode: ev.AccountMode}
			if brokerScopeConcrete(scope) {
				ignored[scopedIgnoreKey(scope, ev.Key)] = struct{}{}
			}
		}
	}
	e.mu.Lock()
	if snap.Kind != "" {
		snap.LoadedFromState = true
		e.snapshot = cloneProposalSnapshot(snap)
	}
	e.ignored = ignored
	e.mu.Unlock()
	return nil
}

func (e *opportunityEngine) bindCore(ctx context.Context, core *corestore.Store) error {
	snap, events, err := e.store.bindCore(ctx, core)
	if err != nil {
		return err
	}
	ignored := map[string]struct{}{}
	for _, ev := range events {
		if ev.Type == "ignored" && ev.Key != "" {
			scope := brokerStateScope{Account: ev.AccountID, Mode: ev.AccountMode}
			if brokerScopeConcrete(scope) {
				ignored[opportunityIgnoreKey(scope, ev.Key)] = struct{}{}
			}
		}
	}
	e.mu.Lock()
	if snap.Kind != "" {
		snap.LoadedFromState = true
		e.snapshot = cloneOpportunitySnapshot(snap)
	}
	e.ignored = ignored
	e.mu.Unlock()
	return nil
}

func (s *proposalStore) bindCore(ctx context.Context, core *corestore.Store) (rpc.TradeProposalSnapshot, []proposalEvent, error) {
	if s == nil || core == nil {
		return rpc.TradeProposalSnapshot{}, nil, errors.New("proposal store is not attached")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, proposalStateKind)
	if err != nil {
		return rpc.TradeProposalSnapshot{}, nil, err
	}
	if !ok {
		return rpc.TradeProposalSnapshot{}, nil, errors.New("proposal state row is missing")
	}
	var snap rpc.TradeProposalSnapshot
	if err := json.Unmarshal(doc.JSON, &snap); err != nil {
		return snap, nil, fmt.Errorf("decode proposal state: %w", err)
	}
	if !proposalAuthoritySnapshotValid(snap) {
		return snap, nil, errors.New("proposal state row is malformed")
	}
	events, err := loadProposalEvents(ctx, core)
	if err != nil {
		return snap, nil, err
	}
	s.mu.Lock()
	s.core, s.events = core, append([]proposalEvent(nil), events...)
	s.revision = doc.Revision
	s.mu.Unlock()
	return snap, events, nil
}

func (s *opportunityStore) bindCore(ctx context.Context, core *corestore.Store) (rpc.OpportunitySnapshot, []opportunityEvent, error) {
	if s == nil || core == nil {
		return rpc.OpportunitySnapshot{}, nil, errors.New("opportunity store is not attached")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, opportunityStateKind)
	if err != nil {
		return rpc.OpportunitySnapshot{}, nil, err
	}
	if !ok {
		return rpc.OpportunitySnapshot{}, nil, errors.New("opportunity state row is missing")
	}
	var snap rpc.OpportunitySnapshot
	if err := json.Unmarshal(doc.JSON, &snap); err != nil {
		return snap, nil, fmt.Errorf("decode opportunity state: %w", err)
	}
	if !opportunityAuthoritySnapshotValid(snap) {
		return snap, nil, errors.New("opportunity state row is malformed")
	}
	events, err := loadOpportunityEvents(ctx, core)
	if err != nil {
		return snap, nil, err
	}
	s.mu.Lock()
	s.core, s.events = core, append([]opportunityEvent(nil), events...)
	s.revision = doc.Revision
	s.mu.Unlock()
	return snap, events, nil
}

func proposalSnapshotClean(snap rpc.TradeProposalSnapshot) bool {
	return snap.Kind == rpc.TradeProposalSnapshotKind &&
		snap.SchemaVersion == rpc.TradeProposalSnapshotSchemaVersion &&
		!snap.LoadedFromState && !snap.AsOf.IsZero() && snap.Revision == "empty" &&
		strings.TrimSpace(snap.AccountID) == "" && strings.TrimSpace(snap.AccountMode) == "" &&
		len(snap.Proposals) == 0
}

func opportunitySnapshotClean(snap rpc.OpportunitySnapshot) bool {
	return snap.Kind == rpc.OpportunitySnapshotKind &&
		snap.SchemaVersion == rpc.OpportunitySnapshotSchemaVersion &&
		!snap.LoadedFromState && !snap.AsOf.IsZero() && snap.Revision == "empty" &&
		strings.TrimSpace(snap.AccountID) == "" && strings.TrimSpace(snap.AccountMode) == "" &&
		len(snap.Opportunities) == 0
}

func proposalAuthoritySnapshotValid(snap rpc.TradeProposalSnapshot) bool {
	if snap.LoadedFromState {
		return false
	}
	if proposalSnapshotClean(snap) {
		return true
	}
	if snap.Kind != rpc.TradeProposalSnapshotKind ||
		snap.SchemaVersion != rpc.TradeProposalSnapshotSchemaVersion ||
		snap.AsOf.IsZero() || !proposalSnapshotPersistable(snap) {
		return false
	}
	for _, proposal := range snap.Proposals {
		if strings.TrimSpace(proposal.Key) == "" || proposal.Revision != snap.Revision {
			return false
		}
	}
	return true
}

func opportunityAuthoritySnapshotValid(snap rpc.OpportunitySnapshot) bool {
	if snap.LoadedFromState {
		return false
	}
	if opportunitySnapshotClean(snap) {
		return true
	}
	if snap.Kind != rpc.OpportunitySnapshotKind ||
		snap.SchemaVersion != rpc.OpportunitySnapshotSchemaVersion ||
		snap.AsOf.IsZero() || !opportunitySnapshotPersistable(snap) {
		return false
	}
	for _, opportunity := range snap.Opportunities {
		if strings.TrimSpace(opportunity.Key) == "" || opportunity.Revision != snap.Revision {
			return false
		}
	}
	return true
}

func (s *proposalStore) SaveCurrentWithEvents(ctx context.Context, snap rpc.TradeProposalSnapshot, events []proposalEvent) error {
	// LoadedFromState describes this process's adapter cache, not the durable
	// proposal document. It is restored only after a successful attach.
	snap.LoadedFromState = false
	if !proposalAuthoritySnapshotValid(snap) {
		return errors.New("proposal state is malformed")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("proposal store is not attached")
	}
	events = append([]proposalEvent(nil), events...)
	for i := range events {
		if events[i].AccountID == "" {
			events[i].AccountID = snap.AccountID
		}
		if events[i].AccountMode == "" {
			events[i].AccountMode = snap.AccountMode
		}
	}
	inputs, normalized, err := proposalEventInputs(ctx, s.core, events)
	if err != nil {
		return err
	}
	update := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: proposalStateKind, ExpectedRevision: s.revision, JSON: raw}
	var saved corestore.StateDocument
	if len(inputs) == 0 {
		saved, err = s.core.CompareAndSwapStateDocument(ctx, update)
	} else {
		saved, _, err = s.core.CompareAndSwapStateDocumentWithEvents(ctx, update, inputs)
	}
	if err != nil {
		return err
	}
	s.revision = saved.Revision
	s.events = append(s.events, normalized...)
	return nil
}

func (s *opportunityStore) SaveCurrent(snap rpc.OpportunitySnapshot) error {
	return s.SaveCurrentWithEvents(context.Background(), snap, nil)
}

func (s *opportunityStore) SaveCurrentWithEvents(ctx context.Context, snap rpc.OpportunitySnapshot, events []opportunityEvent) error {
	snap.LoadedFromState = false
	if !opportunityAuthoritySnapshotValid(snap) {
		return errors.New("opportunity state is malformed")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("opportunity store is not attached")
	}
	events = append([]opportunityEvent(nil), events...)
	for i := range events {
		if events[i].AccountID == "" {
			events[i].AccountID = snap.AccountID
		}
		if events[i].AccountMode == "" {
			events[i].AccountMode = snap.AccountMode
		}
	}
	inputs, normalized, err := opportunityEventInputs(ctx, s.core, events)
	if err != nil {
		return err
	}
	update := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: opportunityStateKind, ExpectedRevision: s.revision, JSON: raw}
	var saved corestore.StateDocument
	if len(inputs) == 0 {
		saved, err = s.core.CompareAndSwapStateDocument(ctx, update)
	} else {
		saved, _, err = s.core.CompareAndSwapStateDocumentWithEvents(ctx, update, inputs)
	}
	if err != nil {
		return err
	}
	s.revision = saved.Revision
	s.events = append(s.events, normalized...)
	return nil
}

func (s *proposalStore) LoadCurrent() (rpc.TradeProposalSnapshot, error) {
	if s == nil {
		return rpc.TradeProposalSnapshot{}, errors.New("proposal store is not attached")
	}
	s.mu.Lock()
	core := s.core
	s.mu.Unlock()
	if core == nil {
		return rpc.TradeProposalSnapshot{}, errors.New("proposal store is not attached")
	}
	doc, ok, err := core.GetStateDocument(context.Background(), daemonStateScope, proposalStateKind)
	if err != nil {
		return rpc.TradeProposalSnapshot{}, err
	}
	if !ok {
		return rpc.TradeProposalSnapshot{}, errors.New("proposal state row is missing")
	}
	var snap rpc.TradeProposalSnapshot
	if err := json.Unmarshal(doc.JSON, &snap); err != nil {
		return snap, err
	}
	if !proposalAuthoritySnapshotValid(snap) {
		return snap, errors.New("proposal state row is malformed")
	}
	return snap, nil
}

func (s *proposalStore) AppendEvent(ev proposalEvent) error {
	return s.appendEvent(context.Background(), ev)
}
func (s *opportunityStore) AppendEvent(ev opportunityEvent) error {
	return s.appendEvent(context.Background(), ev)
}

func (s *proposalStore) appendEvent(ctx context.Context, ev proposalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("proposal store is not attached")
	}
	inputs, normalized, err := proposalEventInputs(ctx, s.core, []proposalEvent{ev})
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return nil
	}
	if _, err := s.core.AppendEvents(ctx, inputs); err != nil {
		return err
	}
	s.events = append(s.events, normalized[0])
	return nil
}

func (s *opportunityStore) appendEvent(ctx context.Context, ev opportunityEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("opportunity store is not attached")
	}
	inputs, normalized, err := opportunityEventInputs(ctx, s.core, []opportunityEvent{ev})
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return nil
	}
	if _, err := s.core.AppendEvents(ctx, inputs); err != nil {
		return err
	}
	s.events = append(s.events, normalized[0])
	return nil
}

func (s *proposalStore) FindSubmittedEvent(orderRef, tokenID string) (proposalEvent, bool, error) {
	if s == nil {
		return proposalEvent{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return proposalEvent{}, false, errors.New("proposal store is not attached")
	}
	orderRef, tokenID = strings.TrimSpace(orderRef), strings.TrimSpace(tokenID)
	if orderRef == "" && tokenID == "" {
		return proposalEvent{}, false, nil
	}
	for _, ev := range slices.Backward(s.events) {

		if ev.Type == "submitted" && (orderRef != "" && ev.OrderRef == orderRef || tokenID != "" && ev.PreviewTokenID == tokenID) {
			return ev, true, nil
		}
	}
	return proposalEvent{}, false, nil
}

func proposalEventInputs(ctx context.Context, core *corestore.Store, events []proposalEvent) ([]corestore.EventInput, []proposalEvent, error) {
	inputs := make([]corestore.EventInput, 0, len(events))
	normalized := make([]proposalEvent, 0, len(events))
	for i, ev := range events {
		if ev.At.IsZero() {
			ev.At = time.Now().UTC()
		}
		if ev.Version == 0 {
			ev.Version = proposalEventFileVersion
		}
		if !proposalEventValid(ev) {
			return nil, nil, errors.New("proposal event is malformed")
		}
		if !operationalProposalEvent(ev.Type) {
			continue
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, err
		}
		key, err := coreStoreEventKey(ctx, core, "proposal", ev.At, raw, i)
		if err != nil {
			return nil, nil, err
		}
		inputs = append(inputs, corestore.EventInput{ScopeKey: daemonStateScope, EventKey: key, Type: proposalCoreEventType, Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: ev.At, PayloadJSON: raw})
		normalized = append(normalized, ev)
	}
	return inputs, normalized, nil
}

func opportunityEventInputs(ctx context.Context, core *corestore.Store, events []opportunityEvent) ([]corestore.EventInput, []opportunityEvent, error) {
	inputs := make([]corestore.EventInput, 0, len(events))
	normalized := make([]opportunityEvent, 0, len(events))
	for i, ev := range events {
		if ev.At.IsZero() {
			ev.At = time.Now().UTC()
		}
		if ev.Version == 0 {
			ev.Version = 1
		}
		if !opportunityEventValid(ev) {
			return nil, nil, errors.New("opportunity event is malformed")
		}
		if !operationalOpportunityEvent(ev.Type) {
			continue
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, err
		}
		key, err := coreStoreEventKey(ctx, core, "opportunity", ev.At, raw, i)
		if err != nil {
			return nil, nil, err
		}
		inputs = append(inputs, corestore.EventInput{ScopeKey: daemonStateScope, EventKey: key, Type: opportunityCoreEventType, Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: ev.At, PayloadJSON: raw})
		normalized = append(normalized, ev)
	}
	return inputs, normalized, nil
}

func operationalProposalEvent(eventType string) bool {
	return eventType == "ignored" || eventType == "submitted"
}

func operationalOpportunityEvent(eventType string) bool {
	return eventType == "ignored" || eventType == "submitted"
}

func loadProposalEvents(ctx context.Context, core *corestore.Store) ([]proposalEvent, error) {
	records, err := loadAllCoreEvents(ctx, core, proposalCoreEventType)
	if err != nil {
		return nil, err
	}
	out := make([]proposalEvent, 0, len(records))
	for _, record := range records {
		var ev proposalEvent
		if err := json.Unmarshal(record.PayloadJSON, &ev); err != nil {
			return nil, fmt.Errorf("decode proposal event: %w", err)
		}
		if !proposalEventValid(ev) || !ev.At.Equal(record.OccurredAt) {
			return nil, errors.New("proposal event row is malformed")
		}
		out = append(out, ev)
	}
	return out, nil
}
func loadOpportunityEvents(ctx context.Context, core *corestore.Store) ([]opportunityEvent, error) {
	records, err := loadAllCoreEvents(ctx, core, opportunityCoreEventType)
	if err != nil {
		return nil, err
	}
	out := make([]opportunityEvent, 0, len(records))
	for _, record := range records {
		var ev opportunityEvent
		if err := json.Unmarshal(record.PayloadJSON, &ev); err != nil {
			return nil, fmt.Errorf("decode opportunity event: %w", err)
		}
		if !opportunityEventValid(ev) || !ev.At.Equal(record.OccurredAt) {
			return nil, errors.New("opportunity event row is malformed")
		}
		out = append(out, ev)
	}
	return out, nil
}

func proposalEventValid(ev proposalEvent) bool {
	if ev.Version != proposalEventFileVersion || ev.At.IsZero() || strings.TrimSpace(ev.Type) == "" {
		return false
	}
	if ev.Type == "ignored" {
		return strings.TrimSpace(ev.Key) != "" && brokerScopeConcrete(brokerStateScope{Account: ev.AccountID, Mode: ev.AccountMode})
	}
	return true
}

func opportunityEventValid(ev opportunityEvent) bool {
	if ev.Version != 1 || ev.At.IsZero() || strings.TrimSpace(ev.Type) == "" {
		return false
	}
	if ev.Type == "ignored" {
		return strings.TrimSpace(ev.Key) != "" && brokerScopeConcrete(brokerStateScope{Account: ev.AccountID, Mode: ev.AccountMode})
	}
	return true
}
