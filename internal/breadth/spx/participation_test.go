package spx

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestParticipationPreservesLegacyHistoryAndRequiresDatedPairs(t *testing.T) {
	now := time.Date(2026, 9, 8, 22, 0, 0, 0, time.UTC)
	var w ConstituentWindow
	if err := json.Unmarshal([]byte(`{"symbol":"SYNTH","closes":[1,2,3],"last_bar_at":"2026-09-04"}`), &w); err != nil {
		t.Fatal(err)
	}
	legacy := slices.Clone(w.Closes)
	p := computeParticipation([]string{"SYNTH"}, map[string]ConstituentWindow{"SYNTH": w}, "2026-09-04", now)
	if p.CoverageAD != 0 || p.CoverageVolume != 0 || p.AdvancePct != nil {
		t.Fatal("legacy closes fabricated dated pairs")
	}
	// Monday is Labor Day. Friday is the required prior official close.
	w = mergeBars(w, []Bar{{Date: "2026-09-04", Close: 3, Volume: new(int64(10)), ObservedAt: now}, {Date: "2026-09-08", Close: 4, Volume: new(int64(0)), ObservedAt: now}}, "SYNTH")
	if !slices.Equal(w.Closes[:3], legacy) {
		t.Fatal("additive collection discarded legacy history")
	}
	p = computeParticipation([]string{"SYNTH"}, map[string]ConstituentWindow{"SYNTH": w}, "2026-09-08", now)
	if p.Advancing != 1 || p.CoverageVolume != 1 || p.UpVolumePct != nil || *p.AdvancePct != 100 || !p.InputObservedAt.Equal(now) {
		t.Fatalf("holiday pair or measured zero lost: %+v", p)
	}
	w.Bars = w.Bars[1:]
	p = computeParticipation([]string{"SYNTH"}, map[string]ConstituentWindow{"SYNTH": w}, "2026-09-08", now)
	if p.CoverageAD != 0 {
		t.Fatal("missing prior session was bridged")
	}
}

func TestParticipationIndependentCoverageAndMembership(t *testing.T) {
	now := time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC)
	windows := map[string]ConstituentWindow{}
	for i, symbol := range []string{"SYNTH_A", "SYNTH_B", "SYNTH_C"} {
		closes := make([]float64, 20)
		for j := range closes {
			closes[j] = 100
		}
		closes[19] = 101 - float64(i)
		w := ConstituentWindow{LastBarAt: "2026-09-24", Closes: closes, Bars: []Bar{{Date: "2026-09-23", Close: 100, ObservedAt: now}, {Date: "2026-09-24", Close: closes[19], Volume: new(int64(100)), ObservedAt: now}}}
		if i == 2 {
			w.Bars[1].Volume = nil
		}
		windows[symbol] = w
	}
	p := computeParticipation([]string{"SYNTH_C", "SYNTH_A", "SYNTH_B"}, windows, "2026-09-24", now)
	if p.Coverage20 != 3 || p.CoverageAD != 3 || p.CoverageVolume != 2 || p.Advancing != 1 || p.Declining != 1 || p.Unchanged != 1 || *p.AdvancePct != 50 || *p.UpVolumePct != 100 {
		t.Fatalf("independent denominator lost: %+v", p)
	}
	q := computeParticipation([]string{"SYNTH_A", "SYNTH_B", "SYNTH_C"}, windows, "2026-09-24", now)
	if q.MembershipID != p.MembershipID || !slices.Equal(p.Members, []string{"SYNTH_A", "SYNTH_B", "SYNTH_C"}) {
		t.Fatal("universe identity depends on input order")
	}
	q = computeParticipation([]string{"SYNTH_A", "SYNTH_B"}, windows, "2026-09-24", now)
	if q.MembershipID == p.MembershipID {
		t.Fatal("membership change hidden")
	}
}

func TestParticipationRereadKeepsAcquisitionButCorrectionAdvancesIt(t *testing.T) {
	first := time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC)
	b := Bar{Date: "2026-09-24", Close: 100, Volume: new(int64(10)), ObservedAt: first}
	later := b
	later.ObservedAt = first.Add(time.Hour)
	got := mergeParticipationBars([]Bar{b}, []Bar{later})
	if !got[0].ObservedAt.Equal(first) {
		t.Fatal("unchanged reread relabeled original acquisition")
	}
	later.Close = 101
	got = mergeParticipationBars(got, []Bar{later})
	if !got[0].ObservedAt.Equal(later.ObservedAt) || got[0].Close != 101 {
		t.Fatal("corrected revision kept old acquisition")
	}
}
