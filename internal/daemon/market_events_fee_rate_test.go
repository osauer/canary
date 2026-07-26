package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestBorrowFeeNotDueNeverFallsBackOrBecomesPolicyEligible(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	called := 0
	cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		called++
		return nil, errors.New("must not run")
	}
	bulk := marketEventBorrowFeeEntry{
		FetchedAt: now.Add(-24 * time.Hour), AsOf: now.Add(-24 * time.Hour),
		Symbols: map[string]marketEventBorrowFeeRecord{"XYZ": {Symbol: "XYZ", FeeRate: 80, Available: 100}},
	}
	health := rpc.SourceHealth{Source: "borrow_fee", Status: rpc.SourceStatusStale, RefreshState: rpc.SourceRefreshNotDue, AsOf: bulk.AsOf}

	rows, gotHealth := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, nil, now, bulk, health)
	if called != 0 {
		t.Fatalf("not-due FTP invoked TWS fallback %d times", called)
	}
	if len(rows) != 1 || rows[0].CoverageScope != rpc.BorrowFeeCoverageGlobal || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].PolicyEligible {
		t.Fatalf("not-due stale bulk coverage = %+v", rows)
	}
	if gotHealth.Status != rpc.SourceStatusStale || borrowFeeFTPPolicyUsable(gotHealth) {
		t.Fatalf("not-due stale health became policy-usable: %+v", gotHealth)
	}
}

func TestExactHeldShortFeeRateTargetsRetainConIDCollisionsAndUnresolvedRoutes(t *testing.T) {
	scope := marketEventFeeRateScopeFingerprint(brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper})
	raw := []*ibkrlib.RawPosition{
		feeRateTestPosition("DU123", 101, "DUP", -10, "SMART"),
		feeRateTestPosition("DU123", 102, "DUP", -20, "SMART"),
		feeRateTestPosition("DU123", 103, "DUP", -30, ""),
		feeRateTestPosition("DU123", 0, "DUP", -5, ""),
		feeRateTestPosition("DU123", 104, "DUP", 40, "SMART"),
		feeRateTestPosition("DU123", 105, "OTHER", -50, "SMART"),
	}
	raw[1].Contract.Multiplier = 2
	raw[1].Contract.SecIDType, raw[1].Contract.SecID = "ISIN", "US0000000001"

	targets, gaps := exactHeldShortFeeRateTargets(raw, []string{"DUP", "MISSING"}, scope)
	conIDs := map[int]bool{}
	for _, target := range targets {
		conIDs[target.contract.ConID] = true
	}
	if len(targets) != 3 || !conIDs[101] || !conIDs[102] || !conIDs[103] {
		t.Fatalf("exact duplicate-symbol targets = %+v", targets)
	}
	if len(gaps) != 1 {
		t.Fatalf("route/symbol gaps = %+v", gaps)
	}
	byReason := map[string]rpc.MarketEventBorrowFeeCoverage{}
	for _, row := range gaps {
		byReason[row.Reason] = row
	}
	if row := byReason["missing_contract_id"]; row.Symbol != "DUP" || row.ContractConID != 0 {
		t.Fatalf("missing exact-contract gap = %+v", row)
	}
	if _, ok := byReason["not_held_short_exact_contract"]; ok {
		t.Fatalf("non-held requested symbol became relevant: %+v", gaps)
	}
}

func TestHeldShortFeeRateFallbackPersistsUncommissionedEvidenceAndRetriesAfterRestart(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	completedDate := feeRateCompletedSessionDate(t, now)
	authority := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	cache.readCachedPositions = feeRateCurrentPositionsReader(now, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
	fetchCalls := 0
	cache.fetchHistoricalFeeRates = func(_ context.Context, contract ibkrlib.Contract, _ int, _ time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		if contract.ConID != 481516 || contract.Symbol != "XYZ" || contract.Exchange != "SMART" {
			t.Fatalf("fallback contract rerouted: %+v", contract)
		}
		return []ibkrlib.HistoricalBar{{Date: strings.ReplaceAll(completedDate, "-", ""), Open: 80, High: 82, Low: 79, Close: 81}}, nil
	}
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }
	primary := failedBorrowFeeHealth(now)

	rows, health := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, primary)
	if fetchCalls != 1 || len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageScaleUnknown || rows[0].CoverageScope != rpc.BorrowFeeCoveragePortfolioOnly || rows[0].PolicyEligible || rows[0].FeeRate == nil || *rows[0].FeeRate != 81 {
		t.Fatalf("first fallback calls=%d rows=%+v", fetchCalls, rows)
	}
	if health.Status != rpc.SourceStatusPartial {
		t.Fatalf("uncommissioned aggregate health = %+v", health)
	}

	doc, ok, err := authority.GetStateDocument(t.Context(), marketEventFeeRateScope, marketEventFeeRateStateKind)
	if err != nil || !ok {
		t.Fatalf("fallback state ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(doc.JSON), "DU123") || strings.Contains(string(doc.JSON), "SECRET") {
		t.Fatalf("raw account/prose persisted: %s", doc.JSON)
	}
	observations, err := authority.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: marketEventFeeRateScope, Source: marketEventFeeRateSource, Kind: marketEventFeeRateObservationKind,
	})
	if err != nil || len(observations) != 1 || observations[0].DecisionEligible {
		t.Fatalf("fallback observations=%+v err=%v", observations, err)
	}
	if strings.Contains(string(observations[0].Payload), "DU123") {
		t.Fatalf("raw account persisted in observation: %s", observations[0].Payload)
	}

	restarted := newMarketEventCache(func() time.Time { return now.Add(time.Minute) })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	restarted.readCachedPositions = feeRateCurrentPositionsReader(now.Add(time.Minute), feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
	restarted.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureNotEntitled, Message: "SECRET broker prose"}
	}
	rows, health = restarted.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now.Add(time.Minute), marketEventBorrowFeeEntry{}, primary)
	if fetchCalls != 2 {
		t.Fatalf("persisted attempt suppressed fresh restart read: calls=%d", fetchCalls)
	}
	if len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].Entitlement != rpc.BorrowFeeEntitlementNotEntitled || rows[0].LastFailure == nil || rows[0].LastFailure.Code != rpc.SourceFailureNotEntitled || strings.Contains(rows[0].Reason, "SECRET") {
		t.Fatalf("restart not-entitled projection = %+v", rows)
	}
	if health.Status != rpc.SourceStatusStale {
		t.Fatalf("fresh typed failure with retained context = %+v", health)
	}
	doc, ok, err = authority.GetStateDocument(t.Context(), marketEventFeeRateScope, marketEventFeeRateStateKind)
	if err != nil || !ok {
		t.Fatalf("post-failure state ok=%v err=%v", ok, err)
	}
	observations, err = authority.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: marketEventFeeRateScope, Source: marketEventFeeRateSource, Kind: marketEventFeeRateObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("post-failure observations=%+v err=%v", observations, err)
	}
	for _, payload := range append([][]byte{doc.JSON}, observations[1].Payload) {
		if strings.Contains(string(payload), rpc.SourceFailureNotEntitled) || strings.Contains(string(payload), "SECRET") || strings.Contains(string(payload), `"failure":`) {
			t.Fatalf("runtime entitlement/failure persisted: %s", payload)
		}
	}

	// Only the identical-request 15-second wire boundary survives restart;
	// entitlement and the longer runtime suppression do not.
	secondAt := now.Add(time.Minute + 5*time.Second)
	secondRestart := newMarketEventCache(func() time.Time { return secondAt })
	if err := secondRestart.UseCoreStore(authority); err != nil {
		t.Fatalf("second restart UseCoreStore: %v", err)
	}
	secondRestart.readCachedPositions = feeRateCurrentPositionsReader(secondAt, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
	secondRestart.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, errors.New("15-second wire boundary was ignored")
	}
	rows, health = secondRestart.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, secondAt, marketEventBorrowFeeEntry{}, primary)
	if fetchCalls != 2 || len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].Entitlement != rpc.BorrowFeeEntitlementUnknown || rows[0].LastFailure != nil || health.Status != rpc.SourceStatusStale {
		t.Fatalf("failed restart cooldown calls=%d rows=%+v health=%+v", fetchCalls, rows, health)
	}

	thirdAt := now.Add(time.Minute + 16*time.Second)
	thirdRestart := newMarketEventCache(func() time.Time { return thirdAt })
	if err := thirdRestart.UseCoreStore(authority); err != nil {
		t.Fatalf("third restart UseCoreStore: %v", err)
	}
	thirdRestart.readCachedPositions = feeRateCurrentPositionsReader(thirdAt, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
	thirdRestart.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, errors.New("fresh runtime failure")
	}
	_, _ = thirdRestart.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, thirdAt, marketEventBorrowFeeEntry{}, primary)
	if fetchCalls != 3 {
		t.Fatalf("restart after wire boundary did not re-read broker evidence: calls=%d", fetchCalls)
	}
}

func TestHeldShortFeeRateProductionRoutePersistsResolvedExactContractAndHonorsSuccessCooldown(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	completedDate := feeRateCompletedSessionDate(t, now)
	authority := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	position := feeRateTestPosition("DU123", 481516, "XYZ", -10, "")
	cache.readCachedPositions = feeRateCurrentPositionsReader(now, position)
	resolveCalls := 0
	cache.resolveHistoricalFeeRoute = func(_ context.Context, contract ibkrlib.Contract, _ time.Duration) (ibkrlib.Contract, error) {
		resolveCalls++
		if contract.ConID != 481516 || contract.Exchange != "" || contract.PrimaryExch != "NASDAQ" {
			t.Fatalf("production position route request = %+v", contract)
		}
		contract.Exchange = "NASDAQ"
		return contract, nil
	}
	fetchCalls := 0
	cache.fetchHistoricalFeeRates = func(_ context.Context, contract ibkrlib.Contract, _ int, _ time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		if contract.ConID != 481516 || contract.Exchange != "NASDAQ" || contract.PrimaryExch != "NASDAQ" {
			t.Fatalf("resolved HMDS contract = %+v", contract)
		}
		return []ibkrlib.HistoricalBar{{Date: strings.ReplaceAll(completedDate, "-", ""), Open: 5, High: 6, Low: 4, Close: 5.5}}, nil
	}
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }
	rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
	if resolveCalls != 1 || fetchCalls != 1 || len(rows) != 1 || rows[0].ContractFingerprint == "" {
		t.Fatalf("route/fetch calls=%d/%d rows=%+v", resolveCalls, fetchCalls, rows)
	}
	if len(cache.borrowFeeFallback.LastGood) != 1 {
		t.Fatalf("persisted last-good = %+v", cache.borrowFeeFallback.LastGood)
	}
	for _, record := range cache.borrowFeeFallback.LastGood {
		if record.Contract.Exchange != "NASDAQ" || record.ContractFingerprint != marketEventFeeRateContractFingerprint(ibkrlib.Contract{
			ConID: 481516, Symbol: "XYZ", SecType: "STK", Exchange: "NASDAQ", PrimaryExch: "NASDAQ",
			Currency: "USD", LocalSymbol: "XYZ", TradingClass: "NMS", Multiplier: 1,
		}) {
			t.Fatalf("persisted route record = %+v", record)
		}
	}
	if err := validateMarketEventFeeRateState(cache.borrowFeeFallback, now); err != nil {
		t.Fatalf("resolved state validation: %v", err)
	}

	restartedAt := now.Add(5 * time.Second)
	restarted := newMarketEventCache(func() time.Time { return restartedAt })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	restarted.readCachedPositions = feeRateCurrentPositionsReader(restartedAt, position)
	restarted.resolveHistoricalFeeRoute = cache.resolveHistoricalFeeRoute
	restarted.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, errors.New("success cooldown was ignored")
	}
	rows, health := restarted.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, restartedAt, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(restartedAt))
	if resolveCalls != 1 || fetchCalls != 1 || len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].Reason != "restart_cooldown_retained_last_good" || health.Status != rpc.SourceStatusStale {
		t.Fatalf("success restart cooldown resolve/fetch=%d/%d rows=%+v health=%+v", resolveCalls, fetchCalls, rows, health)
	}
}

func TestHeldShortFeeRateRouteFailureBackoffRunsBeforeRouteWire(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	position := feeRateTestPosition("DU123", 481516, "XYZ", -10, "")
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }
	resolveCalls := 0
	configure := func(cache *marketEventCache, at time.Time) {
		cache.readCachedPositions = feeRateCurrentPositionsReader(at, position)
		cache.resolveHistoricalFeeRoute = func(context.Context, ibkrlib.Contract, time.Duration) (ibkrlib.Contract, error) {
			resolveCalls++
			return ibkrlib.Contract{}, &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureContractUnavailable}
		}
		cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
			t.Fatal("route failure reached HMDS fetch")
			return nil, nil
		}
	}
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	configure(cache, now)
	rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
	if resolveCalls != 1 || len(rows) != 1 || rows[0].Reason != rpc.SourceFailureContractUnavailable {
		t.Fatalf("first route failure calls=%d rows=%+v", resolveCalls, rows)
	}
	rows, _ = cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now.Add(time.Second), marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
	if resolveCalls != 1 || len(rows) != 1 {
		t.Fatalf("runtime route failure was not reused before wire: calls=%d rows=%+v", resolveCalls, rows)
	}

	restartAt := now.Add(5 * time.Second)
	restarted := newMarketEventCache(func() time.Time { return restartAt })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	configure(restarted, restartAt)
	rows, _ = restarted.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, restartAt, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(restartAt))
	if resolveCalls != 1 || len(rows) != 1 || rows[0].Reason != "restart_wire_cooldown" || rows[0].LastFailure != nil {
		t.Fatalf("restart route boundary calls=%d rows=%+v", resolveCalls, rows)
	}

	afterBoundary := now.Add(16 * time.Second)
	after := newMarketEventCache(func() time.Time { return afterBoundary })
	if err := after.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	configure(after, afterBoundary)
	_, _ = after.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, afterBoundary, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(afterBoundary))
	if resolveCalls != 2 {
		t.Fatalf("route failure remained durable beyond 15-second wire boundary: calls=%d", resolveCalls)
	}
}

func TestHeldShortFeeRateStaleSuccessHonorsNextAttemptBeforeRouteWire(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	position := feeRateTestPosition("DU123", 481516, "XYZ", -10, "")
	cache.readCachedPositions = feeRateCurrentPositionsReader(now, position)
	state := feeRateValidState(t, now)
	key := onlyFeeRateStateKey(t, state)
	record := state.LastGood[key]
	record.SessionDate = "2026-07-20"
	record.AsOf = marketCloseForDate(marketcal.MarketUSEquity, record.SessionDate, now).UTC()
	record.ObservedAt = now.Add(-time.Second)
	state.LastGood[key] = record
	next := now.Add(5 * time.Second)
	state.LastAttempts[key] = marketEventFeeRateAttempt{
		Outcome: marketEventFeeRateOutcomeSuccess, AttemptedAt: record.ObservedAt,
		CompletedAt: record.ObservedAt, NextAttempt: &next,
	}
	cache.borrowFeeFallback = state
	cache.borrowFeeFallbackCurrent = map[string]ibkrlib.HistoricalSessionBinding{key: {}}
	resolveCalls, fetchCalls := 0, 0
	cache.resolveHistoricalFeeRoute = func(context.Context, ibkrlib.Contract, time.Duration) (ibkrlib.Contract, error) {
		resolveCalls++
		return ibkrlib.Contract{}, errors.New("stale NextAttempt was ignored")
	}
	cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, errors.New("stale NextAttempt was ignored")
	}
	rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, func() brokerStateScope {
		return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	}, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
	if resolveCalls != 0 || fetchCalls != 0 || len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].Reason != "completed_session_bar_stale" {
		t.Fatalf("stale success bypassed boundary route/fetch=%d/%d rows=%+v", resolveCalls, fetchCalls, rows)
	}
}

func TestHeldShortFeeRateNoNewCompletedSessionBacksOffRepeatedHeartbeats(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	at := start
	cache := newMarketEventCache(func() time.Time { return at })
	cache.readCachedPositions = feeRateCurrentPositionsReader(start, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
	fetchCalls := 0
	cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		// 2026-07-21 is the latest completed session at start; returning the
		// prior session is valid retained context, not a current publication.
		return []ibkrlib.HistoricalBar{{Date: "20260720", Open: 4, High: 6, Low: 3, Close: 5}}, nil
	}
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }
	for i := range 3 {
		at = start.Add(time.Duration(i) * 30 * time.Second)
		rows, health := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, at, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(at))
		if len(rows) != 1 || rows[0].Status != rpc.BorrowFeeCoverageStale || rows[0].Reason != "completed_session_bar_stale_no_new_data" || rows[0].FeeRate == nil || rows[0].PolicyEligible || health.Status != rpc.SourceStatusStale {
			t.Fatalf("heartbeat %d stale publication rows=%+v health=%+v", i, rows, health)
		}
	}
	if fetchCalls != 1 {
		t.Fatalf("stale daily publication repeated HMDS on heartbeat: calls=%d", fetchCalls)
	}
	key := onlyFeeRateStateKey(t, cache.borrowFeeFallback)
	attempt := cache.borrowFeeFallback.LastAttempts[key]
	if attempt.Outcome != marketEventFeeRateOutcomeFailure || attempt.Failure == nil || attempt.Failure.Code != rpc.SourceFailureNoData || attempt.RuntimeNextAttempt == nil || !attempt.RuntimeNextAttempt.Equal(start.Add(marketEventFeeRateRetryAfter)) || attempt.NextAttempt == nil || !attempt.NextAttempt.Equal(start.Add(marketEventFeeRateWireBoundary)) {
		t.Fatalf("stale publication attempt = %+v", attempt)
	}
}

func TestHeldShortFeeRateReconnectInvalidatesRuntimeReuseAfterWireBoundary(t *testing.T) {
	start := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	at := start
	date := strings.ReplaceAll(feeRateCompletedSessionDate(t, start), "-", "")
	position := feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART")
	connector := &feeRateTestHistoricalConnector{
		positions: []*ibkrlib.RawPosition{position}, health: feeRatePortfolioHealth(start),
		bars: []ibkrlib.HistoricalBar{{Date: date, Open: 4, High: 6, Low: 3, Close: 5}},
	}
	cache := newMarketEventCache(func() time.Time { return at })
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }

	rows, _ := cache.heldShortFeeRateCoverage(t.Context(), []string{"XYZ"}, connector, scope, at, failedBorrowFeeHealth(at))
	if len(rows) != 1 || connector.fetchCallCount() != 1 {
		t.Fatalf("initial FEE_RATE observation rows=%+v fetches=%d", rows, connector.fetchCallCount())
	}

	// A reconnect invalidates runtime entitlement/success reuse, but cannot
	// defeat IBKR's persisted identical-wire boundary.
	at = start.Add(5 * time.Second)
	connector.rejectNextSessionCheck()
	rows, _ = cache.heldShortFeeRateCoverage(t.Context(), []string{"XYZ"}, connector, scope, at, failedBorrowFeeHealth(at))
	if len(rows) != 1 || rows[0].Reason != "restart_cooldown_retained_last_good" || connector.fetchCallCount() != 1 {
		t.Fatalf("reconnect inside wire boundary rows=%+v fetches=%d", rows, connector.fetchCallCount())
	}

	at = start.Add(16 * time.Second)
	connector.rejectNextSessionCheck()
	rows, _ = cache.heldShortFeeRateCoverage(t.Context(), []string{"XYZ"}, connector, scope, at, failedBorrowFeeHealth(at))
	if len(rows) != 1 || connector.fetchCallCount() != 2 {
		t.Fatalf("reconnect reused pre-session success after boundary rows=%+v fetches=%d", rows, connector.fetchCallCount())
	}
}

func TestHeldShortFeeRateQueriesOnlyRelevantSupportedExactShorts(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }

	t.Run("no short stock", func(t *testing.T) {
		cache := newMarketEventCache(func() time.Time { return now })
		longStock := feeRateTestPosition("DU123", 1, "LONG", 10, "SMART")
		shortOption := feeRateTestPosition("DU123", 2, "OPT", -1, "SMART")
		shortOption.Contract.SecType = "OPT"
		cache.readCachedPositions = feeRateCurrentPositionsReader(now, longStock, shortOption)
		fetchCalls := 0
		cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
			fetchCalls++
			return nil, nil
		}
		rows, health := cache.borrowFeeCoverage(t.Context(), []string{"LONG", "OPT"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
		if fetchCalls != 0 || len(rows) != 0 || health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue || health.LastFailure != nil || health.NextAttempt != nil || len(health.Notes) != 1 || !strings.Contains(health.Notes[0], "not_applicable") {
			t.Fatalf("irrelevant positions triggered or degraded fallback calls=%d rows=%+v health=%+v", fetchCalls, rows, health)
		}
	})

	t.Run("missing ConID and non-US are typed gaps", func(t *testing.T) {
		cache := newMarketEventCache(func() time.Time { return now })
		missing := feeRateTestPosition("DU123", 0, "MISSING", -10, "")
		nonUS := feeRateTestPosition("DU123", 7, "EURO", -10, "IBIS")
		nonUS.Contract.PrimaryExch = "IBIS"
		nonUS.Contract.Currency = "EUR"
		cache.readCachedPositions = feeRateCurrentPositionsReader(now, missing, nonUS)
		fetchCalls := 0
		cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
			fetchCalls++
			return nil, nil
		}
		rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"MISSING", "EURO"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
		reasons := map[string]bool{}
		for _, row := range rows {
			reasons[row.Reason] = true
		}
		if fetchCalls != 0 || len(rows) != 2 || !reasons["missing_contract_id"] || !reasons["unsupported_market_calendar"] {
			t.Fatalf("unsupported exact gaps calls=%d rows=%+v", fetchCalls, rows)
		}
	})
}

func TestHeldShortFeeRateRouteResolutionRejectsWrongIdentityAndScopeChange(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		resolve    func(ibkrlib.Contract) (ibkrlib.Contract, error)
		drift      bool
		wantReason string
	}{
		{name: "wrong conID", resolve: func(contract ibkrlib.Contract) (ibkrlib.Contract, error) {
			contract.ConID++
			contract.Exchange = "NASDAQ"
			return contract, nil
		}, wantReason: rpc.SourceFailureContractUnavailable},
		{name: "no details", resolve: func(ibkrlib.Contract) (ibkrlib.Contract, error) {
			return ibkrlib.Contract{}, &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureContractUnavailable}
		}, wantReason: rpc.SourceFailureContractUnavailable},
		{name: "scope changes", resolve: func(contract ibkrlib.Contract) (ibkrlib.Contract, error) {
			contract.Exchange = "NASDAQ"
			return contract, nil
		}, drift: true, wantReason: "scope_changed_during_route_resolution"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := newMarketEventCache(func() time.Time { return now })
			cache.readCachedPositions = feeRateCurrentPositionsReader(now, feeRateTestPosition("DU123", 481516, "XYZ", -10, ""))
			drifted := false
			cache.resolveHistoricalFeeRoute = func(_ context.Context, contract ibkrlib.Contract, _ time.Duration) (ibkrlib.Contract, error) {
				route, err := test.resolve(contract)
				if test.drift {
					drifted = true
				}
				return route, err
			}
			fetchCalls := 0
			cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
				fetchCalls++
				return nil, nil
			}
			scope := func() brokerStateScope {
				if drifted {
					return brokerStateScope{Account: "DU999", Mode: rpc.AccountModePaper}
				}
				return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
			}
			rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
			if fetchCalls != 0 || len(rows) != 1 || rows[0].Reason != test.wantReason || len(cache.borrowFeeFallback.LastGood) != 0 {
				t.Fatalf("calls=%d rows=%+v state=%+v", fetchCalls, rows, cache.borrowFeeFallback)
			}
		})
	}
}

func TestHeldShortFeeRateTypedFailuresNeverBecomeZeroOrClear(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		category    string
		wantStatus  string
		entitlement string
	}{
		{"not entitled", ibkrlib.HistoricalFailureNotEntitled, rpc.BorrowFeeCoverageNotEntitled, rpc.BorrowFeeEntitlementNotEntitled},
		{"no data", ibkrlib.HistoricalFailureNoData, rpc.BorrowFeeCoverageMissing, rpc.BorrowFeeEntitlementUnknown},
		{"paced", ibkrlib.HistoricalFailurePacing, rpc.BorrowFeeCoverageUnavailable, rpc.BorrowFeeEntitlementUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := newMarketEventCache(func() time.Time { return now })
			cache.readCachedPositions = feeRateCurrentPositionsReader(now, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
			cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
				return nil, &ibkrlib.HistoricalRequestError{Category: test.category, Message: "SECRET"}
			}
			rows, health := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, func() brokerStateScope {
				return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
			}, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
			if len(rows) != 1 || rows[0].Status != test.wantStatus || rows[0].Entitlement != test.entitlement || rows[0].FeeRate != nil || rows[0].PolicyEligible || rows[0].LastFailure == nil || strings.Contains(rows[0].Reason, "SECRET") {
				t.Fatalf("typed failure row = %+v", rows)
			}
			if health.Status != rpc.SourceStatusUnknown {
				t.Fatalf("typed current failure health = %+v", health)
			}
		})
	}
}

func TestHeldShortFeeRateRejectsDuplicateSessionsAndTypesEmptyCompletion(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	date := strings.ReplaceAll(feeRateCompletedSessionDate(t, now), "-", "")
	for _, test := range []struct {
		name     string
		bars     []ibkrlib.HistoricalBar
		wantCode string
	}{
		{name: "duplicate session", bars: []ibkrlib.HistoricalBar{
			{Date: date, Open: 1, High: 2, Low: 1, Close: 1.5},
			{Date: date, Open: 1, High: 2, Low: 1, Close: 1.6},
		}, wantCode: rpc.SourceFailureInvalidPayload},
		{name: "empty completion", wantCode: rpc.SourceFailureNoData},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := newMarketEventCache(func() time.Time { return now })
			cache.readCachedPositions = feeRateCurrentPositionsReader(now, feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART"))
			cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
				return test.bars, nil
			}
			rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, func() brokerStateScope {
				return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
			}, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
			if len(rows) != 1 || rows[0].FeeRate != nil || rows[0].LastFailure == nil || rows[0].LastFailure.Code != test.wantCode || rows[0].PolicyEligible {
				t.Fatalf("invalid completed payload rows=%+v", rows)
			}
		})
	}
}

func TestHeldShortFeeRateFinalRecheckRejectsRelevanceAndSessionRaces(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	date := strings.ReplaceAll(feeRateCompletedSessionDate(t, now), "-", "")
	scope := func() brokerStateScope { return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper} }
	position := feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART")
	bars := []ibkrlib.HistoricalBar{{Date: date, Open: 1, High: 2, Low: 1, Close: 1.5}}

	t.Run("held-short relevance changes before persist", func(t *testing.T) {
		authority := openMarketTestCoreStore(t)
		cache := newMarketEventCache(func() time.Time { return now })
		if err := cache.UseCoreStore(authority); err != nil {
			t.Fatal(err)
		}
		reads := 0
		cache.readCachedPositions = func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error) {
			reads++
			positions := []*ibkrlib.RawPosition{position}
			if reads > 1 {
				positions = nil
			}
			return positions, feeRatePortfolioHealth(now), nil
		}
		cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
			return bars, nil
		}
		rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
		if len(rows) != 1 || rows[0].Reason != "scope_changed_before_authority_persist" || len(cache.borrowFeeFallback.LastGood) != 0 {
			t.Fatalf("relevance race rows=%+v state=%+v", rows, cache.borrowFeeFallback)
		}
		if _, ok, err := authority.GetStateDocument(t.Context(), marketEventFeeRateScope, marketEventFeeRateStateKind); err != nil || ok {
			t.Fatalf("relevance-raced evidence persisted ok=%v err=%v", ok, err)
		}
	})

	t.Run("connector session changes immediately after persist", func(t *testing.T) {
		authority := openMarketTestCoreStore(t)
		cache := newMarketEventCache(func() time.Time { return now })
		if err := cache.UseCoreStore(authority); err != nil {
			t.Fatal(err)
		}
		connector := &feeRateTestHistoricalConnector{
			positions: []*ibkrlib.RawPosition{position}, health: feeRatePortfolioHealth(now), bars: bars,
			failCurrentAt: 5,
		}
		rows, _ := cache.heldShortFeeRateCoverage(t.Context(), []string{"XYZ"}, connector, scope, now, failedBorrowFeeHealth(now))
		if len(rows) != 1 || rows[0].Reason != "scope_changed_after_authority_persist" {
			t.Fatalf("post-persist connector race rows=%+v", rows)
		}
		if len(cache.borrowFeeFallback.LastGood) != 1 || cache.borrowFeeFallbackRevision != 1 {
			t.Fatalf("authority head not tracked after post-CAS race: state=%+v revision=%d", cache.borrowFeeFallback, cache.borrowFeeFallbackRevision)
		}
		for key := range cache.borrowFeeFallback.LastGood {
			if _, current := cache.borrowFeeFallbackCurrent[key]; current {
				t.Fatalf("post-race evidence published as current for %s", key)
			}
		}
	})
}

func TestHeldShortFeeRateRejectsForeignRowsAndScopeDriftBeforePublication(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		positions  []*ibkrlib.RawPosition
		drift      bool
		wantReason string
		wantFetch  int
	}{
		{"foreign row", []*ibkrlib.RawPosition{feeRateTestPosition("DU999", 481516, "XYZ", -10, "SMART")}, false, "portfolio_scope_conflict", 0},
		{"scope drift", []*ibkrlib.RawPosition{feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART")}, true, "scope_changed_during_fetch", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := newMarketEventCache(func() time.Time { return now })
			cache.readCachedPositions = feeRateCurrentPositionsReader(now, test.positions...)
			fetchCalls := 0
			cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
				fetchCalls++
				date := feeRateCompletedSessionDate(t, now)
				return []ibkrlib.HistoricalBar{{Date: strings.ReplaceAll(date, "-", ""), Open: 1, High: 1, Low: 1, Close: 1}}, nil
			}
			scopeCalls := 0
			scope := func() brokerStateScope {
				scopeCalls++
				if test.drift && scopeCalls >= 4 {
					return brokerStateScope{Account: "DU999", Mode: rpc.AccountModePaper}
				}
				return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
			}
			rows, _ := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, scope, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
			if fetchCalls != test.wantFetch || len(rows) != 1 || rows[0].Reason != test.wantReason || rows[0].PolicyEligible {
				t.Fatalf("calls=%d scopeCalls=%d rows=%+v", fetchCalls, scopeCalls, rows)
			}
			if len(cache.borrowFeeFallback.LastGood) != 0 || len(cache.borrowFeeFallback.LastAttempts) != 0 {
				t.Fatalf("rejected evidence published: %+v", cache.borrowFeeFallback)
			}
		})
	}
}

func TestHeldShortFeeRateRejectsStalePortfolioReceiptWithoutFetch(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.readCachedPositions = func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error) {
		return []*ibkrlib.RawPosition{feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART")}, ibkrlib.PortfolioStreamHealth{
			Account: "DU123", InitialCompletedAt: now.Add(-portfolioStreamReceiptMaxAge - time.Second),
		}, nil
	}
	fetchCalls := 0
	cache.fetchHistoricalFeeRates = func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
		fetchCalls++
		return nil, nil
	}
	rows, health := cache.borrowFeeCoverage(t.Context(), []string{"XYZ"}, nil, func() brokerStateScope {
		return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	}, now, marketEventBorrowFeeEntry{}, failedBorrowFeeHealth(now))
	if fetchCalls != 0 || len(rows) != 1 || rows[0].Reason != "portfolio_stream_stale" || rows[0].PolicyEligible || health.Status != rpc.SourceStatusUnknown {
		t.Fatalf("stale receipt calls=%d rows=%+v health=%+v", fetchCalls, rows, health)
	}
}

func TestMarketEventFeeRateStateRejectsTamperAndUnrelatedFailureSemantics(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	state := feeRateValidState(t, now)
	if err := validateMarketEventFeeRateState(state, now); err != nil {
		t.Fatalf("valid state: %v", err)
	}
	key := onlyFeeRateStateKey(t, state)

	tests := []struct {
		name   string
		mutate func(*marketEventFeeRateState)
	}{
		{"future record", func(s *marketEventFeeRateState) {
			row := s.LastGood[key]
			row.ObservedAt = now.Add(time.Second)
			s.LastGood[key] = row
			attempt := s.LastAttempts[key]
			attempt.CompletedAt = row.ObservedAt
			s.LastAttempts[key] = attempt
		}},
		{"wrong session close", func(s *marketEventFeeRateState) {
			row := s.LastGood[key]
			row.AsOf = row.AsOf.Add(time.Minute)
			s.LastGood[key] = row
		}},
		{"record without attempt", func(s *marketEventFeeRateState) { delete(s.LastAttempts, key) }},
		{"unrelated failure stage", func(s *marketEventFeeRateState) {
			at := now
			next := now.Add(time.Minute)
			s.LastAttempts[key] = marketEventFeeRateAttempt{Outcome: marketEventFeeRateOutcomeFailure, AttemptedAt: at, CompletedAt: at, NextAttempt: &next, Failure: &rpc.SourceFailure{Code: rpc.SourceFailureTimeout, Stage: rpc.SourceFailureStageFTPControlConnect, FailedAt: at, Retryable: true}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := cloneMarketEventFeeRateState(state)
			test.mutate(&changed)
			if err := validateMarketEventFeeRateState(changed, now); err == nil {
				t.Fatal("tampered state accepted")
			}
		})
	}
}

func TestUseCoreStoreRejectsUnknownFeeRateAuthorityFields(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	if _, err := authority.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: marketEventFeeRateScope, Kind: marketEventFeeRateStateKind,
		JSON: []byte(`{"version":3,"last_good":{},"last_attempts":{},"unexpected":true}`),
	}); err != nil {
		t.Fatalf("seed malformed authority: %v", err)
	}
	cache := newMarketEventCache(time.Now)
	if err := cache.UseCoreStore(authority); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("malformed authority accepted: %v", err)
	}
}

func TestPersistMarketEventFeeRateStateFailsClosedOnCASConflict(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	state := feeRateValidState(t, now)
	key := onlyFeeRateStateKey(t, state)
	record := state.LastGood[key]
	attempt := state.LastAttempts[key]
	observation := marketEventFeeRateObservation{StateKey: key, Attempt: attempt, Record: &record}
	if revision, err := cache.persistMarketEventFeeRateState(t.Context(), state, 0, []marketEventFeeRateObservation{observation}); err != nil || revision != 1 {
		t.Fatalf("initial persist revision=%d err=%v", revision, err)
	}
	// The direct persistence seam intentionally did not advance the cache's
	// in-memory revision. A second write with the old head must fail rather
	// than overwriting the newer authority or publishing unpersisted data.
	if _, err := cache.persistMarketEventFeeRateState(t.Context(), state, 0, []marketEventFeeRateObservation{observation}); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale CAS error = %v, want revision conflict", err)
	}
	if len(cache.borrowFeeFallback.LastGood) != 0 || len(cache.borrowFeeFallback.LastAttempts) != 0 {
		t.Fatalf("direct failed CAS mutated in-memory fallback: %+v", cache.borrowFeeFallback)
	}
}

func feeRateTestPosition(account string, conID int, symbol string, quantity float64, exchange string) *ibkrlib.RawPosition {
	return &ibkrlib.RawPosition{
		Account: account, Position: quantity,
		Contract: ibkrlib.Contract{ConID: conID, Symbol: symbol, SecType: "STK", Multiplier: 1, Exchange: exchange, PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: symbol, TradingClass: "NMS"},
	}
}

func feeRateCurrentPositionsReader(now time.Time, positions ...*ibkrlib.RawPosition) func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error) {
	return func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error) {
		return positions, feeRatePortfolioHealth(now), nil
	}
}

func feeRatePortfolioHealth(now time.Time) ibkrlib.PortfolioStreamHealth {
	return ibkrlib.PortfolioStreamHealth{
		Account:            "DU123",
		RequestedAt:        now.Add(-2 * time.Minute),
		InitialCompletedAt: now.Add(-time.Minute),
		LastUpdateAt:       now.Add(-time.Second),
	}
}

// feeRateTestHistoricalConnector is the narrow connector seam exercised by
// the fallback race tests. It deliberately owns only FEE_RATE-facing state so
// the tests do not couple to the production connector's portfolio internals.
type feeRateTestHistoricalConnector struct {
	mu sync.Mutex

	positions []*ibkrlib.RawPosition
	health    ibkrlib.PortfolioStreamHealth
	bars      []ibkrlib.HistoricalBar

	currentCalls  int
	failCurrentAt int
	rejectNext    bool
	fetchCalls    int
}

func (c *feeRateTestHistoricalConnector) CaptureHistoricalSession() (ibkrlib.HistoricalSessionBinding, bool) {
	return ibkrlib.HistoricalSessionBinding{}, true
}

func (c *feeRateTestHistoricalConnector) HistoricalSessionCurrent(ibkrlib.HistoricalSessionBinding) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentCalls++
	if c.rejectNext {
		c.rejectNext = false
		return false
	}
	return c.failCurrentAt == 0 || c.currentCalls < c.failCurrentAt
}

func (c *feeRateTestHistoricalConnector) CachedPositionsWithHealth() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*ibkrlib.RawPosition(nil), c.positions...), c.health, nil
}

func (c *feeRateTestHistoricalConnector) ResolveExactHistoricalStockRoute(_ context.Context, contract ibkrlib.Contract, _ time.Duration) (ibkrlib.Contract, error) {
	if contract.Exchange == "" {
		contract.Exchange = contract.PrimaryExch
	}
	return contract, nil
}

func (c *feeRateTestHistoricalConnector) FetchHistoricalDailyFeeRates(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetchCalls++
	return append([]ibkrlib.HistoricalBar(nil), c.bars...), nil
}

func (c *feeRateTestHistoricalConnector) rejectNextSessionCheck() {
	c.mu.Lock()
	c.rejectNext = true
	c.mu.Unlock()
}

func (c *feeRateTestHistoricalConnector) fetchCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchCalls
}

func feeRateCompletedSessionDate(t *testing.T, now time.Time) string {
	t.Helper()
	date, _, ok := lastCompletedMarketSession(now, marketcal.MarketUSEquity)
	if !ok {
		t.Fatalf("no completed session at %s", now)
	}
	return date
}

func failedBorrowFeeHealth(now time.Time) rpc.SourceHealth {
	return rpc.SourceHealth{
		Source: "borrow_fee", Status: rpc.SourceStatusStale, RefreshState: rpc.SourceRefreshFetchFailed,
		AsOf: now.Add(-24 * time.Hour), LastFailure: &rpc.SourceFailure{
			Code: rpc.SourceFailureTimeout, Stage: rpc.SourceFailureStageFTPControlConnect,
			FailedAt: now, Retryable: true,
		},
	}
}

func feeRateValidState(t *testing.T, now time.Time) marketEventFeeRateState {
	t.Helper()
	contract := normalizeFeeRateContract(feeRateTestPosition("DU123", 481516, "XYZ", -10, "SMART").Contract)
	scopeFingerprint := marketEventFeeRateScopeFingerprint(brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper})
	contractFingerprint := marketEventFeeRateContractFingerprint(contract)
	identityFingerprint := marketEventFeeRateIdentityFingerprint(contract)
	key := scopeFingerprint + ":" + identityFingerprint
	date := feeRateCompletedSessionDate(t, now)
	record := marketEventFeeRateRecord{
		ScopeFingerprint: scopeFingerprint, IdentityFingerprint: identityFingerprint, ContractFingerprint: contractFingerprint,
		Contract: feeRateStoredContract(contract), SessionDate: date,
		AsOf: marketCloseForDate(marketcal.MarketUSEquity, date, now).UTC(), ObservedAt: now,
		FeeRate: 81, ScaleStatus: rpc.BorrowFeeScaleUnverified,
	}
	next := now.Add(15 * time.Second)
	return marketEventFeeRateState{
		Version:  marketEventFeeRateStateVersion,
		LastGood: map[string]marketEventFeeRateRecord{key: record},
		LastAttempts: map[string]marketEventFeeRateAttempt{key: {
			Outcome: marketEventFeeRateOutcomeSuccess, AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
		}},
	}
}

func onlyFeeRateStateKey(t *testing.T, state marketEventFeeRateState) string {
	t.Helper()
	for key := range state.LastGood {
		return key
	}
	t.Fatal("state has no last-good key")
	return ""
}

func TestFeeRateObservationPayloadStrictlyExcludesRawAccountAndProse(t *testing.T) {
	state := feeRateValidState(t, time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC))
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"DU123", "SECRET", "account"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("state contains forbidden raw token %q: %s", forbidden, payload)
		}
	}
}
