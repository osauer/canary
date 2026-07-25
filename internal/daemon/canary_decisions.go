package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
	"github.com/osauer/ibkr/v2/internal/stress"
)

// canaryDecisionJournal appends one typed SQLite event per decision-relevant
// portfolio-canary snapshot. It mirrors regimeDecisionJournal's fingerprint
// dedupe and hourly heartbeat. The path branch and writer lock remain only for
// legacy unit/import oracles.
type canaryDecisionJournal struct {
	path string // legacy unit/import helper only
	core *corestore.Store

	mu              sync.Mutex
	lastFingerprint string
	lastWrite       time.Time
}

func canaryDecisionsDefaultPath() (string, error) {
	return defaultTradingStatePath("canary-decisions.jsonl")
}

const (
	canaryDecisionHeartbeat = time.Hour
	// canaryEvaluationEvery is the daemon-owned decision cadence. It matches
	// the established app refresh without depending on an app process being
	// present. Journaling is a retention choice and cannot stop evaluation.
	canaryEvaluationEvery = time.Minute
	// A cold daemon starts the loop before the gateway handshake. Retry the
	// cheap prerequisite check promptly; once an evaluation is attempted, the
	// normal minute cadence resumes even if some inputs are degraded.
	canaryEvaluationRetryEvery = 5 * time.Second
	// canaryJournalEvery remains the five-minute Regime authority window used
	// by regimeSnapshotFreshFor. Canary evaluation no longer runs on it.
	canaryJournalEvery = 5 * time.Minute
)

// canaryDecisionPolicy is the journal line's policy identity block.
type canaryDecisionPolicy struct {
	Policy      string          `json:"policy,omitempty"`
	Profile     string          `json:"profile,omitempty"`
	Version     string          `json:"version,omitempty"`
	Fingerprint rpc.Fingerprint `json:"fingerprint,omitzero"`
}

// canaryDecisionLine is the v1 journal record: the canary's decision
// output, its market/portfolio evidence, and the classified upstream
// fingerprints — enough to replay an alert decision offline.
type canaryDecisionLine struct {
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
	Policy                 canaryDecisionPolicy         `json:"policy,omitzero"`
	Market                 rpc.StressMarketSummary      `json:"market"`
	HeldStress             []rpc.HeldStress             `json:"held_stress,omitempty"`
	Rows                   []rpc.StressRow              `json:"rows,omitempty"`
	SourceFingerprints     rpc.StressSourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceAsOf             rpc.StressSourceAsOf         `json:"source_as_of,omitzero"`
	Warnings               []string                     `json:"warnings,omitempty"`
}

func (s *Server) installCanaryDecisionJournal() {
	path, err := canaryDecisionsDefaultPath()
	if err != nil {
		s.logger.Warnf("canary decisions: resolve state path: %v (journal disabled)", err)
		return
	}
	s.canaryDecisions = &canaryDecisionJournal{path: path}
}

// journalCanaryDecision appends the canary snapshot when its semantic
// fingerprint changed or the heartbeat interval elapsed. Failures degrade
// to warnings — journaling must never fail a snapshot or brief. Disabled
// via `ibkr settings set canary.journal.enabled=false`. Always ends with
// the data-free history-index kick.
func (s *Server) journalCanaryDecision(res *rpc.StressResult) {
	if s == nil || res == nil {
		return
	}
	// Capture the broker authority scope once so the source-neutral alert episode
	// and the legacy calibration journal cannot disagree across a reconnect.
	// Raw scope parts remain inside their owning adapters; the alert registry
	// persists only the opaque episode key derived from them.
	scope := s.currentBrokerStateScope()
	s.observeCanaryAlertShadow(res, scope)
	if s.canaryDecisions == nil {
		return
	}
	if s.canaryJournalEnabled() {
		if err := s.canaryDecisions.append(time.Now(), scope.Account, scope.Mode, res); err != nil {
			s.logger.Warnf("canary: decisions journal append failed: %v", err)
		}
	}
	// Wake the history-index ingester unconditionally (not gated on the
	// append outcome): the kick carries no data, only "look at the file".
	s.kickHistoryIndex()
}

func (s *Server) canaryJournalEnabled() bool {
	if s.platformSettings == nil {
		return true
	}
	data := s.platformSettings.snapshot()
	if data.Canary.Journal.Enabled == nil {
		return true
	}
	return *data.Canary.Journal.Enabled
}

// append journals one deduped canary decision. The mutex is held across
// marshal, directory ensure, open, write, and close — the writer-quiescence
// contract rotation relies on (a live-file rename is invisible to an
// open-per-append writer only while no append is in flight).
func (j *canaryDecisionJournal) append(now time.Time, account, accountMode string, res *rpc.StressResult) error {
	if j == nil || res == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	fp := res.Fingerprint.Key
	if fp != "" && fp == j.lastFingerprint && now.Sub(j.lastWrite) < canaryDecisionHeartbeat {
		return nil
	}
	line := canaryDecisionLine{
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
		Policy: canaryDecisionPolicy{
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
		key, err := coreStoreEventKey(context.Background(), j.core, coreEventCanaryDecision, now, b, 0)
		if err != nil {
			return err
		}
		_, err = j.core.AppendEvents(context.Background(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: key, Type: coreEventCanaryDecision,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: now, PayloadJSON: b,
			Projection: corestore.EventProjection{CanaryTransition: &corestore.CanaryTransitionProjection{
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

// startCanaryEvaluationLoop starts the daemon-owned Canary evaluator. The
// immediate first attempt removes the former five-minute startup blind spot;
// gateway connection and each new Regime publication also wake the loop.
func (s *Server) startCanaryEvaluationLoop(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.canaryEvaluationLoopWG.Go(func() {
		s.runCanaryEvaluationLoop(ctx)
	})
}

func (s *Server) runCanaryEvaluationLoop(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	runCanaryEvaluationLoopWith(
		ctx,
		s.canaryEvaluationWakeChannel(),
		canaryEvaluationEvery,
		canaryEvaluationRetryEvery,
		s.canaryEvaluationTick,
	)
}

type canaryEvaluation func(context.Context) bool

// canaryEvaluationSourceReader keeps the production tick on the same typed
// daemon RPC builders used by request-driven Canary recomputation. Tests swap
// this one narrow seam to exercise the real tick without a broker socket.
type canaryEvaluationSourceReader interface {
	ready() bool
	account(context.Context) (*rpc.AccountResult, error)
	positions(context.Context) (*rpc.PositionsResult, error)
	regime(context.Context) (*rpc.RegimeSnapshotResult, error)
	marketEvents(context.Context, []string) (*rpc.MarketEventsResult, error)
	now() time.Time
}

type daemonCanaryEvaluationSourceReader struct {
	server *Server
}

func (r daemonCanaryEvaluationSourceReader) ready() bool {
	return r.server != nil && r.server.gatewayConnector() != nil
}

func (r daemonCanaryEvaluationSourceReader) account(ctx context.Context) (*rpc.AccountResult, error) {
	return r.server.buildAccountSummary(ctx, false)
}

func (r daemonCanaryEvaluationSourceReader) positions(ctx context.Context) (*rpc.PositionsResult, error) {
	return r.server.handlePositionsList(ctx, &rpc.Request{})
}

func (r daemonCanaryEvaluationSourceReader) regime(ctx context.Context) (*rpc.RegimeSnapshotResult, error) {
	return r.server.briefRegimeSnapshotContext(ctx)
}

func (r daemonCanaryEvaluationSourceReader) marketEvents(ctx context.Context, symbols []string) (*rpc.MarketEventsResult, error) {
	return r.server.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: symbols})})
}

func (r daemonCanaryEvaluationSourceReader) now() time.Time {
	return r.server.briefNow()
}

// runCanaryEvaluationLoopWith keeps the scheduler deterministic in tests. A
// capacity-one wake channel coalesces repeated publications while an
// evaluation is in flight; the evaluation always reads the newest authority.
func runCanaryEvaluationLoopWith(ctx context.Context, wake <-chan struct{}, every, retry time.Duration, evaluate canaryEvaluation) {
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
		// covers both because it reads the latest immutable Regime publication.
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

// canaryEvaluationTick composes and publishes one Canary decision exactly as
// composeBrief does. The journal setting is intentionally absent: it controls
// only the optional retained event inside journalCanaryDecision.
func (s *Server) canaryEvaluationTick(ctx context.Context) bool {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	var reader canaryEvaluationSourceReader = daemonCanaryEvaluationSourceReader{server: s}
	if s.canaryEvaluationSourceReaderForTest != nil {
		reader = s.canaryEvaluationSourceReaderForTest
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
	s.journalCanaryDecision(&can)
	return true
}

// canaryJournalTick remains as a narrow compatibility seam for legacy tests.
// It now performs an evaluation; the journal setting controls retention only.
func (s *Server) canaryJournalTick(ctx context.Context) {
	s.canaryEvaluationTick(ctx)
}
