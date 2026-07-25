package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AppendEvents records a non-empty event batch, its typed projections, and one
// authority-head advance atomically.
func (s *Store) AppendEvents(ctx context.Context, inputs []EventInput) ([]EventReceipt, error) {
	if len(inputs) == 0 {
		return nil, errorsf("at least one event is required")
	}
	for _, input := range inputs {
		if err := validateEventInput(input); err != nil {
			return nil, err
		}
	}
	var receipts []EventReceipt
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		receipts, err = appendEventsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		head, err := advanceHeadTx(ctx, tx, receipts[len(receipts)-1].EventSeq, now)
		if err != nil {
			return err
		}
		for i := range receipts {
			receipts[i].Head = head
		}
		return nil
	})
	return receipts, err
}

// CompareAndSwapStateDocumentWithEvents commits a state revision, a non-empty
// event batch, its projections, and one authority-head advance atomically.
func (s *Store) CompareAndSwapStateDocumentWithEvents(ctx context.Context, update StateDocumentCAS, inputs []EventInput) (StateDocument, []EventReceipt, error) {
	if err := validateStateCAS(update); err != nil {
		return StateDocument{}, nil, err
	}
	if len(inputs) == 0 {
		return StateDocument{}, nil, errorsf("at least one event is required")
	}
	for _, input := range inputs {
		if err := validateEventInput(input); err != nil {
			return StateDocument{}, nil, err
		}
	}
	var saved StateDocument
	var receipts []EventReceipt
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var err error
		saved, err = compareAndSwapStateTx(ctx, tx, update, now)
		if err != nil {
			return err
		}
		receipts, err = appendEventsTx(ctx, tx, inputs, now)
		if err != nil {
			return err
		}
		head, err := advanceHeadTx(ctx, tx, receipts[len(receipts)-1].EventSeq, now)
		if err != nil {
			return err
		}
		for i := range receipts {
			receipts[i].Head = head
		}
		return nil
	})
	return saved, receipts, err
}

// LoadEvents returns matching events in ascending event-sequence order. A zero
// limit defaults to 1,000 rows.
func (s *Store) LoadEvents(ctx context.Context, query EventQuery) ([]EventRecord, error) {
	if query.ScopeKey != "" {
		if err := validateKey("scope key", query.ScopeKey, 512); err != nil {
			return nil, err
		}
	}
	if query.Type != "" {
		if err := validateKey("event type", query.Type, 128); err != nil {
			return nil, err
		}
	}
	if query.Limit < 0 || query.Limit > 10000 {
		return nil, errorsf("event query limit is invalid")
	}
	limit := query.Limit
	if limit == 0 {
		limit = 1000
	}
	clauses := []string{"1=1"}
	args := []any{}
	if query.ScopeKey != "" {
		clauses = append(clauses, "scope_key=?")
		args = append(args, query.ScopeKey)
	}
	if query.Type != "" {
		clauses = append(clauses, "event_type=?")
		args = append(args, query.Type)
	}
	if query.FromAtMS != 0 {
		clauses = append(clauses, "occurred_at_ms>=?")
		args = append(args, query.FromAtMS)
	}
	if query.ToAtMS != 0 {
		clauses = append(clauses, "occurred_at_ms<=?")
		args = append(args, query.ToAtMS)
	}
	if query.AfterEventSeq != 0 {
		clauses = append(clauses, "event_seq>?")
		args = append(args, query.AfterEventSeq)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT event_seq,scope_key,event_key,event_type,action_kind,origin,occurred_at,recorded_at,payload_json
FROM event_log WHERE `+strings.Join(clauses, " AND ")+` ORDER BY event_seq LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var item EventRecord
		var occurred, recorded string
		if err := rows.Scan(&item.EventSeq, &item.ScopeKey, &item.EventKey, &item.Type, &item.Action, &item.Origin, &occurred, &recorded, &item.PayloadJSON); err != nil {
			return nil, err
		}
		item.OccurredAt, err = parseTime(occurred)
		if err != nil {
			return nil, err
		}
		item.RecordedAt, err = parseTime(recorded)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func appendEventsTx(ctx context.Context, tx *sql.Tx, inputs []EventInput, now time.Time) ([]EventReceipt, error) {
	receipts := make([]EventReceipt, 0, len(inputs))
	for _, input := range inputs {
		digest := sha256.Sum256(input.PayloadJSON)
		result, err := tx.ExecContext(ctx, `INSERT INTO event_log(scope_key,event_key,event_type,action_kind,origin,occurred_at,occurred_at_ms,recorded_at,payload_json,payload_sha256)
VALUES(?,?,?,?,?,?,?,?,?,?)`, input.ScopeKey, input.EventKey, input.Type, input.Action, input.Origin, formatTime(input.OccurredAt), input.OccurredAt.UnixMilli(), formatTime(now), input.PayloadJSON, digest[:])
		if err != nil {
			return nil, fmt.Errorf("append event: %w", err)
		}
		seq, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		if err := insertProjection(ctx, tx, seq, input); err != nil {
			return nil, err
		}
		receipts = append(receipts, EventReceipt{EventSeq: seq, RecordedAt: now})
	}
	return receipts, nil
}

func insertProjection(ctx context.Context, tx *sql.Tx, seq int64, input EventInput) error {
	p := input.Projection
	switch {
	case p.RegimeDecision != nil:
		v := p.RegimeDecision
		if _, err := tx.ExecContext(ctx, `INSERT INTO regime_decisions(event_seq,scope_key,decision_key,stage,severity,readiness,confidence,verdict,fingerprint) VALUES(?,?,?,?,?,?,?,?,?)`, seq, input.ScopeKey, v.DecisionKey, v.Stage, nullableString(v.Severity), nullableString(v.Readiness), nullableString(v.Confidence), nullableString(v.Verdict), nullableString(v.Fingerprint)); err != nil {
			return err
		}
		for _, indicator := range v.Indicators {
			if _, err := tx.ExecContext(ctx, `INSERT INTO regime_indicators(decision_event_seq,indicator,status,band,value,depth,streak_sessions,freshness,eligible,latched,thresholds_label) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, seq, indicator.Indicator, nullableString(indicator.Status), nullableString(indicator.Band), indicator.Value, indicator.Depth, indicator.StreakSessions, nullableString(indicator.Freshness), nullableBool(indicator.Eligible), boolInt(indicator.Latched), nullableString(indicator.ThresholdsLabel)); err != nil {
				return err
			}
		}
	case p.RuleTransition != nil:
		v := p.RuleTransition
		_, err := tx.ExecContext(ctx, `INSERT INTO rule_transitions(event_seq,scope_key,rule_id,status,previous_status,policy_id,policy_version,policy_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, seq, input.ScopeKey, v.RuleID, v.Status, nullableString(v.PreviousStatus), nullableString(v.PolicyID), v.PolicyVersion, nullableString(v.PolicyFingerprint))
		return err
	case p.StressTransition != nil:
		v := p.StressTransition
		_, err := tx.ExecContext(ctx, `INSERT INTO stress_transitions(event_seq,scope_key,action,severity,direction,market_stage,input_health,portfolio_alert_relevant) VALUES(?,?,?,?,?,?,?,?)`, seq, input.ScopeKey, v.Action, nullableString(v.Severity), nullableString(v.Direction), nullableString(v.MarketStage), nullableString(v.InputHealth), nullableBool(v.PortfolioAlertRelevant))
		return err
	case p.CapitalEvent != nil:
		v := p.CapitalEvent
		_, err := tx.ExecContext(ctx, `INSERT INTO capital_events(event_seq,scope_key,kind,amount_base_text,effective_at,report_id) VALUES(?,?,?,?,?,?)`, seq, input.ScopeKey, v.Kind, nullableString(v.AmountBaseText), nullableString(v.EffectiveAt), nullableString(v.ReportID))
		return err
	case p.RiskPolicyEvent != nil:
		v := p.RiskPolicyEvent
		_, err := tx.ExecContext(ctx, `INSERT INTO risk_policy_events(event_seq,scope_key,kind,policy_id,policy_version,policy_fingerprint) VALUES(?,?,?,?,?,?)`, seq, input.ScopeKey, v.Kind, nullableString(v.PolicyID), v.PolicyVersion, nullableString(v.PolicyFingerprint))
		return err
	case p.ProposalOutcome != nil:
		v := p.ProposalOutcome
		_, err := tx.ExecContext(ctx, `INSERT INTO proposal_outcomes(event_seq,scope_key,proposal_key,revision,bucket,symbol,sec_type,action,state) VALUES(?,?,?,?,?,?,?,?,?)`, seq, input.ScopeKey, v.ProposalKey, nullableString(v.Revision), nullableString(v.Bucket), nullableString(v.Symbol), nullableString(v.SecType), nullableString(v.Action), v.State)
		return err
	}
	return nil
}

func validateEventInput(input EventInput) error {
	for _, item := range []struct {
		label, value string
		limit        int
	}{{"scope key", input.ScopeKey, 512}, {"event key", input.EventKey, 512}, {"event type", input.Type, 128}, {"event action", input.Action, 128}, {"event origin", input.Origin, 128}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if input.OccurredAt.IsZero() {
		return errorsf("event time is required")
	}
	if !json.Valid(input.PayloadJSON) {
		return errorsf("event payload must be valid JSON")
	}
	count := 0
	p := input.Projection
	for _, set := range []bool{p.RegimeDecision != nil, p.RuleTransition != nil, p.StressTransition != nil, p.CapitalEvent != nil, p.RiskPolicyEvent != nil, p.ProposalOutcome != nil} {
		if set {
			count++
		}
	}
	if count > 1 {
		return errorsf("event may have at most one projection")
	}
	return validateProjection(input.Projection)
}

func validateProjection(p EventProjection) error {
	if p.RegimeDecision != nil {
		if err := validateKey("decision key", p.RegimeDecision.DecisionKey, 512); err != nil {
			return err
		}
		if err := validateKey("regime stage", p.RegimeDecision.Stage, 128); err != nil {
			return err
		}
		for _, v := range p.RegimeDecision.Indicators {
			if err := validateKey("indicator", v.Indicator, 128); err != nil {
				return err
			}
		}
	}
	if p.RuleTransition != nil {
		if err := validateKey("rule id", p.RuleTransition.RuleID, 128); err != nil {
			return err
		}
		return validateKey("rule status", p.RuleTransition.Status, 128)
	}
	if p.StressTransition != nil {
		return validateKey("stress action", p.StressTransition.Action, 128)
	}
	if p.CapitalEvent != nil {
		return validateKey("capital event kind", p.CapitalEvent.Kind, 128)
	}
	if p.RiskPolicyEvent != nil {
		return validateKey("risk policy event kind", p.RiskPolicyEvent.Kind, 128)
	}
	if p.ProposalOutcome != nil {
		if err := validateKey("proposal key", p.ProposalOutcome.ProposalKey, 512); err != nil {
			return err
		}
		return validateKey("proposal state", p.ProposalOutcome.State, 128)
	}
	return nil
}

func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
