package setups

import (
	"math"
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
