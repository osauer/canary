package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestMarketHistoryCompletedRequestsDoNotExhaustAdmission(t *testing.T) {
	s := &Server{}
	// Offline reads still complete and are briefly cached. A rotating research
	// universe must not turn completed entries into occupied work slots.
	for i := range 100 {
		p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: fmt.Sprintf("SYN%d", i), SecType: "STK", Exchange: "NYSE", Currency: "USD"}, Range: "1M"}
		_, err := s.marketHistoryRequest(t.Context(), p)
		if err == nil || strings.Contains(err.Error(), "history capacity") {
			t.Fatalf("request %d: want the underlying offline failure, got %v", i, err)
		}
		if len(s.marketData.history) > 64 {
			t.Fatalf("completed request cache grew without bound: %d", len(s.marketData.history))
		}
	}
}

func TestMarketHistoryActiveRequestsStayBounded(t *testing.T) {
	s := &Server{}
	s.marketData.history = make(map[string]*marketHistoryEntry)
	s.marketData.slots = make(chan struct{}, 2)
	for i := range 64 {
		s.marketData.history[fmt.Sprint(i)] = &marketHistoryEntry{done: make(chan struct{})}
	}
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Exchange: "NYSE", Currency: "USD"}, Range: "1M"}
	if _, err := s.marketHistoryRequest(t.Context(), p); err == nil || !strings.Contains(err.Error(), "history capacity") {
		t.Fatalf("active request bound lost: %v", err)
	}
	// One completed entry is reclaimable; active owners stay intact.
	e := s.marketData.history["0"]
	e.expires = time.Now().Add(time.Minute)
	close(e.done)
	if _, err := s.marketHistoryRequest(t.Context(), p); err == nil || strings.Contains(err.Error(), "history capacity") {
		t.Fatalf("completed entry was not reclaimed: %v", err)
	}
	if len(s.marketData.history) != 64 || s.marketData.history["1"] == nil {
		t.Fatal("admission evicted active work")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.marketHistoryRequest(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller reused work: %v", err)
	}
}
