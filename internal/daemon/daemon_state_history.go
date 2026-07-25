package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// The helpers in this file are the typed SQLite replacements for history.db
// query calls. They return newest-first rows and the pre-limit total.

func (s *Server) sqliteRegimeHistory(ctx context.Context, since, until time.Time, stage string, limit int) ([]rpc.RegimeHistoryEntry, int, error) {
	events, err := s.sqliteHistoryEvents(ctx, coreEventRegimeDecision, since, until)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]rpc.RegimeHistoryEntry, 0, len(events))
	for _, event := range events {
		var line regimeDecisionLine
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			return nil, 0, fmt.Errorf("decode regime decision event %d: %w", event.EventSeq, err)
		}
		if stage != "" && line.Stage != stage {
			continue
		}
		entries = append(entries, rpc.RegimeHistoryEntry{
			At: line.TS, SessionKey: line.SessionKey, TapeSession: line.TapeSession,
			Stage: line.Stage, Severity: line.Severity, Readiness: line.Readiness,
			Confidence: line.Confidence, Verdict: line.Verdict,
			ClusterRed:         line.Composite.ClusterRedCount,
			ClusterYellow:      line.Composite.ClusterYellowCount,
			ClusterEligibleRed: line.Composite.ClusterEligibleRedCount,
			Fingerprint:        line.Fingerprint,
		})
	}
	total := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, total, nil
}

type sqliteRuleTransitionLine struct {
	At                time.Time `json:"at"`
	Rule              string    `json:"rule"`
	Status            string    `json:"status"`
	Was               string    `json:"was"`
	Evidence          string    `json:"evidence"`
	PolicyID          string    `json:"policy_id"`
	PolicyVersion     int       `json:"policy_version"`
	PolicyFingerprint string    `json:"policy_fingerprint"`
}

func (s *Server) sqliteRulesHistory(ctx context.Context, since, until time.Time, rule string, limit int) ([]rpc.RuleTransitionEntry, int, error) {
	events, err := s.sqliteHistoryEvents(ctx, coreEventRuleTransition, since, until)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]rpc.RuleTransitionEntry, 0, len(events))
	for _, event := range events {
		var line sqliteRuleTransitionLine
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			return nil, 0, fmt.Errorf("decode rule transition event %d: %w", event.EventSeq, err)
		}
		if rule != "" && line.Rule != rule {
			continue
		}
		entries = append(entries, rpc.RuleTransitionEntry{
			At: line.At, Rule: line.Rule, Status: line.Status, Was: line.Was,
			Evidence: line.Evidence, PolicyID: line.PolicyID,
			PolicyVersion: line.PolicyVersion, PolicyFingerprint: line.PolicyFingerprint,
		})
	}
	total := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, total, nil
}

func (s *Server) sqliteStressHistory(ctx context.Context, since, until time.Time, severity, action string, limit int) ([]rpc.StressHistoryEntry, int, error) {
	events, err := s.sqliteHistoryEvents(ctx, coreEventStressDecision, since, until)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]rpc.StressHistoryEntry, 0, len(events))
	for _, event := range events {
		var line stressDecisionLine
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			return nil, 0, fmt.Errorf("decode stress decision event %d: %w", event.EventSeq, err)
		}
		if severity != "" && string(line.Severity) != severity {
			continue
		}
		if action != "" && line.Action != action {
			continue
		}
		entries = append(entries, rpc.StressHistoryEntry{
			At: line.TS, SessionKey: line.SessionKey, Fingerprint: line.Fingerprint,
			Account: line.Account, AccountMode: line.AccountMode, Action: line.Action,
			Severity: string(line.Severity), Direction: string(line.Direction),
			MarketStage:            line.Market.RegimePosture.Stage,
			PortfolioAlertRelevant: line.PortfolioAlertRelevant,
			InputHealth:            line.InputHealth, Summary: line.Summary,
		})
	}
	total := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, total, nil
}

func (s *Server) sqliteCapitalEvents(ctx context.Context, since, until time.Time, limit int) ([]rpc.CapitalEventEntry, bool, error) {
	events, err := s.sqliteHistoryEvents(ctx, coreEventCapital, since, until)
	if err != nil {
		return nil, false, err
	}
	entries := make([]rpc.CapitalEventEntry, 0, min(len(events), limit+1))
	for _, event := range events {
		var line capitalEventV1
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			return nil, false, fmt.Errorf("decode capital event %d: %w", event.EventSeq, err)
		}
		entries = append(entries, rpc.CapitalEventEntry{
			At: line.At, Type: line.Type, AmountBase: line.AmountBase,
			EffectiveAt: line.EffectiveAt, Note: line.Note, Origin: line.Origin,
			ReportID: line.ReportID,
		})
	}
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	return entries, truncated, nil
}

func (s *Server) sqliteHistoryEvents(ctx context.Context, eventType string, since, until time.Time) ([]corestore.EventRecord, error) {
	if s == nil || s.coreStore == nil {
		return nil, fmt.Errorf("SQLite authority is unavailable")
	}
	events, err := loadAllCoreEvents(ctx, s.coreStore, eventType)
	if err != nil {
		return nil, err
	}
	filtered := events[:0]
	for _, event := range events {
		if !event.OccurredAt.Before(since) && event.OccurredAt.Before(until) {
			filtered = append(filtered, event)
		}
	}
	slices.Reverse(filtered)
	return filtered, nil
}
