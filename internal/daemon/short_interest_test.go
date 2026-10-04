package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestShortInterestCachedSourceAndFullUniverse(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	s.marketEvents = offlineMarketEventCache(&now)
	pub := shortInterestPublication{URL: "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv", SettlementDate: "2026-09-15", FetchedAt: now.Add(-time.Hour), Rows: []rpc.ShortInterestRow{
		{Symbol: "AAA", Market: "NYSE", Name: "Synthetic Alpha", ShortInterestShares: 100, PreviousShortInterestShares: 50, AverageDailyVolume: 10, DaysToCover: new(10.0), SettlementDate: "2026-09-15"},
		{Symbol: "BBB", Market: "OTC", Name: "Synthetic Beta", ShortInterestShares: 900, AverageDailyVolume: 300, DaysToCover: new(3.0), SettlementDate: "2026-09-15"},
	}}
	read := func(p rpc.ShortInterestScreenParams) *rpc.ShortInterestScreenResult {
		t.Helper()
		p, _ = rpc.NormalizeShortInterestScreenParams(p)
		raw, _ := json.Marshal(p)
		r, err := s.handleShortInterestScreen(t.Context(), &rpc.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		if err = rpc.ValidateShortInterestScreenResult(*r, p); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read(rpc.ShortInterestScreenParams{}); r.Status != "pending" || len(r.Rows) != 0 {
		t.Fatal(r)
	}
	s.shortInterest.failed = true
	if r := read(rpc.ShortInterestScreenParams{}); r.Status != "unavailable" {
		t.Fatal(r)
	}
	s.shortInterest.publication = pub
	s.shortInterest.failed = false
	r := read(rpc.ShortInterestScreenParams{Limit: 1})
	if r.Status != "available" || r.Total != 2 || r.Matching != 2 || r.Rows[0].Symbol != "BBB" || !r.Truncated || r.Coverage.Unavailable != 2 {
		t.Fatalf("independent full universe %+v", r)
	}
	if r := read(rpc.ShortInterestScreenParams{ListedOnly: true}); r.Matching != 1 || r.Rows[0].Symbol != "AAA" {
		t.Fatal(r)
	}
	if r := read(rpc.ShortInterestScreenParams{MinPrice: 5}); r.Matching != 0 || r.Total != 2 || r.Coverage.Complete {
		t.Fatal("unknown price passed", r)
	}
	s.shortInterest.failed = true
	r = read(rpc.ShortInterestScreenParams{})
	if r.Status != "stale" || len(r.Rows) != 2 || !r.FetchedAt.Equal(pub.FetchedAt) || r.SettlementDate != "2026-09-15" {
		t.Fatal("failed source erased/reclocked publication", r)
	}
	s.shortInterest.failed = false
	now = now.Add(49 * time.Hour)
	if r := read(rpc.ShortInterestScreenParams{}); r.Status != "stale" {
		t.Fatal("old cache marked current")
	}
}

func TestShortInterestDurableCacheAndShutdown(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := &Server{socketPath: filepath.Join(dir, "canary.sock"), now: func() time.Time { return now }}
	p := shortInterestPublication{URL: "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv", SettlementDate: "2026-09-15", FetchedAt: now, Rows: []rpc.ShortInterestRow{{Symbol: "AAA", Market: "NYSE", ShortInterestShares: 100, SettlementDate: "2026-09-15"}}}
	if !validShortInterestPublication(p, now) {
		t.Fatal("valid cache rejected")
	}
	if err := writeGammaAtomicJSON(dir, "short-interest.json", p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.startShortInterestRefresh(ctx)
	s.marketData.loopWG.Wait()
	if len(s.shortInterest.publication.Rows) != 1 {
		t.Fatal("cache not restored")
	}
	p.FetchedAt = now.Add(time.Hour)
	if validShortInterestPublication(p, now) {
		t.Fatal("future cache accepted")
	}
}
