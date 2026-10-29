package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A series IBKR refuses for want of a market data permission is final for
// the trading session it answered: the 15-minute retries, and the fresh
// budget every reconnect and daemon start used to grant, repeated the same
// refusal 284 times in ten days for one Xetra line. It is asked again once
// at the next trading session, so an added subscription is picked up; it
// warns once, says what to do, and the refusal survives a restart. A read
// that succeeds clears it.
func TestMandateHistoryPermissionRefusalIsFinalForTheSession(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	db := filepath.Join(t.TempDir(), "state", "daemon.db")
	refused := true
	reads := 0
	var logs bytes.Buffer
	newServer := func() *Server {
		core := openTestCoreStore(t, db)
		t.Cleanup(func() { core.Close() })
		srv := &Server{now: func() time.Time { return now }, coreStore: core, logger: NewLogger(&logs, "debug")}
		srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, at time.Time) (*rpc.MarketHistoryResult, error) {
			reads++
			if refused {
				return nil, &ibkrlib.HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:No market data permissions for IBIS2 STK"}
			}
			day, _, _ := mandateLastSessionsIn(at, marketcal.MarketDEXetra)
			stamp, _ := time.Parse(time.DateOnly, day)
			return &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: at, Source: "synthetic", Points: []rpc.MarketHistoryPoint{{At: stamp, Value: 100}}}, nil
		}
		return srv
	}
	need := mandateHistoryNeed{Key: "strategy:s6-trend:con:7001", Contract: rpc.ContractParams{ConID: 7001, Symbol: "SYNE", SecType: "STK", Exchange: "IBIS2", Currency: "EUR"}, Market: marketcal.MarketDEXetra}
	source := &fakeSetupSource{}
	srv := newServer()
	for range 5 {
		srv.refreshMandateSeries(t.Context(), source, need)
		now = now.Add(mandateHistoryRetryAfter)
	}
	source.stale = true // a reconnect
	srv.refreshMandateSeries(t.Context(), source, need)
	source.stale = false
	srv = newServer() // a daemon restart
	srv.refreshMandateSeries(t.Context(), source, need)
	if reads != 1 {
		t.Fatalf("one session, a reconnect and a restart asked %d times, want once", reads)
	}
	if why, _ := srv.mandateSeriesFailure(bindMandateHistoryNeed(need).Key); !strings.Contains(why, "No market data permissions") {
		t.Fatalf("after the restart the verdict cannot name the refusal: %q", why)
	}

	// The next trading session asks once more; still refused, it says so at
	// INFO, not as a new warning.
	now = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
	srv.refreshMandateSeries(t.Context(), source, need)
	now = now.Add(mandateHistoryRetryAfter)
	srv.refreshMandateSeries(t.Context(), source, need)
	if reads != 2 {
		t.Fatalf("the next session asked %d times in all, want 2", reads)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "market data subscription") {
		t.Fatalf("%d warnings, want one that says what to do:\n%s", n, logs.String())
	}

	// The owner adds the subscription: the session after reads and clears it.
	refused = false
	now = time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC)
	srv.refreshMandateSeries(t.Context(), source, need)
	if reads != 3 || srv.mandateHistoryForNeed(t.Context(), need) == nil {
		t.Fatalf("reads %d, series kept %v; want the series read once it is served", reads, srv.mandateHistoryForNeed(t.Context(), need) != nil)
	}
	if why, _ := newServer().mandateSeriesFailure(bindMandateHistoryNeed(need).Key); why != "" {
		t.Fatalf("a served series still carries the refusal: %q", why)
	}
}

// Any other failed read keeps the session's retry budget.
func TestMandateHistoryOtherFailuresKeepTheirRetries(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	reads := 0
	srv := &Server{now: func() time.Time { return now }}
	srv.mandateHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		reads++
		return nil, &ibkrlib.HistoricalRequestError{Code: 162, Message: "HMDS query returned no data"}
	}
	need := mandateHistoryNeed{Key: "u:SYNX", Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"}}
	for range 5 {
		srv.refreshMandateSeries(t.Context(), &fakeSetupSource{}, need)
		now = now.Add(mandateHistoryRetryAfter)
	}
	if reads != mandateHistoryAttemptsPerSession {
		t.Fatalf("reads %d, want the session budget %d", reads, mandateHistoryAttemptsPerSession)
	}
}
