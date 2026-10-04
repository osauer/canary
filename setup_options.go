package canary

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// SetupOptionsParams chooses a listing stage or an exact standard call tuple.
type SetupOptionsParams = rpc.SetupOptionsParams

// SetupOptionsResult is bounded option discovery without execution authority.
type SetupOptionsResult = rpc.SetupOptionsResult

// SetupOptionExpiry is one observed standard-class expiry.
type SetupOptionExpiry = rpc.SetupOptionExpiry

// SetupOptionCall is one strike with optional indicative quote evidence.
type SetupOptionCall = rpc.SetupOptionCall

// SetupOptionQuote is the one dated quote for the exact selected call; its
// bid and ask are present only when its status is quoted.
type SetupOptionQuote = rpc.SetupOptionQuote

// DiscoverSetupOptions lists or resolves standard calls without preparing an order.
func (c *Client) DiscoverSetupOptions(ctx context.Context, in SetupOptionsParams) (*SetupOptionsResult, error) {
	budget, _ := rpc.LookupMethodTiming(rpc.MethodSetupsOptions)
	ctx, cancel := context.WithTimeout(ctx, budget.ClientTimeout(5*time.Second))
	defer cancel()
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out SetupOptionsResult
	err = conn.Call(ctx, rpc.MethodSetupsOptions, in, &out)
	return &out, daemonError(err)
}
