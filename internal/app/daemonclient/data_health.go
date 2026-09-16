package daemonclient

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
)

// DataHealthClient is the optional passive source-health capability.
type DataHealthClient interface {
	DataHealth(context.Context, rpc.DataHealthParams) (*rpc.DataHealthResult, error)
}

// DataHealth returns one bounded page without acquiring upstream data.
func (c Real) DataHealth(ctx context.Context, p rpc.DataHealthParams) (*rpc.DataHealthResult, error) {
	var out rpc.DataHealthResult
	if err := c.call(ctx, rpc.MethodDataHealth, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
