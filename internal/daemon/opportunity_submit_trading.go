//go:build trading

package daemon

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
)

// submitOptionExercise is intentionally fail-closed. Exercising an option can
// open, increase, reduce, or flip the underlying exposure, while the current
// opportunity policy labels that effect informational and supplies neither an
// approved notional/short-risk interpretation nor a durable one-shot submit
// identity. Treating the opportunity as broker-write authority would invent a
// policy and permit duplicate irreversible instructions.
func (s *Server) submitOptionExercise(ctx context.Context, _ rpc.Opportunity, _ int, _ string, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: option exercise submission is unavailable because exact option-to-underlying risk policy and one-shot authority are not approved; exercise manually in TWS after reviewing the resulting position", ErrTradingDisabled)
}
