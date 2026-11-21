package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Trading halts only matter while US equities trade. On Saturday
// 2026-11-21 Nasdaq's halts feed stopped answering and `canary status` read
// ATTENTION for a feed nothing could use until Monday. While no US exchange
// trades (a weekend or holiday, or outside 04:00 to 20:00 New York time) a
// stale or failing halts feed is not due; during trading hours it is still a
// concern (owner instruction 2026-11-21 12:38 CET).
func TestTradingHaltsFeedIsNotDueWhileUSMarketsAreClosed(t *testing.T) {
	ny := newYorkLocation()
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"Saturday", time.Date(2026, 11, 21, 6, 17, 0, 0, ny), "not_due"},
		{"Monday 03:00 New York", time.Date(2026, 11, 23, 3, 0, 0, 0, ny), "not_due"},
		{"Monday 10:00 New York", time.Date(2026, 11, 23, 10, 0, 0, 0, ny), "limited"},
		{"Monday 19:30 New York, after hours", time.Date(2026, 11, 23, 19, 30, 0, 0, ny), "limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{now: func() time.Time { return tc.at }}
			health := marketEventSourceHealth("trading_halts", rpc.SourceStatusStale, tc.at.Add(-10*time.Minute), tc.at, marketEventsHaltsFreshFor, "medium-low", []string{"using stale cached Nasdaq trade-halt RSS feed: timeout"})
			s.observeEventHealth(rpc.MarketEventsResult{AsOf: tc.at, SourceHealth: []rpc.SourceHealth{health}}, nil, broker.SessionBinding{})
			row, ok := s.observedDataHealth("events:trading_halts", tc.at)
			if !ok || row.State != tc.want {
				t.Fatalf("halts row %+v (ok=%v), want state %s", row, ok, tc.want)
			}
			if tc.want == "not_due" && len(row.ProblemIDs) != 0 {
				t.Fatalf("a closed-market halts outage still counts as a concern: %+v", row)
			}
		})
	}
}
