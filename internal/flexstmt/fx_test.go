package flexstmt

import (
	"strings"
	"testing"
)

const fxSynthetic = `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U-SYNTHETIC" fromDate="20261001" toDate="20261001" whenGenerated="20261002;010000">
<EquitySummaryInBase><EquitySummaryByReportDateInBase currency="EUR" reportDate="20260930" total="1000"/><EquitySummaryByReportDateInBase currency="EUR" reportDate="20261001" ipoSubscription="0" slbCashCollateral="0" slbDirectSecuritiesBorrowed="0" slbDirectSecuritiesLent="0" commodities="0" notes="0" dividendAccruals="0" liteSurchargeAccruals="0" cgtWithholdingAccruals="0" incentiveCouponAccruals="0" brokerFeesAccrualsComponent="0" eventContractInterestAccruals="0" marginFinancingChargeAccruals="0" softDollars="0" forexCfdUnrealizedPl="0" cfdUnrealizedPl="0" physDel="0" crypto="0" bondInterestAccrualsComponent="0" fdicInsuredAccountInterestAccrualsComponent="0" total="900" cash="100" stock="800" options="0" bonds="0" funds="0" interestAccruals="0"/></EquitySummaryInBase>
<CashReport><CashReportCurrency currency="EUR" fromDate="20261001" toDate="20261001" startingCash="100" endingCash="100" endingSettledCash="100" salesTax="0" slbStartingCashCollateral="0" slbEndingCashCollateral="0" slbNetSecuritiesLentActivity="0"/></CashReport>
<InterestAccruals><InterestAccrualsCurrency currency="EUR" fromDate="20261001" toDate="20261001" startingAccrualBalance="0" endingAccrualBalance="0" interestAccrued="0" accrualReversal="0" fxTranslation="0"/></InterestAccruals>
<OpenPositions><OpenPosition accountId="U-SYNTHETIC" conid="100" assetCategory="STK" currency="USD" levelOfDetail="SUMMARY" reportDate="20261001" position="10" multiplier="1" markPrice="100" positionValue="1000"/></OpenPositions>
<ConversionRates><ConversionRate reportDate="20261001" fromCurrency="USD" toCurrency="EUR" rate="0.8"/></ConversionRates>
<Trades/><CashTransactions/><Transfers/><CorporateActions/>
</FlexStatement></FlexStatements></FlexQueryResponse>`

func TestFXSnapshotEvidence(t *testing.T) {
	tests := []struct{ name, source, reason string }{
		{"complete", fxSynthetic, ""},
		{"missing position value", strings.Replace(fxSynthetic, `positionValue="1000"`, "", 1), "native_position_value_missing"},
		{"wrong position units", strings.Replace(fxSynthetic, `multiplier="1"`, `multiplier="2"`, 1), "position_value_does_not_reconcile"},
		{"unproved position category", strings.Replace(fxSynthetic, `assetCategory="STK"`, `assetCategory="CFD"`, 1), "native_position_category_requires_review"},
		{"missing FX rate", strings.Replace(fxSynthetic, `rate="0.8"`, `rate="0.7"`, 1), "nav_component_does_not_reconcile"},
		{"unknown cash event", strings.Replace(fxSynthetic, "<CashTransactions/>", `<CashTransactions><CashTransaction dateTime="20261001;120000" reportDate="20261001" transactionID="1" currency="EUR" amount="0" type="Unknown"/></CashTransactions>`, 1), "unclassified_cash_event"},
		{"off-day flow", strings.Replace(fxSynthetic, "<CashTransactions/>", `<CashTransactions><CashTransaction dateTime="20261001;120000" reportDate="20260930" transactionID="1" currency="EUR" amount="0" type="Deposits/Withdrawals" fxRateToBase="1"/></CashTransactions>`, 1), "cash_event_date_mismatch"},
		{"unexplained cash change", strings.Replace(fxSynthetic, `startingCash="100"`, `startingCash="101"`, 1), "native_cash_movement_does_not_reconcile"},
		{"omitted accrual units", strings.Replace(fxSynthetic, `interestAccrued="0"`, "", 1), "native_accrual_movement_does_not_reconcile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := Parse([]byte(tt.source))
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].FX == nil || rows[0].FX.Reason != tt.reason {
				t.Fatalf("FX evidence %+v, want %s", rows[0].FX, tt.reason)
			}
		})
	}
	t.Run("absent versus empty", func(t *testing.T) {
		before, _, _ := strings.Cut(fxSynthetic, "<InterestAccruals>")
		end := strings.Index(fxSynthetic, "</InterestAccruals>") + len("</InterestAccruals>")
		for _, replacement := range []string{"", "<InterestAccruals/>"} {
			rows, err := Parse([]byte(before + replacement + fxSynthetic[end:]))
			if err != nil {
				t.Fatal(err)
			}
			if replacement == "" && rows[0].FX.Reason != "daily_fx_sections_missing" {
				t.Fatal("absence certified")
			}
			if replacement != "" && rows[0].FX.Reason != "" {
				t.Fatal("explicit empty zero accrual rejected")
			}
		}
	})
}

// TestFXInterestEvidence: the day's accrual and settled cash per currency are
// read even when the attribution gates refuse the day; an ambiguous or
// foreign row is left out, never read as zero.
func TestFXInterestEvidence(t *testing.T) {
	withUSD := strings.Replace(fxSynthetic, "</InterestAccruals>",
		`<InterestAccrualsCurrency currency="USD" fromDate="20261001" toDate="20261001" startingAccrualBalance="0" endingAccrualBalance="-2.8" interestAccrued="-2.8" accrualReversal="0" fxTranslation="0"/></InterestAccruals>`, 1)
	withUSD = strings.Replace(withUSD, "</CashReport>",
		`<CashReportCurrency currency="USD" fromDate="20261001" toDate="20261001" startingCash="-20000" endingCash="-20000" endingSettledCash="-20000" salesTax="0" slbStartingCashCollateral="0" slbEndingCashCollateral="0" slbNetSecuritiesLentActivity="0"/></CashReport>`, 1)
	tests := []struct {
		name, source     string
		accrued, settled map[string]float64
	}{
		{"read", withUSD, map[string]float64{"EUR": 0, "USD": -2.8}, map[string]float64{"EUR": 100, "USD": -20000}},
		{"refused day still read", strings.Replace(withUSD, "<Transfers/>", `<Transfers><Transfer currency="EUR" date="20261001"/></Transfers>`, 1),
			map[string]float64{"EUR": 0, "USD": -2.8}, map[string]float64{"EUR": 100, "USD": -20000}},
		{"unreadable settled cash left out", strings.Replace(withUSD, `endingSettledCash="-20000"`, `endingSettledCash="n/a"`, 1),
			map[string]float64{"EUR": 0, "USD": -2.8}, map[string]float64{"EUR": 100}},
		{"foreign account left out", strings.Replace(withUSD, `<InterestAccrualsCurrency currency="USD"`, `<InterestAccrualsCurrency accountId="U-OTHER" currency="USD"`, 1),
			map[string]float64{"EUR": 0}, map[string]float64{"EUR": 100, "USD": -20000}},
		{"duplicate currency left out", strings.Replace(withUSD, "</InterestAccruals>",
			`<InterestAccrualsCurrency currency="USD" fromDate="20261001" toDate="20261001" interestAccrued="-1"/></InterestAccruals>`, 1),
			map[string]float64{"EUR": 0}, map[string]float64{"EUR": 100, "USD": -20000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := Parse([]byte(tt.source))
			if err != nil {
				t.Fatal(err)
			}
			f := rows[0].FX
			if f == nil {
				t.Fatal("no FX snapshot")
			}
			if len(f.InterestAccrued) != len(tt.accrued) || len(f.SettledEnd) != len(tt.settled) {
				t.Fatalf("accrued %v settled %v, want %v %v", f.InterestAccrued, f.SettledEnd, tt.accrued, tt.settled)
			}
			for c, v := range tt.accrued {
				if got, ok := f.InterestAccrued[c]; !ok || got != v {
					t.Fatalf("accrued[%s] = %v, want %v", c, got, v)
				}
			}
			for c, v := range tt.settled {
				if got, ok := f.SettledEnd[c]; !ok || got != v {
					t.Fatalf("settled[%s] = %v, want %v", c, got, v)
				}
			}
		})
	}
}

func TestFXNativeLendingCollateralPair(t *testing.T) {
	source := strings.Replace(fxSynthetic, `slbCashCollateral="0"`, `slbCashCollateral="100"`, 1)
	source = strings.Replace(source, `slbDirectSecuritiesLent="0"`, `slbDirectSecuritiesLent="-100"`, 1)
	source = strings.Replace(source, `slbEndingCashCollateral="0"`, `slbEndingCashCollateral="100"`, 1)
	source = strings.Replace(source, `slbNetSecuritiesLentActivity="0"`, `slbNetSecuritiesLentActivity="100"`, 1)
	loan := `<SLBOpenContracts><SLBOpenContract accountId="U-SYNTHETIC" currency="EUR" date="20261001" type="ManagedLoan" slbTransactionId="SLB.SYNTHETIC" quantity="-1" collateralAmount="100"/></SLBOpenContracts>`
	source = strings.Replace(source, `<Trades/>`, loan+`<Trades/>`, 1)
	foreignPair := strings.Replace(strings.Replace(source, `currency="EUR" date="20261001" type="ManagedLoan"`, `currency="USD" date="20261001" type="ManagedLoan"`, 1), `collateralAmount="100"`, `collateralAmount="125"`, 1)
	foreignPair = strings.Replace(foreignPair, `</CashReport>`, `<CashReportCurrency currency="USD" fromDate="20261001" toDate="20261001" startingCash="0" endingCash="0" salesTax="0" slbStartingCashCollateral="0" slbEndingCashCollateral="0" slbNetSecuritiesLentActivity="0"/></CashReport>`, 1)
	for _, tt := range []struct{ name, source, reason string }{
		{"managed loan", source, ""},
		{"direct loan", strings.Replace(source, `type="ManagedLoan"`, `type="DirectLoan"`, 1), ""},
		{"native section absent", strings.Replace(source, loan, "", 1), "native_lending_section_missing"},
		{"base-cancelling different currencies", foreignPair, "native_lending_collateral_pair_does_not_reconcile"},
		{"wrong report day", strings.Replace(source, `date="20261001"`, `date="20260930"`, 1), "lending_date_mismatch"},
		{"unproved borrow", strings.Replace(source, `type="ManagedLoan"`, `type="DirectBorrow"`, 1), "native_borrow_requires_review"},
		{"cash collateral movement", strings.Replace(source, `slbNetSecuritiesLentActivity="100"`, `slbNetSecuritiesLentActivity="99"`, 1), "native_collateral_movement_does_not_reconcile"},
		{"lending NAV mismatch", strings.Replace(source, `slbDirectSecuritiesLent="-100"`, `slbDirectSecuritiesLent="-101"`, 1), "nav_component_does_not_reconcile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := Parse([]byte(tt.source))
			if err != nil {
				t.Fatal(err)
			}
			f := rows[0].FX
			if f.Reason != tt.reason {
				t.Fatalf("reason %s, want %s", f.Reason, tt.reason)
			}
			if tt.reason == "" && (f.Book["EUR"] != 100 || f.Book["USD"] != 1000 || len(f.LendingDays) != 1) {
				t.Fatal("paired collateral altered investment book", f)
			}
		})
	}
}
