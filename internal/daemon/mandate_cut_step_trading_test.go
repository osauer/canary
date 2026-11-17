//go:build trading

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// cutStepRig is the scheduler's trading rig with one mandate sale placed and
// working at the broker, and [mandate.reduction] written as the test asks.
type cutStepRig struct {
	*automaticTestRig
	broker   *brokerCallLog
	prop     rpc.TradeProposal
	revision string
	bid, ask float64
	fx       *float64
	sendErr  error
}

// writeCutSteps puts the stepping numbers, and the give bound when asked,
// into the constitution in force.
func (r *automaticTestRig) writeCutSteps(after, steps *int, give *float64) {
	c := &risk.Constitution{Mandate: &risk.ConstitutionMandate{}}
	if r.server.riskPolicies != nil && r.server.riskPolicies.active != nil {
		copied := *r.server.riskPolicies.active
		c = &copied
		m := risk.ConstitutionMandate{}
		if c.Mandate != nil {
			m = *c.Mandate
		}
		c.Mandate = &m
	}
	c.Mandate.Reduction = &risk.ConstitutionMandateReduction{CheapLegGivePctOfLoss: give, UnfilledStepAfterMinutes: after, UnfilledSteps: steps}
	r.server.riskPolicies = &riskPolicyManager{active: c}
}

// newCutStepRig places a 40-share mandate sale at 24.95 (24.90 bid, 25.10
// ask, a mark of 25 and so a worst price of 18.75) and has the broker
// acknowledge it. The owner's numbers are written: 10 minutes, 2 steps.
func newCutStepRig(t *testing.T) *cutStepRig {
	t.Helper()
	r := &cutStepRig{automaticTestRig: newAutomaticTradingRig(t, automaticMandateAuthority), broker: &brokerCallLog{}, bid: 24.9, ask: 25.1}
	r.writeCutSteps(new(10), new(2), nil)
	r.quotes()
	r.server.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		if r.sendErr != nil {
			return r.sendErr
		}
		r.broker.mu.Lock()
		defer r.broker.mu.Unlock()
		r.broker.orders = append(r.broker.orders, *order)
		return nil
	}
	r.place(r.mandateProposal(automaticTestStockRow(), 40, 1, 1))
	return r
}

func (r *cutStepRig) quotes() {
	r.server.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask, mid := r.bid, r.ask, (r.bid+r.ask)/2
		return rpc.OrderQuoteSnapshot{Symbol: contract.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	r.server.fillQuotes = &fillQuoteRuntime{fx: func(context.Context, string) (string, *float64, string, string) {
		return "EUR", r.fx, "test", ""
	}}
}

// place sends prop through the scheduler and acknowledges the order.
func (r *cutStepRig) place(prop rpc.TradeProposal) {
	r.t.Helper()
	r.prop = prop
	r.revision = r.install(prop)
	r.cycle()
	r.notice(r.record(prop.Key, r.revision))
	r.advance(31 * time.Minute)
	r.cycle()
	rec := r.rec()
	if r.broker.count() != 1 || rec.State != rpc.TradeProposalAutomaticSubmitted || rec.OrderRef == "" {
		r.t.Fatalf("the cut was not placed: %d broker calls, record %+v", r.broker.count(), rec)
	}
	r.report(0)
}

// report journals the broker's status of the working order: filled so far.
func (r *cutStepRig) report(filled float64) {
	r.t.Helper()
	view := r.view()
	ev := orderJournalEventFromView(view, orderJournalEventBrokerAcknowledged, r.now)
	ev.Status, ev.SendState = "Submitted", orderSendStateBrokerAcknowledged
	ev.Filled, ev.Remaining = filled, view.Quantity-filled
	if err := r.server.orderJournal.Append(ev); err != nil {
		r.t.Fatalf("journal the broker's report: %v", err)
	}
}

func (r *cutStepRig) rec() automaticSubmissionRecord { return r.record(r.prop.Key, r.revision) }

func (r *cutStepRig) view() rpc.OrderView {
	r.t.Helper()
	views, _, err := r.server.loadOrderViews()
	if err != nil {
		r.t.Fatalf("load order views: %v", err)
	}
	ref := r.rec().OrderRef
	for _, v := range views {
		if v.OrderRef == ref {
			return v
		}
	}
	r.t.Fatalf("no order view for %s", ref)
	return rpc.OrderView{}
}

// pass advances the clock and runs one stepping pass.
func (r *cutStepRig) pass(after time.Duration) {
	r.advance(after)
	r.engine.runMandateCutSteps(context.Background())
}

// sent is how many orders reached the broker; the first is the placement.
func (r *cutStepRig) sent() int { return r.broker.count() }

func (r *cutStepRig) lastLimit() float64 {
	r.broker.mu.Lock()
	defer r.broker.mu.Unlock()
	return r.broker.orders[len(r.broker.orders)-1].LmtPrice
}

// An unfilled mandate cut waits the written minutes, moves its limit to the
// bid, waits again, moves once more, and then rests: two steps, never a
// third (owner decision 2026-11-17 09:33 CET).
func TestUnfilledMandateCutStepsToTheBidTwice(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	if got := r.lastLimit(); got != 24.95 {
		t.Fatalf("placed at %v, want halfway to the bid, 24.95", got)
	}
	if step := r.rec().Step; step == nil || step.WorstPrice != 18.75 || step.SpreadCapPct != 2 || step.Count != 0 {
		t.Fatalf("placement recorded bounds %+v, want worst price 18.75 and a 2%% cap", step)
	}
	r.pass(9 * time.Minute)
	if r.sent() != 1 {
		t.Fatalf("stepped %d times before the wait had passed", r.sent()-1)
	}
	r.pass(time.Minute)
	if r.sent() != 2 || r.lastLimit() != 24.9 {
		t.Fatalf("after 10 minutes: %d sends, last limit %v; want one step to the bid, 24.90", r.sent(), r.lastLimit())
	}
	rec := r.rec()
	if rec.Step.Count != 1 || !rec.Step.Confirmed || rec.Step.From != 24.95 || rec.Step.To != 24.9 || !strings.Contains(rec.Reason, "Canary moved its limit from 24.95 to 24.90 (step 1 of 2)") {
		t.Fatalf("record after the first step: %+v, reason %q", rec.Step, rec.Reason)
	}
	if a, m := r.eventCount(r.prop.Key, r.revision, automaticEventStepAttempted), r.eventCount(r.prop.Key, r.revision, automaticEventStepped); a != 1 || m != 1 {
		t.Fatalf("journaled %d attempts and %d accepted steps, want 1 and 1", a, m)
	}
	// The breach's row, re-served behind the working order, says where the
	// order's limit stands.
	r.brokerOrders = []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: r.view().ReservedOrderID, ClientID: 31, ClientIDPresent: true, Account: r.scope.Account,
		Symbol: "SYN", SecType: "STK", ConID: 101, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, TotalQuantity: 40, Remaining: 40, Status: "Submitted"}}
	waiting := r.prop
	waiting.Revision = "a-later-revision"
	blockers := r.engine.reductionOrderBlockers(context.Background(), waiting, false)
	if len(blockers) != 1 || blockers[0].Code != reductionOrderExistingCode || !strings.Contains(blockers[0].Message, "The working order: unfilled, so Canary moved its limit from 24.95 to 24.90 (step 1 of 2)") {
		t.Fatalf("the waiting row does not say where the working order stands: %+v", blockers)
	}
	r.brokerOrders = nil
	r.report(0)
	r.bid, r.ask = 24.7, 24.9
	r.pass(9 * time.Minute)
	if r.sent() != 2 {
		t.Fatal("the second step did not wait its own ten minutes")
	}
	r.pass(time.Minute)
	if r.sent() != 3 || r.lastLimit() != 24.7 || r.rec().Step.Count != 2 {
		t.Fatalf("after the second wait: %d sends, last limit %v, record %+v", r.sent(), r.lastLimit(), r.rec().Step)
	}
	r.report(0)
	r.bid, r.ask = 24.4, 24.6
	r.pass(30 * time.Minute)
	r.pass(30 * time.Minute)
	if r.sent() != 3 {
		t.Fatalf("a third step was taken: %d sends", r.sent())
	}
}

// A bid below the worst price the order was placed under moves the limit to
// that worst price and no further, and a limit already there rests.
func TestMandateCutStepNeverGoesBelowTheWorstPrice(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	r.bid, r.ask = 18.4, 18.5
	r.pass(10 * time.Minute)
	if r.sent() != 2 || r.lastLimit() != 18.75 {
		t.Fatalf("with the bid at 18.40: %d sends, last limit %v; want one step to the worst price 18.75", r.sent(), r.lastLimit())
	}
	r.report(0)
	r.pass(10 * time.Minute)
	if r.sent() != 2 {
		t.Fatalf("a limit at its worst price moved again: %d sends, last %v", r.sent(), r.lastLimit())
	}
	if rec := r.rec(); rec.Step.Count != 1 || !strings.Contains(rec.Step.Rests, "lowest its bounds allow") || !strings.Contains(rec.Reason, "lowest its bounds allow") {
		t.Fatalf("record of the resting limit: %+v, reason %q", rec.Step, rec.Reason)
	}
}

// Beyond the spread cap the give bound holds as well: the limit moves no
// further from the mid than cheap_leg_give_pct_of_loss of the loss the cut
// removes allows, and takes no step where that cannot be measured.
func TestMandateCutStepBeyondTheSpreadCapIsBoundedByTheGive(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		give *float64
		fx   *float64
		want float64 // 0: no step
	}{
		// One contract may give 2% × 1500 / 5 = 6 in the base currency,
		// 6 / (100 × 0.9) = 0.0667 a share: 0.50 - 0.0667 = 0.4333, on the
		// penny grid 0.44. The bid, 0.20, and the worst price, 0.38, are
		// both beyond it.
		"give bound written":   {new(2.0), new(0.9), 0.44},
		"give bound unwritten": {nil, new(0.9), 0},
		"no exchange rate":     {new(2.0), nil, 0},
	} {
		r := &cutStepRig{automaticTestRig: newAutomaticTradingRig(t, automaticMandateAuthority), broker: &brokerCallLog{}, bid: 0.48, ask: 0.52, fx: new(0.9)}
		r.writeCutSteps(new(10), new(2), tc.give)
		r.quotes()
		r.broker.install(r.server)
		r.server.orderPreviewPositionImpact = fixedPreviewPosition(5, 0, rpc.OrderPositionEffectClose)
		row := automaticTestStockRow()
		row.ConID, row.SecType, row.Quantity, row.Mark, row.Bid, row.Ask, row.Multiplier = 201, "OPT", 5, 0.5, new(0.48), new(0.52), 100
		prop := r.mandateProposal(row, 5, 1, 1)
		prop.Contract = rpc.ContractParams{ConID: 201, Symbol: "SYN", SecType: "OPT", Exchange: "SMART", Currency: "USD", Right: "C", Strike: 40, Expiry: "20261218", Multiplier: 100}
		prop.Key = proposalKey(prop.Bucket, prop.Contract, prop.Action)
		prop.LimitPrice = new(0.5)
		prop.Mandate.LossCutBase = 1500
		r.place(prop)
		if got := r.lastLimit(); got != 0.49 {
			t.Fatalf("%s: placed at %v, want 0.49", name, got)
		}
		r.fx = tc.fx
		r.bid, r.ask = 0.20, 0.80
		r.pass(10 * time.Minute)
		switch {
		case tc.want == 0 && r.sent() != 1:
			t.Fatalf("%s: stepped to %v with no measurable give", name, r.lastLimit())
		case tc.want == 0 && !strings.Contains(r.rec().Step.Rests, "wider than its 25% cap"):
			t.Fatalf("%s: record does not say why it rests: %+v", name, r.rec().Step)
		case tc.want != 0 && (r.sent() != 2 || r.lastLimit() != tc.want):
			t.Fatalf("%s: %d sends, last limit %v; want one step to %v", name, r.sent(), r.lastLimit(), tc.want)
		}
	}
}

// A partly filled cut steps its remainder only: the broker gets the
// cumulative quantity, the sold part included, at the new limit.
func TestMandateCutStepKeepsWhatIsLeftOfAPartFill(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	r.report(15)
	r.server.orderPreviewPositionImpact = fixedPreviewPosition(25, 0, rpc.OrderPositionEffectClose)
	r.pass(10 * time.Minute)
	if r.sent() != 2 || r.lastLimit() != 24.9 {
		t.Fatalf("%d sends, last limit %v", r.sent(), r.lastLimit())
	}
	if got := r.broker.orders[1].TotalQty; got != 40 {
		t.Fatalf("the step sent a total of %d, want the order's 40 with 15 filled", got)
	}
}

// Without both numbers written, or with zero steps, a placed cut keeps its
// limit.
func TestMandateCutKeepsItsLimitWhileTheStepsAreUnwritten(t *testing.T) {
	t.Parallel()
	for name, write := range map[string]func(*cutStepRig){
		"nothing written": func(r *cutStepRig) { r.writeCutSteps(nil, nil, nil) },
		"no step count":   func(r *cutStepRig) { r.writeCutSteps(new(10), nil, nil) },
		"zero steps":      func(r *cutStepRig) { r.writeCutSteps(new(10), new(0), nil) },
	} {
		r := newCutStepRig(t)
		write(r)
		r.pass(time.Hour)
		if r.sent() != 1 {
			t.Fatalf("%s: the limit moved to %v", name, r.lastLimit())
		}
	}
}

// A step is taken only under the authority the order was placed under:
// disarmed, armed anew, the bucket no longer pre-authorised or trading
// frozen, and the order keeps its limit.
func TestMandateCutStepNeedsTheStandingAuthority(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*cutStepRig){
		"disarmed":       func(r *cutStepRig) { r.disarmStanding() },
		"armed anew":     func(r *cutStepRig) { r.disarmStanding(); r.advance(time.Second); r.armStanding("protect") },
		"trading frozen": func(r *cutStepRig) { r.server.platformSettings = frozenPlatformSettings() },
		"bucket no longer pre-authorised": func(r *cutStepRig) {
			r.server.protectionPolicies = writePreAuthPolicy(r.t, preAuthPolicyTOML(automaticTrailingStopAuthority, 2))
			r.server.protectionPolicies.reload()
		},
	} {
		r := newCutStepRig(t)
		change(r)
		r.pass(10 * time.Minute)
		r.pass(10 * time.Minute)
		if r.sent() != 1 {
			t.Fatalf("%s: the limit moved to %v", name, r.lastLimit())
		}
		if r.rec().Step.Count != 0 {
			t.Fatalf("%s: a refused step was spent: %+v", name, r.rec().Step)
		}
	}
}

// Once the breach's row is no longer served the cut is no longer wanted at
// a worse price, and a held row waits for its verdict.
func TestMandateCutStepNeedsTheBreachStillServed(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	r.install()
	r.pass(10 * time.Minute)
	if r.sent() != 1 {
		t.Fatalf("stepped to %v after the breach ended", r.lastLimit())
	}
	if rests := r.rec().Step.Rests; !strings.Contains(rests, "no longer served") {
		t.Fatalf("record does not say why it rests: %q", rests)
	}

	held := newCutStepRig(t)
	prop := held.prop
	prop.Mandate.Held, prop.Mandate.HoldReason = true, "the verdict is older than the book"
	held.engine.mu.Lock()
	held.engine.snapshot.Proposals = []rpc.TradeProposal{prop}
	held.engine.mu.Unlock()
	held.pass(10 * time.Minute)
	if held.sent() != 1 {
		t.Fatalf("stepped to %v while the row was held", held.lastLimit())
	}
}

// A cut that filled or was cancelled ends its stepping for good, quietly.
func TestMandateCutStepEndsWithTheOrder(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	ev := orderJournalEventFromView(r.view(), orderJournalEventBrokerAcknowledged, r.now)
	ev.Status, ev.SendState, ev.Filled, ev.Remaining = "Filled", orderSendStateBrokerAcknowledged, 40, 0
	if err := r.server.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	if r.view().Open {
		t.Fatal("fixture: the filled order still reads as open")
	}
	r.pass(10 * time.Minute)
	if rec := r.rec(); r.sent() != 1 || !rec.Step.Done || rec.stepDue(r.now.Add(time.Hour), 10*time.Minute, 2) {
		t.Fatalf("a filled cut is still stepping: %d sends, %+v", r.sent(), rec.Step)
	}
}

// A step is spent before the broker call. A send whose outcome is uncertain
// is never repeated; one proven unsent is given back and tried again.
func TestMandateCutStepIsSpentOnceUnlessProvenUnsent(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	r.sendErr = errors.New("socket closed mid-write")
	r.pass(10 * time.Minute)
	r.sendErr = nil
	if rec := r.rec(); rec.Step.Count != 1 || rec.Step.Confirmed || !strings.Contains(rec.Reason, "tried to move its limit from 24.95 to 24.90; whether the broker took the change is not known") {
		t.Fatalf("an uncertain send: step %+v, reason %q; want it spent and worded as an attempt", rec.Step, rec.Reason)
	}
	r.pass(time.Minute)
	if r.sent() != 1 {
		t.Fatal("an uncertain step was sent again within its wait")
	}

	u := newCutStepRig(t)
	u.sendErr = ibkrlib.WithSendDisposition(errors.New("not connected"), ibkrlib.SendDispositionDefinitelyUnsent)
	u.pass(10 * time.Minute)
	u.sendErr = nil
	if rec := u.rec(); rec.Step.Count != 0 {
		t.Fatalf("a step proven unsent stayed spent: %+v", rec.Step)
	}
	// The broker's next report shows the limit it still holds.
	ev := orderJournalEventFromView(u.view(), orderJournalEventBrokerAcknowledged, u.now)
	ev.Status, ev.SendState, ev.LimitPrice = "Submitted", orderSendStateBrokerAcknowledged, 24.95
	if err := u.server.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	u.pass(time.Minute)
	if u.sent() != 2 || u.lastLimit() != 24.9 || u.rec().Step.Count != 1 {
		t.Fatalf("the unsent step was not retried: %d sends, record %+v", u.sent(), u.rec().Step)
	}
}

// The scheduler's origin may change one order only: the one its grant
// names, only its limit, only downward, only to the granted price at or
// above the worst price. Without such a grant it changes nothing, and a
// step's grant places nothing.
func TestDaemonPreAuthorisedModifyNeedsAStepGrantForThatOrder(t *testing.T) {
	t.Parallel()
	r := newCutStepRig(t)
	view := r.view()
	origin := rpc.OrderOriginDaemonPreAuthorised
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 40, LimitPrice: 24.9}
	if err := r.server.automaticStepGrantCovers(rpc.OrderOriginHumanTTY, view, draft); err != nil {
		t.Fatalf("another origin is not this check's to refuse: %v", err)
	}
	grant := func(step *automaticStepGrant) {
		r.server.automaticGrant.Store(&automaticWriteGrant{Key: r.prop.Key, Revision: r.revision, Bucket: rpc.TradeProposalBucketMandateReduction, Step: step})
	}
	good := automaticStepGrant{OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Limit: 24.9, Floor: 18.75}
	for name, tc := range map[string]struct {
		step  *automaticStepGrant
		draft rpc.OrderDraft
	}{
		"a submission's grant, no step": {nil, draft},
		"another order":                 {&automaticStepGrant{OrderRef: "ord-other", ReservedOrderID: view.ReservedOrderID, Limit: 24.9, Floor: 18.75}, draft},
		"another limit than granted":    {&good, rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 40, LimitPrice: 24.8}},
		"a changed quantity":            {&good, rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 20, LimitPrice: 24.9}},
		"below the worst price":         {&automaticStepGrant{OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Limit: 18, Floor: 18.75}, rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 40, LimitPrice: 18}},
		"a raised limit":                {&automaticStepGrant{OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Limit: 25.5, Floor: 18.75}, rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 40, LimitPrice: 25.5}},
		"a purchase":                    {&good, rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 40, LimitPrice: 24.9}},
		"no worst price on the grant":   {&automaticStepGrant{OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Limit: 24.9}, draft},
	} {
		grant(tc.step)
		if err := r.server.automaticStepGrantCovers(origin, view, tc.draft); err == nil {
			t.Fatalf("%s: the modify was admitted", name)
		}
	}
	grant(&good)
	if err := r.server.automaticStepGrantCovers(origin, view, draft); err != nil {
		t.Fatalf("the granted step was refused: %v", err)
	}
	if err := r.server.automaticStepGrantRefusesPlace(origin); err == nil {
		t.Fatal("a step's grant admitted a placement")
	}
	r.server.automaticGrant.Store(nil)
	if err := r.server.automaticStepGrantCovers(origin, view, draft); err == nil {
		t.Fatal("a modify with no grant was admitted")
	}
}

// Stops and every other pre-authorised row carry no stepping state, so the
// pass never touches their orders.
func TestOnlyAMandateSaleCarriesSteppingState(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticMandateAuthority)
	if step := rig.engine.mandateCutStepBounds(rig.stopProposal()); step != nil {
		t.Fatalf("a trailing stop got stepping state: %+v", step)
	}
	buy := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	buy.Action = rpc.OrderActionBuy
	if step := rig.engine.mandateCutStepBounds(buy); step != nil {
		t.Fatalf("a purchase got stepping state: %+v", step)
	}
	rec := automaticSubmissionRecord{State: rpc.TradeProposalAutomaticSubmitted, EngineBucket: rpc.TradeProposalBucketTrailingStop, OrderRef: "ord-1",
		SubmittedAt: rig.now.Add(-time.Hour), Step: &automaticCutStep{WorstPrice: 1, SpreadCapPct: 2}}
	if rec.stepDue(rig.now, 10*time.Minute, 2) {
		t.Fatal("a record of another bucket came due for a step")
	}
}
