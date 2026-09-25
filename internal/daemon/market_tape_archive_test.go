package daemon

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func tapeArchiveTestStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func archiveTapeFixture(t *testing.T, store *corestore.Store, now time.Time, rows ...rpc.MarketTapeSession) {
	t.Helper()
	if _, err := archiveMarketTape(t.Context(), store, &rpc.MarketTapeResult{AsOf: now, Sessions: rows}); err != nil {
		t.Fatal(err)
	}
}

func TestMarketTapeArchiveRetainsOriginalCorrectionsAndDeduplicatesClocks(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	store := tapeArchiveTestStore(t, path)
	now, spx, qqq, breadth := tapeFixture(t)
	tape, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archiveMarketTape(t.Context(), store, tape); err != nil {
		t.Fatal(err)
	}
	date := tape.Sessions[4].Date
	original, _, _, err := loadTapeDay(t.Context(), store, date)
	if err != nil {
		t.Fatal(err)
	}
	if original.First.Timing != "reconstructed" || original.First.Revision != 1 || original.First.Session.SPX.WindowChangePct != nil {
		t.Fatal("initial import claims advance knowledge or retains a view-dependent base")
	}
	// Wider display range, source refresh clocks and producer recomputation
	// must not generate more versions of the same measured day.
	tape, err = buildMarketTape(rpc.MarketTapeParams{Sessions: 20}, now.Add(time.Minute), spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	tape.Sessions[19].Breadth.Participation.RecordedAt = now.Add(time.Minute)
	if _, err := archiveMarketTape(t.Context(), store, tape); err != nil {
		t.Fatal(err)
	}
	day, _, _, _ := loadTapeDay(t.Context(), store, date)
	if day.Latest.Revision != 1 || !day.Latest.CapturedAt.Equal(now) {
		t.Fatal("unchanged values were recaptured as new evidence")
	}
	// Correct, then revert: both are versions; the first remains unchanged.
	for i, price := range []float64{200, original.First.Session.SPX.Close} {
		tape.AsOf = now.Add(time.Duration(i+2) * time.Minute)
		tape.Sessions[19].SPX.Close = price
		if _, err := archiveMarketTape(t.Context(), store, tape); err != nil {
			t.Fatal(err)
		}
	}
	day, _, _, _ = loadTapeDay(t.Context(), store, date)
	if day.Latest.Revision != 3 || day.First.Session.SPX.Close != original.First.Session.SPX.Close || !day.First.CapturedAt.Equal(now) {
		t.Fatal("correction overwrote original or revert disappeared")
	}
	observations, err := store.ListObservations(t.Context(), corestore.ObservationQuery{ScopeKey: tapeDayScope(date), Kind: tapeSessionKind})
	if err != nil || len(observations) != 3 {
		t.Fatalf("immutable revisions: %d %v", len(observations), err)
	}
	for i, observation := range observations {
		var capture rpc.MarketTapeCapture
		if err := json.Unmarshal(observation.Payload, &capture); err != nil {
			t.Fatal(err)
		}
		if observation.DecisionEligible || capture.Revision != int64(i+1) || !observation.ObservedAt.Equal(capture.CapturedAt) {
			t.Fatal("archive was promoted to decision authority or capture time was backdated")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := tapeArchiveTestStore(t, path)
	// No connector or in-memory history: the public read works after restart.
	s := &Server{coreStore: reopened, now: func() time.Time { return now.Add(5 * time.Minute) }}
	got, err := s.handleMarketTape(t.Context(), &rpc.Request{Params: json.RawMessage(`{"history":true,"sessions":5}`)})
	if err != nil || got.History.Rows[4].Latest.Revision != 3 || got.History.Rows[4].First.Revision != 1 {
		t.Fatalf("restart lost versions: %v", err)
	}
}

func TestMarketTapeArchiveSurvivesRollingWindowAndHistoryReadDoesNotWrite(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	calendar, err := tapeArchiveCalendar(start, 140)
	if err != nil {
		t.Fatal(err)
	}
	if len(calendar) < 80 {
		t.Fatal("fixture too short")
	}
	for i, session := range calendar[:80] {
		archiveTapeFixture(t, store, session.Close.Add(time.Hour), rpc.MarketTapeSession{Date: session.Date, SPX: &rpc.MarketTapePrice{Close: 100 + float64(i)}, QQQ: &rpc.MarketTapePrice{Close: 200 + float64(i)}})
	}
	statusBefore, _, err := store.GetStateDocument(t.Context(), tapeArchiveScope, tapeArchiveKind)
	if err != nil {
		t.Fatal(err)
	}
	before := calendar[5].Date
	got, err := readMarketTapeHistory(t.Context(), store, rpc.MarketTapeParams{Sessions: 5, History: true, Before: before}, calendar[79].Close.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History.Rows) != 5 || got.History.Rows[0].Date != calendar[0].Date || got.History.Rows[0].First == nil || got.History.NextBefore != calendar[0].Date {
		t.Fatal("oldest records were rolled away or pagination skips days")
	}
	if value := got.History.Rows[0].FollowUps[1].SPXChangePct; value == nil || math.Abs(*value-3) > 1e-9 {
		t.Fatalf("old forward closes no longer available: %v", value)
	}
	statusAfter, _, _ := store.GetStateDocument(t.Context(), tapeArchiveScope, tapeArchiveKind)
	if statusAfter.Revision != statusBefore.Revision {
		t.Fatal("history read mutated collection state")
	}
}

func TestMarketTapeFollowUpsUseOfficialDaysAndSeparateWaitingMissingZero(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	// Friday before Labor Day: one trading day is Tuesday, three is Thursday.
	now := time.Date(2026, 9, 9, 22, 0, 0, 0, time.UTC)
	archiveTapeFixture(t, store, now,
		rpc.MarketTapeSession{Date: "2026-09-04", SPX: &rpc.MarketTapePrice{Close: 100}, QQQ: &rpc.MarketTapePrice{Close: 200}},
		rpc.MarketTapeSession{Date: "2026-09-08", SPX: &rpc.MarketTapePrice{Close: 100}},
	)
	got, err := readMarketTapeHistory(t.Context(), store, rpc.MarketTapeParams{Sessions: 5, History: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	var friday rpc.MarketTapeHistoryRow
	for _, row := range got.History.Rows {
		if row.Date == "2026-09-04" {
			friday = row
		}
	}
	one, three := friday.FollowUps[0], friday.FollowUps[1]
	if one.Date != "2026-09-08" || one.Status != "partial" || one.SPXChangePct == nil || *one.SPXChangePct != 0 || one.QQQChangePct != nil {
		t.Fatalf("holiday, zero or missing handling: %+v", one)
	}
	if three.Date != "2026-09-10" || three.Status != "pending" || three.SPXChangePct != nil {
		t.Fatalf("future data became an outcome: %+v", three)
	}
	if five := friday.FollowUps[2]; five.Sessions != 5 || five.Date != "2026-09-14" || five.Status != "pending" {
		t.Fatalf("five-session follow-up skipped the holiday incorrectly: %+v", five)
	}
	got, err = readMarketTapeHistory(t.Context(), store, rpc.MarketTapeParams{Sessions: 5, History: true, Before: "2026-09-10"}, now.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got.History.Rows {
		if row.Date == "2026-09-04" && (row.FollowUps[1].Status != "unavailable" || row.FollowUps[1].SPXChangePct != nil) {
			t.Fatal("closed but missing target remained waiting or was invented")
		}
	}
	// A failed refresh is retained as a version but cannot erase known closes.
	archiveTapeFixture(t, store, now.Add(time.Minute), rpc.MarketTapeSession{Date: "2026-09-04", Breadth: &rpc.MarketTapeBreadth{PctAbove50DMA: new(40.0)}})
	day, _, _, err := loadTapeDay(t.Context(), store, "2026-09-04")
	if err != nil || day.SPX == nil || day.SPX.Value != 100 || day.SPX.Revision != 1 {
		t.Fatal("source outage erased the original close")
	}
}

func TestMarketTapeArchiveCaptureTimingAndConcurrentDuplicate(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if _, err := updateTapeArchiveStatus(t.Context(), store, start, false); err != nil {
		t.Fatal(err)
	}
	row := rpc.MarketTapeSession{Date: "2026-09-24", SPX: &rpc.MarketTapePrice{Close: 100}}
	now := time.Date(2026, 9, 24, 21, 0, 0, 0, time.UTC)
	archiveTapeFixture(t, store, now, row)
	day, _, _, _ := loadTapeDay(t.Context(), store, row.Date)
	if day.First.Timing != "before_next_open" {
		t.Fatal("fresh after-close capture incorrectly labelled as reconstructed")
	}
	row.SPX.Close = 101
	capture, hash, err := tapeArchiveCapture(row, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	capture.Timing = "before_next_open"
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := saveTapeDay(context.Background(), store, capture, hash); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	observations, err := store.ListObservations(t.Context(), corestore.ObservationQuery{ScopeKey: tapeDayScope(row.Date)})
	if err != nil || len(observations) != 2 {
		t.Fatalf("concurrent duplicates: %d %v", len(observations), err)
	}
	row.SPX.Close = 102
	archiveTapeFixture(t, store, now.AddDate(0, 0, 1), row)
	day, _, _, _ = loadTapeDay(t.Context(), store, row.Date)
	if day.First.Timing != "before_next_open" || day.Latest.Timing != "reconstructed" {
		t.Fatal("late correction masquerades as known before next open")
	}
	// A pre-settle or invented weekend session must not be accepted.
	for _, bad := range []rpc.MarketTapeResult{{AsOf: now.Add(-50 * time.Minute), Sessions: []rpc.MarketTapeSession{row}}, {AsOf: now.AddDate(0, 0, 3), Sessions: []rpc.MarketTapeSession{{Date: "2026-09-26", SPX: &rpc.MarketTapePrice{Close: 100}}}}} {
		if _, err := archiveMarketTape(t.Context(), store, &bad); err == nil {
			t.Fatal("unsettled or non-session row archived")
		}
	}
}

func TestMarketTapeCollectionCadenceUsesCloseAndStartup(t *testing.T) {
	for _, tc := range []struct {
		last, now string
		want      bool
	}{
		{"", "2026-09-24T10:00:00Z", true},
		{"2026-09-24T10:00:00Z", "2026-09-24T20:10:00Z", false},
		{"2026-09-24T10:00:00Z", "2026-09-24T20:15:00Z", true},
		{"2026-09-24T20:15:00Z", "2026-09-24T20:45:00Z", true},
		{"2026-09-24T22:55:00Z", "2026-09-25T12:00:00Z", false},
		{"2026-09-25T22:55:00Z", "2026-09-26T21:00:00Z", false},
		// Thanksgiving's shortened Friday session closes at 18:00 UTC.
		{"2026-11-27T10:00:00Z", "2026-11-27T18:15:00Z", true},
	} {
		last, _ := time.Parse(time.RFC3339, tc.last)
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := tapeCollectionDue(last, now); got != tc.want {
			t.Errorf("last=%s now=%s: %v", tc.last, tc.now, got)
		}
	}
}

func TestMarketTapeCollectorBoundsRequestsAndKeepsFailureVisible(t *testing.T) {
	s := &Server{}
	read := func(ctx context.Context) (*rpc.MarketTapeResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > marketHistoryRefreshWindow || time.Until(deadline) < 3*time.Minute || ibkrlib.RequestPriorityFrom(ctx) != ibkrlib.PriorityBackground {
			t.Fatal("unbounded collection or interactive priority")
		}
		if !s.marketTapeCollecting.Load() {
			t.Fatal("active collection invisible to idle shutdown")
		}
		return nil, context.DeadlineExceeded
	}
	if s.collectMarketTape(t.Context(), read) || !s.marketTapeArchiveFailed.Load() || s.marketTapeCollecting.Load() {
		t.Fatal("failed collection masquerades as success or stays busy")
	}
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	now, spx, qqq, _ := tapeFixture(t)
	s.coreStore, s.now = store, func() time.Time { return now }
	var mu sync.Mutex
	var requests []rpc.MarketHistoryParams
	result, err := s.buildAndArchiveMarketTape(t.Context(), rpc.MarketTapeParams{Sessions: 60}, func(_ context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		mu.Lock()
		requests = append(requests, p)
		mu.Unlock()
		if p.Contract.Symbol == "SPX" {
			return spx, nil
		}
		return qqq, nil
	})
	if err != nil || result.Archive == nil || len(requests) != 15 {
		t.Fatalf("bounded collection: %d %v", len(requests), err)
	}
	allowed := map[string]bool{"SPX": true, "QQQ": true}
	for _, c := range tapeResearchContracts() {
		allowed[c.Symbol] = true
	}
	for _, p := range requests {
		if p.Range != "6M" || !allowed[p.Contract.Symbol] {
			t.Fatal("archive expanded acquisition beyond the fixed benchmark/basket histories")
		}
		delete(allowed, p.Contract.Symbol)
	}
	if len(allowed) != 0 {
		t.Fatal("duplicate request or missing basket member")
	}
	if s.marketTapeArchiveFailed.Load() {
		t.Fatal("successful storage failed to clear failure")
	}
}
