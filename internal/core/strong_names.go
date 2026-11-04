package core

import (
	"cmp"
	"math"
	"slices"
)

// Strategy one, strong names on short dips (owner decisions 2026-11-03
// 13:29 CET). The rule is strong-names-v1, the lab spec s7_strong_names: US
// common shares liquid enough and calm enough, in a rising trend, ranked by
// their 126-session return over SPY's into a watch list of 20 and a leading
// set of 8 (selection only, never an entry gate), entered the session after
// a close 3% to 12% under the 10-session high while the regime gate is on or
// half, in shares, sized to lose 0.5% of the risk base at a stop 2.5 average
// true ranges under the entry, at most 5% of the risk base per name, times
// the regime gate's scale, with one add at a new 20-session high, held under
// a 2.5-ATR trailing stop and out on two closes under the 50-session
// average, at most 8 names, and a name leaving the watch list after 10
// sessions outside the top 20 or on a trend break.
//
// The numbers are the rule's, compiled with it like the core's: a change is
// a new rule and a new spec version, never a policy key. How much of the
// risk budget the strategy may use is the register's risk allocation, which
// is zero until the owner writes one.
//
// Like the rest of the package it holds no state and does no I/O: the
// watch list is walked from the history, so a restart never changes it.

// The rule and the spec it implements.
const (
	StrongNamesRuleID = "strong-names-v1"
	StrongNamesSpecID = "s7_strong_names"
	// StrongNamesSpecHash is the lab registry's hash of the spec as
	// registered on 2026-11-03 (R0).
	StrongNamesSpecHash = "3e395e3b54e1321ab87d49b6059af1d05e0e83e54e8c674882deeea0ee5c090d"
)

// StrongNamesRule is the rule's numbers.
type StrongNamesRule struct {
	MinPrice           float64 `json:"min_price"`
	MinDollarVolume    float64 `json:"min_dollar_volume"`
	LiquiditySessions  int     `json:"liquidity_sessions"`
	MaxVolatility      float64 `json:"max_volatility"`
	VolatilitySessions int     `json:"volatility_sessions"`
	FastAverage        int     `json:"fast_average"`
	SlowAverage        int     `json:"slow_average"`
	StrengthSessions   int     `json:"strength_sessions"`
	WatchSize          int     `json:"watch_size"`
	LeadingSize        int     `json:"leading_size"`
	DipMinPct          float64 `json:"dip_min_pct"`
	DipMaxPct          float64 `json:"dip_max_pct"`
	HighSessions       int     `json:"high_sessions"`
	RiskPct            float64 `json:"risk_pct"`
	ATRMultiple        float64 `json:"atr_multiple"`
	ATRSessions        int     `json:"atr_sessions"`
	MaxNotionalPct     float64 `json:"max_notional_pct"`
	AddHighSessions    int     `json:"add_high_sessions"`
	MaxLots            int     `json:"max_lots"`
	TrendBreakCloses   int     `json:"trend_break_closes"`
	MaxNames           int     `json:"max_names"`
	DropSessions       int     `json:"drop_sessions"`
	JudgeSessions      int     `json:"judge_sessions"`
	WalkSessions       int     `json:"walk_sessions"`
}

// StrongNamesV1 is strong-names-v1 (B1-B9 as decided).
var StrongNamesV1 = StrongNamesRule{
	MinPrice: 10, MinDollarVolume: 20e6, LiquiditySessions: 20, MaxVolatility: 0.70, VolatilitySessions: 20,
	FastAverage: 50, SlowAverage: 200, StrengthSessions: 126, WatchSize: 20, LeadingSize: 8,
	DipMinPct: 3, DipMaxPct: 12, HighSessions: 10, RiskPct: 0.5, ATRMultiple: 2.5, ATRSessions: 20, MaxNotionalPct: 5,
	AddHighSessions: 20, MaxLots: 2, TrendBreakCloses: 2, MaxNames: 8, DropSessions: 10, JudgeSessions: 5,
	// The watch list's walk: long enough for a name to enter and serve its
	// ten sessions outside before the latest one, short enough that the
	// breadth engine's 253 closes give every walked session its
	// 200-session average.
	WalkSessions: 50,
}

// NameSeries is one name's daily closes and volumes, ascending.
type NameSeries struct {
	Dates   []string  `json:"dates"`
	Closes  []float64 `json:"closes"`
	Volumes []float64 `json:"volumes,omitempty"`
}

// NameReading is one name on one session.
type NameReading struct {
	Symbol string `json:"symbol"`
	// Close, DollarVolume (the average of close times volume) and
	// Volatility (annualised, from daily log returns) decide eligibility.
	Close        float64 `json:"close"`
	DollarVolume float64 `json:"dollar_volume"`
	Volatility   float64 `json:"volatility"`
	Eligible     bool    `json:"eligible"`
	Why          string  `json:"why,omitempty"`
	// FastAvg and SlowAvg are the 50- and 200-session averages; Trend is
	// the close above both and the 50 above the 200.
	FastAvg float64 `json:"fast_avg"`
	SlowAvg float64 `json:"slow_avg"`
	Trend   bool    `json:"trend"`
	// Strength is the 126-session return less SPY's, in percent; Rank its
	// place among the eligible names (0: unranked).
	Strength float64 `json:"strength"`
	Rank     int     `json:"rank,omitempty"`
	// High is the highest close of the last ten sessions and DipPct how far
	// the close sits under it.
	High   float64 `json:"high"`
	DipPct float64 `json:"dip_pct"`
}

// readName reads the name at series index i against SPY's return over the
// same sessions. ok is false while the history is too short.
func (r StrongNamesRule) readName(sym string, s NameSeries, i int, spyRet float64) (NameReading, bool) {
	need := max(r.SlowAverage, r.StrengthSessions+1, r.LiquiditySessions, r.VolatilitySessions+1, r.HighSessions)
	if i+1 < need {
		return NameReading{Symbol: sym}, false
	}
	c := s.Closes
	out := NameReading{Symbol: sym, Close: c[i]}
	sum := 0.0
	for j := i + 1 - r.LiquiditySessions; j <= i; j++ {
		v := 0.0
		if j < len(s.Volumes) {
			v = s.Volumes[j]
		}
		sum += c[j] * v
	}
	out.DollarVolume = sum / float64(r.LiquiditySessions)
	var rets []float64
	for j := i + 1 - r.VolatilitySessions; j <= i; j++ {
		rets = append(rets, math.Log(c[j]/c[j-1]))
	}
	out.Volatility = SigmaSample(rets)
	out.FastAvg, out.SlowAvg = mean(c[i+1-r.FastAverage:i+1]), mean(c[i+1-r.SlowAverage:i+1])
	out.Trend = c[i] > out.FastAvg && c[i] > out.SlowAvg && out.FastAvg > out.SlowAvg
	out.Strength = (c[i]/c[i-r.StrengthSessions] - 1 - spyRet) * 100
	out.High = slices.Max(c[i+1-r.HighSessions : i+1])
	out.DipPct = (out.High - c[i]) / out.High * 100
	switch {
	case c[i] < r.MinPrice:
		out.Why = "price under the minimum"
	case out.DollarVolume < r.MinDollarVolume:
		out.Why = "average dollar volume under the minimum"
	case out.Volatility > r.MaxVolatility:
		out.Why = "volatility above the maximum"
	default:
		out.Eligible = true
	}
	return out, true
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// StrongNamesInput is what one screen reads: SPY's closes (its dates are the
// sessions), every name's series, the regime gate and the sleeve.
type StrongNamesInput struct {
	SPY   Series
	Names map[string]NameSeries
	// Gate is the regime gate's state and scale (MeasureGate).
	GateState string
	GateScale float64
	// ATR is the 20-session average true range per name, where the daemon
	// read highs and lows (the leading set only).
	ATR map[string]float64
	// RiskBase is the risk base in the names' currency; AllocationPct the
	// register's risk allocation for the sleeve (zero while unwritten).
	RiskBase      float64
	AllocationPct float64
	// Held is the sleeve's open names with their lots.
	Held map[string]int
}

// WatchEntry is one name on the watch list.
type WatchEntry struct {
	NameReading
	Since   string `json:"since"`
	Outside int    `json:"outside,omitempty"`
	Leading bool   `json:"leading"`
}

// Departure is a name that left the watch list during the walk.
type Departure struct {
	Symbol string `json:"symbol"`
	On     string `json:"on"`
	Why    string `json:"why"`
}

// EntryPlan is the trade plan for one candidate: the trigger,
// the instrument, the stop, the size and why it is what it is.
type EntryPlan struct {
	Symbol string  `json:"symbol"`
	Ref    float64 `json:"ref_close"`
	DipPct float64 `json:"dip_pct"`
	ATR    float64 `json:"atr,omitempty"`
	Stop   float64 `json:"stop,omitempty"`
	// RuleUnits is the rule's size before the allocation: the risk at the
	// stop, capped by the notional, times the regime scale. Units is what
	// the sleeve's allocation lets through: zero while it is zero.
	RuleUnits int    `json:"rule_units"`
	Units     int    `json:"units"`
	Why       string `json:"why"`
	// JudgeAfter is how many sessions after the entry it is judged.
	JudgeAfter int `json:"judge_after"`
}

// StrongNamesScreen is one session's screen.
type StrongNamesScreen struct {
	Rule       string           `json:"rule"`
	Spec       string           `json:"spec"`
	SpecHash   string           `json:"spec_hash"`
	Numbers    StrongNamesRule  `json:"numbers"`
	Session    string           `json:"session"`
	Universe   int              `json:"universe"`
	Eligible   int              `json:"eligible"`
	Watch      []WatchEntry     `json:"watch"`
	Departures []Departure      `json:"departures,omitempty"`
	Entries    []EntryPlan      `json:"entries"`
	Held       []HoldingReading `json:"held,omitempty"`
	Reason     string           `json:"reason,omitempty"`
}

// HoldingReading is an open name's add and exit reading (B7, B8).
type HoldingReading struct {
	Symbol string `json:"symbol"`
	Lots   int    `json:"lots"`
	// Exit is set on a trend break: TrendBreakCloses closes under the
	// 50-session average; the sale goes at the next open.
	Exit string `json:"exit,omitempty"`
	// Add is true at a new 20-session high while the gate is on and the
	// lots are under the maximum; the five-session judgement is the
	// daemon's to check.
	Add bool   `json:"add,omitempty"`
	Why string `json:"why,omitempty"`
}

// Screen runs strong-names-v1 on the latest session of in.SPY.
func (r StrongNamesRule) Screen(in StrongNamesInput) StrongNamesScreen {
	out := StrongNamesScreen{Rule: StrongNamesRuleID, Spec: StrongNamesSpecID, SpecHash: StrongNamesSpecHash, Numbers: r, Universe: len(in.Names)}
	n := len(in.SPY.Dates)
	if n <= r.StrengthSessions+r.WalkSessions {
		out.Reason = "SPY's history is too short for the screen"
		return out
	}
	out.Session = in.SPY.Dates[n-1]
	idx := map[string]map[string]int{}
	for sym, s := range in.Names {
		m := make(map[string]int, len(s.Dates))
		for i, d := range s.Dates {
			if i < len(s.Closes) && s.Closes[i] > 0 {
				m[d] = i
			}
		}
		idx[sym] = m
	}
	symbols := make([]string, 0, len(in.Names))
	for sym := range in.Names {
		symbols = append(symbols, sym)
	}
	slices.Sort(symbols)
	type member struct {
		since   string
		outside int
	}
	members := map[string]*member{}
	var last map[string]NameReading
	for k := n - r.WalkSessions; k < n; k++ {
		d := in.SPY.Dates[k]
		spyRet := in.SPY.Closes[k]/in.SPY.Closes[k-r.StrengthSessions] - 1
		readings := map[string]NameReading{}
		var ranked []NameReading
		for _, sym := range symbols {
			i, ok := idx[sym][d]
			if !ok {
				continue
			}
			nr, ok := r.readName(sym, in.Names[sym], i, spyRet)
			if !ok {
				continue
			}
			readings[sym] = nr
			if nr.Eligible {
				ranked = append(ranked, nr)
			}
		}
		slices.SortFunc(ranked, func(a, b NameReading) int {
			switch {
			case a.Strength > b.Strength:
				return -1
			case a.Strength < b.Strength:
				return 1
			}
			return cmp.Compare(a.Symbol, b.Symbol)
		})
		for i := range ranked {
			nr := readings[ranked[i].Symbol]
			nr.Rank = i + 1
			readings[ranked[i].Symbol] = nr
		}
		for _, sym := range symbols {
			nr, read := readings[sym]
			m := members[sym]
			top := read && nr.Rank > 0 && nr.Rank <= r.WatchSize
			switch {
			case m == nil && top && nr.Trend:
				members[sym] = &member{since: d}
			case m == nil:
			case read && !nr.Trend:
				delete(members, sym)
				out.Departures = append(out.Departures, Departure{Symbol: sym, On: d, Why: "trend broke"})
			case top:
				m.outside = 0
			default:
				m.outside++
				if m.outside >= r.DropSessions {
					delete(members, sym)
					out.Departures = append(out.Departures, Departure{Symbol: sym, On: d, Why: "outside the top 20 for 10 sessions"})
				}
			}
		}
		last = readings
	}
	for _, sym := range symbols {
		if nr, ok := last[sym]; ok && nr.Eligible {
			out.Eligible++
		}
		m := members[sym]
		if m == nil {
			continue
		}
		nr := last[sym]
		out.Watch = append(out.Watch, WatchEntry{NameReading: nr, Since: m.since, Outside: m.outside, Leading: nr.Rank > 0 && nr.Rank <= r.LeadingSize})
	}
	slices.SortFunc(out.Watch, func(a, b WatchEntry) int {
		ra, rb := a.Rank, b.Rank
		if ra == 0 {
			ra = math.MaxInt
		}
		if rb == 0 {
			rb = math.MaxInt
		}
		if ra != rb {
			return cmp.Compare(ra, rb)
		}
		return cmp.Compare(a.Symbol, b.Symbol)
	})
	open := len(in.Held)
	for _, w := range out.Watch {
		if !w.Leading || !w.Trend || in.Held[w.Symbol] > 0 || w.DipPct < r.DipMinPct || w.DipPct > r.DipMaxPct {
			continue
		}
		p := EntryPlan{Symbol: w.Symbol, Ref: w.Close, DipPct: w.DipPct, JudgeAfter: r.JudgeSessions}
		switch {
		case in.GateState != GateOn && in.GateState != GateHalf:
			p.Why = "the regime gate is off: no new entries"
		case open >= r.MaxNames:
			p.Why = "the strategy holds its maximum of names"
		default:
			p.ATR = in.ATR[w.Symbol]
			p.RuleUnits, p.Stop, p.Why = r.size(w.Close, p.ATR, in.RiskBase, in.GateScale)
			p.Units = p.RuleUnits
			if p.RuleUnits > 0 && !(in.AllocationPct > 0) {
				p.Units, p.Why = 0, "the strategy has no risk allocation: the plan is recorded, nothing is bought"
			}
		}
		out.Entries = append(out.Entries, p)
	}
	for _, sym := range symbols {
		if lots := in.Held[sym]; lots > 0 {
			out.Held = append(out.Held, r.hold(sym, in.Names[sym], idx[sym][out.Session], lots, in.GateState))
		}
	}
	return out
}

// size is B6: units losing RiskPct of the risk base at the stop ATRMultiple
// ATRs under the close, capped at MaxNotionalPct of the risk base, times the
// regime scale.
func (r StrongNamesRule) size(close, atr, riskBase, scale float64) (int, float64, string) {
	if !(atr > 0) {
		return 0, 0, "no average true range read: no size"
	}
	if !(riskBase > 0) {
		return 0, 0, "no risk base: no size"
	}
	stop := close - r.ATRMultiple*atr
	byRisk := r.RiskPct / 100 * riskBase / (r.ATRMultiple * atr)
	byCap := r.MaxNotionalPct / 100 * riskBase / close
	units := math.Floor(math.Min(byRisk, byCap) * scale / 100)
	why := "risk at the stop"
	if byCap < byRisk {
		why = "the notional cap per name"
	}
	if units < 1 {
		return 0, stop, why + " sizes under one share"
	}
	return int(units), stop, why
}

// hold is B7 and B8 on an open name at series index i.
func (r StrongNamesRule) hold(sym string, s NameSeries, i, lots int, gate string) HoldingReading {
	out := HoldingReading{Symbol: sym, Lots: lots}
	c := s.Closes
	if i+1 < max(r.FastAverage+r.TrendBreakCloses-1, r.AddHighSessions+1) {
		out.Why = "history too short"
		return out
	}
	under := 0
	for j := i - r.TrendBreakCloses + 1; j <= i; j++ {
		if c[j] < mean(c[j+1-r.FastAverage:j+1]) {
			under++
		}
	}
	if under == r.TrendBreakCloses {
		out.Exit = "two closes under the 50-session average: sell at the next open"
		return out
	}
	newHigh := c[i] > slices.Max(c[i-r.AddHighSessions:i])
	switch {
	case !newHigh:
	case lots >= r.MaxLots:
		out.Why = "at the maximum of lots"
	case gate != GateOn:
		out.Why = "no adds unless the regime gate is on"
	default:
		out.Add, out.Why = true, "a new 20-session high: one add of the same risk if its five-session judgement is not negative"
	}
	return out
}
