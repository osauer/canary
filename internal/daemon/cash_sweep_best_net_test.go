package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The sweep buys the listed bill that earns most over the cash it replaces;
// the rung only breaks ties, and with no bill above the cash the currency
// holds and lists nothing (owner decisions 2026-10-23 13:16 and 16:30 CEST).
func TestCashSweepBuysTheBestNetBillOrHolds(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	withRate := func(rate float64) cashSweepInput {
		in := cashSweepTestInput(map[string]float64{"USD": 60000})
		row := in.Ledger["USD"]
		row.Interest = &rpc.CashInterestRates{Credit: *cashInterestTestSide(rate)}
		in.Ledger["USD"] = row
		return in
	}
	// Rung 1 targets 28 days, so the 35-day bill is nearest; the 60-day bill
	// is priced at 99.0, a 1% discount over 60 days, and earns most.
	src := usBillSource(cashSweepTestNow())
	src.quotes[7102] = synthLiveQuote(99.0)
	_, got := planAndResolve(t, policy, withRate(0.01), src)
	usd := got["USD"]
	if usd.side != rpc.CashSweepSideInvest || usd.bill == nil || usd.bill.CUSIP != synthCUSIP60 || !strings.Contains(usd.status.Reason, "more than the cash") {
		t.Fatalf("picked %+v (%s), want the best net bill %s", usd.bill, usd.status.Reason, synthCUSIP60)
	}
	// At a 20% cash rate no bill beats the cash: hold, no bill, no row.
	src = usBillSource(cashSweepTestNow())
	_, got = planAndResolve(t, policy, withRate(0.20), src)
	usd = got["USD"]
	if usd.side != "" || usd.bill != nil || usd.status.State != rpc.CashSweepStateHold || !strings.Contains(usd.status.Reason, "earns more than the cash") {
		t.Fatalf("a sweep worse than cash was planned: side %q bill %+v (%s)", usd.side, usd.bill, usd.status.Reason)
	}
	// No cash rate: the estimate is unavailable and the nearest rung stands.
	src = usBillSource(cashSweepTestNow())
	_, got = planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"USD": 60000}), src)
	if usd = got["USD"]; usd.bill == nil || usd.bill.CUSIP != synthCUSIP35 {
		t.Fatalf("without a cash rate the rung order was lost: %+v", usd.bill)
	}
}
