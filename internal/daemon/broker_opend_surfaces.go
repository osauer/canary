package daemon

import (
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
var opendNotServed = []string{"Edge", "reconciliation", "statement reporting", "borrow availability", "earnings (WSH)", "breadth", "cash sweep"}

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
			if ready && len(row.DerivedFrom) > 0 {
				row.DerivedFrom = nil
			}
		}
	}
	return rows, scope
}

// dataHealthIBKROnly reports the sources that exist only over IBKR.
func dataHealthIBKROnly(id string) bool {
	if id == "reporting" {
		return true
	}
	for _, prefix := range []string{"regime:gamma", "regime:breadth", "breadth", "gamma", "chain", "borrow", "earnings", "edge", "recon", "hmds"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
