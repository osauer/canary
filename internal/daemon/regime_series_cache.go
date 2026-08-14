package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	regimeSeriesCacheFreshFor       = 12 * time.Hour
	regimeSeriesCacheMaxFallbackAge = 14 * 24 * time.Hour
	regimeSeriesStateKind           = "regime_official_series.current.v1"
	regimeSeriesObservationKind     = "regime_official_series.snapshot.v1"
	// regimeSeriesMaxObservationLag is how far the newest observation in a
	// freshly fetched series may trail the fetch time before the write is
	// flagged. Every cached official series publishes each business day;
	// six calendar days absorbs a long weekend, a holiday, and a one-day
	// publication lag, and sits below the seven-day stale threshold so the
	// log attributes the shortfall to the upstream before the dashboard
	// lamp can go stale from it. A truncated-but-parsable response is
	// indistinguishable from real publication lag at this boundary; the
	// flag names the expectation so either cause surfaces the moment it is
	// written, not a session later.
	regimeSeriesMaxObservationLag = 6 * 24 * time.Hour
)

// regimeSeriesCache keeps official daily public-rate series from flapping on
// routine regime reads. OAS/funding rows are slow-changing daily data; a
// recently fetched local copy is preferable to marking the indicator missing
// because one HTTP request timed out.
type regimeSeriesCache struct {
	mu             sync.Mutex
	dir            string // sealed legacy cache; never used after UseCoreStore
	authority      *corestore.Store
	mem            map[string]regimeSeriesCacheEntry
	freshFor       time.Duration
	maxFallbackAge time.Duration
	warnf          func(format string, args ...any)
}

type regimeSeriesCacheEntry struct {
	SeriesID  string              `json:"series_id"`
	FetchedAt time.Time           `json:"fetched_at"`
	Points    []regimeSeriesPoint `json:"points"`
}

func newRegimeSeriesCache(dir string, warnf func(format string, args ...any)) *regimeSeriesCache {
	return &regimeSeriesCache{
		dir:            dir,
		mem:            map[string]regimeSeriesCacheEntry{},
		freshFor:       regimeSeriesCacheFreshFor,
		maxFallbackAge: regimeSeriesCacheMaxFallbackAge,
		warnf:          warnf,
	}
}

func (c *regimeSeriesCache) warn(format string, args ...any) {
	if c.warnf != nil {
		c.warnf(format, args...)
	}
}

func (c *regimeSeriesCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return fmt.Errorf("regime series cache: nil store")
	}
	if store == nil {
		return fmt.Errorf("regime series cache: nil corestore")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authority = store
	c.mem = map[string]regimeSeriesCacheEntry{}
	return nil
}

func regimeSeriesCacheDefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "regime-series"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "regime-series"), nil
}

func (c *regimeSeriesCache) fetch(ctx context.Context, seriesID string, fetcher func(context.Context, string) ([]regimeSeriesPoint, error)) ([]regimeSeriesPoint, error) {
	if c == nil {
		return fetcher(ctx, seriesID)
	}
	now := time.Now()
	if points, ok := c.cachedUsable(seriesID, now, true); ok {
		return points, nil
	}
	points, err := fetcher(ctx, seriesID)
	if err == nil {
		return c.put(seriesID, points, now), nil
	}
	if points, ok := c.cachedUsable(seriesID, now, false); ok {
		latest, _ := latestSeriesPoint(points)
		c.warn("official series %s: fetch failed (%v); serving cached series ending %s", seriesID, err, latest.Date.Format("2006-01-02"))
		return points, nil
	}
	c.warn("official series %s: fetch failed (%v); no usable cached fallback", seriesID, err)
	return nil, err
}

func (c *regimeSeriesCache) cachedUsable(seriesID string, now time.Time, requireFreshFetch bool) ([]regimeSeriesPoint, bool) {
	entry, ok := c.get(seriesID)
	if !ok || len(entry.Points) == 0 {
		return nil, false
	}
	if requireFreshFetch && now.Sub(entry.FetchedAt) > c.freshFor {
		return nil, false
	}
	if !requireFreshFetch && latestAge(entry.Points, now) > c.maxFallbackAge {
		return nil, false
	}
	return cloneRegimeSeries(entry.Points), true
}

func (c *regimeSeriesCache) get(seriesID string) (regimeSeriesCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.mem[seriesID]; ok {
		return cloneRegimeSeriesEntry(entry), true
	}
	entry, err := c.loadLocked(seriesID)
	if err != nil {
		return regimeSeriesCacheEntry{}, false
	}
	c.mem[seriesID] = entry
	return cloneRegimeSeriesEntry(entry), true
}

// put stores a freshly fetched series and returns the series the cache
// retained, which is what the caller must use for this read. A fetch whose
// newest observation is older than the stored entry's is refused: an upstream
// that silently serves truncated content must not replace a better copy and
// then be trusted for the full fresh window. The kept entry is re-stamped
// with the fetch time — upstream was just consulted and had nothing newer, so
// retrying before the fresh window lapses would only refetch the same
// regression.
func (c *regimeSeriesCache) put(seriesID string, points []regimeSeriesPoint, fetchedAt time.Time) []regimeSeriesPoint {
	if len(points) == 0 {
		return nil
	}
	candidate, _ := latestSeriesPoint(points)
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.mem[seriesID]
	if !ok {
		if loaded, err := c.loadLocked(seriesID); err == nil {
			existing, ok = loaded, true
		}
	}
	if ok {
		if prev, has := latestSeriesPoint(existing.Points); has && prev.Date.After(candidate.Date) {
			c.warn("official series %s: refusing fetched series ending %s; keeping cached series ending %s",
				seriesID, candidate.Date.Format("2006-01-02"), prev.Date.Format("2006-01-02"))
			existing.FetchedAt = fetchedAt
			c.mem[seriesID] = cloneRegimeSeriesEntry(existing)
			_ = c.saveLocked(existing)
			return cloneRegimeSeries(existing.Points)
		}
	}
	if lag := fetchedAt.Sub(candidate.Date); lag > regimeSeriesMaxObservationLag {
		c.warn("official series %s: fetched series ends %s, %.0f days behind a business-daily publication cadence; serving it, but the upstream is late or truncated",
			seriesID, candidate.Date.Format("2006-01-02"), lag.Hours()/24)
	}
	entry := regimeSeriesCacheEntry{
		SeriesID:  seriesID,
		FetchedAt: fetchedAt,
		Points:    cloneRegimeSeries(points),
	}
	c.mem[seriesID] = cloneRegimeSeriesEntry(entry)
	_ = c.saveLocked(entry)
	return cloneRegimeSeries(entry.Points)
}

func (c *regimeSeriesCache) loadLocked(seriesID string) (regimeSeriesCacheEntry, error) {
	var data []byte
	if c.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(c.authority, regimeSeriesAuthorityScope(seriesID), regimeSeriesStateKind)
		if err != nil {
			return regimeSeriesCacheEntry{}, fmt.Errorf("read official series authority for %s: %w", seriesID, err)
		}
		if !ok {
			return regimeSeriesCacheEntry{}, os.ErrNotExist
		}
	} else {
		var err error
		data, err = os.ReadFile(c.path(seriesID))
		if err != nil {
			return regimeSeriesCacheEntry{}, err
		}
	}
	var entry regimeSeriesCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return regimeSeriesCacheEntry{}, err
	}
	if entry.SeriesID != seriesID || len(entry.Points) == 0 {
		return regimeSeriesCacheEntry{}, fmt.Errorf("regime series cache: bad entry for %s", seriesID)
	}
	return entry, nil
}

func (c *regimeSeriesCache) saveLocked(entry regimeSeriesCacheEntry) error {
	if c.authority != nil {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		latest, _ := latestSeriesPoint(entry.Points)
		metadata, err := json.Marshal(struct {
			Version    int       `json:"version"`
			SeriesID   string    `json:"series_id"`
			FetchedAt  time.Time `json:"fetched_at"`
			LatestDate time.Time `json:"latest_date"`
			PointCount int       `json:"point_count"`
			Method     string    `json:"method"`
		}{
			Version: 1, SeriesID: entry.SeriesID, FetchedAt: entry.FetchedAt,
			LatestDate: latest.Date, PointCount: len(entry.Points), Method: "official published daily series",
		})
		if err != nil {
			return err
		}
		scope := regimeSeriesAuthorityScope(entry.SeriesID)
		return saveMarketState(c.authority, scope, regimeSeriesStateKind, corestore.ObservationInput{
			ScopeKey: scope, Source: regimeSeriesObservationSource(entry.SeriesID),
			Kind: regimeSeriesObservationKind, ObservedAt: entry.FetchedAt,
			ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := c.path(entry.SeriesID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func regimeSeriesAuthorityScope(seriesID string) string {
	return "market/regime/official-series/" + strings.TrimSpace(seriesID)
}

func regimeSeriesObservationSource(seriesID string) string {
	switch seriesID {
	case fredSeriesCP3M:
		return "federal_reserve"
	case fredSeriesTBill3M:
		return "us_treasury"
	default:
		return "fred"
	}
}

func (c *regimeSeriesCache) path(seriesID string) string {
	return filepath.Join(c.dir, sanitizeRegimeSeriesID(seriesID)+".json")
}

func sanitizeRegimeSeriesID(seriesID string) string {
	var b strings.Builder
	for _, r := range seriesID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "series"
	}
	return b.String()
}

func latestAge(points []regimeSeriesPoint, now time.Time) time.Duration {
	latest, ok := latestSeriesPoint(points)
	if !ok || latest.Date.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return now.Sub(latest.Date)
}

func cloneRegimeSeriesEntry(entry regimeSeriesCacheEntry) regimeSeriesCacheEntry {
	entry.Points = cloneRegimeSeries(entry.Points)
	return entry
}

func cloneRegimeSeries(points []regimeSeriesPoint) []regimeSeriesPoint {
	if len(points) == 0 {
		return nil
	}
	return slices.Clone(points)
}
