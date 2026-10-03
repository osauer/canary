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
	r.SetupMatch = new(false)
	r.State = "watching"
	r.Reasons = []string{"no_volume_spike"}
	// Select the newest spike, retaining its own response history. A later
	// recovery never revives an invalidated spike; another spike is a new event.
	spike := -1
	for i, bar := range r.Bars {
		avg := slotAverage(in.Prior, i)
		if avg <= 0 {
			return unknown("baseline_slot_volume_unavailable")
		}
		if avg > 0 && float64(bar.Volume)/avg >= r.Spec.SpikeMultiple {
			spike = i
		}
	}
	if spike < 0 {
		return r
	}
	bar := r.Bars[spike]
	r.SpikeAt = new(bar.End)
	avg := slotAverage(in.Prior, spike)
	multiple := float64(bar.Volume) / avg
	r.Features = rpc.SetupFeatures{SlotVolume: new(bar.Volume), SlotAverage: new(avg), SpikeMultiple: new(multiple)}
	for _, p := range in.Prior {
		b := p.Bars[spike]
		r.Baseline = append(r.Baseline, rpc.SetupBaseline{Date: p.Date, Start: b.Start, End: b.End, Volume: b.Volume})
	}
	r.State = "pending"
	r.Reasons = []string{"volume_spike_waiting_for_price"}
	confirmed := -1
	for i := spike; i < len(r.Bars) && i <= spike+r.Spec.ResponseBars; i++ {
		if i == 0 {
			continue
		}
		if confirmed < 0 && r.Bars[i].Close >= r.Bars[i-1].Close {
			confirmed = i
			r.FirstConfirmedAt = new(r.Bars[i].End)
			r.ConfirmationType = "holding"
			if r.Bars[i].Close > r.Bars[i-1].Close {
				r.ConfirmationType = "rising"
			}
		}
		if confirmed >= 0 && r.Bars[i].Close < r.Bars[i-1].Close {
			r.State = "expired"
			r.Reasons = []string{"price_declined_after_confirmation"}
			return r
		}
	}
	hardEnd := bar.End.Add(time.Duration(r.Spec.ResponseBars+1) * 5 * time.Minute)
	if hardEnd.After(in.Current.Close) {
		hardEnd = in.Current.Close
	}
	if !in.At.Before(hardEnd) || len(r.Bars)-1 > spike+r.Spec.ResponseBars {
		r.State = "expired"
		r.Reasons = []string{"response_window_expired"}
		return r
	}
	valid := latest.End.Add(6 * time.Minute)
	if valid.After(hardEnd) {
		valid = hardEnd
	}
	r.ValidUntil = &valid
	if !in.Historical && !in.ObservedAt.Before(valid) {
		r.State = "expired"
		r.Reasons = []string{"acquisition_missed_deadline"}
		return r
	}
	if confirmed >= 0 {
		r.State = "confirmed"
		r.SetupMatch = new(true)
		r.FirstAvailableAt = new(in.ObservedAt)
		change := (r.Bars[confirmed].Close/r.Bars[confirmed-1].Close - 1) * 100
		if math.IsNaN(change) || math.IsInf(change, 0) {
			return unknown("price_change_not_finite")
		}
		r.Features.PriceChangePct = &change
		r.Reasons = []string{"volume_spike_price_" + r.ConfirmationType}
	}
	return r
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
