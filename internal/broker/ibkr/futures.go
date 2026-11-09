package ibkr

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// FrontContract resolves a futures root to the dated contract IBKR lists as
// its front month: the connector's FrontFuture, the call the market overview
// has always made, with its own three-second timeout and resolution cache.
func (a *Adapter) FrontContract(ctx context.Context, in broker.Instrument, now time.Time) (broker.Instrument, error) {
	if a == nil || a.conn == nil {
		return broker.Instrument{}, ibkrlib.ErrIBKRUnavailable
	}
	c, err := a.conn.FrontFuture(ctx, Contract(in), now)
	if err != nil {
		return broker.Instrument{}, err
	}
	return Instrument(c), nil
}
