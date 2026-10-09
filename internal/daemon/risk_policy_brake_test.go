package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// brakeTestServer is a preview server whose risk policy is an approved
// constitution with order limits, and whose capital store the test latches
// or leaves within the limit (owner decision 2026-10-09 18:46 CEST: an
// engaged brake is a block).
func brakeTestServer(t *testing.T) (*Server, *riskCapitalStore, *risk.Constitution, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.now = func() time.Time { return now }
	srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
	c := testConstitutionV3()
	c.OrderLimits = testOrderLimitsTable(10000)
	m := newRiskPolicyManager("", time.Minute, nil)
	m.active, m.status, m.source = c, rpc.RiskPolicyStatusActive, "file"
	srv.riskPolicies = m
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return now }
	srv.riskCapital = st
	reconcileNow(t, st)
	return srv, st, c, now
}

func brakeStockBuy() rpc.OrderPreviewParams {
	limit := 2.5
	return rpc.OrderPreviewParams{Action: "buy", Quantity: 100, LimitPrice: &limit,
		Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
}

// A latched brake refuses a risk-adding preview with the drawdown_brake
// blocker, lets a reduction and a close through, and the same refusal stands
// at admission and the wire guard.
func TestDrawdownBrakeRefusesRiskAddingOrders(t *testing.T) {
	srv, st, c, now := brakeTestServer(t)
	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope, true)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope, true)
	if rep := st.Report(c, nil, brokerStateScope{}); !rep.BlockLatched {
		t.Fatalf("fixture did not latch: %+v", rep)
	}

	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 100, rpc.OrderPositionEffectOpen)
	_, err := srv.previewOrder(t.Context(), brakeStockBuy())
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != drawdownBrakeCode ||
		!strings.Contains(blockers[0].Message, "Drawdown brake on") || !strings.Contains(blockers[0].Message, "Reductions, closes, cancels and hedges stay open") {
		t.Fatalf("buy under a latched brake: err %v blockers %+v, want drawdown_brake", err, blockers)
	}

	// Admission and the wire guard read the same brake.
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
	if err := srv.riskPolicyBrakeError(draft, rpc.OrderPositionImpact{Before: 0, After: 100, Effect: rpc.OrderPositionEffectOpen}); err == nil || !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "Drawdown brake on") {
		t.Fatalf("admission under a latched brake: %v, want a trading-disabled refusal", err)
	}

	// A reduction and a close pass the brake.
	for _, effect := range []string{rpc.OrderPositionEffectReduce, rpc.OrderPositionEffectClose} {
		sell := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 50, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
		if err := srv.riskPolicyBrakeError(sell, rpc.OrderPositionImpact{Before: 100, After: 50, Effect: effect}); err != nil {
			t.Fatalf("%s under a latched brake: %v, want allowed", effect, err)
		}
	}
	// A policy-classified hedge entry passes too.
	hedge := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 1, Contract: rpc.ContractParams{Symbol: srv.rulebookPolicy().HedgeSymbols[0], SecType: "OPT", Right: "P", Currency: "USD"}}
	if err := srv.riskPolicyBrakeError(hedge, rpc.OrderPositionImpact{Before: 0, After: 1, Effect: rpc.OrderPositionEffectOpen}); err != nil {
		t.Fatalf("hedge put under a latched brake: %v, want allowed", err)
	}
}

// Within the limit the brake refuses nothing and adds no cause; a breached
// warn tier keeps its advisory cause on the preview.
func TestDrawdownBrakeWithinTheLimitRefusesNothing(t *testing.T) {
	srv, st, c, now := brakeTestServer(t)
	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope, true)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 100, rpc.OrderPositionEffectOpen)
	res, err := srv.previewOrder(t.Context(), brakeStockBuy())
	if err != nil {
		t.Fatalf("buy within the limit: %v", err)
	}
	for _, w := range res.Warnings {
		if w.Code == "capital_drawdown" {
			t.Fatalf("within the limit the preview carried a capital cause: %+v", w)
		}
	}
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
	if err := srv.riskPolicyBrakeError(draft, rpc.OrderPositionImpact{Before: 0, After: 100, Effect: rpc.OrderPositionEffectOpen}); err != nil {
		t.Fatalf("admission within the limit: %v", err)
	}
}

// Stale capital evidence refuses a risk-adding order: the brake cannot be
// shown released, so hard enforcement fails closed (decision 7).
func TestDrawdownBrakeFailsClosedOnStaleEvidence(t *testing.T) {
	srv, st, c, now := brakeTestServer(t)
	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope, true)
	later := now.Add(time.Duration(*c.Capital.MaxEquityAgeMinutes+1) * time.Minute)
	srv.now = func() time.Time { return later }
	st.now = func() time.Time { return later }
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
	blocker, _ := srv.riskPolicyBrake(draft, rpc.OrderPositionImpact{Before: 0, After: 100, Effect: rpc.OrderPositionEffectOpen})
	if blocker == nil || blocker.Code != drawdownBrakeCode || !strings.Contains(blocker.Message, "stale") {
		t.Fatalf("buy on stale equity: %+v, want drawdown_brake naming the stale evidence", blocker)
	}
	if err := srv.riskPolicyBrakeError(draft, rpc.OrderPositionImpact{Before: 100, After: 0, Effect: rpc.OrderPositionEffectClose}); err != nil {
		t.Fatalf("close on stale equity: %v, want allowed", err)
	}
}

// The constitution accepts hard, and only hard, as the block tier's class.
func TestConstitutionRejectsRetiredEnforcementClasses(t *testing.T) {
	for _, class := range []string{risk.EnforcementShadow, risk.EnforcementAdvisory} {
		c := testConstitutionV3()
		c.Drawdown.BlockEnforcement = class
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "retired on 2026-10-09") {
			t.Fatalf("%s: %v, want the retired-class refusal", class, err)
		}
	}
	for _, class := range []string{"", risk.EnforcementHard} {
		c := testConstitutionV3()
		c.Drawdown.BlockEnforcement = class
		if err := c.Validate(); err != nil {
			t.Fatalf("%q: %v", class, err)
		}
		if c.EffectiveBlockEnforcement() != risk.EnforcementHard {
			t.Fatalf("%q resolves to %s, want hard", class, c.EffectiveBlockEnforcement())
		}
	}
}
