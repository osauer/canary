package canary

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// SetupMarkoutsParams filters the entry-markout read by order, fill date or symbol.
type SetupMarkoutsParams = rpc.SetupMarkoutsParams

// SetupMarkoutsResult is the entry-markout ledger and its capture clock.
type SetupMarkoutsResult = rpc.SetupMarkoutsResult

// SetupMarkoutTarget is one pending, captured or missing markout target.
type SetupMarkoutTarget = rpc.SetupMarkoutTarget

// SetupMarkouts reads daemon-captured entry markouts. They are an entry
// diagnostic, not realized profit; the read schedules or captures nothing.
func (c *Client) SetupMarkouts(ctx context.Context, in SetupMarkoutsParams) (*SetupMarkoutsResult, error) {
	budget, _ := rpc.LookupMethodTiming(rpc.MethodSetupsMarkouts)
	ctx, cancel := context.WithTimeout(ctx, budget.ClientTimeout(5*time.Second))
	defer cancel()
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out SetupMarkoutsResult
	err = conn.Call(ctx, rpc.MethodSetupsMarkouts, in, &out)
	return &out, daemonError(err)
}
