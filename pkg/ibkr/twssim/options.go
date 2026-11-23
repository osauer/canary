package twssim

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Listed equity options. Each option's price is a Black-Scholes value at a
// flat 30 percent volatility and 4 percent rate, computed from its
// underlying's price when it is listed and again whenever that price moves;
// the bid and ask straddle it by Spread. The model and its greeks are the
// simulator's (assumed), not a market's.
const (
	optionVolatility = 0.30
	optionRate       = 0.04
)

func multiplier(in Instrument) float64 {
	if in.Multiplier > 0 {
		return in.Multiplier
	}
	return 1
}

// OptionConID is the contract id the simulator gives an option: 910000 plus
// the underlying's chain index, expiry index and strike index, so the same
// chain always gets the same ids.
func optionConID(chain, expiry, strike int, right string) int {
	id := 910_000 + chain*10_000 + expiry*200 + strike*2
	if right == "P" {
		id++
	}
	return id
}

// DefaultStrikes are nine distinct strikes from 80 to 120 percent of price,
// on a grid of 0.5 below 25, 1 below 100 and 5 above, so a low-priced stock
// gets no repeated or zero strike.
func DefaultStrikes(price float64) []float64 {
	step := 5.0
	switch {
	case price < 25:
		step = 0.5
	case price < 100:
		step = 1
	}
	var out []float64
	for pct := 80; pct <= 120; pct += 5 {
		k := math.Max(step, math.Round(price*float64(pct)/100/step)*step)
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// ListOptions lists calls and puts of a listed stock for every expiry
// (YYYYMMDD) and strike, with multiplier 100 on SMART.
func (b *Book) ListOptions(underConID int, expiries []string, strikes []float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	under, ok := b.instr[underConID]
	if !ok || under.SecType != "STK" {
		return fmt.Errorf("twssim: %d is not a listed stock", underConID)
	}
	chain := 0
	for _, in := range b.instr {
		if in.SecType == "OPT" && in.UnderConID != underConID {
			chain = max(chain, (in.ConID-910_000)/10_000+1)
		}
	}
	listed := map[string]bool{}
	for _, in := range b.instr {
		if in.SecType == "OPT" && in.UnderConID == underConID {
			listed[fmt.Sprintf("%s/%v/%s", in.Expiry, in.Strike, in.Right)] = true
		}
	}
	for ei, expiry := range expiries {
		for si, strike := range strikes {
			for _, right := range []string{"C", "P"} {
				// A contract already listed (a held one) is not listed twice.
				if listed[fmt.Sprintf("%s/%v/%s", expiry, strike, right)] && b.instr[optionConID(chain, ei, si, right)] == nil {
					continue
				}
				in := &Instrument{
					ConID: optionConID(chain, ei, si, right), Symbol: under.Symbol, SecType: "OPT", Exchange: "SMART", PrimaryExch: "",
					Currency: under.Currency, TradingClass: under.Symbol, Expiry: expiry, Strike: strike, Right: right, Multiplier: 100,
					UnderConID: underConID, MinTick: 0.01, Spread: 0.04,
				}
				in.LocalSymbol = fmt.Sprintf("%-6s%s%s%08d", under.Symbol, expiry[2:], right, int(math.Round(strike*1000)))
				in.LongName = fmt.Sprintf("SYNTHETIC %s %s %s %v", under.Symbol, expiry, right, strike)
				b.instr[in.ConID] = in
			}
		}
	}
	b.repriceLocked(underConID)
	return nil
}

// OptionConIDFor finds a listed option by underlying, expiry, strike and right.
func (b *Book) OptionConIDFor(underConID int, expiry string, strike float64, right string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, in := range b.instr {
		if in.SecType == "OPT" && in.UnderConID == underConID && in.Expiry == expiry && in.Strike == strike && in.Right == right {
			return in.ConID, true
		}
	}
	return 0, false
}

// repriceLocked values every option on an underlying at the book's clock.
func (b *Book) repriceLocked(underConID int) {
	under, ok := b.instr[underConID]
	if !ok {
		return
	}
	now := b.clock()
	for _, in := range b.instr {
		if in.SecType != "OPT" || in.UnderConID != underConID {
			continue
		}
		m := optionModelAt(in, under.Price, now)
		in.Price = math.Max(roundTo(m.price, in.MinTick), in.MinTick)
		in.Spread = math.Max(0.02, roundTo(in.Price*0.04, in.MinTick))
	}
}

func (b *Book) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

type optionModel struct{ price, delta, gamma, vega, theta float64 }

func normCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

func normPDF(x float64) float64 { return math.Exp(-x*x/2) / math.Sqrt(2*math.Pi) }

// optionModelAt is the Black-Scholes value and greeks of one option.
func optionModelAt(in *Instrument, spot float64, now time.Time) optionModel {
	exp, err := time.Parse("20060102", in.Expiry)
	if err != nil || spot <= 0 || in.Strike <= 0 {
		return optionModel{}
	}
	t := math.Max(exp.Add(20*time.Hour).Sub(now).Hours()/24/365, 1.0/365/24)
	sq := optionVolatility * math.Sqrt(t)
	d1 := (math.Log(spot/in.Strike) + (optionRate+optionVolatility*optionVolatility/2)*t) / sq
	d2 := d1 - sq
	disc := math.Exp(-optionRate * t)
	m := optionModel{gamma: normPDF(d1) / (spot * sq), vega: spot * normPDF(d1) * math.Sqrt(t) / 100}
	if in.Right == "C" {
		m.price = spot*normCDF(d1) - in.Strike*disc*normCDF(d2)
		m.delta = normCDF(d1)
		m.theta = (-spot*normPDF(d1)*optionVolatility/(2*math.Sqrt(t)) - optionRate*in.Strike*disc*normCDF(d2)) / 365
	} else {
		m.price = in.Strike*disc*normCDF(-d2) - spot*normCDF(-d1)
		m.delta = normCDF(d1) - 1
		m.theta = (-spot*normPDF(d1)*optionVolatility/(2*math.Sqrt(t)) + optionRate*in.Strike*disc*normCDF(-d2)) / 365
	}
	return m
}

// greeks is an option's model at the book's clock and its underlying's price.
func (b *Book) greeks(in Instrument) (optionModel, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	under := b.instr[in.UnderConID]
	if under == nil {
		return optionModel{}, 0
	}
	return optionModelAt(&in, under.Price, b.clock()), under.Price
}

// optionChains are the reqSecDefOptParams rows of an underlying: one per
// trading class, with its expiries and strikes.
func (b *Book) optionChain(underConID int, symbol string) (tradingClass string, expiries []string, strikes []float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, in := range b.instr {
		if in.SecType != "OPT" || (underConID > 0 && in.UnderConID != underConID) || (underConID <= 0 && !strings.EqualFold(in.Symbol, symbol)) {
			continue
		}
		tradingClass = in.TradingClass
		if !slices.Contains(expiries, in.Expiry) {
			expiries = append(expiries, in.Expiry)
		}
		if !slices.Contains(strikes, in.Strike) {
			strikes = append(strikes, in.Strike)
		}
	}
	slices.Sort(expiries)
	slices.Sort(strikes)
	return tradingClass, expiries, strikes
}

// OptionExpiries are five Friday expiries relative to now's UTC date: the
// first Friday strictly after it (one to seven days out, so always inside
// the seven days an option entry refuses: add_call_plan.go reads the expiry
// as midnight UTC and refuses it unless it is after now plus seven days),
// then that Friday plus 2, 4, 8 and 16 weeks (15-21, 29-35, 57-63 and
// 113-119 days out), one in each horizon `add market` offers. No weekday,
// month end or exchange holiday moves which horizon an expiry falls in.
func OptionExpiries(now time.Time) []string {
	day := now.UTC().Truncate(24 * time.Hour)
	short := day.AddDate(0, 0, 1)
	for short.Weekday() != time.Friday {
		short = short.AddDate(0, 0, 1)
	}
	out := []string{short.Format("20060102")}
	for _, weeks := range []int{2, 4, 8, 16} {
		out = append(out, short.AddDate(0, 0, 7*weeks).Format("20060102"))
	}
	return out
}

// OptionsBook is [Default] with option chains on SYNA and SYNB and an
// owner-like option book beside the 100 SYNA shares: three long SYNA calls
// over two expiries, a long SYNA put the shares cover, and a bear put spread
// on SYNB, 15 units (long the 120 put, short the 110 put, one expiry) that the daemon
// groups as a strategy. Every figure is invented.
func OptionsBook(now time.Time) *Book {
	b := Default()
	b.now = func() time.Time { return now }
	exps := OptionExpiries(now)
	_ = b.ListOptions(900001, exps, []float64{40, 45, 50, 55, 60})
	_ = b.ListOptions(900002, exps, []float64{100, 110, 120, 130, 140})
	b.now = nil
	held := []struct {
		under         int
		exp           int
		strike        float64
		right         string
		qty, avgPrice float64
	}{
		{900001, 3, 55, "C", 2, 2.10}, {900001, 2, 60, "C", 1, 0.80}, {900001, 2, 45, "P", 1, 1.30},
		{900002, 3, 120, "P", 15, 7.40}, {900002, 3, 110, "P", -15, 3.60},
	}
	for _, h := range held {
		if id, ok := b.OptionConIDFor(h.under, exps[h.exp], h.strike, h.right); ok {
			b.SetPosition(id, h.qty, h.avgPrice*100)
		}
	}
	return b
}
