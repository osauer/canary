package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func testAccountSnapshot(at time.Time, value float64) *ibkrlib.RawAccountSummary {
	return &ibkrlib.RawAccountSummary{
		AccountID: "DU123", Currency: "USD", NetLiquidation: &value, AsOf: at,
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{"EUR": {ExchangeRate: 1.1}},
		Raw:            map[string]string{"NetLiquidation": "100"},
	}
}

func TestAccountSnapshotAuthoritySharesFlightAndCurrentResult(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	authority := accountSnapshotAuthority{now: func() time.Time { return now }}
	source := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return testAccountSnapshot(now, 100), ibkrlib.AccountSummaryProvenanceRequest, nil
	}

	const readers = 12
	results := make(chan accountSnapshot, readers)
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			result, err := authority.read(t.Context(), t.Context(), source, fetch)
			if err != nil {
				t.Errorf("read account authority: %v", err)
				return
			}
			results <- result
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	if got := calls.Load(); got != 1 {
		t.Fatalf("broker fetches = %d, want one shared request", got)
	}
	for result := range results {
		if result.provenance != ibkrlib.AccountSummaryProvenanceRequest || !result.observedAt.Equal(now) {
			t.Fatalf("result authority = (%q, %s), want request at %s", result.provenance, result.observedAt, now)
		}
		*result.raw.NetLiquidation = 999
		result.raw.Raw["NetLiquidation"] = "999"
	}

	again, err := authority.read(t.Context(), t.Context(), source, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if *again.raw.NetLiquidation != 100 || again.raw.Raw["NetLiquidation"] != "100" {
		t.Fatalf("shared snapshot was mutated by a caller: %+v", again.raw)
	}
}

func TestAccountSnapshotAuthorityNeverCachesUnstampedFallback(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	authority := accountSnapshotAuthority{now: func() time.Time { return now }}
	source := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}}
	var calls atomic.Int32
	fetch := func(context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		calls.Add(1)
		return testAccountSnapshot(now, 100), ibkrlib.AccountSummaryProvenanceCachedFallback, nil
	}

	for range 2 {
		result, err := authority.read(t.Context(), t.Context(), source, fetch)
		if err != nil {
			t.Fatal(err)
		}
		if result.provenance != ibkrlib.AccountSummaryProvenanceCachedFallback || !result.observedAt.IsZero() {
			t.Fatalf("fallback gained decision authority: provenance=%q observed_at=%s", result.provenance, result.observedAt)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fallback fetches = %d, want retry rather than cached authority", got)
	}
}

func TestAccountSnapshotAuthorityDoesNotCrossBrokerScope(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	authority := accountSnapshotAuthority{now: func() time.Time { return now }}
	var calls atomic.Int32
	fetch := func(context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		calls.Add(1)
		return testAccountSnapshot(now, 100), ibkrlib.AccountSummaryProvenanceRequest, nil
	}
	first := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}}
	second := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModeLive}}
	if _, err := authority.read(t.Context(), t.Context(), first, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.read(t.Context(), t.Context(), second, fetch); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("broker fetches across paper/live scopes = %d, want 2", got)
	}
}

func TestAccountSnapshotAuthorityCallerCancellationDoesNotCancelSharedFlight(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	authority := accountSnapshotAuthority{now: func() time.Time { return now }}
	source := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-release:
			return testAccountSnapshot(now, 100), ibkrlib.AccountSummaryProvenanceRequest, nil
		}
	}

	cancelledCtx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, err := authority.read(cancelledCtx, t.Context(), source, fetch)
		firstDone <- err
	}()
	<-started
	cancel()
	if err := <-firstDone; err == nil {
		t.Fatal("cancelled caller unexpectedly succeeded")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := authority.read(t.Context(), t.Context(), source, fetch)
		secondDone <- err
	}()
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatalf("remaining caller lost shared flight: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("broker fetches = %d, want cancelled waiter to leave shared request running", got)
	}
}

type fakeDailyPnLAuthorityConnector struct {
	snapshot       ibkrlib.AccountDailyPnL
	hasSnapshot    bool
	subscribed     []string
	repairOpenArgs []bool
}

func (f *fakeDailyPnLAuthorityConnector) AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool) {
	return f.snapshot, f.hasSnapshot
}

func (f *fakeDailyPnLAuthorityConnector) SubscribeAccountPnL(account string) error {
	f.subscribed = append(f.subscribed, account)
	return nil
}

func (f *fakeDailyPnLAuthorityConnector) MaybeResubscribeStaleDailyPnL(marketOpen bool) bool {
	f.repairOpenArgs = append(f.repairOpenArgs, marketOpen)
	return false
}

func TestMaintainDailyPnLAuthorityOwnsSubscriptionAndRepair(t *testing.T) {
	connector := &fakeDailyPnLAuthorityConnector{}
	maintainDailyPnLAuthority(connector, "DU123", true)
	if len(connector.subscribed) != 1 || connector.subscribed[0] != "DU123" {
		t.Fatalf("subscription attempts = %v, want DU123", connector.subscribed)
	}
	if len(connector.repairOpenArgs) != 1 || !connector.repairOpenArgs[0] {
		t.Fatalf("repair calls = %v, want one market-open check", connector.repairOpenArgs)
	}
}
