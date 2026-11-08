package scorecard

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// Fetch reads the daemon's decision scorecard. The daemon evaluates it, so a
// reader compiled at an older version still shows the installed daemon's
// verdict.
func Fetch(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, params rpc.ScorecardReadParams) (rpc.ScorecardResult, error) {
	params, err := rpc.NormalizeScorecardReadParams(params)
	if err != nil {
		return rpc.ScorecardResult{}, err
	}
	var out rpc.ScorecardResult
	if err := rpc.Call(ctx, conn, api.ScorecardRead, params, &out); err != nil {
		return rpc.ScorecardResult{}, err
	}
	return out, nil
}
