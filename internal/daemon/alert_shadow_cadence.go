package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// alertShadowObservationEvery is an engineering heartbeat, not a market or
// delivery threshold. It keeps alert producer coverage inside the
// shortest one-minute silence horizon even when no UI or CLI client is open.
const alertShadowObservationEvery = 30 * time.Second

func (s *Server) startAlertShadowObservationLoops(ctx context.Context) {
	if s == nil || s.alertShadow == nil || ctx == nil {
		return
	}
	s.alertShadowLoopWG.Add(6)
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowRegimeLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowProtectionLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowDataHealthLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowNudgesLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowRulebookLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowOrderIntegrityLoop(ctx)
	}()
}

func (s *Server) runAlertShadowRegimeLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		observeCtx, cancel := context.WithTimeout(ctx, regimeSnapshotRefreshTimeout)
		_, err := s.handleRegimeSnapshot(observeCtx, &rpc.Request{})
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Debugf("alert producer: Regime heartbeat unavailable: %v", err)
		}
	}
}

func (s *Server) runAlertShadowProtectionLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.observeProtectionAlertShadowHeartbeat(ctx)
	}
}

func (s *Server) runAlertShadowDataHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_ = s.statusHealthSnapshot()
	}
}

// Risk Policy, Reconciliation, and Governance deliberately share the one
// canonical Nudge evaluation. Keeping this heartbeat on that boundary avoids
// reassembling policy gates or treating a skipped/failed dependency as clean.
func (s *Server) runAlertShadowNudgesLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.observeNudgesAlertShadowHeartbeat(ctx)
	}
}

func (s *Server) observeNudgesAlertShadowHeartbeat(ctx context.Context) {
	s.observeNudgesAlertShadowHeartbeatWith(ctx, s.composeNudgesSnapshotContextWithAuthority)
}

type alertShadowNudgeCompose func(context.Context, *alertShadowNudgeInput) (rpc.NudgesSnapshotResult, error)

func (s *Server) observeNudgesAlertShadowHeartbeatWith(ctx context.Context, compose alertShadowNudgeCompose) {
	if s == nil || s.alertShadow == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	brokerScope := s.currentBrokerStateScope()
	shadowScope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil || compose == nil {
		return
	}
	observeCtx, cancel := requestCtx(ctx, rpc.MethodNudgesSnapshot)
	var input alertShadowNudgeInput
	result, err := compose(observeCtx, &input)
	readErr := observeCtx.Err()
	cancel()
	if err != nil && ctx.Err() == nil {
		s.debugf("alert producer: Nudge heartbeat unavailable: %v", err)
		return
	}
	if readErr != nil || ctx.Err() != nil || !sameBrokerScope(brokerScope, s.currentBrokerStateScope()) || input.Scope != shadowScope {
		return
	}

	// Keep the same mandatory canonical wire boundary as nudges.snapshot.
	// This heartbeat is another caller of the composition, not another owner
	// of Nudge copy or validation semantics.
	wire, err := json.Marshal(result)
	if err != nil {
		s.debugf("alert producer: Nudge heartbeat canonical marshal unavailable: %v", err)
		return
	}
	var canonical rpc.NudgesSnapshotResult
	if err := json.Unmarshal(wire, &canonical); err != nil {
		s.debugf("alert producer: Nudge heartbeat canonical decode unavailable: %v", err)
		return
	}
	if ctx.Err() != nil || !sameBrokerScope(brokerScope, s.currentBrokerStateScope()) {
		return
	}
	input.Snapshot = canonical
	s.observeNudgesAlertShadow(ctx, input)
}

// The 30-second heartbeat is projection-only. It never issues account,
// positions, quote, Greeks, earnings, or regime reads; it projects the latest
// scope/generation-bound canonical result or an explicit uncovered result.
func (s *Server) runAlertShadowRulebookLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.observeRulebookAlertShadowHeartbeat(ctx)
	}
}

func (s *Server) observeRulebookAlertShadowHeartbeat(ctx context.Context) {
	s.observeRulebookAlertShadowHeartbeatWith(ctx, func(context.Context, bool, bool) (*rpc.RulesResult, bool) {
		binding := s.currentRulebookBinding()
		if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
			return cached, true
		}
		return s.rulebookUnavailableResult("canonical_cache_missing_or_stale"), true
	})
}

type alertShadowRulebookEvaluate func(context.Context, bool, bool) (*rpc.RulesResult, bool)

type alertShadowReadContext func(context.Context) (context.Context, context.CancelFunc)

func (s *Server) observeRulebookAlertShadowHeartbeatWith(ctx context.Context, evaluate alertShadowRulebookEvaluate) {
	s.observeRulebookAlertShadowHeartbeatWithReadContext(ctx, evaluate, func(parent context.Context) (context.Context, context.CancelFunc) {
		return requestCtx(parent, rpc.MethodRulesSnapshot)
	})
}

func (s *Server) observeRulebookAlertShadowHeartbeatWithReadContext(ctx context.Context, evaluate alertShadowRulebookEvaluate, derive alertShadowReadContext) {
	if s == nil || s.alertShadow == nil || ctx == nil || ctx.Err() != nil || evaluate == nil || derive == nil {
		return
	}
	binding := s.currentRulebookBinding()
	scope := binding.scope
	if !brokerScopeConcrete(scope) {
		return
	}
	readCtx, cancel := derive(ctx)
	readCtx = suppressProtectionAlertShadowObservation(readCtx)
	result, evaluated := evaluate(readCtx, true, false)
	readErr := readCtx.Err()
	cancel()
	if !evaluated {
		return
	}
	if ctx.Err() != nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return
	}
	if readErr != nil {
		if result == nil {
			s.debugf("alert producer: Rulebook heartbeat unavailable: %v", readErr)
			return
		}
		unavailable := *result
		unavailable.AsOf = s.orderNow().UTC()
		unavailable.Status = "degraded"
		unavailable.Rules = nil
		unavailable.InputHealth = []rpc.SourceHealth{
			{Source: "account", Status: "unavailable", AsOf: unavailable.AsOf, Notes: []string{"heartbeat request did not complete"}},
			{Source: "positions", Status: "unavailable", AsOf: unavailable.AsOf, Notes: []string{"heartbeat request did not complete"}},
		}
		result = &unavailable
		s.debugf("alert producer: Rulebook heartbeat unavailable: %v", readErr)
	}
	if binding.connector == nil {
		// A Connector may appear while an unbound evaluation is in flight. Even
		// if that read happens to look complete, it has no before/after session
		// frontier and therefore cannot recover an episode.
		unavailable := cloneRulesResult(result)
		if unavailable.Status == "ok" {
			unavailable.Status = "degraded"
		}
		s.observeRulebookAlertShadow(ctx, unavailable, scope)
		return
	}
	if !binding.brokerCaptured {
		unavailable := cloneRulesResult(result)
		unavailable.Status = "degraded"
		unavailable.Rules = nil
		s.observeRulebookAlertShadow(ctx, unavailable, scope)
		return
	}
	committed, commitErr := s.withStableBrokerEvidence(daemonBrokerEvidenceBinding{
		scope: binding.scope, connector: binding.connector, connectorEpoch: binding.connectorEpoch, broker: binding.broker,
	}, func() error { return s.commitRulebookAlertShadow(ctx, result, scope) })
	if commitErr != nil {
		s.warnf("alert producer: Rulebook heartbeat commit failed: %v", commitErr)
	} else if !committed {
		s.debugf("alert producer: Rulebook heartbeat evidence changed before commit")
	}
}

// Order Integrity reuses the exact journal/portfolio read model behind
// orders.open. The read is scoped only after reconciliation, just as the RPC
// handler is, and only current portfolio-stream evidence can make an empty
// open-order set a trustworthy negative. A failed read is observed explicitly
// as unavailable so it cannot recover a prior mismatch during the silence
// horizon.
func (s *Server) runAlertShadowOrderIntegrityLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.observeOrderIntegrityAlertShadowHeartbeat(ctx)
	}
}

type alertShadowOrderIntegrityRead func(context.Context) ([]rpc.OrderView, orderIntegrityEvaluation, error)

func (s *Server) observeOrderIntegrityAlertShadowHeartbeat(ctx context.Context) {
	// Same bounded rebuild-on-drop as the protection heartbeat: a stable
	// commit dropped for advancing broker or journal evidence starves the
	// silence horizon if the only retry is the next 30s tick.
	for range protectionHeartbeatStableAttempts {
		settled := s.observeOrderIntegrityAlertShadowHeartbeatWith(ctx, func(readCtx context.Context) ([]rpc.OrderView, orderIntegrityEvaluation, error) {
			views, _, evaluation, err := s.loadOrderViewsReconciledWithHealth(readCtx)
			return views, evaluation, err
		})
		if settled || ctx == nil || ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) observeOrderIntegrityAlertShadowHeartbeatWith(ctx context.Context, read alertShadowOrderIntegrityRead) bool {
	return s.observeOrderIntegrityAlertShadowHeartbeatWithReadContext(ctx, read, func(parent context.Context) (context.Context, context.CancelFunc) {
		return requestCtx(parent, rpc.MethodOrdersOpen)
	})
}

// The false return means the stable-evidence commit dropped the observation
// because evidence advanced mid-flight; every other outcome reports true.
func (s *Server) observeOrderIntegrityAlertShadowHeartbeatWithReadContext(ctx context.Context, read alertShadowOrderIntegrityRead, derive alertShadowReadContext) bool {
	if s == nil || s.alertShadow == nil || ctx == nil || ctx.Err() != nil || read == nil || derive == nil {
		return true
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return true
	}
	readCtx, cancel := derive(ctx)
	readCtx = suppressProtectionAlertShadowObservation(readCtx)
	views, evaluation, err := read(readCtx)
	readErr := readCtx.Err()
	cancel()
	if ctx.Err() != nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		// Same starvation shape as a commit drop: nothing was observed this
		// pass. Let the bounded retry rebuild against the current scope.
		return false
	}
	if readErr != nil {
		err = readErr
	}
	if err != nil {
		evaluation = orderIntegrityEvaluation{
			AsOf: s.orderNow().UTC(), Status: orderIntegrityHealthUnavailable,
			Scope: scope, Orders: []rpc.OrderView{},
		}
		s.observeOrderIntegrityAlertShadow(ctx, evaluation)
		s.debugf("alert producer: Order Integrity heartbeat unavailable: %v", err)
		return true
	}
	if !sameBrokerScope(scope, evaluation.Scope) {
		return false
	}

	orders := make([]rpc.OrderView, 0, len(views))
	for _, view := range views {
		if view.Open && orderViewMatchesBrokerScope(view, evaluation.Scope) {
			orders = append(orders, view)
		}
	}
	sortOrderViews(orders)
	evaluation.Orders = orders
	if s.orderLifecyclePersistenceUncertain.Load() {
		evaluation.Status = orderIntegrityHealthUnavailable
	}
	return s.observeOrderIntegrityAlertShadow(ctx, evaluation)
}

type suppressProtectionAlertShadowObservationKey struct{}

func suppressProtectionAlertShadowObservation(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressProtectionAlertShadowObservationKey{}, true)
}

// protectionHeartbeatStableAttempts bounds the immediate rebuilds after a
// stable-evidence commit drop. A busy portfolio stream can advance its
// generation inside every read-to-commit window; with only the 30s tick to
// retry, consecutive drops starve the one-minute silence horizon and the
// composer reports producer_silent for a healthy producer. Retrying rebuilds
// from the advanced evidence — never re-submitting the dropped observation —
// and the bound keeps a hot stream from pinning the loop.
const protectionHeartbeatStableAttempts = 3

func (s *Server) observeProtectionAlertShadowHeartbeat(ctx context.Context) {
	for range protectionHeartbeatStableAttempts {
		if s.observeProtectionAlertShadowHeartbeatOnce(ctx) {
			return
		}
		if ctx == nil || ctx.Err() != nil {
			return
		}
	}
}

// observeProtectionAlertShadowHeartbeatOnce rebuilds only the protection facts
// needed by the alert producer. It reads the portfolio cache and SQLite order
// journal; it never requests quotes, Greeks, account summaries, or broker
// writes. The canonical cache read may exercise the connector's existing,
// throttled account-updates re-subscription when a held account has no cached
// rows. That is a read-side stream repair, not an order or account mutation.
// The false return means the stable-evidence commit dropped the observation
// because evidence advanced mid-flight; every other outcome reports true.
func (s *Server) observeProtectionAlertShadowHeartbeatOnce(ctx context.Context) bool {
	if s == nil || s.alertShadow == nil {
		return true
	}
	now := s.orderNow().UTC()
	s.mu.Lock()
	ep, c, connectorEpoch := s.endpoint, s.connector, s.connectorEpoch
	s.mu.Unlock()
	configuredAccount := ""
	port := ep.Port
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
	}
	connectedAccount := ""
	if c != nil {
		connectedAccount = c.AccountID()
	}
	scope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount)
	shadowScope, err := newAlertShadowBrokerScope(scope)
	if err != nil {
		return true
	}
	input := alertShadowProtectionInput{
		AsOf: now, Status: orderIntegrityHealthUnavailable, Scope: shadowScope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: rpc.ProtectionCoverageStateUnknown},
	}
	if c == nil || !c.IsReady() {
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	rawPositions, health, positionsErr := c.CachedPositionsWithHealth()
	input.EvidenceAsOf = health.InitialCompletedAt.UTC()
	if health.LastUpdateAt.After(input.EvidenceAsOf) {
		input.EvidenceAsOf = health.LastUpdateAt.UTC()
	}
	input.Status = classifyOrderIntegrityPortfolioHealth(scope, health, now)
	if positionsErr != nil {
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	positions, scoped := protectionHeartbeatPositions(rawPositions, scope, now)
	if !scoped {
		input.Status = orderIntegrityHealthUnavailable
		input.Summary = rpc.ProtectionCoverageSummary{
			AsOf: now, Status: rpc.ProtectionCoverageStateUnknown,
			WarningCodes: []string{"portfolio_scope_conflict"},
		}
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	snapshotBinding := protectionOrderSnapshotBinding{
		scope: scope, connector: c, connectorEpoch: connectorEpoch, generation: c.OrderLifecycleGeneration(),
	}
	snapshotBinding.session, _ = c.CaptureSession()
	snapshot, snapshotErr := s.protectionSnapshotOpenOrders(ctx, snapshotBinding)
	now = s.orderNow().UTC()
	input.AsOf = now
	input.Summary.AsOf = now
	if ctx == nil || ctx.Err() != nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		// A mid-heartbeat scope move is the same starvation shape as a commit
		// drop: nothing was observed this pass. Let the bounded retry rebuild
		// against whatever scope is current now.
		return false
	}
	if snapshotErr != nil || !snapshot.Complete || snapshot.AsOf.IsZero() || snapshot.AsOf.After(now) {
		input.Status = orderIntegrityHealthUnavailable
		input.Summary = rpc.ProtectionCoverageSummary{
			AsOf: now, Status: rpc.ProtectionCoverageStateUnknown,
			WarningCodes: []string{"api_order_snapshot_unavailable", "manual_tws_orders_uncovered", "unmatched_api_orders_uncovered"},
			Message:      "complete all-client API open-order inventory unavailable; manual TWS and non-journaled API orders are outside this producer's authority",
		}
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	input.OrderSnapshotComplete = true
	input.OrderSnapshotAsOf = snapshot.AsOf.UTC()
	input.OrderUniverse = protectionOrderUniverseJournaledAPI
	receiptBinding := snapshotBinding
	receiptBinding.session = snapshot.Session
	receiptBinding.generation = snapshot.Generation
	if s.orderSnapshotFn != nil && receiptBinding.session == (ibkrlib.ConnectorSessionBinding{}) {
		// The in-process test seam has no socket token. Production snapshots
		// always carry the exact opaque session returned by Connector.
		receiptBinding.session = snapshotBinding.session
	}

	views, _, orderHead, orderErr := s.loadOrderViewsAtStableHead()
	if orderErr != nil {
		input.Status = orderIntegrityHealthUnavailable
		input.Summary = *buildProtectionCoverage(positions, nil, false, "order journal unavailable", now)
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	input.orderJournal = s.orderJournal
	input.orderAuthorityHeadSeq = orderHead
	orders := make([]rpc.OrderView, 0, len(views))
	missingJournalOrders := make([]rpc.OrderView, 0)
	matchedInventory := make([]bool, len(snapshot.Orders))
	for _, view := range views {
		if !view.Open || !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		index, brokerOrder, matched := openOrderSnapshotMatch(snapshot, view)
		if !matched {
			missingJournalOrders = append(missingJournalOrders, view)
			continue
		}
		matchedInventory[index] = true
		if protectionCoverageOrderIsStockLike(view) {
			orders = append(orders, protectionOrderViewFromSnapshot(view, brokerOrder, snapshot.AsOf))
		}
	}
	if protectionHeartbeatIdentityAmbiguous(positions, orders) {
		input.Status = orderIntegrityHealthUnavailable
		input.Summary = rpc.ProtectionCoverageSummary{
			AsOf: now, Status: rpc.ProtectionCoverageStateUnknown,
			WarningCodes: []string{"contract_identity_ambiguous", "manual_tws_orders_uncovered"},
		}
		s.observeProtectionAlertShadow(ctx, input)
		return true
	}
	reconcileFlatPositionProtectiveOrders(orders, positions, now)
	input.Summary = *buildProtectionCoverage(positions, orders, true, "", now)
	input.Summary.WarningCodes = appendCoverageCode(input.Summary.WarningCodes, "daemon_journaled_api_orders_checked")
	input.Summary.WarningCodes = appendCoverageCode(input.Summary.WarningCodes, "manual_tws_orders_uncovered")
	input.Summary.WarningCodes = appendCoverageCode(input.Summary.WarningCodes, "unmatched_api_orders_uncovered")
	input.Summary.Message = "alert authority covers daemon-journaled API protective orders checked against all-client API inventory; manual TWS and unmatched other-client API orders remain uncovered"
	for _, matched := range matchedInventory {
		if !matched {
			markProtectionSummaryUnmatchedInventory(&input.Summary)
			break
		}
	}
	markProtectionSummaryJournalOrdersAbsent(&input.Summary, missingJournalOrders)
	if s.orderLifecyclePersistenceUncertain.Load() {
		markProtectionSummaryPersistenceUncertain(&input.Summary)
	}
	brokerBinding := ibkrlib.BrokerEvidenceBinding{
		Session: receiptBinding.session, OrderLifecycleGeneration: receiptBinding.generation,
		PortfolioProjectionGeneration: health.ProjectionGeneration,
	}
	return s.observeProtectionAlertShadowStable(ctx, daemonBrokerEvidenceBinding{
		scope: scope, connector: c, connectorEpoch: connectorEpoch, broker: brokerBinding,
	}, input)
}

func markProtectionSummaryJournalOrdersAbsent(summary *rpc.ProtectionCoverageSummary, orders []rpc.OrderView) {
	if summary == nil || len(orders) == 0 {
		return
	}
	for _, order := range orders {
		underlying := strings.ToUpper(strings.TrimSpace(order.Symbol))
		if underlying == "" {
			underlying = "JOURNAL_ORDER"
		}
		coverageOrder := protectionCoverageOrder(order)
		summary.ByUnderlying = append(summary.ByUnderlying, rpc.ProtectionCoverageRow{
			Underlying: underlying, State: rpc.ProtectionCoverageStateUnknown,
			Orders:       []rpc.ProtectionCoverageOrder{coverageOrder},
			WarningCodes: []string{"journal_order_absent_from_broker_snapshot", "reconcile_required"},
			Message:      "an open daemon-journaled order was absent from the complete broker API snapshot; terminal state is unknown",
		})
		summary.Counts.Unknown++
	}
	summary.Status = rpc.ProtectionCoverageStateUnknown
	summary.WarningCodes = appendCoverageCode(summary.WarningCodes, "journal_order_absent_from_broker_snapshot")
	sortProtectionCoverageRows(summary.ByUnderlying)
}

func protectionOrderViewFromSnapshot(view rpc.OrderView, order ibkrlib.OrderLifecycleEvent, asOf time.Time) rpc.OrderView {
	out := view
	if order.OrderID > 0 {
		out.ReservedOrderID = order.OrderID
	}
	if order.PermID > 0 {
		out.PermID = order.PermID
	}
	if order.ClientIDPresent {
		out.ClientID = order.ClientID
	}
	if order.ConID > 0 {
		out.ConID = order.ConID
	}
	if order.Symbol != "" {
		out.Symbol = order.Symbol
	}
	if order.SecType != "" {
		out.SecType = order.SecType
	}
	if order.Expiry != "" {
		out.Expiry = order.Expiry
	}
	if order.Strike > 0 {
		out.Strike = order.Strike
	}
	if order.Right != "" {
		out.Right = order.Right
	}
	if order.Multiplier > 0 {
		out.Multiplier = order.Multiplier
	}
	if order.Exchange != "" {
		out.Exchange = order.Exchange
	}
	if order.Currency != "" {
		out.Currency = order.Currency
	}
	if order.LocalSymbol != "" {
		out.LocalSymbol = order.LocalSymbol
	}
	if order.TradingClass != "" {
		out.TradingClass = order.TradingClass
	}
	if order.Action != "" {
		out.Action = order.Action
	}
	if order.OrderType != "" {
		out.OrderType = order.OrderType
	}
	if order.TIF != "" {
		out.TIF = order.TIF
	}
	if order.TotalQuantity > 0 {
		out.Quantity = order.TotalQuantity
	}
	if order.Status != "" {
		out.Status = order.Status
		out.LifecycleStatus = mapBrokerOrderLifecycleStatus(order.Status, order.Filled, order.Remaining)
	}
	out.Filled = order.Filled
	if order.Remaining > 0 {
		out.Remaining = order.Remaining
	}
	out.SendState = orderSendStateBrokerAcknowledged
	out.Open = !orderLifecycleStatusIsTerminal(out.LifecycleStatus)
	out.BrokerTruthAsOf = asOf.UTC()
	out.UpdatedAt = asOf.UTC()
	if trail := trailSpecFromLifecycle(order); trail != nil {
		out.Trail = trail
	}
	return out
}

func markProtectionSummaryUnmatchedInventory(summary *rpc.ProtectionCoverageSummary) {
	if summary == nil {
		return
	}
	summary.ByUnderlying = append(summary.ByUnderlying, rpc.ProtectionCoverageRow{
		Underlying: "UNMATCHED_API_ORDER", State: rpc.ProtectionCoverageStateUnknown,
		WarningCodes: []string{"unmatched_api_orders_uncovered"},
		Message:      "an all-client API open-order row is outside the daemon journal authority",
	})
	summary.Counts.Unknown++
	summary.Status = rpc.ProtectionCoverageStateUnknown
	summary.WarningCodes = appendCoverageCode(summary.WarningCodes, "unmatched_api_orders_uncovered")
	sortProtectionCoverageRows(summary.ByUnderlying)
}

func markProtectionSummaryPersistenceUncertain(summary *rpc.ProtectionCoverageSummary) {
	if summary == nil {
		return
	}
	summary.ByUnderlying = append(summary.ByUnderlying, rpc.ProtectionCoverageRow{
		Underlying: "ORDER_JOURNAL", State: rpc.ProtectionCoverageStateUnknown,
		WarningCodes: []string{"order_lifecycle_persistence_incomplete"},
		Message:      "a broker lifecycle callback was not durably recorded; authoritative reconciliation is required",
	})
	summary.Counts.Unknown++
	summary.Status = rpc.ProtectionCoverageStateUnknown
	summary.WarningCodes = appendCoverageCode(summary.WarningCodes, "order_lifecycle_persistence_incomplete")
	sortProtectionCoverageRows(summary.ByUnderlying)
}

func protectionHeartbeatPositions(raw []*ibkrlib.RawPosition, scope brokerStateScope, asOf time.Time) (*rpc.PositionsResult, bool) {
	result := &rpc.PositionsResult{AsOf: asOf.UTC(), Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{}}
	if !cachedPositionsMatchBrokerScope(raw, scope) {
		return result, false
	}
	for _, position := range raw {
		if position == nil || position.Position == 0 {
			continue
		}
		view := rpc.PositionView{
			Symbol: strings.ToUpper(strings.TrimSpace(position.Contract.Symbol)), SecType: positionSecType(position.Contract.SecType),
			ConID: position.Contract.ConID, Exchange: position.Contract.Exchange, Currency: position.Contract.Currency,
			LocalSymbol: position.Contract.LocalSymbol, TradingClass: position.Contract.TradingClass, Quantity: position.Position,
		}
		if protectionCoveragePositionIsStockLike(view) {
			result.Stocks = append(result.Stocks, view)
		}
	}
	return result, true
}

// protectionHeartbeatIdentityAmbiguous rejects every relevant match that would
// fall back to a symbol because either side lacks a contract ID. Distinct
// positive ConIDs remain safe because the coverage matcher uses exact contract
// identity first.
func protectionHeartbeatIdentityAmbiguous(positions *rpc.PositionsResult, orders []rpc.OrderView) bool {
	if positions == nil {
		return true
	}
	bySymbol := make(map[string][]rpc.PositionView)
	for _, position := range positions.Stocks {
		if !protectionCoveragePositionIsStockLike(position) || position.Quantity == 0 {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(position.Symbol))
		if symbol == "" {
			return true
		}
		bySymbol[symbol] = append(bySymbol[symbol], position)
	}
	for symbol, sameSymbol := range bySymbol {
		seenConIDs := make(map[int]struct{}, len(sameSymbol))
		for _, position := range sameSymbol {
			if position.ConID > 0 {
				if _, duplicate := seenConIDs[position.ConID]; duplicate {
					return true
				}
				seenConIDs[position.ConID] = struct{}{}
			} else if len(sameSymbol) > 1 {
				return true
			}
		}
		for _, order := range orders {
			if !protectionCoverageOrderIsStopProtective(order) {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(order.Symbol), symbol) {
				continue
			}
			if order.ConID <= 0 {
				return true
			}
			for _, position := range sameSymbol {
				if position.ConID <= 0 {
					return true
				}
			}
		}
	}
	return false
}
