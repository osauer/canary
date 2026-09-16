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
	row.LastSuccess = previous.row.LastSuccess
	row.History = slices.Clone(previous.row.History)
	if row.FirstObserved.IsZero() {
		row.FirstObserved = row.CheckedAt
	}
	if row.State == "current" || row.State == "not_due" || row.State == "limited" && row.DataType != "" && row.Failure == nil {
		row.LastSuccess = row.CheckedAt
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
		}
		state.diagnosticRevision++
		row.History = append(row.History, rpc.DataHealthTransition{At: row.CheckedAt, State: row.State, DataType: row.DataType, Reason: reason})
		if len(row.History) > 6 {
			row.History = row.History[len(row.History)-6:]
		}
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
	return row.State + "|" + row.DataType + "|" + code + "|" + access
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
		code = rpc.SourceFailureContractUnavailable
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
	if health.RefreshState == rpc.SourceRefreshNotDue && row.State == "current" && health.LastFailure == nil {
		row.State, row.Receiving = "not_due", "As scheduled"
	}
	if health.RefreshState == rpc.SourceRefreshPending && row.State == "current" {
		row.State, row.Receiving = "limited", "Computing · prior context"
	}
	if row.State == "current" && health.LastFailure != nil {
		row.State, row.Receiving = "limited", "Prior data · latest refresh failed"
	}
	if row.State == "current" && health.MaxAgeSeconds > 0 && health.AgeSeconds > health.MaxAgeSeconds {
		row.State, row.Receiving = "limited", "Stale producer observation"
	}
	if row.State == "limited" || row.State == "unavailable" {
		row.ProblemIDs = []string{id}
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
	summary.Label = fmt.Sprintf("%d data problems · %d unverified", summary.Problems, summary.Unverified)
	if summary.Problems == 0 && summary.Unverified == 0 {
		summary.Label = "Required sources are available"
	}
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
	// Keep a page below Desk's 32 KiB observation envelope, even with histories.
	bytes := 0
	for i := p.Offset; i < end; i++ {
		raw, _ := json.Marshal(rows[i])
		if bytes+len(raw) > 24*1024 && i > p.Offset {
			end = i
			break
		}
		bytes += len(raw)
	}
	result := rpc.DataHealthResult{SchemaVersion: rpc.DataHealthSchemaVersion, Revision: revision, AsOf: now, ValidUntil: now.Add(dataHealthObservationValidity), ScopeState: scope, Summary: summary, Sources: slices.Clone(rows[p.Offset:end]), Offset: p.Offset, Complete: end == len(rows)}
	ranked := slices.Clone(rows)
	slices.SortStableFunc(ranked, func(a, b rpc.DataSourceHealth) int {
		return dataHealthStateOrder(a.State) - dataHealthStateOrder(b.State)
	})
	seenConcerns := map[string]bool{}
	for _, row := range ranked {
		if !row.Required || row.State == "current" || row.State == "not_due" {
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
	return result, nil
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
