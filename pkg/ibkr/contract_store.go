package ibkr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContractStore serializes snapshots of resolved contracts for reuse across
// Before [ContractStore.UseAuthority] succeeds, the store uses the legacy
// tests, not as runtime authority. After an authority is attached, every load
// legacy file.
type ContractStore struct {
	dir string

	mu        sync.Mutex
	authority ContractCacheAuthority
}

// ContractCacheAuthority stores the encoded contract-cache envelope for a
// [ContractStore]. SaveContractCache must publish payload and observedAt as one
// Once [ContractStore.UseAuthority] succeeds, the store never reads or writes
// its legacy JSON path. An authority that reports ok=false represents an empty
type ContractCacheAuthority interface {
	LoadContractCache() (payload []byte, ok bool, err error)
	SaveContractCache(payload []byte, observedAt time.Time) error
}

// NewContractStore returns a store whose legacy JSON codec is rooted at dir.
// It performs no I/O and creates dir only on the first legacy save. dir is
// ignored after [ContractStore.UseAuthority] succeeds.
func NewContractStore(dir string) *ContractStore {
	return &ContractStore{dir: dir}
}

// UseAuthority validates authority's current payload and, on success, switches
// start. A nil store, nil authority, load failure, or invalid current envelope
// returns an error and leaves the existing backend unchanged. UseAuthority
// does not import or merge the legacy JSON file.
func (s *ContractStore) UseAuthority(authority ContractCacheAuthority) error {
	if s == nil {
		return errors.New("contract cache: nil store")
	}
	if authority == nil {
		return errors.New("contract cache: nil authority")
	}
	raw, ok, err := authority.LoadContractCache()
	if err != nil {
		return fmt.Errorf("load contract authority: %w", err)
	}
	if ok {
		if _, err := decodeContractCache(raw, true); err != nil {
			return fmt.Errorf("validate contract authority: %w", err)
		}
	}
	s.mu.Lock()
	s.authority = authority
	s.mu.Unlock()
	return nil
}

// contractStoreVersion is the current encoded payload version. Legacy-file
// envelopes produce a cold-cache result. Authority payloads must match the
// current version exactly.
const contractStoreVersion = 3

// contractStoreFile is the filename inside ContractStore.dir.
const contractStoreFile = "contracts.json"

// contractCacheFile is the shared SQLite/legacy-codec payload shape.
type contractCacheFile struct {
	Version     int                            `json:"version"`
	AsOf        time.Time                      `json:"as_of"`
	MembersHash string                         `json:"members_hash,omitempty"`
	Contracts   map[string]ContractDetailsLite `json:"contracts"`
	Options     map[string]ContractDetailsLite `json:"options,omitempty"`
}

// Load returns a caller-owned symbol-to-contract map and the membership hash
// missing payload, or newer legacy-file envelope returns (nil, "", nil) as a
// cold cache. Read and decode failures return an error. Older legacy envelopes
// [ContractStore.UseAuthority].
func (s *ContractStore) Load() (map[string]ContractDetailsLite, string, error) {
	if s == nil {
		return nil, "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok, err := s.loadEnvelopeLocked()
	if err != nil || !ok {
		return nil, "", err
	}
	// Older legacy envelopes remain readable; newer ones produce a cold result
	if f.Version > contractStoreVersion {
		return nil, "", nil
	}
	return f.Contracts, f.MembersHash, nil
}

// LoadOptions returns a caller-owned option-key-to-contract map. Options whose
// snapshot without rewriting persistence. Legacy keys without a trading class
// are migrated in memory to an empty class segment; malformed legacy keys are
// skipped. A nil store, missing payload, newer legacy envelope, or a snapshot
// failures return an error. Calls are serialized with [ContractStore.Save].
func (s *ContractStore) LoadOptions() (map[string]ContractDetailsLite, error) {
	if s == nil {
		return map[string]ContractDetailsLite{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok, err := s.loadEnvelopeLocked()
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]ContractDetailsLite{}, nil
	}
	if f.Version > contractStoreVersion {
		return map[string]ContractDetailsLite{}, nil
	}
	out := make(map[string]ContractDetailsLite, len(f.Options))
	today := nyDateString(time.Now())
	for k, v := range f.Options {
		if v.Expiry != "" && v.Expiry < today {
			continue
		}
		// Migrate the legacy key shape, which omitted the trading-class
		if pipes := strings.Count(k, "|"); pipes == 3 {
			parts := strings.SplitN(k, "|", 2)
			if len(parts) == 2 {
				k = parts[0] + "||" + parts[1]
			}
		} else if pipes != 4 {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func (s *ContractStore) loadEnvelopeLocked() (contractCacheFile, bool, error) {
	var raw []byte
	if s.authority != nil {
		payload, ok, err := s.authority.LoadContractCache()
		if err != nil || !ok {
			return contractCacheFile{}, ok, err
		}
		raw = payload
	} else {
		payload, err := os.ReadFile(filepath.Join(s.dir, contractStoreFile))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return contractCacheFile{}, false, nil
			}
			return contractCacheFile{}, false, fmt.Errorf("read contracts: %w", err)
		}
		raw = payload
	}
	f, err := decodeContractCache(raw, s.authority != nil)
	if err != nil {
		return contractCacheFile{}, false, err
	}
	return f, true, nil
}

func decodeContractCache(raw []byte, strictCurrent bool) (contractCacheFile, error) {
	var f contractCacheFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return contractCacheFile{}, fmt.Errorf("decode contracts: %w", err)
	}
	if !strictCurrent {
		return f, nil
	}
	if f.Version != contractStoreVersion || f.AsOf.IsZero() || f.Contracts == nil {
		return contractCacheFile{}, fmt.Errorf("invalid contract cache envelope: version=%d as_of=%s", f.Version, f.AsOf)
	}
	for key, detail := range f.Contracts {
		if strings.TrimSpace(key) == "" || detail.ConID == 0 {
			return contractCacheFile{}, fmt.Errorf("invalid contract cache row %q", key)
		}
	}
	for key, detail := range f.Options {
		normalized, err := normalizeCurrentOptionDetail(key, detail)
		if err != nil {
			return contractCacheFile{}, err
		}
		f.Options[key] = normalized
	}
	return f, nil
}

// normalizeCurrentOptionDetail makes the canonical cache key the source of
// truth for tuple fields omitted from the value. Malformed keys, zero ConIDs,
// and fields that contradict the key remain errors.
func normalizeCurrentOptionDetail(key string, detail ContractDetailsLite) (ContractDetailsLite, error) {
	parts := strings.Split(key, "|")
	if len(parts) != 5 {
		return ContractDetailsLite{}, fmt.Errorf("invalid option contract cache row %q", key)
	}
	symbol := strings.ToUpper(strings.TrimSpace(parts[0]))
	tradingClass := strings.ToUpper(strings.TrimSpace(parts[1]))
	expiry := strings.TrimSpace(parts[2])
	right := strings.ToUpper(strings.TrimSpace(parts[4]))
	strike, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	parsedExpiry, expiryErr := time.Parse("20060102", expiry)
	if err != nil || expiryErr != nil || parsedExpiry.Format("20060102") != expiry || symbol == "" || tradingClass == "" || strike <= 0 || math.IsNaN(strike) || math.IsInf(strike, 0) || (right != "C" && right != "P") || detail.ConID <= 0 || optionContractKey(symbol, tradingClass, expiry, strike, right) != key {
		return ContractDetailsLite{}, fmt.Errorf("invalid option contract cache row %q", key)
	}
	if detail.Symbol == "" {
		detail.Symbol = symbol
	} else if strings.ToUpper(strings.TrimSpace(detail.Symbol)) != symbol {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q symbol contradicts key", key)
	}
	if detail.SecType == "" {
		detail.SecType = "OPT"
	} else if !strings.EqualFold(strings.TrimSpace(detail.SecType), "OPT") {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q security type contradicts key", key)
	}
	if detail.TradingClass == "" {
		detail.TradingClass = tradingClass
	} else if tradingClass != "" && strings.ToUpper(strings.TrimSpace(detail.TradingClass)) != tradingClass {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q trading class contradicts key", key)
	}
	if detail.Expiry == "" {
		detail.Expiry = expiry
	} else if strings.TrimSpace(detail.Expiry) != expiry {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q expiry contradicts key", key)
	}
	if detail.Strike == 0 {
		detail.Strike = strike
	} else if detail.Strike != strike {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q strike contradicts key", key)
	}
	if detail.Right == "" {
		detail.Right = right
	} else if strings.ToUpper(strings.TrimSpace(detail.Right)) != right {
		return ContractDetailsLite{}, fmt.Errorf("option contract cache row %q right contradicts key", key)
	}
	return detail, nil
}

// nyDateString returns the New York trading-session date as YYYYMMDD.
// Falls back to UTC date if the zone fails to load — keeps the GC
func nyDateString(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("20060102")
	}
	return now.UTC().Format("20060102")
}

// Save publishes one filtered contract-cache snapshot. It copies its map
// inputs and does not mutate them. Contracts with a zero ConID and options
// expired before the current New York date are omitted. Every option key and
// value must form a valid, matching tuple; otherwise Save returns an error
// without publishing. membersHash may be empty when membership is not tracked.
//
// Calls on one store are serialized. An attached authority receives one
// encoded envelope; the legacy codec writes a temporary file and renames it
// over dir/contracts.json.
func (s *ContractStore) Save(contracts map[string]ContractDetailsLite, options map[string]ContractDetailsLite, membersHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make(map[string]ContractDetailsLite, len(contracts))
	for sym, detail := range contracts {
		if shouldPersistContract(detail) {
			filtered[sym] = detail
		}
	}

	// Validate and normalize every option before publication so a bad producer
	// cannot poison durable state and defer the failure until the next restart.
	filteredOptions := make(map[string]ContractDetailsLite, len(options))
	today := nyDateString(time.Now())
	for k, detail := range options {
		normalized, err := normalizeCurrentOptionDetail(k, detail)
		if err != nil {
			return fmt.Errorf("refuse invalid option contract cache save: %w", err)
		}
		if normalized.Expiry < today {
			continue
		}
		filteredOptions[k] = normalized
	}

	env := contractCacheFile{
		Version:     contractStoreVersion,
		AsOf:        time.Now().UTC(),
		MembersHash: membersHash,
		Contracts:   filtered,
		Options:     filteredOptions,
	}
	if s.authority != nil {
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode contracts: %w", err)
		}
		return s.authority.SaveContractCache(payload, env.AsOf)
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	target := filepath.Join(s.dir, contractStoreFile)
	tmp, err := os.CreateTemp(s.dir, contractStoreFile+".tmp.*")
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
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode contracts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer to skip the second Close
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename contracts: %w", err)
	}
	return nil
}

// shouldPersistContract reports whether a contract is resolved. The ordinary
// cache does not retain security type, so ConID is the only reliable filter.
func shouldPersistContract(d ContractDetailsLite) bool {
	return d.ConID != 0
}

// MembersHash returns the first 16 lowercase hexadecimal characters of a
// SHA-256 hash of members. It trims and uppercases each element before sorting,
// so input order, case, and surrounding whitespace do not affect the result.
// Duplicate elements remain significant.
func MembersHash(members []string) string {
	// Normalise BEFORE sorting so case/whitespace variants collapse to
	// the same sort position. Sort-then-normalise would have left
	// " BRK.b" sorting under leading-space, ahead of "AAPL", while the
	// canonical "BRK.B" sorts after — different hash for the same set.
	normalised := make([]string, len(members))
	for i, m := range members {
		normalised[i] = strings.ToUpper(strings.TrimSpace(m))
	}
	sort.Strings(normalised)
	h := sha256.New()
	for _, m := range normalised {
		h.Write([]byte(m))
		h.Write([]byte{0}) // delimiter to avoid "A"+"B" == "AB" collisions
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// DefaultContractStoreDir returns the root for the retired legacy JSON codec:
// $XDG_CACHE_HOME/ibkr when XDG_CACHE_HOME is set, otherwise
// $HOME/.cache/ibkr as resolved by [os.UserHomeDir]. It does not create the
// directory. Authority-backed stores do not use this path.
func DefaultContractStoreDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr"), nil
}

// SnapshotContracts returns a caller-owned copy of the connector's resolved
func (c *Connector) SnapshotContracts() map[string]ContractDetailsLite {
	c.contractMu.RLock()
	defer c.contractMu.RUnlock()
	out := make(map[string]ContractDetailsLite, len(c.contractCache))
	for sym, detail := range c.contractCache {
		if detail.ConID != 0 {
			out[sym] = detail
		}
	}
	return out
}

// SnapshotOptionContracts returns a caller-owned copy of the active
func (c *Connector) SnapshotOptionContracts() map[string]ContractDetailsLite {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil
	}
	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	out := make(map[string]ContractDetailsLite, len(conn.optionContractCache))
	for key, detail := range conn.optionContractCache {
		if detail.ConID != 0 {
			out[key] = detail
		}
	}
	return out
}

// IsOptionContractCached reports whether the active connection has a resolved
func (c *Connector) IsOptionContractCached(symbol, tradingClass, expiry string, strike float64, right string) bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	key := optionContractKey(symbol, tradingClass, expiry, strike, right)
	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	entry, ok := conn.optionContractCache[key]
	return ok && entry.ConID != 0
}

// SeedOptionContracts copies resolved entries into the active connection's
func (c *Connector) SeedOptionContracts(options map[string]ContractDetailsLite) int {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || len(options) == 0 {
		return 0
	}
	conn.optionContractMu.Lock()
	defer conn.optionContractMu.Unlock()
	seeded := 0
	for key, detail := range options {
		if detail.ConID == 0 {
			continue
		}
		if existing, ok := conn.optionContractCache[key]; ok && existing.ConID != 0 {
			continue
		}
		conn.optionContractCache[key] = detail
		seeded++
	}
	return seeded
}
