//go:build trading

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A pre-authorised mandate cut gives a little on price after the open
// (spec TM-05.18): halfway from the mid toward the bid while the spread is
// within the line's cap; beyond the cap it gives nothing and goes at the
// mid, as before.
func TestMandateReductionGivesHalfTheSpreadWithinTheCap(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		bid, ask, want float64
	}{
		"within the 2% stock cap": {24.9, 25.1, 24.95},
		"beyond the cap":          {24.5, 25.5, 25},
	} {
		rig := newAutomaticTradingRig(t, automaticMandateAuthority)
		rig.server.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
			bid, ask, mid := tc.bid, tc.ask, (tc.bid+tc.ask)/2
			return rpc.OrderQuoteSnapshot{Symbol: contract.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
		}
		broker := &brokerCallLog{}
		broker.install(rig.server)
		prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
		revision := rig.install(prop)
		rig.cycle()
		rig.notice(rig.record(prop.Key, revision))
		rig.advance(31 * time.Minute)
		rig.cycle()
		if broker.count() != 1 {
			t.Fatalf("%s: broker calls %d, record %+v", name, broker.count(), rig.record(prop.Key, revision))
		}
		if got := broker.orders[0].LmtPrice; got != tc.want {
			t.Fatalf("%s: limit %v, want %v", name, got, tc.want)
		}
	}
}

// Any other pre-authorised row keeps the patient mid.
func TestPreAuthorisedStopKeepsThePatientMid(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticMandateAuthority)
	if bound := rig.engine.mandateReductionGive(rig.stopProposal()); bound != nil {
		t.Fatalf("a trailing stop row got a give: %+v", bound)
	}
	row := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	bound := rig.engine.mandateReductionGive(row)
	if bound == nil || bound.Concession != 0.5 || bound.MaxSpreadPctOfMid != 2 || bound.WorstPrice != 18.75 {
		t.Fatalf("mandate row give %+v", bound)
	}
}
