//go:build !trading

package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) proposalPlaceOrder(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) proposalSubmitWriteBlockers(origin string) []rpc.TradingBlocker {
	auth := s.brokerWriteAuthorization(s.currentTradingStatus())
	for _, blocker := range liveOriginBlockers(auth.Status, origin) {
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, blocker)
		auth.Allowed = false
	}
	if auth.Allowed {
		return nil
	}
	return auth.Blockers
}
