package daemon

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func syntheticFlexProjectionInputs() (flexCashBaseline, brokerStateScope, cashSweepLedgerRow, cashSweepSettlement, cashSweepCommitments) {
	scope := brokerStateScope{Account: "U-SYNTHETIC", Mode: rpc.AccountModeLive}
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	baseline := flexCashBaseline{Scope: scope, QueryFingerprint: flexQueryFingerprint("synthetic-query"), ReportFingerprint: "synthetic-report",
		ToDate: day, ReportDate: day, AcceptedAt: day.Add(30 * time.Hour), ActivityFrom: day,
		Currencies: map[string]flexCashBaselineCurrency{"EUR": {EndingCash: new(12000.0), EndingSettledCash: new(10000.0)}}}
	current := cashSweepLedgerRow{Observed: true, TradeDate: 16000, ExchangeRate: 1}
	known := cashSweepSettlement{Known: true, PurchaseCosts: map[string]float64{"EUR": 1500}, SaleProceeds: map[string]float64{"EUR": 6000}}
	commitments := cashSweepCommitments{Known: true, ByCurrency: map[string]float64{"EUR": 500}}
	return baseline, scope, current, known, commitments
}

func TestFlexProjectionDeductsBuysExcludesSalesAndCapsAtLiveCash(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                                   string
		live, purchases, sales, committed, reserve, cash, free float64
	}{
		{"purchase principal deduction", 16000, 1500, 6000, 500, 1000, 8500, 7000},
		{"larger sales cannot add cash", 12000, 1500, 900000, 500, 1000, 0, 0},
		{"lower live cash excludes sale credits", 6000, 1500, 1000, 500, 1000, 5000, 3500},
		{"negative baseline estimate clamps", 12000, 11000, 6000, 500, 1000, 0, 0},
		{"negative free clamps", 16000, 1500, 6000, 9000, 1000, 8500, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline, scope, current, known, commitments := syntheticFlexProjectionInputs()
			current.TradeDate = tc.live
			known.PurchaseCosts["EUR"] = tc.purchases
			known.SaleProceeds["EUR"] = tc.sales
			commitments.ByCurrency["EUR"] = tc.committed
			p := cashSweepFlexProjection(baseline, nil, scope, "EUR", current, known, commitments, tc.reserve)
			if p.State != "held" || p.EstimatedCash == nil || *p.EstimatedCash != tc.cash || p.EstimatedFree == nil || *p.EstimatedFree != tc.free || p.KnownExcludedSales == nil || *p.KnownExcludedSales != tc.sales || len(p.CoverageGaps) == 0 {
				t.Fatalf("projection=%+v", p)
			}
			*p.BaselineSettledCash = 1
			p.CoverageGaps[0] = "mutated"
			if *baseline.Currencies["EUR"].EndingSettledCash != 10000 || flexCashProjectionCoverageGaps[0] == "mutated" {
				t.Fatal("projection aliases source evidence")
			}
		})
	}
}

func TestFlexProjectionUnknownEvidenceNeverBecomesZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		change   func(*flexCashBaseline, *cashSweepLedgerRow, *cashSweepSettlement, *cashSweepCommitments)
		wantCash bool
	}{
		{"unknown purchases", func(_ *flexCashBaseline, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments) {
			k.Known = false
		}, false},
		{"currency purchase gap", func(_ *flexCashBaseline, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments) {
			k.Unknown = map[string]string{"EUR": "missing fees"}
		}, false},
		{"global purchase gap", func(_ *flexCashBaseline, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments) {
			k.Unknown = map[string]string{"": "unknown execution scope"}
		}, false},
		{"unknown commitments", func(_ *flexCashBaseline, _ *cashSweepLedgerRow, _ *cashSweepSettlement, c *cashSweepCommitments) {
			c.Known = false
		}, true},
		{"currency commitment gap", func(_ *flexCashBaseline, _ *cashSweepLedgerRow, _ *cashSweepSettlement, c *cashSweepCommitments) {
			c.Unknown = map[string]string{"EUR": "working fees unknown"}
		}, true},
		{"missing current observation", func(_ *flexCashBaseline, c *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments) {
			c.Observed = false
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, s, c, k, commit := syntheticFlexProjectionInputs()
			tc.change(&b, &c, &k, &commit)
			p := cashSweepFlexProjection(b, nil, s, "EUR", c, k, commit, 1000)
			if p.State != "held" || (p.EstimatedCash != nil) != tc.wantCash || p.EstimatedFree != nil {
				t.Fatalf("projection=%+v", p)
			}
		})
	}
	b, s, c, k, commit := syntheticFlexProjectionInputs()
	b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(0.0), EndingSettledCash: new(0.0)}
	k.PurchaseCosts["EUR"] = 0
	commit.ByCurrency["EUR"] = 0
	p := cashSweepFlexProjection(b, nil, s, "EUR", c, k, commit, 0)
	if p.State != "held" || p.EstimatedCash == nil || *p.EstimatedCash != 0 || p.EstimatedFree == nil || *p.EstimatedFree != 0 {
		t.Fatal("explicit broker zero became absent")
	}
	b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(0.0)}
	p = cashSweepFlexProjection(b, nil, s, "EUR", c, k, commit, 0)
	if p.State != "unavailable" || p.EstimatedCash != nil || p.EstimatedFree != nil {
		t.Fatal("missing baseline settled balance became zero")
	}
}

func TestFlexProjectionRejectsNonfiniteOverflowAndWrongScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		change    func(*flexCashBaseline, *brokerStateScope, *cashSweepLedgerRow, *cashSweepSettlement, *cashSweepCommitments, *float64)
		wantState string
		wantCash  bool
	}{
		{"wrong account", func(b *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			b.Scope.Account = "U-OTHER"
		}, "unavailable", false},
		{"wrong mode", func(b *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			b.Scope.Mode = rpc.AccountModePaper
		}, "unavailable", false},
		{"baseline nan", func(b *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(math.NaN()), EndingSettledCash: new(10000.0)}
		}, "unavailable", false},
		{"current infinite", func(_ *flexCashBaseline, _ *brokerStateScope, c *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			c.TradeDate = math.Inf(1)
		}, "held", false},
		{"purchase infinite", func(_ *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			k.PurchaseCosts["EUR"] = math.Inf(1)
		}, "held", false},
		{"sale nan", func(_ *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			k.SaleProceeds["EUR"] = math.NaN()
		}, "held", false},
		{"cash subtraction overflow", func(b *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, k *cashSweepSettlement, _ *cashSweepCommitments, _ *float64) {
			b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(10000.0), EndingSettledCash: new(-math.MaxFloat64)}
			k.PurchaseCosts["EUR"] = math.MaxFloat64
		}, "held", false},
		{"reserve nan", func(_ *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, _ *cashSweepSettlement, _ *cashSweepCommitments, r *float64) {
			*r = math.NaN()
		}, "held", true},
		{"commitment infinite", func(_ *flexCashBaseline, _ *brokerStateScope, _ *cashSweepLedgerRow, _ *cashSweepSettlement, c *cashSweepCommitments, _ *float64) {
			c.ByCurrency["EUR"] = math.Inf(1)
		}, "held", true},
		{"free subtraction overflow", func(b *flexCashBaseline, _ *brokerStateScope, c *cashSweepLedgerRow, k *cashSweepSettlement, commit *cashSweepCommitments, r *float64) {
			b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(10000.0), EndingSettledCash: new(-math.MaxFloat64)}
			c.TradeDate = -math.MaxFloat64
			k.PurchaseCosts["EUR"] = 0
			commit.ByCurrency["EUR"] = math.MaxFloat64
			*r = math.MaxFloat64
		}, "held", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, s, c, k, commit := syntheticFlexProjectionInputs()
			reserve := 1000.0
			tc.change(&b, &s, &c, &k, &commit, &reserve)
			p := cashSweepFlexProjection(b, nil, s, "EUR", c, k, commit, reserve)
			if p.State != tc.wantState || (p.EstimatedCash != nil) != tc.wantCash || p.EstimatedFree != nil || len(p.CoverageGaps) == 0 {
				t.Fatalf("projection=%+v", p)
			}
		})
	}
	b, s, c, k, commit := syntheticFlexProjectionInputs()
	p := cashSweepFlexProjection(b, errors.New("synthetic source unavailable"), s, "EUR", c, k, commit, 1000)
	if p.State != "unavailable" || p.EstimatedCash != nil || p.EstimatedFree != nil {
		t.Fatal("unavailable source admitted estimate")
	}
}

func TestFlexProjectionHiddenWithdrawalOffsetBySaleCannotAdmitSweep(t *testing.T) {
	t.Parallel()
	b, s, c, k, commit := syntheticFlexProjectionInputs()
	// An unobserved withdrawal of 1,000 and an unsettled sale of 1,000 leave
	// trade-date cash unchanged. Matching cash is no completeness witness.
	b.Currencies["EUR"] = flexCashBaselineCurrency{EndingCash: new(10000.0), EndingSettledCash: new(10000.0)}
	c.TradeDate = 10000
	k.PurchaseCosts["EUR"] = 0
	k.SaleProceeds["EUR"] = 1000
	commit.ByCurrency["EUR"] = 0
	p := cashSweepFlexProjection(b, nil, s, "EUR", c, k, commit, 1000)
	if p.State != "held" || p.EstimatedCash == nil || *p.EstimatedCash != 9000 || len(p.CoverageGaps) == 0 {
		t.Fatal("hidden offsetting cash movements certified coverage")
	}
	in := cashSweepTestInput(map[string]float64{"EUR": 10000})
	in.Settlement = cashSweepSettlement{Reason: "complete cash activity coverage is unavailable"}
	in.FlexProjections = map[string]*rpc.CashSweepSettlementProjection{"EUR": p}
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 1e6), in, cashSweepTestNow()), "EUR")
	if cp.status.State != rpc.CashSweepStateSettlementUnknown || cp.side != "" || cp.status.SettledCash != nil || cp.status.Cash != nil || cp.status.Free != nil || in.Ledger["EUR"].Settled != nil {
		t.Fatalf("diagnostic projection supplied cash authority: %+v", cp.status)
	}
	if cp.status.SettlementProjection == nil || cp.status.SettlementProjection.EstimatedFree == nil {
		t.Fatal("held diagnostic estimate not visible separately")
	}
}
