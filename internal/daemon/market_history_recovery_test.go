package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestHistoryReconciliationFailureAllowsTailAndLaterFullRecovery(t *testing.T) {
	s, p, key, first, original := historyFixture(t)
	seed := func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &original, nil
	}
	if _, err := s.readRetainedHistory(t.Context(), key, p, first, seed); err != nil {
		t.Fatal(err)
	}
	clock := first.AddDate(0, 0, 10)
	s.now = func() time.Time { return clock }
	s.marketData.interest = map[string]marketHistoryInterest{key: {Params: p, Until: clock.Add(24 * time.Hour)}}
	fullCalls, tailCalls := 0, 0
	fullWorks := false
	fetch := func(_ context.Context, _ rpc.MarketHistoryParams, tail int, at time.Time) (*rpc.MarketHistoryResult, error) {
		if tail <= 0 {
			fullCalls++
			if !fullWorks {
				return nil, errors.New("synthetic full-range timeout")
			}
		} else {
			tailCalls++
		}
		r := original
		r.AsOf = at
		r.Points = slices.Clone(original.Points)
		if tail > 0 {
			r.RequestedStart = at.AddDate(0, 0, -tail)
			r.Points = slices.DeleteFunc(r.Points, func(p rpc.MarketHistoryPoint) bool { return p.At.Before(r.RequestedStart) })
		}
		r.Points = append(r.Points, rpc.MarketHistoryPoint{At: time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC), Value: 102})
		r.Start, r.End = r.Points[0].At, r.Points[len(r.Points)-1].At
		return &r, nil
	}
	request := func(ctx context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		return s.readRetainedHistory(ctx, key, p, clock, fetch)
	}
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	clock = clock.Add(2 * time.Minute)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	saved, _, err := s.loadMarketHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if fullCalls != 1 || tailCalls != 1 || !saved.Result.End.After(original.End) || !saved.FullReadAt.Equal(first) {
		t.Fatalf("tail must advance without clearing full-read debt: full=%d tail=%d end=%s full_at=%s", fullCalls, tailCalls, saved.Result.End, saved.FullReadAt)
	}
	if item := s.marketData.interest[key]; item.Failures != 0 {
		t.Fatalf("successful tail remained backed off: %+v", item)
	}
	got, err := request(t.Context(), p)
	if err != nil || !got.Cache.ReconciliationDue || got.Cache.Detail == "" || got.Cache.RefreshFailed || fullCalls != 1 || tailCalls != 1 {
		t.Fatalf("current tail must disclose pending reconciliation without refetch: %+v %v", got, err)
	}
	fullWorks = true
	clock = clock.Add(14 * time.Minute)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	saved, _, err = s.loadMarketHistory(t.Context(), key)
	if err != nil || fullCalls != 2 || !saved.FullReadAt.Equal(clock) {
		t.Fatalf("full retry did not recover: full=%d saved=%+v err=%v", fullCalls, saved, err)
	}
	if got, err := request(t.Context(), p); err != nil || got.Cache.ReconciliationDue {
		t.Fatalf("reconciled range remains pending: %+v %v", got, err)
	}
}

func TestHistoryReconciliationBackoffNeverSplicesChangedOverlap(t *testing.T) {
	s, p, key, first, original := historyFixture(t)
	overlap := rpc.MarketHistoryPoint{At: original.End.AddDate(0, 0, -3), Value: 100}
	original.Points = slices.Insert(original.Points, 1, overlap)
	if _, err := s.readRetainedHistory(t.Context(), key, p, first, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &original, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := first.AddDate(0, 0, 10)
	full, tails := 0, 0
	fetch := func(_ context.Context, _ rpc.MarketHistoryParams, tail int, at time.Time) (*rpc.MarketHistoryResult, error) {
		if tail <= 0 {
			full++
			return nil, errors.New("synthetic full-range timeout")
		}
		tails++
		r := original
		r.AsOf, r.RequestedStart = at, overlap.At
		r.Points = []rpc.MarketHistoryPoint{{At: overlap.At, Value: 50}, {At: original.End, Value: 51}, {At: at.Add(-time.Hour), Value: 52}}
		r.Start, r.End = r.Points[0].At, r.Points[2].At
		return &r, nil
	}
	if _, err := s.readRetainedHistory(t.Context(), key, p, clock, fetch); err != nil {
		t.Fatal(err)
	}
	got, err := s.readRetainedHistory(t.Context(), key, p, clock.Add(2*time.Minute), fetch)
	if err != nil || !got.Cache.RefreshFailed || !got.End.Equal(original.End) || full != 1 || tails != 1 {
		t.Fatalf("changed overlap bypassed full-read gate: result=%+v full=%d tails=%d err=%v", got, full, tails, err)
	}
	saved, _, err := s.loadMarketHistory(t.Context(), key)
	if err != nil || !slices.EqualFunc(saved.Result.Points, original.Points, func(a, b rpc.MarketHistoryPoint) bool { return a.At.Equal(b.At) && a.Value == b.Value }) {
		t.Fatal("correction failure changed last-good history", err)
	}
}

func TestHistoryReconciliationReservationBoundsConcurrentReaders(t *testing.T) {
	s := &Server{}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for range 32 {
		wg.Go(func() {
			if s.reserveHistoryReconciliation("one-series", now) {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent full reads admitted: %d", admitted.Load())
	}
	if !s.reserveHistoryReconciliation("one-series", now.Add(15*time.Minute)) {
		t.Fatal("reservation never permits recovery")
	}
}
