package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Default calibration window for the zero-gamma compute. Tuned for
// the trader-side review: 6 expirations beats the SpotGamma 4-expiry
// default in nominal coverage; ±10 % strike width defines the candidate
// window and the nearest-80-strikes cap keeps the leg count reasonable;
// ±15 % sweep range comfortably brackets the typical zero crossing
// without inflating the profile point count.
//
// WorkerCount 4 matches the documented safe gateway throttle elsewhere
// in this package (handleChainFetch, around handlers.go:1628). Bumping
// it requires retuning AcquireMarketDataSlot and is a deliberate
// follow-up, not a v1 knob.
const (
	defaultExpiryCount    = 6
	defaultStrikeWidthPct = 0.10
	defaultSweepRangePct  = 0.15
	defaultWorkerCount    = 4

	// maxGammaStrikesPerExpiry caps the listed strikes walked for each
	// expiry after the ±StrikeWidthPct filter and ATM-outward ordering.
	// This keeps the default fan-out at 6 × 80 × 2 = 960 option legs,
	// matching the compute budget below. It is especially important for
	// SPX/SPXW, where 5-point strike grids inside ±10% can otherwise
	// expand to 3k+ subscriptions outside RTH with little extra signal.
	maxGammaStrikesPerExpiry = 80

	// Horizon bucket boundaries in fractional years.
	zeroDTECutoffYears = 1.0 / 365.0 // strictly-less-than: anything inside one calendar day is "0DTE"
	nearDTECutoffYears = 7.0 / 365.0 // upper bound on the 1-7 bucket; >7 falls in term

	// sweepPoints is the number of (spot, GEX) samples in the profile.
	sweepPoints = 60

	// topStrikesK is the number of concentration rows on the result —
	topStrikesK = 10

	// Throttle-signal abort. The option-chain fan-out makes hundreds
	// security definition") instead of real details. Continuing the
	// observed contract-resolve failure ratio exceeds throttleAbortPct,
	// failures).
	throttleSampleSize = 50
	throttleAbortPct   = 0.05

	// computeETA is the static initial seconds-to-complete estimate the
	// cache stamps on a fresh kickoff. Calibration after the v0.24.x
	// IV-source fix:
	//   6 expirations × ~80 strikes × 2 sides ≈ 960 legs (worst case)
	//   actual landing rate ≈ 1-2 s/leg on warm contract cache
	//   960 / 4 workers × 1.5 s/leg ≈ 6 min worst case
	//   typical wall-clock 2-4 min.
	// 240s is the new conservative midpoint.
	computeETA = 240

	// earlyAbortAfter is how long the fan-out runs before checking
	// ticks the compute needs, and the right thing is to fail fast
	earlyAbortAfter = 30 * time.Second

	// optionOpenInterestGrace is the short post-IV wait before a gamma
	optionOpenInterestGrace = 750 * time.Millisecond

	// gammaMethodToken is the stable wire token consumers (renderers,
	// read from the gateway's optional Greeks tick (fixes a v2 race
	gammaMethodToken = "bs-gamma-profile-v3-stickymoneyness-0dte-split"

	// MinLegCoverageFraction is the persist-or-not threshold: a
	// IBKR gateway's OPT model-tick delivery is bursty during RTH —
	MinLegCoverageFraction = 0.2
)

// gammaMethodologyCitations is the short bibliography the compute
var gammaMethodologyCitations = []string{
	"Perfiliev (2022) — BS-sweep baseline",
	"Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics",
	"SqueezeMetrics (2017) — naive-sign GEX, deprecated 2022+",
	"Cboe 2025 — 0DTE = ~59% of SPX volume",
}

// checkLegCoverage returns nil if the fan-out's leg-landing fraction
func checkLegCoverage(landed, total int, throttled bool) error {
	if total == 0 {
		// Defensive zero-divide guard. The only way to reach here
		return fmt.Errorf("low leg coverage: empty jobs list — no chain to compute over")
	}
	coverage := float64(landed) / float64(total)
	if coverage >= MinLegCoverageFraction {
		return nil
	}
	throttledHint := ""
	if throttled {
		throttledHint = " (gateway throttled the fan-out)"
	}
	return fmt.Errorf("low leg coverage: %d/%d legs landed (%.0f%%), below minimum %.0f%%%s — not persisting; gammaErrorRetryTTL will let the next call re-attempt", landed, total, coverage*100, MinLegCoverageFraction*100, throttledHint)
}

// legData carries the per-leg inputs the aggregator needs from the
// fan-out into the sweep. Captured at fetch time; iv stays fixed
// during the spot sweep (a documented v1 limitation — sticky-strike
// skew is on the deferred backlog).
//
// tradingClass disambiguates SPX-class AM-monthlies from SPXW-class
// PM-weeklies on shared third-Friday dates. For single-class
// underlyings (SPY) the field equals the symbol. The settlement
// instant in dteYears branches on it.
type legData struct {
	expiryYMD    string
	dte          float64 // years; positive
	strike       float64
	right        string // "C" | "P"
	tradingClass string // "SPY" | "SPX" | "SPXW" | …
	isCall       bool
	iv           float64
	ivSource     string
	oi           int64
	oiObserved   bool
	oiLive       bool
	oiCarried    bool
	oiObservedAt time.Time
	// gamma is the gateway-supplied model-computation gamma at the
	// snapshot spot; used for the at-spot aggregate. The sweep
	// recomputes gamma via Black-Scholes for each scenario spot.
	gammaAtSnapshot float64
}

type gammaLegSpec struct {
	expiryYMD    string
	expiryDate   string
	strike       float64
	right        string
	tradingClass string
}

// legResult is the per-leg payload returned by a legFetcher. Bundled as
// BS-IV fallback) within budget — the aggregator only counts a leg when
// Throttle reports a contract-resolve failure on a strike that came
type legResult struct {
	OI         int64
	OIObserved bool
	IV         float64
	Gamma      float64
	IVDerived  bool
	IVSource   string
	OK         bool
	Throttle   bool
	Failure    string
}

const (
	gammaLegFailureContractMissing      = "contract_missing"
	gammaLegFailureTimeout              = "timeout"
	gammaLegFailurePacing               = "pacing"
	gammaLegFailureFarm                 = "farm"
	gammaLegFailureEntitlement          = "entitlement"
	gammaLegFailureSubscriptionRejected = "subscription_reject"
	gammaIVSourceModelTick              = "model_tick"
	gammaIVSourceLiveMid                = "derived_live_mid"
	gammaIVSourcePrevClose              = "derived_prev_close"
	gammaOISourceLiveObserved           = "live_observed"
	gammaOISourceCarriedForward         = "carried_forward"
	gammaOISourceMissing                = "missing"
	gammaOISourceMixed                  = "mixed"
)

// gammaLogger is the minimal logging surface computeGammaZeroFor uses to
// emit kickoff / progress / abort lines. Defined as an interface so tests
// can drive the compute with a no-op recorder; production passes the
// daemon's *Logger. Nil is accepted and treated as no-op.
type gammaLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// gammaLogfWrap returns a struct that turns nil-safe Infof/Warnf into
// no-ops. Lets every log call site stay free of nil checks without
// repeating boilerplate.
type gammaLogf struct{ inner gammaLogger }

func (g gammaLogf) Infof(format string, args ...any) {
	if g.inner != nil {
		g.inner.Infof(format, args...)
	}
}
func (g gammaLogf) Warnf(format string, args ...any) {
	if g.inner != nil {
		g.inner.Warnf(format, args...)
	}
}

// legFetcher abstracts the per-leg subscribe-collect-unsubscribe so
// clock. A delayed spot may consume only IBKR's delayed model tick 83.
type legFetcher func(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, tradingClass, expiryYMD string,
	strike float64,
	right string,
	snapshotSpot float64,
	snapshotAt time.Time,
	snapshotDataType string,
) legResult

// productionLegFetcher is the live-gateway implementation. It mirrors
// the data-collection pattern in handlers.go's fillOptionLeg (the chain
// command's per-strike fill): subscribe the option, wait for the
// open-interest tick to land in the MarketData cache, then read the
// per-strike IV from OptionIV and the Greeks from OptionGreeks.
//
// Two-stage data collection:
//
//	Stage 1  — gateway model tick. Tick 21 (OPTION_COMPUTATION,
//	           tickType=13 live/frozen or 83 delayed) routes into
//	           optIV[key] / optGreeks[key];
//	           fastest path with the gateway's own σ. Verified to fire
//	           off-hours under the daemon's default MarketDataType=2 —
//	           same path the internal chain fetch relies on for ATM IV.
//	Stage 2  — BS-IV fallback. When the gateway never pushed a model
//	           tick, solve for σ via Newton-Raphson against the option's
//	           bid/ask mid or prior-session close (tick 9, always pushed
//	           on subscribe regardless of trading state). Gamma is then
//	           computed via bsGamma using the derived σ.
//
// Open interest (ticks 27/28) is read opportunistically from the per-
// subscription cache at the end — never as a gate. Missing OI is
// unknown, not zero: the leg can enrich IV/skew fitting, but it is
// omitted from OI-weighted dealer GEX until an OI tick is observed.
// SPY OI may be absent outside regular option hours; SPX OI should be
// stable across session phases, so missing SPX OI is a data-quality
// finding rather than expected off-hours sparsity.
//
// Per-leg budget is 1.5 s for the model-tick poll. Active strikes
// produce a model tick within ~500 ms; dead deep-OTM strikes time out
// and fall through to Stage 2 which back-solves σ from cached prices.
func productionLegFetcher(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, tradingClass, expiryYMD string,
	strike float64,
	right string,
	snapshotSpot float64,
	snapshotAt time.Time,
	snapshotDataType string,
) legResult {
	if c == nil {
		return legResult{Throttle: true}
	}
	key, _, err := c.SubscribeOption(ctx, underlying, tradingClass, expiryYMD, strike, right)
	if err != nil {
		// SubscribeOption's error path has two distinct shapes:
		failure := classifyGammaLegFailure(err)
		throttle := failure != gammaLegFailureContractMissing && failure != gammaLegFailureEntitlement
		return legResult{Throttle: throttle, Failure: failure}
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	// Stage 1: model-tick poll. handleOptionComputation commits
	// optIV[key] / optGreeks[key] once IBKR sends a non-sentinel model
	deadline := time.Now().Add(1500 * time.Millisecond)
	var iv, gamma float64
	err = pollUntilWithReject(ctx, deadline, c.SubscriptionRejectCh(key), key, func() bool {
		if v, optionDataType, found := c.OptionIVWithDataType(key); found && v > 0 && gammaOptionModelDataTypeCompatible(snapshotDataType, optionDataType) {
			iv = v
			if g, found := c.OptionGreeks(key); found {
				gamma = g.Gamma
			}
		}
		return iv > 0
	})
	if IsSubscriptionRejected(err) {
		// Gateway pushed a terminal error for this reqID (200 "no
		// security definition", 354 "not subscribed", 10197 "competing
		// session", …). The subscription will never produce ticks.
		// the fan-out is overloading the wire.
		return legResult{Failure: classifyGammaLegFailure(err)}
	}

	if iv > 0 {
		// Opportunistic OI read. May be 0 for strikes the gateway never
		// Do not read only once: IV/model ticks often arrive before the
		// one-shot OI tick. A short grace materially improves OI capture
		oi, oiObserved := waitForOptionOpenInterest(ctx, time.Now().Add(optionOpenInterestGrace), func() (int64, bool) {
			return optionOpenInterest(c, key)
		})
		return legResult{OI: oi, OIObserved: oiObserved, IV: iv, Gamma: gamma, IVSource: gammaIVSourceModelTick, OK: true}
	}
	if snapshotDataType == rpc.MarketDataDelayed {
		// A quote/previous-close inversion has no typed source clock. Using it
		// beside a delayed spot would recreate the exact mixed-time input this
		// fallback is designed to prevent, so delayed runs require tick 83.
		return legResult{Failure: gammaLegFailureTimeout}
	}
	// Stage 2: BS-IV fallback when model tick never arrived.
	bid, ask, hasQuote := c.OptionQuoteBidAsk(key)
	var price float64
	var ivSource string
	if hasQuote && bid > 0 && ask > 0 {
		price = (bid + ask) / 2
		ivSource = gammaIVSourceLiveMid
	} else if px, ok := c.OptionPrevClose(key); ok && px > 0 {
		price = px
		ivSource = gammaIVSourcePrevClose
	}
	oi, oiObserved := waitForOptionOpenInterest(ctx, time.Now().Add(optionOpenInterestGrace), func() (int64, bool) {
		return optionOpenInterest(c, key)
	})
	fallback := bsIVFallback(snapshotSpot, snapshotAt, expiryYMD, tradingClass, strike, right, oi, oiObserved, price, ivSource)
	if !fallback.OK {
		fallback.Failure = gammaLegFailureTimeout
	}
	return fallback
}

func gammaOptionModelDataTypeCompatible(spotDataType string, optionDataType int) bool {
	switch spotDataType {
	case rpc.MarketDataDelayed:
		return optionDataType == ibkrlib.OptionModelDataTypeDelayed
	case "", rpc.MarketDataLive, rpc.MarketDataFrozen:
		return optionDataType != ibkrlib.OptionModelDataTypeDelayed
	default:
		return false
	}
}

func classifyGammaLegFailure(err error) string {
	if err == nil {
		return ""
	}
	if rej, ok := errors.AsType[*SubscriptionRejectedError](err); ok {
		return classifyGammaRejectionCode(rej.Rejection.Code, rej.Rejection.Message)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "contract details unavailable"), strings.Contains(msg, "no security definition"):
		return gammaLegFailureContractMissing
	case strings.Contains(msg, "returned zero contract details"):
		return gammaLegFailureContractMissing
	case strings.Contains(msg, "354"), strings.Contains(msg, "entitlement"), strings.Contains(msg, "not subscribed"):
		return gammaLegFailureEntitlement
	case strings.Contains(msg, "pacing"), strings.Contains(msg, "rate"):
		return gammaLegFailurePacing
	case strings.Contains(msg, "farm"):
		return gammaLegFailureFarm
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return gammaLegFailureTimeout
	default:
		return gammaLegFailureSubscriptionRejected
	}
}

func classifyGammaRejectionCode(code int, message string) string {
	msg := strings.ToLower(message)
	switch code {
	case 200, 320, 321, 322:
		return gammaLegFailureContractMissing
	case 354, 10197:
		return gammaLegFailureEntitlement
	}
	switch {
	case strings.Contains(msg, "pacing"), strings.Contains(msg, "rate"):
		return gammaLegFailurePacing
	case strings.Contains(msg, "farm"):
		return gammaLegFailureFarm
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return gammaLegFailureTimeout
	default:
		return gammaLegFailureSubscriptionRejected
	}
}

func optionOpenInterest(c *ibkrlib.Connector, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	if d, ok := c.MarketDataSnapshot()[key]; ok {
		return d.OpenInt, d.OpenIntObserved
	}
	return 0, false
}

func waitForOptionOpenInterest(ctx context.Context, deadline time.Time, read func() (int64, bool)) (int64, bool) {
	if read == nil {
		return 0, false
	}
	var oi int64
	var observed bool
	_ = pollUntil(ctx, deadline, func() bool {
		oi, observed = read()
		return observed
	})
	return oi, observed
}

// bsIVFallback assembles a leg result from Black-Scholes back-solving when
// composition: optionPriceForBSIV is the only connector read it depends
// solving from yesterday's close at 14:00 ET on expiry day must use
func bsIVFallback(snapshotSpot float64, snapshotAt time.Time, expiryYMD, tradingClass string, strike float64, right string, oi int64, oiObserved bool, price float64, ivSource string) legResult {
	dte := dteYears(expiryYMD, tradingClass, snapshotAt)
	if dte <= 0 || price <= 0 {
		return legResult{}
	}
	iv := bsImpliedVolatility(price, snapshotSpot, strike, dte, 0, 0, right == "C")
	if iv <= 0 {
		return legResult{}
	}
	if ivSource == "" {
		ivSource = gammaIVSourcePrevClose
	}
	gamma := bsGamma(snapshotSpot, strike, dte, iv, 0, 0)
	return legResult{OI: oi, OIObserved: oiObserved, IV: iv, Gamma: gamma, IVDerived: true, IVSource: ivSource, OK: true}
}

func gammaIVSource(r legResult) string {
	switch r.IVSource {
	case gammaIVSourceModelTick, gammaIVSourceLiveMid, gammaIVSourcePrevClose:
		return r.IVSource
	}
	if r.IVDerived {
		return gammaIVSourcePrevClose
	}
	return gammaIVSourceModelTick
}

func countGammaIVSources(legs []legData) (modelTick, derivedMid, derivedClose int) {
	for _, leg := range legs {
		switch leg.ivSource {
		case gammaIVSourceLiveMid:
			derivedMid++
		case gammaIVSourcePrevClose:
			derivedClose++
		default:
			modelTick++
		}
	}
	return modelTick, derivedMid, derivedClose
}

// computeGammaZeroFor runs the full Phase 2 compute for one underlying.
// The caller (the cache's background goroutine) supplies a ctx bounded
// only by daemon shutdown — not RPC deadlines — and an atomic progress
// counter the fan-out updates as it advances. Returns a populated
// result on success or a classified error on failure (stale spot, no
// usable legs, gateway disconnect).
//
// `underlying` is the symbol whose option chain drives the compute —
// "SPY" or "SPX" today. The function is structurally
// single-underlying: callers that want SPY+SPX run it once per
// underlying and aggregate at a higher layer.
//
// Underlying choice notes (carried forward from the SPY-only era):
// SPY has continuous extended-hours quoting on SMART/ARCA, a single
// trading class (so the secDefOptParams response is a clean per-expiry
// strike grid rather than a multi-class superset that triggers spurious
// "no security definition" errors), and active dealer hedging flow
// that produces real IV ticks pre-market. SPX (the index) by contrast
// has no spot trading outside RTH, so IBKR's model-computation engine
// doesn't push IV ticks for SPX options off-hours, and an SPX-only
// off-hours compute will land few legs. The BS-IV fallback and a
// permissive MinLegCoverageFractionSPX (~0.05) are the off-hours
// posture.
//
// Methodology (bs-gamma-profile-v3-stickymoneyness-0dte-split):
//
//  1. Snapshot SPY spot. Refuse on stale (data_type != live and not
//     empty-pending) — the compute is anchored on a single spot and a
//     known-bad spot poisons everything downstream.
//
//  2. Enumerate expirations + listed strikes via FetchOptionExpiryStrikes.
//
//  3. Pick the nearest N non-0DTE-post-settlement expirations. The
//     0DTE filter is the *evening* of expiration day in NY: at 09:30
//     ET on a 3rd Friday expiry, we still include it; at 16:15 ET
//     on any expiry day, we drop it.
//
//  4. Per expiry, filter listed strikes to those within ±StrikeWidthPct
//     of spot, then cap to the nearest strikes by moneyness. Far-OTM
//     strikes contribute negligibly to dealer GEX and just inflate the
//     leg count / gateway pressure.
//
//  5. Fan-out per-leg subscriptions at WorkerCount concurrency. Each
//     worker captures OI + IV + gateway-Γ for one (expiry, strike,
//     right). Failures (no OI, no IV, gateway dropout) are dropped
//     from the aggregate; the leg count surfaces on the result so
//     consumers can flag low-coverage runs.
//
//  6. Aggregate at spot:
//     dealer GEX = Σ sign(right) × Γ_leg × OI_leg × 100 × spot² × 0.01
//     |gex|      = Σ          |Γ_leg| × OI_leg × 100 × spot² × 0.01
//     The sign convention assumes the 2018 Perfiliev default
//     (long calls, short puts) — documented as a regime hint, not a
//     dollar-precise level.
//
//  7. Sweep spot ∈ [1−SweepRangePct, 1+SweepRangePct] × snapshot_spot
//     in sweepPoints steps. For each scenario spot, recompute Γ_leg
//     via bsGamma with the leg's captured IV and DTE (sticky-IV
//     during sweep; documented v1 limitation). Sum signed
//     contributions to build the profile.
//
//  8. Find the zero crossing on the swept profile via linear interp;
//     compute GapPct from spot.
//
//  9. Rank legs by |Γ × OI| at snapshot spot; surface the top
//     topStrikesK as the magnitude signal (sign-agnostic, robust to
//     the dealer-positioning assumption).
//
// On any step's failure the function returns a classified error;
// step-internal partial failures (e.g., 50/960 legs dropped) attach a
// structured warning instead and continue.
func computeGammaZeroFor(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying string,
	params rpc.GammaZeroParams,
	fetch legFetcher,
	now func() time.Time,
	progress *atomic.Int32,
	logger gammaLogger,
	oiStore *gammaOpenInterestStore,
	grids *expiryGridStore,
) (*rpc.GammaZeroComputed, error) {
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	if fetch == nil {
		fetch = productionLegFetcher
	}
	if now == nil {
		now = time.Now
	}
	sym := strings.TrimSpace(strings.ToUpper(underlying))
	if sym == "" {
		return nil, fmt.Errorf("zero-gamma: empty underlying symbol")
	}
	log := gammaLogf{inner: logger}
	params = normalizeGammaParams(params)
	startWall := now()
	oiState, oiStateErr := loadGammaOIStateForCompute(oiStore)
	if oiStateErr != nil {
		log.Warnf("gamma.oi_store.load err=%v", oiStateErr)
	}
	log.Infof("gamma.kickoff underlying=%s workers=%d expiry_count=%d strike_width_pct=%.2f sweep_range_pct=%.2f",
		sym, params.WorkerCount, params.ExpiryCount, params.StrikeWidthPct, params.SweepRangePct)

	// 1. Underlying spot snapshot.
	progress.Store(2)
	spot, dataType, spotErr := snapshotUnderlyingForGamma(ctx, c, sym, 5*time.Second)
	if spot <= 0 {
		return nil, gammaSpotUnavailableError(sym, spotErr)
	}
	if !isAcceptableDataType(dataType) {
		return nil, fmt.Errorf("zero-gamma: %s spot is %s; refusing to compute on stale data", sym, dataType)
	}
	spotAt := now()

	// 2. Expirations + strikes. The secDefOptParams response is large
	// and streams in over tens of seconds for SPX — see
	// gammaExpiriesFetchTimeout for the measured budget rationale. A
	// failed live fetch falls back to the persisted expiry grid when a
	// recent one exists (expiries_stale warning discloses the age).
	//
	// Branch on underlying: SPX is multi-class (SPX-AM monthlies +
	// SPXW-PM weeklies share third-Friday dates as distinct contracts),
	// so it pulls the classed strike grid and applies a per-class
	// settlement cutoff. SPY-style single-class underlyings keep the
	// existing merged-across-classes path; tradingClass on each leg is
	// just the symbol.
	progress.Store(5)
	picked, gridFallback, err := buildPickedExpirations(c, sym, spotAt, params.ExpiryCount, grids, log)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: fetch %s expiries: %w", sym, err)
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("zero-gamma: no usable %s expirations after 0DTE filtering", sym)
	}
	if gridFallback != nil {
		log.Warnf("gamma.expiries: %s live fetch failed (%v); using cached grid as_of=%s (%dd old)",
			sym, gridFallback.liveErr, gridFallback.asOf.Format(time.RFC3339), gridFallback.staleDays(spotAt))
	}
	progress.Store(10)

	// 4. Build the per-expiry strike grids, ordered ATM-outward.
	// (especially far-OTM strikes that exist only for select events).
	// model-computation engine only fires for actively-quoted strikes.
	// failures and the compute aborts before ever reaching ATM.
	var jobs []gammaLegSpec
	collection := newGammaCollectionDiagnostics(sym, picked)
	strikeBudgetCapped := false
	for _, p := range picked {
		strikes := filterStrikesAroundSpot(p.strikes, spot, params.StrikeWidthPct)
		ordered := sortStrikesATMOutward(strikes, spot)
		strikeCapped := false
		if capped, ok := capStrikesATMOutward(ordered, maxGammaStrikesPerExpiry); ok {
			ordered = capped
			strikeBudgetCapped = true
			strikeCapped = true
		}
		collection.noteStrikeSelection(p, len(strikes), len(ordered), strikeCapped, maxGammaStrikesPerExpiry)
		for _, k := range ordered {
			jobs = append(jobs, gammaLegSpec{expiryYMD: p.expiryYMD, expiryDate: p.date, strike: k, right: "C", tradingClass: p.tradingClass})
			jobs = append(jobs, gammaLegSpec{expiryYMD: p.expiryYMD, expiryDate: p.date, strike: k, right: "P", tradingClass: p.tradingClass})
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no %s strikes within ±%.0f%% of spot %.2f",
			sym, params.StrikeWidthPct*100, spot)
	}
	log.Infof("gamma.jobs total=%d picked=%d spot=%.2f", len(jobs), len(picked), spot)

	// 4b. Bulk-prewarm contracts and drop jobs the gateway does not list —
	jobs, err = prewarmGammaContracts(ctx, c, sym, picked, jobs, collection, log, now)
	if err != nil {
		return nil, err
	}

	// Keep the connection's current market-data mode. The ordinary path is
	// fails to deliver OI off-hours (10/1260 in the last failed run) AND

	// 5. Fan-out over the worker pool. The abort machinery and per-leg
	fan := &gammaLegFanout{
		c:          c,
		sym:        sym,
		spot:       spot,
		spotAt:     spotAt,
		dataType:   dataType,
		fetch:      fetch,
		workers:    params.WorkerCount,
		progress:   progress,
		collection: collection,
		oiState:    oiState,
		startWall:  startWall,
		log:        log,
	}
	legs, liveOIUpdates, stats, err := fan.run(ctx, jobs)
	if err != nil {
		return nil, err
	}
	progress.Store(85)

	// 6-7. Sweep + aggregate.
	skewByExpiry, skewFitQuality, skewFallbacks := buildSkewCurves(legs, spot)
	// Partition legs into the v3 three-bucket horizon split:
	var zeroDTELegs, oneToSevenLegs, termLegs []legData
	// At-spot aggregate: Σ |Γ_i| × OI_i × multiplier × spot² over all
	// Greeks-haven't race left gammaAtSnapshot = 0 and every such leg
	// whenever IV > 0 — which is the OK-leg invariant.
	// same race-affected legs.
	legDiagnostics := buildGammaLegDiagnostics(sym, legs, spot)
	gexLegs, gammaTotalAbs := prepareGEXLegs(legs, spot)
	if len(gexLegs) == 0 {
		diagnostic := gammaSourceFailureDiagnostic(
			sym, spot, spotAt, dataType, picked, legs, stats.derivedIVs, legDiagnostics, collection.finish(time.Since(startWall)),
			params, startWall, now(),
		)
		return diagnostic, fmt.Errorf("zero-gamma: no usable GEX legs: %d priced legs landed, but none had non-zero open-interest-weighted gamma (%s)",
			len(legs), formatGammaLegDiagnostics(legDiagnostics))
	}
	if len(legs) < gammaMinPricedLegs || len(gexLegs) < gammaMinGEXLegs {
		diagnostic := gammaSourceFailureDiagnostic(
			sym, spot, spotAt, dataType, picked, legs, stats.derivedIVs, legDiagnostics, collection.finish(time.Since(startWall)),
			params, startWall, now(),
		)
		return diagnostic, fmt.Errorf("zero-gamma: low usable leg count: %d priced legs/%d OI-weighted GEX legs; need at least %d/%d (%s)",
			len(legs), len(gexLegs), gammaMinPricedLegs, gammaMinGEXLegs, formatGammaLegDiagnostics(legDiagnostics))
	}

	for _, l := range gexLegs {
		calendarDTE, calendarOK := gammaCalendarDTE(l.expiryYMD, l.tradingClass, spotAt)
		switch {
		case calendarOK && calendarDTE <= 0:
			zeroDTELegs = append(zeroDTELegs, l)
		case calendarOK && calendarDTE <= 7:
			oneToSevenLegs = append(oneToSevenLegs, l)
		case calendarOK:
			termLegs = append(termLegs, l)
		case l.dte < zeroDTECutoffYears:
			zeroDTELegs = append(zeroDTELegs, l)
		case l.dte <= nearDTECutoffYears:
			oneToSevenLegs = append(oneToSevenLegs, l)
		default:
			termLegs = append(termLegs, l)
		}
	}

	profile := sweepProfile(gexLegs, spot, params.SweepRangePct, skewByExpiry)
	profile0DTE := sweepProfile(zeroDTELegs, spot, params.SweepRangePct, skewByExpiry)
	profile1to7 := sweepProfile(oneToSevenLegs, spot, params.SweepRangePct, skewByExpiry)
	profileTerm := sweepProfile(termLegs, spot, params.SweepRangePct, skewByExpiry)
	progress.Store(90)

	// 8. Zero crossings: combined + 0DTE + 1-7 + term.
	zg, gammaSign := findZeroCrossing(profile)
	var gapPct *float64
	if zg != nil {
		v := (spot - *zg) / *zg * 100
		gapPct = &v
	}
	zg0DTE, sign0DTE := findZeroCrossing(profile0DTE)
	zg1to7, sign1to7 := findZeroCrossing(profile1to7)
	zgTerm, signTerm := findZeroCrossing(profileTerm)

	// 9. Top strikes by magnitude.
	topStrikes := rankTopStrikesByAbsGEX(gexLegs, spot, topStrikesK, sym)

	// Coverage gate. A compute whose successful-leg fraction falls
	if err := checkLegCoverage(len(legs), len(jobs), stats.throttled); err != nil {
		log.Warnf("gamma.abort reason=low_coverage landed=%d/%d elapsed=%s err=%v",
			len(legs), len(jobs), time.Since(startWall).Round(time.Millisecond), err)
		return nil, err
	}

	// Warnings. Ordered "throttled" first because it explains why
	var warnings []string
	if stats.throttled {
		warnings = append(warnings, "throttled")
	}
	if gammaOIMissingCount(legDiagnostics) > 0 {
		warnings = append(warnings, "oi_missing")
	}
	if strikeBudgetCapped {
		warnings = append(warnings, "strike_budget_capped")
	}
	if gridFallback != nil {
		// Provenance disclosure: legs were enumerated from a cached
		// expiry grid because the live secdef fetch failed. The legs
		warnings = append(warnings, fmt.Sprintf("expiries_stale:%dd", gridFallback.staleDays(spotAt)))
	}
	if zg == nil {
		warnings = append(warnings, "no_crossing_in_window")
	}
	if len(zeroDTELegs) == 0 {
		warnings = append(warnings, "0dte_no_legs")
	}
	if len(oneToSevenLegs) == 0 {
		warnings = append(warnings, "1to7_no_legs")
	}
	if len(termLegs) == 0 {
		warnings = append(warnings, "term_no_legs")
	}
	// Surface per-expiry skew-fit fallbacks so a renderer can show
	// silently using the legacy recipe for that expiry. Each fallback
	for _, expYMD := range skewFallbacks {
		warnings = append(warnings, "skew_fallback:"+expYMD)
	}

	derivedCount := stats.derivedIVs
	if derivedCount > 0 && derivedCount == len(legs) {
		// All legs used the BS-IV fallback — useful signal for the
		// renderer, since the resulting flip level reflects prior-
		// session prices rather than live model ticks.
		warnings = append(warnings, "all_iv_derived")
	}

	// Empty-bucket sign normalisation. findZeroCrossing returns
	if len(zeroDTELegs) == 0 {
		sign0DTE = "no_data"
	}
	if len(oneToSevenLegs) == 0 {
		sign1to7 = "no_data"
	}
	if len(termLegs) == 0 {
		signTerm = "no_data"
	}

	skewModel := ""
	if len(skewFitQuality) > 0 {
		skewModel = "sticky-moneyness-v1"
	}

	// Concentration ratio: share of the sign-agnostic |Γ|·OI sum parked
	// (every leg failed) and for a degenerate sum-of-zeros.
	var topConcentrationPct float64
	if len(topStrikes) > 0 && gammaTotalAbs > 0 {
		topConcentrationPct = topStrikes[0].AbsGEX / gammaTotalAbs * 100
	}

	res := &rpc.GammaZeroComputed{
		SpotUnderlying:          spot,
		SpotAt:                  spotAt,
		DataType:                dataType,
		ZeroGamma:               zg,
		GapPct:                  gapPct,
		GammaSign:               gammaSign,
		Profile:                 profile,
		ZeroGamma0DTE:           zg0DTE,
		Profile0DTE:             profile0DTE,
		GammaSign0DTE:           sign0DTE,
		LegCount0DTE:            len(zeroDTELegs),
		ZeroGamma1to7:           zg1to7,
		Profile1to7:             profile1to7,
		GammaSign1to7:           sign1to7,
		LegCount1to7:            len(oneToSevenLegs),
		ZeroGammaTerm:           zgTerm,
		ProfileTerm:             profileTerm,
		GammaSignTerm:           signTerm,
		LegCountTerm:            len(termLegs),
		SkewModel:               skewModel,
		SkewFitQuality:          skewFitQuality,
		GammaTotalAbs:           gammaTotalAbs,
		GammaTotalAbsConvention: "sign-agnostic",
		TopStrikes:              topStrikes,
		TopConcentrationPct:     topConcentrationPct,
		SweepLowAbs:             spot * (1 - params.SweepRangePct),
		SweepHighAbs:            spot * (1 + params.SweepRangePct),
		Expirations:             pickedDatesFromPicked(picked),
		LegCount:                len(gexLegs),
		PricedLegCount:          len(legs),
		DerivedIVLegs:           derivedCount,
		ModelTickLegs:           stats.modelTickIVs,
		DerivedLiveMidLegs:      stats.derivedMidIVs,
		DerivedPrevCloseLegs:    stats.derivedCloseIVs,
		LegDiagnostics:          legDiagnostics,
		CollectionDiagnostics:   collection.finish(time.Since(startWall)),
		Warnings:                warnings,
		Params:                  params,
		Scope:                   strings.ToLower(sym),
		Source:                  fmt.Sprintf("computed from IBKR %s option chain", sym),
		Method:                  gammaMethodToken,
		MethodologyCitations:    gammaMethodologyCitations,
		AsOf:                    now(),
		DurationMS:              now().Sub(startWall).Milliseconds(),
	}
	progress.Store(100)
	zeroGammaStr := "—"
	if zg != nil {
		zeroGammaStr = fmt.Sprintf("%.2f", *zg)
	}
	log.Infof("gamma.done gex_legs=%d priced_legs=%d/%d model_tick_iv=%d derived_mid_iv=%d derived_close_iv=%d derived_iv=%d spot=%.2f zero_gamma=%s sign=%s elapsed=%s",
		len(gexLegs), len(legs), len(jobs), res.ModelTickLegs, res.DerivedLiveMidLegs, res.DerivedPrevCloseLegs, derivedCount, spot, zeroGammaStr, gammaSign,
		time.Since(startWall).Round(time.Millisecond))
	if err := validateGammaComputed(res); err != nil {
		return nil, err
	}
	if len(liveOIUpdates) > 0 && oiStore != nil {
		// Persist only after the compute is accepted. A rejected low-coverage
		// or no-GEX refresh must not mutate the carried-forward OI state.
		if err := oiStore.SaveMerged(liveOIUpdates); err != nil {
			log.Warnf("gamma.oi_store.save live_updates=%d err=%v", len(liveOIUpdates), err)
		} else {
			log.Infof("gamma.oi_store.save live_updates=%d", len(liveOIUpdates))
		}
	}
	return hydrateGammaComputed(res), nil
}

// prewarmGammaContracts bulk-prewarms option contracts for the picked
// expirations, then filters jobs down to the contracts the gateway actually
// lists. The prewarm is the load-bearing optimization: without it, each of
// the ~1600 legs would independently pay a reqContractDetails round-trip
// with up-to-4-exchange retry loop, which the IBKR per-account throttle
// caps at ~50 attempts before aborting the whole fan-out. The bulk prewarm
// issues one partial-Contract reqContractDetails per expiration (no Strike,
// no Right) and the gateway streams every listed strike × C/P back in one
// burst — same primitive TWS uses internally to populate a chain instantly.
// Round-trip count drops from ~1600 to len(picked) (~6).
//
// TradingClass is load-bearing: omitting it interleaves multi-class
// listings (SPY+SPYW, SPX+SPXW) and cache entries shadow each other.
// SPY+weeklies all share class "SPY"; SPX has two distinct classes
// (SPX-AM + SPXW-PM) which require independent prewarm passes.
//
// Errors per expiry are localised — one timed-out expiry doesn't fail the
// others, and the per-leg fetcher still has its own resolveOptionContract
// fallback for cache misses. The prewarm is a fast path, not a hard
// dependency.
func prewarmGammaContracts(
	ctx context.Context,
	c *ibkrlib.Connector,
	sym string,
	picked []pickedExpiration,
	jobs []gammaLegSpec,
	collection *gammaCollectionDiagnostics,
	log gammaLogf,
	now func() time.Time,
) ([]gammaLegSpec, error) {
	expsByClass := map[string][]string{}
	for _, p := range picked {
		expsByClass[p.tradingClass] = append(expsByClass[p.tradingClass], p.expiryYMD)
	}
	prewarmStart := now()
	prewarmTotal := 0
	prewarmComplete := make(map[string]bool, len(picked))
	prewarmBlocksFallback := make(map[string]bool, len(picked))
	for class, ymds := range expsByClass {
		prewarmResults := c.PrewarmOptionChain(ctx, sym, ymds, class, 30*time.Second)
		for _, r := range prewarmResults {
			key := gammaPrewarmKey(class, r.Expiry)
			prewarmComplete[key] = r.Err == nil && r.Dropped == 0
			prewarmBlocksFallback[key] = r.Dropped > 0 || gammaPrewarmFailureBlocksFallback(r.Err)
			collection.notePrewarm(class, r.Expiry, r.Cached, r.Dropped, r.Err)
			if r.Err != nil {
				log.Warnf("gamma.prewarm class=%s expiry=%s cached=%d dropped=%d elapsed=%s err=%v",
					class, r.Expiry, r.Cached, r.Dropped, r.Elapsed.Round(time.Millisecond), r.Err)
				continue
			}
			if r.Dropped > 0 {
				log.Warnf("gamma.prewarm class=%s expiry=%s cached=%d dropped=%d elapsed=%s err=contract details truncated",
					class, r.Expiry, r.Cached, r.Dropped, r.Elapsed.Round(time.Millisecond))
				continue
			}
			log.Infof("gamma.prewarm class=%s expiry=%s cached=%d dropped=%d elapsed=%s",
				class, r.Expiry, r.Cached, r.Dropped, r.Elapsed.Round(time.Millisecond))
			prewarmTotal += r.Cached
		}
	}
	log.Infof("gamma.prewarm.done total_cached=%d wall_clock=%s",
		prewarmTotal, time.Since(prewarmStart).Round(time.Millisecond))

	// Filter jobs to only those whose (symbol, expiry, strike, right)
	// Throttle=true. Even 5% of such failures trip the throttle-abort
	beforeFilter := len(jobs)
	filteredJobs := jobs[:0]
	incompletePrewarmKept := 0
	for _, j := range jobs {
		prewarmKey := gammaPrewarmKey(j.tradingClass, j.expiryYMD)
		complete := prewarmComplete[prewarmKey]
		cached := c.IsOptionContractCached(sym, j.tradingClass, j.expiryYMD, j.strike, j.right)
		if keepGammaJobAfterPrewarm(complete, cached, prewarmBlocksFallback[prewarmKey]) {
			filteredJobs = append(filteredJobs, j)
			collection.noteRequested(j)
			if !complete {
				incompletePrewarmKept++
			}
		} else {
			collection.noteFailure(j, gammaLegFailureContractMissing)
		}
	}
	jobs = filteredJobs
	if len(jobs) < beforeFilter {
		log.Infof("gamma.filter dropped=%d kept_incomplete_prewarm=%d from=%d to=%d (strikes not in complete prewarm cache)",
			beforeFilter-len(jobs), incompletePrewarmKept, beforeFilter, len(jobs))
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no cached option contracts after prewarm (prewarm landed %d total)",
			prewarmTotal)
	}
	return jobs, nil
}

// gammaFanoutStats carries the fan-out's aggregate counters into the result
// IV-source counts feed the result's provenance fields and the failure
type gammaFanoutStats struct {
	throttled       bool
	derivedIVs      int
	modelTickIVs    int
	derivedMidIVs   int
	derivedCloseIVs int
}

// gammaLegFanout is stage 5 of computeGammaZeroFor: fan the filtered jobs
type gammaLegFanout struct {
	c          *ibkrlib.Connector
	sym        string
	spot       float64
	spotAt     time.Time
	dataType   string
	fetch      legFetcher
	workers    int
	progress   *atomic.Int32
	collection *gammaCollectionDiagnostics
	oiState    map[string]gammaOIRecord
	startWall  time.Time
	log        gammaLogf
}

func (f *gammaLegFanout) run(ctx context.Context, jobs []gammaLegSpec) ([]legData, map[string]gammaOIRecord, gammaFanoutStats, error) {
	// Mutex around the shared aggregation slice and live-OI map; the
	var (
		legs            []legData
		mu              sync.Mutex
		done            atomic.Int32
		noContract      atomic.Int32
		derivedIVs      atomic.Int32
		modelTickIVs    atomic.Int32
		derivedMidIVs   atomic.Int32
		derivedCloseIVs atomic.Int32
		throttledAbort  atomic.Bool
		earlyAbort      atomic.Bool
		total           = int32(len(jobs))
	)
	liveOIUpdates := map[string]gammaOIRecord{}

	// Early-abort watchdog. After earlyAbortAfter elapses, if zero legs
	abortTimer := time.AfterFunc(earlyAbortAfter, func() {
		mu.Lock()
		landed := len(legs)
		mu.Unlock()
		if landed == 0 {
			earlyAbort.Store(true)
		}
	})
	defer abortTimer.Stop()

	runBounded(jobs, f.workers, func(j gammaLegSpec) {
		if ctx.Err() != nil || throttledAbort.Load() || earlyAbort.Load() {
			return
		}
		r := f.fetch(ctx, f.c, f.sym, j.tradingClass, j.expiryYMD, j.strike, j.right, f.spot, f.spotAt, f.dataType)
		// Always increment the progress counter — failed legs still
		d := done.Add(1)
		if r.Throttle {
			nc := noContract.Add(1)
			if throttleDetected(d, nc) {
				throttledAbort.Store(true)
			}
		}
		if total > 0 {
			f.progress.Store(10 + int32(75*float64(d)/float64(total)))
		}
		if !r.OK {
			f.collection.noteFailure(j, r.Failure)
			return
		}
		dte := dteYears(j.expiryYMD, j.tradingClass, f.spotAt)
		if dte <= 0 || r.IV <= 0 {
			// Belt-and-suspenders: skip legs whose DTE/IV degenerate
			f.collection.noteFailure(j, gammaLegFailureTimeout)
			return
		}
		ivSource := gammaIVSource(r)
		switch ivSource {
		case gammaIVSourceLiveMid:
			derivedIVs.Add(1)
			derivedMidIVs.Add(1)
		case gammaIVSourcePrevClose:
			derivedIVs.Add(1)
			derivedCloseIVs.Add(1)
		default:
			modelTickIVs.Add(1)
		}
		oi, oiObserved, oiLive, oiCarried, oiObservedAt := gammaOIForLegResult(
			f.sym, j.tradingClass, j.expiryYMD, j.strike, j.right, r, f.oiState, f.spotAt)
		if oiLive {
			key := gammaOIKey(f.sym, j.tradingClass, j.expiryYMD, j.strike, j.right)
			mu.Lock()
			liveOIUpdates[key] = gammaOIRecordForLeg(f.sym, j.tradingClass, j.expiryYMD, j.strike, j.right, oi, oiObservedAt)
			mu.Unlock()
		}
		f.collection.notePriced(j, ivSource, oi, oiObserved, oiLive, oiCarried, oiObservedAt)
		mu.Lock()
		legs = append(legs, legData{
			expiryYMD:       j.expiryYMD,
			dte:             dte,
			strike:          j.strike,
			right:           j.right,
			tradingClass:    j.tradingClass,
			isCall:          j.right == "C",
			iv:              r.IV,
			ivSource:        ivSource,
			oi:              oi,
			oiObserved:      oiObserved,
			oiLive:          oiLive,
			oiCarried:       oiCarried,
			oiObservedAt:    oiObservedAt,
			gammaAtSnapshot: r.Gamma,
		})
		mu.Unlock()
	})

	stats := gammaFanoutStats{
		throttled:       throttledAbort.Load(),
		derivedIVs:      int(derivedIVs.Load()),
		modelTickIVs:    int(modelTickIVs.Load()),
		derivedMidIVs:   int(derivedMidIVs.Load()),
		derivedCloseIVs: int(derivedCloseIVs.Load()),
	}
	fanoutElapsed := time.Since(f.startWall).Round(time.Millisecond)
	if ctx.Err() != nil {
		f.log.Warnf("gamma.abort reason=ctx_cancelled landed=%d/%d elapsed=%s err=%v",
			len(legs), len(jobs), fanoutElapsed, ctx.Err())
		return nil, nil, stats, ctx.Err()
	}
	if len(legs) == 0 {
		switch {
		case earlyAbort.Load():
			f.log.Warnf("gamma.abort reason=early_abort landed=%d/%d elapsed=%s no_contract=%d",
				len(legs), len(jobs), fanoutElapsed, noContract.Load())
			// Both the model-tick path AND the BS-IV fallback failed
			return nil, nil, stats, fmt.Errorf("zero-gamma: no option data landed in first %ds (neither model ticks nor prior-session prices for BS-IV fallback). Check gateway entitlement and farm-connection notices in the daemon log",
				int(earlyAbortAfter.Seconds()))
		case throttledAbort.Load():
			f.log.Warnf("gamma.abort reason=throttled landed=%d/%d elapsed=%s no_contract=%d",
				len(legs), len(jobs), fanoutElapsed, noContract.Load())
			return nil, nil, stats, fmt.Errorf("zero-gamma: gateway throttled (%d of %d first-wave legs failed contract resolution); aborted to avoid compounding rate-limit pressure",
				noContract.Load(), done.Load())
		default:
			f.log.Warnf("gamma.abort reason=no_legs landed=%d/%d elapsed=%s",
				len(legs), len(jobs), fanoutElapsed)
			return nil, nil, stats, fmt.Errorf("zero-gamma: all %d legs failed to return usable IV/pricing", len(jobs))
		}
	}
	f.log.Infof("gamma.fanout.done landed=%d/%d model_tick_iv=%d derived_mid_iv=%d derived_close_iv=%d derived_iv=%d elapsed=%s",
		len(legs), len(jobs), stats.modelTickIVs, stats.derivedMidIVs, stats.derivedCloseIVs, stats.derivedIVs, fanoutElapsed)
	return legs, liveOIUpdates, stats, nil
}

func gammaSourceFailureDiagnostic(
	sym string,
	spot float64,
	spotAt time.Time,
	dataType string,
	picked []pickedExpiration,
	legs []legData,
	derivedIVs int,
	legDiagnostics *rpc.GammaLegDiagnostics,
	collection []rpc.GammaCollectionDiagnostic,
	params rpc.GammaZeroParams,
	startWall time.Time,
	asOf time.Time,
) *rpc.GammaZeroComputed {
	warnings := []string{"oi_missing"}
	if derivedIVs > 0 && derivedIVs == len(legs) {
		warnings = append(warnings, "all_iv_derived")
	}
	modelTickIVs, derivedMidIVs, derivedCloseIVs := countGammaIVSources(legs)
	out := &rpc.GammaZeroComputed{
		SpotUnderlying:        spot,
		SpotAt:                spotAt,
		DataType:              dataType,
		GammaSign:             "no_data",
		GammaSign0DTE:         "no_data",
		GammaSign1to7:         "no_data",
		GammaSignTerm:         "no_data",
		GammaTotalAbs:         0,
		SweepLowAbs:           spot * (1 - params.SweepRangePct),
		SweepHighAbs:          spot * (1 + params.SweepRangePct),
		Expirations:           pickedDatesFromPicked(picked),
		LegCount:              0,
		PricedLegCount:        len(legs),
		DerivedIVLegs:         derivedIVs,
		ModelTickLegs:         modelTickIVs,
		DerivedLiveMidLegs:    derivedMidIVs,
		DerivedPrevCloseLegs:  derivedCloseIVs,
		LegDiagnostics:        legDiagnostics,
		CollectionDiagnostics: collection,
		Warnings:              warnings,
		Params:                params,
		Scope:                 strings.ToLower(sym),
		Source:                fmt.Sprintf("computed from IBKR %s option chain", sym),
		Method:                gammaMethodToken,
		MethodologyCitations:  gammaMethodologyCitations,
		AsOf:                  asOf,
		DurationMS:            asOf.Sub(startWall).Milliseconds(),
	}
	return hydrateGammaComputed(out)
}

// buildSkewCurves groups legs by expiry, fits a quadratic
//
//	  range) for the result envelope — only fitted expiries appear here
//	- skewFallbacks: list of expiryYMDs that failed to fit and fell back
func buildSkewCurves(legs []legData, snapshotSpot float64) (map[string]SkewCurve, map[string]rpc.SkewFitInfo, []string) {
	byExpiry := map[string][]legData{}
	for _, l := range legs {
		byExpiry[l.expiryYMD] = append(byExpiry[l.expiryYMD], l)
	}
	curves := make(map[string]SkewCurve, len(byExpiry))
	quality := make(map[string]rpc.SkewFitInfo, len(byExpiry))
	var fallbacks []string
	// Stable iteration order so the warnings list is deterministic for
	// regression tests.
	expiryOrder := make([]string, 0, len(byExpiry))
	for k := range byExpiry {
		expiryOrder = append(expiryOrder, k)
	}
	sort.Strings(expiryOrder)
	for _, expYMD := range expiryOrder {
		expLegs := byExpiry[expYMD]
		curve := fitSkewCurve(expLegs, snapshotSpot)
		curves[expYMD] = curve
		if !curve.ok {
			fallbacks = append(fallbacks, expYMD)
			continue
		}
		r2, residualRMS := skewFitStats(curve, expLegs, snapshotSpot)
		quality[expYMD] = rpc.SkewFitInfo{
			Points:      curve.nPoints,
			RSquared:    r2,
			ResidualRMS: residualRMS,
			Range:       [2]float64{curve.mLo, curve.mHi},
		}
	}
	return curves, quality, fallbacks
}

// normalizeGammaParams fills in defaults for unset fields. Mirrors the
// defaults — keeps the wire-shape contract liberal.
func normalizeGammaParams(p rpc.GammaZeroParams) rpc.GammaZeroParams {
	if p.ExpiryCount <= 0 {
		p.ExpiryCount = defaultExpiryCount
	}
	if p.StrikeWidthPct <= 0 {
		p.StrikeWidthPct = defaultStrikeWidthPct
	}
	if p.SweepRangePct <= 0 {
		p.SweepRangePct = defaultSweepRangePct
	}
	if p.WorkerCount <= 0 {
		p.WorkerCount = defaultWorkerCount
	}
	return p
}

// snapshotUnderlyingForGamma polls the connector's market-data cache for
// Caller MUST hold an active market-data subscription for sym for the
// follows. IBKR requires the underlying to be subscribed for the model
// and the briefSnapshotFull subscribe/unsubscribe race that previously
// pollErr is the poll's own reason for stopping and is only meaningful
// predicate never fires, so a usable spot can accompany a deadline error.
func snapshotUnderlyingForGamma(ctx context.Context, c *ibkrlib.Connector, sym string, timeout time.Duration) (spot float64, dataType string, pollErr error) {
	if c == nil {
		return 0, "", ibkrlib.ErrIBKRUnavailable
	}
	sym = normSym(sym)
	var bid, ask, last, mark, closePx float64
	pollErr = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		bid, ask, last, mark, closePx = d.Bid, d.Ask, d.Last, d.MarkPrice, d.Close
		if dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || closePx > 0) {
			dataType = marketDataTypeName(c.MarketDataTypeForSymbol(sym))
		}
		return bid > 0 || ask > 0 || last > 0 || mark > 0
	})
	switch {
	case last > 0:
		spot = last
	case bid > 0 && ask > 0:
		spot = (bid + ask) / 2
	case bid > 0:
		spot = bid
	case ask > 0:
		spot = ask
	case mark > 0:
		spot = mark
	case closePx > 0:
		spot = closePx
	}
	if dataType == "" && spot > 0 {
		dataType = "live"
	}
	return spot, dataType, pollErr
}

type gammaSpotError struct {
	message string
	code    int
	cause   error
}

func (e *gammaSpotError) Error() string { return e.message }
func (e *gammaSpotError) Unwrap() error { return e.cause }

// gammaSpotUnavailableError explains why the underlying spot step found no
// usable price. The generic "no live tick" wording is true of a budget
// timeout and equally true of an account that is simply not subscribed to
// the index — and only the second one is the user's to fix. pollMarketData
// already aborts within milliseconds on a terminal gateway rejection
// (200/321/354/10197), so when that is what happened the code belongs in
// the phase error instead of being discarded.
//
// The broker's rejection text is deliberately not interpolated: it is
// untrusted free text that would reach both the wire error and
// summarizeGammaPhaseFailure, whose digit matching a foreign message could
// silently retarget. The typed code carries the meaning on its own.
func gammaSpotUnavailableError(sym string, pollErr error) error {
	if rejected, ok := errors.AsType[*SubscriptionRejectedError](pollErr); ok {
		// 354 — "Requested market data is not subscribed", the one
		// terminal code the account holder can act on directly.
		if rejected.Rejection.Code == 354 {
			return &gammaSpotError{
				message: fmt.Sprintf("zero-gamma: no %s spot available: this account is not subscribed to %s market data (IBKR 354)", sym, sym),
				code:    354,
			}
		}
		return &gammaSpotError{
			message: fmt.Sprintf("zero-gamma: no %s spot available: gateway rejected the %s market-data subscription (IBKR %d)", sym, sym, rejected.Rejection.Code),
			code:    rejected.Rejection.Code,
		}
	}
	if absent, ok := errors.AsType[*ibkrlib.MarketDataAbsenceError](pollErr); ok {
		return &gammaSpotError{
			message: fmt.Sprintf("zero-gamma: no %s spot available: %s", sym, absent),
			code:    absent.Code,
			cause:   absent,
		}
	}
	return &gammaSpotError{message: fmt.Sprintf("zero-gamma: no %s spot available (gateway returned no live tick)", sym)}
}

// throttleDetected reports whether the fan-out's observed
// contract-resolve failure ratio is high enough to abort. Pure helper
func throttleDetected(done, noContract int32) bool {
	if done < throttleSampleSize {
		return false
	}
	return float64(noContract)/float64(done) > throttleAbortPct
}

// isAcceptableDataType reports whether the gateway's per-reqID feed
// state is acceptable for the zero-gamma compute.
//
// Accepted:
//   - "live" — real-time ticks; obvious choice.
//   - "frozen" — IBKR's term for the last live tick captured before
//     a session boundary or feed pause. For SPX this is typically
//     yesterday's regular-session close. The spec explicitly says
//     a daily refresh is sufficient, and frozen is exactly that:
//     the official anchor for an end-of-day-style compute, just
//     labelled honestly. Renderers can dim the headline by reading
//     `data_type=frozen` from the result envelope.
//   - "" — no marketDataType notice has arrived yet (typical in the
//     first few hundred ms of a fresh subscription). Treated as
//     live per rpc.IsLiveDataType convention.
//   - "delayed" — only when the production leg fetcher can bind every
//     option IV to IBKR's delayed model-computation tick 83. The source is
//     15-20 minutes old but clock-aligned end to end and labeled on the
//     result; that is inside gamma's one-hour RTH rankability horizon.
//
// Rejected:
//   - "delayed-frozen" — the prior close, not the RTH delayed stream.
//   - Anything else (unexpected value) — stale-by-default.
func isAcceptableDataType(dt string) bool {
	switch dt {
	case "", rpc.MarketDataLive, rpc.MarketDataFrozen, rpc.MarketDataDelayed:
		return true
	default:
		return false
	}
}

// classSettlementInstant returns the NY-time settlement instant for an
// opening quotation, but IBKR/TWS keys those contracts by their Thursday
func classSettlementInstant(tradingClass string, year int, month time.Month, day int, loc *time.Location) time.Time {
	if strings.EqualFold(strings.TrimSpace(tradingClass), "SPX") {
		sessionDate := time.Date(year, month, day, 0, 0, 0, 0, loc)
		if isSPXAMMonthlyLastTradeDate(sessionDate.Format("2006-01-02")) {
			sessionDate = sessionDate.AddDate(0, 0, 1)
			return time.Date(sessionDate.Year(), sessionDate.Month(), sessionDate.Day(), 9, 30, 0, 0, loc)
		}
		return time.Date(year, month, day, 9, 30, 0, 0, loc)
	}
	return time.Date(year, month, day, 16, 0, 0, 0, loc)
}

// classSettlementBuffer is the post-settlement grace window the
// expiry-filter uses before tagging a same-day listing as "expired."
// Mirrors the original 15-minute buffer on the unified 16:15 cutoff;
// applied symmetrically to AM-settled and PM-settled classes so the
// boundary semantics stay consistent across the SPX/SPXW split.
const classSettlementBuffer = 15 * time.Minute

func selectExpirationCandidates(strikes map[string][]float64, tradingClass string, now time.Time) []string {
	loc := newYorkLocation()
	nyNow := now.In(loc)
	today := nyNow.Format("2006-01-02")
	settlementCutoff := classSettlementInstant(tradingClass, nyNow.Year(), nyNow.Month(), nyNow.Day(), loc).Add(classSettlementBuffer)
	pastCutoff := nyNow.After(settlementCutoff)

	var candidates []string
	for date := range strikes {
		if date < today {
			continue // expired any time before today
		}
		if date == today && pastCutoff {
			continue // 0DTE post-settlement
		}
		candidates = append(candidates, date)
	}
	sort.Strings(candidates)
	return candidates
}

// pickExpirationSlots applies the front-week, EOW, monthly, quarterly, then
// candidates: sorted ascending, must already be non-expired.
func pickExpirationSlots(candidates []string, nyNow time.Time, count int) []string {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}
	used := make(map[string]struct{}, count)
	picks := make([]string, 0, count)

	// attempt tries to add the first candidate matching predicate that
	// hasn't been used yet. Returns true when the slot was filled.
	attempt := func(predicate func(string) bool) bool {
		if len(picks) >= count {
			return false
		}
		for _, d := range candidates {
			if _, ok := used[d]; ok {
				continue
			}
			if predicate(d) {
				used[d] = struct{}{}
				picks = append(picks, d)
				return true
			}
		}
		return false
	}

	always := func(string) bool { return true }

	// Slots 1-2: front-week-1, front-week-2 — nearest two unused.
	attempt(always)
	attempt(always)

	// Slot 3: EOW — this calendar week's Friday from nyNow (>= today).
	thisFri := thisWeekFriday(nyNow)
	attempt(func(d string) bool { return d == thisFri })

	// Slot 4: next-monthly — next 3rd-Friday in candidates.
	attempt(isThirdFridayDate)

	// Slot 5: next-quarterly — next 3rd-Friday of Mar/Jun/Sep/Dec.
	attempt(isQuarterlyThirdFridayDate)

	// Fill: nearest unused until count is reached.
	for _, d := range candidates {
		if len(picks) >= count {
			break
		}
		if _, ok := used[d]; ok {
			continue
		}
		used[d] = struct{}{}
		picks = append(picks, d)
	}

	sort.Strings(picks)
	return picks
}

// thisWeekFriday returns the YYYY-MM-DD of the calendar Friday >= nyNow's
func thisWeekFriday(nyNow time.Time) string {
	daysToFri := (int(time.Friday) - int(nyNow.Weekday()) + 7) % 7
	fri := time.Date(nyNow.Year(), nyNow.Month(), nyNow.Day()+daysToFri, 0, 0, 0, 0, nyNow.Location())
	return fri.Format("2006-01-02")
}

// isThirdFridayDate reports whether a YYYY-MM-DD string is the 3rd
func isThirdFridayDate(yyyyMMdd string) bool {
	t, err := time.Parse("2006-01-02", yyyyMMdd)
	if err != nil {
		return false
	}
	if t.Weekday() != time.Friday {
		return false
	}
	d := t.Day()
	return d >= 15 && d <= 21
}

func isSPXAMMonthlyLastTradeDate(yyyyMMdd string) bool {
	t, err := time.Parse("2006-01-02", yyyyMMdd)
	if err != nil || t.Weekday() != time.Thursday {
		return false
	}
	return isThirdFridayDate(t.AddDate(0, 0, 1).Format("2006-01-02"))
}

func isSPXAMQuarterlyLastTradeDate(yyyyMMdd string) bool {
	if !isSPXAMMonthlyLastTradeDate(yyyyMMdd) {
		return false
	}
	t, _ := time.Parse("2006-01-02", yyyyMMdd)
	switch t.AddDate(0, 0, 1).Month() {
	case time.March, time.June, time.September, time.December:
		return true
	}
	return false
}

// isQuarterlyThirdFridayDate reports whether a YYYY-MM-DD is the 3rd
func isQuarterlyThirdFridayDate(yyyyMMdd string) bool {
	if !isThirdFridayDate(yyyyMMdd) {
		return false
	}
	t, _ := time.Parse("2006-01-02", yyyyMMdd)
	switch t.Month() {
	case time.March, time.June, time.September, time.December:
		return true
	}
	return false
}

// sortStrikesATMOutward returns the input strike list reordered by
// pre-sort order); strikes are float64 so exact ties on $0.50/$1
func sortStrikesATMOutward(strikes []float64, spot float64) []float64 {
	if len(strikes) <= 1 {
		return strikes
	}
	out := make([]float64, len(strikes))
	copy(out, strikes)
	sort.SliceStable(out, func(i, j int) bool {
		return math.Abs(out[i]-spot) < math.Abs(out[j]-spot)
	})
	return out
}

func capStrikesATMOutward(strikes []float64, maxCount int) ([]float64, bool) {
	if maxCount <= 0 || len(strikes) <= maxCount {
		return strikes, false
	}
	return strikes[:maxCount], true
}

// filterStrikesAroundSpot returns the subset of listed strikes within
func filterStrikesAroundSpot(strikes []float64, spot, widthPct float64) []float64 {
	if spot <= 0 || widthPct <= 0 || len(strikes) == 0 {
		return nil
	}
	lo := spot * (1 - widthPct)
	hi := spot * (1 + widthPct)
	var out []float64
	for _, k := range strikes {
		if k >= lo && k <= hi {
			out = append(out, k)
		}
	}
	sort.Float64s(out)
	return out
}

// compactExpiry converts YYYY-MM-DD to YYYYMMDD — the format
func compactExpiry(date string) string {
	if len(date) == 10 && date[4] == '-' && date[7] == '-' {
		return date[:4] + date[5:7] + date[8:10]
	}
	return date // best-effort
}

func gammaPrewarmKey(tradingClass, expiryYMD string) string {
	return strings.ToUpper(strings.TrimSpace(tradingClass)) + "|" + strings.TrimSpace(expiryYMD)
}

// keepGammaJobAfterPrewarm reports whether a leg job survives the
// job survives only while the per-leg resolver fallback is still an option:
// its expiry's prewarm must be incomplete (a complete prewarm IS the
// prewarm failure class must not block fallback (zero-detail and timeout
func keepGammaJobAfterPrewarm(prewarmComplete, cached, fallbackBlocked bool) bool {
	if prewarmComplete || fallbackBlocked {
		return cached
	}
	return true
}

func gammaPrewarmFailureBlocksFallback(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") {
		return true
	}
	if classifyGammaLegFailure(err) != gammaLegFailureContractMissing {
		return false
	}
	return strings.Contains(lower, "returned zero contract details")
}

type gammaCollectionDiagnostics struct {
	mu         sync.Mutex
	underlying string
	order      []string
	byKey      map[string]*rpc.GammaCollectionDiagnostic
}

func newGammaCollectionDiagnostics(underlying string, picked []pickedExpiration) *gammaCollectionDiagnostics {
	out := &gammaCollectionDiagnostics{
		underlying: strings.ToUpper(strings.TrimSpace(underlying)),
		byKey:      make(map[string]*rpc.GammaCollectionDiagnostic, len(picked)),
	}
	for _, p := range picked {
		key := gammaPrewarmKey(p.tradingClass, p.expiryYMD)
		if _, ok := out.byKey[key]; ok {
			continue
		}
		out.order = append(out.order, key)
		out.byKey[key] = &rpc.GammaCollectionDiagnostic{
			Underlying:             out.underlying,
			TradingClass:           strings.ToUpper(strings.TrimSpace(p.tradingClass)),
			Expiry:                 p.date,
			MarketDataGenericTicks: ibkrlib.OptionSubscriptionGenericTicks,
			OIGenericTickRequested: gammaOIGenericTickRequested(ibkrlib.OptionSubscriptionGenericTicks),
			StrikeCap:              maxGammaStrikesPerExpiry,
			ExpiryCapTruncated:     p.capTruncated,
		}
	}
	return out
}

func (d *gammaCollectionDiagnostics) noteStrikeSelection(p pickedExpiration, strikeCandidates, strikeSelected int, capped bool, cap int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rowLocked(p.tradingClass, p.expiryYMD)
	if row == nil {
		return
	}
	row.StrikeCandidates = strikeCandidates
	row.StrikeSelected = strikeSelected
	row.StrikeCap = cap
	row.StrikeCapTruncated = capped
}

func (d *gammaCollectionDiagnostics) notePrewarm(tradingClass, expiryYMD string, cached, dropped int, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rowLocked(tradingClass, expiryYMD)
	if row == nil {
		return
	}
	row.QualifiedContracts = cached
	if dropped > 0 {
		row.ContractMissingLegs += dropped
	}
	if err == nil {
		return
	}
	switch classifyGammaLegFailure(err) {
	case gammaLegFailureTimeout:
		row.Timeouts++
	case gammaLegFailurePacing:
		row.PacingErrors++
	case gammaLegFailureFarm:
		row.FarmErrors++
	case gammaLegFailureEntitlement:
		row.EntitlementErrors++
	case gammaLegFailureContractMissing:
		if gammaPrewarmFailureBlocksFallback(err) {
			return
		}
		row.ContractMissingLegs++
	default:
		row.SubscriptionRejects++
	}
}

func (d *gammaCollectionDiagnostics) noteRequested(j gammaLegSpec) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if row := d.rowLocked(j.tradingClass, j.expiryYMD); row != nil {
		row.RequestedLegs++
	}
}

func (d *gammaCollectionDiagnostics) notePriced(j gammaLegSpec, ivSource string, oi int64, oiObserved, oiLive, oiCarried bool, observedAt time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rowLocked(j.tradingClass, j.expiryYMD)
	if row == nil {
		return
	}
	row.PricedLegs++
	switch ivSource {
	case gammaIVSourceLiveMid:
		row.DerivedLiveMidLegs++
	case gammaIVSourcePrevClose:
		row.DerivedPrevCloseLegs++
	default:
		row.ModelTickLegs++
	}
	switch {
	case oiLive:
		row.OILiveObservedLegs++
	case oiCarried:
		row.OICarriedForwardLegs++
		if row.CarriedForwardSource == "" {
			row.CarriedForwardSource = gammaOIStateFilename
		}
		if row.CarriedForwardObservedAt == "" && !observedAt.IsZero() {
			row.CarriedForwardObservedAt = observedAt.Format(time.RFC3339)
		}
	case !oiObserved:
		row.OIMissingLegs++
	}
	if oiObserved && !oiLive && !oiCarried {
		row.OILiveObservedLegs++
	}
	if oi > 0 {
		row.OIPositiveLegs++
	}
}

func (d *gammaCollectionDiagnostics) noteFailure(j gammaLegSpec, failure string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rowLocked(j.tradingClass, j.expiryYMD)
	if row == nil {
		return
	}
	switch failure {
	case gammaLegFailureContractMissing:
		row.ContractMissingLegs++
	case gammaLegFailureTimeout, "":
		row.Timeouts++
	case gammaLegFailurePacing:
		row.PacingErrors++
	case gammaLegFailureFarm:
		row.FarmErrors++
	case gammaLegFailureEntitlement:
		row.EntitlementErrors++
	default:
		row.SubscriptionRejects++
	}
}

func (d *gammaCollectionDiagnostics) rowLocked(tradingClass, expiryYMD string) *rpc.GammaCollectionDiagnostic {
	key := gammaPrewarmKey(tradingClass, expiryYMD)
	row := d.byKey[key]
	if row != nil {
		return row
	}
	row = &rpc.GammaCollectionDiagnostic{
		Underlying:             d.underlying,
		TradingClass:           strings.ToUpper(strings.TrimSpace(tradingClass)),
		Expiry:                 displayExpiry(expiryYMD),
		MarketDataGenericTicks: ibkrlib.OptionSubscriptionGenericTicks,
		OIGenericTickRequested: gammaOIGenericTickRequested(ibkrlib.OptionSubscriptionGenericTicks),
		StrikeCap:              maxGammaStrikesPerExpiry,
	}
	d.byKey[key] = row
	d.order = append(d.order, key)
	return row
}

func (d *gammaCollectionDiagnostics) finish(elapsed time.Duration) []rpc.GammaCollectionDiagnostic {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.byKey) == 0 {
		return nil
	}
	out := make([]rpc.GammaCollectionDiagnostic, 0, len(d.order))
	for _, key := range d.order {
		row := d.byKey[key]
		if row == nil {
			continue
		}
		row.CollectionDurationMS = elapsed.Milliseconds()
		row.OISourceStatus = gammaOISourceStatus(*row)
		out = append(out, *row)
	}
	return out
}

func gammaOISourceStatus(row rpc.GammaCollectionDiagnostic) string {
	live := row.OILiveObservedLegs
	carried := row.OICarriedForwardLegs
	missing := row.OIMissingLegs
	switch {
	case live > 0 && carried == 0 && missing == 0:
		return gammaOISourceLiveObserved
	case live == 0 && carried > 0 && missing == 0:
		return gammaOISourceCarriedForward
	case live > 0 || carried > 0:
		return gammaOISourceMixed
	default:
		return gammaOISourceMissing
	}
}

func gammaOIGenericTickRequested(genericTicks string) bool {
	for tick := range strings.SplitSeq(genericTicks, ",") {
		if strings.TrimSpace(tick) == ibkrlib.OptionOpenInterestGenericTick {
			return true
		}
	}
	return false
}

func loadGammaOIStateForCompute(store *gammaOpenInterestStore) (map[string]gammaOIRecord, error) {
	if store == nil {
		return nil, nil
	}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	return state, nil
}

func gammaOIForLegResult(
	underlying, tradingClass, expiryYMD string,
	strike float64,
	right string,
	result legResult,
	state map[string]gammaOIRecord,
	now time.Time,
) (oi int64, observed, live, carried bool, observedAt time.Time) {
	if result.OIObserved {
		return result.OI, true, true, false, now
	}
	if len(state) == 0 {
		return 0, false, false, false, time.Time{}
	}
	key := gammaOIKey(underlying, tradingClass, expiryYMD, strike, right)
	rec, ok := state[key]
	if !ok || !validCarriedGammaOI(rec, now) {
		return 0, false, false, false, time.Time{}
	}
	return rec.OpenInterest, true, false, true, rec.ObservedAt
}

// dteYears computes years-to-expiry from an option's YYYYMMDD expiry
// string under the correct settlement-instant for the option's trading
// class. SPX-class AM monthlies are keyed by the Thursday last-trade date
// and settle Friday 09:30 ET; SPXW weeklies, SPY, and equities expire at
// 16:00 ET (PM close). Empty tradingClass falls back to 16:00 ET —
// back-compat for the SPY-only path before the SPX coverage arc.
//
// Zero on parse failure or non-positive deltas — the compute's per-leg
// gate filters those out.
//
// Why this matters: an SPX-class third-Friday option at 10:00 ET on
// expiry day has already settled at 09:30; pricing it with 6.5 extra
// hours of TTE under the legacy 16:00 instant would over-state its
// gamma. The aggregate is dollar-significant — third-Friday SPX gamma
// dominates the day-of-expiry book.
func dteYears(expiryYMD, tradingClass string, now time.Time) float64 {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	day, err := time.ParseInLocation("20060102", expiryYMD, loc)
	if err != nil {
		return 0
	}
	expWall := classSettlementInstant(tradingClass, day.Year(), day.Month(), day.Day(), loc)
	delta := expWall.Sub(now.In(loc))
	if delta <= 0 {
		return 0
	}
	return delta.Hours() / (24 * 365.0)
}

func gammaCalendarDTE(expiryYMD, tradingClass string, now time.Time) (int, bool) {
	loc := newYorkLocation()
	day, err := time.ParseInLocation("20060102", expiryYMD, loc)
	if err != nil {
		return 0, false
	}
	nyNow := now.In(loc)
	today := time.Date(nyNow.Year(), nyNow.Month(), nyNow.Day(), 0, 0, 0, 0, loc)
	expiryDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	if strings.EqualFold(strings.TrimSpace(tradingClass), "SPX") {
		settlement := classSettlementInstant(tradingClass, day.Year(), day.Month(), day.Day(), loc)
		settlementDay := time.Date(settlement.Year(), settlement.Month(), settlement.Day(), 0, 0, 0, 0, loc)
		if expiryDay.Before(today) && today.Equal(settlementDay) && nyNow.Before(settlement.Add(classSettlementBuffer)) {
			return 0, true
		}
	}
	return int(expiryDay.Sub(today).Hours() / 24), true
}

// sweepProfile builds the (spot, signed_gex) sweep over [1−range,
// 1+range] × snapshotSpot in sweepPoints steps. Each scenario spot
// recomputes per-leg Γ via Black-Scholes.
//
// skewByExpiry maps each leg's expiryYMD to a fitted skew curve. For
// each leg in the inner loop the IV is looked up at the
// scenario-spot's moneyness (σ = curve.IVAtMoneyness(ln(K/S_scenario))),
// implementing the sticky-moneyness convention. When the curve for an
// expiry is unfit (fewer than 3 points or degenerate solve), the leg
// falls back to its captured IV — the v1 sticky-IV behaviour for that
// expiry only. Pass nil to disable skew lookups entirely (used by the
// fallback test path).
func sweepProfile(legs []legData, snapshotSpot, sweepRangePct float64, skewByExpiry map[string]SkewCurve) []rpc.GammaProfilePoint {
	if snapshotSpot <= 0 || sweepRangePct <= 0 || sweepPoints < 2 {
		return nil
	}
	loSpot := snapshotSpot * (1 - sweepRangePct)
	hiSpot := snapshotSpot * (1 + sweepRangePct)
	step := (hiSpot - loSpot) / float64(sweepPoints-1)

	out := make([]rpc.GammaProfilePoint, sweepPoints)
	for i := range sweepPoints {
		scenarioSpot := loSpot + float64(i)*step
		gex := 0.0
		for _, l := range legs {
			σ := l.iv
			if skewByExpiry != nil {
				curve, ok := skewByExpiry[l.expiryYMD]
				if ok && curve.ok {
					m := math.Log(l.strike / scenarioSpot)
					if v := curve.IVAtMoneyness(m); v > 0 {
						σ = v
					}
				}
			}
			γ := bsGamma(scenarioSpot, l.strike, l.dte, σ, 0, 0)
			gex += dealerGEX(γ, float64(l.oi), 100, scenarioSpot, l.isCall)
		}
		out[i] = rpc.GammaProfilePoint{Spot: scenarioSpot, GEX: gex}
	}
	return out
}

// rankTopStrikesByAbsGEX returns the top-k legs ranked by sign-agnostic
func rankTopStrikesByAbsGEX(legs []legData, spot float64, k int, underlying string) []rpc.StrikeConcentration {
	if k <= 0 || len(legs) == 0 {
		return nil
	}
	type ranked struct {
		row    rpc.StrikeConcentration
		absGEX float64
	}
	rows := make([]ranked, 0, len(legs))
	for _, l := range legs {
		v := absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
		if v == 0 {
			// Skip legs where the gateway didn't deliver a gamma tick;
			continue
		}
		rows = append(rows, ranked{
			row: rpc.StrikeConcentration{
				Underlying:   underlying,
				TradingClass: l.tradingClass,
				Strike:       l.strike,
				Expiry:       l.expiryYMD[:4] + "-" + l.expiryYMD[4:6] + "-" + l.expiryYMD[6:8],
				Right:        l.right,
				AbsGEX:       v,
				OI:           l.oi,
			},
			absGEX: v,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].absGEX > rows[j].absGEX })
	if len(rows) > k {
		rows = rows[:k]
	}
	out := make([]rpc.StrikeConcentration, len(rows))
	for i, r := range rows {
		out[i] = r.row
	}
	return out
}

func prepareGEXLegs(legs []legData, spot float64) ([]legData, float64) {
	gexLegs := make([]legData, 0, len(legs))
	total := 0.0
	for _, l := range legs {
		l.gammaAtSnapshot = bsGamma(spot, l.strike, l.dte, l.iv, 0, 0)
		v := absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
		if v == 0 {
			continue
		}
		total += v
		gexLegs = append(gexLegs, l)
	}
	return gexLegs, total
}
