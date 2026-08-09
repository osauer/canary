package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"time"
)

const marketAuthorityWriteAttempts = 4

// attachCoreMarketAuthority is the single startup switch for every
// market-observation projection. The Server constructors still build legacy
// codecs so the cutover importer can read them, but startup must call this
// under the persistence lock before opening the daemon socket or gateway.
// After success, no listed store reads or writes a legacy path.
func (s *Server) attachCoreMarketAuthority(store *corestore.Store) error {
	if s == nil {
		return errors.New("attach market authority: nil server")
	}
	if store == nil {
		return errors.New("attach market authority: nil corestore")
	}
	if s.zeroGamma == nil || s.gammaOI == nil || s.gammaGrids == nil || s.regimeHistory == nil || s.regimeSeries == nil || s.streaks == nil || s.breadth == nil {
		return errors.New("attach market authority: market stores are not installed")
	}
	if err := s.zeroGamma.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma authority: %w", err)
	}
	if err := s.gammaOI.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma OI authority: %w", err)
	}
	if err := s.gammaGrids.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma expiry-grid authority: %w", err)
	}
	if err := s.regimeHistory.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach regime HMDS authority: %w", err)
	}
	if err := s.regimeSeries.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach official-series authority: %w", err)
	}
	if err := s.streaks.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach regime-streak authority: %w", err)
	}
	if err := s.breadth.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach breadth authority: %w", err)
	}
	if s.contractStore == nil {
		// The legacy constructor can leave this nil when HOME/XDG resolution
		// fails. daemon.db does not need a cache directory, so install a cold
		// codec and attach it directly.
		s.contractStore = ibkrlib.NewContractStore("")
	}
	if err := s.contractStore.UseAuthority(coreContractCacheAuthority{store: store}); err != nil {
		return fmt.Errorf("attach contract-cache authority: %w", err)
	}
	if s.fxRates == nil {
		s.fxRates = newFXRateCache()
	}
	if err := s.fxRates.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach FX-rate authority: %w", err)
	}
	if s.earnings == nil {
		logf := func(string, ...any) {}
		if s.logger != nil {
			logf = s.logger.Warnf
		}
		s.earnings = newEarningsCacheMemory(logf)
	}
	if err := s.earnings.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach earnings authority: %w", err)
	}
	if s.earningsTerminal == nil {
		s.earningsTerminal = newEarningsTerminalStore("")
	}
	if err := s.earningsTerminal.UseCoreStore(context.Background(), store, s.orderNow()); err != nil {
		return fmt.Errorf("attach earnings terminal authority: %w", err)
	}
	if s.marketEvents == nil {
		s.marketEvents = newMarketEventCache(s.now)
	}
	if err := s.marketEvents.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach market-events authority: %w", err)
	}
	if s.membersCachePath != "" {
		if err := spx.UseCoreMembersStore(s.membersCachePath, store); err != nil {
			return fmt.Errorf("attach SPX-members authority: %w", err)
		}
	}
	return nil
}

// loadMarketState reads the current typed market document from daemon.db.
// Callers validate the domain envelope after this transport-level read.
func loadMarketState(store *corestore.Store, scopeKey, kind string) ([]byte, bool, error) {
	if store == nil {
		return nil, false, errors.New("market observation authority is not attached")
	}
	doc, ok, err := store.GetStateDocument(context.Background(), scopeKey, kind)
	if err != nil || !ok {
		return nil, ok, err
	}
	return append([]byte(nil), doc.JSON...), true, nil
}

// saveMarketState atomically publishes the current operational document. Beta
// builds once duplicated every refresh into the immutable observation ledger;
// production has no generic history reader, so that copy only caused unbounded
// authority growth. Narrow evidence classes that require receipts use their
// own typed writers instead.
func saveMarketState(store *corestore.Store, scopeKey, stateKind string, input corestore.ObservationInput) error {
	return saveMarketStateContext(context.Background(), store, scopeKey, stateKind, input)
}

func saveMarketStateContext(ctx context.Context, store *corestore.Store, scopeKey, stateKind string, input corestore.ObservationInput) error {
	if store == nil {
		return errors.New("market observation authority is not attached")
	}
	return saveMarketDocument(ctx, store, scopeKey, stateKind, input.Payload)
}

// saveMarketDocument publishes the current document without appending an
// observation. It is the common path for refreshable current market state.
func saveMarketDocument(ctx context.Context, store *corestore.Store, scopeKey, stateKind string, payload []byte) error {
	if store == nil {
		return errors.New("market observation authority is not attached")
	}
	for range marketAuthorityWriteAttempts {
		doc, ok, err := store.GetStateDocument(ctx, scopeKey, stateKind)
		if err != nil {
			return err
		}
		var revision int64
		if ok {
			revision = doc.Revision
		}
		_, err = store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey:         scopeKey,
			Kind:             stateKind,
			ExpectedRevision: revision,
			JSON:             payload,
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return fmt.Errorf("save market document %s/%s: %w after %d attempts", scopeKey, stateKind, corestore.ErrRevisionConflict, marketAuthorityWriteAttempts)
}

// expiryIVCache memoises per-(symbol, expiry) ATM implied-volatility lookups so
// repeated daemon consumers skip the per-expiry market-data subscribe cycle.
// The cache survives across adapter calls in the daemon process.
//
// TTL varies with market phase: short during regular trading hours when
// IV moves intraday, long outside RTH when nothing is recomputing. The
// dividing line is intentionally generous (3am – 9pm ET catches all four
// US sessions plus a buffer) so we don't have to reason about overnight
// futures vs equities.
type expiryIVCache struct {
	inner *ttlMap[expiryIVKey, expiryIVEntry]
}

type expiryIVKey struct {
	symbol string // upper-cased
	expiry string // YYYY-MM-DD
}

type expiryIVEntry struct {
	iv      float64 // 0 when status != "ok"
	status  string  // "ok" | "timeout" | "unavailable"
	source  string  // "live_model" | "unavailable"
	quality string  // "live_model" | "unavailable"
	asOf    time.Time
}

func newExpiryIVCache() *expiryIVCache {
	return &expiryIVCache{
		inner: newTTLMap[expiryIVKey, expiryIVEntry](func(_ expiryIVEntry, now time.Time) time.Duration {
			return expiryIVTTL(now)
		}),
	}
}

// get returns (entry, true) when a non-stale entry exists for the key.
// Staleness is decided against now per expiryIVTTL — callers don't have
// to thread their own clock in (tests inject via testNow if needed).
func (c *expiryIVCache) get(symbol, expiry string, now time.Time) (expiryIVEntry, bool) {
	return c.inner.get(expiryIVKey{symbol: symbol, expiry: expiry}, now)
}

// put records the IV result. Negative-cache "timeout" and "unavailable"
// entries get the same TTL as successful fills — without that, a single
// dead expiry would be re-fetched on every chain refresh and chew through
// the gateway's market-data slot budget.
func (c *expiryIVCache) put(symbol, expiry string, e expiryIVEntry, now time.Time) {
	c.inner.put(expiryIVKey{symbol: symbol, expiry: expiry}, e, now)
}

// expiryIVTTL picks the freshness budget for a cached IV based on whether
// now falls in a US-equities-active window. We don't have a market phase
// per symbol on the daemon side — every option here is on a US underlying,
// so wall-clock America/New_York is the right proxy. Errors loading the
// zone (rare; would mean the system has no tzdata) fall back to UTC and
// the conservative 60s TTL.
//
// 9:30am – 4pm ET, Mon–Fri  → 60 s   (IV moves intraday, freshness matters)
// any other hour            →  4 h   (IV is approximately static; caching
//
//	hard avoids burning slots overnight)
var expiryIVNYZone = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func expiryIVTTL(now time.Time) time.Duration {
	local := now.In(expiryIVNYZone)
	wd := local.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return 4 * time.Hour
	}
	hour := local.Hour()
	min := local.Minute()
	// 9:30 – 16:00 ET ≡ minutes-since-midnight in [570, 960).
	mins := hour*60 + min
	if mins >= 570 && mins < 960 {
		return 60 * time.Second
	}
	return 4 * time.Hour
}

// greeksCache memoises per-option model-computation Greeks so the
// positions handler doesn't churn a fresh option subscription for every
// held leg on every invocation. The keys are the OPRA-style option
// market-data keys (the same shape SubscribeOption returns), so callers
// in handlePositionsList build the same key from the position's contract
// fields and look up directly.
//
// TTL is tuned for actionability — Greeks shift slowly relative to spot
// (delta drifts on the order of minutes for liquid names), and a stale
// cached value is far better than nil for the portfolio-aggregate
// rendering. 60 s strikes the balance: short enough that an aggressive
// `watch -n 60 canary positions` re-warms each cycle, long enough that
// back-to-back invocations during a decision pause cost zero gateway
// round trips.
//
// Negative caching is essential. The model-computation tick (msg 21
// tickType 13) silently drops for far-OTM and illiquid OOH legs; we
// still want to remember "we tried and got nothing" so we don't re-
// poll a dead stream on the next call.
//
// Negative entries use a much shorter TTL than positive entries. A
// cold-daemon prewarm commonly fails to receive model ticks within
// the 2.5 s budget — the option-tick pipeline takes a few seconds to
// settle on a fresh connector. Under a single 60 s TTL, that one
// transient miss locked retries out for a full minute, well past the
// point the gateway started delivering ticks. A short negative TTL
// lets the next prewarm re-subscribe and capture the live values
// promptly, while still protecting the gateway from rapid re-poll
// loops within a few seconds.
type greeksCache struct {
	inner *ttlMap[string, greeksEntry]
}

type greeksEntry struct {
	value      ibkrlib.Greeks
	underlying float64 // model-computation underlying price, 0 if unavailable
	ok         bool    // false → negative cache: we tried and got nothing valid
}

const (
	// greeksTTL bounds positive entries — captured Greeks shift slowly
	// relative to spot (delta drifts on the order of minutes for liquid
	// names), and a stale cached value is far better than nil for the
	// portfolio aggregate. 60 s is short enough that an aggressive
	// `watch -n 60 canary positions` re-warms each cycle, long enough that
	// back-to-back invocations during a decision pause cost zero gateway
	// round trips.
	greeksTTL = 60 * time.Second

	// greeksNegativeTTL bounds ok=false entries. Held short so a single
	// cold-handshake miss doesn't lock out retries for a full minute —
	// see type-doc comment. Long enough to suppress a tight retry loop
	// from a misbehaving caller within the same RPC tick.
	greeksNegativeTTL = 10 * time.Second
)

func newGreeksCache() *greeksCache {
	return &greeksCache{
		inner: newTTLMap[string, greeksEntry](func(e greeksEntry, _ time.Time) time.Duration {
			if !e.ok {
				return greeksNegativeTTL
			}
			return greeksTTL
		}),
	}
}

func (c *greeksCache) get(key string, now time.Time) (greeksEntry, bool) {
	return c.inner.get(key, now)
}

func (c *greeksCache) put(key string, e greeksEntry, now time.Time) {
	c.inner.put(key, e, now)
}

// prevCloseCache memoises per-symbol previous-regular-session-close prices
// so positions / quote calls don't issue a fresh market-data subscribe
// for every held underlying on every invocation. The value (tick 9 in
// IBKR's protocol) is static across a full trading day — the cache TTL
// just has to be longer than typical session lengths and short enough
// that an overnight value can refresh on the next morning's first call.
//
// Negative caching is essential: an inactive symbol (delisted, halted)
// produces a zero-Close subscription that we still want to remember so
// the next 19 positions calls in the same session don't re-poll the same
// dead stream. The TTL applies symmetrically.
type prevCloseCache struct {
	inner *ttlMap[string, prevCloseEntry]
}

type prevCloseEntry struct {
	value float64 // 0 → negative cache (subscription returned no Close)
}

// prevCloseTTL is the maximum age of a cached prev-close before the next
// caller is forced to re-fetch. 12 hours covers the longest natural
// trading-session gap (Friday close ~21:00 UTC → Monday pre-market ~09:00
// UTC) while ensuring overnight values do refresh by morning. Daemons
// restarted between sessions repopulate naturally on first use.
const prevCloseTTL = 12 * time.Hour

func newPrevCloseCache() *prevCloseCache {
	return &prevCloseCache{
		inner: newTTLMap[string, prevCloseEntry](func(_ prevCloseEntry, _ time.Time) time.Duration {
			return prevCloseTTL
		}),
	}
}

func (c *prevCloseCache) get(symbol string, now time.Time) (prevCloseEntry, bool) {
	return c.inner.get(symbol, now)
}

func (c *prevCloseCache) put(symbol string, e prevCloseEntry, now time.Time) {
	c.inner.put(symbol, e, now)
}

// computePositionDayChange returns (chg, chg_pct) pointers describing how
// far the position's current mark sits from the previous regular-session
// close. Both stay nil unless we have a usable mark AND a positive
// cached prev close — no fabrication, no divide-by-zero.
func computePositionDayChange(mark, prevClose float64) (*float64, *float64) {
	if mark <= 0 || prevClose <= 0 {
		return nil, nil
	}
	chg := mark - prevClose
	pct := chg / prevClose * 100
	return &chg, &pct
}

type quoteLiquidityCache struct {
	inner *ttlMap[quoteLiquidityKey, quoteLiquidityEntry]
}

type quoteLiquidityKey struct {
	symbol   string
	market   string
	exchange string
	primary  string
	currency string
}

type quoteLiquidityEntry struct {
	avgVolume       int64
	avgDollarVolume float64
	status          string
	source          string
	sampleDays      int
	asOf            time.Time
}

func newQuoteLiquidityCache() *quoteLiquidityCache {
	return &quoteLiquidityCache{
		inner: newTTLMap[quoteLiquidityKey, quoteLiquidityEntry](func(e quoteLiquidityEntry, _ time.Time) time.Duration {
			if e.status == "ok" || e.status == "partial" {
				return 4 * time.Hour
			}
			return 5 * time.Minute
		}),
	}
}

func (c *quoteLiquidityCache) get(key quoteLiquidityKey, now time.Time) (quoteLiquidityEntry, bool) {
	if c == nil || c.inner == nil {
		return quoteLiquidityEntry{}, false
	}
	return c.inner.get(key, now)
}

func (c *quoteLiquidityCache) put(key quoteLiquidityKey, e quoteLiquidityEntry, now time.Time) {
	if c == nil || c.inner == nil {
		return
	}
	c.inner.put(key, e, now)
}

const (
	contractAuthorityScope = "market/contracts"
	contractStateKind      = "contract_cache.current.v3"
)

type coreContractCacheAuthority struct {
	store *corestore.Store
}

func (a coreContractCacheAuthority) LoadContractCache() ([]byte, bool, error) {
	return loadMarketState(a.store, contractAuthorityScope, contractStateKind)
}

// SaveContractCache publishes the cache as current state only. It deliberately
// appends no observation: the contract cache is a local copy of contract
// details IBKR re-serves on request, nothing but the next boot reads it, and no
// decision rests on it. Writing the whole cache into the immutable ledger once
// a minute put 5.1 GB of unread snapshots in daemon.db, and because every boot
// re-hashes the ledger before opening the socket, that cost was paid as startup
// latency. See internal-docs/design/authority-contract-cache-bloat.md.
func (a coreContractCacheAuthority) SaveContractCache(payload []byte, _ time.Time) error {
	return saveMarketDocument(context.Background(), a.store, contractAuthorityScope, contractStateKind, payload)
}
