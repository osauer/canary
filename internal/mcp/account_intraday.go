package mcp

import (
	"context"
	"encoding/json"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

func accountIntradayTool() Tool {
	return Tool{Name: "canary_account_intraday", Title: "Canary Account Intraday Value", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodAccountIntraday},
		Description: "Read how the account's net liquidation moved through the current and the previous US-equity regular session, as the daemon recorded it: one broker account read per five-minute bucket from the open, in the account base currency, with today's calendar session so a session not yet open reads apart from a closed day. A bucket with no admissible read has no point and is a gap, never a carried value; opening_value is present only when the first bucket was recorded. Use canary_account for the current figures and Day P&L, canary_reporting_performance for statement returns. Display evidence only: no valuation clock from the broker, no history before recording began, and no effect on any decision or order.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			if err := rpc.ValidateAccountIntradayParams(args); err != nil {
				return nil, err
			}
			var out rpc.AccountIntradayResult
			if err := rpc.Call(ctx, conn, api.AccountIntraday, rpc.NoParams{}, &out); err != nil {
				return nil, err
			}
			return json.Marshal(out)
		}}
}
