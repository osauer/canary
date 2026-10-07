// Package setups evaluates a fixed entry-observation template without I/O.
package setups

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Session contains the calendar window and observed regular-session bars.
type Session struct {
	Date        string
	Open, Close time.Time
	Bars        []rpc.SetupBar
}

// Input freezes exact identity, source acquisition, decision clock and bars.
type Input struct {
	Contract           rpc.ContractParams
	At, ObservedAt     time.Time
	BaselineObservedAt time.Time
	Historical         bool
	Current            Session
	Prior              []Session
}

// Evaluate calculates the setup using only bars completed by At. Policy is separate.
func Evaluate(spec rpc.SetupSpec, in Input) rpc.SetupResult {
	r := rpc.SetupResult{Version: 1, Spec: spec, Contract: in.Contract, EvaluatedAt: in.At, ObservedAt: in.ObservedAt, SessionDate: in.Current.Date, EvidenceKind: "current_observation", State: "unavailable", Reasons: []string{}, Bars: []rpc.SetupBar{}, Baseline: []rpc.SetupBaseline{}, PriceBasis: "IBKR TRADES", VolumeBasis: "IBKR historical trade volume; same source and interval"}
	if in.Historical {
		r.EvidenceKind = "historical_reconstruction"
	}
	r.BaselineObservedAt = in.BaselineObservedAt
	unknown := func(reason string) rpc.SetupResult {
		r.SetupMatch = nil
		r.State = "unavailable"
		r.Reasons = []string{reason}
		return r
	}
	var err error
	r.Spec, err = rpc.NormalizeSetupSpec(spec)
	if err != nil {
		return unknown("invalid_spec: " + err.Error())
	}
	if in.At.IsZero() || in.ObservedAt.IsZero() || in.Contract.ConID <= 0 || in.Current.Open.IsZero() || !in.Current.Close.After(in.Current.Open) {
		return unknown("source_identity_or_clock_missing")
	}
	if !in.Historical && in.ObservedAt.Before(in.At) {
		return unknown("source_precedes_decision")
	}
	if in.At.Before(in.Current.Open) || !in.At.Before(in.Current.Close) {
		return unknown("outside_regular_session")
	}
	for _, b := range in.Current.Bars {
		if !b.End.After(in.At) {
			r.Bars = append(r.Bars, b)
		}
	}
	if len(r.Bars) < 1 {
		return unknown("no_completed_bars")
	}
	if err := ValidateSession(Session{Date: in.Current.Date, Open: in.Current.Open, Close: in.Current.Close, Bars: r.Bars}, false); err != nil {
		return unknown("current_" + err.Error())
	}
	latest := r.Bars[len(r.Bars)-1]
	if in.At.Sub(latest.End) >= 6*time.Minute {
		return unknown("current_bars_stale")
	}
	if len(in.Prior) != r.Spec.BaselineSessions {
		return unknown("baseline_sessions_incomplete")
	}
	seen := map[string]bool{}
	for _, p := range in.Prior {
		if seen[p.Date] || p.Date >= in.Current.Date || !p.Close.Before(in.Current.Open) || p.Close.Sub(p.Open) != in.Current.Close.Sub(in.Current.Open) {
			return unknown("baseline_session_mismatch")
		}
		seen[p.Date] = true
		if err := ValidateSession(p, true); err != nil {
			return unknown("baseline_" + err.Error())
		}
	}
	r.BaselineSessions = len(in.Prior)
	// Hash only selected, known-before-cutoff observations; future bars are not input.
	frozen := in
	frozen.Current.Bars = r.Bars
	b, _ := json.Marshal(struct {
		Spec  rpc.SetupSpec
		Input Input
	}{r.Spec, frozen})
	sum := sha256.Sum256(b)
	r.InputHash = hex.EncodeToString(sum[:])
	avgs := make([]float64, len(r.Bars))
	for i := range r.Bars {
		if avgs[i] = slotAverage(in.Prior, i); avgs[i] <= 0 {
			return unknown("baseline_slot_volume_unavailable")
		}
	}
	r.Usual = usualProfile(in.Current, in.Prior)
	r.RecentSessions = recentSessions(in.Prior, recentSessionCount)
	r.Features.TradeSize, r.Features.TradeSizeUsual, r.Features.TradeSizeBars = recentTradeSize(r.Bars, r.Usual)
	// The trace keeps only the bars where the reading changed; each step holds
	// until the next, so a quiet day is one step.
	var last verdict
	for k := range r.Bars {
		v := decide(r.Spec, r.Bars[:k+1], avgs, r.Bars[k].End, in.Current.Close)
		if k > 0 && v.state == last.state && v.reason == last.reason && v.spike == last.spike && v.confirmed == last.confirmed {
			continue
		}
		last = v
		step := rpc.SetupTraceStep{End: r.Bars[k].End, State: v.state, Reason: v.reason, ConfirmationType: v.confirmation}
		if v.spike >= 0 {
			step.SpikeAt = new(r.Bars[v.spike].End)
		}
		if v.confirmed >= 0 {
			step.FirstConfirmedAt = new(r.Bars[v.confirmed].End)
		}
		r.Trace = append(r.Trace, step)
	}
	v := decide(r.Spec, r.Bars, avgs, in.At, in.Current.Close)
	r.SetupMatch = new(false)
	r.State, r.Reasons = v.state, []string{v.reason}
	if v.spike < 0 {
		return r
	}
	// Select the newest spike, retaining its own response history. A later
	// recovery never revives an invalidated spike; another spike is a new event.
	bar := r.Bars[v.spike]
	r.SpikeAt = new(bar.End)
	r.Features.SlotVolume, r.Features.SlotAverage, r.Features.SpikeMultiple = new(bar.Volume), new(avgs[v.spike]), new(float64(bar.Volume)/avgs[v.spike])
	for _, p := range in.Prior {
		b := p.Bars[v.spike]
		r.Baseline = append(r.Baseline, rpc.SetupBaseline{Date: p.Date, Start: b.Start, End: b.End, Volume: b.Volume})
	}
	if v.confirmed >= 0 {
		r.FirstConfirmedAt = new(r.Bars[v.confirmed].End)
		r.ConfirmationType = v.confirmation
	}
	if v.state == "expired" {
		return r
	}
	valid := latest.End.Add(6 * time.Minute)
	if valid.After(v.hardEnd) {
		valid = v.hardEnd
	}
	r.ValidUntil = &valid
	if !in.Historical && !in.ObservedAt.Before(valid) {
		r.State = "expired"
		r.Reasons = []string{"acquisition_missed_deadline"}
		return r
	}
	if v.state == "confirmed" {
		r.SetupMatch = new(true)
		r.FirstAvailableAt = new(in.ObservedAt)
		change := (r.Bars[v.confirmed].Close/r.Bars[v.confirmed-1].Close - 1) * 100
		if math.IsNaN(change) || math.IsInf(change, 0) {
			return unknown("price_change_not_finite")
		}
		r.Features.PriceChangePct = &change
	}
	return r
}

// verdict is the rule's reading of completed bars at one decision clock,
// before acquisition deadlines: Evaluate adds those, and the trace reads
// each bar's close without them.
type verdict struct {
	state, reason, confirmation string
	spike, confirmed            int
	hardEnd                     time.Time
}

// decide applies volume_turn_v1 to bars completed by at. avgs holds each
// slot's comparable-session mean volume, all positive.
func decide(spec rpc.SetupSpec, bars []rpc.SetupBar, avgs []float64, at, sessionClose time.Time) verdict {
	v := verdict{state: "watching", reason: "no_volume_spike", spike: -1, confirmed: -1}
	for i, bar := range bars {
		if float64(bar.Volume)/avgs[i] >= spec.SpikeMultiple {
			v.spike = i
		}
	}
	if v.spike < 0 {
		return v
	}
	v.state, v.reason = "pending", "volume_spike_waiting_for_price"
	for i := v.spike; i < len(bars) && i <= v.spike+spec.ResponseBars; i++ {
		if i == 0 {
			continue
		}
		if v.confirmed < 0 && bars[i].Close >= bars[i-1].Close {
			v.confirmed, v.confirmation = i, "holding"
			if bars[i].Close > bars[i-1].Close {
				v.confirmation = "rising"
			}
		}
		if v.confirmed >= 0 && bars[i].Close < bars[i-1].Close {
			v.state, v.reason = "expired", "price_declined_after_confirmation"
			return v
		}
	}
	v.hardEnd = bars[v.spike].End.Add(time.Duration(spec.ResponseBars+1) * 5 * time.Minute)
	if v.hardEnd.After(sessionClose) {
		v.hardEnd = sessionClose
	}
	if !at.Before(v.hardEnd) || len(bars)-1 > v.spike+spec.ResponseBars {
		v.state, v.reason = "expired", "response_window_expired"
		return v
	}
	if v.confirmed >= 0 {
		v.state, v.reason = "confirmed", "volume_spike_price_"+v.confirmation
	}
	return v
}

// recentSessionCount is how many of the latest prior sessions travel as
// hourly bars: with today, the five sessions a buildup is read across.
const recentSessionCount = 4

// usualProfile is each slot's comparable-session mean for today's whole
// session. Prior sessions are complete and the same length as today's.
func usualProfile(current Session, prior []Session) []rpc.SetupUsual {
	slots := int(current.Close.Sub(current.Open) / (5 * time.Minute))
	out := make([]rpc.SetupUsual, 0, slots)
	for i := range slots {
		u := rpc.SetupUsual{End: current.Open.Add(time.Duration(i+1) * 5 * time.Minute)}
		var volume, trades float64
		counted := true
		for _, p := range prior {
			volume += float64(p.Bars[i].Volume)
			if p.Bars[i].Trades == nil {
				counted = false
			} else {
				trades += float64(*p.Bars[i].Trades)
			}
		}
		u.Volume = volume / float64(len(prior))
		if counted {
			u.Trades = new(trades / float64(len(prior)))
		}
		out = append(out, u)
	}
	return out
}

// recentSessions aggregates the latest n prior sessions into hourly bars from
// the open; a short final hour keeps its own end.
func recentSessions(prior []Session, n int) []rpc.SetupRecentSession {
	var out []rpc.SetupRecentSession
	for _, p := range prior[max(0, len(prior)-n):] {
		s := rpc.SetupRecentSession{Date: p.Date}
		for start := 0; start < len(p.Bars); start += 12 {
			s.Bars = append(s.Bars, aggregateBars(p.Bars[start:min(start+12, len(p.Bars))]))
		}
		out = append(out, s)
	}
	return out
}

// aggregateBars joins consecutive bars into one; Trades is absent when any
// part lacks a count.
func aggregateBars(bars []rpc.SetupBar) rpc.SetupBar {
	out := rpc.SetupBar{Start: bars[0].Start, End: bars[len(bars)-1].End, Open: bars[0].Open, High: bars[0].High, Low: bars[0].Low, Close: bars[len(bars)-1].Close}
	var trades int64
	counted := true
	for _, b := range bars {
		out.High, out.Low = max(out.High, b.High), min(out.Low, b.Low)
		out.Volume += b.Volume
		if b.Trades == nil {
			counted = false
		} else {
			trades += *b.Trades
		}
	}
	if counted {
		out.Trades = new(trades)
	}
	return out
}

// recentTradeSize is the average trade over the latest hour of completed bars
// and the usual average trade for the same slots, as total volume over total
// trades. Any missing or zero count leaves both unknown.
func recentTradeSize(bars []rpc.SetupBar, usual []rpc.SetupUsual) (*float64, *float64, int) {
	n := min(12, len(bars))
	if n == 0 || len(usual) < len(bars) {
		return nil, nil, 0
	}
	var volume, trades, usualVolume, usualTrades float64
	for i := len(bars) - n; i < len(bars); i++ {
		if bars[i].Trades == nil || usual[i].Trades == nil {
			return nil, nil, 0
		}
		volume += float64(bars[i].Volume)
		trades += float64(*bars[i].Trades)
		usualVolume += usual[i].Volume
		usualTrades += *usual[i].Trades
	}
	if trades <= 0 || usualTrades <= 0 {
		return nil, nil, 0
	}
	return new(volume / trades), new(usualVolume / usualTrades), n
}

func slotAverage(prior []Session, slot int) float64 {
	var total float64
	for _, s := range prior {
		total += float64(s.Bars[slot].Volume)
	}
	return total / float64(len(prior))
}

// ValidateSession rejects gaps, invalid OHLCV and, when complete, a missing tail.
func ValidateSession(s Session, complete bool) error {
	if len(s.Bars) == 0 || len(s.Bars) > 78 {
		return fmt.Errorf("bars_incomplete")
	}
	for i, b := range s.Bars {
		if !b.Start.Equal(s.Open.Add(time.Duration(i)*5*time.Minute)) || !b.End.Equal(b.Start.Add(5*time.Minute)) || b.End.After(s.Close) {
			return fmt.Errorf("bar_gap_or_time_mismatch")
		}
		if b.Volume < 0 || !finitePositive(b.Close) || !finitePositive(b.Open) || !finitePositive(b.High) || !finitePositive(b.Low) || b.High < b.Low || b.High < math.Max(b.Open, b.Close) || b.Low > math.Min(b.Open, b.Close) {
			return fmt.Errorf("bar_values_invalid")
		}
	}
	if complete && !s.Bars[len(s.Bars)-1].End.Equal(s.Close) {
		return fmt.Errorf("bars_incomplete")
	}
	return nil
}
func finitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
