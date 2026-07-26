package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// handleGammaZeroSPX returns the current dealer zero-gamma estimate for
// SPX (Indicator 4 of the risk-regime dashboard). The compute is heavy
// (multi-minute fan-out across hundreds of option legs) and runs on a
// daemon-internal goroutine. The RPC always returns within budget: it
// serves the last known-good snapshot when one exists, starts or joins
// a background refresh when a better same-session result is expected,
// and reports Status="computing" only when no serveable result exists.
//
// Concurrency contract:
//
//   - The first caller of an NY trading session kicks the compute. If
//     a prior known-good snapshot exists, the caller gets that ready
//     context while the refresh runs; otherwise they get
//     Status="computing" + an EtaSeconds hint.
//   - Concurrent callers within the same session share the in-flight
//     job (singleflight) — no duplicate fan-outs against the same
//     gateway slot pool.
//   - Callers after the compute finishes get Status="ready" with the
//     cached payload; future calls refresh behind it when the option
//     data session or soft TTL says a better result is likely.
//   - Force=true starts a fresh diagnostic compute. If a good cached value
//     already exists, the cache keeps serving it and promotes the forced run
//     only on success; otherwise force supersedes the in-flight/error state.
//
// Methodology lives in docs/docs/internals/regime-dashboard.md. The result
// envelope's Method field is "bs-gamma-profile-v3-stickymoneyness-0dte-split";
// renderers can deep-link to the disclosure.
func (s *Server) handleGammaZeroSPX(ctx context.Context, req *rpc.Request) (*rpc.GammaZeroSPXResult, error) {
	var p rpc.GammaZeroSPXParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}

	// Scope: which underlying(s) to compute. Empty defaults to combined
	// (the canonical headline post-step-7). Single-underlying paths
	// (--only=spy / --only=spx) first reuse the combined cache's
	// matching per-index slice when available so drill-down matches the
	// default gamma/regime view. Force=true bypasses that canonical
	// slice for the diagnostic run, but an existing good served cache is
	// promoted over only if the forced run succeeds.
	scope, scopeErr := gammaScopeForRequest(p.Scope)
	if scopeErr != nil {
		return nil, fmt.Errorf("zero-gamma: %w", scopeErr)
	}

	if !p.Force {
		now := time.Now()
		if env, ok := s.zeroGamma.snapshotCombinedSlice(scope, func() time.Time { return now }); ok {
			own := s.zeroGamma.snapshotCurrent(scope, func() time.Time { return now })
			if preferOwnGammaSnapshot(env, own) {
				return &own, nil
			}
			return &env, nil
		}
	}

	// Background ctx for the compute goroutine — independent of the
	// per-RPC ctx because the compute outlives any single client call.
	// serverCtx is set on Start and matches the daemon's lifetime, so
	// daemon shutdown cancels the compute cleanly.
	s.mu.Lock()
	parent := s.serverCtx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}

	// Build the compute closure. The cache layer owns goroutine
	// lifecycle; we hand it a function that closes over the gateway
	// connector + params.
	//
	// The closure acquires a refcounted Hold on the underlying for
	// the entire lifetime of the compute. IBKR's TWS API requires a
	// market-data subscription on the option's underlying to push
	// OPTION_COMPUTATION (msg 21) ticks for OPT subscriptions; without
	// it the model engine has no live spot anchor and the per-leg fan-
	// out lands ~0% IV/greeks (observed: 12/1256 legs at 1% coverage
	// pre-market). subManager.Hold is refcounted, so a concurrent
	// regime snapshot on the same symbol is safe — the line stays
	// open until the compute releases.
	//
	// Per-scope compute selection:
	//   combined  → SPY phase then SPX phase, with separate Holds
	//               (computeGammaCombined enforces the underlying-hold
	//               transition audit checklist item from design §7.1).
	//   spy / spx → single-underlying phase under one Hold.
	params := normalizeGammaParams(rpc.GammaZeroParams{})
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		switch scope {
		case rpc.GammaZeroScopeCombined:
			return computeGammaCombined(bgCtx, s, c, params, prog)
		case rpc.GammaZeroScopeSPX:
			return runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 0)
		default: // GammaZeroScopeSPY
			return runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
		}
	}

	var job *gammaComputation
	if p.Force {
		job = s.zeroGamma.force(parent, scope, time.Now(), computeETA, compute)
	} else {
		job, _ = s.zeroGamma.kickOrJoin(parent, scope, time.Now(), computeETA, compute)
	}

	// kickOrJoin returns (nil, false) when the session is closed and no
	// persisted result is available — the off-hours "never compute"
	// contract from gamma_zero_cache.go. There's no job to wait on; go
	// straight to snapshot, which will report Cold.
	if job != nil && p.WaitMs > 0 {
		// Cap the wait at the RPC deadline so we always return before
		// the dispatcher times us out. The per-method deadline for
		// GammaZeroSPX is intentionally long enough to make WaitMs
		// usable but shorter than the bg compute itself, so a high
		// WaitMs still returns "computing" if the compute hasn't
		// finished.
		waitCtx, waitCancel := context.WithTimeout(ctx, time.Duration(p.WaitMs)*time.Millisecond)
		defer waitCancel()
		select {
		case <-job.done:
			// compute finished — fall through to snapshot
		case <-waitCtx.Done():
			// either WaitMs elapsed or the RPC deadline fired —
			// either way, return current state
		}
	}

	env := s.zeroGamma.snapshotForScope(scope, job, time.Now)
	return &env, nil
}

func preferOwnGammaSnapshot(canonical, own rpc.GammaZeroSPXResult) bool {
	if own.Status != rpc.GammaZeroStatusReady || own.Result == nil || canonical.Result == nil {
		return false
	}
	if own.Result.AsOf.After(canonical.Result.AsOf) {
		return true
	}
	if own.Result.AsOf.Before(canonical.Result.AsOf) {
		return false
	}
	return gammaSnapshotHasSPXCacheFallback(canonical.Result) && !gammaSnapshotHasSPXCacheFallback(own.Result)
}

func gammaSnapshotHasSPXCacheFallback(result *rpc.GammaZeroComputed) bool {
	if result == nil {
		return false
	}
	for _, code := range result.Warnings {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(code)), "spx_cache_fallback") {
			return true
		}
	}
	for _, detail := range result.WarningDetails {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(detail.Code)), "spx_cache_fallback") {
			return true
		}
	}
	return false
}
