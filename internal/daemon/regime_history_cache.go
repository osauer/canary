package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	regimeHistoryCacheFreshFor       = 12 * time.Hour
	regimeHistoryCacheMaxFallbackAge = 14 * 24 * time.Hour
	regimeHistoryStateKind           = "regime_hmds.current.v1"
	regimeHistoryObservationKind     = "regime_hmds.snapshot.v1"
	regimeHistorySource              = "ibkr.tws.hmds"
)

// regimeHistoryCache keeps daily HMDS baselines from flapping on routine
// regime reads. Live HMDS remains the source of truth when available; a recent
// cached copy is used when the gateway is slow, thin, or temporarily refuses
// history.
type regimeHistoryCache struct {
	mu             sync.Mutex
	dir            string // sealed legacy cache; never used after UseCoreStore
	authority      *corestore.Store
	mem            map[string]regimeHistoryCacheEntry
	freshFor       time.Duration
	maxFallbackAge time.Duration
}

type regimeHistoryCacheEntry struct {
	Key       string                  `json:"key"`
	Symbol    string                  `json:"symbol"`
	Days      int                     `json:"days"`
	FetchedAt time.Time               `json:"fetched_at"`
	Bars      []ibkrlib.HistoricalBar `json:"bars"`
}

func newRegimeHistoryCache(dir string) *regimeHistoryCache {
	return &regimeHistoryCache{
		dir:            dir,
		mem:            map[string]regimeHistoryCacheEntry{},
		freshFor:       regimeHistoryCacheFreshFor,
		maxFallbackAge: regimeHistoryCacheMaxFallbackAge,
	}
}

func (c *regimeHistoryCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return fmt.Errorf("regime history cache: nil store")
	}
	if store == nil {
		return fmt.Errorf("regime history cache: nil corestore")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authority = store
	c.mem = map[string]regimeHistoryCacheEntry{}
	return nil
}

func regimeHistoryCacheDefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "regime-history"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "regime-history"), nil
}

func (c *regimeHistoryCache) fetch(ctx context.Context, sym string, days int, fetcher func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error)) ([]ibkrlib.HistoricalBar, error) {
	if c == nil {
		return fetcher(ctx, sym, days)
	}
	now := time.Now()
	if bars, ok := c.cachedUsable(sym, days, now, true); ok {
		return bars, nil
	}
	bars, err := fetcher(ctx, sym, days)
	if err == nil && len(bars) >= regimeHistoryMinBars(days) {
		c.put(sym, days, bars, now)
		return bars, nil
	}
	if bars, ok := c.cachedUsable(sym, days, now, false); ok {
		return bars, nil
	}
	return bars, err
}

func (c *regimeHistoryCache) cachedUsable(sym string, days int, now time.Time, requireFreshFetch bool) ([]ibkrlib.HistoricalBar, bool) {
	entry, ok := c.get(sym, days)
	if !ok || len(entry.Bars) == 0 {
		return nil, false
	}
	if requireFreshFetch && now.Sub(entry.FetchedAt) > c.freshFor {
		return nil, false
	}
	if !requireFreshFetch && latestHistoricalBarAge(entry.Bars, now) > c.maxFallbackAge {
		return nil, false
	}
	return cloneHistoricalBars(entry.Bars), true
}

func (c *regimeHistoryCache) get(sym string, days int) (regimeHistoryCacheEntry, bool) {
	key := regimeHistoryCacheKey(sym, days)
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.mem[key]; ok {
		return cloneRegimeHistoryEntry(entry), true
	}
	entry, err := c.loadLocked(sym, days)
	if err != nil {
		return regimeHistoryCacheEntry{}, false
	}
	c.mem[key] = entry
	return cloneRegimeHistoryEntry(entry), true
}

func (c *regimeHistoryCache) put(sym string, days int, bars []ibkrlib.HistoricalBar, fetchedAt time.Time) {
	if len(bars) == 0 {
		return
	}
	entry := regimeHistoryCacheEntry{
		Key:       regimeHistoryCacheKey(sym, days),
		Symbol:    sym,
		Days:      days,
		FetchedAt: fetchedAt,
		Bars:      cloneHistoricalBars(bars),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem[entry.Key] = cloneRegimeHistoryEntry(entry)
	_ = c.saveLocked(entry)
}

func (c *regimeHistoryCache) loadLocked(sym string, days int) (regimeHistoryCacheEntry, error) {
	key := regimeHistoryCacheKey(sym, days)
	var data []byte
	if c.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(c.authority, regimeHistoryAuthorityScope(sym, days), regimeHistoryStateKind)
		if err != nil {
			return regimeHistoryCacheEntry{}, fmt.Errorf("read regime HMDS authority for %s: %w", key, err)
		}
		if !ok {
			return regimeHistoryCacheEntry{}, os.ErrNotExist
		}
	} else {
		var err error
		data, err = os.ReadFile(c.path(sym, days))
		if err != nil {
			return regimeHistoryCacheEntry{}, err
		}
	}
	var entry regimeHistoryCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return regimeHistoryCacheEntry{}, err
	}
	if entry.Key != key || entry.Symbol != sym || entry.Days != days || len(entry.Bars) == 0 {
		return regimeHistoryCacheEntry{}, fmt.Errorf("regime history cache: bad entry for %s", key)
	}
	return entry, nil
}

func (c *regimeHistoryCache) saveLocked(entry regimeHistoryCacheEntry) error {
	if c.authority != nil {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		metadata, err := json.Marshal(struct {
			Version   int       `json:"version"`
			Symbol    string    `json:"symbol"`
			Days      int       `json:"days"`
			FetchedAt time.Time `json:"fetched_at"`
			BarCount  int       `json:"bar_count"`
			Method    string    `json:"method"`
		}{
			Version: 1, Symbol: entry.Symbol, Days: entry.Days,
			FetchedAt: entry.FetchedAt, BarCount: len(entry.Bars), Method: "IBKR HMDS daily bars",
		})
		if err != nil {
			return err
		}
		scope := regimeHistoryAuthorityScope(entry.Symbol, entry.Days)
		return saveMarketState(c.authority, scope, regimeHistoryStateKind, corestore.ObservationInput{
			ScopeKey: scope, Source: regimeHistorySource, Kind: regimeHistoryObservationKind,
			ObservedAt: entry.FetchedAt, ContentType: "application/json",
			Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := c.path(entry.Symbol, entry.Days)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func regimeHistoryAuthorityScope(sym string, days int) string {
	return "market/regime/hmds/" + regimeHistoryCacheKey(sym, days)
}

func (c *regimeHistoryCache) path(sym string, days int) string {
	return filepath.Join(c.dir, sanitizeRegimeSeriesID(regimeHistoryCacheKey(sym, days))+".json")
}

func regimeHistoryCacheKey(sym string, days int) string {
	return fmt.Sprintf("%s-%dd", normSym(sym), days)
}

func regimeHistoryMinBars(days int) int {
	switch {
	case days >= 365:
		return 50
	case days >= HYGLookbackDays:
		return 50
	case days >= USDJPYLookbackDays:
		return 8
	default:
		return 1
	}
}

func latestHistoricalBarAge(bars []ibkrlib.HistoricalBar, now time.Time) time.Duration {
	for _, bar := range slices.Backward(bars) {
		at := historyBarAsOf(bar, now)
		if !at.IsZero() {
			return now.Sub(at)
		}
	}
	return time.Duration(1<<63 - 1)
}

func cloneRegimeHistoryEntry(entry regimeHistoryCacheEntry) regimeHistoryCacheEntry {
	entry.Bars = cloneHistoricalBars(entry.Bars)
	return entry
}

func cloneHistoricalBars(bars []ibkrlib.HistoricalBar) []ibkrlib.HistoricalBar {
	if len(bars) == 0 {
		return nil
	}
	return slices.Clone(bars)
}
