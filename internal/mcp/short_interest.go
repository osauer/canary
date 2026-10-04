package mcp

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func shortInterestTool() Tool {
	return Tool{
		Name: "canary_short_interest_screen", Title: "Canary US Equity Short Interest", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodShortInterestScreen},
		Description: "Discover US equities (including listed funds and OTC) with large reported short positions using FINRA's twice-monthly public publication. Filters and sorting apply before limiting results; default ranks short shares, not percent of float. Free float is unavailable. Settlement date is distinct from fetch time; failed refreshes retain stale publication provenance. FINRA days to cover is floored at 1 and average daily shares covers the reporting interval. Optional recent price and 20-session dollar-turnover context is bounded background acquisition with explicit partial coverage, not an all-market liquidity guarantee. Short interest is neither daily short-sale volume nor borrowing cost. Use canary_lending_screen for borrower fees, canary_lending_fees for earned lending income. Read-only; no orders, enrollment, subscriptions or policy changes.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"limit":                     json.RawMessage(`{"type":"integer","minimum":1,"maximum":100,"description":"Maximum results after filtering and sorting; default 50"}`),
			"sort_by":                   schemaEnum(rpc.ShortInterestSortKeys, "Ranking column, default short_interest_shares; missing evidence always last"),
			"sort_dir":                  schemaEnum([]string{"asc", "desc"}, "Sort direction, default desc"),
			"min_price":                 json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum covered USD price; unknown values excluded; default 0 disables"}`),
			"min_avg_dollar_volume_20d": json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum covered 20-completed-session average USD turnover; default 0 disables"}`),
			"min_average_volume":        json.RawMessage(`{"type":"integer","minimum":0,"description":"Minimum FINRA reporting-cycle average daily shares, not 20-session volume; default 0"}`),
			"min_days_to_cover":         json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum FINRA days to cover; published values at or below one floor at 1; default 0"}`),
			"listed_only":               json.RawMessage(`{"type":"boolean","description":"Exclude OTC rows; listed funds remain included; default false"}`),
			"exclude":                   json.RawMessage(`{"type":"array","maxItems":100,"items":{"type":"string"},"description":"Case-insensitive symbols to omit, default empty"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var p rpc.ShortInterestScreenParams
			if err := unmarshalArgs(args, &p); err != nil {
				return nil, err
			}
			p, err := rpc.NormalizeShortInterestScreenParams(p)
			if err != nil {
				return nil, err
			}
			var r rpc.ShortInterestScreenResult
			if err := conn.Call(ctx, rpc.MethodShortInterestScreen, p, &r); err != nil {
				return nil, err
			}
			if err := rpc.ValidateShortInterestScreenResult(r, p); err != nil {
				return nil, err
			}
			return json.Marshal(r)
		},
	}
}
