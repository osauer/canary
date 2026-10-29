package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Decision-grade daily history (internal-docs/design/mandate-risk-budget.md).
// The chart store serves what it has and labels gaps; the mandate's verdict
// must instead refuse to judge on a missing or stale series, so it keeps its
// own documents with the source time of every read. Reads go through the
// same broker fetch helper as the charts, on the background pacing lane,
// within the sixty-per-ten-minutes historical budget: at most one successful
// refresh per series per broker session, only when the series lacks the
// last completed session.

const (
	stateKindMandateHistory = "mandate_history"
	mandateHistoryDocVer    = 1
	// mandateHistoryKeep bounds the closes a series holds; mandate.Closes
	// are needed after alignment across calendars.
	mandateHistoryKeep = 320
	// mandateHistoryFetchDays is the calendar span one read asks for: about
	// 275 sessions, so a mixed US and European book still shares 253 dates.
	mandateHistoryFetchDays = 400
	// mandateHistoryRefreshTick is how often the worker looks for due series;
	// mandateHistoryRefreshWindow bounds one read behind the pacing queue.
	mandateHistoryRefreshTick   = time.Minute
	mandateHistoryRefreshWindow = 4 * time.Minute
	// mandateHistoryRetryAfter spaces failed attempts within a session, and
	// mandateHistoryAttemptsPerSession bounds them.
	mandateHistoryRetryAfter         = 15 * time.Minute
	mandateHistoryAttemptsPerSession = 3
	// stateKindMandateHistoryRefusals keeps the series IBKR refused for want
	// of a market data permission, so a reconnect or restart does not ask
	// again inside the session the refusal answered.
	stateKindMandateHistoryRefusals = "mandate_history_refusals"
)

// mandateHistoryRefusal is IBKR's refusal of a series for want of a market
// data permission: final for the completed session it answered (Session, on
// Market's calendar), asked again once at the next one so an added
// subscription is picked up without a restart.
type mandateHistoryRefusal struct {
	At      time.Time        `json:"at"`
	Error   string           `json:"error"`
	Session string           `json:"session"`
	Market  marketcal.Market `json:"market,omitempty"`
}

// mandateHistorySeries is one persisted series.
type mandateHistorySeries struct {
	Version  int                `json:"version"`
	Key      string             `json:"key"`
	Contract rpc.ContractParams `json:"contract"`
	Dates    []string           `json:"dates"`
	Closes   []float64          `json:"closes"`
	// FetchedAt is the source time of the last successful broker read and
	// Source the broker's basis label.
	FetchedAt time.Time `json:"fetched_at"`
	Source    string    `json:"source,omitempty"`
	// Market is the calendar the series is judged fresh on, when the need
	// named one (the core's listing exchange); empty is the US calendar.
	Market marketcal.Market `json:"market,omitempty"`
}

// mandateHistoryNeed is one series the book needs: its key, the contract
// the broker is asked for and, for the core's series, the calendar of its
// listing exchange.
type mandateHistoryNeed struct {
	Key       string
	Contract  rpc.ContractParams
	Market    marketcal.Market
	Ambiguous bool
}

type mandateHistoryAttempt struct {
	completedSession string
	market           marketcal.Market
	at               time.Time
	session          ibkrlib.ConnectorSessionBinding
	attempts         int
	succeeded        bool
}

// mandateHistoryFailure is a series' last failed read, kept until a read
// succeeds, so a hold can name the broker's answer.
type mandateHistoryFailure struct {
	at  time.Time
	err string
}

// mandateHistoryStore caches the series in memory, loading each from SQLite
// on first use and writing through on every successful read.
type mandateHistoryStore struct {
	mu        sync.Mutex
	series    map[string]*mandateHistorySeries
	revisions map[string]int64
	loaded    map[string]bool
	attempts  map[string]mandateHistoryAttempt
	failures  map[string]mandateHistoryFailure
	// refusals mirrors stateKindMandateHistoryRefusals, loaded on first use.
	refusals        map[string]mandateHistoryRefusal
	refusalsLoaded  bool
	refusalsVersion int64
	// requested holds series an order asked for that the book does not
	// need (a name or currency not held yet), read on the same background
	// lane and budget until they are fresh or mandateHistoryRequestKeep
	// passes.
	requested map[string]mandateHistoryRequest
}

// mandateHistoryRequest is one series an order asked for, and when.
type mandateHistoryRequest struct {
	need mandateHistoryNeed
	at   time.Time
}

// mandateHistoryRequestKeep bounds how long an order's request for a
// series stands.
const mandateHistoryRequestKeep = 24 * time.Hour

// requestMandateHistory asks the background lane for series the book does
// not need yet: the order that needed them was refused unmeasured, and a
// retry once they are fresh is measured.
func (s *Server) requestMandateHistory(needs ...mandateHistoryNeed) {
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.requested == nil {
		st.requested = map[string]mandateHistoryRequest{}
	}
	now := s.nowUTC()
	for _, need := range needs {
		need = bindMandateHistoryNeed(need)
		if _, ok := st.requested[need.Key]; !ok {
			st.requested[need.Key] = mandateHistoryRequest{need: need, at: now}
		}
	}
}

// mandateHistoryRequests lists the standing requests, dropping the ones
// older than mandateHistoryRequestKeep.
func (s *Server) mandateHistoryRequests(now time.Time) []mandateHistoryNeed {
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []mandateHistoryNeed
	for key, req := range st.requested {
		if now.Sub(req.at) > mandateHistoryRequestKeep {
			delete(st.requested, key)
			continue
		}
		out = append(out, req.need)
	}
	slices.SortFunc(out, func(a, b mandateHistoryNeed) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// mandateHistoryNeedsWithRequests is what one background pass reads: the
// book's needs, then the standing requests the book does not already need.
func (s *Server) mandateHistoryNeedsWithRequests(ctx context.Context, pos *rpc.PositionsResult, base string, now time.Time) []mandateHistoryNeed {
	needs := mandateHistoryNeeds(pos, base, s.registerFundLines(ctx))
	for _, req := range s.mandateHistoryRequests(now) {
		if !slices.ContainsFunc(needs, func(n mandateHistoryNeed) bool {
			return bindMandateHistoryNeed(n).Key == bindMandateHistoryNeed(req).Key
		}) {
			needs = append(needs, req)
		}
	}
	return needs
}

func mandateHistoryScope(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "mandate/history/" + hex.EncodeToString(sum[:])
}

// mandateHistoryNeeds lists the series a positions read needs: every held
// stock and ETF (a line on mandate.ReturnsProxies reads its proxy's, the
// index, also when lines names its ISIN by leg ID), the underlying of every
// held option, the index proxy and the base currency's pair against every
// other held currency.
func mandateHistoryNeeds(pos *rpc.PositionsResult, base string, lines map[string]string) []mandateHistoryNeed {
	base = normCcy(base)
	needs := map[string]mandateHistoryNeed{}
	add := func(need mandateHistoryNeed) {
		if existing, ok := needs[need.Key]; ok {
			if existing.Ambiguous || mandateHistoryContractsConflict(existing.Contract, need.Contract) {
				existing.Ambiguous = true
				needs[need.Key] = existing
				return
			}
			if existing.Contract.ConID > 0 {
				return
			}
			if need.Contract.PrimaryExch == "" {
				need.Contract.PrimaryExch = existing.Contract.PrimaryExch
			}
		}
		needs[need.Key] = need
	}
	add(mandateIndexHistoryNeed())
	currencies := map[string]bool{}
	if pos != nil {
		for _, row := range pos.Stocks {
			if !positionQuotesAsStock(row) || positionWarningHasCode(row.WarningDetails, zeroValueStockPositionCode) {
				continue
			}
			sym := normSym(row.Symbol)
			currencies[normCcy(row.Currency)] = true
			if mandate.ReturnsSeriesFor(sym, row.Currency, lines[mandateLegID(row)]) != sym {
				continue // moved by its proxy's returns (mandate.ReturnsProxies), which the book always reads
			}
			add(mandateHistoryNeed{Key: "u:" + sym, Contract: rpc.ContractParams{ConID: row.ConID, Symbol: sym, SecType: "STK", Currency: normCcy(row.Currency), Exchange: "SMART", PrimaryExch: strings.ToUpper(strings.TrimSpace(row.PrimaryExch))}})
		}
		for _, row := range pos.Options {
			sym := normSym(row.Symbol)
			add(mandateHistoryNeed{Key: "u:" + sym, Contract: mandateOptionUnderlyingContract(sym, normCcy(row.Currency))})
			currencies[normCcy(row.Currency)] = true
		}
	}
	for ccy := range currencies {
		if ccy == "" || ccy == base || base == "" {
			continue
		}
		add(mandateHistoryNeed{Key: "fx:" + ccy, Contract: rpc.ContractParams{Symbol: base, SecType: "CASH", Currency: ccy, Exchange: "IDEALPRO"}})
	}
	out := make([]mandateHistoryNeed, 0, len(needs))
	for _, need := range needs {
		out = append(out, need)
	}
	slices.SortFunc(out, func(a, b mandateHistoryNeed) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// mandateOptionUnderlyingContract is the contract an option's underlying
// history is read for: a cash index (rpc.IndexUnderlyingContract: SPX, NDX,
// RUT, VIX) as the index, with the identity the replay and the market tape
// read it by; every other underlying as a stock. The broker lists no stock
// named VIX, so a stock read leaves the series missing and the verdict
// unknown.
func mandateOptionUnderlyingContract(sym, ccy string) rpc.ContractParams {
	if index, ok := rpc.IndexUnderlyingContract(sym); ok {
		index.PrimaryExch = ""
		return index
	}
	return rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: ccy, Exchange: "SMART"}
}

func (s *Server) mandateHistorySeries(ctx context.Context, key string) *mandateHistorySeries {
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.series == nil {
		st.series, st.revisions, st.loaded, st.attempts = map[string]*mandateHistorySeries{}, map[string]int64{}, map[string]bool{}, map[string]mandateHistoryAttempt{}
	}
	if !st.loaded[key] {
		st.loaded[key] = true
		if s.coreStore != nil {
			doc, ok, err := s.coreStore.GetStateDocument(ctx, mandateHistoryScope(key), stateKindMandateHistory)
			if err == nil && ok {
				var saved mandateHistorySeries
				if json.Unmarshal(doc.JSON, &saved) == nil && saved.Version == mandateHistoryDocVer && saved.Key == key && validMandateSeries(saved) == nil {
					st.series[key] = completedMandateSeries(&saved)
					st.revisions[key] = doc.Revision
				}
			}
		}
	}
	return st.series[key]
}

// completedMandateSeries also sanitizes documents retained by older builds:
// a bar dated after the last session completed at fetch time is not a close.
// Keep the original persisted observation until the normal refresh replaces it.
func completedMandateSeries(series *mandateHistorySeries) *mandateHistorySeries {
	last, _, covered := mandateLastSessionsIn(series.FetchedAt, mandateSeriesMarket(series))
	if !covered {
		return nil
	}
	n := len(series.Dates)
	for n > 0 && series.Dates[n-1] > last {
		n--
	}
	if n == 0 {
		return nil
	}
	if n == len(series.Dates) {
		return series
	}
	out := *series
	out.Dates, out.Closes = slices.Clone(series.Dates[:n]), slices.Clone(series.Closes[:n])
	return &out
}

func validMandateSeries(s mandateHistorySeries) error {
	if len(s.Dates) == 0 || len(s.Dates) != len(s.Closes) || len(s.Dates) > mandateHistoryKeep || s.FetchedAt.IsZero() {
		return errors.New("series shape or clock invalid")
	}
	for i := range s.Dates {
		if _, err := time.Parse(time.DateOnly, s.Dates[i]); err != nil || !isFinitePrice(s.Closes[i]) || i > 0 && s.Dates[i] <= s.Dates[i-1] {
			return errors.New("series dates or closes invalid")
		}
	}
	return nil
}

// saveMandateSeries writes one series through to SQLite and the cache. A
// failed write keeps the fresh series in memory for this process and says so.
func (s *Server) saveMandateSeries(ctx context.Context, series *mandateHistorySeries) error {
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.series == nil {
		st.series, st.revisions, st.loaded, st.attempts = map[string]*mandateHistorySeries{}, map[string]int64{}, map[string]bool{}, map[string]mandateHistoryAttempt{}
	}
	st.series[series.Key] = series
	st.loaded[series.Key] = true
	if s.coreStore == nil {
		return nil
	}
	raw, err := json.Marshal(series)
	if err != nil {
		return err
	}
	saved, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: mandateHistoryScope(series.Key), Kind: stateKindMandateHistory, ExpectedRevision: st.revisions[series.Key], JSON: raw})
	if err != nil {
		var conflict *corestore.RevisionConflictError
		if errors.As(err, &conflict) && conflict.Exists {
			st.revisions[series.Key] = conflict.Actual
		}
		return err
	}
	st.revisions[series.Key] = saved.Revision
	return nil
}

// mandateLastSessions returns the dates of the last completed US equity
// session and the one before it.
func mandateLastSessions(now time.Time) (last, previous string, ok bool) {
	return mandateLastSessionsIn(now, marketcal.MarketUSEquity)
}

// mandateLastSessionsIn returns the dates of the last completed session of
// a market and the one before it.
func mandateLastSessionsIn(now time.Time, market marketcal.Market) (last, previous string, ok bool) {
	session, _, found := lastCompletedMarketSessionWindow(now, market)
	if !found {
		return "", "", false
	}
	before, _, foundBefore := lastCompletedMarketSessionWindow(session.Close.Add(-time.Minute), market)
	if !foundBefore {
		return session.Date, "", false
	}
	return session.Date, before.Date, true
}

// mandateSeriesMarket is the calendar a series' freshness is judged on:
// the one its read named (the core sleeve's own series, core_target.go, on
// its listing exchange's), else the US one every series the book reads is
// judged on.
func mandateSeriesMarket(series *mandateHistorySeries) marketcal.Market {
	if series != nil && series.Market != "" {
		return series.Market
	}
	return marketcal.MarketUSEquity
}

// mandateSeriesStale reports whether a series lags more than one completed
// session, so the evening after a close is current until the refresh
// lands and the next morning is not. For the book the US calendar stands
// in for every venue (a European daily bar dates its own session, one
// holiday apart at most); the core's series is judged on its listing
// exchange's.
func mandateSeriesStale(series *mandateHistorySeries, now time.Time) bool {
	if series == nil || len(series.Dates) == 0 {
		return true
	}
	last, previous, ok := mandateLastSessionsIn(now, mandateSeriesMarket(series))
	if !ok {
		return true
	}
	newest := series.Dates[len(series.Dates)-1]
	return newest < previous && newest != last
}

// mandateSeriesDue reports whether the series lacks the last completed
// session, so a refresh is worth a broker read.
func mandateSeriesDue(series *mandateHistorySeries, now time.Time) bool {
	if series == nil || len(series.Dates) == 0 {
		return true
	}
	last, _, ok := mandateLastSessionsIn(now, mandateSeriesMarket(series))
	return ok && series.Dates[len(series.Dates)-1] < last
}

// mandateSeriesInput converts a persisted series to the evaluator's: an FX
// series (the base currency's pair, quoted as units of the other currency
// per base unit) is inverted to base units per unit of that currency.
func mandateSeriesInput(series *mandateHistorySeries, now time.Time) mandate.Series {
	out := mandate.Series{Key: series.Key, Dates: slices.Clone(series.Dates), Closes: slices.Clone(series.Closes), AsOf: series.FetchedAt, Stale: mandateSeriesStale(series, now)}
	if strings.HasPrefix(series.Key, "fx:") {
		for i, c := range out.Closes {
			out.Closes[i] = 1 / c
		}
	}
	return out
}

func (s *Server) startMandateHistoryRefresh(ctx context.Context) {
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(mandateHistoryRefreshTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s.refreshMandateHistoryOnce(ctx)
			s.refreshMandateVerdict(ctx)
			s.refreshStrategies(ctx)
		}
	})
}

// refreshMandateHistoryOnce reads every due series once, in key order, on
// the background lane. Nothing runs while the constitution carries no
// [mandate] table, so an inactive mandate costs no broker read.
func (s *Server) refreshMandateHistoryOnce(ctx context.Context) {
	if s.riskPolicies == nil || !s.riskPolicies.snapshot().policy.MandateActive() {
		return
	}
	c := s.gatewayConnector()
	if c == nil {
		return
	}
	pos, err := s.mandatePositions(ctx)
	if err != nil || pos == nil {
		return
	}
	base := ""
	if pos.Portfolio != nil {
		base = pos.Portfolio.BaseCurrency
	}
	// The core's contract first, from IBKR's record for its ISIN, so the
	// book's needs know its line and the core's read names the listing as
	// IBKR does (core_resolve.go).
	coreLine := s.coreResolve(ctx, s.riskPolicies.snapshot().policy)
	needs := s.mandateHistoryNeedsWithRequests(ctx, pos, base, s.nowUTC())
	if coreLine != nil {
		needs = append(needs, coreSeriesNeed(coreLine))
	}
	for _, need := range needs {
		if ctx.Err() != nil {
			return
		}
		s.refreshMandateSeries(ctx, c, need)
	}
}

// refreshMandateSeries reads one series when it is due and the session
// budget allows: one complete read per completed market session, failed attempts
// retried after mandateHistoryRetryAfter up to mandateHistoryAttemptsPerSession.
type mandateHistorySession interface {
	CaptureHistoricalSession() (ibkrlib.HistoricalSessionBinding, bool)
	HistoricalSessionCurrent(ibkrlib.HistoricalSessionBinding) bool
}

func (s *Server) refreshMandateSeries(ctx context.Context, c mandateHistorySession, need mandateHistoryNeed) {
	if need.Ambiguous {
		return
	}
	need = bindMandateHistoryNeed(need)
	now := s.nowUTC()
	series := s.mandateHistoryForNeed(ctx, need)
	if !mandateSeriesDue(series, now) {
		return
	}
	session, ok := c.CaptureHistoricalSession()
	if !ok {
		return
	}
	market := need.Market
	if market == "" {
		market = marketcal.MarketUSEquity
	}
	completed, _, covered := mandateLastSessionsIn(now, market)
	if !covered {
		return
	}
	if r, ok := s.mandateHistoryRefusalFor(ctx, need.Key); ok && r.Market == market && completed <= r.Session {
		return
	}
	st := &s.mandateHistory
	st.mu.Lock()
	attempt, seen := st.attempts[need.Key]
	sameSession := seen && c.HistoricalSessionCurrent(attempt.session) && attempt.session == session && attempt.market == market && completed <= attempt.completedSession
	if sameSession && (attempt.succeeded || attempt.attempts >= mandateHistoryAttemptsPerSession || now.Before(attempt.at.Add(mandateHistoryRetryAfter))) {
		st.mu.Unlock()
		return
	}
	if !sameSession {
		attempt = mandateHistoryAttempt{session: session, market: market, completedSession: completed}
	}
	attempt.at, attempt.attempts = now, attempt.attempts+1
	st.attempts[need.Key] = attempt
	st.mu.Unlock()

	if !s.readMandateSeries(ctx, need, now) {
		return
	}
	refreshed := s.mandateHistoryForNeed(ctx, need)
	complete := refreshed != nil && len(refreshed.Dates) > 0 && refreshed.Dates[len(refreshed.Dates)-1] >= completed
	st.mu.Lock()
	attempt = st.attempts[need.Key]
	attempt.succeeded = complete
	st.attempts[need.Key] = attempt
	st.mu.Unlock()
}

// readMandateSeries reads one series on the background lane and keeps it.
// A failed read, other than a gateway that is down or a stop, is kept as
// the series' last failure (the broker's answer, for a hold to name) until
// a read succeeds.
func (s *Server) readMandateSeries(ctx context.Context, need mandateHistoryNeed, now time.Time) bool {
	need = bindMandateHistoryNeed(need)
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), mandateHistoryRefreshWindow)
	fresh, err := s.fetchMandateSeries(readCtx, need, now)
	cancel()
	st := &s.mandateHistory
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			st.mu.Lock()
			if st.failures == nil {
				st.failures = map[string]mandateHistoryFailure{}
			}
			st.failures[need.Key] = mandateHistoryFailure{at: now, err: errText(err)}
			st.mu.Unlock()
			switch {
			case ibkrlib.HistoricalNotEntitled(err):
				s.noteMandateHistoryRefusal(ctx, need, now, err)
			case s.logger != nil:
				s.logger.Warnf("mandate history %s: %v; the verdict reads unknown until a read succeeds", need.Key, err)
			}
		}
		return false
	}
	if err := s.saveMandateSeries(ctx, fresh); err != nil && s.logger != nil {
		s.logger.Warnf("mandate history %s: read %d closes but the database save failed: %v", need.Key, len(fresh.Closes), err)
	}
	st.mu.Lock()
	delete(st.failures, need.Key)
	st.mu.Unlock()
	if r, ok := s.mandateHistoryRefusalFor(ctx, need.Key); ok {
		s.setMandateHistoryRefusal(ctx, need.Key, nil)
		if s.logger != nil {
			s.logger.Infof("mandate history %s: IBKR serves it again; the market data permission refusal of %s is cleared", need.Key, r.At.UTC().Format(time.DateTime))
		}
	}
	return true
}

// noteMandateHistoryRefusal records a series IBKR refused for want of a
// market data permission as final for the session it answered. The first
// refusal warns once, with what to do; a repeat at a later session is INFO.
func (s *Server) noteMandateHistoryRefusal(ctx context.Context, need mandateHistoryNeed, now time.Time, err error) {
	market := need.Market
	if market == "" {
		market = marketcal.MarketUSEquity
	}
	completed, _, _ := mandateLastSessionsIn(now, market)
	_, known := s.mandateHistoryRefusalFor(ctx, need.Key)
	s.setMandateHistoryRefusal(ctx, need.Key, &mandateHistoryRefusal{At: now, Error: errText(err), Session: completed, Market: market})
	if s.logger == nil {
		return
	}
	if known {
		s.logger.Infof("mandate history %s: still no market data permission at IBKR (%v); asked again at the next trading session", need.Key, err)
		return
	}
	s.logger.Warnf("mandate history %s: IBKR has no market data permission for it (%v); the verdict reads unknown. Add the exchange's market data subscription at IBKR; Canary asks once each trading session and stops warning", need.Key, err)
}

// mandateHistoryRefusalFor returns the standing permission refusal of the
// series, loading the record on first use. A server without the SQLite
// authority keeps it in memory.
func (s *Server) mandateHistoryRefusalFor(ctx context.Context, key string) (mandateHistoryRefusal, bool) {
	s.loadMandateHistoryRefusals(ctx)
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.refusals[key]
	return r, ok
}

func (s *Server) loadMandateHistoryRefusals(ctx context.Context) {
	st := &s.mandateHistory
	st.mu.Lock()
	loaded := st.refusalsLoaded
	st.mu.Unlock()
	if loaded {
		return
	}
	records, revision := map[string]mandateHistoryRefusal{}, int64(0)
	if s.coreStore != nil {
		doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindMandateHistoryRefusals)
		if err != nil {
			if s.logger != nil {
				s.logger.Warnf("mandate history refusals: %v", err)
			}
			return
		}
		if ok {
			if err := json.Unmarshal(doc.JSON, &records); err != nil && s.logger != nil {
				s.logger.Warnf("mandate history refusals: decode: %v", err)
			}
			revision = doc.Revision
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.refusalsLoaded {
		st.refusals, st.refusalsVersion, st.refusalsLoaded = records, revision, true
	}
}

// setMandateHistoryRefusal records r for the series, or clears it when r is
// nil, and writes the record through.
func (s *Server) setMandateHistoryRefusal(ctx context.Context, key string, r *mandateHistoryRefusal) {
	s.loadMandateHistoryRefusals(ctx)
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.refusals == nil {
		st.refusals = map[string]mandateHistoryRefusal{}
	}
	if r == nil {
		delete(st.refusals, key)
	} else {
		st.refusals[key] = *r
	}
	if s.coreStore == nil {
		return
	}
	raw, err := json.Marshal(st.refusals)
	if err == nil {
		var doc corestore.StateDocument
		doc, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindMandateHistoryRefusals, ExpectedRevision: st.refusalsVersion, JSON: raw})
		if err == nil {
			st.refusalsVersion = doc.Revision
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Warnf("mandate history refusals: save: %v", err)
	}
}

// mandateSeriesFailure is a series' last failed read since its last
// successful one: the broker's answer and when it came.
func (s *Server) mandateSeriesFailure(key string) (string, time.Time) {
	st := &s.mandateHistory
	st.mu.Lock()
	f := st.failures[key]
	st.mu.Unlock()
	if f.err == "" {
		if r, ok := s.mandateHistoryRefusalFor(context.Background(), key); ok {
			return r.Error, r.At
		}
	}
	return f.err, f.at
}

// fetchMandateSeries reads mandateHistoryFetchDays of daily closes through
// the charts' broker fetch helper and keeps the newest mandateHistoryKeep.
func (s *Server) fetchMandateSeries(ctx context.Context, need mandateHistoryNeed, now time.Time) (*mandateHistorySeries, error) {
	need = bindMandateHistoryNeed(need)
	if need.Ambiguous {
		return nil, errors.New("history identity is ambiguous")
	}
	fetch := s.fetchMarketHistoryDays
	if s.mandateHistoryFetchForTest != nil {
		fetch = s.mandateHistoryFetchForTest
	}
	res, err := fetch(ctx, rpc.MarketHistoryParams{Contract: need.Contract, Range: "5Y"}, -mandateHistoryFetchDays, now)
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.Points) == 0 {
		return nil, errors.New("no daily bars")
	}
	if !mandateHistoryContractMatches(res.Contract, need.Contract) {
		return nil, errors.New("daily history belongs to a different or unproven contract")
	}
	if res.Interval != "1 day" {
		return nil, fmt.Errorf("broker answered with %s bars, not daily", res.Interval)
	}
	series := &mandateHistorySeries{Version: mandateHistoryDocVer, Key: need.Key, Contract: res.Contract, FetchedAt: res.AsOf, Source: res.Source, Market: need.Market}
	for _, p := range res.Points {
		if !isFinitePrice(p.Value) || math.IsNaN(p.Value) {
			return nil, fmt.Errorf("invalid close on %s", p.At.UTC().Format(time.DateOnly))
		}
		day := p.At.UTC().Format(time.DateOnly)
		if n := len(series.Dates); n > 0 && day <= series.Dates[n-1] {
			return nil, errors.New("unordered daily bars")
		}
		series.Dates = append(series.Dates, day)
		series.Closes = append(series.Closes, p.Value)
	}
	if len(series.Dates) > mandateHistoryKeep {
		series.Dates = series.Dates[len(series.Dates)-mandateHistoryKeep:]
		series.Closes = series.Closes[len(series.Closes)-mandateHistoryKeep:]
	}
	if err := validMandateSeries(*series); err != nil {
		return nil, err
	}
	if series.FetchedAt.After(now) {
		series.FetchedAt = now
	}
	series = completedMandateSeries(series)
	if series == nil {
		return nil, errors.New("no completed daily bars")
	}
	return series, nil
}

// mandatePositions is the positions read the mandate uses: the full view
// with marks, IV, spot and FX rates, bounded by the method's own deadline.
func (s *Server) mandatePositions(ctx context.Context) (*rpc.PositionsResult, error) {
	readCtx, cancel := requestCtx(ctx, rpc.MethodPositionsList)
	defer cancel()
	if s.mandatePositionsForTest != nil {
		return s.mandatePositionsForTest(readCtx)
	}
	return s.handlePositionsList(readCtx, &rpc.Request{})
}
