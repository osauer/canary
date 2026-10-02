package rpc

import "testing"

func TestCashSweepReserveAndTraceCloneIsolation(t *testing.T) {
	in := &TradeProposalCashSweepStatus{ReserveCushionEUR: new(10000.), Currencies: []TradeProposalCashSweepCurrency{{Currency: "EUR", EffectiveReserve: new(5000.), BufferAllocation: new(2000.), FundingNeed: new(2000.)}}, DecisionTrace: []CashSweepDecisionTrace{{ReserveCushionEUR: new(10000.), Currencies: []CashSweepDecisionCurrency{{Currency: "USD", Cash: new(20000.), EffectiveReserve: new(15000.)}}}}}
	out := CloneCashSweepStatus(in)
	*out.ReserveCushionEUR, *out.Currencies[0].EffectiveReserve, *out.Currencies[0].BufferAllocation, *out.Currencies[0].FundingNeed = -1, -1, -1, -1
	*out.DecisionTrace[0].ReserveCushionEUR, *out.DecisionTrace[0].Currencies[0].Cash, *out.DecisionTrace[0].Currencies[0].EffectiveReserve = -1, -1, -1
	if *in.ReserveCushionEUR != 10000 || *in.Currencies[0].EffectiveReserve != 5000 || *in.Currencies[0].BufferAllocation != 2000 || *in.Currencies[0].FundingNeed != 2000 || *in.DecisionTrace[0].Currencies[0].Cash != 20000 || *in.DecisionTrace[0].Currencies[0].EffectiveReserve != 15000 {
		t.Fatal("reserve/audit pointers aliased")
	}
}
