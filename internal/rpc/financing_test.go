package rpc

import (
	"math"
	"strings"
	"testing"
	"time"
)

func financingTestResult() FinancingFeesResult {
	from := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	return FinancingFeesResult{Summary: FinancingSummary{SchemaVersion: FinancingSchemaVersion, State: FinancingComplete, From: from, To: from.AddDate(0, 0, 2), BaseCurrency: "EUR", EarnedBase: new(3.78), KnownEarnedBase: new(3.78), Native: []FinancingCurrencyAmount{{Currency: "USD", Amount: 4.2}}, FeeCount: 2, CoveredDays: 2, ExpectedDays: 2, PNLReconciliation: "unproved", PaymentLinkage: "unavailable", Fingerprint: "finance_" + strings.Repeat("a", 32)}, Fees: []FinancingFee{{ID: "fee_" + strings.Repeat("a", 32), ConID: 900901, Symbol: "SYNTH-LEND", ValueDate: from.AddDate(0, 0, 2), Currency: "USD", NetFee: new(2.1), FXRateToBase: new(.9), BaseAmount: new(1.89)}}, FilteredCount: 2}
}

func TestFinancingBoundsAndMissingAwarePublication(t *testing.T) {
	t.Parallel()
	if got, err := NormalizeFinancingFeesParams(FinancingFeesParams{}); err != nil || got.Window != "365d" || got.Limit != 25 {
		t.Fatal("shared defaults", err)
	}
	for _, p := range []FinancingFeesParams{{Limit: 101}, {ConID: -1}, {Window: "all"}, {From: "2026-09-30"}, {From: "2026-02-30", To: "2026-10-01"}, {From: "2024-01-01", To: "2026-10-01"}, {Fingerprint: "private-reference"}, {Cursor: strings.Repeat("x", 257)}} {
		if _, err := NormalizeFinancingFeesParams(p); err == nil {
			t.Fatalf("unbounded/ambiguous parameters accepted: %+v", p)
		}
	}
	if err := ValidateFinancingFeesResult(financingTestResult()); err != nil {
		t.Fatal(err)
	}
	for _, params := range []FinancingFeesParams{{ConID: 900901}, {From: "2026-09-30", To: "2026-10-01"}, {Fingerprint: "finance_" + strings.Repeat("b", 32)}} {
		if err := ValidateFinancingFeesResponse(financingTestResult(), params); err == nil {
			t.Fatal("another requested read was accepted")
		}
	}
	for name, mutate := range map[string]func(*FinancingFeesResult){
		"nonfinite fee":                      func(r *FinancingFeesResult) { r.Fees[0].NetFee = new(math.NaN()) },
		"opening fee":                        func(r *FinancingFeesResult) { r.Fees[0].ValueDate = r.Summary.From },
		"wrong contract":                     func(r *FinancingFeesResult) { r.ConID = 900902 },
		"wrong FX arithmetic":                func(r *FinancingFeesResult) { r.Fees[0].BaseAmount = new(100.0) },
		"partial promoted to complete total": func(r *FinancingFeesResult) { r.Summary.State = FinancingPartial },
		"invented PnL link":                  func(r *FinancingFeesResult) { r.Summary.PNLReconciliation = "included" },
		"invented payment":                   func(r *FinancingFeesResult) { r.Summary.PaymentLinkage = "paid" },
		"duplicate rows":                     func(r *FinancingFeesResult) { r.Fees = append(r.Fees, r.Fees[0]) },
		"wrong expected coverage":            func(r *FinancingFeesResult) { r.Summary.ExpectedDays = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			r := financingTestResult()
			mutate(&r)
			if err := ValidateFinancingFeesResult(r); err == nil {
				t.Fatal("invalid financing publication accepted")
			}
		})
	}
	zero := financingTestResult()
	zero.Summary.EarnedBase = new(0.0)
	zero.Summary.KnownEarnedBase = new(0.0)
	zero.Summary.FeeCount = 0
	zero.Summary.Native = nil
	zero.Fees = nil
	zero.FilteredCount = 0
	if err := ValidateFinancingFeesResult(zero); err != nil {
		t.Fatal("confirmed empty became missing", err)
	}
	missingFX := financingTestResult()
	missingFX.Summary.EarnedBase = nil
	missingFX.Summary.KnownEarnedBase = nil
	missingFX.Fees[0].BaseAmount = nil
	missingFX.Fees[0].FXRateToBase = nil
	if err := ValidateFinancingFeesResult(missingFX); err != nil {
		t.Fatal("native fees lost without FX", err)
	}
}
