package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
)

// strategyLineTestBook holds 3 SYND 100 calls: 1 the bought leg of a bull
// call spread Canary opened (100/110, one unit), 2 held beside it.
func strategyLineTestBook(t *testing.T, longs float64) (*Server, *rpc.PositionsResult) {
	t.Helper()
	row := func(conID int, qty, strike float64) rpc.PositionView {
		return rpc.PositionView{Symbol: "SYND", SecType: "OPT", ConID: conID, Currency: "USD", Exchange: "SMART", Quantity: qty, Multiplier: 100, Expiry: "20270115", Strike: strike, Right: "C", TradingClass: "SYND"}
	}
	open := strategyOpenTestDraft("SYND", 1, 3, addLeg{7101, "C", 100, "20270115", 1}, addLeg{7102, "C", 110, "20270115", -1})
	open.StrategyGroup.StrategyID = positionstrategy.StrategyID("SYND", 7101, 7102)
	srv := &Server{orderJournal: &orderJournalStore{}, strategyLineageLoaded: true,
		strategyLineage: map[string]rpc.StrategyOrderDraft{open.StrategyGroup.StrategyID: *open.StrategyGroup}}
	options := []rpc.PositionView{row(7101, longs, 100), row(7102, -1, 110)}
	groups, issues := positionstrategy.InferStrategies(nil, options)
	groups, issues = srv.reconcileJournaledStrategyGroups(nil, options, groups, issues)
	return srv, &rpc.PositionsResult{Options: options, Strategies: groups, StrategyIssues: issues}
}

// The line of a contract a strategy also holds is what is held outside the
// strategy: 2 of 3 calls. With every call in the spread there is no line,
// and a call the spread's sold leg needs is never a line's.
func TestOptionLineIsWhatIsHeldOutsideTheStrategy(t *testing.T) {
	srv, pos := strategyLineTestBook(t, 3)
	if line, b := strategyFreeLine(pos, pos.Options[0]); line != 2 || b != nil {
		t.Fatalf("3 held, 1 in the spread: line %v blocker %+v", line, b)
	}
	if line := optionLineFromRows(srv, pos.Options, 7101, 3); line != 2 {
		t.Fatalf("from the broker rows: line %v, want 2", line)
	}
	_, pos = strategyLineTestBook(t, 1)
	if line, b := strategyFreeLine(pos, pos.Options[0]); line != 0 || b == nil || !strings.Contains(b.Message, "leg of a held multi-leg position") {
		t.Fatalf("only the spread's call: line %v blocker %+v", line, b)
	}
	// A call outside any strategy is all line.
	free := rpc.PositionView{Symbol: "SYNE", SecType: "OPT", ConID: 7201, Quantity: 4, Multiplier: 100, Right: "C"}
	if line, b := strategyFreeLine(&rpc.PositionsResult{Options: []rpc.PositionView{free}}, free); line != 4 || b != nil {
		t.Fatalf("a call alone: line %v blocker %+v", line, b)
	}
}

// Reduce on the line sells the calls outside the spread and never the
// spread's: a reduction sized past the line is refused.
func TestReduceSellsOnlyTheLineBesideASpread(t *testing.T) {
	_, pos := strategyLineTestBook(t, 3)
	row, blockers := reduceTargetRow(pos, 7101, "")
	if len(blockers) != 0 || row.Quantity != 2 {
		t.Fatalf("reduce target: %+v %+v", row, blockers)
	}
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: rpc.ContractParams{ConID: 7101, Symbol: "SYND", SecType: "OPT"}, Quantity: 2}
	if _, blockers := reduceTokenPositionCheck(pos, draft); len(blockers) != 0 {
		t.Fatalf("selling the 2 beside the spread: %+v", blockers)
	}
	draft.Quantity = 3
	if _, blockers := reduceTokenPositionCheck(pos, draft); len(blockers) == 0 {
		t.Fatal("a reduction into the spread's bought call passed")
	}
}

// An exit plan reads its line again whenever it acts: a closed spread frees
// its call to the line, calls sold by hand leave only what is outside the
// spread, and none left reads as the line gone, never as the spread's call.
func TestExitPlanLineIsReadAgainWhenItActs(t *testing.T) {
	srv, pos := strategyLineTestBook(t, 3)
	rows := pos.Options
	srv.heldOptionRowsForTest = func(string) ([]rpc.PositionView, error) { return rows, nil }
	call := rpc.ContractParams{ConID: 7101, Symbol: "SYND", SecType: "OPT"}
	line := func(held float64) float64 {
		t.Helper()
		v, err := srv.optionLineQuantity(t.Context(), call, held)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := line(3); got != 2 {
		t.Fatalf("3 held, 1 in the spread: %v", got)
	}
	rows = []rpc.PositionView{pos.Options[0]} // the spread closed: the sold call is gone
	rows[0].Quantity = 2
	if got := line(2); got != 2 {
		t.Fatalf("after the spread closed: %v", got)
	}
	rows = []rpc.PositionView{pos.Options[0], pos.Options[1]}
	rows[0].Quantity = 1 // the 2 beside it sold by hand
	if got := line(1); got != 0 {
		t.Fatalf("only the spread's call left: %v", got)
	}
	srv.heldOptionRowsForTest = func(string) ([]rpc.PositionView, error) { return nil, ErrTradingDisabled }
	if _, err := srv.optionLineQuantity(t.Context(), call, 3); err == nil {
		t.Fatal("an unreadable book read as a whole line")
	}
}
