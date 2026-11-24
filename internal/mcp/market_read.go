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
		Description: "Read what today's US stock session is doing against yesterday's: the S&P 500 index against the prior session's high and low with today's five-minute bars, SPY volume against the same time of day in the prior recorded sessions, VIX against its 25 line and its prior close, and the largest companies (one share line each) against their opening gaps. Tags such as continuation, breakout_thin_volume, bounce_failed, rally_fading, stress_rising, fear_easing or leaders_fading name a condition only once it holds for two completed five-minute closes, and carry the numbers they rest on and when they began; during the session they describe the latest bar, after the close they keep the session's final read, and before the open there are none. tag_events traces when each index, volume and VIX condition began and ended over the session, for marking on a chart. Every level comes from data and every threshold is returned, marked heuristic. A value that could not be measured is absent with a reason, never zero. Use canary_market_tape for completed-session history, canary_regime for the risk regime, canary_market_history for one instrument's chart. Read-only and descriptive: no forecast, no effect on any decision or order.",
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
