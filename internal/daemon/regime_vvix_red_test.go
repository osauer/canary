package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// calmRegimeSessions returns n consecutive sessions of the embedded calendar
// from 2026-01-05 with a calm reading on every row the history replay builds.
func calmRegimeSessions(t *testing.T, n int) []regimeHistoryDay {
	t.Helper()
	cal, ny := marketcal.New(), newYorkLocation()
	var days []regimeHistoryDay
	for day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC); len(days) < n; day = day.AddDate(0, 0, 1) {
		noon := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, ny)
		open := true
		for _, market := range []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions} {
			s, err := cal.SessionAt(market, noon)
			if err != nil {
				t.Fatal(err)
			}
			open = open && s.IsOpen
		}
		if open {
			days = append(days, regimeHistoryDay{
				date: day.Format("2006-01-02"), day: day,
				spy: 100, hyg: 80, vix: 15, vix3m: 17, vvix: 85, usdjpy: 150,
				hyOAS: new(3.0), breadth: new(60.0),
			})
		}
	}
	return days
}

// regimeVotes runs the daemon's band rules, streak and eligibility pass,
// cluster combination and stage vote over the days, one evaluation a session
// as the history replay does it, and returns each session's read from row
// first on. Rows before first only feed the lookbacks.
func regimeVotes(t *testing.T, days []regimeHistoryDay, first int) map[int]*rpc.RegimeSnapshotResult {
	t.Helper()
	at, ownDate := regimeReplayInstants(t, days)
	in := &regimeReplayInputs{days: days, fundingLast: -1}
	for _, d := range days {
		in.hyg = append(in.hyg, ibkrlib.HistoricalBar{Date: d.date, Close: d.hyg})
		in.spy = append(in.spy, ibkrlib.HistoricalBar{Date: d.date, Close: d.spy, High: d.spy})
		in.vvix = append(in.vvix, regimeSeriesPoint{Date: d.day, Value: d.vvix})
	}
	streaks := NewStreakStore(t.TempDir()).cloneForRegimeEvaluation()
	out := map[int]*rpc.RegimeSnapshotResult{}
	for i, d := range days {
		in.hyOAS = append(in.hyOAS, regimeSeriesPoint{Date: d.day, Value: *d.hyOAS})
		if i < first {
			continue
		}
		snap := in.snapshot(t.Context(), i, at[i], ownDate[i])
		snap.TapeSessionState, snap.TapeSessionReason, snap.TapeNextOpen = rpc.TapeSessionFor(snap.AsOf)
		policies := (&Server{}).populateStreaksWithStore(snap, streaks)
		vote, votePolicies := regimeReplayVoteOnly(snap, policies)
		regimeReplayStage(vote, votePolicies)
		out[i] = vote
	}
	return out
}

// The VVIX row turns red at 120 and leaves red below 115 (owner decision
// 2026-11-18 12:59 CET; it was 110 and 105). A red on its first session is at
// the gate's day-one depth, so it is eligible at once, and an eligible
// witness red stands. A red that exit hysteresis holds between 115 and 120
// keeps its eligibility and keeps standing until it is released.
func TestVVIXRedLineAt120(t *testing.T) {
	const first = 8
	timeline := func(yenRed bool) map[int]*rpc.RegimeSnapshotResult {
		days := calmRegimeSessions(t, 28)
		for i := 14; i <= 19; i++ {
			days[i].vvix = 113
		}
		days[20].vvix = 115 // five sessions after a 113: at level, not rising
		days[24].vvix = 121
		days[25].vvix = 116
		days[26].vvix = 114
		if yenRed {
			days[24].usdjpy = 150 * 0.975 // the yen 2.5% stronger than seven sessions back
		}
		return regimeVotes(t, days, first)
	}
	read := timeline(false)
	openers := func(i int) []string { return rpc.RegimeWarningOpeners(read[i]) }

	// The baseline has to be quiet, or "opens no warning by itself" says nothing.
	if r := read[13]; r.Lifecycle.Stage != rpc.LifecycleQuiet || len(openers(13)) != 0 ||
		r.VolOfVol.Band != "green" || r.VolOfVol.Freshness == nil || r.VolOfVol.Freshness.Class != rpc.RegimeFreshnessFresh {
		t.Fatalf("calm baseline: stage %s, openers %v, VVIX band %q, freshness %+v", r.Lifecycle.Stage, openers(13), r.VolOfVol.Band, r.VolOfVol.Freshness)
	}

	if r := read[20]; r.VolOfVol.Band != "green" || r.Lifecycle.Stage != rpc.LifecycleQuiet || len(openers(20)) != 0 {
		t.Errorf("VVIX 115, not rising: band %q, stage %s, openers %v; want green, quiet, none", r.VolOfVol.Band, r.Lifecycle.Stage, openers(20))
	}

	r := read[24]
	eligible := r.VolOfVol.Eligibility != nil && r.VolOfVol.Eligibility.Eligible
	if r.VolOfVol.Band != "red" || !eligible || r.VolOfVol.Streak == nil || r.VolOfVol.Streak.Sessions != 1 ||
		r.Lifecycle.Stage != rpc.LifecycleEarlyWarning || !slices.Equal(openers(24), []string{rpc.RegimeWarningEligibleRed}) {
		t.Errorf("VVIX 121 on its first session: band %q, eligible %v, streak %+v, stage %s, openers %v; want red, eligible, one session, early_warning opened by an eligible red",
			r.VolOfVol.Band, eligible, r.VolOfVol.Streak, r.Lifecycle.Stage, openers(24))
	}
	if beside := timeline(true)[24]; beside.USDJPY.Band != "red" || beside.Lifecycle.Stage != rpc.LifecycleConfirmedStress {
		t.Errorf("VVIX 121 beside an eligible yen red: yen band %q, stage %s; want red and confirmed_stress", beside.USDJPY.Band, beside.Lifecycle.Stage)
	}

	// Held red: still an eligible witness red, so it still stands.
	if r := read[25]; r.VolOfVol.Band != "red" || r.Lifecycle.Stage != rpc.LifecycleEarlyWarning ||
		!slices.Equal(openers(25), []string{rpc.RegimeWarningEligibleRed}) {
		t.Errorf("VVIX 116 the session after a red: band %q, stage %s, openers %v; want red held, early_warning opened by an eligible red",
			r.VolOfVol.Band, r.Lifecycle.Stage, openers(25))
	}
	if r := read[26]; r.VolOfVol.Band == "red" || len(openers(26)) != 0 {
		t.Errorf("VVIX 114 after a held red: band %q, openers %v; want the red released and no warning", r.VolOfVol.Band, openers(26))
	}

	if th := read[24].VolOfVol.Thresholds; th == nil || th.Label != "vvix_daily_v3" || th.Red != "VVIX >= 120" {
		t.Errorf("served VVIX thresholds %+v; want label vvix_daily_v3 and red at 120", th)
	}
	if reason := read[24].VolOfVol.BandReason; reason != ">=120 vol-of-vol shock" {
		t.Errorf("VVIX band reason %q, want >=120 vol-of-vol shock", reason)
	}
}
