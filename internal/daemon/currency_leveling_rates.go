package daemon

import (
	"context"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

// Interest rates for currency leveling (internal-docs/design/currency-leveling.md,
// "Several currencies"; owner decision 2026-10-06 07:11 CEST: rates come from
// the broker's own statements, there are no rate settings). The daily Flex
// statements Canary already keeps for currency reporting carry, per currency,
// the interest accrued that day and the settled cash it accrued on. A loan
// rate is measured over days the currency was borrowed, a cash rate over days
// it held cash.

// currencyLevelingRate is one currency's measured rates: annual decimals, nil
// when not observed, each with the latest statement day it read.
type currencyLevelingRate struct {
	Loan, Cash               *float64
	LoanThrough, CashThrough string
}

const (
	// currencyLevelingRateDays is how many of the latest statement days with
	// the right sign a rate averages over.
	currencyLevelingRateDays = 10
	// currencyLevelingRateLookback bounds how old a statement day may be.
	currencyLevelingRateLookback = 180 * 24 * time.Hour
	// currencyLevelingRateMaxGapDays is the longest gap between two
	// statements whose second accrual is read against the first's balance (a
	// weekend and a holiday); a longer gap means statements are missing.
	currencyLevelingRateMaxGapDays = 4
	// currencyLevelingRateFloorBase is the smallest balance, in base
	// currency, a day is measured on: below it the broker's rounding of a day's
	// interest to the cent outweighs the interest itself.
	currencyLevelingRateFloorBase = 1000.0
	// currencyLevelingRatesTTL is how long measured rates are reused: the
	// statements arrive once a day.
	currencyLevelingRatesTTL = time.Hour
)

// loanRate is what borrowing ccy costs. A currency not borrowed recently
// takes its cash rate as the floor (in one currency a loan costs at least
// what cash earns), so the loan ranks lower and fewer conversions qualify;
// bound reports that stand-in.
func (r currencyLevelingRate) loanRate() (rate float64, through string, bound, ok bool) {
	switch {
	case r.Loan != nil:
		return *r.Loan, r.LoanThrough, false, true
	case r.Cash != nil:
		return *r.Cash, r.CashThrough, true, true
	}
	return 0, "", false, false
}

// cashRate is what ccy's cash earns. A currency that held no cash recently
// takes its loan rate as the ceiling, so it looks dearer to spend; bound
// reports that stand-in.
func (r currencyLevelingRate) cashRate() (rate float64, through string, bound, ok bool) {
	switch {
	case r.Cash != nil:
		return *r.Cash, r.CashThrough, false, true
	case r.Loan != nil:
		return *r.Loan, r.LoanThrough, true, true
	}
	return 0, "", false, false
}

// currencyLevelingRatesFrom measures every currency's loan and cash rate from
// single-day statements: interest accrued over the latest ten days with a
// negative (loan) or positive (cash) settled balance, divided by that balance
// times the calendar days it stood, annualised on 365 days, so currencies with
// 360- and 365-day conventions compare directly. Each day's accrual is read
// against the previous statement's ending settled cash. toBase values a
// balance for the floor. through is the latest statement day read.
func currencyLevelingRatesFrom(statements []flexstmt.Statement, toBase func(string) (float64, bool), now time.Time) (rates map[string]currencyLevelingRate, through string) {
	type day struct {
		at               time.Time
		generated        time.Time
		accrued, settled map[string]float64
	}
	byDay := map[string]day{}
	for _, st := range statements {
		f := st.FX
		if f == nil || !f.SingleDay || f.InterestAccrued == nil || f.SettledEnd == nil {
			continue
		}
		key := st.ToDate.Format("2006-01-02")
		if old, ok := byDay[key]; ok && !st.WhenGenerated.After(old.generated) {
			continue
		}
		byDay[key] = day{at: st.ToDate, generated: st.WhenGenerated, accrued: f.InterestAccrued, settled: f.SettledEnd}
	}
	keys := slices.Sorted(maps.Keys(byDay))
	type obs struct {
		day              string
		accrued, balDays float64
	}
	loans, cash := map[string][]obs{}, map[string][]obs{}
	oldest := now.Add(-currencyLevelingRateLookback)
	for i := 1; i < len(keys); i++ {
		prev, cur := byDay[keys[i-1]], byDay[keys[i]]
		gap := int(math.Round(cur.at.Sub(prev.at).Hours() / 24))
		if gap < 1 || gap > currencyLevelingRateMaxGapDays || cur.at.Before(oldest) {
			continue
		}
		for ccy, accrued := range cur.accrued {
			bal, ok := prev.settled[ccy]
			rate, rateOK := toBase(ccy)
			if !ok || !rateOK || !finiteProtectionOptionPolicyValue(accrued) || math.Abs(bal)*rate < currencyLevelingRateFloorBase {
				continue
			}
			o := obs{day: keys[i], accrued: accrued, balDays: bal * float64(gap)}
			if bal < 0 {
				loans[ccy] = append(loans[ccy], o)
			} else {
				cash[ccy] = append(cash[ccy], o)
			}
		}
	}
	measure := func(list []obs) (*float64, string) {
		if len(list) == 0 {
			return nil, ""
		}
		list = list[max(0, len(list)-currencyLevelingRateDays):]
		var accrued, balDays float64
		for _, o := range list {
			accrued += o.accrued
			balDays += o.balDays
		}
		if balDays == 0 {
			return nil, ""
		}
		r := accrued / balDays * 365
		if !finiteProtectionOptionPolicyValue(r) {
			return nil, ""
		}
		return new(r), list[len(list)-1].day
	}
	seen := map[string]bool{}
	for ccy := range loans {
		seen[ccy] = true
	}
	for ccy := range cash {
		seen[ccy] = true
	}
	rates = map[string]currencyLevelingRate{}
	for _, ccy := range slices.Sorted(maps.Keys(seen)) {
		var r currencyLevelingRate
		r.Loan, r.LoanThrough = measure(loans[ccy])
		r.Cash, r.CashThrough = measure(cash[ccy])
		if r.Loan != nil || r.Cash != nil {
			rates[ccy] = r
		}
		for _, t := range []string{r.LoanThrough, r.CashThrough} {
			through = max(through, t)
		}
	}
	return rates, through
}

// currencyLevelingRatesCache keeps the measured rates for an hour per broker
// scope: reading the statements reads every retained file.
type currencyLevelingRatesCache struct {
	mu      sync.Mutex
	scope   brokerStateScope
	at      time.Time
	rates   map[string]currencyLevelingRate
	through string
	reason  string
}

// currencyLevelingRates returns the measured rates for scope, the latest day
// they read, and why none are available.
func (e *proposalEngine) currencyLevelingRates(ctx context.Context, scope brokerStateScope, toBase func(string) (float64, bool), now time.Time) (map[string]currencyLevelingRate, string, string) {
	if e.levelingRatesForTest != nil {
		return e.levelingRatesForTest(now)
	}
	c := &e.levelingRates
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && sameBrokerScope(c.scope, scope) && now.Sub(c.at) >= 0 && now.Sub(c.at) < currencyLevelingRatesTTL {
		return c.rates, c.through, c.reason
	}
	rates, through, reason := map[string]currencyLevelingRate(nil), "", ""
	if e.server == nil {
		reason = "no daemon is attached, so the broker's statements cannot be read"
	} else if statements, err := e.server.fxStatements(ctx); err != nil {
		reason = "the broker's daily statements are unavailable (" + err.Error() + "), so what each currency costs and earns is unknown"
	} else if rates, through = currencyLevelingRatesFrom(statements, toBase, now); len(rates) == 0 {
		reason = "the broker's daily statements show no interest yet, so what each currency costs and earns is unknown"
	}
	c.scope, c.at, c.rates, c.through, c.reason = scope, now, rates, through, reason
	return rates, through, reason
}
