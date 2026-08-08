package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimeDecisionJournal appends one typed SQLite event per decision-relevant
// snapshot. It is the forward-collection corpus for threshold calibration;
// the path branch survives only as a unit/import oracle. Events are deduped on
// the semantic fingerprint with an hourly heartbeat for time-in-state data.
type regimeDecisionJournal struct {
	path string // legacy unit/import helper only
	core *corestore.Store

	mu              sync.Mutex
	lastFingerprint string
	lastWrite       time.Time
}

func regimeDecisionsDefaultPath() (string, error) {
	return defaultTradingStatePath("regime-decisions.jsonl")
}

const regimeDecisionHeartbeat = time.Hour

// regimeDecisionLineVersion advances whenever the replay payload's rendered
// contract changes. Version 2 predates the complete per-indicator depth scale
// and later label/replay additions. Versions 3 and 4 carried a shadow scoring
// block, which was removed: 4 is the last version that renders one, so lines
// written under it cannot be reproduced by this renderer. Startup binds every
// older version by publication identity; only the current version is compared
// byte-for-byte, which is what lets a rendered field be added or removed
// without leaving the daemon unable to start.
const regimeDecisionLineVersion = 5

// regimeDecisionLine is the v1 event payload: enough raw measurement,
// gate evidence, and decision output to measure false-alarm and recall
// rates offline and to replay incidents.
type regimeDecisionLine struct {
	V           int       `json:"v"`
	TS          time.Time `json:"ts"`
	SessionKey  string    `json:"session_key"`
	Fingerprint string    `json:"fingerprint"`
	// CurrencyPolicy names the input-currency policy the decision ran under.
	// v1 events predate the marker; a backtest partitions on it rather than
	// blending pre- and post-cutover behaviour.
	CurrencyPolicy string `json:"currency_policy,omitempty"`
	// SnapshotRevision binds a due journal event to the authoritative Regime
	// publication that produced it. Zero is retained only for legacy/import
	// helpers; runtime events use a stable per-revision event key.
	SnapshotRevision int64 `json:"snapshot_revision,omitempty"`
	// Runtime revisions carry the complete authoritative tuple. Fingerprint
	// remains below as the legacy/history-index key, while this typed value
	// preserves both fingerprint version and key.
	SnapshotPublishedAt time.Time       `json:"snapshot_published_at"`
	SnapshotFingerprint rpc.Fingerprint `json:"snapshot_fingerprint"`
	// TapeSession discloses the official-calendar classification the tape
	// terms ran under ("trading_date"/"closed_date"; empty outside embedded
	// coverage), so weekend/holiday journal lines are self-explaining in
	// calibration audits.
	TapeSession string                             `json:"tape_session,omitempty"`
	Stage       string                             `json:"stage"`
	Severity    string                             `json:"severity"`
	Readiness   string                             `json:"readiness"`
	Confidence  string                             `json:"confidence"`
	Verdict     string                             `json:"verdict"`
	ConfirmedBy []string                           `json:"confirmed_by,omitempty"`
	Unconfirmed []string                           `json:"unconfirmed,omitempty"`
	Governors   []rpc.GovernorAction               `json:"governors,omitempty"`
	Composite   rpc.RegimeComposite                `json:"composite"`
	Indicators  map[string]regimeDecisionIndicator `json:"indicators"`
	DataQuality []rpc.DataQualityHealth            `json:"data_quality,omitempty"`
}

type regimeDecisionIndicator struct {
	Status          string   `json:"status,omitempty"`
	Band            string   `json:"band,omitempty"`
	Value           *float64 `json:"value,omitempty"`
	Depth           *float64 `json:"depth,omitempty"`
	StreakSessions  int      `json:"streak_sessions,omitempty"`
	Freshness       string   `json:"freshness,omitempty"`
	Eligible        *bool    `json:"eligible,omitempty"`
	Latched         bool     `json:"latched,omitempty"`
	ThresholdsLabel string   `json:"thresholds_label,omitempty"`
}

func (s *Server) journalRegimeDecisionPublicationContext(ctx context.Context, res *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) error {
	if s == nil || s.regimeDecisions == nil || res == nil {
		return nil
	}
	if !s.regimeJournalEnabled() {
		return nil
	}
	when := publication.PublishedAt.UTC()
	if when.IsZero() {
		when = time.Now().UTC()
	}
	if err := s.regimeDecisions.appendPublicationContext(ctx, when, res, publication); err != nil {
		s.logger.Warnf("regime: decisions journal append failed: %v", err)
		return err
	}
	return nil
}

func (s *Server) regimeJournalEnabled() bool {
	return regimeJournalEnabledFrom(s.platformSettings.snapshot())
}

// append journals one deduped regime decision. Since phase 2 the mutex is
// held across marshal, directory ensure, open, write, and close — the
// writer-quiescence contract journal rotation relies on (a live-file
// rename is invisible to an open-per-append writer only while no append
// is in flight).
func (j *regimeDecisionJournal) appendPublicationContext(ctx context.Context, now time.Time, res *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) error {
	if j == nil || res == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if publication.Revision > 0 {
		publication.PublishedAt = publication.PublishedAt.UTC()
		if err := validateRegimeSnapshotPublication(publication); err != nil {
			return err
		}
		if res.Fingerprint != publication.Fingerprint {
			return fmt.Errorf("regime decision snapshot fingerprint does not match publication revision %d", publication.Revision)
		}
		now = publication.PublishedAt
	}
	fp := res.Fingerprint.Key
	// Authoritative publications reach this writer only after the durable
	// projection marker selected the recorded disposition. They use a
	// revision-stable event key; equal-fingerprint skips are represented by
	// that marker and never enter this append path. Legacy/import callers have
	// no publication identity, so they retain process-local heartbeat dedupe.
	if publication.Revision == 0 && fp != "" && fp == j.lastFingerprint && now.Sub(j.lastWrite) < regimeDecisionHeartbeat {
		return nil
	}
	line := buildRegimeDecisionLine(now, res, publication)
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if j.core != nil {
		indicators := make([]corestore.RegimeIndicatorProjection, 0, len(line.Indicators))
		for _, indicator := range streakIndicators {
			key := indicator.key()
			value, ok := line.Indicators[key]
			if !ok {
				continue
			}
			var streak *int64
			if value.StreakSessions != 0 {
				v := int64(value.StreakSessions)
				streak = &v
			}
			indicators = append(indicators, corestore.RegimeIndicatorProjection{
				Indicator: key, Status: value.Status, Band: value.Band,
				Value: value.Value, Depth: value.Depth, StreakSessions: streak,
				Freshness: value.Freshness, Eligible: value.Eligible,
				Latched: value.Latched, ThresholdsLabel: value.ThresholdsLabel,
			})
		}
		key := ""
		if publication.Revision > 0 {
			key = fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
		} else {
			var err error
			key, err = coreStoreEventKey(ctx, j.core, coreEventRegimeDecision, now, b, 0)
			if err != nil {
				return err
			}
		}
		projection := corestore.EventProjection{}
		if line.Stage != "" {
			projection.RegimeDecision = &corestore.RegimeDecisionProjection{
				DecisionKey: key, Stage: line.Stage, Severity: line.Severity,
				Readiness: line.Readiness, Confidence: line.Confidence,
				Verdict: line.Verdict, Fingerprint: line.Fingerprint, Indicators: indicators,
			}
		}
		_, err = j.core.AppendEvents(ctx, []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRegimeDecision,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: now, PayloadJSON: b, Projection: projection,
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

func buildRegimeDecisionLine(now time.Time, res *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) regimeDecisionLine {
	line := regimeDecisionLine{
		V:                regimeDecisionLineVersion,
		CurrencyPolicy:   rpc.RegimeCurrencyPolicyVersion,
		TS:               now,
		SessionKey:       nyTradingSessionKey(nyTime(now)),
		Fingerprint:      res.Fingerprint.Key,
		SnapshotRevision: publication.Revision,
		TapeSession:      res.TapeSessionState,
		Stage:            res.Lifecycle.Stage,
		Severity:         res.Lifecycle.Severity,
		Readiness:        res.Lifecycle.Readiness,
		Confidence:       res.Lifecycle.Confidence,
		Verdict:          res.Composite.Verdict,
		ConfirmedBy:      res.Lifecycle.ConfirmedBy,
		Unconfirmed:      res.Lifecycle.Unconfirmed,
		Governors:        res.Lifecycle.Governors,
		Composite:        res.Composite,
		Indicators:       regimeDecisionIndicators(res),
		DataQuality:      res.DataQuality,
	}
	if publication.Revision > 0 {
		line.SnapshotPublishedAt = publication.PublishedAt.UTC()
		line.SnapshotFingerprint = publication.Fingerprint
	}
	return line
}

func regimeDecisionIndicators(res *rpc.RegimeSnapshotResult) map[string]regimeDecisionIndicator {
	out := make(map[string]regimeDecisionIndicator, len(streakIndicators))
	for _, ind := range streakIndicators {
		key := ind.key()
		_, value := ind.bandAndValue(res)
		status, meta, streak := regimeDecisionRowView(res, key)
		entry := regimeDecisionIndicator{
			Status: status,
			Band:   meta.Band,
			Depth:  ind.depth(res),
		}
		if meta.Band != "" && meta.Band != "unranked" {
			v := value
			entry.Value = &v
		}
		if streak != nil {
			entry.StreakSessions = streak.Sessions
		}
		if meta.Freshness != nil {
			entry.Freshness = meta.Freshness.Class
		}
		if meta.Eligibility != nil {
			e := meta.Eligibility.Eligible
			entry.Eligible = &e
			entry.Latched = meta.Eligibility.Latched
		}
		if meta.Thresholds != nil {
			entry.ThresholdsLabel = meta.Thresholds.Label
		}
		out[key] = entry
	}
	return out
}

func regimeDecisionRowView(res *rpc.RegimeSnapshotResult, key string) (string, rpc.RegimeIndicatorMeta, *rpc.StreakInfo) {
	switch key {
	case StreakKeyVIXTerm:
		return res.VIXTermStructure.Status, res.VIXTermStructure.RegimeIndicatorMeta, res.VIXTermStructure.Streak
	case StreakKeyVolOfVol:
		return res.VolOfVol.Status, res.VolOfVol.RegimeIndicatorMeta, res.VolOfVol.Streak
	case StreakKeyHYGSPY:
		return res.HYGSPYDivergence.Status, res.HYGSPYDivergence.RegimeIndicatorMeta, res.HYGSPYDivergence.Streak
	case StreakKeyCredit:
		return res.CreditSpreads.Status, res.CreditSpreads.RegimeIndicatorMeta, res.CreditSpreads.Streak
	case StreakKeyFunding:
		return res.FundingStress.Status, res.FundingStress.RegimeIndicatorMeta, res.FundingStress.Streak
	case StreakKeyUSDJPY:
		return res.USDJPY.Status, res.USDJPY.RegimeIndicatorMeta, res.USDJPY.Streak
	case StreakKeyGammaZero:
		return res.GammaZero.Status, res.GammaZero.RegimeIndicatorMeta, res.GammaZero.Streak
	case StreakKeyBreadth:
		return res.Breadth.Status, res.Breadth.RegimeIndicatorMeta, res.Breadth.Streak
	default:
		return "", rpc.RegimeIndicatorMeta{}, nil
	}
}
