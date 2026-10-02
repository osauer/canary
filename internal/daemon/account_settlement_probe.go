package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func (s *Server) handleAccountSummaryRequest(ctx context.Context, req *rpc.Request) (any, error) {
	var params rpc.AccountSummaryParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, errors.New("invalid account summary params")
		}
	}
	if !params.SettlementProbe {
		return s.handleAccountSummary(ctx)
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
	return buildSettlementComparison(ctx,
		func(readCtx context.Context, budget time.Duration) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
			return c.RequestAccountSummaryWithProvenance(readCtx, budget)
		},
		c.RequestSettlementCashProbe,
		func() bool { return c.SessionCurrent(session) && sameBrokerScope(scope, s.currentBrokerStateScope()) })
}

func buildSettlementComparison(ctx context.Context, normal func(context.Context, time.Duration) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error), probeRead func(context.Context, time.Duration) ibkrlib.AccountSettlementProbe, current func() bool) (*rpc.AccountSettlementComparison, error) {
	// Reserve three seconds for the bounded probe plus ordered cancellation.
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < 4*time.Second {
		return &rpc.AccountSettlementComparison{NormalStatus: "skipped_budget", Probe: rpc.AccountSettlementProbe{Status: "skipped_budget", CancelStatus: "not_started"}, AsOf: time.Now().UTC()}, nil
	}
	normalBudget := min(time.Until(deadline)-3*time.Second, 6*time.Second)
	normalCtx, cancel := context.WithTimeout(ctx, normalBudget)
	raw, provenance, err := normal(normalCtx, normalBudget)
	cancel()
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	if !current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	result := &rpc.AccountSettlementComparison{NormalStatus: "completed", NormalObservation: accountSettlementObservation(raw, provenance)}
	if provenance != ibkrlib.AccountSummaryProvenanceRequest {
		result.NormalStatus = "completed_without_request_rows"
	}
	probe := probeRead(ctx, 2*time.Second)
	result.Probe = rpc.AccountSettlementProbe{Status: probe.Status, CancelStatus: probe.CancelStatus, BrokerErrorCode: probe.BrokerErrorCode}
	if probe.Observation != nil {
		result.Probe.Observation = accountSettlementObservation(&ibkrlib.RawAccountSummary{SettlementObservation: probe.Observation}, ibkrlib.AccountSummaryProvenanceRequest)
	}
	if !current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	result.AsOf = time.Now().UTC()
	return result, nil
}
