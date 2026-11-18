package daemon

import (
	"context"
	"encoding/csv"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

const (
	// regimeReplayWarmup is the first row the replay votes on. SPY's 52-week
	// high needs 252 bars (the bar fallback in fetchRegimeHYGSPY), the longest
	// lookback of any row; the rows before it only feed the averages.
	regimeReplayWarmup = 251
	// regimeReplayAhead is how far the outcome columns look, in sessions.
	regimeReplayAhead = 20
	// regimeReplayHighWindow and regimeReplayFall define a fall worth
	// recalling: a close this far below the highest close of the prior window.
	regimeReplayHighWindow = 60
	regimeReplayFall       = 0.08

	regimeReplayNoHistory = "no history for this session"
)

// regimeReplaySeries names the two stage series the replay reports.
var regimeReplaySeries = [2]string{"vote only", "as served"}

var regimeReplayStages = []string{
	rpc.LifecycleQuiet, rpc.LifecycleEarlyWarning, rpc.LifecycleConfirmedStress, rpc.LifecyclePanic,
	rpc.LifecycleStabilization, rpc.LifecycleOpportunity, rpc.LifecycleDataQuality,
}

// regimeHistoryDay is one row of the history file. A nil pointer is a blank
// cell: the input has no reading that day.
type regimeHistoryDay struct {
	date                               string
	day                                time.Time
	spy, hyg, vix, vix3m, vvix, usdjpy float64
	hyOAS, cp, tbill, breadth          *float64
}

// regimeReplayInputs holds the history in the shapes the production helpers
// read: closes as daily bars, official values as observations. hyOAS, cp and
// tbill grow as the replay advances and hold the publications so far.
type regimeReplayInputs struct {
	days             []regimeHistoryDay
	hyg, spy         []ibkrlib.HistoricalBar
	vvix             []regimeSeriesPoint
	hyOAS, cp, tbill []regimeSeriesPoint
	fundingLast      int // last row both funding legs reach
}

// regimeReplaySession is one replayed session: the stage in each series, one
// letter per indicator in streakIndicators order (see regimeBandLetter), and,
// for the vote-only series, the headline and the arms that open a warning.
type regimeReplaySession struct {
	row      int
	date     string
	stage    [2]string
	bands    []byte
	headline string
	openers  []string
}

// TestRegimeHistoryReplay is the history harness for the Regime stage vote:
// the daemon's own band rules, streak and eligibility pass, cluster
// combination and stage vote, run once per session over a file of daily
// closes, with a report of how often each stage fired and what SPY did next.
// It is skipped unless CANARY_REGIME_HISTORY names the file (columns date,
// spy, hyg, vix, vix3m, vvix, usdjpy, hy_oas, cp_90d, tbill_3m and
// breadth_pct_above_50dma; a blank cell is an input with no reading):
//
//	CANARY_REGIME_HISTORY=history.csv go test ./internal/daemon -run TestRegimeHistoryReplay -count=1 -v
//
// What the replay assumes, because a daily file cannot say otherwise:
//
//   - A session is evaluated once, at 12:00 New York, with that day's closes
//     standing for the live prints and that day's official values (VVIX, HY
//     OAS, commercial paper, bills, breadth) already published. The daemon
//     sees those a session later.
//   - SPY's 52-week high is its highest close, not its highest trade.
//   - There is no dealer-gamma history, so gamma never has a reading.
//
// It reports two stage series. "As served" is what BuildRegimeLifecycle
// returns when an input with no reading is the unavailable row the daemon
// would serve. "Vote only" leaves such an input out of the vote without
// calling it a data defect (regimeReplayVoteOnly), so the series measures
// the vote and not the reach of the file.
//
// The file is market data and stays outside Git.
func TestRegimeHistoryReplay(t *testing.T) {
	path := os.Getenv("CANARY_REGIME_HISTORY")
	if path == "" {
		t.Skip("CANARY_REGIME_HISTORY unset: the history replay runs only on request")
	}
	days := loadRegimeHistory(t, path)
	if len(days) <= regimeReplayWarmup+regimeReplayAhead {
		t.Fatalf("history has %d rows; the replay needs more than %d", len(days), regimeReplayWarmup+regimeReplayAhead)
	}
	at, ownDate := regimeReplayInstants(t, days)

	in := &regimeReplayInputs{days: days}
	spy := make([]float64, len(days))
	lastCP, lastBill := -1, -1
	for i, d := range days {
		spy[i] = d.spy
		in.hyg = append(in.hyg, ibkrlib.HistoricalBar{Date: d.date, Close: d.hyg})
		in.spy = append(in.spy, ibkrlib.HistoricalBar{Date: d.date, Close: d.spy, High: d.spy})
		in.vvix = append(in.vvix, regimeSeriesPoint{Date: d.day, Value: d.vvix})
		if d.cp != nil {
			lastCP = i
		}
		if d.tbill != nil {
			lastBill = i
		}
	}
	in.fundingLast = min(lastCP, lastBill)

	// The streak state lives in the evaluator the daemon itself uses between
	// publications: the real Tick and Latch, nothing written to disk.
	streaks := NewStreakStore(t.TempDir()).cloneForRegimeEvaluation()
	var sessions []regimeReplaySession
	for i, d := range days {
		if d.hyOAS != nil {
			in.hyOAS = append(in.hyOAS, regimeSeriesPoint{Date: d.day, Value: *d.hyOAS})
		}
		if d.cp != nil {
			in.cp = append(in.cp, regimeSeriesPoint{Date: d.day, Value: *d.cp})
		}
		if d.tbill != nil {
			in.tbill = append(in.tbill, regimeSeriesPoint{Date: d.day, Value: *d.tbill})
		}
		if i < regimeReplayWarmup {
			continue
		}
		snap := in.snapshot(t.Context(), i, at[i], ownDate[i])
		snap.TapeSessionState, snap.TapeSessionReason, snap.TapeNextOpen = rpc.TapeSessionFor(snap.AsOf)
		policies := (&Server{}).populateStreaksWithStore(snap, streaks)
		for _, ind := range streakIndicators {
			// A row with a reading must be current at the replay's instant,
			// or its sessions are not banked and the vote is not the daemon's.
			status, _, _ := regimeDecisionRowView(snap, ind.key())
			if p := policies[ind.key()]; status == rpc.RegimeStatusOK && p.freshness.Class != rpc.RegimeFreshnessFresh {
				t.Fatalf("%s: %s has a reading but is %s at the replay instant %s", d.date, ind.key(), p.freshness.Class, at[i].Format(time.RFC3339))
			}
		}
		vote, votePolicies := regimeReplayVoteOnly(snap, policies)
		s := regimeReplaySession{row: i, date: d.date}
		s.stage[0] = regimeReplayStage(vote, votePolicies)
		s.stage[1] = regimeReplayStage(snap, policies)
		if s.stage[0] == rpc.LifecycleDataQuality {
			t.Errorf("%s: the vote-only series read data_quality, so an input the replay built is not usable", d.date)
		}
		for _, ind := range streakIndicators {
			_, meta, _ := regimeDecisionRowView(vote, ind.key())
			s.bands = append(s.bands, regimeBandLetter(meta))
		}
		s.headline, s.openers = vote.Composite.Verdict, rpc.RegimeWarningOpeners(vote)
		sessions = append(sessions, s)
	}

	var b strings.Builder
	first := sessions[0]
	fmt.Fprintf(&b, "\nRegime history replay of %s: %d rows, %s to %s; %d sessions replayed from %s (the rows before it only feed the averages).\n",
		filepath.Base(path), len(days), days[0].date, days[len(days)-1].date, len(sessions), first.date)
	b.WriteString("A session is evaluated once, at 12:00 New York, on that day's closes and that day's official values.\n")
	b.WriteString("vote only: an input with no reading is left out of the vote. as served: it is the unavailable row the daemon would serve.\n")
	b.WriteString("Headlines and warning openers are those of the vote-only series.\n")
	fmt.Fprintf(&b, "Outcome: SPY over the next %d sessions against that day's close; the last %d sessions of the file have none.\n", regimeReplayAhead, regimeReplayAhead)

	regimeReplayWindow(&b, "whole file", spy, sessions)
	for year := days[first.row].day.Year(); year <= days[len(days)-1].day.Year(); year++ {
		y := strconv.Itoa(year)
		regimeReplayWindow(&b, "year "+y, spy, regimeReplayBetween(sessions, y, y+"-12-31"))
	}
	regimeReplayWindow(&b, "2023-10-31 onward (cash credit spreads exist)", spy, regimeReplayBetween(sessions, "2023-10-31", "9999"))
	regimeReplayRecall(&b, days, spy, sessions)
	regimeReplayDaily(&b, days, sessions)
	t.Log(b.String())
}

// loadRegimeHistory reads the history file into date order.
func loadRegimeHistory(t *testing.T, path string) []regimeHistoryDay {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil || len(records) == 0 {
		t.Fatalf("read history: %d rows, %v", len(records), err)
	}
	column := map[string]int{}
	for i, name := range records[0] {
		column[strings.TrimSpace(name)] = i
	}
	days := make([]regimeHistoryDay, 0, len(records)-1)
	for _, rec := range records[1:] {
		reading := func(name string) *float64 {
			i, ok := column[name]
			if !ok {
				t.Fatalf("history file has no %s column", name)
			}
			raw := strings.TrimSpace(rec[i])
			if raw == "" {
				return nil
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("%s %s: %v", rec[0], name, err)
			}
			return &v
		}
		price := func(name string) float64 {
			v := reading(name)
			if v == nil || *v <= 0 {
				t.Fatalf("%s: %s has no value; the replay needs it on every row", rec[0], name)
			}
			return *v
		}
		date := strings.TrimSpace(rec[column["date"]])
		day, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatal(err)
		}
		days = append(days, regimeHistoryDay{
			date: date, day: day,
			spy: price("spy"), hyg: price("hyg"), vix: price("vix"), vix3m: price("vix3m"),
			vvix: price("vvix"), usdjpy: price("usdjpy"),
			hyOAS: reading("hy_oas"), cp: reading("cp_90d"), tbill: reading("tbill_3m"),
			breadth: reading("breadth_pct_above_50dma"),
		})
	}
	slices.SortFunc(days, func(a, b regimeHistoryDay) int { return strings.Compare(a.date, b.date) })
	return days
}

// regimeReplayInstants gives each session the instant it is evaluated at:
// 12:00 New York, inside every regular and shortened session, where each
// row's cadence rule reads a same-day print as current. The embedded
// calendar does not reach back before 2026 and the cadence rules fail closed
// outside it (the VIX term row reads overdue and never banks a session), so
// an earlier session is evaluated at a stand-in: noon of a session after the
// file ends, a different one for each neighbour so the streak store still
// counts one session per row. Every date in such a snapshot follows the
// stand-in; the file's date is only the label. ownDate reports the sessions
// that kept theirs.
func regimeReplayInstants(t *testing.T, days []regimeHistoryDay) (at []time.Time, ownDate []bool) {
	t.Helper()
	ny := newYorkLocation()
	cal := marketcal.New()
	noon := func(day time.Time) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, ny)
	}
	// inSession reports whether the equity and options calendars are both
	// open; known is false outside their coverage.
	inSession := func(at time.Time) (open, known bool) {
		for _, market := range []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions} {
			s, err := cal.SessionAt(market, at)
			if err != nil || s.State == marketcal.StateUnknown {
				return false, false
			}
			if !s.IsOpen {
				return false, true
			}
		}
		return true, true
	}
	var standIns []time.Time
	for day := days[len(days)-1].day.AddDate(0, 0, 1); ; day = day.AddDate(0, 0, 1) {
		open, known := inSession(noon(day))
		if !known {
			break
		}
		if open {
			standIns = append(standIns, noon(day))
		}
	}
	used := 0
	for _, d := range days {
		open, known := inSession(noon(d.day))
		switch {
		case known && !open:
			t.Fatalf("%s is in the history file but is not a session in the embedded calendar", d.date)
		case known:
			at, ownDate = append(at, noon(d.day)), append(ownDate, true)
		case len(standIns) < 2:
			t.Fatalf("%s is outside the embedded calendar and it has no later sessions to stand in", d.date)
		default:
			at, ownDate = append(at, standIns[used%len(standIns)]), append(ownDate, false)
			used++
		}
	}
	return at, ownDate
}

// snapshot builds the eight rows the daemon would serve for session i,
// evaluated at at. Each row carries the fields its fetcher derives, computed
// with the fetcher's own helpers where it has them; an input with no reading
// is the row its fetcher returns when the source has nothing.
func (in *regimeReplayInputs) snapshot(ctx context.Context, i int, at time.Time, ownDate bool) *rpc.RegimeSnapshotResult {
	d, prev := in.days[i], in.days[i-1]
	today := at.Format("2006-01-02")
	live := &rpc.Quality{AsOf: at, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm}
	snap := &rpc.RegimeSnapshotResult{AsOf: at}

	// fetchRegimeVIXTerm: ratio VIX / VIX3M from two live legs, and VIX's
	// day change against the previous close.
	snap.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, DataType: rpc.MarketDataLive, VIX3MSource: rpc.VIX3MSourceGateway,
		VIX: new(d.vix), VIX3M: new(d.vix3m), Ratio: new(d.vix / d.vix3m),
		VIXPrevClose: new(prev.vix), VIXChangePct: new((d.vix - prev.vix) / prev.vix * 100),
		VIXQuality: live, VIX3MQuality: live,
	}

	// fetchRegimeVolOfVol: the latest official close and its change over
	// five observations.
	snap.VolOfVol = rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Symbol: "VVIX", Last: new(d.vvix), AsOfDate: today}
	if lagged, ok := laggedSeriesPoint(in.vvix[:i+1], 5); ok && lagged.Value > 0 {
		snap.VolOfVol.Change5D = new((d.vvix - lagged.Value) / lagged.Value * 100)
	}

	// fetchRegimeHYGSPY: HYG against the average of its last 50 closes, SPY
	// against the highest of its last 252 bars, and SPY's day change.
	snap.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{
		Status: rpc.RegimeStatusOK, HYGDataType: rpc.MarketDataLive, HYGQuality: live,
		HYGPrice: new(d.hyg), HYG50DMA: new(averageClose(in.hyg[:i+1], 50)),
		SPYPrice: new(d.spy), SPY52WHigh: new(maxHigh(in.spy[:i+1], 252)),
		SPYPrevClose: new(prev.spy), SPYChange: new(d.spy - prev.spy), SPYChangePct: new((d.spy - prev.spy) / prev.spy * 100),
	}

	// fetchRegimeCreditSpreads: the latest HY OAS and its change over 20
	// observations. The band reads nothing else, so IG OAS is left out.
	snap.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusError, ErrorMessage: regimeReplayNoHistory}
	if d.hyOAS != nil {
		snap.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: d.hyOAS, AsOfDate: today}
		if lagged, ok := laggedSeriesPoint(in.hyOAS, 20); ok {
			snap.CreditSpreads.HY20DChange = new(*d.hyOAS - lagged.Value)
		}
	}

	// fetchRegimeFundingStress itself, over the publications up to the
	// session. It joins the sparse commercial-paper leg to the bill leg by
	// date, so it runs only where the session keeps its own date.
	snap.FundingStress = rpc.RegimeFundingStress{Status: rpc.RegimeStatusError, ErrorMessage: regimeReplayNoHistory}
	if ownDate && i <= in.fundingLast {
		snap.FundingStress = fetchRegimeFundingStress(ctx, &regimeDeps{
			officialSeries: func(_ context.Context, id string) ([]regimeSeriesPoint, error) {
				if id == fredSeriesCP3M {
					return in.cp, nil
				}
				return in.tbill, nil
			},
			now: func() time.Time { return at },
		})
	}

	// fetchRegimeUSDJPY: the change against the close seven bars back.
	week := in.days[i-7].usdjpy
	snap.USDJPY = rpc.RegimeUSDJPY{
		Status: rpc.RegimeStatusOK, Symbol: "USD.JPY", DataType: rpc.MarketDataLive, LastQuality: live,
		Last: new(d.usdjpy), Close7DAgo: new(week), WeeklyChange: new((d.usdjpy - week) / week * 100),
	}

	// fetchRegimeGamma with nothing cached: there is no gamma history.
	snap.GammaZero = rpc.RegimeGammaZero{
		Status:   rpc.RegimeStatusUnavailable,
		Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold},
	}

	// fetchRegimeBreadth: the session's own reading, or a cold engine.
	snap.Breadth = rpc.RegimeBreadth{Status: rpc.RegimeStatusUnavailable, Envelope: rpc.BreadthSPXResult{State: rpc.BreadthStateCold}}
	if d.breadth != nil {
		snap.Breadth = rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK, PctAbove50DMA: *d.breadth,
			Envelope: rpc.BreadthSPXResult{State: rpc.BreadthStateReady, PctAbove50DMA: *d.breadth, SessionKey: today, AsOf: at},
		}
	}
	return snap
}

// regimeReplayVoteOnly presents every input with no reading as a healthy row
// that ranks nothing: status ok, current, unranked, and no band held over
// from its last reading. BuildRegimeLifecycle then votes on the inputs that
// have readings, and its required-input gate has nothing to object to. The
// assumption is that a missing input would have added neither a red nor a
// green: without a cash credit read the credit cluster has no stress witness,
// and without gamma or breadth a witness has one co-sign fewer.
func regimeReplayVoteOnly(snap *rpc.RegimeSnapshotResult, policies map[string]regimeRowPolicy) (*rpc.RegimeSnapshotResult, map[string]regimeRowPolicy) {
	vote, votePolicies := *snap, maps.Clone(policies)
	silent := regimeRowPolicy{freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}}
	noReading := func(status string) bool {
		return status != rpc.RegimeStatusOK && status != rpc.RegimeStatusStale
	}
	if noReading(vote.CreditSpreads.Status) {
		vote.CreditSpreads, votePolicies[StreakKeyCredit] = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK}, silent
	}
	if noReading(vote.FundingStress.Status) {
		vote.FundingStress, votePolicies[StreakKeyFunding] = rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK}, silent
	}
	if noReading(vote.GammaZero.Status) {
		vote.GammaZero, votePolicies[StreakKeyGammaZero] = rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK}, silent
	}
	if noReading(vote.Breadth.Status) {
		vote.Breadth, votePolicies[StreakKeyBreadth] = rpc.RegimeBreadth{Status: rpc.RegimeStatusOK}, silent
	}
	return &vote, votePolicies
}

// regimeReplayStage finishes a snapshot the way acquireRegimeSnapshot does
// after its policy pass, in the same order, and returns the stage. That
// function reads the wall clock and a broker connection, so the replay calls
// what it calls.
func regimeReplayStage(snap *rpc.RegimeSnapshotResult, policies map[string]regimeRowPolicy) string {
	annotateRegimeMetadata(snap, policies)
	snap.Composite = buildRegimeComposite(snap)
	snap.Summary = buildRegimeSummary(snap)
	snap.DataQuality = regimeSnapshotDataQuality(snap)
	snap.SourceHealth = rpc.BuildRegimeSourceHealth(snap, snap.AsOf)
	snap.Lifecycle = rpc.BuildRegimeLifecycle(snap)
	snap.Composite.Verdict = rpc.RegimeHeadline(snap.Composite, snap.Lifecycle.Stage)
	return snap.Lifecycle.Stage
}

// regimeBandLetter is one row's served band as a letter: g green, y yellow,
// r red, R red and eligible to confirm, - no reading.
func regimeBandLetter(meta rpc.RegimeIndicatorMeta) byte {
	switch meta.Band {
	case "green":
		return 'g'
	case "yellow":
		return 'y'
	case "red":
		if meta.Eligibility != nil && meta.Eligibility.Eligible {
			return 'R'
		}
		return 'r'
	}
	return '-'
}

// regimeStageRank orders the stages that warn: early_warning, then
// confirmed_stress, then panic. Every other stage is no warning.
func regimeStageRank(stage string) int {
	return 1 + slices.Index([]string{rpc.LifecycleEarlyWarning, rpc.LifecycleConfirmedStress, rpc.LifecyclePanic}, stage)
}

func regimeReplayBetween(sessions []regimeReplaySession, from, to string) []regimeReplaySession {
	var out []regimeReplaySession
	for _, s := range sessions {
		if s.date >= from && s.date <= to {
			out = append(out, s)
		}
	}
	return out
}

func regimeReplayShare(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

// regimeReplayWindow prints the three tables for one stretch of sessions:
// stages, indicators, and what SPY did next.
func regimeReplayWindow(b *strings.Builder, name string, spy []float64, sessions []regimeReplaySession) {
	if len(sessions) == 0 {
		return
	}
	fmt.Fprintf(b, "\n== %s: %d sessions, %s to %s ==\n", name, len(sessions), sessions[0].date, sessions[len(sessions)-1].date)

	fmt.Fprintf(b, "%-18s %15s %5s %15s %5s\n", "stage", regimeReplaySeries[0], "runs", regimeReplaySeries[1], "runs")
	for _, stage := range regimeReplayStages {
		fmt.Fprintf(b, "%-18s", stage)
		for series := range regimeReplaySeries {
			n, runs := 0, 0
			for k, s := range sessions {
				if s.stage[series] != stage {
					continue
				}
				n++
				if k == 0 || sessions[k-1].stage[series] != stage {
					runs++
				}
			}
			fmt.Fprintf(b, " %15s %5d", regimeReplayShare(n, len(sessions)), runs)
		}
		b.WriteByte('\n')
	}

	// What a quiet session still says in its headline, and which arms opened
	// each early warning, both as shares of all sessions in the window.
	share := func(title, column string, keep func(regimeReplaySession) []string) {
		count, alone := map[string]int{}, map[string]int{}
		for _, s := range sessions {
			labels := keep(s)
			for _, label := range labels {
				count[label]++
				if len(labels) == 1 {
					alone[label]++
				}
			}
		}
		if len(count) == 0 {
			return
		}
		fmt.Fprintln(b, strings.TrimRight(fmt.Sprintf("%-44s %8s %8s %13s", title, "sessions", "share", column), " "))
		for _, label := range slices.Sorted(maps.Keys(count)) {
			line := fmt.Sprintf("%-44s %8d %8s", label, count[label], regimeReplayShare(count[label], len(sessions)))
			if column != "" {
				line += fmt.Sprintf(" %13s", regimeReplayShare(alone[label], len(sessions)))
			}
			fmt.Fprintln(b, line)
		}
	}
	share("headline on quiet sessions", "", func(s regimeReplaySession) []string {
		if s.stage[0] != rpc.LifecycleQuiet {
			return nil
		}
		return []string{s.headline}
	})
	share("early_warning opened by", "the only arm", func(s regimeReplaySession) []string {
		if s.stage[0] != rpc.LifecycleEarlyWarning {
			return nil
		}
		return s.openers
	})

	fmt.Fprintf(b, "%-18s %8s %7s %13s %7s   (shares of the sessions with a reading)\n", "indicator", "readings", "red", "red+eligible", "yellow")
	for k, ind := range streakIndicators {
		var readings, red, eligible, yellow int
		for _, s := range sessions {
			switch s.bands[k] {
			case 'R':
				eligible++
				red++
			case 'r':
				red++
			case 'y':
				yellow++
			case '-':
				continue
			}
			readings++
		}
		fmt.Fprintf(b, "%-18s %8d %7s %13s %7s\n", ind.key(), readings,
			regimeReplayShare(red, readings), regimeReplayShare(eligible, readings), regimeReplayShare(yellow, readings))
	}

	fmt.Fprintf(b, "%-34s %8s %12s %11s %11s\n", fmt.Sprintf("SPY, next %d sessions", regimeReplayAhead), "sessions", "mean return", "low <= -5%", "low <= -8%")
	outcome := func(label string, keep func(regimeReplaySession) bool) {
		var n, low5, low8 int
		var sum float64
		for _, s := range sessions {
			if !keep(s) || s.row+regimeReplayAhead >= len(spy) {
				continue
			}
			n++
			sum += spy[s.row+regimeReplayAhead]/spy[s.row] - 1
			low := slices.Min(spy[s.row+1:s.row+regimeReplayAhead+1])/spy[s.row] - 1
			if low <= -0.05 {
				low5++
			}
			if low <= -0.08 {
				low8++
			}
		}
		if n > 0 {
			fmt.Fprintf(b, "%-34s %8d %+11.2f%% %11s %11s\n", label, n, 100*sum/float64(n), regimeReplayShare(low5, n), regimeReplayShare(low8, n))
		}
	}
	outcome("all sessions", func(regimeReplaySession) bool { return true })
	for series, seriesName := range regimeReplaySeries {
		for _, stage := range regimeReplayStages {
			outcome(seriesName+": "+stage, func(s regimeReplaySession) bool { return s.stage[series] == stage })
		}
	}
	for k, ind := range streakIndicators {
		outcome(ind.key()+" red", func(s regimeReplaySession) bool { return s.bands[k] == 'R' || s.bands[k] == 'r' })
	}
}

// regimeReplayRecall lists SPY's falls and when each series first warned. A
// fall starts on the first close 8% or more below the highest close of the
// prior 60 sessions and lasts until SPY closes above the highest close of
// its prior 60 sessions again; the high is that starting high, the low the
// lowest close in between. A warning counts from 20 sessions before the high
// up to the low.
func regimeReplayRecall(b *strings.Builder, days []regimeHistoryDay, spy []float64, sessions []regimeReplaySession) {
	fmt.Fprintf(b, "\n== recall: SPY falls of %.0f%% or more from the highest close of the prior %d sessions ==\n", 100*regimeReplayFall, regimeReplayHighWindow)
	fmt.Fprintf(b, "first warning from %d sessions before the high up to the low; offsets are sessions from the high and from the low\n", regimeReplayAhead)
	for i := regimeReplayHighWindow; i < len(spy); i++ {
		prior := spy[i-regimeReplayHighWindow : i]
		high := i - regimeReplayHighWindow + slices.Index(prior, slices.Max(prior))
		if spy[i] > (1-regimeReplayFall)*spy[high] {
			continue
		}
		low := i
		for i++; i < len(spy) && spy[i] <= slices.Max(spy[i-regimeReplayHighWindow:i]); i++ {
			if spy[i] < spy[low] {
				low = i
			}
		}
		fmt.Fprintf(b, "high %s  low %s  %+.1f%%\n", days[high].date, days[low].date, 100*(spy[low]/spy[high]-1))
		from := high - regimeReplayAhead
		if from < regimeReplayWarmup {
			b.WriteString("    not replayed: the fall begins before the first replayed session\n")
			continue
		}
		for series, seriesName := range regimeReplaySeries {
			fmt.Fprintf(b, "    %-9s", seriesName)
			for rank, label := range []string{rpc.LifecycleEarlyWarning, rpc.LifecycleConfirmedStress} {
				found := "none"
				for row := from; row <= low; row++ {
					if regimeStageRank(sessions[row-regimeReplayWarmup].stage[series]) > rank {
						found = fmt.Sprintf("%s (high%+d, low%+d)", days[row].date, row-high, row-low)
						break
					}
				}
				fmt.Fprintf(b, "  %s or worse: %s", label, found)
			}
			b.WriteByte('\n')
		}
	}
}

// regimeReplayDaily prints one line per session, so two runs of the replay
// can be compared day by day.
func regimeReplayDaily(b *strings.Builder, days []regimeHistoryDay, sessions []regimeReplaySession) {
	b.WriteString("\n== daily series ==\n")
	b.WriteString("served bands: g green, y yellow, r red, R red and eligible to confirm, - no reading; in the order")
	for _, ind := range streakIndicators {
		b.WriteString(" " + ind.key())
	}
	fmt.Fprintf(b, "\n%-10s %-16s %-16s %-8s %8s  %s\n", "date", regimeReplaySeries[0], regimeReplaySeries[1], "bands", "SPY", "warning openers")
	for _, s := range sessions {
		line := fmt.Sprintf("%-10s %-16s %-16s %-8s %8.2f  %s", s.date, s.stage[0], s.stage[1], s.bands, days[s.row].spy, strings.Join(s.openers, " "))
		fmt.Fprintln(b, strings.TrimRight(line, " "))
	}
}
