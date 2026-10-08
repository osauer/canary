package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The connector keeps a warned entitlement gap (code 354) at INFO for a day
// and hands that memory to its successor on a reconnect. A daemon restart
// dropped it, so every start warned once more per missing subscription: NDX
// after each of six starts on 2026-10-06. The daemon records the gaps when a
// connector retires and seeds the first connector of the next process.
//
// Owner decision (Oliver, 2026-10-08 07:15 CEST): a code-354 "market data not
// subscribed" notice warns once per subscription key per 24 h; repeats inside
// the window log at INFO, remembered across reconnects and daemon restarts.
// This replaces the 2026-10-03 rule that warned again after every daemon
// start. Documents written before then may hold one exact-session key per
// order preview ("…|EXACT:<seq>"); loading merges them into one gap per
// instrument (MarketDataMemory.WithEntitlementGaps normalises the keys).
const (
	entitlementGapScope = "market-data"
	entitlementGapKind  = "entitlement_gaps"
)

type entitlementGapDoc struct {
	Version int `json:"version"`
	// Gaps maps a subscription key to when its gap first warned.
	Gaps map[string]time.Time `json:"gaps"`
}

// loadEntitlementGaps seeds s.marketDataMemory from the store once per
// process. A missing or unreadable document starts empty: the cost is one
// warning per gap, never a lost quote, since the connector re-probes either way.
func (s *Server) loadEntitlementGaps() {
	s.mu.Lock()
	loaded := s.entitlementGapsLoaded
	s.entitlementGapsLoaded = true
	s.mu.Unlock()
	if loaded || s.coreStore == nil {
		return
	}
	raw, ok, err := loadMarketState(s.coreStore, entitlementGapScope, entitlementGapKind)
	var doc entitlementGapDoc
	if err == nil && ok && (json.Unmarshal(raw, &doc) != nil || doc.Version != 1) {
		err = errors.New("unreadable document")
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Warnf("market data: recorded entitlement gaps unavailable (%v); each gap warns once more", err)
		}
		return
	}
	if len(doc.Gaps) == 0 {
		return
	}
	s.mu.Lock()
	s.marketDataMemory = s.marketDataMemory.WithEntitlementGaps(doc.Gaps)
	s.mu.Unlock()
}

// persistEntitlementGaps records a retiring connector's live gaps; the export
// has already dropped lapsed ones.
func (s *Server) persistEntitlementGaps(memory ibkrlib.MarketDataMemory) {
	if s.coreStore == nil {
		return
	}
	doc := entitlementGapDoc{Version: 1, Gaps: memory.EntitlementGaps()}
	if doc.Gaps == nil {
		doc.Gaps = map[string]time.Time{}
	}
	for key, at := range doc.Gaps {
		doc.Gaps[key] = at.UTC()
	}
	raw, err := json.Marshal(doc)
	if err == nil {
		err = saveMarketDocument(context.Background(), s.coreStore, entitlementGapScope, entitlementGapKind, raw)
	}
	if err != nil && s.logger != nil {
		s.logger.Warnf("market data: recording entitlement gaps failed: %v", err)
	}
}
