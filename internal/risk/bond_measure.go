package risk

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Bond measures (internal-docs/design/bond-risk.md, phase 1): the yield,
// modified duration and loss on a parallel rise in yields of one bill or
// bond, from its clean price per 100, annual coupon and maturity, and the
// book's sums. Measurement only: no threshold reads these yet.

// BondShockPoints is the parallel rise in yields, in percentage points, the
// rate shock loss is stated for: the convention bond risk is read in, not a
// limit.
const BondShockPoints = 1.0

// BondCouponsPerYear is the coupon frequency assumed per currency, until an
// issuer source names it: annual in EUR (Bunds, OATs and most euro bonds),
// semi-annual in USD, GBP and CAD.
func BondCouponsPerYear(currency string) int {
	if strings.EqualFold(strings.TrimSpace(currency), "EUR") {
		return 1
	}
	return 2
}

// BondMeasure is one bond's yield and rate sensitivity at a price.
type BondMeasure struct {
	// YieldPct is the annual yield to maturity in percent, compounded at the
	// coupon frequency.
	YieldPct float64
	// ModifiedDuration is the price's relative fall per point of yield, in
	// years.
	ModifiedDuration float64
	AccruedPer100    float64
	DirtyPer100      float64

	flows          []bondFlow
	couponsPerYear int
	yield          float64
}

type bondFlow struct {
	years  float64
	amount float64
}

// MeasureBond solves the yield to maturity at a clean price and derives the
// modified duration. Coupon dates step back from maturity by the frequency;
// accrued interest runs evenly between them; time is counted in days over
// 365.25. A zero coupon is a single payment at maturity.
func MeasureBond(cleanPer100, couponPct float64, maturity, asOf time.Time, couponsPerYear int) (BondMeasure, error) {
	day := func(t time.Time) time.Time {
		y, m, d := t.UTC().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	maturity, asOf = day(maturity), day(asOf)
	switch {
	case !(cleanPer100 > 0) || math.IsInf(cleanPer100, 0):
		return BondMeasure{}, errors.New("the price is not positive")
	case math.IsNaN(couponPct) || couponPct < 0 || couponPct > 100:
		return BondMeasure{}, errors.New("the coupon is outside 0 to 100 percent")
	case !maturity.After(asOf):
		return BondMeasure{}, errors.New("the bond has matured")
	case couponsPerYear != 1 && couponsPerYear != 2 && couponsPerYear != 4 && couponsPerYear != 12:
		return BondMeasure{}, fmt.Errorf("a coupon frequency of %d a year is not supported", couponsPerYear)
	}
	years := func(t time.Time) float64 { return t.Sub(asOf).Hours() / 24 / 365.25 }
	m := BondMeasure{couponsPerYear: couponsPerYear}
	if couponPct == 0 {
		m.flows = []bondFlow{{years: years(maturity), amount: 100}}
	} else {
		step := 12 / couponsPerYear
		var dates []time.Time
		prev := maturity
		for k := 1; prev.After(asOf); k++ {
			dates = append(dates, prev)
			prev = addMonthsClamped(maturity, -step*k)
		}
		slices.Reverse(dates)
		perCoupon := couponPct / float64(couponsPerYear)
		next := dates[0]
		m.AccruedPer100 = perCoupon * asOf.Sub(prev).Hours() / next.Sub(prev).Hours()
		for _, d := range dates {
			m.flows = append(m.flows, bondFlow{years: years(d), amount: perCoupon})
		}
		m.flows[len(m.flows)-1].amount += 100
	}
	m.DirtyPer100 = cleanPer100 + m.AccruedPer100
	lo, hi := -0.5, 1.0
	if m.price(lo) < m.DirtyPer100 || m.price(hi) > m.DirtyPer100 {
		return BondMeasure{}, fmt.Errorf("no yield between -50%% and 100%% prices it at %.4f", cleanPer100)
	}
	for range 200 {
		mid := (lo + hi) / 2
		if m.price(mid) > m.DirtyPer100 {
			lo = mid
		} else {
			hi = mid
		}
	}
	m.yield = (lo + hi) / 2
	m.YieldPct = m.yield * 100
	price, weighted := 0.0, 0.0
	for _, cf := range m.flows {
		pv := m.discount(cf, m.yield)
		price += pv
		weighted += cf.years * pv
	}
	m.ModifiedDuration = weighted / price / (1 + m.yield/float64(couponsPerYear))
	return m, nil
}

// LossFraction is the share of the bond's value lost if its yield rose by
// points percentage points, by full repricing (convexity included).
func (m BondMeasure) LossFraction(points float64) float64 {
	if len(m.flows) == 0 {
		return 0
	}
	now := m.price(m.yield)
	if !(now > 0) {
		return 0
	}
	return 1 - m.price(m.yield+points/100)/now
}

func (m BondMeasure) price(y float64) float64 {
	p := 0.0
	for _, cf := range m.flows {
		p += m.discount(cf, y)
	}
	return p
}

func (m BondMeasure) discount(cf bondFlow, y float64) float64 {
	f := float64(m.couponsPerYear)
	return cf.amount / math.Pow(1+y/f, f*cf.years)
}

// addMonthsClamped moves t by months, keeping the day of month where the
// target month has it and the month's last day where it does not.
func addMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, 0, 0, 0, 0, t.Location())
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(d, last), 0, 0, 0, 0, t.Location())
}

// BondBookLine is one held bill or bond as the book reads it.
type BondBookLine struct {
	Symbol      string
	Currency    string
	Issuer      string
	IssuerClass string
	// MarketValueBase is the broker's market value in the account base; nil
	// when it could not be converted.
	MarketValueBase *float64
	// RateShockLossBase is the loss on a BondShockPoints rise; nil when the
	// line could not be measured, and Unmeasured then says why.
	RateShockLossBase *float64
	Unmeasured        string
}

// BondBook is the held bonds' risk, summed. Shares of NLV are nil while NLV
// is unknown; Unmeasured names every line whose value or rate risk is
// missing from the sums, which then understate.
type BondBook struct {
	BaseCurrency        string               `json:"base_currency,omitempty"`
	ShockPoints         float64              `json:"shock_points"`
	MarketValueBase     float64              `json:"market_value_base"`
	PctNLV              *float64             `json:"pct_nlv,omitempty"`
	RateShockLossBase   float64              `json:"rate_shock_loss_base"`
	RateShockLossPctNLV *float64             `json:"rate_shock_loss_pct_nlv,omitempty"`
	NonGovernmentBase   float64              `json:"non_government_base"`
	NonGovernmentPctNLV *float64             `json:"non_government_pct_nlv,omitempty"`
	Currencies          []BondBookCurrency   `json:"currencies,omitempty"`
	Issuers             []BondBookIssuer     `json:"issuers,omitempty"`
	Unmeasured          []BondBookUnmeasured `json:"unmeasured,omitempty"`
}

// BondBookCurrency is one currency curve's share of the book; curves are
// never netted against each other.
type BondBookCurrency struct {
	Currency          string  `json:"currency"`
	MarketValueBase   float64 `json:"market_value_base"`
	RateShockLossBase float64 `json:"rate_shock_loss_base"`
}

// BondBookIssuer is one issuer's holding; IssuerClass says whether it is a
// government.
type BondBookIssuer struct {
	Issuer          string   `json:"issuer"`
	IssuerClass     string   `json:"issuer_class,omitempty"`
	MarketValueBase float64  `json:"market_value_base"`
	PctNLV          *float64 `json:"pct_nlv,omitempty"`
}

// BondBookUnmeasured names a line the sums miss and why.
type BondBookUnmeasured struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// Issuer classes of BondBookLine.IssuerClass.
const (
	BondIssuerGovernment      = "government"
	BondIssuerInvestmentGrade = "investment_grade"
)

// SummarizeBondBook sums the lines; nil when no line is held. A line without
// a base value counts nowhere and is named; a line with a value but no rate
// measure counts in the values and is named.
func SummarizeBondBook(lines []BondBookLine, nlvBase *float64, baseCurrency string) *BondBook {
	if len(lines) == 0 {
		return nil
	}
	book := &BondBook{BaseCurrency: strings.ToUpper(strings.TrimSpace(baseCurrency)), ShockPoints: BondShockPoints}
	byCcy := map[string]*BondBookCurrency{}
	byIssuer := map[string]*BondBookIssuer{}
	for _, l := range lines {
		if l.MarketValueBase == nil {
			book.Unmeasured = append(book.Unmeasured, BondBookUnmeasured{Symbol: l.Symbol, Reason: nonEmptyReason(l.Unmeasured, "its value in the base currency is unknown")})
			continue
		}
		value := *l.MarketValueBase
		book.MarketValueBase += value
		ccy := strings.ToUpper(strings.TrimSpace(l.Currency))
		c := byCcy[ccy]
		if c == nil {
			c = &BondBookCurrency{Currency: ccy}
			byCcy[ccy] = c
		}
		c.MarketValueBase += value
		issuer := strings.TrimSpace(l.Issuer)
		if issuer == "" {
			issuer = "unknown issuer"
		}
		i := byIssuer[issuer]
		if i == nil {
			i = &BondBookIssuer{Issuer: issuer, IssuerClass: l.IssuerClass}
			byIssuer[issuer] = i
		}
		i.MarketValueBase += value
		if l.IssuerClass != BondIssuerGovernment {
			book.NonGovernmentBase += value
		}
		if l.RateShockLossBase == nil {
			book.Unmeasured = append(book.Unmeasured, BondBookUnmeasured{Symbol: l.Symbol, Reason: nonEmptyReason(l.Unmeasured, "its rate risk could not be measured")})
			continue
		}
		book.RateShockLossBase += *l.RateShockLossBase
		c.RateShockLossBase += *l.RateShockLossBase
	}
	share := func(v float64) *float64 {
		if nlvBase == nil || !(*nlvBase > 0) {
			return nil
		}
		return new(v / *nlvBase * 100)
	}
	book.PctNLV, book.RateShockLossPctNLV, book.NonGovernmentPctNLV = share(book.MarketValueBase), share(book.RateShockLossBase), share(book.NonGovernmentBase)
	for _, c := range byCcy {
		book.Currencies = append(book.Currencies, *c)
	}
	slices.SortFunc(book.Currencies, func(a, b BondBookCurrency) int {
		return compareDesc(a.MarketValueBase, b.MarketValueBase, a.Currency, b.Currency)
	})
	for _, i := range byIssuer {
		i.PctNLV = share(i.MarketValueBase)
		book.Issuers = append(book.Issuers, *i)
	}
	slices.SortFunc(book.Issuers, func(a, b BondBookIssuer) int {
		return compareDesc(a.MarketValueBase, b.MarketValueBase, a.Issuer, b.Issuer)
	})
	return book
}

func nonEmptyReason(reason, fallback string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	return fallback
}

func compareDesc(a, b float64, an, bn string) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return strings.Compare(an, bn)
}
