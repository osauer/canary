package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func gammaTestJobs(n int) []gammaLegSpec {
	jobs := make([]gammaLegSpec, 0, n)
	for i := range n {
		jobs = append(jobs, gammaLegSpec{expiryYMD: "20261120", expiryDate: "2026-11-20", strike: 5000 + float64(i)*5, right: "C", tradingClass: "SPX"})
	}
	return jobs
}

func newGammaTestFanout(fetch legFetcher, linkDown func() bool) *gammaLegFanout {
	return &gammaLegFanout{
		sym:        "SPX",
		spot:       5100,
		spotAt:     time.Now(),
		dataType:   rpc.MarketDataLive,
		fetch:      fetch,
		workers:    4,
		progress:   &atomic.Int32{},
		collection: newGammaCollectionDiagnostics("SPX", nil),
		startWall:  time.Now(),
		linkDown:   linkDown,
	}
}

// The 2026-10-01 incident: a fan-out in flight when TWS reported 1100 kept
// subscribing ~100 SPX options a minute for the rest of its job list. Once the
// link latch reads down, no further leg may be requested and the compute
// fails with the backend-link error instead of publishing a partial slice.
func TestGammaFanoutStopsWhenBackendLinkDrops(t *testing.T) {
	var fetched, down atomic.Int32
	fetch := func(context.Context, *ibkrlib.Connector, string, string, string, float64, string, float64, time.Time, string) legResult {
		if fetched.Add(1) == 20 {
			down.Store(1)
		}
		return legResult{OK: true, IV: 0.2, Gamma: 0.001, OI: 10, OIObserved: true, IVSource: gammaIVSourceModelTick}
	}
	fan := newGammaTestFanout(fetch, func() bool { return down.Load() == 1 })
	legs, _, _, err := fan.run(t.Context(), gammaTestJobs(400))
	if !errors.Is(err, errGammaBackendLinkDown) {
		t.Fatalf("err = %v, want errGammaBackendLinkDown", err)
	}
	if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		t.Fatalf("backend-link abort must read as broker unavailability, got %v", err)
	}
	if legs != nil {
		t.Fatalf("partial pre-outage legs published: %d", len(legs))
	}
	// Workers already past the check may finish their leg; nothing beyond
	// one in-flight leg per worker may follow the loss.
	if got := fetched.Load(); got > 20+int32(fan.workers) {
		t.Fatalf("fetched %d legs after the link dropped at leg 20", got)
	}
}

func TestGammaFanoutLinkDownBeforeStartRequestsNothing(t *testing.T) {
	var fetched atomic.Int32
	fetch := func(context.Context, *ibkrlib.Connector, string, string, string, float64, string, float64, time.Time, string) legResult {
		fetched.Add(1)
		return legResult{}
	}
	fan := newGammaTestFanout(fetch, func() bool { return true })
	if _, _, _, err := fan.run(t.Context(), gammaTestJobs(50)); !errors.Is(err, errGammaBackendLinkDown) {
		t.Fatalf("err = %v, want errGammaBackendLinkDown", err)
	}
	if n := fetched.Load(); n != 0 {
		t.Fatalf("fetched %d legs while the link was down", n)
	}
}

func TestGammaFanoutHealthyLinkRunsEveryLeg(t *testing.T) {
	var fetched atomic.Int32
	fetch := func(context.Context, *ibkrlib.Connector, string, string, string, float64, string, float64, time.Time, string) legResult {
		fetched.Add(1)
		return legResult{OK: true, IV: 0.2, Gamma: 0.001, OI: 10, OIObserved: true, IVSource: gammaIVSourceModelTick}
	}
	fan := newGammaTestFanout(fetch, func() bool { return false })
	legs, _, _, err := fan.run(t.Context(), gammaTestJobs(40))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fetched.Load() != 40 || len(legs) != 40 {
		t.Fatalf("fetched=%d legs=%d, want 40/40", fetched.Load(), len(legs))
	}
}

// SPY failing on the backend link must not fall through to the SPX-only
// fallback, which would open the SPX hold and fan-out against a dead link.
func TestGammaCombinedSkipsSPXAfterBackendLinkLoss(t *testing.T) {
	var phases []string
	old := runGammaUnderlyingPhase
	t.Cleanup(func() { runGammaUnderlyingPhase = old })
	runGammaUnderlyingPhase = func(_ context.Context, _ *Server, _ *ibkrlib.Connector, underlying string, _ rpc.GammaZeroParams, _ *atomic.Int32, _ int32) (*rpc.GammaZeroComputed, error) {
		phases = append(phases, underlying)
		return nil, errGammaBackendLinkDown
	}
	_, err := computeGammaCombined(t.Context(), &Server{}, nil, rpc.GammaZeroParams{}, &atomic.Int32{})
	if !errors.Is(err, errGammaBackendLinkDown) {
		t.Fatalf("err = %v, want errGammaBackendLinkDown", err)
	}
	if len(phases) != 1 || phases[0] != "SPY" {
		t.Fatalf("phases = %v, want only SPY", phases)
	}
}

func TestGammaBackendLinkDownNilConnector(t *testing.T) {
	if gammaBackendLinkDown(nil) {
		t.Fatal("nil connector must not read as link down")
	}
}
