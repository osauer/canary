package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type reduceHeldPosition struct {
	contract rpc.ContractParams
	quantity float64
}

func (s *Server) reduceFillBinding() protectionOrderSnapshotBinding {
	binding := s.currentProtectionOrderSnapshotBinding()
	if s.reduceFillGenerationForTest != nil {
		binding.generation = s.reduceFillGenerationForTest()
	}
	return binding
}

func (s *Server) reduceFillBindingCurrent(binding protectionOrderSnapshotBinding) bool {
	current := s.reduceFillBinding()
	if binding != current {
		return false
	}
	if s.reduceFillSnapshotForTest != nil || (binding.connector == nil && s.hasOrderPreviewBrokerTestSeam()) {
		return true
	}
	return binding.connector != nil && binding.connector.SessionCurrent(binding.session)
}

// A fill can arrive before updatePortfolio changes the held quantity. A new
// valuation tick is not proof that the fill is reflected. For affected lines,
// require a completed, request-correlated positions receipt to agree with
// the stream; do not release filled capacity against an older quantity. The
// lifecycle frontier also binds the read and the eventual first broker byte.
func (s *Server) ensureReduceFillCoherence(ctx context.Context, held []reduceHeldPosition, timeoutMs int) (protectionOrderSnapshotBinding, error) {
	return s.ensureOrderFillCoherence(ctx, held, timeoutMs, "")
}

// ensureOrderFillCoherence also checks purchase fills for premium admission.
// Empty action retains the reducing side selected from each held quantity.
func (s *Server) ensureOrderFillCoherence(ctx context.Context, held []reduceHeldPosition, timeoutMs int, requestedAction string) (protectionOrderSnapshotBinding, error) {
	binding := s.reduceFillBinding()
	var inventory journalInventory
	load := func() bool {
		var ok bool
		inventory, ok = s.journalOrderViewsForInventory()
		return ok
	}
	loaded := false
	if binding.connector == nil && s.hasOrderPreviewBrokerTestSeam() {
		loaded = load()
	} else if binding.connector != nil {
		// A callback advances the lifecycle counter before journaling. The
		// exclusive evidence barrier drains that publication before we scan,
		// and validates the frontier captured before the scan began.
		evidence, ok := binding.connector.CaptureBrokerEvidence()
		if ok && evidence.Session == binding.session && evidence.OrderLifecycleGeneration == binding.generation {
			loaded = binding.connector.WithStableBrokerEvidence(evidence, load)
		}
	}
	if !loaded {
		return binding, fmt.Errorf("%w: reduction fill evidence changed or cannot be read; preview again", ErrTradingDisabled)
	}
	check := make(map[int]float64)
	// Purchase callers supply exactly the contracts with observed fills,
	// including all-client snapshot rows that have no local order token.
	if requestedAction != "" {
		for _, row := range held {
			check[row.contract.ConID] = row.quantity
		}
	}
	for i := range inventory.views {
		view := &inventory.views[i]
		if view.Filled == 0 || view.ReservedOrderID <= 0 || view.OrderRef == "" || view.PreviewTokenID == "" ||
			!orderViewMatchesBrokerScope(*view, binding.scope) {
			continue
		}
		order := journalOrderIdentity(*view)
		order.Remaining, order.Legs = view.Filled, inventory.legsOf(view)
		for _, row := range held {
			action := requestedAction
			if action == "" {
				action = rpc.OrderActionSell
				if row.quantity < 0 {
					action = rpc.OrderActionBuy
				}
			}
			quantity := order.competesWith(row.contract, action)
			if math.IsNaN(quantity) || math.IsInf(quantity, 0) {
				return binding, fmt.Errorf("%w: filled combo quantities are unavailable; reconcile orders before reducing", ErrTradingDisabled)
			}
			if quantity > 0 {
				check[row.contract.ConID] = row.quantity
			}
		}
	}
	if len(check) == 0 {
		if !s.reduceFillBindingCurrent(binding) {
			return binding, fmt.Errorf("%w: broker orders changed during reduction position check; preview again", ErrTradingDisabled)
		}
		return binding, nil
	}
	snapshot, err := s.orderFillPositionsReceipt(ctx, binding, timeoutMs)
	if err != nil {
		return binding, err
	}

	quantities := make(map[int]float64)
	for _, row := range snapshot {
		if row == nil || !strings.EqualFold(row.Account, binding.scope.Account) {
			continue
		}
		if _, wanted := check[row.Contract.ConID]; !wanted {
			continue
		}
		if _, duplicate := quantities[row.Contract.ConID]; duplicate || math.IsNaN(row.Position) || math.IsInf(row.Position, 0) {
			return binding, fmt.Errorf("%w: broker reduction position snapshot is ambiguous", ErrTradingDisabled)
		}
		quantities[row.Contract.ConID] = row.Position
	}
	for conID, quantity := range check {
		if math.Abs(quantities[conID]-quantity) > 1e-9 {
			return binding, fmt.Errorf("%w: the held quantity has not caught up with broker fills; wait for positions to agree before reducing", ErrTradingDisabled)
		}
	}
	return binding, nil
}

// orderFillPositionsReceipt is the request-correlated account receipt shared by
// subset reduction checks and the hedge's complete-book quantity check. It
// never runs under the wire guard; the captured lifecycle frontier must survive
// the request before its quantities can establish admission authority.
func (s *Server) orderFillPositionsReceipt(ctx context.Context, binding protectionOrderSnapshotBinding, timeoutMs int) ([]*ibkrlib.RawPosition, error) {
	var snapshot []*ibkrlib.RawPosition
	var err error
	if s.reduceFillSnapshotForTest != nil {
		snapshot, err = s.reduceFillSnapshotForTest(ctx, orderPreviewTimeout(timeoutMs))
	} else if binding.connector != nil {
		snapshot, err = binding.connector.RefreshPositionsForSession(ctx, binding.session, orderPreviewTimeout(timeoutMs))
	} else {
		err = fmt.Errorf("broker position snapshot unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("%w: filled reduction position unconfirmed: %v", ErrTradingDisabled, err)
	}
	if !s.reduceFillBindingCurrent(binding) {
		return nil, fmt.Errorf("%w: broker orders changed during reduction position check; preview again", ErrTradingDisabled)
	}
	return snapshot, nil
}
