package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func performanceDay(day string) time.Time {
	t, _ := time.Parse("2006-01-02", day)
	return t
}

func performanceFloat(v float64) *float64 { return &v }

// Two overlapping generations of the same account: the newer restates one
// equity day and repeats every line. The series must keep one value per day,
// count each flow and trade once, sign transfers out, and exclude the previous
// year and closed-lot restatements from the year-to-date sums.
func TestReportingPerformanceMergesRestatementsAndCountsEachLineOnce(t *testing.T) {
	older := flexstmt.Statement{
		AccountID: "DUPERF1", FromDate: performanceDay("2025-12-29"), ToDate: performanceDay("2026-01-06"), WhenGenerated: performanceDay("2026-01-07"),
		FXRates: []flexstmt.FXRate{{FromCurrency: "USD", ToCurrency: "EUR", Rate: performanceFloat(0.9)}},
		Equity: []flexstmt.EquityRow{
			{ReportDate: performanceDay("2025-12-31"), TotalBase: 100000},
			{ReportDate: performanceDay("2026-01-02"), TotalBase: 101000},
			{ReportDate: performanceDay("2026-01-05"), TotalBase: 99000},
		},
		Cash: []flexstmt.CashLine{
			{ID: "flow-1", Category: flexstmt.CategoryFlow, Type: "Deposits/Withdrawals", AmountBase: performanceFloat(5000), ValueDate: performanceDay("2026-01-05")},
			{ID: "div-old", Category: flexstmt.CategoryClassified, Type: "Dividends", AmountBase: performanceFloat(80), ValueDate: performanceDay("2025-12-30")},
			{ID: "div-1", Category: flexstmt.CategoryClassified, Type: "Dividends", AmountBase: performanceFloat(120), ValueDate: performanceDay("2026-01-05")},
			{ID: "wht-1", Category: flexstmt.CategoryClassified, Type: "Withholding Tax", AmountBase: performanceFloat(-18), ValueDate: performanceDay("2026-01-05")},
			{ID: "odd-1", Category: flexstmt.CategoryUncategorized, Type: "Mystery", AmountBase: performanceFloat(-7), ValueDate: performanceDay("2026-01-05")},
		},
		Transfers: []flexstmt.Transfer{
			{ID: "xfer-1", Direction: "OUT", AmountBase: performanceFloat(2000), Date: performanceDay("2026-01-06")},
		},
		Trades: []flexstmt.Trade{
			{RecordID: "t-old", Currency: "EUR", ReportDate: performanceDay("2025-12-30"), RealizedPNL: performanceFloat(999), LevelOfDetail: "EXECUTION"},
			{RecordID: "t-1", Currency: "USD", FXRateToBase: performanceFloat(0.9), ReportDate: performanceDay("2026-01-02"), RealizedPNL: performanceFloat(200), Commission: performanceFloat(-2), LevelOfDetail: "EXECUTION"},
			{RecordID: "t-1-lot", Currency: "USD", FXRateToBase: performanceFloat(0.9), ReportDate: performanceDay("2026-01-02"), RealizedPNL: performanceFloat(200), LevelOfDetail: "CLOSED_LOT"},
			{RecordID: "t-2", Currency: "EUR", ReportDate: performanceDay("2026-01-05"), RealizedPNL: performanceFloat(0), Commission: performanceFloat(-1.5), LevelOfDetail: "EXECUTION"},
			{RecordID: "t-3", Currency: "CHF", ReportDate: performanceDay("2026-01-05"), RealizedPNL: performanceFloat(50), LevelOfDetail: "EXECUTION"},
		},
	}
	newer := older
	newer.FromDate, newer.ToDate, newer.WhenGenerated = performanceDay("2026-01-05"), performanceDay("2026-01-07"), performanceDay("2026-01-08")
	newer.Equity = []flexstmt.EquityRow{
		{ReportDate: performanceDay("2026-01-05"), TotalBase: 99500},
		{ReportDate: performanceDay("2026-01-07"), TotalBase: 102000},
	}
	now := time.Date(2026, 1, 8, 9, 0, 0, 0, time.UTC)
	result := buildReportingPerformance([]flexstmt.Statement{older, newer}, now)
	if err := rpc.ValidateReportingPerformanceResult(result); err != nil {
		t.Fatalf("invalid result: %v", err)
	}
	if result.BaseCurrency != "EUR" || result.Evidence.State != rpc.ReportingEvidenceObserved || result.Evidence.CoverageFrom != "2025-12-29" || result.Evidence.CoverageTo != "2026-01-07" || result.Evidence.EquityDays != 4 || result.Evidence.UnclassifiedLines != 1 {
		t.Fatalf("evidence misreported: %+v", result.Evidence)
	}
	wantDays := []rpc.PerformanceDay{{Day: "2025-12-31", EquityBase: 100000}, {Day: "2026-01-02", EquityBase: 101000}, {Day: "2026-01-05", EquityBase: 99500}, {Day: "2026-01-07", EquityBase: 102000}}
	if len(result.Days) != len(wantDays) {
		t.Fatalf("days = %+v", result.Days)
	}
	for i, day := range wantDays {
		if result.Days[i] != day {
			t.Fatalf("day %d = %+v, want %+v (newest generation must win a restated day)", i, result.Days[i], day)
		}
	}
	wantFlows := []rpc.PerformanceFlow{{Day: "2026-01-05", Type: rpc.PerformanceFlowDeposit, AmountBase: 5000}, {Day: "2026-01-06", Type: rpc.PerformanceFlowTransferOut, AmountBase: -2000}}
	if len(result.Flows) != len(wantFlows) {
		t.Fatalf("flows counted twice or lost: %+v", result.Flows)
	}
	for i, flow := range wantFlows {
		if result.Flows[i] != flow {
			t.Fatalf("flow %d = %+v, want %+v", i, result.Flows[i], flow)
		}
	}
	ytd := result.YearToDate
	if ytd == nil || ytd.From != "2026-01-01" || ytd.Through != "2026-01-07" {
		t.Fatalf("year-to-date window = %+v", ytd)
	}
	if ytd.RealisedBase != 180 || ytd.Closes != 1 || ytd.UnconvertedTrades != 1 {
		t.Fatalf("realised must convert USD at the statement rate, count one close, skip the closed lot and the previous year, and report the CHF row as unconverted: %+v", ytd)
	}
	if ytd.CommissionsBase != -1.8-1.5 || ytd.DividendsBase != 120 || ytd.WithholdingTaxBase != -18 || ytd.OtherBase != 0 {
		t.Fatalf("income sums wrong: %+v", ytd)
	}
}

func TestReportingPerformanceWithoutStatementsOrScopeReportsNoSeries(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	empty := buildReportingPerformance(nil, now)
	if err := rpc.ValidateReportingPerformanceResult(empty); err != nil {
		t.Fatal(err)
	}
	if empty.Evidence.State != rpc.ReportingEvidenceNotReceived || len(empty.Days) != 0 || len(empty.Flows) != 0 || empty.YearToDate != nil {
		t.Fatalf("no statements must read as not received, never as a flat series: %+v", empty)
	}
	// A daemon without one selected broker account must not fold sibling
	// accounts into one series.
	srv := &Server{now: func() time.Time { return now }}
	result, err := srv.handleReportingPerformance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.State != rpc.ReportingEvidenceDegraded || result.Evidence.Reason != rpc.ReconReportReasonAuthorityUnavailable || len(result.Days) != 0 {
		t.Fatalf("unscoped read must be degraded with its reason: %+v", result.Evidence)
	}
}

func TestReportingPerformanceValidationRejectsMisorderedOrUnknownRows(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	good := emptyReportingPerformance(now)
	good.Evidence.State = rpc.ReportingEvidenceObserved
	good.Days = []rpc.PerformanceDay{{Day: "2026-09-16", EquityBase: 1}, {Day: "2026-09-17", EquityBase: 2}}
	good.Flows = []rpc.PerformanceFlow{{Day: "2026-09-17", Type: rpc.PerformanceFlowDeposit, AmountBase: 10}}
	if err := rpc.ValidateReportingPerformanceResult(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*rpc.ReportingPerformanceResult){
		"descending days": func(r *rpc.ReportingPerformanceResult) { r.Days[0], r.Days[1] = r.Days[1], r.Days[0] },
		"duplicate day":   func(r *rpc.ReportingPerformanceResult) { r.Days[1].Day = r.Days[0].Day },
		"unknown flow":    func(r *rpc.ReportingPerformanceResult) { r.Flows[0].Type = "gift" },
		"zero flow":       func(r *rpc.ReportingPerformanceResult) { r.Flows[0].AmountBase = 0 },
		"unknown state":   func(r *rpc.ReportingPerformanceResult) { r.Evidence.State = "guessed" },
		"nil series":      func(r *rpc.ReportingPerformanceResult) { r.Days = nil },
	} {
		bad := good
		bad.Days = append([]rpc.PerformanceDay(nil), good.Days...)
		bad.Flows = append([]rpc.PerformanceFlow(nil), good.Flows...)
		mutate(&bad)
		if err := rpc.ValidateReportingPerformanceResult(bad); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}
