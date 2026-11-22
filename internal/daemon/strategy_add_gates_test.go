package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	brokerlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// journalWorkingStrategyOrder records a strategy order as sent and working,
// with its legs as the strategy preview writes them.
func journalWorkingStrategyOrder(t *testing.T, s *Server, draft rpc.OrderDraft, ref string, id int) {
	t.Helper()
	draft.OrderRef = ref
	scope := s.currentBrokerStateScope()
	ev := strategyPreviewJournalEvent(draft, rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002"}, ref+"-token", s.orderNow(), rpc.OrderWhatIfResult{})
	ev.Type, ev.ReservedOrderID = orderJournalEventSendAttempted, id
	ev.Remaining, ev.Status = float64(draft.Quantity), "Submitted"
	if err := s.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
}

func emptyOpenOrderInventory(s *Server) {
	s.openOrderInventoryForTest = func(context.Context, bool) (brokerlib.OpenOrderSnapshot, brokerStateScope, error) {
		return brokerlib.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, s.currentBrokerStateScope(), nil
	}
}

// An add that is not covered (here a diagonal: the sold call expires first)
// buys options whose premium counts in full once held, so the premium limits
// read it as they read a single option buy of the same contracts. Until this
// test it met the loss gate only, and each further add passed the same way.
func TestUncoveredStrategyAddMeetsThePremiumLimits(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return strategyAddBook(srv, mandateTestLongOptionBook(t, srv, 18)), nil
	}
	srv.evaluateMandate(t.Context(), false)
	emptyOpenOrderInventory(srv)
	diagonal := func(units int) rpc.OrderDraft {
		return strategyAddTestDraft("SYNB", "EUR", units, 18, 20, addLeg{9005, "C", 80, "20270319", 1}, addLeg{addCallShort, "C", 90, "20270115", -1})
	}
	single := rpc.OrderDraft{Action: "BUY", Quantity: 5, LimitPrice: 20, OrderType: "LMT", OrderRef: "single",
		Contract: rpc.ContractParams{ConID: 9005, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 80, Expiry: "20270115", Multiplier: 100}}
	if b := srv.mandateBudgetGate(single, rpc.OrderPositionImpact{Before: 18, After: 23, Effect: rpc.OrderPositionEffectIncrease}); b == nil || b.Code != rpc.MandatePremiumBlockerCode {
		t.Fatalf("fixture: five more calls bought alone = %+v, want the premium limit to refuse", b)
	}
	if strategyAddCovered(diagonal(5).StrategyGroup) {
		t.Fatal("fixture: a diagonal read as covered")
	}
	if b := srv.mandateBudgetGate(diagonal(5), strategyAddImpact(18, 5)); b == nil || b.Code != rpc.MandatePremiumBlockerCode {
		t.Fatalf("five more units of the diagonal = %+v, want the premium limit to refuse as it refuses the calls alone", b)
	}
	if b := srv.mandateBudgetGate(diagonal(1), strategyAddImpact(18, 1)); b != nil && b.Code == rpc.MandatePremiumBlockerCode {
		t.Fatalf("one unit inside the limit was refused by the premium limit: %+v", b)
	}
	// A bought put on a hedge symbol carries a value too: an uncovered put
	// combination there is measured, not refused as unmeasurable.
	hedge := srv.rulebookPolicy().HedgeSymbols[0]
	puts := strategyAddTestDraft(hedge, "USD", 1, 2, 18, addLeg{addHedgeLongPut, "P", 500, "20270319", 1}, addLeg{addHedgeShortPut, "P", 450, "20270115", -1})
	_, input, _, _ := srv.currentMandateVerdict()
	if premium, why := strategyAddBoughtPremiumBase(puts.StrategyGroup, input); why != "" || !(premium > 0) {
		t.Fatalf("uncovered put combination on a hedge symbol: premium %v, %q; want a measured premium", premium, why)
	}
}

// A working strategy order carries the held ratio of each leg, negative for
// a sold one. The share-cover reading refused every such order as unreadable,
// on any name, and so withdrew the hedge entry of a protective put or put
// spread exactly while another strategy order was working.
func TestWorkingStrategyOrderKeepsTheProtectivePutOpen(t *testing.T) {
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		return mandateTestLongOptionBook(t, s, 60), nil
	}
	s.evaluateMandate(t.Context(), false)
	emptyOpenOrderInventory(s)
	put := rpc.OrderDraft{Action: "BUY", Quantity: 1, LimitPrice: 3, OrderType: "LMT", OrderRef: "new-put",
		Contract: rpc.ContractParams{ConID: 9010, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Right: "P", Strike: 75, Expiry: "20270115", Multiplier: 100}}
	spread := strategyAddTestDraft("SYNB", "EUR", 1, 1, 2, addLeg{9010, "P", 75, "", 1}, addLeg{9011, "P", 65, "", -1})
	spread.OrderRef = "new-spread"
	if !s.protectivePut(put) || !s.strategyAddHedgeEntry(spread) {
		t.Fatal("fixture: the put and the put spread do not protect the shares held")
	}
	journalWorkingStrategyOrder(t, s, strategyAddHedgeSpread(s, 1, 18), "work-add", 601)
	if _, _, err := s.pendingPutCover(put); err != nil {
		t.Fatalf("a working spread order on another name made the cover unreadable: %v", err)
	}
	if !s.protectivePut(put) || !s.strategyAddHedgeEntry(spread) {
		t.Fatal("a working spread order on another name withdrew the hedge entry")
	}
	// A working put spread on the same name still counts its bought puts.
	same := strategyAddTestDraft("SYNB", "EUR", 2, 1, 2, addLeg{9010, "P", 75, "", 1}, addLeg{9011, "P", 65, "", -1})
	journalWorkingStrategyOrder(t, s, same, "work-same", 602)
	if _, bought, err := s.pendingPutCover(put); err != nil || bought != 200 {
		t.Fatalf("working put spread on the name: bought cover %v err %v, want 200 shares", bought, err)
	}
}
