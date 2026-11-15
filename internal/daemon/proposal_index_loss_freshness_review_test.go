package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestMandateIndexLossInputExpires(t *testing.T) {
	for _, tc := range []struct {
		name      string
		at        time.Time
		available bool
	}{
		{"current", mandateTestNow, true},
		{"age limit", mandateTestNow.Add(-mandateVerdictMaxAge), true},
		{"expired", mandateTestNow.Add(-mandateVerdictMaxAge - time.Nanosecond), false},
		{"missing time", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := indexLossServer(t, mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50)))
			s.now = func() time.Time { return mandateTestNow }
			result, input, _, fp := s.mandateVerdicts.current()
			s.mandateVerdicts.set(result, input, tc.at, fp, false, s.captureMandateBookBinding())
			got, _, why := s.mandateCurrentInput()
			if (got != nil) != tc.available || (!tc.available && why == "") {
				t.Fatalf("available=%t reason=%q; want available=%t and a reason when unavailable", got != nil, why, tc.available)
			}
		})
	}
}

func TestProposalIndexLossCachedFiguresExpireWithoutEvaluation(t *testing.T) {
	s := indexLossServer(t, mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50)))
	now := mandateTestNow
	s.now = func() time.Time { return now }
	snap := rpc.TradeProposalSnapshot{Revision: "same-revision", Proposals: []rpc.TradeProposal{{
		Key: "rr-SPY", Revision: "r1", Bucket: rpc.TradeProposalBucketRiskReduction,
		Action: rpc.OrderActionSell, Quantity: 40, Symbol: "SPY", SecType: "STK", OrderType: rpc.OrderTypeLMT,
		Contract: rpc.ContractParams{ConID: 9101, Symbol: "SPY", SecType: "STK", Currency: "EUR"},
	}}}
	e := &proposalEngine{server: s}
	e.decorateIndexLoss(&snap)
	if snap.IndexLoss == nil || snap.IndexLoss.IndexLossBeforeBase == nil || snap.Proposals[0].IndexLoss == nil || snap.Proposals[0].IndexLoss.IndexLossAfterBase == nil {
		t.Fatal("precondition: fresh evaluation must price the snapshot and row")
	}
	result, input, at, fp := s.mandateVerdicts.current()
	now = now.Add(mandateVerdictMaxAge + time.Nanosecond)
	e.decorateIndexLoss(&snap)
	for _, figure := range []*rpc.IndexLoss{snap.IndexLoss, snap.Proposals[0].IndexLoss} {
		if figure == nil || figure.IndexLossBeforeBase != nil || figure.IndexLossAfterBase != nil || figure.IndexLossUnknown == "" {
			t.Fatalf("expired cached figure = %+v; want no numbers and an explanation", figure)
		}
	}
	keptResult, keptInput, keptAt, keptFP := s.mandateVerdicts.current()
	if keptResult != result || keptInput != input || keptAt != at || keptFP != fp {
		t.Fatal("serving an expired figure must not replace the kept evaluation")
	}
}
