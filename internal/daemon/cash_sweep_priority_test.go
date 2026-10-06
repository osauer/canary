package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func sweepFundingFixture(now time.Time) *risk.CashSweepFundingEvidence {
	return &risk.CashSweepFundingEvidence{AsOf: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute), ScenarioFingerprint: "synthetic-stress-v1",
		FundingNative: map[string]float64{"EUR": 2000, "USD": 10000}, NAVFloorPassed: true, MarginPassed: true}
}

func observedSweepFundingInput(cash map[string]float64) cashSweepInput {
	in := cashSweepTestInput(cash)
	for ccy, row := range in.Ledger {
		row.Settled = new(row.TradeDate)
		in.Ledger[ccy] = row
	}
	return in
}

func TestCashSweepPriorityProtectsNativeCashAndDefaultsUSDFirst(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 100000)
	policy.Cash.Sweep.ReserveCushionEUR = new(10000.)
	in := observedSweepFundingInput(map[string]float64{"EUR": 50000, "USD": 50000})
	in.FundingEvidence = sweepFundingFixture(now)
	plan := cashSweepPlanFor(policy, in, now)
	if plan.status.CurrencyPriority != rpc.CashSweepPriorityUSDFirst || plan.currencies[0].status.Currency != "USD" || !plan.status.Shadow {
		t.Fatal(plan.status)
	}
	usd := cashSweepCurrencyOf(t, plan, "USD")
	if usd.status.FundingNeed == nil || usd.status.BufferAllocation == nil || usd.status.EffectiveReserve == nil {
		t.Fatal("reserve missing")
	}
	if usd.free != 50000-*usd.status.EffectiveReserve || usd.quantity > int(usd.free) || usd.status.KeepCash != 5000 {
		t.Fatal("native reserve relaxed or floor overwritten", usd)
	}
	// The aggregate cushion is one amount, not 10k per currency.
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if *eur.status.BufferAllocation+*usd.status.BufferAllocation*.9 < 9999.99 || *eur.status.BufferAllocation+*usd.status.BufferAllocation*.9 > 10000.01 {
		t.Fatal("cushion duplicated")
	}
	for _, mode := range []string{rpc.CashSweepPriorityUSDFirst, rpc.CashSweepPriorityBalanced, rpc.CashSweepPriorityEURFirst} {
		policy.Cash.Sweep.CurrencyPriority = mode
		p := cashSweepPlanFor(policy, in, now)
		want := "EUR" // largest native surplus in EUR-equivalent terms for Balanced
		if mode == rpc.CashSweepPriorityUSDFirst {
			want = "USD"
		}
		if p.currencies[0].status.Currency != want {
			t.Fatalf("%s first %s", mode, p.currencies[0].status.Currency)
		}
		for _, cp := range p.currencies {
			if cp.status.Currency != "EUR" && cp.status.Currency != "USD" {
				t.Fatal("conversion manufactured")
			}
		}
	}
}

func TestCashSweepReserveOptInMissingEvidenceHoldsAndRetainsCashGap(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	in := cashSweepTestInput(map[string]float64{"EUR": 50000, "USD": 50000})
	legacy := cashSweepPlanFor(policy, in, cashSweepTestNow())
	if legacy.status.ReserveState != "" || legacy.currencies[0].side == "" {
		t.Fatal("existing policy changed")
	}
	policy.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityUSDFirst
	plan := cashSweepPlanFor(policy, in, cashSweepTestNow())
	if plan.status.ReserveState != "unavailable" || !strings.Contains(plan.status.ReserveReason, "reserve_calibration_required") {
		t.Fatal(plan.status)
	}
	for _, cp := range plan.currencies {
		if cp.side != "" || cp.status.Free != nil {
			t.Fatal("unknown reserve authorized money", cp)
		}
	}
	in.Settlement.Known, in.Settlement.Reason = false, "intraday cash activity is incomplete"
	plan = cashSweepPlanFor(policy, in, cashSweepTestNow())
	if plan.currencies[0].status.State != rpc.CashSweepStateSettlementUnknown {
		t.Fatal("reserve hid cash-coverage gap", plan.status)
	}
	policy.Cash.Sweep.CurrencyPriority = "convert_eur_to_usd"
	if err := validateCashSweepPolicy("cash_sweep", policy.Cash.Sweep); err == nil {
		t.Fatal("fourth mode accepted")
	}
}

func TestCashSweepPriorityLiquidityRestorationFirst(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	policy.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityUSDFirst
	in := observedSweepFundingInput(map[string]float64{"EUR": 1000, "USD": 50000})
	in.FundingEvidence = sweepFundingFixture(now)
	in.Holdings["EUR"] = []cashSweepHolding{cashSweepTestBill(801, "EUR", "de_bubill", 10, 60)}
	p := cashSweepPlanFor(policy, in, now)
	if p.currencies[0].status.Currency != "EUR" || p.currencies[0].side != rpc.CashSweepSideRedeem {
		t.Fatal("USD preference displaced EUR funding", p.status)
	}
}

func TestCashSweepInvestmentRequiresEveryNativeReserveActuallyFunded(t *testing.T) {
	now := cashSweepTestNow()
	p := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	p.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityEURFirst
	in := observedSweepFundingInput(map[string]float64{"EUR": 50000, "USD": 1000})
	in.FundingEvidence = sweepFundingFixture(now)
	in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", "us_tbill", 40, 60)}
	plan := cashSweepPlanFor(p, in, now)
	if plan.status.ReserveState != "funding_hold" || !strings.Contains(plan.status.ReserveReason, "USD cash reserve is not funded") {
		t.Fatal(plan.status)
	}
	if cashSweepCurrencyOf(t, plan, "EUR").side != "" {
		t.Fatal("EUR spent an unfunded USD cushion")
	}
	if cashSweepCurrencyOf(t, plan, "USD").side != rpc.CashSweepSideRedeem {
		t.Fatal("funding top-up was blocked")
	}
	// A pending USD sale larger than the shortfall is still no settled funding.
	in.Settlement.EquivalentSales["USD"] = 30000
	plan = cashSweepPlanFor(p, in, now)
	if cashSweepCurrencyOf(t, plan, "EUR").side != "" || plan.status.ReserveState != "funding_hold" {
		t.Fatal("pending sale funded EUR sweep")
	}
	row := in.Ledger["USD"]
	row.TradeDate, row.Settled = 50000, new(50000.)
	in.Ledger["USD"] = row
	in.Settlement.EquivalentSales["USD"] = 0
	plan = cashSweepPlanFor(p, in, now)
	if plan.status.ReserveState != "ready" || cashSweepCurrencyOf(t, plan, "EUR").side != rpc.CashSweepSideInvest {
		t.Fatal("actual native funding did not clear hold", plan.status)
	}
	in.Commitments.ByCurrency["USD"] = math.NaN()
	plan = cashSweepPlanFor(p, in, now)
	if cashSweepCurrencyOf(t, plan, "EUR").side != "" || plan.status.ReserveState != "funding_hold" {
		t.Fatal("nonfinite commitment funded aggregate cushion")
	}
	delete(in.Commitments.ByCurrency, "USD")
	// A complete journal estimate still cannot stand as broker cash authority.
	row.Settled = nil
	in.Ledger["USD"] = row
	plan = cashSweepPlanFor(p, in, now)
	if cashSweepCurrencyOf(t, plan, "EUR").side != "" || plan.status.ReserveState != "funding_hold" {
		t.Fatal("journal estimate funded aggregate cushion")
	}
}

func TestCashSweepAutomaticPriorityLeavesOtherBucketsAndGatesInPlace(t *testing.T) {
	records := []automaticSubmissionRecord{{Key: "eur", Revision: "r", Bucket: rpc.TradeProposalBucketCashSweep}, {Key: "stop", Revision: "r", Bucket: "trailing_stop"}, {Key: "usd", Revision: "r", Bucket: rpc.TradeProposalBucketCashSweep}}
	proposals := []rpc.TradeProposal{{Key: "eur", Revision: "r", CashSweep: &rpc.TradeProposalCashSweep{PriorityRank: 2}}, {Key: "usd", Revision: "r", CashSweep: &rpc.TradeProposalCashSweep{PriorityRank: 1}}}
	prioritizeCashSweepRecords(records, proposals)
	if records[0].Key != "usd" || records[1].Key != "stop" || records[2].Key != "eur" {
		t.Fatal(records)
	}
	if records[0].State != "" || records[0].due(time.Now(), true) {
		t.Fatal("priority invented authority")
	}
}
