package daemon

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashSweepMinimumIsAWholeOrderInBaseOnBothSides(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	policy.Buckets.CashSweep.MinOrderNotional = 10000
	in := cashSweepTestInput(map[string]float64{"USD": 15500, "EUR": 2000})
	in.Holdings["EUR"] = eurRedeemInput(50000).Holdings["EUR"]
	plan := cashSweepPlanFor(policy, in, cashSweepTestNow())
	usd := cashSweepCurrencyOf(t, plan, "USD")
	if usd.side != "" {
		t.Fatal("10500 USD is below the EUR-equivalent whole-order floor")
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideRedeem || float64(eur.quantity)*0.995 < 10000 {
		t.Fatalf("small cash gap produced a small order: %+v", eur)
	}
	prop, preview := sweepFeePreview(30000, 10000, 10)
	prop.CashSweep.MinOrderNotionalBase = 10000
	preview.Draft.Quantity = 10
	preview.Draft.LimitPrice = 99.99
	if got := cashSweepEconomicsBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_below_minimum_tranche") {
		t.Fatalf("lot/price rounding lost the floor: %v", got)
	}
	prop.CashSweep.Side = rpc.CashSweepSideRedeem
	prop.CashSweep.MinTranche = 0
	preview.Draft.LimitPrice = 100
	prop.CashSweep.RedemptionTarget = 10000
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("fee changed the gross whole-order floor: %v", got)
	}
	if got := cashSweepEconomicsAdvisory(prop, preview); got.State != "partial_restoration" || got.NetProceeds == nil || *got.NetProceeds != 9990 || got.RemainingGap == nil || *got.RemainingGap != 10 {
		t.Fatalf("sale fee or remaining target disappeared: %+v", got)
	}
	preview.Draft.LimitPrice = 100.1
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("whole-order floor failed: %v", got)
	}
	preview.Draft.LimitPrice = 99.99
	if got := cashSweepEconomicsBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_below_minimum_tranche") {
		t.Fatalf("sale below gross floor admitted: %v", got)
	}
	preview.Draft.LimitPrice = 100
	preview.WhatIf.Margin.Commission, preview.WhatIf.Margin.MinCommission = nil, nil
	preview.WhatIf.Margin.MaxCommission = new(10000.0)
	if got := cashSweepEconomicsBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_net_proceeds_invalid") {
		t.Fatalf("sale cannot restore cash: %v", got)
	}
}

func TestCashSweepPurchaseGainIncludesCashInterestAndSpread(t *testing.T) {
	prop, preview := sweepFeePreview(30000, 990, 1)
	prop.CashSweep.MinTranche = 0
	prop.CashSweep.MinNetGainBase = 25
	prop.CashSweep.Bill.Maturity = cashSweepDay(preview.AsOf).AddDate(0, 0, 90).Format(time.DateOnly)
	prop.CashSweep.CashInterestRateUpper = new(0.04)
	prop.CashSweep.CashInterestValidThrough = "2027-12-31"
	preview.Quote.Ask = new(99.0)
	preview.Quote.DataType = rpc.MarketDataLive
	// Ten units, 10000 face. Gross discount 100, fee 1, forgone interest ~98.
	preview.Draft.Quantity = 10
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) > 0 {
		t.Fatalf("advisory cash opportunity cost blocked the order: %v", got)
	}
	if got := cashSweepEconomicsAdvisory(prop, preview); got.State != "below_benchmark" || got.IncrementalGainBase == nil || *got.IncrementalGainBase >= 25 {
		t.Fatalf("ignored cash opportunity cost: %+v", got)
	}
	prop.CashSweep.CashInterestRateUpper = new(0.0)
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("explicit zero interest with worthwhile bill: %v", got)
	}
	if got := cashSweepEconomicsAdvisory(prop, preview); got.State != "estimated" || got.IncrementalGainBase == nil || math.Abs(*got.IncrementalGainBase-99) > 1e-9 {
		t.Fatalf("explicit zero interest was lost: %+v", got)
	}
	// The actual limit is attractive, but an ask including spread is too costly.
	preview.Quote.Ask = new(99.9)
	if got := cashSweepEconomicsAdvisory(prop, preview); got.State != "below_benchmark" || got.IncrementalGainBase == nil || math.Abs(*got.IncrementalGainBase-9) > 1e-9 {
		t.Fatalf("spread was ignored: %+v", got)
	}
	for _, change := range []func(){func() { prop.CashSweep.CashInterestRateUpper = nil }, func() { prop.CashSweep.CashInterestRateUpper = new(math.NaN()) }, func() {
		prop.CashSweep.CashInterestRateUpper = new(0.04)
		prop.CashSweep.CashInterestValidThrough = "2026-01-01"
	}} {
		change()
		if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
			t.Fatalf("unknown/stale cash interest blocked the order: %v", got)
		}
		if got := cashSweepEconomicsAdvisory(prop, preview); got.State != "unavailable" || got.IncrementalGainBase != nil {
			t.Fatalf("unknown/stale cash interest became zero: %+v", got)
		}
	}
}

func TestCashSweepPaymentCalendarsAreNotStockCalendars(t *testing.T) {
	parse := func(v string) time.Time {
		d, err := time.Parse(time.DateOnly, v)
		if err != nil {
			t.Fatal(err)
		}
		return d.Add(12 * time.Hour)
	}
	for _, tc := range []struct {
		ccy, trade, want string
		lag              int
	}{
		{"USD", "2026-10-09", "2026-10-13", 1}, // Columbus Day
		{"USD", "2026-11-10", "2026-11-12", 1}, // Veterans Day
		{"USD", "2026-07-02", "2026-07-03", 1}, // Fedwire open, equities closed
		{"EUR", "2026-04-02", "2026-04-07", 1}, // Good Friday + Easter Monday
		{"EUR", "2026-04-30", "2026-05-05", 2}, // Lag is route evidence, not T+1
	} {
		got, reason := cashSweepSettlementDate(tc.ccy, "SMART", new(tc.lag), "2028-12-31", parse(tc.trade))
		if reason != "" || got.Format(time.DateOnly) != tc.want {
			t.Fatalf("%s %s: %v %s", tc.ccy, tc.trade, got, reason)
		}
	}
	for _, tc := range []struct {
		ccy, trade string
		lag        *int
		valid      string
	}{
		{"USD", "2026-10-01", nil, "2028-12-31"},
		{"USD", "2026-10-01", new(1), "2026-09-30"},
		{"USD", "2028-12-29", new(1), "2029-12-31"},
		{"GBP", "2026-10-01", new(1), "2028-12-31"},
	} {
		if _, reason := cashSweepSettlementDate(tc.ccy, "SMART", tc.lag, tc.valid, parse(tc.trade)); reason == "" {
			t.Fatalf("unsupported evidence guessed: %+v", tc)
		}
	}
}

func syntheticWorkingBuyFee() (brokerStateScope, ibkrlib.OrderLifecycleEvent, orderJournalEvent, cashSweepFeeEvidence) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	o := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 71, ClientID: 15, ClientIDPresent: true, OrderRef: "synthetic-medium-fee", Account: scope.Account, Status: "Submitted", ConID: 711, SecType: "STK", Exchange: "SMART", Currency: "USD", Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, TotalQuantity: 10, Filled: 4, Remaining: 6, LimitPrice: 100}
	ev := orderJournalEvent{At: cashSweepTestNow(), Type: orderJournalEventSendAttempted, ReservedOrderID: o.OrderID, ClientID: o.ClientID, Account: scope.Account, Endpoint: "127.0.0.1:4001", Mode: scope.Mode, OrderRef: o.OrderRef, ConID: o.ConID, SecType: o.SecType, Exchange: o.Exchange, Currency: o.Currency, Action: o.Action, OrderType: o.OrderType, TIF: o.TIF, Quantity: o.TotalQuantity, LimitPrice: o.LimitPrice, SendState: orderSendStateSendAttempted}
	attachOrderFeeBound(&ev, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{CommissionCurrency: "USD", MaxCommission: new(10.0)}})
	return scope, o, ev, cashSweepFeeEvidence{Now: cashSweepTestNow().Add(time.Minute), Endpoint: ev.Endpoint}
}

func TestCashSweepFeeBoundSurvivesSQLiteRestartAndPartialFill(t *testing.T) {
	scope, o, ev, evidence := syntheticWorkingBuyFee()
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	journal := newTestOrderJournalStore(t, path)
	if err := journal.Append(ev); err != nil {
		t.Fatal(err)
	}
	if err := journal.authority.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestOrderJournalStore(t, path)
	events, err := reopened.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Events = events
	got := cashSweepCommitmentsFrom([]ibkrlib.OrderLifecycleEvent{o}, nil, scope, evidence)
	if len(got.Unknown) != 0 || got.ByCurrency["USD"] != 610 {
		t.Fatalf("restart lost whole commission or counted filled principal: %+v", got)
	}
	for name, change := range map[string]func(*ibkrlib.OrderLifecycleEvent, *cashSweepFeeEvidence){
		"changed quantity": func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.TotalQuantity++ },
		"changed limit":    func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.LimitPrice++ },
		"client unknown":   func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.ClientIDPresent = false },
		"other endpoint":   func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Endpoint = "127.0.0.1:4002" },
		"next day":         func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Now = e.Now.AddDate(0, 0, 1) },
		"unused preview": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			v := ev
			v.Type = orderJournalEventPreviewed
			e.Events = []orderJournalEvent{v}
		},
		"unbounded newer modify": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			v := ev
			v.Type = orderJournalEventModifyRequested
			v.FeeUpper = nil
			e.Events = append(e.Events, v)
		},
	} {
		t.Run(name, func(t *testing.T) {
			order, proof := o, evidence
			proof.Events = append([]orderJournalEvent(nil), evidence.Events...)
			change(&order, &proof)
			got := cashSweepCommitmentsFrom([]ibkrlib.OrderLifecycleEvent{order}, nil, scope, proof)
			if len(got.Unknown) == 0 {
				t.Fatal("stale or unrelated bound admitted a sweep")
			}
		})
	}
}

func TestCashSweepUnacknowledgedBuyCannotDisappearFromCommitments(t *testing.T) {
	scope, _, ev, evidence := syntheticWorkingBuyFee()
	evidence.Events = []orderJournalEvent{ev}
	got := cashSweepCommitmentsFrom(nil, nil, scope, evidence)
	if got.Unknown[""] == "" {
		t.Fatal("empty inventory erased a local buy before broker acknowledgement")
	}
}

func TestCashSweepRetainedPreviewExpirySeparatesAdvisoryFromSettlement(t *testing.T) {
	prop, preview := sweepFeePreview(30000, 1000, 1)
	prop.CashSweep.MinNetGainBase = 25
	prop.CashSweep.CashInterestRateUpper = new(0.0)
	prop.CashSweep.CashInterestValidThrough = preview.AsOf.Format(time.DateOnly)
	if got := cashSweepCurrentEvidenceBlockers(prop, preview.AsOf); len(got) > 0 {
		t.Fatalf("valid assumption refused: %v", got)
	}
	if got := cashSweepCurrentEvidenceBlockers(prop, preview.AsOf.AddDate(0, 0, 1)); len(got) != 0 {
		t.Fatalf("expired advisory cash-interest assumption blocked order consumption: %v", got)
	}
	prop.CashSweep.SettlementValidThrough = preview.AsOf.Format(time.DateOnly)
	if got := cashSweepCurrentEvidenceBlockers(prop, preview.AsOf.AddDate(0, 0, 1)); !hasTradingBlocker(got, "cash_sweep_settlement_calendar_unknown") {
		t.Fatalf("old preview renewed expired route: %v", got)
	}
}
