package daemon

import (
	"testing"
	"time"
)

func TestLendingHighDatesPersistDistinctAndReset(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	entry := marketEventBorrowFeeEntry{SourceURL: "ftp://synthetic.invalid/usa.txt", Symbols: map[string]marketEventBorrowFeeRecord{}}
	save := func(at time.Time, rate float64) {
		t.Helper()
		entry.AsOf = at
		entry.FetchedAt = at
		entry.Symbols["AAA"] = marketEventBorrowFeeRecord{Symbol: "AAA", ConID: "91001", Currency: "USD", FeeRate: rate}
		if err := cache.persistBorrowFeeSuccess(t.Context(), entry, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for d := 2; d >= 0; d-- {
		save(now.AddDate(0, 0, -d), 70)
	}
	save(now, 70)
	history := lendingFeeHistory(cache.borrowingDates, "AAA", "91001", now)
	if len(history) != 3 || !history[0].AsOf.Before(history[2].AsOf) || history[2].FeeRate != 70 {
		t.Fatal("canonical dated chart samples lost")
	}
	if len(lendingFeeHistory(cache.borrowingDates, "AAA", "", now)) != 0 {
		t.Fatal("unknown identity inherited history")
	}
	restarted := newMarketEventCache(func() time.Time { return now })
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if got := lendingHighDates(restarted.borrowingDates, "AAA", "91001", 50, now); got != 3 {
		t.Fatalf("restart/repeat source date: %d", got)
	}
	save(now.Add(time.Minute), 30)
	if got := lendingHighDates(cache.borrowingDates, "AAA", "91001", 50, now.Add(time.Minute)); got != 0 {
		t.Fatalf("lower rate failed to reset: %d", got)
	}
	save(now.Add(2*time.Minute), 70)
	if got := lendingHighDates(cache.borrowingDates, "AAA", "91001", 50, now.Add(2*time.Minute)); got != 1 {
		t.Fatalf("same-day recovery restored older streak: %d", got)
	}
	if got := lendingHighDates(cache.borrowingDates, "AAA", "91002", 50, now.Add(2*time.Minute)); got != 0 {
		t.Fatal("changed contract inherited streak")
	}
	if got := lendingHighDates(cache.borrowingDates, "AAA", "91001", 50, now.AddDate(0, 0, 7)); got != 0 {
		t.Fatal("old dates counted beyond week")
	}
}
