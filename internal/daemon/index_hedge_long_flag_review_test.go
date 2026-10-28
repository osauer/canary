package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestIndexHedgeNotNeededKeepsExternalLongFuture(t *testing.T) {
	short := mandateReductionFuture(750001, -2, 6000)
	long := mandateReductionFuture(750002, 1, 6000)
	long.LocalSymbol = "MESH7"
	long.Expiry = "20270319"
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), short, long)
	result, in, at, fp := s.currentMandateVerdict()
	for i := range in.Legs {
		if in.Legs[i].ID == mandateConLegID(long.ConID) {
			in.Legs[i].Quantity = 120
		}
	}
	v := mandate.Evaluate(*in)
	result.Verdict = &v
	s.mandateVerdicts.set(result, in, at, fp, false, s.captureMandateBookBinding())
	without := *in
	without.Legs = nil
	for _, l := range in.Legs {
		if mandate.IsIndexFuture(l) && l.Quantity < 0 {
			continue
		}
		without.Legs = append(without.Legs, l)
	}
	check := mandate.Evaluate(without)
	if check.Status != mandate.StatusReduce || check.Loss.TotalBase <= check.Loss.RoomBase {
		t.Fatalf("fixture without shorts must remain over room: %+v", check.Loss)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if res.HedgeNotNeeded {
		t.Fatalf("incorrect not-needed flag despite retained long future: loss=%g room=%g message=%q", check.Loss.TotalBase, check.Loss.RoomBase, res.HedgeNotNeededMessage)
	}
}
