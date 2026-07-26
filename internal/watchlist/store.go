// Package watchlist stores the user's local ibkr watchlist.
package watchlist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/xdgcache"
)

const (
	fileVersion = 1
	listName    = "default"
)

// File is the persisted on-disk shape.
type File struct {
	Version int      `json:"version"`
	Symbols []string `json:"symbols"`
}

// Snapshot is the public read shape returned by the CLI and MCP tool.
type Snapshot struct {
	Name    string    `json:"name"`
	Symbols []string  `json:"symbols"`
	AsOf    time.Time `json:"as_of"`
}

// Store reads and writes one watchlist file.
type Store struct {
	Path string
}

// DefaultPath returns the local watchlist path. Watchlists are user data, not
// cache: deleting them should be an explicit user choice.
func DefaultPath() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "watchlist.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "ibkr", "watchlist.json"), nil
}

// New returns a store rooted at path.
func New(path string) *Store {
	return &Store{Path: path}
}

// NormalizeSymbol mirrors the quote command's symbol normalization: trim and
// uppercase only. Contract routing remains the quote path's job.
func NormalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// NormalizeSymbols splits the same comma-separated symbol input that
// `canary quote` accepts.
func NormalizeSymbols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = NormalizeSymbol(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load returns the persisted file, or an empty v1 file when none exists.
func (s *Store) Load() (*File, error) {
	if s == nil || s.Path == "" {
		return nil, fmt.Errorf("watchlist path is empty")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{Version: fileVersion, Symbols: []string{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.Path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	if f.Version == 0 {
		f.Version = fileVersion
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("unsupported watchlist version %d", f.Version)
	}
	f.Symbols = normalizeStored(f.Symbols)
	return &f, nil
}

// Snapshot reads the current list and returns the public shape.
func (s *Store) Snapshot() (*Snapshot, error) {
	f, err := s.Load()
	if err != nil {
		return nil, err
	}
	return snapshot(f), nil
}

// Add inserts symbols in order, skipping symbols already present.
func (s *Store) Add(symbols []string) (*Snapshot, error) {
	return s.mutate(func(f *File) bool {
		changed := false
		seen := set(f.Symbols)
		for _, sym := range normalizeStored(symbols) {
			if seen[sym] {
				continue
			}
			f.Symbols = append(f.Symbols, sym)
			seen[sym] = true
			changed = true
		}
		return changed
	})
}

// Remove deletes symbols when present.
func (s *Store) Remove(symbols []string) (*Snapshot, error) {
	remove := set(normalizeStored(symbols))
	return s.mutate(func(f *File) bool {
		if len(remove) == 0 {
			return false
		}
		out := f.Symbols[:0]
		changed := false
		for _, sym := range f.Symbols {
			if remove[sym] {
				changed = true
				continue
			}
			out = append(out, sym)
		}
		f.Symbols = out
		return changed
	})
}

// Clear removes every symbol.
func (s *Store) Clear() (*Snapshot, error) {
	return s.mutate(func(f *File) bool {
		if len(f.Symbols) == 0 {
			return false
		}
		f.Symbols = []string{}
		return true
	})
}

func (s *Store) mutate(fn func(*File) bool) (*Snapshot, error) {
	if s == nil || s.Path == "" {
		return nil, fmt.Errorf("watchlist path is empty")
	}
	lock, err := xdgcache.OpenLock(s.Path + ".lock")
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	f, err := s.Load()
	if err != nil {
		return nil, err
	}
	if fn(f) {
		if err := s.save(f); err != nil {
			return nil, err
		}
	}
	return snapshot(f), nil
}

func (s *Store) save(f *File) error {
	f.Version = fileVersion
	f.Symbols = normalizeStored(f.Symbols)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watchlist: %w", err)
	}
	data = append(data, '\n')
	return xdgcache.WriteAtomic(s.Path, data)
}

func snapshot(f *File) *Snapshot {
	syms := make([]string, len(f.Symbols))
	copy(syms, f.Symbols)
	return &Snapshot{
		Name:    listName,
		Symbols: syms,
		AsOf:    time.Now(),
	}
}

func normalizeStored(symbols []string) []string {
	out := make([]string, 0, len(symbols))
	seen := map[string]bool{}
	for _, sym := range symbols {
		sym = NormalizeSymbol(sym)
		if sym == "" || seen[sym] {
			continue
		}
		out = append(out, sym)
		seen[sym] = true
	}
	return out
}

func set(symbols []string) map[string]bool {
	out := make(map[string]bool, len(symbols))
	for _, sym := range symbols {
		out[sym] = true
	}
	return out
}
