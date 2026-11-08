package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	"github.com/osauer/canary/v2/internal/stress"
)

// handleStressSnapshot serves the portfolio Stress assessment composed in the
// daemon from the same reads a reader would make, so the CLI, MCP, the app
// and the exported Go client all show the installed daemon's verdict.
func (s *Server) handleStressSnapshot(ctx context.Context, req *rpc.Request) (*rpc.StressSnapshotResult, error) {
	var p rpc.StressSnapshotParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, paramsFor(api.StressSnapshot, &p)); err != nil {
			return nil, errBadRequest("stress.snapshot params: " + err.Error())
		}
	}
	res, regime, err := stress.Compose(ctx, stressComposeReader{server: s})
	if err != nil {
		return nil, err
	}
	out := &rpc.StressSnapshotResult{Stress: res}
	if p.RegimeMonitor {
		monitor := rpc.CompactRegimeMonitor(&regime)
		out.RegimeMonitor = &monitor
	}
	return out, nil
}

// handlePositionsRisk serves the compact portfolio-risk view of
// positions.list. The daemon flags the option legs, so readers show the
// installed daemon's thresholds rather than the ones compiled into them.
func (s *Server) handlePositionsRisk(ctx context.Context, req *rpc.Request) (*rpc.PositionsRiskResult, error) {
	res, err := s.handlePositionsList(ctx, req)
	if err != nil {
		return nil, err
	}
	out := rpc.CompactPositionsRisk(res, 5)
	// The keep list resolved against this very book, so a reader marks the
	// locked lines the way every sweep sees them.
	if locks, err := s.holdingLocks(ctx); err != nil {
		out.HoldingLocksUnavailable = "the locked holdings could not be read: " + err.Error()
	} else {
		locks.Decorate(res)
		out.HoldingLocks = &locks
	}
	return &out, nil
}

// stressComposeReader answers Compose's reads from the daemon's own handlers,
// each under its usual deadline. Results pass through their wire encoding so
// the composition sees exactly what a remote reader saw.
type stressComposeReader struct{ server *Server }

func (r stressComposeReader) Call(ctx context.Context, method string, params, out any) error {
	req := &rpc.Request{Method: method}
	if params != nil {
		buf, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = buf
	}
	ctx, cancel := requestCtx(ctx, method)
	defer cancel()
	var res any
	var err error
	switch method {
	case rpc.MethodAccountSummary:
		res, err = r.server.handleAccountSummaryRequest(ctx, req)
	case rpc.MethodPositionsList:
		res, err = r.server.handlePositionsList(ctx, req)
	case rpc.MethodRegimeSnapshot:
		res, err = r.server.handleRegimeSnapshot(ctx, req)
	case rpc.MethodMarketEventsSnapshot:
		res, err = r.server.handleMarketEventsSnapshot(ctx, req)
	case rpc.MethodRulesSnapshot:
		res, err = r.server.handleRulesSnapshot(ctx, req)
	default:
		return fmt.Errorf("stress.snapshot: no daemon read for %s", method)
	}
	if err != nil {
		return err
	}
	buf, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, out)
}

// handleScorecardRead serves the decision scorecard the worker last saved
// (internal-docs/design/decision-scorecard.md). Readers never score: the
// daemon's publication is the verdict, bounded to one sleeve and a decision
// page on request.
func (s *Server) handleScorecardRead(ctx context.Context, req *rpc.Request) (*rpc.ScorecardResult, error) {
	var p rpc.ScorecardReadParams
	if err := decodeFor(api.ScorecardRead, req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeScorecardReadParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		out := scorecardStateOnlyResult(rpc.ScorecardStateUnavailable, "flex_configuration_required")
		return &out, nil
	}
	if s.coreStore == nil {
		return nil, fmt.Errorf("scorecard authority is unavailable")
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		out := scorecardStateOnlyResult(rpc.ScorecardStateUnavailable, "account_scope_unavailable")
		return &out, nil
	}
	publication, ok, err := s.loadScorecardPublication(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		out := scorecardStateOnlyResult(rpc.ScorecardStateBackfilling, "snapshot_pending")
		return &out, nil
	}
	if publication.ScopeFingerprint != edgeScopeFingerprint(scope) {
		out := scorecardStateOnlyResult(rpc.ScorecardStateUnavailable, "account_scope_changed")
		return &out, nil
	}
	out, err := scorecardBoundResult(publication.Result, p)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// scorecardBoundResult narrows a publication to one sleeve and one decision
// page without changing any figure; the sleeve evidence keeps its full
// sample.
func scorecardBoundResult(result rpc.ScorecardResult, p rpc.ScorecardReadParams) (rpc.ScorecardResult, error) {
	out := result
	if p.Sleeve != "" {
		known := false
		sleeves := make([]rpc.ScorecardSleeve, 0, len(out.Sleeves))
		for _, sleeve := range out.Sleeves {
			if sleeve.Sleeve == p.Sleeve {
				known = true
				sleeves = append(sleeves, sleeve)
			}
		}
		if !known && len(out.Sleeves) > 0 {
			return rpc.ScorecardResult{}, errBadRequest("scorecard sleeve is unknown: " + p.Sleeve)
		}
		decisions := make([]rpc.ScorecardDecision, 0, len(out.Decisions))
		for _, decision := range out.Decisions {
			if decision.Sleeve == p.Sleeve {
				decisions = append(decisions, decision)
			}
		}
		out.Sleeves, out.Decisions = sleeves, decisions
	}
	out.DecisionsTotal = len(out.Decisions)
	if len(out.Decisions) > p.Limit {
		out.Decisions = append([]rpc.ScorecardDecision(nil), out.Decisions[:p.Limit]...)
	}
	return out, nil
}
