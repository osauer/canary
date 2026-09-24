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

// dataHealthDiagnosticVersion is written on every retained diagnostic record.
// A version 2 last_success was set only from a producer's source clock. An
// unversioned record may come from an earlier recorder that also advanced
// last_success on not-due and not-applicable verdicts; see
// legacyDiagnosticLastSuccess.
const dataHealthDiagnosticVersion = 2

type dataHealthDiagnostic struct {
	Version       int                        `json:"version,omitempty"`
	ID            string                     `json:"id"`
	FirstObserved time.Time                  `json:"first_observed"`
	LastSuccess   time.Time                  `json:"last_success,omitzero"`
	Transitions   []rpc.DataHealthTransition `json:"transitions"`
	Truncated     bool                       `json:"truncated,omitempty"`
}

// Restore diagnostic chronology only. No entitlement, current data mode,
// freshness, connector binding or check eligibility is restored from storage.
// Startup attaches the market-event authorities first, so the borrow-fee
// receipt they prove can corroborate a legacy record.
func (s *Server) loadDataHealthHistory() {
	if s.coreStore == nil {
		return
	}
	var borrowFeeReceipt time.Time
	if s.marketEvents != nil {
		borrowFeeReceipt = s.marketEvents.borrowFeeReceiptAt()
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
		if record.Version != dataHealthDiagnosticVersion {
			record.LastSuccess = legacyDiagnosticLastSuccess(record.ID, record.LastSuccess, borrowFeeReceipt)
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

// legacyDiagnosticLastSuccess returns the last_success an unversioned record
// keeps. Such a record comes either from the receipt-only recorder before
// records were versioned or from an earlier recorder that set last_success to
// the check time of any row reported current or not due. Of the sources that
// recorder kept (account, positions, IBKR feeds and market-event rows), only
// the borrow-fee fallback reported that without a delivery: it answered "no
// exact held short stock" with status OK, as_of now and not due. Every other
// clock dates a real delivery, if by its observer, and is kept.
// events:borrow_fee keeps at most borrowFeeReceipt, the newest receipt its
// durable authorities prove, and nothing when they have never recorded one.
func legacyDiagnosticLastSuccess(id string, lastSuccess, borrowFeeReceipt time.Time) time.Time {
	if id == "events:borrow_fee" && borrowFeeReceipt.Before(lastSuccess) {
		return borrowFeeReceipt
	}
	return lastSuccess
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
		records = append(records, dataHealthDiagnostic{Version: dataHealthDiagnosticVersion, ID: id, FirstObserved: o.row.FirstObserved, LastSuccess: o.row.LastSuccess, Transitions: history, Truncated: o.row.HistoryTruncated || truncated})
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
