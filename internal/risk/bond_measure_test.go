package risk

import (
	"math"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f ± %g", name, got, want, tol)
	}
}

// Known answers: a 10-year 3% annual bond at par yields 3% with a modified
// duration of 8.530; a 2-year zero priced for 4% annual has 2/1.04 = 1.923;
// a 7-year 5% semi-annual note at par on a coupon date yields 5%.
func TestMeasureBondKnownAnswers(t *testing.T) {
	asOf := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	m, err := MeasureBond(100, 3, asOf.AddDate(10, 0, 0), asOf, 1)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "10y par yield", m.YieldPct, 3, 0.01)
	near(t, "10y par duration", m.ModifiedDuration, 8.530, 0.01)
	near(t, "10y par accrued", m.AccruedPer100, 0, 1e-9)
	// A one-point rise reprices the bond to about 91.9: an 8.1% loss.
	near(t, "10y par loss", m.LossFraction(1), 0.0811, 0.002)

	zero, err := MeasureBond(100/math.Pow(1.04, 2), 0, asOf.AddDate(2, 0, 0), asOf, 1)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "2y zero yield", zero.YieldPct, 4, 0.01)
	near(t, "2y zero duration", zero.ModifiedDuration, 1.923, 0.005)

	note, err := MeasureBond(100, 5, asOf.AddDate(7, 0, 0), asOf, 2)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "7y note yield", note.YieldPct, 5, 0.01)
	if note.ModifiedDuration < 5.6 || note.ModifiedDuration > 6.0 {
		t.Errorf("7y note duration = %.3f, want about 5.8", note.ModifiedDuration)
	}
}

// Halfway through a 6% semi-annual period, 1.5 per 100 has accrued; a price
// above what the bond still pays yields below zero.
func TestMeasureBondAccruedAndNegativeYield(t *testing.T) {
	maturity := time.Date(2031, 4, 15, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC) // between 15 Apr and 15 Oct 2026
	m, err := MeasureBond(100, 6, maturity, mid, 2)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "accrued", m.AccruedPer100, 1.5, 0.02)
	rich, err := MeasureBond(101, 0, time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC), mid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rich.YieldPct >= 0 {
		t.Errorf("a zero above par yields %.3f%%, want below zero", rich.YieldPct)
	}
	for name, run := range map[string]func() error{
		"matured":        func() error { _, err := MeasureBond(99, 1, mid, mid, 1); return err },
		"no price":       func() error { _, err := MeasureBond(0, 1, maturity, mid, 1); return err },
		"coupon over":    func() error { _, err := MeasureBond(99, 101, maturity, mid, 1); return err },
		"odd frequency":  func() error { _, err := MeasureBond(99, 1, maturity, mid, 3); return err },
		"price too high": func() error { _, err := MeasureBond(1e6, 1, maturity, mid, 1); return err },
	} {
		if run() == nil {
			t.Errorf("%s: measured", name)
		}
	}
	// End of month: an Aug 31 maturity steps back to Feb 28, not Mar 3.
	if got := addMonthsClamped(time.Date(2031, 8, 31, 0, 0, 0, 0, time.UTC), -6); got.Month() != time.February || got.Day() != 28 {
		t.Errorf("31 Aug - 6 months = %s", got.Format(time.DateOnly))
	}
}

// The book sums values by currency and issuer, counts non-government bonds,
// and names every line it could not value or measure.
func TestSummarizeBondBook(t *testing.T) {
	if SummarizeBondBook(nil, new(100000.0), "EUR") != nil {
		t.Fatal("an empty book was summarized")
	}
	lines := []BondBookLine{
		{Symbol: "UST", Currency: "USD", Issuer: "United States Treasury", IssuerClass: BondIssuerGovernment, MarketValueBase: new(9000.0), RateShockLossBase: new(500.0)},
		{Symbol: "BUND", Currency: "EUR", Issuer: "Federal Republic of Germany", IssuerClass: BondIssuerGovernment, MarketValueBase: new(10000.0), RateShockLossBase: new(800.0)},
		{Symbol: "CORP", Currency: "EUR", Issuer: "Synthetic Corp", IssuerClass: BondIssuerInvestmentGrade, MarketValueBase: new(4000.0), Unmeasured: "no evidence"},
		{Symbol: "LOST", Currency: "GBP", Issuer: "United Kingdom", IssuerClass: BondIssuerGovernment},
	}
	book := SummarizeBondBook(lines, new(100000.0), "eur")
	if book.BaseCurrency != "EUR" || book.ShockPoints != 1 || book.MarketValueBase != 23000 || book.RateShockLossBase != 1300 || book.NonGovernmentBase != 4000 {
		t.Fatalf("book = %+v", book)
	}
	near(t, "pct nlv", *book.PctNLV, 23, 1e-9)
	near(t, "loss pct nlv", *book.RateShockLossPctNLV, 1.3, 1e-9)
	near(t, "non-government pct", *book.NonGovernmentPctNLV, 4, 1e-9)
	if len(book.Currencies) != 2 || book.Currencies[0].Currency != "EUR" || book.Currencies[0].MarketValueBase != 14000 || book.Currencies[0].RateShockLossBase != 800 {
		t.Fatalf("currencies = %+v", book.Currencies)
	}
	if len(book.Issuers) != 3 || book.Issuers[0].Issuer != "Federal Republic of Germany" || *book.Issuers[0].PctNLV != 10 {
		t.Fatalf("issuers = %+v", book.Issuers)
	}
	if len(book.Unmeasured) != 2 || book.Unmeasured[0].Symbol != "CORP" || book.Unmeasured[0].Reason != "no evidence" || book.Unmeasured[1].Symbol != "LOST" {
		t.Fatalf("unmeasured = %+v", book.Unmeasured)
	}
	if noNLV := SummarizeBondBook(lines, nil, "EUR"); noNLV.PctNLV != nil || noNLV.RateShockLossPctNLV != nil || noNLV.Issuers[0].PctNLV != nil {
		t.Fatalf("shares without NLV = %+v", noNLV)
	}
}
