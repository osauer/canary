package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	marketEventFeeRateLookbackDays = 10
	marketEventFeeRateTimeout      = 20 * time.Second
	marketEventFeeRateRetryAfter   = 15 * time.Minute
	marketEventFeeRateWireBoundary = 15 * time.Second
)

// borrowFeeFTPPolicyUsable is deliberately strict. A stale last-good returned with
// a nil error after a failed/backed-off refresh is context, not a current
// negative and cannot keep the held-short fallback suppressed.
func borrowFeeFTPPolicyUsable(health rpc.SourceHealth) bool {
	if health.Status != rpc.SourceStatusOK {
		return false
	}
	return health.RefreshState == rpc.SourceRefreshCurrent || health.RefreshState == rpc.SourceRefreshNotDue
}

func borrowFeeFTPSuppressesFallback(health rpc.SourceHealth) bool {
	return health.RefreshState == rpc.SourceRefreshNotDue || borrowFeeFTPPolicyUsable(health)
}

func (c *marketEventCache) borrowFeeCoverage(
	ctx context.Context,
	symbols []string,
	connector *ibkrlib.Connector,
	scopeProvider func() brokerStateScope,
	now time.Time,
	bulk marketEventBorrowFeeEntry,
	bulkHealth rpc.SourceHealth,
) ([]rpc.MarketEventBorrowFeeCoverage, rpc.SourceHealth) {
	if borrowFeeFTPSuppressesFallback(bulkHealth) {
		return bulkBorrowFeeCoverage(symbols, bulk, bulkHealth)
	}
	var historical feeRateHistoricalConnector
	if connector != nil {
		historical = connector
	}
	return c.heldShortFeeRateCoverage(ctx, symbols, historical, scopeProvider, now, bulkHealth)
}

func bulkBorrowFeeCoverage(symbols []string, bulk marketEventBorrowFeeEntry, health rpc.SourceHealth) ([]rpc.MarketEventBorrowFeeCoverage, rpc.SourceHealth) {
	policyUsable := borrowFeeFTPPolicyUsable(health)
	rows := make([]rpc.MarketEventBorrowFeeCoverage, 0, len(symbols))
	observed := 0
	for _, symbol := range symbols {
		row := rpc.MarketEventBorrowFeeCoverage{
			Symbol:         symbol,
			CoverageScope:  rpc.BorrowFeeCoverageGlobal,
			Status:         rpc.BorrowFeeCoverageMissing,
			Reason:         "bulk_record_missing",
			Source:         rpc.BorrowFeeSourceBulkShortStock,
			DataType:       rpc.BorrowFeeDataTypeBulkFeeRate,
			AsOf:           bulk.AsOf.UTC(),
			ObservedAt:     bulk.FetchedAt.UTC(),
			Entitlement:    rpc.BorrowFeeEntitlementUnknown,
			ScaleStatus:    rpc.BorrowFeeScalePercentAnnualized,
			PolicyEligible: false,
		}
		if record, ok := bulk.Symbols[symbol]; ok {
			feeRate := record.FeeRate
			row.Status = rpc.BorrowFeeCoverageObserved
			row.Reason = "bulk_record_observed"
			row.FeeRate = &feeRate
			row.Entitlement = rpc.BorrowFeeEntitlementObserved
			row.PolicyEligible = policyUsable
			if !policyUsable {
				row.Status = rpc.BorrowFeeCoverageStale
				row.Reason = "bulk_record_not_current"
			}
			observed++
		}
		rows = append(rows, row)
	}
	switch {
	case observed == len(rows):
		// Preserve the typed current/not-due source health.
	case observed > 0:
		health.Status = rpc.SourceStatusPartial
		health.Confidence = "medium-low"
		health.Notes = append(health.Notes, "bulk borrow-fee coverage is incomplete for requested symbols")
	default:
		health.Status = rpc.SourceStatusUnknown
		health.Confidence = "low"
		health.Notes = append(health.Notes, "bulk borrow-fee coverage has no requested-symbol rows")
	}
	return rows, health
}

type marketEventFeeRateContract struct {
	ConID        int     `json:"conid"`
	Symbol       string  `json:"symbol"`
	SecType      string  `json:"sec_type"`
	Expiry       string  `json:"expiry,omitempty"`
	Strike       float64 `json:"strike,omitempty"`
	Right        string  `json:"right,omitempty"`
	Multiplier   int     `json:"multiplier,omitempty"`
	Exchange     string  `json:"exchange"`
	PrimaryExch  string  `json:"primary_exchange,omitempty"`
	Currency     string  `json:"currency"`
	LocalSymbol  string  `json:"local_symbol,omitempty"`
	TradingClass string  `json:"trading_class,omitempty"`
	SecIDType    string  `json:"sec_id_type,omitempty"`
	SecID        string  `json:"sec_id,omitempty"`
}

type marketEventFeeRateRecord struct {
	ScopeFingerprint    string                     `json:"scope_fingerprint"`
	IdentityFingerprint string                     `json:"identity_fingerprint"`
	ContractFingerprint string                     `json:"contract_fingerprint"`
	Contract            marketEventFeeRateContract `json:"contract"`
	SessionDate         string                     `json:"session_date"`
	AsOf                time.Time                  `json:"as_of"`
	ObservedAt          time.Time                  `json:"observed_at"`
	FeeRate             float64                    `json:"fee_rate"`
	ScaleStatus         string                     `json:"scale_status"`
}

type marketEventFeeRateAttempt struct {
	Outcome            string             `json:"outcome"`
	AttemptedAt        time.Time          `json:"attempted_at"`
	CompletedAt        time.Time          `json:"completed_at"`
	NextAttempt        *time.Time         `json:"next_attempt,omitempty"`
	RuntimeNextAttempt *time.Time         `json:"-"`
	Failure            *rpc.SourceFailure `json:"-"`
}

type marketEventFeeRateState struct {
	Version      int                                  `json:"version"`
	LastGood     map[string]marketEventFeeRateRecord  `json:"last_good"`
	LastAttempts map[string]marketEventFeeRateAttempt `json:"last_attempts"`
}

const (
	marketEventFeeRateStateVersion   = 3
	marketEventFeeRateOutcomeSuccess = "success"
	marketEventFeeRateOutcomeFailure = "failure"
)

type marketEventFeeRateTarget struct {
	contract            ibkrlib.Contract
	identityFingerprint string
	contractFingerprint string
	stateKey            string
}

type marketEventFeeRateFetchResult struct {
	target marketEventFeeRateTarget
	bars   []ibkrlib.HistoricalBar
	err    error
}

type feeRateHistoricalConnector interface {
	CaptureHistoricalSession() (ibkrlib.HistoricalSessionBinding, bool)
	HistoricalSessionCurrent(ibkrlib.HistoricalSessionBinding) bool
	CachedPositionsWithHealth() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error)
	ResolveExactHistoricalStockRoute(context.Context, ibkrlib.Contract, time.Duration) (ibkrlib.Contract, error)
	FetchHistoricalDailyFeeRates(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error)
}

func (c *marketEventCache) heldShortFeeRateCoverage(
	ctx context.Context,
	symbols []string,
	connector feeRateHistoricalConnector,
	scopeProvider func() brokerStateScope,
	now time.Time,
	primaryHealth rpc.SourceHealth,
) ([]rpc.MarketEventBorrowFeeCoverage, rpc.SourceHealth) {
	c.borrowFeeFallbackMu.Lock()
	defer c.borrowFeeFallbackMu.Unlock()

	now = now.UTC()
	if scopeProvider == nil || (connector == nil && c.readCachedPositions == nil) {
		return feeRateGapResult(primaryHealth, symbols, "scope_unavailable", now)
	}
	expectedScope := scopeProvider()
	if !brokerScopeConcrete(expectedScope) {
		return feeRateGapResult(primaryHealth, symbols, "scope_unavailable", now)
	}
	var expectedSession ibkrlib.HistoricalSessionBinding
	if connector != nil {
		var ok bool
		expectedSession, ok = connector.CaptureHistoricalSession()
		if !ok {
			return feeRateGapResult(primaryHealth, symbols, "gateway_unavailable", now)
		}
	}
	readPositions := c.readCachedPositions
	if readPositions == nil {
		readPositions = connector.CachedPositionsWithHealth
	}
	raw, receipt, err := readPositions()
	if err != nil || !sameBrokerScope(expectedScope, scopeProvider()) {
		return feeRateGapResult(primaryHealth, symbols, "portfolio_stream_unavailable", now)
	}
	health := classifyPortfolioStreamHealth(expectedScope, receipt, now)
	if health != orderIntegrityHealthCurrent {
		reason := "portfolio_stream_unavailable"
		if health == orderIntegrityHealthStale {
			reason = "portfolio_stream_stale"
		}
		return feeRateGapResult(primaryHealth, symbols, reason, now)
	}
	if !cachedPositionsMatchBrokerScope(raw, expectedScope) {
		return feeRateGapResult(primaryHealth, symbols, "portfolio_scope_conflict", now)
	}

	scopeFingerprint := marketEventFeeRateScopeFingerprint(expectedScope)
	relevanceFingerprint := marketEventFeeRateRelevanceFingerprint(raw, symbols)
	targets, gaps := exactHeldShortFeeRateTargets(raw, symbols, scopeFingerprint)
	state := cloneMarketEventFeeRateState(c.borrowFeeFallback)
	if state.Version == 0 {
		state = newMarketEventFeeRateState()
	}

	rows := append([]rpc.MarketEventBorrowFeeCoverage(nil), gaps...)
	routeTargets := make([]marketEventFeeRateTarget, 0, len(targets))
	for _, target := range targets {
		if row, ok := c.reusableFeeRateRow(state, target, now, connector, expectedSession); ok {
			rows = append(rows, row)
			continue
		}
		routeTargets = append(routeTargets, target)
	}
	resolvedTargets, routeFailures, routeGaps := c.resolveFeeRateTargetRoutes(ctx, connector, routeTargets, scopeFingerprint)
	rows = append(rows, routeGaps...)
	if ctx.Err() != nil || !sameBrokerScope(expectedScope, scopeProvider()) || !feeRateSessionCurrent(connector, expectedSession) {
		return feeRateGapResult(primaryHealth, symbols, "scope_changed_during_route_resolution", c.now().UTC())
	}

	fetch := c.fetchHistoricalFeeRates
	if len(resolvedTargets) > 0 && fetch == nil {
		if connector == nil {
			return feeRateGapResult(primaryHealth, symbols, "gateway_unavailable", now)
		}
		fetch = connector.FetchHistoricalDailyFeeRates
	}
	results := make([]marketEventFeeRateFetchResult, len(resolvedTargets))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 2)
	for i, target := range resolvedTargets {
		wg.Go(func() {
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				results[i] = marketEventFeeRateFetchResult{target: target, err: ctx.Err()}
				return
			}
			bars, fetchErr := fetch(ctx, target.contract, marketEventFeeRateLookbackDays, marketEventFeeRateTimeout)
			results[i] = marketEventFeeRateFetchResult{target: target, bars: bars, err: fetchErr}
		})
	}
	wg.Wait()
	results = append(results, routeFailures...)

	if ctx.Err() != nil || !sameBrokerScope(expectedScope, scopeProvider()) || !feeRateSessionCurrent(connector, expectedSession) {
		return feeRateGapResult(primaryHealth, symbols, "scope_changed_during_fetch", now)
	}

	completedAt := c.now().UTC()
	if completedAt.Before(now) {
		completedAt = now
	}
	observations := make([]marketEventFeeRateObservation, 0, len(results))
	for _, result := range results {
		attemptedAt := now
		if result.err != nil {
			failure := feeRateSourceFailure(result.err, completedAt)
			wireNext := completedAt.Add(marketEventFeeRateWireBoundary)
			runtimeNext := completedAt.Add(marketEventFeeRateRetryAfter)
			if requestErr, ok := errors.AsType[*ibkrlib.HistoricalRequestError](result.err); ok && requestErr.RetryAfter > marketEventFeeRateRetryAfter {
				runtimeNext = completedAt.Add(requestErr.RetryAfter)
			}
			attempt := marketEventFeeRateAttempt{
				Outcome: marketEventFeeRateOutcomeFailure, AttemptedAt: attemptedAt,
				CompletedAt: completedAt, NextAttempt: &wireNext, RuntimeNextAttempt: &runtimeNext, Failure: &failure,
			}
			state.LastAttempts[result.target.stateKey] = attempt
			observations = append(observations, marketEventFeeRateObservation{StateKey: result.target.stateKey, Attempt: attempt})
			rows = append(rows, feeRateFailureRow(result.target, state.LastGood[result.target.stateKey], attempt))
			continue
		}
		record, selectErr := selectCompletedFeeRateRecord(result.target, scopeFingerprint, result.bars, completedAt)
		if selectErr != nil {
			failure := feeRateSourceFailure(selectErr, completedAt)
			wireNext := completedAt.Add(marketEventFeeRateWireBoundary)
			runtimeNext := completedAt.Add(marketEventFeeRateRetryAfter)
			attempt := marketEventFeeRateAttempt{Outcome: marketEventFeeRateOutcomeFailure, AttemptedAt: attemptedAt, CompletedAt: completedAt, NextAttempt: &wireNext, RuntimeNextAttempt: &runtimeNext, Failure: &failure}
			state.LastAttempts[result.target.stateKey] = attempt
			observations = append(observations, marketEventFeeRateObservation{StateKey: result.target.stateKey, Attempt: attempt})
			rows = append(rows, feeRateFailureRow(result.target, state.LastGood[result.target.stateKey], attempt))
			continue
		}
		completedSession, _, _ := lastCompletedMarketSession(completedAt, marketcal.MarketUSEquity)
		if record.SessionDate != completedSession {
			// A valid older bar is useful retained context, but it is not a
			// successful observation of the latest completed publication. Keep
			// only the 15-second identical-wire boundary durable; the longer
			// publication retry is runtime/session-local.
			failure := feeRateSourceFailure(&ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureNoData}, completedAt)
			wireNext := completedAt.Add(marketEventFeeRateWireBoundary)
			runtimeNext := completedAt.Add(marketEventFeeRateRetryAfter)
			attempt := marketEventFeeRateAttempt{
				Outcome: marketEventFeeRateOutcomeFailure, AttemptedAt: attemptedAt,
				CompletedAt: completedAt, NextAttempt: &wireNext, RuntimeNextAttempt: &runtimeNext, Failure: &failure,
			}
			retained, retainedOK := state.LastGood[result.target.stateKey]
			var observedRecord *marketEventFeeRateRecord
			if !retainedOK || retained.IdentityFingerprint != result.target.identityFingerprint || record.SessionDate >= retained.SessionDate {
				state.LastGood[result.target.stateKey] = record
				retained = record
				observedRecord = &record
			}
			state.LastAttempts[result.target.stateKey] = attempt
			observations = append(observations, marketEventFeeRateObservation{StateKey: result.target.stateKey, Attempt: attempt, Record: observedRecord})
			row := feeRateFailureRow(result.target, retained, attempt)
			row.Reason = "completed_session_bar_stale_no_new_data"
			row.Entitlement = rpc.BorrowFeeEntitlementObserved
			rows = append(rows, row)
			continue
		}
		next := completedAt.Add(marketEventFeeRateWireBoundary)
		attempt := marketEventFeeRateAttempt{Outcome: marketEventFeeRateOutcomeSuccess, AttemptedAt: attemptedAt, CompletedAt: completedAt, NextAttempt: &next}
		state.LastGood[result.target.stateKey] = record
		state.LastAttempts[result.target.stateKey] = attempt
		observations = append(observations, marketEventFeeRateObservation{StateKey: result.target.stateKey, Attempt: attempt, Record: &record})
		rows = append(rows, feeRateRecordRow(record, completedAt, completedAt))
	}

	if len(observations) > 0 {
		if !c.feeRateEvaluationStillCurrent(readPositions, expectedScope, scopeProvider, symbols, relevanceFingerprint, connector, expectedSession, c.now().UTC()) {
			return feeRateGapResult(primaryHealth, symbols, "scope_changed_before_authority_persist", c.now().UTC())
		}
		revision, persistErr := c.persistMarketEventFeeRateState(ctx, state, c.borrowFeeFallbackRevision, observations)
		if persistErr != nil {
			failure := &rpc.SourceFailure{Code: rpc.SourceFailureAuthorityWriteFailed, Stage: rpc.SourceFailureStageAuthorityPersist, FailedAt: now, Retryable: false}
			failedRows := feeRateSymbolGaps(symbols, "authority_write_failed")
			for i := range failedRows {
				failedRows[i].LastFailure = cloneBorrowFeeSourceFailure(failure)
			}
			return failedRows, feeRateAggregateHealth(primaryHealth, failedRows, now)
		}
		c.mu.Lock()
		c.borrowFeeFallback = cloneMarketEventFeeRateState(state)
		c.borrowFeeFallbackRevision = revision
		c.mu.Unlock()
		if !c.feeRateEvaluationStillCurrent(readPositions, expectedScope, scopeProvider, symbols, relevanceFingerprint, connector, expectedSession, c.now().UTC()) {
			return feeRateGapResult(primaryHealth, symbols, "scope_changed_after_authority_persist", c.now().UTC())
		}
		c.mu.Lock()
		if c.borrowFeeFallbackCurrent == nil {
			c.borrowFeeFallbackCurrent = map[string]ibkrlib.HistoricalSessionBinding{}
		}
		for _, observation := range observations {
			c.borrowFeeFallbackCurrent[observation.StateKey] = expectedSession
		}
		c.mu.Unlock()
	} else if !c.feeRateEvaluationStillCurrent(readPositions, expectedScope, scopeProvider, symbols, relevanceFingerprint, connector, expectedSession, c.now().UTC()) {
		return feeRateGapResult(primaryHealth, symbols, "scope_changed_before_return", c.now().UTC())
	}

	sortFeeRateCoverage(rows)
	return rows, feeRateAggregateHealth(primaryHealth, rows, completedAt)
}

func newMarketEventFeeRateState() marketEventFeeRateState {
	return marketEventFeeRateState{Version: marketEventFeeRateStateVersion, LastGood: map[string]marketEventFeeRateRecord{}, LastAttempts: map[string]marketEventFeeRateAttempt{}}
}

func exactHeldShortFeeRateTargets(raw []*ibkrlib.RawPosition, symbols []string, scopeFingerprint string) ([]marketEventFeeRateTarget, []rpc.MarketEventBorrowFeeCoverage) {
	wanted := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		wanted[symbol] = true
	}
	targets := []marketEventFeeRateTarget{}
	gaps := []rpc.MarketEventBorrowFeeCoverage{}
	seenTarget := map[string]bool{}
	seenGap := map[string]bool{}
	for _, position := range raw {
		if position == nil || position.Position >= 0 || !strings.EqualFold(position.Contract.SecType, "STK") {
			continue
		}
		contract := normalizeFeeRateContract(position.Contract)
		if !wanted[contract.Symbol] {
			continue
		}
		if contract.ConID <= 0 {
			gapKey := contract.Symbol + "\x00" + contract.LocalSymbol + "\x00" + contract.TradingClass
			if !seenGap[gapKey] {
				seenGap[gapKey] = true
				gaps = append(gaps, feeRateSymbolGap(contract.Symbol, "missing_contract_id"))
			}
			continue
		}
		identityFingerprint := marketEventFeeRateIdentityFingerprint(contract)
		identityKey := scopeFingerprint + ":" + identityFingerprint
		if seenTarget[identityKey] {
			continue
		}
		seenTarget[identityKey] = true
		if contract.Currency == "" {
			gaps = append(gaps, rpc.MarketEventBorrowFeeCoverage{
				Symbol: contract.Symbol, ContractConID: contract.ConID, ContractFingerprint: identityFingerprint,
				CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly, Status: rpc.BorrowFeeCoverageUnavailable,
				Reason: "incomplete_exact_route", Source: rpc.BorrowFeeSourceTWSHistorical,
				DataType: rpc.BorrowFeeDataTypeHistorical, Entitlement: rpc.BorrowFeeEntitlementUnknown,
				ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
			})
			continue
		}
		if !ibkrlib.HistoricalFeeRateUSRouteSupported(contract, contract.Exchange != "") {
			gaps = append(gaps, rpc.MarketEventBorrowFeeCoverage{
				Symbol: contract.Symbol, ContractConID: contract.ConID, ContractFingerprint: identityFingerprint,
				CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly, Status: rpc.BorrowFeeCoverageUnavailable,
				Reason: "unsupported_market_calendar", Source: rpc.BorrowFeeSourceTWSHistorical,
				DataType: rpc.BorrowFeeDataTypeHistorical, Entitlement: rpc.BorrowFeeEntitlementUnknown,
				ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
			})
			continue
		}
		target := marketEventFeeRateTarget{
			contract: contract, identityFingerprint: identityFingerprint, stateKey: identityKey,
		}
		if contract.Exchange != "" {
			target = bindFeeRateTargetRoute(target, scopeFingerprint, contract)
		}
		targets = append(targets, target)
	}
	slices.SortFunc(targets, func(a, b marketEventFeeRateTarget) int {
		if a.contract.Symbol != b.contract.Symbol {
			return strings.Compare(a.contract.Symbol, b.contract.Symbol)
		}
		return strings.Compare(a.contractFingerprint, b.contractFingerprint)
	})
	return targets, gaps
}

func bindFeeRateTargetRoute(target marketEventFeeRateTarget, scopeFingerprint string, routed ibkrlib.Contract) marketEventFeeRateTarget {
	target.contract = normalizeFeeRateContract(routed)
	target.contractFingerprint = marketEventFeeRateContractFingerprint(target.contract)
	if target.stateKey == "" {
		target.stateKey = scopeFingerprint + ":" + target.identityFingerprint
	}
	return target
}

func feeRateTargetFingerprint(target marketEventFeeRateTarget) string {
	if target.contractFingerprint != "" {
		return target.contractFingerprint
	}
	return target.identityFingerprint
}

func feeRateSessionCurrent(connector feeRateHistoricalConnector, binding ibkrlib.HistoricalSessionBinding) bool {
	return connector == nil || connector.HistoricalSessionCurrent(binding)
}

func marketEventFeeRateRelevanceFingerprint(raw []*ibkrlib.RawPosition, symbols []string) string {
	wanted := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		wanted[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}
	entries := make([]string, 0)
	for _, position := range raw {
		if position == nil || position.Position >= 0 || !strings.EqualFold(position.Contract.SecType, "STK") {
			continue
		}
		contract := normalizeFeeRateContract(position.Contract)
		if !wanted[contract.Symbol] {
			continue
		}
		if contract.ConID > 0 {
			entries = append(entries, marketEventFeeRateIdentityFingerprint(contract))
			continue
		}
		entries = append(entries, strings.Join([]string{
			"missing", contract.Symbol, contract.Currency, contract.LocalSymbol, contract.TradingClass,
			contract.SecIDType, contract.SecID,
		}, "\x00"))
	}
	slices.Sort(entries)
	digest := sha256.Sum256([]byte("market-event-fee-rate-relevance-v1\x00" + strings.Join(entries, "\x01")))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (c *marketEventCache) feeRateEvaluationStillCurrent(
	readPositions func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error),
	expectedScope brokerStateScope,
	scopeProvider func() brokerStateScope,
	symbols []string,
	expectedRelevance string,
	connector feeRateHistoricalConnector,
	binding ibkrlib.HistoricalSessionBinding,
	evaluateAt time.Time,
) bool {
	if scopeProvider == nil || !sameBrokerScope(expectedScope, scopeProvider()) || !feeRateSessionCurrent(connector, binding) {
		return false
	}
	raw, receipt, err := readPositions()
	if err != nil || classifyPortfolioStreamHealth(expectedScope, receipt, evaluateAt.UTC()) != orderIntegrityHealthCurrent ||
		!cachedPositionsMatchBrokerScope(raw, expectedScope) {
		return false
	}
	if !sameBrokerScope(expectedScope, scopeProvider()) || !feeRateSessionCurrent(connector, binding) {
		return false
	}
	return marketEventFeeRateRelevanceFingerprint(raw, symbols) == expectedRelevance
}

func (c *marketEventCache) resolveFeeRateTargetRoutes(ctx context.Context, connector feeRateHistoricalConnector, targets []marketEventFeeRateTarget, scopeFingerprint string) ([]marketEventFeeRateTarget, []marketEventFeeRateFetchResult, []rpc.MarketEventBorrowFeeCoverage) {
	resolver := c.resolveHistoricalFeeRoute
	if resolver == nil && connector != nil {
		resolver = connector.ResolveExactHistoricalStockRoute
	}
	resolved := make([]marketEventFeeRateTarget, len(targets))
	failures := make([]error, len(targets))
	unsupported := make([]bool, len(targets))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 2)
	for i, target := range targets {
		wg.Go(func() {
			if target.contract.Exchange != "" {
				resolved[i] = target
				return
			}
			if resolver == nil {
				failures[i] = &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureGatewayUnavailable}
				return
			}
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				failures[i] = ctx.Err()
				return
			}
			route, err := resolver(ctx, target.contract, min(marketEventFeeRateTimeout, 5*time.Second))
			if err != nil {
				failures[i] = err
				return
			}
			route = normalizeFeeRateContract(route)
			if route.ConID != target.contract.ConID || route.Symbol != target.contract.Symbol || route.SecType != target.contract.SecType ||
				route.Currency != target.contract.Currency || route.Exchange == "" {
				failures[i] = &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureContractUnavailable}
				return
			}
			if !ibkrlib.HistoricalFeeRateUSRouteSupported(route, true) {
				unsupported[i] = true
				return
			}
			resolved[i] = bindFeeRateTargetRoute(target, scopeFingerprint, route)
		})
	}
	wg.Wait()
	ready := make([]marketEventFeeRateTarget, 0, len(targets))
	failed := make([]marketEventFeeRateFetchResult, 0)
	gaps := make([]rpc.MarketEventBorrowFeeCoverage, 0)
	for i, target := range targets {
		if unsupported[i] {
			gaps = append(gaps, rpc.MarketEventBorrowFeeCoverage{
				Symbol: target.contract.Symbol, ContractConID: target.contract.ConID,
				ContractFingerprint: target.identityFingerprint, CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly,
				Status: rpc.BorrowFeeCoverageUnavailable, Reason: "unsupported_market_calendar",
				Source: rpc.BorrowFeeSourceTWSHistorical, DataType: rpc.BorrowFeeDataTypeHistorical,
				Entitlement: rpc.BorrowFeeEntitlementUnknown, ScaleStatus: rpc.BorrowFeeScaleUnverified,
				PolicyEligible: false,
			})
			continue
		}
		if failures[i] == nil {
			ready = append(ready, resolved[i])
			continue
		}
		failed = append(failed, marketEventFeeRateFetchResult{target: target, err: failures[i]})
	}
	return ready, failed, gaps
}

func (c *marketEventCache) reusableFeeRateRow(
	state marketEventFeeRateState,
	target marketEventFeeRateTarget,
	now time.Time,
	connector feeRateHistoricalConnector,
	expectedSession ibkrlib.HistoricalSessionBinding,
) (rpc.MarketEventBorrowFeeCoverage, bool) {
	attempt, attempted := state.LastAttempts[target.stateKey]
	// Persisted attempt outcomes are historical context only. A restart always
	// performs a fresh entitlement/data read before calling them current.
	c.mu.Lock()
	attemptSession, currentProcessAttempt := c.borrowFeeFallbackCurrent[target.stateKey]
	c.mu.Unlock()
	currentProcessAttempt = currentProcessAttempt && attemptSession == expectedSession && feeRateSessionCurrent(connector, attemptSession)
	if !attempted {
		return rpc.MarketEventBorrowFeeCoverage{}, false
	}
	// Broker entitlement/currentness is process-local, but wire admission is
	// durable. A restart inside the last request's pacing window must not repeat
	// an identical HMDS call. Project only unknown/stale retained context until
	// the persisted boundary permits a fresh observation.
	if !currentProcessAttempt {
		if attempt.NextAttempt != nil && now.Before(*attempt.NextAttempt) {
			return feeRateRestartCooldownRow(target, state.LastGood[target.stateKey], attempt), true
		}
		return rpc.MarketEventBorrowFeeCoverage{}, false
	}
	if attempt.Outcome == marketEventFeeRateOutcomeFailure {
		next := attempt.NextAttempt
		if attempt.RuntimeNextAttempt != nil {
			next = attempt.RuntimeNextAttempt
		}
		if next != nil && now.Before(*next) {
			return feeRateFailureRow(target, state.LastGood[target.stateKey], attempt), true
		}
	}
	if attempt.Outcome == marketEventFeeRateOutcomeSuccess {
		if record, ok := state.LastGood[target.stateKey]; ok {
			row := feeRateRecordRow(record, record.ObservedAt, now)
			if attempt.NextAttempt != nil && now.Before(*attempt.NextAttempt) {
				return row, true
			}
			completedDate, _, current := lastCompletedMarketSession(now, marketcal.MarketUSEquity)
			if current && record.SessionDate == completedDate {
				return row, true
			}
		}
	}
	return rpc.MarketEventBorrowFeeCoverage{}, false
}

func feeRateRestartCooldownRow(target marketEventFeeRateTarget, retained marketEventFeeRateRecord, attempt marketEventFeeRateAttempt) rpc.MarketEventBorrowFeeCoverage {
	row := rpc.MarketEventBorrowFeeCoverage{
		Symbol: target.contract.Symbol, ContractConID: target.contract.ConID,
		ContractFingerprint: feeRateTargetFingerprint(target), CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly,
		Status: rpc.BorrowFeeCoverageUnavailable, Reason: "restart_wire_cooldown",
		Source: rpc.BorrowFeeSourceTWSHistorical, DataType: rpc.BorrowFeeDataTypeHistorical,
		ObservedAt: attempt.CompletedAt.UTC(), Entitlement: rpc.BorrowFeeEntitlementUnknown,
		ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
		LastFailure: cloneBorrowFeeSourceFailure(attempt.Failure),
	}
	if retained.IdentityFingerprint == target.identityFingerprint {
		feeRate := retained.FeeRate
		row.ContractConID = retained.Contract.ConID
		row.ContractFingerprint = retained.ContractFingerprint
		row.Status = rpc.BorrowFeeCoverageStale
		row.Reason = "restart_cooldown_retained_last_good"
		row.AsOf = retained.AsOf.UTC()
		row.FeeRate = &feeRate
	}
	return row
}

func feeRateRecordRow(record marketEventFeeRateRecord, observedAt, evaluateAt time.Time) rpc.MarketEventBorrowFeeCoverage {
	feeRate := record.FeeRate
	status := rpc.BorrowFeeCoverageScaleUnknown
	reason := "scale_not_commissioned"
	if completedDate, _, ok := lastCompletedMarketSession(evaluateAt, marketcal.MarketUSEquity); ok && record.SessionDate != completedDate {
		status = rpc.BorrowFeeCoverageStale
		reason = "completed_session_bar_stale"
	}
	return rpc.MarketEventBorrowFeeCoverage{
		Symbol: record.Contract.Symbol, ContractConID: record.Contract.ConID,
		ContractFingerprint: record.ContractFingerprint, CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly,
		Status: status, Reason: reason, Source: rpc.BorrowFeeSourceTWSHistorical,
		DataType: rpc.BorrowFeeDataTypeHistorical, AsOf: record.AsOf.UTC(), ObservedAt: observedAt.UTC(),
		FeeRate: &feeRate, Entitlement: rpc.BorrowFeeEntitlementObserved,
		ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
	}
}

func feeRateFailureRow(target marketEventFeeRateTarget, retained marketEventFeeRateRecord, attempt marketEventFeeRateAttempt) rpc.MarketEventBorrowFeeCoverage {
	row := rpc.MarketEventBorrowFeeCoverage{
		Symbol: target.contract.Symbol, ContractConID: target.contract.ConID,
		ContractFingerprint: feeRateTargetFingerprint(target), CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly,
		Status: rpc.BorrowFeeCoverageUnavailable, Reason: "historical_fee_unavailable",
		Source: rpc.BorrowFeeSourceTWSHistorical, DataType: rpc.BorrowFeeDataTypeHistorical,
		ObservedAt: attempt.CompletedAt.UTC(), Entitlement: rpc.BorrowFeeEntitlementUnknown,
		ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
		LastFailure: cloneBorrowFeeSourceFailure(attempt.Failure),
	}
	if attempt.Failure != nil {
		row.Reason = attempt.Failure.Code
		switch attempt.Failure.Code {
		case rpc.SourceFailureNotEntitled:
			row.Status = rpc.BorrowFeeCoverageNotEntitled
			row.Entitlement = rpc.BorrowFeeEntitlementNotEntitled
		case rpc.SourceFailureNoData:
			row.Status = rpc.BorrowFeeCoverageMissing
		}
	}
	if retained.IdentityFingerprint == target.identityFingerprint {
		feeRate := retained.FeeRate
		row.ContractConID = retained.Contract.ConID
		row.ContractFingerprint = retained.ContractFingerprint
		row.Status = rpc.BorrowFeeCoverageStale
		row.Reason = "retained_last_good_after_failure"
		if attempt.Failure != nil && attempt.Failure.Code == rpc.SourceFailureNoData {
			row.Reason = "completed_session_bar_stale_no_new_data"
		}
		row.AsOf = retained.AsOf.UTC()
		row.FeeRate = &feeRate
	}
	return row
}

func feeRateSymbolGap(symbol, reason string) rpc.MarketEventBorrowFeeCoverage {
	return rpc.MarketEventBorrowFeeCoverage{
		Symbol: symbol, CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly,
		Status: rpc.BorrowFeeCoverageUnavailable, Reason: reason,
		Source: rpc.BorrowFeeSourceTWSHistorical, DataType: rpc.BorrowFeeDataTypeHistorical,
		Entitlement: rpc.BorrowFeeEntitlementUnknown, ScaleStatus: rpc.BorrowFeeScaleUnverified,
		PolicyEligible: false,
	}
}

func feeRateSymbolGaps(symbols []string, reason string) []rpc.MarketEventBorrowFeeCoverage {
	rows := make([]rpc.MarketEventBorrowFeeCoverage, 0, len(symbols))
	for _, symbol := range symbols {
		rows = append(rows, feeRateSymbolGap(symbol, reason))
	}
	return rows
}

func feeRateGapResult(primary rpc.SourceHealth, symbols []string, reason string, now time.Time) ([]rpc.MarketEventBorrowFeeCoverage, rpc.SourceHealth) {
	rows := feeRateSymbolGaps(symbols, reason)
	return rows, feeRateAggregateHealth(primary, rows, now)
}

func feeRateAggregateHealth(primary rpc.SourceHealth, rows []rpc.MarketEventBorrowFeeCoverage, now time.Time) rpc.SourceHealth {
	health := primary
	health.Source = "borrow_fee"
	if len(rows) == 0 {
		return rpc.SourceHealth{
			Source: "borrow_fee", Status: rpc.SourceStatusOK, AsOf: now.UTC(),
			Confidence: "high", RefreshState: rpc.SourceRefreshNotDue,
			Notes: []string{"not_applicable: no exact currently held short-stock contracts require borrow-fee evidence"},
		}
	}
	health.Status = rpc.SourceStatusUnknown
	health.Confidence = "low"
	health.Notes = append(slices.Clone(primary.Notes), "FTP bulk is unusable; TWS historical coverage is limited to exact currently held short stocks")
	latest := time.Time{}
	usable := 0
	stale := 0
	for _, row := range rows {
		evidenceAt := row.AsOf
		if evidenceAt.IsZero() {
			evidenceAt = row.ObservedAt
		}
		if evidenceAt.After(latest) {
			latest = evidenceAt
		}
		switch row.Status {
		case rpc.BorrowFeeCoverageObserved, rpc.BorrowFeeCoverageScaleUnknown:
			usable++
		case rpc.BorrowFeeCoverageStale:
			stale++
		}
	}
	switch {
	case usable > 0:
		health.Status = rpc.SourceStatusPartial
		health.Confidence = "low"
	case len(rows) > 0 && stale == len(rows):
		health.Status = rpc.SourceStatusStale
		health.Confidence = "low"
	}
	if !latest.IsZero() {
		if latest.After(now) {
			latest = now
		}
		health.AsOf = latest.UTC()
		health.AgeSeconds = max(0, int64(now.Sub(latest).Seconds()))
	}
	return health
}

func selectCompletedFeeRateRecord(target marketEventFeeRateTarget, scopeFingerprint string, bars []ibkrlib.HistoricalBar, observedAt time.Time) (marketEventFeeRateRecord, error) {
	completedDate, _, ok := lastCompletedMarketSession(observedAt, marketcal.MarketUSEquity)
	if !ok {
		return marketEventFeeRateRecord{}, &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureNoData}
	}
	var selected ibkrlib.HistoricalBar
	selectedDate := ""
	seenDates := make(map[string]struct{}, len(bars))
	for _, bar := range bars {
		date := historyBarSessionDate(bar)
		if date == "" {
			return marketEventFeeRateRecord{}, &ibkrlib.HistoricalDataValidationError{Reason: "invalid_daily_as_of"}
		}
		if _, duplicate := seenDates[date]; duplicate {
			return marketEventFeeRateRecord{}, &ibkrlib.HistoricalDataValidationError{Reason: "duplicate_session_date"}
		}
		seenDates[date] = struct{}{}
		if date > completedDate || (selectedDate != "" && date < selectedDate) {
			continue
		}
		selected = bar
		selectedDate = date
	}
	if selectedDate == "" || math.IsNaN(selected.Close) || math.IsInf(selected.Close, 0) || selected.Close < 0 {
		return marketEventFeeRateRecord{}, &ibkrlib.HistoricalRequestError{Category: ibkrlib.HistoricalFailureNoData}
	}
	asOf := marketCloseForDate(marketcal.MarketUSEquity, selectedDate, observedAt)
	if asOf.IsZero() || asOf.After(observedAt) {
		return marketEventFeeRateRecord{}, &ibkrlib.HistoricalDataValidationError{Reason: "invalid_daily_as_of"}
	}
	return marketEventFeeRateRecord{
		ScopeFingerprint: scopeFingerprint, IdentityFingerprint: target.identityFingerprint,
		ContractFingerprint: target.contractFingerprint,
		Contract:            feeRateStoredContract(target.contract), SessionDate: selectedDate,
		AsOf: asOf.UTC(), ObservedAt: observedAt.UTC(), FeeRate: selected.Close,
		ScaleStatus: rpc.BorrowFeeScaleUnverified,
	}, nil
}

func feeRateSourceFailure(err error, failedAt time.Time) rpc.SourceFailure {
	failure := rpc.SourceFailure{Code: rpc.SourceFailureTransportFailed, Stage: rpc.SourceFailureStageHistoricalFeeRequest, FailedAt: failedAt.UTC(), Retryable: true}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.Code = rpc.SourceFailureTimeout
		return failure
	}
	if validation, ok := errors.AsType[*ibkrlib.HistoricalDataValidationError](err); ok {
		_ = validation
		failure.Code = rpc.SourceFailureInvalidPayload
		failure.Stage = rpc.SourceFailureStageHistoricalFeeDecode
		failure.Retryable = false
		return failure
	}
	requestErr, ok := errors.AsType[*ibkrlib.HistoricalRequestError](err)
	if !ok {
		return failure
	}
	switch requestErr.Category {
	case ibkrlib.HistoricalFailureNotEntitled:
		failure.Code, failure.Retryable = rpc.SourceFailureNotEntitled, false
	case ibkrlib.HistoricalFailureNoData:
		failure.Code = rpc.SourceFailureNoData
	case ibkrlib.HistoricalFailurePacing:
		failure.Code = rpc.SourceFailurePacing
	case ibkrlib.HistoricalFailureGatewayUnavailable:
		failure.Code = rpc.SourceFailureGatewayUnavailable
	case ibkrlib.HistoricalFailureContractUnavailable:
		failure.Code, failure.Retryable = rpc.SourceFailureContractUnavailable, false
	case ibkrlib.HistoricalFailureInvalidPayload:
		failure.Code, failure.Stage, failure.Retryable = rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageHistoricalFeeDecode, false
	case ibkrlib.HistoricalFailureProtocolRejected:
		failure.Code = rpc.SourceFailureProtocolRejected
	}
	return failure
}

func normalizeFeeRateContract(contract ibkrlib.Contract) ibkrlib.Contract {
	contract.Symbol = strings.ToUpper(strings.TrimSpace(contract.Symbol))
	contract.SecType = strings.ToUpper(strings.TrimSpace(contract.SecType))
	contract.Exchange = strings.ToUpper(strings.TrimSpace(contract.Exchange))
	contract.PrimaryExch = strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	contract.Currency = strings.ToUpper(strings.TrimSpace(contract.Currency))
	contract.LocalSymbol = strings.TrimSpace(contract.LocalSymbol)
	contract.TradingClass = strings.TrimSpace(contract.TradingClass)
	contract.Expiry = strings.TrimSpace(contract.Expiry)
	contract.Right = strings.ToUpper(strings.TrimSpace(contract.Right))
	contract.SecIDType = strings.ToUpper(strings.TrimSpace(contract.SecIDType))
	contract.SecID = strings.TrimSpace(contract.SecID)
	return contract
}

func feeRateStoredContract(contract ibkrlib.Contract) marketEventFeeRateContract {
	contract = normalizeFeeRateContract(contract)
	return marketEventFeeRateContract{
		ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType,
		Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier,
		Exchange: contract.Exchange, PrimaryExch: contract.PrimaryExch, Currency: contract.Currency,
		LocalSymbol: contract.LocalSymbol, TradingClass: contract.TradingClass,
		SecIDType: contract.SecIDType, SecID: contract.SecID,
	}
}

func marketEventFeeRateContractFingerprint(contract ibkrlib.Contract) string {
	contract = normalizeFeeRateContract(contract)
	payload, _ := json.Marshal(feeRateStoredContract(contract))
	digest := sha256.Sum256(append([]byte("market-event-fee-rate-route-v2\x00"), payload...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func marketEventFeeRateIdentityFingerprint(contract ibkrlib.Contract) string {
	contract = normalizeFeeRateContract(contract)
	identity := struct {
		ConID     int    `json:"conid"`
		Symbol    string `json:"symbol"`
		SecType   string `json:"sec_type"`
		Currency  string `json:"currency"`
		SecIDType string `json:"sec_id_type,omitempty"`
		SecID     string `json:"sec_id,omitempty"`
	}{
		ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType,
		Currency: contract.Currency, SecIDType: contract.SecIDType, SecID: contract.SecID,
	}
	payload, _ := json.Marshal(identity)
	digest := sha256.Sum256(append([]byte("market-event-fee-rate-identity-v2\x00"), payload...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func marketEventFeeRateScopeFingerprint(scope brokerStateScope) string {
	payload := strings.ToUpper(strings.TrimSpace(scope.Account)) + "\x00" + strings.ToLower(strings.TrimSpace(scope.Mode))
	digest := sha256.Sum256([]byte("market-event-fee-rate-scope-v1\x00" + payload))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func sortFeeRateCoverage(rows []rpc.MarketEventBorrowFeeCoverage) {
	slices.SortFunc(rows, func(a, b rpc.MarketEventBorrowFeeCoverage) int {
		if a.Symbol != b.Symbol {
			return strings.Compare(a.Symbol, b.Symbol)
		}
		if a.ContractConID != b.ContractConID {
			return cmp.Compare(a.ContractConID, b.ContractConID)
		}
		return strings.Compare(a.ContractFingerprint, b.ContractFingerprint)
	})
}

func cloneMarketEventFeeRateState(in marketEventFeeRateState) marketEventFeeRateState {
	out := newMarketEventFeeRateState()
	if in.Version != 0 {
		out.Version = in.Version
	}
	maps.Copy(out.LastGood, in.LastGood)
	for key, attempt := range in.LastAttempts {
		if attempt.NextAttempt != nil {
			next := *attempt.NextAttempt
			attempt.NextAttempt = &next
		}
		if attempt.RuntimeNextAttempt != nil {
			next := *attempt.RuntimeNextAttempt
			attempt.RuntimeNextAttempt = &next
		}
		if attempt.Failure != nil {
			failure := *attempt.Failure
			attempt.Failure = &failure
		}
		out.LastAttempts[key] = attempt
	}
	return out
}
