package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

// stressDecisionJournal appends one typed SQLite event per decision-relevant
// dedupe and hourly heartbeat. The path branch and writer lock remain only for
// legacy unit/import oracles.
type stressDecisionJournal struct {
	path string // legacy unit/import helper only
	core *corestore.Store

	mu              sync.Mutex
	lastFingerprint string
	lastWrite       time.Time
}

// legacyStressDecisionsFile is the pre-rename on-disk name of the portfolio-
// The daemon has not appended to this file since the SQLite authority cutover
// Start always does). What remains on an operator's disk is frozen legacy
// evidence that the cutover importer reads once and then seals under this exact
// basename into legacy-sealed/<cutover-id>/.
// reject any rotation intent a crash left pending. recoverLegacyDecisionRotations
// treats an unresolvable intent as a hard startup failure, so the rename would
// brick startup for exactly the operator who crashed mid-rotation. The name is
const legacyStressDecisionsFile = "canary-decisions.jsonl"

func stressDecisionsDefaultPath() (string, error) {
	return defaultTradingStatePath(legacyStressDecisionsFile)
}

const (
	stressDecisionHeartbeat = time.Hour
	// stressEvaluationEvery is the daemon-owned decision cadence. It matches
	stressEvaluationEvery = time.Minute
	// A cold daemon starts the loop before the gateway handshake. Retry the
	stressEvaluationRetryEvery = 5 * time.Second
	// stressJournalEvery remains the five-minute Regime authority window used
	stressJournalEvery = 5 * time.Minute
)

// stressDecisionPolicy is the journal line's policy identity block.
type stressDecisionPolicy struct {
	Policy      string          `json:"policy,omitempty"`
	Profile     string          `json:"profile,omitempty"`
	Version     string          `json:"version,omitempty"`
	Fingerprint rpc.Fingerprint `json:"fingerprint,omitzero"`
}

// stressDecisionLine is the v1 journal record: the stress sensor's decision
type stressDecisionLine struct {
	V                      int                          `json:"v"`
	TS                     time.Time                    `json:"ts"`
	SessionKey             string                       `json:"session_key"`
	Fingerprint            string                       `json:"fingerprint"`
	Account                string                       `json:"account,omitempty"`
	AccountMode            string                       `json:"account_mode,omitempty"`
	Action                 string                       `json:"action,omitempty"`
	Severity               risk.SignalSeverity          `json:"severity"`
	Direction              risk.SignalDirection         `json:"direction,omitempty"`
	MarketConfirmation     string                       `json:"market_confirmation,omitempty"`
	PortfolioFit           string                       `json:"portfolio_fit,omitempty"`
	PortfolioAlertRelevant *bool                        `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string                       `json:"input_health,omitempty"`
	PlannerModeHint        risk.PlannerMode             `json:"planner_mode_hint,omitempty"`
	PlannerReadiness       risk.PlannerReadiness        `json:"planner_readiness,omitempty"`
	Summary                string                       `json:"summary"`
	PrimaryDrivers         []risk.SignalID              `json:"primary_drivers,omitempty"`
	Policy                 stressDecisionPolicy         `json:"policy,omitzero"`
	Market                 rpc.StressMarketSummary      `json:"market"`
	HeldStress             []rpc.HeldStress             `json:"held_stress,omitempty"`
	Rows                   []rpc.StressRow              `json:"rows,omitempty"`
	SourceFingerprints     rpc.StressSourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceAsOf             rpc.StressSourceAsOf         `json:"source_as_of,omitzero"`
	Warnings               []string                     `json:"warnings,omitempty"`
}

func (s *Server) installStressDecisionJournal() {
	path, err := stressDecisionsDefaultPath()
	if err != nil {
		s.logger.Warnf("stress decisions: resolve state path: %v (journal disabled)", err)
		return
	}
	s.stressDecisions = &stressDecisionJournal{path: path}
}

// journalStressDecision appends the stress snapshot when its semantic
// fingerprint changed or the heartbeat interval elapsed. Failures degrade
// to warnings — journaling must never fail a snapshot or brief. Disabled
func (s *Server) journalStressDecision(res *rpc.StressResult) {
	if s == nil || res == nil {
		return
	}
	// Capture the broker authority scope once so the source-neutral alert episode
	// and the legacy calibration journal cannot disagree across a reconnect.
	// persists only the opaque episode key derived from them.
	scope := s.currentBrokerStateScope()
	s.observeStressAlertShadow(res, scope)
	if s.stressDecisions == nil {
		return
	}
	if s.stressJournalEnabled() {
		if err := s.stressDecisions.append(time.Now(), scope.Account, scope.Mode, res); err != nil {
			s.logger.Warnf("stress: decisions journal append failed: %v", err)
		}
	}
}

func (s *Server) stressJournalEnabled() bool {
	return stressJournalEnabledFrom(s.platformSettings.snapshot())
}

// append journals one deduped stress decision. The mutex is held across
// open-per-append writer only while no append is in flight).
func (j *stressDecisionJournal) append(now time.Time, account, accountMode string, res *rpc.StressResult) error {
	if j == nil || res == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	fp := res.Fingerprint.Key
	if fp != "" && fp == j.lastFingerprint && now.Sub(j.lastWrite) < stressDecisionHeartbeat {
		return nil
	}
	line := stressDecisionLine{
		V:                      1,
		TS:                     now,
		SessionKey:             nyTradingSessionKey(nyTime(now)),
		Fingerprint:            fp,
		Account:                account,
		AccountMode:            accountMode,
		Action:                 res.Action,
		Severity:               res.Severity,
		Direction:              res.Direction,
		MarketConfirmation:     res.MarketConfirmation,
		PortfolioFit:           res.PortfolioFit,
		PortfolioAlertRelevant: res.PortfolioAlertRelevant,
		InputHealth:            res.InputHealth,
		PlannerModeHint:        res.PlannerModeHint,
		PlannerReadiness:       res.PlannerReadiness,
		Summary:                res.Summary,
		PrimaryDrivers:         res.PrimaryDrivers,
		Policy: stressDecisionPolicy{
			Policy:      res.Policy,
			Profile:     res.PolicyProfile,
			Version:     res.PolicyVersion,
			Fingerprint: res.PolicyFingerprint,
		},
		Market:             res.Market,
		HeldStress:         res.Portfolio.HeldStress,
		Rows:               res.Rows,
		SourceFingerprints: res.SourceFingerprints,
		SourceAsOf:         res.SourceAsOf,
		Warnings:           res.Warnings,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if j.core != nil {
		key, err := coreStoreEventKey(context.Background(), j.core, coreEventStressDecision, now, b, 0)
		if err != nil {
			return err
		}
		_, err = j.core.AppendEvents(context.Background(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: key, Type: coreEventStressDecision,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: now, PayloadJSON: b,
			Projection: corestore.EventProjection{StressTransition: &corestore.StressTransitionProjection{
				Action: line.Action, Severity: string(line.Severity), Direction: string(line.Direction),
				MarketStage: line.Market.RegimePosture.Stage, InputHealth: line.InputHealth,
				PortfolioAlertRelevant: line.PortfolioAlertRelevant,
			}},
		}})
		if err != nil {
			return err
		}
		j.lastFingerprint, j.lastWrite = fp, now
		return nil
	}
	j.lastFingerprint, j.lastWrite = fp, now
	b = append(b, '\n')
	if err := ensurePrivateStateDir(j.path); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// startStressEvaluationLoop starts the daemon-owned Stress evaluator. The
func (s *Server) startStressEvaluationLoop(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.stressEvaluationLoopWG.Go(func() {
		s.runStressEvaluationLoop(ctx)
	})
}

func (s *Server) runStressEvaluationLoop(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	runStressEvaluationLoopWith(
		ctx,
		s.stressEvaluationWakeChannel(),
		stressEvaluationEvery,
		stressEvaluationRetryEvery,
		s.stressEvaluationTick,
	)
}

type stressEvaluation func(context.Context) bool

// stressEvaluationSourceReader keeps the production tick on the same typed
// this one narrow seam to exercise the real tick without a broker socket.
type stressEvaluationSourceReader interface {
	ready() bool
	account(context.Context) (*rpc.AccountResult, error)
	positions(context.Context) (*rpc.PositionsResult, error)
	regime(context.Context) (*rpc.RegimeSnapshotResult, error)
	marketEvents(context.Context, []string) (*rpc.MarketEventsResult, error)
	now() time.Time
}

type daemonStressEvaluationSourceReader struct {
	server *Server
}

func (r daemonStressEvaluationSourceReader) ready() bool {
	return r.server != nil && r.server.gatewayConnector() != nil
}

func (r daemonStressEvaluationSourceReader) account(ctx context.Context) (*rpc.AccountResult, error) {
	return r.server.buildAccountSummary(ctx, false)
}

func (r daemonStressEvaluationSourceReader) positions(ctx context.Context) (*rpc.PositionsResult, error) {
	return r.server.handlePositionsList(ctx, &rpc.Request{})
}

func (r daemonStressEvaluationSourceReader) regime(ctx context.Context) (*rpc.RegimeSnapshotResult, error) {
	return r.server.briefRegimeSnapshotContext(ctx)
}

func (r daemonStressEvaluationSourceReader) marketEvents(ctx context.Context, symbols []string) (*rpc.MarketEventsResult, error) {
	return r.server.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: symbols})})
}

func (r daemonStressEvaluationSourceReader) now() time.Time {
	return r.server.briefNow()
}

// runStressEvaluationLoopWith keeps the scheduler deterministic in tests. A
// evaluation is in flight; the evaluation always reads the newest authority.
func runStressEvaluationLoopWith(ctx context.Context, wake <-chan struct{}, every, retry time.Duration, evaluate stressEvaluation) {
	if ctx == nil || evaluate == nil || every <= 0 || retry <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		// A timer and a publication may become ready together. One evaluation
		select {
		case <-wake:
		default:
		}
		next := every
		if !evaluate(ctx) {
			next = retry
		}
		timer.Reset(next)
	}
}

// stressEvaluationTick composes and publishes one Stress decision exactly as
// only the optional retained event inside journalStressDecision.
func (s *Server) stressEvaluationTick(ctx context.Context) bool {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	var reader stressEvaluationSourceReader = daemonStressEvaluationSourceReader{server: s}
	if s.stressEvaluationSourceReaderForTest != nil {
		reader = s.stressEvaluationSourceReaderForTest
	}
	if !reader.ready() {
		return false
	}
	regime, err := reader.regime(ctx)
	if err != nil || regime == nil {
		return false // cached snapshot is nil until the first regime poll
	}
	acct, _ := reader.account(ctx)
	pos, _ := reader.positions(ctx)
	pos = s.analysisPositions(pos, reader.now())
	var events *rpc.MarketEventsResult
	if pos != nil {
		events, _ = reader.marketEvents(ctx, marketEventSymbolsFromPositions(pos))
	}
	in := rpc.StressInput{Now: reader.now()}
	if acct != nil {
		in.Account = *acct
	}
	if pos != nil {
		in.Positions = *pos
	}
	in.Regime = *regime
	if events != nil {
		in.MarketEvents = *events
	}
	can := stress.ComputeStress(in)
	s.journalStressDecision(&can)
	return true
}
