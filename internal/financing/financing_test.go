package financing

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func date(v string) time.Time { d, _ := time.Parse(time.DateOnly, v); return d }

func fixture(t *testing.T) []flexstmt.Statement {
	t.Helper()
	data, err := os.ReadFile("../flexstmt/testdata/lending-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := flexstmt.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func calculate(rows []flexstmt.Statement) Result {
	return Calculate(rows, "synthetic-scope", date("2026-09-29"), date("2026-10-01"), "EUR")
}

func TestCustomerNetAttributionBoundariesUnitsAndPrivacy(t *testing.T) {
	result := calculate(fixture(t))
	if result.Summary.State != rpc.FinancingComplete || result.Summary.EarnedBase == nil || math.Abs(*result.Summary.EarnedBase-3.78) > 1e-12 || result.Summary.CoveredDays != 2 || len(result.Fees) != 2 {
		t.Fatalf("attribution=%+v", result)
	}
	if result.Summary.Native[0].Amount != 4.2 || *result.Fees[0].NetFee != 2.1 || *result.Fees[0].FXRateToBase != .9 || result.Summary.PNLReconciliation != "unproved" {
		t.Fatal("net amounts, conversion or evidence boundary changed")
	}
	page := rpc.FinancingFeesResult{Summary: result.Summary, Fees: result.Fees, FilteredCount: 2}
	if err := rpc.ValidateFinancingFeesResult(page); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page)
	for _, private := range []string{"DUSYNTHFIN", "synthetic-loan-1", "SLBFee", "grossLendFee"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private evidence leaked: %s", private)
		}
	}
	narrow := Calculate(fixture(t), "synthetic-scope", date("2026-09-30"), date("2026-10-01"), "EUR")
	if len(narrow.Fees) != 1 || math.Abs(*narrow.Summary.EarnedBase-1.89) > 1e-12 {
		t.Fatal("opening boundary included twice")
	}
}

func TestOptionalMissingPartialEmptyAndFXRemainDistinct(t *testing.T) {
	rows := fixture(t)
	rows[0].Financing.FeesPresent = false
	missing := calculate(rows)
	if missing.Summary.State != rpc.FinancingUnavailable || missing.Summary.EarnedBase != nil || len(missing.Fees) != 0 {
		t.Fatal("missing sections fabricated zero")
	}
	rows = fixture(t)
	rows[0].Financing.Fees = nil
	empty := calculate(rows)
	if empty.Summary.State != rpc.FinancingComplete || empty.Summary.EarnedBase == nil || *empty.Summary.EarnedBase != 0 {
		t.Fatal("complete empty reporting withheld zero")
	}
	rows = fixture(t)
	rows[0].FromDate = date("2026-10-01")
	rows[0].Financing.Fees = rows[0].Financing.Fees[1:]
	partial := calculate(rows)
	if partial.Summary.State != rpc.FinancingPartial || partial.Summary.EarnedBase != nil || partial.Summary.KnownEarnedBase == nil || partial.Summary.CoveredDays != 1 {
		t.Fatal("partial range certified total")
	}
	rows = fixture(t)
	rows[0].Financing.Fees[0].FXRateToBase = nil
	noFX := calculate(rows)
	if noFX.Summary.EarnedBase != nil || noFX.Summary.KnownEarnedBase != nil || noFX.Summary.Reason != "base_conversion_unavailable" || noFX.Summary.Native[0].Amount != 4.2 {
		t.Fatal("missing FX zero-filled or native income lost")
	}
	rows = fixture(t)
	rows[0].Financing.Fees[0].FXBaseCurrency = "GBP"
	wrongBase := calculate(rows)
	if wrongBase.Summary.EarnedBase != nil || wrongBase.Summary.Native[0].Amount != 4.2 {
		t.Fatal("historical broker FX applied to a different account base")
	}
	rows = fixture(t)
	rows[0].Financing.Fees[0].NetFee = nil
	rows[0].Financing.FeeReason = "net_fee_missing"
	noAmount := calculate(rows)
	if noAmount.Summary.State != rpc.FinancingPartial || noAmount.Summary.EarnedBase != nil {
		t.Fatal("missing customer fee certified total")
	}
}

func TestRestatementsDuplicatesAndEmptyReplacement(t *testing.T) {
	rows := fixture(t)
	duplicated := calculate(append(rows, rows...))
	if len(duplicated.Fees) != 2 || *duplicated.Summary.EarnedBase != *calculate(rows).Summary.EarnedBase {
		t.Fatal("duplicate import doubled income")
	}
	revised := fixture(t)[0]
	revised.WhenGenerated = revised.WhenGenerated.Add(time.Hour)
	revised.Financing.Fees[0].NetFee = new(-1.1)
	combined := calculate(append(rows, revised))
	if len(combined.Fees) != 2 || math.Abs(*combined.Summary.EarnedBase-.9) > 1e-12 {
		t.Fatal("signed restatement did not replace old row")
	}
	revised.Financing.Fees = nil
	cleared := calculate(append(rows, revised))
	if len(cleared.Fees) != 0 || *cleared.Summary.EarnedBase != 0 {
		t.Fatal("newer explicit empty fees resurrected old income")
	}
}

func TestDatedLoanAnchorNetRateAndClosedSnapshot(t *testing.T) {
	rows := fixture(t)
	loans := LoanAnnotations(rows, date("2026-10-01"))
	loan, ok := loans[900901]
	if !ok || loan.Quantity != 60 || loan.OwnedQuantity == nil || *loan.OwnedQuantity != 100 || loan.NetRatePct == nil || *loan.NetRatePct != 4.2 || *loan.Collateral != 18000 {
		t.Fatalf("loan=%+v", loan)
	}
	if LoanAnnotations(rows, date("2026-10-02"))[900901].State != "stale" {
		t.Fatal("loan date refreshed by rendering")
	}
	rows[0].Positions[0].Quantity = new(20.0)
	if len(LoanAnnotations(rows, date("2026-10-01"))) != 0 {
		t.Fatal("loan clamped to a mismatched holding")
	}
	rows = fixture(t)
	closed := fixture(t)[0]
	closed.ToDate = date("2026-10-02")
	closed.WhenGenerated = closed.WhenGenerated.AddDate(0, 0, 1)
	closed.Financing.Loans = nil
	closed.Positions = nil
	if len(LoanAnnotations(append(rows, closed), date("2026-10-02"))) != 0 || len(calculate(rows).Fees) != 2 {
		t.Fatal("empty loan snapshot failed to clear or historical fees disappeared")
	}
}
