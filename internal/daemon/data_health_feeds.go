package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	quoteSourceID   = "ibkr:quotes"
	historySourceID = "ibkr:history"
)

func instrumentHealthID(id string) bool {
	return strings.HasPrefix(id, "quote:") || strings.HasPrefix(id, "history:")
}

func feedSource(id string) rpc.DataSourceHealth {
	name, kind, affects := "IBKR quote feed", "market_feed", "Market quotes"
	if id == historySourceID {
		name, kind, affects = "IBKR historical data", "history", "Price history"
	}
	row := unknownDataSource(id, name, "IBKR", kind, affects)
	row.Detail = "Service availability does not establish coverage or suitability for any individual instrument. See its quote or chart for that evidence."
	row.Action = "Managed by Canary's existing data producer"
	return row
}

// The health ledger is keyed only by source and bounded observation category.
// Inactive contracts and absent instrument data belong to instrument evidence;
// they cannot create, degrade, or schedule a data-source health record.
func (s *Server) recordFeedHealth(row rpc.DataSourceHealth, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding) {
	if row.ID != quoteSourceID && row.ID != historySourceID {
		return
	}
	if !s.orderNow().Before(row.ValidUntil) {
		return
	}
	if row.Access != nil && row.Access.Code == 200 {
		row.Access = nil // an unresolved instrument is not a source restriction
	}
	// Quote shells retain a typed refusal even when no price was returned.
	// Do not lose that provider evidence by treating the shell as mere no-data.
	if row.Access != nil && row.Failure != nil && row.Failure.Code == rpc.SourceFailureNoData {
		row.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: "quote_request", FailedAt: row.Access.ObservedAt, Retryable: true}
	}
	key := "mode:" + row.DataType
	if row.Failure != nil {
		switch row.Failure.Code {
		case rpc.SourceFailureTimeout, rpc.SourceFailureDNSFailed, rpc.SourceFailureConnectionRefused, rpc.SourceFailureTransportFailed, rpc.SourceFailureProtocolRejected, rpc.SourceFailureAuthenticationRejected, rpc.SourceFailureInvalidPayload, rpc.SourceFailureNotEntitled, rpc.SourceFailureGatewayUnavailable, rpc.SourceFailurePacing:
		default:
			return
		}
		key = "failure:" + row.Failure.Code
	} else if row.ID == quoteSourceID {
		switch row.DataType {
		case rpc.MarketDataLive, rpc.MarketDataDelayed, rpc.MarketDataFrozen, rpc.MarketDataDelayedFrozen, rpc.MarketDataPrevClose, rpc.MarketDataUnknown:
		default:
			row.DataType = rpc.MarketDataUnknown
		}
		key = "mode:" + row.DataType
	}
	// A successful quote in the same mode may come from another feed segment.
	// Keep restricted observations separately until their own evidence expires.
	if row.Access != nil {
		key += ":restricted"
	}
	s.dataHealth.mu.Lock()
	if s.dataHealth.feedObservations == nil {
		s.dataHealth.feedObservations = map[string]map[string]dataHealthObservation{}
	}
	if s.dataHealth.feedObservations[row.ID] == nil {
		s.dataHealth.feedObservations[row.ID] = map[string]dataHealthObservation{}
	}
	observations := s.dataHealth.feedObservations[row.ID]
	// A success in one feed segment cannot clear a different segment's
	// restriction or failure. Each source-level category has its own expiry.
	if !observations[key].row.CheckedAt.After(row.CheckedAt) {
		observations[key] = dataHealthObservation{row: row, connector: c, binding: binding, broker: c != nil}
	}
	s.dataHealth.mu.Unlock()
	// Persist only the source assessment, not the request or its contract.
	s.recordDataHealth(s.feedDataHealth(row.ID, s.orderNow()), c, binding, c != nil)
}

func (s *Server) feedDataHealth(id string, now time.Time) rpc.DataSourceHealth {
	s.dataHealth.mu.Lock()
	var observations []dataHealthObservation
	for _, observation := range s.dataHealth.feedObservations[id] {
		observations = append(observations, observation)
	}
	previous := s.dataHealth.observations[id].row
	s.dataHealth.mu.Unlock()
	row := feedSource(id)
	row.FirstObserved, row.LastSuccess, row.History = previous.FirstObserved, previous.LastSuccess, slices.Clone(previous.History)
	row.HistoryTruncated = previous.HistoryTruncated
	var truncated bool
	row.History, truncated = retainDataHealthHistory(row.History, now)
	row.HistoryTruncated = row.HistoryTruncated || truncated
	row.Availability = "unknown"
	var modes []string
	success, failed := false, false
	for _, observation := range observations {
		o := observation.row
		if !now.Before(o.ValidUntil) || observation.broker && (!observation.connector.SessionCurrent(observation.binding) || observation.connector.BackendLink().Down) {
			continue
		}
		if o.CheckedAt.After(row.CheckedAt) {
			row.CheckedAt = o.CheckedAt
		}
		if o.ReceivedAt.After(row.ReceivedAt) {
			row.ReceivedAt = o.ReceivedAt
		}
		if row.ValidUntil.IsZero() || o.ValidUntil.Before(row.ValidUntil) {
			row.ValidUntil = o.ValidUntil
		}
		if o.Failure == nil {
			success = true
			if o.DataType != "" && !slices.Contains(modes, o.DataType) {
				modes = append(modes, o.DataType)
			}
		} else {
			failed = true
			if row.Failure == nil || o.Failure.FailedAt.After(row.Failure.FailedAt) {
				row.Failure = o.Failure
			}
		}
		if o.Access != nil && (row.Access == nil || o.Access.ObservedAt.After(row.Access.ObservedAt)) {
			row.Access = o.Access
		}
	}
	if !success && !failed {
		return row
	}
	row.Delivery = "producer_observation"
	row.State, row.Receiving = "current", "Receiving data"
	row.Availability = "available"
	if failed {
		row.State, row.Receiving = "unavailable", "Service request failed"
		row.Availability = "unavailable"
		if success {
			row.State, row.Receiving = "limited", "Receiving data · service errors observed"
			row.Availability = "limited"
		}
	}
	if len(modes) > 0 {
		slices.Sort(modes)
		row.DataType = modes[0]
		if len(modes) > 1 {
			row.DataType = "mixed"
		}
		var labels []string
		for _, mode := range modes {
			label := map[string]string{"live": "live", "delayed": "delayed", "frozen": "frozen", "delayed-frozen": "delayed last-session", "prev_close": "previous-close", "unknown": "unverified mode"}[mode]
			if label == "" {
				label = "unverified mode"
			}
			labels = append(labels, label)
			// A usable delayed quote is a feed mode, not a service failure.
			// Instrument clocks and suitability remain in the quote evidence.
			if mode == rpc.MarketDataUnknown {
				row.State = "limited"
			}
		}
		row.Receiving = "Receiving " + strings.Join(labels, " and ") + " data"
		if failed {
			row.Receiving += " · service errors observed"
		}
	}
	if row.Access != nil {
		// Keep live-access diagnostics even when a usable fallback arrived.
		// A refusal without a quote is retained above as a request failure.
		row.NextAttempt = row.Access.RetryAt
		row.Action = "Canary retries restricted live access automatically when due"
	}
	if row.State != "current" {
		row.ProblemIDs = []string{id}
	}
	return row
}

// Record the acquisition service result, never a selected chart window's
// completeness. A short listing history is not a provider outage.
func (s *Server) observeHistorySource(received bool, err error, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding) {
	if c != nil && !c.SessionCurrent(binding) {
		return
	}
	row := feedSource(historySourceID)
	row.CheckedAt = s.orderNow()
	row.ValidUntil = row.CheckedAt.Add(30 * time.Minute)
	row.Failure = quoteHealthFailure(err, row.CheckedAt)
	if !received && row.Failure == nil {
		return
	}
	if row.Failure != nil {
		row.ValidUntil = row.CheckedAt.Add(quoteHealthObservationValidity)
		row.Failure.Stage = "history_request"
	} else {
		row.ReceivedAt = row.CheckedAt
	}
	s.recordFeedHealth(row, c, binding)
}
