package daemon

import (
	"context"
	"math"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A mandate belongs to one owner-signed queue and its resulting broker order.
// Neither an arbitrary existing stop nor a later holding inherits it.
func (s *Server) fullPositionStopMandates(scope brokerStateScope, views []rpc.OrderView) map[string]bool {
	out := map[string]bool{}
	if s == nil || s.tradeProposals == nil {
		return out
	}
	for _, r := range s.tradeProposals.queued.list() {
		if r.State == rpc.QueuedAuthSent && r.OrderRef != "" && ownerQueueProtection(r.Terms) && r.MandateEndedAt.IsZero() &&
			!r.CancelRequested && sameBrokerScope(r.scope(), scope) && queuedRecordProblem(r) == "" {
			for _, v := range views {
				if orderViewMatchesBrokerScope(v, scope) && v.OrderRef == r.OrderRef && v.PreviewTokenID == r.PreviewTokenID &&
					v.ConID == r.Terms.Contract.ConID && v.Action == r.Terms.Action && (r.PermID == 0 || v.PermID == r.PermID) {
					out[r.OrderRef] = true
				}
			}
		}
	}
	return out
}

func (s *Server) fullPositionResizeGranted(view rpc.OrderView, quantity int) bool {
	g := s.protectiveStopGuardGrant.Load()
	return g != nil && g.Kind == protectiveStopGuardResize && g.OrderRef == view.OrderRef && g.ReservedOrderID == view.ReservedOrderID &&
		g.Quantity == quantity && g.Filled == view.Filled && s.fullPositionStopMandates(s.currentBrokerStateScope(), []rpc.OrderView{view})[view.OrderRef]
}

// The existing modify validator remains shrink-only. Its sole growth
// exception is this request-scoped, mandate-backed, quantity-only resize.
func (s *Server) validateModifyDraft(view rpc.OrderView, draft rpc.OrderDraft) error {
	if !s.fullPositionResizeGranted(view, draft.Quantity) {
		return validateModifyDraft(view, draft)
	}
	g := s.protectiveStopGuardGrant.Load()
	if !trailSpecsEquivalent(g.Trail, draft.Trail) || view.Trail == nil || draft.Trail.InitialStopPrice < view.Trail.InitialStopPrice {
		return errBadRequest("full-position resize must preserve the current trail")
	}
	unchanged := *draft.Trail
	unchanged.InitialStopPrice = view.Trail.InitialStopPrice
	if !trailSpecsEquivalent(view.Trail, &unchanged) {
		return errBadRequest("full-position resize cannot change trail settings")
	}
	view.Remaining = math.Max(view.Remaining, float64(draft.Quantity))
	return validateModifyDraft(view, draft)
}

func (s *Server) validateProtectiveModifyQuantity(view rpc.OrderView, draft rpc.OrderDraft, current float64) error {
	// The guard has just replanned this exact shared-capacity allocation under
	// brokerWriteMu. Preserve that bound rather than expanding to the whole
	// holding. Normal position/inventory authority still reaches the wire guard.
	if view.EntryLink != nil {
		g := s.protectiveStopGuardGrant.Load()
		if g != nil && g.Kind == protectiveStopGuardShrink && g.OrderRef == view.OrderRef && g.ReservedOrderID == view.ReservedOrderID && g.Quantity == draft.Quantity && g.Filled == view.Filled && g.Position == current && draft.Quantity > 0 && float64(draft.Quantity) <= current {
			return validateModifyDraft(view, draft)
		}
	}
	if s.fullPositionResizeGranted(view, draft.Quantity) {
		if current <= 0 || float64(draft.Quantity) != current {
			return errBadRequest("full-position coverage must equal the current position")
		}
		return nil
	}
	return validateProtectiveModifyQuantity(view, draft, current)
}

// A complete current flat position terminates the mandate, even while
// trading is frozen. A missing or stale portfolio cannot terminate it.
func (e *proposalEngine) endFlatProtectionQueues(ctx context.Context) {
	if e.server == nil {
		return
	}
	scope := e.currentScope()
	positions, ok := e.server.protectiveStopGuardPositions(ctx, scope)
	if !ok {
		return
	}
	flat := map[string]bool{}
	for _, r := range e.queued.list() {
		if !ownerQueueProtection(r.Terms) || !sameBrokerScope(r.scope(), scope) || !r.MandateEndedAt.IsZero() {
			continue
		}
		qty, err := exactRiskPositionQuantity(positions, r.Terms.Contract)
		if err == nil && qty <= 0 {
			flat[r.Terms.QueueID] = true
		}
	}
	if len(flat) == 0 {
		return
	}
	now := e.clock()
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for id := range flat {
			r := records[id]
			if r == nil || !r.MandateEndedAt.IsZero() {
				continue
			}
			r.MandateEndedAt = now
			attempted := r.OrderRef != "" || r.PreviewTokenID != ""
			if attempted {
				r.CancelRequested = true
			}
			if !attempted && (r.waiting() || r.State == rpc.QueuedAuthPrepared) {
				events = append(events, r.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonPositionChanged, "the position closed; protection mandate ended", now))
			} else {
				events = append(events, r.note("mandate_ended", r.State, now))
			}
		}
		return events
	})
	if err != nil {
		e.server.warnf("end closed-position protection mandates: %v", err)
	}
}

// The broker modify API expects total quantity, including prior fills. The
// preview and risk checks deliberately approve remaining coverage.
func protectionModifyTotal(remaining int, filled float64) (int, error) {
	if remaining <= 0 || filled < 0 || math.Trunc(filled) != filled || math.IsNaN(filled) || math.IsInf(filled, 0) || filled > 1000000 {
		return 0, errBadRequest("cannot verify filled quantity for protection resize")
	}
	return remaining + int(filled), nil
}
