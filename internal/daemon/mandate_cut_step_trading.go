//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// stepMandateCut takes one due step of a placed mandate cut through the
// ordinary modify path with the daemon-preauthorised origin. Under
// brokerWriteMu it reads the record, the served row, the order and the quote
// again and acts only on what it finds there, so a fill, the breach's end or
// a disarm between the pass and the lock never becomes a write. A veto of
// the row waiting behind the order does not stop its steps; cancelling the
// order does.
// outcome says what was done or why the limit rests; err is a step that
// could not be attempted and is tried again.
func (e *proposalEngine) stepMandateCut(ctx context.Context, rec automaticSubmissionRecord, wait time.Duration, steps int) (outcome string, err error) {
	s := e.server
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	now := e.clock()
	current, ok := e.automatic.get(rec.Key, rec.Revision)
	if !ok || !current.stepDue(now, wait, steps) {
		return "", nil
	}
	if paused := s.configAutomationPausedBlocker(); paused != nil {
		return "", errors.New(paused.Message)
	}
	snap := e.Snapshot(false)
	if why := e.mandateCutSnapshotUnconfirmed(snap, now); why != "" {
		return "", errors.New(why)
	}
	prop, wanted := mandateCutWanted(snap, current)
	switch {
	case !wanted:
		e.noteMandateCutRests(ctx, current, "the breach's row for this line is no longer served, so the order keeps its limit", false)
		return "", nil
	case len(mandateHeldBlockers(prop)) > 0:
		return "", errors.New("the mandate verdict cannot be confirmed right now")
	case e.automaticSessionDue(prop, now).After(now):
		return "", errors.New("the session is closed or inside its opening offset")
	}
	views, events, err := s.loadOrderViews()
	if err != nil {
		return "", err
	}
	known, working := false, false
	for _, v := range views {
		if v.OrderRef == current.OrderRef {
			known, working = true, working || v.Open
		}
	}
	if known && !working {
		// Filled, cancelled or expired: nothing is left to step.
		e.noteMandateCutRests(ctx, current, "the order no longer works", true)
		return "", nil
	}
	if mandateCutChangedByHand(events, current.OrderRef) {
		e.noteMandateCutRests(ctx, current, "the order was changed by hand, so Canary leaves its limit where it was set", true)
		return "", nil
	}
	status := s.currentTradingStatus()
	view, err := s.openOrderViewForWrite(current.OrderRef, status)
	if err != nil {
		return "", err
	}
	remaining := orderViewRemainingQuantity(view)
	if !view.ModifyEligible || !strings.EqualFold(view.OrderType, rpc.OrderTypeLMT) || !strings.EqualFold(view.Action, rpc.OrderActionSell) ||
		remaining <= 0 || remaining != math.Trunc(remaining) {
		return "", errors.New("the order is not a working sale limit that can be changed")
	}
	contract := rpc.ContractParams{
		ConID: view.ConID, Symbol: view.Symbol, SecType: strings.ToUpper(strings.TrimSpace(view.SecType)),
		Exchange: view.Exchange, PrimaryExch: view.PrimaryExch, Currency: view.Currency,
		LocalSymbol: view.LocalSymbol, TradingClass: view.TradingClass,
		Expiry: view.Expiry, Strike: view.Strike, Right: view.Right, Multiplier: view.Multiplier,
	}
	if !positiveFinite(current.Step.WorstPrice) || !positiveFinite(current.Step.SpreadCapPct) {
		e.noteMandateCutRests(ctx, current, "the order carries no worst price to step inside", true)
		return "", nil
	}
	fx := 0.0
	if current.Step.GivePerUnitBase > 0 {
		if _, rate, _, _ := s.fillQuoteFX(ctx, nonEmptyString(view.Currency, prop.Contract.Currency)); rate != nil {
			fx = *rate
		}
	}
	// One preview prices the step from the fresh bid and ask it reads
	// itself, inside the bounds, and signs the limit the modify may send.
	preview, err := s.previewOrder(ctx, rpc.OrderPreviewParams{
		Action: rpc.OrderActionSell, Contract: contract, Quantity: int(remaining), OrderType: rpc.OrderTypeLMT,
		Strategy: rpc.OrderStrategyBoundedLimit, Bounded: current.Step.bounded(contract, fx),
		AdaptivePriority: view.AdaptivePriority, TIF: view.TIF, OutsideRTH: view.OutsideRTH, ReplaceID: view.OrderRef,
		TimeoutMs: int(mandateCutStepTimeout.Milliseconds()), Source: view.Source,
	})
	if err != nil {
		if previewRefusedWith(err, previewBoundedSpreadCode) {
			e.noteMandateCutRests(ctx, current, fmt.Sprintf("the spread is wider than its %g%% cap and no give can be measured for it, so the order keeps its limit", current.Step.SpreadCapPct), false)
			return "", nil
		}
		return "", fmt.Errorf("preview: %w", err)
	}
	if preview == nil || !preview.SubmitEligible || preview.Draft.Quantity != int(remaining) || !positiveFinite(preview.Draft.LimitPrice) {
		return "", errors.New("the step's preview is not submit-eligible")
	}
	target := preview.Draft.LimitPrice
	if !(target < view.LimitPrice-1e-9) {
		e.noteMandateCutRests(ctx, current, fmt.Sprintf("its limit %s is already the lowest its bounds allow on the current bid", formatStepPrice(view.LimitPrice)), false)
		return "", nil
	}
	s.automaticGrant.Store(&automaticWriteGrant{Key: current.Key, Revision: current.Revision, Bucket: current.Bucket, Quantity: current.Quantity, ArmedAt: current.ArmedAt,
		Step: &automaticStepGrant{OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Limit: target, Floor: current.Step.WorstPrice}})
	defer s.automaticGrant.Store(nil)

	taken := *current.Step
	taken.Count, taken.Of, taken.At, taken.From, taken.To, taken.TokenID, taken.Rests, taken.Confirmed = taken.Count+1, steps, now, view.LimitPrice, target, preview.PreviewTokenID, "", false
	recorded := false
	_, err = s.modifyOrderStaged(ctx, rpc.OrderModifyParams{ID: view.OrderRef, PreviewToken: preview.PreviewToken, Origin: rpc.OrderOriginDaemonPreAuthorised, TimeoutMs: int(mandateCutStepTimeout.Milliseconds())}, func() error {
		// The step is spent before anything can reach the broker.
		stageErr := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			r, ok := records[automaticRecordKey(current.Key, current.Revision)]
			if !ok || r.Step == nil || r.Step.Count != current.Step.Count || r.State != rpc.TradeProposalAutomaticSubmitted {
				return nil
			}
			step := taken
			r.Step, r.Reason = &step, mandateCutStepReason(taken)
			recorded = true
			return []automaticSubmissionEvent{{At: now, Type: automaticEventStepAttempted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Origin: rpc.OrderOriginDaemonPreAuthorised, Reason: r.Reason}}
		})
		if stageErr != nil {
			return fmt.Errorf("persist the step: %w", stageErr)
		}
		if !recorded {
			return errors.New("the record changed before the step")
		}
		return nil
	})
	if err == nil {
		// The modify path accepted the change; the record now says so.
		confirmErr := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			r, ok := records[automaticRecordKey(current.Key, current.Revision)]
			if !ok || r.Step == nil || r.Step.TokenID != taken.TokenID {
				return nil
			}
			step := *r.Step
			step.Confirmed = true
			r.Step, r.Reason = &step, mandateCutStepReason(step)
			return []automaticSubmissionEvent{{At: e.clock(), Type: automaticEventStepped, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Origin: rpc.OrderOriginDaemonPreAuthorised, Reason: r.Reason}}
		})
		if confirmErr != nil {
			s.warnf("mandate cut step: record the accepted step of %s: %v", current.Key, confirmErr)
		}
		return fmt.Sprintf("limit moved from %s to %s (step %d of %d)", formatStepPrice(taken.From), formatStepPrice(taken.To), taken.Count, steps), nil
	}
	if recorded && ibkrlib.SendDispositionOf(err) == ibkrlib.SendDispositionDefinitelyUnsent {
		// Proven unsent: the step is given back, and taken again once the
		// broker's next report of the order shows the limit it still holds.
		// Any other outcome after the record may have reached the broker and
		// stays spent, worded as an attempt.
		undoErr := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			r, ok := records[automaticRecordKey(current.Key, current.Revision)]
			if !ok || r.Step == nil || r.Step.TokenID != taken.TokenID {
				return nil
			}
			back := *current.Step
			r.Step, r.Reason = &back, current.Reason
			return []automaticSubmissionEvent{{At: e.clock(), Type: automaticEventStepRests, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Reason: "the step was not sent and is tried again: " + err.Error()}}
		})
		if undoErr != nil {
			s.warnf("mandate cut step: give back the unsent step of %s: %v", current.Key, undoErr)
		}
	}
	return "", err
}

// automaticStepGrantCovers is the per-order half of the daemon-preauthorised
// origin's policy for a modify, checked once the write path has resolved the
// order and its signed draft: the scheduler may change only the order its
// grant names, only its limit, only downward for a sale, and only to the
// granted limit at or above the floor the order was placed under.
func (s *Server) automaticStepGrantCovers(origin string, view rpc.OrderView, draft rpc.OrderDraft) error {
	if origin != rpc.OrderOriginDaemonPreAuthorised {
		return nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("%w: a daemon-preauthorised modify %s", ErrTradingDisabled, why)
	}
	grant := s.automaticGrant.Load()
	switch {
	case grant == nil || grant.Step == nil:
		return refuse("is issued only by the scheduler's step of one placed mandate cut; none is in progress")
	case grant.Step.OrderRef == "" || grant.Step.OrderRef != view.OrderRef || grant.Step.ReservedOrderID != view.ReservedOrderID:
		return refuse("does not name this order")
	case !strings.EqualFold(view.OrderType, rpc.OrderTypeLMT) || !strings.EqualFold(view.Action, rpc.OrderActionSell) || !strings.EqualFold(draft.Action, rpc.OrderActionSell):
		return refuse("steps a sale limit only")
	case float64(draft.Quantity) != orderViewRemainingQuantity(view):
		return refuse("may change the limit only, never the quantity")
	case draft.LimitPrice != grant.Step.Limit || !positiveFinite(grant.Step.Floor) || draft.LimitPrice < grant.Step.Floor-1e-9:
		return refuse("is outside the granted limit or below the worst price the order was placed under")
	case !(draft.LimitPrice < view.LimitPrice):
		return refuse("may only lower a sale's limit")
	}
	return nil
}

// automaticStepGrantRefusesPlace keeps a step's grant from admitting a new
// order: while the scheduler steps a placed cut, its origin places nothing.
func (s *Server) automaticStepGrantRefusesPlace(origin string) error {
	if origin != rpc.OrderOriginDaemonPreAuthorised {
		return nil
	}
	if grant := s.automaticGrant.Load(); grant != nil && grant.Step != nil {
		return fmt.Errorf("%w: the scheduler's grant is for one order's step and places nothing", ErrTradingDisabled)
	}
	return nil
}
