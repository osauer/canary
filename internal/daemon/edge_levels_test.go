package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// scaledSetupSource serves the fake's flat 100 bars at 95, so the session's
// five-minute range (94.05 to 95.95) agrees with the seeded daily bar (94 to
// 96).
type scaledSetupSource struct{ *fakeSetupSource }

func (s scaledSetupSource) SessionBars(ctx context.Context, c broker.Instrument, start, end time.Time, timeout time.Duration) ([]broker.Bar, error) {
	bars, err := s.fakeSetupSource.SessionBars(ctx, c, start, end, timeout)
	for i := range bars {
		bars[i].Open, bars[i].High, bars[i].Low, bars[i].Close = bars[i].Open*0.95, bars[i].High*0.95, bars[i].Low*0.95, bars[i].Close*0.95
	}
	return bars, err
}

func TestEdgeLevelsReplaysTheHeldSessionsAndKeepsTheirBars(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	seedReplayHistory(t, srv, store, now)
	fake := newFakeSetupSource()
	srv.setupSourceForTest = scaledSetupSource{fake}

	// The SYNX call is in the statements of 2 to 4 September, so the
	// sessions after them are the sample: the 3rd, the 4th and the 8th (the
	// 7th is Labor Day). While the broker read is held, the run answers with
	// every day pending.
	release := fake.holdSymbol("SPY")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	first, err := srv.handleEdgeLevels(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if first.Pending != 3 || !first.Incomplete || len(first.Looks) != 2 || len(first.Looks[1].Days) != 0 || !first.NotExecution {
		t.Fatalf("first run = pending %d incomplete %v looks %d", first.Pending, first.Incomplete, len(first.Looks))
	}
	release()
	done := srv.startLevelFill(t.Context(), nil, nil)
	<-done

	second, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != rpc.EdgeStateCurrent || second.Pending != 0 || len(second.Looks) != 2 || second.Looks[0].LookMinutes != 30 {
		t.Fatalf("second run = %+v", second)
	}
	for _, look := range second.Looks {
		if len(look.Days) != 3 || look.Sample.Names != 1 {
			t.Fatalf("look %d: %d days, %d names", look.LookMinutes, len(look.Days), look.Sample.Names)
		}
	}
	day := second.Looks[1].Days[0]
	if day.Session != "2026-09-03" || day.Prior.Date != "2026-09-02" || day.Contracts != 2 || day.ATR14 != 2 {
		t.Fatalf("first day = %+v", day)
	}
	// Flat at 95 against a prior range of 94 to 96: watched, not down on the
	// day, so only held is valued.
	if day.Row != rpc.EdgeLevelsRowWatched || day.DownOnDay || len(day.Look.Results) != 1 || day.Look.MomentPrice != 95 {
		t.Fatalf("first day row = %s down %v results %d price %v", day.Row, day.DownOnDay, len(day.Look.Results), day.Look.MomentPrice)
	}
	if err := rpc.ValidateEdgeLevelsResult(*second); err != nil {
		t.Fatal(err)
	}

	// A third run reads nothing from the broker: the bars are kept.
	before := len(fake.symbolReads("SYNX")) + len(fake.symbolReads("SPY"))
	if _, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if after := len(fake.symbolReads("SYNX")) + len(fake.symbolReads("SPY")); after != before {
		t.Fatalf("broker reads %d after the bars were kept, %d before", after, before)
	}
}

func TestEdgeLevelsNeedsFlex(t *testing.T) {
	srv := &Server{cfg: nil}
	result, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil || result.State != rpc.EdgeStateActionRequired || !result.NotExecution {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{"from":"2026-13-01"}`)}); err == nil {
		t.Fatal("a bad date was accepted")
	}
}

func TestEdgeLevelsANameTheBrokerCannotReadIsLeftOutNotWaitedOn(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	seedReplayHistory(t, srv, store, now)
	fake := newFakeSetupSource()
	fake.setFail("SYNX", errors.New("no security definition"))
	srv.setupSourceForTest = scaledSetupSource{fake}
	if _, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	<-srv.startLevelFill(t.Context(), nil, nil)
	result, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	// SPY, read after SYNX failed, is kept; SYNX's days are left out.
	if result.Pending != 0 || result.Looks[1].Sample.Excluded["five_minute_bars_unavailable"] != 3 || len(fake.symbolReads("SPY")) == 0 {
		t.Fatalf("pending %d excluded %v spy reads %d", result.Pending, result.Looks[1].Sample.Excluded, len(fake.symbolReads("SPY")))
	}
}

func TestLevelRangedCoverageIgnoresCloseOnlyBars(t *testing.T) {
	day := func(d string, high float64) edgecore.UnderlyingBar {
		at, _ := time.Parse(time.DateOnly, d)
		return edgecore.UnderlyingBar{Day: at, Close: 95, High: high, Low: high - 2}
	}
	// Close-only bars from January on, ranged bars only from June: the
	// history spans January by its closes but not by its ranges.
	bars := []edgecore.UnderlyingBar{day("2026-01-02", 0), day("2026-03-02", 0), day("2026-06-01", 96), day("2026-09-01", 96)}
	first, _ := time.Parse(time.DateOnly, "2026-01-02")
	last, _ := time.Parse(time.DateOnly, "2026-09-01")
	if !edgecore.ExitReplayHistoryCovers(bars, first, last) || edgecore.ExitReplayHistoryCovers(levelRanged(bars), first, last) {
		t.Fatal("close-only bars must not count as the ranged history the rule reads")
	}
}

// seedLevelDaily keeps a synthetic daily history of one name (95, ranged 94
// to 96, every weekday) in the market-history cache under the given range.
func seedLevelDaily(t *testing.T, store *corestore.Store, now time.Time, symbol, historyRange string, from, to time.Time) {
	t.Helper()
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: historyRange}
	key, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	result := rpc.MarketHistoryResult{TimestampKind: "session_date", PriceBasis: "TRADES", RegularHoursOnly: true, Contract: normalized.Contract, Range: historyRange, Interval: "1 day", Source: "hmds", AsOf: now, CoverageStatus: "complete"}
	result.Contract.ConID = 4242
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			result.Points = append(result.Points, rpc.MarketHistoryPoint{At: day, Value: 95, Open: new(95.0), High: new(96.0), Low: new(94.0)})
		}
	}
	result.RequestedStart, result.Start, result.End = result.Points[0].At, result.Points[0].At, result.Points[len(result.Points)-1].At
	payload, err := json.Marshal(storedMarketHistory{Version: 1, Identity: key, FullReadAt: now, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveChartCache(t.Context(), key, payload); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeLevelsEverySessionReplaysTheHeldNamesOnSessionsNoCallWasHeld(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	seedReplayHistory(t, srv, store, now)
	srv.setupSourceForTest = scaledSetupSource{newFakeSetupSource()}
	// The SYNX call is in the statements of 2 to 4 September only; the week
	// of the 14th is replayed all the same, one contract a session.
	params := json.RawMessage(`{"from":"2026-09-14","to":"2026-09-18","every_session":true}`)
	if _, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: params}); err != nil {
		t.Fatal(err)
	}
	<-srv.startLevelFill(t.Context(), nil, nil)
	result, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	look := result.Looks[1]
	if !result.EverySession || result.Pending != 0 || len(look.Days) != 5 || look.Sample.Names != 1 || look.Days[0].Session != "2026-09-14" || look.Days[0].Contracts != 1 {
		t.Fatalf("every-session run = pending %d days %d: %+v", result.Pending, len(look.Days), look.Sample)
	}
}

func TestEdgeLevelsEverySessionBeforeTheCalendarTakesItsSessionsFromTheIndexBars(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	seedReplayHistory(t, srv, store, now)
	srv.setupSourceForTest = scaledSetupSource{newFakeSetupSource()}
	params := json.RawMessage(`{"from":"2025-06-02","to":"2025-06-06","every_session":true}`)

	// Without the index's daily history of the period there are no sessions
	// to name, and the run says it is waiting for it.
	first, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: params})
	if err != nil || first.State != rpc.EdgeStateBackfilling || first.Reason != "index_history_pending" {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	from, to := time.Date(2025, time.April, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, time.July, 1, 0, 0, 0, 0, time.UTC)
	seedLevelDaily(t, store, now, "SPY", "5Y", from, to)
	seedLevelDaily(t, store, now, "SYNX", "5Y", from, to)
	if _, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: params}); err != nil {
		t.Fatal(err)
	}
	<-srv.startLevelFill(t.Context(), nil, nil)
	result, err := srv.handleEdgeLevels(t.Context(), &rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Looks) != 2 {
		t.Fatalf("run before the calendar = state %s reason %s", result.State, result.Reason)
	}
	look := result.Looks[1]
	if result.State != rpc.EdgeStateCurrent || result.Pending != 0 || len(look.Days) != 5 || look.Days[0].Session != "2025-06-02" || look.Days[0].Prior.Date != "2025-05-30" {
		t.Fatalf("run before the calendar = state %s pending %d days %d excluded %v", result.State, result.Pending, len(look.Days), look.Sample.Excluded)
	}
}

func TestLevelRecordedSessionsAreTheIndexBarsAndEndWhereItStoppedTrading(t *testing.T) {
	bar := func(d string) edgecore.UnderlyingBar {
		at, _ := time.Parse(time.DateOnly, d)
		return edgecore.UnderlyingBar{Day: at, Close: 95, High: 96, Low: 94}
	}
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	// No bar on the 24th (a holiday): the 25th follows the 23rd.
	sessions := levelRecordedSessions([]edgecore.UnderlyingBar{bar("2022-11-22"), bar("2022-11-23"), bar("2022-11-25"), bar("2022-11-28")}, "2022-11-23", "2022-11-25", now)
	if len(sessions) != 2 || sessions[1].Date != "2022-11-25" || sessions[1].Prior != "2022-11-23" || sessions[0].Prior != "2022-11-22" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if got := sessions[1].Close.Sub(sessions[1].Open); got != 6*time.Hour+30*time.Minute || sessions[1].Open.UTC().Hour() != 14 {
		t.Fatalf("regular hours = %s from %s", got, sessions[1].Open.UTC())
	}
	// The index's last bar started at 12:55 New York: the session closed at 13:00.
	var index []edgecore.LevelBar
	for at := sessions[1].Open; at.Before(sessions[1].Open.Add(3*time.Hour + 30*time.Minute)); at = at.Add(5 * time.Minute) {
		index = append(index, edgecore.LevelBar{Start: at, Open: 95, High: 95, Low: 95, Close: 95})
	}
	short := levelRecordedClose(sessions[1], index)
	if !short.Short || short.Close.Sub(short.Open) != 3*time.Hour+30*time.Minute {
		t.Fatalf("early close = %+v", short)
	}
	if full := levelRecordedClose(sessions[0], nil); full.Short || !full.Close.Equal(sessions[0].Close) {
		t.Fatalf("a session without index bars changed: %+v", full)
	}
}
