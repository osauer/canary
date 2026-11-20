package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

const (
	edgeSessionStampsStateKind = "edge_session_stamps"
	edgeSessionStampsVersion   = 1
)

// edgeSessionStampLog keeps what Canary served when each US session ended:
// Regime's stage, and the Rulebook's premium budget act level in force. Two
// forward tests label a decision with them (option_buys_after_regime_warning
// with the stage of the session before the buy, option_buys_below_premium_line
// with the line of the day), and neither can be worked out afterwards: a
// stage recomputed later follows the vote as it is then and would relabel
// past sessions, and the policy file keeps no history of its values.
//
// A session gets one record, written from the first complete Regime snapshot
// served at or after its official close and before midnight New York, and
// that record is never rewritten. Records are added in session order only,
// so nothing goes back to fill a session that has none: a daemon that was
// not running between a close and midnight leaves that session unrecorded,
// and a reader sees it as missing instead of as a later reconstruction.
//
// Like the forward-test registry the log is not regenerable. It lives
// outside the Edge publication, which every analytical version bump throws
// away, and it is not among the documents a store repair may drop. Regime
// decides the stage and the Rulebook the line; this log only writes down
// what they said, and nothing reads it back into a verdict, a grade, a
// warning or an order.
type edgeSessionStampLog struct {
	Version  int                `json:"version"`
	Sessions []edgeSessionStamp `json:"sessions"`
}

// edgeSessionStamp is one US session's record. Session is the session's New
// York date. Regime's stage rules carry no version number of their own, so
// the record names the build that computed the stage, which identifies them
// exactly, beside the fingerprint scheme and the snapshot revision.
type edgeSessionStamp struct {
	Session                  string    `json:"session"`
	ClosedAt                 time.Time `json:"closed_at"`
	ServedAt                 time.Time `json:"served_at"`
	RegimeStage              string    `json:"regime_stage"`
	SnapshotRevision         int64     `json:"snapshot_revision,omitempty"`
	RegimeFingerprintVersion string    `json:"regime_fingerprint_version,omitempty"`
	DaemonVersion            string    `json:"daemon_version,omitempty"`
	DaemonRevision           string    `json:"daemon_revision,omitempty"`
	// The Rulebook's premium budget act level at that moment, in percent of
	// net liquidation, in the regime set the Rulebook was applying, with the
	// set's name in the Rulebook's own words and the policy's fingerprint.
	PremiumBudgetActPct       *float64  `json:"premium_budget_act_pct,omitempty"`
	PremiumBudgetSet          string    `json:"premium_budget_set,omitempty"`
	RulebookPolicyFingerprint string    `json:"rulebook_policy_fingerprint,omitempty"`
	RecordedAt                time.Time `json:"recorded_at"`
}

// usSessionEndedBy returns the US session of at's New York date when at is
// at or after that session's official close: the half day after Thanksgiving
// ends at 13:00. A day without a session, a moment before the close, and a
// date outside the embedded calendar have none.
func usSessionEndedBy(at time.Time) (marketcal.Session, bool) {
	session, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, at)
	if err != nil || session.Close.IsZero() || at.Before(session.Close) {
		return marketcal.Session{}, false
	}
	if session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose {
		return marketcal.Session{}, false
	}
	return session, true
}

func (s *Server) loadEdgeSessionStamps(ctx context.Context) (edgeSessionStampLog, int64, error) {
	out := edgeSessionStampLog{Version: edgeSessionStampsVersion}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgeSessionStampsStateKind)
	if err != nil || !ok {
		return out, 0, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, 0, fmt.Errorf("decode Edge session stamps: %w", err)
	}
	// A log written by a later build is left alone: rewriting it in this
	// build's shape could drop what that build recorded.
	if out.Version > edgeSessionStampsVersion {
		return out, 0, fmt.Errorf("unsupported future Edge session stamp version %d", out.Version)
	}
	return out, doc.Revision, nil
}

// recordEdgeSessionStamp writes the session's record when view is the first
// snapshot served after its close. It runs beside every Regime read and
// returns nothing: Regime must never wait on, fail with or learn of this
// record. A failed write is retried by the next snapshot of the same evening
// and warned about once per session.
func (s *Server) recordEdgeSessionStamp(ctx context.Context, view regimeSnapshotCacheView) {
	if s == nil || s.coreStore == nil || view.Snapshot == nil || view.Snapshot.AsOf.IsZero() || view.Snapshot.Lifecycle.Stage == "" {
		return
	}
	servedAt := view.Snapshot.AsOf.UTC()
	session, ok := usSessionEndedBy(servedAt)
	if !ok {
		return
	}
	// Settled sessions cost no store read: a snapshot is served every few
	// seconds all evening.
	if settled := s.edgeSessionStamped.Load(); settled != nil && *settled >= session.Date {
		return
	}
	stamp := edgeSessionStamp{
		Session: session.Date, ClosedAt: session.Close.UTC(), ServedAt: servedAt,
		RegimeStage: view.Snapshot.Lifecycle.Stage, SnapshotRevision: view.Revision,
		RegimeFingerprintVersion: view.Fingerprint.Version,
		DaemonVersion:            s.version, DaemonRevision: buildRevision(),
		RecordedAt: s.nowUTC(),
	}
	policy, status := s.activeRulebookPolicy()
	latched, carried := s.rulebookRegimeStage(policy, servedAt)
	_, act, set := policy.PremiumBudgetInForce(latched.Bucket, carried)
	stamp.PremiumBudgetActPct, stamp.PremiumBudgetSet, stamp.RulebookPolicyFingerprint = &act, set, status.Fingerprint.Key
	if err := s.appendEdgeSessionStamp(ctx, stamp); err != nil {
		if warned := s.edgeSessionStampWarned.Swap(&session.Date); warned == nil || *warned != session.Date {
			s.warnf("Canary Edge: the Regime stage of the %s session is not recorded yet: %v", session.Date, err)
		}
		return
	}
	s.edgeSessionStamped.Store(&session.Date)
}

// appendEdgeSessionStamp adds the record unless its session, or a later one,
// already has one. That is the whole rule that keeps a record from being
// rewritten: nothing here replaces an entry, and a clock set back cannot
// write into the past.
func (s *Server) appendEdgeSessionStamp(ctx context.Context, stamp edgeSessionStamp) error {
	for range 4 {
		log, revision, err := s.loadEdgeSessionStamps(ctx)
		if err != nil {
			return err
		}
		if n := len(log.Sessions); n > 0 && log.Sessions[n-1].Session >= stamp.Session {
			return nil
		}
		log.Version = edgeSessionStampsVersion
		log.Sessions = append(log.Sessions, stamp)
		raw, err := json.Marshal(log)
		if err != nil {
			return err
		}
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: edgeSessionStampsStateKind, ExpectedRevision: revision, JSON: raw})
		if err == nil {
			return nil
		}
		// Another reader recorded meanwhile: read again, so its record is
		// kept and nothing is overwritten.
		if _, conflict := err.(*corestore.RevisionConflictError); !conflict {
			return err
		}
	}
	return fmt.Errorf("edge session stamp conflicted repeatedly")
}

// edgeDecisionSessions names the US session of a decision's New York date
// (empty when that day had none) and the session before that date, from the
// official calendar. Outside the calendar's coverage neither is named:
// guessing a weekday would label a holiday as a session.
func edgeDecisionSessions(executedAt time.Time) (prior, own string) {
	cal := marketcal.New()
	day := nyTime(executedAt)
	noon := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, day.Location())
	for back := range 11 {
		session, err := cal.SessionAt(marketcal.MarketUSEquity, noon.AddDate(0, 0, -back))
		if err != nil || session.State == marketcal.StateUnknown {
			return "", ""
		}
		if session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose {
			continue
		}
		if back == 0 {
			own = session.Date
			continue
		}
		return session.Date, own
	}
	return "", own
}

// edgeChangeSessionStamps serves, for a decision executed at executedAt,
// the record of the US session before its New York date and the record of
// its own session. Each is looked up by exact date: a session without a
// record is served as not recorded, never with a neighbour's stage.
func (s *Server) edgeChangeSessionStamps(ctx context.Context, executedAt time.Time) (prior, own *rpc.EdgeSessionStamp) {
	if s == nil || s.coreStore == nil || executedAt.IsZero() {
		return nil, nil
	}
	priorDate, ownDate := edgeDecisionSessions(executedAt)
	if priorDate == "" && ownDate == "" {
		return nil, nil
	}
	log, _, err := s.loadEdgeSessionStamps(ctx)
	if err != nil {
		return nil, nil
	}
	view := func(date string) *rpc.EdgeSessionStamp {
		if date == "" {
			return nil
		}
		for _, stamp := range log.Sessions {
			if stamp.Session != date {
				continue
			}
			closedAt, servedAt := stamp.ClosedAt, stamp.ServedAt
			return &rpc.EdgeSessionStamp{
				Session: date, ClosedAt: &closedAt, ServedAt: &servedAt, RegimeStage: stamp.RegimeStage,
				SnapshotRevision: stamp.SnapshotRevision, RegimeFingerprintVersion: stamp.RegimeFingerprintVersion,
				DaemonVersion: stamp.DaemonVersion, DaemonRevision: stamp.DaemonRevision,
				PremiumBudgetActPct: cloneAmount(stamp.PremiumBudgetActPct), PremiumBudgetSet: stamp.PremiumBudgetSet,
				RulebookPolicyFingerprint: stamp.RulebookPolicyFingerprint,
			}
		}
		return &rpc.EdgeSessionStamp{Session: date, Missing: rpc.EdgeSessionStampNotRecorded}
	}
	return view(priorDate), view(ownDate)
}
