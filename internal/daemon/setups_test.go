package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/setups"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestSetupCalendarSelectsTwentyPriorComparableSessions(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-09-30T10:15:00-04:00")
	current, prior, err := setupSessionWindows(at, 20)
	if err != nil || len(prior) != 20 {
		t.Fatal(err, len(prior))
	}
	for i, p := range prior {
		if !p.Close.Before(current.Open) || p.Close.Sub(p.Open) != current.Close.Sub(current.Open) || p.Open.Weekday() == time.Saturday || p.Open.Weekday() == time.Sunday {
			t.Fatal("noncomparable session", p)
		}
		if i > 0 && !p.Open.After(prior[i-1].Open) {
			t.Fatal("unordered baseline")
		}
	}
	// Thanksgiving's early close cannot borrow regular-session baselines.
	early, _ := time.Parse(time.RFC3339, "2026-11-27T10:15:00-05:00")
	if _, _, err := setupSessionWindows(early, 20); err == nil {
		t.Fatal("invented twenty early-close sessions")
	}
}

func TestSetupHistoryAcquisitionIsBoundedAndNoFutureBarsLeak(t *testing.T) {
	start := time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)
	end := start.Add(29 * 24 * time.Hour)
	count := 0
	bars, err := collectSetupHistory(t.Context(), start, end, func(_ context.Context, from, to time.Time) ([]ibkr.HistoricalBar, error) {
		count++
		if to.Sub(from) > 7*24*time.Hour || to.After(end) {
			t.Fatal("unbounded read")
		}
		return []ibkr.HistoricalBar{{Time: from}, {Time: to}, {Time: from.Add(-time.Minute)}}, nil
	})
	if err != nil || count != 5 || len(bars) != 5 {
		t.Fatal(err, count, len(bars))
	}
	for _, b := range bars {
		if b.Time.Before(start) || !b.Time.Before(end) {
			t.Fatal("outside history window")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = collectSetupHistory(ctx, start, end, func(context.Context, time.Time, time.Time) ([]ibkr.HistoricalBar, error) {
		t.Fatal("read after cancellation")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSetupProfileWaitHonorsCancellation(t *testing.T) {
	var cache setupProfileCache
	if err := cache.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer cache.unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(cache.lock(ctx), context.Canceled) {
		t.Fatal("cache blocked cancellation")
	}
}

type setupEvaluation struct {
	result *rpc.SetupResult
	err    error
}

func goEvaluateSetup(ctx context.Context, s *Server, symbol string) <-chan setupEvaluation {
	out := make(chan setupEvaluation, 1)
	go func() {
		r, err := evaluateSetupForTest(ctx, s, symbol, 0, time.Time{})
		out <- setupEvaluation{r, err}
	}()
	return out
}

func TestSetupCachedProfileDoesNotWaitForColdAcquisition(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("warm-up evaluation: %+v", r)
	}
	baseline, current := src.reads("AAA")
	if baseline != 5 || current != 1 {
		t.Fatalf("cold acquisition reads baseline=%d current=%d", baseline, current)
	}
	// BBB's cold acquisition holds the serializing gate while its broker read
	// is outstanding.
	release := src.holdSymbol("BBB")
	defer release()
	cold := goEvaluateSetup(t.Context(), s, "BBB")
	within(t, src.entered, "BBB's cold read")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := evaluateSetupForTest(ctx, s, "AAA", 0, time.Time{})
	if err != nil || r.State != "watching" || r.BaselineSessions != 20 {
		t.Fatalf("cached profile waited behind a cold acquisition: %+v %v", r, err)
	}
	// Same bar: the retained current-session read answers too.
	if baseline, current = src.reads("AAA"); baseline != 5 || current != 1 {
		t.Fatalf("cached profile re-read history: baseline=%d current=%d", baseline, current)
	}
	release()
	if got := within(t, cold, "BBB's evaluation"); got.err != nil || got.result.State != "watching" {
		t.Fatalf("cold acquisition: %+v %v", got.result, got.err)
	}
}

func TestSetupColdAcquisitionRechecksCacheAfterGate(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	waiting := make(chan struct{}, 1)
	s.setupProfiles.waitForTest = func() { waiting <- struct{}{} }
	release := src.holdSymbol("AAA")
	defer release()
	first := goEvaluateSetup(t.Context(), s, "AAA")
	within(t, src.entered, "the first cold read")
	second := goEvaluateSetup(t.Context(), s, "AAA")
	within(t, waiting, "the second request at the gate")
	release()
	for _, ch := range []<-chan setupEvaluation{first, second} {
		if got := within(t, ch, "an evaluation"); got.err != nil || got.result.State != "watching" {
			t.Fatalf("evaluation: %+v %v", got.result, got.err)
		}
	}
	// The waiter found the profile its predecessor stored instead of
	// collecting the same twenty sessions again, and shared its current read.
	if baseline, current := src.reads("AAA"); baseline != 5 || current != 1 {
		t.Fatalf("duplicate cold acquisition: baseline=%d current=%d", baseline, current)
	}
}

func setupResultJSONWithoutBaselineClock(t *testing.T, r *rpc.SetupResult) string {
	t.Helper()
	c := *r
	c.BaselineObservedAt, c.InputHash = time.Time{}, ""
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSetupProfileRollsForwardInsteadOfRebuilding(t *testing.T) {
	ny := setupNY(t)
	day1 := time.Date(2026, 9, 30, 10, 17, 0, 0, ny)
	clock := &setupTestClock{t: day1}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" || !r.BaselineObservedAt.Equal(day1) {
		t.Fatalf("day 1: %+v", r)
	}
	if reads := src.baselineReads("AAA"); len(reads) != 5 {
		t.Fatalf("cold window used %d reads", len(reads))
	}

	// The next session needs Sep 2..Sep 30; nineteen are held, so only Sep 30
	// is read and Sep 1 leaves the window.
	day2 := time.Date(2026, 10, 1, 10, 17, 0, 0, ny)
	clock.set(day2)
	spike := time.Date(2026, 10, 1, 10, 5, 0, 0, ny)
	src.setVolume(spike, 1000)
	r := mustEvaluateSetup(t, s, "AAA", time.Time{})
	reads := src.baselineReads("AAA")
	if len(reads) != 6 || !reads[5].start.Equal(time.Date(2026, 9, 30, 9, 30, 0, 0, ny)) || !reads[5].end.Equal(time.Date(2026, 9, 30, 16, 0, 0, 0, ny)) {
		t.Fatalf("roll-forward reads: %+v", reads)
	}
	if r.State != "confirmed" || len(r.Baseline) != 20 || r.Baseline[0].Date != "2026-09-02" || r.Baseline[19].Date != "2026-09-30" {
		t.Fatalf("rolled window: %+v", r)
	}
	// Nineteen sessions were read on day 1, so the baseline clock stays there.
	if !r.BaselineObservedAt.Equal(day1) {
		t.Fatalf("baseline clock %s, want oldest acquisition %s", r.BaselineObservedAt, day1)
	}
	// The evaluator receives exactly the evidence a full rebuild would read.
	fresh := newFakeSetupSource()
	fresh.setVolume(spike, 1000)
	cold := mustEvaluateSetup(t, setupTestServer(fresh, clock), "AAA", time.Time{})
	if got, want := setupResultJSONWithoutBaselineClock(t, r), setupResultJSONWithoutBaselineClock(t, cold); got != want {
		t.Fatalf("rolled evaluation differs from rebuild:\n%s\n%s", got, want)
	}

	// After a long weekend the two missing sessions share one read.
	clock.set(time.Date(2026, 10, 5, 10, 17, 0, 0, ny))
	if r = mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("after weekend: %+v", r)
	}
	reads = src.baselineReads("AAA")
	if len(reads) != 7 || !reads[6].start.Equal(time.Date(2026, 10, 1, 9, 30, 0, 0, ny)) || !reads[6].end.Equal(time.Date(2026, 10, 2, 16, 0, 0, 0, ny)) {
		t.Fatalf("weekend roll reads: %+v", reads)
	}
	for _, row := range s.setupProfiles.rows {
		if len(row.sessions) != 20 {
			t.Fatalf("row kept %d sessions outside the window", len(row.sessions)-20)
		}
		if _, ok := row.sessions["2026-09-02"]; ok {
			t.Fatal("session outside the window was retained")
		}
	}
}

func TestSetupReplayKeepsItsOwnProfile(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	// A same-session replay has the live window and reads no history.
	if r := mustEvaluateSetup(t, s, "AAA", time.Date(2026, 9, 30, 10, 5, 0, 0, ny)); r.EvidenceKind != "historical_reconstruction" || r.State != "watching" {
		t.Fatalf("same-session replay: %+v", r)
	}
	if reads := src.baselineReads("AAA"); len(reads) != 5 {
		t.Fatalf("same-session replay re-read the baseline: %d", len(reads))
	}
	// A replay on another session builds its own window and leaves the live
	// window intact; repeating it reuses the replay window.
	past := time.Date(2026, 9, 15, 10, 17, 0, 0, ny)
	var afterReplay int
	for i := range 2 {
		if r := mustEvaluateSetup(t, s, "AAA", past); r.State != "watching" || r.SessionDate != "2026-09-15" {
			t.Fatalf("replay: %+v", r)
		}
		baseline, _ := src.reads("AAA")
		if i == 0 && (baseline-5 < 4 || baseline-5 > 5) {
			t.Fatalf("replay window used %d reads", baseline-5)
		}
		if i == 1 && baseline != afterReplay {
			t.Fatalf("repeated replay re-read %d sessions", baseline-afterReplay)
		}
		afterReplay = baseline
	}
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	if baseline, _ := src.reads("AAA"); baseline != afterReplay {
		t.Fatalf("live window was rebuilt after a replay: %d reads", baseline-afterReplay)
	}
}

func TestSetupFetchRangesAreSingleBoundedReads(t *testing.T) {
	ny := setupNY(t)
	_, window, err := setupSessionWindows(time.Date(2026, 9, 30, 10, 17, 0, 0, ny), 20)
	if err != nil {
		t.Fatal(err)
	}
	ranges := setupFetchRanges(window)
	covered := 0
	for _, r := range ranges {
		if r[1].Sub(r[0]) > 7*24*time.Hour || !r[1].After(r[0]) {
			t.Fatalf("range %v exceeds one bounded read", r)
		}
		for _, w := range window {
			if !w.Open.Before(r[0]) && !w.Close.After(r[1]) {
				covered++
			}
		}
	}
	if covered != 20 || len(ranges) != 5 {
		t.Fatalf("ranges %d covered %d sessions", len(ranges), covered)
	}
	if got := setupFetchRanges(window[18:]); len(got) != 1 {
		t.Fatalf("adjacent sessions use %d reads", len(got))
	}
	if got := setupFetchRanges([]setups.Session{window[0], window[19]}); len(got) != 2 {
		t.Fatalf("distant sessions use %d reads", len(got))
	}
}

func TestSetupIncompleteWindowKeepsCompleteSessions(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	src.setGap("2026-09-16", true)
	s := setupTestServer(src, clock)
	r := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if r.State != "unavailable" || len(r.Reasons) != 1 || r.Reasons[0] != "baseline_history_unavailable: baseline_bars_incomplete (retry after 10:32)" {
		t.Fatalf("incomplete window: %+v", r.Reasons)
	}
	src.setGap("2026-09-16", false)
	clock.set(time.Date(2026, 9, 30, 10, 32, 0, 0, ny))
	if r = mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("after repair: %+v", r)
	}
	// Only the incomplete session is read again.
	reads := src.baselineReads("AAA")
	if len(reads) != 6 || !reads[5].start.Equal(time.Date(2026, 9, 16, 9, 30, 0, 0, ny)) || !reads[5].end.Equal(time.Date(2026, 9, 16, 16, 0, 0, 0, ny)) {
		t.Fatalf("repair reads: %+v", reads)
	}
}

func TestSetupBaselineMissIsRememberedForFifteenMinutes(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 30, 0, ny)}
	src := newFakeSetupSource()
	src.setGap("2026-09-16", true)
	src.setFail("BBB", errors.New("historical data farm returned no data"))
	s := setupTestServer(src, clock)
	reason := func(symbol string) string {
		t.Helper()
		r := mustEvaluateSetup(t, s, symbol, time.Time{})
		if r.State != "unavailable" || len(r.Reasons) != 1 {
			return r.State
		}
		return r.Reasons[0]
	}
	const incomplete = "baseline_history_unavailable: baseline_bars_incomplete (retry after 10:33)"
	const failed = "baseline_history_unavailable: historical data farm returned no data (retry after 10:33)"
	if got := reason("AAA"); got != incomplete {
		t.Fatalf("incomplete window: %s", got)
	}
	if got := reason("BBB"); got != failed {
		t.Fatalf("failed read: %s", got)
	}
	aaa, _ := src.reads("AAA")
	bbb, _ := src.reads("BBB")
	// Within fifteen minutes the remembered reason answers without a read,
	// even after the history was repaired.
	src.setGap("2026-09-16", false)
	src.setFail("BBB", nil)
	for _, at := range []time.Time{time.Date(2026, 9, 30, 10, 20, 0, 0, ny), time.Date(2026, 9, 30, 10, 32, 29, 0, ny)} {
		clock.set(at)
		if got := reason("AAA"); got != incomplete {
			t.Fatalf("remembered incomplete window: %s", got)
		}
		if got := reason("BBB"); got != failed {
			t.Fatalf("remembered failed read: %s", got)
		}
	}
	if a, _ := src.reads("AAA"); a != aaa {
		t.Fatalf("memo re-read AAA %d times", a-aaa)
	}
	if b, _ := src.reads("BBB"); b != bbb {
		t.Fatalf("memo re-read BBB %d times", b-bbb)
	}
	// After expiry the next evaluation reads again: only the incomplete
	// session for AAA, the whole window for BBB.
	clock.set(time.Date(2026, 9, 30, 10, 32, 30, 0, ny))
	if got := reason("AAA"); got != "watching" {
		t.Fatalf("retry after expiry: %s", got)
	}
	if got := reason("BBB"); got != "watching" {
		t.Fatalf("retry after expiry: %s", got)
	}
	if a, _ := src.reads("AAA"); a != aaa+1 {
		t.Fatalf("AAA retry used %d reads", a-aaa)
	}
}

func TestSetupBaselineMissIgnoresCancellationAndReconnect(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	release := src.holdSymbol("AAA")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan setupEvaluation, 1)
	go func() {
		r, err := evaluateSetupForTest(ctx, s, "AAA", 0, time.Time{})
		done <- setupEvaluation{r, err}
	}()
	within(t, src.entered, "the read")
	cancel()
	got := within(t, done, "the cancelled evaluation")
	release()
	if got.err != nil || got.result.State != "unavailable" || got.result.Reasons[0] != "baseline_history_unavailable: context canceled" {
		t.Fatalf("cancelled read: %+v %v", got.result, got.err)
	}
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("a cancelled caller blocked the next read: %+v", r.Reasons)
	}

	// A read cut off by a broker reconnect is not a verdict on the history.
	src.setFail("BBB", errors.New("broker session changed during setup history"))
	src.mu.Lock()
	src.stale = true
	src.mu.Unlock()
	if r := mustEvaluateSetup(t, s, "BBB", time.Time{}); r.Reasons[0] != "baseline_history_unavailable: broker session changed during setup history" {
		t.Fatalf("reconnect: %+v", r.Reasons)
	}
	src.setFail("BBB", nil)
	src.mu.Lock()
	src.stale = false
	src.mu.Unlock()
	if r := mustEvaluateSetup(t, s, "BBB", time.Time{}); r.State != "watching" {
		t.Fatalf("a reconnect blocked the next read: %+v", r.Reasons)
	}
}

func TestSetupMissMemoryIsBounded(t *testing.T) {
	var cache setupProfileCache
	at := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	for i := range setupProfileMissLimit + 10 {
		cache.rememberMiss(strconv.Itoa(i), "2026-09-30", "baseline_bars_incomplete", at.Add(time.Duration(i)*time.Millisecond))
	}
	if len(cache.misses) != setupProfileMissLimit {
		t.Fatalf("misses = %d", len(cache.misses))
	}
	if _, ok := cache.miss("0", "2026-09-30", at); ok {
		t.Fatal("oldest miss survived the bound")
	}
	// Expired entries are dropped before anything live.
	cache.rememberMiss("late", "2026-09-30", "baseline_bars_incomplete", at.Add(setupProfileMissTTL+time.Minute))
	if len(cache.misses) != 1 {
		t.Fatalf("expired misses kept: %d", len(cache.misses))
	}
}

func TestSetupProfileEvictionPrefersRowsOutsideTheLiveSession(t *testing.T) {
	live := "2026-09-30"
	p := setupProfileCache{live: live, today: map[string]bool{}, rows: map[string]*setupProfileRow{}}
	for i := range 18 {
		key := fmt.Sprintf("live%02d", i)
		p.today[key] = true
		p.rows[key] = &setupProfileRow{session: live, used: uint64(10 + i)}
	}
	p.today["live18"], p.today["live19"] = true, true
	p.rows["stale"] = &setupProfileRow{session: "2026-09-29", used: 30}
	p.rows["replay"] = &setupProfileRow{session: "2026-09-15", used: 40}
	// At capacity a live row evicts the least recently used row outside the
	// live session, however recently live rows were used.
	if !p.admit(live) || p.rows["stale"] != nil || len(p.rows) != 19 {
		t.Fatalf("first eviction kept %v", slices.Sorted(maps.Keys(p.rows)))
	}
	p.rows["live18"] = &setupProfileRow{session: live, used: 50}
	if !p.admit(live) || p.rows["replay"] != nil {
		t.Fatalf("second eviction kept %v", slices.Sorted(maps.Keys(p.rows)))
	}
	p.rows["live19"] = &setupProfileRow{session: live, used: 51}
	// Only live rows remain: a replay is served without being kept, and a
	// live row past the bound replaces the least recently used live row.
	if p.admit("2026-09-15") || len(p.rows) != 20 {
		t.Fatal("a replay evicted a live profile")
	}
	if !p.admit(live) || p.rows["live00"] != nil || len(p.rows) != 19 {
		t.Fatalf("live eviction kept %v", slices.Sorted(maps.Keys(p.rows)))
	}
	// The bound follows the distinct contracts evaluated in the live session
	// up to 40.
	for i := range 25 {
		p.today[fmt.Sprintf("extra%02d", i)] = true
	}
	if p.capacity() != 40 {
		t.Fatalf("capacity = %d", p.capacity())
	}
	p.today = map[string]bool{"a": true}
	if p.capacity() != 20 {
		t.Fatalf("capacity = %d", p.capacity())
	}
}

// setupRowSessions maps each cached row's symbol (with an @date suffix for
// replay rows) to the latest session it served.
func setupRowSessions(t *testing.T, s *Server) map[string]string {
	t.Helper()
	s.setupProfiles.mu.Lock()
	defer s.setupProfiles.mu.Unlock()
	out := map[string]string{}
	for key, row := range s.setupProfiles.rows {
		raw, suffix, _ := strings.Cut(key, "}@")
		if suffix != "" {
			raw += "}"
			suffix = "@" + suffix
		}
		var c rpc.ContractParams
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		out[c.Symbol+suffix] = row.session
	}
	return out
}

func TestSetupReplaysAndOneOffNamesKeepWatchedProfiles(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	watched := make([]string, 20)
	for i := range watched {
		watched[i] = fmt.Sprintf("SYNX%02d", i)
		mustEvaluateSetup(t, s, watched[i], time.Time{})
	}
	// A replay of another session on a watched name is served but not kept:
	// every row serves the live session.
	if r := mustEvaluateSetup(t, s, watched[0], time.Date(2026, 9, 15, 10, 17, 0, 0, ny)); r.State != "watching" {
		t.Fatalf("replay: %+v", r.Reasons)
	}
	if rows := setupRowSessions(t, s); len(rows) != 20 {
		t.Fatalf("replay changed the live rows: %v", rows)
	}
	// A one-off live name grows the bound instead of evicting a watched one.
	mustEvaluateSetup(t, s, "BBB", time.Time{})
	if rows := setupRowSessions(t, s); len(rows) != 21 {
		t.Fatalf("one-off name: %v", rows)
	}
	before := map[string]int{}
	for _, name := range watched {
		mustEvaluateSetup(t, s, name, time.Time{})
		before[name], _ = src.reads(name)
	}
	for _, name := range watched[1:] {
		if before[name] < 4 || before[name] > 5 {
			t.Fatalf("%s re-read its window: %d reads", name, before[name])
		}
	}

	// Next session: half the watchlist has rolled when a new name arrives at
	// the bound. The least recently used row outside the live session goes:
	// yesterday's one-off, not a watched name that has not rolled yet.
	clock.set(time.Date(2026, 10, 1, 10, 17, 0, 0, ny))
	for _, name := range watched[:10] {
		mustEvaluateSetup(t, s, name, time.Time{})
	}
	mustEvaluateSetup(t, s, "SYNX99", time.Time{})
	rows := setupRowSessions(t, s)
	if _, ok := rows["BBB"]; ok || len(rows) != 21 {
		t.Fatalf("eviction after the session change: %v", rows)
	}
	for _, name := range watched {
		mustEvaluateSetup(t, s, name, time.Time{})
		if after, _ := src.reads(name); after != before[name]+1 {
			t.Fatalf("%s used %d reads to roll", name, after-before[name])
		}
	}
}

func TestSetupCurrentBarsAreReadOncePerCompletedBar(t *testing.T) {
	ny := setupNY(t)
	first := time.Date(2026, 9, 30, 10, 17, 0, 0, ny)
	clock := &setupTestClock{t: first}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	r1 := mustEvaluateSetup(t, s, "AAA", time.Time{})
	// Until the 10:15-10:20 bar completes, the retained read answers with
	// its own decision and acquisition clocks and the same evidence hash.
	clock.set(time.Date(2026, 9, 30, 10, 19, 59, 0, ny))
	r2 := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if _, current := src.reads("AAA"); current != 1 {
		t.Fatalf("current reads = %d before a new bar completed", current)
	}
	if r2.State != "watching" || !r2.EvaluatedAt.Equal(first) || !r2.ObservedAt.Equal(first) || r2.InputHash != r1.InputHash || len(r2.Bars) != 9 {
		t.Fatalf("reused read: %+v", r2)
	}
	second := time.Date(2026, 9, 30, 10, 20, 0, 0, ny)
	clock.set(second)
	r3 := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if _, current := src.reads("AAA"); current != 2 || !r3.EvaluatedAt.Equal(second) || len(r3.Bars) != 10 {
		t.Fatalf("new bar: reads=%d %+v", current, r3)
	}
	// A replay always reads and never feeds the live memo.
	replay := mustEvaluateSetup(t, s, "AAA", time.Date(2026, 9, 30, 10, 18, 0, 0, ny))
	if _, current := src.reads("AAA"); current != 3 || replay.EvidenceKind != "historical_reconstruction" || len(replay.Bars) != 9 {
		t.Fatalf("replay: reads=%d %+v", current, replay)
	}
	clock.set(time.Date(2026, 9, 30, 10, 21, 0, 0, ny))
	r5 := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if _, current := src.reads("AAA"); current != 3 || !r5.EvaluatedAt.Equal(second) || r5.EvidenceKind != "current_observation" {
		t.Fatalf("live read after replay: reads=%d %+v", current, r5)
	}
}

func TestSetupCurrentBarsWaitForThePublishedBar(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 20, 5, 0, ny)}
	src := newFakeSetupSource()
	src.setUnpublished(time.Date(2026, 9, 30, 10, 15, 0, 0, ny))
	s := setupTestServer(src, clock)
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); len(r.Bars) != 9 {
		t.Fatalf("lagging farm: %d bars", len(r.Bars))
	}
	// The 10:15-10:20 bar should have completed but was not in the read, so
	// the next evaluation reads again instead of reusing it.
	clock.set(time.Date(2026, 9, 30, 10, 20, 40, 0, ny))
	src.setUnpublished(time.Time{})
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); len(r.Bars) != 10 {
		t.Fatalf("published bar: %d bars", len(r.Bars))
	}
	clock.set(time.Date(2026, 9, 30, 10, 22, 0, 0, ny))
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	if _, current := src.reads("AAA"); current != 2 {
		t.Fatalf("current reads = %d", current)
	}
}

func TestSetupCurrentBarsNeverPrecedeTheirBaseline(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	// The profile is rebuilt later in the same bar (as after an eviction): a
	// read older than the baseline is not reused.
	s.setupProfiles.mu.Lock()
	s.setupProfiles.rows = nil
	s.setupProfiles.mu.Unlock()
	later := time.Date(2026, 9, 30, 10, 18, 0, 0, ny)
	clock.set(later)
	r := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if _, current := src.reads("AAA"); current != 2 || !r.BaselineObservedAt.Equal(later) || r.ObservedAt.Before(r.BaselineObservedAt) {
		t.Fatalf("current reads=%d baseline=%s observed=%s", current, r.BaselineObservedAt, r.ObservedAt)
	}
}

func TestSetupConcurrentLiveEvaluationsShareOneCurrentRead(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	waiting := make(chan struct{}, 1)
	s.setupProfiles.currentWaitForTest = func() { waiting <- struct{}{} }
	for _, tc := range []struct {
		at   time.Time
		fail error
	}{
		{at: time.Date(2026, 9, 30, 10, 20, 30, 0, ny)},
		{at: time.Date(2026, 9, 30, 10, 25, 30, 0, ny), fail: errors.New("historical data farm timed out")},
	} {
		clock.set(tc.at)
		src.setFail("AAA", tc.fail)
		_, before := src.reads("AAA")
		release := src.holdSymbol("AAA")
		first := goEvaluateSetup(t.Context(), s, "AAA")
		within(t, src.entered, "the first current read")
		second := goEvaluateSetup(t.Context(), s, "AAA")
		within(t, waiting, "the second request waiting for that read")
		release()
		for _, ch := range []<-chan setupEvaluation{first, second} {
			got := within(t, ch, "an evaluation")
			if got.err != nil {
				t.Fatal(got.err)
			}
			if tc.fail == nil && (got.result.State != "watching" || !got.result.EvaluatedAt.Equal(tc.at)) {
				t.Fatalf("shared read: %+v", got.result)
			}
			if tc.fail != nil && got.result.Reasons[0] != "current_history_unavailable: historical data farm timed out" {
				t.Fatalf("shared failure: %+v", got.result.Reasons)
			}
		}
		if _, after := src.reads("AAA"); after != before+1 {
			t.Fatalf("concurrent evaluations used %d current reads", after-before)
		}
		src.mu.Lock()
		delete(src.hold, "AAA")
		src.mu.Unlock()
	}
}
