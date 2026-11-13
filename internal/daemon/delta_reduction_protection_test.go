package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	protectionTestCalls  = 9811
	protectionTestCallsB = 9812
	protectionTestPuts   = 9821
	protectionTestShort  = 9831
)

// protectionTestBook is a book of bought calls under index protection sized
// for more than it holds: SYNA calls carry +500,000 EUR of delta, SYNB calls
// +300,000 EUR, the hedge-listed puts −1,000,000 EUR, so the whole book
// leans short by 200,000 EUR. extra rows join it.
func protectionTestBook(extra ...rpc.PositionView) *rpc.PositionsResult {
	option := func(symbol string, conID int, right string, qty, delta, spot float64) rpc.PositionView {
		return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeOption, ConID: conID, Currency: "EUR", Quantity: qty, Multiplier: 100,
			Mark: 3, MarketValue: 300 * qty, Expiry: "20270115", Strike: 100, Right: right, Delta: new(delta), Underlying: new(spot)}
	}
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	pos := &rpc.PositionsResult{
		AsOf: mandateTestNow,
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Scope: rpc.AccountDataScope{AccountID: deltaTestScope.Account, AccountMode: deltaTestScope.Mode}},
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Options: []rpc.PositionView{
			option("SYNA", protectionTestCalls, "C", 100, 0.5, 100),
			option("SYNB", protectionTestCallsB, "C", 60, 0.5, 100),
			option(hedge, protectionTestPuts, "P", 40, -0.5, 500),
		},
	}
	for _, row := range extra {
		if row.SecType == rpc.SecTypeOption {
			pos.Options = append(pos.Options, row)
		} else {
			pos.Stocks = append(pos.Stocks, row)
		}
	}
	return pos
}

func protectionTestDraft(row rpc.PositionView, qty int) (rpc.OrderDraft, rpc.OrderPositionImpact) {
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: qty, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: rpc.ContractParams{ConID: row.ConID, Symbol: row.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "EUR",
			Expiry: row.Expiry, Strike: row.Strike, Right: row.Right, Multiplier: 100}}
	return draft, protectiveExitTestPosition(row.Quantity, rpc.OrderActionSell, qty)
}

// A sale of a bought call passes the caps although the index puts make the
// whole book lean short: the book's delta is measured without its protection
// (owner decision 2026-11-13 12:27 CET). Counted in, the sale raised the
// whole book's absolute delta and kept the cap, which is the reading a
// policy with no hedge list still gives. A sale of the protection itself is
// judged against the whole book, as before the decision.
func TestDeltaExitLeavesProtectionOutOfTheBook(t *testing.T) {
	t.Parallel()
	pol := risk.DefaultRulebookPolicy()
	pos := protectionTestBook()
	calls, puts := pos.Options[0], pos.Options[2]
	limits := deltaTestLimits()

	draft, position := protectionTestDraft(calls, 40)
	whole := measureDeltaReduction(pos, deltaTestScope, draft, risk.RulebookPolicy{})
	if !whole.Current || whole.BookBefore != -200000 || whole.ProtectionLeftOut != 0 {
		t.Fatalf("whole book = %+v, want −200,000 EUR with nothing left out", whole)
	}
	if ok, why := deltaReducingExit(draft, position, whole, deltaTestInventory); ok || !strings.Contains(why, "the whole book (200,000 EUR before, 400,000 EUR after)") {
		t.Fatalf("call sale with protection counted: exempt %v %q, want capped on the whole book", ok, why)
	}
	ev := measureDeltaReduction(pos, deltaTestScope, draft, pol)
	if !ev.Current || ev.NetBefore != 500000 || ev.BookBefore != 800000 || ev.ProtectionLeftOut != -1000000 {
		t.Fatalf("book without protection = %+v, want 800,000 EUR and −1,000,000 EUR left out", ev)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(12000), "EUR", deltaTestInventory, ev); err != nil {
		t.Fatalf("a 40-contract call sale under protection must pass the 5-contract cap: %v", err)
	}

	// Selling protection is judged against the whole book: here it takes
	// the book from 200,000 EUR short to 50,000 EUR long, which lowers it.
	draft, position = protectionTestDraft(puts, 10)
	sold := measureDeltaReduction(pos, deltaTestScope, draft, pol)
	if !sold.Current || sold.BookBefore != -200000 || sold.ProtectionLeftOut != 0 {
		t.Fatalf("sale of protection = %+v, want the whole book", sold)
	}
	if ok, why := deltaReducingExit(draft, position, sold, deltaTestInventory); !ok {
		t.Fatalf("put sale that lowers the whole book: %q", why)
	}
	// In a book that is long with its protection, the same sale raises the
	// whole book's delta and keeps the cap.
	long := protectionTestBook(rpc.PositionView{Symbol: "SYNC", SecType: rpc.SecTypeStock, ConID: 9841, Currency: "EUR", Quantity: 5000, Multiplier: 1, Mark: 100, MarketValue: 500000})
	sold = measureDeltaReduction(long, deltaTestScope, draft, pol)
	if ok, why := deltaReducingExit(draft, position, sold, deltaTestInventory); ok || !strings.Contains(why, "the whole book (300,000 EUR before, 550,000 EUR after)") {
		t.Fatalf("put sale in a long book: exempt %v %q, want capped on the whole book", ok, why)
	}

	// A book short for another reason keeps the cap, and the refusal says
	// which book was measured.
	short := protectionTestBook(rpc.PositionView{Symbol: "SYNC", SecType: rpc.SecTypeStock, ConID: protectionTestShort, Currency: "EUR", Quantity: -9000, Multiplier: 1, Mark: 100, MarketValue: -900000})
	draft, position = protectionTestDraft(calls, 40)
	ev = measureDeltaReduction(short, deltaTestScope, draft, pol)
	if ok, why := deltaReducingExit(draft, position, ev, deltaTestInventory); ok || !strings.Contains(why, "the book without its protection puts (100,000 EUR before, 300,000 EUR after)") {
		t.Fatalf("call sale in a book short without its protection: exempt %v %q, want capped", ok, why)
	}
}

// The forced reduction sizes an option cut with the order gate's own test:
// by the money caps where the gate passes the sale as a delta-reducing exit,
// by the contract cap where it does not, so no row is larger than the gate
// admits (2026-11-03: a cut past the cap was refused at every send) and none
// is held to 5 contracts where the gate would lift the cap (2026-11-13: a
// large cut planned as one order for every five contracts). The planner's
// row is sent through the gate on the same positions.
func TestMandateCycleQuantityFollowsTheOrderGate(t *testing.T) {
	t.Parallel()
	pol := risk.DefaultRulebookPolicy()
	limits := deltaTestLimits()
	gate := func(pos *rpc.PositionsResult, pol risk.RulebookPolicy, row rpc.PositionView, qty int) error {
		draft, position := protectionTestDraft(row, qty)
		ev := measureDeltaReduction(pos, deltaTestScope, draft, pol)
		return validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(float64(qty)*300), "EUR", deltaTestInventory, ev)
	}
	input := func(pos *rpc.PositionsResult, pol risk.RulebookPolicy) mandateReductionInput {
		return mandateReductionInput{MaxOrderNotional: 1e9, OrderCapBase: limits.CapBase, MaxOptionContracts: limits.MaxOptionContracts,
			Exits: &deltaExitPlan{pos: pos, pol: pol}}
	}

	// 12,000 EUR at 300 EUR a contract is 40 contracts.
	pos := protectionTestBook()
	calls := pos.Options[0]
	in := input(pos, pol)
	c := &mandateCut{row: calls, full: 100, step: 1, units: 100}
	qty, waits := mandateCycleQuantity(c, in, "EUR")
	if qty != 40 || !strings.Contains(waits, "hold one order to 40") {
		t.Fatalf("cut the gate admits: %d %q, want 40 by the money cap", qty, waits)
	}
	if err := gate(pos, pol, calls, qty); err != nil {
		t.Fatalf("the gate refuses the planner's own row: %v", err)
	}
	// With the protection counted the gate keeps the cap, and so does the row.
	none := risk.RulebookPolicy{}
	if qty, waits := mandateCycleQuantity(c, input(pos, none), "EUR"); qty != 5 || !strings.Contains(waits, "hold one order to 5") {
		t.Fatalf("cut the gate does not admit: %d %q, want the contract cap", qty, waits)
	}
	if err := gate(pos, none, calls, 40); err == nil || !strings.Contains(err.Error(), "exceeds the option cap in force of 5 contracts") {
		t.Fatalf("the gate must refuse 40 contracts where the planner held 5: %v", err)
	}
	// Positions that are not current admit nothing.
	stale := protectionTestBook()
	stale.Authority = nil
	if qty, _ := mandateCycleQuantity(c, input(stale, pol), "EUR"); qty != 5 {
		t.Fatalf("cut without current positions: %d, want the contract cap", qty)
	}

	// An order last held within the contract cap is sized again once the
	// gate lifts the cap; one the money cap set is kept through a mark move.
	c = &mandateCut{row: calls, full: 100, step: 1, units: 100, prevUnits: 100, prevQty: 5}
	if qty, _ := mandateCycleQuantity(c, in, "EUR"); qty != 40 {
		t.Fatalf("order kept at the lifted contract cap: %d, want 40", qty)
	}
	c = &mandateCut{row: calls, full: 100, step: 1, units: 100, prevUnits: 100, prevQty: 38}
	if qty, waits := mandateCycleQuantity(c, in, "EUR"); qty != 38 || !strings.Contains(waits, "kept at 38") {
		t.Fatalf("order kept through a mark move: %d %q, want 38", qty, waits)
	}

	// The second row of a cycle is judged against the book as the first
	// leaves it. Without its protection this book is 150,000 EUR long: the
	// first sale takes 200,000 EUR off and leaves it 50,000 EUR short, which
	// is smaller, and a second call sale from there raises its delta.
	short := protectionTestBook(rpc.PositionView{Symbol: "SYNC", SecType: rpc.SecTypeStock, ConID: protectionTestShort, Currency: "EUR", Quantity: -6500, Multiplier: 1, Mark: 100, MarketValue: -650000})
	in = input(short, pol)
	first := &mandateCut{row: short.Options[0], full: 100, step: 1, units: 100}
	second := &mandateCut{row: short.Options[1], full: 60, step: 1, units: 60}
	if qty, _ := mandateCycleQuantity(second, in, "EUR"); qty != 40 {
		t.Fatalf("second line judged alone: %d, want 40", qty)
	}
	if qty, _ = mandateCycleQuantity(first, in, "EUR"); qty != 40 {
		t.Fatalf("first line: %d, want 40", qty)
	}
	in.Exits.record(first.row, rpc.OrderActionSell, qty)
	if qty, _ := mandateCycleQuantity(second, in, "EUR"); qty != 5 {
		t.Fatalf("second line after the first has taken the book short: %d, want the contract cap", qty)
	}
}

// The policy check judges an exit with the gate's own book: a call line
// under protection is exitable past the caps, and the protection line is
// judged against the whole book.
func TestPolicyCheckExitLeavesProtectionOutOfTheBook(t *testing.T) {
	t.Parallel()
	pol := risk.DefaultRulebookPolicy()
	book := PolicyCheckBookFrom(&rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 240000, AsOf: mandateTestNow}, protectionTestBook())
	if book == nil || len(book.Positions) != 3 {
		t.Fatalf("book = %+v", book)
	}
	calls, puts := book.Positions[0], book.Positions[2]
	if lowers, known, why := book.exitLowersAbsoluteDelta(calls, 40, pol); !known || !lowers {
		t.Fatalf("call exit under protection: lowers %v known %v %q", lowers, known, why)
	}
	if lowers, known, why := book.exitLowersAbsoluteDelta(calls, 40, risk.RulebookPolicy{}); !known || lowers || !strings.Contains(why, "the whole book") {
		t.Fatalf("call exit with protection counted: lowers %v known %v %q", lowers, known, why)
	}
	if lowers, known, why := book.exitLowersAbsoluteDelta(puts, 10, pol); !known || !lowers {
		t.Fatalf("protection exit that lowers the whole book: lowers %v known %v %q", lowers, known, why)
	}
}
