package stress

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Bond risk phase 1: held bonds add a "Rates and credit" row that only
// observes, and the equity exposure row names its gross without bonds while
// its verdict stays as before. Without bonds neither appears.
func TestStressShowsBondRiskWithoutJudgingIt(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	acct := rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 200000, GrossPositionValue: 300000}
	book := risk.SummarizeBondBook([]risk.BondBookLine{
		{Symbol: "SYNT", Currency: "USD", Issuer: "United States Treasury", IssuerClass: risk.BondIssuerGovernment, MarketValueBase: new(40000.0), RateShockLossBase: new(2300.0)},
		{Symbol: "SYNC", Currency: "EUR", Issuer: "Synthetic Corp", IssuerClass: risk.BondIssuerInvestmentGrade, MarketValueBase: new(10000.0), Unmeasured: "no evidence"},
	}, new(200000.0), "EUR")
	p := summarizeStressPortfolio(acct, rpc.PositionsResult{BondRisk: book}, rpc.MarketEventsResult{}, now)
	if p.BondRisk == nil || p.GrossExcludingBondsPctNLV == nil || *p.GrossExcludingBondsPctNLV != 125 {
		t.Fatalf("summary = %+v", p)
	}
	row, ok := stressRatesCreditRow(p)
	if !ok || row.Title != "Rates and credit" || row.Severity != risk.SeverityObserve ||
		!strings.Contains(row.Evidence, "bonds 25.0% NLV; a one-point rise in yields costs 1.1% NLV; non-government 5.0% NLV; largest issuer United States Treasury 20.0% NLV; 1 bond line(s) not in the sums") {
		t.Fatalf("row = %+v", row)
	}
	if exposure := stressExposureRow(p, StressMarketSummary{}); !strings.Contains(exposure.Evidence, "gross without bonds 125% NLV") {
		t.Fatalf("exposure evidence = %q", exposure.Evidence)
	}
	plain := summarizeStressPortfolio(acct, rpc.PositionsResult{}, rpc.MarketEventsResult{}, now)
	if _, ok := stressRatesCreditRow(plain); ok || plain.GrossExcludingBondsPctNLV != nil || strings.Contains(stressExposureRow(plain, StressMarketSummary{}).Evidence, "without bonds") {
		t.Fatal("a book without bonds shows bond risk")
	}
}
