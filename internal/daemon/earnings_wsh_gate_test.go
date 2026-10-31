package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The optional WSH feed is checked once for the account, not per name
// (owner, 2026-10-31 11:39 CET: optional, only with the subscription, and not
// tried while absent). While the account is not entitled no name triggers a
// WSH request: each records the account verdict, Nasdaq is still fetched,
// and the check repeats after a day. An entitled account fetches per name;
// an inconclusive check (gateway down) leaves the per-name path as it was.
func TestEarningsSecondaryFeedIsCheckedOncePerAccount(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sym, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/analyst/"), "/")
		fmt.Fprintf(w, `{"data":{"announcement":"Earnings announcement* for %s: "},"message":null,"status":{"rCode":200}}`, sym)
	}))
	defer srv.Close()

	cache := newEarningsCacheCold(t.TempDir(), nil, nil)
	cache.fetchURL = srv.URL + "/api/analyst/%s/earnings-date"
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	cache.clock = func() time.Time { return now }
	var fetches, probes atomic.Int32
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		fetches.Add(1)
		return transportFailureResult(rpc.SourceFailureTimeout, rpc.SourceFailureStageWSHEvent, true, now), errors.New("synthetic WSH outcome")
	}); err != nil {
		t.Fatal(err)
	}
	access := earningsSecondaryNotEntitled
	cache.setSecondaryEntitlementProbe(func(context.Context) earningsSecondaryAccess {
		probes.Add(1)
		return access
	})
	refresh := func(syms ...string) {
		for _, sym := range syms {
			cache.refreshTarget(context.Background(), earningsRefreshTarget{Symbol: sym})
		}
	}
	check := func(step string, wantProbes, wantFetches int32) {
		t.Helper()
		if got := probes.Load(); got != wantProbes {
			t.Fatalf("%s: %d entitlement checks, want %d", step, got, wantProbes)
		}
		if got := fetches.Load(); got != wantFetches {
			t.Fatalf("%s: %d WSH requests, want %d", step, got, wantFetches)
		}
	}

	refresh("SYNTHA", "SYNTHB", "SYNTHC")
	check("not entitled at startup", 1, 0)
	for _, sym := range []string{"SYNTHA", "SYNTHB", "SYNTHC"} {
		state := cache.symbols[sym]
		wsh := state.Providers[earningsWSHProvider].LastAttempt
		if !earningsProviderUnentitled(wsh.Status, wsh.LastFailure) {
			t.Fatalf("%s: WSH attempt %+v, want the account's not-entitled verdict", sym, wsh)
		}
		if state.Providers[earningsNasdaqProvider].LastAttempt.Status != rpc.EarningsStatusNoDatePublished {
			t.Fatalf("%s: Nasdaq was not fetched: %+v", sym, state.Providers[earningsNasdaqProvider])
		}
		if state.Resolution.Status != rpc.EarningsStatusNoDatePublished {
			t.Fatalf("%s: an unentitled feed must not hold back Nasdaq's definitive answer: %+v", sym, state.Resolution)
		}
	}

	now = now.Add(earningsNonRetryableFailureRetry + time.Hour)
	refresh("SYNTHA", "SYNTHB")
	check("the next day re-checks once", 2, 0)

	access = earningsSecondaryEntitled
	now = now.Add(earningsNonRetryableFailureRetry + time.Hour)
	refresh("SYNTHA", "SYNTHB")
	check("a subscription added since fetches per name", 3, 2)

	access = earningsSecondaryUnknown
	now = now.Add(earningsNonRetryableFailureRetry + time.Hour)
	refresh("SYNTHC")
	check("an inconclusive check keeps the per-name path", 4, 3)
}

// The account check is one WSH metadata read: no entitlement is a definitive
// no, a metadata answer is a yes, and anything else (no gateway, a timeout,
// an old protocol) is inconclusive.
func TestProbeWSHEntitlementMapsTheMetadataAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		client wshEntitlementClient
		want   earningsSecondaryAccess
	}{
		"no gateway":   {nil, earningsSecondaryUnknown},
		"entitled":     {wshEntitlementAnswer{}, earningsSecondaryEntitled},
		"not entitled": {wshEntitlementAnswer{err: &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorEntitlementRequired, Operation: "metadata"}}, earningsSecondaryNotEntitled},
		"timeout":      {wshEntitlementAnswer{err: &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorTimeout, Operation: "metadata"}}, earningsSecondaryUnknown},
		"transport":    {wshEntitlementAnswer{err: errors.New("synthetic")}, earningsSecondaryUnknown},
	} {
		if got := probeWSHEntitlementFrom(t.Context(), tc.client); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

type wshEntitlementAnswer struct{ err error }

func (a wshEntitlementAnswer) ProbeWSHEntitlement(context.Context) error { return a.err }
