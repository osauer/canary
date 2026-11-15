package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/scorecard"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The decision scorecard (internal-docs/design/decision-scorecard.md): an
// append-only ledger of position changes per sleeve in daemon.db, a bounded
// history cache, and the evaluation the read serves. The worker owns every
// write; the read returns the saved publication.
const (
	scorecardLedgerScopePrefix    = "scorecard/ledger/"
	scorecardDecisionKind         = "scorecard.decision.v1"
	scorecardSource               = "scorecard"
	scorecardBarsStateKind        = "scorecard_bars"
	scorecardPublicationStateKind = "scorecard_publication"
	scorecardBarsVersion          = 1
	// scorecardPublicationVersion 3 carries the name-week gate, the sector
	// adjustment and the fill-quote slippage; an older publication read the
	// week-cluster gate and is not served.
	scorecardPublicationVersion = 3

	// scorecardMaxFetchesPerPass bounds the IBKR history requests one pass
	// books on the background lane (60 per 10 minutes shared with every
	// other history reader); the rest wait for the next pass.
	scorecardMaxFetchesPerPass = 20
	// scorecardMaxBars bounds each cached series.
	scorecardMaxBars = 800
	// scorecardLookbackCalendarDays precedes the earliest decision so the
	// 252-session estimation window is covered.
	scorecardLookbackCalendarDays = 400
	// scorecardMaturityCalendarDays follows a decision until its 21st session.
	scorecardMaturityCalendarDays = 32
	scorecardMaxLookbackDays      = 3 * 365
	scorecardPassInterval         = 2 * time.Hour
	// scorecardFetchTimeout bounds one read behind the background pacing
	// queue. The historical bucket admits one request per 10 s, shared with
	// Edge's fan-out and the market and mandate history readers, so a read
	// can wait minutes before it is sent; the market history refresh waits
	// the same window (marketHistoryRefreshWindow).
	scorecardFetchTimeout = 4 * time.Minute
	// scorecardRetryBase is the wait after a failed read: one pass, then
	// doubling per consecutive failure up to scorecardMaxRetryAfter.
	scorecardRetryBase     = scorecardPassInterval
	scorecardMaxRetryAfter = 24 * time.Hour
	scorecardLedgerPage    = 10000
)

type scorecardBarCache struct {
	Version          int                           `json:"version"`
	ScopeFingerprint string                        `json:"scope_fingerprint"`
	Names            map[string]scorecardBarSeries `json:"names"`
	Market           scorecardBarSeries            `json:"market"`
	// Sectors holds one series per sector fund the ledger needs, by fund
	// symbol, read on the same lane and cadence as the market proxy.
	Sectors map[string]scorecardBarSeries `json:"sectors,omitempty"`
}

type scorecardBarSeries struct {
	ConID    int64  `json:"conid"`
	Symbol   string `json:"symbol"`
	Currency string `json:"currency"`
	// FetchedAt is the last successful read. A failure never moves it, so
	// one failed read does not cost the name its session.
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// Lookback is the calendar-day span the last fetch asked for; a name
	// whose history is shorter than that is not asked again for it.
	Lookback int `json:"lookback,omitempty"`
	// Failure is the last failed read since the last successful one: the
	// broker's answer, when it came, how many reads in a row failed and
	// when the name is asked again. The bars of the last success stay.
	Failure  string              `json:"failure,omitempty"`
	FailedAt time.Time           `json:"failed_at,omitzero"`
	Failures int                 `json:"failures,omitempty"`
	RetryAt  time.Time           `json:"retry_at,omitzero"`
	Bars     []edgecore.DailyBar `json:"bars"`
}

type scorecardPublication struct {
	Version             int                 `json:"version"`
	ScopeFingerprint    string              `json:"scope_fingerprint"`
	EvidenceFingerprint string              `json:"evidence_fingerprint,omitempty"`
	Result              rpc.ScorecardResult `json:"result"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

func (s *Server) startScorecardWorker(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.mu.Lock()
	if s.scorecardWake != nil {
		s.mu.Unlock()
		return
	}
	s.scorecardWake = make(chan struct{}, 1)
	wake := s.scorecardWake
	s.scorecardWorkerWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.scorecardWorkerWG.Done()
		ticker := time.NewTicker(scorecardPassInterval)
		defer ticker.Stop()
		for {
			s.scorecardBusy.Store(true)
			if err := s.rebuildScorecard(ctx); err != nil && ctx.Err() == nil {
				s.warnf("decision scorecard pass failed: %v", err)
				s.publishScorecardState(context.WithoutCancel(ctx), rpc.ScorecardStateUnavailable, "authority_unavailable")
			}
			s.scorecardBusy.Store(false)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
}

func (s *Server) kickScorecardRebuild() {
	if s == nil {
		return
	}
	s.mu.Lock()
	wake := s.scorecardWake
	s.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// rebuildScorecard is one pass: derive the ledger from the retained
// statements, append what is new, fetch the history still missing within the
// budget, evaluate and save the publication.
func (s *Server) rebuildScorecard(ctx context.Context) error {
	if s == nil || s.coreStore == nil {
		return fmt.Errorf("scorecard SQLite authority is unavailable")
	}
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		return s.publishScorecardState(ctx, rpc.ScorecardStateUnavailable, "flex_configuration_required")
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return s.publishScorecardState(ctx, rpc.ScorecardStateUnavailable, "account_scope_unavailable")
	}
	scopeFingerprint := edgeScopeFingerprint(scope)
	loadCtx, cancel := context.WithTimeout(ctx, edgeStatementLoadTimeout)
	projected, err := s.loadEdgeProjectionEvidence(loadCtx, scope, scopeFingerprint)
	cancel()
	if err != nil {
		return err
	}
	if len(projected.Statements) == 0 {
		return s.saveScorecardPublication(ctx, scorecardPublication{ScopeFingerprint: scopeFingerprint, Result: scorecardStateOnlyResult(rpc.ScorecardStateBackfilling, "statement_account_pending")})
	}
	base := inferEdgeBaseCurrency(projected.Statements)
	evidence, err := edgecore.PositionChanges(projected.Statements, base)
	if err != nil {
		return err
	}
	localEvents, localProposals, _ := s.edgeProtectionEvidence(ctx, scope)
	ledger, skipped := scorecard.BuildLedger(evidence, scorecardSleeveResolver(localEvents, localProposals))
	ledgerScope := scorecardLedgerScopePrefix + scopeFingerprint
	persisted, err := s.loadScorecardLedger(ctx, ledgerScope)
	if err != nil {
		return err
	}
	if err := s.appendScorecardDecisions(ctx, ledgerScope, persisted, ledger); err != nil {
		return err
	}
	current := map[string]bool{}
	for _, d := range ledger {
		current[d.ID] = true
	}
	superseded := 0
	for id := range persisted {
		if !current[id] {
			superseded++
		}
	}

	now := s.edgeNow()
	cache, err := s.loadScorecardBarCache(ctx)
	if err != nil {
		return err
	}
	if cache.ScopeFingerprint != scopeFingerprint {
		cache = scorecardBarCache{Version: scorecardBarsVersion, ScopeFingerprint: scopeFingerprint, Names: map[string]scorecardBarSeries{}}
	}
	sectorFunds, sectorNames := scorecardSectorMap(ledger, nil)
	pending, err := s.fetchScorecardNeeds(ctx, &cache, scorecardHistoryNeeds(ledger, sectorFunds, cache, now), now)
	if err != nil {
		return err
	}
	history, state := scorecardHistoryFromCache(cache, pending)
	history.SectorFunds, history.SectorNames = sectorFunds, sectorNames
	result := scorecard.Evaluate(scorecard.Input{
		AsOf: now, EvidenceTo: scorecardEvidenceTo(projected), BaseCurrency: base, Decisions: ledger, History: history,
		Marks: evidence.Marks, Events: evidence.Events, Costs: s.scorecardFillCosts(ledger), Skipped: skipped, Superseded: superseded, HistoryState: state,
	})
	switch {
	case pending > 0:
		result.State, result.Reason = rpc.ScorecardStateBackfilling, "history_backfill_pending"
	case len(state.Failures) > 0 || state.Missing > 0:
		result.Reason = "history_partial"
	}
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return nil
	}
	return s.saveScorecardPublication(ctx, scorecardPublication{ScopeFingerprint: scopeFingerprint, EvidenceFingerprint: projected.Fingerprint, Result: result})
}

// scorecardHistoryNeed is one history fetch the ledger still needs: the
// market proxy, a sector fund (both by symbol) or a name by contract.
type scorecardHistoryNeed struct {
	market   bool
	sector   bool
	conid    int64
	symbol   string
	currency string
	lookback int
	earliest time.Time
}

// scorecardHistoryNeeds lists the names whose cached history does not yet
// cover their decisions: the market proxy first, then one series per
// sector fund the ledger's names belong to (at most one per sector, read
// once per session like the market), then the names by earliest decision.
// A name read today is not read again today; a name whose read failed is
// asked again at its retry time.
func scorecardHistoryNeeds(ledger []scorecard.Decision, sectorFunds map[int64]string, cache scorecardBarCache, now time.Time) []scorecardHistoryNeed {
	type span struct {
		symbol, currency string
		earliest, latest time.Time
	}
	spans := map[int64]*span{}
	var earliestAll, latestAll time.Time
	for _, d := range ledger {
		if d.UnderlyingConID == 0 {
			continue
		}
		day := d.ExecutedAt.UTC()
		item := spans[d.UnderlyingConID]
		if item == nil {
			item = &span{symbol: d.Underlying, currency: d.Currency, earliest: day, latest: day}
			spans[d.UnderlyingConID] = item
		}
		if day.Before(item.earliest) {
			item.earliest = day
		}
		if day.After(item.latest) {
			item.latest = day
		}
		if earliestAll.IsZero() || day.Before(earliestAll) {
			earliestAll = day
		}
		if day.After(latestAll) {
			latestAll = day
		}
	}
	var out []scorecardHistoryNeed
	if !earliestAll.IsZero() && scorecardSeriesStale(cache.Market, earliestAll, latestAll, now) {
		out = append(out, scorecardHistoryNeed{market: true, symbol: "SPY", currency: "USD", lookback: scorecardLookback(earliestAll, now), earliest: earliestAll})
	}
	// A sector fund is needed from the earliest decision of any name in its
	// sector to the latest; funds are read in symbol order.
	type sectorSpan struct{ earliest, latest time.Time }
	sectors := map[string]*sectorSpan{}
	for conid, fund := range sectorFunds {
		item := spans[conid]
		if item == nil {
			continue
		}
		span := sectors[fund]
		if span == nil {
			span = &sectorSpan{earliest: item.earliest, latest: item.latest}
			sectors[fund] = span
		}
		if item.earliest.Before(span.earliest) {
			span.earliest = item.earliest
		}
		if item.latest.After(span.latest) {
			span.latest = item.latest
		}
	}
	funds := make([]string, 0, len(sectors))
	for fund := range sectors {
		funds = append(funds, fund)
	}
	sort.Strings(funds)
	for _, fund := range funds {
		span := sectors[fund]
		if scorecardSeriesStale(cache.Sectors[fund], span.earliest, span.latest, now) {
			out = append(out, scorecardHistoryNeed{sector: true, symbol: fund, currency: "USD", lookback: scorecardLookback(span.earliest, now), earliest: span.earliest})
		}
	}
	ids := make([]int64, 0, len(spans))
	for conid := range spans {
		ids = append(ids, conid)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := spans[ids[i]], spans[ids[j]]
		if !a.earliest.Equal(b.earliest) {
			return a.earliest.Before(b.earliest)
		}
		return ids[i] < ids[j]
	})
	for _, conid := range ids {
		item := spans[conid]
		series := cache.Names[strconv.FormatInt(conid, 10)]
		if !scorecardSeriesStale(series, item.earliest, item.latest, now) {
			continue
		}
		out = append(out, scorecardHistoryNeed{conid: conid, symbol: item.symbol, currency: item.currency, lookback: scorecardLookback(item.earliest, now), earliest: item.earliest})
	}
	return out
}

// scorecardSeriesStale reports whether a series must be fetched: its last
// read failed and its retry time has come, or it is empty, was fetched with
// a shorter lookback than the earliest decision now needs, or ends before
// the latest decision could have matured and was not read since. One
// successful fetch per name per session.
func scorecardSeriesStale(series scorecardBarSeries, earliest, latest, now time.Time) bool {
	if series.Failure != "" {
		// A failure the shipped cache stamped as fetched today carries no
		// retry time; it waits one pass from the stamp.
		retryAt := series.RetryAt
		if retryAt.IsZero() {
			retryAt = series.FetchedAt.Add(scorecardRetryBase)
		}
		return !now.Before(retryAt)
	}
	if !series.FetchedAt.IsZero() && sameUTCDay(series.FetchedAt, now) {
		return false
	}
	if len(series.Bars) == 0 {
		return true
	}
	if scorecardLookback(earliest, now) > series.Lookback+30 {
		return true
	}
	last := series.Bars[len(series.Bars)-1].Day
	maturity := latest.AddDate(0, 0, scorecardMaturityCalendarDays)
	return last.Before(maturity) && series.FetchedAt.Before(minTime(maturity, now).Add(-18*time.Hour))
}

func scorecardLookback(earliest, now time.Time) int {
	days := int(now.Sub(earliest.AddDate(0, 0, -scorecardLookbackCalendarDays)).Hours()/24) + 1
	return max(scorecardLookbackCalendarDays, min(scorecardMaxLookbackDays, days))
}

// fetchScorecardNeeds reads the needs in order, at most
// scorecardMaxFetchesPerPass, and keeps each answer in the cache as it
// arrives. A gateway that is away ends the pass without a mark against any
// name: the needs it did not reach are pending for the next pass. It returns
// how many needs were not read.
func (s *Server) fetchScorecardNeeds(ctx context.Context, cache *scorecardBarCache, needs []scorecardHistoryNeed, now time.Time) (int, error) {
	fetched := 0
	for _, need := range needs {
		if fetched >= scorecardMaxFetchesPerPass || ctx.Err() != nil {
			break
		}
		key := strconv.FormatInt(need.conid, 10)
		prev := cache.Market
		switch {
		case need.sector:
			prev = cache.Sectors[need.symbol]
		case !need.market:
			prev = cache.Names[key]
		}
		series, err := s.fetchScorecardSeries(ctx, prev, need, now)
		if errors.Is(err, ibkrlib.ErrIBKRUnavailable) || ctx.Err() != nil {
			break
		}
		fetched++
		if err != nil && scorecardFailureWarnDue(prev, now) {
			s.warnf("decision scorecard: history read for %s (conid %d) failed: %v; %s, next try %s", need.symbol, need.conid, err, scorecardKeptWords(prev), series.RetryAt.UTC().Format(time.RFC3339))
		}
		switch {
		case need.market:
			cache.Market = series
		case need.sector:
			if cache.Sectors == nil {
				cache.Sectors = map[string]scorecardBarSeries{}
			}
			cache.Sectors[need.symbol] = series
		default:
			cache.Names[key] = series
		}
		if err := s.saveScorecardBarCache(ctx, *cache); err != nil {
			return 0, err
		}
	}
	return len(needs) - fetched, nil
}

// fetchScorecardSeries reads one series' daily closes on the background
// lane: the market proxy and the sector funds by symbol, a name by its exact
// contract first, then the symbol route for an index underlying.
// A success replaces the bars and stamps the session; a failure keeps the
// last success's bars and records the broker's answer with the time the
// name is asked again. The error is returned for the caller's own decision.
func (s *Server) fetchScorecardSeries(ctx context.Context, prev scorecardBarSeries, need scorecardHistoryNeed, now time.Time) (scorecardBarSeries, error) {
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), scorecardFetchTimeout)
	defer cancel()
	series := prev
	series.ConID, series.Symbol, series.Currency = need.conid, need.symbol, need.currency
	var bars []edgecore.DailyBar
	var err error
	if need.market || need.sector {
		bars, err = s.fetchEdgeMarketBars(readCtx, need.symbol, need.lookback)
	} else {
		bars, err = s.fetchEdgeBars(readCtx, edgeContractSpec{ConID: need.conid, Symbol: need.symbol, Currency: need.currency}, need.lookback)
		if err != nil && need.symbol != "" && readCtx.Err() == nil && !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			bars, err = s.fetchEdgeMarketBars(readCtx, need.symbol, need.lookback)
		}
	}
	if err != nil {
		series.Failures = prev.Failures + 1
		series.Failure, series.FailedAt = errText(err), now
		series.RetryAt = now.Add(scorecardRetryAfter(series.Failures))
		return series, err
	}
	series.Bars, series.FetchedAt, series.Lookback = scorecardNormalizeBars(bars, need.conid), now, need.lookback
	series.Failure, series.FailedAt, series.Failures, series.RetryAt = "", time.Time{}, 0, time.Time{}
	return series, nil
}

// scorecardRetryAfter is the wait after the n-th consecutive failed read:
// one pass, doubling, at most a day.
func scorecardRetryAfter(failures int) time.Duration {
	wait := scorecardRetryBase
	for i := 1; i < failures && wait < scorecardMaxRetryAfter; i++ {
		wait *= 2
	}
	return min(wait, scorecardMaxRetryAfter)
}

// scorecardFailureWarnDue reports whether a failed read is its name's first
// of the day: the log warns once per name per day, the result lists every
// failure.
func scorecardFailureWarnDue(prev scorecardBarSeries, now time.Time) bool {
	return prev.FailedAt.IsZero() || !sameUTCDay(prev.FailedAt, now)
}

func scorecardKeptWords(series scorecardBarSeries) string {
	if len(series.Bars) == 0 {
		return "no history behind its scores"
	}
	return fmt.Sprintf("the %d closes of the last successful read stay", len(series.Bars))
}

func sameUTCDay(a, b time.Time) bool {
	return a.UTC().Format(time.DateOnly) == b.UTC().Format(time.DateOnly)
}

// scorecardHistoryFromCache assembles the evidence behind the scores and the
// state the result reports: the names with and without bars, the reads
// still pending and every name whose last read failed, the market proxy
// first, then by symbol.
func scorecardHistoryFromCache(cache scorecardBarCache, pending int) (scorecard.History, rpc.ScorecardHistory) {
	history := scorecard.History{Underlying: make(map[int64][]edgecore.DailyBar, len(cache.Names)), Market: append([]edgecore.DailyBar(nil), cache.Market.Bars...), MarketSymbol: "SPY"}
	state := rpc.ScorecardHistory{Names: len(cache.Names), Pending: pending, LastFetchedAt: scorecardLastFetch(cache)}
	if cache.Market.Failure != "" {
		state.Failures = append(state.Failures, scorecardHistoryFailure(cache.Market))
	}
	if len(cache.Sectors) > 0 {
		history.Sector = make(map[string][]edgecore.DailyBar, len(cache.Sectors))
		funds := make([]string, 0, len(cache.Sectors))
		for fund := range cache.Sectors {
			funds = append(funds, fund)
		}
		sort.Strings(funds)
		for _, fund := range funds {
			series := cache.Sectors[fund]
			if series.Failure != "" {
				state.Failures = append(state.Failures, scorecardHistoryFailure(series))
			}
			if len(series.Bars) > 0 {
				history.Sector[fund] = append([]edgecore.DailyBar(nil), series.Bars...)
			}
		}
	}
	var failed []rpc.ScorecardHistoryFailure
	for _, series := range cache.Names {
		if series.Failure != "" {
			failed = append(failed, scorecardHistoryFailure(series))
		}
		if len(series.Bars) == 0 {
			state.Missing++
			continue
		}
		history.Underlying[series.ConID] = append([]edgecore.DailyBar(nil), series.Bars...)
	}
	sort.Slice(failed, func(i, j int) bool {
		if failed[i].Symbol != failed[j].Symbol {
			return failed[i].Symbol < failed[j].Symbol
		}
		return failed[i].ConID < failed[j].ConID
	})
	state.Failures = append(state.Failures, failed...)
	return history, state
}

// scorecardHistoryFailure is the served form of a failed series. A failure
// the shipped cache stamped as fetched carries the stamp as its time and
// waits one pass from it.
func scorecardHistoryFailure(series scorecardBarSeries) rpc.ScorecardHistoryFailure {
	failedAt, retryAt := series.FailedAt, series.RetryAt
	if failedAt.IsZero() {
		failedAt = series.FetchedAt
	}
	if retryAt.IsZero() {
		retryAt = series.FetchedAt.Add(scorecardRetryBase)
	}
	return rpc.ScorecardHistoryFailure{Symbol: series.Symbol, ConID: series.ConID, Bars: len(series.Bars), Failure: series.Failure, FailedAt: failedAt, RetryAt: retryAt}
}

// scorecardNormalizeBars keeps one positive close per day, oldest first,
// bounded to the newest scorecardMaxBars sessions.
func scorecardNormalizeBars(bars []edgecore.DailyBar, conid int64) []edgecore.DailyBar {
	byDay := map[string]edgecore.DailyBar{}
	for _, bar := range bars {
		if bar.Close > 0 && !bar.Day.IsZero() {
			byDay[bar.Day.UTC().Format(time.DateOnly)] = edgecore.DailyBar{ConID: conid, Day: bar.Day, Close: bar.Close}
		}
	}
	keys := make([]string, 0, len(byDay))
	for key := range byDay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > scorecardMaxBars {
		keys = keys[len(keys)-scorecardMaxBars:]
	}
	out := make([]edgecore.DailyBar, 0, len(keys))
	for _, key := range keys {
		out = append(out, byDay[key])
	}
	return out
}

func scorecardLastFetch(cache scorecardBarCache) time.Time {
	last := cache.Market.FetchedAt
	for _, series := range cache.Sectors {
		if series.FetchedAt.After(last) {
			last = series.FetchedAt
		}
	}
	for _, series := range cache.Names {
		if series.FetchedAt.After(last) {
			last = series.FetchedAt
		}
	}
	return last
}

func scorecardEvidenceTo(projected edgeProjectionEvidence) time.Time {
	var out time.Time
	for _, statement := range projected.Statements {
		if statement.ToDate.After(out) {
			out = statement.ToDate
		}
	}
	return out
}

// loadScorecardLedger returns the persisted decision ids for one scope.
func (s *Server) loadScorecardLedger(ctx context.Context, scopeKey string) (map[string]bool, error) {
	out := map[string]bool{}
	var after int64
	for {
		page, err := s.coreStore.ListObservations(ctx, corestore.ObservationQuery{ScopeKey: scopeKey, Source: scorecardSource, Kind: scorecardDecisionKind, AfterObservationID: after, Limit: scorecardLedgerPage})
		if err != nil {
			return nil, fmt.Errorf("load scorecard ledger: %w", err)
		}
		for _, row := range page {
			var d scorecard.Decision
			if err := json.Unmarshal(row.Payload, &d); err != nil {
				return nil, fmt.Errorf("decode scorecard decision %d: %w", row.ID, err)
			}
			out[d.ID] = true
			after = max(after, row.ID)
		}
		if len(page) < scorecardLedgerPage {
			return out, nil
		}
	}
}

// appendScorecardDecisions appends the decisions not yet persisted, in ledger
// order, as immutable non-decision observations. Known ids are never
// rewritten.
func (s *Server) appendScorecardDecisions(ctx context.Context, scopeKey string, known map[string]bool, ledger []scorecard.Decision) error {
	var inputs []corestore.ObservationInput
	for _, d := range ledger {
		if known[d.ID] {
			continue
		}
		payload, err := json.Marshal(d)
		if err != nil {
			return err
		}
		inputs = append(inputs, corestore.ObservationInput{ScopeKey: scopeKey, Source: scorecardSource, Kind: scorecardDecisionKind, ObservedAt: d.ExecutedAt, ContentType: "application/json", Payload: payload, DecisionEligible: false})
		known[d.ID] = true
	}
	for start := 0; start < len(inputs); start += 500 {
		end := min(len(inputs), start+500)
		if _, err := s.coreStore.AppendObservations(ctx, inputs[start:end]); err != nil {
			return fmt.Errorf("append scorecard ledger: %w", err)
		}
	}
	return nil
}

func (s *Server) loadScorecardBarCache(ctx context.Context) (scorecardBarCache, error) {
	out := scorecardBarCache{Version: scorecardBarsVersion, Names: map[string]scorecardBarSeries{}}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, scorecardBarsStateKind)
	if err != nil || !ok {
		return out, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, fmt.Errorf("decode scorecard history cache: %w", err)
	}
	if out.Version != scorecardBarsVersion {
		return scorecardBarCache{Version: scorecardBarsVersion, Names: map[string]scorecardBarSeries{}}, nil
	}
	if out.Names == nil {
		out.Names = map[string]scorecardBarSeries{}
	}
	return out, nil
}

func (s *Server) saveScorecardBarCache(ctx context.Context, cache scorecardBarCache) error {
	cache.Version = scorecardBarsVersion
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return s.replaceEdgeStateDocument(ctx, scorecardBarsStateKind, raw)
}

func (s *Server) loadScorecardPublication(ctx context.Context) (scorecardPublication, bool, error) {
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, scorecardPublicationStateKind)
	if err != nil || !ok {
		return scorecardPublication{}, ok, err
	}
	var out scorecardPublication
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, false, fmt.Errorf("decode scorecard publication: %w", err)
	}
	if out.Version != scorecardPublicationVersion {
		return scorecardPublication{}, false, nil
	}
	return out, true, nil
}

func (s *Server) saveScorecardPublication(ctx context.Context, publication scorecardPublication) error {
	publication.Version = scorecardPublicationVersion
	publication.UpdatedAt = s.edgeNow()
	if publication.Result.AsOf.IsZero() {
		publication.Result.AsOf = publication.UpdatedAt
	}
	raw, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	return s.replaceEdgeStateDocument(ctx, scorecardPublicationStateKind, raw)
}

func (s *Server) publishScorecardState(ctx context.Context, state, reason string) error {
	if s == nil || s.coreStore == nil {
		return nil
	}
	publication, ok, _ := s.loadScorecardPublication(ctx)
	if !ok {
		publication = scorecardPublication{Result: scorecardStateOnlyResult(state, reason)}
	} else {
		publication.Result.State, publication.Result.Reason = state, reason
	}
	return s.saveScorecardPublication(ctx, publication)
}

func scorecardStateOnlyResult(state, reason string) rpc.ScorecardResult {
	return rpc.ScorecardResult{
		SchemaVersion: scorecard.SchemaVersion, State: state, Reason: reason, PrimaryHorizon: scorecard.PrimaryHorizon, Horizons: append([]int(nil), scorecard.Horizons[:]...),
		Sleeves: []rpc.ScorecardSleeve{}, Decisions: []rpc.ScorecardDecision{},
		Coverage: rpc.ScorecardCoverage{RuleOriginated: map[string]int{}, Skipped: map[string]int{}, ScoredByHorizon: map[int]int{}, UnknownByReason: map[string]int{}},
		History:  rpc.ScorecardHistory{MarketSymbol: "SPY"}, NotExecution: true,
	}
}

// scorecardSleeveResolver reads Canary's own order journal and proposal
// records: a fill whose execution the journal matches to a protection or
// automatic proposal bucket, or to a daemon-originated write, is the rule
// sleeve with that origin. Every other fill (hand, TWS, console, or any fill
// the bounded journal read could not see) is the owner's.
func scorecardSleeveResolver(events []orderJournalEvent, proposals []proposalEvent) scorecard.SleeveResolver {
	byExecution := map[string][]orderJournalEvent{}
	for _, ev := range events {
		if ev.ExecID != "" {
			byExecution[ev.ExecID] = append(byExecution[ev.ExecID], ev)
		}
	}
	bucketByRef := map[string]string{}
	entryByRef := map[string]proposalEvent{}
	for _, p := range proposals {
		if p.Type == "submitted" && p.OrderRef != "" && p.Bucket != "" {
			bucketByRef[p.OrderRef] = p.Bucket
			if p.Bucket == rpc.TradeProposalBucketStrategyRebalance {
				entryByRef[p.OrderRef] = p
			}
		}
	}
	return func(change edgecore.PositionChange) scorecard.Sleeve {
		for _, execID := range change.ExecutionIDs {
			for _, ev := range byExecution[execID] {
				// Another register entry's row fills in that entry's sleeve,
				// as the core's in the core's (strategy-register.md, section 5).
				if p, ok := entryByRef[ev.OrderRef]; ok && ev.OrderRef != "" && p.Sleeve != "" {
					return scorecard.Sleeve{Name: p.Sleeve, Origin: "proposal:" + rpc.TradeProposalBucketStrategyRebalance, StrategyRef: p.Strategy}
				}
				if sleeve, ok := scorecardMachineSleeve(ev, bucketByRef); ok {
					return sleeve
				}
			}
		}
		return scorecard.Sleeve{Name: scorecard.SleeveOwner}
	}
}

// scorecardMachineSleeve is the sleeve of a fill a machine produced: the
// core's own row (core_rebalance) is the core sleeve's, reported as the
// rule's fill under its rule id and never as the owner's decision
// (core-sleeve.md, review S1); every other rule origin is the rule sleeve.
func scorecardMachineSleeve(ev orderJournalEvent, bucketByRef map[string]string) (scorecard.Sleeve, bool) {
	if ev.OrderRef != "" && bucketByRef[ev.OrderRef] == rpc.TradeProposalBucketCoreRebalance {
		return scorecard.Sleeve{Name: rpc.ScorecardSleeveCore, Origin: "proposal:" + rpc.TradeProposalBucketCoreRebalance, StrategyRef: core.RuleID}, true
	}
	if origin := scorecardRuleOrigin(ev, bucketByRef); origin != "" {
		return scorecard.Sleeve{Name: rpc.ScorecardSleeveRule, Origin: origin}, true
	}
	return scorecard.Sleeve{}, false
}

func scorecardRuleOrigin(ev orderJournalEvent, bucketByRef map[string]string) string {
	if bucket := bucketByRef[ev.OrderRef]; ev.OrderRef != "" && scorecardRuleBucket(bucket) {
		return "proposal:" + bucket
	}
	switch ev.Origin {
	case rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginDaemonProtectiveGuard:
		return "origin:" + ev.Origin
	}
	if ev.TransmitOrigin == corestore.OriginDaemon {
		return "origin:" + string(ev.TransmitOrigin)
	}
	return ""
}

// scorecardRuleBucket names the proposal buckets whose fills are a rule's,
// not a decision: protection, hygiene, budget and expiry exits, the mandate's
// forced reduction, strategy exits and the cash sweeps.
func scorecardRuleBucket(bucket string) bool {
	switch bucket {
	// "option_profit_trail" has no rpc constant yet; the owner named it as
	// a rule bucket, so a proposal that carries it is tagged when it exists.
	case rpc.TradeProposalBucketTrailingStop, rpc.TradeProposalBucketOptionLossExit, "option_profit_trail",
		rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketMandateReduction, rpc.TradeProposalBucketOptionExpiryClose, rpc.TradeProposalBucketThetaHygiene,
		rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketStrategyExit, rpc.TradeProposalBucketOptionExitReview,
		rpc.TradeProposalBucketCashSweep, rpc.TradeProposalBucketCurrencyLeveling:
		return true
	default:
		return false
	}
}
