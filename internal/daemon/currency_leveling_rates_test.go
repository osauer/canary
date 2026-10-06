package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

// levelingStatementDay is one synthetic single-day statement: each
// currency's accrual and ending settled cash.
func levelingStatementDay(day time.Time, generated time.Time, accrued, settled map[string]float64) flexstmt.Statement {
	return flexstmt.Statement{AccountID: "DU1234567", FromDate: day, ToDate: day, WhenGenerated: generated,
		FX: &flexstmt.FXSnapshot{SingleDay: true, InterestAccrued: accrued, SettledEnd: settled}}
}

// levelingStatements are weekday statements from 1 September 2026: USD
// borrowed (−20,000) at 5.1% a year on 365 days, EUR cash (+60,000) earning
// 1.4%, a small CHF balance that rounds to nothing. Each day accrues on the
// previous statement's balance for the calendar days since it.
func levelingStatements(days int) []flexstmt.Statement {
	var out []flexstmt.Statement
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	prev := time.Time{}
	for len(out) < days {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			gap := 1.0
			if !prev.IsZero() {
				gap = day.Sub(prev).Hours() / 24
			}
			out = append(out, levelingStatementDay(day, day.Add(30*time.Hour),
				map[string]float64{"USD": -20000 * 0.051 * gap / 365, "EUR": 60000 * 0.014 * gap / 365, "CHF": 0},
				map[string]float64{"USD": -20000, "EUR": 60000, "CHF": 300}))
			prev = day
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func TestCurrencyLevelingRatesFromStatements(t *testing.T) {
	toBase := func(ccy string) (float64, bool) {
		r, ok := levelingRateToBase[ccy]
		return r, ok
	}
	now := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	statements := levelingStatements(25)
	rates, through := currencyLevelingRatesFrom(statements, toBase, now)
	usd, eur := rates["USD"], rates["EUR"]
	if usd.Loan == nil || math.Abs(*usd.Loan-0.051) > 1e-9 || usd.Cash != nil {
		t.Fatalf("USD = %+v, want a 5.1%% loan rate and no cash rate", usd)
	}
	if eur.Cash == nil || math.Abs(*eur.Cash-0.014) > 1e-9 || eur.Loan != nil {
		t.Fatalf("EUR = %+v, want a 1.4%% cash rate", eur)
	}
	if _, ok := rates["CHF"]; ok {
		t.Fatalf("CHF = %+v: a balance below the floor is not measured", rates["CHF"])
	}
	last := statements[len(statements)-1].ToDate.Format("2006-01-02")
	if through != last || usd.LoanThrough != last {
		t.Fatalf("through = %s / %s, want %s", through, usd.LoanThrough, last)
	}
	// A re-fetched day keeps the newest statement only. The last day is
	// Monday 5 October, three calendar days after Friday's statement.
	refetched := statements[len(statements)-1]
	refetched.WhenGenerated = refetched.WhenGenerated.Add(time.Hour)
	refetched.FX = &flexstmt.FXSnapshot{SingleDay: true, InterestAccrued: map[string]float64{"USD": -20000 * 0.06 * 3 / 365}, SettledEnd: map[string]float64{"USD": -20000}}
	rates, _ = currencyLevelingRatesFrom(append(statements, refetched), toBase, now)
	if r := rates["USD"]; r.Loan == nil || *r.Loan <= 0.051 {
		t.Fatalf("USD after a newer statement of the last day = %+v, want it read", r)
	}
	// A gap longer than a holiday weekend is not read against stale balances,
	// and days older than the lookback are not read at all.
	gapped := append(append([]flexstmt.Statement{}, statements[:5]...), statements[15:]...)
	if r, _ := currencyLevelingRatesFrom(gapped, toBase, now); r["USD"].Loan == nil || math.Abs(*r["USD"].Loan-0.051) > 1e-9 {
		t.Fatalf("USD across a gap = %+v, want the gap skipped", r["USD"])
	}
	if r, _ := currencyLevelingRatesFrom(statements, toBase, now.AddDate(1, 0, 0)); len(r) != 0 {
		t.Fatalf("rates a year later = %+v, want none", r)
	}
	if r, through := currencyLevelingRatesFrom(nil, toBase, now); len(r) != 0 || through != "" {
		t.Fatalf("no statements = %+v %s", r, through)
	}
}

func TestCurrencyLevelingRateStandInDirection(t *testing.T) {
	cashOnly := currencyLevelingRate{Cash: new(0.033), CashThrough: "2026-10-05"}
	if rate, _, bound, ok := cashOnly.loanRate(); !ok || !bound || rate != 0.033 {
		t.Fatalf("loan rate from cash = %v %v %v", rate, bound, ok)
	}
	loanOnly := currencyLevelingRate{Loan: new(0.051), LoanThrough: "2026-07-08"}
	if rate, through, bound, ok := loanOnly.cashRate(); !ok || !bound || rate != 0.051 || through != "2026-07-08" {
		t.Fatalf("cash rate from loan = %v %s %v %v", rate, through, bound, ok)
	}
	if _, _, _, ok := (currencyLevelingRate{}).loanRate(); ok {
		t.Fatal("a currency never measured has a rate")
	}
}
