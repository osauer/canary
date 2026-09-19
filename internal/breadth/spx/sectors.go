package spx

import (
	"maps"
	"strings"
	"sync"
	"time"
)

// sectorRegistry holds the GICS sector per S&P-500 ticker. It starts from the
// release-time baseline and is replaced whenever the membership refresher
// parses a page that carries the sector column. It is a classification aid
// for held positions, not breadth input: breadth reads the member list only.
var sectorRegistry = struct {
	sync.RWMutex
	sectors map[string]string
	asOf    time.Time
}{sectors: sp500Sectors, asOf: sp500AsOf}

// SectorOf reports the GICS sector Wikipedia lists for an S&P-500 ticker.
// Symbols are matched case-insensitively; a class-share ticker matches with
// either "." or "-" as its separator.
func SectorOf(symbol string) (string, bool) {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	if key == "" {
		return "", false
	}
	sectorRegistry.RLock()
	defer sectorRegistry.RUnlock()
	if s, ok := sectorRegistry.sectors[key]; ok {
		return s, true
	}
	if alt := strings.ReplaceAll(key, "-", "."); alt != key {
		if s, ok := sectorRegistry.sectors[alt]; ok {
			return s, true
		}
	}
	if alt := strings.ReplaceAll(key, ".", "-"); alt != key {
		if s, ok := sectorRegistry.sectors[alt]; ok {
			return s, true
		}
	}
	return "", false
}

// SectorsAsOf reports when the current sector map was observed.
func SectorsAsOf() time.Time {
	sectorRegistry.RLock()
	defer sectorRegistry.RUnlock()
	return sectorRegistry.asOf
}

// SetSectors replaces the registry with a freshly parsed map. A map smaller
// than the membership sanity floor is ignored: a page whose sector column
// moved must not blank out the classification of the whole book.
func SetSectors(sectors map[string]string, asOf time.Time) bool {
	if len(sectors) < MinMembers {
		return false
	}
	next := make(map[string]string, len(sectors))
	for k, v := range sectors {
		next[strings.ToUpper(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	sectorRegistry.Lock()
	sectorRegistry.sectors = next
	sectorRegistry.asOf = asOf
	sectorRegistry.Unlock()
	return true
}

// SectorList returns a copy of the current sector map and its observation
// time, for the release-time generator and diagnostics.
func SectorList() (map[string]string, time.Time) {
	sectorRegistry.RLock()
	defer sectorRegistry.RUnlock()
	return maps.Clone(sectorRegistry.sectors), sectorRegistry.asOf
}
