package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// StreakEntry is one indicator's persisted band history. LastBand is
// the band classification observed on the most recent successful tick;
// LastSession is the NY-tz session key (YYYY-MM-DD) the tick happened
// in. Sessions counts how many NY sessions in a row the indicator has
// reported LastBand. LastValue is the raw measurement at LastSession
// — kept for diagnostics so a human inspecting the file can verify the
// classification.
type StreakEntry struct {
	LastBand    string  `json:"last_band"`
	SinceDate   string  `json:"since_date"`
	LastSession string  `json:"last_session"`
	Sessions    int     `json:"sessions"`
	LastValue   float64 `json:"last_value"`
	// EligibleLatched records that this red streak earned confirmation
	EligibleLatched bool `json:"eligible_latched,omitempty"`
	// LastBandAt is when LastBand was last measured from live inputs. It
	LastBandAt time.Time `json:"last_band_at"`
}

// streakStoreFile is the on-disk shape. Version field for future
// migrations; Notes documents the daemon-side band classification
// (a slight violation of the daemon's "no derived bands on the wire"
type streakStoreFile struct {
	Version             int                    `json:"version"`
	AsOf                time.Time              `json:"as_of"`
	SnapshotRevision    int64                  `json:"snapshot_revision,omitempty"`
	SnapshotPublishedAt time.Time              `json:"snapshot_published_at"`
	SnapshotFingerprint rpc.Fingerprint        `json:"snapshot_fingerprint"`
	Notes               string                 `json:"notes"`
	Entries             map[string]StreakEntry `json:"entries"`
}

const (
	streakStoreVersion    = 1
	streakStoreFileN      = "regime-streaks.json"
	streakStateKind       = "regime_streaks.current.v1"
	streakObservationKind = "regime_streaks.snapshot.v1"
	streakAuthorityScope  = "market/regime/streaks"
	streakSource          = "daemon.regime_classifier"
	streakStoreNotes      = "Per-indicator consecutive-sessions-in-band tally. The daemon classifies bands using the spec's default thresholds (see docs/docs/internals/regime-dashboard.md) — slightly violating the wire-shape posture that derived bands belong in the renderer, accepted because streak persistence requires a stable daemon-side classification. Breadth bands are simplified to value-only (<40=red, 40-55=yellow, >55=green) for streak purposes; the renderer can still apply the spec's 'SPX near highs' modifier for display colour."
)

// StreakStore persists the streak counters across daemon restarts.
// per-indicator entries keyed on a stable token.
// they persist across days and only change band on a band transition
type StreakStore struct {
	dir       string // sealed legacy cache; never used after UseCoreStore
	authority *corestore.Store
	// volatile marks a detached evaluation clone. It applies the exact Tick
	volatile bool

	mu      sync.Mutex
	entries map[string]StreakEntry
	loaded  bool
	asOf    time.Time
	// publication is the exact authoritative snapshot tuple carried by the
	// current SQLite projection. Revision zero is confined to legacy file and
	// unit-helper writes which predate the snapshot publication barrier.
	publication regimeSnapshotPublication
	stateExists bool
	loadErr     error
}

// NewStreakStore returns a store rooted at dir. Construction is lazy
func NewStreakStore(dir string) *StreakStore {
	return &StreakStore{
		dir:     dir,
		entries: map[string]StreakEntry{},
	}
}

// UseCoreStore makes daemon.db the sole runtime persistence authority and
// discards any legacy state that may have been loaded before attachment.
func (s *StreakStore) UseCoreStore(store *corestore.Store) error {
	if s == nil {
		return fmt.Errorf("regime streaks: nil store")
	}
	if store == nil {
		return fmt.Errorf("regime streaks: nil corestore")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authority = store
	s.entries = map[string]StreakEntry{}
	s.loaded = false
	s.asOf = time.Time{}
	s.publication = regimeSnapshotPublication{}
	s.stateExists = false
	s.loadErr = nil
	return nil
}

// load reads the on-disk file into s.entries. Idempotent — subsequent
// calls after the first are no-ops. Caller MUST hold s.mu.
// future format bump triggers a graceful rebuild rather than corrupting
func (s *StreakStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true // mark loaded even on parse failure, so we don't retry every call
	var data []byte
	if s.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(s.authority, streakAuthorityScope, streakStateKind)
		if err != nil {
			s.loadErr = fmt.Errorf("load regime streak state: %w", err)
			return
		}
		if !ok {
			return
		}
		s.stateExists = true
	} else {
		path := filepath.Join(s.dir, streakStoreFileN)
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				// Legacy import/test mode deliberately degrades corrupt files
				// to an empty state; runtime authority errors never fall back.
				_ = err
			}
			return
		}
	}
	var f streakStoreFile
	if err := json.Unmarshal(data, &f); err != nil {
		if s.authority != nil {
			s.loadErr = fmt.Errorf("decode regime streak state: %w", err)
		}
		return
	}
	if f.Version != streakStoreVersion {
		if s.authority != nil {
			s.loadErr = fmt.Errorf("decode regime streak state: unsupported version %d", f.Version)
		}
		return
	}
	s.asOf = f.AsOf.UTC()
	publication := regimeSnapshotPublication{
		Revision: f.SnapshotRevision, PublishedAt: f.SnapshotPublishedAt.UTC(), Fingerprint: f.SnapshotFingerprint,
	}
	if hasAnyRegimeSnapshotPublicationIdentity(publication) {
		if err := validateRegimeSnapshotPublication(publication); err != nil {
			s.loadErr = fmt.Errorf("decode regime streak publication: %w", err)
			return
		}
		if !s.asOf.Equal(publication.PublishedAt) {
			s.loadErr = fmt.Errorf("decode regime streak publication: as_of %s does not match snapshot_published_at %s", s.asOf, publication.PublishedAt)
			return
		}
		s.publication = publication
	}
	if f.Entries != nil {
		s.entries = f.Entries
	}
}

// saveLocked writes the entries map atomically. Caller MUST hold s.mu.
// Legacy helpers may choose to ignore its returned error. Runtime Regime
// publication treats an exact-tuple write failure as a projection-barrier
// failure and withholds the snapshot until recovery succeeds.
func (s *StreakStore) saveLocked() error {
	return s.saveLockedContext(context.Background())
}

func (s *StreakStore) saveLockedContext(ctx context.Context) error {
	return s.saveLockedContextAt(ctx, time.Now().UTC())
}

func (s *StreakStore) saveLockedContextAt(ctx context.Context, now time.Time) error {
	return s.saveLockedContextPublication(ctx, regimeSnapshotPublication{PublishedAt: now})
}

// saveLockedContextPublication persists both streak content and, for runtime
// projections, the exact authoritative snapshot tuple. Revision-zero callers
// retain the legacy helper contract and carry only as_of.
func (s *StreakStore) saveLockedContextPublication(ctx context.Context, publication regimeSnapshotPublication) error {
	if s.volatile {
		return nil
	}
	now := publication.PublishedAt.UTC()
	if now.IsZero() {
		return errors.New("regime streak projection timestamp is required")
	}
	if publication.Revision > 0 {
		publication.PublishedAt = now
		if err := validateRegimeSnapshotPublication(publication); err != nil {
			return err
		}
	} else {
		publication = regimeSnapshotPublication{}
	}
	file := streakStoreFile{
		Version:             streakStoreVersion,
		AsOf:                now,
		SnapshotRevision:    publication.Revision,
		SnapshotPublishedAt: publication.PublishedAt,
		SnapshotFingerprint: publication.Fingerprint,
		Notes:               streakStoreNotes,
		Entries:             s.entries,
	}
	if s.authority != nil {
		payload, err := json.Marshal(file)
		if err != nil {
			return fmt.Errorf("encode streak state: %w", err)
		}
		metadata, err := json.Marshal(struct {
			Version             int             `json:"version"`
			AsOf                time.Time       `json:"as_of"`
			SnapshotRevision    int64           `json:"snapshot_revision,omitempty"`
			SnapshotPublishedAt time.Time       `json:"snapshot_published_at"`
			SnapshotFingerprint rpc.Fingerprint `json:"snapshot_fingerprint"`
			EntryCount          int             `json:"entry_count"`
			Method              string          `json:"method"`
		}{
			Version: streakStoreVersion, AsOf: now,
			SnapshotRevision: publication.Revision, SnapshotPublishedAt: publication.PublishedAt,
			SnapshotFingerprint: publication.Fingerprint, EntryCount: len(s.entries),
			Method: "versioned daemon regime-band streak classifier",
		})
		if err != nil {
			return fmt.Errorf("encode streak metadata: %w", err)
		}
		if err := saveMarketStateContext(ctx, s.authority, streakAuthorityScope, streakStateKind, corestore.ObservationInput{
			ScopeKey: streakAuthorityScope, Source: streakSource, Kind: streakObservationKind,
			ObservedAt: now, ContentType: "application/json",
			Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		}); err != nil {
			return err
		}
		s.asOf = now
		s.publication = publication
		s.stateExists = true
		s.loadErr = nil
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, streakStoreFileN)
	tmp, err := os.CreateTemp(s.dir, streakStoreFileN+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file); err != nil {
		return fmt.Errorf("encode streaks: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename streaks: %w", err)
	}
	s.asOf = now
	s.publication = publication
	s.stateExists = true
	s.loadErr = nil
	return nil
}

// Tick advances the streak counter for indicatorKey using the supplied
// updated state, and persists the file. Empty band freezes the counter
//   - Empty band (indicator computing / unavailable / error): freeze
func (s *StreakStore) Tick(indicatorKey string, value float64, band string, nowNY time.Time) *StreakInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	// Sessions are NY TRADING days. A weekend or holiday poll keys to the
	// most recent trading day, so it can never inflate the counter — the
	today := nyTradingSessionKey(nowNY)

	// Empty band = freeze: return whatever's already there without
	// mutating. nil if we've never seen a band for this indicator.
	if band == "" {
		entry, ok := s.entries[indicatorKey]
		if !ok {
			return nil
		}
		return entryToInfo(entry)
	}

	entry, exists := s.entries[indicatorKey]
	switch {
	case !exists:
		// First-ever observation for this indicator. Start at 1.
		entry = StreakEntry{
			LastBand:    band,
			SinceDate:   today,
			LastSession: today,
			Sessions:    1,
			LastValue:   value,
		}
	case entry.LastBand == band && entry.LastSession == today:
		// Same band, same session — no-op. Multiple calls within one
		entry.LastValue = value
	case entry.LastBand == band:
		// Same band, new session — increment. EligibleLatched survives:
		entry.LastSession = today
		entry.Sessions++
		entry.LastValue = value
	default:
		// Band change — reset to day 1 of the new band and drop the
		// eligibility latch (it is a property of the ended red streak).
		entry = StreakEntry{
			LastBand:    band,
			SinceDate:   today,
			LastSession: today,
			Sessions:    1,
			LastValue:   value,
		}
	}
	// Reached only for a non-empty band (the freeze path returned above), so
	// this dates the last LIVE measurement, never a frozen carry.
	entry.LastBandAt = nowNY
	s.entries[indicatorKey] = entry
	// Legacy direct Tick callers retain best-effort persistence. Production
	// evaluates on a volatile clone and commits through the exact publication
	_ = s.saveLocked()
	return entryToInfo(entry)
}

// Get returns the current StreakInfo for an indicator without
func (s *StreakStore) Get(indicatorKey string) *StreakInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	entry, ok := s.entries[indicatorKey]
	if !ok {
		return nil
	}
	return entryToInfo(entry)
}

// PrevBand returns the band recorded on the most recent tick — the input
// exit-hysteresis classification needs. Empty when never seen.
func (s *StreakStore) PrevBand(indicatorKey string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.entries[indicatorKey].LastBand
}

// PrevBandAt returns when PrevBand was last measured from live inputs. Zero
func (s *StreakStore) PrevBandAt(indicatorKey string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.entries[indicatorKey].LastBandAt
}

// Latched reports the eligibility latch for an indicator's current streak.
func (s *StreakStore) Latched(indicatorKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.entries[indicatorKey].EligibleLatched
}

// Latch marks the indicator's current red streak as having earned
// confirmation eligibility. No-op when the entry is missing or not red —
// the latch only ever decorates a live red streak. Best-effort persist,
// same contract as Tick.
func (s *StreakStore) Latch(indicatorKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	entry, ok := s.entries[indicatorKey]
	if !ok || entry.LastBand != "red" || entry.EligibleLatched {
		return
	}
	entry.EligibleLatched = true
	s.entries[indicatorKey] = entry
	_ = s.saveLocked()
}

func entryToInfo(e StreakEntry) *rpc.StreakInfo {
	return &rpc.StreakInfo{
		Band:     e.LastBand,
		Sessions: e.Sessions,
		Since:    e.SinceDate,
	}
}

// StreakInfo is the in-package alias for rpc.StreakInfo so callers in
// the daemon package can avoid importing the rpc package solely for
// the type name.
type StreakInfo = rpc.StreakInfo

// Indicator keys for the streak store. Stable strings; each maps to
// one regime row. Constants here so a typo at a call site fails at
// compile time rather than silently writing to a misnamed key.
const (
	StreakKeyVIXTerm   = "vix_term"
	StreakKeyVolOfVol  = "vol_of_vol"
	StreakKeyHYGSPY    = "hyg_spy"
	StreakKeyCredit    = "credit_spreads"
	StreakKeyFunding   = "funding_stress"
	StreakKeyUSDJPY    = "usdjpy"
	StreakKeyGammaZero = "gamma_zero"
	StreakKeyBreadth   = "breadth"
)

// classifyVIXTermBand maps a VIX/VIX3M ratio to its band per the spec's
// "freeze the counter".
func classifyVIXTermBand(ratio *float64) string {
	if ratio == nil {
		return ""
	}
	switch {
	case *ratio < 0.92:
		return "green"
	case *ratio < 1.00:
		return "yellow"
	default:
		return "red"
	}
}

// classifyVolOfVolBand applies vvix_daily_v2: amber is a transition, not a
// zone. VVIX's structural floor sits at 85-90, so the v1 static 90-110 band
// kept the row amber on 47% of calm days (10y Cboe replay, 2026-08-11);
// requiring the level AND a +3% rise over 5 sessions cuts that to 17% while
// every labeled crisis 2015-2024 still lit ahead of its date (COVID lead 7
// sessions). Red at 110 is untouched. A missing 5-session change is a data
// defect and must not soften the warning: level alone then holds amber,
// matching the credit veto's unknown-assumes-the-worst rule. Operator
// decision 2026-08-11 (backtest tradeoff curve presented and signed).
func classifyVolOfVolBand(vvix, rise5d *float64) string {
	if vvix == nil {
		return ""
	}
	switch {
	case *vvix >= 110:
		return "red"
	case *vvix < 90:
		return "green"
	case rise5d == nil || *rise5d >= vvixRise5DPct:
		return "yellow"
	default:
		return "green"
	}
}

// vvixRise5DPct is the 5-session rise that turns an at-level VVIX into an
// amber transition under vvix_daily_v2.
const vvixRise5DPct = 3.0

// classifyHYGSPYBand maps the HYG/50DMA + SPY/52w-high pair to its
// band per the spec's §2 thresholds. Daemon-side simplification: the
// "5+ sessions below" red trigger requires session history we don't
// track separately, so the daemon classifies on the same-day signal
// (HYG vs 50dma + SPY proximity to 52w high) and the consecutive-
// sessions count emerges naturally from the streak counter itself —
// "red · day 5" reads the same as the spec's "5+ sessions" requirement.
func classifyHYGSPYBand(r rpc.RegimeHYGSPYDivergence) string {
	if r.HYGPrice == nil || r.HYG50DMA == nil {
		return ""
	}
	if *r.HYGPrice >= *r.HYG50DMA {
		return "green"
	}
	// HYG below 50dma. Yellow vs red depends on SPY proximity to highs.
	if r.SPY52WHigh == nil || r.SPYPrice == nil {
		// Can't classify without the SPY anchor — freeze.
		return ""
	}
	const nearHigh = 0.97 // SPY ≥ 0.97 × 52w high = "near highs"
	if *r.SPYPrice >= nearHigh**r.SPY52WHigh {
		return "red"
	}
	return "yellow"
}

func classifyCreditSpreadsBand(r rpc.RegimeCreditSpreads) string {
	if r.HYOAS == nil {
		return ""
	}
	if *r.HYOAS >= 5.5 || (r.HY20DChange != nil && *r.HY20DChange >= 1.0) {
		return "red"
	}
	if *r.HYOAS >= 4.0 || (r.HY20DChange != nil && *r.HY20DChange >= 0.5) {
		return "yellow"
	}
	return "green"
}

func classifyFundingStressBand(spreadBps *float64) string {
	if spreadBps == nil {
		return ""
	}
	switch {
	case *spreadBps < 25:
		return "green"
	case *spreadBps < 75:
		return "yellow"
	default:
		return "red"
	}
}

// classifyUSDJPYBand maps the weekly USD/JPY change to its band per
// the spec's §3 thresholds. Convention: WeeklyChange negative = yen
// strengthening = the stress signal.
func classifyUSDJPYBand(weeklyChange *float64) string {
	if weeklyChange == nil {
		return ""
	}
	yenMove := -*weeklyChange // positive when yen strengthening
	switch {
	case yenMove < 1.0:
		return "green"
	case yenMove < 2.0:
		return "yellow"
	default:
		return "red"
	}
}

// classifyGammaBand maps a (gap_pct, sign) pair to its band per the
// spec's §4 thresholds. Three paths matching the renderer's gamma-row
// logic: a real crossing reads on gap distance; no-crossing reads on
// the signed-profile sign.
func classifyGammaBand(gapPct *float64, gammaSign string) string {
	if gapPct != nil {
		const yellowGap = 2.0 // ±2% of zero-gamma
		switch {
		case *gapPct > yellowGap:
			return "green"
		case *gapPct >= -yellowGap:
			return "yellow"
		default:
			return "red"
		}
	}
	// No crossing — band on the signed-profile direction.
	switch gammaSign {
	case "positive":
		return "green" // dealer long-γ across sweep = stabilising regime
	case "negative":
		return "red" // dealer short-γ across sweep = amplifying regime
	}
	return ""
}

func classifyGammaComputedBand(c *rpc.GammaZeroComputed) string {
	if !gammaComputedExplicitlyRankable(c) {
		return ""
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		return combineGammaComputedBands(c)
	}
	return classifyGammaBand(c.GapPct, c.GammaSign)
}

func gammaComputedExplicitlyRankable(c *rpc.GammaZeroComputed) bool {
	return c != nil && c.Quality != nil && c.Quality.Rankability == rpc.GammaRankabilityRankable
}

func combineGammaComputedBands(c *rpc.GammaZeroComputed) string {
	if !gammaComputedExplicitlyRankable(c) {
		return ""
	}
	type weightedBand struct {
		band   string
		weight float64
	}
	var bands []weightedBand
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil {
			continue
		}
		if band := classifyGammaComputedBand(sub); band != "" {
			bands = append(bands, weightedBand{band: band, weight: rpc.GammaIndexWeight(key, sub)})
		}
	}
	if len(bands) == 0 {
		return ""
	}
	first := bands[0].band
	for _, band := range bands[1:] {
		if band.band != first {
			first = ""
			break
		}
	}
	if first != "" {
		return first
	}
	total := 0.0
	redWeight := 0.0
	for _, band := range bands {
		total += band.weight
		if band.band == "red" {
			redWeight += band.weight
		}
	}
	if total > 0 && redWeight/total >= 0.5 {
		return "red"
	}
	return "yellow"
}

func gammaComputedStreakValue(c *rpc.GammaZeroComputed) float64 {
	if c == nil {
		return 0
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		if gap := rpc.GammaCombinedGapPct(c); gap != nil {
			return *gap
		}
		return c.GammaTotalAbs
	}
	if c.GapPct != nil {
		return *c.GapPct
	}
	return 0
}

// classifyBreadthBand maps the % above 50-DMA reading to its band.
func classifyBreadthBand(value float64) string {
	switch {
	case value < 40:
		return "red"
	case value <= 55:
		return "yellow"
	default:
		return "green"
	}
}

// DefaultStreakStoreDir returns the on-disk cache root for the streak
// engine ($XDG_CACHE_HOME/ibkr/) so all daemon caches live together.
func DefaultStreakStoreDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr"), nil
}

// nyDateNow returns time.Now() interpreted in NY local time. Mirrors
func nyDateNow() time.Time {
	now := time.Now()
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc)
	}
	return now.UTC()
}

// nyTime converts a timestamp to NY local time (UTC fallback).
func nyTime(t time.Time) time.Time {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return t.In(loc)
	}
	return t.UTC()
}

// usEquityRTHOpen reports whether the regular US cash-equity session is open
func usEquityRTHOpen(now time.Time) bool {
	s, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	return err == nil && s.IsOpen
}

// nyTradingSessionKey returns the YYYY-MM-DD key of the current NY trading
// trading day (weekends and holidays key backwards, never forwards). Falls
func nyTradingSessionKey(nowNY time.Time) string {
	cal := marketcal.New()
	for i := range 7 {
		d := nowNY.AddDate(0, 0, -i)
		s, err := cal.SessionAt(marketcal.MarketUSEquity, d)
		if err != nil {
			break
		}
		switch s.State {
		case marketcal.StateRegular, marketcal.StateEarlyClose:
			return s.Date
		case marketcal.StateUnknown:
			return nowNY.Format("2006-01-02")
		}
	}
	return nowNY.Format("2006-01-02")
}
