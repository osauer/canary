package daemon

import (
	"context"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	purgeStatusNoOrders  = "no_orders"
	purgeStatusActive    = "active"
	purgeStatusOpen      = "open"
	purgeStatusFilled    = "filled"
	purgeStatusClosed    = "closed"
	purgeStatusAttention = "attention"
)

func (s *Server) handlePurgeStatus(_ context.Context, req *rpc.Request) (*rpc.PurgeStatusResult, error) {
	var p rpc.PurgeStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return nil, err
	}
	scope := s.currentBrokerStateScope()
	if strings.TrimSpace(p.Account) != "" {
		scope.Account = strings.TrimSpace(p.Account)
	}
	rows, totals, err := s.purgeLedger.Snapshot(scope, strings.TrimSpace(p.PurgeID))
	if err != nil {
		return nil, err
	}
	res := &rpc.PurgeStatusResult{
		Kind:    "ibkr.purge_status",
		PurgeID: strings.TrimSpace(p.PurgeID),
		Account: scope.Account,
		Status:  purgeStatusNoOrders,
		Rows:    rows,
		Totals:  totals,
		AsOf:    s.orderNow(),
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	for _, view := range views {
		if !strings.EqualFold(view.Source, purgeExecuteSource) && !strings.EqualFold(view.Source, purgeRestoreSource) {
			continue
		}
		if res.PurgeID != "" && !strings.EqualFold(view.PurgeID, res.PurgeID) {
			continue
		}
		if !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		res.TotalOrders++
		if view.Open {
			res.OpenOrders++
		}
		switch view.LifecycleStatus {
		case rpc.OrderLifecycleFilled:
			res.FilledOrders++
		case rpc.OrderLifecycleCancelled:
			res.CancelledOrders++
		case rpc.OrderLifecycleRejected, rpc.OrderLifecycleInactive, rpc.OrderLifecycleUnknownReconcileRequired:
			res.AttentionOrders++
		}
		if len(res.Orders) < limit {
			res.Orders = append(res.Orders, view)
		}
	}
	res.Status = purgeStatusFromCounts(*res)
	res.Message = purgeStatusMessage(*res)
	return res, nil
}

func purgeStatusFromCounts(res rpc.PurgeStatusResult) string {
	switch {
	case res.TotalOrders == 0:
		if res.Totals.ActiveRows > 0 {
			return purgeStatusActive
		}
		return purgeStatusNoOrders
	case res.AttentionOrders > 0:
		return purgeStatusAttention
	case res.OpenOrders > 0:
		return purgeStatusOpen
	case res.Totals.ActiveRows > 0:
		return purgeStatusActive
	case res.FilledOrders == res.TotalOrders:
		return purgeStatusFilled
	default:
		return purgeStatusClosed
	}
}

func purgeStatusMessage(res rpc.PurgeStatusResult) string {
	if res.TotalOrders == 0 {
		if res.Totals.ActiveRows > 0 {
			return "purge ledger has active rows with no open purge/restore orders"
		}
		if res.PurgeID != "" {
			return "no locally tracked purge/restore orders matched " + res.PurgeID
		}
		return "no locally tracked purge/restore orders"
	}
	if res.AttentionOrders > 0 {
		return "one or more purge/restore orders need reconciliation"
	}
	if res.OpenOrders > 0 {
		return "purge/restore orders are still working"
	}
	if res.Totals.ActiveRows > 0 {
		return "purge ledger has remaining quantity to restore"
	}
	if res.FilledOrders == res.TotalOrders {
		return "all tracked purge/restore orders are filled"
	}
	return "tracked purge/restore orders are no longer open"
}
