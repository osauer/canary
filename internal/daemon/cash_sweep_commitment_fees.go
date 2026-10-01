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

func cashSweepWorkingFee(o ibkrlib.OrderLifecycleEvent, scope brokerStateScope, evidence []cashSweepFeeEvidence) (float64, bool) {
	if len(evidence) != 1 || evidence[0].Now.IsZero() || o.OrderID <= 0 || o.ConID <= 0 || o.OrderRef == "" || !o.ClientIDPresent || o.WhatIf {
		return 0, false
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
			ev.Action != o.Action || ev.OrderType != o.OrderType || ev.Quantity != o.TotalQuantity || ev.LimitPrice != o.LimitPrice || ev.TriggerMethod != o.TriggerMethod || ev.OutsideRTH != o.OutsideRth || ev.Trail != nil || ev.StrategyGroup != nil ||
			ev.FeeUpper == nil || !finiteProtectionOptionPolicyValue(*ev.FeeUpper) || *ev.FeeUpper < 0 || *ev.FeeUpper == math.MaxFloat64 || normCcy(ev.FeeCurrency) != normCcy(o.Currency) || strings.TrimSpace(ev.FeeCurrency) == "" {
			return 0, false
		}
		// Retain the whole fee bound after a partial fill; pruning it proportionally
		// could lose a minimum commission. Already-paid fees are deliberately not
		// credited back without final broker evidence.
		return *ev.FeeUpper, true
	}
	return 0, false
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
