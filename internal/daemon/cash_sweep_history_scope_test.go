package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepHistoryWithoutProposalsCannotCrossAccountScope(t *testing.T) {
	for _, current := range []brokerStateScope{{Account: "UTESTB", Mode: "paper"}, {Account: "UTESTA", Mode: "live"}, {}} {
		t.Run(current.Account+current.Mode, func(t *testing.T) {
			e := &proposalEngine{scope: func() brokerStateScope { return current }, snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AccountID: "UTESTA", AccountMode: "paper", CashSweep: &rpc.TradeProposalCashSweepStatus{Mode: "shadow", DecisionTrace: []rpc.CashSweepDecisionTrace{{At: time.Now().UTC(), Currencies: []rpc.CashSweepDecisionCurrency{{Currency: "USD", Cash: new(1000.)}}}}}}}
			s := &Server{tradeProposals: e}
			got := s.handleTradeProposalsSnapshot(&rpc.Request{})
			if got.CashSweep != nil || len(got.Proposals) != 0 || len(got.Blockers) == 0 {
				t.Fatal("old scoped cash history leaked across account/mode change", got)
			}
			if e.snapshot.CashSweep == nil || len(e.snapshot.CashSweep.DecisionTrace) != 1 {
				t.Fatal("serve guard destroyed retained audit")
			}
		})
	}
}

func TestCashSweepHistoryServeGuardAlsoFencesScopedBudget(t *testing.T) {
	for _, current := range []brokerStateScope{{Account: "UTESTB", Mode: "paper"}, {Account: "UTESTA", Mode: "live"}, {}} {
		t.Run(current.Account+current.Mode, func(t *testing.T) {
			e := &proposalEngine{scope: func() brokerStateScope { return current }, snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AccountID: "UTESTA", AccountMode: "paper", BudgetReduction: &rpc.TradeProposalBudgetStatus{NLVBase: new(100000.), BaseCurrency: "EUR"}}}
			s := &Server{tradeProposals: e}
			got := s.handleTradeProposalsSnapshot(&rpc.Request{})
			if got.BudgetReduction != nil || len(got.Proposals) != 0 || len(got.Blockers) == 0 {
				t.Fatal("old scoped budget leaked across account/mode change")
			}
			if e.snapshot.BudgetReduction == nil || *e.snapshot.BudgetReduction.NLVBase != 100000 {
				t.Fatal("serve guard destroyed retained budget")
			}
		})
	}
}

func TestCashSweepHistoryServeGuardPreservesSameScopeAndUnscopedShell(t *testing.T) {
	scope := brokerStateScope{Account: "UTESTA", Mode: "paper"}
	e := &proposalEngine{scope: func() brokerStateScope { return scope }, snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AccountID: scope.Account, AccountMode: scope.Mode, BudgetReduction: &rpc.TradeProposalBudgetStatus{NLVBase: new(100000.), BaseCurrency: "EUR"}}}
	if got := e.Snapshot(false); got.BudgetReduction == nil || *got.BudgetReduction.NLVBase != 100000 || len(got.Blockers) != 0 {
		t.Fatal("same-scope financial status was hidden")
	}
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Blockers: []rpc.TradingBlocker{{Code: "synthetic_gateway_unavailable"}}}
	e.scope = func() brokerStateScope { return brokerStateScope{} }
	if got := e.Snapshot(false); len(got.Blockers) != 1 || got.Blockers[0].Code != "synthetic_gateway_unavailable" || got.CashSweep != nil || got.BudgetReduction != nil {
		t.Fatal("true unscoped shell was replaced")
	}
}
