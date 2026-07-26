package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleAutoTradeStatus() *rpc.AutoTradeStatus {
	st := s.autoTradeStatus()
	return &st
}

func (s *Server) handleTradeProposalsSnapshot(req *rpc.Request) *rpc.TradeProposalSnapshot {
	var p rpc.TradeProposalSnapshotParams
	_ = decodeParams(req.Params, &p)
	if s.tradeProposals == nil {
		snap := emptyProposalSnapshot(s.orderNow())
		return &snap
	}
	snap := s.tradeProposals.Snapshot(p.Show)
	return &snap
}

func (s *Server) handleTradeProposalsRefresh(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalSnapshot, error) {
	var p rpc.TradeProposalRefreshParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		snap := emptyProposalSnapshot(s.orderNow())
		return &snap, nil
	}
	snap, err := s.tradeProposals.Refresh(ctx, p.Show)
	return &snap, err
}

func (s *Server) handleTradeProposalsPreview(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPreviewResult, error) {
	var p rpc.TradeProposalPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPreviewResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "proposal_engine_unavailable", Message: "proposal engine is unavailable"}}}, nil
	}
	res, err := s.tradeProposals.Preview(ctx, p)
	return &res, err
}

func (s *Server) handleTradeProposalsSubmit(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalSubmitResult, error) {
	var p rpc.TradeProposalSubmitParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalSubmitResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "proposal_engine_unavailable", Message: "proposal engine is unavailable"}}}, nil
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	res, err := s.tradeProposals.Submit(ctx, p)
	return &res, err
}

func (s *Server) handleTradeProposalsIgnore(req *rpc.Request) *rpc.TradeProposalIgnoreResult {
	var p rpc.TradeProposalIgnoreParams
	_ = decodeParams(req.Params, &p)
	if s.tradeProposals == nil {
		return &rpc.TradeProposalIgnoreResult{Accepted: false, Key: p.Key, Revision: p.Revision, Message: "proposal engine is unavailable", AsOf: s.orderNow()}
	}
	res := s.tradeProposals.Ignore(p)
	return &res
}

func (s *Server) autoTradeStatus() rpc.AutoTradeStatus {
	now := s.orderNow()
	cfg := s.cfg.AutoTrade.WithDefaults()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	trading := s.tradingStatus(ep)
	policy := rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDisabled}
	if s.protectionPolicies != nil {
		policy = s.protectionPolicies.Status()
	}
	out := rpc.AutoTradeStatus{
		Kind:             "ibkr.auto_trade_status",
		AsOf:             now,
		Trading:          trading,
		ProposalsEnabled: cfg.ProposalsEnabledResolved(),
		FastPathEnabled:  cfg.FastPathEnabledResolved(),
		HotReload:        cfg.HotReloadEnabled(),
		ReloadInterval:   cfg.ReloadIntervalDuration().String(),
		ProposalCadence:  cfg.ProposalCadenceDuration().String(),
		Policy:           policy,
	}
	if !out.ProposalsEnabled {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "proposals_disabled", Message: "manual proposals are disabled by config"})
	}
	if policy.Status == rpc.ProtectionPolicyStatusDrift || policy.Status == rpc.ProtectionPolicyStatusError {
		out.Blockers = append(out.Blockers, policy.Blockers...)
	}
	out.Blocked = len(out.Blockers) > 0
	return out
}
