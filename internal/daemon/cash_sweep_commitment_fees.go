package daemon

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The upper bound is stored in the same transaction as the exact send or
// modification attempt. An unused preview therefore cannot certify a working
// order. Legacy records remain unknown; no commission is guessed on upgrade.
func attachOrderFeeBound(ev *orderJournalEvent, whatIf rpc.OrderWhatIfResult) {
	if fee, ok := cashSweepFeeUpper(&rpc.OrderPreviewResult{WhatIf: whatIf}, ev.Currency); ok {
		ev.FeeUpper, ev.FeeCurrency = new(fee), normCcy(ev.Currency)
	}
}

type cashSweepFeeEvidence struct {
	Events   []orderJournalEvent
	Now      time.Time
	Endpoint string
}

// cashSweepWorkingAttempt binds both commission and bond cash obligations
// to the latest exact send or modification, never an unused preview.
func cashSweepWorkingAttempt(o ibkrlib.OrderLifecycleEvent, scope brokerStateScope, evidence []cashSweepFeeEvidence) (orderJournalEvent, bool) {
	if len(evidence) != 1 || evidence[0].Now.IsZero() || o.OrderID <= 0 || o.ConID <= 0 || o.OrderRef == "" || !o.ClientIDPresent || o.WhatIf {
		return orderJournalEvent{}, false
	}
	e := evidence[0]
	// Examine the latest attempted place/modify for this identity, including a
	// failed/ambiguous modify. Never fall back to an older matching fee bound.
	for _, ev := range slices.Backward(e.Events) {
		if ev.ReservedOrderID != o.OrderID || ev.ClientID != o.ClientID || ev.Account != scope.Account || ev.Mode != scope.Mode || ev.Endpoint != e.Endpoint || ev.OrderRef != o.OrderRef {
			continue
		}
		if ev.Type != orderJournalEventSendAttempted && ev.Type != orderJournalEventModifyRequested {
			continue
		}
		if ev.At.IsZero() || ev.At.After(e.Now) || !cashSweepDay(ev.At).Equal(cashSweepDay(e.Now)) || ev.TIF != rpc.OrderTIFDay || o.TIF != rpc.OrderTIFDay ||
			ev.ConID != o.ConID || ev.SecType != o.SecType || normCcy(ev.Currency) != normCcy(o.Currency) || ev.Multiplier != o.Multiplier || ev.Exchange != o.Exchange ||
			!orderAlgorithmMatches(ev.AdaptivePriority, o.AdaptivePriority, o.AlgoStrategy, o.AlgoKnown) || ev.Action != o.Action || ev.OrderType != o.OrderType || ev.Quantity != o.TotalQuantity || ev.LimitPrice != o.LimitPrice || ev.TriggerMethod != o.TriggerMethod || ev.OutsideRTH != o.OutsideRth || ev.Trail != nil || ev.StrategyGroup != nil {
			return orderJournalEvent{}, false
		}
		return ev, true
	}
	return orderJournalEvent{}, false
}

func cashSweepWorkingFee(o ibkrlib.OrderLifecycleEvent, scope brokerStateScope, evidence []cashSweepFeeEvidence) (float64, bool) {
	ev, ok := cashSweepWorkingAttempt(o, scope, evidence)
	if !ok || ev.FeeUpper == nil || !finiteProtectionOptionPolicyValue(*ev.FeeUpper) || *ev.FeeUpper < 0 || *ev.FeeUpper == math.MaxFloat64 || normCcy(ev.FeeCurrency) != normCcy(o.Currency) || strings.TrimSpace(ev.FeeCurrency) == "" {
		return 0, false
	}
	// Retain the whole fee bound after a partial fill; pruning it proportionally
	// could lose a minimum commission. Already-paid fees are not credited back.
	return *ev.FeeUpper, true
}

// cashSweepWorkingBondCommitment preserves the signed bond's units and
// accrued-interest bound. Security type alone never proves a zero coupon.
// Keep the whole accrued bound after a partial fill, as with the fee bound.
func cashSweepWorkingBondCommitment(o ibkrlib.OrderLifecycleEvent, scope brokerStateScope, evidence []cashSweepFeeEvidence) (float64, bool) {
	if !cashSweepBondSecType(o.SecType) || o.Action != rpc.OrderActionBuy || (o.OrderType != rpc.OrderTypeLMT && o.OrderType != "STP LMT") {
		return 0, false
	}
	ev, ok := cashSweepWorkingAttempt(o, scope, evidence)
	if !ok || ev.Bond == nil {
		return 0, false
	}
	b := ev.Bond
	if !positiveFinite(b.FacePerUnit) || b.PriceConvention != rpc.BondPriceConventionPer100 ||
		!finiteProtectionOptionPolicyValue(b.AccruedBound) || b.AccruedBound < 0 || !positiveFinite(o.LimitPrice) {
		return 0, false
	}
	if b.Instrument == rpc.OrderBondInstrumentByIdentifier {
		if b.Coupon == nil || !finiteProtectionOptionPolicyValue(*b.Coupon) || *b.Coupon < 0 {
			return 0, false
		}
		coupon := *b.Coupon
		if b.AccruedBound < o.TotalQuantity*b.FacePerUnit*coupon/100 {
			return 0, false
		}
	} else {
		conv, known := cashSweepInstrumentConventions[b.Instrument]
		if !known || conv.PriceConvention != rpc.BondPriceConventionPer100 || conv.FacePerUnit != b.FacePerUnit || conv.QuantityUnit != b.QuantityUnit ||
			(b.Coupon != nil && *b.Coupon != 0) {
			return 0, false
		}
	}
	amount := brokerOrderRemaining(o)*o.LimitPrice*b.FacePerUnit/100 + b.AccruedBound
	return amount, positiveFinite(amount)
}

// An all-client broker snapshot may predate a local send. Its empty inventory
// cannot erase an unacknowledged or uncertain cash-consuming intent.
func cashSweepUnacknowledgedBuyGuard(out *cashSweepCommitments, orders []ibkrlib.OrderLifecycleEvent, scope brokerStateScope, evidence cashSweepFeeEvidence) {
	for _, view := range buildOrderViews(evidence.Events) {
		if !view.Open || !strings.EqualFold(view.Action, rpc.OrderActionBuy) || !orderViewMatchesBrokerScope(view, scope) || view.Endpoint != evidence.Endpoint {
			continue
		}
		seen := false
		for _, order := range orders {
			if brokerOrderWorking(order) && openOrderSnapshotEventMatches(order, view) {
				seen = true
				break
			}
		}
		if !seen {
			out.Unknown[""] = "a local buy intent is not reconciled with the current broker inventory, so cash commitments remain unknown"
			return
		}
	}
}
