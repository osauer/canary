package flexstmt

import (
	"slices"
	"strings"
	"testing"
)

func TestParseEdgeExecutionEvidenceAndCoverage(t *testing.T) {
	t.Parallel()
	xml := `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U-PRIVATE" fromDate="20260101" toDate="20260131" whenGenerated="20260201;010203">
<Trades><Trade accountId="U-PRIVATE" assetCategory="STK" currency="USD" fxRateToBase="0.92" symbol="ACME" conid="123" underlyingConid="0" underlyingSymbol="" multiplier="1" tradeID="t1" ibOrderID="o1" ibExecID="e1" transactionID="x1" dateTime="20260105;153045" tradeDate="20260105" buySell="BUY" quantity="10" tradePrice="100.25" proceeds="-1002.5" ibCommission="-1" ibCommissionCurrency="USD" taxes="0" openCloseIndicator="O" cost="1003.5" fifoPnlRealized="0" mtmPnl="2.5" closePrice="100.5" netCash="-1003.5" levelOfDetail="EXECUTION" /></Trades>
<SecuritiesInfo><SecurityInfo assetCategory="STK" currency="USD" symbol="ACME" description="ACME CORP" conid="123" multiplier="1" listingExchange="NYSE" /></SecuritiesInfo>
<OpenPositions><OpenPosition accountId="U-PRIVATE" assetCategory="STK" currency="USD" fxRateToBase="0.92" symbol="ACME" conid="123" reportDate="20260131" position="10" multiplier="1" markPrice="110" costBasisPrice="100.35" costBasisMoney="1003.5" fifoPnlUnrealized="96.5" side="Long" openDateTime="20260105;153045" levelOfDetail="SUMMARY" /></OpenPositions>
<OptionEAE><OptionEAE accountId="U-PRIVATE" assetCategory="OPT" currency="USD" fxRateToBase="0.92" symbol="ACME  260116C00100000" conid="456" underlyingConid="123" underlyingSymbol="ACME" date="20260116" transactionType="Expiration" quantity="-1" tradePrice="0" markPrice="0" proceeds="0" commisionsAndTax="0" costBasis="-200" realizedPnl="-200" mtmPnl="0" tradeID="oe1" /></OptionEAE>
<CorporateActions><CorporateAction accountId="U-PRIVATE" assetCategory="STK" currency="USD" fxRateToBase="0.92" symbol="ACME" conid="123" multiplier="1" reportDate="20260120" dateTime="20260120;000000" quantity="10" proceeds="0" amount="0" fifoPnlRealized="0" mtmPnl="0" type="FS" transactionID="ca1" /></CorporateActions>
<Transfers><Transfer accountId="U-PRIVATE" assetCategory="STK" currency="USD" fxRateToBase="0.92" symbol="ACME" conid="123" transactionID="tr1" date="20260102" direction="IN" quantity="5" cashTransfer="0" positionAmountInBase="460" description="ignored evidence text" /></Transfers>
<CashTransactions><CashTransaction transactionID="c1" type="Deposits/Withdrawals" currency="EUR" fxRateToBase="1" amount="1000" dateTime="20260102;120000" settleDate="20260102" description="ignored evidence text" /></CashTransactions>
<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20260101" total="10000"/><EquitySummaryByReportDateInBase reportDate="20260131" total="11000"/></EquitySummaryInBase>
<ConversionRates><ConversionRate dateTime="20260106;000000" fromCurrency="USD" toCurrency="EUR" rate="0.92" /></ConversionRates>
</FlexStatement></FlexStatements></FlexQueryResponse>`
	statements, err := Parse([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 {
		t.Fatalf("statements = %d, want 1", len(statements))
	}
	st := statements[0]
	if st.ManifestVersion != ManifestVersion || len(st.Coverage) != len(CanonicalQueryManifest()) {
		t.Fatalf("manifest coverage = %q / %d", st.ManifestVersion, len(st.Coverage))
	}
	if len(st.Trades) != 1 || st.Trades[0].ConID != 123 || st.Trades[0].OrderID != "o1" || st.Trades[0].Quantity == nil || *st.Trades[0].Quantity != 10 {
		t.Fatalf("trade = %+v", st.Trades)
	}
	if len(st.Positions) != 1 || st.Positions[0].Quantity == nil || *st.Positions[0].Quantity != 10 {
		t.Fatalf("positions = %+v", st.Positions)
	}
	if len(st.Instruments) != 1 || st.Instruments[0].ConID != 123 || st.Instruments[0].Symbol != "ACME" {
		t.Fatalf("instruments = %+v", st.Instruments)
	}
	if len(st.OptionEvents) != 1 || st.OptionEvents[0].TransactionType != "EXPIRATION" ||
		st.OptionEvents[0].MarkPrice == nil || st.OptionEvents[0].CommissionTax == nil ||
		st.OptionEvents[0].CostBasis == nil || st.OptionEvents[0].RealizedPNL == nil {
		t.Fatalf("option events = %+v", st.OptionEvents)
	}
	if len(st.CorporateActions) != 1 || st.CorporateActions[0].Type != "FS" {
		t.Fatalf("corporate actions = %+v", st.CorporateActions)
	}
	if len(st.FXRates) != 1 || st.FXRates[0].Rate == nil || *st.FXRates[0].Rate != .92 {
		t.Fatalf("fx rates = %+v", st.FXRates)
	}
	if len(st.Transfers) != 1 || st.Transfers[0].TransactionID != "tr1" || st.Transfers[0].ConID != 123 || st.Transfers[0].Quantity == nil || *st.Transfers[0].Quantity != 5 {
		t.Fatalf("transfers = %+v", st.Transfers)
	}
	if len(st.Cash) != 1 || st.Cash[0].TransactionID != "c1" {
		t.Fatalf("cash transactions = %+v", st.Cash)
	}
	for _, coverage := range st.Coverage {
		if !coverage.Present {
			t.Errorf("section %s not present", coverage.Key)
		}
	}
	for _, section := range QueryRequirementEvidence(statements) {
		if section.Key == "trades" && section.Status != QueryRequirementObserved {
			t.Fatalf("current Trades wire coverage = %+v", section)
		}
		if section.Key == "option_events" && section.Status != QueryRequirementObserved {
			t.Fatalf("OptionEAE wire coverage = %+v", section)
		}
	}
}

func TestParseKeepsMissingNumericAbsentAndRejectsMalformedNumeric(t *testing.T) {
	t.Parallel()
	base := `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U" fromDate="20260101" toDate="20260102" whenGenerated="20260103"><Trades><Trade conid="123" dateTime="20260102;120000" buySell="BUY" quantity="1" tradePrice="%s" levelOfDetail="EXECUTION" /></Trades><EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20260102" total="1"/></EquitySummaryInBase></FlexStatement></FlexStatements></FlexQueryResponse>`
	statements, err := Parse([]byte(strings.Replace(base, "%s", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if statements[0].Trades[0].Price != nil {
		t.Fatalf("missing price became %v", *statements[0].Trades[0].Price)
	}
	if _, err := Parse([]byte(strings.Replace(base, "%s", "not-a-number", 1))); err == nil || !strings.Contains(err.Error(), "not numeric") {
		t.Fatalf("malformed numeric error = %v", err)
	}
}

func TestCanonicalManifestReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	a := CanonicalQueryManifest()
	a[0].RequiredFields[0] = "mutated"
	b := CanonicalQueryManifest()
	if b[0].RequiredFields[0] == "mutated" {
		t.Fatal("canonical manifest was mutated through caller copy")
	}
}

func TestLegacyExecutionAliasesSatisfyCurrentManifest(t *testing.T) {
	t.Parallel()
	fields := append([]string(nil), CanonicalQueryManifest()[0].RequiredFields...)
	for i, field := range fields {
		switch field {
		case "dateTime":
			fields[i] = "tradeTime"
		case "ibCommission":
			fields[i] = "IBCommission"
		case "ibCommissionCurrency":
			fields[i] = "IBCommissionCurrency"
		}
	}
	evidence := QueryRequirementEvidence([]Statement{{Coverage: []SectionCoverage{{
		Key: "trades", Present: true, RowCount: 1, ObservedFields: fields,
	}}}})
	if evidence[0].Status != QueryRequirementObserved {
		t.Fatalf("legacy Trades aliases = %+v", evidence[0])
	}
}

func TestMissingQueryRequirementsUsesCanonicalSectionsAndObservedFields(t *testing.T) {
	t.Parallel()
	xml := `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U" fromDate="20260101" toDate="20260102" whenGenerated="20260103"><Trades><Trade conid="123" dateTime="20260102;120000" buySell="BUY" quantity="1" tradePrice="10" levelOfDetail="EXECUTION" /></Trades><OpenPositions></OpenPositions></FlexStatement></FlexStatements></FlexQueryResponse>`
	statements, err := Parse([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	missing := MissingQueryRequirements(statements)
	contains := func(value string) bool {
		return slices.Contains(missing, value)
	}
	if !contains("trades.assetCategory") || contains("equity") {
		t.Fatalf("missing requirements = %+v", missing)
	}
	if contains("open_positions.position") {
		t.Fatalf("empty present section invented field absence: %+v", missing)
	}
	var trades SectionCoverage
	for _, coverage := range statements[0].Coverage {
		if coverage.Key == "trades" {
			trades = coverage
			break
		}
	}
	if len(trades.MissingFields) == 0 || trades.MissingFields[0] == "" {
		t.Fatalf("trade field diagnostics = %+v", trades)
	}
	for _, section := range QueryRequirementEvidence(statements) {
		if section.Key == "equity" && section.Status != QueryRequirementAbsent {
			t.Fatalf("absent section status = %q", section.Status)
		}
	}
}

func TestQueryRequirementEvidenceDistinguishesAbsentEmptyMissingAndNotReceived(t *testing.T) {
	t.Parallel()

	none := QueryRequirementEvidence(nil)
	if len(none) != len(CanonicalQueryManifest()) {
		t.Fatalf("not-received evidence count = %d", len(none))
	}
	for _, section := range none {
		if section.Status != QueryRequirementNotReceived {
			t.Fatalf("section %s status = %q", section.Key, section.Status)
		}
	}

	statements := []Statement{{Coverage: []SectionCoverage{
		{Key: "transfers", Present: true},
		{Key: "equity", Present: true, RowCount: 1, ObservedFields: []string{"reportDate"}},
	}}}
	byKey := map[string]QuerySectionEvidence{}
	for _, section := range QueryRequirementEvidence(statements) {
		byKey[section.Key] = section
	}
	if got := byKey["trades"].Status; got != QueryRequirementAbsent {
		t.Fatalf("trades status = %q", got)
	}
	if got := byKey["transfers"].Status; got != QueryRequirementEmpty {
		t.Fatalf("transfers status = %q", got)
	}
	if got := byKey["equity"]; got.Status != QueryRequirementMissing || len(got.MissingFields) != 1 || got.MissingFields[0] != "total" {
		t.Fatalf("equity evidence = %+v", got)
	}
	if _, ok := byKey["fx_rates"]; ok {
		t.Fatal("optional currency conversion rates appeared as a required query section")
	}
}

func TestQuerySchemaFingerprintContainsNoStatementValues(t *testing.T) {
	t.Parallel()

	a := []Statement{{AccountID: "first", Coverage: []SectionCoverage{{Key: "transfers", Present: true}}}}
	b := []Statement{{AccountID: "second", Coverage: []SectionCoverage{{Key: "transfers", Present: true}}}}
	if gotA, gotB := QuerySchemaFingerprint(a), QuerySchemaFingerprint(b); gotA == "" || gotA != gotB {
		t.Fatalf("schema fingerprints = %q, %q", gotA, gotB)
	}
	if got := QuerySchemaFingerprint(nil); got != "" {
		t.Fatalf("missing report fingerprint = %q", got)
	}
}
