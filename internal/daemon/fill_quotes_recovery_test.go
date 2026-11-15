package daemon

import (
	"context"
	"errors"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFillQuoteRecoversAfterTransientWatermarkFailure(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	fail := false
	core, e := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db"), CommitObserver: func(corestore.AuthorityHead) error {
		if fail {
			return errors.New("synthetic transient head-file failure")
		}
		return nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer core.Close()
	rt, e := bindFillQuoteRuntime(t.Context(), core)
	if e != nil {
		t.Fatal(e)
	}
	srv := &Server{fillQuotes: rt, now: func() time.Time { return fillQuoteTestAt }, logger: NewLogger(nil, "error")}
	reads := 0
	rt.quote = func(context.Context, rpc.ContractParams, time.Duration) (fillQuoteRead, error) {
		reads++
		return fillQuoteRead{Bid: new(1.20), Ask: new(1.30), DataType: rpc.MarketDataLive, QuoteAt: fillQuoteTestAt}, nil
	}
	rt.fx = func(context.Context, string) (string, *float64, string, string) {
		return "USD", new(1.0), "same_currency", ""
	}
	fill := fillQuoteFillFromJournal(fillQuoteTestJournalFill("synthetic-recovery-1"), rpc.FillQuoteSourceCanary)
	if !rt.enqueue(fill) {
		t.Fatal("initial enqueue failed")
	}
	fail = true
	srv.captureQueuedFillQuotes(t.Context())
	fail = false
	recovered, e := core.RecoverTransientHeadWatermark(t.Context())
	if e != nil || !recovered {
		t.Fatalf("recover failed %v %v", recovered, e)
	}
	srv.captureQueuedFillQuotes(t.Context())
	srv.captureQueuedFillQuotes(t.Context())
	rows, e := core.ListObservations(t.Context(), corestore.ObservationQuery{ScopeKey: fillQuoteScopeKey, Kind: fillQuoteKind, Limit: 10})
	if e != nil {
		t.Fatal(e)
	}
	requeued := rt.enqueue(fill)
	visible := rt.list(fill.account, "paper")
	t.Logf("store ready=%v durable fill rows=%d report fills=%d next poll requeued=%v claimed=%v", core.Health().Ready, len(rows), len(visible), requeued, rt.claimed[fill.record.ExecID])
	if len(rows) != 1 || len(visible) != 1 || requeued || reads != 1 || len(rt.pending) != 0 {
		t.Fatalf("recovery must publish the original capture exactly once: reads=%d pending=%d", reads, len(rt.pending))
	}
	if visible[0].Quote.Bid == nil || *visible[0].Quote.Bid != 1.20 {
		t.Fatal("original quote lost")
	}
}
