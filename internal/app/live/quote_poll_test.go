package live

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/rpc"
)

type quotePollClient struct {
	daemonclient.Client
	duringStatus, duringPositions, duringTrading, duringNudges func()
	quote                                                      func(rpc.ContractParams) (*rpc.Quote, error)
}

func (c *quotePollClient) Status(context.Context) (*rpc.HealthResult, error) {
	if c.duringStatus != nil {
		c.duringStatus()
	}
	return &rpc.HealthResult{}, nil
}
func (*quotePollClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return &rpc.MarketCalendarResult{}, nil
}
func (*quotePollClient) Account(context.Context) (*rpc.AccountResult, error) {
	return &rpc.AccountResult{}, nil
}
func (c *quotePollClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	if c.duringPositions != nil {
		c.duringPositions()
	}
	return &rpc.PositionsResult{}, nil
}
func (c *quotePollClient) Quote(_ context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	return c.quote(contract)
}
func (c *quotePollClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	if c.duringTrading != nil {
		c.duringTrading()
	}
	return &rpc.TradingStatus{}, nil
}
func (*quotePollClient) AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error) {
	return &rpc.AutoTradeStatus{}, nil
}
func (*quotePollClient) TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	return &rpc.TradeProposalSnapshot{}, nil
}
func (*quotePollClient) OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	return &rpc.OpportunitySnapshot{}, nil
}
func (*quotePollClient) Settings(context.Context) (*rpc.PlatformSettings, error) {
	return &rpc.PlatformSettings{}, nil
}
func (c *quotePollClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	if c.duringNudges != nil {
		c.duringNudges()
	}
	return &rpc.NudgesSnapshotResult{}, nil
}

func newQuotePollService() (*Service, *quotePollClient, time.Time) {
	now := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	client := &quotePollClient{quote: func(contract rpc.ContractParams) (*rpc.Quote, error) {
		return &rpc.Quote{Symbol: contract.Symbol, AsOf: now, Price: new(100.0)}, nil
	}}
	s := New(client, time.Second, time.Minute)
	s.now = func() time.Time { return now }
	s.nextStress, s.nextNudges = now.Add(time.Hour), now.Add(time.Hour)
	for _, q := range marketQuoteContracts {
		s.applyMarketQuoteFrame(q.label, rpc.Frame{T: now, Last: new(100.0)})
	}
	return s, client, now
}

func TestPollPreservesStreamQuotes(t *testing.T) {
	for _, path := range []string{"full_early", "full_late", "status", "nudges"} {
		for _, update := range []string{"price", "error_frame", "error", "recovery"} {
			t.Run(path+"/"+update, func(t *testing.T) {
				s, client, now := newQuotePollService()
				if update == "recovery" {
					s.applyMarketQuoteError("SPY", errors.New("old error"))
				}
				events, release := s.Subscribe()
				defer release()
				price, wantError := 101.0, ""
				hook := func() {
					switch update {
					case "error_frame":
						price, wantError = 100, "unavailable: fixture"
						s.applyMarketQuoteFrame("SPY", rpc.Frame{Error: &rpc.FrameError{Code: "unavailable", Message: "fixture"}})
					case "error":
						price, wantError = 100, "fixture"
						s.applyMarketQuoteError("SPY", errors.New(wantError))
					default:
						s.applyMarketQuoteFrame("SPY", rpc.Frame{T: now, Last: new(price)})
					}
				}
				var result Snapshot
				switch path {
				case "full_early":
					client.duringPositions = hook
					result = s.PollOnce(t.Context())
				case "full_late":
					client.duringTrading = hook
					result = s.PollOnce(t.Context())
				case "status":
					client.duringStatus = hook
					result = s.pollStatus(t.Context())
				case "nudges":
					client.duringNudges = hook
					result = s.PollNudgesOnce(t.Context())
				}
				check := func(q *MarketQuotes) {
					t.Helper()
					if q == nil || q.Quotes["SPY"].Price == nil || *q.Quotes["SPY"].Price != price || q.Errors["SPY"] != wantError {
						t.Fatalf("poll rolled back stream price or error: %+v", q)
					}
				}
				check(result.Quotes)
				check(s.Snapshot().Quotes)
				// Once the stream event is emitted, no later event may roll it back.
				seenStream := false
				for len(events) > 0 {
					ev := <-events
					if ev.Type == "market_quotes" {
						check(ev.Data.(*MarketQuotes))
						seenStream = true
					} else if ev.Type == "snapshot" && seenStream {
						check(ev.Data.(Snapshot).Quotes)
					}
				}
				if !seenStream {
					t.Fatal("stream event was not published")
				}
			})
		}
	}
}

func TestQuoteRPCPreservesConcurrentStreamObservation(t *testing.T) {
	for _, update := range []string{"price", "error", "recovery"} {
		t.Run(update, func(t *testing.T) {
			s, client, now := newQuotePollService()
			// Force the quote fallback for every symbol; one stream changes while
			// its request is in flight, while the other symbols must still refresh.
			for _, q := range marketQuoteContracts {
				s.applyMarketQuoteFrame(q.label, rpc.Frame{T: now.Add(-time.Minute), Last: new(100.0)})
			}
			client.quote = func(contract rpc.ContractParams) (*rpc.Quote, error) {
				if contract.Symbol == "SPY" {
					if update == "error" {
						s.applyMarketQuoteError("SPY", errors.New("stream unavailable"))
					} else {
						s.applyMarketQuoteFrame("SPY", rpc.Frame{T: now, Last: new(101.0)})
					}
					if update == "recovery" {
						return nil, errors.New("earlier RPC failure")
					}
				}
				return &rpc.Quote{Symbol: contract.Symbol, AsOf: now, Price: new(99.0)}, nil
			}
			quotes, err := s.marketQuotes(t.Context(), now, nil)
			if err != nil {
				t.Fatal(err)
			}
			price, wantError := 101.0, ""
			if update == "error" {
				price, wantError = 100, "stream unavailable"
			}
			if *quotes.Quotes["SPY"].Price != price || quotes.Errors["SPY"] != wantError || *quotes.Quotes["QQQ"].Price != 99 {
				t.Fatalf("RPC overwrote stream or dropped another symbol: %+v", quotes)
			}
		})
	}
}

func TestQuotePollDoesNotRegressObservationTime(t *testing.T) {
	s, client, now := newQuotePollService()
	client.quote = func(contract rpc.ContractParams) (*rpc.Quote, error) {
		return &rpc.Quote{Symbol: contract.Symbol, AsOf: now.Add(-time.Hour), Price: new(99.0)}, nil
	}
	// A poll started before a stream tick must retain the tick's receipt time,
	// even when it has no additional symbols to refresh.
	quotes, err := s.marketQuotes(t.Context(), now.Add(-time.Second), nil)
	if err != nil || !quotes.AsOf.Equal(now) {
		t.Fatalf("poll regressed receipt time: as_of=%v err=%v", quotes.AsOf, err)
	}
	// A fallback RPC may itself carry an older cached quote.
	quotes, err = s.marketQuotes(t.Context(), now.Add(time.Minute), nil)
	if err != nil || *quotes.Quotes["SPY"].Price != 100 {
		t.Fatalf("older RPC quote replaced current price: quotes=%+v err=%v", quotes, err)
	}
}
