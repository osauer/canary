package setups

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func fixture() (rpc.SetupSpec, Input) {
	loc, _ := time.LoadLocation("America/New_York")
	open := time.Date(2026, 9, 30, 9, 30, 0, 0, loc)
	bars := func(start time.Time) []rpc.SetupBar {
		out := make([]rpc.SetupBar, 78)
		for i := range out {
			at := start.Add(time.Duration(i) * 5 * time.Minute)
			out[i] = rpc.SetupBar{Start: at, End: at.Add(5 * time.Minute), Open: 100, High: 102, Low: 98, Close: 100, Volume: 100}
		}
		return out
	}
	in := Input{At: open.Add(25 * time.Minute), ObservedAt: open.Add(25 * time.Minute), Contract: rpc.ContractParams{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD"}, Current: Session{Date: "2026-09-30", Open: open, Close: open.Add(390 * time.Minute), Bars: bars(open)}}
	for i := 1; i <= 20; i++ {
		o := open.AddDate(0, 0, -i)
		in.Prior = append(in.Prior, Session{Date: o.Format("2006-01-02"), Open: o, Close: o.Add(390 * time.Minute), Bars: bars(o)})
	}
	return rpc.SetupSpec{Revision: "test-v1"}, in
}

func TestSetupPriceRatioMustBeFinite(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	in.Current.Bars[1].Close = math.SmallestNonzeroFloat64
	in.Current.Bars[1].Low = math.SmallestNonzeroFloat64
	r := Evaluate(spec, in)
	if r.SetupMatch != nil || r.State != "unavailable" || r.Reasons[0] != "price_change_not_finite" {
		t.Fatal("nonfinite feature survived", r)
	}
}

func TestObservedConfirmationDoesNotBackdate(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	in.Current.Bars[2].Close = 99
	in.Current.Bars[3].Close = 99
	in.At = in.Current.Bars[2].End
	in.ObservedAt = in.At
	pending := Evaluate(spec, in)
	if pending.State != "pending" || pending.SetupMatch == nil || *pending.SetupMatch || pending.FirstConfirmedAt != nil {
		t.Fatalf("premature confirmation: %+v", pending)
	}
	in.At = in.Current.Bars[3].End
	in.ObservedAt = in.At.Add(time.Second)
	r := Evaluate(spec, in)
	if r.State != "confirmed" || r.SetupMatch == nil || !*r.SetupMatch || !r.FirstConfirmedAt.Equal(in.Current.Bars[3].End) || r.ConfirmationType != "holding" || !r.FirstAvailableAt.Equal(in.ObservedAt) {
		t.Fatalf("wrong observed confirmation: %+v", r)
	}
	if !r.ValidUntil.Equal(in.Current.Bars[3].End.Add(6 * time.Minute)) {
		t.Fatal("missing next-bar acquisition deadline")
	}
}

func TestPartialAndFutureBarsCannotAffectDecisionOrHash(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	in.At = in.Current.Bars[2].End.Add(-time.Second)
	in.ObservedAt = in.At
	a := Evaluate(spec, in)
	if a.State != "watching" || len(a.Bars) != 2 {
		t.Fatalf("partial bar leaked: %+v", a)
	}
	in.Current.Bars[2].Close = 100000
	in.Current.Bars[2].Volume = 100000000
	in.Current.Bars[5].Volume = 900000000
	b := Evaluate(spec, in)
	if a.InputHash != b.InputHash || b.State != a.State {
		t.Fatal("future data changed decision")
	}
}

func TestDeclineInvalidatesAndRecoveryDoesNotRevive(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	in.Current.Bars[3].Close = 99
	in.Current.Bars[4].Close = 100
	r := Evaluate(spec, in)
	if r.State != "expired" || r.SetupMatch == nil || *r.SetupMatch || r.FirstConfirmedAt == nil || r.Reasons[0] != "price_declined_after_confirmation" {
		t.Fatalf("revived invalid event: %+v", r)
	}
}

func TestResponseDeadlineAndAcquisitionGrace(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	r := Evaluate(spec, in)
	if r.State != "confirmed" || !r.ValidUntil.Equal(in.Current.Bars[5].End) {
		t.Fatalf("hard deadline lost: %+v", r)
	}
	in.At = in.Current.Bars[5].End
	in.ObservedAt = in.At
	r = Evaluate(spec, in)
	if r.State != "expired" || *r.SetupMatch {
		t.Fatal("response survived hard deadline")
	}
	in.At = in.Current.Bars[4].End.Add(6 * time.Minute)
	in.ObservedAt = in.At
	in.Current.Bars = in.Current.Bars[:5]
	r = Evaluate(spec, in)
	if r.SetupMatch != nil || r.Reasons[0] != "current_bars_stale" {
		t.Fatal("missing refresh treated as current")
	}
}

func TestMissingOrIncompatibleInputsStayUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"gap", func(in *Input) { in.Prior[0].Bars = append(in.Prior[0].Bars[:20], in.Prior[0].Bars[21:]...) }},
		{"same day", func(in *Input) { in.Prior[0].Date = in.Current.Date }},
		{"duplicate", func(in *Input) { in.Prior[1] = in.Prior[0] }},
		{"partial baseline", func(in *Input) { in.Prior = in.Prior[:19] }},
		{"early close", func(in *Input) { in.Prior[0].Close = in.Prior[0].Open.Add(210 * time.Minute) }},
		{"wrong time", func(in *Input) { in.Current.Bars[1].Start = in.Current.Bars[1].Start.Add(time.Minute) }},
		{"negative volume", func(in *Input) { in.Current.Bars[1].Volume = -1 }},
		{"unknown identity", func(in *Input) { in.Contract.ConID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, in := fixture()
			tc.mutate(&in)
			r := Evaluate(spec, in)
			if r.SetupMatch != nil || r.State != "unavailable" {
				t.Fatalf("unknown became false/true: %+v", r)
			}
		})
	}
}

func TestOpeningSpikeNeedsPreviousCompletedClose(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[0].Volume = 300
	in.At = in.Current.Bars[0].End
	in.ObservedAt = in.At
	r := Evaluate(spec, in)
	if r.State != "pending" || r.SetupMatch == nil || *r.SetupMatch || r.FirstConfirmedAt != nil {
		t.Fatal("opening price substituted for completed close")
	}
	in.At = in.Current.Bars[1].End
	in.ObservedAt = in.At
	r = Evaluate(spec, in)
	if r.State != "confirmed" || !r.FirstConfirmedAt.Equal(in.Current.Bars[1].End) {
		t.Fatal("next completed close could not confirm opening spike")
	}
}

func TestSlowAcquisitionCannotPublishExpiredEvidence(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300
	in.ObservedAt = in.At.Add(6 * time.Minute)
	r := Evaluate(spec, in)
	if r.State != "expired" || *r.SetupMatch || r.Reasons[0] != "acquisition_missed_deadline" {
		t.Fatal("slow current read stayed actionable", r)
	}
}

func TestHistoricalResultDoesNotClaimOriginalAvailability(t *testing.T) {
	spec, in := fixture()
	in.Historical = true
	in.Current.Bars[2].Volume = 300
	in.ObservedAt = in.At.Add(24 * time.Hour)
	r := Evaluate(spec, in)
	if r.EvidenceKind != "historical_reconstruction" || !r.FirstAvailableAt.Equal(in.ObservedAt) || r.PolicyChecked {
		t.Fatal("reconstruction gained authority or backdated availability")
	}
}

// sameClock reports whether two optional result clocks name the same instant.
func sameClock(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// A volume run that keeps clearing the spike multiple is re-anchored on every
// completed bar: the newest qualifying bar is the spike, so SpikeAt advances
// bar by bar, and while that bar's close holds or rises against the bar before
// it, the spike bar is its own first confirmation.
func TestContinuingRunSelectsNewestQualifyingBarAdvancesSpikeAtAndConfirmsOnTheSpikeBarWhileClosesHoldOrRise(t *testing.T) {
	spec, in := fixture()
	// Bars 2-5 each trade 3x their slot mean; closes hold, rise, hold, rise.
	run := []struct {
		close float64
		kind  string
	}{2: {100, "holding"}, 3: {101, "rising"}, 4: {101, "holding"}, 5: {102, "rising"}}
	for i := 2; i < len(run); i++ {
		in.Current.Bars[i].Volume = 300
		in.Current.Bars[i].Close = run[i].close
	}
	var previous time.Time
	for i := 2; i < len(run); i++ {
		in.At = in.Current.Bars[i].End
		in.ObservedAt = in.At
		r := Evaluate(spec, in)
		if r.State != "confirmed" || r.SetupMatch == nil || !*r.SetupMatch {
			t.Fatalf("bar %d: holding or rising run did not confirm: %+v", i, r)
		}
		if r.SpikeAt == nil || !r.SpikeAt.Equal(in.Current.Bars[i].End) || !r.SpikeAt.After(previous) {
			t.Fatalf("bar %d: spike is not the newest qualifying bar or did not advance: %v after %v", i, r.SpikeAt, previous)
		}
		if !sameClock(r.FirstConfirmedAt, r.SpikeAt) || r.ConfirmationType != run[i].kind {
			t.Fatalf("bar %d: first confirmation %v (%s), want the spike bar %v (%s)", i, r.FirstConfirmedAt, r.ConfirmationType, r.SpikeAt, run[i].kind)
		}
		if *r.Features.SlotVolume != 300 || !r.Baseline[0].Start.Equal(in.Prior[0].Bars[i].Start) {
			t.Fatalf("bar %d: features or baseline describe an earlier run bar: %+v %+v", i, r.Features, r.Baseline[0])
		}
		previous = *r.SpikeAt
	}
}

// Two adjacent down closes inside a continuing run leave each newest spike
// unconfirmed: the result is pending with SetupMatch false. It is not expired,
// because the earlier confirmed spike is superseded by the newer observation.
func TestContinuingRunWithTwoAdjacentDownClosesIsPendingWithSetupMatchFalse(t *testing.T) {
	spec, in := fixture()
	// Bars 2-5 each trade 3x their slot mean; closes rise twice, then fall twice.
	for i, c := range map[int]float64{2: 101, 3: 102, 4: 101, 5: 100} {
		in.Current.Bars[i].Volume = 300
		in.Current.Bars[i].Close = c
	}
	in.At = in.Current.Bars[3].End
	in.ObservedAt = in.At
	if r := Evaluate(spec, in); r.State != "confirmed" || !sameClock(r.FirstConfirmedAt, r.SpikeAt) {
		t.Fatalf("rising run before the declines did not confirm: %+v", r)
	}
	for _, i := range []int{4, 5} {
		in.At = in.Current.Bars[i].End
		in.ObservedAt = in.At
		r := Evaluate(spec, in)
		if r.State != "pending" || r.SetupMatch == nil || *r.SetupMatch || r.FirstConfirmedAt != nil || r.Reasons[0] != "volume_spike_waiting_for_price" {
			t.Fatalf("bar %d: down close in a run was not pending with setup_match false: %+v", i, r)
		}
		if r.SpikeAt == nil || !r.SpikeAt.Equal(in.Current.Bars[i].End) {
			t.Fatalf("bar %d: spike is not the newest qualifying bar: %v", i, r.SpikeAt)
		}
	}
}

// A reconstruction a day later, from the same bars and decision clock, must
// reproduce the live observation's facts. Only the evidence kind and the
// availability clock differ, by design.
func TestHistoricalReplayReproducesLiveFactsWhileEvidenceKindAndAvailabilityDiffer(t *testing.T) {
	spec, live := fixture()
	live.Current.Bars[2].Volume = 300
	live.Current.Bars[2].Close = 101
	live.Current.Bars[3].Close = 101
	live.Current.Bars[4].Close = 101
	live.ObservedAt = live.At.Add(time.Second)
	replay := live
	replay.Historical = true
	replay.ObservedAt = live.At.Add(24 * time.Hour)
	a, b := Evaluate(spec, live), Evaluate(spec, replay)
	if a.State != "confirmed" || b.State != a.State {
		t.Fatalf("fixture did not confirm in both evaluations: live %s, replay %s", a.State, b.State)
	}
	if !sameClock(a.SpikeAt, b.SpikeAt) || !sameClock(a.FirstConfirmedAt, b.FirstConfirmedAt) || a.ConfirmationType != b.ConfirmationType {
		t.Fatalf("replay moved the event: live %v/%v/%s, replay %v/%v/%s", a.SpikeAt, a.FirstConfirmedAt, a.ConfirmationType, b.SpikeAt, b.FirstConfirmedAt, b.ConfirmationType)
	}
	if !reflect.DeepEqual(a.Features, b.Features) || !reflect.DeepEqual(a.Bars, b.Bars) || !reflect.DeepEqual(a.Baseline, b.Baseline) || !reflect.DeepEqual(a.SetupMatch, b.SetupMatch) {
		t.Fatal("replay changed features, bars, baseline or setup_match")
	}
	if a.EvidenceKind != "current_observation" || b.EvidenceKind != "historical_reconstruction" {
		t.Fatalf("evidence kinds not distinguished: live %s, replay %s", a.EvidenceKind, b.EvidenceKind)
	}
	if !sameClock(a.FirstAvailableAt, &live.ObservedAt) || !sameClock(b.FirstAvailableAt, &replay.ObservedAt) {
		t.Fatalf("availability not bound to each acquisition: live %v, replay %v", a.FirstAvailableAt, b.FirstAvailableAt)
	}
	// InputHash intentionally binds the clocks: At, ObservedAt and Historical
	// are hashed inputs, so equal facts acquired at different times hash
	// differently. It is not a facts hash and must not be used as one.
	if a.InputHash == b.InputHash {
		t.Fatal("input hash no longer binds the acquisition clocks")
	}
}

// withTrades gives every bar of every session the same trade count.
func withTrades(in *Input, current, prior int64) {
	for i := range in.Current.Bars {
		in.Current.Bars[i].Trades = new(current)
	}
	for s := range in.Prior {
		for i := range in.Prior[s].Bars {
			in.Prior[s].Bars[i].Trades = new(prior)
		}
	}
}

func TestTraceReadsEachBarCloseAsEvaluateWould(t *testing.T) {
	spec, in := fixture()
	in.Current.Bars[2].Volume = 300 // 3.0× the slot mean of 100
	in.Current.Bars[2].Close = 99   // the spike bar falls
	in.Current.Bars[3].Close = 99   // the next bar holds
	in.Current.Bars[4].Close = 98   // then falls: the event ends
	in.At = in.Current.Bars[4].End
	in.ObservedAt = in.At
	r := Evaluate(spec, in)
	// Bar 1 reads as bar 0 did, so the trace holds four steps.
	want := []struct {
		bar           int
		state, reason string
	}{
		{0, "watching", "no_volume_spike"},
		{2, "pending", "volume_spike_waiting_for_price"},
		{3, "confirmed", "volume_spike_price_holding"},
		{4, "expired", "price_declined_after_confirmation"},
	}
	if len(r.Trace) != len(want) {
		t.Fatalf("trace has %d steps, want %d: %+v", len(r.Trace), len(want), r.Trace)
	}
	for i, w := range want {
		if step := r.Trace[i]; !step.End.Equal(in.Current.Bars[w.bar].End) || step.State != w.state || step.Reason != w.reason {
			t.Fatalf("trace[%d] = %+v, want bar %d %s/%s", i, step, w.bar, w.state, w.reason)
		}
	}
	// Every bar reads as the latest step at or before it, and that matches a
	// reconstruction at the bar's close.
	for k := range 5 {
		var step rpc.SetupTraceStep
		for _, s := range r.Trace {
			if !s.End.After(in.Current.Bars[k].End) {
				step = s
			}
		}
		at := in
		at.At, at.ObservedAt, at.Historical = in.Current.Bars[k].End, in.Current.Bars[k].End, true
		if e := Evaluate(spec, at); e.State != step.State || e.Reasons[0] != step.Reason {
			t.Fatalf("bar %d reads %s/%s; Evaluate says %s/%v", k, step.State, step.Reason, e.State, e.Reasons)
		}
	}
	if r.Trace[2].SpikeAt == nil || !r.Trace[2].SpikeAt.Equal(in.Current.Bars[2].End) || r.Trace[2].FirstConfirmedAt == nil || !r.Trace[2].FirstConfirmedAt.Equal(in.Current.Bars[3].End) || r.Trace[2].ConfirmationType != "holding" {
		t.Fatalf("confirmed step lost its spike or confirmation: %+v", r.Trace[2])
	}
	if r.Trace[0].SpikeAt != nil {
		t.Fatal("a step before the spike names one")
	}
}

func TestQuietDayTraceIsOneStep(t *testing.T) {
	spec, in := fixture()
	in.At = in.Current.Bars[76].End
	in.ObservedAt = in.At
	if r := Evaluate(spec, in); len(r.Trace) != 1 || r.Trace[0].State != "watching" {
		t.Fatalf("quiet day trace: %+v", r.Trace)
	}
}

func TestUsualCoversTheWholeSessionFromPriorSessionsOnly(t *testing.T) {
	spec, in := fixture()
	withTrades(&in, 20, 10)
	in.Prior[0].Bars[40].Volume = 2100 // slot 40 mean: (19×100 + 2100) / 20 = 200
	in.Prior[3].Bars[41].Trades = nil  // one session without a count: slot 41 trades unknown
	r := Evaluate(spec, in)
	if len(r.Usual) != 78 || !r.Usual[77].End.Equal(in.Current.Close) {
		t.Fatalf("usual covers %d slots", len(r.Usual))
	}
	if r.Usual[40].Volume != 200 || r.Usual[0].Volume != 100 || r.Usual[0].Trades == nil || *r.Usual[0].Trades != 10 {
		t.Fatalf("wrong slot means: %+v %+v", r.Usual[0], r.Usual[40])
	}
	if r.Usual[41].Trades != nil {
		t.Fatal("a slot missing one session's count must not average the rest")
	}
	in.Current.Bars[60].Volume = 999999 // today's later bar is not input
	if again := Evaluate(spec, in); again.Usual[60].Volume != r.Usual[60].Volume {
		t.Fatal("today's future bar changed the usual profile")
	}
}

func TestTradeSizeComparesTheLatestHourWithItsUsualSlots(t *testing.T) {
	spec, in := fixture()
	withTrades(&in, 20, 10) // today 100/20 = 5 per trade; usual 100/10 = 10
	r := Evaluate(spec, in)
	if r.Features.TradeSize == nil || *r.Features.TradeSize != 5 || r.Features.TradeSizeUsual == nil || *r.Features.TradeSizeUsual != 10 || r.Features.TradeSizeBars != 5 {
		t.Fatalf("five completed bars: %+v", r.Features)
	}
	in.At = in.Current.Bars[20].End
	in.ObservedAt = in.At
	if r = Evaluate(spec, in); r.Features.TradeSizeBars != 12 {
		t.Fatalf("window must stop at an hour, got %d bars", r.Features.TradeSizeBars)
	}
	in.Current.Bars[18].Trades = nil
	if r = Evaluate(spec, in); r.Features.TradeSize != nil || r.Features.TradeSizeUsual != nil || r.Features.TradeSizeBars != 0 {
		t.Fatal("a bar without a count must leave trade size unknown")
	}
	_, plain := fixture()
	if r = Evaluate(spec, plain); r.Features.TradeSize != nil {
		t.Fatal("bars without counts produced a trade size")
	}
}

func TestRecentSessionsAreTheLatestFourInHours(t *testing.T) {
	spec, in := fixture()
	withTrades(&in, 20, 10)
	r := Evaluate(spec, in)
	if len(r.RecentSessions) != 4 {
		t.Fatalf("got %d recent sessions", len(r.RecentSessions))
	}
	last := in.Prior[len(in.Prior)-1]
	got := r.RecentSessions[3]
	if got.Date != last.Date || r.RecentSessions[0].Date != in.Prior[16].Date {
		t.Fatalf("recent sessions out of order: %s … %s", r.RecentSessions[0].Date, got.Date)
	}
	// 78 five-minute bars: six full hours and a closing half hour.
	if len(got.Bars) != 7 || got.Bars[0].Volume != 1200 || got.Bars[6].Volume != 600 || !got.Bars[6].End.Equal(last.Close) || *got.Bars[0].Trades != 120 {
		t.Fatalf("wrong hourly bars: %d, %+v, %+v", len(got.Bars), got.Bars[0], got.Bars[6])
	}
	in.Prior[19].Bars[3].Trades = nil
	if r = Evaluate(spec, in); r.RecentSessions[3].Bars[0].Trades != nil || r.RecentSessions[3].Bars[1].Trades == nil {
		t.Fatal("only the hour missing a count should lose its trades")
	}
}

func TestTradeCountsAreHashedFacts(t *testing.T) {
	spec, in := fixture()
	withTrades(&in, 20, 10)
	a := Evaluate(spec, in)
	in.Current.Bars[1].Trades = new(int64(21))
	if b := Evaluate(spec, in); a.InputHash == b.InputHash {
		t.Fatal("a changed trade count kept the input hash")
	}
}
