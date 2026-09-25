package daemon

import (
	"context"
	"errors"
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

// marketHistoryRefreshTick is how often the worker looks for due series.
const marketHistoryRefreshTick = 30 * time.Second

// marketHistoryRefreshGrace is the worker's own cycle: the wait for its next
// tick plus the longest refresh it allows. A record that falls due is read
// within it, so only a record behind by more is served as refresh due.
const marketHistoryRefreshGrace = marketHistoryRefreshTick + marketHistoryRefreshWindow

func (s *Server) startMarketHistoryRefresh(ctx context.Context) {
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(marketHistoryRefreshTick)
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
// fifteen minutes); a later success clears the streak and says so. A series
// whose contract the broker has said it cannot define is not read at all
// while that verdict holds; see rememberMarketHistoryDefinitionMiss.
func (s *Server) refreshMarketHistoryInterest(ctx context.Context, key string, request func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error)) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	s.marketData.mu.Lock()
	item, ok := s.marketData.interest[key]
	miss, refused := s.marketData.definitionMisses[item.Params.Contract]
	s.marketData.mu.Unlock()
	if !ok || now.After(item.Until) || now.Before(item.RetryAt) || refused && s.marketHistoryDefinitionMissHolds(miss, now) {
		return
	}
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), marketHistoryRefreshWindow)
	result, err := request(readCtx, item.Params)
	cancel()
	verdict := errors.Is(err, ibkrlib.ErrContractNoDefinition)
	s.marketData.mu.Lock()
	current := s.marketData.interest[key]
	failed := err != nil || result != nil && result.Cache != nil && result.Cache.RefreshFailed
	recovered := !failed && current.Failures > 0
	if failed {
		current.Failures = min(current.Failures+1, 5)
		current.RetryAt = now.Add(min(15*time.Minute, time.Duration(1<<current.Failures)*30*time.Second))
	} else {
		current.Failures = 0
		current.RetryAt = time.Time{}
		delete(s.marketData.definitionMisses, item.Params.Contract)
	}
	s.marketData.interest[key] = current
	s.marketData.mu.Unlock()
	if verdict {
		s.rememberMarketHistoryDefinitionMiss(item.Params.Contract, now, err)
	}
	if s.logger == nil {
		return
	}
	if err != nil && !verdict {
		s.logger.Warnf("market history refresh %s %s: %v; next attempt after %s", item.Params.Contract.Symbol, item.Params.Range, err, current.RetryAt.Format(time.TimeOnly))
	} else if recovered {
		s.logger.Infof("market history refresh %s %s: recovered", item.Params.Contract.Symbol, item.Params.Range)
	}
}

// marketHistoryDefinitionMiss is the broker's "no security definition"
// verdict for one history contract. The answer does not change within a
// broker session, so the refresh worker stops asking until that session ends,
// and for at least marketHistoryDefinitionMissFloor either way. Connector and
// Session name the session that answered; both are zero when none was ready.
type marketHistoryDefinitionMiss struct {
	At        time.Time
	Connector *ibkrlib.Connector
	Session   ibkrlib.ConnectorSessionBinding
}

// marketHistoryDefinitionMissFloor is how long a definition verdict pauses a
// contract's refresh even across a reconnect: the connector's longest
// re-resolution backoff, which the quote-history cache keeps the verdict for
// as well.
const marketHistoryDefinitionMissFloor = 30 * time.Minute

// rememberMarketHistoryDefinitionMiss records the broker's verdict for
// contract and says so once at WARN; a verdict that still holds is neither
// re-recorded nor repeated. Any recorded history keeps being served. Lapsed
// verdicts are dropped on the way, and the memory is bounded like interest.
func (s *Server) rememberMarketHistoryDefinitionMiss(contract rpc.ContractParams, now time.Time, cause error) {
	s.mu.Lock()
	connector := s.connector
	s.mu.Unlock()
	session, _ := connector.CaptureSession()
	s.marketData.mu.Lock()
	if s.marketData.definitionMisses == nil {
		s.marketData.definitionMisses = make(map[rpc.ContractParams]marketHistoryDefinitionMiss)
	}
	misses := s.marketData.definitionMisses
	if miss, ok := misses[contract]; ok && s.marketHistoryDefinitionMissHolds(miss, now) {
		s.marketData.mu.Unlock()
		return
	}
	for other, miss := range misses {
		if !s.marketHistoryDefinitionMissHolds(miss, now) {
			delete(misses, other)
		}
	}
	if len(misses) >= 64 {
		s.marketData.mu.Unlock()
		return
	}
	misses[contract] = marketHistoryDefinitionMiss{At: now, Connector: connector, Session: session}
	s.marketData.mu.Unlock()
	if s.logger != nil {
		s.logger.Warnf("market history refresh %s: %v; paused for the rest of this broker session and until at least %s (recorded history is still served)", contract.Symbol, cause, now.Add(marketHistoryDefinitionMissFloor).Format(time.TimeOnly))
	}
}

// clearMarketHistoryDefinitionMiss forgets a verdict the broker has since
// contradicted with bars.
func (s *Server) clearMarketHistoryDefinitionMiss(contract rpc.ContractParams) {
	s.marketData.mu.Lock()
	delete(s.marketData.definitionMisses, contract)
	s.marketData.mu.Unlock()
}

// marketHistoryDefinitionMissHolds reports whether a verdict still pauses a
// contract's refresh: for the floor after it was given, and beyond that for
// as long as the broker session that gave it is current.
func (s *Server) marketHistoryDefinitionMissHolds(miss marketHistoryDefinitionMiss, now time.Time) bool {
	if now.Before(miss.At.Add(marketHistoryDefinitionMissFloor)) {
		return true
	}
	if s.marketHistorySessionCurrentForTest != nil {
		return s.marketHistorySessionCurrentForTest(miss.Connector, miss.Session)
	}
	return miss.Connector.SessionCurrent(miss.Session)
}
