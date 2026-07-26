package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaZeroStore persists the gamma-zero compute result across daemon
// restarts. The TTL of the result is one NY trading session, and the
// compute itself can run 5+ minutes cold after the slot-anchored
// expiry picker, so restart-cost matters.
//
// Runtime persistence uses one daemon.db state document and immutable
// observation stream per scope. The legacy per-scope JSON codec remains only
// for deterministic cutover import and isolated tests. The store is not folded
// into a generic cache because gamma's invalidation semantics are domain-
// specific and the shared code would be tiny.
type gammaZeroStore struct {
	// dir is the sealed legacy-cache location used only by the cutover
	// importer and file-codec tests. authority, once attached, is the sole
	// runtime read/write path and never falls back to dir.
	dir       string
	authority *corestore.Store
}

const (
	gammaZeroStateKind       = "gamma_zero.current.v1"
	gammaZeroObservationKind = "gamma_zero.compute.v1"
	gammaZeroSource          = "ibkr.tws.option_chain"
)

func gammaZeroAuthorityScope(scope string) string {
	return "market/gamma/zero/" + scope
}

// gammaZeroStoreFilename returns the canonical filename for a given
// scope. Kept as a small helper so both Load and Save use the exact
// same mapping (no opportunity for a "save here, load there" bug to
// silently drift).
func gammaZeroStoreFilename(scope string) string {
	return "gamma-zero-" + scope + ".json"
}

// gammaZeroPersistEnvelope is the persisted payload shape. The header
// fields are independent cold-cache gates:
//
//   - Version mismatch: a future format bump triggers cold rebuild
//     rather than half-decoding into the new shape.
//   - SessionKey mismatch with today's NY date: a cached result from
//     a prior session is gracefully ignored on load.
//   - Scope mismatch with the requested scope: defense against a
//     renamed file or a write-to-wrong-name bug. The envelope
//     announces its own scope; Load rejects mismatches.
//   - Method mismatch: a methodology bump (e.g. perfiliev-bs-sweep-v2
//     → v3) invalidates pre-bump persisted results.
type gammaZeroPersistEnvelope struct {
	Version    int                    `json:"version"`
	SessionKey string                 `json:"session_key"`
	Scope      string                 `json:"scope"`
	Method     string                 `json:"method"`
	Result     *rpc.GammaZeroComputed `json:"result"`
}

// currentGammaPersistVersion is the schema version of the persisted
// envelope. Bump on any incompatible shape change to the envelope
// itself; not bumped for additive changes inside Result, which are
// handled by Result.Method.
const currentGammaPersistVersion = 1

// newGammaZeroStore returns a store rooted at dir. The directory is
// created lazily on first write (mkdir-on-Save) so tests that pass
// an unwritable dir don't fail at construction.
func newGammaZeroStore(dir string) *gammaZeroStore {
	return &gammaZeroStore{dir: dir}
}

// UseCoreStore switches all runtime reads and writes to daemon.db. Callers
// attach it under the daemon persistence lock before any cache load.
func (s *gammaZeroStore) UseCoreStore(store *corestore.Store) error {
	if s == nil {
		return errors.New("gamma-zero cache: nil store")
	}
	if store == nil {
		return errors.New("gamma-zero cache: nil corestore")
	}
	s.authority = store
	return nil
}

// UseCoreStore attaches both the served gamma snapshot store and the skew
// diagnostics stream without relying on legacy path resolution. It must run
// before ensureLoaded; daemon startup enforces that ordering under the
// persistence lock.
func (c *gammaZeroCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("gamma cache: nil cache")
	}
	if c.store == nil {
		c.store = newGammaZeroStore("")
	}
	if err := c.store.UseCoreStore(store); err != nil {
		return err
	}
	if c.skewDiag == nil {
		c.skewDiag = &gammaSkewDiagJournal{}
	}
	return c.skewDiag.UseCoreStore(store)
}

// Load returns the persisted result for scope or (nil, nil) on:
//   - missing state (cold start for this scope),
//   - version mismatch,
//   - session-key mismatch with today's NY date,
//   - method mismatch with the persisted Result.Method,
//   - scope mismatch (envelope's Scope ≠ requested scope; defense against a
//     wrong-key write or malformed legacy import).
//
// An error is returned only for actual I/O problems or JSON
// corruption — neither should happen in steady state but both must
// surface clearly when they do. Callers treat (nil, nil) as cold and
// kick a fresh compute for that scope.
func (s *gammaZeroStore) Load(scope string, nyNow time.Time) (*rpc.GammaZeroComputed, error) {
	data, ok, err := s.loadEnvelope(scope)
	if err != nil || !ok {
		return nil, err
	}
	var env gammaZeroPersistEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode gamma-zero cache scope=%s: %w", scope, err)
	}
	if env.Version != currentGammaPersistVersion {
		return nil, nil
	}
	if env.SessionKey != nySessionKey(nyNow) {
		return nil, nil
	}
	if env.Scope != scope {
		// Scope-mismatch gate: a file at gamma-zero-spy.json whose
		// envelope says Scope="spy+spx" indicates a renamed/linked
		// file or a write-to-wrong-name bug. Treat as cold rather than
		// returning the wrong-shape payload as the requested scope.
		return nil, nil
	}
	if env.Result == nil {
		return nil, nil
	}
	// Method-token gate: the persisted Result's Method must match
	// what the envelope claims (sanity) AND match the current
	// methodology the daemon would write. The second check is the
	// load-bearing one; the first is defense-in-depth against a
	// hand-edited cache file.
	if env.Result.Method != env.Method {
		return nil, nil
	}
	if env.Method != gammaMethodToken {
		return nil, nil
	}
	return env.Result, nil
}

// LoadStale returns the persisted result for scope without the
// session-key freshness gate. Mirrors Load except it accepts a result
// whose env.SessionKey was recorded under a prior NY trading date.
// Version / Scope / Method gates still apply — a v1-shape file from a
// prior methodology era is still rejected as cold.
//
// Used by the cache boot path when today's session-keyed state is
// absent. The stale value is loaded as last-known-good context:
// during regular option hours kickOrJoin refreshes behind it, and
// outside regular option hours it is served without a non-force
// refresh. Freshness/quality gates make the stale age visible to
// callers.
func (s *gammaZeroStore) LoadStale(scope string) (*rpc.GammaZeroComputed, error) {
	data, ok, err := s.loadEnvelope(scope)
	if err != nil || !ok {
		return nil, err
	}
	var env gammaZeroPersistEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode gamma-zero cache scope=%s: %w", scope, err)
	}
	if env.Version != currentGammaPersistVersion {
		return nil, nil
	}
	if env.Scope != scope {
		return nil, nil
	}
	if env.Result == nil {
		return nil, nil
	}
	if env.Result.Method != env.Method {
		return nil, nil
	}
	if env.Method != gammaMethodToken {
		return nil, nil
	}
	return env.Result, nil
}

func (s *gammaZeroStore) loadEnvelope(scope string) ([]byte, bool, error) {
	if s.authority != nil {
		data, ok, err := loadMarketState(s.authority, gammaZeroAuthorityScope(scope), gammaZeroStateKind)
		if err != nil {
			return nil, false, fmt.Errorf("read gamma-zero authority scope=%s: %w", scope, err)
		}
		return data, ok, nil
	}
	path := filepath.Join(s.dir, gammaZeroStoreFilename(scope))
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read legacy gamma-zero cache scope=%s: %w", scope, err)
	}
	return data, true, nil
}

// Save commits the current result and its immutable observation to the scope's
// canonical daemon.db authority key. The file branch is legacy/test-only.
// sessionKey is captured separately (rather than derived from
// time.Now() inside Save) so the caller's notion of "what session
// this compute belongs to" is the same one the cache uses for
// keying — keeps the load gate honest under DST boundaries and tests
// that pass synthetic times.
//
// Returns an error for I/O or encoding failures. Callers log and
// continue; persistence failure must NOT fail the compute itself.
func (s *gammaZeroStore) Save(scope, sessionKey string, r *rpc.GammaZeroComputed) error {
	if r == nil {
		return errors.New("gamma-zero cache: nil result")
	}
	env := gammaZeroPersistEnvelope{
		Version:    currentGammaPersistVersion,
		SessionKey: sessionKey,
		Scope:      scope,
		Method:     r.Method,
		Result:     r,
	}
	if s.authority != nil {
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode gamma-zero authority scope=%s: %w", scope, err)
		}
		metadata, err := json.Marshal(struct {
			Version    int       `json:"version"`
			SessionKey string    `json:"session_key"`
			Scope      string    `json:"scope"`
			Method     string    `json:"method"`
			AsOf       time.Time `json:"as_of"`
			Quality    any       `json:"quality,omitempty"`
		}{
			Version: currentGammaPersistVersion, SessionKey: sessionKey,
			Scope: scope, Method: r.Method, AsOf: r.AsOf, Quality: r.Quality,
		})
		if err != nil {
			return fmt.Errorf("encode gamma-zero metadata scope=%s: %w", scope, err)
		}
		return saveMarketState(s.authority, gammaZeroAuthorityScope(scope), gammaZeroStateKind, corestore.ObservationInput{
			ScopeKey:         gammaZeroAuthorityScope(scope),
			Source:           gammaZeroSource,
			Kind:             gammaZeroObservationKind,
			ObservedAt:       r.AsOf,
			ContentType:      "application/json",
			Payload:          payload,
			MetadataJSON:     metadata,
			DecisionEligible: true,
		})
	}
	return s.writeAtomic(gammaZeroStoreFilename(scope), env)
}

// writeAtomic encodes v as JSON (pretty-printed, indent=2 — so a human
// debugging the cache can `cat` and read it) and replaces dir/name in
// a single atomic os.Rename. Mirrors the same helper in
// internal/breadth/spx/store.go; the two stores are deliberately
// distinct and the small code duplication is preferred over a generic
// shared layer.
func (s *gammaZeroStore) writeAtomic(name string, v any) error {
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
	// the cache dir doesn't accumulate junk.
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

// gammaZeroStoreDefaultDir resolves the on-disk cache root the daemon
// uses by default: $XDG_CACHE_HOME/ibkr/gamma-zero/, falling back to
// $HOME/.cache/ibkr/gamma-zero/ when XDG_CACHE_HOME is unset (XDG
// spec's documented default).
//
// Returns an error only if neither XDG_CACHE_HOME nor HOME is set,
// which on a real OS user account doesn't happen. Tests should
// construct newGammaZeroStore directly with t.TempDir().
func gammaZeroStoreDefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "gamma-zero"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "gamma-zero"), nil
}
