package mcp

import (
	"context"
	"encoding/json"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
)

func marketReadTool() Tool {
	return Tool{Name: "canary_market_read", Title: "Canary Market Read", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodMarketRead},
		Description: "Read what today's US stock session is doing against yesterday's: the S&P 500 index against the prior session's high and low with today's five-minute bars, SPY volume against the same time of day in the prior recorded sessions, VIX against its 25 line and its prior close, and the largest companies (one share line each) against their opening gaps. Tags such as testing_prior_high or testing_prior_low (the latest close is beyond the level, not yet held), continuation, gap_holds (the session holds an opening gap beyond the prior range without adding to it), breakout_thin_volume, breakdown_on_volume, bounce_failed, rally_fading, stress_rising, fear_easing, leaders_fading, leaders_gap_down_hold, leaders_reclaiming or leaders_breaking_down name a condition only once it holds for two completed five-minute closes, and carry the numbers they rest on and when they began; during the session they describe the latest bar, after the close they keep the session's final read, and before the open there are none. While the session is open, relative sets the Nasdaq 100's day change beside the S&P 500's, with nasdaq_lagging or nasdaq_leading as tags read from current quotes. Before the open, indicated and indications say what the open is indicated to do from the ES and NQ front futures and the leaders' pre-market prices (indicated_gap_down, indicated_below_prior_low, leaders_indicated_down, leaders_near_prior_low, nasdaq_indicated_weaker and their mirrors); these are pre-market readings of thin trade that have not held, not tags. tag_events traces when each index, volume and VIX condition began and ended over the session, for marking on a chart. held reads the underlyings of the account's current stock and option positions (at most twelve; symbols only, no quantities, values or account ids, so the read is account-scoped): each row's prior session high, low and close, today's regular-session high, low and their midpoint (day_mid), the last price against the prior range (position), and the same open and gap state the leaders use; before the open only the prior session and last price, after the close the finished session. Every level comes from data and every threshold is returned, marked heuristic. A value that could not be measured is absent with a reason, never zero; the read answers within a few seconds with what it has, and slower inputs arrive in a later read. Use canary_market_tape for completed-session history, canary_regime for the risk regime, canary_market_history for one instrument's chart. Read-only and descriptive: no forecast, no effect on any decision or order.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			if err := rpc.ValidateMarketReadParams(args); err != nil {
				return nil, err
			}
			var out rpc.MarketReadResult
			if err := rpc.Call(ctx, conn, api.MarketRead, rpc.NoParams{}, &out); err != nil {
				return nil, err
			}
			return json.Marshal(out)
		}}
}
