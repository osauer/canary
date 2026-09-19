package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type marketHistoryInterest struct {
	Params   rpc.MarketHistoryParams
	Until    time.Time
	RetryAt  time.Time
	Failures int
}

func (s *Server) rememberMarketHistory(p rpc.MarketHistoryParams) {
	key, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		return
	}
	s.marketData.mu.Lock()
	defer s.marketData.mu.Unlock()
	if s.marketData.interest == nil {
		s.marketData.interest = make(map[string]marketHistoryInterest)
	}
	now := time.Now()
	for key, item := range s.marketData.interest {
		if now.After(item.Until) {
			delete(s.marketData.interest, key)
		}
	}
	item, exists := s.marketData.interest[key]
	if !exists && len(s.marketData.interest) >= 64 {
		return
	}
	if !exists {
		item.Params = normalized
	}
	// The widest observed daily request covers shorter selections too.
	oldDays, _, _ := marketHistoryWindow(item.Params.Range, now)
	newDays, _, _ := marketHistoryWindow(p.Range, now)
	if newDays > oldDays {
		item.Params = normalized
	}
	item.Until = now.Add(24 * time.Hour)
	s.marketData.interest[key] = item
}

// marketHistoryRefreshWindow bounds one background refresh. IBKR paces
// historical reads at sixty per ten minutes for the whole connector and the
// daemon's own closed-market quote context keeps that bucket near empty, so a
// refresh routinely waits behind several ten-second token refills before its
// bars arrive within a second. The window outlasts that queue; the limiter's
// background lane keeps the worker behind at most its small pool ahead of any
// interactive request, so waiting here delays nobody.
const marketHistoryRefreshWindow = 4 * time.Minute

func (s *Server) startMarketHistoryRefresh(ctx context.Context) {
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s.marketData.mu.Lock()
			keys := make([]string, 0, len(s.marketData.interest))
			for key := range s.marketData.interest {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			s.marketData.mu.Unlock()
			for _, key := range keys {
				if ctx.Err() != nil {
					return
				}
				s.refreshMarketHistoryInterest(ctx, key, s.marketHistoryRequest)
			}
		}
	})
}

// refreshMarketHistoryInterest reads one remembered series through the same
// bounded request path, on the background pacing lane and within the refresh
// window, without extending interest merely because this reader ran. A
// failed or refused refresh backs the key off (thirty seconds doubling to
// fifteen minutes); a later success clears the streak and says so.
func (s *Server) refreshMarketHistoryInterest(ctx context.Context, key string, request func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error)) {
	s.marketData.mu.Lock()
	item, ok := s.marketData.interest[key]
	s.marketData.mu.Unlock()
	if !ok || time.Now().After(item.Until) || time.Now().Before(item.RetryAt) {
		return
	}
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), marketHistoryRefreshWindow)
	result, err := request(readCtx, item.Params)
	cancel()
	s.marketData.mu.Lock()
	current := s.marketData.interest[key]
	failed := err != nil || result != nil && result.Cache != nil && result.Cache.RefreshFailed
	recovered := !failed && current.Failures > 0
	if failed {
		current.Failures = min(current.Failures+1, 5)
		current.RetryAt = time.Now().Add(min(15*time.Minute, time.Duration(1<<current.Failures)*30*time.Second))
	} else {
		current.Failures = 0
		current.RetryAt = time.Time{}
	}
	s.marketData.interest[key] = current
	s.marketData.mu.Unlock()
	if s.logger == nil {
		return
	}
	if err != nil {
		s.logger.Warnf("market history refresh %s %s: %v; next attempt after %s", item.Params.Contract.Symbol, item.Params.Range, err, current.RetryAt.Format(time.TimeOnly))
	} else if recovered {
		s.logger.Infof("market history refresh %s %s: recovered", item.Params.Contract.Symbol, item.Params.Range)
	}
}
