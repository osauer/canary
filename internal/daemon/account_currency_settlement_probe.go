package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"time"
)

func (s *Server) handleCurrencySettlementProbe(ctx context.Context) (any, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < 3*time.Second {
		return &rpc.AccountCurrencySettlementProbe{Status: "skipped_budget", CancelStatus: "not_started", ClockSource: "local_receive"}, nil
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	session, ok := c.CaptureSession()
	scope := s.currentBrokerStateScope()
	if !ok || !brokerScopeConcrete(scope) {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	probe := c.RequestCurrencySettlementProbe(ctx, 2*time.Second)
	if !c.SessionCurrent(session) || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	return accountCurrencySettlementProbe(probe), nil
}
func accountCurrencySettlementProbe(in ibkrlib.AccountCurrencySettlementProbe) *rpc.AccountCurrencySettlementProbe {
	out := &rpc.AccountCurrencySettlementProbe{Status: in.Status, CancelStatus: in.CancelStatus, ClockSource: in.ClockSource, BrokerErrorCode: in.BrokerErrorCode, SocketEpoch: in.SocketEpoch, RequestedAt: in.RequestedAt, CompletedAt: in.CompletedAt, LastCallbackAt: in.LastCallbackAt, ReadAt: in.ReadAt, Callbacks: in.Callbacks, RowsTruncated: in.RowsTruncated, Rows: make([]rpc.AccountStreamRow, 0, len(in.Rows))}
	for _, row := range in.Rows {
		out.Rows = append(out.Rows, rpc.AccountStreamRow{Key: row.Key, Currency: row.Currency, Source: row.Source, ValueStatus: row.ValueStatus, Callbacks: row.Callbacks, FirstReceivedAt: row.FirstReceivedAt, LastReceivedAt: row.LastReceivedAt})
	}
	return out
}
