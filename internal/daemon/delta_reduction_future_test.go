package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// futureTestHedge is a held short of the index future worth short EUR of
// delta at a rate of 0.5: 5 USD a point at 5,000.
func futureTestHedge(contracts float64) rpc.PositionView {
	return rpc.PositionView{Symbol: hedgeFutureSymbol, SecType: rpc.SecTypeFuture, ConID: 9851, Currency: "USD", FXRate: new(0.5),
		Quantity: -contracts, Multiplier: 5, Mark: 5000, MarketValue: -contracts * 25000, Expiry: "20261218"}
}

// A held short of the index future is a line of the book like any other
// (owner decision 2026-11-14 05:45 CET, "Measure it, same test"). Until then
// the gate had no delta for it, so one hedge contract held every forced cut
// to the option contract cap. Its delta is its contract value, the figure the
// mandate and the reduce plan already use. The test is the same: the sale
// passes while the book with the hedge is still long, and keeps the cap once
// the hedge has tipped it short.
func TestDeltaExitCountsAHeldIndexFuture(t *testing.T) {
	t.Parallel()
	pol := risk.DefaultRulebookPolicy()
	limits := deltaTestLimits()

	// Without its protection puts the book is 800,000 EUR long. 20 contracts
	// are 250,000 EUR short: the book stays long and a call sale lowers it.
	pos := protectionTestBook(futureTestHedge(20))
	calls := pos.Options[0]
	draft, position := protectionTestDraft(calls, 40)
	ev := measureDeltaReduction(pos, deltaTestScope, draft, pol)
	if !ev.Current || ev.BookBefore != 550000 || ev.NetBefore != 500000 {
		t.Fatalf("book with a 20-contract hedge = %+v, want current at 550,000 EUR", ev)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(12000), "EUR", deltaTestInventory, ev); err != nil {
		t.Fatalf("a call sale in a book still long with its hedge must pass the contract cap: %v", err)
	}
	in := mandateReductionInput{MaxOrderNotional: 1e9, OrderCapBase: limits.CapBase, MaxOptionContracts: limits.MaxOptionContracts,
		Exits: &deltaExitPlan{pos: pos, pol: pol}}
	if qty, _ := mandateCycleQuantity(&mandateCut{row: calls, full: 100, step: 1, units: 100}, in, "EUR"); qty != 40 {
		t.Fatalf("forced cut beside a held hedge: %d contracts, want 40 by the money cap", qty)
	}

	// 70 contracts are 875,000 EUR short: the hedge has tipped the book
	// 75,000 EUR short, a call sale raises that, and the cap stays.
	pos = protectionTestBook(futureTestHedge(70))
	ev = measureDeltaReduction(pos, deltaTestScope, draft, pol)
	if ok, why := deltaReducingExit(draft, position, ev, deltaTestInventory); ok || !strings.Contains(why, "(75,000 EUR before, 275,000 EUR after)") {
		t.Fatalf("call sale in a book the hedge has tipped short: exempt %v %q, want capped", ok, why)
	}
	in.Exits = &deltaExitPlan{pos: pos, pol: pol}
	if qty, _ := mandateCycleQuantity(&mandateCut{row: calls, full: 100, step: 1, units: 100}, in, "EUR"); qty != 5 {
		t.Fatalf("forced cut in a book the hedge has tipped short: %d contracts, want the contract cap", qty)
	}

	// What cannot be measured still keeps the cap: a hedge without a price
	// or a rate, and any other future.
	for name, edit := range map[string]func(*rpc.PositionView){
		"no price":       func(r *rpc.PositionView) { r.Mark = 0 },
		"no rate":        func(r *rpc.PositionView) { r.FXRate = nil },
		"stale":          func(r *rpc.PositionView) { r.Stale = true },
		"another future": func(r *rpc.PositionView) { r.Symbol = "SYNF" },
	} {
		row := futureTestHedge(20)
		edit(&row)
		if ev := measureDeltaReduction(protectionTestBook(row), deltaTestScope, draft, pol); ev.Current || ev.Reason == "" {
			t.Fatalf("%s: measurement = %+v, want not current with a reason", name, ev)
		}
	}

	// The policy check reads the same book.
	book := PolicyCheckBookFrom(&rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 240000, AsOf: mandateTestNow}, protectionTestBook(futureTestHedge(20)))
	if book == nil || len(book.UnmeasuredLines) != 0 {
		t.Fatalf("policy check book with a held hedge = %+v, want it measured", book)
	}
	var line PolicyCheckPosition
	for _, p := range book.Positions {
		if p.ConID == protectionTestCalls {
			line = p
		}
	}
	if lowers, known, why := book.exitLowersAbsoluteDelta(line, 40, pol); !known || !lowers {
		t.Fatalf("policy check beside a held hedge: lowers %v known %v %q", lowers, known, why)
	}
	book = PolicyCheckBookFrom(&rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 240000, AsOf: mandateTestNow}, protectionTestBook(futureTestHedge(70)))
	if lowers, known, _ := book.exitLowersAbsoluteDelta(line, 40, pol); !known || lowers {
		t.Fatalf("policy check in a book the hedge has tipped short: lowers %v known %v, want known and not lowering", lowers, known)
	}
}
