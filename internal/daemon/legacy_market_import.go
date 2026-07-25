package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/breadth/spx"
	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/daemon/history"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

type legacyMarketImportManifest struct {
	StartedAt       time.Time                    `json:"started_at"`
	CompletedAt     time.Time                    `json:"completed_at"`
	Artifacts       []legacyMarketImportArtifact `json:"artifacts"`
	ImportedFiles   int                          `json:"imported_files"`
	ImportedRecords int                          `json:"imported_records"`
	StateDocuments  int                          `json:"state_documents"`
	Observations    int                          `json:"observations"`
}

type legacyMarketImportArtifact struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Records int    `json:"records,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

type legacyMarketImportPlan struct {
	artifactIndex int
	stateDocs     int
	observations  int
	apply         func(context.Context, *corestore.Store) error
}

// recoverLegacyDecisionRotations reuses the retired history index's verified
// file-side rotation contract before SQLite cutover reads legacy regime and
// stress measurements. Callers must hold the daemon persistence lock and must
// run this before any rotated/live source scan. The legacy history database is
// opened only when it already exists or filesystem recovery evidence exists,
// so a normal empty cutover does not recreate discarded derived state.
func (s *Server) recoverLegacyDecisionRotations(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dbPath, err := defaultTradingStatePath("history.db")
	if err != nil {
		return fmt.Errorf("resolve legacy history database: %w", err)
	}
	regimePath, err := regimeDecisionsDefaultPath()
	if err != nil {
		return fmt.Errorf("resolve legacy regime journal: %w", err)
	}
	rulesPath, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		return fmt.Errorf("resolve legacy rules journal: %w", err)
	}
	stressJournalPath, err := stressDecisionsDefaultPath()
	if err != nil {
		return fmt.Errorf("resolve legacy stress journal: %w", err)
	}
	rotatedDir := filepath.Join(filepath.Dir(regimePath), "rotated")

	needed, err := legacyRotationRecoveryNeeded(dbPath, rotatedDir)
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	opts := history.Options{
		DBPath:            dbPath,
		RegimeJournalPath: regimePath,
		RulesJournalPath:  rulesPath,
		StressJournalPath: stressJournalPath,
		RotatedDir:        rotatedDir,
		Logf:              s.warnf,
		Infof:             s.infof,
	}
	legacy, err := history.Open(opts)
	if err != nil {
		return fmt.Errorf("open legacy rotation recovery index: %w", err)
	}
	legacy.RecoverRotations(s.historyRotationSources())
	if err := legacy.Close(); err != nil {
		return fmt.Errorf("close legacy rotation recovery index: %w", err)
	}
	if err := verifyLegacyRotationRecovery(ctx, dbPath, rotatedDir); err != nil {
		return err
	}
	return nil
}

func legacyRotationRecoveryNeeded(dbPath, rotatedDir string) (bool, error) {
	dbExists := false
	if info, err := os.Lstat(dbPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("legacy history database is not a regular non-symlink file: %s", dbPath)
		}
		dbExists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspect legacy history database: %w", err)
	}
	dirInfo, err := os.Lstat(rotatedDir)
	if errors.Is(err, fs.ErrNotExist) {
		return dbExists, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect legacy rotation directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return false, fmt.Errorf("legacy rotation path is not a real directory: %s", rotatedDir)
	}
	entries, err := os.ReadDir(rotatedDir)
	if err != nil {
		return false, fmt.Errorf("scan legacy rotation directory: %w", err)
	}
	for _, entry := range entries {
		if isLegacyRotationRecoveryArtifact(entry.Name()) {
			return true, nil
		}
	}
	return dbExists, nil
}

func verifyLegacyRotationRecovery(ctx context.Context, dbPath, rotatedDir string) error {
	entries, err := os.ReadDir(rotatedDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("verify legacy rotation directory: %w", err)
	}
	for _, entry := range entries {
		if isLegacyRotationRecoveryArtifact(entry.Name()) {
			return fmt.Errorf("legacy rotation recovery remains unresolved: %s", entry.Name())
		}
	}

	dsn := &url.URL{Scheme: "file", Path: dbPath}
	query := dsn.Query()
	query.Set("mode", "ro")
	query.Set("_dqs", "0")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return fmt.Errorf("verify legacy rotation index: %w", err)
	}
	defer db.Close()
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rotation_log WHERE state = 'pending'`).Scan(&pending); err != nil {
		return fmt.Errorf("verify legacy pending rotations: %w", err)
	}
	if pending != 0 {
		return fmt.Errorf("legacy rotation recovery left %d pending rotation(s)", pending)
	}
	return nil
}

func isLegacyRotationRecoveryArtifact(name string) bool {
	return strings.HasPrefix(name, ".rotation-intent-") || strings.HasPrefix(name, ".tmp-")
}

const (
	legacyRegimeMeasurementScope  = "market/legacy/regime-measurements"
	legacyRegimeMeasurementSource = "legacy.regime_decision_journal"
	legacyRegimeMeasurementKind   = "regime_measurement.v1"
	legacyStressMeasurementScope  = "market/legacy/stress-measurements"
	legacyStressMeasurementSource = "legacy.stress_decision_journal"
	legacyStressMeasurementKind   = "stress_market_measurement.v1"

	// legacyRegimeJournalFamily and legacyStressJournalFamily select one
	// decision-journal evidence family. The value is not a sensor name: it is
	// pattern-matched against on-disk evidence — rotated/<family>-decisions-
	// *.jsonl.gz and <family>-decisions.jsonl — and it labels the cutover
	// manifest artifact kinds for those same files. The stress family stays
	// "canary" because the files it names were written under that name and
	// are sealed, not rewritten; see legacyStressDecisionsFile.
	legacyRegimeJournalFamily = "regime"
	legacyStressJournalFamily = "canary"
)

type legacyRegimeMeasurementV1 struct {
	V           int                                `json:"v"`
	TS          time.Time                          `json:"ts"`
	SessionKey  string                             `json:"session_key"`
	TapeSession string                             `json:"tape_session,omitempty"`
	Indicators  map[string]regimeDecisionIndicator `json:"indicators"`
	DataQuality []rpc.DataQualityHealth            `json:"data_quality,omitempty"`
}

// legacyStressMarketMeasurement deliberately omits RegimeVerdict and
// RegimePosture: both are decision outputs embedded in StressMarketSummary,
// not market measurements suitable for a clean-slate epoch.
type legacyStressMarketMeasurement struct {
	RedClusters                int        `json:"red_clusters"`
	EligibleRedClusters        int        `json:"eligible_red_clusters"`
	EligibleRedClusterNames    []string   `json:"eligible_red_cluster_names,omitempty"`
	YellowClusters             int        `json:"yellow_clusters"`
	RankedClusters             int        `json:"ranked_clusters"`
	UnrankedClusters           int        `json:"unranked_clusters"`
	RedClusterNames            []string   `json:"red_cluster_names,omitempty"`
	YellowClusterNames         []string   `json:"yellow_cluster_names,omitempty"`
	UnconfirmedRedClusterNames []string   `json:"unconfirmed_red_cluster_names,omitempty"`
	AmbiguousClusters          []string   `json:"ambiguous_clusters,omitempty"`
	PartialClusters            []string   `json:"partial_clusters,omitempty"`
	ComputingClusters          []string   `json:"computing_clusters,omitempty"`
	DegradedClusters           []string   `json:"degraded_clusters,omitempty"`
	StaleClusters              []string   `json:"stale_clusters,omitempty"`
	SPYPrice                   *float64   `json:"spy_price,omitempty"`
	SPYChangePct               *float64   `json:"spy_change_pct,omitempty"`
	VIX                        *float64   `json:"vix,omitempty"`
	VIXChangePct               *float64   `json:"vix_change_pct,omitempty"`
	TapeSessionState           string     `json:"tape_session_state,omitempty"`
	TapeSessionReason          string     `json:"tape_session_reason,omitempty"`
	TapeNextOpen               *time.Time `json:"tape_next_open,omitempty"`
}

type legacyStressSourceAsOf struct {
	Regime       time.Time `json:"regime,omitzero"`
	MarketEvents time.Time `json:"market_events,omitzero"`
}

type legacyStressSourceFingerprints struct {
	Regime       *rpc.Fingerprint `json:"regime,omitempty"`
	MarketEvents *rpc.Fingerprint `json:"market_events,omitempty"`
}

type legacyStressMeasurementV1 struct {
	V                  int                            `json:"v"`
	TS                 time.Time                      `json:"ts"`
	SessionKey         string                         `json:"session_key"`
	Market             legacyStressMarketMeasurement  `json:"market"`
	SourceAsOf         legacyStressSourceAsOf         `json:"source_as_of,omitzero"`
	SourceFingerprints legacyStressSourceFingerprints `json:"source_fingerprints,omitzero"`
}

// importLegacyMarketObservations is the single cutover entry point for
// irreplaceable legacy market and gamma artifacts. It performs a complete
// read/validate/hash preflight before the first SQLite mutation, so malformed
// allowlisted input fails the unpublished cutover rather than yielding a
// partial authority. It never renames or deletes sources; sealing is a later
// orchestration decision after parity checks.
func importLegacyMarketObservations(ctx context.Context, authority *corestore.Store) (legacyMarketImportManifest, error) {
	manifest := legacyMarketImportManifest{StartedAt: time.Now().UTC()}
	if authority == nil {
		return manifest, errors.New("legacy market import: nil corestore")
	}
	plans, err := preflightLegacyMarketObservations(&manifest)
	if err != nil {
		manifest.CompletedAt = time.Now().UTC()
		return manifest, err
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return manifest, err
		}
		artifact := &manifest.Artifacts[plan.artifactIndex]
		if err := plan.apply(ctx, authority); err != nil {
			artifact.Status = "failed"
			artifact.Error = err.Error()
			manifest.CompletedAt = time.Now().UTC()
			return manifest, fmt.Errorf("import legacy %s %s: %w", artifact.Kind, artifact.Path, err)
		}
		artifact.Status = "imported"
		manifest.ImportedFiles++
		manifest.ImportedRecords += artifact.Records
		manifest.StateDocuments += plan.stateDocs
		manifest.Observations += plan.observations
	}
	manifest.CompletedAt = time.Now().UTC()
	return manifest, nil
}

func preflightLegacyMarketObservations(manifest *legacyMarketImportManifest) ([]legacyMarketImportPlan, error) {
	gammaDir, err := gammaZeroStoreDefaultDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy gamma directory: %w", err)
	}
	hmdsDir, err := regimeHistoryCacheDefaultDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy regime HMDS directory: %w", err)
	}
	seriesDir, err := regimeSeriesCacheDefaultDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy regime series directory: %w", err)
	}
	streakDir, err := DefaultStreakStoreDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy regime streak directory: %w", err)
	}
	breadthDir, err := spx.DefaultDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy breadth directory: %w", err)
	}
	skewPath, err := gammaSkewDiagDefaultPath()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy gamma skew path: %w", err)
	}
	regimeJournalPath, err := regimeDecisionsDefaultPath()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy regime journal path: %w", err)
	}
	stressJournalPath, err := stressDecisionsDefaultPath()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy stress journal path: %w", err)
	}
	rotatedDir := filepath.Join(filepath.Dir(regimeJournalPath), "rotated")

	var plans []legacyMarketImportPlan
	for _, scope := range knownGammaScopes {
		plan, ok, err := preflightLegacyGammaZero(manifest, filepath.Join(gammaDir, gammaZeroStoreFilename(scope)), scope)
		if err != nil {
			return nil, err
		}
		if ok {
			plans = append(plans, plan)
		}
	}
	if plan, ok, err := preflightLegacyGammaOI(manifest, filepath.Join(gammaDir, gammaOIStateFilename)); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	gridPaths, err := legacyGlob(manifest, "gamma_expiry_grid", filepath.Join(gammaDir, "expiry-grid-*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range gridPaths {
		plan, err := preflightLegacyExpiryGrid(manifest, path)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	hmdsPaths, err := legacyGlob(manifest, "regime_hmds", filepath.Join(hmdsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range hmdsPaths {
		plan, err := preflightLegacyRegimeHMDS(manifest, path)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	seriesPaths, err := legacyGlob(manifest, "regime_official_series", filepath.Join(seriesDir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range seriesPaths {
		plan, err := preflightLegacyRegimeSeries(manifest, path)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	if plan, ok, err := preflightLegacyStreaks(manifest, filepath.Join(streakDir, streakStoreFileN)); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	for _, item := range []struct {
		kind string
		name string
	}{
		{"breadth_snapshot", "snapshot.json"},
		{"breadth_windows", "windows.json"},
		{"breadth_history", "history.json"},
	} {
		plan, ok, err := preflightLegacyBreadth(manifest, item.kind, filepath.Join(breadthDir, item.name))
		if err != nil {
			return nil, err
		}
		if ok {
			plans = append(plans, plan)
		}
	}
	if plan, ok, err := preflightLegacyGammaSkew(manifest, skewPath); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	for _, family := range []string{legacyRegimeJournalFamily, legacyStressJournalFamily} {
		archivePaths, err := legacyGlob(manifest, family+"_measurement_archive", filepath.Join(rotatedDir, family+"-decisions-*.jsonl.gz"))
		if err != nil {
			return nil, err
		}
		for _, path := range archivePaths {
			plan, err := preflightLegacyDecisionMeasurements(manifest, path, family, true)
			if err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
	}
	for _, live := range []struct {
		family string
		path   string
	}{{legacyRegimeJournalFamily, regimeJournalPath}, {legacyStressJournalFamily, stressJournalPath}} {
		plan, ok, err := preflightOptionalLegacyDecisionMeasurements(manifest, live.path, live.family, false)
		if err != nil {
			return nil, err
		}
		if ok {
			plans = append(plans, plan)
		}
	}
	residualPlans, err := preflightLegacyResidualMarketObservations(manifest)
	if err != nil {
		return nil, err
	}
	plans = append(plans, residualPlans...)
	return plans, nil
}

func preflightLegacyGammaZero(manifest *legacyMarketImportManifest, path, expectedScope string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "gamma_zero", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var env gammaZeroPersistEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Result == nil || env.Scope != expectedScope || env.Version != currentGammaPersistVersion || env.Result.AsOf.IsZero() || env.Method == "" || env.Result.Method != env.Method {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid gamma-zero envelope: decode=%v scope=%q version=%d", err, env.Scope, env.Version))
	}
	manifest.Artifacts[index].Records = 1
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": env.Version, "session_key": env.SessionKey, "scope": env.Scope,
		"method": env.Method, "as_of": env.Result.AsOf, "quality": env.Result.Quality,
	})
	input := corestore.ObservationInput{
		ScopeKey: gammaZeroAuthorityScope(env.Scope), Source: gammaZeroSource,
		Kind: gammaZeroObservationKind, ObservedAt: env.Result.AsOf,
		ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, input.ScopeKey, gammaZeroStateKind, input), true, nil
}

func preflightLegacyGammaOI(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "gamma_open_interest", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var env gammaOIStateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != gammaOIPersistVersion || env.Contracts == nil || env.UpdatedAt.IsZero() {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid gamma OI envelope: decode=%v version=%d", err, env.Version))
	}
	for key, rec := range env.Contracts {
		if key == "" || rec.ObservedAt.IsZero() || gammaOIKey(rec.Underlying, rec.TradingClass, rec.ExpiryYMD, rec.Strike, rec.Right) != key {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid gamma OI record %q", key))
		}
	}
	manifest.Artifacts[index].Records = len(env.Contracts)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": env.Version, "updated_at": env.UpdatedAt,
		"method": "IBKR generic tick 101 openInterest", "contract_count": len(env.Contracts),
	})
	input := corestore.ObservationInput{
		ScopeKey: gammaOIAuthorityScope, Source: gammaOISource, Kind: gammaOIObservationKind,
		ObservedAt: env.UpdatedAt, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, input.ScopeKey, gammaOIStateKind, input), true, nil
}

func preflightLegacyExpiryGrid(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, error) {
	raw, index, _, err := readLegacyArtifact(manifest, "gamma_expiry_grid", path)
	if err != nil {
		return legacyMarketImportPlan{}, err
	}
	var env expiryGridPersistEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != currentExpiryGridPersistVersion || env.AsOf.IsZero() || env.Symbol == "" || len(env.Classed) == 0 || filepath.Base(path) != expiryGridFilename(env.Symbol) {
		return legacyMarketImportPlan{}, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid expiry-grid envelope: decode=%v symbol=%q version=%d", err, env.Symbol, env.Version))
	}
	manifest.Artifacts[index].Records = len(env.Classed)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": env.Version, "symbol": env.Symbol, "as_of": env.AsOf,
		"method": "IBKR security-definition option parameters", "expiry_days": len(env.Classed),
	})
	input := corestore.ObservationInput{
		ScopeKey: expiryGridAuthorityScope(env.Symbol), Source: expiryGridSource, Kind: expiryGridObservationKind,
		ObservedAt: env.AsOf, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, input.ScopeKey, expiryGridStateKind, input), nil
}

func preflightLegacyRegimeHMDS(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, error) {
	raw, index, _, err := readLegacyArtifact(manifest, "regime_hmds", path)
	if err != nil {
		return legacyMarketImportPlan{}, err
	}
	var entry regimeHistoryCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Key != regimeHistoryCacheKey(entry.Symbol, entry.Days) || entry.FetchedAt.IsZero() || len(entry.Bars) == 0 || filepath.Base(path) != sanitizeRegimeSeriesID(entry.Key)+".json" {
		return legacyMarketImportPlan{}, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid regime HMDS entry: decode=%v key=%q", err, entry.Key))
	}
	manifest.Artifacts[index].Records = len(entry.Bars)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": 1, "symbol": entry.Symbol, "days": entry.Days, "fetched_at": entry.FetchedAt,
		"method": "IBKR HMDS daily bars", "bar_count": len(entry.Bars),
	})
	scope := regimeHistoryAuthorityScope(entry.Symbol, entry.Days)
	input := corestore.ObservationInput{
		ScopeKey: scope, Source: regimeHistorySource, Kind: regimeHistoryObservationKind,
		ObservedAt: entry.FetchedAt, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, scope, regimeHistoryStateKind, input), nil
}

func preflightLegacyRegimeSeries(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, error) {
	raw, index, _, err := readLegacyArtifact(manifest, "regime_official_series", path)
	if err != nil {
		return legacyMarketImportPlan{}, err
	}
	var entry regimeSeriesCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil || strings.TrimSpace(entry.SeriesID) == "" || entry.FetchedAt.IsZero() || len(entry.Points) == 0 || filepath.Base(path) != sanitizeRegimeSeriesID(entry.SeriesID)+".json" {
		return legacyMarketImportPlan{}, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid official series entry: decode=%v series=%q", err, entry.SeriesID))
	}
	manifest.Artifacts[index].Records = len(entry.Points)
	latest, _ := latestSeriesPoint(entry.Points)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": 1, "series_id": entry.SeriesID, "fetched_at": entry.FetchedAt,
		"latest_date": latest.Date, "method": "official published daily series", "point_count": len(entry.Points),
	})
	scope := regimeSeriesAuthorityScope(entry.SeriesID)
	input := corestore.ObservationInput{
		ScopeKey: scope, Source: regimeSeriesObservationSource(entry.SeriesID), Kind: regimeSeriesObservationKind,
		ObservedAt: entry.FetchedAt, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, scope, regimeSeriesStateKind, input), nil
}

func preflightLegacyStreaks(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "regime_streaks", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var file streakStoreFile
	if err := json.Unmarshal(raw, &file); err != nil || file.Version != streakStoreVersion || file.AsOf.IsZero() || file.Entries == nil {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid regime streak state: decode=%v version=%d", err, file.Version))
	}
	for key, entry := range file.Entries {
		if strings.TrimSpace(key) == "" || entry.Sessions < 1 || entry.LastBand == "" || entry.LastSession == "" || entry.SinceDate == "" {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid regime streak entry %q", key))
		}
	}
	manifest.Artifacts[index].Records = len(file.Entries)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": file.Version, "as_of": file.AsOf,
		"method": "versioned daemon regime-band streak classifier", "entry_count": len(file.Entries),
	})
	input := corestore.ObservationInput{
		ScopeKey: streakAuthorityScope, Source: streakSource, Kind: streakObservationKind,
		ObservedAt: file.AsOf, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return stateImportPlan(index, streakAuthorityScope, streakStateKind, input), true, nil
}

func preflightLegacyBreadth(manifest *legacyMarketImportManifest, kind, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, kind, path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	artifact := manifest.Artifacts[index]
	switch kind {
	case "breadth_snapshot":
		var value spx.Snapshot
		if err := json.Unmarshal(raw, &value); err != nil || value.AsOf.IsZero() || value.Method == "" {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid breadth snapshot: %v", err))
		}
		manifest.Artifacts[index].Records = 1
		metadata := legacyMarketMetadata(artifact, map[string]any{
			"version": 1, "as_of": value.AsOf, "session_key": value.SessionKey,
			"method": value.Method, "coverage": value.Coverage, "member_count": value.MemberCount,
		})
		return legacyMarketImportPlan{artifactIndex: index, observations: 1, apply: func(ctx context.Context, store *corestore.Store) error {
			return spx.ImportLegacySnapshot(ctx, store, raw, metadata, value.AsOf)
		}}, true, nil
	case "breadth_windows":
		var value spx.WindowSet
		if err := json.Unmarshal(raw, &value); err != nil || value.Version != spx.CurrentWindowSetVersion || value.AsOf.IsZero() || value.Windows == nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid breadth windows: decode=%v version=%d", err, value.Version))
		}
		manifest.Artifacts[index].Records = len(value.Windows)
		metadata := legacyMarketMetadata(artifact, map[string]any{
			"version": value.Version, "as_of": value.AsOf, "method": spx.MethodConstituentFanout,
			"window_count": len(value.Windows),
		})
		return legacyMarketImportPlan{artifactIndex: index, observations: 1, apply: func(ctx context.Context, store *corestore.Store) error {
			return spx.ImportLegacyWindows(ctx, store, raw, metadata, value.AsOf)
		}}, true, nil
	case "breadth_history":
		var value spx.HistorySet
		if err := json.Unmarshal(raw, &value); err != nil || value.Version != spx.CurrentHistorySetVersion || value.Points == nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid breadth history: decode=%v version=%d", err, value.Version))
		}
		manifest.Artifacts[index].Records = len(value.Points)
		observedAt := time.Now().UTC()
		metadata := legacyMarketMetadata(artifact, map[string]any{
			"version": value.Version, "method": spx.MethodConstituentFanout, "point_count": len(value.Points),
		})
		return legacyMarketImportPlan{artifactIndex: index, observations: 1, apply: func(ctx context.Context, store *corestore.Store) error {
			return spx.ImportLegacyHistory(ctx, store, raw, metadata, observedAt)
		}}, true, nil
	default:
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("unknown breadth artifact kind %q", kind))
	}
}

func preflightLegacyGammaSkew(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "gamma_skew_diagnostics", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var inputs []corestore.ObservationInput
	for lineIndex, rawLine := range bytes.Split(raw, []byte{'\n'}) {
		rawLine = bytes.TrimSuffix(rawLine, []byte{'\r'})
		if len(bytes.TrimSpace(rawLine)) == 0 {
			continue
		}
		var line gammaSkewDiagLine
		if err := json.Unmarshal(rawLine, &line); err != nil || line.V != gammaSkewDiagVersion || line.AsOf.IsZero() || line.Scope == "" || line.Slice == "" {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid gamma skew line %d: decode=%v version=%d", lineIndex+1, err, line.V))
		}
		metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
			"version": line.V, "line": lineIndex + 1, "session_key": line.SessionKey,
			"scope": line.Scope, "slice": line.Slice, "as_of": line.AsOf,
			"method": "gamma skew diagnostic v1", "rankability": line.Rankability,
		})
		inputs = append(inputs, corestore.ObservationInput{
			ScopeKey: gammaSkewDiagScopeKey(line.Scope, line.Slice), Source: "ibkr.gamma.compute",
			Kind: gammaSkewDiagObservationKind, ObservedAt: line.AsOf,
			ContentType: "application/json", Payload: append([]byte(nil), rawLine...), MetadataJSON: metadata,
			DecisionEligible: false,
		})
	}
	if len(inputs) == 0 {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, errors.New("gamma skew journal contains no records"))
	}
	manifest.Artifacts[index].Records = len(inputs)
	return legacyMarketImportPlan{
		artifactIndex: index, observations: len(inputs),
		apply: func(ctx context.Context, store *corestore.Store) error {
			_, err := store.AppendObservations(ctx, inputs)
			return err
		},
	}, true, nil
}

func preflightLegacyDecisionMeasurements(manifest *legacyMarketImportManifest, path, family string, compressed bool) (legacyMarketImportPlan, error) {
	plan, ok, err := preflightOptionalLegacyDecisionMeasurements(manifest, path, family, compressed)
	if err != nil {
		return legacyMarketImportPlan{}, err
	}
	if !ok {
		return legacyMarketImportPlan{}, fmt.Errorf("legacy %s measurement archive disappeared during preflight: %s", family, path)
	}
	return plan, nil
}

func preflightOptionalLegacyDecisionMeasurements(manifest *legacyMarketImportManifest, path, family string, compressed bool) (legacyMarketImportPlan, bool, error) {
	kind := family + "_measurement_live"
	if compressed {
		kind = family + "_measurement_archive"
	}
	rawFile, index, ok, err := readLegacyArtifact(manifest, kind, path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	content := rawFile
	if compressed {
		reader, err := gzip.NewReader(bytes.NewReader(rawFile))
		if err != nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("open gzip: %w", err))
		}
		content, err = io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("read gzip: %w", err))
		}
		if closeErr != nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("close gzip: %w", closeErr))
		}
	}

	inputs := make([]corestore.ObservationInput, 0)
	for lineIndex, rawLine := range bytes.Split(content, []byte{'\n'}) {
		rawLine = bytes.TrimSuffix(rawLine, []byte{'\r'})
		if len(bytes.TrimSpace(rawLine)) == 0 {
			continue
		}
		lineDigest := sha256.Sum256(rawLine)
		extra := map[string]any{
			"schema_version":     1,
			"source_line":        lineIndex + 1,
			"source_line_sha256": hex.EncodeToString(lineDigest[:]),
			"compressed_archive": compressed,
		}
		switch family {
		case legacyRegimeJournalFamily:
			var line regimeDecisionLine
			if err := decodeStrictLegacyJSON(rawLine, &line); err != nil || line.V != 1 || line.TS.IsZero() || line.SessionKey == "" || line.Indicators == nil {
				return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid regime measurement line %d: decode=%v version=%d", lineIndex+1, err, line.V))
			}
			payload, err := json.Marshal(legacyRegimeMeasurementV1{
				V: 1, TS: line.TS, SessionKey: line.SessionKey, TapeSession: line.TapeSession,
				Indicators: line.Indicators, DataQuality: line.DataQuality,
			})
			if err != nil {
				return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("encode regime measurement line %d: %w", lineIndex+1, err))
			}
			extra["session_key"] = line.SessionKey
			extra["tape_session"] = line.TapeSession
			extra["indicator_count"] = len(line.Indicators)
			inputs = append(inputs, corestore.ObservationInput{
				ScopeKey: legacyRegimeMeasurementScope, Source: legacyRegimeMeasurementSource,
				Kind: legacyRegimeMeasurementKind, ObservedAt: line.TS,
				ContentType: "application/json", Payload: payload,
				MetadataJSON: legacyMarketMetadata(manifest.Artifacts[index], extra), DecisionEligible: false,
			})
		case legacyStressJournalFamily:
			var line stressDecisionLine
			if err := decodeStrictLegacyJSON(rawLine, &line); err != nil || line.V != 1 || line.TS.IsZero() || line.SessionKey == "" {
				return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid stress measurement line %d: decode=%v version=%d", lineIndex+1, err, line.V))
			}
			payload, err := json.Marshal(projectLegacyStressMeasurement(line))
			if err != nil {
				return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("encode stress measurement line %d: %w", lineIndex+1, err))
			}
			extra["session_key"] = line.SessionKey
			inputs = append(inputs, corestore.ObservationInput{
				ScopeKey: legacyStressMeasurementScope, Source: legacyStressMeasurementSource,
				Kind: legacyStressMeasurementKind, ObservedAt: line.TS,
				ContentType: "application/json", Payload: payload,
				MetadataJSON: legacyMarketMetadata(manifest.Artifacts[index], extra), DecisionEligible: false,
			})
		default:
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("unknown decision measurement family %q", family))
		}
	}
	if compressed && len(inputs) == 0 {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, errors.New("measurement archive contains no records"))
	}
	manifest.Artifacts[index].Records = len(inputs)
	return legacyMarketImportPlan{
		artifactIndex: index, observations: len(inputs),
		apply: func(ctx context.Context, store *corestore.Store) error {
			if len(inputs) == 0 {
				return nil
			}
			_, err := store.AppendObservations(ctx, inputs)
			return err
		},
	}, true, nil
}

func decodeStrictLegacyJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func projectLegacyStressMeasurement(line stressDecisionLine) legacyStressMeasurementV1 {
	market := line.Market
	return legacyStressMeasurementV1{
		V: 1, TS: line.TS, SessionKey: line.SessionKey,
		Market: legacyStressMarketMeasurement{
			RedClusters: market.RedClusters, EligibleRedClusters: market.EligibleRedClusters,
			EligibleRedClusterNames: market.EligibleRedClusterNames,
			YellowClusters:          market.YellowClusters, RankedClusters: market.RankedClusters,
			UnrankedClusters: market.UnrankedClusters, RedClusterNames: market.RedClusterNames,
			YellowClusterNames:         market.YellowClusterNames,
			UnconfirmedRedClusterNames: market.UnconfirmedRedClusterNames,
			AmbiguousClusters:          market.AmbiguousClusters, PartialClusters: market.PartialClusters,
			ComputingClusters: market.ComputingClusters, DegradedClusters: market.DegradedClusters,
			StaleClusters: market.StaleClusters, SPYPrice: market.SPYPrice,
			SPYChangePct: market.SPYChangePct, VIX: market.VIX, VIXChangePct: market.VIXChangePct,
			TapeSessionState: market.TapeSessionState, TapeSessionReason: market.TapeSessionReason,
			TapeNextOpen: market.TapeNextOpen,
		},
		SourceAsOf: legacyStressSourceAsOf{
			Regime: line.SourceAsOf.Regime, MarketEvents: line.SourceAsOf.MarketEvents,
		},
		SourceFingerprints: legacyStressSourceFingerprints{
			Regime: line.SourceFingerprints.Regime, MarketEvents: line.SourceFingerprints.MarketEvents,
		},
	}
}

func stateImportPlan(index int, scopeKey, stateKind string, input corestore.ObservationInput) legacyMarketImportPlan {
	_ = scopeKey
	_ = stateKind
	input.DecisionEligible = false
	return legacyMarketImportPlan{
		artifactIndex: index, observations: 1,
		apply: func(ctx context.Context, store *corestore.Store) error {
			_, err := store.AppendObservation(ctx, input)
			return err
		},
	}
}

func readLegacyArtifact(manifest *legacyMarketImportManifest, kind, path string) ([]byte, int, bool, error) {
	artifact := legacyMarketImportArtifact{Kind: kind, Path: path, Status: "preflight"}
	index := len(manifest.Artifacts)
	manifest.Artifacts = append(manifest.Artifacts, artifact)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		manifest.Artifacts[index].Status = "missing"
		return nil, index, false, nil
	}
	if err != nil {
		return nil, index, false, invalidLegacyArtifact(manifest, index, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, index, false, invalidLegacyArtifact(manifest, index, errors.New("artifact is not a regular non-symlink file"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, index, false, invalidLegacyArtifact(manifest, index, err)
	}
	digest := sha256.Sum256(raw)
	manifest.Artifacts[index].SHA256 = hex.EncodeToString(digest[:])
	manifest.Artifacts[index].Bytes = int64(len(raw))
	manifest.Artifacts[index].Status = "validated"
	return raw, index, true, nil
}

func legacyGlob(manifest *legacyMarketImportManifest, kind, pattern string) ([]string, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		manifest.Artifacts = append(manifest.Artifacts, legacyMarketImportArtifact{
			Kind: kind, Path: pattern, Status: "missing",
		})
	}
	return paths, nil
}

func invalidLegacyArtifact(manifest *legacyMarketImportManifest, index int, err error) error {
	manifest.Artifacts[index].Status = "invalid"
	manifest.Artifacts[index].Error = err.Error()
	return fmt.Errorf("legacy market artifact %s: %w", manifest.Artifacts[index].Path, err)
}

func legacyMarketMetadata(artifact legacyMarketImportArtifact, extra map[string]any) []byte {
	metadata := map[string]any{
		"imported_from_legacy": true,
		"decision_eligible":    false,
		"legacy_file":          filepath.Base(artifact.Path),
		"legacy_file_sha256":   artifact.SHA256,
		"legacy_file_bytes":    artifact.Bytes,
	}
	maps.Copy(metadata, extra)
	encoded, _ := json.Marshal(metadata)
	return encoded
}
