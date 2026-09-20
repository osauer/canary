package spx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The sector map parsed from the daily membership page is kept as a state
// document beside the member list, under the same authority binding, so a
// restart resumes from the last accepted map instead of the release baseline.
// It is a classification aid rather than evidence: only the current document
// is kept, and a document the binary's own baseline outdates is ignored.
const (
	sectorsStateKind          = "spx_sectors.current.v1"
	currentSectorsFileVersion = 1
)

type sectorsFile struct {
	Version int               `json:"version"`
	AsOf    time.Time         `json:"as_of"`
	Source  string            `json:"source"`
	URL     string            `json:"url"`
	Count   int               `json:"count"`
	Sectors map[string]string `json:"sectors"`
}

// SaveSectors persists an accepted sector map to daemon.db under the members
// authority bound to path. Before a binding exists there is nothing to
// persist and nil is returned: the daemon keeps no file cache for sectors and
// the release baseline is the floor.
func SaveSectors(path string, sectors map[string]string, asOf time.Time) error {
	authority := membersAuthority(path)
	if authority == nil {
		return nil
	}
	env, err := validateSectorsEnvelope(sectorsFile{
		Version: currentSectorsFileVersion, AsOf: asOf,
		Source: "wikipedia", URL: WikipediaURL,
		Count: len(sectors), Sectors: sectors,
	})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal sectors authority: %w", err)
	}
	return saveAuthorityDocument(context.Background(), authority, sectorsStateKind, payload)
}

// LoadSectors reads the persisted sector map for the members authority bound
// to path. ok is false when no binding exists or nothing usable is stored.
func LoadSectors(path string) (sectors map[string]string, asOf time.Time, ok bool) {
	authority := membersAuthority(path)
	if authority == nil {
		return nil, time.Time{}, false
	}
	doc, found, err := authority.GetStateDocument(context.Background(), membersAuthorityScope, sectorsStateKind)
	if err != nil || !found {
		return nil, time.Time{}, false
	}
	var env sectorsFile
	if err := json.Unmarshal(doc.JSON, &env); err != nil {
		return nil, time.Time{}, false
	}
	env, err = validateSectorsEnvelope(env)
	if err != nil {
		return nil, time.Time{}, false
	}
	return env.Sectors, env.AsOf, true
}

// RestoreSectors applies the persisted map when it is newer than the map in
// use. At start that is the release baseline, which every release
// regenerates, so a document an older binary wrote before that release never
// wins over it. It reports the map's observation time and whether it applied.
func RestoreSectors(path string) (time.Time, bool) {
	sectors, asOf, ok := LoadSectors(path)
	if !ok || !asOf.After(SectorsAsOf()) {
		return time.Time{}, false
	}
	if !SetSectors(sectors, asOf) {
		return time.Time{}, false
	}
	return asOf, true
}

func validateSectorsEnvelope(env sectorsFile) (sectorsFile, error) {
	if env.Version != currentSectorsFileVersion || env.AsOf.IsZero() {
		return sectorsFile{}, fmt.Errorf("invalid sectors envelope: version=%d as_of=%s", env.Version, env.AsOf)
	}
	if env.Source != "wikipedia" || env.URL != WikipediaURL {
		return sectorsFile{}, errors.New("sectors authority envelope is not canonical")
	}
	normalized := make(map[string]string, len(env.Sectors))
	for k, v := range env.Sectors {
		symbol := strings.ToUpper(strings.TrimSpace(k))
		sector := strings.TrimSpace(v)
		if symbol == "" || sector == "" || strings.ContainsAny(symbol+sector, "\r\n\t") {
			return sectorsFile{}, fmt.Errorf("invalid sector entry %q: %q", k, v)
		}
		if _, dup := normalized[symbol]; dup {
			return sectorsFile{}, fmt.Errorf("duplicate sector entry %q", symbol)
		}
		normalized[symbol] = sector
	}
	if n := len(normalized); n < MinMembers || n > MaxMembers {
		return sectorsFile{}, fmt.Errorf("invalid sectors count %d", n)
	}
	if env.Count != len(normalized) {
		return sectorsFile{}, errors.New("sectors authority envelope is not canonical")
	}
	env.Sectors = normalized
	return env, nil
}
