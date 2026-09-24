package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestBorrowInventoryRejectsExpiredOrUndatedReceipt(t *testing.T) {
	now := time.Now().UTC()
	for _, at := range []time.Time{{}, now.Add(-3 * time.Minute), now.Add(time.Minute)} {
		md := ibkr.MarketData{ShortableObserved: true, ShortableShares: 0, ShortableTickAt: at, DataType: "live"}
		if _, ok := marketEventBorrowInventoryFlag("SYNTH", md, now); ok {
			t.Fatalf("invalid receipt became a current borrow flag: %s", at)
		}
	}
	md := ibkr.MarketData{ShortableObserved: true, ShortableShares: 0, ShortableTickAt: now, DataType: "live"}
	if _, ok := marketEventBorrowInventoryFlag("SYNTH", md, now); !ok {
		t.Fatal("observed zero inventory was discarded")
	}
}

func TestBorrowInventoryReceiptLifetimeAndRecovery(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	c := newMarketEventCache(func() time.Time { return now })
	first := now.Add(-time.Minute)
	ticks := map[string]*ibkr.MarketData{"SYNTH_A": {ShortableObserved: true, ShortableShares: 0, ShortableTickAt: first, DataType: "live"}}
	var calls atomic.Int32
	probe := func(context.Context, string) (*ibkr.MarketData, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	}
	read := func() (rpc.SourceHealth, rpc.MarketEventsResult) {
		t.Helper()
		res := rpc.MarketEventsResult{}
		health := c.readBorrowInventory(t.Context(), []string{"SYNTH_A", "SYNTH_B"}, nil, ibkr.ConnectorSessionBinding{}, &res, func() bool { return true }, func(sym string) *ibkr.MarketData { return ticks[sym] }, probe)
		return health, res
	}
	health, res := read()
	if health.Status != rpc.SourceStatusPartial || health.AsOf != first || health.NextAttempt == nil || len(res.Flags) != 1 {
		t.Fatalf("partial coverage not preserved: %+v flags=%d", health, len(res.Flags))
	}
	now = now.Add(4 * time.Second)
	ticks = nil // releasing the short subscription discarded the connector row
	health, res = read()
	if health.Status != rpc.SourceStatusPartial || health.AsOf != first || calls.Load() != 1 || len(res.Flags) != 1 {
		t.Fatalf("subscription release erased current receipt: %+v calls=%d", health, calls.Load())
	}
	ticks = map[string]*ibkr.MarketData{"SYNTH_B": {ShortableObserved: true, ShortableShares: 25000, ShortableTickAt: now, DataType: "live"}}
	health, _ = read()
	if health.Status != rpc.SourceStatusOK || health.AsOf != first || health.NextAttempt != nil || calls.Load() != 1 {
		t.Fatalf("new tick did not supersede backoff: %+v calls=%d", health, calls.Load())
	}
	now = now.Add(3 * time.Minute)
	ticks = nil
	health, res = read()
	if health.Status != rpc.SourceStatusUnknown || !health.AsOf.IsZero() || len(res.Flags) != 0 || health.NextAttempt == nil {
		t.Fatalf("retention renewed expired tick: %+v flags=%d", health, len(res.Flags))
	}
	c.clearShortableAbsence()
	before := calls.Load()
	_, _ = read()
	if calls.Load() != before+2 {
		t.Fatal("reconnect retained negative probe authority")
	}
}

func TestBorrowInventoryCancellationAndSessionLoss(t *testing.T) {
	for _, sessionLoss := range []bool{false, true} {
		t.Run(fmtBool(sessionLoss), func(t *testing.T) {
			now := time.Now().UTC()
			c := newMarketEventCache(func() time.Time { return now })
			var current atomic.Bool
			current.Store(true)
			started, finish := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probe := func(context.Context, string) (*ibkr.MarketData, error) {
				close(started)
				<-finish
				return &ibkr.MarketData{ShortableObserved: true, ShortableShares: 0, ShortableTickAt: now}, nil
			}
			done := make(chan rpc.SourceHealth, 1)
			res := rpc.MarketEventsResult{}
			go func() {
				done <- c.readBorrowInventory(ctx, []string{"SYNTH"}, nil, ibkr.ConnectorSessionBinding{}, &res, current.Load, func(string) *ibkr.MarketData { return nil }, probe)
			}()
			<-started
			queuedCtx, queuedCancel := context.WithCancel(t.Context())
			queued := make(chan rpc.SourceHealth, 1)
			go func() {
				queued <- c.readBorrowInventory(queuedCtx, []string{"SYNTH"}, nil, ibkr.ConnectorSessionBinding{}, &rpc.MarketEventsResult{}, current.Load, func(string) *ibkr.MarketData { return nil }, probe)
			}()
			queuedCancel()
			select {
			case h := <-queued:
				if h.Status != rpc.SourceStatusUnknown {
					t.Fatal("queued cancellation established data")
				}
			case <-time.After(time.Second):
				t.Fatal("queued cancellation blocked")
			}
			if sessionLoss {
				current.Store(false)
				c.clearShortableAbsence()
			} else {
				cancel()
			}
			close(finish)
			h := <-done
			if h.Status != rpc.SourceStatusUnknown || len(res.Flags) != 0 || len(c.shortableAbsent) != 0 || len(c.shortableReceipts) != 0 {
				t.Fatal("canceled/previous-session probe published authority")
			}
		})
	}
}

func fmtBool(value bool) string {
	if value {
		return "session_loss"
	}
	return "cancellation"
}

func TestBorrowFeeCanceledRefreshRetainsPriorFailure(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	c := newMarketEventCache(func() time.Time { return now })
	prior := now.Add(-time.Hour)
	next := prior.Add(marketEventsBorrowFeeRetryAfter)
	c.borrowFeesLastAttempt = &marketEventBorrowFeeAttempt{Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: prior, CompletedAt: prior, NextAttempt: &next, Failure: &rpc.SourceFailure{Code: rpc.SourceFailureTimeout, Stage: rpc.SourceFailureStageFTPControlConnect, FailedAt: prior}}
	orig := fetchIBKRBorrowFees
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })
	started := make(chan struct{})
	fetchIBKRBorrowFees = func(ctx context.Context, _ string) (marketEventBorrowFeeEntry, error) {
		close(started)
		<-ctx.Done()
		return marketEventBorrowFeeEntry{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := make(chan rpc.SourceHealth, 1)
	go func() { _, h, _ := c.loadBorrowFees(ctx); first <- h }()
	<-started
	queuedCtx, queuedCancel := context.WithCancel(t.Context())
	queued := make(chan rpc.SourceHealth, 1)
	go func() { _, h, _ := c.loadBorrowFees(queuedCtx); queued <- h }()
	queuedCancel()
	select {
	case h := <-queued:
		if h.LastFailure == nil || h.LastFailure.FailedAt != prior || h.NextAttempt == nil || *h.NextAttempt != next {
			t.Fatal("queued cancellation replaced prior provider evidence")
		}
	case <-time.After(time.Second):
		t.Fatal("queued cancellation blocked")
	}
	cancel()
	h := <-first
	if h.LastFailure == nil || h.LastFailure.FailedAt != prior || h.NextAttempt == nil || *h.NextAttempt != next || c.borrowFeesLastAttempt.CompletedAt != prior {
		t.Fatal("canceled fetch replaced prior provider evidence")
	}
}

func TestBorrowFeeFTPCancellationClosesStalledGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ibkrBorrowFeeFTP.retrieve(ctx, listener.Addr().String())
		done <- err
	}()
	conn := <-accepted
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled transfer succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("FTP greeting ignored cancellation")
	}
}

func TestBorrowInventoryRestartAndCacheBounds(t *testing.T) {
	now := time.Now().UTC()
	c := newMarketEventCache(func() time.Time { return now })
	store := openMarketTestCoreStore(t)
	if err := c.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	md := &ibkr.MarketData{ShortableObserved: true, ShortableShares: 0, ShortableTickAt: now, DataType: "live"}
	var symbols []string
	for i := range marketEventsInventoryCacheLimit + 20 {
		symbols = append(symbols, fmt.Sprintf("SYNTH_%d", i))
	}
	probe := func(context.Context, string) (*ibkr.MarketData, error) { return nil, context.DeadlineExceeded }
	_ = c.readBorrowInventory(t.Context(), symbols, nil, ibkr.ConnectorSessionBinding{}, &rpc.MarketEventsResult{}, func() bool { return true }, func(string) *ibkr.MarketData { return md }, probe)
	if len(c.shortableReceipts) > marketEventsInventoryCacheLimit {
		t.Fatal("unbounded receipt cache")
	}
	for _, sym := range symbols {
		c.rememberShortableAbsent(sym, now, ibkr.ConnectorSessionBinding{})
	}
	if len(c.shortableAbsent) > marketEventsInventoryCacheLimit {
		t.Fatal("unbounded negative cache")
	}
	restarted := newMarketEventCache(func() time.Time { return now })
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if len(restarted.shortableReceipts) != 0 || len(restarted.shortableAbsent) != 0 {
		t.Fatal("restart restored current/negative inventory authority")
	}
	res := rpc.MarketEventsResult{}
	health := restarted.readBorrowInventory(t.Context(), []string{symbols[0]}, nil, ibkr.ConnectorSessionBinding{}, &res, func() bool { return true }, func(string) *ibkr.MarketData { return nil }, probe)
	if health.Status != rpc.SourceStatusUnknown || len(res.Flags) != 0 {
		t.Fatal("durable diagnostic receipt became current inventory")
	}
}

func TestBorrowFeeCancellationDoesNotCreateProviderBackoff(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	c := newMarketEventCache(func() time.Time { return now })
	orig := fetchIBKRBorrowFees
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })
	calls := 0
	fetchIBKRBorrowFees = func(ctx context.Context, _ string) (marketEventBorrowFeeEntry, error) {
		calls++
		return marketEventBorrowFeeEntry{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := c.loadBorrowFees(ctx)
	if !errors.Is(err, context.Canceled) || calls != 0 || c.borrowFeesLastAttempt != nil {
		t.Fatalf("canceled caller created provider failure: err=%v calls=%d attempt=%+v", err, calls, c.borrowFeesLastAttempt)
	}
}

// A long-only book reaches the fee-rate fallback while the bulk file is in
// backoff. The fallback finding no exact held short stock is no evidence about
// the provider: its failure stays and no source clock is invented.
func TestBorrowFeeIrrelevantScopePreservesProviderFailure(t *testing.T) {
	now := time.Date(2026, 9, 23, 19, 50, 0, 0, time.UTC) // 15:50 ET, in backoff
	c := offlineMarketEventCache(&now)
	c.borrowFeesLastAttempt = retainedBorrowFeeDNSFailure()
	c.readCachedPositions = syntheticPortfolioStream(syntheticLongOnlyBook(), now.Add(-time.Minute))
	stubBorrowFeeFetch(t, failBorrowFeeFetch)
	bulk, primary, _ := c.loadBorrowFees(t.Context())
	if primary.RefreshState != rpc.SourceRefreshFetchFailedBackoff {
		t.Fatalf("fixture did not reach the backoff fallback: %+v", primary)
	}
	rows, health := c.borrowFeeCoverage(t.Context(), syntheticBorrowSymbols, nil, func() brokerStateScope { return borrowHealthScope }, now, bulk, primary)
	if len(rows) != 0 {
		t.Fatalf("long-only book produced fee-rate targets: %+v", rows)
	}
	if health.Status == rpc.SourceStatusOK || health.LastFailure == nil || !health.AsOf.IsZero() {
		t.Fatalf("empty portfolio scope fabricated provider recovery: %+v", health)
	}
	if row := projectSourceHealth("events:borrow_fee", "borrow fee", "Canary market events", "market_events", health, now); !row.SourceAt.IsZero() || row.Availability != "unavailable" {
		t.Fatalf("irrelevant scope row: source_at=%s availability=%q", row.SourceAt, row.Availability)
	}
}
