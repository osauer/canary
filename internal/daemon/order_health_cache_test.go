package daemon

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func healthJournalFixture(t *testing.T) (*Server, orderJournalEvent) {
	t.Helper()
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	ev := orderJournalEvent{
		At: s.orderNow(), Type: orderJournalEventStatusUpdated,
		OrderRef: "synthetic-health-order", ReservedOrderID: 41, ClientID: 31,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD",
		Action: "BUY", Quantity: 10, Remaining: 10, Status: "Submitted", TIF: "GTC",
	}
	var events []orderJournalEvent
	for i := range 400 {
		row := ev
		row.At = row.At.Add(time.Duration(i) * time.Millisecond)
		events = append(events, row)
	}
	if err := s.orderJournal.AppendAll(events); err != nil {
		t.Fatal(err)
	}
	return s, ev
}

func TestHealthOrderJournalWarmReadsAvoidHistoryReplay(t *testing.T) {
	s, _ := healthJournalFixture(t)
	read := func() {
		t.Helper()
		summary, err := s.orderJournalSummary()
		if err != nil || summary.OpenOrders != 1 || summary.LastEvent == "" {
			t.Fatalf("summary: %+v err=%v", summary, err)
		}
		scoped, total, err := s.openBrokerOrderCounts()
		if err != nil || scoped != 1 || total != 1 {
			t.Fatalf("working counts: %d/%d err=%v", scoped, total, err)
		}
	}
	read()
	cold := uncachedOrderReadAllocations(t, s)
	warm := testing.AllocsPerRun(3, read)
	if warm >= cold/10 {
		t.Fatalf("health replays unchanged order history: warm %.0f, full replay %.0f allocations", warm, cold)
	}
	t.Logf("health order reads: warm %.0f, full replay %.0f allocations", warm, cold)
	sequence := 0
	unrelated := testing.AllocsPerRun(3, func() {
		sequence++
		_, err := s.coreStore.AppendEvents(t.Context(), []corestore.EventInput{{
			ScopeKey: "settings", EventKey: fmt.Sprintf("synthetic-%d", sequence),
			Type: "preference", Action: "save", Origin: "agent", OccurredAt: s.orderNow(), PayloadJSON: []byte(`{}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
		read()
	})
	if unrelated >= cold/5 {
		t.Fatalf("unrelated event caused order replay: %.0f allocations vs %.0f full replay", unrelated, cold)
	}
	t.Logf("unrelated event plus health reads: %.0f allocations", unrelated)
}

func TestHealthOrderJournalRechecksScopeExpiryAndNewEvents(t *testing.T) {
	s, ev := healthJournalFixture(t)
	check := func(wantScoped, wantTotal int) {
		t.Helper()
		scoped, total, err := s.openBrokerOrderCounts()
		if err != nil || scoped != wantScoped || total != wantTotal {
			t.Fatalf("working counts: %d/%d, want %d/%d, err=%v", scoped, total, wantScoped, wantTotal, err)
		}
		summary, err := s.orderJournalSummary()
		if err != nil || summary.OpenOrders != wantScoped {
			t.Fatalf("summary: %+v err=%v", summary, err)
		}
	}
	check(1, 1)
	s.cfg.Gateway.Account = "DU7654321"
	check(0, 1)
	s.cfg.Gateway.Account = ev.Account
	check(1, 1)
	ev.TIF, ev.At = "DAY", ev.At.Add(time.Second)
	if err := s.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	now := s.orderNow()
	s.now = func() time.Time { return now.Add(36 * time.Hour) }
	check(0, 0)
	// A prior time-dependent projection must not mutate the retained fold.
	s.now = func() time.Time { return now }
	check(1, 1)
	ev.At, ev.Status, ev.Remaining, ev.Filled = ev.At.Add(time.Minute), "Filled", 0, ev.Quantity
	if err := s.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
	summary, err := s.orderJournalSummary()
	if err != nil || summary.LastEvent != fmt.Sprintf("%s %s at %s", ev.Type, orderJournalEventLabel(ev), ev.At.Format(time.RFC3339)) {
		t.Fatalf("new last event was not observed: %+v err=%v", summary, err)
	}
}

func TestHealthOrderJournalConcurrentReadsAndAuthorityReplacement(t *testing.T) {
	s, _ := healthJournalFixture(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			summary, err := s.orderJournalSummary()
			if err != nil || summary.OpenOrders != 1 {
				t.Errorf("concurrent summary: %+v err=%v", summary, err)
			}
		})
	}
	wg.Wait()
	s.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "replacement.jsonl"))
	if summary, err := s.orderJournalSummary(); err != nil || summary.OpenOrders != 0 || summary.LastEvent != "" {
		t.Fatalf("replacement authority reused old history: %+v err=%v", summary, err)
	}
	store, err := s.orderJournal.coreStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.orderJournalSummary(); err == nil {
		t.Fatal("closed storage reused a successful health summary")
	}
	if _, _, err := s.openBrokerOrderCounts(); err == nil {
		t.Fatal("closed storage reported a known working-order count")
	}
}
