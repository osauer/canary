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

func TestFillQuoteCapturedDuringUnavailableStoreRetriesOriginalPayload(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fail := false
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db"), CommitObserver: func(corestore.AuthorityHead) error {
		if fail {
			return errors.New("synthetic watermark outage")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rt, err := bindFillQuoteRuntime(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{fillQuotes: rt, now: func() time.Time { return fillQuoteTestAt }, logger: NewLogger(nil, "error")}
	quotes, fxReads := 0, 0
	rt.quote = func(context.Context, rpc.ContractParams, time.Duration) (fillQuoteRead, error) {
		quotes++
		return fillQuoteRead{Bid: new(1.2), Ask: new(1.3), DataType: rpc.MarketDataLive, QuoteAt: fillQuoteTestAt}, nil
	}
	rt.fx = func(context.Context, string) (string, *float64, string, string) {
		fxReads++
		return "USD", new(0.88), "original synthetic fx", ""
	}
	fail = true
	_, err = store.AppendObservation(t.Context(), corestore.ObservationInput{ScopeKey: "synthetic-other", Kind: "outage", Source: "test", ObservedAt: fillQuoteTestAt, ContentType: "application/json", Payload: []byte(`{}`)})
	if err == nil || store.Health().Ready {
		t.Fatal("outage fixture did not suspend writes")
	}
	fill := fillQuoteFillFromJournal(fillQuoteTestJournalFill("synthetic-before-commit-retry"), rpc.FillQuoteSourceCanary)
	if !rt.enqueue(fill) {
		t.Fatal("enqueue")
	}
	srv.captureQueuedFillQuotes(t.Context())
	if len(rt.pending) != 1 || len(rt.list(fill.account, "paper")) != 0 {
		t.Fatal("must retain unconfirmed payload privately")
	}
	fail = false
	ok, err := store.RecoverTransientHeadWatermark(t.Context())
	if err != nil || !ok {
		t.Fatalf("recovery %v %v", ok, err)
	}
	srv.captureQueuedFillQuotes(t.Context())
	srv.captureQueuedFillQuotes(t.Context())
	records := rt.list(fill.account, "paper")
	rows, err := store.ListObservations(t.Context(), corestore.ObservationQuery{ScopeKey: fillQuoteScopeKey, Kind: fillQuoteKind, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("records=%d durable=%d quote reads=%d FX reads=%d pending=%d", len(records), len(rows), quotes, fxReads, len(rt.pending))
	if len(records) != 1 || len(rows) != 1 || quotes != 1 || fxReads != 1 || len(rt.pending) != 0 {
		t.Fatal("recovery must commit the original payload once")
	}
	if records[0].Quote.Bid == nil || *records[0].Quote.Bid != 1.2 || records[0].FXRate == nil || *records[0].FXRate != 0.88 || records[0].FXSource != "original synthetic fx" {
		t.Fatal("original quote or FX payload changed")
	}
}
