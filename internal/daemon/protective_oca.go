package daemon

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// One-cancels-all beside Canary's own stop (owner decision 2026-10-12 19:55
// CEST, which admits a sale of at most the confirmed long position whatever
// else is working, and the residual-risk fix recorded in
// internal-docs/design/protective-stop-guard.md, "One-cancels-all").
//
// Two orders Canary itself placed or admitted must never sell the same
// shares twice. A sell stop or trail Canary places is born in its own IBKR
// one-cancels-all group (Order.proto ocaGroup=27, ocaType=30), type 2:
// when a member fills, in whole or in part, the broker reduces the others
// and routes one member at a time. A later Canary sale of the same contract
// that, with Canary's own working stop, would sell more than is held joins
// that group by name, so the first fill reduces the other at the broker
// without waiting for the guard. A stop the broker reports without a group
// (placed before groups existed) cannot be joined, and the IB API offers no
// way to put a working order into a group other than a full modify whose
// acceptance of a changed group is undocumented; Canary does not try. That
// stop is shrunk to what the sale leaves, or cancelled when nothing is
// left, through the guard's gated modify and cancel path, the broker's
// answer is awaited, and only then is the sale sent, plain. When the change
// fails or stays unconfirmed, the sale is refused with the reason and the
// stop is left as it is; an unlinked sale is never sent beside its own
// stop. A hand order in TWS is the owner's: never linked, never changed.
// The protective stop guard stays as the second line of defence and counts
// an unlinked Canary sale against its stops (planProtectiveStopGuard).

const (
	// protectiveOCAGroupPrefix names the group a Canary stop is born in,
	// after its order reference, which is unique per order.
	protectiveOCAGroupPrefix = "canary-oca-"
	// protectiveOCAType is reduce-with-block: the broker reduces the other
	// members by a fill and routes one member at a time.
	protectiveOCAType = ibkrlib.OcaTypeReduceWithBlock
	// protectiveOCAConfirmWait bounds how long a sale waits for the broker
	// to confirm the shrink or cancel of Canary's own stop before it is
	// sent; unconfirmed, the sale is refused.
	protectiveOCAConfirmWait = 3 * time.Second
	protectiveOCAConfirmPoll = 50 * time.Millisecond
)

// protectiveOCAGroupFor is the group a Canary stop is born in.
func protectiveOCAGroupFor(orderRef string) string {
	return protectiveOCAGroupPrefix + strings.TrimSpace(orderRef)
}

// protectiveOCAStop is one of Canary's own working sell stops or trails on
// the contract a sale is being admitted for.
type protectiveOCAStop struct {
	// View is the journal row; Order the broker's inventory row, zero when
	// the inventory did not show the order (unavailable, or sent after the
	// snapshot and not yet acknowledged).
	View  rpc.OrderView
	Order ibkrlib.OrderLifecycleEvent
	// Remaining is the broker's when echoed, else the journal's.
	Remaining float64
	// Group and Type are the broker's echo of the stop's one-cancels-all
	// membership; empty when none or not echoed.
	Group string
	Type  int
}

// protectiveOCAPlan is what admission of a sale does about Canary's own
// stops on its contract: the membership the sale is sent with (none when
// Group is empty) and the shrink or cancel steps that must succeed first.
type protectiveOCAPlan struct {
	Refusal string
	Group   string
	Type    int
	Steps   []protectiveStopGuardStep
	Stops   []protectiveOCAStop
}

// protectiveOCACandidate is a single-contract sale of a resolved contract:
// the shape that can compete with Canary's own stop.
func protectiveOCACandidate(draft rpc.OrderDraft) bool {
	return draft.StrategyGroup == nil && draft.Quantity > 0 && draft.Contract.ConID > 0 &&
		strings.EqualFold(strings.TrimSpace(draft.Action), rpc.OrderActionSell)
}

// canaryWorkingSellStops lists Canary's own open sell stops and trails on
// the draft's exact contract from the journal, with the broker's remaining
// quantity and group echo where the inventory shows them. A nil snapshot is
// an unavailable inventory: the journal rows stand alone. With a complete
// snapshot, a journal row the broker no longer shows working is gone,
// unless it moved after the snapshot began (the cache window), when it
// counts without an echo.
func canaryWorkingSellStops(views []rpc.OrderView, snapshot *ibkrlib.OpenOrderSnapshot, scope brokerStateScope, draft rpc.OrderDraft) []protectiveOCAStop {
	var working []brokerWorkingOrder
	var since time.Time
	if snapshot != nil {
		working = brokerWorkingOrders(*snapshot, views, scope)
		since = snapshot.AsOf.Add(-journalInventoryMargin)
	}
	var stops []protectiveOCAStop
	for i := range views {
		view := &views[i]
		if view.OrderRef == draft.OrderRef || !protectiveStopGuardCanaryPlaced(view, scope) ||
			!strings.EqualFold(strings.TrimSpace(view.Action), rpc.OrderActionSell) || !isProtectiveStopOrderType(view.OrderType) ||
			!journalOrderIdentity(*view).sameContract(draft.Contract) {
			continue
		}
		stop := protectiveOCAStop{View: *view, Remaining: orderViewRemainingQuantity(*view)}
		if snapshot != nil {
			echoed := false
			for _, row := range working {
				if row.Journal != nil && row.Journal.OrderRef == view.OrderRef && row.Journal.ReservedOrderID == view.ReservedOrderID {
					stop.Order, stop.Remaining = row.Order, brokerOrderRemaining(row.Order)
					stop.Group, stop.Type = strings.TrimSpace(row.Order.OcaGroup), row.Order.OcaType
					echoed = true
					break
				}
			}
			if !echoed && !view.UpdatedAt.After(since) {
				continue
			}
		}
		if stop.Remaining <= 0 {
			continue
		}
		stops = append(stops, stop)
	}
	slices.SortStableFunc(stops, func(a, b protectiveOCAStop) int { return strings.Compare(a.View.OrderRef, b.View.OrderRef) })
	return stops
}

// planProtectiveOCA decides the membership of a sale and what must happen
// to Canary's own stops first. held is the confirmed long position the
// admission read. It is pure.
func planProtectiveOCA(draft rpc.OrderDraft, held float64, stops []protectiveOCAStop) protectiveOCAPlan {
	plan := protectiveOCAPlan{Stops: stops}
	if !protectiveOCACandidate(draft) {
		return plan
	}
	// A held child protects a pending entry, including its partial fills. Its
	// shares cannot be separated safely from the current holding here. Refuse
	// before changing any stop until the exact pair is confirmed active.
	for _, stop := range stops {
		if stop.View.EntryLink != nil && stop.View.EntryState != "active" {
			plan.Refusal = "an attached entry stop is not confirmed active; wait for the entry and stop to resolve before selling this holding"
			return plan
		}
	}
	ownGroup := func() {
		if isProtectiveStopOrderType(draft.OrderType) {
			plan.Group, plan.Type = protectiveOCAGroupFor(draft.OrderRef), protectiveOCAType
		}
	}
	total := 0.0
	for _, stop := range stops {
		total += stop.Remaining
	}
	combined := total + float64(draft.Quantity)
	if len(stops) == 0 || combined <= held+1e-9 {
		// Nothing competes: a stop still gets its own group so a later sale
		// can join it; a sale goes out plain.
		ownGroup()
		return plan
	}
	if len(stops) == 1 && stops[0].Group != "" && stops[0].Type == protectiveOCAType {
		plan.Group, plan.Type = stops[0].Group, stops[0].Type
		return plan
	}
	// No group to join. Shrink the stops to what the sale leaves of the
	// position, cancelling what reaches nothing, before the sale is sent.
	units := positionUnits(draft.Contract.SecType)
	excess := combined - held
	for _, stop := range stops {
		if excess <= 1e-9 {
			break
		}
		cut := math.Min(stop.Remaining, excess)
		target := stop.Remaining - cut
		step := protectiveStopGuardStep{Order: stop.Order, View: stop.View, Position: held, Remaining: stop.Remaining}
		if target < 1-1e-9 {
			step.Kind = protectiveStopGuardCancel
			step.Reason = fmt.Sprintf("the sale of %d of the %.4g %s held leaves nothing for Canary's own stop of %.4g, which cannot join a group; cancelling it before the sale is sent",
				draft.Quantity, held, units, stop.Remaining)
		} else {
			step.Kind, step.Target = protectiveStopGuardShrink, int(math.Floor(target+1e-9))
			step.Reason = fmt.Sprintf("the sale of %d of the %.4g %s held leaves %d for Canary's own stop of %.4g, which cannot join a group; shrinking it before the sale is sent",
				draft.Quantity, held, units, step.Target, stop.Remaining)
		}
		plan.Steps = append(plan.Steps, step)
		excess -= cut
	}
	ownGroup()
	return plan
}

// positionUnits names what a contract's quantity counts.
func positionUnits(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "OPT", "FOP":
		return "contracts"
	case "":
		return "shares"
	}
	if ibkrlib.IsBillOrBond(secType) {
		return "units"
	}
	return "shares"
}

// journalUnseenWorkingSales lists Canary's own open sell orders of this
// scope that the complete inventory does not show but that moved after the
// snapshot began: sent inside the cache window and not yet acknowledged.
// They count as committed sales for the guard; they are never acted on.
func journalUnseenWorkingSales(snapshot ibkrlib.OpenOrderSnapshot, views []rpc.OrderView, scope brokerStateScope, working []brokerWorkingOrder) []brokerWorkingOrder {
	since := snapshot.AsOf.Add(-journalInventoryMargin)
	var unseen []brokerWorkingOrder
	for i := range views {
		view := &views[i]
		if !protectiveStopGuardCanaryPlaced(view, scope) || !strings.EqualFold(strings.TrimSpace(view.Action), rpc.OrderActionSell) || !view.UpdatedAt.After(since) {
			continue
		}
		if slices.ContainsFunc(working, func(row brokerWorkingOrder) bool {
			return row.Journal != nil && row.Journal.OrderRef == view.OrderRef && row.Journal.ReservedOrderID == view.ReservedOrderID
		}) {
			continue
		}
		unseen = append(unseen, brokerWorkingOrder{Journal: view, Unseen: true, Order: ibkrlib.OrderLifecycleEvent{
			Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: view.ReservedOrderID, PermID: view.PermID, ClientID: view.ClientID, ClientIDPresent: true,
			Symbol: view.Symbol, SecType: view.SecType, ConID: view.ConID, Currency: view.Currency, Action: view.Action, OrderType: view.OrderType,
			TotalQuantity: view.Quantity, Filled: view.Filled, Remaining: orderViewRemainingQuantity(*view), Status: view.Status,
			OcaGroup: view.OCAGroup, OcaType: view.OCAType,
		}})
	}
	return unseen
}
