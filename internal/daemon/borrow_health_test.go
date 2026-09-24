package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

var borrowHealthScope = brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}

// offlineMarketEventCache serves current Reg SHO and halt lists from memory so
// a snapshot never reaches the network.
func offlineMarketEventCache(now *time.Time) *marketEventCache {
	c := newMarketEventCache(func() time.Time { return *now })
	c.regSHOFreshFor, c.haltsFreshFor = 24*time.Hour, 24*time.Hour
	c.regSHO = marketEventRegSHOEntry{FetchedAt: *now, AsOf: *now, SourceURL: "synthetic", Symbols: map[string]marketEventRegSHORecord{}}
	c.halts = marketEventHaltsEntry{FetchedAt: *now, AsOf: *now, SourceURL: "synthetic"}
	c.fetchHistoricalFeeRates = func(context.Context, ibkr.Contract, int, time.Duration) ([]ibkr.HistoricalBar, error) {
		return nil, context.DeadlineExceeded
	}
	return c
}

// retainedBorrowFeeDNSFailure is the durable failure a restart re-projects:
// the last attempt failed at DNS and no file was ever delivered.
func retainedBorrowFeeDNSFailure() *marketEventBorrowFeeAttempt {
	failedAt := time.Date(2026, 9, 23, 19, 48, 36, 0, time.UTC)
	next := failedAt.Add(marketEventsBorrowFeeRetryAfter)
	return &marketEventBorrowFeeAttempt{
		Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: failedAt, CompletedAt: failedAt, NextAttempt: &next,
		Failure: &rpc.SourceFailure{Code: rpc.SourceFailureDNSFailed, Stage: rpc.SourceFailureStageFTPControlConnect, FailedAt: failedAt, Retryable: true},
	}
}

func stubBorrowFeeFetch(t *testing.T, fetch func(context.Context, string) (marketEventBorrowFeeEntry, error)) {
	t.Helper()
	orig := fetchIBKRBorrowFees
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })
	fetchIBKRBorrowFees = fetch
}

func failBorrowFeeFetch(context.Context, string) (marketEventBorrowFeeEntry, error) {
	return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureDNSFailed, rpc.SourceFailureStageFTPControlConnect, true)
}

// syntheticPortfolioStream is a portfolio stream whose last receipt completed
// at receivedAt; a zero receivedAt is a stream still downloading, as right
// after a reconnect or resubscription.
func syntheticPortfolioStream(raw []*ibkr.RawPosition, receivedAt time.Time) func() ([]*ibkr.RawPosition, ibkr.PortfolioStreamHealth, error) {
	return func() ([]*ibkr.RawPosition, ibkr.PortfolioStreamHealth, error) {
		return raw, ibkr.PortfolioStreamHealth{Account: borrowHealthScope.Account, InitialCompletedAt: receivedAt}, nil
	}
}

func syntheticLongOnlyBook() []*ibkr.RawPosition {
	return []*ibkr.RawPosition{
		{Account: "DU123", Contract: ibkr.Contract{ConID: 910001, Symbol: "SYNL", SecType: "STK", Currency: "USD", Exchange: "SMART"}, Position: 100},
		{Account: "DU123", Contract: ibkr.Contract{ConID: 910002, Symbol: "SYNC", SecType: "OPT", Right: "C", Strike: 50, Expiry: "20261218", Multiplier: 100, Currency: "USD"}, Position: 2},
	}
}

func syntheticBookWithShortStock() []*ibkr.RawPosition {
	return append(syntheticLongOnlyBook(), &ibkr.RawPosition{Account: "DU123", Contract: ibkr.Contract{ConID: 910003, Symbol: "SYNS", SecType: "STK", Currency: "USD", Exchange: "SMART"}, Position: -50})
}

var syntheticBorrowSymbols = []string{"SYNC", "SYNL", "SYNS"}

func eventHealthRow(t *testing.T, s *Server, id string, now time.Time) rpc.DataSourceHealth {
	t.Helper()
	row, ok := s.observedDataHealth(id, now)
	if !ok {
		t.Fatalf("%s was not recorded", id)
	}
	return row
}

// A long-only book needs no borrow evidence at any hour, and saying so must not
// hide the provider's own outage or its real receipts: the portfolio verdict
// and the provider facts are independent.
func TestBorrowHealthApplicabilityIndependentOfCadence(t *testing.T) {
	notDue := time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC) // 07:00 ET
	due := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)    // 11:00 ET
	delivered := func(context.Context, string) (marketEventBorrowFeeEntry, error) {
		return marketEventBorrowFeeEntry{AsOf: due.Add(-5 * time.Minute), SourceURL: "ftp://synthetic.invalid/usa.txt", Symbols: map[string]marketEventBorrowFeeRecord{
			"SYNC": {Symbol: "SYNC", FeeRate: 0.3, Available: 500000},
			"SYNL": {Symbol: "SYNL", FeeRate: 0.25, Available: 900000},
			"SYNS": {Symbol: "SYNS", FeeRate: 0.4, Available: 200000},
		}}, nil
	}
	for _, tc := range []struct {
		name     string
		now      time.Time
		book     []*ibkr.RawPosition
		streamAt time.Duration
		fetch    func(context.Context, string) (marketEventBorrowFeeEntry, error)
		required bool
		receipt  bool
	}{
		{"not_due_long_only", notDue, syntheticLongOnlyBook(), -time.Minute, failBorrowFeeFetch, false, false},
		{"due_failed_fetch_long_only", due, syntheticLongOnlyBook(), -time.Minute, failBorrowFeeFetch, false, false},
		{"due_delivered_long_only", due, syntheticLongOnlyBook(), -time.Minute, delivered, false, true},
		{"not_due_exact_short_stock", notDue, syntheticBookWithShortStock(), -time.Minute, failBorrowFeeFetch, true, false},
		{"due_failed_fetch_exact_short_stock", due, syntheticBookWithShortStock(), -time.Minute, failBorrowFeeFetch, true, false},
		// No verdict from a current stream exists yet, as after a restart.
		{"not_due_stale_portfolio", notDue, syntheticLongOnlyBook(), -10 * time.Minute, failBorrowFeeFetch, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			c := offlineMarketEventCache(&now)
			c.borrowFeesLastAttempt = retainedBorrowFeeDNSFailure()
			c.readCachedPositions = syntheticPortfolioStream(tc.book, now.Add(tc.streamAt))
			stubBorrowFeeFetch(t, tc.fetch)
			result := c.snapshot(t.Context(), syntheticBorrowSymbols, nil, nil, func() brokerStateScope { return borrowHealthScope })
			s := &Server{now: func() time.Time { return now }}
			s.observeEventHealth(result, nil, ibkr.ConnectorSessionBinding{})
			fee := eventHealthRow(t, s, "events:borrow_fee", now)
			inventory := eventHealthRow(t, s, "events:borrow_inventory", now)

			if tc.receipt {
				asOf := due.Add(-5 * time.Minute)
				if fee.Failure != nil || fee.Availability != "available" || !fee.ReceivedAt.Equal(asOf) || !fee.LastSuccess.Equal(asOf) {
					t.Fatalf("real receipt hidden: availability=%q received=%s last_success=%s failure=%+v", fee.Availability, fee.ReceivedAt, fee.LastSuccess, fee.Failure)
				}
			} else {
				if fee.Failure == nil || fee.Failure.Code != rpc.SourceFailureDNSFailed || fee.Availability != "unavailable" {
					t.Fatalf("provider outage hidden: availability=%q failure=%+v", fee.Availability, fee.Failure)
				}
				if !fee.SourceAt.IsZero() || !fee.LastSuccess.IsZero() {
					t.Fatalf("undelivered source carries a clock: source_at=%s last_success=%s", fee.SourceAt, fee.LastSuccess)
				}
			}
			if fee.Required != tc.required || inventory.Required != tc.required {
				t.Fatalf("required fee=%v inventory=%v, want %v (fee state=%s applicability=%q)", fee.Required, inventory.Required, tc.required, fee.State, fee.Applicability)
			}
			report, err := finalizeDataHealth([]rpc.DataSourceHealth{fee, inventory}, "current", now, rpc.DataHealthParams{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.required {
				if report.Summary.Unverified+report.Summary.Problems == 0 {
					t.Fatalf("required borrow evidence was waived: %+v", report.Summary)
				}
				return
			}
			if fee.State != "not_relevant" || fee.Applicability != "not_relevant" || inventory.Applicability != "not_relevant" || len(report.Concerns) != 0 || report.Summary.Unverified != 0 {
				t.Fatalf("long-only book still carries a borrow concern: state=%s concerns=%+v summary=%+v", fee.State, report.Concerns, report.Summary)
			}
		})
	}
}

// Reconnects, resubscriptions and quiet periods leave the portfolio stream
// briefly not current. Borrow applicability must not flap to required and back
// across them, nor outlive a real outage, a scope change or a restart.
func TestBorrowApplicabilityBridgesPortfolioStreamGaps(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC) // 11:00 ET
	stubBorrowFeeFetch(t, failBorrowFeeFetch)
	now := t0
	scope := borrowHealthScope
	book := syntheticLongOnlyBook()
	var streamAt time.Time
	c := offlineMarketEventCache(&now)
	c.borrowFeesLastAttempt = retainedBorrowFeeDNSFailure()
	c.readCachedPositions = func() ([]*ibkr.RawPosition, ibkr.PortfolioStreamHealth, error) {
		return syntheticPortfolioStream(book, streamAt)()
	}
	s := &Server{now: func() time.Time { return now }}
	step := func(t *testing.T, cache *marketEventCache, at, received time.Time, symbols []string, wantRequired bool) {
		t.Helper()
		now, streamAt = at, received
		result := cache.snapshot(t.Context(), symbols, nil, nil, func() brokerStateScope { return scope })
		s.observeEventHealth(result, nil, ibkr.ConnectorSessionBinding{})
		for _, id := range []string{"events:borrow_fee", "events:borrow_inventory"} {
			if row := eventHealthRow(t, s, id, now); row.Required != wantRequired {
				t.Fatalf("%s at +%s: required=%v applicability=%q, want required=%v", id, at.Sub(t0), row.Required, row.Applicability, wantRequired)
			}
		}
	}
	transitions := func() int { return len(eventHealthRow(t, s, "events:borrow_fee", now).History) }

	step(t, c, t0, t0.Add(-time.Minute), syntheticBorrowSymbols, false)
	step(t, c, t0.Add(2*time.Minute), time.Time{}, syntheticBorrowSymbols, false)             // resubscribed, downloading
	step(t, c, t0.Add(8*time.Minute), t0.Add(-time.Minute), syntheticBorrowSymbols, false)    // quiet: receipt stale
	step(t, c, t0.Add(10*time.Minute), t0.Add(10*time.Minute), syntheticBorrowSymbols, false) // current again
	step(t, c, t0.Add(24*time.Minute), t0.Add(10*time.Minute), syntheticBorrowSymbols, false) // stale, within the bound
	if n := transitions(); n != 1 {
		t.Fatalf("stream gaps flapped borrow applicability: %d transitions", n)
	}
	step(t, c, t0.Add(26*time.Minute), t0.Add(10*time.Minute), syntheticBorrowSymbols, true) // outage beyond the bound
	step(t, c, t0.Add(27*time.Minute), t0.Add(27*time.Minute), syntheticBorrowSymbols, false)

	t.Run("new_name_during_gap", func(t *testing.T) {
		step(t, c, t0.Add(28*time.Minute), time.Time{}, []string{"SYNC", "SYNL", "SYNN", "SYNS"}, true)
	})
	t.Run("other_account", func(t *testing.T) {
		step(t, c, t0.Add(29*time.Minute), t0.Add(29*time.Minute), syntheticBorrowSymbols, false)
		scope = brokerStateScope{Account: "DU999", Mode: rpc.AccountModePaper}
		t.Cleanup(func() { scope = borrowHealthScope })
		step(t, c, t0.Add(30*time.Minute), time.Time{}, syntheticBorrowSymbols, true)
	})
	t.Run("restart", func(t *testing.T) {
		restarted := offlineMarketEventCache(&now)
		restarted.borrowFeesLastAttempt = retainedBorrowFeeDNSFailure()
		restarted.readCachedPositions = c.readCachedPositions
		step(t, restarted, t0.Add(31*time.Minute), time.Time{}, syntheticBorrowSymbols, true)
	})
	t.Run("short_stock_opened", func(t *testing.T) {
		book = syntheticBookWithShortStock()
		step(t, c, t0.Add(32*time.Minute), t0.Add(32*time.Minute), syntheticBorrowSymbols, true)
		step(t, c, t0.Add(33*time.Minute), time.Time{}, syntheticBorrowSymbols, true)
	})
}

// The pre-versioned recorder advanced last_success on not-due and
// not-applicable verdicts, and max(previous, new) kept that value forever. Only
// events:borrow_fee could be fabricated that way, so only it is corroborated.
func TestLegacyDiagnosticsLastSuccessNotRestoredWithoutReceipt(t *testing.T) {
	fabricated := time.Date(2026, 9, 23, 19, 59, 55, 0, time.UTC)
	receipt := time.Date(2026, 9, 23, 20, 5, 0, 0, time.UTC)
	fileAsOf := time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		lastGood   bool
		wantBorrow time.Time
	}{
		{"authority_never_succeeded", false, time.Time{}},
		{"authority_proves_earlier_receipt", true, fileAsOf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _, _ := historyFixture(t)
			now := time.Date(2026, 9, 24, 11, 14, 49, 0, time.UTC)
			s.now = func() time.Time { return now }
			// The borrow-fee authority as the previous daemon left it.
			previous := newMarketEventCache(func() time.Time { return fileAsOf })
			if err := previous.UseCoreStore(s.coreStore); err != nil {
				t.Fatal(err)
			}
			if tc.lastGood {
				entry := marketEventBorrowFeeEntry{FetchedAt: fileAsOf, AsOf: fileAsOf, SourceURL: "ftp://synthetic.invalid/usa.txt", Symbols: map[string]marketEventBorrowFeeRecord{"SYNA": {Symbol: "SYNA", FeeRate: 0.25, Available: 1000}}}
				if err := previous.persistBorrowFeeSuccess(t.Context(), entry, fileAsOf, fileAsOf); err != nil {
					t.Fatal(err)
				}
			}
			if err := previous.persistBorrowFeeFailure(t.Context(), previous.borrowFees, *retainedBorrowFeeDNSFailure()); err != nil {
				t.Fatal(err)
			}
			// An unversioned document: the fabricated borrow-fee clock beside a
			// genuine one, and a versioned borrow-fee record.
			legacy := `[
				{"id":"events:borrow_fee","first_observed":"2026-09-23T18:59:55Z","last_success":"2026-09-23T19:59:55Z","transitions":[{"at":"2026-09-23T19:59:55Z","state":"not_due"}]},
				{"id":"macro:synthetic","first_observed":"2026-09-23T19:05:00Z","last_success":"2026-09-23T20:05:00Z","transitions":[{"at":"2026-09-23T20:05:00Z","state":"current"}]},
				{"version":2,"id":"events:synthetic_versioned","first_observed":"2026-09-23T19:05:00Z","last_success":"2026-09-23T20:05:00Z","transitions":[{"at":"2026-09-23T20:05:00Z","state":"current"}]}
			]`
			if err := saveMarketDocument(t.Context(), s.coreStore, "data-health-diagnostics", dataHealthHistoryKind, []byte(legacy)); err != nil {
				t.Fatal(err)
			}

			// Restart: the authorities attach before diagnostics load.
			s.installMarketEventCache()
			if err := s.marketEvents.UseCoreStore(s.coreStore); err != nil {
				t.Fatal(err)
			}
			s.loadDataHealthHistory()
			// Outside the session the producer reports only its retained state.
			_, health, _ := s.marketEvents.loadBorrowFees(t.Context())
			s.observeEventHealth(rpc.MarketEventsResult{AsOf: now, SourceHealth: []rpc.SourceHealth{health}}, nil, ibkr.ConnectorSessionBinding{})

			if fee := eventHealthRow(t, s, "events:borrow_fee", now); !fee.LastSuccess.Equal(tc.wantBorrow) {
				t.Fatalf("borrow-fee last_success %s, want %s: a legacy clock outlived its producer receipts", fee.LastSuccess, tc.wantBorrow)
			}
			for _, id := range []string{"macro:synthetic", "events:synthetic_versioned"} {
				if row := eventHealthRow(t, s, id, now); !row.LastSuccess.Equal(receipt) {
					t.Fatalf("%s lost its genuine receipt clock: %s", id, row.LastSuccess)
				}
			}
			s.persistDataHealthHistory(t.Context())
			raw, _, err := loadMarketState(s.coreStore, "data-health-diagnostics", dataHealthHistoryKind)
			if err != nil {
				t.Fatal(err)
			}
			var saved []map[string]any
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			for _, record := range saved {
				if record["version"] != float64(2) {
					t.Fatalf("persisted diagnostic is unversioned: %v", record)
				}
				if record["id"] == "events:borrow_fee" && record["last_success"] == fabricated.Format(time.RFC3339) {
					t.Fatalf("fabricated clock persisted again: %v", record)
				}
			}
		})
	}
}

// A source that never delivered has no source clock, whichever no-data path
// answers: not due, failed while due, or a canceled refresh.
func TestBorrowFeeNotDueWithoutDataHasNoSourceClock(t *testing.T) {
	stubBorrowFeeFetch(t, func(context.Context, string) (marketEventBorrowFeeEntry, error) {
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect, true)
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		now  time.Time
		ctx  context.Context
	}{
		{"not_due", time.Date(2026, 9, 24, 11, 14, 49, 0, time.UTC), t.Context()},
		{"due_failed_fetch", time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC), t.Context()},
		{"due_backoff", time.Date(2026, 9, 23, 19, 50, 0, 0, time.UTC), t.Context()},
		{"canceled_refresh", time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC), canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMarketEventCache(func() time.Time { return tc.now })
			c.borrowFeesLastAttempt = retainedBorrowFeeDNSFailure()
			_, health, _ := c.loadBorrowFees(tc.ctx)
			if !health.AsOf.IsZero() || health.AgeSeconds != 0 {
				t.Fatalf("no-data borrow fee claims a source clock: as_of=%s age=%d", health.AsOf, health.AgeSeconds)
			}
			row := projectSourceHealth("events:borrow_fee", "borrow fee", "Canary market events", "market_events", health, tc.now)
			if !row.SourceAt.IsZero() || !row.ReceivedAt.IsZero() || row.Failure == nil {
				t.Fatalf("no-data row: source_at=%s received_at=%s failure=%+v", row.SourceAt, row.ReceivedAt, row.Failure)
			}
		})
	}
}
