package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// tapeBrokerFixture answers every tape history read with synthetic daily bars
// named for the requested contract, as the broker would after resolving it.
func tapeBrokerFixture(t *testing.T, reads *atomic.Int32) (time.Time, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error)) {
	t.Helper()
	now, spx, _, _ := tapeFixture(t)
	ids := map[string]int{"SPX": 1, "QQQ": 2, "SPY": 3, "VIX": 4}
	for i, m := range tapeLeaderWeights {
		ids[m.symbol] = 10 + i
	}
	return now, func(_ context.Context, p rpc.MarketHistoryParams, _ int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		reads.Add(1)
		r := *spx
		r.Contract = p.Contract
		r.Contract.ConID = ids[p.Contract.Symbol]
		r.Range = p.Range
		r.Points = append([]rpc.MarketHistoryPoint(nil), spx.Points...)
		r.RequestedStart = r.Points[0].At
		r.Start, r.End = r.Points[0].At, r.Points[len(r.Points)-1].At
		return &r, nil
	}
}

// The tape's research inputs live in the shared chart cache, which holds 750
// series and drops the least recently written. A busy day of other chart reads
// pushes them out after the evening collection. An interactive tape read after
// that must still see SPY, VIX and the leaders, and must not replace the
// complete evening capture with a poorer one.
func TestInteractiveTapeAfterChartCacheEvictionKeepsTheEveningCapture(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	var reads atomic.Int32
	now, fetch := tapeBrokerFixture(t, &reads)
	s := &Server{coreStore: store, now: func() time.Time { return now }, marketHistoryFetchForTest: fetch}

	// The evening collection: every leg through the bounded request path.
	evening, err := s.buildAndArchiveMarketTape(t.Context(), rpc.MarketTapeParams{Sessions: 5}, s.marketHistoryRequest)
	if err != nil {
		t.Fatal(err)
	}
	date := evening.Sessions[len(evening.Sessions)-1].Date
	before, _, _, err := loadTapeDay(t.Context(), store, date)
	if err != nil || before.Latest.Session.SPY == nil || before.Latest.Session.VIX == nil || before.Latest.Session.Leaders == nil || before.Latest.Session.Leaders.Price == nil {
		t.Fatalf("evening capture incomplete: %+v %v", before.Latest.Session, err)
	}

	// Other chart reads fill the cache past its bound.
	for i := range 751 {
		hash := sha256.Sum256(fmt.Appendf(nil, "other series %d", i))
		if _, err := store.SaveChartCache(t.Context(), hex.EncodeToString(hash[:]), []byte(`{"other":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	// The bounded request path keeps completed reads for a minute; an
	// interactive call hours later finds none of them.
	s.marketData.mu.Lock()
	s.marketData.history = nil
	s.marketData.mu.Unlock()

	now = now.Add(time.Hour)
	result, err := s.handleMarketTape(t.Context(), &rpc.Request{Params: []byte(`{"sessions":5}`)})
	if err != nil {
		t.Fatal(err)
	}
	row := result.Sessions[len(result.Sessions)-1]
	if row.SPY == nil || row.VIX == nil || row.Leaders == nil || row.Leaders.Price == nil {
		t.Errorf("interactive tape lost its research legs after eviction: spy %v vix %v leaders %+v", row.SPY != nil, row.VIX != nil, row.Leaders)
	}
	after, _, _, err := loadTapeDay(t.Context(), store, date)
	if err != nil {
		t.Fatal(err)
	}
	if tapeSessionLegs(after.Latest.Session) < tapeSessionLegs(before.Latest.Session) {
		t.Errorf("a poorer interactive capture replaced the evening one: %d legs, was %d", tapeSessionLegs(after.Latest.Session), tapeSessionLegs(before.Latest.Session))
	}
}

// A capture with fewer measured legs never replaces a fuller latest one; an
// equally complete correction still does.
func TestTapeDayKeepsTheMoreCompleteCapture(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	now, spx, qqq, breadth := tapeFixture(t)
	tape, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	full := tape.Sessions[len(tape.Sessions)-1]
	full.SPY = &rpc.MarketTapePrice{Close: 500}
	full.VIX = &rpc.MarketTapePrice{Close: 20}
	save := func(row rpc.MarketTapeSession, at time.Time) {
		t.Helper()
		capture, fingerprint, err := tapeArchiveCapture(row, nil, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveTapeDay(t.Context(), store, capture, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	latest := func() rpc.MarketTapeSession {
		t.Helper()
		day, _, _, err := loadTapeDay(t.Context(), store, full.Date)
		if err != nil {
			t.Fatal(err)
		}
		return day.Latest.Session
	}
	save(full, now)
	poorer := full
	poorer.SPY, poorer.VIX = nil, nil
	save(poorer, now.Add(time.Hour))
	if got := latest(); got.SPY == nil || got.VIX == nil {
		t.Fatal("a capture missing SPY and VIX replaced a complete one")
	}
	corrected := full
	corrected.SPX = &rpc.MarketTapePrice{Close: full.SPX.Close + 1}
	save(corrected, now.Add(2*time.Hour))
	if got := latest(); got.SPX == nil || got.SPX.Close != corrected.SPX.Close {
		t.Fatal("an equally complete correction did not replace the latest capture")
	}
}
