package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// defaultExpiryIVCap is how many expiries get IV by default — the front
// weeklies, the next few monthlies, plus the next quarterly. Anything
// further out is rarely on the decision path and would burn slot budget
// on every chain refresh. Callers can override via params.AllExpiries.
const defaultExpiryIVCap = 12

// chainExpiryWorkers bounds the per-expiry ATM IV fetcher fan-out.
// The IBKR market-data farm throttles aggressive subscribe churn; 4
// concurrent legs is the documented safe ceiling we already use for the
// chain strikes loop (see handleChainFetch). Higher values trigger
// "market data subscription limit reached" against the entitled slot
// count well before the fan-out wins anything.
const chainExpiryWorkers = 4

// handleChainExpiries returns the sorted, deduped option expiries for the
// underlying. WithIV (default-on via CLI) fetches per-expiry ATM implied
// volatility through a bounded worker pool, with daemon-side caching so
// the second invocation within the TTL is instant. AllExpiries lifts the
// default 12-expiry cap. On any per-strike error the row keeps IV=nil
// with IVStatus="timeout"|"unavailable" — never fail the whole call.
func (s *Server) handleChainExpiries(ctx context.Context, req *rpc.Request) (*rpc.ChainExpiriesResult, error) {
	var p rpc.ChainExpiriesParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	sym := normSym(p.Symbol)
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	if p.MinDTE < 0 || p.MaxDTE < 0 || p.TargetDTE < 0 {
		return nil, errBadRequest("min_dte, max_dte, and target_dte must be >= 0")
	}
	if p.MinDTE > 0 && p.MaxDTE > 0 && p.MinDTE > p.MaxDTE {
		return nil, errBadRequest("min_dte must be <= max_dte")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}

	// Per-stage budget visibility. Captured at INFO so an off-hours
	// timeout shows immediately where the 25 s budget went — "spent 8 s
	// in expiries+strikes, 0 ms in IV fan-out" tells investigators the
	// SECDEF farm is sick, not the IV path.
	start := time.Now()
	var spotMs, expiriesMs, fanoutMs int64
	defer func() {
		s.logger.Infof("chain.expiries %s done in %dms (expiries+strikes=%dms, spot=%dms, iv-fanout=%dms)",
			sym, time.Since(start).Milliseconds(), expiriesMs, spotMs, fanoutMs)
	}()

	tExpiries := time.Now()
	expiries, strikesByExpiry, err := fetchExpiriesAndStrikes(c, sym, 12*time.Second)
	expiriesMs = time.Since(tExpiries).Milliseconds()
	if err != nil {
		return nil, wrapChainExpiriesErr(sym, err)
	}
	today := todayLocal()
	expiries = selectChainExpiriesByDTE(expiries, today, p.MinDTE, p.MaxDTE, p.TargetDTE)

	res := &rpc.ChainExpiriesResult{
		Symbol:   sym,
		AsOf:     time.Now(),
		Expiries: make([]rpc.ChainExpiry, 0, len(expiries)),
	}

	if !p.WithIV {
		for _, e := range expiries {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
		return res, nil
	}

	// Cap the expiry list before IV fetch so the slow path stays bounded.
	// expiries is already sorted ascending by fetchExpiriesAndStrikes, so
	// slicing from the front picks the nearest expiries which is what
	// users actually evaluate.
	work := expiries
	if !p.AllExpiries && len(work) > defaultExpiryIVCap {
		work = work[:defaultExpiryIVCap]
	}
	if p.RequireLiveIV && !rpc.IsOptionRTH(time.Now()) {
		for _, e := range work {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{
				Date:      e,
				DTE:       dteFromDate(today, e),
				IVStatus:  "unavailable",
				IVSource:  "unavailable",
				IVQuality: "unavailable",
			})
		}
		res.WarningDetails = append(res.WarningDetails, rpc.DataWarning{
			Code:     "live_option_iv_unavailable",
			Scope:    sym,
			Severity: "data_quality",
			Message:  "Live option IV is unavailable because the regular U.S. option-data surface is outside RTH.",
			Impact:   "Products such as SPX may have extended sessions, but expiry IV and 1-sigma moves need a broad live quote/model surface.",
			Action:   "Retry during 09:30-16:00 ET for the most complete quote/OI/IV surface, or omit require_live_iv for an off-hours/test run.",
		})
		return res, nil
	}

	// Spot is required to pick the ATM strike. A single brief subscribe
	// shared across all expiries — pre-fix this ran once before the loop
	// already; only the loop changed shape (parallel + cached).
	tSpot := time.Now()
	spot := s.chainSpotForATM(ctx, c, sym, 5*time.Second)
	spotMs = time.Since(tSpot).Milliseconds()
	if spot.Price > 0 {
		res.Spot = spot.Price
		res.SpotSource = spot.Source
		res.SpotAsOf = spot.AsOf
	}

	now := time.Now()
	rows := make([]rpc.ChainExpiry, len(work))
	type job struct {
		idx       int
		expiry    string
		expiryYMD string
		atm       float64
	}
	var jobs []job
	for i, e := range work {
		row := rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)}
		// Cache lookup first — a hit avoids the round-trip entirely.
		if cached, ok := s.expiryIVs.get(sym, e, now); ok {
			if cached.iv > 0 {
				v := cached.iv
				row.IV = &v
			}
			row.IVStatus = cached.status
			row.IVSource = "cached"
			if cached.status == "ok" {
				row.IVQuality = "cached"
			} else {
				row.IVQuality = "unavailable"
			}
			row.IVAsOf = cached.asOf
			rows[i] = row
			continue
		}
		strikes := strikesByExpiry[e]
		if spot.Price <= 0 || len(strikes) == 0 {
			row.IVStatus = "unavailable"
			row.IVSource = "unavailable"
			row.IVQuality = "unavailable"
			row.IVAsOf = now
			rows[i] = row
			// Negative-cache so we don't re-poll every refresh.
			s.expiryIVs.put(sym, e, expiryIVEntry{status: "unavailable", source: "unavailable", quality: "unavailable", asOf: now}, now)
			continue
		}
		atm := closestStrike(strikes, spot.Price)
		expiryYMD := strings.ReplaceAll(e, "-", "")
		rows[i] = row // populate placeholder; worker will overwrite IV/IVStatus
		jobs = append(jobs, job{idx: i, expiry: e, expiryYMD: expiryYMD, atm: atm})
	}

	// Workers write index-disjoint rows[j.idx], so no per-write mutex is
	// needed — wg.Wait inside runBounded provides happens-before to the
	// caller. The expiryIVs cache is responsible for its own locking.
	tFanout := time.Now()
	runBounded(jobs, chainExpiryWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		obs := collectExpiryATMIV(ctx, c, sym, j.expiryYMD, j.atm, 2*time.Second)
		entry := expiryIVEntry{status: obs.status, source: obs.source, quality: obs.quality, asOf: obs.asOf}
		if obs.iv != nil {
			entry.iv = *obs.iv
		}
		s.expiryIVs.put(sym, j.expiry, entry, time.Now())
		if obs.iv != nil {
			rows[j.idx].IV = obs.iv
		}
		rows[j.idx].IVStatus = obs.status
		rows[j.idx].IVSource = obs.source
		rows[j.idx].IVQuality = obs.quality
		rows[j.idx].IVAsOf = obs.asOf
	})
	fanoutMs = time.Since(tFanout).Milliseconds()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decorate each row with the 1-σ implied move now that IV is settled.
	// Pure derivation from spot + IV + DTE — no extra round trips. Skips
	// rows missing any of the three so the field stays nil rather than
	// silently absorbing a zero.
	for i := range rows {
		if mv, mvPct, ok := computeImpliedMove(spot.Price, rows[i].IV, rows[i].DTE); ok {
			rows[i].ImpliedMove = &mv
			rows[i].ImpliedMovePct = &mvPct
		}
	}
	res.WarningDetails = append(res.WarningDetails, chainSpotWarning(sym, spot)...)
	res.WarningDetails = append(res.WarningDetails, annotateRepeatedExpiryIV(sym, rows)...)
	res.WarningDetails = append(res.WarningDetails, chainExpiryIVWarnings(sym, rows, p.RequireLiveIV)...)

	// Append the working set, then the rest (without IV) when caller
	// asked for the full list. AllExpiries=false drops the tail.
	res.Expiries = append(res.Expiries, rows...)
	if p.AllExpiries && len(expiries) > len(work) {
		for _, e := range expiries[len(work):] {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
	}
	return res, nil
}

func selectChainExpiriesByDTE(expiries []string, today time.Time, minDTE, maxDTE, targetDTE int) []string {
	if minDTE <= 0 && maxDTE <= 0 && targetDTE <= 0 {
		return expiries
	}
	filtered := make([]string, 0, len(expiries))
	for _, expiry := range expiries {
		dte := dteFromDate(today, expiry)
		if minDTE > 0 && dte < minDTE {
			continue
		}
		if maxDTE > 0 && dte > maxDTE {
			continue
		}
		filtered = append(filtered, expiry)
	}
	if targetDTE <= 0 || len(filtered) <= 1 {
		return filtered
	}
	best := filtered[0]
	bestDistance := absInt(dteFromDate(today, best) - targetDTE)
	for _, expiry := range filtered[1:] {
		distance := absInt(dteFromDate(today, expiry) - targetDTE)
		if distance < bestDistance {
			best = expiry
			bestDistance = distance
		}
	}
	return []string{best}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func chainExpiryIVWarnings(symbol string, rows []rpc.ChainExpiry, requireLive bool) []rpc.DataWarning {
	if len(rows) == 0 {
		return nil
	}
	var usable int
	for _, row := range rows {
		if row.IV != nil && row.ImpliedMove != nil && row.IVQuality != "unavailable" {
			usable++
		}
	}
	if usable > 0 {
		return nil
	}
	code := "expiry_iv_unavailable"
	if requireLive {
		code = "live_option_iv_unavailable"
	}
	return []rpc.DataWarning{{
		Code:     code,
		Scope:    symbol,
		Severity: "data_quality",
		Message:  "No requested expiries returned usable IV and 1-sigma implied moves.",
		Impact:   "The chain is not sufficient for option instrument or strike selection.",
		Action:   "Retry during U.S. option regular trading hours, or use shares-only/test-mode logic.",
	}}
}

// todayLocal returns today's date at midnight local time. Surfaced as a
// helper so dteFromDate and the no-IV / AllExpiries-tail paths agree on
// the reference instant — they all read the same wall clock at handler
// entry.
func todayLocal() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// dteFromDate returns the calendar day count from today's local date to
// the YYYY-MM-DD expiry. Same-day returns 0; one calendar day out returns
// 1. Returns 0 on parse failure or expired dates — callers treat 0 as
// "intraday" and downstream math (sqrt(DTE/365)) safely yields 0 too.
func dteFromDate(today time.Time, expiry string) int {
	t, err := time.ParseInLocation("2006-01-02", expiry, today.Location())
	if err != nil {
		return 0
	}
	days := int(t.Sub(today).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// computeImpliedMove returns the 1-σ expected dollar move by expiration,
// computed from spot × IV × √(DTE/365). Industry-standard "expected move
// by expiry" formula — same shape the CBOE option calculator uses.
//
// Returns (move, movePct, true) when spot > 0, IV is non-nil and > 0,
// and DTE >= 0. The percent value is `move / spot` (a fraction, so 0.042
// means 4.2%). A DTE of 0 yields a zero move, which is correct: at expiry
// the option's time value collapses to intrinsic.
func computeImpliedMove(spot float64, iv *float64, dte int) (float64, float64, bool) {
	if spot <= 0 || iv == nil || *iv <= 0 || dte < 0 {
		return 0, 0, false
	}
	mv := spot * (*iv) * math.Sqrt(float64(dte)/365.0)
	return mv, mv / spot, true
}

// fetchExpiriesAndStrikes is a small seam for tests — the connector's
// FetchOptionExpiries and FetchOptionExpiryStrikes share an internal fetcher,
// but the daemon needs both halves and the connector public surface returns
// them via separate calls. We do one round trip via the strikes path (which
// is a superset) and derive the sorted-expiry list from the map keys.
var fetchExpiriesAndStrikes = func(connector chainExpiriesConnector, symbol string, timeout time.Duration) ([]string, map[string][]float64, error) {
	strikes, err := connector.FetchOptionExpiryStrikes(symbol, timeout)
	if err != nil {
		return nil, nil, err
	}
	expiries := make([]string, 0, len(strikes))
	for k := range strikes {
		expiries = append(expiries, k)
	}
	slices.Sort(expiries)
	return expiries, strikes, nil
}

// chainExpiriesConnector is the narrow connector surface handleChainExpiries
// uses. Defined here (not in pkg/ibkr) so tests can stub the daemon side
// without lifting the dependency back into the library.
type chainExpiriesConnector interface {
	FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error)
}

// wrapChainExpiriesErr turns the low-level pkg/ibkr errors that surface from
// the chain-expiries fetch into something a user can act on. The big one:
// ErrContractDetailsTimeout, which happens when the IBKR security-definition
// data farm is degraded (often pre-market or just after gateway start). The
// underlying quote subscription typically works in this state — the chain
// path is a separate gateway request that depends on contract resolution.
// Surfacing a generic "internal: timeout" leaves the user guessing whether
// it's a bug, a bad symbol, or a transient gateway condition.
func wrapChainExpiriesErr(symbol string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ibkrlib.ErrContractDetailsTimeout) {
		return &chainContractTimeoutError{symbol: symbol, cause: err}
	}
	return err
}

// chainContractTimeoutError carries the gateway-side "no security definitions"
// case. classifyError maps it to rpc.CodeTimeout so the CLI can recognise it
// distinctly from CodeInternal. The Error() text is the user-facing message —
// keep it short, name the symbol, and point at a concrete next step.
type chainContractTimeoutError struct {
	symbol string
	cause  error
}

func (e *chainContractTimeoutError) Error() string {
	return fmt.Sprintf("option chain unavailable for %s: gateway did not deliver security definitions in time. This is usually transient — try again in a moment, or run `canary status` to verify the gateway connection.", e.symbol)
}

func (e *chainContractTimeoutError) Unwrap() error { return e.cause }

// closestStrike picks the strike closest to spot. For ties (which only happens
// when strikes straddle spot equidistantly) the lower strike wins for
// determinism — IBKR's IV surface is symmetric enough that this rarely matters.
func closestStrike(strikes []float64, spot float64) float64 {
	best := strikes[0]
	bestDist := math.Abs(best - spot)
	for _, k := range strikes[1:] {
		d := math.Abs(k - spot)
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

type expiryIVObservation struct {
	iv      *float64
	status  string
	source  string
	quality string
	asOf    time.Time
}

func unavailableExpiryIV(status string) expiryIVObservation {
	if status == "" {
		status = "unavailable"
	}
	return expiryIVObservation{
		status:  status,
		source:  "unavailable",
		quality: "unavailable",
		asOf:    time.Now(),
	}
}

// collectExpiryATMIV subscribes to the ATM option for one expiry, polls the
// connector's IV cache for up to perStrikeTimeout, then unsubscribes. The IV is
// routed through a per-contract key so concurrent expiry fan-out for the same
// underlying cannot overwrite a sibling expiry's model tick.
func collectExpiryATMIV(ctx context.Context, c *ibkrlib.Connector, symbol, expiryYMD string, strike float64, perStrikeTimeout time.Duration) expiryIVObservation {
	expiryT, err := time.Parse("20060102", expiryYMD)
	if err != nil {
		return unavailableExpiryIV("unavailable")
	}
	reqID, key, err := c.SubscribeOptionIVKeyed(ctx, symbol, expiryT, strike, "C")
	if err != nil {
		return unavailableExpiryIV("unavailable")
	}
	// reqID-scoped cancel: the 4-worker fan-out at collectExpiryIVs runs
	// multiple expiries against the same underlier concurrently. A
	// symbol-scoped UnsubscribeMarketData here would either no-op (the
	// common case, since SubscribeOptionIV doesn't install a streaming
	// entry under the symbol) or — worse — tear down an unrelated
	// quote --watch subscription.
	defer c.CancelOptionIV(reqID)

	deadline := time.Now().Add(perStrikeTimeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		if iv, ok := c.OptionIV(key); ok && iv > 0 {
			v := iv
			return expiryIVObservation{
				iv:      &v,
				status:  "ok",
				source:  "live_model",
				quality: "live_model",
				asOf:    time.Now(),
			}
		}
		if time.Now().After(deadline) {
			return unavailableExpiryIV("timeout")
		}
		select {
		case <-ctx.Done():
			return unavailableExpiryIV("timeout")
		case <-poll.C:
		}
	}
}

func annotateRepeatedExpiryIV(symbol string, rows []rpc.ChainExpiry) []rpc.DataWarning {
	type bucket struct {
		count int
		iv    float64
	}
	counts := make(map[string]bucket)
	for _, row := range rows {
		if row.IV == nil || *row.IV <= 0 {
			continue
		}
		key := fmt.Sprintf("%.6f", *row.IV)
		b := counts[key]
		b.count++
		b.iv = *row.IV
		counts[key] = b
	}
	var out []rpc.DataWarning
	for key, b := range counts {
		if b.count < 3 {
			continue
		}
		for i := range rows {
			if rows[i].IV == nil || fmt.Sprintf("%.6f", *rows[i].IV) != key {
				continue
			}
			rows[i].IVQuality = "reused_fallback"
		}
		out = append(out, rpc.DataWarning{
			Code:     "repeated_expiry_iv",
			Scope:    symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("ATM IV %.1f%% repeated across %d expiries.", b.iv*100, b.count),
			Impact:   "IBKR may have reused a fallback/model value; term-structure and LEAPS-vs-stock decisions are degraded.",
			Action:   "Inspect the strike grid for live bid/ask before using these IVs for structure selection.",
		})
	}
	return out
}

// handleChainFetch returns ATM ± width strikes for the specified expiry.
// Greeks are populated only when IBKR delivers them.
func (s *Server) handleChainFetch(ctx context.Context, req *rpc.Request) (*rpc.ChainResult, error) {
	var p rpc.ChainFetchParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Symbol == "" {
		return nil, errBadRequest("symbol required")
	}
	if p.Width < 0 {
		return nil, errBadRequest("width must be >= 0")
	}
	if p.Side == "" {
		p.Side = "both"
	}
	if !validChainSide(p.Side) {
		return nil, errBadRequest("side must be calls, puts, or both")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	expiryYMD, err := normalizeExpiry(p.Expiry)
	if err != nil {
		return nil, err
	}
	dte := daysUntil(expiryYMD)

	// Per-stage budget visibility (same rationale as handleChainExpiries).
	// "snapshot=4s, legs=21s" tells investigators where the 25 s budget
	// went without re-running with debug logging.
	start := time.Now()
	sym := strings.ToUpper(p.Symbol)
	var snapshotMs, legsMs int64
	defer func() {
		s.logger.Infof("chain.fetch %s %s done in %dms (snapshot=%dms, legs=%dms)",
			sym, expiryYMD, time.Since(start).Milliseconds(), snapshotMs, legsMs)
	}()

	tSnapshot := time.Now()
	spot := s.chainSpotForATM(ctx, c, sym, 5*time.Second)
	snapshotMs = time.Since(tSnapshot).Milliseconds()
	if spot.Price <= 0 {
		if s.gatewayConnector() == nil {
			return nil, s.gatewayUnavailableError()
		}
		return nil, fmt.Errorf("no spot price available for %s (market closed or symbol inactive)", p.Symbol)
	}
	step := strikeStep(spot.Price)
	atm := math.Round(spot.Price/step) * step
	requestedClass := strings.ToUpper(strings.TrimSpace(p.TradingClass))
	surface, err := selectChainFetchSurface(c, sym, expiryYMD, requestedClass, spot.Price, p.Width, time.Now())
	if err != nil {
		return nil, err
	}

	res := &rpc.ChainResult{
		Symbol:       sym,
		TradingClass: surface.tradingClass,
		Spot:         spot.Price,
		SpotSource:   spot.Source,
		SpotAsOf:     spot.AsOf,
		Expiry:       displayExpiry(expiryYMD),
		DTE:          dte,
		DataType:     spot.DataType,
		FeedType:     spot.FeedType,
		SessionState: rpc.ClassifySession(time.Now()).String(),
		AsOf:         time.Now(),
	}
	if !rpc.IsOptionRTH(res.AsOf) {
		if res.DataType != "" {
			res.FeedType = res.DataType
		}
		res.DataType = rpc.MarketDataClosed
	}

	wantCalls := p.Side == "calls" || p.Side == "both"
	wantPuts := p.Side == "puts" || p.Side == "both"

	// Pre-build the strike grid in display order, then fan the per-leg
	// fills out to a bounded worker pool. Pre-fix the loop was sequential
	// — for width=5 both sides that's 22 legs × ~2.5s each ≈ 55s,
	// reliably blowing past the CLI's 60s budget. 4 concurrent legs is
	// the documented safe gateway throttle (v0.2 backlog notes); the
	// gateway-side rate limiter (AcquireMarketDataSlot) serialises
	// further if we'd exceed the entitled slot count.
	res.Strikes = surface.strikes
	if len(res.Strikes) == 0 {
		res.Strikes = heuristicChainRows(atm, step, p.Width)
	}

	type job struct {
		idx   int
		right string
	}
	var jobs []job
	for idx := range res.Strikes {
		if wantCalls {
			jobs = append(jobs, job{idx: idx, right: "C"})
		}
		if wantPuts {
			jobs = append(jobs, job{idx: idx, right: "P"})
		}
	}

	// Two workers can target the same strike (one C-leg, one P-leg)
	// writing disjoint fields. Go's memory model still requires a
	// happens-before for the publish, so one mutex around mergeStrikeSide
	// is plenty — contention is bounded at one merge per leg.
	var mergeMu sync.Mutex
	tLegs := time.Now()
	runBounded(jobs, 4, func(j job) {
		if ctx.Err() != nil {
			return
		}
		var local rpc.ChainStrike
		local.Strike = res.Strikes[j.idx].Strike
		fillOptionLeg(ctx, c, &local, sym, surface.tradingClass, expiryYMD, local.Strike, j.right)
		mergeMu.Lock()
		mergeStrikeSide(&res.Strikes[j.idx], &local, j.right)
		mergeMu.Unlock()
	})
	legsMs = time.Since(tLegs).Milliseconds()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res.TradableSummary, res.LiquiditySummary = chainSummaries(res, wantCalls, wantPuts)
	res.WarningDetails = append(surface.warnings, chainWarningDetails(res, wantCalls, wantPuts)...)
	return res, nil
}

type chainSpotSelection struct {
	Price    float64
	DataType string
	FeedType string
	Source   string
	AsOf     time.Time
}

func (s *Server) chainSpotForATM(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) chainSpotSelection {
	sym := normSym(symbol)
	if sym == "" {
		return chainSpotSelection{}
	}
	if spot := s.chainSnapshotSpot(ctx, c, sym, timeout); spot.Price > 0 {
		return spot
	}
	if spot := s.chainQuoteSpotFallback(ctx, sym, timeout); spot.Price > 0 {
		return spot
	}
	if px, feedType := chainHistoricalSpotFallback(ctx, c, sym, timeout); px > 0 {
		return chainSpotSelection{
			Price:    px,
			DataType: rpc.MarketDataPrevClose,
			FeedType: feedType,
			Source:   "historical_close",
			AsOf:     time.Now(),
		}
	}
	return chainSpotSelection{}
}

func (s *Server) chainSnapshotSpot(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) chainSpotSelection {
	if s == nil || s.subs == nil {
		var warnf func(string, ...any)
		if s != nil {
			warnf = s.warnf
		}
		bid, ask, last, mark, closePx, dt := briefSnapshotFull(ctx, c, symbol, timeout, warnf)
		return chainSpotFromSnapshot(bid, ask, last, mark, closePx, dt, time.Now())
	}
	release, err := s.subs.Hold(ctx, symbol)
	if err != nil {
		return chainSpotSelection{}
	}
	defer release()
	bid, ask, last, mark, closePx, dt := briefSnapshotFullHeld(ctx, c, symbol, timeout)
	return chainSpotFromSnapshot(bid, ask, last, mark, closePx, dt, time.Now())
}

func chainSpotFromSnapshot(bid, ask, last, mark, closePx float64, dataType string, asOf time.Time) chainSpotSelection {
	if dataType == "" {
		dataType = rpc.MarketDataLive
	}
	switch {
	case last > 0:
		return chainSpotSelection{Price: last, DataType: dataType, Source: "last", AsOf: asOf}
	case bid > 0 && ask > 0:
		return chainSpotSelection{Price: (bid + ask) / 2, DataType: dataType, Source: "mid", AsOf: asOf}
	case bid > 0:
		return chainSpotSelection{Price: bid, DataType: dataType, Source: "bid", AsOf: asOf}
	case ask > 0:
		return chainSpotSelection{Price: ask, DataType: dataType, Source: "ask", AsOf: asOf}
	case mark > 0:
		return chainSpotSelection{Price: mark, DataType: dataType, Source: "mark", AsOf: asOf}
	case closePx > 0:
		return chainSpotSelection{Price: closePx, DataType: rpc.MarketDataPrevClose, FeedType: dataType, Source: "prev_close", AsOf: asOf}
	default:
		return chainSpotSelection{}
	}
}

func (s *Server) chainQuoteSpotFallback(ctx context.Context, symbol string, timeout time.Duration) chainSpotSelection {
	timeoutMs := int(timeout / time.Millisecond)
	if timeoutMs <= 0 {
		timeoutMs = int((5 * time.Second) / time.Millisecond)
	}
	params, err := json.Marshal(rpc.QuoteSnapshotParams{
		Contract:  rpc.ContractParams{Symbol: symbol},
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		return chainSpotSelection{}
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{ID: "chain-spot-fallback", Method: rpc.MethodQuoteSnapshot, Params: params})
	if err != nil {
		return chainSpotSelection{}
	}
	return chainSpotFromQuote(q)
}

func chainSpotFromQuote(q *rpc.Quote) chainSpotSelection {
	if q == nil || q.Price == nil || *q.Price <= 0 {
		return chainSpotSelection{}
	}
	dataType := q.DataType
	if dataType == "" {
		dataType = rpc.MarketDataLive
	}
	source := q.PriceSource
	if source == "" {
		source = "quote"
	}
	spot := chainSpotSelection{
		Price:    *q.Price,
		DataType: dataType,
		FeedType: q.FeedType,
		Source:   source,
		AsOf:     q.PriceAt,
	}
	if source == "prev_close" || source == "historical_close" || dataType == rpc.MarketDataPrevClose {
		spot.DataType = rpc.MarketDataPrevClose
		if spot.FeedType == "" && dataType != rpc.MarketDataPrevClose {
			spot.FeedType = dataType
		}
	}
	if spot.AsOf.IsZero() {
		spot.AsOf = q.AsOf
	}
	return spot
}

func chainHistoricalSpotFallback(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) (float64, string) {
	if c == nil || !chainCanUseHistoricalSpot(marketcal.MarketUSEquity, time.Now()) {
		return 0, ""
	}
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	fallbackCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bars, err := c.FetchHistoricalDailyBars(fallbackCtx, symbol, 10, 0)
	if err != nil {
		return 0, ""
	}
	return chainHistoricalSpotFromBars(bars)
}

func chainCanUseHistoricalSpot(market marketcal.Market, at time.Time) bool {
	session, err := marketcal.New().SessionAt(market, at)
	if err != nil || session.State == marketcal.StateUnknown {
		return false
	}
	return !session.IsOpen
}

func chainHistoricalSpotFromBars(bars []ibkrlib.HistoricalBar) (float64, string) {
	for _, bar := range slices.Backward(bars) {
		if bar.Close > 0 {
			return bar.Close, rpc.MarketDataFrozen
		}
	}
	return 0, ""
}

type chainFetchSurface struct {
	tradingClass string
	strikes      []rpc.ChainStrike
	warnings     []rpc.DataWarning
}

func selectChainFetchSurface(c *ibkrlib.Connector, symbol, expiryYMD, requestedClass string, spot float64, width int, now time.Time) (chainFetchSurface, error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	class := strings.ToUpper(strings.TrimSpace(requestedClass))
	if class == "" {
		class = sym
	}

	if sym != "SPX" {
		classed, err := c.FetchOptionExpiryStrikesClassed(sym, 10*time.Second)
		if err != nil {
			step := strikeStep(spot)
			atm := math.Round(spot/step) * step
			return chainFetchSurface{
				tradingClass: class,
				strikes:      heuristicChainRows(atm, step, width),
				warnings: []rpc.DataWarning{{
					Code:     "classed_chain_unavailable",
					Scope:    sym,
					Severity: "data_quality",
					Message:  "Option-chain strike enumeration was unavailable; falling back to a generic strike grid.",
					Impact:   "The displayed strikes may skip actively listed contracts near the money.",
					Action:   "Retry after the security-definition farm recovers; compare the strike grid against TWS.",
				}},
			}, nil
		}
		expiryDate := displayExpiry(expiryYMD)
		entries := normalisedSPXChainEntries(classed[expiryDate], sym)
		if len(entries) == 0 {
			return chainFetchSurface{}, errBadRequest(fmt.Sprintf("expiry %s is not listed for %s", expiryDate, sym))
		}
		entry, auto, err := selectDefaultChainEntry(sym, entries, class, requestedClass != "", expiryDate)
		if err != nil {
			return chainFetchSurface{}, err
		}
		out := chainFetchSurface{
			tradingClass: entry.TradingClass,
			strikes:      chainRowsFromListedStrikes(entry.Strikes, spot, width),
		}
		if auto {
			out.warnings = append(out.warnings, rpc.DataWarning{
				Code:     "trading_class_auto",
				Scope:    sym,
				Severity: "info",
				Message:  fmt.Sprintf("%s expiry %s lists multiple trading classes; selected %s automatically.", sym, expiryDate, entry.TradingClass),
				Impact:   "Trading classes can have different listed strikes or session behavior on the same date.",
				Action:   "Pass trading_class (MCP) or --class (CLI) when you need a specific class.",
			})
		}
		return chainFetchSurface{
			tradingClass: out.tradingClass,
			strikes:      out.strikes,
			warnings:     out.warnings,
		}, nil
	}

	expiryDate := displayExpiry(expiryYMD)
	classed, err := c.FetchOptionExpiryStrikesClassed(sym, 10*time.Second)
	if err != nil {
		step := strikeStep(spot)
		atm := math.Round(spot/step) * step
		return chainFetchSurface{
			tradingClass: class,
			strikes:      heuristicChainRows(atm, step, width),
			warnings: []rpc.DataWarning{{
				Code:     "spx_classed_chain_unavailable",
				Scope:    sym,
				Severity: "data_quality",
				Message:  "SPX classed option-chain enumeration was unavailable; falling back to the generic strike grid.",
				Impact:   "SPX/SPXW contract selection may be wrong for daily, weekly, or same-date classed expiries.",
				Action:   "Retry after the security-definition farm recovers; compare the selected trading_class against TWS.",
			}},
		}, nil
	}

	entries := normalisedSPXChainEntries(classed[expiryDate], sym)
	if len(entries) == 0 {
		return chainFetchSurface{}, errBadRequest(fmt.Sprintf("expiry %s is not listed for %s", expiryDate, sym))
	}
	entry, auto, err := selectSPXChainEntry(entries, class, requestedClass != "", expiryDate, now)
	if err != nil {
		return chainFetchSurface{}, err
	}
	out := chainFetchSurface{
		tradingClass: entry.TradingClass,
		strikes:      chainRowsFromListedStrikes(entry.Strikes, spot, width),
	}
	if auto {
		out.warnings = append(out.warnings, rpc.DataWarning{
			Code:     "spx_trading_class_auto",
			Scope:    sym,
			Severity: "info",
			Message:  fmt.Sprintf("SPX expiry %s lists multiple trading classes; selected %s automatically.", expiryDate, entry.TradingClass),
			Impact:   "SPX and SPXW can have different settlement conventions and ConIDs on the same date.",
			Action:   "Pass trading_class (MCP) or --class (CLI) when you need a specific SPX class.",
		})
	}
	return out, nil
}

func selectDefaultChainEntry(symbol string, entries []ibkrlib.ExpiryClassedStrikes, class string, explicit bool, expiryDate string) (ibkrlib.ExpiryClassedStrikes, bool, error) {
	if explicit {
		for _, entry := range entries {
			if entry.TradingClass == class {
				return entry, false, nil
			}
		}
		return ibkrlib.ExpiryClassedStrikes{}, false, errBadRequest(fmt.Sprintf("trading_class %s is not listed for %s expiry %s; available classes: %s", class, symbol, expiryDate, strings.Join(spxChainClasses(entries), ", ")))
	}
	if entry, ok := spxChainEntryByClass(entries, class); ok {
		return entry, false, nil
	}
	if entry, ok := spxChainEntryByClass(entries, symbol); ok {
		return entry, false, nil
	}
	if len(entries) == 1 {
		return entries[0], false, nil
	}
	return entries[0], true, nil
}

func normalisedSPXChainEntries(entries []ibkrlib.ExpiryClassedStrikes, fallbackClass string) []ibkrlib.ExpiryClassedStrikes {
	out := make([]ibkrlib.ExpiryClassedStrikes, 0, len(entries))
	for _, entry := range entries {
		class := strings.ToUpper(strings.TrimSpace(entry.TradingClass))
		if class == "" {
			class = fallbackClass
		}
		strikes := dedupeSortedPositiveFloats(entry.Strikes)
		if len(strikes) == 0 {
			continue
		}
		out = append(out, ibkrlib.ExpiryClassedStrikes{
			TradingClass: class,
			Strikes:      strikes,
		})
	}
	slices.SortFunc(out, func(a, b ibkrlib.ExpiryClassedStrikes) int {
		return strings.Compare(a.TradingClass, b.TradingClass)
	})
	return out
}

func selectSPXChainEntry(entries []ibkrlib.ExpiryClassedStrikes, class string, explicit bool, expiryDate string, now time.Time) (ibkrlib.ExpiryClassedStrikes, bool, error) {
	if explicit {
		for _, entry := range entries {
			if entry.TradingClass == class {
				return entry, false, nil
			}
		}
		return ibkrlib.ExpiryClassedStrikes{}, false, errBadRequest(fmt.Sprintf("trading_class %s is not listed for SPX expiry %s; available classes: %s", class, expiryDate, strings.Join(spxChainClasses(entries), ", ")))
	}
	if len(entries) == 1 {
		return entries[0], false, nil
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	if day, parseErr := time.ParseInLocation("2006-01-02", expiryDate, loc); parseErr == nil && now.In(loc).Format("2006-01-02") == expiryDate {
		spxCutoff := classSettlementInstant("SPX", day.Year(), day.Month(), day.Day(), loc).Add(classSettlementBuffer)
		if now.In(loc).After(spxCutoff) {
			if entry, ok := spxChainEntryByClass(entries, "SPXW"); ok {
				return entry, true, nil
			}
		}
	}
	if entry, ok := spxChainEntryByClass(entries, "SPX"); ok {
		return entry, true, nil
	}
	if entry, ok := spxChainEntryByClass(entries, "SPXW"); ok {
		return entry, true, nil
	}
	return entries[0], true, nil
}

func spxChainEntryByClass(entries []ibkrlib.ExpiryClassedStrikes, class string) (ibkrlib.ExpiryClassedStrikes, bool) {
	for _, entry := range entries {
		if entry.TradingClass == class {
			return entry, true
		}
	}
	return ibkrlib.ExpiryClassedStrikes{}, false
}

func spxChainClasses(entries []ibkrlib.ExpiryClassedStrikes) []string {
	classes := make([]string, 0, len(entries))
	for _, entry := range entries {
		classes = append(classes, entry.TradingClass)
	}
	return classes
}

func heuristicChainRows(atm, step float64, width int) []rpc.ChainStrike {
	n := 2*width + 1
	rows := make([]rpc.ChainStrike, n)
	for i := range n {
		offset := i - width
		rows[i] = rpc.ChainStrike{Strike: atm + float64(offset)*step, IsATM: offset == 0}
	}
	return rows
}

func chainRowsFromListedStrikes(strikes []float64, spot float64, width int) []rpc.ChainStrike {
	clean := dedupeSortedPositiveFloats(strikes)
	if len(clean) == 0 {
		return nil
	}
	center := closestStrikeIndex(clean, spot)
	count := min(2*width+1, len(clean))
	start := max(center-width, 0)
	end := start + count
	if end > len(clean) {
		end = len(clean)
		start = max(0, end-count)
	}
	rows := make([]rpc.ChainStrike, 0, end-start)
	for idx := start; idx < end; idx++ {
		rows = append(rows, rpc.ChainStrike{Strike: clean[idx], IsATM: idx == center})
	}
	return rows
}

func closestStrikeIndex(strikes []float64, spot float64) int {
	best := 0
	bestDist := math.Abs(strikes[0] - spot)
	for i, strike := range strikes[1:] {
		d := math.Abs(strike - spot)
		if d < bestDist {
			best = i + 1
			bestDist = d
		}
	}
	return best
}

func dedupeSortedPositiveFloats(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		if v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	dst := out[:0]
	for _, v := range out {
		if len(dst) == 0 || dst[len(dst)-1] != v {
			dst = append(dst, v)
		}
	}
	return dst
}

func displayExpiry(expiryYMD string) string {
	if len(expiryYMD) == 8 {
		return expiryYMD[:4] + "-" + expiryYMD[4:6] + "-" + expiryYMD[6:8]
	}
	return expiryYMD
}

// mergeStrikeSide copies the side-specific fields (call or put)
// populated by a worker into the shared row. Disjoint by construction
// — the C worker only writes Call*, the P worker only writes Put* —
// but go through one helper so the field list stays in one place.
func mergeStrikeSide(dst, src *rpc.ChainStrike, right string) {
	if right == "C" {
		dst.CallBid = src.CallBid
		dst.CallAsk = src.CallAsk
		dst.CallLast = src.CallLast
		dst.CallPrevClose = src.CallPrevClose
		dst.CallIV = src.CallIV
		dst.CallDelta = src.CallDelta
		dst.CallOI = src.CallOI
		dst.CallAsOf = src.CallAsOf
		dst.CallDataStatus = src.CallDataStatus
		dst.CallIVStatus = src.CallIVStatus
		dst.CallOIStatus = src.CallOIStatus
		return
	}
	dst.PutBid = src.PutBid
	dst.PutAsk = src.PutAsk
	dst.PutLast = src.PutLast
	dst.PutPrevClose = src.PutPrevClose
	dst.PutIV = src.PutIV
	dst.PutDelta = src.PutDelta
	dst.PutOI = src.PutOI
	dst.PutAsOf = src.PutAsOf
	dst.PutDataStatus = src.PutDataStatus
	dst.PutIVStatus = src.PutIVStatus
	dst.PutOIStatus = src.PutOIStatus
}

func fillOptionLeg(ctx context.Context, c *ibkrlib.Connector, row *rpc.ChainStrike, symbol, tradingClass, expiryYMD string, strike float64, right string) {
	key, _, err := c.SubscribeOption(ctx, symbol, tradingClass, expiryYMD, strike, right)
	if err != nil {
		setOptionLegUnavailable(row, right, "subscribe_error")
		return
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	asOf := time.Now()
	deadline := time.Now().Add(2500 * time.Millisecond)
	var bid, ask, last float64
	if err := pollMarketData(ctx, c, key, deadline, func(d *ibkrlib.MarketData) bool {
		asOf = time.Now()
		if d.Bid > 0 || d.Ask > 0 || d.Last > 0 {
			bid, ask, last = d.Bid, d.Ask, d.Last
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Tick 13 (model option computation) typically arrives a beat after
	// the first bid/ask print. IV gets its own 1 s budget, capped to
	// whatever's left of the leg's overall deadline so a slow quote can't
	// outlive the per-request budget.
	//
	// Pre-market / after-hours, the option book may have no bid/ask/last
	// but IBKR can still deliver IV via model computation. The poll runs
	// unconditionally so those fills land — costs at most one extra 1 s
	// per dead leg, bounded by the per-request budget.
	var iv float64
	ivDeadline := time.Now().Add(1 * time.Second)
	if ivDeadline.After(deadline) {
		ivDeadline = deadline
	}
	if err := pollUntil(ctx, ivDeadline, func() bool {
		v, ok := c.OptionIV(key)
		if ok && v > 0 {
			iv = v
			asOf = time.Now()
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Greeks: the same SubscribeOption path drives msg-21 model-
	// computation ticks, so by the time we have IV the per-leg Delta
	// is typically already cached. No extra wait — we just read what
	// landed. Gamma/Theta/Vega aren't surfaced on the chain wire
	// shape today; if a future chain consumer wants them we extend
	// ChainStrike rather than fold them into the same fields.
	var delta *float64
	if g, ok := c.OptionGreeks(key); ok {
		// OptionGreeks' ok flag is the "at least one field populated
		// from a valid model-computation tick" gate; genuine zero delta
		// (far-OTM near expiry) must surface as a non-nil pointer per
		// the wire contract.
		d := g.Delta
		delta = &d
	}
	var prevClose float64
	if px, ok := c.OptionPrevClose(key); ok && px > 0 {
		prevClose = px
	}
	// Opportunistic OI read off the same subscription. Tick types 27
	// (callOpenInterest) and 28 (putOpenInterest) land on the cached
	// MarketData.OpenInt plus OpenIntObserved — same pattern gamma uses.
	// Zero OI is still a real observed value when OpenIntObserved=true;
	// nil means the OI tick did not arrive.
	var oi *int64
	oiDeadline := time.Now().Add(optionOpenInterestGrace)
	if oiDeadline.After(deadline) {
		oiDeadline = deadline
	}
	if v, observed := waitForOptionOpenInterest(ctx, oiDeadline, func() (int64, bool) {
		if d, ok := c.MarketDataSnapshot()[key]; ok && d.OpenIntObserved {
			return d.OpenInt, true
		}
		return 0, false
	}); observed {
		oi = &v
	}
	dataStatus := optionLegDataStatus(bid, ask, last, prevClose, iv, delta)
	ivStatus := "unavailable"
	if iv > 0 {
		ivStatus = "ok"
	}
	oiStatus := "unavailable"
	if oi != nil {
		oiStatus = "ok"
	}
	if right == "C" {
		if bid > 0 {
			v := bid
			row.CallBid = &v
		}
		if ask > 0 {
			v := ask
			row.CallAsk = &v
		}
		if last > 0 {
			v := last
			row.CallLast = &v
		}
		if prevClose > 0 {
			v := prevClose
			row.CallPrevClose = &v
		}
		if iv > 0 {
			v := iv
			row.CallIV = &v
		}
		row.CallDelta = delta
		row.CallOI = oi
		row.CallAsOf = asOf
		row.CallDataStatus = dataStatus
		row.CallIVStatus = ivStatus
		row.CallOIStatus = oiStatus
		return
	}
	if bid > 0 {
		v := bid
		row.PutBid = &v
	}
	if ask > 0 {
		v := ask
		row.PutAsk = &v
	}
	if last > 0 {
		v := last
		row.PutLast = &v
	}
	if prevClose > 0 {
		v := prevClose
		row.PutPrevClose = &v
	}
	if iv > 0 {
		v := iv
		row.PutIV = &v
	}
	row.PutDelta = delta
	row.PutOI = oi
	row.PutAsOf = asOf
	row.PutDataStatus = dataStatus
	row.PutIVStatus = ivStatus
	row.PutOIStatus = oiStatus
}

func optionLegDataStatus(bid, ask, last, prevClose, iv float64, delta *float64) string {
	switch {
	case bid > 0 || ask > 0 || last > 0:
		return "quoted"
	case prevClose > 0:
		return "prev_close"
	case iv > 0 || delta != nil:
		return "model_only"
	default:
		return "no_quote"
	}
}

func setOptionLegUnavailable(row *rpc.ChainStrike, right, status string) {
	if status == "" {
		status = "unavailable"
	}
	now := time.Now()
	if right == "C" {
		row.CallAsOf = now
		row.CallDataStatus = status
		row.CallIVStatus = "unavailable"
		row.CallOIStatus = "unavailable"
		return
	}
	row.PutAsOf = now
	row.PutDataStatus = status
	row.PutIVStatus = "unavailable"
	row.PutOIStatus = "unavailable"
}

func chainSummaries(res *rpc.ChainResult, wantCalls, wantPuts bool) (*rpc.ChainTradableSummary, *rpc.ChainLiquiditySummary) {
	if res == nil {
		return nil, nil
	}
	tradable := &rpc.ChainTradableSummary{}
	regularOptionsClosed := res.DataType == rpc.MarketDataClosed
	feedLooksLive := res.FeedType == rpc.MarketDataLive || (res.FeedType == "" && res.DataType == rpc.MarketDataLive)
	forceStaleQuotes := res.SessionState == rpc.SessionClosed.String() && !feedLooksLive
	var nearestCall, nearestPut, minSpread, atmLive *rpc.ChainLegSummary
	nearestCallDist, nearestPutDist, minSpreadPct := math.MaxFloat64, math.MaxFloat64, math.MaxFloat64

	observe := func(row rpc.ChainStrike, right string, bid, ask, last *float64, oi *int64, delta *float64, dataStatus string) {
		tradable.TotalLegs++
		if oi != nil {
			tradable.OICoveredLegs++
		}
		switch dataStatus {
		case "quoted":
			if forceStaleQuotes {
				tradable.StaleLegs++
			}
		case "prev_close":
			tradable.StaleLegs++
		case "model_only":
			tradable.ModelOnlyLegs++
		case "subscribe_error":
			tradable.SubscribeErrorLegs++
		case "no_quote", "unavailable":
			tradable.NoQuoteLegs++
		}

		hasBid := bid != nil && *bid > 0
		hasAsk := ask != nil && *ask > 0
		if forceStaleQuotes {
			return
		}
		if hasBid != hasAsk {
			tradable.OneSidedLiveLegs++
		}
		if !hasBid || !hasAsk {
			return
		}
		tradable.LiveBidAskLegs++
		mid := (*bid + *ask) / 2
		spread := *ask - *bid
		spreadPct := 0.0
		if mid > 0 {
			spreadPct = spread / mid
		}
		leg := &rpc.ChainLegSummary{
			Right:     right,
			Strike:    row.Strike,
			Bid:       *bid,
			Ask:       *ask,
			Mid:       mid,
			Spread:    spread,
			SpreadPct: spreadPct,
			OI:        oi,
			Delta:     delta,
		}
		dist := math.Abs(row.Strike - res.Spot)
		if right == "C" && dist < nearestCallDist {
			nearestCall = leg
			nearestCallDist = dist
		}
		if right == "P" && dist < nearestPutDist {
			nearestPut = leg
			nearestPutDist = dist
		}
		if spreadPct < minSpreadPct {
			minSpread = leg
			minSpreadPct = spreadPct
		}
		if row.IsATM && (atmLive == nil || spreadPct < atmLive.SpreadPct) {
			atmLive = leg
		}
	}

	for _, row := range res.Strikes {
		if wantCalls {
			observe(row, "C", row.CallBid, row.CallAsk, row.CallLast, row.CallOI, row.CallDelta, row.CallDataStatus)
		}
		if wantPuts {
			observe(row, "P", row.PutBid, row.PutAsk, row.PutLast, row.PutOI, row.PutDelta, row.PutDataStatus)
		}
	}
	if tradable.TotalLegs > 0 {
		tradable.OICoveragePct = float64(tradable.OICoveredLegs) / float64(tradable.TotalLegs)
	}
	tradable.OptionsTradable = tradable.LiveBidAskLegs > 0
	if !tradable.OptionsTradable {
		switch {
		case regularOptionsClosed || forceStaleQuotes:
			tradable.FeedGap = "stale_close_only"
		case tradable.SubscribeErrorLegs > 0:
			tradable.FeedGap = "unknown_feed_gap"
		case tradable.TotalLegs > 0:
			tradable.FeedGap = "thin_contract"
		default:
			tradable.FeedGap = "unknown_feed_gap"
		}
	}

	liquidity := &rpc.ChainLiquiditySummary{
		LiquidityGrade:           "untradable",
		NearestLiveCall:          nearestCall,
		NearestLivePut:           nearestPut,
		MinSpreadLiveStrike:      minSpread,
		OICoveragePct:            tradable.OICoveragePct,
		RecommendedStructureHint: "untradable_chain",
	}
	if atmLive != nil {
		v := atmLive.SpreadPct
		liquidity.ATMSpreadPct = &v
	}
	switch {
	case tradable.LiveBidAskLegs == 0:
		// Defaults already express the state.
	case liquidity.ATMSpreadPct != nil && *liquidity.ATMSpreadPct <= 0.15 && nearestCall != nil:
		liquidity.LiquidityGrade = "good"
		liquidity.RecommendedStructureHint = "calls_ok"
	case minSpread != nil && minSpread.SpreadPct <= 0.25:
		liquidity.LiquidityGrade = "fair"
		liquidity.RecommendedStructureHint = "shares_or_spreads"
	default:
		liquidity.LiquidityGrade = "poor"
		liquidity.RecommendedStructureHint = "stock_only"
	}
	return tradable, liquidity
}

func chainWarningDetails(res *rpc.ChainResult, wantCalls, wantPuts bool) []rpc.DataWarning {
	if res == nil {
		return nil
	}
	out := chainSpotWarning(res.Symbol, chainSpotSelection{
		Price:  res.Spot,
		Source: res.SpotSource,
		AsOf:   res.SpotAsOf,
	})
	if res.DataType == rpc.MarketDataClosed {
		out = append(out, rpc.DataWarning{
			Code:     "options_closed",
			Scope:    res.Symbol,
			Severity: "info",
			Message:  "The regular U.S. listed-options data surface is outside RTH.",
			Impact:   "SPX/VIX extended or curb trading may still exist, but this response should be treated as closed-session context unless live bid/ask, OI, and IV fields landed.",
			Action:   "Retry during 09:30-16:00 ET for the most complete executable quote/OI/IV surface; compare TWS if you expect extended-session quotes.",
		})
	}
	if res.TradableSummary != nil && !res.TradableSummary.OptionsTradable {
		action := "Retry during RTH or compare against TWS option-chain display and market-data subscriptions."
		if res.TradableSummary.FeedGap == "thin_contract" {
			action = "Treat this as stock-only unless a wider chain request finds live two-sided strikes."
		}
		out = append(out, rpc.DataWarning{
			Code:     "no_live_option_bid_ask",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  "No requested option legs returned live two-sided bid/ask quotes.",
			Impact:   "The displayed chain is not executable for trade-structure selection.",
			Action:   action,
		})
	}
	var prevCloseOnly, modelOnly, missingOI, missingIV int
	for _, row := range res.Strikes {
		if wantCalls {
			if row.CallDataStatus == "prev_close" {
				prevCloseOnly++
			}
			if row.CallDataStatus == "model_only" {
				modelOnly++
			}
			if row.CallOIStatus == "unavailable" {
				missingOI++
			}
			if row.CallIVStatus == "unavailable" {
				missingIV++
			}
		}
		if wantPuts {
			if row.PutDataStatus == "prev_close" {
				prevCloseOnly++
			}
			if row.PutDataStatus == "model_only" {
				modelOnly++
			}
			if row.PutOIStatus == "unavailable" {
				missingOI++
			}
			if row.PutIVStatus == "unavailable" {
				missingIV++
			}
		}
	}
	if prevCloseOnly > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "prev_close_legs",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("%d option legs used prior-session close as the only price anchor.", prevCloseOnly),
			Impact:   "The leg has price context but no executable bid/ask/last quote within the fill window.",
			Action:   "Use call_prev_close/put_prev_close only as stale context, not a live fill price.",
		})
	}
	if modelOnly > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "model_only_legs",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("%d option legs returned model data without bid/ask/last.", modelOnly),
			Impact:   "IV/delta may be usable for context, but the legs were not quotable within the fill window.",
		})
	}
	if missingOI > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "oi_unavailable",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("Open interest was unavailable for %d option legs.", missingOI),
			Impact:   "Liquidity and gamma filters should not assume missing OI is zero.",
		})
	}
	if missingIV > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "iv_unavailable",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("Implied volatility was unavailable for %d option legs.", missingIV),
			Impact:   "The gateway did not deliver a model fit for those strikes within the chain budget.",
		})
	}
	return out
}

func chainSpotWarning(symbol string, spot chainSpotSelection) []rpc.DataWarning {
	if spot.Price <= 0 {
		return nil
	}
	switch spot.Source {
	case "prev_close", "historical_close":
		return []rpc.DataWarning{{
			Code:     "selected_chain_spot_prev_close",
			Scope:    symbol,
			Severity: "data_quality",
			Message:  "ATM strike selection used the underlying's prior regular-session close.",
			Impact:   "The strike grid is suitable for off-hours inspection, but the ATM anchor is stale.",
			Action:   "Retry during the regular session for a live underlying spot.",
		}}
	default:
		return nil
	}
}
