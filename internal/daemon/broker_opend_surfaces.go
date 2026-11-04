package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
)

// ibkrOnlyMethods answer "not applicable" under another broker: statements, Edge,
// reconciliation and the US borrow data all rest on IBKR's inputs.
var ibkrOnlyMethods = map[string]bool{
	rpc.MethodEdgeSnapshot:         true,
	rpc.MethodEdgeReplay:           true,
	rpc.MethodReconSnapshot:        true,
	rpc.MethodReconCheck:           true,
	rpc.MethodReconStatus:          true,
	rpc.MethodReconBacktest:        true,
	rpc.MethodReconDismiss:         true,
	rpc.MethodReconEquity:          true,
	rpc.MethodReportingStatus:      true,
	rpc.MethodReportingValidate:    true,
	rpc.MethodReportingCosts:       true,
	rpc.MethodReportingPerformance: true,
	rpc.MethodLendingMarket:        true,
	rpc.MethodLendingScreen:        true,
	rpc.MethodFinancingFees:        true,
	rpc.MethodShortInterestScreen:  true,
}

// opendNotServed names, in the reader's words, what OpenD does not serve.
var opendNotServed = []string{"regime", "Edge", "reconciliation", "statement reporting", "borrow availability", "earnings (WSH)", "breadth", "gamma", "cash sweep"}

// brokerCapabilities says what the daemon's broker can do.
func (s *Server) brokerCapabilities() *rpc.BrokerCapabilities {
	if s != nil && s.opend != nil {
		return &rpc.BrokerCapabilities{Broker: "opend", Preview: rpc.BrokerPreviewMaxQuantity, Unlock: "owner_in_opend", Statements: "none"}
	}
	return &rpc.BrokerCapabilities{Broker: "ibkr", Preview: rpc.BrokerPreviewWhatIf, Unlock: "not_required", Statements: "flex", Reconciliation: true, Edge: true}
}

// opendHealth is OpenD's own state for the status rows.
func (s *Server) opendHealth() *rpc.OpenDHealth {
	conn := s.opend.Connector()
	h := conn.Health()
	load := conn.Load()
	out := &rpc.OpenDHealth{
		MinServerVersion:  int(futu.MinServerVersion),
		ServerVersion:     int(h.Session.ServerVersion),
		ConnID:            h.Session.ConnID,
		Environment:       string(h.Environment),
		Markets:           h.Markets,
		QuoteRights:       h.QuoteRights,
		SubscriptionsUsed: load.SubscriptionsUsed,
		SubscriptionQuota: load.SubscriptionQuota,
		HistoryUsed:       load.HistoryUsed,
		HistoryQuota:      load.HistoryQuota,
		NotServed:         opendNotServed,
	}
	if h.StateKnown {
		out.ProgramStatus = h.State.ProgramStatus
		out.ProgramDetail = h.State.ProgramDetail
		out.BuildNo = int(h.State.BuildNo)
		out.QuoteLoggedIn = h.State.QuoteLoggedIn
		out.TradeLoggedIn = h.State.TradeLoggedIn
	}
	return out
}

// opendDataHealth rewrites the rows the IBKR connector's absence would misreport.
// A source the broker does not serve is not_applicable with the reason "IBKR only"
// and never counts against the verdict; the session and the account rows read
// OpenD's session and name OpenD as their provider.
func (s *Server) opendDataHealth(rows []rpc.DataSourceHealth, scope string, now time.Time) ([]rpc.DataSourceHealth, string) {
	if s.opend == nil {
		return rows, scope
	}
	health := s.opend.Health()
	ready := health.Ready && !health.BackendDown
	if ready {
		scope = "current"
	}
	for i := range rows {
		row := &rows[i]
		switch {
		case row.ID == "broker_connection":
			row.Provider = "OpenD"
			row.CheckedAt, row.ValidUntil = now, now.Add(dataHealthObservationValidity)
			if ready {
				row.State, row.Receiving, row.Availability = "current", "Connected", "available"
				row.ProblemIDs, row.Action = nil, ""
			} else {
				row.Action = "Canary reconnects automatically when OpenD is available"
			}
		case dataHealthIBKROnly(row.ID):
			*row = rpc.DataSourceHealth{
				ID: row.ID, Name: row.Name, Kind: row.Kind, Provider: "IBKR",
				State: rpc.SourceStatusNotApplicable, Availability: rpc.SourceStatusNotApplicable,
				Receiving: "IBKR only", CheckedAt: now,
			}
		case row.Provider == "IBKR":
			row.Provider = "OpenD"
			row.Name = strings.ReplaceAll(row.Name, "IBKR", "OpenD")
		}
	}
	return rows, scope
}

// dataHealthIBKROnly reports the sources that exist only over IBKR.
func dataHealthIBKROnly(id string) bool {
	if id == "reporting" {
		return true
	}
	for _, name := range []string{"regime", "breadth", "gamma", "chain", "borrow", "shortable", "earnings", "edge", "recon", "hmds"} {
		if strings.Contains(id, name) {
			return true
		}
	}
	return false
}

// opendSubsystems lists the subsystems this broker has.
func (s *Server) opendSubsystems(connected bool) []rpc.SubsystemHealth {
	status := "ready"
	if !connected {
		status = "unavailable"
	}
	return []rpc.SubsystemHealth{
		s.authoritySubsystemHealth(),
		s.configSubsystemHealth(),
		s.watchlistSubsystemHealth(),
		{Name: "quote", Status: status},
		{Name: "history", Status: status},
	}
}

// opendCheckCode is the reference the data check reads: one snapshot request.
const opendCheckCode = "SPY"

// runOpenDDataCheck proves the quote path with one snapshot of a reference name.
func (s *Server) runOpenDDataCheck(ctx context.Context) rpc.DataCheckResult {
	result := rpc.DataCheckResult{StartedAt: s.orderNow(), Detail: "One snapshot of a reference name; other sources report what they last read."}
	if _, err := s.opendQuote(ctx, rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{Symbol: opendCheckCode}}); err != nil {
		result.Deferred = 1
		result.Detail = "OpenD did not answer the reference quote; required checks deferred."
	}
	result.CompletedAt = s.orderNow()
	return result
}

// brokerWritesNotServedCode refuses every broker write under OpenD until phase D.
const brokerWritesNotServedCode = "broker_writes_not_served"
