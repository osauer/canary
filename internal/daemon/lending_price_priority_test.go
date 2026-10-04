package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestLendingDisplayedPriorityBeyondFullBackgroundQueue(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	s := &Server{}
	bulk := marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{}}
	background := []string{}
	for i := range 150 {
		symbol := fmt.Sprintf("A%03d", i)
		background = append(background, symbol)
		bulk.Symbols[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, ConID: fmt.Sprint(91000 + i), Currency: "USD"}
	}
	health := rpc.SourceHealth{Status: rpc.SourceStatusOK, RefreshState: rpc.SourceRefreshNotDue}
	s.lendingMarketRowsForFamily(bulk, health, background, now, "short_interest")
	focus := []string{"ZZZ", "YYY", "XXX", "WWW"} // deliberately reverse alphabetical
	rows := s.lendingMarketRowsForFamily(bulk, health, focus, now, "short_interest", focus)
	if len(s.lendingMarket.entries) != 154 || rows[0].Status != "pending" || rows[0].Contract.ConID != 0 {
		t.Fatal("full background queue blocked displayed identity resolution")
	}
	for i, want := range []string{"ZZZ", "YYY", "XXX", "A000", "WWW"} {
		symbol, e := selectLendingMarket(&s.lendingMarket, now, "2026-10-02")
		if symbol != want {
			t.Fatalf("job%d got%s want%s", i, symbol, want)
		}
		e.retry = now.Add(time.Hour)
		s.lendingMarket.entries[symbol] = e
	}
	// Replacing the displayed set leaves at most100 foreground interests,
	// including repeated tab/sort changes; it never resolves the whole universe.
	many := []string{}
	for i := range 130 {
		many = append(many, fmt.Sprintf("Z%03d", i))
	}
	rows = s.lendingMarketRowsForFamily(bulk, health, many, now, "short_interest", many)
	active := 0
	for _, e := range s.lendingMarket.entries {
		if now.Before(e.focusUntil) {
			active++
		}
	}
	if _, exists := s.lendingMarket.entries["ZZZ"]; exists {
		t.Fatal("replaced unresolved focus retained reserved admission")
	}
	if lendingMarketDue(lendingMarketEntry{contract: rpc.ContractParams{Symbol: "OLD"}, focusUntil: now.Add(-time.Minute)}, now, "2026-10-02") {
		t.Fatal("expired displayed interest triggered background identity resolution")
	}
	if active != 100 || rows[129].Status != "unavailable" {
		t.Fatalf("unbounded identity focus active=%d", active)
	}
}

type lendingFakeIdentity struct {
	current   bool
	ready     bool
	result    ibkrlib.ResolvedOrderContract
	err       error
	requested ibkrlib.Contract
}

func (f *lendingFakeIdentity) CaptureSession() (ibkrlib.ConnectorSessionBinding, bool) {
	return ibkrlib.ConnectorSessionBinding{}, f.ready
}
func (f *lendingFakeIdentity) SessionCurrent(ibkrlib.ConnectorSessionBinding) bool { return f.current }
func (f *lendingFakeIdentity) ResolveOrderContractForSession(_ context.Context, _ ibkrlib.ConnectorSessionBinding, c ibkrlib.Contract, _ time.Duration) (ibkrlib.ResolvedOrderContract, error) {
	f.requested = c
	return f.result, f.err
}

func TestLendingExactDisplayedIdentityResolution(t *testing.T) {
	source := rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	good := ibkrlib.ResolvedOrderContract{Contract: ibkrlib.Contract{Symbol: "SYNTH", SecType: "STK", Currency: "USD", ConID: 91001, Exchange: "SMART", PrimaryExch: "NASDAQ", LocalSymbol: "SYNTH"}}
	f := &lendingFakeIdentity{ready: true, current: true, result: good}
	got, err := resolveLendingMarketIdentity(t.Context(), source, f)
	if err != nil || got.ConID != 91001 || f.requested.Symbol != "SYNTH" || f.requested.SecType != "STK" {
		t.Fatalf("exact fallback: %+v %v", got, err)
	}
	cases := []func(){func() { f.current = false }, func() { f.err = errors.New("contract details are ambiguous") }, func() { f.result.Contract.SecType = "WAR" }, func() { f.result.Contract.Symbol = "SYNTHCOMMON" }, func() { f.result.Contract.Currency = "EUR" }, func() { f.result.Contract.ConID = 0 }}
	for _, mutate := range cases {
		*f = lendingFakeIdentity{ready: true, current: true, result: good}
		mutate()
		if _, err := resolveLendingMarketIdentity(t.Context(), source, f); err == nil {
			t.Fatal("ambiguous, changed-session or wrong-type identity accepted")
		}
	}
}

func TestLendingDatedWeekendPriceSurvivesYearHistoryFailure(t *testing.T) {
	c, _, now := lendingMarketFixture(t)
	closeAt := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	empty := projectLendingMarket(c, nil, nil, now)
	q := &rpc.Quote{Contract: c, RegularClose: new(20.0), RegularCloseAt: closeAt, RegularChangePct: new(2.0), PrevClose: new(19.0), Last: new(21.0)}
	price := overlayLendingQuote(empty, c, q, now)
	if price.Price == nil || *price.Price != 20 || !price.PriceAt.Equal(closeAt) || price.PriceKind != "close" || price.YTDChangePct != nil {
		t.Fatalf("weekend dated close lost or wrong source: %+v", price)
	}
	failed := projectLendingMarket(c, nil, nil, now.Add(time.Minute))
	retained := retainLendingCompletedPrice(price, failed, "2026-10-02")
	if retained.Price == nil || *retained.Price != 20 {
		t.Fatal("year-history failure erased phase-one close")
	}
	if retainLendingCompletedPrice(price, failed, "2026-10-05").Price != nil {
		t.Fatal("old session close renewed after next session")
	}
	q.RegularCloseAt = time.Time{}
	if overlayLendingQuote(empty, c, q, now).Price != nil {
		t.Fatal("undated last/prev-close invented a price date")
	}
	q.RegularCloseAt = closeAt.AddDate(0, 0, -1)
	if overlayLendingQuote(empty, c, q, now).Price != nil {
		t.Fatal("older regular close promoted to latest session")
	}
}

func TestLendingThinEnrichmentPreservesMatchingCompletedFields(t *testing.T) {
	c, h, now := lendingMarketFixture(t)
	old := projectLendingMarket(c, h, nil, now)
	thin := old
	thin.Volume = nil
	thin.VolumeDate = ""
	thin.DayChangePct = nil
	thin.AvgDollarVolume20D = nil
	thin.LiquidityAsOf = ""
	thin.YTDChangePct = nil
	thin.YTDAsOf = ""
	thin.YTDBaseDate = ""
	thin.Status = "partial"
	got := retainLendingCompletedPrice(old, thin, "2026-10-02")
	if got.Volume == nil || got.AvgDollarVolume20D == nil || got.DayChangePct == nil || got.YTDChangePct == nil {
		t.Fatal("thin yearly history erased independently dated price-phase fields")
	}
	thin.Price = new(25.0)
	got = retainLendingCompletedPrice(old, thin, "2026-10-02")
	if got.DayChangePct != nil || got.AvgDollarVolume20D != nil || got.YTDChangePct != nil {
		t.Fatal("corrected close inherited mismatched return/turnover metrics")
	}
	e := lendingMarketEntry{daily: thin, fullHistoryAt: now}
	if lendingNeedsHistory(e, now.Add(5*time.Minute), "2026-10-02") {
		t.Fatal("quote refresh forces yearly reacquisition")
	}
	if !lendingNeedsHistory(e, now.Add(time.Hour), "2026-10-02") {
		t.Fatal("incomplete history never retries")
	}
}

func TestShortInterestFinalMarketRankingOwnsDisplayedPriority(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	s.marketEvents = offlineMarketEventCache(&now)
	s.marketEvents.borrowFees = marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{}}
	s.shortInterest.publication = shortInterestPublication{URL: "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv", SettlementDate: "2026-09-15", FetchedAt: now}
	s.lendingMarket.entries = map[string]lendingMarketEntry{}
	for i, symbol := range []string{"AAA", "BBB"} {
		c := rpc.ContractParams{Symbol: symbol, SecType: "STK", Currency: "USD", ConID: 91001 + i, Exchange: "SMART"}
		s.marketEvents.borrowFees.Symbols[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, ConID: fmt.Sprint(c.ConID), Currency: "USD", FeeRate: 60}
		s.shortInterest.publication.Rows = append(s.shortInterest.publication.Rows, rpc.ShortInterestRow{Symbol: symbol, Name: "Synthetic " + symbol, Market: "NYSE", SettlementDate: "2026-09-15", ShortInterestShares: 100, AverageDailyVolume: 50})
		price := float64(10 + i*90)
		daily := rpc.LendingMarketRow{Symbol: symbol, Contract: c, Status: "partial", CheckedAt: now.Add(-time.Hour), ValidUntil: now.Add(-time.Minute), Price: &price, PriceKind: "close", PriceAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
		s.lendingMarket.entries[symbol] = lendingMarketEntry{contract: c, family: "short_interest", daily: daily, row: daily, until: now.Add(time.Hour), attempted: now.Add(-time.Hour), pricePhaseDone: true}
	}
	// Raw source rows contain no prices, so the preliminary rank is AAA first.
	// After cached context joins, BBB is the only visible match and must get focus.
	p, _ := rpc.NormalizeShortInterestScreenParams(rpc.ShortInterestScreenParams{Limit: 1, MinPrice: 50, SortBy: "price", SortDir: "desc"})
	raw, _ := json.Marshal(p)
	out, err := s.handleShortInterestScreen(t.Context(), &rpc.Request{Params: raw})
	if err != nil || len(out.Rows) != 1 || out.Rows[0].Symbol != "BBB" {
		t.Fatalf("final price rank/filter changed: %+v %v", out, err)
	}
	if err := rpc.ValidateShortInterestScreenResult(*out, p); err != nil {
		t.Fatal(err)
	}
	symbol, _ := selectLendingMarket(&s.lendingMarket, now, "2026-10-02")
	if symbol != "BBB" || s.lendingMarket.entries["BBB"].focusRank != 0 {
		t.Fatal("preliminary source prefix displaced final visible row")
	}
}

func TestShortInterestFilteredKnownMatchRetainsProvisionalIdentity(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	s.marketEvents = offlineMarketEventCache(&now)
	s.marketEvents.borrowFees = marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{"BBB": {Symbol: "BBB", ConID: "91002", Currency: "USD", FeeRate: 60}}}
	s.shortInterest.publication = shortInterestPublication{URL: "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv", SettlementDate: "2026-09-15", FetchedAt: now, Rows: []rpc.ShortInterestRow{
		{Symbol: "AAA", Name: "Synthetic Alpha", Market: "NYSE", ShortInterestShares: 200, AverageDailyVolume: 50, SettlementDate: "2026-09-15"},
		{Symbol: "BBB", Name: "Synthetic Beta", Market: "NYSE", ShortInterestShares: 100, AverageDailyVolume: 50, SettlementDate: "2026-09-15"},
	}}
	c := rpc.ContractParams{Symbol: "BBB", ConID: 91002, SecType: "STK", Currency: "USD", Exchange: "SMART"}
	daily := rpc.LendingMarketRow{Symbol: "BBB", Contract: c, Status: "partial", Price: new(20.0), PriceKind: "close", PriceAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	s.lendingMarket.entries = map[string]lendingMarketEntry{"BBB": {contract: c, daily: daily, until: now.Add(time.Hour), family: "short_interest", attempted: now.Add(-time.Hour), pricePhaseDone: true}}
	p, _ := rpc.NormalizeShortInterestScreenParams(rpc.ShortInterestScreenParams{Limit: 2, MinPrice: 5, SortBy: "short_interest_shares", SortDir: "desc"})
	raw, _ := json.Marshal(p)
	out, err := s.handleShortInterestScreen(t.Context(), &rpc.Request{Params: raw})
	if err != nil || len(out.Rows) != 1 || out.Rows[0].Symbol != "BBB" {
		t.Fatalf("known filtered match lost: %+v %v", out, err)
	}
	unresolved, exists := s.lendingMarket.entries["AAA"]
	if !exists || unresolved.contract.ConID != 0 || !now.Before(unresolved.focusUntil) {
		t.Fatal("known match canceled missing-feed candidate before resolution")
	}
	if s.lendingMarket.entries["BBB"].focusRank != 0 || unresolved.focusRank != 1 {
		t.Fatal("discovery tail displaced visible row priority")
	}
}

func TestLendingForegroundRetriesCannotStarveUnattemptedRows(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cache := lendingMarketCache{entries: map[string]lendingMarketEntry{
		"AAA": {contract: rpc.ContractParams{Symbol: "AAA", ConID: 91001}, family: "short_interest", focusFamily: "short_interest", focusRank: 0, focusUntil: now.Add(time.Hour), until: now.Add(time.Hour), attempted: now.Add(-time.Hour)},
		"ZZZ": {contract: rpc.ContractParams{Symbol: "ZZZ", ConID: 91002}, family: "short_interest", focusFamily: "short_interest", focusRank: 99, focusUntil: now.Add(time.Hour), until: now.Add(time.Hour)},
	}}
	symbol, _ := selectLendingMarket(&cache, now, "2026-10-02")
	if symbol != "ZZZ" {
		t.Fatal("due low-rank failure starved never-attempted displayed row")
	}
}
