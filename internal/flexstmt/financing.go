package flexstmt

import (
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// LendingLoan is a dated managed-loan balance. Quantity is positive shares
// lent; broker loan identifiers remain private projection evidence.
type LendingLoan struct {
	RecordID, AccountID, LoanID, Symbol, Currency string
	ConID                                         int64
	AsOf                                          time.Time
	Quantity                                      float64
	Collateral, FeeRatePct                        *float64
}

// LendingActivity is a managed-loan change, never a trade or external flow.
type LendingActivity struct {
	RecordID, AccountID, LoanID, Symbol, Currency string
	ConID                                         int64
	Date                                          time.Time
	Quantity                                      *float64
}

// LendingFee is a customer net earned fee. Signed corrections are retained;
// NetFee is not gross income and must never be halved again.
type LendingFee struct {
	RecordID, AccountID, LoanID, Symbol, Currency string
	ConID                                         int64
	ValueDate, StartDate                          time.Time
	Quantity, NetFee, NetRatePct, Collateral      *float64
	FXRateToBase                                  *float64
	FXBaseCurrency                                string
}

// FinancingStatement preserves optional section presence, valid rows and
// closed diagnostic codes. A missing/invalid section cannot certify zero.
type FinancingStatement struct {
	LoansPresent, ActivitiesPresent, FeesPresent bool
	LoanReason, ActivityReason, FeeReason        string
	Loans                                        []LendingLoan
	Activities                                   []LendingActivity
	Fees                                         []LendingFee
}

type financingSection struct {
	Rows []financingNode `xml:",any"`
}
type financingNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
}

func (n financingNode) text(key string) string { return (fxNode{Attrs: n.Attrs}).text(key) }

func parseFinancing(ctx context.Context, data []byte, statements []Statement) {
	var doc struct {
		Statements []struct {
			Loans      *financingSection `xml:"SLBOpenContracts"`
			Activities *financingSection `xml:"SLBActivities"`
			Fees       *financingSection `xml:"SLBFees"`
			Equity     *financingSection `xml:"EquitySummaryInBase"`
		} `xml:"FlexStatements>FlexStatement"`
	}
	if unmarshalContext(ctx, data, &doc) != nil || len(doc.Statements) != len(statements) {
		return
	}
	for i, raw := range doc.Statements {
		st := &statements[i]
		f := &FinancingStatement{LoansPresent: raw.Loans != nil, ActivitiesPresent: raw.Activities != nil, FeesPresent: raw.Fees != nil}
		st.Financing = f
		if raw.Loans != nil {
			seen := map[string]bool{}
			for _, row := range raw.Loans.Rows {
				loan, err := parseLendingLoan(row, *st)
				if err != nil || seen[loan.RecordID] {
					f.LoanReason = "invalid_loan_records"
					continue
				}
				seen[loan.RecordID] = true
				f.Loans = append(f.Loans, loan)
			}
		}
		if raw.Activities != nil {
			seen := map[string]bool{}
			for _, row := range raw.Activities.Rows {
				activity, err := parseLendingActivity(row, *st)
				if err != nil || seen[activity.RecordID] {
					f.ActivityReason = "invalid_activity_records"
					continue
				}
				seen[activity.RecordID] = true
				f.Activities = append(f.Activities, activity)
			}
		}
		if raw.Fees != nil {
			base := lendingBaseCurrency(raw.Equity, st.ToDate)
			seen := map[string]bool{}
			for _, row := range raw.Fees.Rows {
				fee, err := parseLendingFee(row, *st)
				if err != nil || seen[fee.RecordID] {
					f.FeeReason = "invalid_fee_records"
					continue
				}
				seen[fee.RecordID] = true
				fee.FXBaseCurrency = base
				f.Fees = append(f.Fees, fee)
				if fee.NetFee == nil {
					f.FeeReason = "net_fee_missing"
				}
			}
		}
	}
}

// A conversion factor's target currency must be vouched by the same statement,
// rather than applying a historical broker factor to today's account base.
func lendingBaseCurrency(section *financingSection, to time.Time) string {
	if section == nil {
		return ""
	}
	base := ""
	for _, row := range section.Rows {
		if row.XMLName.Local != "EquitySummaryByReportDateInBase" {
			continue
		}
		day, err := parseFlexDate(row.text("reportDate"))
		if err != nil || !day.Equal(to) {
			continue
		}
		unit := strings.ToUpper(row.text("currency"))
		if !lendingCurrency(unit) || base != "" && base != unit {
			return ""
		}
		base = unit
	}
	return base
}

func lendingIdentity(row financingNode, st Statement, tag string) (int64, string, string, error) {
	if row.XMLName.Local != tag || row.text("type") != "ManagedLoan" ||
		row.text("accountId") != st.AccountID || row.text("model") != "" || row.text("assetCategory") != "STK" {
		return 0, "", "", fmt.Errorf("unsupported lending identity")
	}
	conID, err := strconv.ParseInt(row.text("conid"), 10, 64)
	currency := strings.ToUpper(row.text("currency"))
	if err != nil || conID <= 0 || !lendingCurrency(currency) || row.text("symbol") == "" || len(row.text("symbol")) > 80 {
		return 0, "", "", fmt.Errorf("missing lending identity")
	}
	id := firstNonEmpty(row.text("slbTransactionId"), row.text("transactionID"), row.text("uniqueID"))
	// Fee reports may omit the loan ID: account/contract/value-date/currency
	// remains a stable restatement identity, rather than hashing the amount.
	if tag != "SLBFee" && id == "" {
		return 0, "", "", fmt.Errorf("missing loan identity")
	}
	return conID, currency, id, nil
}

func lendingCurrency(v string) bool {
	if len(v) != 3 {
		return false
	}
	for _, c := range v {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func lendingNumber(row financingNode, key string, nonnegative bool) (*float64, error) {
	v, err := optionalFloat(key, row.text(key))
	if err != nil || v != nil && nonnegative && *v < 0 {
		return nil, fmt.Errorf("invalid lending number")
	}
	return v, nil
}

func lendingRecordID(kind, account, loan string, conID int64, day time.Time, currency string) string {
	return brokerRecordID(kind, "", account, loan, strconv.FormatInt(conID, 10), day.Format(time.DateOnly), currency, "ManagedLoan")
}

func parseLendingLoan(row financingNode, st Statement) (LendingLoan, error) {
	var loan LendingLoan
	conID, currency, id, err := lendingIdentity(row, st, "SLBOpenContract")
	if err != nil {
		return loan, err
	}
	day, err := parseFlexDate(row.text("date"))
	quantity, qerr := lendingNumber(row, "quantity", false)
	if err != nil || !day.Equal(st.ToDate) || qerr != nil || quantity == nil || *quantity >= 0 {
		return loan, fmt.Errorf("invalid managed-loan balance")
	}
	loan = LendingLoan{AccountID: st.AccountID, LoanID: id, Symbol: row.text("symbol"), Currency: currency,
		ConID: conID, AsOf: day, Quantity: -*quantity,
		RecordID: lendingRecordID("loan", st.AccountID, id, conID, day, currency)}
	if loan.Collateral, err = lendingNumber(row, "collateralAmount", true); err != nil {
		return LendingLoan{}, err
	}
	loan.FeeRatePct, err = lendingNumber(row, "feeRate", true)
	return loan, err
}

func parseLendingActivity(row financingNode, st Statement) (LendingActivity, error) {
	var activity LendingActivity
	conID, currency, id, err := lendingIdentity(row, st, "SLBActivity")
	if err != nil {
		return activity, err
	}
	day, err := parseFlexDate(row.text("date"))
	if err != nil || day.Before(st.FromDate) || day.After(st.ToDate) {
		return activity, fmt.Errorf("invalid loan activity date")
	}
	activity = LendingActivity{AccountID: st.AccountID, LoanID: id, Symbol: row.text("symbol"), Currency: currency, ConID: conID, Date: day,
		RecordID: lendingRecordID("loan_activity", st.AccountID, id, conID, day, currency)}
	activity.Quantity, err = lendingNumber(row, "quantity", false)
	return activity, err
}

func parseLendingFee(row financingNode, st Statement) (LendingFee, error) {
	var fee LendingFee
	conID, currency, id, err := lendingIdentity(row, st, "SLBFee")
	if err != nil {
		return fee, err
	}
	day, err := parseFlexDate(row.text("valueDate"))
	if err != nil || day.Before(st.FromDate) || day.After(st.ToDate) {
		return fee, fmt.Errorf("invalid lending fee date")
	}
	fee = LendingFee{AccountID: st.AccountID, LoanID: id, Symbol: row.text("symbol"), Currency: currency, ConID: conID, ValueDate: day,
		RecordID: lendingRecordID("lend_fee", st.AccountID, id, conID, day, currency)}
	if fee.StartDate, err = optionalDate(row.text("startDate")); err != nil || fee.StartDate.After(day) {
		return LendingFee{}, fmt.Errorf("invalid loan start date")
	}
	for _, item := range []struct {
		key      string
		out      **float64
		positive bool
	}{{"quantity", &fee.Quantity, false}, {"netLendFee", &fee.NetFee, false}, {"netLendFeeRate", &fee.NetRatePct, false}, {"collateralAmount", &fee.Collateral, true}, {"fxRateToBase", &fee.FXRateToBase, true}} {
		if *item.out, err = lendingNumber(row, item.key, item.positive); err != nil {
			return LendingFee{}, err
		}
	}
	// The direction is established by ManagedLoan, not by changing the sign
	// of fees. Quantity is display-only; corrections keep their signed fee.
	if fee.Quantity != nil {
		fee.Quantity = new(math.Abs(*fee.Quantity))
	}
	if fee.FXRateToBase != nil && *fee.FXRateToBase == 0 {
		fee.FXRateToBase = nil
	}
	return fee, nil
}
