package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// expiryGridStore keeps the last successfully fetched classed
// the stack; when IBKR's secdefil farm broke at 09:33 ET on 2026-06-09
// settlement-cutoff filters at compute time, so a stale grid only loses
// dailies listed since the snapshot — never produces an already-settled
// never serve stale data while the gateway is healthy. Persistence
type expiryGridStore struct {
	dir       string // sealed legacy cache; never used after UseCoreStore
	authority *corestore.Store

	mu  sync.Mutex
	mem map[string]expiryGridEntry
	// diskChecked marks symbols whose file was already read (or found
	diskChecked map[string]bool
}

type expiryGridEntry struct {
	classed map[string][]ibkrlib.ExpiryClassedStrikes
	asOf    time.Time
}

// expiryGridPersistEnvelope is the on-disk shape. Version bumps on
// incompatible changes; the classed map reuses the wire type the
// connector returns so load → use needs no conversion.
type expiryGridPersistEnvelope struct {
	Version int                                       `json:"version"`
	Symbol  string                                    `json:"symbol"`
	AsOf    time.Time                                 `json:"as_of"`
	Classed map[string][]ibkrlib.ExpiryClassedStrikes `json:"classed"`
}

const currentExpiryGridPersistVersion = 1

const (
	expiryGridStateKind       = "gamma_expiry_grid.current.v1"
	expiryGridObservationKind = "gamma_expiry_grid.snapshot.v1"
	expiryGridSource          = "ibkr.tws.secdef_opt_params"
)

func expiryGridAuthorityScope(sym string) string {
	return "market/gamma/expiry-grid/" + normSym(sym)
}

// gammaExpiryGridMaxAge bounds how old a fallback grid may be. Five
const gammaExpiryGridMaxAge = 5 * 24 * time.Hour

func newExpiryGridStore(dir string) *expiryGridStore {
	return &expiryGridStore{
		dir:         dir,
		mem:         map[string]expiryGridEntry{},
		diskChecked: map[string]bool{},
	}
}

func (g *expiryGridStore) UseCoreStore(store *corestore.Store) error {
	if g == nil {
		return fmt.Errorf("expiry grid: nil store")
	}
	if store == nil {
		return fmt.Errorf("expiry grid: nil corestore")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authority = store
	// An attachment is an authority boundary: discard anything that may have
	// been hydrated from legacy files before the daemon acquired its lock.
	g.mem = map[string]expiryGridEntry{}
	g.diskChecked = map[string]bool{}
	return nil
}

// noteFetched records a successful live fetch, in memory and on disk.
// grid only replaces the held one when it has at least half as many
func (g *expiryGridStore) noteFetched(sym string, classed map[string][]ibkrlib.ExpiryClassedStrikes, now time.Time) error {
	if g == nil || len(classed) == 0 {
		return nil
	}
	sym = normSym(sym)
	g.mu.Lock()
	defer g.mu.Unlock()
	if held, ok := g.heldLocked(sym); ok && len(classed)*2 < len(held.classed) {
		return fmt.Errorf("expiry grid for %s: refusing to replace %d-date grid with %d-date fetch (likely partial frames)",
			sym, len(held.classed), len(classed))
	}
	g.mem[sym] = expiryGridEntry{classed: classed, asOf: now}
	g.diskChecked[sym] = true // memory is now at least as fresh as disk
	return g.writeAtomicLocked(sym, expiryGridPersistEnvelope{
		Version: currentExpiryGridPersistVersion,
		Symbol:  sym,
		AsOf:    now,
		Classed: classed,
	})
}

// fallback returns the freshest known grid for sym when it is no older
func (g *expiryGridStore) fallback(sym string, now time.Time) (map[string][]ibkrlib.ExpiryClassedStrikes, time.Time, bool) {
	if g == nil {
		return nil, time.Time{}, false
	}
	sym = normSym(sym)
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.heldLocked(sym)
	if !ok {
		return nil, time.Time{}, false
	}
	if age := now.Sub(entry.asOf); age < 0 || age > gammaExpiryGridMaxAge {
		return nil, time.Time{}, false
	}
	return entry.classed, entry.asOf, true
}

// heldLocked returns the in-memory entry, hydrating it from disk on the
// first miss. Caller holds g.mu.
func (g *expiryGridStore) heldLocked(sym string) (expiryGridEntry, bool) {
	if entry, ok := g.mem[sym]; ok {
		return entry, true
	}
	if g.diskChecked[sym] || (g.authority == nil && g.dir == "") {
		return expiryGridEntry{}, false
	}
	g.diskChecked[sym] = true
	var data []byte
	if g.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(g.authority, expiryGridAuthorityScope(sym), expiryGridStateKind)
		if err != nil || !ok {
			return expiryGridEntry{}, false
		}
	} else {
		var err error
		data, err = os.ReadFile(filepath.Join(g.dir, expiryGridFilename(sym)))
		if err != nil {
			// Missing file is the normal cold case; read errors degrade to
			// "no fallback available" — the caller already holds the live
			// fetch error, which is the one worth surfacing.
			return expiryGridEntry{}, false
		}
	}
	var env expiryGridPersistEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return expiryGridEntry{}, false
	}
	if env.Version != currentExpiryGridPersistVersion || env.Symbol != sym || len(env.Classed) == 0 {
		return expiryGridEntry{}, false
	}
	entry := expiryGridEntry{classed: env.Classed, asOf: env.AsOf}
	g.mem[sym] = entry
	return entry, true
}

// writeAtomicLocked mirrors gammaZeroStore.writeAtomic — per the
// convention there, the small duplication is preferred over a generic
// shared store layer. Caller holds g.mu.
func (g *expiryGridStore) writeAtomicLocked(sym string, env expiryGridPersistEnvelope) error {
	if g.authority != nil {
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode expiry grid authority for %s: %w", sym, err)
		}
		metadata, err := json.Marshal(struct {
			Version    int       `json:"version"`
			Symbol     string    `json:"symbol"`
			AsOf       time.Time `json:"as_of"`
			Method     string    `json:"method"`
			ExpiryDays int       `json:"expiry_days"`
		}{
			Version: currentExpiryGridPersistVersion, Symbol: sym, AsOf: env.AsOf,
			Method: "IBKR security-definition option parameters", ExpiryDays: len(env.Classed),
		})
		if err != nil {
			return fmt.Errorf("encode expiry grid metadata for %s: %w", sym, err)
		}
		return saveMarketState(g.authority, expiryGridAuthorityScope(sym), expiryGridStateKind, corestore.ObservationInput{
			ScopeKey: expiryGridAuthorityScope(sym), Source: expiryGridSource,
			Kind: expiryGridObservationKind, ObservedAt: env.AsOf,
			ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	if g.dir == "" {
		return nil
	}
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", g.dir, err)
	}
	name := expiryGridFilename(sym)
	target := filepath.Join(g.dir, name)
	tmp, err := os.CreateTemp(g.dir, name+".tmp.*")
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
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return nil
}

func expiryGridFilename(sym string) string {
	return "expiry-grid-" + strings.ToLower(sym) + ".json"
}

// expiryGridFallbackInfo travels from buildPickedExpirations back to
// the compute when the picked expirations came from the cache rather
// than a live fetch: the grid's age feeds the expiries_stale warning
// and the live error is preserved for the log line.
type expiryGridFallbackInfo struct {
	asOf    time.Time
	liveErr error
}

// staleDays buckets the grid age into whole days (minimum 1) for the
// warning code — coarse on purpose so semantically identical runs keep
// identical warning strings.
func (f *expiryGridFallbackInfo) staleDays(now time.Time) int {
	if f == nil {
		return 0
	}
	days := int((now.Sub(f.asOf) + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
	return max(days, 1)
}
