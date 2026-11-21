package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The check proves the history service itself when no chart read has, so a
// restart or a quiet stretch does not leave it "Not checked" (owner
// instruction 2026-11-21 14:05 CET).
func TestHistorySourceCheckProvesAnUncheckedService(t *testing.T) {
	now := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC) // a Saturday
	s := &Server{now: func() time.Time { return now }}
	var reads []rpc.MarketHistoryParams
	answer := func(received bool, err error) func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return func(_ context.Context, p rpc.MarketHistoryParams, _ int, _ time.Time) (*rpc.MarketHistoryResult, error) {
			reads = append(reads, p)
			s.observeHistorySource(received, err, nil, broker.SessionBinding{})
			return nil, err
		}
	}

	if row := s.feedDataHealth(historySourceID, now); row.State != "unknown" {
		t.Fatalf("a fresh daemon has not checked history: %+v", row)
	}
	s.checkHistorySource(t.Context(), answer(true, nil))
	if len(reads) != 1 || reads[0].Contract.Symbol != "SPY" {
		t.Fatalf("an unchecked service is read once, on the reference chart: %+v", reads)
	}
	if row := s.feedDataHealth(historySourceID, now); row.State != "current" {
		t.Fatalf("the check's answer proves the service: %+v", row)
	}

	// A proven service is not read again while its observation holds.
	now = now.Add(29 * time.Minute)
	s.checkHistorySource(t.Context(), answer(true, nil))
	if len(reads) != 1 {
		t.Fatalf("a current row needs no read: %d reads", len(reads))
	}

	// After a quiet stretch the observation lapses and the check proves it again.
	now = now.Add(2 * time.Minute)
	s.checkHistorySource(t.Context(), answer(true, nil))
	if row := s.feedDataHealth(historySourceID, now); len(reads) != 2 || row.State != "current" {
		t.Fatalf("a lapsed observation is proven again: %d reads, %+v", len(reads), row)
	}
}

func TestHistorySourceCheckDoesNotHammerAQuietOrBrokenService(t *testing.T) {
	now := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	reads := 0
	answer := func(received bool, err error) func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
			reads++
			s.observeHistorySource(received, err, nil, broker.SessionBinding{})
			return nil, err
		}
	}

	// An empty answer records nothing, so the row stays unchecked; the
	// check still waits out the observation window before asking again.
	s.checkHistorySource(t.Context(), answer(false, nil))
	now = now.Add(30 * time.Second)
	s.checkHistorySource(t.Context(), answer(false, nil))
	if reads != 1 {
		t.Fatalf("an empty answer was re-asked on the next tick: %d reads", reads)
	}

	// A timeout marks the service and is not repeated inside its window.
	now = now.Add(quoteHealthObservationValidity)
	s.checkHistorySource(t.Context(), answer(false, context.DeadlineExceeded))
	if row := s.feedDataHealth(historySourceID, now); reads != 2 || row.State != "unavailable" {
		t.Fatalf("a failed read marks the service: %d reads, %+v", reads, row)
	}
	now = now.Add(4 * time.Minute)
	s.checkHistorySource(t.Context(), answer(false, context.DeadlineExceeded))
	if reads != 2 {
		t.Fatalf("a failure was re-asked inside its window: %d reads", reads)
	}
	now = now.Add(time.Minute)
	s.checkHistorySource(t.Context(), answer(true, nil))
	if row := s.feedDataHealth(historySourceID, now); reads != 3 || row.State != "current" {
		t.Fatalf("after the window the service is asked again: %d reads, %+v", reads, row)
	}
}
