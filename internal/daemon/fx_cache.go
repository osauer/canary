package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// fxCacheFreshWindow: a cached rate younger than this is served without
// spending a gateway snapshot quote. The app host polls positions every
// ~5s; while the streaming $LEDGER rate stays bogus (observed: persistent
// unit USD rate on a EUR-base live account) every poll would otherwise pay
// up to 2×1.2s of FX quote budget per suspicious currency.
const fxCacheFreshWindow = 15 * time.Minute

// fxCacheTTL bounds how stale a last-known-good rate may be served when
// live resolution fails. 72h spans a weekend: FX snapshot quotes can be
// unavailable off-hours, and a 24h bound would resurrect the every-poll
// base-field flap each Saturday. G10 FX drift over a closed weekend is
// immaterial next to the alternative — dropping every *_base field and
// flipping the positions fingerprint (which churns proposal revisions
// mid-confirm) whenever one quote times out.
const fxCacheTTL = 72 * time.Hour

// fxPersistMinInterval throttles best-effort durable flushes of the cache.
// harvest calls put once per currency on every ~5s app-host poll, and
// rewriting the projection that often buys nothing: a persisted `at` up to a
// minute stale merely shifts the fresh/TTL windows by that much after a
// restart.
const fxPersistMinInterval = time.Minute

const (
	fxAuthorityScope    = "market/fx/lkg"
	fxStateKind         = "fx_rates.current.v1"
	fxObservationKind   = "fx_rates.snapshot.v1"
	fxObservationSource = "ibkr.tws.fx_rate"
)

type fxCachedRate struct {
	rate float64
	at   time.Time
}

// fxRateCache keeps last-known-good BASE-per-CCY exchange rates across
// requests. Rates only — ledger rows carry account-scoped balances
// (CashBalance, NetLiquidationByCurrency) and must never be cached here.
type fxRateCache struct {
	mu    sync.Mutex
	now   func() time.Time
	rates map[string]fxCachedRate
	// degraded holds pairs currently served from cache after a failed
	// live resolution, so the transition WARN/INFO logs once per episode
	// instead of every poll (same dedupe pattern as lastGatewayUnreachable).
	degraded map[string]bool
	// store persists last-known-good rates across daemon restarts; nil
	// (newFXRateCache) keeps the cache memory-only.
	store *fxRateStore
	// lastPersist throttles store flushes to fxPersistMinInterval.
	lastPersist time.Time
	// persistFailing dedupes save-error WARNs to once per episode
	// (markDegraded's transition pattern).
	persistFailing bool
	logger         *Logger
}

func newFXRateCache() *fxRateCache {
	return &fxRateCache{now: time.Now, rates: map[string]fxCachedRate{}, degraded: map[string]bool{}}
}

// newFXRateCacheWithStore returns a cache whose last-known-good rates
// survive daemon restarts. Persisted rates still within fxCacheTTL of
// now seed the cache at construction, so a restart during IBKR's
// nightly reset window (or a weekend, when FX snapshot quotes can be
// unavailable for days) serves *_base fields immediately instead of
// nil until the first successful live resolution. Best-effort
// throughout: a load failure logs and starts cold.
func newFXRateCacheWithStore(store *fxRateStore, now func() time.Time, logger *Logger) *fxRateCache {
	c := newFXRateCacheWithStoreCold(store, now, logger)
	loaded, err := store.load(now())
	if err != nil && logger != nil {
		logger.Warnf("fx rate cache: load persisted rates: %v (starting cold)", err)
	}
	maps.Copy(c.rates, loaded)
	if len(loaded) > 0 && logger != nil {
		logger.Infof("fx rate cache: restored %d persisted rate(s)", len(loaded))
	}
	return c
}

// newFXRateCacheWithStoreCold installs the legacy codec path without reading
// it. Production construction uses this before the persistence lock; the
// unpublished cutover importer is the only production legacy reader.
func newFXRateCacheWithStoreCold(store *fxRateStore, now func() time.Time, logger *Logger) *fxRateCache {
	c := newFXRateCache()
	if now != nil {
		c.now = now
	}
	c.store = store
	c.logger = logger
	return c
}

// UseCoreStore replaces any eagerly loaded legacy projection with daemon.db.
// Missing SQLite state is an intentional cold start. Once this succeeds the
// attached fxRateStore cannot fall back to or mirror fx-rates.json.
func (f *fxRateCache) UseCoreStore(store *corestore.Store) error {
	if f == nil {
		return errors.New("fx rate cache: nil cache")
	}
	if store == nil {
		return errors.New("fx rate cache: nil corestore")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.store == nil {
		f.store = newFXRateStore("")
	}
	loaded, err := f.store.useCoreStore(store, f.now())
	if err != nil {
		return err
	}
	f.rates = loaded
	f.degraded = map[string]bool{}
	f.lastPersist = time.Time{}
	f.persistFailing = false
	return nil
}

func fxPairKey(baseCcy, ccy string) string {
	return normCcy(baseCcy) + "/" + normCcy(ccy)
}

func (f *fxRateCache) get(baseCcy, ccy string, maxAge time.Duration) (rate float64, age time.Duration, ok bool) {
	if f == nil {
		return 0, 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, found := f.rates[fxPairKey(baseCcy, ccy)]
	if !found || entry.rate <= 0 {
		return 0, 0, false
	}
	age = f.now().Sub(entry.at)
	if age > maxAge {
		return 0, 0, false
	}
	return entry.rate, age, true
}

func (f *fxRateCache) put(baseCcy, ccy string, rate float64) {
	if f == nil || rate <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates[fxPairKey(baseCcy, ccy)] = fxCachedRate{rate: rate, at: f.now()}
	f.persistLocked()
}

// persistLocked flushes the rate map to the store, best-effort and
// throttled to fxPersistMinInterval. The write stays under f.mu — a
// <1 KiB atomic file replace at most once a minute is cheaper than a
// snapshot-outside-the-lock dance. Save errors WARN once per episode,
// not once per flush.
func (f *fxRateCache) persistLocked() {
	if f.store == nil {
		return
	}
	now := f.now()
	if now.Sub(f.lastPersist) < fxPersistMinInterval {
		return
	}
	f.lastPersist = now
	err := f.store.save(f.rates)
	switch {
	case err != nil && !f.persistFailing:
		f.persistFailing = true
		if f.logger != nil {
			f.logger.Warnf("fx rate cache: persist: %v (rates survive in memory only)", err)
		}
	case err == nil && f.persistFailing:
		f.persistFailing = false
		if f.logger != nil {
			f.logger.Infof("fx rate cache: persist recovered")
		}
	}
}

// harvest stores valid streaming-ledger rates so the cache is warm before
// the first repair failure. The ==1.0 exclusion mirrors the repair's
// suspicion filter: live gateways stream fake unit rates for non-base
// currencies, and caching one would defeat the repair.
func (f *fxRateCache) harvest(ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) {
	if f == nil {
		return
	}
	base := normCcy(baseCcy)
	if base == "" {
		return
	}
	for ccy, row := range ledger {
		ccy = normCcy(ccy)
		if ccy == "" || ccy == base || row.ExchangeRate <= 0 || row.ExchangeRate == 1.0 {
			continue
		}
		f.put(base, ccy, row.ExchangeRate)
	}
}

// markDegraded flips the per-pair serve-mode and reports whether it
// changed, so callers log transitions once instead of once per poll.
func (f *fxRateCache) markDegraded(baseCcy, ccy string, degraded bool) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fxPairKey(baseCcy, ccy)
	if f.degraded[key] == degraded {
		return false
	}
	if degraded {
		f.degraded[key] = true
	} else {
		delete(f.degraded, key)
	}
	return true
}

// repairCurrencyLedgerFXRatesCached is the Server-scoped variant of
// repairCurrencyLedgerFXRates: same suspicious-rate policy, but resolution
// consults the last-known-good cache. Before this cache, one transient FX
// snapshot failure zeroed the rate and stripped every *_base field from
// that single response — flipping SPA money formats between consecutive
// SSE snapshots and churning the positions fingerprint. A cached rate
// fresher than fxCacheFreshWindow short-circuits the quote entirely.
func (s *Server) repairCurrencyLedgerFXRatesCached(ctx context.Context, c *ibkrlib.Connector, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) map[string]ibkrlib.CurrencyLedger {
	if s == nil || s.fxRates == nil {
		return repairCurrencyLedgerFXRates(ctx, c, ledger, baseCcy)
	}
	s.fxRates.harvest(ledger, baseCcy)
	return repairCurrencyLedgerFXRatesWithResolver(ctx, ledger, baseCcy, fxRepairQuoteBudget, s.cachedFXResolver(c))
}

func (s *Server) cachedFXResolver(c *ibkrlib.Connector) currencyRateResolver {
	return func(ctx context.Context, baseCcy, ccy string, timeout time.Duration) (float64, bool) {
		if rate, _, ok := s.fxRates.get(baseCcy, ccy, fxCacheFreshWindow); ok {
			return rate, true
		}
		if rate, ok := resolveBasePerCurrencyFXRate(ctx, c, baseCcy, ccy, timeout); ok && rate > 0 {
			s.fxRates.put(baseCcy, ccy, rate)
			if s.fxRates.markDegraded(baseCcy, ccy, false) && s.logger != nil {
				s.logger.Infof("fx rate %s: live resolution recovered", fxPairKey(baseCcy, ccy))
			}
			return rate, true
		}
		rate, age, ok := s.fxRates.get(baseCcy, ccy, fxCacheTTL)
		if !ok {
			return 0, false
		}
		if s.fxRates.markDegraded(baseCcy, ccy, true) && s.logger != nil {
			s.logger.Warnf("fx rate %s: live resolution failed; serving last-known-good %.6f (age %s)", fxPairKey(baseCcy, ccy), rate, age.Round(time.Second))
		}
		return rate, true
	}
}

// fxRatePersistVersion is the schema version of the on-disk envelope.
// Bump on any incompatible shape change; a mismatch loads cold.
const fxRatePersistVersion = 1

// fxRateStoreFilename lives directly in the shared $XDG_CACHE_HOME/ibkr/
// root — the convention for single-file daemon caches (regime-streaks.json).
const fxRateStoreFilename = "fx-rates.json"

type fxPersistedRate struct {
	Rate float64   `json:"rate"`
	At   time.Time `json:"at"`
}

type fxRatePersistEnvelope struct {
	Version int                        `json:"version"`
	Rates   map[string]fxPersistedRate `json:"rates"`
}

// fxRateStore persists pair→{rate, at} across daemon restarts, so a
// restart during IBKR's nightly server-reset window (or a weekend)
// doesn't start cold and serve nil *_base fields until the first
// successful live resolution. Rates only — ledger rows carry
// account-scoped balances and must never be persisted here. Production uses
// atomic daemon.db state + observation writes; the JSON branch is retained
// only as a legacy cutover codec and for isolated tests.
type fxRateStore struct {
	dir       string // sealed legacy cache; unused after useCoreStore
	authority *corestore.Store
}

// newFXRateStore returns a store rooted at dir. The directory is
// created lazily on first save so tests passing an unwritable dir
// don't fail at construction.
func newFXRateStore(dir string) *fxRateStore {
	return &fxRateStore{dir: dir}
}

func (s *fxRateStore) useCoreStore(store *corestore.Store, now time.Time) (map[string]fxCachedRate, error) {
	if s == nil {
		return nil, errors.New("fx rate store: nil store")
	}
	raw, ok, err := loadMarketState(store, fxAuthorityScope, fxStateKind)
	if err != nil {
		return nil, fmt.Errorf("read FX authority: %w", err)
	}
	loaded := map[string]fxCachedRate{}
	if ok {
		loaded, err = decodeFXRateEnvelope(raw, now, true)
		if err != nil {
			return nil, fmt.Errorf("decode FX authority: %w", err)
		}
	}
	s.authority = store
	return loaded, nil
}

// load returns the persisted rates still within fxCacheTTL of now.
// Missing file and version mismatch are cold starts (nil, nil); an
// error surfaces only for I/O problems or JSON corruption. Entries
// past the TTL or with non-positive rates are dropped on load — get
// would refuse to serve them anyway, and re-seeding them would only
// keep dead pairs in the file.
func (s *fxRateStore) load(now time.Time) (map[string]fxCachedRate, error) {
	var data []byte
	if s.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(s.authority, fxAuthorityScope, fxStateKind)
		if err != nil || !ok {
			return map[string]fxCachedRate{}, err
		}
		return decodeFXRateEnvelope(data, now, true)
	}
	var err error
	data, err = os.ReadFile(filepath.Join(s.dir, fxRateStoreFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read fx rate cache: %w", err)
	}
	return decodeFXRateEnvelope(data, now, false)
}

func decodeFXRateEnvelope(data []byte, now time.Time, strict bool) (map[string]fxCachedRate, error) {
	var env fxRatePersistEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode fx rate cache: %w", err)
	}
	if env.Version != fxRatePersistVersion {
		if strict {
			return nil, fmt.Errorf("invalid FX rate version %d", env.Version)
		}
		return nil, nil
	}
	if strict && env.Rates == nil {
		return nil, errors.New("FX rate authority has no rates map")
	}
	rates := make(map[string]fxCachedRate, len(env.Rates))
	for pair, entry := range env.Rates {
		parts := strings.Split(pair, "/")
		validPair := len(parts) == 2 && fxPairKey(parts[0], parts[1]) == pair
		if !validPair || entry.Rate <= 0 || entry.At.IsZero() || now.Before(entry.At) {
			if strict {
				return nil, fmt.Errorf("invalid FX rate row %q", pair)
			}
			continue
		}
		if now.Sub(entry.At) > fxCacheTTL {
			continue
		}
		rates[pair] = fxCachedRate{rate: entry.Rate, at: entry.At}
	}
	return rates, nil
}

// save atomically replaces the on-disk envelope with the given rates.
// Pretty-printed so a human debugging the cache can `cat` and read it.
func (s *fxRateStore) save(rates map[string]fxCachedRate) error {
	env := fxRatePersistEnvelope{
		Version: fxRatePersistVersion,
		Rates:   make(map[string]fxPersistedRate, len(rates)),
	}
	for pair, entry := range rates {
		env.Rates[pair] = fxPersistedRate{Rate: entry.rate, At: entry.at}
	}
	if s.authority != nil {
		for pair, entry := range rates {
			parts := strings.Split(pair, "/")
			if len(parts) != 2 || fxPairKey(parts[0], parts[1]) != pair || entry.rate <= 0 || entry.at.IsZero() {
				return fmt.Errorf("invalid FX rate row %q", pair)
			}
		}
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode FX authority: %w", err)
		}
		observedAt := latestFXRateTime(rates)
		if observedAt.IsZero() {
			observedAt = time.Now().UTC()
		}
		metadata, err := json.Marshal(struct {
			Version   int    `json:"version"`
			PairCount int    `json:"pair_count"`
			Method    string `json:"method"`
		}{fxRatePersistVersion, len(rates), "IBKR ledger or FX snapshot quote"})
		if err != nil {
			return fmt.Errorf("encode FX metadata: %w", err)
		}
		return saveMarketState(s.authority, fxAuthorityScope, fxStateKind, corestore.ObservationInput{
			ScopeKey: fxAuthorityScope, Source: fxObservationSource, Kind: fxObservationKind,
			ObservedAt: observedAt, ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, fxRateStoreFilename)
	tmp, err := os.CreateTemp(s.dir, fxRateStoreFilename+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any error past this point, remove the orphaned temp file so
	// the cache dir doesn't accumulate junk.
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode %s: %w", fxRateStoreFilename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer to skip the second Close
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", fxRateStoreFilename, err)
	}
	return nil
}

func latestFXRateTime(rates map[string]fxCachedRate) time.Time {
	var latest time.Time
	for _, entry := range rates {
		if entry.at.After(latest) {
			latest = entry.at
		}
	}
	return latest
}

// fxRateStoreDefaultDir resolves the shared daemon cache root
// ($XDG_CACHE_HOME/ibkr/, falling back to $HOME/.cache/ibkr/), matching
// the streak and contract stores so all daemon caches live together.
// Errors only when neither XDG_CACHE_HOME nor HOME is set, which on a
// real OS user account doesn't happen; tests construct newFXRateStore
// directly with t.TempDir().
func fxRateStoreDefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr"), nil
}
