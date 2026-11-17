package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

const hedgeTrimFoldConID = 9951

// hedgeTrimFoldRow is a synthetic unblocked sale of qty of one hedge-listed
// put, held 10 strong, for the named bucket. The risk-reduction row names no
// issuer, so it is rule 12's trim.
func hedgeTrimFoldRow(bucket string, qty int, orderType string) rpc.TradeProposal {
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	return rpc.TradeProposal{
		Key: bucket + ":synthetic-put", Revision: "r1", State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: hedge, SecType: "OPT",
		Action: rpc.OrderActionSell, Quantity: qty, MaxQuantity: 10, PositionQuantity: 10, PositionEffect: rpc.OrderPositionEffectReduce, OrderType: orderType,
		Contract: rpc.ContractParams{ConID: hedgeTrimFoldConID, Symbol: hedge, SecType: "OPT", Right: "P", Expiry: "20270115", Strike: 400, Multiplier: 100},
		Reason:   bucket + " reason",
	}
}

func hedgeTrimFoldLossExit(qty int) rpc.TradeProposal {
	exit := hedgeTrimFoldRow(rpc.TradeProposalBucketOptionLossExit, qty, rpc.OrderTypeLMT)
	exit.OptionExit = &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionLoss, Readiness: "ready"}
	return exit
}

func hedgeTrimFoldUntouched(t *testing.T, what string, rows ...rpc.TradeProposal) {
	t.Helper()
	for _, p := range rows {
		if p.State != rpc.TradeProposalStateGenerated || p.CoveredBy != "" || len(p.Blockers) != 0 || len(p.Covers) != 0 {
			t.Fatalf("%s: %s was merged: %+v", what, p.Key, p)
		}
	}
}

// Owner instruction 2026-11-17 21:22 CET: one put was offered twice, the
// budget governor and the protection trim each asking to sell the same seven
// contracts. A row that asks for no more than the trim folds into it: one
// card, both reasons, the larger size and never the sum.
func TestHedgeTrimFoldsABudgetRowOfTheSameSize(t *testing.T) {
	for _, order := range [][]string{
		{rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketRiskReduction},
		{rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketBudgetReduction},
	} {
		rows := []rpc.TradeProposal{hedgeTrimFoldRow(order[0], 7, rpc.OrderTypeLMT), hedgeTrimFoldRow(order[1], 7, rpc.OrderTypeLMT)}
		mergeSameContractProposals(rows, nil)
		by := rowsByBucket(t, rows, rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketRiskReduction)
		trim, budget := by[rpc.TradeProposalBucketRiskReduction], by[rpc.TradeProposalBucketBudgetReduction]
		got := approvableFor(rows, hedgeTrimFoldConID)
		if len(got) != 1 || got[0].Key != trim.Key || got[0].Quantity != 7 {
			t.Fatalf("approvable rows for one put = %+v, want the trim's sale of 7 alone", got)
		}
		if !hedgeTrimProposal(got[0]) || len(trim.Covers) != 1 ||
			trim.Covers[0] != (rpc.TradeProposalCoverage{Bucket: budget.Bucket, Key: budget.Key, Quantity: 7, OrderType: rpc.OrderTypeLMT, Reason: budget.Reason}) {
			t.Fatalf("the trim does not carry the governor's reason: %+v", trim)
		}
		if budget.State != rpc.TradeProposalStateBlocked || budget.CoveredBy != trim.Key || !hasTradingBlocker(budget.Blockers, coveredByProposalCode) ||
			!strings.Contains(budget.Blockers[0].Message, "the protection trim asks to sell 7 now") {
			t.Fatalf("the governor's row is not covered by the trim: %+v", budget)
		}
	}
}

// A row asking for less than the trim folds the same way, at the trim's size.
// A loss exit of the trim's own size yields to it, although an exit leads
// every other tie: the surviving card keeps the trim's identity.
func TestHedgeTrimFoldsSmallerAndEqualRows(t *testing.T) {
	rows := []rpc.TradeProposal{hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 3, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	if got := approvableFor(rows, hedgeTrimFoldConID); len(got) != 1 || !hedgeTrimProposal(got[0]) || got[0].Quantity != 7 || len(got[0].Covers) != 1 || got[0].Covers[0].Quantity != 3 {
		t.Fatalf("approvable = %+v, want the trim's 7 carrying the governor's 3", got)
	}
	if rows[0].CoveredBy != rows[1].Key {
		t.Fatalf("the smaller governor row stayed a second card: %+v", rows[0])
	}

	rows = []rpc.TradeProposal{hedgeTrimFoldLossExit(7), hedgeTrimFoldRow(rpc.TradeProposalBucketThetaHygiene, 2, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	exit, theta, trim := rows[0], rows[1], rows[2]
	if got := approvableFor(rows, hedgeTrimFoldConID); len(got) != 1 || got[0].Key != trim.Key || len(trim.Covers) != 2 {
		t.Fatalf("approvable = %+v, want the trim alone with two reasons", got)
	}
	if exit.CoveredBy != trim.Key || exit.OptionExit.Readiness != "blocked" || theta.CoveredBy != trim.Key {
		t.Fatalf("exit = %+v, theta = %+v", exit, theta)
	}
}

// Against a row that asks for more than its own order the trim stays its own
// card (owner decision 2026-11-14): the larger row would leave the owner one
// choice, selling more protection than the band asks. The other rows merge as
// if the trim were not there.
func TestHedgeTrimStaysItsOwnCardBesideALargerRow(t *testing.T) {
	rows := []rpc.TradeProposal{hedgeTrimFoldLossExit(10), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	hedgeTrimFoldUntouched(t, "a whole-line exit beside the trim", rows...)

	// One row larger, one no larger: the trim stays out altogether, and the
	// exit covers the governor's row as before. Two cards, not three.
	rows = []rpc.TradeProposal{hedgeTrimFoldLossExit(10), hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 7, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	hedgeTrimFoldUntouched(t, "the trim beside a larger exit and an equal governor row", rows[2])
	if rows[1].CoveredBy != rows[0].Key || len(approvableFor(rows, hedgeTrimFoldConID)) != 2 {
		t.Fatalf("rows = %+v", rows)
	}

	// A governor row one contract larger than the trim's order is larger.
	rows = []rpc.TradeProposal{hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 8, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	hedgeTrimFoldUntouched(t, "a larger governor row beside the trim", rows...)
}

// The fold is for sales that ask the same thing now. A trailing stop alone
// beside the trim stays as it was, both on offer; a stop beside a sale the
// trim folds is covered as that sale already covered it.
func TestHedgeTrimFoldLeavesAStopAlone(t *testing.T) {
	trail := func() rpc.TradeProposal {
		p := hedgeTrimFoldRow(rpc.TradeProposalBucketTrailingStop, 10, rpc.OrderTypeTRAILLIMIT)
		p.OptionExit = &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionProfitTrail, Readiness: "ready"}
		return p
	}
	rows := []rpc.TradeProposal{trail(), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	hedgeTrimFoldUntouched(t, "a stop alone beside the trim", rows...)

	rows = []rpc.TradeProposal{trail(), hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 7, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	if got := approvableFor(rows, hedgeTrimFoldConID); len(got) != 1 || got[0].Key != rows[2].Key || rows[0].CoveredBy != rows[2].Key || rows[1].CoveredBy != rows[2].Key {
		t.Fatalf("rows = %+v", rows)
	}
}

// The fold never touches a row Canary places itself: beside one, the trim
// stays out of the merge exactly as before, so the fold can neither cover an
// automatic order nor lend the trim to one. A held, blocked or shadow row is
// not folded either.
func TestHedgeTrimFoldNeverMeetsAnAutomaticRow(t *testing.T) {
	automatic := func(p rpc.TradeProposal) sameContractChannel {
		if p.Bucket == rpc.TradeProposalBucketBudgetReduction {
			return sameContractAutomatic
		}
		return sameContractManual
	}
	for _, qty := range []int{3, 7, 10} {
		rows := []rpc.TradeProposal{hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, qty, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
		mergeSameContractProposals(rows, automatic)
		hedgeTrimFoldUntouched(t, "the trim beside an automatic governor row", rows...)
	}
	excluded := func(p rpc.TradeProposal) sameContractChannel {
		if p.Bucket == rpc.TradeProposalBucketBudgetReduction {
			return sameContractExcluded
		}
		return sameContractManual
	}
	rows := []rpc.TradeProposal{hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 7, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, excluded)
	hedgeTrimFoldUntouched(t, "the trim beside an ended pre-authorised row", rows...)

	shadow := hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 7, rpc.OrderTypeLMT)
	shadow.Shadow = true
	rows = []rpc.TradeProposal{shadow, hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	hedgeTrimFoldUntouched(t, "the trim beside a shadow row", rows...)
}

// With the engine's own channels and the governor pre-authorised: a sale of a
// hedge-listed put is never automatic, so the governor's row waits for the
// owner, folds under the trim and is not stranded, and the card that is left
// is outside the pre-authorised vocabulary. On another underlying the
// governor's row stays Canary's own order.
func TestHedgeTrimFoldUnderAPreAuthorisedGovernor(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction"]`)
	rig.server.coreStore = rig.core
	e := rig.engine
	if hedge := e.rulebookPolicy().HedgeSymbols[0]; hedge != risk.DefaultRulebookPolicy().HedgeSymbols[0] {
		t.Fatalf("the rig's hedge symbol %q is not the fixture's", hedge)
	}
	rows := []rpc.TradeProposal{hedgeTrimFoldRow(rpc.TradeProposalBucketBudgetReduction, 7, rpc.OrderTypeLMT), hedgeTrimFoldRow(rpc.TradeProposalBucketRiskReduction, 7, rpc.OrderTypeLMT)}
	e.mergeSameContract(rows)
	got := approvableFor(rows, hedgeTrimFoldConID)
	if len(got) != 1 || !hedgeTrimProposal(got[0]) || len(got[0].Covers) != 1 || got[0].Covers[0].Automatic || rows[0].CoveredBy != rows[1].Key {
		t.Fatalf("rows = %+v", rows)
	}
	if bucket := e.automaticBucket(got[0]); bucket != "" {
		t.Fatalf("the folded card maps to the pre-authorised bucket %q", bucket)
	}
	e.mu.Lock()
	merged := len(e.mergedAutomatic)
	e.mu.Unlock()
	if merged != 0 {
		t.Fatalf("the merge counted %d row(s) of a hedge-listed put as automatic", merged)
	}

	single := sameContractRow(rpc.TradeProposalBucketBudgetReduction, 7, 7, rpc.OrderTypeLMT)
	single.Revision = "r1"
	if ch := e.sameContractChannels()(single); ch != sameContractAutomatic {
		t.Fatalf("a single-name governor row has channel %d, want automatic: the rig does not exercise the pre-authorised path", ch)
	}
}
