package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

// handleStressSnapshot serves the portfolio Stress assessment composed in the
// daemon from the same reads a reader would make, so the CLI, MCP, the app
// and the exported Go client all show the installed daemon's verdict.
func (s *Server) handleStressSnapshot(ctx context.Context, req *rpc.Request) (*rpc.StressSnapshotResult, error) {
	var p rpc.StressSnapshotParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, &p); err != nil {
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
