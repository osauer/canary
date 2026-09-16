package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"time"
)

// Capture successful producer authority without issuing a second read. Market
// values and account identity never enter the health record.
func (s *Server) observeAccountRPC(value any, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding) {
	var authority *rpc.AccountDataAuthority
	var id, name string
	switch r := value.(type) {
	case *rpc.AccountResult:
		if r == nil {
			return
		}
		authority = r.Authority
		id, name = "account", "Account fields"
	case *rpc.PositionsResult:
		if r == nil {
			return
		}
		authority = r.Authority
		id, name = "positions", "Position and valuation data"
	default:
		return
	}
	if authority == nil || c == nil || !c.SessionCurrent(binding) {
		return
	}
	row := unknownDataSource(id, name, "IBKR", "account_data", "Portfolio display and assessments")
	now := s.orderNow()
	row.CheckedAt, row.ReceivedAt, row.ValidUntil = now, authority.AsOf, now.Add(time.Minute)
	row.Detail = string(authority.Reason)
	switch {
	case authority.Availability == rpc.AccountDataUnavailable:
		row.State, row.Receiving = "unavailable", "Producer authority unavailable"
	case authority.Availability == rpc.AccountDataAvailable && authority.Freshness == rpc.AccountDataFreshnessCurrent:
		row.State, row.Receiving = "current", "Current producer authority"
	case authority.Freshness == rpc.AccountDataFreshnessStale:
		row.State, row.Receiving = "limited", "Stale producer authority"
	}
	if row.State == "unavailable" || row.State == "limited" {
		row.ProblemIDs = []string{id}
	}
	row.Action = "Managed by Canary's account-data producer"
	s.recordDataHealth(row, c, binding, true)
}
