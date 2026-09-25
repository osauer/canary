package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// This validity dates the health observation, not the market price. Native
// producer freshness and source clocks remain separate and are never extended.
const dataHealthObservationValidity = time.Minute
const quoteHealthObservationValidity = 5 * time.Minute
const dataHealthHistoryAge = 7 * 24 * time.Hour
const dataHealthHistoryLimit = 128
const dataHealthPageBytes = 30 * 1024

type dataHealthObservation struct {
	row       rpc.DataSourceHealth
	connector *ibkr.Connector
	binding   ibkr.ConnectorSessionBinding
	broker    bool
}

type dataHealthState struct {
	mu                          sync.Mutex
	observations                map[string]dataHealthObservation
	checkResult                 rpc.DataCheckResult
	loopWG                      sync.WaitGroup
	wake                        chan struct{}
	diagnosticRevision          uint64
	feedObservations            map[string]map[string]dataHealthObservation
	reports                     map[string]rpc.DataHealthResult
	latest                      rpc.DataHealthResult
	persistedDiagnosticRevision uint64
	historyUnavailable          bool
}

func unknownDataSource(id, name, provider, kind string, affects ...string) rpc.DataSourceHealth {
	return rpc.DataSourceHealth{ID: id, Name: name, Provider: provider, Kind: kind, Required: true, State: "unknown", Receiving: "Not checked", Affects: affects, Action: "Awaiting producer observation"}
}

func (s *Server) recordDataHealth(row rpc.DataSourceHealth, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, broker bool) {
	if row.ID == "" || instrumentHealthID(row.ID) {
		return
	}
	state := &s.dataHealth
	observedAt := s.orderNow()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.observations == nil {
		state.observations = make(map[string]dataHealthObservation)
	}
	previous, found := state.observations[row.ID]
	if len(state.observations) >= 2048 && !found {
		return
	} // The fixed source catalogue still lists unobserved services as unknown.
	row.FirstObserved = previous.row.FirstObserved
	// Only actual producer clocks establish successful receipt. Not-due and
	// not-relevant verdicts must not renew that history on every read; their
	// producers report no source clock unless something was delivered.
	if previous.row.LastSuccess.After(row.LastSuccess) {
		row.LastSuccess = previous.row.LastSuccess
	}
	row.History = slices.Clone(previous.row.History)
	row.HistoryTruncated = previous.row.HistoryTruncated
	if row.FirstObserved.IsZero() {
		row.FirstObserved = observedAt
	}
	if row.ReceivedAt.After(row.LastSuccess) && !row.ReceivedAt.After(row.CheckedAt) {
		row.LastSuccess = row.ReceivedAt
	}
	if row.LastSuccess != previous.row.LastSuccess {
		state.diagnosticRevision++
	}
	if !found || dataHealthCondition(row) != dataHealthCondition(previous.row) {
		reason := ""
		if row.Failure != nil {
			reason = row.Failure.Code
		} else if row.Access != nil {
			reason = row.Access.Reason
		} else if row.Usability == "blocked" || row.Usability == "limited" {
			reason = "analytics_" + row.Usability
		}
		state.diagnosticRevision++
		row.History = append(row.History, rpc.DataHealthTransition{At: observedAt, State: row.State, DataType: row.DataType, Reason: dataHealthHistoryReason(reason)})
	}
	var truncated bool
	row.History, truncated = retainDataHealthHistory(row.History, observedAt)
	row.HistoryTruncated = row.HistoryTruncated || truncated
	if truncated {
		state.diagnosticRevision++
	}
	state.observations[row.ID] = dataHealthObservation{row: row, connector: c, binding: binding, broker: broker}
}

func dataHealthCondition(row rpc.DataSourceHealth) string {
	code := ""
	if row.Failure != nil {
		code = row.Failure.Code
	}
	access := ""
	if row.Access != nil {
		access = fmt.Sprintf("%d:%s", row.Access.Code, row.Access.Reason)
	}
	return row.State + "|" + row.DataType + "|" + code + "|" + access + "|" + row.Availability + "|" + row.CadenceState + "|" + row.Applicability + "|" + row.Usability
}

func dataHealthHistoryReason(reason string) string {
	if len(reason) > 64 {
		return "unclassified_source_failure"
	}
	for _, ch := range reason {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == ':' || ch == '-') {
			return "unclassified_source_failure"
		}
	}
	return reason
}

func retainDataHealthHistory(history []rpc.DataHealthTransition, now time.Time) ([]rpc.DataHealthTransition, bool) {
	before := len(history)
	cutoff := now.Add(-dataHealthHistoryAge)
	history = slices.DeleteFunc(history, func(h rpc.DataHealthTransition) bool { return h.At.IsZero() || h.At.Before(cutoff) || h.At.After(now) })
	if len(history) > dataHealthHistoryLimit {
		history = history[len(history)-dataHealthHistoryLimit:]
	}
	return history, len(history) != before
}

func quoteHealthFailure(err error, now time.Time) *rpc.SourceFailure {
	if err == nil {
		return nil
	}
	code := ""
	var absent *ibkr.MarketDataAbsenceError
	var missing *ibkr.ContractResolutionMissError
	var historical *ibkr.HistoricalRequestError
	switch {
	case errors.As(err, &absent):
		code = rpc.SourceFailureNotEntitled
		if absent.Code == 200 {
			code = rpc.SourceFailureContractUnavailable
		}
	case errors.As(err, &missing), errors.Is(err, ibkr.ErrContractNoDefinition), errors.Is(err, ibkr.ErrSymbolInactive):
		code = rpc.SourceFailureContractUnavailable
	case errors.Is(err, ibkr.ErrIBKRUnavailable):
		code = rpc.SourceFailureGatewayUnavailable
	case errors.Is(err, ibkr.ErrContractDetailsTimeout):
		code = rpc.SourceFailureTimeout
	case errors.Is(err, context.DeadlineExceeded):
		code = rpc.SourceFailureTimeout
	case errors.As(err, &historical):
		code = historical.Category
	}
	if code == "" {
		return nil
	}
	return &rpc.SourceFailure{Code: code, Stage: "quote_request", FailedAt: now, Retryable: true}
}

func projectQuoteSource(q *rpc.Quote, err error, now time.Time) rpc.DataSourceHealth {
	row := feedSource(quoteSourceID)
	row.CheckedAt, row.ValidUntil = now, now.Add(quoteHealthObservationValidity)
	row.Delivery = "producer_observation"
	row.Failure = quoteHealthFailure(err, now)
	if q == nil || q.Price == nil || *q.Price <= 0 || math.IsNaN(*q.Price) || math.IsInf(*q.Price, 0) {
		if row.Failure == nil {
			row.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureNoData, Stage: "quote_observation", FailedAt: now}
		}
		row.State = "unavailable"
		return row
	}
	row.DataType = q.FeedType
	if row.DataType == "" {
		row.DataType = q.DataType
	}
	if row.DataType == "" {
		row.DataType = rpc.MarketDataUnknown
	}
	// A service observation has no individual instrument's price clock.
	row.ReceivedAt = q.ReceivedAt
	if row.ReceivedAt.IsZero() {
		row.ValidUntil = now
	} else {
		row.ValidUntil = row.ReceivedAt.Add(quoteHealthObservationValidity)
	}
	row.State = "current"
	if row.DataType != rpc.MarketDataLive {
		row.State = "limited"
	}
	return row
}

func (s *Server) observeQuoteHealth(contract rpc.ContractParams, q *rpc.Quote, err error, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding) {
	if c != nil && !c.SessionCurrent(binding) {
		return
	}
	row := projectQuoteSource(q, err, s.orderNow())
	s.attachQuoteAccess(&row, contract, c)
	s.recordFeedHealth(row, c, binding)
}

func projectSourceHealth(id, name, provider, kind string, health rpc.SourceHealth, now time.Time, affects ...string) rpc.DataSourceHealth {
	row := unknownDataSource(id, name, provider, kind, affects...)
	row.CheckedAt, row.SourceAt = now, health.AsOf
	row.SourceTimeKind = "producer_observation"
	row.ValidUntil = now.Add(dataHealthObservationValidity)
	row.Failure = health.LastFailure
	row.Action = "Observed automatically"
	if health.NextAttempt != nil {
		row.NextAttempt = *health.NextAttempt
		row.Action = "Producer retry scheduled"
	}
	row.DerivedFrom = slices.Clone(health.DerivedFrom)
	row.Applicability = health.Applicability
	if health.RefreshState == rpc.SourceRefreshNotDue {
		row.CadenceState = "not_due"
	}
	if health.RefreshState == rpc.SourceRefreshPending {
		row.CadenceState = "pending"
	}
	switch health.Status {
	case "ok", "current", "available", "ready":
		row.State, row.Receiving = "current", "Current"
	case "inactive", "not_relevant":
		row.State, row.Receiving, row.Required = "not_relevant", "Not needed", false
	case "unavailable", "error", "failed", "mismatch":
		row.State, row.Receiving = "unavailable", "Unavailable"
	case "partial", "degraded", "stale", "overdue", "pending", "computing", "backfilling":
		row.State, row.Receiving = "limited", strings.ReplaceAll(health.Status, "_", " ")
	default:
		row.State, row.Receiving = "unknown", "Health unverified"
	}
	switch row.State {
	case "current":
		row.Availability = "available"
	case "unavailable":
		row.Availability = "unavailable"
	case "limited":
		row.Availability = "limited"
	default:
		row.Availability = "unknown"
	}
	if !health.AsOf.IsZero() && (row.State == "current" || row.State == "limited") {
		row.ReceivedAt = health.AsOf
	}
	if health.RefreshState == rpc.SourceRefreshNotDue && row.State == "current" && health.LastFailure == nil {
		row.State, row.Receiving = "not_due", "As scheduled"
	}
	if health.RefreshState == rpc.SourceRefreshPending && row.State == "current" {
		row.State, row.Receiving = "limited", "Computing · prior context"
	}
	if row.State == "current" && health.LastFailure != nil {
		row.State, row.Receiving = "limited", "Prior data · latest refresh failed"
	}
	if health.LastFailure != nil {
		row.Availability = "unavailable"
	}
	if row.State == "current" && health.MaxAgeSeconds > 0 && health.AgeSeconds > health.MaxAgeSeconds {
		row.State, row.Receiving = "limited", "Stale producer observation"
		row.Availability = "unknown"
	}
	if row.State == "limited" || row.State == "unavailable" {
		row.ProblemIDs = []string{id}
	}
	if health.Applicability == "not_relevant" {
		// The scope waives the need, not the provider facts: availability,
		// failure and any real receipt stay as observed, but add no concern.
		row.State, row.Receiving, row.Required = "not_relevant", "Not needed for the current scope", false
		row.ProblemIDs = nil
	}
	return row
}

func finalizeDataHealth(rows []rpc.DataSourceHealth, scope string, now time.Time, p rpc.DataHealthParams) (rpc.DataHealthResult, error) {
	if p.Offset < 0 || p.Limit < 0 || p.Limit > 64 {
		return rpc.DataHealthResult{}, errBadRequest("health page requires offset >= 0 and limit <= 64")
	}
	limit := p.Limit
	if limit == 0 {
		limit = 24
	}
	slices.SortFunc(rows, func(a, b rpc.DataSourceHealth) int {
		if dataHealthKindOrder(a.Kind) != dataHealthKindOrder(b.Kind) {
			return dataHealthKindOrder(a.Kind) - dataHealthKindOrder(b.Kind)
		}
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.ID, b.ID)
	})
	byID := make(map[string]rpc.DataSourceHealth, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	problems := map[string]bool{}
	summary := rpc.DataHealthSummary{State: "current", Total: len(rows)}
	for i := range rows {
		r := &rows[i]
		if !r.Required {
			continue
		}
		summary.Required++
		switch r.State {
		case "current", "not_due":
			summary.Current++
		case "limited":
			summary.Limited++
		case "unavailable":
			summary.Unavailable++
		default:
			summary.Unverified++
		}
		if dataHealthExpectedDelay(*r) {
			summary.ExpectedDelays++
			continue
		}
		// Inherited causes are supplied by the producer. Suppress a derived
		// row only when those leaf observations really exist in this report.
		inherited := len(r.DerivedFrom) > 0
		for _, id := range r.DerivedFrom {
			if leaf, ok := byID[id]; !ok || len(leaf.ProblemIDs) == 0 {
				inherited = false
			}
		}
		if inherited {
			continue
		}
		for _, id := range r.ProblemIDs {
			problems[id] = true
		}
	}
	summary.Problems = len(problems)
	if scope != "current" {
		summary.State = "unknown"
	}
	if summary.Unverified > 0 {
		summary.State = "unknown"
	}
	if summary.Limited > 0 {
		summary.State = "limited"
	}
	if summary.Unavailable > 0 {
		summary.State = "unavailable"
	}
	summary.Label = dataHealthSummaryLabel(summary)
	raw, _ := json.Marshal(struct {
		Scope string
		Rows  []rpc.DataSourceHealth
	}{scope, rows})
	hash := sha256.Sum256(raw)
	revision := hex.EncodeToString(hash[:12])
	if p.Revision != "" && p.Revision != revision {
		return rpc.DataHealthResult{}, errBadRequest("health report changed; restart pagination")
	}
	if p.Offset > len(rows) {
		return rpc.DataHealthResult{}, errBadRequest("health offset exceeds report")
	}
	end := min(len(rows), p.Offset+limit)
	result := rpc.DataHealthResult{SchemaVersion: rpc.DataHealthSchemaVersion, Revision: revision, AsOf: now, ValidUntil: now.Add(dataHealthObservationValidity), ScopeState: scope, Summary: summary, Sources: slices.Clone(rows[p.Offset:end]), Offset: p.Offset, Complete: end == len(rows)}
	ranked := slices.Clone(rows)
	slices.SortStableFunc(ranked, func(a, b rpc.DataSourceHealth) int {
		return dataHealthStateOrder(a.State) - dataHealthStateOrder(b.State)
	})
	seenConcerns := map[string]bool{}
	for _, row := range ranked {
		if !row.Required || row.State == "current" || row.State == "not_due" || dataHealthExpectedDelay(row) {
			continue
		}
		cause := row.ID
		if len(row.ProblemIDs) > 0 {
			cause = row.ProblemIDs[0]
		}
		if seenConcerns[cause] {
			continue
		}
		seenConcerns[cause] = true
		result.Concerns = append(result.Concerns, rpc.DataHealthConcern{SourceID: row.ID, State: row.State, Label: row.Name + ": " + row.Receiving})
		if len(result.Concerns) == 8 {
			break
		}
	}
	if result.Sources == nil {
		result.Sources = []rpc.DataSourceHealth{}
	}
	if end < len(rows) {
		result.NextOffset = &end
	}
	return fitDataHealthPage(result)
}

// dataHealthExpectedDelay reports a limited row that its producer holds out
// of current only for an expected cause. Its state stays limited; the summary
// counts it as an expected delay, never as a source concern or a ranked
// concern.
func dataHealthExpectedDelay(row rpc.DataSourceHealth) bool {
	return row.State == "limited" && rpc.DataHealthCauseExpected(row.Cause)
}

// dataHealthSummaryLabel keeps expected delays apart from source concerns.
func dataHealthSummaryLabel(s rpc.DataHealthSummary) string {
	if s.Problems == 0 && s.ExpectedDelays == 0 && s.Unverified == 0 {
		return "Required sources are available"
	}
	return fmt.Sprintf("%d source %s · %d expected %s · %d unknown coverage", s.Problems, plural(s.Problems, "concern", "concerns"), s.ExpectedDelays, plural(s.ExpectedDelays, "delay", "delays"), s.Unverified)
}

// Bound the actual JSON envelope, including concern labels and histories.
// Shrinking a page advances by the rows emitted; a single oversize source is
// an explicit error, never a silently incomplete source catalogue.
func fitDataHealthPage(result rpc.DataHealthResult) (rpc.DataHealthResult, error) {
	for {
		raw, err := json.Marshal(result)
		if err != nil {
			return rpc.DataHealthResult{}, err
		}
		if len(raw) <= dataHealthPageBytes {
			return result, nil
		}
		if len(result.Sources) <= 1 {
			return rpc.DataHealthResult{}, errBadRequest("source health exceeds page budget")
		}
		result.Sources = result.Sources[:len(result.Sources)-1]
		next := result.Offset + len(result.Sources)
		result.NextOffset, result.Complete = &next, false
	}
}

func dataHealthKindOrder(kind string) int {
	switch kind {
	case "connection":
		return 0
	case "scope":
		return 1
	case "account_data":
		return 2
	case "market_feed":
		return 3
	case "history":
		return 4
	case "analytics_input":
		return 5
	case "market_events":
		return 6
	case "calendar":
		return 7
	case "public_source":
		return 8
	case "reporting":
		return 9
	default:
		return 10
	}
}
func dataHealthStateOrder(state string) int {
	switch state {
	case "unavailable":
		return 0
	case "limited":
		return 1
	case "unknown":
		return 2
	default:
		return 3
	}
}
