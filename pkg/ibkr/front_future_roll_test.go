package ibkr

import (
	"testing"
	"time"
)

func TestFrontFutureRollCutoffIsolationAndAge(t *testing.T) {
	for _, hedgeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "hedge first", false: "ordinary first"}[hedgeFirst], func(t *testing.T) {
			c := frontFutureTestConnector(t)
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			want := Contract{Symbol: "MES", SecType: "FUT", Currency: "USD", Exchange: "CME"}
			near := ContractDetailsLite{ConID: 1, Symbol: "MES", TradingClass: "MES", SecType: "FUT", Currency: "USD", Exchange: "CME", Expiry: "20260918"}
			later := near
			later.ConID = 2
			later.Expiry = "20261218"
			calls := 0
			fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
				calls++
				return []ContractDetailsLite{near, later}, nil
			}
			read := func(hedge bool) {
				t.Helper()
				cutoff := now
				id := 1
				if hedge {
					cutoff = now.Add(7 * 24 * time.Hour)
					id = 2
				}
				got, err := c.frontFutureAfter(t.Context(), want, now, cutoff, time.Second, fetch)
				if err != nil || got.ConID != id {
					t.Fatalf("hedge=%v got=%+v err=%v", hedge, got, err)
				}
			}
			read(hedgeFirst)
			read(!hedgeFirst)
			read(hedgeFirst)
			if calls != 2 {
				t.Fatalf("want two reusable cutoff identities, calls=%d", calls)
			}
			now = now.Add(frontFutureCacheAge + time.Minute)
			read(true)
			if calls != 3 {
				t.Fatalf("roll window extended cache freshness: calls=%d", calls)
			}
			now = time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
			got, err := c.frontFuture(t.Context(), want, now, time.Second, fetch)
			if err != nil || got.ConID != 2 {
				t.Fatalf("expiry transition=%+v err=%v", got, err)
			}
		})
	}
}
