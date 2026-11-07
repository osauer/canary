package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Owner decision 2026-10-10: exact owner orders use midpoint-at-send within
// a signed cap/floor. Full-position stops wait until broker acceptance and
// follow position size without resetting the trail. V1 rule queues retain
// their original, narrower authority.
func ownerQueueProtection(t rpc.QueuedAuthTerms) bool {
	return t.Version == 2 && t.OwnerOrder != nil && t.OwnerOrder.FullPosition
}

// V2 never interprets a legacy error without a send disposition as proof
// that nothing went out. An acknowledgement always takes precedence.
func queuedJournalSendOutcome(events []orderJournalEvent, rec queuedAuthRecord) (orderSendOutcome, string) {
	if rec.Terms.Version != 2 {
		return orderJournalSendOutcome(events, rec.PreviewTokenID)
	}
	var attempted, unsent, unknown bool
	var ref string
	for _, ev := range events {
		if rec.PreviewTokenID == "" || ev.PreviewTokenID != rec.PreviewTokenID {
			continue
		}
		if ev.OrderRef != "" {
			ref = ev.OrderRef
		}
		switch ev.Type {
		case orderJournalEventSendCompleted, orderJournalEventBrokerAcknowledged, orderJournalEventStatusUpdated:
			return orderSendReached, ref
		case orderJournalEventSendAttempted:
			attempted = true
		case orderJournalEventSendError:
			if ev.SendDisposition == "definitely_unsent" {
				unsent = true
			} else {
				unknown = true
			}
		}
	}
	if unknown || attempted && !unsent {
		return orderSendUnclear, ref
	}
	if unsent {
		return orderSendFailedUnsent, ref
	}
	return orderSendNone, ref
}

func ownerQueueProblem(t rpc.QueuedAuthTerms) string {
	o := t.OwnerOrder
	if t.Version != 2 || o == nil || t.Contract.ConID <= 0 || !slices.Contains([]string{"STK", "OPT"}, t.Contract.SecType) ||
		t.AccountID == "" || !slices.Contains([]string{"paper", "live"}, t.AccountMode) || t.MaxQuantity <= 0 || t.MaxQuantity > 1000000 ||
		!slices.Contains([]string{"BUY", "SELL"}, t.Action) || t.Currency != t.Contract.Currency || t.ArmDeadline.IsZero() {
		return "invalid owner-order identity or quantity"
	}
	if o.FullPosition {
		if t.Contract.SecType != "STK" || t.Action != "SELL" || !isTrailOrderType(o.OrderType) || o.Trail == nil ||
			!positiveFinite(o.Trail.InitialStopPrice) || !t.NotAfter.IsZero() || t.Style != "broker_trail" ||
			o.AdaptivePriority != "" || o.Add || !slices.Contains([]string{"DAY", "GTC"}, t.TIF) || t.PositionQuantity <= 0 {
			return "invalid full-position protection mandate"
		}
		if err := validateModifyTrailDraft(o.OrderType, rpc.OrderDraft{OrderType: o.OrderType, Trail: o.Trail}); err != nil {
			return err.Error()
		}
	} else if o.OrderType != "LMT" || o.Trail != nil || t.Style != "midpoint_bounded" || t.Concession != 0 ||
		!positiveFinite(t.WorstPrice) || !positiveFinite(t.MaxSpreadPctOfMid) || t.MaxSpreadPctOfMid > 100 || t.TIF != "DAY" ||
		!t.NotAfter.After(t.NotBefore) || t.NotAfter.Sub(t.NotBefore) > queuedSendWindow || o.AdaptivePriority != queuedOwnerAdaptivePriority(t.Contract) || o.OutsideRTH {
		return "invalid midpoint owner order"
	}
	if !o.FullPosition && (!slices.Contains([]string{"open", "increase", "reduce", "close"}, t.PositionEffect) ||
		(t.Action == "SELL" && t.PositionQuantity <= 0) ||
		(proposalCloseReduceEffect(t.PositionEffect) && float64(t.MaxQuantity) > math.Abs(t.PositionQuantity))) {
		return "owner order would reverse or open a short position"
	}
	return ""
}

// prepareOwnerQueue resolves broker identity and position before producing
// signed terms. It does not mint a preview token or transmit an order.
func (e *proposalEngine) prepareOwnerQueue(ctx context.Context, p rpc.TradeProposalQueuePrepareParams) (out rpc.TradeProposalQueuePrepareResult, err error) {
	now, s := e.clock(), e.server
	out.AsOf = now
	block := func(code, message string) (rpc.TradeProposalQueuePrepareResult, error) {
		out.Blockers = preparedBlocker(code, message)
		return out, nil
	}
	if !e.queued.attached() || s == nil || s.protectionPolicies == nil {
		return block("queue_unavailable", "Queued authorisations are unavailable.")
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) || p.AccountID != scope.Account || p.AccountMode != scope.Mode {
		return block("queued_scope_mismatch", "Confirm the connected account and mode before queueing.")
	}
	if s.tradingFrozen() {
		return block(rpc.ReadinessTradingFrozen, tradingFrozenBlockerMessage)
	}
	var order rpc.OrderPreviewParams
	var prop rpc.TradeProposal
	if p.Order != nil {
		if p.Key != "" || p.Revision != "" || p.Quantity != 0 {
			return block("queue_request_invalid", "Choose an exact order or a proposal, not both.")
		}
		order = *p.Order
		if order.ReplaceID != "" || order.BondOrder != nil || order.Source != "" {
			return block("queue_request_invalid", "This queue supports a new stock or single-leg option order only.")
		}
	} else {
		var blockers []rpc.TradingBlocker
		prop, blockers, err = e.resolveProposal(ctx, p.Key, p.Revision)
		if err != nil || len(blockers) > 0 {
			out.Blockers = blockers
			return out, err
		}
		if prop.Shadow {
			out.Blockers = shadowProposalBlockers(prop)
			return out, nil
		}
		// A held mandate row waits for a verdict; nobody queues it either.
		if blockers := mandateHeldBlockers(prop); len(blockers) > 0 {
			out.Blockers = blockers
			return out, nil
		}
		// The core's row is sent in session at the live mid, inside its
		// window (core-sleeve.md, "Window and order terms"): a queue to the
		// open would cap it at the row's reference price, the series' last
		// close, and a higher open would leave it unsent, and the queue's
		// terms carry none of the row's typed buy exception. Its readiness
		// already says it is not queueable; this is the same answer here.
		if prop.Bucket == rpc.TradeProposalBucketCoreRebalance {
			return block("queue_bucket_not_queueable", "The core's row is sent in session at the live mid, inside its window; review it while its exchange is open.")
		}
		if prop.Bucket == rpc.TradeProposalBucketStrategyRebalance {
			return block("queue_bucket_not_queueable", "A strategy's row is sent in session at the live mid, inside its window, as the core's is; review it while its exchange is open.")
		}
		out.Proposal = &prop
		quantity := prop.Quantity
		if p.Quantity < 0 || p.Quantity > prop.Quantity {
			return block("queue_quantity", "The exact quantity must fit the proposal.")
		}
		if p.Quantity > 0 {
			quantity = p.Quantity
		}
		order = proposalOrderPreviewParams(prop, quantity, 0)
		if p.WorstPrice == 0 && prop.LimitPrice != nil {
			p.WorstPrice = *prop.LimitPrice
		}
	}
	if order.AttachedStop != nil {
		return block("protected_add_queue_unsupported", "Entry and attached stop require an immediate exact review; this pair cannot be queued.")
	}
	order.Action = strings.ToUpper(strings.TrimSpace(order.Action))
	order.OrderType = strings.ToUpper(strings.TrimSpace(order.OrderType))
	if order.OrderType == "" {
		order.OrderType = rpc.OrderTypeLMT
	}
	if order.TIF == "" {
		order.TIF = rpc.OrderTIFDay
	}
	if order.Contract.ConID <= 0 || !slices.Contains([]string{"STK", "OPT"}, order.Contract.SecType) || order.Quantity <= 0 {
		return block("queue_contract_inexact", "Choose an exact contract and positive quantity.")
	}
	authority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return block(rpc.ReadinessBrokerUnavailable, "Broker identity could not be verified.")
	}
	contract, err := s.resolvePreviewOrderContract(ctx, authority, order.Contract, previewMinTickTimeout)
	if err != nil {
		return block(previewContractUnresolvedCode, err.Error())
	}
	if !p.FullPosition {
		market, known := quoteSessionMarketForContract(contract)
		session, ok := s.previewSession(market, now)
		if !known || !ok {
			return block("queue_session_unknown", "The current exchange session cannot be verified.")
		}
		offset, _ := s.readinessOpeningOffset(market, prop.Bucket, now)
		if session.IsOpen && !now.Before(session.Open.Add(offset)) {
			return block("queue_not_offered", "The market is ready; place the order now.")
		}
	}
	position, err := s.capturePreviewOrderPositionAuthority(ctx, s.currentTradingStatus(), contract, order.Action, order.Quantity)
	if err != nil {
		return block(previewPositionUnavailableCode, err.Error())
	}
	policy, status := s.protectionPolicies.Active()
	bound := p.WorstPrice
	if bound == 0 && order.LimitPrice != nil {
		bound = *order.LimitPrice
	}
	prop.Contract, prop.Action, prop.Quantity, prop.PositionQuantity, prop.PositionEffect = contract, order.Action, order.Quantity, position.Impact.Before, position.Impact.Effect
	prop.Revision, prop.CreatedAt = nonEmptyString(prop.Revision, "owner"), now
	prop.LimitPrice = &bound
	if prop.Key == "" {
		prop.Key = fmt.Sprintf("owner:%d:%s", contract.ConID, order.Action)
		prop.Bucket = "owner_order"
	}
	var terms rpc.QueuedAuthTerms
	if p.FullPosition {
		if s.hasAttachedStockStop(contract) {
			return block("attached_stop_overlap", "An added-lot stop already exists; review its protection before creating a full-position mandate.")
		}
		if order.Action != "SELL" || contract.SecType != "STK" || !isTrailOrderType(order.OrderType) || position.Impact.Before < 1 || math.Trunc(position.Impact.Before) != position.Impact.Before {
			return block("queue_protection_invalid", "Full-position trailing protection requires whole long stock shares.")
		}
		if order.Trail == nil || !positiveFinite(order.Trail.InitialStopPrice) {
			return block("queue_stop_missing", "The exact trail and starting stop are required.")
		}
		market, ok := quoteSessionMarketForContract(contract)
		if !ok {
			return block("queue_session_unknown", "The contract market could not be identified.")
		}
		terms = rpc.QueuedAuthTerms{Version: 2, Broker: scope.brokerField(), AccountID: scope.Account, AccountMode: scope.Mode, Key: prop.Key, Bucket: prop.Bucket, RevisionAtQueue: prop.Revision, Contract: contract, Action: order.Action, PositionEffect: "close", PositionQuantity: position.Impact.Before, MaxQuantity: int(position.Impact.Before), Style: "broker_trail", Currency: contract.Currency, Market: string(market), NotBefore: now, ArmDeadline: now.Add(queuedArmWindow), TIF: order.TIF, PolicyFingerprint: status.EffectiveFingerprint}
	} else {
		var blockers []rpc.TradingBlocker
		terms, blockers = e.queuedTerms(prop, policy, status, order.Quantity, scope, now)
		if len(blockers) > 0 {
			out.Blockers = blockers
			return out, nil
		}
		terms.Version, terms.Style, terms.Concession, terms.WorstPrice = 2, "midpoint_bounded", 0, bound
	}
	terms.OwnerOrder = &rpc.QueuedOwnerOrder{OrderType: order.OrderType, Trail: cloneTrailSpec(order.Trail), TriggerMethod: order.TriggerMethod, OutsideRTH: order.OutsideRTH, FullPosition: p.FullPosition, Add: order.Action == "BUY" && contract.SecType == "STK" && position.Impact.Before >= 0}
	if !p.FullPosition {
		terms.OwnerOrder.AdaptivePriority = queuedOwnerAdaptivePriority(contract)
	}
	if problem := ownerQueueProblem(terms); problem != "" {
		return block("queue_terms_invalid", problem)
	}
	if blockers := e.automaticIntentBlockers(prop, scope); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	id, reference, err := mintQueuedReference()
	if err != nil {
		return out, err
	}
	terms.QueueID = id
	digest, err := queuedTermsDigest(terms)
	if err != nil {
		return out, err
	}
	rec := &queuedAuthRecord{Version: queuedDocumentVersion, ReferenceHash: sha256.Sum256([]byte(reference)), Terms: terms, TermsDigest: digest, ContractSide: sameContractSide(prop), CreatedAt: now.UTC()}
	var refused bool
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		for _, other := range records {
			if other.ContractSide == rec.ContractSide && sameBrokerScope(other.scope(), scope) && (other.liveIntent() || other.State == rpc.QueuedAuthSent) {
				refused = true
				return nil
			}
		}
		var events []queuedAuthEvent
		for _, other := range records {
			if other.ContractSide == rec.ContractSide && sameBrokerScope(other.scope(), scope) && other.State == rpc.QueuedAuthPrepared {
				events = append(events, other.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonSuperseded, "replaced before confirmation", now))
			}
		}
		records[id] = rec
		return append(events, rec.transition(queuedEventPrepared, rpc.QueuedAuthPrepared, now))
	})
	if err != nil {
		return out, err
	}
	if refused {
		return block("queued_intent_exists", "An order for this contract and side is already queued or working.")
	}
	view := rec.view()
	out.Accepted, out.QueuedRef, out.Queue = true, reference, &view
	out.Readiness = e.classifyReadiness(prop, nil, true, nil)
	if p.FullPosition && out.Readiness.Code == rpc.ReadinessOpeningWindow {
		out.Readiness.Code = rpc.ReadinessReady
	}
	return out, nil
}

func (e *proposalEngine) resolveOwnerQueue(ctx context.Context, rec queuedAuthRecord) (rpc.TradeProposal, []rpc.TradingBlocker, string, string) {
	t := rec.Terms
	p := rpc.TradeProposal{Key: t.Key, Bucket: t.Bucket, Revision: t.RevisionAtQueue, Contract: t.Contract, Action: t.Action, Quantity: t.MaxQuantity, PositionQuantity: t.PositionQuantity, PositionEffect: t.PositionEffect, OrderType: t.OwnerOrder.OrderType, TIF: t.TIF, Trail: cloneTrailSpec(t.OwnerOrder.Trail)}
	position, err := e.server.capturePreviewOrderPositionAuthority(ctx, e.server.currentTradingStatus(), t.Contract, t.Action, t.MaxQuantity)
	if err != nil {
		return p, preparedBlocker(previewPositionUnavailableCode, err.Error()), "", ""
	}
	current := position.Impact.Before
	if ownerQueueProtection(t) {
		if e.server.hasAttachedStockStop(t.Contract) {
			return p, preparedBlocker("attached_stop_overlap", "An added-lot stop overlaps this full-position mandate; review protection first."), "", ""
		}
		if current <= 0 {
			return p, nil, queuedReasonPositionChanged, "the position closed; its protection queue is cancelled"
		}
		if math.Trunc(current) != current || current > 1000000 {
			return p, preparedBlocker("queue_position_unavailable", "Whole-share coverage cannot be verified."), "", ""
		}
		p.Quantity, p.PositionQuantity = int(current), current
	} else if current != t.PositionQuantity {
		return p, nil, queuedReasonPositionChanged, "the position changed after confirmation; prepare the exact order again"
	}
	_, policy := e.server.protectionPolicies.Active()
	if policy.EffectiveFingerprint != t.PolicyFingerprint {
		return p, preparedBlocker("queue_policy_changed", "The policy changed after confirmation; the queue is held."), "", ""
	}
	return p, nil, "", ""
}

// submitOwnerQueue runs through the existing preview and place gates. The
// queue's shared beforePlace callback journals its intent before any frame.
func (e *proposalEngine) submitOwnerQueue(ctx context.Context, rec queuedAuthRecord, prop rpc.TradeProposal, opts proposalSubmitOptions) (out rpc.TradeProposalSubmitResult, err error) {
	if ownerQueueProtection(rec.Terms) && e.server.hasAttachedStockStop(rec.Terms.Contract) {
		return out, errBadRequest("attached stop overlaps full-position protection")
	}
	if rec.Terms.Action == "BUY" && rec.Terms.Contract.SecType == "STK" {
		ctx = context.WithValue(ctx, ownerQueueAdmissionKey{}, &ownerQueueAdmission{QueueID: rec.Terms.QueueID, ConID: rec.Terms.Contract.ConID, Quantity: rec.Terms.MaxQuantity, Cap: rec.Terms.WorstPrice})
	}
	// All intents, including owner-authored rows, share exact-contract/side
	// broker inventory and unacknowledged-send checks under brokerWriteMu.
	if blockers := e.reductionOrderBlockers(ctx, prop, true); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	t, o := rec.Terms, rec.Terms.OwnerOrder
	p := rpc.OrderPreviewParams{Action: t.Action, Contract: t.Contract, Quantity: prop.Quantity, OrderType: o.OrderType, TIF: t.TIF, Trail: cloneTrailSpec(o.Trail), TriggerMethod: o.TriggerMethod, OutsideRTH: o.OutsideRTH, AdaptivePriority: o.AdaptivePriority, Add: o.Add, Source: "owner-queue", TimeoutMs: int(queuedSubmitTimeout / time.Millisecond)}
	if o.FullPosition {
		p.Strategy = rpc.OrderStrategyBrokerTrail
	} else {
		p.Strategy = rpc.OrderStrategyBoundedLimit
		p.Bounded = &rpc.OrderBoundedLimit{Concession: 0, WorstPrice: t.WorstPrice, MaxSpreadPctOfMid: t.MaxSpreadPctOfMid}
	}
	preview, err := e.server.previewOrder(ctx, p)
	if err != nil {
		return out, err
	}
	out.Preview = sanitizeProposalPreviewForProposal(preview, prop)
	if !preview.SubmitEligible {
		out.Blockers = preparedBlocker("preview_not_submit_eligible", "The broker preview is not eligible.")
		return out, nil
	}
	if preview.Draft.Add != nil && len(preview.Draft.Add.Plan.Warnings) > 0 {
		out.Blockers = preparedBlocker("queue_new_add_warning", "New addition warnings require a fresh owner confirmation.")
		return out, nil
	}
	if err = opts.beforePlace(preview); err != nil {
		return out, err
	}
	placed, err := e.server.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, Origin: rpc.OrderOriginDaemonOwnerQueued})
	if err != nil {
		opts.placeRefused(err)
		return out, err
	}
	out.Accepted, out.Place, out.OrderRef, out.PreviewTokenID = placed.Accepted, placed, placed.OrderRef, placed.PreviewTokenID
	return out, nil
}

// queuedOwnerAdaptivePriority is the algorithm a queued midpoint order carries.
// Adaptive runs only on SMART, so an order on a listing's own exchange goes as a
// plain limit at the same bounded price.
func queuedOwnerAdaptivePriority(contract rpc.ContractParams) string {
	if strings.EqualFold(strings.TrimSpace(contract.Exchange), "SMART") {
		return "Normal"
	}
	return ""
}
