//go:build trading

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
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

// An option cut whose spread is wider than its cap goes at the bid when the
// give is at most [mandate.reduction] cheap_leg_give_pct_of_loss of the loss
// the cut removes (owner decision 2026-11-03 13:29 CET, spec TM-05.18 C4);
// a give above it, or the bound unwritten, keeps the mid.
func TestMandateReductionCheapLegGoesAtTheBid(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lossCut float64
		written bool
		want    float64
	}{
		// give: (0.50 - 0.30) × 5 × 100 × 0.9 = 90 in the base currency.
		"give 1.8% of the loss removed": {5000, true, 0.30},
		"give 3.6% of the loss removed": {2500, true, 0.50},
		"bound unwritten":               {5000, false, 0.50},
	} {
		rig := newAutomaticTradingRig(t, automaticMandateAuthority)
		c := &risk.Constitution{Mandate: &risk.ConstitutionMandate{}}
		if rig.server.riskPolicies != nil && rig.server.riskPolicies.active != nil {
			copied := *rig.server.riskPolicies.active
			c = &copied
			if c.Mandate == nil {
				c.Mandate = &risk.ConstitutionMandate{}
			} else {
				m := *c.Mandate
				c.Mandate = &m
			}
		}
		if tc.written {
			c.Mandate.Reduction = &risk.ConstitutionMandateReduction{CheapLegGivePctOfLoss: new(2.0)}
		}
		rig.server.riskPolicies = &riskPolicyManager{active: c}
		rig.server.fillQuotes = &fillQuoteRuntime{fx: func(context.Context, string) (string, *float64, string, string) {
			return "EUR", new(0.9), "test", ""
		}}
		rig.server.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
			bid, ask, mid := 0.30, 0.70, 0.50
			return rpc.OrderQuoteSnapshot{Symbol: contract.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
		}
		rig.server.orderPreviewPositionImpact = fixedPreviewPosition(5, 0, rpc.OrderPositionEffectClose)
		broker := &brokerCallLog{}
		broker.install(rig.server)
		row := automaticTestStockRow()
		row.ConID, row.SecType, row.Quantity, row.Mark, row.Bid, row.Ask, row.Multiplier = 201, "OPT", 5, 0.5, new(0.30), new(0.70), 100
		prop := rig.mandateProposal(row, 5, 1, 1)
		prop.Contract = rpc.ContractParams{ConID: 201, Symbol: "SYN", SecType: "OPT", Exchange: "SMART", Currency: "USD", Right: "C", Strike: 40, Expiry: "20261218", Multiplier: 100}
		prop.Key = proposalKey(prop.Bucket, prop.Contract, prop.Action)
		limit := 0.5
		prop.LimitPrice = &limit
		prop.Mandate.LossCutBase = tc.lossCut
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
