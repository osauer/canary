package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Desk says "roll due" from ten days before a held contract's expiry
// (owner decision 2026-10-28 20:07 CET). The test clock is 2026-10-14.
func TestIndexHedgeRollDueFromTenDaysBeforeExpiry(t *testing.T) {
	for _, tc := range []struct {
		expiry string
		due    bool
	}{
		{"20261218", false},
		{"20261025", false},
		{"20261024", true},
		{"20261020", true},
	} {
		future := mandateReductionFuture(hedgeTestConID, -2, 6000)
		future.Expiry = tc.expiry
		s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), future)
		res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
		if res.RollDue != tc.due || tc.due != (res.RollDueMessage != "") {
			t.Fatalf("expiry %s: roll_due %v %q, want %v", tc.expiry, res.RollDue, res.RollDueMessage, tc.due)
		}
		if tc.due && !strings.Contains(res.RollDueMessage, "MESZ6 expires "+tc.expiry[:4]+"-"+tc.expiry[4:6]+"-"+tc.expiry[6:]) {
			t.Fatalf("expiry %s: message %q names neither the contract nor its date", tc.expiry, res.RollDueMessage)
		}
	}
	// No hedge held, no roll.
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	if res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true}); res.RollDue || res.HedgeNotNeeded {
		t.Fatalf("no hedge held: %+v", res)
	}
}

// Desk says "hedge no longer needed" while a hedge is held and the book
// without it reads within the mandate; a book over its loss room without
// the hedge does not.
func TestIndexHedgeNotNeededWhenTheBookIsInsideTheLimit(t *testing.T) {
	book := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(hedgeTestConID, -2, 6000))
	for _, tc := range []struct {
		ratio  float64
		needed bool
	}{
		{0.5, false},
		{1.2, true},
	} {
		s := hedgeOrderServer(t, 25, book.Stocks...)
		keepMandateEvaluationAt(t, s, book, tc.ratio)
		res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
		if res.HedgeNotNeeded == tc.needed || (res.HedgeNotNeeded && !strings.Contains(res.HedgeNotNeededMessage, "without the hedge reads within the mandate")) {
			t.Fatalf("ratio %g: hedge_not_needed %v %q, want %v", tc.ratio, res.HedgeNotNeeded, res.HedgeNotNeededMessage, !tc.needed)
		}
	}
}
