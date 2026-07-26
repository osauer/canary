//go:build trading

package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) proposalPlaceOrder(ctx context.Context, p rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return s.placeOrder(ctx, p)
}

func (s *Server) proposalSubmitWriteBlockers(origin string) []rpc.TradingBlocker {
	auth := s.brokerWriteAuthorizationForRequest(origin)
	if auth.Allowed {
		return nil
	}
	return auth.Blockers
}
