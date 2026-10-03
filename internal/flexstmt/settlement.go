package flexstmt

import (
	"context"
	"encoding/xml"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CashBalance is one native-currency end-of-report balance. It is a historical
// baseline, never live available cash. Original statement scope, dates and
// generation identity must remain attached by the retaining consumer.
type CashBalance struct {
	AccountID, Currency           string
	FromDate, ToDate, ReportDate  time.Time
	EndingCash, EndingSettledCash *float64
}

// SettlementManifestVersion identifies optional cash-sweep query requirements.
const SettlementManifestVersion = "canary-settlement-flex-v1"

var settlementManifest = ManifestSection{
	Key: "cash_report", Label: "Cash Report", Container: "CashReport", Row: "CashReportCurrency",
	RequiredFields: []string{"accountId", "currency", "fromDate", "toDate", "endingCash", "endingSettledCash"},
}

// SettlementQueryManifest returns the optional section independently of the
// canonical Recon/Edge manifest. Selecting it cannot change reporting readiness.
func SettlementQueryManifest() []ManifestSection {
	copy := settlementManifest
	copy.RequiredFields = slices.Clone(copy.RequiredFields)
	return []ManifestSection{copy}
}

// SettlementRequirementEvidence reports only observed optional query columns.
// Observed means selected columns, not valid scope, balances or continuity.
func SettlementRequirementEvidence(statements []Statement) []QuerySectionEvidence {
	evidence := QuerySectionEvidence{Key: settlementManifest.Key, Status: QueryRequirementNotReceived}
	fields := map[string]bool{}
	present, rows := false, 0
	for _, st := range statements {
		present = present || st.SettlementCoverage.Present
		rows += st.SettlementCoverage.RowCount
		for _, field := range st.SettlementCoverage.ObservedFields {
			fields[field] = true
		}
	}
	switch {
	case len(statements) == 0:
	case !present:
		evidence.Status = QueryRequirementAbsent
	case rows == 0:
		evidence.Status = QueryRequirementEmpty
	default:
		for _, field := range settlementManifest.RequiredFields {
			if !fields[field] {
				evidence.MissingFields = append(evidence.MissingFields, field)
			}
		}
		evidence.Status = QueryRequirementObserved
		if len(evidence.MissingFields) > 0 {
			evidence.Status = QueryRequirementMissing
		}
	}
	return []QuerySectionEvidence{evidence}
}

type xmlCashRow struct {
	XMLName xml.Name
	Attr    []xml.Attr `xml:",any,attr"`
}
type xmlCashReport struct {
	XMLName xml.Name
	Attr    []xml.Attr   `xml:",any,attr"`
	Rows    []xmlCashRow `xml:"CashReportCurrency"`
}
type xmlCashStatement struct {
	XMLName xml.Name
	Attr    []xml.Attr      `xml:",any,attr"`
	Reports []xmlCashReport `xml:"CashReport"`
}
type xmlCashResponse struct {
	XMLName    xml.Name           `xml:"FlexQueryResponse"`
	Statements []xmlCashStatement `xml:"FlexStatements>FlexStatement"`
}

// parseCashBalances deliberately isolates optional authority rejection from
// existing reporting. Malformed settlement evidence clears all native balances
// of that statement; unrelated Recon/Edge records remain usable.
func parseCashBalances(ctx context.Context, data []byte, statements []Statement) {
	var doc xmlCashResponse
	if unmarshalContext(ctx, data, &doc) != nil || len(doc.Statements) != len(statements) {
		return
	}
	for i, raw := range doc.Statements {
		st := &statements[i]
		st.SettlementCoverage = SectionCoverage{Key: settlementManifest.Key, Present: len(raw.Reports) > 0}
		observed := map[string]bool{}
		for _, report := range raw.Reports {
			for _, row := range report.Rows {
				st.SettlementCoverage.RowCount++
				for _, attr := range row.Attr {
					if attr.Name.Space == "" {
						observed[attr.Name.Local] = true
					}
				}
			}
		}
		for field := range observed {
			st.SettlementCoverage.ObservedFields = append(st.SettlementCoverage.ObservedFields, field)
		}
		sort.Strings(st.SettlementCoverage.ObservedFields)
		if st.SettlementCoverage.RowCount > 0 {
			for _, field := range settlementManifest.RequiredFields {
				if !observed[field] {
					st.SettlementCoverage.MissingFields = append(st.SettlementCoverage.MissingFields, field)
				}
			}
		}
		st.CashBalanceError = parseStatementCashBalances(raw, st, doc.XMLName.Space)
		if st.CashBalanceError != "" {
			st.CashBalances = nil
		}
	}
}

func parseStatementCashBalances(raw xmlCashStatement, st *Statement, namespace string) string {
	if len(raw.Reports) == 0 {
		return "cash_report_missing"
	}
	if len(raw.Reports) != 1 || namespace != "" || raw.XMLName.Space != "" || raw.Reports[0].XMLName.Space != "" {
		return "cash_report_ambiguous_section"
	}
	parent, ok := settlementAttributes(raw.Attr)
	if !ok || parent["accountId"] == "" || parent["accountId"] != st.AccountID || parent["model"] != "" {
		return "cash_report_invalid_scope"
	}
	container, ok := settlementAttributes(raw.Reports[0].Attr)
	if !ok || container["model"] != "" || container["accountId"] != "" && container["accountId"] != st.AccountID {
		return "cash_report_invalid_scope"
	}
	for _, scope := range []map[string]string{parent, container} {
		if scope["accountSegment"] != "" && scope["accountSegment"] != "ALL" || scope["segment"] != "" && scope["segment"] != "ALL" {
			return "cash_report_ambiguous_segments"
		}
	}
	for _, scope := range []map[string]string{parent, container} {
		for field, expected := range map[string]time.Time{"fromDate": st.FromDate, "toDate": st.ToDate, "reportDate": st.ToDate, "whenGenerated": st.WhenGenerated} {
			if rawDate, exists := scope[field]; exists {
				date, err := parseFlexDate(rawDate)
				if err != nil || !date.Equal(expected) {
					return "cash_report_invalid_dates"
				}
			}
		}
	}
	if !settlementDay(st.FromDate) || !settlementDay(st.ToDate) || st.FromDate.After(st.ToDate) || st.WhenGenerated.Before(st.ToDate) {
		return "cash_report_invalid_dates"
	}
	seen := map[string]bool{}
	for _, rawRow := range raw.Reports[0].Rows {
		if rawRow.XMLName.Space != "" {
			return "cash_report_ambiguous_section"
		}
		row, ok := settlementAttributes(rawRow.Attr)
		if !ok {
			return "cash_report_ambiguous_attributes"
		}
		ccy := row["currency"]
		// Aggregated base-currency translation is not a native cash balance.
		if ccy == "BASE_SUMMARY" || ccy == "BASE" {
			continue
		}
		if !settlementCurrency(ccy) || row["accountId"] != st.AccountID || strings.TrimSpace(row["accountId"]) != row["accountId"] || row["model"] != "" {
			return "cash_report_invalid_scope"
		}
		if row["accountSegment"] != "" && row["accountSegment"] != "ALL" {
			return "cash_report_ambiguous_segments"
		}
		if row["segment"] != "" && row["segment"] != "ALL" {
			return "cash_report_ambiguous_segments"
		}
		if seen[ccy] {
			return "cash_report_duplicate_currency"
		}
		seen[ccy] = true
		from, e1 := parseFlexDate(row["fromDate"])
		to, e2 := parseFlexDate(row["toDate"])
		// Native CashReportCurrency exports may omit reportDate. Bind that
		// absent field to the row's exact period end, which is checked against
		// the parent below. An explicitly supplied value must still validate;
		// empty, malformed or conflicting values cannot use this fallback.
		report, e3 := to, e2
		if reportDate, supplied := row["reportDate"]; supplied {
			report, e3 = parseFlexDate(reportDate)
		}
		if e1 != nil || e2 != nil || e3 != nil || !from.Equal(st.FromDate) || !to.Equal(st.ToDate) || !report.Equal(st.ToDate) || !settlementDay(from) || !settlementDay(to) || !settlementDay(report) {
			return "cash_report_invalid_dates"
		}
		cash, cashErr := settlementAmount(row["endingCash"])
		settled, settledErr := settlementAmount(row["endingSettledCash"])
		if cashErr != "" {
			return cashErr
		}
		if settledErr != "" {
			return settledErr
		}
		if !settlementSegmentsAgree(row) {
			return "cash_report_ambiguous_segments"
		}
		st.CashBalances = append(st.CashBalances, CashBalance{AccountID: st.AccountID, Currency: ccy, FromDate: from, ToDate: to, ReportDate: report, EndingCash: &cash, EndingSettledCash: &settled})
	}
	if len(st.CashBalances) == 0 {
		return "cash_report_native_balances_missing"
	}
	return ""
}

func settlementAttributes(attrs []xml.Attr) (map[string]string, bool) {
	out := map[string]string{}
	for _, attr := range attrs {
		key := attr.Name.Local
		if attr.Name.Space != "" {
			return nil, false
		}
		if _, exists := out[key]; exists {
			return nil, false
		}
		for _, expected := range []string{"accountId", "currency", "model", "fromDate", "toDate", "reportDate", "whenGenerated", "endingCash", "endingSettledCash", "segment", "accountSegment"} {
			if strings.EqualFold(key, expected) && key != expected {
				return nil, false
			}
		}
		out[key] = attr.Value
	}
	return out, true
}

func settlementCurrency(ccy string) bool {
	if len(ccy) != 3 {
		return false
	}
	for _, ch := range ccy {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}
func settlementDay(day time.Time) bool {
	return !day.IsZero() && day.Equal(day.UTC().Truncate(24*time.Hour))
}
func settlementAmount(raw string) (float64, string) {
	if raw == "" {
		return 0, "cash_report_missing_amount"
	}
	// Bound exact-decimal validation too: a tiny scientific exponent must
	// neither allocate an enormous rational nor silently underflow to zero.
	if len(raw) > 128 {
		return 0, "cash_report_invalid_amount"
	}
	if at := strings.IndexAny(raw, "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(raw[at+1:], 10, 32)
		if err != nil || exponent < -324 || exponent > 308 {
			return 0, "cash_report_invalid_amount"
		}
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, "cash_report_nonfinite_amount"
	}
	for _, ch := range raw {
		if ch >= '0' && ch <= '9' || ch == '+' || ch == '-' || ch == '.' || ch == 'e' || ch == 'E' {
			continue
		}
		return 0, "cash_report_invalid_amount"
	}
	if n == 0 {
		exact, ok := new(big.Rat).SetString(raw)
		if !ok || exact.Sign() != 0 {
			return 0, "cash_report_invalid_amount"
		}
	}
	return n, ""
}

// When segment columns exist, require both securities and commodities and
// every displayed extra segment to reconcile to the explicit total. Missing
// segment fields never become invented zeros or alternative authority.
func settlementSegmentsAgree(row map[string]string) bool {
	have := false
	for key := range row {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "endingcash") && key != "endingCash" || strings.HasPrefix(lower, "endingsettledcash") && key != "endingSettledCash" {
			have = true
			if !slices.Contains([]string{"endingCashSec", "endingCashCom", "endingCashPaxos", "endingSettledCashSec", "endingSettledCashCom", "endingSettledCashPaxos"}, key) {
				return false
			}
		}
	}
	if !have {
		return true
	}
	sumCash, sumSettled := new(big.Rat), new(big.Rat)
	segments := []string{"Sec", "Com"}
	if _, ok := row["endingCashPaxos"]; ok {
		segments = append(segments, "Paxos")
	} else if _, ok := row["endingSettledCashPaxos"]; ok {
		segments = append(segments, "Paxos")
	}
	for _, segment := range segments {
		_, ce := settlementAmount(row["endingCash"+segment])
		_, se := settlementAmount(row["endingSettledCash"+segment])
		if ce != "" || se != "" {
			return false
		}
		c, cashOK := new(big.Rat).SetString(row["endingCash"+segment])
		s, settledOK := new(big.Rat).SetString(row["endingSettledCash"+segment])
		if !cashOK || !settledOK {
			return false
		}
		sumCash.Add(sumCash, c)
		sumSettled.Add(sumSettled, s)
	}
	cash, cashOK := new(big.Rat).SetString(row["endingCash"])
	settled, settledOK := new(big.Rat).SetString(row["endingSettledCash"])
	return cashOK && settledOK && sumCash.Cmp(cash) == 0 && sumSettled.Cmp(settled) == 0
}
