package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	regimeProjectionReceiptKind    = "regime_snapshot.projections.v1"
	regimeProjectionReceiptVersion = 2
	regimeProjectionReceiptLegacy  = 1

	regimeDecisionProjectionStateKind    = "regime_snapshot.decision_projection.v1"
	regimeDecisionProjectionStateVersion = 1
	regimeDecisionEventRecorded          = "recorded"
	regimeDecisionEventDisabled          = "disabled_by_setting"
)

// regimeProjectionReceipt is written only after streaks, the rule-stage
// latch, and the decision journal have all accepted one authoritative Regime
// publication. Its snapshot revision and SQLite commit timestamp form a stable
// recovery identity across restart; wall-clock snapshot as_of alone does not.
type regimeProjectionReceipt struct {
	Version             int             `json:"version"`
	SnapshotRevision    int64           `json:"snapshot_revision"`
	SnapshotPublishedAt time.Time       `json:"snapshot_published_at"`
	SnapshotFingerprint rpc.Fingerprint `json:"snapshot_fingerprint"`
	DecisionEvent       string          `json:"decision_event"`
}

// regimeProjectionPlan pins the one safe recovery relation. A projection may
// already match the current publication, or may advance exactly once from the
// prior receipted publication (missing/legacy metadata is accepted only for a
// first publication with no receipt). When the receipt already names current,
// validateOnly forbids every content or metadata repair.
type regimeProjectionPlan struct {
	publication      regimeSnapshotPublication
	receipt          regimeProjectionReceipt
	receiptOK        bool
	validateOnly     bool
	initial          bool
	legacyReceipt    bool
	previous         *regimeSnapshotPublication
	previousDecision string
}

type regimeProjectionPosition uint8

const (
	regimeProjectionInitial regimeProjectionPosition = iota
	regimeProjectionPrevious
	regimeProjectionCurrent
)

type regimeDecisionProjectionState struct {
	Version             int             `json:"version"`
	SnapshotRevision    int64           `json:"snapshot_revision"`
	SnapshotPublishedAt time.Time       `json:"snapshot_published_at"`
	SnapshotFingerprint rpc.Fingerprint `json:"snapshot_fingerprint"`
	DecisionEvent       string          `json:"decision_event"`
}

func (cache *regimeSnapshotCache) publication() (regimeSnapshotPublication, *rpc.RegimeSnapshotResult, error) {
	if cache == nil {
		return regimeSnapshotPublication{}, nil, errors.New("regime snapshot cache is unavailable")
	}
	view, err := cache.current()
	if err != nil {
		return regimeSnapshotPublication{}, nil, err
	}
	if view.Snapshot == nil {
		return regimeSnapshotPublication{}, nil, nil
	}
	if view.Health.LastSuccessAt == nil {
		return regimeSnapshotPublication{}, nil, errors.New("regime snapshot publication timestamp is unavailable")
	}
	return regimeSnapshotPublication{
		Revision: view.Revision, PublishedAt: view.Health.LastSuccessAt.UTC(), Fingerprint: view.Fingerprint,
	}, view.Snapshot, nil
}

// reconcileRegimeSnapshotProjections repairs the narrow crash window between
// the authoritative snapshot CAS and its derived projector commits. It runs
// before the RPC socket is published. Replays are idempotent and the receipt is
// advanced only after every projector succeeds.
func (s *Server) reconcileRegimeSnapshotProjections(ctx context.Context, cache *regimeSnapshotCache) error {
	publication, snapshot, err := cache.publication()
	if err != nil {
		return err
	}
	if snapshot == nil {
		receipt, ok, err := s.loadRegimeProjectionReceipt(ctx)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("regime projection receipt revision %d exists without an authoritative snapshot", receipt.SnapshotRevision)
		}
		return nil
	}
	plan, err := s.prepareRegimeProjectionPlan(ctx, publication)
	if err != nil {
		return err
	}
	streaks, err := s.regimeProjectionStreakStore()
	if err != nil {
		return err
	}
	if err := streaks.reconcileRegimeProjection(ctx, snapshot, plan); err != nil {
		return err
	}
	if err := s.reconcileRulesRegimeStageProjection(ctx, snapshot, plan); err != nil {
		return err
	}
	decisionEvent, err := s.reconcileRegimeDecisionProjection(ctx, snapshot, plan)
	if err != nil {
		return err
	}
	if err := s.recordRegimeProjectionReceiptWithDecision(ctx, publication, decisionEvent); err != nil {
		return err
	}
	return s.publishRulesRegimeStageProjection(ctx, publication)
}

// commitRegimeSnapshotProjections is the normal after-publish barrier. The
// rule-stage candidate is durable before the decision disposition and receipt,
// but cannot become the in-memory rulebook latch until both have committed.
func (s *Server) commitRegimeSnapshotProjections(ctx context.Context, snapshot *rpc.RegimeSnapshotResult, evaluated *StreakStore, publication regimeSnapshotPublication) error {
	plan, err := s.prepareRegimeProjectionPlan(ctx, publication)
	if err != nil {
		return err
	}
	streaks, err := s.regimeProjectionStreakStore()
	if err != nil {
		return err
	}
	if evaluated != nil {
		if err := streaks.commitRegimeEvaluation(ctx, evaluated, plan); err != nil {
			return err
		}
	} else if err := streaks.reconcileRegimeProjection(ctx, snapshot, plan); err != nil {
		return err
	}
	if err := s.reconcileRulesRegimeStageProjection(ctx, snapshot, plan); err != nil {
		return err
	}
	decisionEvent, err := s.reconcileRegimeDecisionProjection(ctx, snapshot, plan)
	if err != nil {
		return err
	}
	if err := s.recordRegimeProjectionReceiptWithDecision(ctx, publication, decisionEvent); err != nil {
		return err
	}
	return s.publishRulesRegimeStageProjection(ctx, publication)
}

func (s *Server) regimeProjectionStreakStore() (*StreakStore, error) {
	if s == nil || s.coreStore == nil {
		return nil, errors.New("regime streak SQLite authority is unavailable")
	}
	if s.streaks == nil {
		streaks := NewStreakStore("")
		if err := streaks.UseCoreStore(s.coreStore); err != nil {
			return nil, err
		}
		s.streaks = streaks
	}
	return s.streaks, nil
}

func (s *Server) prepareRegimeProjectionPlan(ctx context.Context, publication regimeSnapshotPublication) (regimeProjectionPlan, error) {
	publication.PublishedAt = publication.PublishedAt.UTC()
	if err := validateRegimeSnapshotPublication(publication); err != nil {
		return regimeProjectionPlan{}, err
	}
	plan := regimeProjectionPlan{publication: publication, initial: publication.Revision == 1}
	receipt, ok, err := s.loadRegimeProjectionReceipt(ctx)
	if err != nil {
		return regimeProjectionPlan{}, err
	}
	if !ok {
		if publication.Revision != 1 {
			return regimeProjectionPlan{}, fmt.Errorf("regime projection receipt is missing for snapshot revision %d", publication.Revision)
		}
		return plan, nil
	}
	plan.receipt, plan.receiptOK = receipt, true
	if receipt.Version == regimeProjectionReceiptLegacy {
		if receipt.SnapshotRevision != 1 {
			return regimeProjectionPlan{}, fmt.Errorf("legacy regime projection receipt revision %d cannot be repaired", receipt.SnapshotRevision)
		}
		plan.legacyReceipt = true
	}
	if receipt.SnapshotRevision > publication.Revision {
		return regimeProjectionPlan{}, fmt.Errorf("regime projection receipt revision %d is ahead of snapshot revision %d", receipt.SnapshotRevision, publication.Revision)
	}
	if exactRegimeSnapshotPublication(publicationFromRegimeProjectionReceipt(receipt), publication) {
		plan.validateOnly = true
		return plan, nil
	}
	if receipt.SnapshotRevision == publication.Revision {
		return regimeProjectionPlan{}, fmt.Errorf("regime projection receipt revision %d cannot safely recover divergent snapshot publication", receipt.SnapshotRevision)
	}
	if receipt.SnapshotRevision != publication.Revision-1 {
		return regimeProjectionPlan{}, fmt.Errorf("regime projection receipt revision %d cannot safely recover snapshot revision %d", receipt.SnapshotRevision, publication.Revision)
	}
	previous := publicationFromRegimeProjectionReceipt(receipt)
	plan.previous = &previous
	plan.previousDecision = receipt.DecisionEvent
	if plan.legacyReceipt {
		plan.previousDecision = regimeDecisionEventRecorded
	}
	return plan, nil
}

func (s *Server) loadRegimeProjectionReceipt(ctx context.Context) (regimeProjectionReceipt, bool, error) {
	if s == nil || s.coreStore == nil {
		return regimeProjectionReceipt{}, false, errors.New("regime projection SQLite authority is unavailable")
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, regimeProjectionReceiptKind)
	if err != nil || !ok {
		return regimeProjectionReceipt{}, ok, err
	}
	receipt, err := decodeRegimeProjectionReceipt(doc.JSON)
	if err != nil {
		return regimeProjectionReceipt{}, false, fmt.Errorf("decode regime projection receipt: %w", err)
	}
	return receipt, true, nil
}

func (s *Server) recordRegimeProjectionReceipt(ctx context.Context, publication regimeSnapshotPublication) error {
	return s.recordRegimeProjectionReceiptWithDecision(ctx, publication, regimeDecisionEventRecorded)
}

func (s *Server) recordRegimeProjectionReceiptWithDecision(ctx context.Context, publication regimeSnapshotPublication, decisionEvent string) error {
	if s == nil || s.coreStore == nil {
		return errors.New("regime projection SQLite authority is unavailable")
	}
	if err := validateRegimeSnapshotPublication(publication); err != nil {
		return err
	}
	if !validRegimeDecisionEventDisposition(decisionEvent) {
		return fmt.Errorf("regime projection decision event disposition %q is invalid", decisionEvent)
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, regimeProjectionReceiptKind)
	if err != nil {
		return fmt.Errorf("load regime projection receipt for update: %w", err)
	}
	if ok {
		current, err := decodeRegimeProjectionReceipt(doc.JSON)
		if err != nil {
			return fmt.Errorf("decode regime projection receipt for update: %w", err)
		}
		if exactRegimeSnapshotPublication(publicationFromRegimeProjectionReceipt(current), publication) {
			if current.Version == regimeProjectionReceiptVersion && current.DecisionEvent == decisionEvent {
				return nil
			}
			if current.Version == regimeProjectionReceiptVersion && current.DecisionEvent != decisionEvent {
				return fmt.Errorf("regime projection receipt decision disposition mismatch at snapshot revision %d", publication.Revision)
			}
		}
		if current.SnapshotRevision > publication.Revision {
			return fmt.Errorf("refuse to move regime projection receipt backwards from revision %d to %d", current.SnapshotRevision, publication.Revision)
		}
		if current.SnapshotRevision == publication.Revision &&
			!exactRegimeSnapshotPublication(publicationFromRegimeProjectionReceipt(current), publication) {
			return fmt.Errorf("refuse to replace divergent regime projection receipt at revision %d", publication.Revision)
		}
	}
	receipt := regimeProjectionReceipt{
		Version: regimeProjectionReceiptVersion, SnapshotRevision: publication.Revision,
		SnapshotPublishedAt: publication.PublishedAt.UTC(), SnapshotFingerprint: publication.Fingerprint,
		DecisionEvent: decisionEvent,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode regime projection receipt: %w", err)
	}
	expected := int64(0)
	if ok {
		expected = doc.Revision
	}
	if _, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: regimeProjectionReceiptKind, ExpectedRevision: expected, JSON: raw,
	}); err != nil {
		return fmt.Errorf("persist regime projection receipt: %w", err)
	}
	return nil
}

func validateRegimeSnapshotPublication(publication regimeSnapshotPublication) error {
	if publication.Revision <= 0 || publication.PublishedAt.IsZero() || publication.Fingerprint.Key == "" || publication.Fingerprint.Version == "" {
		return errors.New("regime snapshot publication identity is incomplete")
	}
	return nil
}

func publicationFromRegimeProjectionReceipt(receipt regimeProjectionReceipt) regimeSnapshotPublication {
	return regimeSnapshotPublication{
		Revision: receipt.SnapshotRevision, PublishedAt: receipt.SnapshotPublishedAt.UTC(), Fingerprint: receipt.SnapshotFingerprint,
	}
}

func hasAnyRegimeSnapshotPublicationIdentity(publication regimeSnapshotPublication) bool {
	return publication.Revision != 0 || !publication.PublishedAt.IsZero() ||
		publication.Fingerprint.Version != "" || publication.Fingerprint.Key != ""
}

func exactRegimeSnapshotPublication(left, right regimeSnapshotPublication) bool {
	return left.Revision == right.Revision && left.PublishedAt.Equal(right.PublishedAt) && left.Fingerprint == right.Fingerprint
}

func validRegimeDecisionEventDisposition(disposition string) bool {
	return disposition == regimeDecisionEventRecorded || disposition == regimeDecisionEventDisabled
}

func decodeRegimeProjectionReceipt(raw []byte) (regimeProjectionReceipt, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var receipt regimeProjectionReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return regimeProjectionReceipt{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return regimeProjectionReceipt{}, errors.New("regime projection receipt has trailing JSON")
		}
		return regimeProjectionReceipt{}, err
	}
	if receipt.Version != regimeProjectionReceiptVersion && receipt.Version != regimeProjectionReceiptLegacy {
		return regimeProjectionReceipt{}, errors.New("regime projection receipt version is invalid")
	}
	if receipt.SnapshotRevision <= 0 || receipt.SnapshotPublishedAt.IsZero() || receipt.SnapshotFingerprint.Key == "" || receipt.SnapshotFingerprint.Version == "" {
		return regimeProjectionReceipt{}, errors.New("regime projection receipt is invalid")
	}
	if receipt.Version == regimeProjectionReceiptVersion && !validRegimeDecisionEventDisposition(receipt.DecisionEvent) {
		return regimeProjectionReceipt{}, errors.New("regime projection receipt decision event disposition is invalid")
	}
	if receipt.Version == regimeProjectionReceiptLegacy && receipt.DecisionEvent != "" {
		return regimeProjectionReceipt{}, errors.New("legacy regime projection receipt has a decision event disposition")
	}
	return receipt, nil
}

func (s *StreakStore) reconcileRegimeProjection(ctx context.Context, snapshot *rpc.RegimeSnapshotResult, plan regimeProjectionPlan) error {
	if s == nil || snapshot == nil {
		return nil
	}
	publication := plan.publication
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	position, err := s.regimeProjectionPosition(plan)
	if err != nil {
		return err
	}
	beforeEntries := cloneStreakEntries(s.entries)
	beforeAsOf := s.asOf
	beforePublication := s.publication
	beforeExists := s.stateExists
	next, err := projectedRegimeStreakEntries(s.entries, snapshot, publication)
	if err != nil {
		return err
	}
	if position == regimeProjectionCurrent {
		if !maps.Equal(s.entries, next) {
			return fmt.Errorf("regime streak projection content mismatch at snapshot revision %d", publication.Revision)
		}
		return nil
	}
	s.entries = next
	s.loaded = true
	if err := s.saveLockedContextPublication(ctx, publication); err != nil {
		s.entries = beforeEntries
		s.asOf = beforeAsOf
		s.publication = beforePublication
		s.stateExists = beforeExists
		return fmt.Errorf("repair regime streak projection: %w", err)
	}
	return nil
}

func projectedRegimeStreakEntries(current map[string]StreakEntry, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) (map[string]StreakEntry, error) {
	next := cloneStreakEntries(current)
	lastSession := nyTradingSessionKey(nyTime(snapshot.AsOf))
	for _, indicator := range streakIndicators {
		key := indicator.key()
		_, meta, streak := regimeDecisionRowView(snapshot, key)
		if streak == nil {
			continue // an unranked row froze, rather than cleared, prior state
		}
		if streak.Sessions <= 0 || streak.Since == "" || (streak.Band != "green" && streak.Band != "yellow" && streak.Band != "red") {
			return nil, fmt.Errorf("reconcile regime streak %s: invalid served streak", key)
		}
		_, value := indicator.bandAndValue(snapshot)
		prior, existed := next[key]
		sameStreak := prior.LastBand == streak.Band && prior.SinceDate == streak.Since
		lastSessionForTarget := lastSession
		switch {
		case !existed:
			if streak.Sessions != 1 {
				return nil, fmt.Errorf("reconcile regime streak %s: missing prior state for session count %d", key, streak.Sessions)
			}
		case sameStreak && streak.Sessions == prior.Sessions:
			lastSessionForTarget = prior.LastSession
		case sameStreak && streak.Sessions == prior.Sessions+1:
			// One crash gap can advance at most one trading-session tick.
		case sameStreak:
			return nil, fmt.Errorf("reconcile regime streak %s: impossible session transition %d -> %d", key, prior.Sessions, streak.Sessions)
		case streak.Sessions != 1:
			return nil, fmt.Errorf("reconcile regime streak %s: reset state has session count %d", key, streak.Sessions)
		}
		latched := streak.Band == "red" && sameStreak && prior.EligibleLatched
		// A newly earned latch is applied after eligibility is attached to the
		// wire, so Eligibility.Latched can still be false on that publication.
		// Eligibility.Eligible is therefore the recovery proof for the newly
		// latched case; an existing same-streak latch is preserved when overdue
		// evidence temporarily makes Eligible false.
		if streak.Band == "red" && meta.Eligibility != nil && meta.Eligibility.Eligible {
			latched = true
		}
		next[key] = StreakEntry{
			LastBand: streak.Band, SinceDate: streak.Since, LastSession: lastSessionForTarget,
			Sessions: streak.Sessions, LastValue: value, EligibleLatched: latched,
		}
	}
	return next, nil
}

func (s *StreakStore) regimeProjectionPosition(plan regimeProjectionPlan) (regimeProjectionPosition, error) {
	if s.loadErr != nil {
		return 0, s.loadErr
	}
	if err := validateRegimeStreakEntries(s.entries); err != nil {
		return 0, err
	}
	publication := plan.publication
	if s.stateExists && hasAnyRegimeSnapshotPublicationIdentity(s.publication) {
		switch {
		case exactRegimeSnapshotPublication(s.publication, publication):
			return regimeProjectionCurrent, nil
		case s.publication.Revision > publication.Revision:
			return 0, fmt.Errorf("regime streak projection revision %d is ahead of snapshot revision %d", s.publication.Revision, publication.Revision)
		case s.publication.Revision == publication.Revision:
			return 0, fmt.Errorf("regime streak projection diverges at snapshot revision %d", publication.Revision)
		case plan.previous != nil && exactRegimeSnapshotPublication(s.publication, *plan.previous):
			return regimeProjectionPrevious, nil
		case plan.previous != nil && s.publication.Revision == plan.previous.Revision:
			return 0, fmt.Errorf("regime streak projection diverges from receipted revision %d", plan.previous.Revision)
		default:
			return 0, fmt.Errorf("regime streak projection revision %d cannot safely advance to snapshot revision %d", s.publication.Revision, publication.Revision)
		}
	}
	if plan.validateOnly {
		return 0, fmt.Errorf("regime streak projection metadata is missing at receipted snapshot revision %d", publication.Revision)
	}
	if !plan.initial || plan.previous != nil {
		return 0, fmt.Errorf("legacy regime streak projection metadata cannot safely advance to snapshot revision %d", publication.Revision)
	}
	return regimeProjectionInitial, nil
}

func validateRegimeStreakEntries(entries map[string]StreakEntry) error {
	known := make(map[string]struct{}, len(streakIndicators))
	for _, indicator := range streakIndicators {
		known[indicator.key()] = struct{}{}
	}
	for key, entry := range entries {
		if _, ok := known[key]; !ok {
			return fmt.Errorf("regime streak projection contains unknown indicator %q", key)
		}
		if entry.LastBand != "green" && entry.LastBand != "yellow" && entry.LastBand != "red" {
			return fmt.Errorf("regime streak projection %s has invalid band %q", key, entry.LastBand)
		}
		if entry.Sessions <= 0 || entry.SinceDate == "" || entry.LastSession == "" {
			return fmt.Errorf("regime streak projection %s has incomplete streak state", key)
		}
		if _, err := time.Parse("2006-01-02", entry.SinceDate); err != nil {
			return fmt.Errorf("regime streak projection %s has invalid since date", key)
		}
		if _, err := time.Parse("2006-01-02", entry.LastSession); err != nil {
			return fmt.Errorf("regime streak projection %s has invalid last session", key)
		}
		if entry.EligibleLatched && entry.LastBand != "red" {
			return fmt.Errorf("regime streak projection %s has a latch outside a red streak", key)
		}
	}
	return nil
}

func (s *Server) reconcileRulesRegimeStageProjection(ctx context.Context, snapshot *rpc.RegimeSnapshotResult, plan regimeProjectionPlan) error {
	return s.projectRulesRegimeStageExact(ctx, snapshot, plan.publication, plan)
}

func (s *Server) projectRulesRegimeStageExact(ctx context.Context, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication, plan regimeProjectionPlan) error {
	if s == nil || s.coreStore == nil {
		return errors.New("rules regime stage SQLite authority is unavailable")
	}
	if snapshot == nil {
		return errors.New("rules regime stage snapshot is unavailable")
	}
	if !exactRegimeSnapshotPublication(publication, plan.publication) {
		return errors.New("rules regime stage projection plan does not match publication")
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage)
	if err != nil {
		return fmt.Errorf("read rules regime stage projection: %w", err)
	}
	state := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	if ok {
		state, err = decodeRulesRegimeStageState(doc.JSON)
		if err != nil {
			return fmt.Errorf("decode rules regime stage projection: %w", err)
		}
	}
	position, err := rulesRegimeProjectionPosition(state, ok, plan)
	if err != nil {
		return err
	}
	if position == regimeProjectionCurrent {
		if !state.previousPresent {
			return fmt.Errorf("rules regime stage projection revision %d is missing its publication barrier predecessor", publication.Revision)
		}
		base := state.previousState()
		if plan.previous != nil && !exactRegimeSnapshotPublication(base.publication, *plan.previous) {
			return fmt.Errorf("rules regime stage projection predecessor diverges from receipted revision %d", plan.previous.Revision)
		}
		expected := projectedRulesRegimeStageState(base, snapshot, publication)
		if !equalRulesRegimeStageState(state, expected) {
			return fmt.Errorf("rules regime stage projection content mismatch at snapshot revision %d", publication.Revision)
		}
		return nil
	}

	base := state.withoutProjectionHistory()
	expected := projectedRulesRegimeStageState(base, snapshot, publication)
	raw, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("encode rules regime stage projection: %w", err)
	}
	expectedRevision := int64(0)
	if ok {
		expectedRevision = doc.Revision
	}
	if _, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage,
		ExpectedRevision: expectedRevision, JSON: raw,
	}); err != nil {
		return fmt.Errorf("write rules regime stage projection: %w", err)
	}
	return nil
}

func rulesRegimeProjectionPosition(state rulesRegimeStageState, exists bool, plan regimeProjectionPlan) (regimeProjectionPosition, error) {
	publication := plan.publication
	if exists && hasAnyRegimeSnapshotPublicationIdentity(state.publication) {
		switch {
		case exactRegimeSnapshotPublication(state.publication, publication):
			return regimeProjectionCurrent, nil
		case state.publication.Revision > publication.Revision:
			return 0, fmt.Errorf("rules regime stage projection revision %d is ahead of snapshot revision %d", state.publication.Revision, publication.Revision)
		case state.publication.Revision == publication.Revision:
			return 0, fmt.Errorf("rules regime stage projection diverges at snapshot revision %d", publication.Revision)
		case plan.previous != nil && exactRegimeSnapshotPublication(state.publication, *plan.previous):
			return regimeProjectionPrevious, nil
		case plan.previous != nil && state.publication.Revision == plan.previous.Revision:
			return 0, fmt.Errorf("rules regime stage projection diverges from receipted revision %d", plan.previous.Revision)
		default:
			return 0, fmt.Errorf("rules regime stage projection revision %d cannot safely advance to snapshot revision %d", state.publication.Revision, publication.Revision)
		}
	}
	if plan.validateOnly {
		return 0, fmt.Errorf("rules regime stage projection metadata is missing at receipted snapshot revision %d", publication.Revision)
	}
	if !plan.initial || plan.previous != nil {
		return 0, fmt.Errorf("legacy rules regime stage projection metadata cannot safely advance to snapshot revision %d", publication.Revision)
	}
	return regimeProjectionInitial, nil
}

func projectedRulesRegimeStageState(base rulesRegimeStageState, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) rulesRegimeStageState {
	base = base.withoutProjectionHistory()
	next := base
	next.Version = rulesRegimeStageStateVer
	if snapshot.TapeSessionState != rpc.TapeSessionClosedDate {
		if bucket := bucketRegimeStage(snapshot.Lifecycle.Stage); bucket != "" {
			next.Bucket = bucket
			next.Stage = snapshot.Lifecycle.Stage
			next.AsOf = publication.PublishedAt.UTC()
		}
	}
	next.publication = publication
	next.previousPresent = true
	next.previousBucket = base.Bucket
	next.previousStage = base.Stage
	next.previousAsOf = base.AsOf
	next.previous = base.publication
	return next
}

func (s *Server) publishRulesRegimeStageProjection(ctx context.Context, publication regimeSnapshotPublication) error {
	if s == nil || s.coreStore == nil {
		return errors.New("rules regime stage SQLite authority is unavailable")
	}
	receipt, ok, err := s.loadRegimeProjectionReceipt(ctx)
	if err != nil {
		return fmt.Errorf("load rule-stage publication receipt: %w", err)
	}
	if !ok || receipt.Version != regimeProjectionReceiptVersion ||
		!exactRegimeSnapshotPublication(publicationFromRegimeProjectionReceipt(receipt), publication) {
		return fmt.Errorf("refuse to publish unreceipted rules regime stage revision %d", publication.Revision)
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage)
	if err != nil || !ok {
		if err != nil {
			return fmt.Errorf("load receipted rules regime stage projection: %w", err)
		}
		return errors.New("load receipted rules regime stage projection: state is missing")
	}
	state, err := decodeRulesRegimeStageState(doc.JSON)
	if err != nil {
		return fmt.Errorf("decode receipted rules regime stage projection: %w", err)
	}
	if !exactRegimeSnapshotPublication(state.publication, publication) {
		return fmt.Errorf("rules regime stage projection does not match receipted snapshot revision %d", publication.Revision)
	}
	state = state.withoutProjectionHistory()
	s.publishRulesRegimeStageState(state, publication)
	return nil
}

func (s *Server) reconcileRegimeDecisionProjection(ctx context.Context, snapshot *rpc.RegimeSnapshotResult, plan regimeProjectionPlan) (string, error) {
	if s == nil || s.coreStore == nil || snapshot == nil {
		return "", errors.New("regime decision projection authority is unavailable")
	}
	if s.regimeDecisions == nil {
		s.regimeDecisions = &regimeDecisionJournal{core: s.coreStore}
	} else if s.regimeDecisions.core == nil {
		s.regimeDecisions.core = s.coreStore
	}
	publication := plan.publication
	events, err := loadAllCoreEvents(ctx, s.coreStore, coreEventRegimeDecision)
	if err != nil {
		return "", fmt.Errorf("load regime decisions for projection recovery: %w", err)
	}
	currentEvent, currentLine, currentOK, err := findRegimeDecisionProjectionEvent(events, publication.Revision)
	if err != nil {
		return "", err
	}
	if currentOK {
		if err := validateRegimeDecisionProjectionEvent(currentEvent, currentLine, snapshot, publication); err != nil {
			return "", err
		}
	}
	for _, event := range events {
		var line regimeDecisionLine
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			return "", fmt.Errorf("decode regime decision for projection recovery: %w", err)
		}
		if line.SnapshotRevision > publication.Revision {
			return "", fmt.Errorf("regime decision projection revision %d is ahead of snapshot revision %d", line.SnapshotRevision, publication.Revision)
		}
	}

	marker, markerOK, markerDoc, err := s.loadRegimeDecisionProjectionState(ctx)
	if err != nil {
		return "", err
	}
	markerPosition, err := regimeDecisionProjectionPosition(marker, markerOK, plan)
	if err != nil {
		return "", err
	}
	if plan.previous != nil {
		if err := validatePriorRegimeDecisionDisposition(events, *plan.previous, plan.previousDecision); err != nil {
			return "", err
		}
	}

	disposition := ""
	if markerPosition == regimeProjectionCurrent {
		disposition = marker.DecisionEvent
	} else if currentOK {
		disposition = regimeDecisionEventRecorded
	} else if s.regimeJournalEnabled() {
		disposition = regimeDecisionEventRecorded
	} else {
		disposition = regimeDecisionEventDisabled
	}
	if plan.validateOnly {
		want := plan.receipt.DecisionEvent
		if plan.legacyReceipt {
			want = regimeDecisionEventRecorded
		}
		if disposition != want {
			return "", fmt.Errorf("regime decision projection disposition %q does not match receipted disposition %q", disposition, want)
		}
		if markerPosition != regimeProjectionCurrent && !plan.legacyReceipt {
			return "", fmt.Errorf("regime decision projection marker is missing at receipted snapshot revision %d", publication.Revision)
		}
	}

	if markerPosition != regimeProjectionCurrent {
		if plan.validateOnly && !plan.legacyReceipt {
			return "", fmt.Errorf("regime decision projection marker cannot be repaired at receipted snapshot revision %d", publication.Revision)
		}
		if err := s.persistRegimeDecisionProjectionState(ctx, markerDoc, markerOK, publication, disposition); err != nil {
			return "", err
		}
	}

	switch disposition {
	case regimeDecisionEventRecorded:
		if !currentOK {
			if plan.validateOnly {
				return "", fmt.Errorf("regime decision event is missing at receipted snapshot revision %d", publication.Revision)
			}
			if err := s.regimeDecisions.appendPublicationContext(ctx, publication.PublishedAt, snapshot, publication); err != nil {
				return "", fmt.Errorf("repair regime decision projection: %w", err)
			}
			s.kickHistoryIndex()
			currentLine = buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication)
		}
		s.regimeDecisions.mu.Lock()
		s.regimeDecisions.lastFingerprint = currentLine.Fingerprint
		s.regimeDecisions.lastWrite = currentLine.TS
		s.regimeDecisions.mu.Unlock()
	case regimeDecisionEventDisabled:
		if currentOK {
			return "", fmt.Errorf("regime decision event exists at disabled snapshot revision %d", publication.Revision)
		}
	default:
		return "", fmt.Errorf("regime decision projection disposition %q is invalid", disposition)
	}
	return disposition, nil
}

func findRegimeDecisionProjectionEvent(events []corestore.EventRecord, revision int64) (corestore.EventRecord, regimeDecisionLine, bool, error) {
	wantKey := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, revision)
	var found corestore.EventRecord
	var line regimeDecisionLine
	count := 0
	for _, event := range events {
		var candidate regimeDecisionLine
		if err := json.Unmarshal(event.PayloadJSON, &candidate); err != nil {
			return corestore.EventRecord{}, regimeDecisionLine{}, false, fmt.Errorf("decode regime decision projection event: %w", err)
		}
		if candidate.SnapshotRevision == revision || event.EventKey == wantKey {
			count++
			found, line = event, candidate
		}
	}
	if count > 1 {
		return corestore.EventRecord{}, regimeDecisionLine{}, false, fmt.Errorf("multiple regime decision events exist at snapshot revision %d", revision)
	}
	return found, line, count == 1, nil
}

func validateRegimeDecisionProjectionEvent(event corestore.EventRecord, line regimeDecisionLine, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) error {
	wantKey := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
	if event.EventKey != wantKey || line.SnapshotRevision != publication.Revision {
		return fmt.Errorf("regime decision event key/revision mismatch at snapshot revision %d", publication.Revision)
	}
	if !event.OccurredAt.Equal(publication.PublishedAt) || !line.TS.Equal(publication.PublishedAt) ||
		!line.SnapshotPublishedAt.Equal(publication.PublishedAt) {
		return fmt.Errorf("regime decision projection publication time mismatch at snapshot revision %d", publication.Revision)
	}
	if line.SnapshotFingerprint != publication.Fingerprint || line.Fingerprint != publication.Fingerprint.Key {
		return fmt.Errorf("regime decision projection fingerprint mismatch at snapshot revision %d", publication.Revision)
	}
	expected := buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication)
	actualJSON, err := json.Marshal(line)
	if err != nil {
		return err
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualJSON, expectedJSON) {
		return fmt.Errorf("regime decision projection content mismatch at snapshot revision %d", publication.Revision)
	}
	return nil
}

func validatePriorRegimeDecisionDisposition(events []corestore.EventRecord, publication regimeSnapshotPublication, disposition string) error {
	event, line, ok, err := findRegimeDecisionProjectionEvent(events, publication.Revision)
	if err != nil {
		return err
	}
	switch disposition {
	case regimeDecisionEventRecorded:
		wantKey := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
		if !ok || event.EventKey != wantKey || line.SnapshotRevision != publication.Revision ||
			!event.OccurredAt.Equal(publication.PublishedAt) || !line.TS.Equal(publication.PublishedAt) ||
			!line.SnapshotPublishedAt.Equal(publication.PublishedAt) || line.SnapshotFingerprint != publication.Fingerprint ||
			line.Fingerprint != publication.Fingerprint.Key {
			return fmt.Errorf("receipted regime decision event is not exact at snapshot revision %d", publication.Revision)
		}
	case regimeDecisionEventDisabled:
		if ok {
			return fmt.Errorf("receipted disabled regime decision event exists at snapshot revision %d", publication.Revision)
		}
	default:
		return fmt.Errorf("receipted regime decision disposition %q is invalid", disposition)
	}
	return nil
}

func (s *Server) loadRegimeDecisionProjectionState(ctx context.Context) (regimeDecisionProjectionState, bool, corestore.StateDocument, error) {
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, regimeDecisionProjectionStateKind)
	if err != nil || !ok {
		return regimeDecisionProjectionState{}, ok, doc, err
	}
	decoder := json.NewDecoder(bytes.NewReader(doc.JSON))
	decoder.DisallowUnknownFields()
	var state regimeDecisionProjectionState
	if err := decoder.Decode(&state); err != nil {
		return regimeDecisionProjectionState{}, false, doc, fmt.Errorf("decode regime decision projection marker: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON")
		}
		return regimeDecisionProjectionState{}, false, doc, fmt.Errorf("decode regime decision projection marker: %w", err)
	}
	publication := regimeDecisionProjectionPublication(state)
	if state.Version != regimeDecisionProjectionStateVersion || validateRegimeSnapshotPublication(publication) != nil || !validRegimeDecisionEventDisposition(state.DecisionEvent) {
		return regimeDecisionProjectionState{}, false, doc, errors.New("decode regime decision projection marker: invalid state")
	}
	return state, true, doc, nil
}

func regimeDecisionProjectionPublication(state regimeDecisionProjectionState) regimeSnapshotPublication {
	return regimeSnapshotPublication{
		Revision: state.SnapshotRevision, PublishedAt: state.SnapshotPublishedAt.UTC(), Fingerprint: state.SnapshotFingerprint,
	}
}

func regimeDecisionProjectionPosition(state regimeDecisionProjectionState, exists bool, plan regimeProjectionPlan) (regimeProjectionPosition, error) {
	if !exists {
		if plan.validateOnly && !plan.legacyReceipt {
			return 0, fmt.Errorf("regime decision projection marker is missing at receipted snapshot revision %d", plan.publication.Revision)
		}
		if plan.previous != nil || plan.initial || plan.legacyReceipt {
			return regimeProjectionInitial, nil
		}
		return 0, fmt.Errorf("regime decision projection marker cannot safely advance to snapshot revision %d", plan.publication.Revision)
	}
	publication := regimeDecisionProjectionPublication(state)
	switch {
	case exactRegimeSnapshotPublication(publication, plan.publication):
		return regimeProjectionCurrent, nil
	case publication.Revision > plan.publication.Revision:
		return 0, fmt.Errorf("regime decision projection marker revision %d is ahead of snapshot revision %d", publication.Revision, plan.publication.Revision)
	case publication.Revision == plan.publication.Revision:
		return 0, fmt.Errorf("regime decision projection marker diverges at snapshot revision %d", plan.publication.Revision)
	case plan.previous != nil && exactRegimeSnapshotPublication(publication, *plan.previous):
		if state.DecisionEvent != plan.previousDecision {
			return 0, fmt.Errorf("regime decision projection marker disposition diverges from receipted revision %d", plan.previous.Revision)
		}
		return regimeProjectionPrevious, nil
	case plan.previous != nil && publication.Revision == plan.previous.Revision:
		return 0, fmt.Errorf("regime decision projection marker diverges from receipted revision %d", plan.previous.Revision)
	default:
		return 0, fmt.Errorf("regime decision projection marker revision %d cannot safely advance to snapshot revision %d", publication.Revision, plan.publication.Revision)
	}
}

func (s *Server) persistRegimeDecisionProjectionState(ctx context.Context, doc corestore.StateDocument, exists bool, publication regimeSnapshotPublication, disposition string) error {
	state := regimeDecisionProjectionState{
		Version: regimeDecisionProjectionStateVersion, SnapshotRevision: publication.Revision,
		SnapshotPublishedAt: publication.PublishedAt.UTC(), SnapshotFingerprint: publication.Fingerprint,
		DecisionEvent: disposition,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode regime decision projection marker: %w", err)
	}
	expectedRevision := int64(0)
	if exists {
		expectedRevision = doc.Revision
	}
	if _, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: regimeDecisionProjectionStateKind,
		ExpectedRevision: expectedRevision, JSON: raw,
	}); err != nil {
		return fmt.Errorf("persist regime decision projection marker: %w", err)
	}
	return nil
}
