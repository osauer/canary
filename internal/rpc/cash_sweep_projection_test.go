package rpc

import "testing"

func TestCashSweepProjectionCloneIsolatesAllMoneyAndCoverage(t *testing.T) {
	t.Parallel()
	original := &CashSweepSettlementProjection{State: "held", Source: "flex_projection", QueryFingerprint: "synthetic-query", ReportFingerprint: "synthetic-report", BaselineCash: new(100.0), BaselineSettledCash: new(90.0), KnownPurchases: new(10.0), KnownExcludedSales: new(20.0), EstimatedCash: new(80.0), EstimatedFree: new(50.0), CoverageGaps: []string{"intraday cash coverage"}}
	for _, clone := range []*CashSweepSettlementProjection{
		CloneCashSweepSettlementProjection(original),
		CloneCashSweepStatus(&TradeProposalCashSweepStatus{Currencies: []TradeProposalCashSweepCurrency{{Currency: "EUR", SettlementProjection: original}}}).Currencies[0].SettlementProjection,
	} {
		if clone == original || clone.State != original.State || clone.QueryFingerprint != original.QueryFingerprint || clone.ReportFingerprint != original.ReportFingerprint {
			t.Fatal("projection identity clone invalid")
		}
		from := []*float64{original.BaselineCash, original.BaselineSettledCash, original.KnownPurchases, original.KnownExcludedSales, original.EstimatedCash, original.EstimatedFree}
		to := []*float64{clone.BaselineCash, clone.BaselineSettledCash, clone.KnownPurchases, clone.KnownExcludedSales, clone.EstimatedCash, clone.EstimatedFree}
		for i, value := range to {
			if value == nil || value == from[i] || *value != *from[i] {
				t.Fatal("projection money aliases original")
			}
			*value = -1
			if *from[i] == -1 {
				t.Fatal("clone mutation changed original money")
			}
		}
		clone.CoverageGaps[0] = "mutated"
		if original.CoverageGaps[0] == "mutated" {
			t.Fatal("clone coverage aliases original")
		}
	}
	if CloneCashSweepSettlementProjection(nil) != nil {
		t.Fatal("nil projection fabricated")
	}
	absent := CloneCashSweepSettlementProjection(&CashSweepSettlementProjection{State: "unavailable"})
	if absent.BaselineCash != nil || absent.EstimatedFree != nil || absent.CoverageGaps != nil {
		t.Fatal("missing evidence converted to zero or empty data")
	}
	zero := CloneCashSweepSettlementProjection(&CashSweepSettlementProjection{BaselineSettledCash: new(0.0), EstimatedFree: new(0.0)})
	if zero.BaselineSettledCash == nil || *zero.BaselineSettledCash != 0 || zero.EstimatedFree == nil || *zero.EstimatedFree != 0 {
		t.Fatal("explicit zero converted to unavailable")
	}
}
