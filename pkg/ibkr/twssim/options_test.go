package twssim

import (
	"math"
	"slices"
	"testing"
	"time"
)

// The short expiry is inside seven days and every other one beyond it, on
// every weekday, at either side of midnight UTC and across a month end, by
// the rule add call-plan applies: refused unless expiry (midnight UTC) is
// after now plus seven days.
func TestOptionExpiriesHoldTheirHorizonsOnEveryDay(t *testing.T) {
	refused := func(expiry string, now time.Time) bool {
		d, err := time.Parse("20060102", expiry)
		if err != nil {
			t.Fatal(err)
		}
		return !d.After(now.Add(7 * 24 * time.Hour))
	}
	bands := [][2]int{{1, 7}, {15, 21}, {29, 35}, {57, 63}, {113, 119}}
	for _, start := range []time.Time{
		time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), // a Monday
		time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), // Thanksgiving week
		time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC), // across a month and year end
		time.Date(2027, 2, 22, 0, 0, 0, 0, time.UTC),  // across a short month
	} {
		for d := range 7 {
			for _, offset := range []time.Duration{time.Second, 12 * time.Hour, 24*time.Hour - time.Second} {
				now := start.AddDate(0, 0, d).Add(offset)
				exps := OptionExpiries(now)
				if len(exps) != 5 {
					t.Fatalf("%s: %v", now, exps)
				}
				for i, e := range exps {
					exp, _ := time.Parse("20060102", e)
					days := int(exp.Sub(now.UTC().Truncate(24*time.Hour)).Hours() / 24)
					if exp.Weekday() != time.Friday || days < bands[i][0] || days > bands[i][1] {
						t.Fatalf("%s: expiry %d %s is %d days out, want a Friday in %v", now, i, e, days, bands[i])
					}
					if refused(e, now) != (i == 0) {
						t.Fatalf("%s: expiry %s refused=%t", now, e, refused(e, now))
					}
				}
			}
		}
	}
}

// A low-priced stock's default strikes are distinct and positive, and a held
// contract the chain would repeat is listed once.
func TestDefaultStrikesAndHeldTwin(t *testing.T) {
	ks := DefaultStrikes(4.8)
	if len(ks) < 5 || slices.Contains(ks, 0) || len(slices.Compact(slices.Clone(ks))) != len(ks) {
		t.Fatalf("strikes %v", ks)
	}
	b := Default()
	expiry := OptionExpiries(time.Now())[2]
	b.AddInstrument(Instrument{ConID: 960001, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", TradingClass: "SYNA", Expiry: expiry, Strike: 50, Right: "C", Multiplier: 100, UnderConID: 900001, Price: 2})
	if err := b.ListOptions(900001, OptionExpiries(time.Now()), []float64{45, 50, 55}); err != nil {
		t.Fatal(err)
	}
	if got := b.lookupContract(contractQuery{symbol: "SYNA", secType: "OPT", expiry: expiry, strike: 50, right: "C"}); len(got) != 1 || got[0].ConID != 960001 {
		t.Fatalf("SYNA 50 calls at %s: %+v", expiry, got)
	}
}

// An option's bid and ask sit the same distance either side of its price on
// the tick grid, whatever the date and the stock's price: the midpoint a
// client computes from the two sides is the price, a price an order can
// carry. A spread of an odd number of ticks put the midpoint between two
// ticks on some days (the fourth SYNA 50 call every Friday, at 2.66 with
// bid 2.61 and ask 2.72), so a sale priced at the midpoint on the grid sat
// one tick above it and never filled (2026-11-27).
func TestOptionQuotesStraddleThePriceOnEveryDay(t *testing.T) {
	start := time.Date(2026, 11, 23, 0, 30, 0, 0, time.UTC) // a Monday
	for h := range 24 * 14 {
		now := start.Add(time.Duration(h) * time.Hour)
		b := OptionsBook(now)
		b.now = func() time.Time { return now }
		for _, stock := range []float64{44, 48.5, 50, 52, 53, 57.25} {
			b.SetPrice(900001, stock)
			for _, in := range b.instr {
				if in.SecType != "OPT" || in.UnderConID != 900001 {
					continue
				}
				q := quoteOf(*in)
				ticks := math.Round(in.Spread / in.MinTick)
				if math.Abs(q.bid+q.ask-2*in.Price) > 1e-9 || math.Abs(q.ask-q.bid-in.Spread) > 1e-9 || math.Mod(ticks, 2) != 0 || q.bid <= 0 && in.Price > in.MinTick {
					t.Fatalf("%s, SYNA at %v: %s %v%s priced %v quotes %v/%v on a spread of %v", now.Format("Mon 2006-01-02 15:04"), stock, in.Expiry, in.Strike, in.Right, in.Price, q.bid, q.ask, in.Spread)
				}
			}
		}
	}
}
