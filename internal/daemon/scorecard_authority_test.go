package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/scorecard"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestScorecardLedgerAppendsEachDecisionOnceAndReloads(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{coreStore: store}
	scope := scorecardLedgerScopePrefix + "scope_test"
	at := time.Date(2026, 1, 12, 15, 0, 0, 0, time.UTC)
	ledger := []scorecard.Decision{
		{ID: "decision_a", Sleeve: "owner", Underlying: "SYNA", UnderlyingConID: 1, ConID: 1, Symbol: "SYNA", Instrument: scorecard.InstrumentStock, Action: "open", Side: "buy", Sign: 1, Quantity: 10, ExecutedAt: at, ClusterKey: "owner:2026-W03"},
		{ID: "decision_b", Sleeve: "owner", Underlying: "SYNA", UnderlyingConID: 1, ConID: 1, Symbol: "SYNA", Instrument: scorecard.InstrumentStock, Action: "exit", Side: "sell", Sign: -1, Quantity: 10, ExecutedAt: at.AddDate(0, 0, 9), ClusterKey: "owner:2026-W04"},
	}
	known, err := srv.loadScorecardLedger(t.Context(), scope)
	if err != nil || len(known) != 0 {
		t.Fatalf("fresh ledger = %v, %v", known, err)
	}
	if err := srv.appendScorecardDecisions(t.Context(), scope, known, ledger); err != nil {
		t.Fatal(err)
	}
	reloaded, err := srv.loadScorecardLedger(t.Context(), scope)
	if err != nil || len(reloaded) != 2 || !reloaded["decision_a"] || !reloaded["decision_b"] {
		t.Fatalf("reloaded ledger = %v, %v", reloaded, err)
	}
	// A second pass with the same derivation appends nothing; a new
	// decision appends once.
	ledger = append(ledger, scorecard.Decision{ID: "decision_c", Sleeve: "owner", ConID: 2, UnderlyingConID: 2, Instrument: scorecard.InstrumentStock, ExecutedAt: at.AddDate(0, 0, 20)})
	if err := srv.appendScorecardDecisions(t.Context(), scope, reloaded, ledger); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListObservations(t.Context(), corestore.ObservationQuery{ScopeKey: scope, Source: scorecardSource, Kind: scorecardDecisionKind})
	if err != nil || len(rows) != 3 {
		t.Fatalf("observations = %d, %v", len(rows), err)
	}
	for _, row := range rows {
		if row.DecisionEligible {
			t.Fatalf("ledger row %d is decision-eligible", row.ID)
		}
	}
}

func TestScorecardHistoryNeedsPlanMarketFirstOncePerSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	ledger := []scorecard.Decision{
		{ID: "a", UnderlyingConID: 2, Underlying: "SYNB", Currency: "USD", ExecutedAt: time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)},
		{ID: "b", UnderlyingConID: 1, Underlying: "SYNA", Currency: "USD", ExecutedAt: time.Date(2025, 9, 1, 15, 0, 0, 0, time.UTC)},
		{ID: "c", UnderlyingConID: 1, Underlying: "SYNA", Currency: "USD", ExecutedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)},
		{ID: "d", UnderlyingConID: 0, Underlying: "", ExecutedAt: now},
	}
	// Both names are Information Technology: one sector fund serves them,
	// read after the market and before the names, from the earliest entry.
	sectorFunds := map[int64]string{1: "XLK", 2: "XLK"}
	needs := scorecardHistoryNeeds(ledger, sectorFunds, scorecardBarCache{Names: map[string]scorecardBarSeries{}}, now)
	if len(needs) != 4 || !needs[0].market || !needs[1].sector || needs[1].symbol != "XLK" || needs[1].lookback != needs[0].lookback || needs[2].conid != 1 || needs[3].conid != 2 {
		t.Fatalf("needs = %+v", needs)
	}
	// Without a sector map only the market and the names are read.
	needs = scorecardHistoryNeeds(ledger, nil, scorecardBarCache{Names: map[string]scorecardBarSeries{}}, now)
	if len(needs) != 3 || !needs[0].market || needs[1].conid != 1 || needs[2].conid != 2 {
		t.Fatalf("needs = %+v", needs)
	}
	// The market and the oldest name need 400 days before the 2025 entry;
	// the September name needs 400 days before its own entry (443 today).
	if needs[0].lookback < 400+365 || needs[1].lookback != needs[0].lookback || needs[2].lookback != 443 || needs[0].lookback > scorecardMaxLookbackDays {
		t.Fatalf("lookbacks = %d %d %d", needs[0].lookback, needs[1].lookback, needs[2].lookback)
	}
	// A name read today is not read again today; a name whose read failed
	// waits for its retry time; a covered name is not fetched at all.
	covered := make([]edgecore.DailyBar, 0, 500)
	for i := range 500 {
		covered = append(covered, edgecore.DailyBar{ConID: 2, Day: now.AddDate(0, 0, -500+i), Close: 100})
	}
	cache := scorecardBarCache{Names: map[string]scorecardBarSeries{
		"1": {ConID: 1, Failure: "historical data timeout for SYNA after 45s", FailedAt: now.Add(-time.Hour), Failures: 1, RetryAt: now.Add(time.Hour)},
		"2": {ConID: 2, FetchedAt: now.AddDate(0, 0, -1), Lookback: 443, Bars: covered},
	}, Market: scorecardBarSeries{FetchedAt: now.Add(-time.Hour), Lookback: 900, Bars: covered},
		Sectors: map[string]scorecardBarSeries{"XLK": {Symbol: "XLK", FetchedAt: now.Add(-time.Hour), Lookback: 900, Bars: covered}}}
	if needs := scorecardHistoryNeeds(ledger, sectorFunds, cache, now); len(needs) != 0 {
		t.Fatalf("needs after today's fetches = %+v", needs)
	}
	// At its retry time the failed name is asked again, alone.
	if needs := scorecardHistoryNeeds(ledger, sectorFunds, cache, now.Add(time.Hour)); len(needs) != 1 || needs[0].conid != 1 {
		t.Fatalf("needs at the retry time = %+v", needs)
	}
	// Tomorrow the market and the sector fund advance (the October decision
	// is immature) and the failed name is still due; the matured, covered
	// name is not, and a short listing fetched with the full lookback is not
	// asked again.
	tomorrow := now.AddDate(0, 0, 1)
	needs = scorecardHistoryNeeds(ledger, sectorFunds, cache, tomorrow)
	if len(needs) != 3 || !needs[0].market || !needs[1].sector || needs[2].conid != 1 {
		t.Fatalf("needs tomorrow = %+v", needs)
	}
	short := scorecardBarSeries{ConID: 2, FetchedAt: now.AddDate(0, 0, -1), Lookback: 443, Bars: covered[400:]}
	if scorecardSeriesStale(short, ledger[0].ExecutedAt, ledger[0].ExecutedAt, tomorrow) {
		t.Fatal("a short listing fetched with the full lookback was asked again")
	}
	if got := scorecardNormalizeBars(append([]edgecore.DailyBar{{Day: now, Close: 0}, {Day: now, Close: 5}}, covered...), 2); len(got) != 501 || got[500].Close != 5 || got[0].ConID != 2 {
		t.Fatalf("normalized = %d bars, last %+v", len(got), got[len(got)-1])
	}
}

func TestScorecardReadBoundsToSleeveAndPage(t *testing.T) {
	t.Parallel()
	result := rpc.ScorecardResult{
		Sleeves:   []rpc.ScorecardSleeve{{Sleeve: "owner", Horizon: 10, Clusters: 3}, {Sleeve: "algo", Horizon: 10, Clusters: 1}},
		Decisions: []rpc.ScorecardDecision{{ID: "1", Sleeve: "owner"}, {ID: "2", Sleeve: "algo"}, {ID: "3", Sleeve: "owner"}, {ID: "4", Sleeve: "owner"}},
	}
	got, err := scorecardBoundResult(result, rpc.ScorecardReadParams{Sleeve: "owner", Limit: 2})
	if err != nil || len(got.Sleeves) != 1 || got.Sleeves[0].Clusters != 3 || got.DecisionsTotal != 3 || len(got.Decisions) != 2 || got.Decisions[1].ID != "3" {
		t.Fatalf("bound = %+v, %v", got, err)
	}
	if _, err := scorecardBoundResult(result, rpc.ScorecardReadParams{Sleeve: "nobody", Limit: 2}); err == nil {
		t.Fatal("unknown sleeve was accepted")
	}
	all, err := scorecardBoundResult(result, rpc.ScorecardReadParams{Limit: 50})
	if err != nil || len(all.Sleeves) != 2 || all.DecisionsTotal != 4 || len(all.Decisions) != 4 {
		t.Fatalf("unbounded = %+v, %v", all, err)
	}
}

func TestScorecardSleeveResolverTagsRuleOriginsNeverOwner(t *testing.T) {
	t.Parallel()
	events := []orderJournalEvent{
		{ExecID: "e-stop", OrderRef: "canary-stop-1", Origin: rpc.OrderOriginPairedDevice},
		{ExecID: "e-sweep", OrderRef: "canary-sweep-1", Origin: rpc.OrderOriginPairedDevice},
		{ExecID: "e-pre", OrderRef: "canary-pre-1", Origin: rpc.OrderOriginDaemonPreAuthorised},
		{ExecID: "e-daemon", OrderRef: "canary-d-1", TransmitOrigin: corestore.OriginDaemon},
		{ExecID: "e-hand", OrderRef: "canary-hand-1", Origin: rpc.OrderOriginHumanTTY},
	}
	proposals := []proposalEvent{
		{Type: "submitted", OrderRef: "canary-stop-1", Bucket: rpc.TradeProposalBucketTrailingStop},
		{Type: "submitted", OrderRef: "canary-sweep-1", Bucket: rpc.TradeProposalBucketCashSweep},
		{Type: "previewed", OrderRef: "canary-hand-1", Bucket: rpc.TradeProposalBucketTrailingStop},
	}
	resolve := scorecardSleeveResolver(events, proposals)
	for _, tc := range []struct {
		exec       string
		wantSleeve string
		wantOrigin string
	}{
		{"e-stop", rpc.ScorecardSleeveRule, "proposal:trailing_stop"},
		{"e-sweep", rpc.ScorecardSleeveRule, "proposal:cash_sweep"},
		{"e-pre", rpc.ScorecardSleeveRule, "origin:" + rpc.OrderOriginDaemonPreAuthorised},
		{"e-daemon", rpc.ScorecardSleeveRule, "origin:daemon_internal"},
		{"e-hand", scorecard.SleeveOwner, ""},
		{"e-tws", scorecard.SleeveOwner, ""},
	} {
		got := resolve(edgecore.PositionChange{ExecutionIDs: []string{tc.exec}})
		if got.Name != tc.wantSleeve || got.Origin != tc.wantOrigin {
			t.Fatalf("%s resolved to %+v, want %s %q", tc.exec, got, tc.wantSleeve, tc.wantOrigin)
		}
	}
}

// The mandate's forced reduction is a rule's sale, never the owner's: a fill
// of a mandate_reduction row approved on the paired device counts as rule,
// as one the daemon placed under pre-authorisation does by its origin.
func TestScorecardSleeveResolverTagsMandateReductionAsRule(t *testing.T) {
	t.Parallel()
	if !scorecardRuleBucket(rpc.TradeProposalBucketMandateReduction) {
		t.Fatal("mandate_reduction is not a rule bucket")
	}
	events := []orderJournalEvent{
		{ExecID: "e-mandate", OrderRef: "canary-mandate-1", Origin: rpc.OrderOriginPairedDevice},
		{ExecID: "e-mandate-auto", OrderRef: "canary-mandate-2", Origin: rpc.OrderOriginDaemonPreAuthorised},
	}
	proposals := []proposalEvent{
		{Type: "submitted", OrderRef: "canary-mandate-1", Bucket: rpc.TradeProposalBucketMandateReduction},
		{Type: "submitted", OrderRef: "canary-mandate-2", Bucket: rpc.TradeProposalBucketMandateReduction},
	}
	resolve := scorecardSleeveResolver(events, proposals)
	for _, exec := range []string{"e-mandate", "e-mandate-auto"} {
		got := resolve(edgecore.PositionChange{ExecutionIDs: []string{exec}})
		if got.Name != rpc.ScorecardSleeveRule || got.Origin != "proposal:"+rpc.TradeProposalBucketMandateReduction {
			t.Fatalf("%s resolved to %+v, want rule proposal:mandate_reduction", exec, got)
		}
	}
}

// A failed read is not written off for the day: the name is asked again at
// its retry time (the next pass, then doubling to at most a day), and a
// success today is the only fetch that counts as today's.
func TestScorecardFailedSeriesIsRetriedAtItsRetryTimeNotTomorrow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 1, 24, 0, 0, time.UTC)
	entry := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	// A failure the shipped cache stamped as fetched today (no retry time
	// yet) is retried one pass later, not tomorrow.
	legacy := scorecardBarSeries{ConID: 1, Symbol: "SYNA", FetchedAt: now, Failure: "historical data timeout for SYNA after 45s"}
	if scorecardSeriesStale(legacy, entry, entry, now.Add(time.Hour)) {
		t.Fatal("a failed series was asked again before its retry time")
	}
	if !scorecardSeriesStale(legacy, entry, entry, now.Add(scorecardPassInterval)) {
		t.Fatal("a failed series was written off for the day instead of retried at its retry time")
	}
	fetched := scorecardBarSeries{ConID: 1, Symbol: "SYNA", FetchedAt: now, Lookback: 400, Bars: []edgecore.DailyBar{{ConID: 1, Day: now.AddDate(0, 0, -1), Close: 100}}}
	if scorecardSeriesStale(fetched, entry, entry, now.Add(scorecardPassInterval)) {
		t.Fatal("a series fetched today was asked again today")
	}
	failed := scorecardBarSeries{ConID: 1, Symbol: "SYNA", FetchedAt: now.AddDate(0, 0, -1), Failure: "historical data timeout for SYNA after 45s", FailedAt: now, Failures: 3, RetryAt: now.Add(8 * time.Hour)}
	if scorecardSeriesStale(failed, entry, entry, now.Add(7*time.Hour)) || !scorecardSeriesStale(failed, entry, entry, now.Add(8*time.Hour)) {
		t.Fatal("a failed series is asked again at its retry time, not before")
	}
	// The wait doubles per consecutive failure, from one pass to a day.
	for failures, want := range map[int]time.Duration{1: 2 * time.Hour, 2: 4 * time.Hour, 3: 8 * time.Hour, 4: 16 * time.Hour, 5: 24 * time.Hour, 9: 24 * time.Hour} {
		if got := scorecardRetryAfter(failures); got != want {
			t.Fatalf("retry after %d failures = %s, want %s", failures, got, want)
		}
	}
	// The log warns once per name per day; the result lists every failure.
	if !scorecardFailureWarnDue(scorecardBarSeries{}, now) || scorecardFailureWarnDue(failed, now.Add(3*time.Hour)) || !scorecardFailureWarnDue(failed, now.AddDate(0, 0, 1)) {
		t.Fatal("the failure warning is due once per name per day")
	}
}

// A failed read keeps the closes of the last successful one and leaves its
// session stamp alone; a gateway that is away ends the pass with no mark
// against any name. The result lists every failure with its retry time.
func TestScorecardFetchPassKeepsBarsOnFailureAndStopsWhenTheGatewayIsAway(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 18, 1, 24, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	srv := &Server{coreStore: store}
	srv.edgeFetchMarketBarsFn = func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return nil, errors.New("historical data timeout for SPY after 45s")
	}
	srv.edgeFetchBarsFn = func(_ context.Context, contract ibkrlib.Contract, _ int) ([]ibkrlib.HistoricalBar, error) {
		if contract.ConID == 1 {
			return []ibkrlib.HistoricalBar{{Date: "2026-10-16", Close: 101}, {Date: "2026-10-17", Close: 102}}, nil
		}
		return nil, fmt.Errorf("gateway unavailable: %w", ibkrlib.ErrIBKRUnavailable)
	}
	kept := []edgecore.DailyBar{{Day: yesterday.AddDate(0, 0, -1), Close: 500}, {Day: yesterday, Close: 501}}
	cache := scorecardBarCache{Version: scorecardBarsVersion, ScopeFingerprint: "scope_test", Names: map[string]scorecardBarSeries{}, Market: scorecardBarSeries{Symbol: "SPY", Currency: "USD", FetchedAt: yesterday, Lookback: 400, Bars: kept}}
	needs := []scorecardHistoryNeed{
		{market: true, symbol: "SPY", currency: "USD", lookback: 400},
		{conid: 1, symbol: "SYNA", currency: "USD", lookback: 400},
		{conid: 2, symbol: "SYNB", currency: "USD", lookback: 400},
		{conid: 3, symbol: "SYNC", currency: "USD", lookback: 400},
	}
	pending, err := srv.fetchScorecardNeeds(t.Context(), &cache, needs, now)
	if err != nil || pending != 2 {
		t.Fatalf("pending = %d, %v; want 2 (the pass ends when the gateway is away)", pending, err)
	}
	market := cache.Market
	if len(market.Bars) != 2 || !market.FetchedAt.Equal(yesterday) || market.Failure == "" || !market.FailedAt.Equal(now) || market.Failures != 1 || !market.RetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("failed market series = %+v; want yesterday's bars and stamp kept, failure recorded, retry in one pass", market)
	}
	name := cache.Names["1"]
	if len(name.Bars) != 2 || !name.FetchedAt.Equal(now) || name.Failure != "" || name.Lookback != 400 {
		t.Fatalf("fetched series = %+v", name)
	}
	if _, ok := cache.Names["2"]; ok {
		t.Fatal("a name the gateway could not serve was marked")
	}
	reloaded, err := srv.loadScorecardBarCache(t.Context())
	if err != nil || len(reloaded.Market.Bars) != 2 || reloaded.Market.Failure != market.Failure || len(reloaded.Names) != 1 {
		t.Fatalf("saved cache = %+v, %v", reloaded, err)
	}
	history, state := scorecardHistoryFromCache(cache, pending)
	if len(history.Market) != 2 || len(history.Underlying[1]) != 2 || state.Names != 1 || state.Missing != 0 || state.Pending != 2 || !state.LastFetchedAt.Equal(now) {
		t.Fatalf("history = %d market bars, %d names; state = %+v", len(history.Market), len(history.Underlying), state)
	}
	if len(state.Failures) != 1 || state.Failures[0].Symbol != "SPY" || state.Failures[0].ConID != 0 || state.Failures[0].Bars != 2 || state.Failures[0].Failure != market.Failure || !state.Failures[0].RetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("failures = %+v", state.Failures)
	}
	// A second failure doubles the wait and keeps the bars; a success
	// clears the failure and stamps the session.
	again, err := srv.fetchScorecardSeries(t.Context(), cache.Market, needs[0], now.Add(2*time.Hour))
	if err == nil || len(again.Bars) != 2 || again.Failures != 2 || !again.RetryAt.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("second failure = %+v, %v", again, err)
	}
	srv.edgeFetchMarketBarsFn = func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return []ibkrlib.HistoricalBar{{Date: "2026-10-17", Close: 503}}, nil
	}
	fixed, err := srv.fetchScorecardSeries(t.Context(), again, needs[0], now.Add(6*time.Hour))
	if err != nil || len(fixed.Bars) != 1 || fixed.Failure != "" || fixed.Failures != 0 || !fixed.RetryAt.IsZero() || !fixed.FetchedAt.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("recovered series = %+v, %v", fixed, err)
	}
}

// TestScorecardSectorMapNamesOneFundPerSector: a name in the registry gets
// its sector fund once however many decisions it has; a name off the list
// and an index underlying get none.
func TestScorecardSectorMapNamesOneFundPerSector(t *testing.T) {
	t.Parallel()
	registry := map[string]string{"SYNA": "Information Technology", "SYNB": "Health Care"}
	sectorOf := func(symbol string) (string, bool) {
		sector, ok := registry[symbol]
		return sector, ok
	}
	ledger := []scorecard.Decision{
		{UnderlyingConID: 1, Underlying: "SYNA"}, {UnderlyingConID: 1, Underlying: "SYNA"}, {UnderlyingConID: 2, Underlying: "SYNB"},
		{UnderlyingConID: 3, Underlying: "SYNX"}, {UnderlyingConID: 4, Underlying: "SPY"}, {UnderlyingConID: 0, Underlying: "SYNA"},
	}
	funds, names := scorecardSectorMap(ledger, sectorOf)
	if len(funds) != 2 || funds[1] != "XLK" || funds[2] != "XLV" || names[1] != "Information Technology" || names[2] != "Health Care" {
		t.Fatalf("funds = %v names = %v", funds, names)
	}
}

// TestScorecardFillCostsMatchByExecutionIDThenByTime: a decision whose
// statement execution ids met retained records sums their half-spreads; one
// without such ids takes the retained fill on the same contract nearest in
// time within five minutes, and none further away; a record whose quote was
// unavailable carries its reason and no cost.
func TestScorecardFillCostsMatchByExecutionIDThenByTime(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	at := fillQuoteTestAt
	quoted := func(execID string, conID int, execAt time.Time, price, mid float64, side string, qty float64) rpc.FillQuoteRecord {
		return rpc.FillQuoteRecord{Version: fillQuoteVersion, ExecID: execID, Source: rpc.FillQuoteSourceBroker, Seen: rpc.FillQuoteSeenPoll,
			Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNX", SecType: "OPT", Currency: "USD", Multiplier: 100}, Side: side, Quantity: qty, Price: price, ExecTime: execAt, ObservedAt: execAt,
			Quote:        rpc.FillQuote{Status: rpc.FillQuoteQuoted, Bid: new(mid - 0.05), Ask: new(mid + 0.05), Mid: new(mid), DataType: rpc.MarketDataLive, QuoteAt: execAt.Add(2 * time.Second), GapMS: new(int64(2000))},
			BaseCurrency: "EUR", FXRate: new(0.9)}
	}
	for _, rec := range []rpc.FillQuoteRecord{
		quoted("e-1", 900001, at, 1.30, 1.25, rpc.OrderActionBuy, 2),
		quoted("e-2", 900001, at.Add(30*time.Second), 1.28, 1.25, rpc.OrderActionBuy, 1),
		quoted("e-3", 900002, at.Add(2*time.Minute), 10.40, 10.50, rpc.OrderActionSell, 100),
	} {
		if err := s.fillQuotes.append("DU1234567", rec); err != nil {
			t.Fatal(err)
		}
	}
	unavailable := quoted("e-4", 900003, at, 5, 5, rpc.OrderActionBuy, 1)
	unavailable.Quote = rpc.FillQuote{Status: rpc.FillQuoteUnavailable, Reason: "timeout"}
	if err := s.fillQuotes.append("DU1234567", unavailable); err != nil {
		t.Fatal(err)
	}
	ledger := []scorecard.Decision{
		{ID: "by-exec", ConID: 900001, ExecutedAt: at, Lineage: scorecard.Lineage{ExecutionIDs: []string{"e-1", "e-2", "e-missing"}}},
		{ID: "by-time", ConID: 900002, ExecutedAt: at.Add(4 * time.Minute), Lineage: scorecard.Lineage{ExecutionIDs: []string{"statement-only"}}},
		{ID: "too-far", ConID: 900002, ExecutedAt: at.Add(20 * time.Minute)},
		{ID: "unquoted", ConID: 900003, ExecutedAt: at, Lineage: scorecard.Lineage{ExecutionIDs: []string{"e-4"}}},
	}
	costs := s.scorecardFillCosts(ledger)
	byExec := costs["by-exec"]
	// (1.30 − 1.25) × 2 × 100 + (1.28 − 1.25) × 1 × 100 = 13 USD, 11.70 EUR;
	// 400 bp and 240 bp weighted 2:1.
	if byExec.Match != "exec_id" || byExec.Fills != 3 || byExec.Quoted != 2 || byExec.HalfSpread == nil || math.Abs(*byExec.HalfSpread-13) > 1e-9 || byExec.HalfSpreadBase == nil || math.Abs(*byExec.HalfSpreadBase-11.7) > 1e-9 || byExec.Currency != "USD" || byExec.Bp == nil || math.Abs(*byExec.Bp-1040.0/3) > 1e-6 || byExec.GapMS == nil || *byExec.GapMS != 2000 || byExec.Reason != "" {
		t.Fatalf("by exec id = %+v (half %v base %v bp %v)", byExec, derefFloat(byExec.HalfSpread), derefFloat(byExec.HalfSpreadBase), derefFloat(byExec.Bp))
	}
	byTime := costs["by-time"]
	// A sell of 100 contracts at 10.40 against a mid of 10.50 pays
	// 0.10 × 100 × 100 = 1,000 USD.
	if byTime.Match != "time" || byTime.Fills != 1 || byTime.Quoted != 1 || byTime.HalfSpread == nil || math.Abs(*byTime.HalfSpread-1000) > 1e-9 {
		t.Fatalf("by time = %+v", byTime)
	}
	if _, ok := costs["too-far"]; ok {
		t.Fatal("a fill twenty minutes from the retained one was matched")
	}
	unq := costs["unquoted"]
	if unq.Quoted != 0 || unq.HalfSpread != nil || unq.Reason != "timeout" {
		t.Fatalf("unquoted = %+v", unq)
	}
}

// TestScorecardFillCostsMeetTheStatementClockInItsZone: the statement writes
// its trade time on the broker's Eastern clock (11:30:00) while the retained
// record keeps the true instant (15:30:20 UTC, twenty seconds later). The
// ledger built from that statement must meet the record within the
// five-minute tolerance; read as UTC it would miss by four hours and the
// decision would be called execution_not_observed.
func TestScorecardFillCostsMeetTheStatementClockInItsZone(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	xml := `<FlexQueryResponse><FlexStatements><FlexStatement accountId="DU1234567" fromDate="20261019" toDate="20261019" whenGenerated="20261020;010203">
<Trades><Trade accountId="DU1234567" assetCategory="STK" currency="USD" fxRateToBase="0.9" symbol="SYNX" conid="900002" tradeID="t1" ibOrderID="o1" ibExecID="statement-only" dateTime="20261019;113000" tradeDate="20261019" buySell="BUY" quantity="100" tradePrice="10.40" multiplier="1" ibCommission="-1" taxes="0" openCloseIndicator="O" levelOfDetail="EXECUTION" /></Trades>
<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20261019" total="10000"/></EquitySummaryInBase>
</FlexStatement></FlexStatements></FlexQueryResponse>`
	statements, err := flexstmt.Parse([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := edgecore.PositionChanges(statements, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	ledger, _ := scorecard.BuildLedger(evidence, nil)
	if len(ledger) != 1 {
		t.Fatalf("ledger = %+v", ledger)
	}
	instant := time.Date(2026, 10, 19, 15, 30, 20, 0, time.UTC)
	rec := rpc.FillQuoteRecord{Version: fillQuoteVersion, ExecID: "seen-by-poll", Source: rpc.FillQuoteSourceBroker, Seen: rpc.FillQuoteSeenPoll,
		Contract: rpc.ContractParams{ConID: 900002, Symbol: "SYNX", SecType: "STK", Currency: "USD"}, Side: rpc.OrderActionBuy, Quantity: 100, Price: 10.40, ExecTime: instant, ObservedAt: instant,
		Quote:        rpc.FillQuote{Status: rpc.FillQuoteQuoted, Bid: new(10.30), Ask: new(10.40), Mid: new(10.35), DataType: rpc.MarketDataLive, QuoteAt: instant.Add(2 * time.Second), GapMS: new(int64(2000))},
		BaseCurrency: "EUR", FXRate: new(0.9)}
	if err := s.fillQuotes.append("DU1234567", rec); err != nil {
		t.Fatal(err)
	}
	cost, ok := s.scorecardFillCosts(ledger)[ledger[0].ID]
	if !ok || cost.Match != "time" || cost.Quoted != 1 || cost.HalfSpread == nil || math.Abs(*cost.HalfSpread-5) > 1e-9 {
		t.Fatalf("statement fill at 11:30 Eastern did not meet the record at 15:30:20Z: %+v (ledger executed at %s)", cost, ledger[0].ExecutedAt)
	}
}

// TestScorecardFillCostsBindARecordOnce: a record a decision claimed by
// execution id is not also the nearest-in-time stand-in for a second
// decision on the same contract minutes later; that decision stays without
// a cost, whichever order the ledger lists them in.
func TestScorecardFillCostsBindARecordOnce(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	at := fillQuoteTestAt
	rec := rpc.FillQuoteRecord{Version: fillQuoteVersion, ExecID: "e-1", Source: rpc.FillQuoteSourceBroker, Seen: rpc.FillQuoteSeenPoll,
		Contract: rpc.ContractParams{ConID: 900001, Symbol: "SYNX", SecType: "OPT", Currency: "USD", Multiplier: 100}, Side: rpc.OrderActionBuy, Quantity: 2, Price: 1.30, ExecTime: at, ObservedAt: at,
		Quote:        rpc.FillQuote{Status: rpc.FillQuoteQuoted, Bid: new(1.20), Ask: new(1.30), Mid: new(1.25), DataType: rpc.MarketDataLive, QuoteAt: at.Add(2 * time.Second), GapMS: new(int64(2000))},
		BaseCurrency: "EUR", FXRate: new(0.9)}
	if err := s.fillQuotes.append("DU1234567", rec); err != nil {
		t.Fatal(err)
	}
	claimed := scorecard.Decision{ID: "by-exec", ConID: 900001, ExecutedAt: at.Add(2 * time.Minute), Lineage: scorecard.Lineage{ExecutionIDs: []string{"e-1"}}}
	later := scorecard.Decision{ID: "statement-only", ConID: 900001, ExecutedAt: at.Add(time.Minute), Lineage: scorecard.Lineage{ExecutionIDs: []string{"not-retained"}}}
	for _, ledger := range [][]scorecard.Decision{{claimed, later}, {later, claimed}} {
		costs := s.scorecardFillCosts(ledger)
		if got := costs["by-exec"]; got.Match != "exec_id" || got.Quoted != 1 {
			t.Fatalf("exec-id match = %+v", got)
		}
		if got, ok := costs["statement-only"]; ok {
			t.Fatalf("the record bound twice: %+v", got)
		}
	}
}
