package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/register"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The strategy replay (strategy-evaluator.md §9), witnessed on the core:
// the daemon reads five years of bars for the entry's contract, runs the
// rule over the month-ends of the span on the entry's calendar with the
// paper holding carried, names what it ran on, and freezes, records and
// sends nothing. No market data and no account figure.

// replayTestBars is a synthetic daily series on every weekday from start to
// the last Xetra session before mandateTestNow: a slow cycle with a drift,
// so the trend turns on and off and the 15% floor binds. Holidays of 2026
// carry a bar too, so the calendar has something to skip and name.
func replayTestBars(t *testing.T, start string) (dates []string, closes []float64) {
	t.Helper()
	last, _, ok := mandateLastSessionsIn(mandateTestNow, marketcal.MarketDEXetra)
	if !ok {
		t.Fatal("no last Xetra session")
	}
	day, err := time.Parse(time.DateOnly, start)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; day = day.AddDate(0, 0, 1) {
		d := day.Format(time.DateOnly)
		if d > last {
			break
		}
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		dates = append(dates, d)
		closes = append(closes, math.Round((100+30*math.Sin(2*math.Pi*float64(i)/500)+0.02*float64(i))*100)/100)
		i++
	}
	return dates, closes
}

func replayTestHistory(dates []string, closes []float64, contract rpc.ContractParams) *rpc.MarketHistoryResult {
	res := &rpc.MarketHistoryResult{Contract: contract, Range: "5Y", Interval: "1 day", Source: "test bars", AsOf: mandateTestNow.Add(-time.Minute)}
	for i, d := range dates {
		at, _ := time.Parse(time.DateOnly, d)
		res.Points = append(res.Points, rpc.MarketHistoryPoint{At: at, Value: closes[i]})
	}
	return res
}

func replayCore(t *testing.T, srv *Server, from, to string) (*rpc.StrategyReplayResult, error) {
	t.Helper()
	params, _ := json.Marshal(rpc.StrategyReplayParams{ID: "core", From: from, To: to})
	return srv.handleStrategyReplay(t.Context(), &rpc.Request{Params: params})
}

func TestStrategyReplayRunsTheRuleOverTheStoredMonthEnds(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	dates, closes := replayTestBars(t, "2022-01-03")
	var asked []rpc.MarketHistoryParams
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, tail int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		asked = append(asked, p)
		if tail != 0 {
			t.Fatalf("the replay asked for a tail of %d days; it reads the whole range", tail)
		}
		return replayTestHistory(dates, closes, p.Contract), nil
	}
	res, err := replayCore(t, srv, "2022-01-01", "2030-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Range != strategyReplayRange || asked[0].Contract.ConID != coreTestConID {
		t.Fatalf("broker asked %+v", asked)
	}
	latest, _, _ := coreDecisionDays(mandateTestNow, marketcal.MarketDEXetra)
	if res.To != latest || res.From != "2022-01-01" {
		t.Fatalf("span %s to %s; the last decided month-end is %s", res.From, res.To, latest)
	}
	if res.Strategy != "core" || res.Rule != core.RuleID || res.SpecHash != core.SpecHash || res.Signal != core.TrendSMA10.ID {
		t.Fatalf("names %s %s %s %s", res.Strategy, res.Rule, res.SpecHash, res.Signal)
	}
	if res.Instrument == nil || res.Instrument.ConID != coreTestConID || res.Calendar != string(marketcal.MarketDEXetra) || res.Source != "test bars" {
		t.Fatalf("ran on %+v, calendar %s, source %s", res.Instrument, res.Calendar, res.Source)
	}
	if !slices.Equal(res.Dates, dates) || res.SeriesFrom != dates[0] || res.SeriesTo != dates[len(dates)-1] || res.Sizing != core.ReplaySizing || res.Terms != (core.ReplayTerms{BandPct: 10, MinOrderBase: 2500}) {
		t.Fatalf("series %s to %s (%d), sizing %+v, terms %+v", res.SeriesFrom, res.SeriesTo, len(res.Dates), res.Sizing, res.Terms)
	}
	if !slices.Contains(res.OffCalendar, "2026-01-01") {
		t.Fatalf("the bar on New Year 2026 is not named off-calendar: %v", res.OffCalendar)
	}
	if len(res.Rows) == 0 || res.Rows[0].Decision != "2022-01-31" || res.Rows[0].Status != core.StatusNotLive || res.Rows[len(res.Rows)-1].Decision != latest {
		t.Fatalf("rows run %s to %s, first %s", res.Rows[0].Decision, res.Rows[len(res.Rows)-1].Decision, res.Rows[0].Status)
	}
	held, decided, seen := 0, 0, map[string]bool{}
	for i, r := range res.Rows {
		if i > 0 && r.Decision <= res.Rows[i-1].Decision {
			t.Fatalf("rows out of order at %s", r.Decision)
		}
		if r.HeldBefore != held {
			t.Fatalf("%s: held before %d, carried %d", r.Decision, r.HeldBefore, held)
		}
		if r.Status == core.StatusOK {
			decided++
			seen[r.Action] = true
			if r.TrendOn && r.TargetShares <= 0 || !r.TrendOn && r.TargetShares != 0 {
				t.Fatalf("%s: trend %v target %d", r.Decision, r.TrendOn, r.TargetShares)
			}
		}
		held = r.HeldAfter
	}
	if decided == 0 || !seen[core.ActionBuy] || !seen[core.ActionSellAll] {
		t.Fatalf("%d decided months, actions %v: the cycle should buy and sell every share", decided, seen)
	}
	// Read-only: no month frozen, no shadow record, no row.
	if f := srv.coreFrozen(t.Context(), coreScope(srv.currentBrokerStateScope(), coreTestConID)); f != nil {
		t.Fatalf("the replay froze %s", f.Decision)
	}
	if v := srv.shadowView(t.Context(), "core", register.StateShadow); v != nil && v.Records != 0 {
		t.Fatalf("the replay wrote %d shadow records", v.Records)
	}
}

// Every refusal is named: the span's shape, an unknown id, a broker read
// that fails.
func TestStrategyReplayRefusals(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	srv.mandateHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return nil, errors.New("gateway unavailable")
	}
	for _, tc := range []struct{ from, to, want string }{
		{"2022-1-1", "2022-12-31", "YYYY-MM-DD"},
		{"2023-01-01", "2022-12-31", "is after"},
		{"2022-01-01", "2022-12-31", "daily history for the replay: gateway unavailable"},
	} {
		_, err := replayCore(t, srv, tc.from, tc.to)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s to %s: %v, want %q", tc.from, tc.to, err, tc.want)
		}
	}
	params, _ := json.Marshal(rpc.StrategyReplayParams{ID: "nothing", From: "2022-01-01", To: "2022-12-31"})
	if _, err := srv.handleStrategyReplay(t.Context(), &rpc.Request{Params: params}); err == nil || !strings.Contains(err.Error(), `no strategy "nothing"`) {
		t.Fatalf("unknown id: %v", err)
	}
}

// TestStrategyReplayFromSeriesFile is the series-file harness: the replay
// the handler serves, run over a `canary market --json` dump instead of a
// live broker read, so the owner can produce the daemon's table while the
// installed daemon predates strategy.replay or the gateway is down. It is
// skipped unless CANARY_REPLAY_SERIES names the dump; CANARY_REPLAY_MARKET
// (de_xetra, uk_lse), CANARY_REPLAY_FROM and CANARY_REPLAY_TO bound it,
// CANARY_REPLAY_BAND and CANARY_REPLAY_MIN are the entry's terms (10 and
// 2500 unless set), and CANARY_REPLAY_OUT receives the table:
//
//	CANARY_REPLAY_SERIES=iuse.json CANARY_REPLAY_MARKET=uk_lse CANARY_REPLAY_FROM=2021-10-01 \
//	CANARY_REPLAY_TO=2026-09-30 CANARY_REPLAY_OUT=core-replay.json \
//	go test ./internal/daemon -run TestStrategyReplayFromSeriesFile -count=1
//
// The dump and the table are runtime data: they stay outside Git.
func TestStrategyReplayFromSeriesFile(t *testing.T) {
	path := os.Getenv("CANARY_REPLAY_SERIES")
	if path == "" {
		t.Skip("CANARY_REPLAY_SERIES unset: the series-file harness runs only on request")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var hist rpc.MarketHistoryResult
	if err := json.Unmarshal(raw, &hist); err != nil {
		t.Fatal(err)
	}
	dates, closes, err := replaySeries(&hist, hist.Contract)
	if err != nil {
		t.Fatal(err)
	}
	band, minimum := 10.0, 2500.0
	if v := os.Getenv("CANARY_REPLAY_BAND"); v != "" {
		band, _ = strconv.ParseFloat(v, 64)
	}
	if v := os.Getenv("CANARY_REPLAY_MIN"); v != "" {
		minimum, _ = strconv.ParseFloat(v, 64)
	}
	c := testCoreConstitution()
	c.Mandate.Core.BandPct, c.Mandate.Core.MinOrderBase = &band, &minimum
	r, ok := resolvedCore(register.Resolve(c.Mandate.Register(), register.Catalogue()))
	if !ok {
		t.Fatal("no core entry")
	}
	k := hist.Contract
	contract := &coreContract{Version: 1, ISIN: core.ISIN, ConID: k.ConID, Symbol: k.Symbol, LocalSymbol: k.LocalSymbol, TradingClass: k.TradingClass, PrimaryExch: k.PrimaryExch, Currency: k.Currency,
		Market: marketcal.Market(os.Getenv("CANARY_REPLAY_MARKET")), At: hist.AsOf}
	from, to := os.Getenv("CANARY_REPLAY_FROM"), os.Getenv("CANARY_REPLAY_TO")
	if _, _, err := replaySpan(from, to); err != nil {
		t.Fatal(err)
	}
	res := replayStrategySeries(r, contract, core.TrendSMA10, dates, closes, from, to, time.Now().UTC())
	res.Source, res.FetchedAt = hist.Source, hist.AsOf
	out, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if dest := os.Getenv("CANARY_REPLAY_OUT"); dest != "" {
		if err := os.WriteFile(dest, out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: %d rows, %d closes", dest, len(res.Rows), len(res.Dates))
	}
}
