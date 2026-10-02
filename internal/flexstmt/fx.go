package flexstmt

import (
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// FXSnapshot is a reconciled single-reporting-day native currency book.
// An invalid or incomplete snapshot retains Reason; it never certifies zero.
type FXSnapshot struct {
	Day, PreviousDay, BaseCurrency, Reason string
	SingleDay, Foreign, ClosingForeign     bool
	Book, Rates                            map[string]float64
	CashStart, CashEnd                     map[string]float64
	InterestStart, InterestEnd             map[string]float64
	Conversion, External                   map[string]float64
	ExternalBase, NAV                      float64
}

type fxNode struct {
	Attrs []xml.Attr `xml:",any,attr"`
}

func (n fxNode) text(key string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == key {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

func (n fxNode) number(key string) (float64, error) {
	x, err := strconv.ParseFloat(n.text(key), 64)
	if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, fmt.Errorf("missing or invalid %s", key)
	}
	return x, nil
}

type fxSection struct {
	Rows []fxNode `xml:",any"`
}
type fxRawStatement struct {
	Cash             *fxSection `xml:"CashReport"`
	Positions        *fxSection `xml:"OpenPositions"`
	Interest         *fxSection `xml:"InterestAccruals"`
	NAV              *fxSection `xml:"EquitySummaryInBase"`
	Trades           *fxSection `xml:"Trades"`
	CashTransactions *fxSection `xml:"CashTransactions"`
	Transfers        *fxSection `xml:"Transfers"`
	CorporateActions *fxSection `xml:"CorporateActions"`
}

func parseFXSnapshots(data []byte, statements []Statement) {
	var doc struct {
		Statements []fxRawStatement `xml:"FlexStatements>FlexStatement"`
	}
	if xml.Unmarshal(data, &doc) != nil || len(doc.Statements) != len(statements) {
		return
	}
	for i, raw := range doc.Statements {
		st := &statements[i]
		f := &FXSnapshot{Day: st.ToDate.Format("2006-01-02"), SingleDay: st.FromDate.Equal(st.ToDate), Book: map[string]float64{}, Rates: map[string]float64{}, CashStart: map[string]float64{}, CashEnd: map[string]float64{}, InterestStart: map[string]float64{}, InterestEnd: map[string]float64{}, Conversion: map[string]float64{}, External: map[string]float64{}}
		st.FX = f
		if !f.SingleDay {
			f.Reason = "daily_statement_required"
			continue
		}
		if err := buildFXSnapshot(raw, *st, f); err != nil {
			f.Reason = err.Error()
		}
	}
}

func buildFXSnapshot(raw fxRawStatement, st Statement, f *FXSnapshot) error {
	if raw.NAV == nil || raw.Cash == nil || raw.Positions == nil || raw.Interest == nil || raw.Trades == nil || raw.CashTransactions == nil || raw.Transfers == nil || raw.CorporateActions == nil {
		return fmt.Errorf("daily_fx_sections_missing")
	}
	if len(raw.Transfers.Rows) != 0 {
		return fmt.Errorf("asset_or_cash_transfer_requires_review")
	}
	// Corporate actions can contain external distributions and spin-offs. Do
	// not fabricate attribution until a complete native event bridge is known.
	if len(raw.CorporateActions.Rows) != 0 {
		return fmt.Errorf("corporate_action_requires_review")
	}
	checkRow := func(row fxNode) error {
		if a := row.text("accountId"); a != "" && a != st.AccountID {
			return fmt.Errorf("fx_account_scope_mismatch")
		}
		if row.text("model") != "" {
			return fmt.Errorf("fx_model_scope_unsupported")
		}
		return nil
	}
	seenNAV := map[string]bool{}
	for _, row := range raw.NAV.Rows {
		if err := checkRow(row); err != nil {
			return err
		}
		if seenNAV[row.text("reportDate")] {
			return fmt.Errorf("duplicate_nav_date")
		}
		seenNAV[row.text("reportDate")] = true
		d, err := parseFlexDate(row.text("reportDate"))
		if err != nil {
			return fmt.Errorf("invalid_nav_date")
		}
		day := d.Format("2006-01-02")
		if day < f.Day && day > f.PreviousDay {
			f.PreviousDay = day
		}
		if day == f.Day {
			f.BaseCurrency = strings.ToUpper(row.text("currency"))
			f.NAV, err = row.number("total")
			if err != nil {
				return fmt.Errorf("invalid_nav_value")
			}
		}
	}
	if f.BaseCurrency == "" || f.PreviousDay == "" {
		return fmt.Errorf("nav_currency_or_boundary_missing")
	}
	f.Rates[f.BaseCurrency] = 1
	for _, rate := range st.FXRates {
		if rate.ToCurrency == f.BaseCurrency && rate.Date.Equal(st.ToDate) && rate.Rate != nil && *rate.Rate > 0 {
			if old, ok := f.Rates[rate.FromCurrency]; ok && math.Abs(old-*rate.Rate) > 1e-12 {
				return fmt.Errorf("conflicting_daily_fx_rate")
			}
			f.Rates[rate.FromCurrency] = *rate.Rate
		}
	}
	addBalance := func(section *fxSection, startKey, endKey string, starts, ends map[string]float64) error {
		seen := map[string]bool{}
		for _, row := range section.Rows {
			if err := checkRow(row); err != nil {
				return err
			}
			c := strings.ToUpper(row.text("currency"))
			if c == "BASE_SUMMARY" {
				continue
			}
			if c == "" || seen[c] {
				return fmt.Errorf("fx_duplicate_or_missing_currency")
			}
			seen[c] = true
			from, e1 := parseFlexDate(row.text("fromDate"))
			to, e2 := parseFlexDate(row.text("toDate"))
			if e1 != nil || e2 != nil || !from.Equal(st.ToDate) || !to.Equal(st.ToDate) {
				return fmt.Errorf("fx_balance_date_mismatch")
			}
			a, e1 := row.number(startKey)
			b, e2 := row.number(endKey)
			if e1 != nil || e2 != nil {
				return fmt.Errorf("fx_balance_value_missing")
			}
			starts[c], ends[c] = a, b
			f.Book[c] += b
			if c != f.BaseCurrency && b != 0 {
				f.ClosingForeign = true
			}
			if c != f.BaseCurrency && (a != 0 || b != 0) {
				f.Foreign = true
			}
		}
		return nil
	}
	if err := addBalance(raw.Cash, "startingCash", "endingCash", f.CashStart, f.CashEnd); err != nil {
		return err
	}
	if err := addBalance(raw.Interest, "startingAccrualBalance", "endingAccrualBalance", f.InterestStart, f.InterestEnd); err != nil {
		return err
	}
	for _, row := range raw.Interest.Rows {
		if row.text("currency") == "BASE_SUMMARY" {
			continue
		}
		a, e1 := row.number("interestAccrued")
		b, e2 := row.number("accrualReversal")
		translation, e3 := row.number("fxTranslation")
		c := row.text("currency")
		if e1 != nil || e2 != nil || e3 != nil || math.Abs(f.InterestEnd[c]-f.InterestStart[c]-a-b-translation) > .02 {
			return fmt.Errorf("native_accrual_movement_does_not_reconcile")
		}
	}
	if len(f.CashEnd) == 0 {
		return fmt.Errorf("native_cash_balances_missing")
	}
	positionTotals := map[string]float64{}
	seenPositions := map[string]bool{}
	for _, row := range raw.Positions.Rows {
		if err := checkRow(row); err != nil {
			return err
		}
		if strings.ToUpper(row.text("levelOfDetail")) != "SUMMARY" {
			return fmt.Errorf("position_summary_required")
		}
		day, err := parseFlexDate(row.text("reportDate"))
		if err != nil || !day.Equal(st.ToDate) {
			return fmt.Errorf("position_date_mismatch")
		}
		id := row.text("conid")
		if id == "" || seenPositions[id] {
			return fmt.Errorf("duplicate_position_summary")
		}
		seenPositions[id] = true
		c := strings.ToUpper(row.text("currency"))
		v, err := row.number("positionValue")
		if c == "" || err != nil {
			return fmt.Errorf("native_position_value_missing")
		}
		q, e1 := row.number("position")
		m, e2 := row.number("multiplier")
		p, e3 := row.number("markPrice")
		if e1 != nil || e2 != nil || e3 != nil || m <= 0 || math.Abs(v-q*m*p) > .02 {
			return fmt.Errorf("position_value_does_not_reconcile")
		}
		positionTotals[row.text("assetCategory")] += v * f.Rates[c]
		f.Book[c] += v
		if c != f.BaseCurrency && q != 0 {
			f.ClosingForeign = true
			f.Foreign = true
		}
	}
	ledger := map[string]float64{}
	seenTrades := map[string]bool{}
	for _, row := range raw.Trades.Rows {
		if err := checkRow(row); err != nil {
			return err
		}
		if strings.ToUpper(row.text("levelOfDetail")) != "EXECUTION" {
			continue
		}
		id := row.text("transactionID")
		if id == "" || seenTrades[id] {
			return fmt.Errorf("trade_identity_missing_or_duplicate")
		}
		seenTrades[id] = true
		day, err := parseFlexDate(row.text("reportDate"))
		if err != nil || !day.Equal(st.ToDate) {
			return fmt.Errorf("trade_date_mismatch")
		}
		proceeds, ep := row.number("proceeds")
		taxes, et := row.number("taxes")
		commission, ec := row.number("ibCommission")
		c := row.text("currency")
		cc := row.text("ibCommissionCurrency")
		if ep != nil || et != nil || ec != nil || c == "" || cc == "" {
			return fmt.Errorf("trade_cash_units_missing")
		}
		ledger[c] += proceeds + taxes
		ledger[cc] += commission
		if c != f.BaseCurrency {
			f.Foreign = true
		}
		if row.text("assetCategory") != "CASH" {
			continue
		}
		pair := strings.Split(row.text("symbol"), ".")
		q, e1 := row.number("quantity")
		p, e2 := row.number("proceeds")
		price, e3 := row.number("tradePrice")
		tax, e4 := row.number("taxes")
		if len(pair) != 2 || pair[1] != row.text("currency") || e1 != nil || e2 != nil || e3 != nil || e4 != nil || tax != 0 || math.Abs(p+q*price) > .02 {
			return fmt.Errorf("fx_conversion_units_unproved")
		}
		ledger[pair[0]] += q
		f.Conversion[pair[0]] += q
		f.Conversion[pair[1]] += p
		if q != 0 {
			f.Foreign = true
		}
	}
	seenCash := map[string]bool{}
	for _, row := range raw.CashTransactions.Rows {
		if err := checkRow(row); err != nil {
			return err
		}
		day, err := parseFlexDate(row.text("reportDate"))
		if err != nil || !day.Equal(st.ToDate) {
			return fmt.Errorf("cash_event_date_mismatch")
		}
		id := row.text("transactionID")
		if id == "" || seenCash[id] {
			return fmt.Errorf("cash_event_identity_missing_or_duplicate")
		}
		seenCash[id] = true
		c := strings.ToUpper(row.text("currency"))
		a, ea := row.number("amount")
		if c == "" || ea != nil {
			return fmt.Errorf("cash_event_units_missing")
		}
		ledger[c] += a
		typ := row.text("type")
		if typ != "Deposits/Withdrawals" && !knownNonFlowTypes[typ] {
			return fmt.Errorf("unclassified_cash_event")
		}
		if typ != "Deposits/Withdrawals" {
			continue
		}
		x, e2 := row.number("fxRateToBase")
		if e2 != nil || x <= 0 {
			return fmt.Errorf("external_flow_fx_missing")
		}
		f.External[c] += a
		f.ExternalBase += a * x
		if c != f.BaseCurrency && a != 0 {
			f.Foreign = true
		}
	}
	for _, row := range raw.Cash.Rows {
		c := row.text("currency")
		if c == "BASE_SUMMARY" {
			continue
		}
		tax, err := row.number("salesTax")
		if err != nil {
			return fmt.Errorf("cash_sales_tax_missing")
		}
		if math.Abs(f.CashEnd[c]-f.CashStart[c]-ledger[c]-tax) > .02 {
			return fmt.Errorf("native_cash_movement_does_not_reconcile")
		}
		delete(ledger, c)
	}
	for _, v := range ledger {
		if math.Abs(v) > .02 {
			return fmt.Errorf("cash_ledger_currency_missing")
		}
	}
	for _, row := range raw.NAV.Rows {
		d, _ := parseFlexDate(row.text("reportDate"))
		if !d.Equal(st.ToDate) {
			continue
		}
		for _, key := range []string{"ipoSubscription", "slbDirectSecuritiesBorrowed", "slbDirectSecuritiesLent", "commodities", "notes", "dividendAccruals", "liteSurchargeAccruals", "cgtWithholdingAccruals", "incentiveCouponAccruals", "brokerFeesAccrualsComponent", "eventContractInterestAccruals", "marginFinancingChargeAccruals", "softDollars", "forexCfdUnrealizedPl", "cfdUnrealizedPl", "physDel", "crypto", "bondInterestAccrualsComponent", "fdicInsuredAccountInterestAccrualsComponent"} {
			v, err := row.number(key)
			if err != nil {
				return fmt.Errorf("nav_component_coverage_missing")
			}
			if math.Abs(v) > .000001 {
				return fmt.Errorf("unsupported_native_nav_component")
			}
		}
		checks := map[string]float64{"cash": 0, "stock": positionTotals["STK"], "options": positionTotals["OPT"], "bonds": positionTotals["BOND"], "funds": positionTotals["FUND"], "interestAccruals": 0}
		for c, v := range f.CashEnd {
			checks["cash"] += v * f.Rates[c]
		}
		for c, v := range f.InterestEnd {
			checks["interestAccruals"] += v * f.Rates[c]
		}
		for key, want := range checks {
			got, err := row.number(key)
			if err != nil || math.Abs(got-want) > .02 {
				return fmt.Errorf("nav_component_does_not_reconcile")
			}
		}
	}
	translated := 0.0
	for c, b := range f.Book {
		x, ok := f.Rates[c]
		if !ok || x <= 0 {
			return fmt.Errorf("daily_fx_rate_missing")
		}
		translated += b * x
	}
	if math.Abs(translated-f.NAV) > .02 {
		return fmt.Errorf("native_book_does_not_reconcile_to_nav")
	}
	return nil
}
