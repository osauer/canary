package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// The rulebook's regime-conditional thresholds (rules 3/4/12) consume a
// bucketed regime lifecycle stage. The daemon latches the bucket whenever a
// regime snapshot completes and persists it, so a restart mid-stress cannot
// silently reset thresholds to calm. A stale latch is served as "carried" —
// the evaluator then applies worse-of(carried, calm) semantics — and the
// rules path kicks one async regime refresh to re-freshen it (single-flight
// with cooldown; never from the preview path, never synchronously).

const (
	rulesRegimeStageFile     = "rules-regime-stage.json"
	rulesRegimeKickCooldown  = 10 * time.Minute
	rulesRegimeKickTimeout   = 5 * time.Minute
	rulesRegimeStageStateVer = 1
)

type rulesRegimeStageState struct {
	Version int       `json:"version"`
	Bucket  string    `json:"bucket"`
	Stage   string    `json:"stage"`
	AsOf    time.Time `json:"as_of"`

	// The exact snapshot identity and prior receipted latch deliberately stay
	// unexported so ordinary rule evaluation cannot confuse publication time
	// with stage age. Custom JSON methods make both durable. A newly staged
	// projection retains the prior latch until the all-projections receipt is
	// committed and the in-memory publish barrier opens.
	publication     regimeSnapshotPublication
	previousPresent bool
	previousBucket  string
	previousStage   string
	previousAsOf    time.Time
	previous        regimeSnapshotPublication
}

type rulesRegimeStageStateJSON struct {
	Version                     int             `json:"version"`
	Bucket                      string          `json:"bucket"`
	Stage                       string          `json:"stage"`
	AsOf                        time.Time       `json:"as_of"`
	SnapshotRevision            int64           `json:"snapshot_revision,omitempty"`
	SnapshotPublishedAt         time.Time       `json:"snapshot_published_at"`
	SnapshotFingerprint         rpc.Fingerprint `json:"snapshot_fingerprint"`
	PreviousPresent             bool            `json:"previous_present,omitempty"`
	PreviousBucket              string          `json:"previous_bucket,omitempty"`
	PreviousStage               string          `json:"previous_stage,omitempty"`
	PreviousAsOf                time.Time       `json:"previous_as_of"`
	PreviousSnapshotRevision    int64           `json:"previous_snapshot_revision,omitempty"`
	PreviousSnapshotPublishedAt time.Time       `json:"previous_snapshot_published_at"`
	PreviousSnapshotFingerprint rpc.Fingerprint `json:"previous_snapshot_fingerprint"`
}

func (state rulesRegimeStageState) MarshalJSON() ([]byte, error) {
	return json.Marshal(rulesRegimeStageStateJSON{
		Version: state.Version, Bucket: state.Bucket, Stage: state.Stage, AsOf: state.AsOf,
		SnapshotRevision: state.publication.Revision, SnapshotPublishedAt: state.publication.PublishedAt,
		SnapshotFingerprint: state.publication.Fingerprint, PreviousPresent: state.previousPresent,
		PreviousBucket: state.previousBucket, PreviousStage: state.previousStage, PreviousAsOf: state.previousAsOf,
		PreviousSnapshotRevision: state.previous.Revision, PreviousSnapshotPublishedAt: state.previous.PublishedAt,
		PreviousSnapshotFingerprint: state.previous.Fingerprint,
	})
}

func (state *rulesRegimeStageState) UnmarshalJSON(raw []byte) error {
	if state == nil {
		return errors.New("decode rules regime stage into nil state")
	}
	var wire rulesRegimeStageStateJSON
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	*state = rulesRegimeStageState{
		Version: wire.Version, Bucket: wire.Bucket, Stage: wire.Stage, AsOf: wire.AsOf.UTC(),
		publication: regimeSnapshotPublication{
			Revision: wire.SnapshotRevision, PublishedAt: wire.SnapshotPublishedAt.UTC(), Fingerprint: wire.SnapshotFingerprint,
		},
		previousPresent: wire.PreviousPresent, previousBucket: wire.PreviousBucket,
		previousStage: wire.PreviousStage, previousAsOf: wire.PreviousAsOf.UTC(),
		previous: regimeSnapshotPublication{
			Revision: wire.PreviousSnapshotRevision, PublishedAt: wire.PreviousSnapshotPublishedAt.UTC(),
			Fingerprint: wire.PreviousSnapshotFingerprint,
		},
	}
	return nil
}

func decodeRulesRegimeStageState(raw []byte) (rulesRegimeStageState, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state rulesRegimeStageStateJSON
	if err := decoder.Decode(&state); err != nil {
		return rulesRegimeStageState{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return rulesRegimeStageState{}, errors.New("rules regime stage has trailing JSON")
		}
		return rulesRegimeStageState{}, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return rulesRegimeStageState{}, err
	}
	var decoded rulesRegimeStageState
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return rulesRegimeStageState{}, err
	}
	if !hasAnyRegimeSnapshotPublicationIdentity(decoded.publication) && decoded.Stage != "" {
		decoded.Bucket = bucketRegimeStage(decoded.Stage)
	}
	if err := validateRulesRegimeStageState(decoded); err != nil {
		return rulesRegimeStageState{}, err
	}
	return decoded, nil
}

func validateRulesRegimeStageState(state rulesRegimeStageState) error {
	if state.Version != rulesRegimeStageStateVer {
		return fmt.Errorf("unsupported version %d", state.Version)
	}
	if state.Stage == "" {
		if state.Bucket != "" {
			return errors.New("rules regime stage has a bucket without a stage")
		}
	} else if bucket := bucketRegimeStage(state.Stage); bucket == "" || state.Bucket != bucket {
		return fmt.Errorf("rules regime stage %q has invalid bucket %q", state.Stage, state.Bucket)
	}
	if hasAnyRegimeSnapshotPublicationIdentity(state.publication) {
		if err := validateRegimeSnapshotPublication(state.publication); err != nil {
			return fmt.Errorf("rules regime stage publication: %w", err)
		}
	}
	if !state.previousPresent {
		if state.previousBucket != "" || state.previousStage != "" || !state.previousAsOf.IsZero() ||
			hasAnyRegimeSnapshotPublicationIdentity(state.previous) {
			return errors.New("rules regime stage has predecessor data without previous_present")
		}
		return nil
	}
	if state.previousStage == "" {
		if state.previousBucket != "" {
			return errors.New("rules regime stage predecessor has a bucket without a stage")
		}
	} else if bucket := bucketRegimeStage(state.previousStage); bucket == "" || state.previousBucket != bucket {
		return fmt.Errorf("rules regime stage predecessor %q has invalid bucket %q", state.previousStage, state.previousBucket)
	}
	if hasAnyRegimeSnapshotPublicationIdentity(state.previous) {
		if err := validateRegimeSnapshotPublication(state.previous); err != nil {
			return fmt.Errorf("rules regime stage predecessor publication: %w", err)
		}
		if state.publication.Revision > 0 && state.previous.Revision != state.publication.Revision-1 {
			return fmt.Errorf("rules regime stage predecessor revision %d does not precede revision %d", state.previous.Revision, state.publication.Revision)
		}
	} else if state.publication.Revision != 1 {
		return errors.New("rules regime stage predecessor publication is missing")
	}
	return nil
}

func (state rulesRegimeStageState) withoutProjectionHistory() rulesRegimeStageState {
	state.previousPresent = false
	state.previousBucket = ""
	state.previousStage = ""
	state.previousAsOf = time.Time{}
	state.previous = regimeSnapshotPublication{}
	return state
}

func (state rulesRegimeStageState) previousState() rulesRegimeStageState {
	return rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Bucket: state.previousBucket,
		Stage: state.previousStage, AsOf: state.previousAsOf, publication: state.previous,
	}
}

func equalRulesRegimeStageState(left, right rulesRegimeStageState) bool {
	return left.Version == right.Version && left.Bucket == right.Bucket && left.Stage == right.Stage &&
		left.AsOf.Equal(right.AsOf) && exactRegimeSnapshotPublication(left.publication, right.publication) &&
		left.previousPresent == right.previousPresent && left.previousBucket == right.previousBucket &&
		left.previousStage == right.previousStage && left.previousAsOf.Equal(right.previousAsOf) &&
		exactRegimeSnapshotPublication(left.previous, right.previous)
}

func failClosedRulesRegimeStageState() rulesRegimeStageState {
	return rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketConfirmed,
		Stage: "projection_pending_or_clock_invalid", AsOf: time.Time{},
	}
}

func (s *Server) bindRulesRegimeStage(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return fmt.Errorf("rules regime stage SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage)
	if err != nil {
		return fmt.Errorf("load rules regime stage from SQLite: %w", err)
	}
	state := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	if ok {
		state, err = decodeRulesRegimeStageState(doc.JSON)
		if err != nil {
			return fmt.Errorf("decode rules regime stage from SQLite: %w", err)
		}
	} else {
		return fmt.Errorf("rules regime stage is missing from SQLite; cutover bootstrap was not completed")
	}
	// Binding precedes snapshot reconciliation and socket publication. Expose
	// only a latch already covered by the durable receipt; a staged successor
	// stays behind its prior receipted latch until reconciliation validates all
	// projections. Any ambiguous state is explicit fail-closed for internal
	// startup callers.
	visible := failClosedRulesRegimeStageState()
	receipt, receiptOK, receiptErr := s.loadRegimeProjectionReceipt(ctx)
	if receiptErr != nil {
		return fmt.Errorf("load rules regime stage receipt: %w", receiptErr)
	}
	if receiptOK {
		publication := publicationFromRegimeProjectionReceipt(receipt)
		switch {
		case exactRegimeSnapshotPublication(state.publication, publication):
			visible = state.withoutProjectionHistory()
		case state.previousPresent && exactRegimeSnapshotPublication(state.previous, publication):
			visible = state.previousState()
		}
	} else if !hasAnyRegimeSnapshotPublicationIdentity(state.publication) && state.Stage == "" {
		visible = rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	}
	s.rulesRegimeStageMu.Lock()
	s.rulesRegimeStage = visible
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()
	return nil
}

// bucketRegimeStage maps a lifecycle stage onto the rulebook's three
// threshold buckets. data_quality (and empty) return "" — hold the previous
// latch, a data-quality stage is an input problem, not a market read. An
// unrecognized future stage maps to the MIDDLE bucket, never silently to
// calm.
func bucketRegimeStage(stage string) string {
	switch stage {
	case rpc.LifecycleQuiet, rpc.LifecycleOpportunity:
		return risk.RegimeBucketCalm
	case rpc.LifecycleEarlyWarning, rpc.LifecycleStabilization:
		return risk.RegimeBucketEarlyWarning
	case rpc.LifecycleConfirmedStress, rpc.LifecyclePanic:
		return risk.RegimeBucketConfirmed
	case "", rpc.LifecycleDataQuality:
		return ""
	default:
		return risk.RegimeBucketEarlyWarning
	}
}

// latchRulesRegimeStage records the snapshot's lifecycle stage for the
// rulebook and persists it. Called from handleRegimeSnapshot's success path.
func (s *Server) latchRulesRegimeStage(res *rpc.RegimeSnapshotResult) {
	_ = s.latchRulesRegimeStageContext(context.Background(), res)
}

func (s *Server) latchRulesRegimeStageContext(ctx context.Context, res *rpc.RegimeSnapshotResult) error {
	return s.projectRulesRegimeStageAt(ctx, res, time.Now().UTC())
}

func (s *Server) projectRulesRegimeStageAt(ctx context.Context, res *rpc.RegimeSnapshotResult, projectedAt time.Time) error {
	if res == nil {
		return nil
	}
	if res.TapeSessionState == rpc.TapeSessionClosedDate {
		// Closed-date snapshots hold the previous latch (Oliver, 2026-07-19,
		// closed-date tape-gating pass): with tape terms gated, a
		// weekend/holiday stage is cluster-only and must neither re-freshen
		// nor relax the rulebook bucket. The last trading-date stage instead
		// ages past RegimeStageMaxAgeMinutes into the existing carried
		// worse-of(carried, calm) path — stale regime data can hold or
		// tighten a verdict but never relax it — and the first live snapshot
		// at the next open re-latches fresh.
		return nil
	}
	bucket := bucketRegimeStage(res.Lifecycle.Stage)
	if bucket == "" {
		return nil // hold the previous latch
	}
	st := rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: bucket, Stage: res.Lifecycle.Stage, AsOf: projectedAt.UTC()}
	s.rulesRegimeStageMu.Lock()
	if s.rulesRegimeStage == st {
		s.rulesRegimeStageMu.Unlock()
		return nil
	}
	if s.coreStore != nil {
		doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage)
		if err != nil || !ok {
			s.rulesRegimeStageMu.Unlock()
			s.warnf("rules regime stage: SQLite authority read failed: %v", err)
			if err != nil {
				return fmt.Errorf("read rules regime stage projection: %w", err)
			}
			return fmt.Errorf("read rules regime stage projection: state is missing")
		}
		raw, err := json.Marshal(st)
		if err == nil {
			_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
				ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
		}
		if err != nil {
			s.rulesRegimeStageMu.Unlock()
			s.warnf("rules regime stage: SQLite authority write failed: %v", err)
			return fmt.Errorf("write rules regime stage projection: %w", err)
		}
		s.rulesRegimeStage = st
		s.rulesRegimeStageLoaded = true
		s.rulesRegimeStageMu.Unlock()
		return nil
	}
	// Legacy unit/import helper only.
	s.rulesRegimeStage = st
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()
	if path, err := defaultTradingStatePath(rulesRegimeStageFile); err == nil {
		if data, err := json.Marshal(st); err == nil {
			_ = writePrivateStateAtomic(path, data)
		}
	}
	return nil
}

// rulesRegimeStageSnapshot returns the latched bucket, lazily restoring the
// persisted state on first use. Zero state means "never observed".
func (s *Server) rulesRegimeStageSnapshot() rulesRegimeStageState {
	s.rulesRegimeStageMu.Lock()
	defer s.rulesRegimeStageMu.Unlock()
	if !s.rulesRegimeStageLoaded {
		s.rulesRegimeStageLoaded = true
		// A started daemon binds this latch eagerly from SQLite. Reaching the
		// lazy path is confined to legacy unit/import helpers.
		if path, err := defaultTradingStatePath(rulesRegimeStageFile); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				var st rulesRegimeStageState
				if json.Unmarshal(data, &st) == nil && st.Version == rulesRegimeStageStateVer && bucketRegimeStage(st.Stage) != "" {
					// Never trust the stored bucket: a skewed or hand-edited
					// file must not serve calm for a panic stage.
					st.Bucket = bucketRegimeStage(st.Stage)
					s.rulesRegimeStage = st
				}
			}
		}
	}
	state := s.rulesRegimeStage
	now := time.Now().UTC()
	if (!state.AsOf.IsZero() && state.AsOf.After(now)) ||
		(!state.publication.PublishedAt.IsZero() && state.publication.PublishedAt.After(now)) {
		return failClosedRulesRegimeStageState()
	}
	return state
}

// kickRulesRegimeStageRefresh schedules one background regime snapshot so a
// cold or stale stage latch self-heals after one rules evaluation.
// Single-flight with a cooldown: rules calls must never stampede the
// expensive regime fanout.
func (s *Server) kickRulesRegimeStageRefresh(ctx context.Context) {
	s.rulesRegimeStageMu.Lock()
	tooSoon := time.Since(s.rulesRegimeKickAt) < rulesRegimeKickCooldown
	if !tooSoon {
		s.rulesRegimeKickAt = time.Now()
	}
	s.rulesRegimeStageMu.Unlock()
	if tooSoon || !s.rulesRegimeKickBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.rulesRegimeKickBusy.Store(false)
		// Production refreshes belong to the daemon lifetime, never to the
		// short-lived rules request. The detached fallback is confined to
		// legacy unit fixtures that construct Server without Start.
		s.mu.Lock()
		parent := s.serverCtx
		s.mu.Unlock()
		if parent == nil {
			parent = context.WithoutCancel(ctx)
		}
		kctx, cancel := context.WithTimeout(parent, rulesRegimeKickTimeout)
		defer cancel()
		// The latch update happens inside handleRegimeSnapshot on success.
		_, _ = s.handleRegimeSnapshot(kctx, &rpc.Request{})
	}()
}
