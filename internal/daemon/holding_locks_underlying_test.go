package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

// lockNameBook holds a stock with calls written on it, a second name, and
// nothing of a third name a lock still names (the live list on 2026-10-27
// kept a contract a roll had left behind).
func lockNameBook(callConID int) *rpc.PositionsResult {
	return reduceTestPortfolio([]rpc.PositionView{
		{Symbol: "SYNL", SecType: "STOCK", ConID: 801, Currency: "USD", Quantity: 100, Mark: 50},
		{Symbol: "SYNO", SecType: "STOCK", ConID: 803, Currency: "USD", Quantity: 40, Mark: 20},
	}, []rpc.PositionView{
		{Symbol: "SYNL", SecType: "OPTION", ConID: callConID, Currency: "USD", Quantity: 2, Right: "C", Strike: 55, Expiry: "20261218", Multiplier: 100, Mark: 3, Delta: new(0.4), Underlying: new(50.0)},
	})
}

// A lock placed from the stock's contract keeps the name: the reduce plan
// sells neither the shares nor the calls, before and after the calls roll
// into a new contract, and a lock on a name not held keeps nothing and is
// served as not held (owner, 2026-10-28 06:41 CET).
func TestHoldingLockCoversEveryContractOfTheName(t *testing.T) {
	locks := rpc.HoldingLocks{Locks: []rpc.HoldingLock{
		{ConID: 801, Symbol: "SYNL", Role: rpc.HoldingLockRoleCore},
		{ConID: 899, Symbol: "SYNR", Role: rpc.HoldingLockRoleHedge},
	}}
	for _, callConID := range []int{802, 812} { // 812: the calls after a roll
		pos := lockNameBook(callConID)
		book, kept := reduceUnlockedBook(pos, locks)
		cands, _ := reduceScopedCandidates(book, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeAll, Percent: 50}, reduceScopeFacts{})
		for _, id := range []int{801, callConID} {
			if _, ok := reduceFindCandidate(cands, id); ok {
				t.Fatalf("calls %d: the plan sells locked contract %d", callConID, id)
			}
			if reduceKeptReason(kept, id) != reduceKeptLocked {
				t.Fatalf("calls %d: contract %d is not disclosed as locked: %+v", callConID, id, kept)
			}
		}
		if _, ok := reduceFindCandidate(cands, 803); !ok {
			t.Fatalf("calls %d: the unlocked name is not cut: %+v", callConID, cands)
		}
		served := locks
		served.Locks = slices.Clone(locks.Locks)
		served.Decorate(pos)
		if got := served.Locks[0]; got.Held == nil || !*got.Held || !slices.Equal(got.Contracts, []int{801, callConID}) {
			t.Fatalf("calls %d: SYNL served as %+v, want both held contracts", callConID, got)
		}
		if got := served.Locks[1]; got.Held == nil || *got.Held || len(got.Contracts) != 0 {
			t.Fatalf("calls %d: a lock on a name not held served as %+v, want held=false", callConID, got)
		}
	}
	if served := (rpc.HoldingLocks{Locks: slices.Clone(locks.Locks)}); true {
		served.Decorate(nil)
		if served.Locks[0].Held != nil {
			t.Fatal("without a book the lock's coverage is unknown, not false")
		}
	}
}

// The mandate's forced reduction and the premium budget sweep honour the
// resolved set: a lock naming the underlying keeps every option on it.
func TestHoldingLockByNameHoldsTheAutomaticSweeps(t *testing.T) {
	// Mandate: the stock is locked by name alone.
	pos := reductionRoleBook(100)
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	in := mandateReductionInput{Input: &mandate.Input{Legs: []mandate.Leg{{ID: mandateConLegID(990002), Symbol: "SYNTH", Underlying: "SYNTH", Kind: mandate.KindStock, Quantity: 100, Multiplier: 1}}}}
	in.Locked = rpc.HoldingLocks{Locks: []rpc.HoldingLock{{Symbol: "SYNTH", Role: rpc.HoldingLockRoleCore}}}.Resolve(pos).Locked
	candidates, leftOut, _ := (&proposalEngine{}).mandateCandidates(policy, in, pos, optionExitTestTime())
	if len(candidates) != 0 || len(leftOut) != 1 || !strings.Contains(leftOut[0], "locked") {
		t.Fatalf("mandate: candidates %d, left out %q; want the name kept", len(candidates), leftOut)
	}

	// Budget: an option line is kept by a lock on its underlying placed
	// from no contract at all.
	s, _, _ := watchlistServer(t)
	e := &proposalEngine{server: s}
	book := budgetTestBook()
	run := func() map[int]rpc.TradeProposal {
		rows, _ := e.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, book, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
		return budgetRowsByConID(rows)
	}
	policy = budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
	if _, ok := run()[602]; !ok {
		t.Fatal("precondition: the unlocked option line is selected")
	}
	if _, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{Symbol: "BBB", Role: rpc.HoldingLockRoleCore}}, 0, "budget-name"); err != nil {
		t.Fatal(err)
	}
	if _, ok := run()[602]; ok {
		t.Fatal("the budget sweep sold an option on a locked name")
	}
	blockers := e.budgetHoldingLockBlockers(t.Context(), rpc.TradeProposal{Bucket: rpc.TradeProposalBucketBudgetReduction, Contract: rpc.ContractParams{ConID: 612, Symbol: "BBB"}})
	if len(blockers) != 1 || blockers[0].Code != "holding_locked" {
		t.Fatalf("a rolled contract of a locked name = %+v, want holding_locked", blockers)
	}
}

// A list saved before 2026-10-28 held one entry per contract, ordered by
// contract, with the shares and the calls of a name under two roles. It
// still reads, folded to one lock per name; it is never unreadable.
func TestHoldingLocksLegacyListFoldsByName(t *testing.T) {
	s, core, _ := watchlistServer(t)
	legacy := `{"version":1,"locks":[{"con_id":801,"symbol":"SYNL","role":"core"},{"con_id":802,"symbol":"SYNL","role":"hedge"},{"con_id":899,"symbol":"SYNR","role":"core"}]}`
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: []byte(legacy)}); err != nil {
		t.Fatal(err)
	}
	locks, err := s.holdingLocks(t.Context())
	if err != nil {
		t.Fatalf("legacy list unreadable: %v", err)
	}
	if len(locks.Locks) != 2 || locks.Locks[0].Symbol != "SYNL" || locks.Locks[0].Role != rpc.HoldingLockRoleHedge || locks.Locks[0].ConID != 0 || locks.Locks[1].ConID != 899 {
		t.Fatalf("legacy list read as %+v, want SYNL folded and SYNR kept", locks.Locks)
	}
	resolved := locks.Resolve(lockNameBook(812))
	for _, id := range []int{801, 812} {
		if !resolved.Locked(id) {
			t.Fatalf("legacy lock does not cover held contract %d", id)
		}
	}
	if resolved.Locked(803) {
		t.Fatal("an unlocked name resolved as locked")
	}
	// Saving the folded list unchanged is no change; saving a change writes
	// the folded form.
	same, err := replaceHoldingLocks(t, s, locks.Locks, locks.Revision, "fold-same")
	if err != nil || same.Revision != locks.Revision || len(same.Locks) != 2 {
		t.Fatalf("saving the folded list unchanged = %+v err %v", same, err)
	}
	next, err := replaceHoldingLocks(t, s, locks.Locks[:1], locks.Revision, "fold-drop")
	if err != nil || next.Revision != locks.Revision+1 || len(next.Locks) != 1 || next.Locks[0].Symbol != "SYNL" {
		t.Fatalf("saving a change = %+v err %v", next, err)
	}
	doc, found, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindHoldingLocks)
	if err != nil || !found || strings.Contains(string(doc.JSON), "801") {
		t.Fatalf("stored list after a change = %s (found %v err %v), want the folded form", doc.JSON, found, err)
	}
}
