package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// handleTradeProposalsRequestStop generates a protective trailing-stop
// proposal for one named stock/ETF position now. It is a generation verb: no
// broker write happens here, and the returned key/revision flow into the
// ordinary trade_proposals.preview / trade_proposals.submit gates.
func (s *Server) handleTradeProposalsRequestStop(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalRequestStopResult, error) {
	var p rpc.TradeProposalRequestStopParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalRequestStopResult{
			AsOf:     s.orderNow(),
			Blockers: []rpc.TradingBlocker{{Code: "proposal_engine_unavailable", Message: "proposal engine is unavailable"}},
		}, nil
	}
	res, err := s.tradeProposals.RequestStop(ctx, p)
	return &res, err
}

// RequestStop resolves the position, clears a prior advisory ignore for its
// trailing-stop key, refreshes the snapshot, and returns the trailing-stop
// proposal generated for that position. Every generation-time policy gate
// (stock_protection toggle, duplicate protective orders, spread, reference
// price, market events) still applies as proposal blockers.
func (e *proposalEngine) RequestStop(ctx context.Context, p rpc.TradeProposalRequestStopParams) (rpc.TradeProposalRequestStopResult, error) {
	now := e.clock()
	out := rpc.TradeProposalRequestStopResult{ConID: p.ConID, Symbol: strings.ToUpper(strings.TrimSpace(p.Symbol)), AsOf: now}
	posResult, err := e.server.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		out.Blockers = []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error(), Action: "Retry once the daemon has refreshed positions."}}
		return out, nil
	}
	row, blockers := findReducePosition(posResult, p.ConID, p.Symbol)
	if len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	out.ConID = row.ConID
	out.Symbol = strings.ToUpper(strings.TrimSpace(row.Symbol))
	out.SecType = positionWireSecType(row.SecType)
	if blockers := requestStopPositionBlockers(row); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	if e.server.protectionPolicies != nil {
		policy, policyStatus := e.server.protectionPolicies.Active()
		if policyStatus.Status == rpc.ProtectionPolicyStatusActive || policyStatus.Status == rpc.ProtectionPolicyStatusDefault {
			if !policy.Buckets.TrailingStop.Enabled || !policy.Buckets.TrailingStop.StockETF.Enabled {
				out.Blockers = []rpc.TradingBlocker{{
					Code:    "trailing_stop_bucket_disabled",
					Message: "the protection policy disables stock/ETF trailing-stop proposals",
					Action:  "Enable [buckets.trailing_stop] (and its stock_etf table) in the protection policy.",
				}}
				return out, nil
			}
		}
	}
	// An explicit request for this position's stop outranks a prior advisory
	// dismissal. The clear is persisted so a restart does not resurrect it;
	// when persistence fails the request fails closed, mirroring Ignore.
	key := proposalKey(rpc.TradeProposalBucketTrailingStop, proposalContractFromPosition(row, positionWireSecType(row.SecType)), trailActionForPosition(row.Quantity))
	scope := e.currentScope()
	if brokerScopeConcrete(scope) && e.isIgnored(scope, key) {
		ev := proposalEvent{At: now, Type: "unignored", Key: key, AccountID: scope.Account, AccountMode: scope.Mode, Reason: "stop_requested", Message: "advisory ignore cleared by explicit stop request"}
		if err := e.appendEvent(ev); err != nil {
			out.Blockers = []rpc.TradingBlocker{{Code: "ignore_not_cleared", Message: "the prior advisory ignore for this stop could not be durably cleared", Action: "Retry the stop request."}}
			return out, nil
		}
		e.mu.Lock()
		delete(e.ignored, scopedIgnoreKey(scope, key))
		e.mu.Unlock()
		out.IgnoreCleared = true
	}
	snap, err := e.Refresh(ctx, p.Show)
	if err != nil && len(snap.Proposals) == 0 {
		out.Blockers = snap.Blockers
		return out, err
	}
	out.Snapshot = &snap
	out.Revision = snap.Revision
	for i := range snap.Proposals {
		prop := snap.Proposals[i]
		if prop.Bucket != rpc.TradeProposalBucketTrailingStop || !requestStopProposalMatches(prop, row) {
			continue
		}
		out.ProposalKey = prop.Key
		out.Proposal = &prop
		out.Blockers = mergeTradingBlockers(snap.Blockers, prop.Blockers)
		out.Accepted = len(out.Blockers) == 0
		if out.Accepted {
			out.Message = "trailing-stop proposal generated; preview and submit remain gated"
		}
		e.appendEvent(proposalEventForProposal("stop-requested", prop, now, "", "", "protective stop requested for "+out.Symbol))
		return out, nil
	}
	if len(snap.Blockers) > 0 {
		out.Blockers = snap.Blockers
		return out, nil
	}
	out.Blockers = []rpc.TradingBlocker{{
		Code:    "stop_not_generated",
		Message: fmt.Sprintf("the refreshed snapshot holds no trailing-stop proposal for %s", out.Symbol),
		Action:  "The position may be defunct/unquoted or excluded by policy; check canary proposals list and the protection policy.",
	}}
	return out, nil
}

// requestStopPositionBlockers scopes the verb to whole-share stock/ETF
// positions. Option trailing stops are generated by the ordinary cadence and
// carry their own session/quote gates; V1 keeps the targeted verb stock-only.
func requestStopPositionBlockers(row rpc.PositionView) []rpc.TradingBlocker {
	if !protectionCoveragePositionIsStockLike(row) {
		return []rpc.TradingBlocker{{
			Code:    "unsupported_security_type",
			Message: fmt.Sprintf("%s %s is not a stock/ETF position; targeted stop requests cover stock/ETF only", row.Symbol, positionWireSecType(row.SecType)),
			Action:  "Option trailing stops are proposed automatically; see the protection proposals list.",
		}}
	}
	if qty, _ := closeReduceQuantity(row.Quantity); qty == 0 {
		return []rpc.TradingBlocker{{
			Code:    "position_not_protectable",
			Message: fmt.Sprintf("%s holds less than one whole share; the integer stop path has nothing to protect", row.Symbol),
			Action:  "Fractional remainders stay unprotected under the integer order path.",
		}}
	}
	return nil
}

// requestStopProposalMatches binds a generated trailing-stop proposal back to
// the requested position: ConID when the generation carried one, else exact
// symbol + wire security type.
func requestStopProposalMatches(prop rpc.TradeProposal, row rpc.PositionView) bool {
	if prop.Contract.ConID > 0 && row.ConID > 0 {
		return prop.Contract.ConID == row.ConID
	}
	return strings.EqualFold(strings.TrimSpace(prop.Symbol), strings.TrimSpace(row.Symbol)) &&
		strings.EqualFold(prop.SecType, positionWireSecType(row.SecType))
}
