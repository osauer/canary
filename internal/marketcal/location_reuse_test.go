package marketcal

import (
	"sync"
	"testing"
	"time"
)

func TestCalendarQueriesReuseTimezoneRules(t *testing.T) {
	// Separate calendar instances are used by history, quotes and status.
	// Each must reuse the same immutable rules, including across DST dates.
	for _, market := range AllMarkets() {
		t.Run(string(market), func(t *testing.T) {
			first, err := New().Query(Query{Market: market, Date: "2026-10-13", Days: 1})
			if err != nil || first.Session.Open.IsZero() {
				t.Fatalf("initial session: %v", err)
			}
			var workers sync.WaitGroup
			for range 16 {
				workers.Go(func() {
					for _, date := range []string{"2026-03-02", "2026-03-09", "2026-10-26", "2026-11-02"} {
						got, err := New().Query(Query{Market: market, Date: date, Days: 1})
						if err != nil || got.Session.Open.IsZero() {
							t.Errorf("session %s: %v", date, err)
							continue
						}
						if got.Session.Open.Location() != first.Session.Open.Location() {
							t.Error("calendar reloaded immutable timezone rules")
						}
					}
				})
			}
			workers.Wait()
		})
	}
}

func TestCalendarSessionLookupAllocationBudget(t *testing.T) {
	at := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			session, err := New().SessionAt(MarketUSEquity, at)
			if err != nil || !session.IsOpen {
				b.Fatal("lost open session", err)
			}
		}
	})
	t.Logf("calendar session: %d bytes/op, %d ns/op", result.AllocedBytesPerOp(), result.NsPerOp())
	if result.AllocedBytesPerOp() > 2048 {
		t.Fatal("single-session query reloads timezone data")
	}
}
