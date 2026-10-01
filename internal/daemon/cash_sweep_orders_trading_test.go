//go:build trading

package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A pre-authorised cash sweep row waits its full veto window, even under the
// latched brake, and its bill's session, then travels the production write
// path: the broker receives one BOND LMT DAY order for the bill by contract
// id, carrying the line's grid.
func TestCashSweepPreAuthorisedBuySubmitsAfterTheWindowAndTheSession(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = ["cash_sweep"]`)
	rig.latched = true
	// [trading].max_notional binds a sweep order like any other; this order
	// is 54,780 USD.
	rig.server.cfg.Trading.MaxNotional = 1e6
	var mu sync.Mutex
	var contracts []ibkrlib.Contract
	var orders []ibkrlib.RawOrder
	rig.server.orderPlaceBroker = func(_ context.Context, c *ibkrlib.Contract, o *ibkrlib.RawOrder) error {
		mu.Lock()
		defer mu.Unlock()
		contracts, orders = append(contracts, *c), append(orders, *o)
		return nil
	}
	// The rig starts at 04:45 in New York, before the bills' assumed session.
	now := rig.now
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
	cashSweepResolveBills(context.Background(), usBillSource(now), policy.Buckets.CashSweep, &plan, now)
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cashSweepCurrencyOf(t, plan, "USD"))
	if row.Contract.ConID != 7101 || row.Quantity != 55 || !row.AutomaticEligible() {
		t.Fatalf("fixture row = %+v", row)
	}
	line := synthBondLine(7101, synthCUSIP35, "USD", cashSweepDay(now).AddDate(0, 0, 35))
	rig.server.orderBondDetailsForTest = func(context.Context, int, string) ([]ibkrlib.BondContractDetails, error) {
		return []ibkrlib.BondContractDetails{line}, nil
	}
	rig.server.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 99.58, 99.62
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	rig.server.orderPreviewPositionImpact = fixedPreviewPosition(0, 55, rpc.OrderPositionEffectOpen)
	// The broker's WhatIf agrees with the assumed unit: its initial-margin
	// change is the order's value, as in a cash account.
	rig.server.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		before, after := 0.0, float64(d.Quantity)*d.Bond.FacePerUnit*d.LimitPrice/100
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true,
			Margin: &rpc.OrderMarginImpact{Currency: "USD", InitialMarginBefore: &before, InitialMarginAfter: &after,
				CommissionCurrency: "USD", MaxCommission: new(1.0)}}, nil
	}

	revision := rig.install(row)
	rig.cycle()
	rec := rig.record(row.Key, revision)
	open := time.Date(2026, 5, 28, 12, 5, 0, 0, time.UTC) // 08:00 in New York plus the opening offset
	if rec.Bucket != preAuthorisedBucketCashSweep || rec.LatchSkippedWindow || !rec.SubmitAt.Equal(open) {
		t.Fatalf("record = %+v, want the full window moved to %s", rec, open)
	}
	rig.notice(rec)
	rig.advance(time.Hour)
	rig.cycle()
	if len(orders) != 0 {
		t.Fatalf("placed before the bill's session: %+v", orders)
	}
	rig.now = open.Add(5 * time.Minute)
	rig.cycle()
	if rec := rig.record(row.Key, revision); rec.State != rpc.TradeProposalAutomaticSubmitted || len(orders) != 1 {
		t.Fatalf("record = %+v, orders %d", rec, len(orders))
	}
	c, o := contracts[0], orders[0]
	if c.SecType != "BILL" || c.ConID != 7101 || c.Currency != "USD" || c.Multiplier != 0 || c.BondRules == nil || *c.BondRules != usBillRules {
		t.Fatalf("broker contract = %+v", c)
	}
	if o.Action != rpc.OrderActionBuy || o.TotalQty != 55 || o.OrderType != rpc.OrderTypeLMT || o.TIF != rpc.OrderTIFDay || o.LmtPrice != 99.6 || o.OutsideRth {
		t.Fatalf("broker order = %+v", o)
	}
	events, err := rig.server.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == orderJournalEventSendAttempted && event.OrderRef == o.OrderRef {
			found = event.FeeUpper != nil && *event.FeeUpper == 1 && event.FeeCurrency == "USD"
		}
	}
	if !found {
		t.Fatal("production transmit did not atomically retain the exact fee bound")
	}

}

// Even an injected stale revision cannot widen the quantity that received
// an automatic notice. The production grant rejects it before broker reads.
func TestCashSweepAutomaticRevalidationCannotWidenTheNoticedQuantity(t *testing.T) {
	rig := newAutomaticTradingRig(t, `pre_authorised = ["cash_sweep"]`)
	row := reviewSweepRow(t, rig.now, 10000)
	revision := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	rec := rig.record(row.Key, revision)
	rig.notice(rec)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	rig.engine.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		changed := row
		changed.Revision = revision
		changed.Quantity, changed.MaxQuantity = row.Quantity+50, row.Quantity+50
		return changed, nil, nil
	}
	rig.engine.submitAutomatic(context.Background(), rec)
	if broker.count() != 0 || rig.record(row.Key, revision).State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatal("automatic send widened the noticed quantity")
	}
}
