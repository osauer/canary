package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Pre-authorised protection submission (owner decision D3, 2026-09-21; see
// internal-docs/design/pre-authorised-protection.md).
//
// The daemon places the protective orders it already proposes, for the
// buckets the owner lists under [authority].pre_authorised: one durable
// record per proposal key and revision carries the notice, the veto window
// and the outcome. A record is created only for an unblocked proposal, is
// superseded the moment that proposal blocks, disappears or changes revision,
// submits exactly once, and never retries on its own. The broker write goes
// through the ordinary proposal submit path with the daemon-preauthorised
// origin, so every existing gate still decides.

const (
	automaticStateKind       = "trade_proposals_automatic"
	automaticCoreEventType   = "trade_proposal_automatic_event"
	automaticDocumentVersion = 1

	automaticEventCreated    = "created"
	automaticEventNoticed    = "noticed"
	automaticEventVetoed     = "vetoed"
	automaticEventSubmitting = "submitting"
	automaticEventSubmitted  = "submitted"
	automaticEventSuperseded = "superseded"
	automaticEventFailed     = "failed"

	// automaticSubmitTimeout bounds the quote/WhatIf wait of one automatic
	// submission, the same default the CLI uses.
	automaticSubmitTimeout = 5 * time.Second
)

// automaticSubmissionRecord is one durable automatic-submission intent, keyed
// by proposal key and revision. Timestamps are UTC.
type automaticSubmissionRecord struct {
	Version      int    `json:"version"`
	Key          string `json:"key"`
	Revision     string `json:"revision"`
	Bucket       string `json:"bucket"`
	EngineBucket string `json:"engine_bucket,omitempty"`
	Symbol       string `json:"symbol,omitempty"`
	SecType      string `json:"sec_type,omitempty"`
	Action       string `json:"action,omitempty"`
	Quantity     int    `json:"quantity,omitempty"`
	AccountID    string `json:"account_id"`
	AccountMode  string `json:"account_mode"`
	State        string `json:"state"`
	// VetoWindow is the window in force when the record was created.
	VetoWindow         string    `json:"veto_window,omitempty"`
	LatchSkippedWindow bool      `json:"latch_skipped_window,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	SubmitAt           time.Time `json:"submit_at"`
	NoticedAt          time.Time `json:"noticed_at,omitzero"`
	VetoedAt           time.Time `json:"vetoed_at,omitzero"`
	SubmittingAt       time.Time `json:"submitting_at,omitzero"`
	SubmittedAt        time.Time `json:"submitted_at,omitzero"`
	ResolvedAt         time.Time `json:"resolved_at,omitzero"`
	// PreviewTokenID is persisted before the broker call so a restart can
	// find the journaled attempt for this exact record.
	PreviewTokenID string `json:"preview_token_id,omitempty"`
	OrderRef       string `json:"order_ref,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// Origin records who vetoed (human-tty or human-paired-device).
	Origin string `json:"origin,omitempty"`
}

func (r automaticSubmissionRecord) pending() bool {
	return r.State == rpc.TradeProposalAutomaticPending
}

func (r automaticSubmissionRecord) terminal() bool {
	switch r.State {
	case rpc.TradeProposalAutomaticVetoed, rpc.TradeProposalAutomaticSubmitted,
		rpc.TradeProposalAutomaticSuperseded, rpc.TradeProposalAutomaticFailed:
		return true
	}
	return false
}

func (r automaticSubmissionRecord) view(preAuthorised bool, now time.Time) *rpc.TradeProposalAutomatic {
	out := &rpc.TradeProposalAutomatic{
		PreAuthorised: preAuthorised, Bucket: r.Bucket, State: r.State,
		CreatedAt: r.CreatedAt, SubmitAt: r.SubmitAt, NoticedAt: r.NoticedAt, VetoedAt: r.VetoedAt,
		SubmittedAt: r.SubmittedAt, OrderReference: r.OrderRef, Reason: r.Reason,
		LatchSkippedWindow: r.LatchSkippedWindow, VetoWindow: r.VetoWindow,
	}
	if r.pending() && r.NoticedAt.IsZero() && !r.LatchSkippedWindow && !now.Before(r.SubmitAt) {
		out.Reason = "waiting for the phone notice to be recorded before the window can close"
	}
	return out
}

type automaticSubmissionEvent struct {
	Version     int       `json:"version"`
	At          time.Time `json:"at"`
	Type        string    `json:"type"`
	Key         string    `json:"key"`
	Revision    string    `json:"revision"`
	Bucket      string    `json:"bucket,omitempty"`
	State       string    `json:"state,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	AccountMode string    `json:"account_mode,omitempty"`
	SubmitAt    time.Time `json:"submit_at,omitzero"`
	OrderRef    string    `json:"order_ref,omitempty"`
	Origin      string    `json:"origin,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

type automaticSubmissionDocument struct {
	Version int                         `json:"version"`
	Records []automaticSubmissionRecord `json:"records"`
}

// automaticSubmissionStore owns the records in daemon.db. Every mutation is
// a read-modify-write under mu followed by a compare-and-swap of the whole
// document with its events in one transaction, so a veto and a cycle can
// never overwrite each other and a crash leaves either the old or the new
// document, never a torn one.
type automaticSubmissionStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	records  map[string]*automaticSubmissionRecord
}

func automaticRecordKey(key, revision string) string {
	return strings.TrimSpace(key) + "|" + strings.TrimSpace(revision)
}

func (s *automaticSubmissionStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return errors.New("automatic submission store is not attached")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, automaticStateKind)
	if err != nil {
		return err
	}
	records := map[string]*automaticSubmissionRecord{}
	var revision int64
	if ok {
		var parsed automaticSubmissionDocument
		if err := json.Unmarshal(doc.JSON, &parsed); err != nil {
			return fmt.Errorf("decode automatic submission state: %w", err)
		}
		for i := range parsed.Records {
			rec := parsed.Records[i]
			if strings.TrimSpace(rec.Key) == "" || strings.TrimSpace(rec.Revision) == "" {
				return errors.New("automatic submission state is malformed")
			}
			records[automaticRecordKey(rec.Key, rec.Revision)] = &rec
		}
		revision = doc.Revision
	}
	s.mu.Lock()
	s.core, s.records, s.revision = core, records, revision
	s.mu.Unlock()
	return nil
}

func (s *automaticSubmissionStore) attached() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.core != nil
}

// list returns copies of every record, oldest first.
func (s *automaticSubmissionStore) list() []automaticSubmissionRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *automaticSubmissionStore) listLocked() []automaticSubmissionRecord {
	out := make([]automaticSubmissionRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return automaticRecordKey(out[i].Key, out[i].Revision) < automaticRecordKey(out[j].Key, out[j].Revision)
	})
	return out
}

func (s *automaticSubmissionStore) get(key, revision string) (automaticSubmissionRecord, bool) {
	if s == nil {
		return automaticSubmissionRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[automaticRecordKey(key, revision)]
	if !ok {
		return automaticSubmissionRecord{}, false
	}
	return *rec, true
}

// update runs mutate against the live record map under the store lock and
// persists the result with its events atomically. mutate returns the events
// to append; returning none means nothing changed and nothing is written.
func (s *automaticSubmissionStore) update(ctx context.Context, mutate func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent) error {
	if s == nil {
		return errors.New("automatic submission store is not attached")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return errors.New("automatic submission store is not attached")
	}
	working := make(map[string]*automaticSubmissionRecord, len(s.records))
	for k, rec := range s.records {
		copied := *rec
		working[k] = &copied
	}
	events := mutate(working)
	if len(events) == 0 {
		return nil
	}
	doc := automaticSubmissionDocument{Version: automaticDocumentVersion}
	for _, rec := range working {
		doc.Records = append(doc.Records, *rec)
	}
	sort.Slice(doc.Records, func(i, j int) bool {
		return automaticRecordKey(doc.Records[i].Key, doc.Records[i].Revision) < automaticRecordKey(doc.Records[j].Key, doc.Records[j].Revision)
	})
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	inputs := make([]corestore.EventInput, 0, len(events))
	for i, ev := range events {
		if ev.Version == 0 {
			ev.Version = automaticDocumentVersion
		}
		if ev.At.IsZero() {
			ev.At = time.Now().UTC()
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		eventKey, err := coreStoreEventKey(ctx, s.core, "proposal-automatic", ev.At, payload, i)
		if err != nil {
			return err
		}
		inputs = append(inputs, corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: automaticCoreEventType, Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: ev.At, PayloadJSON: payload})
	}
	saved, _, err := s.core.CompareAndSwapStateDocumentWithEvents(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: automaticStateKind, ExpectedRevision: s.revision, JSON: raw,
	}, inputs)
	if err != nil {
		return fmt.Errorf("persist automatic submission state: %w", err)
	}
	s.revision = saved.Revision
	s.records = working
	return nil
}

// automaticBucketFor maps a proposal onto the policy's pre-authorised bucket
// vocabulary. The option profit trail shares the engine's trailing_stop
// bucket, so the engine bucket alone cannot name it. Anything outside the
// vocabulary (theta hygiene, risk reduction, exit reviews, unit exits)
// returns "" and can never be pre-authorised.
func automaticBucketFor(p rpc.TradeProposal) string {
	switch p.Bucket {
	case rpc.TradeProposalBucketTrailingStop:
		if p.OptionExit == nil {
			return preAuthorisedBucketTrailingStop
		}
		if p.OptionExit.Kind == risk.OptionExitActionProfitTrail {
			return preAuthorisedBucketOptionProfitTrail
		}
		return ""
	case rpc.TradeProposalBucketOptionLossExit:
		if p.OptionExit != nil && p.OptionExit.Kind == risk.OptionExitActionLoss {
			return preAuthorisedBucketOptionLossExit
		}
		return ""
	case preAuthorisedBucketBudgetReduction:
		return preAuthorisedBucketBudgetReduction
	default:
		return ""
	}
}

// automaticPolicy returns the active policy when it is safe to act on; a
// drifted or errored policy pre-authorises nothing.
func (e *proposalEngine) automaticPolicy() (protectionPolicy, bool) {
	if e == nil || e.server == nil || e.server.protectionPolicies == nil {
		return protectionPolicy{}, false
	}
	policy, status := e.server.protectionPolicies.Active()
	switch status.Status {
	case rpc.ProtectionPolicyStatusActive, rpc.ProtectionPolicyStatusDefault:
		return policy, true
	default:
		return protectionPolicy{}, false
	}
}

func (e *proposalEngine) automaticLatched(scope brokerStateScope) bool {
	if e == nil {
		return false
	}
	if e.latchedForTest != nil {
		return e.latchedForTest(scope)
	}
	return e.server.drawdownBlockLatched(scope)
}

// drawdownBlockLatched reads the risk constitution's drawdown block latch
// for one broker scope through the daemon's capital runtime, the same read
// the brief's ready.latch row is built from. Unavailable state is not a
// latch.
func (s *Server) drawdownBlockLatched(scope brokerStateScope) bool {
	if s == nil || s.riskCapital == nil || !brokerScopeConcrete(scope) {
		return false
	}
	var policy *risk.Constitution
	if s.riskPolicies != nil {
		policy = s.riskPolicies.snapshot().policy
	}
	return s.riskCapital.Report(policy, nil, scope).BlockLatched
}

// proposalUnblocked reports whether prop, as served in snap, may be acted on:
// the snapshot carries no blockers of its own, the row is generated rather
// than blocked, and it belongs to the served revision.
func proposalUnblocked(snap rpc.TradeProposalSnapshot, prop rpc.TradeProposal) bool {
	if len(snap.Blockers) > 0 || snap.LoadedFromState {
		return false
	}
	if prop.State == rpc.TradeProposalStateBlocked || len(prop.Blockers) > 0 {
		return false
	}
	return prop.Revision != "" && prop.Revision == snap.Revision
}

func proposalBlockedReason(snap rpc.TradeProposalSnapshot, prop rpc.TradeProposal) string {
	if len(snap.Blockers) > 0 {
		return "proposal set blocked: " + snap.Blockers[0].Code
	}
	if len(prop.Blockers) > 0 {
		return "proposal blocked: " + prop.Blockers[0].Code
	}
	if prop.State == rpc.TradeProposalStateBlocked {
		return "proposal blocked"
	}
	return "proposal no longer actionable"
}

// runAutomaticCycle is one pass of the pre-authorised scheduler, run after
// every proposal refresh in Run: resolve intents a previous process left at
// the broker, reconcile records against the served snapshot, then submit
// whatever is due.
func (e *proposalEngine) runAutomaticCycle(ctx context.Context) {
	if e == nil || !e.automatic.attached() || ctx == nil || ctx.Err() != nil {
		return
	}
	e.recoverAutomaticAfterRestart(ctx)
	e.reconcileAutomatic(ctx)
	e.submitDueAutomatic(ctx)
}

// reconcileAutomatic aligns records with the current snapshot: a pending
// record whose proposal is gone, blocked or re-revised is superseded, and
// every unblocked pre-authorised proposal without a record for its key and
// revision gets one, with its window already set.
func (e *proposalEngine) reconcileAutomatic(ctx context.Context) {
	snap := e.Snapshot(false)
	if snap.LoadedFromState {
		// A restored snapshot is last-session evidence; wait for a fresh
		// generation before creating or superseding anything from it.
		return
	}
	now := e.clock()
	scope := e.currentScope()
	policy, policyOK := e.automaticPolicy()
	latched := brokerScopeConcrete(scope) && e.automaticLatched(scope)
	window := policy.Authority.vetoWindow()
	present := make(map[string]rpc.TradeProposal, len(snap.Proposals))
	for _, prop := range snap.Proposals {
		present[prop.Key] = prop
	}
	err := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		var events []automaticSubmissionEvent
		for _, rec := range records {
			if !rec.pending() {
				continue
			}
			prop, ok := present[rec.Key]
			reason := ""
			switch {
			case !ok:
				reason = "proposal disappeared from the current set"
			case prop.Revision != rec.Revision:
				reason = "proposal revision changed; a new window starts for the new revision"
			case !proposalUnblocked(snap, prop):
				reason = proposalBlockedReason(snap, prop)
			case policyOK && !policy.Authority.preAuthorised(rec.Bucket):
				reason = "bucket is no longer pre-authorised by the active policy"
			case !policyOK:
				reason = "protection policy is not active"
			}
			if reason == "" {
				continue
			}
			rec.State = rpc.TradeProposalAutomaticSuperseded
			rec.Reason = reason
			rec.ResolvedAt = now
			events = append(events, automaticSubmissionEvent{At: now, Type: automaticEventSuperseded, Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, State: rec.State, AccountID: rec.AccountID, AccountMode: rec.AccountMode, Reason: reason})
		}
		if !policyOK || !brokerScopeConcrete(scope) {
			return events
		}
		for _, prop := range snap.Proposals {
			bucket := automaticBucketFor(prop)
			if !policy.Authority.preAuthorised(bucket) || !proposalUnblocked(snap, prop) {
				continue
			}
			if _, exists := records[automaticRecordKey(prop.Key, prop.Revision)]; exists {
				// One record per key and revision: a veto, an outcome or a
				// failure all hold until the revision changes.
				continue
			}
			rec := &automaticSubmissionRecord{
				Version: automaticDocumentVersion, Key: prop.Key, Revision: prop.Revision, Bucket: bucket, EngineBucket: prop.Bucket,
				Symbol: prop.Symbol, SecType: prop.SecType, Action: prop.Action, Quantity: prop.Quantity,
				AccountID: snap.AccountID, AccountMode: snap.AccountMode, State: rpc.TradeProposalAutomaticPending,
				VetoWindow: window.String(), CreatedAt: now, SubmitAt: now.Add(window),
			}
			if latched {
				rec.LatchSkippedWindow = true
				rec.SubmitAt = now
			}
			records[automaticRecordKey(rec.Key, rec.Revision)] = rec
			events = append(events, automaticSubmissionEvent{At: now, Type: automaticEventCreated, Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, State: rec.State, AccountID: rec.AccountID, AccountMode: rec.AccountMode, SubmitAt: rec.SubmitAt, Reason: automaticCreatedReason(rec)})
		}
		return events
	})
	if err != nil && e.server != nil {
		e.server.warnf("pre-authorised protection: reconcile records: %v", err)
	}
}

func automaticCreatedReason(rec *automaticSubmissionRecord) string {
	if rec.LatchSkippedWindow {
		return "drawdown brake latched; submitting without the veto window"
	}
	return "veto window " + rec.VetoWindow
}

// markAutomaticNoticed records that the Protection alert producer accepted
// the notice episode for these records. The veto window is measured from
// the notice, so a delayed notice delays the submission rather than
// shortening the owner's chance to veto. A latched record ignores this.
func (e *proposalEngine) markAutomaticNoticed(ctx context.Context, keys []automaticNoticeKey, at time.Time) {
	if e == nil || !e.automatic.attached() || len(keys) == 0 {
		return
	}
	err := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		var events []automaticSubmissionEvent
		for _, k := range keys {
			rec, ok := records[automaticRecordKey(k.Key, k.Revision)]
			if !ok || !rec.pending() || !rec.NoticedAt.IsZero() {
				continue
			}
			rec.NoticedAt = at.UTC()
			if !rec.LatchSkippedWindow {
				if window, err := time.ParseDuration(rec.VetoWindow); err == nil && window > 0 {
					if due := rec.NoticedAt.Add(window); due.After(rec.SubmitAt) {
						rec.SubmitAt = due
					}
				}
			}
			events = append(events, automaticSubmissionEvent{At: at.UTC(), Type: automaticEventNoticed, Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, State: rec.State, AccountID: rec.AccountID, AccountMode: rec.AccountMode, SubmitAt: rec.SubmitAt})
		}
		return events
	})
	if err != nil && e.server != nil {
		e.server.warnf("pre-authorised protection: record notice: %v", err)
	}
}

// automaticNoticeKey names one record for the alert producer.
type automaticNoticeKey struct {
	Key      string
	Revision string
	Bucket   string
	SubmitAt time.Time
	Latched  bool
}

// automaticPendingNotices lists the pending records in scope for the
// Protection alert producer. It carries no symbol, quantity or order data:
// the notice names the bucket and the timing; the Protection row holds the
// rest.
func (e *proposalEngine) automaticPendingNotices(scope brokerStateScope) []automaticNoticeKey {
	if e == nil || !e.automatic.attached() {
		return nil
	}
	var out []automaticNoticeKey
	for _, rec := range e.automatic.list() {
		if !rec.pending() || !sameBrokerScope(brokerStateScope{Account: rec.AccountID, Mode: rec.AccountMode}, scope) {
			continue
		}
		out = append(out, automaticNoticeKey{Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, SubmitAt: rec.SubmitAt, Latched: rec.LatchSkippedWindow})
	}
	return out
}

// submitDueAutomatic submits every pending record whose window has closed
// and whose notice was recorded (a latched record needs no notice).
func (e *proposalEngine) submitDueAutomatic(ctx context.Context) {
	now := e.clock()
	for _, rec := range e.automatic.list() {
		if !rec.pending() || now.Before(rec.SubmitAt) {
			continue
		}
		if rec.NoticedAt.IsZero() && !rec.LatchSkippedWindow {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		e.submitAutomatic(ctx, rec)
	}
}

// automaticWriteGrant is the request-scoped fact the broker-write gate
// checks for the daemon-preauthorised origin: which proposal the scheduler
// is submitting right now. It exists only while the scheduler holds
// brokerWriteMu, so no other write can observe it.
type automaticWriteGrant struct {
	Key      string
	Revision string
	Bucket   string
}

// submitAutomatic performs one automatic submission. The intent (state
// submitting, preview token id) is persisted after the preview is minted and
// before the broker call, so a crash in between is resolved from the order
// journal on restart and never produces a second order.
func (e *proposalEngine) submitAutomatic(ctx context.Context, rec automaticSubmissionRecord) {
	s := e.server
	if s == nil {
		return
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	current, ok := e.automatic.get(rec.Key, rec.Revision)
	if !ok || !current.pending() {
		// A veto landed between the list and the lock.
		return
	}
	now := e.clock()
	grant := &automaticWriteGrant{Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket}
	s.automaticGrant.Store(grant)
	defer s.automaticGrant.Store(nil)
	params := rpc.TradeProposalSubmitParams{Key: rec.Key, Revision: rec.Revision, Origin: rpc.OrderOriginDaemonPreAuthorised, TimeoutMs: int(automaticSubmitTimeout.Milliseconds())}
	staged := false
	res, err := e.submit(ctx, params, proposalSubmitOptions{automatic: true, beforePlace: func(preview *rpc.OrderPreviewResult) error {
		stageErr := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			r, ok := records[automaticRecordKey(rec.Key, rec.Revision)]
			if !ok || !r.pending() {
				return nil
			}
			r.State = rpc.TradeProposalAutomaticSubmitting
			r.SubmittingAt = e.clock()
			r.PreviewTokenID = preview.PreviewTokenID
			return []automaticSubmissionEvent{{At: r.SubmittingAt, Type: automaticEventSubmitting, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode}}
		})
		if stageErr != nil {
			return fmt.Errorf("persist automatic submission intent: %w", stageErr)
		}
		if got, ok := e.automatic.get(rec.Key, rec.Revision); !ok || got.State != rpc.TradeProposalAutomaticSubmitting {
			return errors.New("automatic submission was vetoed before the broker call")
		}
		staged = true
		return nil
	}})
	finish := e.clock()
	outcomeErr := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r, ok := records[automaticRecordKey(rec.Key, rec.Revision)]
		if !ok || r.terminal() {
			return nil
		}
		r.ResolvedAt = finish
		switch {
		case err == nil && res.Accepted:
			r.State = rpc.TradeProposalAutomaticSubmitted
			r.SubmittedAt = finish
			r.OrderRef = res.OrderRef
			r.Reason = "placed by the daemon under the pre-authorised protection policy"
			return []automaticSubmissionEvent{{At: finish, Type: automaticEventSubmitted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Origin: rpc.OrderOriginDaemonPreAuthorised}}
		case !staged && automaticBlockersSupersede(res.Blockers):
			r.State = rpc.TradeProposalAutomaticSuperseded
			r.Reason = automaticOutcomeReason(res, err)
			return []automaticSubmissionEvent{{At: finish, Type: automaticEventSuperseded, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, Reason: r.Reason}}
		default:
			r.State = rpc.TradeProposalAutomaticFailed
			r.Reason = automaticOutcomeReason(res, err)
			if staged && r.OrderRef == "" && res.OrderRef != "" {
				r.OrderRef = res.OrderRef
			}
			return []automaticSubmissionEvent{{At: finish, Type: automaticEventFailed, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, Reason: r.Reason}}
		}
	})
	if outcomeErr != nil {
		s.warnf("pre-authorised protection: record outcome for %s: %v", rec.Key, outcomeErr)
	}
	switch {
	case err == nil && res.Accepted:
		s.infof("pre-authorised protection: submitted %s bucket %s after %s", rec.Key, rec.Bucket, finish.Sub(now).Round(time.Millisecond))
	default:
		s.warnf("pre-authorised protection: %s bucket %s not submitted: %s", rec.Key, rec.Bucket, automaticOutcomeReason(res, err))
	}
}

// automaticSubmitBlockers is the proposal-side half of the origin gate: the
// revalidated proposal must be the one the scheduler holds a grant for, and
// its bucket must be pre-authorised by the policy active now.
func (e *proposalEngine) automaticSubmitBlockers(prop rpc.TradeProposal) []rpc.TradingBlocker {
	if e == nil || e.server == nil {
		return []rpc.TradingBlocker{{Code: "daemon_origin_unauthorised", Message: "automatic submission requires the daemon scheduler"}}
	}
	grant := e.server.automaticGrant.Load()
	bucket := automaticBucketFor(prop)
	if grant == nil || grant.Key != prop.Key || grant.Revision != prop.Revision || grant.Bucket != bucket {
		return []rpc.TradingBlocker{{Code: "daemon_origin_unauthorised", Message: "automatic submission grant does not name this proposal key, revision and bucket", Action: "Submit through `canary proposals submit` from a human terminal instead."}}
	}
	policy, ok := e.automaticPolicy()
	if !ok || !policy.Authority.preAuthorised(bucket) {
		return []rpc.TradingBlocker{{Code: "bucket_not_pre_authorised", Message: fmt.Sprintf("bucket %q is not pre-authorised by the active protection policy", nonEmptyString(bucket, prop.Bucket)), Action: "List the bucket under [authority].pre_authorised and bump policy_version, or submit by hand."}}
	}
	return nil
}

// automaticBlockersSupersede reports blockers that mean the proposal moved
// on rather than that the write was refused: the next cycle creates a fresh
// record for the new revision instead of recording a failure.
func automaticBlockersSupersede(blockers []rpc.TradingBlocker) bool {
	for _, b := range blockers {
		switch b.Code {
		case "stale_revision", "proposal_not_found":
			return true
		}
	}
	return false
}

func automaticOutcomeReason(res rpc.TradeProposalSubmitResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if len(res.Blockers) > 0 {
		b := res.Blockers[0]
		if strings.TrimSpace(b.Message) == "" {
			return b.Code
		}
		return b.Code + ": " + b.Message
	}
	if strings.TrimSpace(res.Message) != "" {
		return res.Message
	}
	return "broker did not accept the order"
}

// recoverAutomaticAfterRestart resolves records a previous process left in
// submitting: the intent was persisted before the broker call, so the order
// journal decides what happened. A journaled send for the record's preview
// token means the order went out; a staged attempt without an outcome is
// uncertain and stays with the broker statements; no journal trace means the
// process died before the send. None of them is retried.
func (e *proposalEngine) recoverAutomaticAfterRestart(ctx context.Context) {
	var stuck []automaticSubmissionRecord
	for _, rec := range e.automatic.list() {
		if rec.State == rpc.TradeProposalAutomaticSubmitting && rec.SubmittingAt.Before(e.startedAt) {
			stuck = append(stuck, rec)
		}
	}
	if len(stuck) == 0 {
		return
	}
	var events []orderJournalEvent
	if e.server != nil && e.server.orderJournal != nil {
		loaded, err := e.server.orderJournal.LoadEvents(0)
		if err != nil {
			if e.server != nil {
				e.server.warnf("pre-authorised protection: restart recovery cannot read the order journal: %v", err)
			}
			return
		}
		events = loaded
	}
	now := e.clock()
	err := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		var out []automaticSubmissionEvent
		for _, stale := range stuck {
			r, ok := records[automaticRecordKey(stale.Key, stale.Revision)]
			if !ok || r.State != rpc.TradeProposalAutomaticSubmitting {
				continue
			}
			state, orderRef, reason := automaticRestartOutcome(events, r.PreviewTokenID)
			r.State, r.Reason, r.ResolvedAt = state, reason, now
			if orderRef != "" {
				r.OrderRef = orderRef
			}
			eventType := automaticEventFailed
			if state == rpc.TradeProposalAutomaticSubmitted {
				eventType = automaticEventSubmitted
				r.SubmittedAt = now
			}
			out = append(out, automaticSubmissionEvent{At: now, Type: eventType, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Reason: reason})
		}
		return out
	})
	if err != nil && e.server != nil {
		e.server.warnf("pre-authorised protection: restart recovery: %v", err)
	}
}

// automaticRestartOutcome classifies a submitting record from the journal
// events that carry its preview token.
func automaticRestartOutcome(events []orderJournalEvent, tokenID string) (state, orderRef, reason string) {
	if strings.TrimSpace(tokenID) == "" {
		return rpc.TradeProposalAutomaticFailed, "", "daemon restarted before the broker send; not retried"
	}
	attempted := false
	for _, ev := range events {
		if ev.PreviewTokenID != tokenID {
			continue
		}
		switch ev.Type {
		case orderJournalEventSendCompleted, orderJournalEventBrokerAcknowledged, orderJournalEventStatusUpdated:
			return rpc.TradeProposalAutomaticSubmitted, ev.OrderRef, "order reached the broker before the daemon restarted; confirmed from the order journal"
		case orderJournalEventSendAttempted:
			attempted = true
			if orderRef == "" {
				orderRef = ev.OrderRef
			}
		case orderJournalEventSendError:
			if ev.SendDisposition == "" || ev.SendDisposition == "definitely_unsent" {
				return rpc.TradeProposalAutomaticFailed, ev.OrderRef, "broker send failed before the daemon restarted; not retried"
			}
			attempted = true
			if orderRef == "" {
				orderRef = ev.OrderRef
			}
		}
	}
	if attempted {
		return rpc.TradeProposalAutomaticFailed, orderRef, "broker send outcome unknown across the restart; confirm against the order journal and broker statements, not retried"
	}
	return rpc.TradeProposalAutomaticFailed, "", "daemon restarted before the broker send; not retried"
}

// Veto marks the pending automatic submission for a key vetoed. Only a human
// origin may veto; the hold lasts until the proposal's revision changes.
func (e *proposalEngine) Veto(ctx context.Context, p rpc.TradeProposalVetoParams) (rpc.TradeProposalVetoResult, error) {
	now := e.clock()
	key, revision := strings.TrimSpace(p.Key), strings.TrimSpace(p.Revision)
	if !originIsHuman(p.Origin) {
		return rpc.TradeProposalVetoResult{}, errBadRequest("proposal veto is human-only: agent, missing and unknown origins are refused; run `canary proposals veto` from an interactive terminal or use the paired app")
	}
	if key == "" {
		return rpc.TradeProposalVetoResult{}, errBadRequest("proposal key is required")
	}
	if !e.automatic.attached() {
		return rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: revision, Message: "automatic submission records are unavailable", AsOf: now}, nil
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		return rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: revision, Message: "proposal veto requires a concrete account and paper/live mode", AsOf: now}, nil
	}
	snap := e.Snapshot(false)
	var served *rpc.TradeProposal
	for i := range snap.Proposals {
		if snap.Proposals[i].Key == key {
			served = &snap.Proposals[i]
			break
		}
	}
	policy, policyOK := e.automaticPolicy()
	var result rpc.TradeProposalVetoResult
	err := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		// Find the record: the exact revision when given, otherwise the
		// live one for the key (pending or submitting first, then the
		// served revision).
		var rec *automaticSubmissionRecord
		if revision != "" {
			rec = records[automaticRecordKey(key, revision)]
		} else {
			for _, candidate := range records {
				if candidate.Key != key {
					continue
				}
				if candidate.State == rpc.TradeProposalAutomaticPending || candidate.State == rpc.TradeProposalAutomaticSubmitting {
					rec = candidate
					break
				}
			}
			if rec == nil && served != nil {
				rec = records[automaticRecordKey(key, served.Revision)]
			}
		}
		if rec == nil {
			if served == nil || (revision != "" && served.Revision != revision) {
				result = rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: revision, Message: "no automatic submission is pending for this proposal key at this revision", AsOf: now}
				return nil
			}
			bucket := automaticBucketFor(*served)
			if !policyOK || !policy.Authority.preAuthorised(bucket) {
				result = rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: served.Revision, Message: "this proposal's bucket is not pre-authorised; nothing submits itself, so there is nothing to veto", AsOf: now}
				return nil
			}
			// No record yet (the row may still be blocked): hold the key at
			// this revision so it never gets a window later.
			rec = &automaticSubmissionRecord{
				Version: automaticDocumentVersion, Key: key, Revision: served.Revision, Bucket: bucket, EngineBucket: served.Bucket,
				Symbol: served.Symbol, SecType: served.SecType, Action: served.Action, Quantity: served.Quantity,
				AccountID: snap.AccountID, AccountMode: snap.AccountMode, State: rpc.TradeProposalAutomaticPending,
				VetoWindow: policy.Authority.vetoWindow().String(), CreatedAt: now, SubmitAt: now,
			}
			records[automaticRecordKey(rec.Key, rec.Revision)] = rec
		}
		switch rec.State {
		case rpc.TradeProposalAutomaticVetoed:
			result = rpc.TradeProposalVetoResult{Accepted: true, Key: key, Revision: rec.Revision, State: rec.State, Message: "already vetoed; the hold lasts until the proposal revision changes", Automatic: rec.view(true, now), AsOf: now}
			return nil
		case rpc.TradeProposalAutomaticSubmitting:
			result = rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: rec.Revision, State: rec.State, Message: "the order is already being placed; cancel it through the orders surface once it is journaled", Automatic: rec.view(true, now), AsOf: now}
			return nil
		case rpc.TradeProposalAutomaticPending:
		default:
			result = rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: rec.Revision, State: rec.State, Message: "this automatic submission is already " + rec.State + " and cannot be vetoed", Automatic: rec.view(true, now), AsOf: now}
			return nil
		}
		rec.State = rpc.TradeProposalAutomaticVetoed
		rec.VetoedAt = now
		rec.ResolvedAt = now
		rec.Origin = p.Origin
		rec.Reason = nonEmptyString(strings.TrimSpace(p.Reason), "vetoed by the owner")
		result = rpc.TradeProposalVetoResult{Accepted: true, Key: key, Revision: rec.Revision, State: rec.State, Message: "automatic submission vetoed; the hold lasts until the proposal revision changes", Automatic: rec.view(true, now), AsOf: now}
		return []automaticSubmissionEvent{{At: now, Type: automaticEventVetoed, Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, State: rec.State, AccountID: rec.AccountID, AccountMode: rec.AccountMode, Origin: p.Origin, Reason: rec.Reason}}
	})
	if err != nil {
		return rpc.TradeProposalVetoResult{Accepted: false, Key: key, Revision: revision, Message: "veto was not persisted: " + err.Error(), AsOf: now}, nil
	}
	return result, nil
}

// decorateAutomatic attaches the pre-authorisation fact and any automatic
// record to each served proposal. Records are keyed by key and revision, so
// a row shows only the record for the revision it carries.
func (e *proposalEngine) decorateAutomatic(snap *rpc.TradeProposalSnapshot) {
	if e == nil || snap == nil {
		return
	}
	policy, policyOK := e.automaticPolicy()
	now := e.clock()
	for i := range snap.Proposals {
		prop := &snap.Proposals[i]
		bucket := automaticBucketFor(*prop)
		pre := policyOK && policy.Authority.preAuthorised(bucket)
		if rec, ok := e.automatic.get(prop.Key, prop.Revision); ok {
			prop.Automatic = rec.view(pre, now)
			continue
		}
		if bucket == "" && !pre {
			continue
		}
		prop.Automatic = &rpc.TradeProposalAutomatic{PreAuthorised: pre, Bucket: bucket}
		if pre {
			prop.Automatic.VetoWindow = policy.Authority.vetoWindow().String()
		}
	}
}

// automaticPendingCount reports pending records in the given scope for the
// status surface.
func (e *proposalEngine) automaticPendingCount(scope brokerStateScope) int {
	if e == nil || !e.automatic.attached() {
		return 0
	}
	n := 0
	for _, rec := range e.automatic.list() {
		if rec.pending() && sameBrokerScope(brokerStateScope{Account: rec.AccountID, Mode: rec.AccountMode}, scope) {
			n++
		}
	}
	return n
}

func (s *Server) handleTradeProposalsVeto(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalVetoResult, error) {
	var p rpc.TradeProposalVetoParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalVetoResult{Accepted: false, Key: p.Key, Revision: p.Revision, Message: "proposal engine is unavailable", AsOf: s.orderNow()}, nil
	}
	res, err := s.tradeProposals.Veto(ctx, p)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
