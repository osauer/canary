//go:build trading

package daemon

import (
	"context"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

func (s *Server) proposalPlaceOrder(ctx context.Context, p rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return s.placeOrder(ctx, p)
}

// proposalCancelOrder is the queued executor's broker cancel: an exit plan's
// triggered trail cancelling its own first sale, under the grant that names
// it (queuedGrantCoversCancel).
func (s *Server) proposalCancelOrder(ctx context.Context, orderRef string) error {
	_, err := s.cancelOrderForAction(ctx, rpc.OrderCancelParams{ID: orderRef, Origin: rpc.OrderOriginDaemonOwnerQueued}, corestore.ActionCancel)
	return err
}

func (s *Server) proposalSubmitWriteBlockers(origin string) []rpc.TradingBlocker {
	auth := s.brokerWriteAuthorizationForRequest(origin)
	if auth.Allowed {
		return nil
	}
	return auth.Blockers
}
