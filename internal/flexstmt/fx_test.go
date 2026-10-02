package flexstmt

import (
	"strings"
	"testing"
)

const fxSynthetic = `<FlexQueryResponse><FlexStatements><FlexStatement accountId="U-SYNTHETIC" fromDate="20261001" toDate="20261001" whenGenerated="20261002;010000">
<EquitySummaryInBase><EquitySummaryByReportDateInBase currency="EUR" reportDate="20260930" total="1000"/><EquitySummaryByReportDateInBase currency="EUR" reportDate="20261001" ipoSubscription="0" slbDirectSecuritiesBorrowed="0" slbDirectSecuritiesLent="0" commodities="0" notes="0" dividendAccruals="0" liteSurchargeAccruals="0" cgtWithholdingAccruals="0" incentiveCouponAccruals="0" brokerFeesAccrualsComponent="0" eventContractInterestAccruals="0" marginFinancingChargeAccruals="0" softDollars="0" forexCfdUnrealizedPl="0" cfdUnrealizedPl="0" physDel="0" crypto="0" bondInterestAccrualsComponent="0" fdicInsuredAccountInterestAccrualsComponent="0" total="900" cash="100" stock="800" options="0" bonds="0" funds="0" interestAccruals="0"/></EquitySummaryInBase>
<CashReport><CashReportCurrency currency="EUR" fromDate="20261001" toDate="20261001" startingCash="100" endingCash="100" endingSettledCash="100" salesTax="0"/></CashReport>
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
