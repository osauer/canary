package mandate

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{MaxLossPct: 30, VolTargetPct: 15, GapShockPct: 25, GapRoomPct: 33.3}
}

// syntheticSeries builds n daily closes from a seeded random walk with the
// given annualised volatility, correlated to driver by rho when given.
func syntheticSeries(key string, n int, vol float64, seed uint64, driver []float64, rho float64) Series {
	r := rand.New(rand.NewPCG(seed, seed+1))
	s := Series{Key: key, AsOf: testNow}
	price := 100.0
	day := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		if i > 0 {
			z := r.NormFloat64()
			if driver != nil {
				z = rho*driver[i] + math.Sqrt(1-rho*rho)*z
			}
			price *= math.Exp(vol / math.Sqrt(252) * z)
		}
		s.Dates = append(s.Dates, day.Format("2006-01-02"))
		s.Closes = append(s.Closes, price)
		day = day.AddDate(0, 0, 1)
	}
	return s
}

func driverReturns(n int, seed uint64) []float64 {
	r := rand.New(rand.NewPCG(seed, seed+1))
	out := make([]float64, n)
	for i := range out {
		out[i] = r.NormFloat64()
	}
	return out
}

func knownCapital(t *testing.T, equity, reserve, hwm, flows float64) Capital {
	t.Helper()
	c := EvaluateCapital(testPolicy(), CapitalInput{BaseCurrency: "EUR", Seeded: true, StartedAt: "2026-10-14", EquityBase: equity, EquityAsOf: testNow.Add(-time.Minute), EquityAgeLimit: time.Hour,
		TaxReserveBase: reserve, FlowsSinceStart: flows, HWMAdjustedEquity: hwm, HWMAsOf: testNow.Add(-24 * time.Hour), Now: testNow})
	if !c.Known {
		t.Fatalf("capital unknown: %v", c.Reasons)
	}
	return c
}

func testInput(t *testing.T, legs []Leg) Input {
	t.Helper()
	n := 280
	drv := driverReturns(n, 7)
	series := map[string]Series{
		IndexSymbol: syntheticSeries(IndexSymbol, n, 0.16, 11, drv, 1),
		"SYNA":      syntheticSeries("SYNA", n, 0.30, 12, drv, 0.6),
		"SYNB":      syntheticSeries("SYNB", n, 0.40, 13, drv, 0.3),
	}
	fx := map[string]Series{"USD": syntheticSeries("USD", n, 0.08, 14, nil, 0)}
	return Input{Now: testNow, BaseCurrency: "EUR", Policy: testPolicy(), Capital: knownCapital(t, 300000, 70000, 300000, 0),
		Legs: legs, Series: series, FX: fx, FXNow: map[string]float64{"USD": 0.9}}
}

func TestOptionPriceParityAndIntrinsic(t *testing.T) {
	spot, strike, tt, vol := 100.0, 95.0, 0.5, 0.3
	call, put := OptionPrice(true, spot, strike, tt, vol), OptionPrice(false, spot, strike, tt, vol)
	if parity := call - put - (spot - strike); math.Abs(parity) > 1e-9 {
		t.Fatalf("put-call parity at zero rates broken by %g", parity)
	}
	if deep := OptionPrice(true, 100, 20, 0.1, 0.3); math.Abs(deep-80) > 1e-6 {
		t.Fatalf("deep in-the-money call %g, want ≈ 80", deep)
	}
	if deep := OptionPrice(false, 100, 300, 0.1, 0.3); math.Abs(deep-200) > 1e-6 {
		t.Fatalf("deep in-the-money put %g, want ≈ 200", deep)
	}
	if expired := OptionPrice(true, 100, 90, 0, 0.3); expired != 10 {
		t.Fatalf("call at expiry %g, want intrinsic 10", expired)
	}
	iv, ok := ImpliedVolatility(true, call, spot, strike, tt)
	if !ok || math.Abs(iv-vol) > 1e-5 {
		t.Fatalf("implied volatility %g ok=%v, want %g", iv, ok, vol)
	}
	if _, ok := ImpliedVolatility(true, 4.9, spot, strike, tt); ok {
		t.Fatal("a price below intrinsic solved")
	}
}

func TestCapitalRoomBudgetAndFlows(t *testing.T) {
	p := testPolicy()
	c := knownCapital(t, 300000, 70000, 300000, 0)
	if c.RiskBase != 230000 || c.BudgetBase != 230000 || c.Room != 1 || c.DrawdownPct != 0 || math.Abs(c.BudgetAnnualSD-34500) > 1e-6 {
		t.Fatalf("fresh start: %+v", c)
	}
	// A 10% loss of the risk base: room 1 − 10/30.
	down := knownCapital(t, 277000, 70000, 300000, 0)
	if math.Abs(down.DrawdownPct-10) > 1e-9 || math.Abs(down.Room-(1-10.0/30)) > 1e-9 || down.BudgetBase != 230000 {
		t.Fatalf("10%% drawdown: %+v", down)
	}
	// A withdrawal is not a drawdown but lowers the budget base.
	withdrawn := knownCapital(t, 250000, 70000, 300000, -50000)
	if withdrawn.DrawdownPct != 0 || withdrawn.BudgetBase != 180000 || withdrawn.Room != 1 {
		t.Fatalf("withdrawal: %+v", withdrawn)
	}
	// A changed reserve leaves the drawdown unchanged and moves the base.
	reserve := knownCapital(t, 277000, 100000, 300000, 0)
	if math.Abs(reserve.DrawdownBase-down.DrawdownBase) > 1e-9 || reserve.BudgetBase != 200000 {
		t.Fatalf("reserve change: %+v vs %+v", reserve, down)
	}
	// Room clamps at D and the budget is then zero.
	gone := knownCapital(t, 200000, 70000, 300000, 0)
	if gone.Room != 0 || gone.BudgetAnnualSD != 0 {
		t.Fatalf("at D: %+v", gone)
	}
	if GapCapBase(p, c) != 1*0.30*230000*0.333 {
		t.Fatalf("gap cap %g", GapCapBase(p, c))
	}
	stale := EvaluateCapital(p, CapitalInput{Seeded: true, EquityBase: 300000, EquityAsOf: testNow.Add(-3 * time.Hour), EquityAgeLimit: time.Hour, HWMAdjustedEquity: 300000, Now: testNow})
	if stale.Known || !stale.EquityStale {
		t.Fatalf("stale equity read known: %+v", stale)
	}
	noHorizon := EvaluateCapital(p, CapitalInput{Seeded: true, EquityBase: 300000, EquityAsOf: testNow, HWMAdjustedEquity: 300000, Now: testNow})
	if noHorizon.Known || !strings.Contains(strings.Join(noHorizon.Reasons, " "), "max_equity_age_minutes") {
		t.Fatalf("no age horizon read known: %+v", noHorizon)
	}
	unseeded := EvaluateCapital(p, CapitalInput{EquityBase: 300000, EquityAsOf: testNow, EquityAgeLimit: time.Hour, Now: testNow})
	if unseeded.Known {
		t.Fatal("unseeded capital read known")
	}
}

func TestEvaluateContributionsSumAndVerdict(t *testing.T) {
	iv := 0.35
	legs := []Leg{
		{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 500, Multiplier: 1, Currency: "USD", Spot: 100},
		{ID: "2", Symbol: "SYNB", Underlying: "SYNB", Kind: KindStock, Quantity: 300, Multiplier: 1, Currency: "EUR", Spot: 80},
		{ID: "3", Symbol: "SYNA 110 C", Underlying: "SYNA", Kind: KindOption, Quantity: -2, Multiplier: 100, Currency: "USD", Spot: 100, Right: "C", Strike: 110, TimeToExpiryYears: 0.25, IV: &iv},
		{ID: "4", Symbol: "SPY 90 P", Underlying: "SPY", Kind: KindOption, Quantity: 3, Multiplier: 100, Currency: "USD", Spot: 100, Right: "P", Strike: 90, TimeToExpiryYears: 0.5, IV: &iv},
	}
	in := testInput(t, legs)
	v := Evaluate(in)
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	if v.Scenarios != Scenarios || v.ScenariosFrom == "" || v.ScenariosTo == "" || v.Risk == nil || v.Stresses == nil {
		t.Fatalf("incomplete verdict: %+v", v)
	}
	sum := 0.0
	for _, l := range v.Legs {
		sum += l.ContributionPct
	}
	if math.Abs(sum-100) > 1e-6 {
		t.Fatalf("leg contributions sum to %g, want 100", sum)
	}
	sum = 0
	for _, u := range v.Underlyings {
		sum += u.ContributionPct
	}
	if math.Abs(sum-100) > 1e-6 {
		t.Fatalf("underlying contributions sum to %g, want 100", sum)
	}
	if math.Abs(v.Risk.SDAnnualBase-v.Risk.SD1DBase*math.Sqrt(252)) > 1e-6 || v.Risk.ES99Base <= v.Risk.SD1DBase {
		t.Fatalf("risk figures inconsistent: %+v", v.Risk)
	}
	var spy, syna UnderlyingReport
	for _, u := range v.Underlyings {
		switch u.Underlying {
		case "SPY":
			spy = u
		case "SYNA":
			syna = u
		}
	}
	if spy.Beta != 1 || syna.Beta <= 0.3 || syna.Beta > 1.5 {
		t.Fatalf("betas: SPY %g SYNA %g", spy.Beta, syna.Beta)
	}
	// SYNA carries a short call, so the upside gap is computed; SYNB (long
	// stock only) has none. The gap cap is as specified.
	if syna.GapUpLossBase == nil {
		t.Fatal("short call on SYNA: no upside gap loss")
	}
	wantCap := in.Capital.Room * 0.30 * in.Capital.BudgetBase * 0.333
	if math.Abs(syna.GapCapBase-wantCap) > 1e-6 || math.Abs(v.Stresses.GapCapBase-wantCap) > 1e-6 {
		t.Fatalf("gap cap %g, want %g", syna.GapCapBase, wantCap)
	}
	// A long stock's down gap loses a quarter of its value at the FX rate.
	for _, u := range v.Underlyings {
		if u.Underlying == "SYNB" {
			if u.GapUpLossBase != nil || math.Abs(u.GapDownLossBase-300*80*0.25) > 1e-6 {
				t.Fatalf("SYNB gap: %+v", u)
			}
		}
	}
	if v.Status != StatusWithin {
		t.Fatalf("status %s with risk %.0f vs budget %.0f: %v", v.Status, v.Risk.SDAnnualBase, in.Capital.BudgetAnnualSD, v.Reasons)
	}
	// Ten times the book is over the budget; with the gap cap breached it
	// reads reduce.
	big := in
	big.Legs = nil
	for _, l := range legs {
		l.Quantity *= 10
		big.Legs = append(big.Legs, l)
	}
	bv := Evaluate(big)
	if bv.Status != StatusReduce || len(bv.Stresses.GapBreaches) == 0 {
		t.Fatalf("ten times the book: %s breaches %v", bv.Status, bv.Stresses.GapBreaches)
	}
	// Room 0 makes any risk a reduce, and an empty book stays within.
	noRoom := in
	noRoom.Capital = knownCapital(t, 200000, 70000, 300000, 0)
	if r := Evaluate(noRoom); r.Status != StatusReduce || !math.IsInf(r.Risk.RatioToBudget, 1) {
		t.Fatalf("room 0: %s ratio %g", r.Status, r.Risk.RatioToBudget)
	}
	empty := in
	empty.Legs = nil
	if r := Evaluate(empty); r.Status != StatusWithin || r.Risk.SDAnnualBase != 0 {
		t.Fatalf("empty book: %s %+v", r.Status, r.Risk)
	}
}

func TestEvaluateUnknownOnMissingEvidence(t *testing.T) {
	iv := 0.3
	stock := Leg{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 100}
	option := Leg{ID: "2", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 1, Multiplier: 100, Currency: "USD", Spot: 100, Right: "C", Strike: 100, TimeToExpiryYears: 0.3, IV: &iv}
	cases := map[string]func(in *Input){
		"missing IV":     func(in *Input) { in.Legs[1].IV = nil },
		"missing series": func(in *Input) { delete(in.Series, "SYNA") },
		"stale series":   func(in *Input) { s := in.Series["SYNA"]; s.Stale = true; in.Series["SYNA"] = s },
		"short series": func(in *Input) {
			s := in.Series["SYNA"]
			s.Dates, s.Closes = s.Dates[:200], s.Closes[:200]
			in.Series["SYNA"] = s
		},
		"missing FX":       func(in *Input) { delete(in.FX, "USD") },
		"missing FX rate":  func(in *Input) { delete(in.FXNow, "USD") },
		"unknown capital":  func(in *Input) { in.Capital = Capital{Reasons: []string{"no usable equity observation"}} },
		"daemon reason":    func(in *Input) { in.Reasons = []string{"positions read is not current"} },
		"unsupported kind": func(in *Input) { in.Legs[0].Kind = "future" },
		"missing index":    func(in *Input) { delete(in.Series, IndexSymbol) },
		"few aligned closes": func(in *Input) {
			s := in.Series["SYNA"]
			s.Dates = append([]string{"2000-01-01"}, s.Dates[1:]...)
			in.Series["SYNA"] = s
			s2 := in.Series["SYNB"]
			s2.Dates = append(append([]string{}, s2.Dates[:len(s2.Dates)-30]...), s2.Dates[len(s2.Dates)-30:]...)
			in.Series["SYNB"] = s2
		},
		"option without spot": func(in *Input) { in.Legs[1].Spot = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := testInput(t, []Leg{stock, option})
			if name == "few aligned closes" {
				// Shift one series' dates so that fewer than 253 are shared.
				s := in.Series["SYNA"]
				for i := range s.Dates {
					if i%2 == 0 {
						s.Dates[i] = "1999-" + s.Dates[i][5:]
					}
				}
				in.Series["SYNA"] = s
			} else {
				mutate(&in)
			}
			v := Evaluate(in)
			if v.Status != StatusUnknown || len(v.Reasons) == 0 || v.Risk != nil {
				t.Fatalf("%s: status %s reasons %v", name, v.Status, v.Reasons)
			}
			if name == "missing IV" && !strings.Contains(strings.Join(v.Reasons, " "), "implied volatility") {
				t.Fatalf("missing IV not named: %v", v.Reasons)
			}
		})
	}
}

func TestEvaluateFXConversionAndIncrement(t *testing.T) {
	usd := Leg{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 100}
	in := testInput(t, []Leg{usd})
	v := Evaluate(in)
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	if math.Abs(v.Legs[0].ValueBase-100*100*0.9) > 1e-9 {
		t.Fatalf("USD leg valued at %g EUR, want 9000", v.Legs[0].ValueBase)
	}
	if v.Risk.FXContributionPct == 0 {
		t.Fatal("a USD leg carries no FX contribution")
	}
	// The same leg in base currency carries none, and the local part is all.
	eur := usd
	eur.Currency = "EUR"
	ev := Evaluate(testInput(t, []Leg{eur}))
	if ev.Risk.FXContributionPct != 0 {
		t.Fatalf("EUR leg FX contribution %g", ev.Risk.FXContributionPct)
	}
	// Adding to the line raises the risk; the merged leg is one report.
	more := Evaluate(WithLeg(in, Leg{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 100}))
	if len(more.Legs) != 1 || more.Legs[0].Quantity != 200 || more.Risk.SDAnnualBase <= v.Risk.SDAnnualBase {
		t.Fatalf("increment: %+v risk %g vs %g", more.Legs, more.Risk.SDAnnualBase, v.Risk.SDAnnualBase)
	}
}

func TestShortStockUpsideGapAndIndexStress(t *testing.T) {
	short := Leg{ID: "1", Symbol: "SYNB", Underlying: "SYNB", Kind: KindStock, Quantity: -100, Multiplier: 1, Currency: "EUR", Spot: 50}
	v := Evaluate(testInput(t, []Leg{short}))
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	u := v.Underlyings[0]
	if u.GapUpLossBase == nil || math.Abs(*u.GapUpLossBase-100*50*0.25) > 1e-9 || u.GapDownLossBase >= 0 || u.GapLossBase != *u.GapUpLossBase {
		t.Fatalf("short stock gap: %+v", u)
	}
	// A short of a name with positive beta gains when the index falls, and
	// loses as much when it rises by the same amount.
	if v.Stresses.IndexLossBase >= 0 || math.Abs(v.Stresses.IndexRiseLossBase+v.Stresses.IndexLossBase) > 1e-9 {
		t.Fatalf("index stress on a short: %+v", v.Stresses)
	}
}

// The index rise is the fall's other side (owner, 2026-10-28 13:13 CET): the
// same IndexShockPct up through each name's beta, every leg revalued. The
// index itself moves by exactly the shock; a short call loses more in the
// rise than it gains in the fall.
func TestIndexRiseRevaluesEveryLeg(t *testing.T) {
	index := Leg{ID: "1", Symbol: IndexSymbol, Underlying: IndexSymbol, Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "EUR", Spot: 50}
	v := Evaluate(testInput(t, []Leg{index}))
	if v.Stresses == nil || math.Abs(v.Stresses.IndexLossBase-500) > 1e-9 || math.Abs(v.Stresses.IndexRiseLossBase+500) > 1e-9 {
		t.Fatalf("index line: %+v %v, want 500 lost in the fall and 500 gained in the rise", v.Stresses, v.Reasons)
	}
	iv := 0.3
	shortCall := Leg{ID: "2", Symbol: "SPY 50 C", Underlying: IndexSymbol, Kind: KindOption, Quantity: -1, Multiplier: 100, Currency: "EUR", Spot: 50,
		Right: "C", Strike: 50, TimeToExpiryYears: 0.25, IV: &iv}
	v = Evaluate(testInput(t, []Leg{shortCall}))
	if v.Stresses == nil || v.Stresses.IndexRiseLossBase <= 0 || v.Stresses.IndexRiseLossBase <= -v.Stresses.IndexLossBase {
		t.Fatalf("short call: %+v %v, want the rise's loss above the fall's gain", v.Stresses, v.Reasons)
	}
}

// At a budget of zero (the maximum loss) the ratio is unbounded: it encodes
// as null, which JSON can carry, and reads back unbounded, so a reader never
// falls back to an older verdict for want of an encoding.
func TestRiskRatioEncodesAtZeroBudget(t *testing.T) {
	in := testInput(t, []Leg{{ID: "con:1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 1000, Multiplier: 1, Currency: "EUR", Spot: 100}})
	in.Capital.Room, in.Capital.BudgetAnnualSD = 0, 0
	v := Evaluate(in)
	if v.Status != StatusReduce || v.Risk == nil || !math.IsInf(v.Risk.RatioToBudget, 1) {
		t.Fatalf("verdict %s ratio %+v", v.Status, v.Risk)
	}
	raw, err := json.Marshal(v)
	if err != nil || !strings.Contains(string(raw), `"ratio_to_budget":null`) {
		t.Fatalf("encode: %s %v", raw, err)
	}
	var back Verdict
	if err := json.Unmarshal(raw, &back); err != nil || !math.IsInf(back.Risk.RatioToBudget, 1) || back.Risk.SDAnnualBase != v.Risk.SDAnnualBase {
		t.Fatalf("decode: %+v %v", back.Risk, err)
	}
	// A finite ratio round-trips as the number it is.
	in.Capital.BudgetAnnualSD = 50000
	v = Evaluate(in)
	raw, _ = json.Marshal(v)
	back = Verdict{}
	if err := json.Unmarshal(raw, &back); err != nil || back.Risk.RatioToBudget != v.Risk.RatioToBudget {
		t.Fatalf("finite ratio %v → %v (%v)", v.Risk.RatioToBudget, back.Risk.RatioToBudget, err)
	}
}

// A Micro S&P future is a linear line on the index (the index hedge, owner
// decision 2026-10-28 13:17 CET): SPY's returns give it a beta of 1, it
// takes the index gap, a 10% fall gains a short exactly the shock on its
// contract value, and without a stop it is listed as unstopped and adds
// nothing to the loss measure, as an unstopped share line does.
func TestMicroSP500FutureIsALinearIndexLine(t *testing.T) {
	short := Leg{ID: "con:9", Symbol: "MESZ6", Underlying: MicroSP500Future, Kind: KindStock, Quantity: -2, Multiplier: 5, Currency: "USD", Spot: 6000}
	v := Evaluate(testInput(t, []Leg{short}))
	if v.Stresses == nil {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	gain := 0.10 * 6000 * 5 * 2 * 0.9
	if math.Abs(v.Stresses.IndexLossBase+gain) > 1e-6 || math.Abs(v.Stresses.IndexRiseLossBase-gain) > 1e-6 {
		t.Fatalf("stresses %+v, want %.2f gained in the fall and lost in the rise", v.Stresses, gain)
	}
	u := v.Underlyings[0]
	if !u.BroadIndex || math.Abs(u.Beta-1) > 1e-9 || u.GapShockPct != testPolicy().GapShockPctFor(true) {
		t.Fatalf("underlying %+v, want a broad index line with beta 1", u)
	}
	if v.Loss == nil || v.Loss.TotalBase != 0 || !slices.Contains(v.Loss.Unstopped, "MESZ6") {
		t.Fatalf("loss %+v, want nothing counted and the line unstopped", v.Loss)
	}
	if IsBroadIndex("ES", "USD") || ReturnsSeries("MES", "USD") != "MES" {
		t.Fatal("a share symbol must not match the future's key")
	}
}
