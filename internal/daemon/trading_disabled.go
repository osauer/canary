//go:build !trading

package daemon

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const orderWritesAvailable = false

func (s *Server) handleOrderPlace(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) handleOrderModify(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) handleOrderCancel(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) handleTradingPaperSmoke(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

//lint:ignore U1000 Keeps the non-trading build API aligned with trading builds.
func (s *Server) reserveBoundBrokerOrderID(ctx context.Context, binding brokerWriteTransactionBinding) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return 0, err
	}
	if s.orderReserveBrokerID != nil {
		return s.orderReserveBrokerID(ctx)
	}
	return 0, ErrTradingDisabled
}

//lint:ignore U1000 Keeps the non-trading build API aligned with trading builds.
func (s *Server) submitBoundConfiguredOrder(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.orderWriteBeforeBrokerSend != nil {
		s.orderWriteBeforeBrokerSend()
	}
	return s.withBoundBrokerWriteTransaction(binding, func() error {
		auth := s.brokerWriteAuthorization(status)
		if !auth.Allowed {
			return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
		}
		if s.orderPlaceBroker != nil {
			return s.orderPlaceBroker(ctx, contract, order)
		}
		return ErrTradingDisabled
	})
}
