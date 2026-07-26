//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	// paperSmokeSymbol is fixed: the smoke proves order plumbing, not symbol
	// coverage, and SPY tolerates delayed data on the explicit-limit path.
	paperSmokeSymbol   = "SPY"
	paperSmokeQuantity = 1
	// paperSmokeLimitFactor prices the buy 2 % under the reference: inside
	// default TWS API price-precaution bands (a deep offset is rejected by
	// the percentage constraint, error 109) yet effectively unfillable for a
	// 1-share DAY order inside the smoke's wait window. The fill path still
	// reports failed with a manual-cleanup warning.
	paperSmokeLimitFactor  = 0.98
	paperSmokeDefaultWait  = 30 * time.Second
	paperSmokeMaxWait      = 60 * time.Second
	paperSmokeCancelBudget = 15 * time.Second
	paperSmokePollInterval = 250 * time.Millisecond
	paperSmokeSource       = "paper-smoke"
)

func (s *Server) handleTradingPaperSmoke(ctx context.Context, req *rpc.Request) (*rpc.TradingPaperSmokeResult, error) {
	var p rpc.TradingPaperSmokeParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.runPaperSmoke(ctx, p)
}

// runPaperSmoke performs the daemon-observed paper order round-trip that
// produces live-gate evidence: place a 1-share far-off-market LMT through
// the production preview/place path, wait for broker acknowledgement,
// cancel, wait for the cancel to confirm, then save MAC'd evidence. The
// daemon observes every step itself — a client-orchestrated flow would make
// the evidence trust the client's lifecycle claims.
func (s *Server) runPaperSmoke(ctx context.Context, p rpc.TradingPaperSmokeParams) (*rpc.TradingPaperSmokeResult, error) {
	if s == nil {
		return nil, ErrTradingDisabled
	}
	// Origin note: paper-smoke is deliberately open to automated origins.
	// As of 2026-06-10 the evidence is a release-pipeline quality gate
	// (make release runs the smoke at version bump and aborts on failure),
	// not a runtime live precondition — live enablement rests on the
	// TWS-side API toggle, the trading-capable binary, and the config
	// pins/acks. With nothing unlocked by the evidence, an origin
	// restriction here would only obstruct release automation. The order
	// itself transmits on the paper route, which is open to agents by
	// policy.
	if !s.paperSmokeMu.TryLock() {
		return nil, errBadRequest("a paper-smoke run is already in progress")
	}
	defer s.paperSmokeMu.Unlock()

	auth := s.currentBrokerWriteAuthorization()
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	if status.Mode != config.TradingModePaper {
		return nil, fmt.Errorf("%w: paper-smoke transmits only on the paper route; current mode is %q — point the trading gate at the paper gateway first", ErrTradingDisabled, status.Mode)
	}
	if s.tradingReadiness == nil || s.orderTokens == nil {
		return nil, fmt.Errorf("%w: trading readiness store or evidence signer is unavailable", ErrTradingDisabled)
	}

	contract, err := normalizePreviewContract(rpc.ContractParams{Symbol: paperSmokeSymbol, SecType: "STK"})
	if err != nil {
		return nil, err
	}
	if contract.MinTick <= 0 {
		contract.MinTick = s.resolveContractMinTick(ctx, contract, previewMinTickTimeout)
	}
	quote, err := s.fetchPreviewQuote(ctx, contract, orderPreviewTimeout(0))
	if err != nil {
		return nil, fmt.Errorf("paper-smoke reference quote: %w", err)
	}
	reference := paperSmokeReferencePrice(quote)
	if reference <= 0 {
		return nil, errBadRequest("paper-smoke needs a " + paperSmokeSymbol + " reference price and the quote returned none; retry when bid/last/mark data is available")
	}
	limit := floorPriceToTick(reference*paperSmokeLimitFactor, max(contract.MinTick, 0.01))
	if limit <= 0 {
		return nil, errBadRequest("paper-smoke could not derive a positive limit price")
	}

	preview, err := s.previewOrder(ctx, rpc.OrderPreviewParams{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: paperSmokeSymbol, SecType: "STK"},
		Quantity:   paperSmokeQuantity,
		OrderType:  rpc.OrderTypeLMT,
		Strategy:   rpc.OrderStrategyExplicitLimit,
		LimitPrice: &limit,
		TIF:        rpc.OrderTIFDay,
		Source:     paperSmokeSource,
	})
	if err != nil {
		return nil, fmt.Errorf("paper-smoke preview: %w", err)
	}
	if !preview.SubmitEligible {
		return nil, fmt.Errorf("%w: paper-smoke preview is not submit-eligible (broker WhatIf %s): %s", ErrTradingDisabled, preview.WhatIf.Status, preview.WhatIf.Message)
	}

	res := &rpc.TradingPaperSmokeResult{
		Mode:       status.Mode,
		Account:    status.Account,
		Endpoint:   status.Endpoint,
		ClientID:   status.ClientID,
		Version:    s.version,
		Symbol:     preview.Draft.Contract.Symbol,
		LimitPrice: preview.Draft.LimitPrice,
		Quantity:   paperSmokeQuantity,
		Warnings:   preview.Warnings,
	}

	// The place call is the transmit boundary: every earlier failure leaves
	// existing evidence untouched, every later failure saves result=failed
	// (fail closed — a broken order lifecycle revokes prior valid evidence).
	s.brokerWriteMu.Lock()
	place, placeErr := s.placeOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, Origin: p.Origin})
	s.brokerWriteMu.Unlock()
	if placeErr != nil {
		res.Message = fmt.Sprintf("paper-smoke place failed: %v. The send may be uncertain — reconcile with `canary orders --json` before rerunning.", placeErr)
		return s.finishPaperSmoke(res, status, false)
	}
	res.OrderRef = place.OrderRef
	res.ReservedOrderID = place.ReservedOrderID

	ackView, ackOK := s.waitForOrderView(ctx, place.OrderRef, paperSmokeAckWait(p.TimeoutMs), orderViewAcknowledged)
	res.AckLifecycleStatus = ackView.LifecycleStatus

	// Cleanup must outlive the RPC: the method deadline or a dropped client
	// must not strand a transmitted smoke order, so the cancel phase runs on
	// a detached context with its own fixed budget.
	cancelBudget := paperSmokeCancelBudget
	if s.paperSmokeCancelBudgetOverride > 0 {
		cancelBudget = s.paperSmokeCancelBudgetOverride
	}
	cancelCtx, cancelDone := context.WithTimeout(context.WithoutCancel(ctx), cancelBudget)
	defer cancelDone()
	cancelErr := s.cancelSmokeOrder(cancelCtx, status, place, p.Origin, ackOK && ackView.CancelEligible)
	var cancelOK bool
	if cancelErr == nil {
		var cancelView rpc.OrderView
		cancelView, cancelOK = s.waitForOrderView(cancelCtx, place.OrderRef, cancelBudget, orderViewCancelConfirmed)
		res.CancelLifecycleStatus = cancelView.LifecycleStatus
	}

	if finalView, ok := s.orderViewByRef(place.OrderRef); ok {
		if res.AckLifecycleStatus == "" {
			res.AckLifecycleStatus = finalView.LifecycleStatus
		}
		res.CancelLifecycleStatus = finalView.LifecycleStatus
		if finalView.LifecycleStatus == rpc.OrderLifecycleFilled || finalView.LifecycleStatus == rpc.OrderLifecyclePartiallyFilled {
			res.Message = fmt.Sprintf("paper-smoke order %s filled unexpectedly — close the %d-share %s position in %s manually.", place.OrderRef, paperSmokeQuantity, res.Symbol, status.Account)
			return s.finishPaperSmoke(res, status, false)
		}
	}

	switch {
	case !ackOK:
		res.Message = fmt.Sprintf("broker did not acknowledge order %s within %s; cleanup cancel %s. Check `canary orders --json` for the smoke order.", place.OrderRef, paperSmokeAckWait(p.TimeoutMs), paperSmokeCancelOutcome(cancelErr, cancelOK))
		return s.finishPaperSmoke(res, status, false)
	case cancelErr != nil:
		res.Message = fmt.Sprintf("broker acknowledged order %s but the cleanup cancel failed: %v. Cancel it via `canary order cancel %s` and rerun.", place.OrderRef, cancelErr, place.OrderRef)
		return s.finishPaperSmoke(res, status, false)
	case !cancelOK:
		res.Message = fmt.Sprintf("broker acknowledged order %s but did not confirm the cancel within %s. Check `canary orders --json`, then rerun.", place.OrderRef, paperSmokeCancelBudget)
		return s.finishPaperSmoke(res, status, false)
	}
	res.Message = fmt.Sprintf("paper order round-trip confirmed: broker acknowledged and cancelled %s.", place.OrderRef)
	return s.finishPaperSmoke(res, status, true)
}

// finishPaperSmoke saves the evidence and stamps the result. Evidence is
// bound to the running binary version, so a rebuild requires a rerun.
func (s *Server) finishPaperSmoke(res *rpc.TradingPaperSmokeResult, status rpc.TradingStatus, passed bool) (*rpc.TradingPaperSmokeResult, error) {
	result := tradingPaperSmokeResultFailed
	if passed {
		result = tradingPaperSmokeResultPassed
	}
	now := s.orderNow()
	err := s.tradingReadiness.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       status.Account,
		Endpoint:      status.Endpoint,
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      status.ClientID,
		Version:       s.version,
		Result:        result,
		At:            now,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: save paper-smoke evidence: %v", ErrTradingDisabled, err)
	}
	res.Passed = passed
	res.Result = result
	res.EvidenceSaved = true
	res.EvidenceAt = &now
	res.EvidenceMaxAge = s.effectiveTradingConfig().PaperSmokeMaxAgeDuration().String()
	res.AsOf = s.orderNow()
	return res, nil
}

// cancelSmokeOrder cleans up the smoke order. With a broker-acknowledged
// view the production cancel path applies; before acknowledgement the read
// model refuses (cancel eligibility requires broker confirmation), so fall
// back to a direct broker cancel on the reserved ID with a journaled
// cancel-requested event.
func (s *Server) cancelSmokeOrder(ctx context.Context, status rpc.TradingStatus, place *rpc.OrderPlaceResult, origin string, eligible bool) error {
	if eligible {
		_, err := s.cancelOrderForAction(ctx, rpc.OrderCancelParams{ID: place.OrderRef, Origin: origin}, corestore.ActionSmokeCleanup)
		return err
	}
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(origin, true)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	if bindingErr != nil {
		return bindingErr
	}
	currentStatus := auth.Status
	if !strings.EqualFold(status.Mode, currentStatus.Mode) || !strings.EqualFold(status.Account, currentStatus.Account) ||
		status.Endpoint != currentStatus.Endpoint || status.ClientID != currentStatus.ClientID ||
		!strings.EqualFold(place.Mode, currentStatus.Mode) || !strings.EqualFold(place.Account, currentStatus.Account) ||
		place.Endpoint != currentStatus.Endpoint || place.ClientID != currentStatus.ClientID {
		return brokerWriteTransactionDriftError()
	}
	ev := orderJournalEventForDraft(place.Draft, orderJournalEventCancelRequested, currentStatus, place.PreviewTokenID, place.ReservedOrderID, s.orderNow())
	attemptID, err := randomTokenID()
	if err != nil {
		return fmt.Errorf("prepare paper-smoke cleanup attempt: %w", err)
	}
	ev.AttemptID = attemptID
	ev.ActionKind = corestore.ActionSmokeCleanup
	ev.SendState = orderSendStateSendAttempted
	ev.Origin = normalizedWriteOrigin(origin)
	ev.Message = "paper-smoke cleanup: broker cancel attempted before acknowledgement"
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return err
	}
	if err := s.orderJournal.StagePreTransmit("", "", 0, place.ReservedOrderID, corestore.ActionSmokeCleanup, coreOrderOrigin(origin), []orderJournalEvent{ev}); err != nil {
		return fmt.Errorf("append cancel journal: %w", err)
	}
	if err := s.cancelBoundConfiguredOrder(ctx, binding, currentStatus, place.ReservedOrderID); err != nil {
		if journalErr := s.appendOrderSendError(place.Draft, currentStatus, place.PreviewTokenID, place.ReservedOrderID, attemptID, corestore.ActionSmokeCleanup, err); journalErr != nil {
			err = errors.Join(err, journalErr)
		}
		return err
	}
	return s.appendOrderSendCompleted(place.Draft, currentStatus, place.PreviewTokenID, place.ReservedOrderID, attemptID, corestore.ActionSmokeCleanup)
}

// waitForOrderView polls the order read model (poll first, then every
// paperSmokePollInterval) until cond holds or the wall-clock wait expires.
// Deadlines are real time, never s.now — tests pin s.now to a fixed instant.
func (s *Server) waitForOrderView(ctx context.Context, orderRef string, wait time.Duration, cond func(rpc.OrderView) bool) (rpc.OrderView, bool) {
	deadline := time.Now().Add(wait)
	var last rpc.OrderView
	for {
		if view, ok := s.orderViewByRef(orderRef); ok {
			last = view
			if cond(view) {
				return view, true
			}
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(paperSmokePollInterval):
		}
	}
}

func (s *Server) orderViewByRef(orderRef string) (rpc.OrderView, bool) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return rpc.OrderView{}, false
	}
	for _, view := range views {
		if view.OrderRef == orderRef {
			return view, true
		}
	}
	return rpc.OrderView{}, false
}

func orderViewAcknowledged(view rpc.OrderView) bool {
	if view.SendState == orderSendStateBrokerAcknowledged {
		return true
	}
	switch view.LifecycleStatus {
	case rpc.OrderLifecyclePreSubmitted, rpc.OrderLifecycleSubmitted:
		return true
	default:
		return false
	}
}

func orderViewCancelConfirmed(view rpc.OrderView) bool {
	return view.LifecycleStatus == rpc.OrderLifecycleCancelled
}

func paperSmokeReferencePrice(quote rpc.OrderQuoteSnapshot) float64 {
	for _, v := range []*float64{quote.Bid, quote.Last, quote.Mark, quote.Midpoint, quote.Ask} {
		if v != nil && *v > 0 {
			return *v
		}
	}
	return 0
}

func paperSmokeAckWait(timeoutMs int) time.Duration {
	wait := time.Duration(timeoutMs) * time.Millisecond
	if wait <= 0 {
		return paperSmokeDefaultWait
	}
	return min(wait, paperSmokeMaxWait)
}

func paperSmokeCancelOutcome(cancelErr error, cancelOK bool) string {
	switch {
	case cancelErr != nil:
		return "failed: " + cancelErr.Error()
	case cancelOK:
		return "confirmed"
	default:
		return "sent but unconfirmed"
	}
}
