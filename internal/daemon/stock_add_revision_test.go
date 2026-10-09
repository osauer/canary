package daemon

import (
	"context"
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestStockAddCashReserveFundedOnceWithoutCurrencyPooling(t *testing.T) {
	bucket := &protectionCashSweepPolicy{KeepCash: new(100.)}
	ledger := map[string]cashSweepLedgerRow{
		"USD": {Observed: true, TradeDate: 2000, Settled: new(2000.), ExchangeRate: 1},
		"EUR": {Observed: true, TradeDate: 1000, Settled: new(900.), ExchangeRate: 2},
	}
	commitments := cashSweepCommitments{Known: true, ByCurrency: map[string]float64{"USD": 200, "EUR": 100}}
	got, err := stockAddCashFunding(bucket, ledger, commitments, "EUR", 1000, 2)
	if err != nil || got.Spendable != 700 || got.ReserveInCurrency != 0 || got.OtherReserveFundingBase != 1700 {
		t.Fatalf("reserve duplicated or native cash pooled: %+v %v", got, err)
	}
	ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 600, Settled: new(600.), ExchangeRate: 1}
	got, err = stockAddCashFunding(bucket, ledger, commitments, "EUR", 1000, 2)
	if err != nil || got.ReserveInCurrency != 350 || got.Spendable != 350 {
		t.Fatalf("reserve shortfall lost: %+v %v", got, err)
	}
	ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 2000, ExchangeRate: 1}
	if _, err := stockAddCashFunding(bucket, ledger, commitments, "EUR", 1000, 2); err == nil {
		t.Fatal("unsettled foreign funds financed the reserve")
	}
}

func TestStockAddUsesExactBrokerMarginAndKeepsLookAheadFloor(t *testing.T) {
	in := stockAddTestInput()
	in.Rules.LookAheadExcessLiquidityBase = new(32000.)
	in.Fee = 5
	w := stockAddTestWhatIf(5)
	w.Margin.MaintenanceMarginAfter = new(41000.)
	got, err := stockAddWhatIfMargin(in, w, 100)
	if err != nil || got.ExcessLiquidityBase != 48995 || *got.LookAheadExcessBase != 30995 {
		t.Fatalf("not exact simulated change: %+v %v", got, err)
	}
	in.Requested, in.BrokerMargin = 99, got
	if p := risk.CheckStockAdd(in); len(p.Blockers) == 0 {
		t.Fatal("another quantity borrowed margin evidence")
	}
	in.Requested = 100
	in.FreeCash = 20000
	if p := risk.CheckStockAdd(in); p.Quantity != 100 {
		t.Fatalf("affordable broker-supported buy held: %+v", p)
	}
	w.Margin.MaintenanceMarginAfter = new(51000.)
	in.BrokerMargin, err = stockAddWhatIfMargin(in, w, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p := risk.CheckStockAdd(in); p.Quantity != 0 {
		t.Fatal("look-ahead margin floor bypassed")
	}
	for _, kind := range []string{"missing", "missing currency", "wrong currency", "nonfinite"} {
		w := stockAddTestWhatIf(5)
		switch kind {
		case "missing":
			w.Margin.EquityWithLoanAfter = nil
		case "missing currency":
			w.Margin.Currency = ""
		case "wrong currency":
			w.Margin.Currency = "EUR"
		case "nonfinite":
			w.Margin.MaintenanceMarginAfter = new(math.Inf(1))
		}
		if _, err := stockAddWhatIfMargin(in, w, 100); err == nil {
			t.Fatalf("accepted %s margin", kind)
		}
	}
}

func TestStockAddMaximumDoesNotAssumeMonotoneFees(t *testing.T) {
	s, in := stockAddTestServer(t)
	in.FreeCash = 12.25
	s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		fee := 5.
		if d.Quantity == 11 {
			fee = .25
		}
		return stockAddTestWhatIf(fee), nil
	}
	p := stockAddTestParams()
	p.LimitPrice = 1
	got, err := s.planStockAdd(t.Context(), p)
	if err != nil || got.Quantity != 11 || got.MaxQuantity != 11 || !got.MaximumKnown || *got.MaxCommission != .25 {
		t.Fatalf("missed cheaper larger candidate: %+v %v", got, err)
	}
}

func TestStockAddMaximumBudgetPreservesUsefulPartialEvidence(t *testing.T) {
	s, in := stockAddTestServer(t)
	in.FreeCash = 1000
	calls := 0
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		calls++
		return stockAddTestWhatIf(500), nil
	}
	p := stockAddTestParams()
	p.LimitPrice = 1
	got, err := s.planStockAdd(t.Context(), p)
	if err != nil || got.Quantity != 0 || got.MaximumKnown || got.MaxQuantity != 0 || got.SupportedQuantity != 500 || got.Review != nil || len(got.Allowances) == 0 || len(got.Blockers) != 1 || got.Blockers[0].Code != "add_search_incomplete" || calls != stockAddQuoteBudget {
		t.Fatalf("partial result misrepresented: %+v %v calls=%d", got, err, calls)
	}
	p.Quantity, p.Max = 500, false
	got, err = s.planStockAdd(t.Context(), p)
	if err != nil || got.Quantity != 500 || got.Review == nil || got.MaximumKnown {
		t.Fatalf("explicit affordable candidate held: %+v %v", got, err)
	}
}

func TestStockAddPlanDisclosesProtectionAndAllAllowances(t *testing.T) {
	s, in := stockAddTestServer(t)
	in.CurrentQuantity, in.StopQuantity, in.PendingQuantity = 40, 30, 5
	p := stockAddTestParams()
	p.Quantity, p.Max = 10, false
	got, err := s.planStockAdd(t.Context(), p)
	if err != nil || got.Quantity != 10 || len(got.Allowances) != 4 || len(got.RiskChecks) != 10 || got.Protection == nil || got.Protection.UncoveredBefore != 10 || got.Protection.UncoveredAfter != 20 || got.Protection.PendingQuantity != 5 {
		t.Fatalf("missing position plan: %+v %v", got, err)
	}
}

func TestStockAddBrokerMarginRetainsPendingCashCommitments(t *testing.T) {
	in := stockAddTestInput()
	in.Rules.ExcessLiquidityBase = new(60000.)
	in.Rules.LookAheadExcessLiquidityBase = new(55000.)
	in.PendingCostBase = 40000
	in.Fee = 5
	w := stockAddTestWhatIf(5) // Broker before headroom = 50,000.
	got, err := stockAddWhatIfMargin(in, w, 3)
	if err != nil || got.ExcessLiquidityBase != 9995 || *got.LookAheadExcessBase != 9995 {
		t.Fatalf("pending cash commitments disappeared from broker basis: %+v %v", got, err)
	}
}
