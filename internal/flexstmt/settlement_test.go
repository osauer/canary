package flexstmt

import (
	"strings"
	"testing"
)

const syntheticCashBalanceRow = `<CashReportCurrency accountId="U-SYNTHETIC" currency="EUR" fromDate="20260930" toDate="20260930" reportDate="20260930" endingCash="12000" endingSettledCash="11000"/>`

func syntheticCashBalanceStatement(report string) string {
	return `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U-SYNTHETIC" fromDate="20260930" toDate="20260930" whenGenerated="20261001;010000">` + report + `<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20260930" total="25000"/></EquitySummaryInBase></FlexStatement></FlexStatements></FlexQueryResponse>`
}

func TestSettlementCashBalancesExactNativeEvidence(t *testing.T) {
	t.Parallel()
	base := `<CashReportCurrency accountId="U-SYNTHETIC" currency="BASE_SUMMARY" endingCash="999999" endingSettledCash="999999"/>`
	usd := strings.ReplaceAll(strings.ReplaceAll(syntheticCashBalanceRow, `currency="EUR"`, `currency="USD"`), `endingCash="12000" endingSettledCash="11000"`, `endingCash="0" endingSettledCash="0"`)
	statements, err := Parse([]byte(syntheticCashBalanceStatement("<CashReport>" + base + syntheticCashBalanceRow + usd + "</CashReport>")))
	if err != nil {
		t.Fatal(err)
	}
	st := statements[0]
	if st.CashBalanceError != "" || len(st.CashBalances) != 2 || st.CashBalances[0].EndingSettledCash == nil || *st.CashBalances[0].EndingSettledCash != 11000 || st.CashBalances[1].EndingSettledCash == nil || *st.CashBalances[1].EndingSettledCash != 0 {
		t.Fatalf("settlement=%+v error=%s", st.CashBalances, st.CashBalanceError)
	}
	if st.CashBalances[0].AccountID != st.AccountID || !st.CashBalances[0].ReportDate.Equal(st.ToDate) {
		t.Fatal("original account/report scope lost")
	}
	if !st.SettlementCoverage.Present || st.SettlementCoverage.RowCount != 3 {
		t.Fatalf("coverage=%+v", st.SettlementCoverage)
	}
	if SettlementRequirementEvidence(statements)[0].Status != QueryRequirementObserved {
		t.Fatal("cash section columns not observed")
	}
}

func TestSettlementAuthorityRejectionPreservesReporting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		report string
	}{
		{"absent", ""},
		{"empty", "<CashReport/>"},
		{"foreign container", `<CashReport accountId="U-OTHER">` + syntheticCashBalanceRow + `</CashReport>`},
		{"model container", `<CashReport model="MODEL-A">` + syntheticCashBalanceRow + `</CashReport>`},
		{"segment container", `<CashReport segment="Securities">` + syntheticCashBalanceRow + `</CashReport>`},
		{"aggregate only", `<CashReport><CashReportCurrency currency="BASE_SUMMARY" endingCash="100000" endingSettledCash="100000"/></CashReport>`},
		{"missing settled", strings.ReplaceAll(syntheticCashBalanceRow, ` endingSettledCash="11000"`, "")},
		{"missing cash", strings.ReplaceAll(syntheticCashBalanceRow, ` endingCash="12000"`, "")},
		{"nan", strings.ReplaceAll(syntheticCashBalanceRow, `endingSettledCash="11000"`, `endingSettledCash="NaN"`)},
		{"infinite", strings.ReplaceAll(syntheticCashBalanceRow, `endingCash="12000"`, `endingCash="Inf"`)},
		{"underflow", strings.ReplaceAll(syntheticCashBalanceRow, `endingCash="12000"`, `endingCash="1e-324"`)},
		{"huge exponent", strings.ReplaceAll(syntheticCashBalanceRow, `endingCash="12000"`, `endingCash="0e-999999999"`)},
		{"hexadecimal amount", strings.ReplaceAll(syntheticCashBalanceRow, `endingCash="12000"`, `endingCash="0x1p2"`)},
		{"foreign account", strings.ReplaceAll(syntheticCashBalanceRow, `accountId="U-SYNTHETIC"`, `accountId="U-OTHER"`)},
		{"missing account", strings.ReplaceAll(syntheticCashBalanceRow, ` accountId="U-SYNTHETIC"`, "")},
		{"missing date", strings.ReplaceAll(syntheticCashBalanceRow, ` reportDate="20260930"`, "")},
		{"different date", strings.ReplaceAll(syntheticCashBalanceRow, `reportDate="20260930"`, `reportDate="20260929"`)},
		{"intraday period", strings.ReplaceAll(syntheticCashBalanceRow, `fromDate="20260930"`, `fromDate="20260930;010000"`)},
		{"lowercase currency", strings.ReplaceAll(syntheticCashBalanceRow, `currency="EUR"`, `currency="eur"`)},
		{"model", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` model="MODEL-A"/>`)},
		{"segment", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` accountSegment="Securities"/>`)},
		{"duplicate currency", syntheticCashBalanceRow + syntheticCashBalanceRow},
		{"duplicate attribute", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` endingSettledCash="11000"/>`)},
		{"case alias", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` EndingSettledCash="11000"/>`)},
		{"namespace", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` xmlns="urn:other"/>`)},
		{"segment mismatch", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` endingCashSec="11000" endingCashCom="0" endingSettledCashSec="11000" endingSettledCashCom="0"/>`)},
		{"partial segments", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` endingCashSec="12000" endingSettledCashSec="11000"/>`)},
		{"unknown segment", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` endingSettledCashOther="0"/>`)},
		{"segment case alias", strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` EndingSettledCashSec="999999"/>`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := tc.report
			if report != "" && !strings.HasPrefix(report, "<CashReport") {
				report = "<CashReport>" + report + "</CashReport>"
			}
			statements, err := Parse([]byte(syntheticCashBalanceStatement(report)))
			if err != nil {
				t.Fatalf("optional settlement section broke reporting: %v", err)
			}
			st := statements[0]
			if st.CashBalanceError == "" || len(st.CashBalances) != 0 {
				t.Fatalf("unsafe balance accepted: %+v / %s", st.CashBalances, st.CashBalanceError)
			}
			if len(st.Equity) != 1 || st.Equity[0].TotalBase != 25000 || len(st.Coverage) != len(CanonicalQueryManifest()) {
				t.Fatal("canonical reporting evidence changed")
			}
		})
	}
}

func TestSettlementCompleteSegmentsAndNegativeBalances(t *testing.T) {
	t.Parallel()
	row := strings.ReplaceAll(syntheticCashBalanceRow, "/>", ` endingCashSec="12000" endingCashCom="0" endingSettledCashSec="11000" endingSettledCashCom="0"/>`)
	for _, r := range []string{row, strings.ReplaceAll(strings.ReplaceAll(syntheticCashBalanceRow, `endingCash="12000"`, `endingCash="-100"`), `endingSettledCash="11000"`, `endingSettledCash="-200"`)} {
		sts, err := Parse([]byte(syntheticCashBalanceStatement("<CashReport>" + r + "</CashReport>")))
		if err != nil || sts[0].CashBalanceError != "" || len(sts[0].CashBalances) != 1 {
			t.Fatalf("valid broker cash refused: %v / %+v", err, sts)
		}
	}
}

func TestSettlementOptionalManifestDefensiveAndIndependent(t *testing.T) {
	t.Parallel()
	manifest := SettlementQueryManifest()
	manifest[0].RequiredFields[0] = "mutated"
	if SettlementQueryManifest()[0].RequiredFields[0] != "accountId" {
		t.Fatal("optional manifest authority mutable")
	}
	for _, section := range CanonicalQueryManifest() {
		if section.Key == "cash_report" {
			t.Fatal("optional cash section became canonical reporting requirement")
		}
	}
	statements, err := Parse([]byte(syntheticCashBalanceStatement("")))
	if err != nil {
		t.Fatal(err)
	}
	if SettlementRequirementEvidence(statements)[0].Status != QueryRequirementAbsent || SettlementRequirementEvidence(nil)[0].Status != QueryRequirementNotReceived {
		t.Fatal("missing section confused with empty or zero balances")
	}
	withCash, err := Parse([]byte(syntheticCashBalanceStatement("<CashReport>" + syntheticCashBalanceRow + "</CashReport>")))
	if err != nil {
		t.Fatal(err)
	}
	if QuerySchemaFingerprint(statements) != QuerySchemaFingerprint(withCash) {
		t.Fatal("optional cash section changed canonical reporting schema identity")
	}
}
