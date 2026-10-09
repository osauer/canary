package mcp

import (
	"context"
	"encoding/json"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func portfolioPlanTool() Tool {
	return Tool{Name: "canary_portfolio_plan", Title: "Canary Portfolio Plan", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodPortfolioPlan},
		Description: "Read the next portfolio action under an explicit owner-approved target-band mandate. Canary combines current holdings and the accepted watchlist, applies target bands, entry regimes and priority, and considers existing reductions and protection before sizing one stock addition. Returns hold or cannot-evaluate reasons, desired quantities separately from the checked next order, source evidence and unassigned stocks. Recalculate after each broker outcome; later candidates have no reserved allowance and projected sale proceeds are not spendable cash. Takes no caller-supplied holdings, targets, limits or authority. Read-only; may request bounded broker WhatIf fee and margin checks, but never refreshes the proposal executor, mints an order token, reserves cash or submits. Use canary_portfolio for composition and canary_add for one selected stock's sizing. Not autonomous trading or support for new option, bond or short positions.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			if err := rpc.ValidatePortfolioPlanParams(args); err != nil {
				return nil, err
			}
			var out rpc.PortfolioPlanResult
			if err := conn.Call(ctx, rpc.MethodPortfolioPlan, nil, &out); err != nil {
				return nil, err
			}
			return json.Marshal(out)
		}}
}
