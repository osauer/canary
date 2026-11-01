package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func validateAutomaticLocksUpdate(in *rpc.AutomaticLocksUpdate) error {
	switch in.Action {
	case "initialize":
		if in.Lock != nil {
			return fmt.Errorf("initialize accepts only a lock list")
		}
		normalized, err := rpc.NormalizeAutomaticLocks(in.Locks)
		in.Locks = normalized
		return err
	case "lock", "unlock":
		if in.Lock == nil || len(in.Locks) != 0 {
			return fmt.Errorf("automatic lock mutation requires one lock")
		}
		_, err := rpc.NormalizeAutomaticLocks([]rpc.AutomaticLock{*in.Lock})
		return err
	default:
		return fmt.Errorf("unknown automatic lock action")
	}
}

func applyAutomaticLocksUpdate(current *rpc.HoldingLocks, in rpc.AutomaticLocksUpdate) ([]rpc.AutomaticLock, bool, error) {
	if in.Action == "initialize" {
		if current.AutomaticLocksInitialized {
			return current.AutomaticLocks, true, nil
		}
		return in.Locks, true, nil
	}
	next := slices.DeleteFunc(slices.Clone(current.AutomaticLocks), func(l rpc.AutomaticLock) bool { return l == *in.Lock })
	if in.Action == "lock" {
		next = append(next, *in.Lock)
	}
	next, err := rpc.NormalizeAutomaticLocks(next)
	return next, true, err
}

func (s *Server) automaticLockBlockers(ctx context.Context, account, mode, symbol string, conID int) []rpc.TradingBlocker {
	locks, err := s.automaticLocks(ctx)
	if err != nil {
		return []rpc.TradingBlocker{{Code: "automatic_locks_unavailable", Message: "Automatic locks cannot be confirmed; automatic orders are held."}}
	}
	if locks.AutomaticMatch(account, mode, symbol, conID) {
		return []rpc.TradingBlocker{{Code: "automatic_locked", Message: "This holding is locked against new automatic orders."}}
	}
	return nil
}

// automaticLocks validates the automatic exclusions independently of the
// keep list. An unreadable automatic list fails closed; an invalid keep-list
// entry must not disable unrelated protective stops. Both lists still share
// the same accepted document and serialized mutation receipts.
func (s *Server) automaticLocks(ctx context.Context) (rpc.HoldingLocks, error) {
	out := rpc.HoldingLocks{Version: 1, Locks: []rpc.HoldingLock{}}
	if s == nil || s.coreStore == nil {
		return out, fmt.Errorf("automatic locks durability unavailable")
	}
	err := s.coreStore.WithAcceptedStateDocument(ctx, daemonStateScope, stateKindHoldingLocks, func(doc corestore.StateDocument, found bool) error {
		out.AsOf = s.orderNow().UTC()
		if !found {
			return nil
		}
		var data holdingLocksDocument
		if decodeStrictPlatformSettingsJSON(doc.JSON, &data) != nil || data.Version != 1 || doc.Revision <= 0 {
			return fmt.Errorf("invalid automatic locks document")
		}
		var err error
		out.AutomaticLocks, err = rpc.NormalizeAutomaticLocks(data.AutomaticLocks)
		out.AutomaticLocksInitialized = data.AutomaticLocksInitialized
		out.Revision = doc.Revision
		return err
	})
	return out, err
}

func (e *proposalEngine) automaticLockBlockers(ctx context.Context, p rpc.TradeProposal) []rpc.TradingBlocker {
	if e == nil || e.server == nil {
		return nil
	}
	scope := e.currentScope()
	return e.server.automaticLockBlockers(ctx, scope.Account, scope.Mode, p.Contract.Symbol, p.Contract.ConID)
}

func (s *Server) automaticWireLockBlockers(ctx context.Context, binding brokerWriteTransactionBinding, cancel bool) []rpc.TradingBlocker {
	if cancel {
		return nil
	}
	// The existing stop guard must still shrink an oversized working stop
	// after a sale, including a full-position mandate's "resize". Its fresh
	// request-scoped grant binds the exact order and prior working quantity
	// under brokerWriteMu. Only a strict reduction is exempt; equal-sized or
	// growing protection remains subject to the automatic exclusion.
	if binding.origin == rpc.OrderOriginDaemonProtectiveGuard && binding.riskBound && binding.orderEndpoint != "" {
		grant := s.protectiveStopGuardGrant.Load()
		if grant != nil && (grant.Kind == protectiveStopGuardShrink || grant.Kind == protectiveStopGuardResize) &&
			grant.OrderRef != "" && grant.OrderRef == binding.riskDraft.OrderRef && grant.ReservedOrderID == binding.orderID &&
			grant.Quantity > 0 && grant.Quantity == binding.riskDraft.Quantity && positiveFinite(grant.Remaining) && float64(grant.Quantity) < grant.Remaining {
			return nil
		}
	}
	switch binding.origin {
	case rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginDaemonProtectiveGuard, rpc.OrderOriginDeskAutomatic:
		if !binding.riskBound {
			return []rpc.TradingBlocker{{Code: "automatic_locks_unavailable", Message: "The automatic order's contract cannot be confirmed."}}
		}
		return s.automaticLockBlockers(ctx, binding.scope.Account, binding.scope.Mode, binding.riskDraft.Contract.Symbol, binding.riskDraft.Contract.ConID)
	}
	return nil
}
