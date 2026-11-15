package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func validateProtectedAddScope(scope string, d rpc.OrderDraft) error {
	if scope != rpc.OrderTokenScopeProtectedAdd {
		if d.AttachedStop != nil || d.EntryLink != nil || d.FixedStopPrice != 0 {
			return errBadRequest("attached stop requires a protected Add token")
		}
		return nil
	}
	if d.AttachedStop == nil || d.Add == nil || d.Contract.SecType != "STK" || d.Contract.ConID <= 0 || d.Action != "BUY" || d.OrderType != "LMT" || d.TIF != "DAY" || d.OutsideRTH || d.Trail != nil || d.StrategyGroup != nil || d.EntryLink != nil || d.FixedStopPrice != 0 || d.Quantity < 1 || d.Quantity > 1000000 || !positiveFinite(d.LimitPrice) || !positiveFinite(d.AttachedStop.Price) || d.AttachedStop.Price >= d.LimitPrice {
		return errBadRequest("invalid protected stock Add terms")
	}
	return nil
}

func (s *Server) protectedAddMandateConflict(c rpc.ContractParams) bool {
	if s.tradeProposals == nil {
		return false
	}
	scope := s.currentBrokerStateScope()
	for _, r := range s.tradeProposals.queued.list() {
		if ownerQueueProtection(r.Terms) && sameBrokerScope(r.scope(), scope) && r.Terms.Contract.ConID == c.ConID && r.MandateEndedAt.IsZero() && !r.CancelRequested && (r.waiting() || r.State == rpc.QueuedAuthPrepared || r.State == rpc.QueuedAuthSent) {
			return true
		}
	}
	return false
}

func (s *Server) validateProtectedStockAdd(ctx context.Context, a *orderPreviewBrokerAuthority, d rpc.OrderDraft) error {
	if err := validateProtectedAddScope(rpc.OrderTokenScopeProtectedAdd, d); err != nil {
		return err
	}
	if s.protectedAddMandateConflict(d.Contract) {
		return errBadRequest("an active full-position protection mandate overlaps this added-lot stop; review that mandate first")
	}
	if a == nil || a.connector == nil {
		return errBadRequest("exact broker price increments required for the attached stop")
	}
	exact, err := a.connector.ResolveOrderContractForSession(ctx, a.session, *previewIBKRContract(d.Contract), 3*time.Second)
	if err != nil {
		return err
	}
	rules, err := a.connector.OrderPriceIncrements(ctx, a.session, exact)
	if err != nil {
		return err
	}
	for _, p := range []float64{d.LimitPrice, d.AttachedStop.Price} {
		valid := false
		for i, r := range rules {
			if p >= r.LowEdge && (i == len(rules)-1 || p < rules[i+1].LowEdge) {
				n := (p - r.LowEdge) / r.Increment
				valid = math.Abs(n-math.Round(n)) < 1e-6
				break
			}
		}
		if !valid {
			return fmt.Errorf("price %.8g is not on the exact broker price grid", p)
		}
	}
	return nil
}

func (s *Server) hasAttachedStockStop(c rpc.ContractParams) bool {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return true
	}
	scope := s.currentBrokerStateScope()
	for _, v := range views {
		if orderViewMatchesBrokerScope(v, scope) && v.ConID == c.ConID && v.EntryLink != nil && v.Open {
			return true
		}
	}
	return false
}

func stockEntryLinksEqual(a, b *rpc.StockEntryLink) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Preview and submission must describe the same cumulative broker quantity.
func attachedStockStopModifyOrder(view rpc.OrderView, draft rpc.OrderDraft, status rpc.TradingStatus) (*ibkrlib.RawOrder, error) {
	if err := validateModifyDraft(view, draft); err != nil {
		return nil, err
	}
	order, err := modifyPreviewWhatIfOrder(view, draft, status)
	if err != nil {
		return nil, err
	}
	order.OcaGroup, order.OcaType = view.OCAGroup, view.OCAType
	return order, nil
}
