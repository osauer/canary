package daemon

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Protective stop guard (owner decision 2026-10-05 17:36 CEST; see
// internal-docs/design/protective-stop-guard.md).
//
// On the proposal cadence the daemon keeps every working sell stop it placed
// on a stock no larger than the current long position: a flat or short
// position cancels the stop, a smaller long position shrinks it to the
// position with every other term unchanged. It acts only on a current,
// complete portfolio projection and a complete current broker open-order
// inventory for the connected account, and only after the same step was
// planned on two consecutive passes at least protectiveStopGuardSettle apart,
// so a partial download or an out-of-order fill callback never moves an
// order. Orders Canary did not place are reported, never touched. Every write
// goes through the ordinary modify or cancel path with the
// daemon-protective-guard origin, so every gate still decides: trading mode,
// gateway, account pins, journal and storage health refuse both, and
// trading.freeze refuses a shrink (a modify) but never a cancel.

const (
	// protectiveStopGuardSettle is how long one planned step must persist
	// across consecutive passes before the guard acts on it.
	protectiveStopGuardSettle = 20 * time.Second
	// protectiveStopGuardReadTimeout bounds one pass's open-order read.
	protectiveStopGuardReadTimeout = 10 * time.Second
	// protectiveStopGuardWriteTimeout bounds one shrink's quote and WhatIf.
	protectiveStopGuardWriteTimeout = 5 * time.Second

	protectiveStopGuardShrink = "shrink"
	protectiveStopGuardCancel = "cancel"
	protectiveStopGuardReport = "report"

	// Decision-log outcomes of the guard.
	protectiveStopGuardOutcomeSent     = "sent"
	protectiveStopGuardOutcomeFailed   = "failed"
	protectiveStopGuardOutcomeBlocked  = "blocked"
	protectiveStopGuardOutcomeReported = "reported"
)

// protectiveStopGuardStep is one planned correction of one working stop.
type protectiveStopGuardStep struct {
	Kind      string
	Order     ibkrlib.OrderLifecycleEvent
	View      rpc.OrderView // the journal row; zero for an order Canary did not place
	Position  float64
	Remaining float64
	// Target is the shrunk quantity; zero for a cancel or a report.
	Target int
	// Reason says, without account or order numbers, why the step exists.
	Reason string
}

// key identifies the step across passes: the order and what would be done.
func (st protectiveStopGuardStep) key() string {
	return fmt.Sprintf("%s|%d|%d|%s|%d", st.Kind, st.Order.PermID, st.Order.OrderID, st.View.OrderRef, st.Target)
}

// protectiveStopGuardWriteGrant is the request-scoped fact the write gate
// checks for the daemon-protective-guard origin: which order the guard is
// changing right now and how. It exists only while the guard holds
// brokerWriteMu.
type protectiveStopGuardWriteGrant struct {
	Kind            string
	OrderRef        string
	ReservedOrderID int
	Quantity        int
}

// protectiveStopGuardState is the guard's in-memory pass state.
type protectiveStopGuardState struct {
	seen map[string]time.Time
	// logged remembers the last logged reason per step key so a waiting step
	// logs once per change, not every pass.
	logged map[string]string
	// paused is the last reason the guard could not evaluate at all.
	paused string
}

// protectiveStopGuardStockOrder reports whether a working broker order is a
// sell stop on a stock or ETF.
func protectiveStopGuardStockOrder(order ibkrlib.OrderLifecycleEvent) bool {
	secType := strings.TrimSpace(order.SecType)
	if secType != "" && !isStockLikeRiskSecType(secType) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(order.Action), rpc.OrderActionSell) && isProtectiveStopOrderType(order.OrderType)
}

// protectiveStopGuardCanaryPlaced reports whether the journal row is an
// order Canary placed through its own preview path in this account and mode.
// The write path still checks the endpoint and client pins.
func protectiveStopGuardCanaryPlaced(view *rpc.OrderView, scope brokerStateScope) bool {
	if view == nil || !view.Open || view.ReservedOrderID <= 0 || strings.TrimSpace(view.OrderRef) == "" || strings.TrimSpace(view.PreviewTokenID) == "" {
		return false
	}
	return orderViewMatchesBrokerScope(*view, scope)
}

// planProtectiveStopGuard plans the corrections for one complete inventory
// against one current position projection. It is pure: the caller has
// already proven both inputs current and in scope.
func planProtectiveStopGuard(working []brokerWorkingOrder, positions []*ibkrlib.RawPosition, scope brokerStateScope) []protectiveStopGuardStep {
	var steps []protectiveStopGuardStep
	for _, row := range working {
		order := row.Order
		if !protectiveStopGuardStockOrder(order) {
			continue
		}
		remaining := brokerOrderRemaining(order)
		if remaining <= 0 {
			continue
		}
		contract := rpc.ContractParams{ConID: order.ConID, Symbol: order.Symbol, SecType: "STK", Currency: order.Currency}
		if row.Journal != nil {
			contract.ConID = cmp.Or(contract.ConID, row.Journal.ConID)
			contract.Symbol = cmp.Or(contract.Symbol, row.Journal.Symbol)
			contract.Currency = cmp.Or(contract.Currency, row.Journal.Currency)
		}
		step := protectiveStopGuardStep{Order: order, Remaining: remaining}
		if row.Journal != nil {
			step.View = *row.Journal
		}
		position, err := exactRiskPositionQuantity(positions, contract)
		if err != nil {
			// Ambiguous position evidence never moves an order.
			continue
		}
		step.Position = position
		if position+1e-9 >= remaining {
			continue
		}
		switch {
		case !protectiveStopGuardCanaryPlaced(row.Journal, scope):
			step.Kind = protectiveStopGuardReport
			step.Reason = fmt.Sprintf("a sell stop Canary did not place would sell %.4g shares while %.4g are held; it is yours to change", remaining, position)
		case math.Floor(position+1e-9) < 1:
			step.Kind = protectiveStopGuardCancel
			step.Reason = fmt.Sprintf("the position is %.4g shares, so the stop for %.4g would open a short; cancelling it", position, remaining)
		default:
			step.Kind = protectiveStopGuardShrink
			step.Target = int(math.Floor(position + 1e-9))
			step.Reason = fmt.Sprintf("the position is %.4g shares, so the stop for %.4g is shrunk to %d", position, remaining, step.Target)
		}
		steps = append(steps, step)
	}
	slices.SortStableFunc(steps, func(a, b protectiveStopGuardStep) int { return strings.Compare(a.key(), b.key()) })
	return steps
}

// protectiveStopGuardPositions returns the current, complete portfolio
// projection for scope, or false. It applies the same trust boundary as the
// order write path: a completed account-scoped receipt, no short download,
// fresh, and every row in scope.
func (s *Server) protectiveStopGuardPositions(ctx context.Context, scope brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
	if s == nil || ctx == nil || !brokerScopeConcrete(scope) {
		return nil, false
	}
	if s.protectiveStopGuardPositionsForTest != nil {
		return s.protectiveStopGuardPositionsForTest(ctx, scope)
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, false
	}
	session, ok := c.CaptureSession()
	if !ok {
		return nil, false
	}
	projection, ok := c.CapturePortfolioProjectionForSession(session)
	if !ok || classifyPortfolioStreamHealth(scope, projection.Health, s.orderNow()) != orderIntegrityHealthCurrent ||
		!cachedPositionsMatchBrokerScope(projection.Positions, scope) || !c.SessionCurrent(session) {
		return nil, false
	}
	return projection.Positions, true
}

// runProtectiveStopGuard is one guard pass. It runs after each proposal
// refresh in the engine's Run loop.
func (s *Server) runProtectiveStopGuard(ctx context.Context) {
	if s == nil || ctx == nil || ctx.Err() != nil || !s.orderBrokerWritesEnabled() {
		return
	}
	now := s.orderNow()
	status := s.currentTradingStatus()
	scope := s.currentBrokerStateScope()
	steps, paused := s.planProtectiveStopGuardPass(ctx, scope)
	s.protectiveStopGuardMu.Lock()
	state := &s.protectiveStopGuard
	if state.seen == nil {
		state.seen = map[string]time.Time{}
		state.logged = map[string]string{}
	}
	if paused != "" {
		// No evidence, no decision: the settle clock restarts once evidence
		// returns, so a step is never carried across an evidence gap.
		clear(state.seen)
		changed := state.paused != paused
		state.paused = paused
		s.protectiveStopGuardMu.Unlock()
		if changed {
			s.infof("protective stop guard: paused: %s", paused)
		}
		return
	}
	resumed := state.paused != ""
	state.paused = ""
	present := make(map[string]struct{}, len(steps))
	var due []protectiveStopGuardStep
	for _, step := range steps {
		k := step.key()
		present[k] = struct{}{}
		first, ok := state.seen[k]
		if !ok {
			state.seen[k] = now
			continue
		}
		if now.Sub(first) >= protectiveStopGuardSettle {
			due = append(due, step)
		}
	}
	for k := range state.seen {
		if _, ok := present[k]; !ok {
			delete(state.seen, k)
			delete(state.logged, k)
		}
	}
	s.protectiveStopGuardMu.Unlock()
	if resumed {
		s.infof("protective stop guard: resumed")
	}
	for _, step := range due {
		if ctx.Err() != nil {
			return
		}
		s.applyProtectiveStopGuardStep(ctx, status, step)
	}
}

// planProtectiveStopGuardPass reads the evidence for one pass. A non-empty
// paused names why the guard cannot evaluate at all.
func (s *Server) planProtectiveStopGuardPass(ctx context.Context, scope brokerStateScope) ([]protectiveStopGuardStep, string) {
	if !brokerScopeConcrete(scope) {
		return nil, "no concrete connected account session"
	}
	positions, ok := s.protectiveStopGuardPositions(ctx, scope)
	if !ok {
		return nil, "the current, complete portfolio projection is unavailable"
	}
	readCtx, cancel := context.WithTimeout(ctx, protectiveStopGuardReadTimeout)
	snapshot, inventoryScope, err := s.brokerOpenOrderInventory(readCtx, false)
	cancel()
	if err != nil || !sameBrokerScope(inventoryScope, scope) {
		return nil, "the broker's complete open-order inventory is unavailable"
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return nil, "the order journal is unavailable"
	}
	return planProtectiveStopGuard(brokerWorkingOrders(snapshot, views, scope), positions, scope), ""
}

// applyProtectiveStopGuardStep reports, or acts on, one settled step.
func (s *Server) applyProtectiveStopGuardStep(ctx context.Context, status rpc.TradingStatus, step protectiveStopGuardStep) {
	if step.Kind == protectiveStopGuardReport {
		if s.protectiveStopGuardFirstLog(step.key(), step.Reason) {
			s.warnf("protective stop guard: %s", step.Reason)
			s.recordProtectiveStopGuardDecision(step, status, protectiveStopGuardOutcomeReported, nil, step.Reason)
		}
		return
	}
	var auth brokerWriteAuthorization
	if step.Kind == protectiveStopGuardCancel {
		s.mu.Lock()
		ep := s.endpoint
		s.mu.Unlock()
		auth = s.brokerCancelAuthorization(s.tradingStatusForCancel(ep))
	} else {
		auth = s.brokerWriteAuthorization(s.currentTradingStatus())
	}
	if !auth.Allowed {
		// Kill switch, broker or storage unavailable, or (for a shrink) the
		// freeze: do nothing, and say so once per reason.
		reason := step.Reason + "; not done: " + firstTradingBlockerMessage(auth.Blockers)
		if s.protectiveStopGuardFirstLog(step.key(), reason) {
			s.warnf("protective stop guard: %s", reason)
			s.recordProtectiveStopGuardDecision(step, status, protectiveStopGuardOutcomeBlocked, auth.Blockers, reason)
		}
		return
	}
	err := s.protectiveStopGuardWrite(ctx, step)
	if err != nil {
		reason := step.Reason + "; failed: " + err.Error()
		if s.protectiveStopGuardFirstLog(step.key(), reason) {
			s.warnf("protective stop guard: %s", reason)
			s.recordProtectiveStopGuardDecision(step, status, protectiveStopGuardOutcomeFailed, nil, reason)
		}
		return
	}
	s.infof("protective stop guard: %s (order %s)", step.Reason, step.View.OrderRef)
	s.recordProtectiveStopGuardDecision(step, status, protectiveStopGuardOutcomeSent, nil, step.Reason)
	s.protectiveStopGuardMu.Lock()
	delete(s.protectiveStopGuard.seen, step.key())
	delete(s.protectiveStopGuard.logged, step.key())
	s.protectiveStopGuardMu.Unlock()
}

// protectiveStopGuardFirstLog reports whether reason is new for key.
func (s *Server) protectiveStopGuardFirstLog(key, reason string) bool {
	s.protectiveStopGuardMu.Lock()
	defer s.protectiveStopGuardMu.Unlock()
	if s.protectiveStopGuard.logged == nil {
		s.protectiveStopGuard.logged = map[string]string{}
	}
	if s.protectiveStopGuard.logged[key] == reason {
		return false
	}
	s.protectiveStopGuard.logged[key] = reason
	return true
}

// recordProtectiveStopGuardDecision appends one guard decision to the
// decision log. It carries the order reference, never an account number.
func (s *Server) recordProtectiveStopGuardDecision(step protectiveStopGuardStep, status rpc.TradingStatus, outcome string, blockers []rpc.TradingBlocker, reason string) {
	if s == nil || s.decisions == nil {
		return
	}
	ev := decisionEvent{TS: s.orderNow(), Svc: "canary", Event: "protective_stop_guard." + step.Kind, Outcome: outcome,
		Mode: status.Mode, IDs: decisionIDs{OrderRef: step.View.OrderRef}, Reason: boundedDecisionReason(reason)}
	for _, b := range blockers {
		if code := strings.TrimSpace(b.Code); code != "" && !slices.Contains(ev.Codes, code) {
			ev.Codes = append(ev.Codes, code)
		}
	}
	if err := s.decisions.append(ev); err != nil {
		s.warnf("protective stop guard: append %s: %v", decisionLogFile, err)
	}
}

// protectiveStopGuardOriginBlockers is the origin policy for the guard's own
// writes: accepted only while the guard holds a grant for one exact order.
func (s *Server) protectiveStopGuardOriginBlockers() []rpc.TradingBlocker {
	if s == nil || s.protectiveStopGuardGrant.Load() == nil {
		return []rpc.TradingBlocker{{Code: "daemon_origin_unauthorised",
			Message: "daemon-protective-guard writes are issued only by the daemon's protective stop guard for one of its own stops",
			Action:  "Modify or cancel the order by hand instead."}}
	}
	return nil
}

// protectiveStopGuardGrantCovers is the per-order half of the origin policy,
// checked once the write path has resolved the order: the guard may cancel
// the granted order, or shrink it to exactly the granted quantity.
func (s *Server) protectiveStopGuardGrantCovers(origin, kind string, view rpc.OrderView, quantity int) error {
	if origin != rpc.OrderOriginDaemonProtectiveGuard {
		return nil
	}
	grant := s.protectiveStopGuardGrant.Load()
	if grant == nil || grant.Kind != kind || grant.OrderRef != view.OrderRef || grant.ReservedOrderID != view.ReservedOrderID ||
		(kind == protectiveStopGuardShrink && (grant.Quantity != quantity || float64(quantity) >= orderViewRemainingQuantity(view))) {
		return fmt.Errorf("%w: the protective stop guard holds no grant for this order change", ErrTradingDisabled)
	}
	return nil
}
