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
// observation stream per scope. The legacy per-scope JSON codec remains only
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
func gammaZeroStoreFilename(scope string) string {
	return "gamma-zero-" + scope + ".json"
}

// gammaZeroPersistEnvelope is the persisted payload shape. The header
//
//	a prior session is gracefully ignored on load.
type gammaZeroPersistEnvelope struct {
	Version    int                    `json:"version"`
	SessionKey string                 `json:"session_key"`
	Scope      string                 `json:"scope"`
	Method     string                 `json:"method"`
	Result     *rpc.GammaZeroComputed `json:"result"`
}

// currentGammaPersistVersion is the schema version of the persisted
// envelope. Bump on any incompatible shape change to the envelope
const currentGammaPersistVersion = 1

// newGammaZeroStore returns a store rooted at dir. The directory is
// an unwritable dir don't fail at construction.
func newGammaZeroStore(dir string) *gammaZeroStore {
	return &gammaZeroStore{dir: dir}
}

// UseCoreStore switches all runtime reads and writes to daemon.db. Callers
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
//
//	wrong-key write or malformed legacy import).
//
// An error is returned only for actual I/O problems or JSON
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
		return nil, nil
	}
	if env.Result == nil {
		return nil, nil
	}
	// Method-token gate: the persisted Result's Method must match
	if env.Result.Method != env.Method {
		return nil, nil
	}
	if env.Method != gammaMethodToken {
		return nil, nil
	}
	return env.Result, nil
}

// LoadStale returns the persisted result for scope without the
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
// Returns an error only if neither XDG_CACHE_HOME nor HOME is set,
// which on a real OS user account doesn't happen. Tests should
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
