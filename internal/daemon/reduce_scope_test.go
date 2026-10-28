package daemon

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The owner's book on 2026-10-22: longs, and long index puts large enough to
// make it net short. The net sweep followed the net direction and sold the
// puts; the owner expected the longs cut and the puts kept.
func reduceScopeBook() *rpc.PositionsResult {
	return reduceTestPortfolio([]rpc.PositionView{
		{Symbol: "LONG", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 100},
		{Symbol: "TECH", SecType: "STOCK", ConID: 2, Currency: "USD", Quantity: 7, Mark: 400},
		{Symbol: "SHRT", SecType: "STOCK", ConID: 3, Currency: "USD", Quantity: -50, Mark: 80},
	}, []rpc.PositionView{
		{Symbol: "SPY", SecType: "OPTION", ConID: 9, Currency: "USD", Quantity: 4, Right: "P", Multiplier: 100, Delta: new(-0.5), Underlying: new(600.0)},
	})
}

func reduceKeptReason(kept []rpc.TradeProposalReduceKept, conID int) string {
	for _, k := range kept {
		if k.ConID == conID {
			return k.Reason
		}
	}
	return ""
}

func TestReduceNetSweepNeverSellsALockedHedge(t *testing.T) {
	pos := reduceScopeBook()
	cands, net, _, _, _ := reduceSweepCandidates(pos, pos, 50)
	if _, ok := reduceFindCandidate(cands, 9); !ok || net >= 0 {
		t.Fatalf("precondition: the unlocked net sweep of a net-short book sells the puts; net=%v cands=%+v", net, cands)
	}
	locks := rpc.HoldingLocks{Locks: []rpc.HoldingLock{{ConID: 9, Symbol: "SPY", Role: rpc.HoldingLockRoleHedge}}}
	book, kept := reduceUnlockedBook(pos, locks)
	if len(kept) != 1 || kept[0].ConID != 9 || kept[0].Reason != reduceKeptLocked || kept[0].Role != rpc.HoldingLockRoleHedge {
		t.Fatalf("kept = %+v, want the locked puts disclosed", kept)
	}
	cands, net, _, _, _ = reduceSweepCandidates(book, reduceExposureBook(pos, locks), 50)
	if _, ok := reduceFindCandidate(cands, 9); ok {
		t.Fatal("a locked hedge was selected by the net sweep")
	}
	if net <= 0 {
		t.Fatalf("net of the unlocked book = %v, want long", net)
	}
	if _, ok := reduceFindCandidate(cands, 1); !ok {
		t.Fatalf("the unlocked book's longs are not trimmed: %+v", cands)
	}
	if len(pos.Options) != 1 {
		t.Fatal("taking locked lines out must not change the caller's positions")
	}
}

// The owner's book on 2026-10-28: a locked core long, an unlocked long and
// unlocked long puts that protect the core. Read without the core, the book
// was net short, so the net sweep sold the puts and left the core unhedged.
// A locked core line counts toward the direction and size, though it is
// never traded (owner, 2026-10-28 12:04 CET).
func TestReduceNetSweepCountsALockedCoreLine(t *testing.T) {
	locks := rpc.HoldingLocks{Locks: []rpc.HoldingLock{{Symbol: "CORE", Role: rpc.HoldingLockRoleCore}}}
	// Core 30,000 + long 5,000 - puts 20,000 is net long 15,000: the long is
	// cut and the protection kept. Core 10,000 leaves the book net short
	// 5,000: the puts are trimmed by half of that (5 contracts), not by half
	// of the unlocked book's 15,000 (15).
	for _, c := range []struct {
		core, net  float64
		sells, qty int
	}{{300, 15000, 2, 50}, {100, -5000, 9, 5}} {
		pos := reduceTestPortfolio([]rpc.PositionView{
			{Symbol: "CORE", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: c.core, Mark: 100},
			{Symbol: "LONG", SecType: "STOCK", ConID: 2, Currency: "USD", Quantity: 50, Mark: 100},
		}, []rpc.PositionView{
			{Symbol: "IDX", SecType: "OPTION", ConID: 9, Currency: "USD", Quantity: 40, Right: "P", Multiplier: 100, Delta: new(-0.05), Underlying: new(100.0)},
		})
		unlocked, _ := reduceUnlockedBook(pos, locks)
		cands, net, _, target, blockers := reduceSweepCandidates(unlocked, reduceExposureBook(pos, locks), 50)
		if len(blockers) > 0 {
			t.Fatalf("core %v: blockers %+v", c.core, blockers)
		}
		if !approxEqual(net, c.net, 1e-6) || !approxEqual(target, math.Abs(c.net)/2, 1e-6) {
			t.Fatalf("core %v: net %v target %v, want net %v and half of it", c.core, net, target, c.net)
		}
		if len(cands) != 1 || cands[0].row.ConID != c.sells || cands[0].qty != c.qty {
			t.Fatalf("core %v: candidates %+v, want %d of contract %d only", c.core, cands, c.qty, c.sells)
		}
	}
}

func TestReduceLongsSweepCutsEachLongAndKeepsShorts(t *testing.T) {
	pos := reduceScopeBook()
	cands, kept := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 25}, reduceScopeFacts{})
	long, ok := reduceFindCandidate(cands, 1)
	if !ok || long.qty != 25 {
		t.Fatalf("LONG = %+v, want 25 of 100", long)
	}
	tech, ok := reduceFindCandidate(cands, 2)
	if !ok || tech.qty != 1 {
		t.Fatalf("TECH = %+v, want floor(25%% of 7) = 1", tech)
	}
	if reduceKeptReason(kept, 3) != reduceKeptShort || reduceKeptReason(kept, 9) != reduceKeptShort {
		t.Fatalf("kept = %+v, want the short stock and the long puts kept as short-delta", kept)
	}
	all, _ := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 100}, reduceScopeFacts{})
	if c, _ := reduceFindCandidate(all, 2); c.qty != 7 {
		t.Fatalf("100%% of TECH = %d, want the whole line", c.qty)
	}
	small, _ := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 10}, reduceScopeFacts{})
	if c, _ := reduceFindCandidate(small, 2); scopeBlockerCode(c.blockers) != "percent_too_small" {
		t.Fatalf("10%% of 7 = %+v, want percent_too_small disclosed, never forced up", c)
	}
}

func TestReduceAllSweepBuysBackShortsAndSellsUnlockedPuts(t *testing.T) {
	cands, kept := reduceScopedCandidates(reduceScopeBook(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeAll, Percent: 100}, reduceScopeFacts{})
	if len(kept) != 0 || len(cands) != 4 {
		t.Fatalf("all/100 = %d candidates, kept %+v; want every unlocked line", len(cands), kept)
	}
	if c, _ := reduceFindCandidate(cands, 3); c.qty != 50 || c.row.Quantity >= 0 {
		t.Fatalf("SHRT = %+v, want the whole short bought back", c)
	}
}

func TestReduceScopedSweepBySectorAndBeta(t *testing.T) {
	pos := reduceScopeBook()
	facts := reduceScopeFacts{
		betas:   map[string]float64{"LONG": 0.8, "TECH": 1.6, "SHRT": 2.1},
		sectors: map[string]string{"LONG": "Financials", "TECH": "Information Technology", "SHRT": "Consumer Discretionary"},
	}
	bySector, kept := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50, Sectors: []string{"information technology"}}, facts)
	if len(bySector) != 1 || bySector[0].row.ConID != 2 || bySector[0].sector != "Information Technology" || reduceKeptReason(kept, 1) != reduceKeptOutOfScope {
		t.Fatalf("sector sweep = %+v kept %+v, want TECH alone", bySector, kept)
	}
	byBeta, kept := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50, MinBeta: 1.3}, facts)
	if len(byBeta) != 1 || byBeta[0].row.ConID != 2 || byBeta[0].beta == nil || *byBeta[0].beta != 1.6 || reduceKeptReason(kept, 1) != reduceKeptOutOfScope {
		t.Fatalf("beta sweep = %+v kept %+v, want TECH alone with its beta", byBeta, kept)
	}
	delete(facts.betas, "TECH")
	unknown, _ := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50, MinBeta: 1.3}, facts)
	if c, ok := reduceFindCandidate(unknown, 2); !ok || scopeBlockerCode(c.blockers) != "beta_unavailable" {
		t.Fatalf("TECH without a beta = %+v, want beta_unavailable disclosed, never silently in or out", c)
	}
}

func TestReducePlanLegIsSizedFromTheMarkAndNeverSubmittable(t *testing.T) {
	row := rpc.PositionView{Symbol: "LONG", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 100, MarketValue: 10000, MarketValueBase: new(9000.0)}
	leg := reducePlanLeg(reduceSweepCandidate{row: row, qty: 25})
	if leg.Action != rpc.OrderActionSell || leg.ReduceQuantity != 25 || leg.Notional != 2500 || leg.NotionalBase == nil || *leg.NotionalBase != 2250 || leg.SubmitEligible || leg.PreviewToken != "" {
		t.Fatalf("plan leg = %+v", leg)
	}
	changes := reduceTradeChanges([]rpc.TradeProposalReduceLeg{leg, {ConID: 3, Action: rpc.OrderActionBuy, ReduceQuantity: 50}, {ConID: 4, ReduceQuantity: 5, Blockers: []rpc.TradingBlocker{{Code: "x"}}}})
	if changes[1] != -25 || changes[3] != 50 || len(changes) != 2 {
		t.Fatalf("changes = %+v, want a sale negative, a buy-back positive, a blocked leg left out", changes)
	}
}

func replaceHoldingLocks(t *testing.T, s *Server, locks []rpc.HoldingLock, rev int64, id string) (*rpc.HoldingLocks, error) {
	t.Helper()
	raw, err := json.Marshal(rpc.HoldingLocksReplaceRequest{Locks: locks, ExpectedRevision: rev, RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	return s.handleHoldingLocksReplace(t.Context(), &rpc.Request{Method: rpc.MethodHoldingLocksReplace, Params: raw})
}

func TestHoldingLocksPersistReplayAndFence(t *testing.T) {
	s, _, _ := watchlistServer(t)
	empty, err := s.handleHoldingLocksList(t.Context())
	if err != nil || empty.Revision != 0 || len(empty.Locks) != 0 || empty.Locks == nil {
		t.Fatal(empty, err)
	}
	spy := rpc.HoldingLock{ConID: 9, Symbol: " spy ", Role: "Hedge"}
	saved, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{spy, {ConID: 12, Symbol: "GLD", Role: "core"}}, 0, "lock-1")
	if err != nil || saved.Revision != 1 || len(saved.Locks) != 2 || saved.Locks[0].Symbol != "GLD" || saved.Locks[1].ConID != 9 || saved.Locks[1].Symbol != "SPY" || saved.Locks[1].Role != "hedge" {
		t.Fatalf("saved = %+v err %v, want normalized and ordered by name", saved, err)
	}
	if _, ok := saved.Match("SPY", 0); !ok {
		t.Fatal("a lock covers its name")
	}
	if _, ok := saved.Match("OTHER", 9); !ok {
		t.Fatal("a lock still covers the contract it was placed from")
	}
	if _, ok := saved.Match("OTHER", 1); ok {
		t.Fatal("an unlocked name and contract must not match")
	}
	replay, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{ConID: 12, Symbol: "GLD", Role: "core"}, spy}, 0, "lock-1")
	if err != nil || !replay.Replay || replay.Revision != 1 {
		t.Fatalf("replay = %+v err %v", replay, err)
	}
	var rpcErr *rpc.Error
	if _, err = replaceHoldingLocks(t, s, nil, 0, "lock-2"); err == nil {
		t.Fatal("a nil list must be refused")
	}
	if _, err = replaceHoldingLocks(t, s, []rpc.HoldingLock{}, 0, "lock-3"); !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeHoldingLocksConflict {
		t.Fatalf("stale revision = %v, want a conflict", err)
	}
	if _, err = replaceHoldingLocks(t, s, []rpc.HoldingLock{{ConID: 9, Symbol: "SPY", Role: "keep"}}, 1, "lock-4"); err == nil {
		t.Fatal("an unknown role must be refused")
	}
	folded, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{ConID: 10, Symbol: "SPY", Role: "core"}, spy}, 1, "lock-5")
	if err != nil || folded.Revision != 2 || len(folded.Locks) != 1 || folded.Locks[0].Role != rpc.HoldingLockRoleHedge || folded.Locks[0].ConID != 0 {
		t.Fatalf("folded = %+v err %v, want one SPY lock as hedge spanning contracts", folded, err)
	}
	cleared, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{}, 2, "lock-6")
	if err != nil || cleared.Revision != 3 || len(cleared.Locks) != 0 {
		t.Fatalf("cleared = %+v err %v", cleared, err)
	}
}

func TestReducePortfolioRefusesWhenLocksAreUnreadable(t *testing.T) {
	s := &Server{}
	res := &rpc.TradeProposalReducePortfolioResult{}
	cands, blockers := s.reducePortfolioSelect(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 25}, reduceScopeBook(), res)
	if len(cands) != 0 || scopeBlockerCode(blockers) != "locks_unavailable" {
		t.Fatalf("unreadable keep list = %+v %+v, want the sweep blocked", cands, blockers)
	}
}

func scopeBlockerCode(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return ""
	}
	return blockers[0].Code
}

func TestMandateReductionLeavesALockedLineOut(t *testing.T) {
	pos := reductionRoleBook(100)
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	in := mandateReductionInput{Input: &mandate.Input{Legs: []mandate.Leg{{ID: mandateConLegID(990002), Symbol: "SYNTH", Underlying: "SYNTH", Kind: mandate.KindStock, Quantity: 100, Multiplier: 1}}}}
	candidates, _, _ := (&proposalEngine{}).mandateCandidates(policy, in, pos, optionExitTestTime())
	if len(candidates) != 1 {
		t.Fatalf("precondition: the unlocked stock is a candidate, got %d", len(candidates))
	}
	in.Locked = func(conID int) bool { return conID == 990002 }
	candidates, leftOut, _ := (&proposalEngine{}).mandateCandidates(policy, in, pos, optionExitTestTime())
	if len(candidates) != 0 || len(leftOut) != 1 || !strings.Contains(leftOut[0], "locked") {
		t.Fatalf("locked line: candidates %d, left out %q; want it left out and said why", len(candidates), leftOut)
	}
}

func TestMandateInputWithTradesMovesOnlyTheTradedLines(t *testing.T) {
	in := mandate.Input{Legs: []mandate.Leg{{ID: mandateConLegID(1), Quantity: 100}, {ID: mandateConLegID(1), Quantity: 20}, {ID: mandateConLegID(3), Quantity: -50}, {ID: mandateConLegID(9), Quantity: 4}}}
	out := mandateInputWithTrades(in, map[int]float64{1: -25, 3: 50, 77: -1})
	if out.Legs[0].Quantity != 75 || out.Legs[1].Quantity != 20 || out.Legs[2].Quantity != 0 || out.Legs[3].Quantity != 4 {
		t.Fatalf("legs = %+v", out.Legs)
	}
	if in.Legs[0].Quantity != 100 {
		t.Fatal("the kept input must not change")
	}
	res := &rpc.TradeProposalReducePortfolioResult{}
	(&Server{}).reduceMandateScores(t.Context(), res, nil)
	if res.IndexLossBeforeBase != nil || res.IndexLossUnknown == "" || res.IndexShockPct != mandate.IndexShockPct {
		t.Fatalf("no mandate = %+v, want the loss unknown and said why", res)
	}
}

// A plan says it is one: its legs read planned, never blocked for want of a
// token, and its total is the planned legs' marks (found live 2026-10-22).
func TestReducePlanSaysItIsAPlan(t *testing.T) {
	row := reduceTestStockRow(100)
	row.MarketValue, row.MarketValueBase = 2500, new(2400.0)
	srv, _ := newReducePreviewTestServer(t, row)
	res, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 25, PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.PlanOnly || res.Accepted || res.LegCount != 1 || res.BlockedCount != 0 || res.TotalNotional != 600 || res.Legs[0].PreviewToken != "" || res.Legs[0].SubmitEligible {
		t.Fatalf("plan = %+v, want one planned leg, no token, total 600", res)
	}
}
