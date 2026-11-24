package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// Catch up on startup, then capture every ten minutes from close+15m through
// close+3h, allowing the existing breadth sweep to finish. Collection does not
// keep an otherwise idle daemon alive between passes. The fixed basket uses the
// existing paced history lane; the broad constituent sweep is never duplicated.
func (s *Server) startMarketTapeCollection(ctx context.Context) {
	// The tape joins on IBKR contract ids, so under OpenD each pass would spend
	// history quota on names it then discards.
	if s.coreStore == nil || s.opend != nil {
		return
	}
	if _, err := updateTapeArchiveStatus(ctx, s.coreStore, time.Now(), false); err != nil {
		s.marketTapeArchiveFailed.Store(true)
	}
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		var lastAttempt time.Time
		for {
			now := time.Now()
			if tapeCollectionDue(lastAttempt, now) {
				if s.collectMarketTape(ctx, func(ctx context.Context) (*rpc.MarketTapeResult, error) {
					// Refresh the fixed benchmark/basket histories on the paced lane,
					// without registering all-day interactive chart interest.
					return s.buildAndArchiveMarketTape(ctx, rpc.MarketTapeParams{Sessions: 60}, s.marketHistoryRequest)
				}) {
					lastAttempt = now
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (s *Server) collectMarketTape(ctx context.Context, read func(context.Context) (*rpc.MarketTapeResult, error)) bool {
	s.marketTapeCollecting.Store(true)
	defer s.marketTapeCollecting.Store(false)
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), marketHistoryRefreshWindow)
	defer cancel()
	result, err := read(readCtx)
	if err != nil || result == nil || result.Archive == nil || result.Archive.Status == "collection_failed" {
		s.marketTapeArchiveFailed.Store(true)
		if s.logger != nil && ctx.Err() == nil {
			s.logger.Warnf("market tape: archive collection failed; retrying on the next collection pass")
		}
		return false
	}
	s.marketTapeArchiveFailed.Store(false)
	if len(result.Sessions) == 0 {
		return false
	}
	return tapeSessionLegs(result.Sessions[len(result.Sessions)-1]) == tapeSessionLegCount
}

// tapeSessionLegCount is how many measured legs a complete session has; see
// tapeSessionLegs.
const tapeSessionLegCount = 8

// tapeSessionLegs counts the measured legs of one session: the SPX and QQQ
// closes, breadth, SPY, VIX, the leader basket's level, its company averages
// for every company and its activity for every share line. The collector
// stops for the evening at all of them, and the archive keeps the capture
// with the most.
func tapeSessionLegs(row rpc.MarketTapeSession) int {
	leaders := row.Leaders != nil
	n := 0
	for _, measured := range []bool{
		row.SPX != nil, row.QQQ != nil, row.Breadth != nil, row.SPY != nil, row.VIX != nil,
		leaders && row.Leaders.Price != nil,
		leaders && row.Leaders.Companies.Coverage50 == row.Leaders.Companies.CompanyCount,
		leaders && row.Leaders.VolumeCoverage == len(tapeLeaderWeights),
	} {
		if measured {
			n++
		}
	}
	return n
}

func tapeCollectionDue(lastAttempt, now time.Time) bool {
	if lastAttempt.IsZero() {
		return true
	}
	sessions, err := tapeArchiveCalendar(now.AddDate(0, 0, -10), 11)
	if err != nil {
		return false
	}
	for _, session := range slices.Backward(sessions) {
		close := session.Close
		if close.Add(15 * time.Minute).After(now) {
			continue
		}
		return lastAttempt.Before(close.Add(15*time.Minute)) || now.Before(close.Add(3*time.Hour))
	}
	return false
}
