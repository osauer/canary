package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

type marketHistoryInterest struct {
	Params   rpc.MarketHistoryParams
	Until    time.Time
	RetryAt  time.Time
	Failures int
	// TriedAt is when the refresh worker last took the series up.
	TriedAt time.Time
}

// marketHistoryInterestLimit bounds the series the refresh worker follows.
const marketHistoryInterestLimit = 64

// marketHistoryInterestFor is how long one request keeps a series followed.
const marketHistoryInterestFor = 24 * time.Hour

func (s *Server) rememberMarketHistory(p rpc.MarketHistoryParams) {
	key, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		return
	}
	s.marketData.mu.Lock()
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
	var evicted string
	var dropped marketHistoryInterest
	if !exists && len(s.marketData.interest) >= marketHistoryInterestLimit {
		// Follow the new series in place of the one asked for least
		// recently. Refusing it left a shown chart unrefreshed in silence.
		for other, candidate := range s.marketData.interest {
			if evicted == "" || candidate.Until.Before(dropped.Until) || candidate.Until.Equal(dropped.Until) && other < evicted {
				evicted, dropped = other, candidate
			}
		}
		delete(s.marketData.interest, evicted)
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
	item.Until = now.Add(marketHistoryInterestFor)
	s.marketData.interest[key] = item
	s.marketData.mu.Unlock()
	if evicted != "" && s.logger != nil {
		s.logger.Warnf("market history interest at its %d-series limit: stopped refreshing %s %s (key %s, last asked %s) to follow %s %s (key %s)", marketHistoryInterestLimit, dropped.Params.Contract.Symbol, dropped.Params.Range, evicted, dropped.Until.Add(-marketHistoryInterestFor).Format(time.DateTime), normalized.Contract.Symbol, normalized.Range, key)
	}
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
			s.refreshMarketHistoryDue(ctx, s.marketHistoryRequest)
		}
	})
}

// refreshMarketHistoryDue takes up followed series one at a time, those a
// reader asked for within the last cadence first, the rest in turn, until every series has been taken up within the last
// tick. A series is taken up again once a tick has passed since its last
// turn, so one that a reader keeps asking for (market.read's index, volume
// and VIX series, every thirty seconds) comes round every tick however long
// the paced reads of the others take. Walking all series once per sweep in
// key order let a sweep of sixty-odd paced reads run past twenty minutes, and
// a series late in the key order waited all of it (2026-11-24: SPX and SPY 23
// minutes unrefreshed in an open session). The other series share what the
// pacing leaves, oldest turn first; a request for one reads it on demand.
func (s *Server) refreshMarketHistoryDue(ctx context.Context, request func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error)) {
	for ctx.Err() == nil {
		key, ok := s.nextMarketHistoryRefresh()
		if !ok {
			return
		}
		s.refreshMarketHistoryInterest(ctx, key, request)
	}
}

// marketHistoryInUseFor is how recently a series must have been asked for to
// count as being read: one intraday refresh cadence.
const marketHistoryInUseFor = 5 * time.Minute

// marketHistoryInUse reports whether a reader asked for the series within the
// last intraday cadence.
func marketHistoryInUse(item marketHistoryInterest, now time.Time) bool {
	return now.Sub(item.Until.Add(-marketHistoryInterestFor)) < marketHistoryInUseFor
}

// nextMarketHistoryRefresh picks, among the followed series not taken up
// within the last tick and not backing off, one being read ahead of the
// rest, and within each the one taken up longest ago; it marks it taken up.
func (s *Server) nextMarketHistoryRefresh() (string, bool) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	s.marketData.mu.Lock()
	defer s.marketData.mu.Unlock()
	var best string
	var pick marketHistoryInterest
	for key, item := range s.marketData.interest {
		if now.After(item.Until) || now.Before(item.RetryAt) || now.Sub(item.TriedAt) < marketHistoryRefreshTick {
			continue
		}
		inUse := marketHistoryInUse(item, now)
		if best == "" || inUse && !marketHistoryInUse(pick, now) ||
			inUse == marketHistoryInUse(pick, now) && (item.TriedAt.Before(pick.TriedAt) || item.TriedAt.Equal(pick.TriedAt) && key < best) {
			best, pick = key, item
		}
	}
	if best == "" {
		return "", false
	}
	pick.TriedAt = now
	s.marketData.interest[best] = pick
	return best, true
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
	// The worker's own context ends only at daemon shutdown: the read was
	// abandoned, not failed, and no next attempt will come. Keyed on ctx, not
	// on the error: a coalesced interactive read whose caller left also
	// returns context.Canceled while this worker lives on, and still counts.
	if err != nil && ctx.Err() != nil {
		return
	}
	// Pacing and the request itself can consume the refresh window. Start
	// the existing retry interval when that attempt finishes, not before it.
	now = time.Now()
	if s.now != nil {
		now = s.now()
	}
	verdict := marketHistoryVerdict(err)
	s.marketData.mu.Lock()
	// The series may have expired or been evicted during the read; it then
	// stays gone rather than returning as an entry no request made.
	current, followed := s.marketData.interest[key]
	failed := err != nil || result != nil && result.Cache != nil && result.Cache.RefreshFailed
	recovered := followed && !failed && current.Failures > 0
	if failed {
		current.Failures = min(current.Failures+1, 5)
		current.RetryAt = now.Add(min(15*time.Minute, time.Duration(1<<current.Failures)*30*time.Second))
	} else {
		current.Failures = 0
		current.RetryAt = time.Time{}
		delete(s.marketData.definitionMisses, item.Params.Contract)
	}
	if followed {
		s.marketData.interest[key] = current
	}
	s.marketData.mu.Unlock()
	if verdict {
		s.rememberMarketHistoryDefinitionMiss(item.Params.Contract, now, err)
	}
	if s.logger == nil {
		return
	}
	// The connector announces a stalled Gateway once; a line per series would
	// repeat it on every refresh until the Gateway answers again.
	// A bar farm broken past its attention threshold is announced the same way.
	if err != nil && !verdict && !errors.Is(err, ibkrlib.ErrHistoricalServiceStalled) && !(errors.Is(err, context.DeadlineExceeded) && s.historicalBarFarmOutage()) {
		// An unavailable broker is one incident with its own owner (see
		// logGatewayDependency); a followed series joins it instead of
		// warning every cycle (30 lines per symbol across the 2026-10-03 night).
		if errors.Is(err, ibkrlib.ErrIBKRUnavailable) && s.logGatewayDependency("history refresh requires the broker; recorded history remains available") {
			return
		}
		next := "no longer followed"
		if followed {
			next = "next attempt after " + current.RetryAt.Format(time.TimeOnly)
		}
		s.logger.Warnf("market history refresh %s %s: %v; %s", item.Params.Contract.Symbol, item.Params.Range, err, next)
	} else if recovered {
		s.logger.Infof("market history refresh %s %s: recovered", item.Params.Contract.Symbol, item.Params.Range)
	}
}

// marketHistoryVerdict reports whether err settles that a contract has no
// history to refresh: the broker's "no security definition" answer, or the
// connector refusing a contract it treats as inactive (reviewed terminal
// evidence among them) without asking the broker. Either pauses the series
// quietly rather than warning on every retry.
func marketHistoryVerdict(err error) bool {
	return errors.Is(err, ibkrlib.ErrContractNoDefinition) || errors.Is(err, ibkrlib.ErrSymbolInactive)
}

// marketHistoryDefinitionMiss is the broker's "no security definition"
// verdict for one history contract. The answer does not change within a
// broker session, so the refresh worker stops asking until that session ends,
// and for at least marketHistoryDefinitionMissFloor either way. Session and
// Binding name the broker session that answered; Session is nil when the
// daemon held none and Binding zero when none was ready.
type marketHistoryDefinitionMiss struct {
	At      time.Time
	Session broker.Session
	Binding broker.SessionBinding
}

// newMarketHistoryDefinitionMiss is a verdict given at now by the broker
// session the daemon holds.
func (s *Server) newMarketHistoryDefinitionMiss(now time.Time) marketHistoryDefinitionMiss {
	miss := marketHistoryDefinitionMiss{At: now, Session: s.brokerSession()}
	if miss.Session != nil {
		miss.Binding, _ = miss.Session.Capture()
	}
	return miss
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
	miss := s.newMarketHistoryDefinitionMiss(now)
	s.marketData.mu.Lock()
	if s.marketData.definitionMisses == nil {
		s.marketData.definitionMisses = make(map[rpc.ContractParams]marketHistoryDefinitionMiss)
	}
	misses := s.marketData.definitionMisses
	if held, ok := misses[contract]; ok && s.marketHistoryDefinitionMissHolds(held, now) {
		s.marketData.mu.Unlock()
		return
	}
	for other, held := range misses {
		if !s.marketHistoryDefinitionMissHolds(held, now) {
			delete(misses, other)
		}
	}
	if len(misses) >= 64 {
		s.marketData.mu.Unlock()
		return
	}
	misses[contract] = miss
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
		return s.marketHistorySessionCurrentForTest(miss.Binding)
	}
	return miss.Session != nil && miss.Session.Current(miss.Binding)
}
