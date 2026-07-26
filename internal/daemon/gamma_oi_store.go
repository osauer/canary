package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	gammaOIPersistVersion  = 1
	gammaOIStateFilename   = "gamma-open-interest.json"
	gammaOIStateKind       = "gamma_open_interest.current.v1"
	gammaOIObservationKind = "gamma_open_interest.snapshot.v1"
	gammaOIAuthorityScope  = "market/gamma/open-interest"
	gammaOISource          = "ibkr.tws.generic_tick_101"
	// OI is published once per trading day and remains useful through the next
	// pre-market/open, including a normal weekend gap. Older observations are
	// too stale for an algo signal and must be treated as unknown.
	gammaOICarryMaxAge = 96 * time.Hour
)

type gammaOpenInterestStore struct {
	mu        sync.Mutex
	dir       string // sealed legacy cache; never used after UseCoreStore
	authority *corestore.Store
}

type gammaOIStateEnvelope struct {
	Version   int                      `json:"version"`
	UpdatedAt time.Time                `json:"updated_at"`
	Contracts map[string]gammaOIRecord `json:"contracts"`
}

type gammaOIRecord struct {
	Underlying    string    `json:"underlying"`
	TradingClass  string    `json:"trading_class"`
	Expiry        string    `json:"expiry"`
	ExpiryYMD     string    `json:"expiry_ymd"`
	Strike        float64   `json:"strike"`
	Right         string    `json:"right"`
	OpenInterest  int64     `json:"open_interest"`
	ObservedAt    time.Time `json:"observed_at"`
	SessionKey    string    `json:"session_key"`
	SourceStatus  string    `json:"source_status"`
	GatewaySource string    `json:"gateway_source,omitempty"`
}

func newGammaOpenInterestStore(dir string) *gammaOpenInterestStore {
	return &gammaOpenInterestStore{dir: dir}
}

func (s *gammaOpenInterestStore) UseCoreStore(store *corestore.Store) error {
	if s == nil {
		return errors.New("gamma OI: nil store")
	}
	if store == nil {
		return errors.New("gamma OI: nil corestore")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authority = store
	return nil
}

func gammaOIKey(underlying, tradingClass, expiryYMD string, strike float64, right string) string {
	return strings.ToUpper(strings.TrimSpace(underlying)) + "|" +
		strings.ToUpper(strings.TrimSpace(tradingClass)) + "|" +
		strings.TrimSpace(expiryYMD) + "|" +
		strconv.FormatFloat(strike, 'f', 6, 64) + "|" +
		strings.ToUpper(strings.TrimSpace(right))
}

func gammaOIRecordForLeg(underlying, tradingClass, expiryYMD string, strike float64, right string, oi int64, observedAt time.Time) gammaOIRecord {
	underlying = strings.ToUpper(strings.TrimSpace(underlying))
	tradingClass = strings.ToUpper(strings.TrimSpace(tradingClass))
	right = strings.ToUpper(strings.TrimSpace(right))
	return gammaOIRecord{
		Underlying:    underlying,
		TradingClass:  tradingClass,
		Expiry:        displayExpiry(expiryYMD),
		ExpiryYMD:     expiryYMD,
		Strike:        strike,
		Right:         right,
		OpenInterest:  oi,
		ObservedAt:    observedAt,
		SessionKey:    nySessionKey(observedAt),
		SourceStatus:  "live_observed",
		GatewaySource: "IBKR generic tick 101 openInterest",
	}
}

func (s *gammaOpenInterestStore) Load() (map[string]gammaOIRecord, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *gammaOpenInterestStore) loadLocked() (map[string]gammaOIRecord, error) {
	var data []byte
	if s.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(s.authority, gammaOIAuthorityScope, gammaOIStateKind)
		if err != nil {
			return nil, fmt.Errorf("read gamma OI authority: %w", err)
		}
		if !ok {
			return map[string]gammaOIRecord{}, nil
		}
	} else {
		path := filepath.Join(s.dir, gammaOIStateFilename)
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return map[string]gammaOIRecord{}, nil
			}
			return nil, fmt.Errorf("read legacy gamma OI state: %w", err)
		}
	}
	var env gammaOIStateEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode gamma OI state: %w", err)
	}
	if env.Version != gammaOIPersistVersion || env.Contracts == nil {
		return map[string]gammaOIRecord{}, nil
	}
	return env.Contracts, nil
}

func (s *gammaOpenInterestStore) SaveMerged(updates map[string]gammaOIRecord) error {
	if s == nil || len(updates) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadLocked()
	if err != nil {
		return err
	}
	if current == nil {
		current = make(map[string]gammaOIRecord, len(updates))
	}
	for key, rec := range updates {
		if rec.ObservedAt.IsZero() {
			continue
		}
		if old, ok := current[key]; ok && old.ObservedAt.After(rec.ObservedAt) {
			continue
		}
		current[key] = rec
	}
	now := time.Now().UTC()
	env := gammaOIStateEnvelope{
		Version:   gammaOIPersistVersion,
		UpdatedAt: now,
		Contracts: current,
	}
	if s.authority != nil {
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode gamma OI authority: %w", err)
		}
		metadata, err := json.Marshal(struct {
			Version       int       `json:"version"`
			UpdatedAt     time.Time `json:"updated_at"`
			ContractCount int       `json:"contract_count"`
			UpdateCount   int       `json:"update_count"`
			Method        string    `json:"method"`
		}{
			Version: gammaOIPersistVersion, UpdatedAt: now,
			ContractCount: len(current), UpdateCount: len(updates),
			Method: "IBKR generic tick 101 openInterest",
		})
		if err != nil {
			return fmt.Errorf("encode gamma OI metadata: %w", err)
		}
		return saveMarketState(s.authority, gammaOIAuthorityScope, gammaOIStateKind, corestore.ObservationInput{
			ScopeKey: gammaOIAuthorityScope, Source: gammaOISource,
			Kind: gammaOIObservationKind, ObservedAt: now,
			ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	return writeGammaAtomicJSON(s.dir, gammaOIStateFilename, env)
}

func validCarriedGammaOI(rec gammaOIRecord, now time.Time) bool {
	if rec.ObservedAt.IsZero() || now.IsZero() || now.Before(rec.ObservedAt) {
		return false
	}
	if now.Sub(rec.ObservedAt) > gammaOICarryMaxAge {
		return false
	}
	expiryYMD := strings.TrimSpace(rec.ExpiryYMD)
	if expiryYMD == "" {
		return false
	}
	loc := newYorkLocation()
	day, err := time.ParseInLocation("20060102", expiryYMD, loc)
	if err != nil {
		return false
	}
	settlement := classSettlementInstant(rec.TradingClass, day.Year(), day.Month(), day.Day(), loc).Add(classSettlementBuffer)
	return now.In(loc).Before(settlement)
}

func writeGammaAtomicJSON(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	target := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, name+".tmp.*")
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
	if err := enc.Encode(v); err != nil {
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
