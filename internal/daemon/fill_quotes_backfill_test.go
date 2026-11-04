package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func backfillTestExecution(execID, execTime string, clientID int) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventExecDetails, ExecID: execID, ExecTime: execTime, Account: "DU1234567",
		ClientID: clientID, ClientIDPresent: true, ConID: 900001, Symbol: "SYNX", SecType: "STK", Currency: "USD",
		Exchange: "SMART", ExecutionSide: "BOT", Shares: 10, Price: 100,
	}
}

// The first poll of a broker session asks for the last days once and retains
// what it finds without reading a quote; later polls in the same session ask
// for the current day only; a new session asks again and re-finds nothing it
// already holds; an incomplete answer is owed again.
func TestExecutionsBackfillRunsOncePerBrokerSessionAndRetainsWithoutAQuote(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	rt := s.fillQuotes
	quoteReads := 0
	rt.quote = func(context.Context, rpc.ContractParams, time.Duration) (fillQuoteRead, error) {
		quoteReads++
		return fillQuoteRead{Bid: new(99.9), Ask: new(100.1), QuoteAt: fillQuoteTestAt}, nil
	}
	rt.fx = func(context.Context, string) (string, *float64, string, string) { return "USD", new(1.0), "cached", "" }

	status := rpc.TradingStatus{Account: "DU1234567", Mode: rpc.AccountModePaper, ClientID: 15, GatewayPort: 4002}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	today := backfillTestExecution("exec-today", "20260905-11:59:00", 15)
	earlier := backfillTestExecution("exec-earlier", "20260902-15:30:00", 0)

	var filters []ibkrlib.ExecutionsFilter
	backfillComplete := true
	fetch := func(_ context.Context, account string, filter ibkrlib.ExecutionsFilter, _ time.Duration) (ibkrlib.ExecutionsSnapshot, error) {
		if account != status.Account {
			t.Fatalf("account %q", account)
		}
		filters = append(filters, filter)
		if filter.LastNDays == 0 {
			return ibkrlib.ExecutionsSnapshot{Complete: true, Executions: []ibkrlib.OrderLifecycleEvent{today}}, nil
		}
		return ibkrlib.ExecutionsSnapshot{Complete: backfillComplete, Executions: []ibkrlib.OrderLifecycleEvent{today, earlier}}, nil
	}
	poll := func(epoch uint64) {
		t.Helper()
		filters = nil
		s.pollExecutionsFrom(t.Context(), status, epoch, fetch, fillQuotePoll{at: fillQuoteTestAt, scope: scope, clientID: status.ClientID})
		s.captureQueuedFillQuotes(t.Context())
	}

	poll(1)
	if len(filters) != 2 || filters[0].LastNDays != 0 || filters[1].LastNDays != fillQuoteBackfillDays {
		t.Fatalf("first poll of a session: filters %+v, want the current day then the last %d days", filters, fillQuoteBackfillDays)
	}
	if quoteReads != 1 {
		t.Fatalf("quote reads %d, want one for the current-day fill and none for the backfilled one", quoteReads)
	}
	got := rt.records["exec-today"].Record
	if got.Seen != rpc.FillQuoteSeenPoll || got.Quote.Status != rpc.FillQuoteQuoted {
		t.Fatalf("current-day fill: seen %q status %q", got.Seen, got.Quote.Status)
	}
	got = rt.records["exec-earlier"].Record
	if got.Seen != rpc.FillQuoteSeenBackfill || got.Quote.Status != rpc.FillQuoteUnavailable || got.Quote.Reason != fillQuoteReasonBackfilled || got.Quote.Mid != nil {
		t.Fatalf("backfilled fill: seen %q status %q reason %q mid %v", got.Seen, got.Quote.Status, got.Quote.Reason, got.Quote.Mid)
	}
	if got.Source != rpc.FillQuoteSourceBroker || !got.ExecTime.Equal(time.Date(2026, 9, 2, 15, 30, 0, 0, time.UTC)) {
		t.Fatalf("backfilled fill keeps the broker's execution time and source: %+v", got)
	}
	if p := rt.pollFor(status); !p.otherClientsSeen {
		t.Fatal("a backfilled fill from another client proves execution visibility")
	}

	poll(1)
	if len(filters) != 1 || filters[0].LastNDays != 0 {
		t.Fatalf("second poll of the same session: filters %+v, want the current day only", filters)
	}

	poll(2)
	if len(filters) != 2 || filters[1].LastNDays != fillQuoteBackfillDays {
		t.Fatalf("new broker session: filters %+v, want the backfill again", filters)
	}
	if len(rt.records) != 2 || quoteReads != 1 {
		t.Fatalf("re-found fills are not retained twice: %d records, %d quote reads", len(rt.records), quoteReads)
	}

	backfillComplete = false
	poll(3)
	poll(3)
	if len(filters) != 2 || filters[1].LastNDays != fillQuoteBackfillDays {
		t.Fatalf("an incomplete backfill is owed again next poll: filters %+v", filters)
	}
	backfillComplete = true
	poll(3)
	poll(3)
	if len(filters) != 1 {
		t.Fatalf("a completed backfill settles the session: filters %+v", filters)
	}
}

// A current-day request that fails leaves the backfill for a poll whose
// request succeeded, so a broker outage is not misread as an empty history.
func TestExecutionsBackfillWaitsForACurrentDayAnswer(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	status := rpc.TradingStatus{Account: "DU1234567", Mode: rpc.AccountModePaper, ClientID: 15, GatewayPort: 4002}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	var filters []ibkrlib.ExecutionsFilter
	fetch := func(context.Context, string, ibkrlib.ExecutionsFilter, time.Duration) (ibkrlib.ExecutionsSnapshot, error) {
		return ibkrlib.ExecutionsSnapshot{}, context.DeadlineExceeded
	}
	wrapped := func(ctx context.Context, account string, filter ibkrlib.ExecutionsFilter, timeout time.Duration) (ibkrlib.ExecutionsSnapshot, error) {
		filters = append(filters, filter)
		return fetch(ctx, account, filter, timeout)
	}
	s.pollExecutionsFrom(t.Context(), status, 1, wrapped, fillQuotePoll{at: fillQuoteTestAt, scope: scope, clientID: status.ClientID})
	if len(filters) != 1 || s.fillQuotes.pollFor(status).err != "timeout" {
		t.Fatalf("a failed current-day request sends no backfill: filters %+v, poll %+v", filters, s.fillQuotes.pollFor(status))
	}
}
