package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const dataHealthHistoryKind = "data_health.diagnostics.v1"

type dataHealthDiagnostic struct {
	ID            string                     `json:"id"`
	FirstObserved time.Time                  `json:"first_observed"`
	LastSuccess   time.Time                  `json:"last_success,omitzero"`
	Transitions   []rpc.DataHealthTransition `json:"transitions"`
	Truncated     bool                       `json:"truncated,omitempty"`
}

// Restore diagnostic chronology only. No entitlement, current data mode,
// freshness, connector binding or check eligibility is restored from storage.
func (s *Server) loadDataHealthHistory() {
	if s.coreStore == nil {
		return
	}
	raw, ok, err := loadMarketState(s.coreStore, "data-health-diagnostics", dataHealthHistoryKind)
	var records []dataHealthDiagnostic
	if err == nil && ok && (len(raw) > 2<<20 || json.Unmarshal(raw, &records) != nil || len(records) > 2048) {
		err = errors.New("invalid data health history")
	}
	s.dataHealth.mu.Lock()
	defer s.dataHealth.mu.Unlock()
	if err != nil {
		s.dataHealth.historyUnavailable = true
		return
	}
	if s.dataHealth.observations == nil {
		s.dataHealth.observations = map[string]dataHealthObservation{}
	}
	now := s.orderNow()
	for _, record := range records {
		if record.ID == "" || instrumentHealthID(record.ID) || len(record.ID) > 128 || len(record.Transitions) > dataHealthHistoryLimit || record.FirstObserved.After(now) || record.LastSuccess.After(now) {
			continue
		}
		history := slices.Clone(record.Transitions)
		if len(history) > 0 && record.FirstObserved.Before(history[0].At) {
			record.Truncated = true
		}
		valid := true
		for _, h := range history {
			if dataHealthHistoryReason(h.Reason) != h.Reason || len(h.State) > 32 || len(h.DataType) > 32 {
				valid = false
			}
		}
		if !valid {
			s.dataHealth.historyUnavailable = true
			continue
		}
		history = append(history, rpc.DataHealthTransition{At: now, State: "unknown", Reason: "daemon_restart_observation_gap"})
		history, truncated := retainDataHealthHistory(history, now)
		row := rpc.DataSourceHealth{ID: record.ID, State: "unknown", Receiving: "Awaiting producer observation", FirstObserved: record.FirstObserved, LastSuccess: record.LastSuccess, History: history, HistoryTruncated: record.Truncated || truncated}
		s.dataHealth.observations[record.ID] = dataHealthObservation{row: row}
	}
	s.dataHealth.diagnosticRevision++
}

func (s *Server) persistDataHealthHistory(ctx context.Context) {
	if s.coreStore == nil {
		return
	}
	s.dataHealth.mu.Lock()
	revision := s.dataHealth.diagnosticRevision
	if revision == s.dataHealth.persistedDiagnosticRevision {
		s.dataHealth.mu.Unlock()
		return
	}
	records := make([]dataHealthDiagnostic, 0, len(s.dataHealth.observations))
	for id, o := range s.dataHealth.observations {
		if instrumentHealthID(id) {
			continue
		}
		history, truncated := retainDataHealthHistory(slices.Clone(o.row.History), s.orderNow())
		records = append(records, dataHealthDiagnostic{ID: id, FirstObserved: o.row.FirstObserved, LastSuccess: o.row.LastSuccess, Transitions: history, Truncated: o.row.HistoryTruncated || truncated})
	}
	s.dataHealth.mu.Unlock()
	slices.SortFunc(records, func(a, b dataHealthDiagnostic) int { return a.FirstObserved.Compare(b.FirstObserved) })
	raw, err := json.Marshal(records)
	if err == nil && len(raw) > 2<<20 {
		err = errors.New("data health history limit")
	}
	if err == nil {
		err = saveMarketDocument(ctx, s.coreStore, "data-health-diagnostics", dataHealthHistoryKind, raw)
	}
	s.dataHealth.mu.Lock()
	defer s.dataHealth.mu.Unlock()
	s.dataHealth.historyUnavailable = err != nil
	if err == nil {
		s.dataHealth.persistedDiagnosticRevision = revision
	}
}
