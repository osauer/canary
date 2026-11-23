package twssim

import (
	"fmt"
	"sync"
)

// Market-data feed modes a test or `canary-sim ctl feed` sets per contract.
// Live is the default: a live, two-sided quote. Delayed answers with
// IBKR's delayed market-data type (3) and the same ticks, so a client that
// needs live data refuses it. None answers nothing at all, as a line whose
// data never arrives. OneSided sends the bid alone. NoGreeks sends the
// quote without option computations, so an option's delta is unknown.
const (
	FeedLive     = "live"
	FeedDelayed  = "delayed"
	FeedNone     = "none"
	FeedOneSided = "one-sided"
	FeedNoGreeks = "no-greeks"
)

// quoteFeeds holds the per-contract feed modes and spread overrides.
type quoteFeeds struct {
	mu     sync.Mutex
	mode   map[int]string
	spread map[int]float64
}

// SetFeed sets how a contract's market data is answered from now on.
func (s *Server) SetFeed(conID int, mode string) error {
	switch mode {
	case FeedLive, FeedDelayed, FeedNone, FeedOneSided, FeedNoGreeks:
	default:
		return fmt.Errorf("twssim: feed %q: live, delayed, none, one-sided or no-greeks", mode)
	}
	if _, ok := s.book.instrument(conID); !ok {
		return fmt.Errorf("twssim: contract %d is not listed", conID)
	}
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	if s.feeds.mode == nil {
		s.feeds.mode = map[int]string{}
	}
	if mode == FeedLive {
		delete(s.feeds.mode, conID)
		return nil
	}
	s.feeds.mode[conID] = mode
	return nil
}

// SetSpread quotes a contract pct percent of its price wide, around its
// price, from now on (an option's price still follows its underlying);
// zero restores the book's own spread.
func (s *Server) SetSpread(conID int, pct float64) error {
	if pct < 0 || pct >= 200 {
		return fmt.Errorf("twssim: spread %g%% of the price is out of range", pct)
	}
	if _, ok := s.book.instrument(conID); !ok {
		return fmt.Errorf("twssim: contract %d is not listed", conID)
	}
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	if s.feeds.spread == nil {
		s.feeds.spread = map[int]float64{}
	}
	if pct == 0 {
		delete(s.feeds.spread, conID)
		return nil
	}
	s.feeds.spread[conID] = pct
	return nil
}

// feedFor applies a contract's feed mode and spread override to its quote.
func (s *Server) feedFor(in Instrument, q quote) (quote, string) {
	s.feeds.mu.Lock()
	mode, pct := s.feeds.mode[in.ConID], s.feeds.spread[in.ConID]
	s.feeds.mu.Unlock()
	if pct > 0 && in.Price > 0 {
		q.bid = roundTo(in.Price*(1-pct/200), in.MinTick)
		q.ask = roundTo(in.Price*(1+pct/200), in.MinTick)
	}
	if mode == FeedOneSided {
		q.ask = 0
	}
	if mode == "" {
		mode = FeedLive
	}
	return q, mode
}
