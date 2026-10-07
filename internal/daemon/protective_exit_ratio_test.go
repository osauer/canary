package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A three-unit 1:2 combo already buys back six of eight short puts. A
// separate five-contract buy must keep the cap: both orders could fill
// before positions update and together cross the held line.
func TestExitInventoryCountsComboLegRatios(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	journal := newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	group := &rpc.StrategyOrderDraft{Units: 3, UnitsBefore: 4, UnitsAfter: 1, GuaranteedCombo: true,
		Legs: []rpc.StrategyOrderLeg{
			deltaTestLeg(deltaTestLongPuts, 1, rpc.OrderActionSell, 3, 4),
			deltaTestLeg(deltaTestShortPuts, -2, rpc.OrderActionBuy, 6, -8),
		}}
	for i := range group.Legs {
		group.Legs[i].Contract.Right = "P"
	}
	err := journal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: asOf.Add(time.Second),
		OrderRef: "synthetic-ratio-close", ReservedOrderID: 301, ClientID: 31,
		Account: deltaTestScope.Account, Mode: deltaTestScope.Mode, Endpoint: "127.0.0.1:7497", Symbol: "SYNB", SecType: "BAG",
		Action: rpc.OrderActionSell, Quantity: 3, Status: "Submitted", StrategyGroup: group})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{orderJournal: journal}
	inventory, ok := srv.journalOrderViewsForInventory()
	if !ok {
		t.Fatal("could not read synthetic combo journal")
	}
	draft := deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 5)
	position := protectiveExitTestPosition(-8, draft.Action, draft.Quantity)
	limits := risk.EvaluateOrderLimits(testOrderLimitsTable(10000), "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: asOf}, nil, "")
	for _, brokerAcknowledged := range []bool{false, true} {
		snapshot := ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: asOf}
		if brokerAcknowledged {
			snapshot.Orders = []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder,
				OrderID: 301, PermID: 800301, ClientID: 31, ClientIDPresent: true, Account: deltaTestScope.Account,
				Symbol: "SYNB", SecType: "BAG", Action: rpc.OrderActionSell, TotalQuantity: 3, Remaining: 3, Status: "Submitted"}}
		}
		inv := protectiveExitInventoryFromSnapshot(snapshot, inventory, deltaTestScope, draft, orderPreviewReplaceTarget{})
		if inv.OtherWorkingSameSide != 6 {
			t.Errorf("acknowledged=%t: counted %g competing contracts, want 6", brokerAcknowledged, inv.OtherWorkingSameSide)
		}
		if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(30000), "EUR", inv, deltaTestEvidence()); err == nil {
			t.Errorf("acknowledged=%t: cap exception admitted two exits totaling 11 contracts against 8 held", brokerAcknowledged)
		}
	}
}

// A partially filled combo reserves only its remaining contracts, on the
// leg's actual side. Missing terms never become evidence of zero exposure.
func TestExitInventoryRequiresKnownComboCapacity(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	draft := deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 5)
	position := protectiveExitTestPosition(-8, draft.Action, draft.Quantity)
	limits := risk.EvaluateOrderLimits(testOrderLimitsTable(10000), "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: asOf}, nil, "")
	snapshot := ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: asOf, Orders: []ibkrlib.OrderLifecycleEvent{{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 301, PermID: 800301,
		ClientID: 31, ClientIDPresent: true, Account: deltaTestScope.Account,
		Symbol: "SYNB", SecType: "BAG", Action: rpc.OrderActionSell, TotalQuantity: 3, Filled: 2, Remaining: 1, Status: "Submitted",
	}}}
	view := rpc.OrderView{OrderRef: "synthetic-ratio-close", ReservedOrderID: 301, PermID: 800301,
		ClientID: 31, Account: deltaTestScope.Account, Mode: deltaTestScope.Mode,
		Symbol: "SYNB", SecType: "BAG", Action: rpc.OrderActionSell, Quantity: 3, Remaining: 1, Open: true, UpdatedAt: asOf}
	for _, tc := range []struct {
		name    string
		legs    map[int]rpc.StrategyOrderLeg
		want    float64
		unknown bool
	}{
		{name: "remaining contracts", legs: map[int]rpc.StrategyOrderLeg{deltaTestShortPuts: {Ratio: -2, Action: rpc.OrderActionBuy}}, want: 2},
		{name: "known opposite side", legs: map[int]rpc.StrategyOrderLeg{deltaTestShortPuts: {Ratio: 2, Action: rpc.OrderActionSell}}},
		{name: "unrelated leg", legs: map[int]rpc.StrategyOrderLeg{deltaTestLongCalls: {Ratio: 2, Action: rpc.OrderActionSell}}},
		{name: "hand combo", unknown: true},
		{name: "missing ratio", legs: map[int]rpc.StrategyOrderLeg{deltaTestShortPuts: {Action: rpc.OrderActionBuy}}, unknown: true},
		{name: "missing side", legs: map[int]rpc.StrategyOrderLeg{deltaTestShortPuts: {Ratio: 2}}, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := journalInventory{views: []rpc.OrderView{view}, legsByRef: map[string]map[int]rpc.StrategyOrderLeg{view.OrderRef: tc.legs}}
			inv := protectiveExitInventoryFromSnapshot(snapshot, journal, deltaTestScope, draft, orderPreviewReplaceTarget{})
			err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(30000), "EUR", inv, deltaTestEvidence())
			if tc.unknown {
				if err == nil || !strings.Contains(err.Error(), "cannot determine how much another working order would trade") {
					t.Fatalf("unknown capacity refusal = %v", err)
				}
			} else if inv.OtherWorkingSameSide != tc.want || err != nil {
				t.Fatalf("competing=%g, err=%v, want %g and an eligible reduction", inv.OtherWorkingSameSide, err, tc.want)
			}
		})
	}
}
