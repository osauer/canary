package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleOpportunitiesStatus() *rpc.OpportunityStatus {
	st := s.opportunityStatus()
	return &st
}

func (s *Server) handleOpportunitiesSnapshot(req *rpc.Request) *rpc.OpportunitySnapshot {
	var p rpc.OpportunitySnapshotParams
	_ = decodeParams(req.Params, &p)
	if s.opportunities == nil {
		snap := emptyOpportunitySnapshot(s.orderNow())
		return &snap
	}
	snap := s.opportunities.Snapshot(p.Show)
	return &snap
}

func (s *Server) handleOpportunitiesRefresh(ctx context.Context, req *rpc.Request) (*rpc.OpportunitySnapshot, error) {
	var p rpc.OpportunityRefreshParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		snap := emptyOpportunitySnapshot(s.orderNow())
		return &snap, nil
	}
	snap, err := s.opportunities.Refresh(ctx, p.Show)
	return &snap, err
}

func (s *Server) handleOpportunitiesPreviewExercise(ctx context.Context, req *rpc.Request) (*rpc.OpportunityExercisePreviewResult, error) {
	var p rpc.OpportunityExercisePreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		return &rpc.OpportunityExercisePreviewResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "opportunity_engine_unavailable", Message: "opportunity engine is unavailable"}}}, nil
	}
	res, err := s.opportunities.Preview(ctx, p)
	return &res, err
}

func (s *Server) handleOpportunitiesSubmitExercise(ctx context.Context, req *rpc.Request) (*rpc.OpportunityExerciseSubmitResult, error) {
	var p rpc.OpportunityExerciseSubmitParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		return &rpc.OpportunityExerciseSubmitResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "opportunity_engine_unavailable", Message: "opportunity engine is unavailable"}}}, nil
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	res, err := s.opportunities.Submit(ctx, p)
	return &res, err
}

func (s *Server) handleOpportunitiesIgnore(req *rpc.Request) *rpc.OpportunityIgnoreResult {
	var p rpc.OpportunityIgnoreParams
	_ = decodeParams(req.Params, &p)
	if s.opportunities == nil {
		return &rpc.OpportunityIgnoreResult{Accepted: false, Key: p.Key, Revision: p.Revision, Message: "opportunity engine is unavailable", AsOf: s.orderNow()}
	}
	res := s.opportunities.Ignore(p)
	return &res
}

func (s *Server) opportunityStatus() rpc.OpportunityStatus {
	now := s.orderNow()
	cfg := s.cfg.Opportunities.WithDefaults()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	trading := s.tradingStatus(ep)
	policy := rpc.OpportunityPolicyStatus{Status: rpc.OpportunityPolicyStatusDisabled}
	if s.opportunityPolicies != nil {
		policy = s.opportunityPolicies.Status()
	}
	out := rpc.OpportunityStatus{
		Kind:           rpc.OpportunityStatusKind,
		AsOf:           now,
		Enabled:        cfg.EnabledResolved(),
		HotReload:      cfg.HotReloadEnabled(),
		ReloadInterval: cfg.ReloadIntervalDuration().String(),
		RefreshCadence: cfg.RefreshCadenceDuration().String(),
		Policy:         policy,
		Trading:        trading,
	}
	if !out.Enabled {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "opportunities_disabled", Message: "opportunities are disabled by config"})
	}
	if policy.Status == rpc.OpportunityPolicyStatusDrift || policy.Status == rpc.OpportunityPolicyStatusError {
		out.Blockers = append(out.Blockers, policy.Blockers...)
	}
	out.Blocked = len(out.Blockers) > 0
	return out
}
