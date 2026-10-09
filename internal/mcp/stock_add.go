package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func stockAddTool() Tool {
	return Tool{Name: "canary_add", Title: "Canary Stock Add Plan", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodAddPlan},
		Description: "Calculate the permitted whole-share addition to a selected stock, including opening from a watchlist with no holding. Canary reads actual positions, other exposure, outstanding orders, cash reserves, approved allocation limits and risk budgets. Provide an explicit currency and limit price; choose a positive additional quantity or max=true explicitly. The result separates stock allocation room from the order allowance, exact cash and margin checks, risk evidence and working stop coverage. Missing inputs or unapproved policy hold the plan; a bounded maximum search may return a checked quantity without establishing a maximum. Read-only planning may request a broker WhatIf fee estimate; it returns no preview token, reserves no money and cannot submit, modify or authorise an order. Use canary_positions for holdings, canary_rules for risk explanations, and the owner CLI's add preview for an exact order review. Not for options, bonds, shorts, stock selection, recurring purchases or an investment recommendation.",
		JSONSchema:  schemaObject(map[string]json.RawMessage{"symbol": schemaString("Selected stock symbol, case-insensitive; membership of a watchlist does not authorise a purchase."), "currency": schemaString("Explicit stock trading currency, for example USD or EUR; no automatic conversion."), "limit_price": json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"Maximum price per share in the specified currency."}`), "max": json.RawMessage(`{"type":"boolean","description":"Explicitly request the maximum for one order. Mutually exclusive with quantity; no automatic execution."}`), "quantity": json.RawMessage(`{"type":"integer","minimum":1,"maximum":1000000,"description":"Exact additional whole shares. Choose this or max=true; never the current position or a target total."}`), "con_id": json.RawMessage(`{"type":"integer","minimum":1,"description":"Exact broker stock contract ID when known."}`)}, []string{"symbol", "currency", "limit_price"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var a struct {
				Symbol     string  `json:"symbol"`
				Currency   string  `json:"currency"`
				LimitPrice float64 `json:"limit_price"`
				Max        bool    `json:"max"`
				Quantity   int     `json:"quantity"`
				ConID      int     `json:"con_id"`
			}
			d := json.NewDecoder(bytes.NewReader(args))
			d.DisallowUnknownFields()
			if err := d.Decode(&a); err != nil {
				return nil, err
			}
			if d.Decode(new(any)) != io.EOF {
				return nil, fmt.Errorf("one Add argument object is required")
			}
			p, err := rpc.NormalizeAddParams(rpc.AddParams{Contract: rpc.ContractParams{Symbol: a.Symbol, Currency: a.Currency, SecType: "STK", Exchange: "SMART", ConID: a.ConID}, LimitPrice: a.LimitPrice, Quantity: a.Quantity, Max: a.Max})
			if err != nil {
				return nil, err
			}
			var out rpc.AddPlanResult
			if err := conn.Call(ctx, rpc.MethodAddPlan, p, &out); err != nil {
				return nil, err
			}
			return json.Marshal(out)
		}}
}
