package daemonclient

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// DataHealthClient is the optional passive source-health capability.
type DataHealthClient interface {
	DataHealth(context.Context, rpc.DataHealthParams) (*rpc.DataHealthResult, error)
}

// DataHealth returns one bounded page without acquiring upstream data.
func (c Real) DataHealth(ctx context.Context, p rpc.DataHealthParams) (*rpc.DataHealthResult, error) {
	var out rpc.DataHealthResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.DataHealth, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
