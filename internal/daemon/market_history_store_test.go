package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func historyFixture(t *testing.T) (*Server, rpc.MarketHistoryParams, string, time.Time, rpc.MarketHistoryResult) {
	t.Helper()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{ConID: 123456, Symbol: "SYNTH", SecType: "STK", Exchange: "NYSE", Currency: "USD"}, Range: "1M"}
	key, p, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	volume := int64(1234)
	r := rpc.MarketHistoryResult{Contract: p.Contract, Range: p.Range, Interval: "1 day", PriceBasis: "TRADES", RegularHoursOnly: true, TimestampKind: "session_date", RequestedStart: now.AddDate(0, -1, -1), AsOf: now, CoverageStatus: "available", Points: []rpc.MarketHistoryPoint{{At: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), Value: 100, Volume: &volume}, {At: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC), Value: 101, Volume: &volume}}}
	r.Start, r.End = r.Points[0].At, r.Points[1].At
	return &Server{coreStore: store}, p, key, now, r
}

func TestMarketHistoryDurableOutageAndOfflineReload(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	result, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil || result.Cache.StoredAt.IsZero() {
		t.Fatalf("durable save: %+v %v", result, err)
	}
	// New daemon state: no memory cache and no connector. Public chart reads
	// must return the database record rather than first requiring a broker.
	reloaded := &Server{coreStore: s.coreStore, now: func() time.Time { return now }}
	raw, _ := json.Marshal(p)
	got, err := reloaded.handleMarketHistory(t.Context(), &rpc.Request{Params: raw})
	if err != nil || got.Cache.Selected != "cache" || got.AsOf != r.AsOf || got.Cache.StoredAt != result.Cache.StoredAt || len(got.Points) != 2 {
		t.Fatalf("restart lost recorded history: %+v %v", got, err)
	}
	// A later view trims the requested window, while durable evidence retains
	// the older point and its original source clock.
	reloaded.now = func() time.Time { return now.AddDate(0, 0, 3) }
	got, err = reloaded.handleMarketHistory(t.Context(), &rpc.Request{Params: raw})
	if err != nil || len(got.Points) != 1 || got.AsOf != r.AsOf {
		t.Fatalf("rolling selection changed retained evidence: %+v %v", got, err)
	}
	got, err = reloaded.readRetainedHistory(t.Context(), key, p, now.AddDate(0, 0, 3), func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return nil, errors.New("HMDS disconnected")
	})
	if err != nil || !got.Cache.RefreshFailed || got.AsOf != r.AsOf {
		t.Fatalf("outage rewrote source clock: %+v %v", got, err)
	}
	stored, _, err := s.loadMarketHistory(t.Context(), key)
	if err != nil || stored.Result.AsOf != r.AsOf || len(stored.Result.Points) != 2 {
		t.Fatal("failed read replaced successful document")
	}
}

func TestMarketHistorySMARTCacheUsesResolvedCalendar(t *testing.T) {
	_, p, key, _, r := historyFixture(t)
	p.Range, p.Contract.Exchange, p.Contract.PrimaryExch = "1D", "SMART", ""
	r.Contract.Exchange, r.Contract.PrimaryExch = "SMART", "NYSE"
	r.Range, r.Interval, r.TimestampKind, r.RegularHoursOnly = "1D", "5 mins", "instant", false
	resolved := p
	resolved.Contract = r.Contract
	for _, tc := range []struct{ name, fetchedAt, readAt string }{
		{"weekday_cadence", "2026-09-15T16:00:00Z", "2026-09-15T16:01:00Z"},
		{"closed_weekend", "2026-09-12T00:20:00Z", "2026-09-13T12:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetched, _ := time.Parse(time.RFC3339, tc.fetchedAt)
			now, _ := time.Parse(time.RFC3339, tc.readAt)
			r.AsOf, r.RequestedStart = fetched, historyRequestStart(resolved, fetched)
			r.Points = []rpc.MarketHistoryPoint{{At: fetched.Add(-25 * time.Minute), Value: 101}}
			r.Start, r.End = r.Points[0].At, r.Points[0].At
			saved := mergeMarketHistory(key, nil, r, true, fetched)
			if historyRefreshDue(&saved, p, now) {
				t.Fatal("unresolved SMART identity caused a premature refresh")
			}
			if tc.name == "weekday_cadence" && !historyRefreshDue(&saved, p, fetched.Add(5*time.Minute)) {
				t.Fatal("resolved calendar suppressed the five-minute refresh")
			}
			if got := historyTailDays(&saved, p, now); got <= 0 {
				t.Fatal("unresolved SMART identity forced a full read", got)
			}
			if got := selectStoredHistory(&saved, fetched, p, now, "cache", "", false); got.Cache.PreviousWindow || got.Cache.RefreshDue || got.AsOf != fetched {
				t.Fatalf("current resolved session relabelled: %+v", got)
			}
			if !historyRefreshDue(&saved, p, fetched.AddDate(0, 0, 3)) {
				t.Fatal("resolved calendar suppressed a later session refresh")
			}
		})
	}
}

func TestMarketHistoryIncrementalReadPreservesPrefixAndChecksScope(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	_, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(0, 0, 3)
	var requestedDays int
	next := r
	next.AsOf = later
	next.RequestedStart = later.AddDate(0, 0, -4)
	next.Points = []rpc.MarketHistoryPoint{{At: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), Value: 102}}
	next.End = next.Points[0].At
	next.Start = next.End
	got, err := s.readRetainedHistory(t.Context(), key, p, later, func(_ context.Context, _ rpc.MarketHistoryParams, days int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		requestedDays = days
		return &next, nil
	})
	if err != nil || requestedDays <= 0 || requestedDays > 7 {
		t.Fatalf("full month redownloaded: %d %v", requestedDays, err)
	}
	stored, _, _ := s.loadMarketHistory(t.Context(), key)
	if len(stored.Result.Points) != 3 || stored.Result.RequestedStart != r.RequestedStart || got.AsOf != later {
		t.Fatal("tail refresh erased prefix or coverage")
	}
	next.Contract.ConID++
	got, err = s.readRetainedHistory(t.Context(), key, p, later.Add(24*time.Hour), func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &next, nil
	})
	if err != nil || !got.Cache.RefreshFailed || got.Contract.ConID != p.Contract.ConID {
		t.Fatal("cross-contract response entered the cache")
	}
}

func TestMarketHistoryCanceledAndMalformedReadsCannotPoisonCache(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	_, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = s.readRetainedHistory(ctx, key, p, now.Add(72*time.Hour), func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		cancel()
		r.AsOf = now.Add(72 * time.Hour)
		return &r, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved, _, _ := s.loadMarketHistory(t.Context(), key)
	if !saved.Result.AsOf.Equal(now) {
		t.Fatal("canceled response was saved")
	}
	r.Points[1].At = r.Points[0].At
	got, err := s.readRetainedHistory(t.Context(), key, p, now.Add(72*time.Hour), func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil || !got.Cache.RefreshFailed {
		t.Fatal("duplicate timestamps advertised as new data")
	}
}

func TestMarketHistoryRetentionAndUncachedPrefix(t *testing.T) {
	_, p, key, now, r := historyFixture(t)
	r.Interval = "5 mins"
	r.RegularHoursOnly = false
	r.TimestampKind = "instant"
	r.Points = nil
	for i := range 30 {
		r.Points = append(r.Points, rpc.MarketHistoryPoint{At: now.AddDate(0, 0, i-29), Value: 100})
	}
	r.Start, r.End = r.Points[0].At, r.Points[29].At
	stored := mergeMarketHistory(key, nil, r, true, now)
	if len(stored.Result.Points) != 20 {
		t.Fatalf("intraday retention=%d", len(stored.Result.Points))
	}
	p.Range = "1D"
	got := selectStoredHistory(&stored, now, p, now.AddDate(0, 0, 7), "cache", "", true)
	if !got.Cache.PreviousWindow || got.Cache.Coverage != "previous_window" || got.AsOf != r.AsOf {
		t.Fatal("previous window relabelled as current")
	}
	p.Range = "5Y"
	stored.Result.Interval = "1 day"
	got = selectStoredHistory(&stored, now, p, now, "cache", "", true)
	if got.Cache.Coverage != "partial" {
		t.Fatal("uncached prefix reported complete")
	}
}

func TestMarketHistoryCloseFreshnessDoesNotExpireOverWeekend(t *testing.T) {
	_, p, key, now, r := historyFixture(t)
	saved := mergeMarketHistory(key, nil, r, true, now)
	if historyRefreshDue(&saved, p, now.Add(24*time.Hour)) {
		t.Fatal("Friday completed daily read became stale on Saturday")
	}
	if !historyRefreshDue(&saved, p, now.AddDate(0, 0, 3)) {
		t.Fatal("new Monday close did not request data")
	}
	if got := historyTailDays(&saved, p, now.AddDate(0, 0, 8)); got >= 0 {
		t.Fatal("weekly full correction review missing")
	}
}

func TestMarketHistoryChangedOlderOverlapForcesReconciliation(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	r.Points = append(r.Points[:1], rpc.MarketHistoryPoint{At: now.AddDate(0, 0, -5).Truncate(24 * time.Hour), Value: 105}, r.Points[1])
	_, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := []int{}
	later := now.AddDate(0, 0, 3)
	_, err = s.readRetainedHistory(t.Context(), key, p, later, func(_ context.Context, _ rpc.MarketHistoryParams, days int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		calls = append(calls, days)
		r.AsOf = later
		r.Points[1].Value = 52.5
		return &r, nil
	})
	if err != nil || len(calls) != 2 || calls[0] <= 0 || calls[1] >= 0 {
		t.Fatalf("adjusted overlap spliced into old basis: %v %v", calls, err)
	}
}

func TestMarketHistoryWorkerJoinsShutdown(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(t.Context())
	s.serverCancel = cancel
	s.startMarketHistoryRefresh(ctx)
	// Hold an in-flight publication at the persistence boundary. Shutdown
	// must not return and allow its caller to close SQLite while it is held.
	s.marketData.loopWG.Add(1)
	done := make(chan struct{})
	go func() { s.stopServerContextAndWait(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown passed an in-flight history publication")
	case <-time.After(20 * time.Millisecond):
	}
	s.marketData.loopWG.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("history worker survived shutdown")
	}
}

func TestMarketHistoryThinSuccessCannotEraseKnownSessions(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	fetch := func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	}
	if _, err := s.readRetainedHistory(t.Context(), key, p, now, fetch); err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(0, 0, 8)
	r.AsOf = later
	r.Points = r.Points[1:]
	r.Start = r.End
	got, err := s.readRetainedHistory(t.Context(), key, p, later, fetch)
	if err != nil || !got.Cache.RefreshFailed || got.Cache.Selected != "cache" || got.AsOf != now {
		t.Fatalf("thin response replaced history: %+v %v", got, err)
	}
	stored, _, _ := s.loadMarketHistory(t.Context(), key)
	if len(stored.Result.Points) != 2 {
		t.Fatal("recorded prefix erased")
	}
}

func TestMarketHistoryMissingSessionAndMemoryOnlyAreExplicit(t *testing.T) {
	_, p, key, now, r := historyFixture(t)
	s := &Server{}
	got, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	})
	if err != nil || !got.Cache.StoredAt.IsZero() || got.Cache.Detail == "" {
		t.Fatal("nonpersistent data claimed durable", err)
	}
	if got.Cache.MissingSessions == 0 || got.Cache.Coverage != "partial" {
		t.Fatal("sparse month claimed complete")
	}
}

func TestMarketHistoryPremarketAndClosedIntraday(t *testing.T) {
	_, p, key, now, r := historyFixture(t)
	p.Range = "1D"
	monday := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC) // 05:00 New York
	if got := historyRequestStart(p, monday); got.Format("2006-01-02") != "2026-09-14" {
		t.Fatal("premarket retained Friday as today's session", got)
	}
	r.Interval, r.TimestampKind, r.RegularHoursOnly = "5 mins", "instant", false
	r.AsOf = time.Date(2026, 9, 12, 0, 20, 0, 0, time.UTC)
	r.Points = []rpc.MarketHistoryPoint{{At: now.Add(2*time.Hour + 55*time.Minute), Value: 101}}
	r.Start, r.End = r.Points[0].At, r.Points[0].At
	r.RequestedStart = historyRequestStart(p, now)
	saved := mergeMarketHistory(key, nil, r, true, r.AsOf)
	if historyRefreshDue(&saved, p, now.Add(24*time.Hour)) {
		t.Fatal("completed Friday extended-hours read expired on Saturday")
	}
	if !historyRefreshDue(&saved, p, monday) {
		t.Fatal("new premarket did not request a refresh")
	}
}

// A CME Group future has no embedded venue calendar, but its weekend is
// known: bars read after Friday's close stand until Globex reopens on Sunday
// evening, for the intraday and the daily series alike.
func TestMarketHistoryGlobexWeekendNeedsNoRefresh(t *testing.T) {
	_, p, key, _, r := historyFixture(t)
	p.Range = "1D"
	p.Contract = rpc.ContractParams{ConID: 654321, Symbol: "ES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218"}
	r.Contract = p.Contract
	r.Range, r.Interval, r.TimestampKind, r.RegularHoursOnly = "1D", "5 mins", "instant", false
	fridayClose := time.Date(2026, 9, 18, 21, 0, 0, 0, time.UTC) // 17:00 New York
	r.Points = []rpc.MarketHistoryPoint{{At: fridayClose.Add(-5 * time.Minute), Value: 101}}
	r.Start, r.End = r.Points[0].At, r.Points[0].At
	r.RequestedStart = fridayClose.Add(-24 * time.Hour)
	r.AsOf = fridayClose.Add(10 * time.Minute)
	saved := mergeMarketHistory(key, nil, r, true, r.AsOf)
	sunday := time.Date(2026, 9, 20, 4, 9, 0, 0, time.UTC)
	if historyRefreshDue(&saved, p, sunday) {
		t.Fatal("a futures series read after Friday's close was refreshed on the weekend")
	}
	daily, pDaily := saved, p
	pDaily.Range = "1Y"
	daily.Result.Interval, daily.Result.TimestampKind, daily.Result.RegularHoursOnly = "1 day", "session_date", true
	daily.Result.RequestedStart = sunday.AddDate(-1, 0, -2)
	if historyRefreshDue(&daily, pDaily, sunday) {
		t.Fatal("the daily futures series was refreshed on the weekend too")
	}
	beforeClose := saved
	beforeClose.Result.AsOf = fridayClose.Add(-time.Hour)
	if !historyRefreshDue(&beforeClose, p, sunday) {
		t.Fatal("bars read before Friday's close must be completed once")
	}
	sundayEvening := time.Date(2026, 9, 20, 22, 30, 0, 0, time.UTC) // 18:30 New York
	if !historyRefreshDue(&saved, p, sundayEvening) {
		t.Fatal("Globex reopened on Sunday evening without a refresh")
	}
	if since := globexWeekendSince(time.Date(2026, 9, 18, 20, 59, 0, 0, time.UTC)); !since.IsZero() {
		t.Fatalf("Friday afternoon is a trading session: %v", since)
	}
	if since := globexWeekendSince(sunday); !since.Equal(fridayClose) {
		t.Fatalf("the weekend closure begins at Friday's close: %v", since)
	}
}

// TestMarketHistoryRollingIntradayWindowReconcilesWeekly witnesses intraday
// series whose request window rolls forward. No intraday read covers the
// twenty retained venue dates, so a full read that had to reach them never
// advanced FullReadAt: a week after the first read every refresh became a
// full read and every served record said refresh_due while its bars were
// current. The weekly full read now happens once, later reads are tail reads
// again, and refresh_due waits for the record to fall behind the worker.
func TestMarketHistoryRollingIntradayWindowReconcilesWeekly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract rpc.ContractParams
		rng      string
		step     time.Duration
	}{
		{"us_stock_1D", rpc.ContractParams{ConID: 123456, Symbol: "SYNTH", SecType: "STK", Exchange: "NYSE", Currency: "USD"}, "1D", 5 * time.Minute},
		{"globex_future_1D", rpc.ContractParams{ConID: 654321, Symbol: "SYNF", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218"}, "1D", 5 * time.Minute},
		{"us_stock_5D", rpc.ContractParams{ConID: 123456, Symbol: "SYNTH", SecType: "STK", Exchange: "NYSE", Currency: "USD"}, "5D", 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _, _ := historyFixture(t)
			key, p, err := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: tc.contract, Range: tc.rng})
			if err != nil {
				t.Fatal(err)
			}
			// The synthetic venue prints one bar per read, so a full read
			// returns every stable bar the record holds inside its window.
			var bars []time.Time
			var tails []int
			fetch := func(_ context.Context, p rpc.MarketHistoryParams, tail int, now time.Time) (*rpc.MarketHistoryResult, error) {
				tails = append(tails, tail)
				_, interval, _ := marketHistoryWindow(p.Range, now)
				r := rpc.MarketHistoryResult{Contract: p.Contract, Range: p.Range, Interval: interval, Source: "IBKR historical bars · TRADES", PriceBasis: "TRADES", TimestampKind: "instant", AsOf: now, CoverageStatus: "available", RequestedStart: historyRequestStart(p, now)}
				if tail > 0 && now.AddDate(0, 0, -tail).After(r.RequestedStart) {
					r.RequestedStart = now.AddDate(0, 0, -tail)
				}
				for _, at := range bars {
					if !at.Before(r.RequestedStart) && !at.After(now) {
						r.Points = append(r.Points, rpc.MarketHistoryPoint{At: at, Value: 100})
					}
				}
				r.Start, r.End = r.Points[0].At, r.Points[len(r.Points)-1].At
				return &r, nil
			}
			read := func(now time.Time) (full bool) {
				t.Helper()
				bars = append(bars, now.Truncate(tc.step))
				before := len(tails)
				got, err := s.readRetainedHistory(t.Context(), key, p, now, fetch)
				if err != nil || got.Cache.Selected != "ibkr" || len(tails) != before+1 {
					t.Fatalf("%s: refresh did not read the broker: %+v %v", now, got, err)
				}
				return tails[before] <= 0
			}
			served := func(now time.Time) *rpc.MarketHistoryCache {
				t.Helper()
				s.now = func() time.Time { return now }
				raw, _ := json.Marshal(p)
				got, err := s.handleMarketHistory(t.Context(), &rpc.Request{Params: raw})
				if err != nil {
					t.Fatal(err)
				}
				return got.Cache
			}

			first := time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC) // Monday 11:00 New York
			if !read(first) {
				t.Fatal("first read was not a full read")
			}
			for day := 1; day <= 4; day++ {
				if read(first.AddDate(0, 0, day)) {
					t.Fatalf("day %d: weekday refresh inside the week ran a full read", day)
				}
			}
			week := first.AddDate(0, 0, 7)
			if !read(week) {
				t.Fatal("weekly reconciliation did not run a full read")
			}
			stored, _, err := s.loadMarketHistory(t.Context(), key)
			if err != nil || !stored.FullReadAt.Equal(week) {
				t.Fatalf("full intraday read left FullReadAt behind: %v %v", stored.FullReadAt, err)
			}
			if c := served(week.Add(time.Minute)); c.RefreshDue || c.Selected != "cache" {
				t.Fatalf("current bars after the weekly read served as refresh due: %+v", c)
			}
			for i := 1; i <= 3; i++ {
				if read(week.Add(time.Duration(i) * tc.step)) {
					t.Fatalf("refresh %d after the weekly read ran a full read again", i)
				}
			}
			if read(week.AddDate(0, 0, 1)) {
				t.Fatal("next day's refresh ran a full read")
			}

			// A record inside the worker's cycle is current; one it has had
			// the whole cycle to refresh is due.
			last := week.AddDate(0, 0, 1)
			stored, _, _ = s.loadMarketHistory(t.Context(), key)
			if !historyRefreshDue(stored, p, last.Add(tc.step)) {
				t.Fatal("cadence no longer asks the worker to refresh")
			}
			if c := served(last.Add(tc.step + time.Minute)); c.RefreshDue {
				t.Fatalf("record awaiting the worker's next tick served as refresh due: %+v", c)
			}
			if c := served(last.Add(tc.step + marketHistoryRefreshGrace)); !c.RefreshDue {
				t.Fatalf("record the worker failed to refresh served as current: %+v", c)
			}
		})
	}
}

// IBKR serves a dated futures contract's daily bars about a year back, a
// window that rolls forward with the clock. An equity-index contract becomes
// the front at its quarterly roll a year after listing, so its 1Y record
// begins at that window's edge. A week later the weekly reconciliation no
// longer carried those first sessions, the lost-session check read them as
// missing from a thin response, and while the reconciliation stayed due
// every refresh was a full read it refused: the series stopped at its last
// tail read. Recorded sessions older than the first bar served lie beyond
// the depth the broker serves; they are retained and the rest is
// reconciled. A session missing inside the served span still makes the
// response incomplete.
func TestMarketHistoryFuturesReconciliationKeepsSessionsBeyondServedDepth(t *testing.T) {
	s, _, _, _, _ := historyFixture(t)
	log := &bytes.Buffer{}
	s.logger = NewLogger(log, "warn")
	key, p, err := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: rpc.ContractParams{ConID: 654321, Symbol: "SYNF", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218"}, Range: "1Y"})
	if err != nil {
		t.Fatal(err)
	}
	const depth = 365 * 24 * time.Hour
	listed := time.Date(2025, 3, 21, 0, 0, 0, 0, time.UTC)
	session := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	roll := time.Date(2026, 6, 19, 0, 2, 0, 0, time.UTC) // the new front's first read
	clock := roll
	s.now = func() time.Time { return clock }
	var omit time.Time
	var requested []int
	broker := func(_ context.Context, c ibkrlib.Contract, days int, _ string, _ time.Duration) (ibkrlib.ChartSeries, error) {
		requested = append(requested, days)
		series := ibkrlib.ChartSeries{Contract: c, WhatToShow: "TRADES"}
		for d := listed; !d.Add(20 * time.Hour).After(clock); d = d.AddDate(0, 0, 1) {
			if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || d.Before(clock.Add(-depth)) || d.Equal(omit) {
				continue
			}
			series.Bars = append(series.Bars, ibkrlib.HistoricalBar{Time: d, Open: 100, High: 101, Low: 99, Close: 100.5, Volume: 10})
		}
		return series, nil
	}
	request := func(ctx context.Context, got rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		k, got, err := marketHistoryIdentity(got)
		if err != nil {
			t.Fatal(err)
		}
		return s.readRetainedHistory(ctx, k, got, clock, func(ctx context.Context, p rpc.MarketHistoryParams, tail int, now time.Time) (*rpc.MarketHistoryResult, error) {
			return fetchMarketHistory(ctx, p, tail, now, broker)
		})
	}
	refresh := func() (marketHistoryInterest, *storedMarketHistory) {
		t.Helper()
		s.refreshMarketHistoryInterest(t.Context(), key, request)
		stored, _, err := s.loadMarketHistory(t.Context(), key)
		if err != nil || stored == nil {
			t.Fatalf("no recorded series: %v", err)
		}
		return s.marketData.interest[key], stored
	}

	s.rememberMarketHistory(p)
	_, stored := refresh()
	if first := stored.Result.Points[0].At; !first.Equal(session(2025, 6, 20)) || !stored.FullReadAt.Equal(roll) {
		t.Fatalf("the roll's first read must record the served year: first=%s full_read_at=%s", first, stored.FullReadAt)
	}

	// A week on, the reconciliation is due and the served year has rolled
	// past the record's first five sessions.
	clock = roll.AddDate(0, 0, 7)
	item, stored := refresh()
	if item.Failures != 0 || !item.RetryAt.IsZero() || !stored.Result.AsOf.Equal(clock) || !stored.FullReadAt.Equal(clock) || requested[len(requested)-1] < 366 {
		t.Fatalf("the reconciliation must succeed on what the broker still serves: failures=%d as_of=%s full_read_at=%s days=%v", item.Failures, stored.Result.AsOf, stored.FullReadAt, requested)
	}
	if first, last := stored.Result.Points[0].At, stored.Result.End; !first.Equal(session(2025, 6, 20)) || !last.Equal(session(2026, 6, 25)) {
		t.Fatalf("sessions beyond the served depth must be retained: %s..%s", first, last)
	}
	raw, _ := json.Marshal(p)
	got, err := s.handleMarketHistory(t.Context(), &rpc.Request{Params: raw})
	if err != nil || got.Cache.RefreshDue || got.Cache.RefreshFailed || !got.Cache.FetchedAt.Equal(clock) || got.Cache.Coverage != "observed" {
		t.Fatalf("the 1Y view must be current: %+v %v", got.Cache, err)
	}
	clock = clock.Add(time.Hour)
	if item, stored = refresh(); item.Failures != 0 || !stored.Result.AsOf.Equal(clock) || requested[len(requested)-1] > 7 {
		t.Fatalf("with the reconciliation done the series is back on hourly tail reads: failures=%d days=%v", item.Failures, requested)
	}
	if strings.Contains(log.String(), "lacks sessions") {
		t.Fatalf("sessions beyond the served depth were reported missing: %q", log.String())
	}

	// A session the broker leaves out inside the span it serves is still a
	// thin response, and the record is kept as it was.
	clock, omit = roll.AddDate(0, 0, 14), session(2026, 3, 16)
	before := stored.Result.AsOf
	if item, stored = refresh(); item.Failures != 1 || !stored.Result.AsOf.Equal(before) || !strings.Contains(log.String(), "response lacks sessions the recorded history has") {
		t.Fatalf("a session missing inside the served span must refuse the response: failures=%d as_of=%s %q", item.Failures, stored.Result.AsOf, log.String())
	}
}

// TestMarketHistoryFiveYearReconciliationCountsAfterPruning witnesses the
// daily reconciliation at the retention limit: its read is capped at 1,830
// days while the record still starts a few days earlier, until this very
// merge prunes that prefix. The read covers everything retained afterwards,
// so it is a full reconciliation and must not be repeated on the next tick.
func TestMarketHistoryFiveYearReconciliationCountsAfterPruning(t *testing.T) {
	_, _, key, now, r := historyFixture(t)
	old := r
	old.RequestedStart = now.AddDate(0, 0, -1834)
	old.Points = []rpc.MarketHistoryPoint{{At: old.RequestedStart.Truncate(24 * time.Hour), Value: 90}, r.Points[1]}
	old.Start, old.End = old.Points[0].At, old.Points[1].At
	saved := storedMarketHistory{Version: 1, Identity: key, FullReadAt: now.AddDate(0, 0, -7), Result: old}
	fresh := r
	fresh.RequestedStart = now.AddDate(0, 0, -1830).Truncate(24 * time.Hour)
	next := mergeMarketHistory(key, &saved, fresh, true, now)
	if !next.FullReadAt.Equal(now) || historyReconcileDue(&next, now.Add(time.Minute)) {
		t.Fatalf("capped reconciliation of the whole retained range not recorded: full_read_at=%s requested_start=%s", next.FullReadAt, next.Result.RequestedStart)
	}
}
