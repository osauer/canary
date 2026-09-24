package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// Two caller scopes used to write one source-keyed row: the app excluded a
// held name that expects no market data, the daemon loops included it, and
// that name never delivers tick 236, so inventory alternated partial/ok and
// churned the history. Ad-hoc reads must not overwrite source health either.
func TestEventHealthStableAcrossCallerScopes(t *testing.T) {
	now := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	s.marketEvents = offlineMarketEventCache(&now)
	file := map[string]marketEventBorrowFeeRecord{}
	for _, symbol := range []string{"SYNA", "SYNB", "SYNC", "SYND"} {
		file[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, FeeRate: 0.25, Available: 100000}
	}
	s.marketEvents.borrowFees = marketEventBorrowFeeEntry{FetchedAt: now.Add(-time.Minute), AsOf: now.Add(-time.Minute), SourceURL: "ftp://synthetic.invalid/usa.txt", Symbols: file}
	terminal := rpc.PositionView{Symbol: "SYND", SecType: "STK", Quantity: 5, QuoteExpectation: rpc.QuoteExpectationNone, QuoteExpectationReason: rpc.QuoteExpectationReasonTerminal}
	book := &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{{Symbol: "SYNA", SecType: "STK", Quantity: 10}, terminal},
		Options: []rpc.PositionView{{Symbol: "SYNB", SecType: "OPT", Quantity: 1}, {Symbol: "SYNC", SecType: "OPT", Quantity: 1}},
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "SYNA", Stock: &rpc.PositionView{Symbol: "SYNA", SecType: "STK", Quantity: 10}},
			{Underlying: "SYNB", Options: []rpc.PositionView{{Symbol: "SYNB", SecType: "OPT", Quantity: 1}}},
			{Underlying: "SYNC", Options: []rpc.PositionView{{Symbol: "SYNC", SecType: "OPT", Quantity: 1}}},
			{Underlying: "SYND", Stock: &terminal},
		},
	}
	full := s.canonicalMarketEventSymbols(book) // brief, stress and proposal loops
	app, appUnquoted := rpc.MarketEventScope(book)
	if want := []string{"SYNA", "SYNB", "SYNC", "SYND"}; !slices.Equal(full, want) || !slices.Equal(app, full) || !slices.Equal(appUnquoted, []string{"SYND"}) {
		t.Fatalf("daemon scope %v, app scope %v unquoted %v; want one scope %v keeping the terminal name for Reg SHO and halts", full, app, appUnquoted, want)
	}
	legacyLive := []string{"SYNA", "SYNB", "SYNC"} // the app's former scope
	adHoc := []string{"SYNX"}                      // an explicit --symbol read

	// Source health: only the canonical read records.
	for i := range 12 {
		_ = s.marketEventsForSymbols(t.Context(), [][]string{full, legacyLive, adHoc}[i%3])
	}
	fee := eventHealthRow(t, s, "events:borrow_fee", now)
	if len(fee.History) != 1 || fee.State != "current" {
		t.Fatalf("borrow-fee health flapped across caller scopes: state=%s history=%+v", fee.State, fee.History)
	}

	// Inventory: the terminal name is not expected, so both scopes agree.
	ticks := map[string]*ibkr.MarketData{}
	for _, symbol := range legacyLive {
		ticks[symbol] = &ibkr.MarketData{ShortableObserved: true, ShortableShares: 50000, ShortableTickAt: now.Add(-10 * time.Second), DataType: "live"}
	}
	probe := func(context.Context, string) (*ibkr.MarketData, error) { return nil, context.DeadlineExceeded }
	conditions := map[string]bool{}
	for i := range 6 {
		symbols := [][]string{full, legacyLive}[i%2]
		unquoted, _ := s.marketEvents.scopeFor(symbols, s.currentBrokerStateScope())
		health := s.marketEvents.readBorrowInventory(t.Context(), symbols, unquoted, ibkr.ConnectorSessionBinding{}, &rpc.MarketEventsResult{}, func() bool { return true }, func(sym string) *ibkr.MarketData { return ticks[sym] }, probe)
		if health.Status != rpc.SourceStatusOK {
			t.Fatalf("scope %v inventory %s: %v", symbols, health.Status, health.Notes)
		}
		if slices.Equal(symbols, full) && !strings.Contains(strings.Join(health.Notes, "; "), "1 held symbols expect no market data") {
			t.Fatalf("terminal name reported as missing rather than not expected: %v", health.Notes)
		}
		conditions[dataHealthCondition(projectSourceHealth("events:borrow_inventory", "borrow inventory", "Canary market events", "market_events", health, now))] = true
	}
	if len(conditions) != 1 {
		t.Fatalf("inventory condition depends on caller scope: %v", conditions)
	}
}
