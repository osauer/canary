//go:build !trading

package daemon

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) submitOptionExercise(ctx context.Context, _ orderPreviewTokenPayload, _ rpc.Opportunity, _ int, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: option exercise submit is unavailable in the read-only build", ErrTradingDisabled)
}
