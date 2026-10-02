package flexstmt

import (
	"os"
	"strings"
	"testing"
)

func lendingFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/lending-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFinancingParsesCustomerNetDailyIdentityAndOptionalSections(t *testing.T) {
	statements, err := Parse([]byte(lendingFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	f := statements[0].Financing
	if !f.LoansPresent || !f.FeesPresent || len(f.Loans) != 1 || len(f.Fees) != 2 || len(f.Activities) != 1 || f.FeeReason != "" || f.LoanReason != "" {
		t.Fatalf("financing=%+v", f)
	}
	if f.Loans[0].Quantity != 60 || *f.Fees[0].NetFee != 2.1 || *f.Fees[0].NetRatePct != 4.2 || f.Fees[0].RecordID == f.Fees[1].RecordID {
		t.Fatal("quantity, net customer fee or daily restatement identity changed")
	}
	if len(statements[0].Trades) != 0 || len(statements[0].Cash) != 0 || len(statements[0].Equity) != 2 {
		t.Fatal("lending became trades/cash or altered ordinary evidence")
	}
	empty := `<FlexQueryResponse><FlexStatements><FlexStatement accountId="DUSYNTHFIN" fromDate="20260930" toDate="20261001" whenGenerated="20261002;020000"><SLBFees/><SLBOpenContracts/></FlexStatement></FlexStatements></FlexQueryResponse>`
	rows, err := Parse([]byte(empty))
	if err != nil || !rows[0].Financing.FeesPresent || len(rows[0].Financing.Fees) != 0 {
		t.Fatal("present empty optional section lost")
	}
	rows, err = Parse([]byte(strings.ReplaceAll(empty, "<SLBFees/>", "")))
	if err != nil || rows[0].Financing.FeesPresent {
		t.Fatal("absent section fabricated as zero")
	}
}

func TestFinancingInvalidRowsDoNotBlockReconOrCertifyFees(t *testing.T) {
	for name, input := range map[string]string{
		"wrong account":      strings.ReplaceAll(lendingFixture(t), `accountId="DUSYNTHFIN" conid=`, `accountId="DUOTHERTEST" conid=`),
		"borrow direction":   strings.ReplaceAll(lendingFixture(t), `type="ManagedLoan"`, `type="DirectBorrow"`),
		"model scope":        strings.ReplaceAll(lendingFixture(t), `type="ManagedLoan"`, `model="MODELTEST" type="ManagedLoan"`),
		"nonfinite amount":   strings.ReplaceAll(lendingFixture(t), `netLendFee="2.10"`, `netLendFee="NaN"`),
		"missing net amount": strings.ReplaceAll(lendingFixture(t), `netLendFee="2.10"`, ``),
		"unsupported row":    strings.ReplaceAll(lendingFixture(t), `<SLBFee `, `<UnknownFee `),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := Parse([]byte(input))
			if err != nil || len(rows[0].Equity) != 2 {
				t.Fatalf("optional financing broke ordinary reporting: %v", err)
			}
			if rows[0].Financing.FeeReason == "" {
				t.Fatal("invalid fee section certified complete")
			}
		})
	}
}

func TestFinancingSignedCorrectionsAndAmountIndependentRestatementIdentity(t *testing.T) {
	first, _ := Parse([]byte(lendingFixture(t)))
	revised, err := Parse([]byte(strings.ReplaceAll(lendingFixture(t), `netLendFee="2.10"`, `netLendFee="-1.10"`)))
	if err != nil || *revised[0].Financing.Fees[0].NetFee != -1.1 || first[0].Financing.Fees[0].RecordID != revised[0].Financing.Fees[0].RecordID {
		t.Fatal("correction sign or stable daily identity lost")
	}
}
