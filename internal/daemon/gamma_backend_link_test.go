package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

// Daemon shutdown cancels the server context every gamma compute derives
// from. A fan-out it stops was abandoned, not failed: the abort is said at
// INFO and the compute's own failure line likewise. A forced recompute
// cancelling only the superseded job keeps both WARN lines.
func TestGammaCancelWarnsOnlyWhileTheDaemonLives(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shutdown bool
	}{
		{"daemon shutdown", true},
		{"forced recompute supersedes the job", false},
	} {
		t.Run(tc.name+"/fan-out", func(t *testing.T) {
			log := &bytes.Buffer{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fetch := func(context.Context, *ibkrlib.Connector, string, string, string, float64, string, float64, time.Time, string) legResult {
				cancel()
				return legResult{OK: true, IV: 0.2, Gamma: 0.001, OI: 10, OIObserved: true, IVSource: gammaIVSourceModelTick}
			}
			fan := newGammaTestFanout(fetch, func() bool { return false })
			fan.log = gammaLogf{inner: NewLogger(log, "info")}
			fan.stopping = func() bool { return tc.shutdown }
			if _, _, _, err := fan.run(ctx, gammaTestJobs(8)); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			assertGammaCancelLine(t, log.String(), "gamma.abort reason=ctx_cancelled", tc.shutdown)
		})
		t.Run(tc.name+"/compute job", func(t *testing.T) {
			log := &bytes.Buffer{}
			parent, stop := context.WithCancel(t.Context())
			defer stop()
			now := time.Now()
			cache := newGammaZeroCacheWithStore(nil, now, NewLogger(log, "info"))
			started := make(chan struct{})
			cache.mu.Lock()
			job := cache.spawnJob(parent, rpc.GammaZeroScopeCombined, nySessionKey(now), now, 1, func(ctx context.Context, _ *atomic.Int32) (*rpc.GammaZeroComputed, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			cache.mu.Unlock()
			<-started
			if tc.shutdown {
				stop()
			} else {
				job.cancel()
			}
			<-job.done
			assertGammaCancelLine(t, log.String(), "gamma compute: scope="+rpc.GammaZeroScopeCombined+" failed: context canceled", tc.shutdown)
		})
	}
}

func assertGammaCancelLine(t *testing.T, log, line string, shutdown bool) {
	t.Helper()
	if !strings.Contains(log, line) {
		t.Fatalf("missing %q: %q", line, log)
	}
	if warned := strings.Contains(log, "level=WARN"); warned == shutdown {
		t.Fatalf("shutdown=%t warned=%t: %q", shutdown, warned, log)
	}
}
