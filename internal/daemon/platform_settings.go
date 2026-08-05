package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

type platformSettingsStore struct {
	path     string // legacy importer/test helper only; live daemon binds core
	core     *corestore.Store
	revision int64
	mu       sync.RWMutex
	data     platformSettingsData
}

// Persisted shape versions of the platform-settings state document. Version 1
// spelled the portfolio-stress journal preference "canary"; version 2 spells it
// "stress". Runtime preferences live in daemon.db and change without a restart,
// so an installed daemon.db still holds a version-1 document: every decode goes
// through platformSettingsDocument.upgrade, which carries the stored value into
// the new spelling, and the next settings write persists it as version 2.
const (
	platformSettingsDocVersion       = 2
	platformSettingsDocVersionCanary = 1
)

type platformSettingsData struct {
	Version                  int                         `json:"version"`
	TradingControlGeneration uint64                      `json:"trading_control_generation"`
	Features                 platformFeatureSettingsData `json:"features"`
	Trading                  platformTradingSettingsData `json:"trading"`
	Regime                   platformRegimeSettingsData  `json:"regime"`
	Stress                   platformStressSettingsData  `json:"stress"`
	History                  platformHistorySettingsData `json:"history"`
}

// platformSettingsDocument is the decode shape of a persisted settings
// document: the current fields plus version 1's "canary" spelling of Stress.
// The legacy field lives here rather than on platformSettingsData so the stored
// shape never carries a transitional field — and so the strict cutover decoder
// still accepts a pre-rename file instead of rejecting "canary" as unknown.
type platformSettingsDocument struct {
	platformSettingsData
	LegacyCanary *platformStressSettingsData `json:"canary,omitempty"`
}

// platformSettingsDocumentV1 is the complete persisted version-1 shape.
// Keeping it separate from platformSettingsData is what makes the spelling
// boundary enforceable: a v1 document cannot smuggle in "stress", and a v2
// document cannot retain "canary".
type platformSettingsDocumentV1 struct {
	Version                  int                         `json:"version,omitempty"`
	TradingControlGeneration uint64                      `json:"trading_control_generation"`
	Features                 platformFeatureSettingsData `json:"features"`
	Trading                  platformTradingSettingsData `json:"trading"`
	Regime                   platformRegimeSettingsData  `json:"regime"`
	Canary                   *platformStressSettingsData `json:"canary,omitempty"`
	History                  platformHistorySettingsData `json:"history"`
}

type platformSettingsDataJSON platformSettingsData

func (d platformSettingsData) MarshalJSON() ([]byte, error) {
	switch d.Version {
	case 0, platformSettingsDocVersionCanary:
		legacy, err := d.legacyDocument(nil)
		if err != nil {
			return nil, err
		}
		return json.Marshal(legacy)
	case platformSettingsDocVersion:
		return json.Marshal(platformSettingsDataJSON(d))
	default:
		return nil, fmt.Errorf("unsupported platform settings version %d", d.Version)
	}
}

func (d platformSettingsDocument) MarshalJSON() ([]byte, error) {
	switch d.Version {
	case 0, platformSettingsDocVersionCanary:
		legacy, err := d.platformSettingsData.legacyDocument(d.LegacyCanary)
		if err != nil {
			return nil, err
		}
		return json.Marshal(legacy)
	case platformSettingsDocVersion:
		if d.LegacyCanary != nil {
			return nil, errors.New(`platform settings version 2 cannot encode legacy field "canary"`)
		}
		return json.Marshal(platformSettingsDataJSON(d.platformSettingsData))
	default:
		return nil, fmt.Errorf("unsupported platform settings version %d", d.Version)
	}
}

func (d platformSettingsData) legacyDocument(canary *platformStressSettingsData) (platformSettingsDocumentV1, error) {
	if d.Stress.Journal.Enabled != nil {
		return platformSettingsDocumentV1{}, errors.New(`platform settings version 1 cannot encode current field "stress"`)
	}
	return platformSettingsDocumentV1{
		Version:                  d.Version,
		TradingControlGeneration: d.TradingControlGeneration,
		Features:                 d.Features,
		Trading:                  d.Trading,
		Regime:                   d.Regime,
		Canary:                   canary,
		History:                  d.History,
	}, nil
}

// UnmarshalJSON keeps legacy-file cutover on the same version-specific decode
// contract as live daemon.db reads. Without this hook readOptionalJSON would
// strictly decode only against the transitional union above, which would still
// accept both spellings in one document.
func (d *platformSettingsDocument) UnmarshalJSON(raw []byte) error {
	doc, err := decodePlatformSettingsDocument(raw)
	if err != nil {
		return err
	}
	*d = doc
	return nil
}

// upgrade returns the runtime value for a decoded document. A document with no
// version at all predates versioning and is read as version 1, whose authority
// for the portfolio-stress journal preference is its "canary" object: that
// value is carried over verbatim, so an operator who disabled the journal
// before the rename stays disabled after it. An unknown version is refused
// rather than guessed.
func (d platformSettingsDocument) upgrade() (platformSettingsData, error) {
	data := d.platformSettingsData
	if data.Version == 0 {
		data.Version = platformSettingsDocVersionCanary
	}
	switch data.Version {
	case platformSettingsDocVersionCanary:
		if d.LegacyCanary != nil {
			data.Stress = *d.LegacyCanary
		}
		data.Version = platformSettingsDocVersion
	case platformSettingsDocVersion:
	default:
		return platformSettingsData{}, fmt.Errorf("unsupported version %d", data.Version)
	}
	return data, nil
}

func decodePlatformSettingsDocument(raw []byte) (platformSettingsDocument, error) {
	var fields map[string]json.RawMessage
	if err := decodeStrictPlatformSettingsJSON(raw, &fields); err != nil {
		return platformSettingsDocument{}, err
	}
	if fields == nil {
		return platformSettingsDocument{}, errors.New("platform settings document must be an object")
	}

	version := platformSettingsDocVersionCanary
	versionRaw, versioned := fields["version"]
	if versioned {
		if bytes.Equal(bytes.TrimSpace(versionRaw), []byte("null")) {
			return platformSettingsDocument{}, errors.New("platform settings version must be an integer")
		}
		if err := json.Unmarshal(versionRaw, &version); err != nil {
			return platformSettingsDocument{}, fmt.Errorf("decode platform settings version: %w", err)
		}
	}

	switch version {
	case platformSettingsDocVersionCanary:
		var legacy platformSettingsDocumentV1
		if err := decodeStrictPlatformSettingsJSON(raw, &legacy); err != nil {
			return platformSettingsDocument{}, err
		}
		if versioned && legacy.Version != platformSettingsDocVersionCanary {
			return platformSettingsDocument{}, fmt.Errorf("unsupported version %d", legacy.Version)
		}
		return platformSettingsDocument{
			platformSettingsData: platformSettingsData{
				Version:                  legacy.Version,
				TradingControlGeneration: legacy.TradingControlGeneration,
				Features:                 legacy.Features,
				Trading:                  legacy.Trading,
				Regime:                   legacy.Regime,
				History:                  legacy.History,
			},
			LegacyCanary: legacy.Canary,
		}, nil
	case platformSettingsDocVersion:
		var current platformSettingsData
		if err := decodeStrictPlatformSettingsJSON(raw, &current); err != nil {
			return platformSettingsDocument{}, err
		}
		if current.Version != platformSettingsDocVersion {
			return platformSettingsDocument{}, fmt.Errorf("unsupported version %d", current.Version)
		}
		return platformSettingsDocument{platformSettingsData: current}, nil
	default:
		return platformSettingsDocument{}, fmt.Errorf("unsupported version %d", version)
	}
}

func decodeStrictPlatformSettingsJSON(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("platform settings document contains trailing JSON")
		}
		return fmt.Errorf("platform settings document contains trailing JSON: %w", err)
	}
	return nil
}

// decodePlatformSettings decodes one persisted settings document and upgrades
// it to the current version.
func decodePlatformSettings(raw []byte) (platformSettingsData, error) {
	var doc platformSettingsDocument
	if err := doc.UnmarshalJSON(raw); err != nil {
		return platformSettingsData{}, err
	}
	return doc.upgrade()
}

type platformSettingsUpdateEventV1 struct {
	Version                     int                        `json:"version"`
	Keys                        []string                   `json:"keys"`
	Before                      map[string]json.RawMessage `json:"before"`
	After                       map[string]json.RawMessage `json:"after"`
	ExpectedRevision            int64                      `json:"expected_revision"`
	NewRevision                 int64                      `json:"new_revision"`
	OldTradingControlGeneration uint64                     `json:"old_trading_control_generation"`
	NewTradingControlGeneration uint64                     `json:"new_trading_control_generation"`
}

type platformRegimeSettingsData struct {
	Journal platformRegimeJournalSettingsData `json:"journal"`
}

type platformRegimeJournalSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformStressSettingsData struct {
	Journal platformStressJournalSettingsData `json:"journal"`
}

type platformStressJournalSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformHistorySettingsData struct {
	Rotation platformHistoryRotationSettingsData `json:"rotation"`
}

type platformHistoryRotationSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
	// KeepRawMonths >= 1; nil restores the built-in default of 2.
	KeepRawMonths *int `json:"keep_raw_months,omitempty"`
}

type platformFeatureSettingsData struct {
	PurgeRestore    platformPurgeRestoreSettingsData    `json:"purge_restore"`
	StockProtection platformStockProtectionSettingsData `json:"stock_protection"`
	Rulebook        platformRulebookSettingsData        `json:"rulebook"`
}

type platformRulebookSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
	// EarningsOverrides maps SYMBOL → "YYYY-MM-DD" or "YYYY-MM-DDTamc"/
	// "Tbmo"; overrides are authoritative over fetched dates (rules 6-8).
	EarningsOverrides map[string]string `json:"earnings_overrides,omitempty"`
}

type platformPurgeRestoreSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformStockProtectionSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformTradingSettingsData struct {
	MaxNotional           *float64 `json:"max_notional,omitempty"`
	MaxOptionContracts    *int     `json:"max_option_contracts,omitempty"`
	AllowStockShort       *bool    `json:"allow_stock_short,omitempty"`
	AllowOptionSellToOpen *bool    `json:"allow_option_sell_to_open,omitempty"`
	// Freeze is the runtime trading brake: true blocks every new broker
	// write (place/modify/purge/restore/proposals) via
	// brokerWriteAuthorization while cancels stay allowed. Unlike the
	// limits above it is not gated on tradingLimitWritability — a brake
	// must engage even when order entry is otherwise misconfigured.
	Freeze *bool `json:"freeze,omitempty"`
}

func defaultPlatformSettingsPath() (string, error) {
	return defaultTradingStatePath("platform-settings.json")
}

func newPlatformSettingsStore(path string) (*platformSettingsStore, error) {
	s := &platformSettingsStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// bindCore switches the live store to SQLite authority. The legacy path is
// deliberately ignored after binding; cutover imports happen before this
// method and runtime never falls back to the old file.
func (s *platformSettingsStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return fmt.Errorf("platform settings SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings)
	if err != nil {
		return fmt.Errorf("load platform settings from SQLite: %w", err)
	}
	if !ok {
		return fmt.Errorf("platform settings are missing from SQLite; cutover bootstrap was not completed")
	}
	// A stored version-1 document is upgraded in memory here and rewritten in the
	// new shape by the next settings write; the daemon reports the operator's
	// stored preference from the first read either way.
	data, err := decodePlatformSettings(doc.JSON)
	if err != nil {
		return fmt.Errorf("decode platform settings from SQLite: %w", err)
	}
	revision := doc.Revision
	s.mu.Lock()
	s.core = core
	s.revision = revision
	s.data = data
	s.mu.Unlock()
	return nil
}

func (s *platformSettingsStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data.Version = platformSettingsDocVersion
			return nil
		}
		return fmt.Errorf("read platform settings: %w", err)
	}
	data, err := decodePlatformSettings(raw)
	if err != nil {
		return fmt.Errorf("decode platform settings: %w", err)
	}
	s.data = data
	return nil
}

func (s *platformSettingsStore) snapshot() platformSettingsData {
	if s == nil {
		return platformSettingsData{Version: platformSettingsDocVersion}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// tradingControlGeneration returns the durable generation covering the five
// runtime controls that can change whether a broker write is permitted:
// freeze, both size caps, stock shorting, and option sell-to-open. It is a
// store property rather than an RPC revision so wire guards can compare one
// monotonic value without depending on publication timing at another surface.
func (s *platformSettingsStore) tradingControlGeneration() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.TradingControlGeneration
}

// tradingControlSnapshot applies the four configurable trading-limit
// overrides and returns them with the exact generation observed under the
// same lock. Freeze is intentionally not a config.Trading field, but every
// freeze change still advances the returned generation so a caller binding
// this pair cannot miss a concurrent brake transition.
func (s *platformSettingsStore) tradingControlSnapshot(base config.Trading, applyOverrides bool) (config.Trading, uint64) {
	if s == nil {
		return base, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if applyOverrides {
		if s.data.Trading.MaxNotional != nil {
			base.MaxNotional = *s.data.Trading.MaxNotional
		}
		if s.data.Trading.MaxOptionContracts != nil {
			base.MaxOptionContracts = *s.data.Trading.MaxOptionContracts
		}
		if s.data.Trading.AllowStockShort != nil {
			base.AllowStockShort = *s.data.Trading.AllowStockShort
		}
		if s.data.Trading.AllowOptionSellToOpen != nil {
			base.AllowOptionSellToOpen = *s.data.Trading.AllowOptionSellToOpen
		}
	}
	return base, s.data.TradingControlGeneration
}

// lockTradingControlSnapshot acquires the short final-wire read lease. The
// caller must invoke release after the protected broker send returns. Runtime
// settings remain fully concurrent while an order waits in pacing; only the
// final authority check plus frame write/flush excludes a control commit.
//
//lint:ignore U1000 Used by the trading-tagged physical write guard.
func (s *platformSettingsStore) lockTradingControlSnapshot(base config.Trading, applyOverrides bool) (cfg config.Trading, generation uint64, frozen bool, release func()) {
	if s == nil {
		return base, 0, false, func() {}
	}
	s.mu.RLock()
	if applyOverrides {
		if s.data.Trading.MaxNotional != nil {
			base.MaxNotional = *s.data.Trading.MaxNotional
		}
		if s.data.Trading.MaxOptionContracts != nil {
			base.MaxOptionContracts = *s.data.Trading.MaxOptionContracts
		}
		if s.data.Trading.AllowStockShort != nil {
			base.AllowStockShort = *s.data.Trading.AllowStockShort
		}
		if s.data.Trading.AllowOptionSellToOpen != nil {
			base.AllowOptionSellToOpen = *s.data.Trading.AllowOptionSellToOpen
		}
	}
	frozen = s.data.Trading.Freeze != nil && *s.data.Trading.Freeze
	return base, s.data.TradingControlGeneration, frozen, s.mu.RUnlock
}

func (s *platformSettingsStore) update(fn func(*platformSettingsData) error) error {
	if s == nil {
		return errBadRequest("runtime settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if next.Version == 0 {
		next.Version = platformSettingsDocVersion
	}
	if err := fn(&next); err != nil {
		return err
	}
	// The callback cannot assign or roll back this authority counter. Derive it
	// solely from the committed predecessor and the semantic before/after
	// values. A patch that changes several controls advances once; reapplying
	// an equal value does not advance merely because it allocated a new pointer.
	if err := deriveTradingControlGeneration(s.data, &next); err != nil {
		return err
	}
	if err := s.saveLocked(next); err != nil {
		return err
	}
	// Publish only after the authoritative write commits. A full/busy/corrupt
	// database therefore cannot relax an in-memory freeze or limit.
	s.data = next
	return nil
}

func (s *platformSettingsStore) updateWithAudit(ctx context.Context, at time.Time, origin string, requestedKeys []string, fn func(*platformSettingsData) error) error {
	if s == nil {
		return errBadRequest("runtime settings store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if next.Version == 0 {
		next.Version = platformSettingsDocVersion
	}
	if err := fn(&next); err != nil {
		return err
	}
	if err := deriveTradingControlGeneration(s.data, &next); err != nil {
		return err
	}
	keys := append([]string(nil), requestedKeys...)
	sort.Strings(keys)
	keys = slices.Compact(keys)
	before := make(map[string]json.RawMessage, len(keys))
	after := make(map[string]json.RawMessage, len(keys))
	changed := keys[:0]
	for _, key := range keys {
		oldValue, err := canonicalPlatformSettingValue(s.data, key)
		if err != nil {
			return err
		}
		newValue, err := canonicalPlatformSettingValue(next, key)
		if err != nil {
			return err
		}
		if bytes.Equal(oldValue, newValue) {
			continue
		}
		changed = append(changed, key)
		before[key] = oldValue
		after[key] = newValue
	}
	if len(changed) == 0 {
		return nil
	}
	if s.core == nil {
		if err := s.saveLocked(next); err != nil {
			return err
		}
		s.data = next
		return nil
	}
	rawState, err := json.Marshal(next)
	if err != nil {
		return err
	}
	event := platformSettingsUpdateEventV1{
		Version: 1, Keys: append([]string(nil), changed...), Before: before, After: after,
		ExpectedRevision: s.revision, NewRevision: s.revision + 1,
		OldTradingControlGeneration: s.data.TradingControlGeneration,
		NewTradingControlGeneration: next.TradingControlGeneration,
	}
	rawEvent, err := json.Marshal(event)
	if err != nil {
		return err
	}
	input := corestore.EventInput{
		ScopeKey:    daemonStateScope,
		EventKey:    coreEventKey(coreEventPlatformSettings, at, rawEvent, int(event.NewRevision)),
		Type:        coreEventPlatformSettings,
		Action:      coreEventActionUpdate,
		Origin:      normalizedWriteOrigin(origin),
		OccurredAt:  at,
		PayloadJSON: rawEvent,
	}
	saved, _, err := s.core.CompareAndSwapStateDocumentWithEvents(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings,
		ExpectedRevision: s.revision, JSON: rawState,
	}, []corestore.EventInput{input})
	if err != nil {
		return fmt.Errorf("persist platform settings and audit event in SQLite: %w", err)
	}
	s.revision = saved.Revision
	s.data = next
	return nil
}

func deriveTradingControlGeneration(current platformSettingsData, next *platformSettingsData) error {
	next.TradingControlGeneration = current.TradingControlGeneration
	if sameTradingControls(current.Trading, next.Trading) {
		return nil
	}
	if next.TradingControlGeneration == math.MaxUint64 {
		return fmt.Errorf("platform trading-control generation is exhausted")
	}
	next.TradingControlGeneration++
	return nil
}

func canonicalPlatformSettingValue(data platformSettingsData, key string) (json.RawMessage, error) {
	var value any
	switch key {
	case "features.purge_restore.enabled":
		value = data.Features.PurgeRestore.Enabled
	case "features.stock_protection.enabled":
		value = data.Features.StockProtection.Enabled
	case "features.rulebook.enabled":
		value = data.Features.Rulebook.Enabled
	case "features.rulebook.earnings_overrides":
		value = data.Features.Rulebook.EarningsOverrides
	case "trading.freeze":
		value = data.Trading.Freeze
	case "trading.limits.max_notional":
		value = data.Trading.MaxNotional
	case "trading.limits.max_option_contracts":
		value = data.Trading.MaxOptionContracts
	case "trading.limits.allow_stock_short":
		value = data.Trading.AllowStockShort
	case "trading.limits.allow_option_sell_to_open":
		value = data.Trading.AllowOptionSellToOpen
	case "regime.journal.enabled":
		value = data.Regime.Journal.Enabled
	case "stress.journal.enabled":
		value = data.Stress.Journal.Enabled
	default:
		return nil, errBadRequest("unknown settings field " + key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func sameTradingControls(a, b platformTradingSettingsData) bool {
	return sameOptionalFloat64(a.MaxNotional, b.MaxNotional) &&
		sameOptionalInt(a.MaxOptionContracts, b.MaxOptionContracts) &&
		sameOptionalBool(a.AllowStockShort, b.AllowStockShort) &&
		sameOptionalBool(a.AllowOptionSellToOpen, b.AllowOptionSellToOpen) &&
		sameOptionalBool(a.Freeze, b.Freeze)
}

func sameOptionalBool(a, b *bool) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameOptionalInt(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameOptionalFloat64(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (s *platformSettingsStore) saveLocked(next platformSettingsData) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if s.core != nil {
		saved, err := s.core.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings,
			ExpectedRevision: s.revision, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("persist platform settings in SQLite: %w", err)
		}
		s.revision = saved.Revision
		return nil
	}
	// This branch exists only for legacy importer/unit-test constructors.
	pretty, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateStateAtomic(s.path, append(pretty, '\n'))
}

func (s *Server) handleSettingsGet() (*rpc.PlatformSettings, error) {
	health := s.handleStatusHealth()
	out := s.platformSettingsSnapshot(&platformSettingsObserved{
		DataQuality: health.DataQuality,
		ObservedAt:  s.orderNow(),
	})
	return &out, nil
}

func (s *Server) handleSettingsUpdate(ctx context.Context, req *rpc.Request) (*rpc.PlatformSettings, error) {
	if len(bytes.TrimSpace(req.Params)) == 0 {
		return nil, errBadRequest("settings patch body is required")
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &patch); err != nil {
		return nil, errBadRequest("decode settings patch: " + err.Error())
	}
	origin := ""
	if raw, ok := patch["origin"]; ok {
		delete(patch, "origin")
		if err := json.Unmarshal(raw, &origin); err != nil {
			return nil, errBadRequest("decode settings origin: " + err.Error())
		}
	}
	if len(patch) == 0 {
		health := s.handleStatusHealth()
		out := s.platformSettingsSnapshot(&platformSettingsObserved{
			DataQuality: health.DataQuality,
			ObservedAt:  s.orderNow(),
		})
		return &out, nil
	}
	if err := s.applyPlatformSettingsPatch(ctx, patch, origin); err != nil {
		return nil, err
	}
	health := s.handleStatusHealth()
	out := s.platformSettingsSnapshot(&platformSettingsObserved{
		DataQuality: health.DataQuality,
		ObservedAt:  s.orderNow(),
	})
	return &out, nil
}

func (s *Server) applyPlatformSettingsPatch(ctx context.Context, patch map[string]json.RawMessage, origin string) error {
	flat, err := flattenSettingsPatch(patch)
	if err != nil {
		return err
	}
	specs := settingsSpecsByKey()
	for key := range flat {
		// Freeze and limit authority is terminal-only in every mode. A paired
		// device is a human broker-write origin, but it is not authorized to
		// change the controls that govern later writes.
		if strings.HasPrefix(key, "trading.") && !settingsTradingOriginAuthorized(origin) {
			return errBadRequest("trading safety settings are terminal-only; use an interactive human terminal to change freeze or limits")
		}
	}
	limitsWritable, reason := s.tradingLimitWritability()
	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	return s.platformSettings.updateWithAudit(ctx, s.orderNow(), origin, keys, func(next *platformSettingsData) error {
		for key, raw := range flat {
			if specs[key].Class == rpc.SettingsClassTradingLimit && !limitsWritable {
				return errBadRequest("trading.limits is read-only: " + reason)
			}
			if err := applySettingsKey(next, key, raw); err != nil {
				return err
			}
		}
		return nil
	})
}

func settingsTradingOriginAuthorized(origin string) bool {
	return origin == rpc.OrderOriginHumanTTY
}

func settingsSpecsByKey() map[string]rpc.SettingsKeySpec {
	out := map[string]rpc.SettingsKeySpec{}
	for _, spec := range rpc.SettingsKeys() {
		out[spec.Key] = spec
	}
	return out
}

// flattenSettingsPatch walks the nested patch object and returns raw values
// keyed by dotted registry key. Descent stops at registry keys (the
// earnings-overrides map value is a terminal object), so registry keys are
// the only writable paths and everything else fails with a targeted error.
func flattenSettingsPatch(patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	specs := settingsSpecsByKey()
	prefixes := map[string]bool{}
	for key := range specs {
		parts := strings.Split(key, ".")
		for i := 1; i < len(parts); i++ {
			prefixes[strings.Join(parts[:i], ".")] = true
		}
	}
	flat := map[string]json.RawMessage{}
	var walk func(prefix string, obj map[string]json.RawMessage) error
	walk = func(prefix string, obj map[string]json.RawMessage) error {
		for key, raw := range obj {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if _, ok := specs[path]; ok {
				flat[path] = raw
				continue
			}
			if !prefixes[path] {
				if prefix == "trading" {
					// [trading] carries real read-only observed fields
					// (mode, account, ...); name the distinction.
					return errBadRequest("settings field " + path + " is read-only")
				}
				return errBadRequest("unknown settings field " + path)
			}
			var child map[string]json.RawMessage
			if err := json.Unmarshal(raw, &child); err != nil {
				return errBadRequest(path + " must be an object")
			}
			if err := walk(path, child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", patch); err != nil {
		return nil, err
	}
	return flat, nil
}

// applySettingsKey writes one flattened registry key into the store data.
// Every key in the rpc settings registry must have a case here; the
// registry parity test enforces that, so registry and store cannot drift.
func applySettingsKey(next *platformSettingsData, key string, raw json.RawMessage) error {
	boolField := func(dst **bool) error {
		v, err := nullableBool(raw)
		if err != nil {
			return errBadRequest(key + " must be true, false, or null")
		}
		*dst = v
		return nil
	}
	switch key {
	case "features.purge_restore.enabled":
		return boolField(&next.Features.PurgeRestore.Enabled)
	case "features.stock_protection.enabled":
		return boolField(&next.Features.StockProtection.Enabled)
	case "features.rulebook.enabled":
		return boolField(&next.Features.Rulebook.Enabled)
	case "features.rulebook.earnings_overrides":
		v, err := mergeEarningsOverrides(next.Features.Rulebook.EarningsOverrides, raw)
		if err != nil {
			return err
		}
		next.Features.Rulebook.EarningsOverrides = v
		return nil
	case "trading.freeze":
		return boolField(&next.Trading.Freeze)
	case "trading.limits.max_notional":
		v, err := nullableFloat(raw)
		if err != nil || (v != nil && *v <= 0) {
			return errBadRequest("trading.limits.max_notional must be a positive number or null")
		}
		next.Trading.MaxNotional = v
		return nil
	case "trading.limits.max_option_contracts":
		v, err := nullableInt(raw)
		if err != nil || (v != nil && *v <= 0) {
			return errBadRequest("trading.limits.max_option_contracts must be a positive integer or null")
		}
		next.Trading.MaxOptionContracts = v
		return nil
	case "trading.limits.allow_stock_short":
		return boolField(&next.Trading.AllowStockShort)
	case "trading.limits.allow_option_sell_to_open":
		return boolField(&next.Trading.AllowOptionSellToOpen)
	case "regime.journal.enabled":
		return boolField(&next.Regime.Journal.Enabled)
	case "stress.journal.enabled":
		return boolField(&next.Stress.Journal.Enabled)
	default:
		return errBadRequest("unknown settings field " + key)
	}
}

// mergeEarningsOverrides applies a SYMBOL → date patch onto the current
// overrides: null clears all overrides, a null value clears that one symbol,
// and mentioned symbols upsert — unmentioned symbols survive, so a per-symbol
// `settings set` cannot silently drop the rest of the map. Dates must be
// "YYYY-MM-DD" optionally suffixed with "Tamc"/"Tbmo" — a bad override must
// fail loudly here, not silently skew rules 6-8 later. cur is cloned, never
// mutated, so a failed patch leaves the store snapshot intact.
func mergeEarningsOverrides(cur map[string]string, raw json.RawMessage) (map[string]string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var m map[string]*string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errBadRequest("features.rulebook.earnings_overrides must be an object of SYMBOL to date strings or null")
	}
	out := maps.Clone(cur)
	if out == nil {
		out = map[string]string{}
	}
	for sym, v := range m {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return nil, errBadRequest("features.rulebook.earnings_overrides keys must be symbols")
		}
		if v == nil {
			delete(out, sym)
			continue
		}
		date, half, hasHalf := strings.Cut(*v, "T")
		if hasHalf {
			h := strings.ToLower(half)
			if h != "amc" && h != "bmo" {
				return nil, errBadRequest("features.rulebook.earnings_overrides." + sym + " suffix must be Tamc or Tbmo")
			}
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return nil, errBadRequest("features.rulebook.earnings_overrides." + sym + " must be YYYY-MM-DD with optional Tamc/Tbmo suffix")
		}
		out[sym] = *v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func nullableBool(raw json.RawMessage) (*bool, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func nullableFloat(raw json.RawMessage) (*float64, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func nullableInt(raw json.RawMessage) (*int, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Server) platformSettingsSnapshot(observed *platformSettingsObserved) rpc.PlatformSettings {
	data := s.platformSettings.snapshot()
	trading := s.effectiveTradingConfig()
	autoTrade := config.AutoTrade{}.WithDefaults()
	if s.cfg != nil {
		autoTrade = s.cfg.AutoTrade.WithDefaults()
	}
	status := s.currentTradingStatus()
	limitsWritable, limitReason := s.tradingLimitWritability()
	purgeEnabled := true
	purgeSource := rpc.SettingsSourceRuntime
	if data.Features.PurgeRestore.Enabled != nil {
		purgeEnabled = *data.Features.PurgeRestore.Enabled
	}
	stockProtectionEnabled := true
	if data.Features.StockProtection.Enabled != nil {
		stockProtectionEnabled = *data.Features.StockProtection.Enabled
	}
	rulebookEnabled := true
	if data.Features.Rulebook.Enabled != nil {
		rulebookEnabled = *data.Features.Rulebook.Enabled
	}
	limitAccess := rpc.SettingsAccessRead
	if limitsWritable {
		limitAccess = rpc.SettingsAccessWrite
		limitReason = ""
	}
	limitSource := func(runtime bool) string {
		if runtime && limitsWritable {
			return rpc.SettingsSourceRuntime
		}
		return rpc.SettingsSourceConfig
	}
	out := rpc.PlatformSettings{
		Kind: "ibkr.platform_settings",
		Features: rpc.PlatformFeatureSettings{
			PurgeRestore: rpc.PurgeRestoreSettings{
				Enabled: rpc.SettingsBool{Value: purgeEnabled, Access: rpc.SettingsAccessWrite, Source: purgeSource},
			},
			StockProtection: rpc.StockProtectionSettings{
				Enabled: rpc.SettingsBool{Value: stockProtectionEnabled, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime},
			},
			Rulebook: rpc.RulebookSettings{
				Enabled: rpc.SettingsBool{Value: rulebookEnabled, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime},
				EarningsOverrides: rpc.SettingsStringMap{Value: data.Features.Rulebook.EarningsOverrides,
					Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime,
					Reason: "SYMBOL to YYYY-MM-DD (optional Tamc/Tbmo); authoritative over fetched earnings for rules 6-8"},
			},
		},
		Trading: rpc.PlatformTradingSettings{
			Freeze:               settingsBool(s.tradingFrozen(), rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "runtime brake: true blocks new broker writes; cancels stay allowed"),
			Mode:                 settingsString(status.Mode, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, `set [trading].mode in config.toml to "disabled", "paper", or "live"`),
			Account:              settingsString(status.Account, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [gateway].account in config.toml"),
			Endpoint:             settingsString(status.Endpoint, rpc.SettingsAccessRead, rpc.SettingsSourceObserved, "observed from daemon gateway discovery/config"),
			ClientID:             settingsInt(status.ClientID, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [gateway].client_id in config.toml"),
			MCPTrading:           settingsString(status.MCPTrading, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "MCP broker-write controls are not exposed"),
			LiveOverride:         settingsString(status.LiveOverride, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, `computed from [trading].mode and active blockers; "ready" only on an unblocked live route`),
			BuildWritesAvailable: settingsBool(orderWritesAvailable, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the Canary build"),
			Limits: rpc.TradingLimitSettings{
				MaxNotional:           settingsFloat(trading.MaxNotional, limitAccess, limitSource(data.Trading.MaxNotional != nil), limitReason),
				MaxOptionContracts:    settingsInt(trading.MaxOptionContracts, limitAccess, limitSource(data.Trading.MaxOptionContracts != nil), limitReason),
				AllowStockShort:       settingsBool(trading.AllowStockShort, limitAccess, limitSource(data.Trading.AllowStockShort != nil), limitReason),
				AllowOptionSellToOpen: settingsBool(trading.AllowOptionSellToOpen, limitAccess, limitSource(data.Trading.AllowOptionSellToOpen != nil), limitReason),
			},
		},
		AutoTrade: rpc.PlatformAutoTradeSettings{
			ProposalsEnabled: settingsBool(autoTrade.ProposalsEnabledResolved(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].proposals_enabled in config.toml"),
			FastPathEnabled:  settingsBool(autoTrade.FastPathEnabledResolved(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].fast_path_enabled in config.toml"),
			PolicyFile:       settingsString(autoTrade.PolicyFile, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].policy_file in config.toml"),
			HotReload:        settingsBool(autoTrade.HotReloadEnabled(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].hot_reload in config.toml"),
			ReloadInterval:   settingsString(autoTrade.ReloadIntervalDuration().String(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].reload_interval in config.toml"),
			ProposalCadence:  settingsString(autoTrade.ProposalCadenceDuration().String(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].proposal_cadence in config.toml"),
		},
		Regime: rpc.PlatformRegimeSettings{
			Journal: rpc.RegimeJournalSettings{
				Enabled: settingsBool(regimeJournalEnabledFrom(data), rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "forward regime decision-event collection in daemon.db (calibration corpus); safe to disable"),
			},
		},
		Stress: rpc.PlatformStressSettings{
			Journal: rpc.StressJournalSettings{
				Enabled: settingsBool(stressJournalEnabledFrom(data), rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "forward portfolio-stress decision-event collection in daemon.db (calibration corpus); safe to disable"),
			},
		},
		History: rpc.PlatformHistorySettings{
			Rotation: rpc.HistoryRotationSettings{
				Enabled:       settingsBool(false, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "retired: daemon.db stores decision events directly and has no journal-rotation layer"),
				KeepRawMonths: settingsInt(0, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "retired with the legacy JSONL history-index rotation layer"),
			},
		},
		MarketData: rpc.PlatformMarketDataSetting{Quality: observedMarketDataQuality(observed)},
		Build: rpc.PlatformBuildSettings{
			Channel:                settingsString(buildChannel(), rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the Canary build"),
			TradingWritesAvailable: settingsBool(orderWritesAvailable, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the Canary build"),
		},
		AsOf: s.orderNow(),
	}
	if orderWritesAvailable {
		out.Build.ExperimentalTradingNote = "experimental trading build; runtime limit overrides are writable only when [trading].mode is paper or live"
	} else {
		out.Build.ExperimentalTradingNote = "stable read-only build; trading limit edits are disabled"
	}
	return out
}

func regimeJournalEnabledFrom(data platformSettingsData) bool {
	if data.Regime.Journal.Enabled == nil {
		return false
	}
	return *data.Regime.Journal.Enabled
}

func stressJournalEnabledFrom(data platformSettingsData) bool {
	if data.Stress.Journal.Enabled == nil {
		return false
	}
	return *data.Stress.Journal.Enabled
}

func buildChannel() string {
	if orderWritesAvailable {
		return "experimental-trading"
	}
	return "stable"
}

func settingsBool(value bool, access, source, reason string) rpc.SettingsBool {
	return rpc.SettingsBool{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsFloat(value float64, access, source, reason string) rpc.SettingsFloat {
	return rpc.SettingsFloat{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsInt(value int, access, source, reason string) rpc.SettingsInt {
	return rpc.SettingsInt{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsString(value, access, source, reason string) rpc.SettingsString {
	return rpc.SettingsString{Value: value, Access: access, Source: source, Reason: reason}
}

func (s *Server) tradingLimitWritability() (bool, string) {
	if !orderWritesAvailable {
		return false, "stable build exposes trading limits as read-only"
	}
	tr := config.Trading{}.WithDefaults()
	if s != nil && s.cfg != nil {
		tr = s.cfg.Trading.WithDefaults()
	}
	if !tr.OrderEntryEnabled() {
		return false, `set [trading].mode to "paper" or "live" before editing runtime safety limits`
	}
	return true, ""
}

func (s *Server) effectiveTradingControlSnapshot() (config.Trading, uint64) {
	tr := config.Trading{}.WithDefaults()
	if s == nil {
		return tr, 0
	}
	if s.cfg != nil {
		tr = s.cfg.Trading.WithDefaults()
	}
	writable, _ := s.tradingLimitWritability()
	return s.platformSettings.tradingControlSnapshot(tr, writable)
}

//lint:ignore U1000 Used by the trading-tagged physical write guard.
func (s *Server) lockEffectiveTradingControlSnapshot() (config.Trading, uint64, bool, func()) {
	tr := config.Trading{}.WithDefaults()
	if s == nil {
		return tr, 0, false, func() {}
	}
	if s.cfg != nil {
		tr = s.cfg.Trading.WithDefaults()
	}
	writable, _ := s.tradingLimitWritability()
	return s.platformSettings.lockTradingControlSnapshot(tr, writable)
}

func (s *Server) effectiveTradingConfig() config.Trading {
	tr, _ := s.effectiveTradingControlSnapshot()
	return tr
}

func (s *Server) purgeRestoreEnabled() bool {
	data := s.platformSettings.snapshot()
	if data.Features.PurgeRestore.Enabled == nil {
		return true
	}
	return *data.Features.PurgeRestore.Enabled
}

func (s *Server) stockProtectionEnabled() bool {
	data := s.platformSettings.snapshot()
	if data.Features.StockProtection.Enabled == nil {
		return true
	}
	return *data.Features.StockProtection.Enabled
}

// tradingFrozen reports the runtime trading brake. Default (unset/null) is
// not frozen; only an explicit trading.freeze=true engages it.
func (s *Server) tradingFrozen() bool {
	if s == nil {
		return false
	}
	data := s.platformSettings.snapshot()
	return data.Trading.Freeze != nil && *data.Trading.Freeze
}

type platformSettingsObserved struct {
	Quotes      map[string]rpc.Quote
	DataQuality []rpc.DataQualityHealth
	ObservedAt  time.Time
}

func observedMarketDataQuality(observed *platformSettingsObserved) rpc.PlatformMarketDataQuality {
	out := rpc.PlatformMarketDataQuality{
		Status:  "unknown",
		Summary: "no observed market-data snapshot yet",
		Access:  rpc.SettingsAccessRead,
		Source:  rpc.SettingsSourceObserved,
		Reason:  "observed from quote, position, chain, and status surfaces; entitlements are never stored",
	}
	if observed == nil {
		return out
	}
	out.DataQuality = append([]rpc.DataQualityHealth(nil), observed.DataQuality...)
	out.ObservedAt = observed.ObservedAt
	counts := map[string]int{}
	for _, q := range observed.Quotes {
		key := strings.TrimSpace(q.DataType)
		if key == "" {
			key = rpc.MarketDataLive
		}
		counts[key]++
	}
	if len(counts) > 0 {
		out.QuoteCounts = counts
	}
	switch {
	case len(out.DataQuality) > 0:
		out.Status = "degraded"
		out.Summary = "observed decision surfaces report degraded data quality"
	case len(counts) == 0:
		out.Status = "unknown"
		out.Summary = "no quote feed state observed yet"
	case counts[rpc.MarketDataDelayed] > 0 || counts[rpc.MarketDataDelayedFrozen] > 0:
		out.Status = "delayed"
		out.Summary = "one or more observed quotes are delayed"
	default:
		out.Status = "ok"
		out.Summary = "observed quotes look live or usable"
	}
	return out
}
