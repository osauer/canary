package daemon

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashSweepCommitmentTotalCannotOverflowIntoRedemption(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Account: scope.Account,
		Action: rpc.OrderActionBuy, SecType: "STK", Currency: "USD", OrderType: "LMT", TotalQuantity: 1, Remaining: 1, LimitPrice: 1e308}
	queued := queuedAuthRecord{State: rpc.QueuedAuthArmed, Terms: rpc.QueuedAuthTerms{AccountID: scope.Account, AccountMode: scope.Mode, Action: rpc.OrderActionBuy,
		MaxQuantity: 1, WorstPrice: 1e308, Currency: "USD", Contract: rpc.ContractParams{SecType: "STK", Currency: "USD"}}}
	for _, tc := range []struct {
		name   string
		orders []ibkrlib.OrderLifecycleEvent
		queued []queuedAuthRecord
	}{
		{"working", []ibkrlib.OrderLifecycleEvent{order, order}, nil},
		{"queued", nil, []queuedAuthRecord{queued, queued}},
		{"mixed", []ibkrlib.OrderLifecycleEvent{order}, []queuedAuthRecord{queued}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cashSweepCommitmentsFrom(tc.orders, tc.queued, scope)
			in := cashSweepTestInput(map[string]float64{"USD": 60000})
			in.Commitments = got
			in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", "us_tbill", 10, 60)}
			plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9), in, cashSweepTestNow())
			if cp := cashSweepCurrencyOf(t, plan, "USD"); cp.status.State != rpc.CashSweepStateSettlementUnknown || cp.side != "" {
				t.Fatalf("overflowing commitments manufactured a redemption: %+v", cp)
			}
			if got.Unknown["USD"] == "" || math.IsInf(got.ByCurrency["USD"], 0) {
				t.Fatalf("nonfinite total was recorded as known: %+v", got)
			}
			if _, err := json.Marshal(plan.status); err != nil {
				t.Fatalf("unknown commitment corrupted status diagnostics: %v", err)
			}
		})
	}
}

// A buy stop can gap beyond its trigger; a trailing limit can ratchet its
// limit. Neither current trigger nor stale limit fields may certify cash
// available for a sweep.
func TestCashSweepWorkingBuysRequireAFixedFiniteLimit(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, tc := range []struct {
		name, orderType  string
		price, remaining float64
		bounded          bool
	}{
		{"fixed limit", "LMT", 10, 100, true},
		{"fixed stop limit", "STP LMT", 10, 100, true},
		{"market with stale limit", "MKT", 10, 100, false},
		{"stop market with trigger", "STP", 0, 100, false},
		{"trailing market with trigger", "TRAIL", 0, 100, false},
		{"trailing limit can move", "TRAIL LIMIT", 10, 100, false},
		{"missing order type", "", 10, 100, false},
		{"nonfinite limit", "LMT", math.Inf(1), 100, false},
		{"nonfinite quantity", "LMT", 10, math.NaN(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Account: scope.Account,
				Action: rpc.OrderActionBuy, SecType: "STK", Currency: "USD", OrderType: tc.orderType,
				TotalQuantity: 100, Remaining: tc.remaining, LimitPrice: tc.price, AuxPrice: 10, TrailStopPrice: 10}
			got := cashSweepCommitmentsFrom([]ibkrlib.OrderLifecycleEvent{order}, nil, scope)
			if tc.bounded {
				if !strings.Contains(got.Unknown["USD"], "commission") || got.Unknown[""] == "" || got.ByCurrency["USD"] != 1000 {
					t.Fatalf("fixed limit commitment = %+v", got)
				}
				return
			}
			if got.Unknown["USD"] == "" || got.ByCurrency["USD"] != 0 {
				t.Fatalf("unbounded buy counted as available cash: %+v", got)
			}
			in := cashSweepTestInput(map[string]float64{"USD": 60000})
			in.Commitments = got
			plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9), in, cashSweepTestNow())
			if cp := cashSweepCurrencyOf(t, plan, "USD"); cp.status.State != rpc.CashSweepStateSettlementUnknown || cp.side != "" {
				t.Fatalf("sweep planned with an unbounded buy: %+v", cp)
			}
		})
	}
}

// No journal age can establish when T+2 products or holiday-delayed sales
// settle. The broker's own settled balance remains usable; an estimate does
// not make cash eligible for a sweep.
func TestCashSweepJournalEstimateCannotCertifySettledCash(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, at := range []string{"2026-10-07T09:00:00Z", "2026-10-13T09:00:00Z"} {
		t.Run(at, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, at)
			if err != nil {
				t.Fatal(err)
			}
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			srv.startedAt = now.AddDate(0, 0, -14)
			estimate := (&proposalEngine{server: srv}).cashSweepSettlement(scope, now)
			if estimate.Known || !strings.Contains(estimate.Reason, "verified settlement") {
				t.Fatalf("journal certified settlement: %+v", estimate)
			}
			in := cashSweepTestInput(map[string]float64{"USD": 60000})
			in.Settlement = estimate
			policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
			if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD"); cp.side != "" || cp.status.State != rpc.CashSweepStateSettlementUnknown {
				t.Fatalf("unverified cash produced a sweep: %+v", cp)
			}
			row := in.Ledger["USD"]
			settled := 60000.0
			row.Settled = &settled
			in.Ledger["USD"] = row
			if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD"); cp.side != rpc.CashSweepSideInvest || cp.status.SettledCashSource != rpc.CashSweepSettledSourceBroker {
				t.Fatalf("broker settled cash was not admitted: %+v", cp)
			}
		})
	}
}
