package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
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
