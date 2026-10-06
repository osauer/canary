package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"
)

// A conID that IBKR stops defining does not come back: a relisting gets a new
// conID. The broker's verdict is therefore kept per conID across broker
// sessions and daemon restarts. Parking it for the session alone re-asked
// every delisted name in IBKR's short-stock file after each reconnect or
// restart, a WARN per name (52 lines over five sessions on 2026-10-06). The
// month bounds what a wrong verdict costs: research rows, never an order path.
const lendingContractVerdictMemory = 30 * 24 * time.Hour

// The short-stock file lists a few hundred dead names at most; the limit only
// bounds the document if that ever changes.
const lendingContractVerdictLimit = 4096

const (
	lendingContractVerdictScope = "lending-market"
	lendingContractVerdictKind  = "lending_contract_verdicts"
)

type lendingContractVerdictDoc struct {
	Version int `json:"version"`
	// Verdicts maps a conID to when IBKR last answered that it does not know it.
	Verdicts map[string]time.Time `json:"verdicts"`
}

// verdictUntil reports when a recorded verdict for conID lapses; the caller
// holds c.mu.
func (c *lendingMarketCache) verdictUntil(conID int, now time.Time) (time.Time, bool) {
	at, ok := c.verdicts[conID]
	if !ok || conID <= 0 {
		return time.Time{}, false
	}
	until := at.Add(lendingContractVerdictMemory)
	return until, now.Before(until)
}

// loadLendingContractVerdicts reads the recorded verdicts once. Without a
// store, or when the document cannot be read, the worker starts empty and the
// per-session parking still bounds the asks.
func (s *Server) loadLendingContractVerdicts() {
	cache := &s.lendingMarket
	cache.mu.Lock()
	loaded := cache.verdicts != nil
	cache.mu.Unlock()
	if loaded {
		return
	}
	verdicts := map[int]time.Time{}
	if s.coreStore != nil {
		raw, ok, err := loadMarketState(s.coreStore, lendingContractVerdictScope, lendingContractVerdictKind)
		var doc lendingContractVerdictDoc
		if err == nil && ok && (json.Unmarshal(raw, &doc) != nil || doc.Version != 1) {
			err = errors.New("unreadable document")
		}
		if err != nil && s.logger != nil {
			s.logger.Warnf("lending market: recorded contract verdicts unavailable (%v); unknown contracts are asked again once per broker session", err)
		}
		for key, at := range doc.Verdicts {
			if id, convErr := strconv.Atoi(key); convErr == nil && id > 0 {
				verdicts[id] = at
			}
		}
	}
	cache.mu.Lock()
	if cache.verdicts == nil {
		cache.verdicts = verdicts
	}
	cache.mu.Unlock()
}

// persistLendingContractVerdicts writes the live verdicts, dropping lapsed
// ones and, past the limit, the oldest.
func (s *Server) persistLendingContractVerdicts(ctx context.Context, now time.Time) {
	if s.coreStore == nil {
		return
	}
	cache := &s.lendingMarket
	cache.mu.Lock()
	maps.DeleteFunc(cache.verdicts, func(_ int, at time.Time) bool { return !now.Before(at.Add(lendingContractVerdictMemory)) })
	if extra := len(cache.verdicts) - lendingContractVerdictLimit; extra > 0 {
		ids := slices.SortedFunc(maps.Keys(cache.verdicts), func(a, b int) int { return cache.verdicts[a].Compare(cache.verdicts[b]) })
		for _, id := range ids[:extra] {
			delete(cache.verdicts, id)
		}
	}
	doc := lendingContractVerdictDoc{Version: 1, Verdicts: make(map[string]time.Time, len(cache.verdicts))}
	for id, at := range cache.verdicts {
		doc.Verdicts[strconv.Itoa(id)] = at.UTC()
	}
	cache.mu.Unlock()
	raw, err := json.Marshal(doc)
	if err == nil {
		err = saveMarketDocument(ctx, s.coreStore, lendingContractVerdictScope, lendingContractVerdictKind, raw)
	}
	if err != nil && s.logger != nil {
		s.logger.Warnf("lending market: recording contract verdicts failed: %v", err)
	}
}
