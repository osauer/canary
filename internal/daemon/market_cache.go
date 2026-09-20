package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type marketDataCache struct {
	mu       sync.Mutex
	storeMu  sync.Mutex
	loopWG   sync.WaitGroup
	interest map[string]marketHistoryInterest
	// definitionMisses holds the broker's "no security definition" verdict
	// by normalised history contract, so every remembered range of that
	// contract pauses together; see rememberMarketHistoryDefinitionMiss.
	definitionMisses map[rpc.ContractParams]marketHistoryDefinitionMiss
	history          map[string]*marketHistoryEntry
	slots            chan struct{}
}
type marketHistoryEntry struct {
	done    chan struct{}
	expires time.Time
	result  *rpc.MarketHistoryResult
	err     error
}

func (s *Server) handleMarketHistory(ctx context.Context, req *rpc.Request) (*rpc.MarketHistoryResult, error) {
	var p rpc.MarketHistoryParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.rememberMarketHistory(p)
	key, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		return nil, err
	}
	// A known series renders immediately, including during a broker outage.
	// The joined daemon worker refreshes interest independently of browser life.
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if saved, storedAt, err := s.loadMarketHistory(ctx, key); err != nil {
		return nil, err
	} else if saved != nil {
		return selectStoredHistory(saved, storedAt, normalized, now, "cache", "", false), nil
	}
	return s.marketHistoryRequest(ctx, p)
}

func (s *Server) marketHistoryRequest(ctx context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
	if _, _, err := marketHistoryWindow(p.Range, time.Now()); err != nil {
		return nil, err
	}
	key, p, err := marketHistoryIdentity(p)
	if err != nil {
		return nil, err
	}
	requestBytes, _ := json.Marshal(p)
	requestKey := string(requestBytes)
	cache := &s.marketData
	cache.mu.Lock()
	if cache.history == nil {
		cache.history = map[string]*marketHistoryEntry{}
		cache.slots = make(chan struct{}, 2)
	}
	for k, e := range cache.history {
		select {
		case <-e.done:
			if time.Now().After(e.expires) {
				delete(cache.history, k)
			}
		default:
		}
	}
	if e := cache.history[requestKey]; e != nil {
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.done:
			return e.result, e.err
		}
	}
	if len(cache.history) >= 64 {
		cache.mu.Unlock()
		return nil, errors.New("history capacity reached; retry later")
	}
	e := &marketHistoryEntry{done: make(chan struct{})}
	cache.history[requestKey] = e
	slots := cache.slots
	cache.mu.Unlock()
	select {
	case slots <- struct{}{}:
		e.result, e.err = s.readRetainedHistory(ctx, key, p, time.Now(), s.fetchMarketHistoryDays)
		<-slots
	case <-ctx.Done():
		e.err = ctx.Err()
	}
	cache.mu.Lock()
	e.expires = time.Now().Add(time.Minute)
	if e.err != nil || e.result != nil && e.result.Cache != nil && e.result.Cache.RefreshFailed {
		e.expires = time.Now().Add(15 * time.Second)
	}
	close(e.done)
	cache.mu.Unlock()
	return e.result, e.err
}
