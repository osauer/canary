package spx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

// Store persists the engine's current snapshot, constituent windows, and
// rolling history. UseCoreStore binds normal daemon operation to typed
// daemon.db state and observations. The directory supplied to NewStore remains
// for explicit legacy import and isolated codec use.
type Store struct {
	dir       string // sealed legacy cache; never used after UseCoreStore
	authority *corestore.Store
}

const (
	breadthAuthorityScope          = "market/breadth/spx"
	breadthSource                  = "ibkr.tws.hmds.constituent_fanout"
	breadthSnapshotStateKind       = "breadth_spx.snapshot.current.v1"
	breadthSnapshotObservationKind = "breadth_spx.snapshot.v1"
	breadthWindowsStateKind        = "breadth_spx.windows.current.v2"
	breadthWindowsObservationKind  = "breadth_spx.windows.v2"
	breadthHistoryStateKind        = "breadth_spx.history.current.v2"
	breadthHistoryObservationKind  = "breadth_spx.history.v2"
)

// NewStore returns a Store rooted at dir. The directory is created on
// first write (lazy mkdir keeps tests that pass an unwritable dir
// from failing at construction time). dir must be an absolute path
// or relative to the daemon's working directory; the store does not
// resolve XDG paths itself — that's the caller's job (see DefaultDir).
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// UseCoreStore makes daemon.db the sole runtime persistence path. NewStore's
// directory remains available only to the explicit cutover importer and
// isolated legacy codec tests.
func (s *Store) UseCoreStore(store *corestore.Store) error {
	if s == nil {
		return errors.New("breadth store: nil store")
	}
	if store == nil {
		return errors.New("breadth store: nil corestore")
	}
	s.authority = store
	return nil
}

// LoadSnapshot returns the persisted snapshot or (nil, nil) when no current
// state exists. A methodology-token mismatch is also treated as no state so an
// incompatible payload cannot publish zero-valued measurements as current.
func (s *Store) LoadSnapshot() (*Snapshot, error) {
	data, ok, err := s.load("snapshot.json", breadthSnapshotStateKind)
	if err != nil || !ok {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if snap.Method != methodConstituentFanout {
		// Methodology mismatch: treat as no-cache. The next refresh tick
		// rebuilds from scratch with the current methodology.
		return nil, nil
	}
	return &snap, nil
}

// SaveSnapshot writes snap atomically. Existing file is replaced.
func (s *Store) SaveSnapshot(snap Snapshot) error {
	if s.authority != nil {
		metadata, err := json.Marshal(struct {
			Version    int       `json:"version"`
			AsOf       time.Time `json:"as_of"`
			SessionKey string    `json:"session_key"`
			Method     string    `json:"method"`
			Coverage   int       `json:"coverage"`
			Members    int       `json:"member_count"`
		}{
			Version: 1, AsOf: snap.AsOf, SessionKey: snap.SessionKey,
			Method: snap.Method, Coverage: snap.Coverage, Members: snap.MemberCount,
		})
		if err != nil {
			return err
		}
		return s.saveAuthority(breadthSnapshotStateKind, breadthSnapshotObservationKind, snap.AsOf, snap, metadata)
	}
	return s.writeAtomic("snapshot.json", snap)
}

// LoadWindows returns the persisted constituent windows, or (nil, nil)
// when no file exists or the on-disk schema version doesn't match. The
// version-mismatch case is intentionally non-fatal: a future format
// bump triggers a cold-rebuild rather than a daemon error.
func (s *Store) LoadWindows() (map[string]ConstituentWindow, error) {
	data, ok, err := s.load("windows.json", breadthWindowsStateKind)
	if err != nil || !ok {
		return nil, err
	}
	var set WindowSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decode windows: %w", err)
	}
	if set.Version != CurrentWindowSetVersion {
		// Future-version files are treated as no-cache: safer to
		// cold-rebuild than to mis-interpret an unknown schema.
		return nil, nil
	}
	return set.Windows, nil
}

// SaveWindows persists the full window map. Pass nil to wipe.
func (s *Store) SaveWindows(windows map[string]ConstituentWindow, asOf time.Time) error {
	set := WindowSet{
		Version: CurrentWindowSetVersion,
		AsOf:    asOf,
		Windows: windows,
	}
	if s.authority != nil {
		metadata, err := json.Marshal(struct {
			Version     int       `json:"version"`
			AsOf        time.Time `json:"as_of"`
			WindowCount int       `json:"window_count"`
			Method      string    `json:"method"`
		}{
			Version: CurrentWindowSetVersion, AsOf: asOf,
			WindowCount: len(windows), Method: methodConstituentFanout,
		})
		if err != nil {
			return err
		}
		return s.saveAuthority(breadthWindowsStateKind, breadthWindowsObservationKind, asOf, set, metadata)
	}
	return s.writeAtomic("windows.json", set)
}

// checkpointWindows replaces the resumable window projection without
// appending a full-map immutable observation. The finalise path calls
// SaveWindows once per completed pass to retain the canonical observation;
// interim batches are crash-recovery coordination, not separate market
// evidence. Legacy standalone mode keeps the same atomic-file behavior.
func (s *Store) checkpointWindows(windows map[string]ConstituentWindow, asOf time.Time) error {
	set := WindowSet{
		Version: CurrentWindowSetVersion,
		AsOf:    asOf,
		Windows: windows,
	}
	if s.authority == nil {
		return s.writeAtomic("windows.json", set)
	}
	payload, err := json.Marshal(set)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for range 4 {
		doc, ok, err := s.authority.GetStateDocument(ctx, breadthAuthorityScope, breadthWindowsStateKind)
		if err != nil {
			return err
		}
		var revision int64
		if ok {
			revision = doc.Revision
		}
		_, err = s.authority.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: breadthAuthorityScope, Kind: breadthWindowsStateKind,
			ExpectedRevision: revision, JSON: payload,
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return fmt.Errorf("checkpoint breadth authority %s: %w", breadthWindowsStateKind, corestore.ErrRevisionConflict)
}

// LoadHistory returns the persisted rolling-history series or (nil,
// nil) when no file exists yet. Like the other loaders, an unknown
// schema version triggers a cold rebuild rather than an error so a
// future format bump doesn't poison startup.
func (s *Store) LoadHistory() ([]HistoryPoint, error) {
	data, ok, err := s.load("history.json", breadthHistoryStateKind)
	if err != nil || !ok {
		return nil, err
	}
	var set HistorySet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	if set.Version != CurrentHistorySetVersion {
		return nil, nil
	}
	return set.Points, nil
}

// SaveHistory persists the rolling history. Pass an empty slice to
// wipe.
func (s *Store) SaveHistory(points []HistoryPoint) error {
	set := HistorySet{Version: CurrentHistorySetVersion, Points: points}
	if s.authority != nil {
		now := time.Now().UTC()
		var latest string
		if len(points) > 0 {
			latest = points[len(points)-1].Date
		}
		metadata, err := json.Marshal(struct {
			Version    int       `json:"version"`
			RecordedAt time.Time `json:"recorded_at"`
			LatestDate string    `json:"latest_date,omitempty"`
			PointCount int       `json:"point_count"`
			Method     string    `json:"method"`
		}{
			Version: CurrentHistorySetVersion, RecordedAt: now, LatestDate: latest,
			PointCount: len(points), Method: methodConstituentFanout,
		})
		if err != nil {
			return err
		}
		return s.saveAuthority(breadthHistoryStateKind, breadthHistoryObservationKind, now, set, metadata)
	}
	return s.writeAtomic("history.json", set)
}

func (s *Store) load(legacyName, stateKind string) ([]byte, bool, error) {
	if s.authority != nil {
		doc, ok, err := s.authority.GetStateDocument(context.Background(), breadthAuthorityScope, stateKind)
		if err != nil {
			return nil, false, fmt.Errorf("read breadth authority %s: %w", stateKind, err)
		}
		if !ok {
			return nil, false, nil
		}
		return append([]byte(nil), doc.JSON...), true, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, legacyName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read legacy breadth %s: %w", legacyName, err)
	}
	return data, true, nil
}

func (s *Store) saveAuthority(stateKind, observationKind string, observedAt time.Time, value any, metadata []byte) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.saveAuthorityPayload(context.Background(), stateKind, observationKind, observedAt, payload, metadata)
}

func (s *Store) saveAuthorityPayload(ctx context.Context, stateKind, observationKind string, observedAt time.Time, payload, metadata []byte) error {
	_ = observationKind
	_ = observedAt
	_ = metadata
	for range 4 {
		doc, ok, err := s.authority.GetStateDocument(ctx, breadthAuthorityScope, stateKind)
		if err != nil {
			return err
		}
		var revision int64
		if ok {
			revision = doc.Revision
		}
		_, err = s.authority.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: breadthAuthorityScope, Kind: stateKind,
			ExpectedRevision: revision, JSON: payload,
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return fmt.Errorf("save breadth authority %s: %w", stateKind, corestore.ErrRevisionConflict)
}

// ImportLegacySnapshot/Windows/History are compatibility hooks for the
// cutover preflight. Schema 6 intentionally retires these beta caches after
// validation, so they publish neither history nor current state.
func ImportLegacySnapshot(ctx context.Context, authority *corestore.Store, payload, metadata []byte, observedAt time.Time) error {
	_, _, _, _, _ = ctx, authority, payload, metadata, observedAt
	return nil
}

// ImportLegacyWindows validates through the caller and retains no beta row.
func ImportLegacyWindows(ctx context.Context, authority *corestore.Store, payload, metadata []byte, observedAt time.Time) error {
	_, _, _, _, _ = ctx, authority, payload, metadata, observedAt
	return nil
}

// ImportLegacyHistory validates through the caller and retains no beta row.
func ImportLegacyHistory(ctx context.Context, authority *corestore.Store, payload, metadata []byte, observedAt time.Time) error {
	_, _, _, _, _ = ctx, authority, payload, metadata, observedAt
	return nil
}

// UseCoreStore attaches daemon.db and loads the engine projection from it.
// Production constructs the engine with Options.DeferStoreLoad so no legacy
// cache is read before the daemon acquires its persistence lock.
func (e *Engine) UseCoreStore(store *corestore.Store) error {
	if e == nil || e.store == nil {
		return errors.New("breadth engine: nil store")
	}
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	if err := e.store.UseCoreStore(store); err != nil {
		return err
	}
	snapshot, err := e.store.LoadSnapshot()
	if err != nil {
		return err
	}
	windows, err := e.store.LoadWindows()
	if err != nil {
		return err
	}
	history, err := e.store.LoadHistory()
	if err != nil {
		return err
	}
	if windows == nil {
		windows = map[string]ConstituentWindow{}
	}
	e.mu.Lock()
	e.snapshot = snapshot
	e.windows = windows
	e.history = history
	e.mu.Unlock()
	return nil
}

// writeAtomic encodes v as JSON and replaces dir/name with the result
// in a single atomic os.Rename. Pretty-printed (indent=2) so a human
// debugging the cache can `cat` the file and read it.
func (s *Store) writeAtomic(name string, v any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, name+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any error past this point, remove the orphaned temp file so
	// we don't litter the cache dir.
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer to skip the second Close
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return nil
}

// DefaultDir returns the on-disk cache root the daemon uses by
// default: $XDG_CACHE_HOME/ibkr/breadth-spx/, falling back to
// $HOME/.cache/ibkr/breadth-spx/ when XDG_CACHE_HOME is unset (the
// XDG spec's documented default).
//
// Returns an error only if neither XDG_CACHE_HOME nor HOME is set,
// which on a real OS user account doesn't happen. Tests that need
// a deterministic path should construct NewStore directly with
// t.TempDir() rather than relying on this function.
func DefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "breadth-spx"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "breadth-spx"), nil
}
