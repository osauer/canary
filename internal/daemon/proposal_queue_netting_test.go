package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Review #41 (2026-09-28): a live queued authorisation is the one order
// Canary sends for its contract and side, so the pre-authorised scheduler
// places nothing beside it, the queued row's own key included; and the
// netting sees an order Canary sent a moment ago before the broker lists it,
// because placeOrder returns before the broker acknowledges.

// armQueuedFor stores a queued record for row's contract and side in state.
func armQueuedFor(t *testing.T, rig *automaticTestRig, row rpc.TradeProposal, state string) {
	t.Helper()
	e := rig.engine
	e.queued.mu.Lock()
	defer e.queued.mu.Unlock()
	if e.queued.records == nil {
		e.queued.records = map[string]*queuedAuthRecord{}
	}
	e.queued.records["q-synthetic"] = &queuedAuthRecord{
		Terms:        rpc.QueuedAuthTerms{QueueID: "q-synthetic", Key: row.Key, Bucket: row.Bucket, AccountID: rig.scope.Account, AccountMode: rig.scope.Mode},
		ContractSide: sameContractSide(row), State: state,
	}
}

func TestQueuedIntentKeepsThePreAuthorisedSchedulerOffItsContract(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction", "trailing_stop"]`)
	budget := rig.thetaProposal()
	budget.Bucket = rpc.TradeProposalBucketBudgetReduction
	budget.Key = proposalKey(budget.Bucket, budget.Contract, budget.Action)
	stop := rig.stopProposal()

	// Without a queue both rows get a pending record.
	rev := rig.install(budget, stop)
	rig.cycle()
	if rec := rig.record(budget.Key, rev); rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("budget record = %+v", rec)
	}

	// The owner arms a queue for the budget row: the scheduler withdraws both
	// records for that contract and side, the queued row's own key included.
	armQueuedFor(t, rig, budget, rpc.QueuedAuthArmed)
	rig.cycle()
	for _, key := range []string{budget.Key, stop.Key} {
		if rec := rig.record(key, rev); rec.State != rpc.TradeProposalAutomaticSuperseded || rec.Reason != automaticQueuedReason {
			t.Fatalf("a pre-authorised record stayed live beside the queue: %+v", rec)
		}
	}

	// A new revision while the queue waits (the veto-then-queue path of the
	// review) creates no record either, so nothing but the queue can send.
	budget.Quantity++
	rev2 := rig.install(budget, stop)
	for _, state := range []string{rpc.QueuedAuthArmed, rpc.QueuedAuthHeld, rpc.QueuedAuthSending} {
		armQueuedFor(t, rig, budget, state)
		rig.cycle()
		rig.noRecord(budget.Key, rev2)
		rig.noRecord(stop.Key, rev2)
	}

	// Once the queue has ended the scheduler works again.
	armQueuedFor(t, rig, budget, rpc.QueuedAuthCancelled)
	rig.cycle()
	if rec := rig.record(budget.Key, rev2); rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("an ended queue kept the scheduler off: %+v", rec)
	}
}

// stagedJournalOrder appends a journal row for an order Canary sent that the
// broker has or has not acknowledged, in the live synthetic account.
func stagedJournalOrder(t *testing.T, srv *Server, ref string, conID int, symbol, action, sendState string, at time.Time) {
	t.Helper()
	typ := orderJournalEventSendAttempted
	if sendState == orderSendStateBrokerAcknowledged {
		typ = orderJournalEventBrokerAcknowledged
	}
	if err := srv.orderJournal.Append(orderJournalEvent{
		At: at, Type: typ, OrderRef: ref, ReservedOrderID: 90 + conID%7, ClientID: 15, Account: "DU1234567", Endpoint: "127.0.0.1:4001",
		Mode: rpc.AccountModeLive, Symbol: symbol, SecType: "OPT", ConID: conID, Action: action, OrderType: rpc.OrderTypeLMT,
		TIF: rpc.OrderTIFDay, Quantity: 5, Remaining: 5, SendState: sendState, Source: proposalOrderSource,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSameContractNettingSeesAnOrderCanarySentBeforeTheBrokerListsIt(t *testing.T) {
	_, policy, pos, now := budgetThetaFixture()
	for name, tc := range map[string]struct {
		conID     int
		action    string
		sendState string
		want      string
	}{
		"sent, not yet acknowledged":     {601, rpc.OrderActionSell, orderSendStateSendAttempted, "existing_reduction_order"},
		"sent, outcome unclear":          {601, rpc.OrderActionSell, orderSendStateUncertainSend, "existing_reduction_order"},
		"acknowledged: broker's to list": {601, rpc.OrderActionSell, orderSendStateBrokerAcknowledged, ""},
		"buying the contract":            {601, rpc.OrderActionBuy, orderSendStateSendAttempted, ""},
		"another contract":               {602, rpc.OrderActionSell, orderSendStateSendAttempted, ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newOrderReconcileTestServer(t, now)
			srv.openOrderInventoryForTest = sameContractInventory() // the broker lists nothing yet
			stagedJournalOrder(t, srv, "synthetic-staged", tc.conID, "AAA", tc.action, tc.sendState, now.Add(-time.Second))
			engine := &proposalEngine{server: srv, now: func() time.Time { return now },
				budgetInput: func(*rpc.AccountResult, time.Time) budgetGovernorInput { return budgetLatchedInput() }}
			rows := generateSameContract(t, engine, policy, pos, now)
			by := rowsByBucket(t, rows, rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene)
			theta := by[rpc.TradeProposalBucketThetaHygiene]
			if tc.want == "" {
				if len(theta.Blockers) != 0 {
					t.Fatalf("theta blocked without a competing order: %v", blockerCodes(theta))
				}
				return
			}
			for _, p := range by {
				if !hasTradingBlocker(p.Blockers, tc.want) {
					t.Fatalf("%s row = %v, want %s", p.Bucket, blockerCodes(p), tc.want)
				}
			}
			if len(approvableFor(rows, 601)) != 0 {
				t.Fatal("a row stayed approvable beside Canary's unacknowledged order")
			}
			// Preview and submit read the broker afresh and still see it.
			if b := engine.duplicateProtectiveBlockers(context.Background(), theta); !hasTradingBlocker(b, tc.want) {
				t.Fatalf("submit-time netting missed Canary's unacknowledged order: %+v", b)
			}
		})
	}

	// The option exits share the journal check.
	exit, exitPolicy, exitPos, exitNow := lossExitThetaFixture(t)
	srv := newOrderReconcileTestServer(t, exitNow)
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: exitNow}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModeLive}, nil
	}
	stagedJournalOrder(t, srv, "synthetic-exit", 42, "TEST", rpc.OrderActionSell, orderSendStateSendAttempted, exitNow.Add(-time.Second))
	exit.server = srv
	by := rowsByBucket(t, generateSameContract(t, exit, exitPolicy, exitPos, exitNow), rpc.TradeProposalBucketOptionLossExit)
	if !hasTradingBlocker(by[rpc.TradeProposalBucketOptionLossExit].Blockers, "existing_option_exit_order") {
		t.Fatalf("the loss exit missed Canary's unacknowledged close: %v", blockerCodes(by[rpc.TradeProposalBucketOptionLossExit]))
	}
}
