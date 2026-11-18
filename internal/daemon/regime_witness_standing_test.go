package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A confirmed stress that rests on a term inversion does not drop at the
// close and return at the open. The term row is eligible in its window; after
// the window, on its typed not-due schedule, it keeps standing, so the
// co-signs beside it keep counting: the same evening, the next morning before
// VIX3M's first tick at 09:31, and over the weekend. With the row overdue it
// does not.
func TestConfirmedStressHoldsThroughTheTermRowsNotDueWindow(t *testing.T) {
	days := calmRegimeSessions(t, 16)
	const i = 12
	days[i].vix, days[i].vix3m = 21.2, 20.0 // ratio 1.06: eligible on day one
	days[i].usdjpy = 150 * 0.975            // the yen 2.5% stronger in a week
	days[i].breadth = new(28.0)             // breadth red, eligible on day one
	at, ownDate := regimeReplayInstants(t, days)
	in := &regimeReplayInputs{days: days, fundingLast: -1}
	for _, d := range days {
		in.hyg = append(in.hyg, ibkrlib.HistoricalBar{Date: d.date, Close: d.hyg})
		in.spy = append(in.spy, ibkrlib.HistoricalBar{Date: d.date, Close: d.spy, High: d.spy})
		in.vvix = append(in.vvix, regimeSeriesPoint{Date: d.day, Value: d.vvix})
	}
	streaks := NewStreakStore(t.TempDir()).cloneForRegimeEvaluation()
	evaluate := func(snap *rpc.RegimeSnapshotResult) *rpc.RegimeSnapshotResult {
		snap.TapeSessionState, snap.TapeSessionReason, snap.TapeNextOpen = rpc.TapeSessionFor(snap.AsOf)
		vote, policies := regimeReplayVoteOnly(snap, (&Server{}).populateStreaksWithStore(snap, streaks))
		regimeReplayStage(vote, policies)
		return vote
	}
	for j, d := range days[:i+1] {
		in.hyOAS = append(in.hyOAS, regimeSeriesPoint{Date: d.day, Value: *d.hyOAS})
		if j < 8 {
			continue
		}
		window := evaluate(in.snapshot(t.Context(), j, at[j], ownDate[j]))
		if j == i && (window.Lifecycle.Stage != rpc.LifecycleConfirmedStress || window.VIXTermStructure.Band != "red") {
			t.Fatalf("in the window: stage %s, term band %q, unconfirmed %v; want confirmed_stress on a red term row",
				window.Lifecycle.Stage, window.VIXTermStructure.Band, window.Lifecycle.Unconfirmed)
		}
	}

	// Off VIX3M's window no newer VIX3M can exist. The legs are the frozen
	// last prints and Cboe's dated close vouches for them; every other row
	// serves session i's own reading.
	offWindow := func(late time.Time, crossCheck string) *rpc.RegimeSnapshotResult {
		snap := in.snapshot(t.Context(), i, late, ownDate[i])
		frozen := &rpc.Quality{AsOf: late, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm}
		snap.VIXTermStructure.Status = rpc.RegimeStatusStale
		snap.VIXTermStructure.VIXQuality, snap.VIXTermStructure.VIX3MQuality = frozen, frozen
		snap.VIXTermStructure.VIX3MCrossCheck = crossCheck
		// VVIX holds the newest close its schedule allows: session i's until
		// the next session has closed, then that one's.
		snap.VolOfVol.AsOfDate = days[i].date
		if late.Weekday() == time.Saturday {
			snap.VolOfVol.AsOfDate = days[i+1].date
		}
		snap.Breadth.Envelope.SessionKey = days[i].date
		return evaluate(snap)
	}
	ny := newYorkLocation()
	session := time.Date(days[i].day.Year(), days[i].day.Month(), days[i].day.Day(), 0, 0, 0, 0, ny)

	// At the next open VIX ticks from 09:31 and VIX3M still holds session
	// i's last print: for five minutes that is the prior close, not a gap.
	open := func(late time.Time) *rpc.RegimeSnapshotResult {
		snap := in.snapshot(t.Context(), i, late, ownDate[i])
		snap.VIXTermStructure.VIXQuality = &rpc.Quality{AsOf: late, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm}
		snap.VIXTermStructure.VIX3MQuality = &rpc.Quality{AsOf: session.Add(16*time.Hour + 15*time.Minute), FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm}
		snap.VolOfVol.AsOfDate = days[i].date
		snap.Breadth.Envelope.SessionKey = days[i].date
		return evaluate(snap)
	}
	nextMorning := session.AddDate(0, 0, 1)
	// In time order, so the streak store sees the instants as the daemon would.
	for _, tc := range []struct {
		name   string
		at     time.Time
		atOpen bool
		want   string
	}{
		{"17:30 on the same trading date", at[i].Add(5*time.Hour + 30*time.Minute), false, rpc.LifecycleConfirmedStress},
		{"08:30 the next morning, before VIX3M's window", nextMorning.Add(8*time.Hour + 30*time.Minute), false, rpc.LifecycleConfirmedStress},
		{"09:32 the next morning, before VIX3M's first tick", nextMorning.Add(9*time.Hour + 32*time.Minute), true, rpc.LifecycleConfirmedStress},
		{"09:40 the next morning, still no VIX3M tick", nextMorning.Add(9*time.Hour + 40*time.Minute), true, rpc.LifecycleEarlyWarning},
		{"noon on the Saturday after", session.AddDate(0, 0, 2).Add(12 * time.Hour), false, rpc.LifecycleConfirmedStress},
	} {
		if tc.at.Weekday() == time.Saturday != (tc.name == "noon on the Saturday after") {
			t.Fatalf("%s falls on a %s; the fixture's session %s moved", tc.name, tc.at.Weekday(), days[i].date)
		}
		if tc.atOpen {
			if got := open(tc.at); got.Lifecycle.Stage != tc.want {
				t.Errorf("%s: stage %s, term %+v; want %s", tc.name, got.Lifecycle.Stage, got.VIXTermStructure.Freshness, tc.want)
			}
			continue
		}
		after := offWindow(tc.at, rpc.VIX3MCrossCheckAgree)
		if class := after.VIXTermStructure.Freshness; class == nil || class.Class != rpc.RegimeFreshnessNotDue {
			t.Fatalf("%s the term row reads %+v, want not_due", tc.name, class)
		}
		if after.Lifecycle.Stage != tc.want {
			t.Errorf("%s: stage %s, term eligibility %+v, unconfirmed %v; want %s",
				tc.name, after.Lifecycle.Stage, after.VIXTermStructure.Eligibility, after.Lifecycle.Unconfirmed, tc.want)
		}
		// With the row overdue the latched red drops, and the dark term row
		// beside the red co-signs is the early warning: the rulebook moves from
		// its confirmed bucket to the early-warning one.
		if overdue := offWindow(tc.at, ""); overdue.Lifecycle.Stage != rpc.LifecycleEarlyWarning ||
			bucketRegimeStage(overdue.Lifecycle.Stage) != risk.RegimeBucketEarlyWarning {
			t.Errorf("%s with the term row overdue: stage %s, bucket %q; want early_warning in the early-warning bucket",
				tc.name, overdue.Lifecycle.Stage, bucketRegimeStage(overdue.Lifecycle.Stage))
		}
	}
}

// Every served snapshot says, per row, its role and what its red is worth,
// stamped by the daemon and never persisted. The monitor view passes it
// through, so a reader compiled at another version shows the daemon's verdict.
func TestServedSnapshotCarriesRowStandings(t *testing.T) {
	at := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(at, "standings")
	snapshot.HYGSPYDivergence.Band = "red"
	snapshot.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	raw, _, err := encodeRegimeSnapshotDocument(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "row_standings") {
		t.Fatal("the persisted snapshot carries the response-only row standings")
	}
	view, err := materializeRegimeSnapshotCacheView(regimeSnapshotCacheView{Health: rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityFresh, LastSuccessAt: &at, LastSuccessAgeSeconds: new(int64(0))}}, raw)
	if err != nil {
		t.Fatal(err)
	}
	type standing struct{ Indicator, Name, Role, Red, Note string }
	var served struct {
		RowStandings []standing `json:"row_standings"`
	}
	servedRaw, err := json.Marshal(view.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(servedRaw, &served); err != nil {
		t.Fatal(err)
	}
	byIndicator := map[string]standing{}
	for _, row := range served.RowStandings {
		byIndicator[row.Indicator] = row
	}
	want := standing{Indicator: "hyg_spy", Name: "ETF credit proxy", Role: "co_sign", Red: "context", Note: "Context, not a stress reading on its own"}
	if len(served.RowStandings) != 8 || byIndicator["hyg_spy"] != want || byIndicator["vix_term"].Role != "witness" || byIndicator["vix_term"].Red != "" {
		t.Fatalf("served row standings %+v; want eight rows with the proxy a co-sign whose red is context", served.RowStandings)
	}

	var monitor struct {
		Indicators []struct{ Name, Band, Role, Red, Note string } `json:"indicators"`
	}
	monitorRaw, err := json.Marshal(rpc.CompactRegimeMonitor(view.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(monitorRaw, &monitor); err != nil {
		t.Fatal(err)
	}
	for _, row := range monitor.Indicators {
		if row.Name == "HYG/SPY" && (row.Band != "red" || row.Role != "co_sign" || row.Red != "context" || row.Note != want.Note) {
			t.Fatalf("monitor row %+v; want the served standing beside the red band", row)
		}
	}
}

// The summary words a usable row by its served band and each co-sign by its
// own row: a red held by exit hysteresis reads stressed, not constructive,
// and the proxy red beside a cash red that may not confirm is context.
func TestRegimeSummaryReadsServedBandsRowByRow(t *testing.T) {
	r := coSignRedSnapshot()
	r.HYGSPYDivergence.HYGPrice = new(81.0) // back above its average: calm
	r.VolOfVol.Last = new(116.0)
	annotateCurrentRows(r)
	r.VolOfVol.Band = "red" // held by exit hysteresis since a close at 121
	r.VolOfVol.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	if got := buildRegimeSummary(r).PunchLine; !strings.HasPrefix(got, "vol-of-vol is stressed; ") {
		t.Fatalf("punch line %q; want the held VVIX red named as stressed", got)
	}

	r = coSignRedSnapshot()
	r.CreditSpreads.HYOAS = new(5.8)
	annotateCurrentRows(r)
	r.CreditSpreads.Eligibility = &rpc.RegimeEligibility{Reasons: []string{"data_overdue"}}
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	summary := buildRegimeSummary(r)
	if !strings.HasPrefix(summary.PunchLine, "cash credit spreads is stressed; ") ||
		!strings.HasSuffix(summary.PunchLine, "Context, not a stress reading on its own: ETF credit proxy.") {
		t.Fatalf("punch line %q; want the cash row stressed and the proxy as context", summary.PunchLine)
	}
}

// The row notes read the one role list: each co-sign row carries the role
// sentence and no witness row does.
func TestRowNotesFollowTheRoleList(t *testing.T) {
	notes := map[string]string{
		rpc.RegimeIndicatorVIXTerm: vixTermNotes, rpc.RegimeIndicatorVolOfVol: volOfVolNotes,
		rpc.RegimeIndicatorHYGSPY: hygSpyNotes, rpc.RegimeIndicatorCredit: creditSpreadsNotes,
		rpc.RegimeIndicatorFunding: fundingStressNotes, rpc.RegimeIndicatorUSDJPY: usdJpyNotes,
		rpc.RegimeIndicatorGammaZero: gammaNotes, rpc.RegimeIndicatorBreadth: breadthNotes,
	}
	coSigns := map[string]bool{rpc.RegimeIndicatorHYGSPY: true, rpc.RegimeIndicatorUSDJPY: true, rpc.RegimeIndicatorGammaZero: true, rpc.RegimeIndicatorBreadth: true}
	for indicator, text := range notes {
		if got := strings.Contains(text, "This row is a co-sign"); got != coSigns[indicator] {
			t.Errorf("%s notes carry the co-sign sentence: %v, want %v", indicator, got, coSigns[indicator])
		}
	}
}
