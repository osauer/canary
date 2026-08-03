package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
	"github.com/osauer/canary/v2/internal/watchlist"
)

// Tool is the registered shape of an MCP tool exposed by `canary mcp`.
// JSONSchema is sent to the MCP client verbatim; Handler is invoked when
// the client issues tools/call with a matching name. Handlers receive the
// daemon connection and the raw JSON arguments (an empty object when the
// client omits arguments).
type Tool struct {
	Name               string
	Title              string
	Description        string
	MonitorDescription string
	JSONSchema         json.RawMessage
	ReadOnlyHint       *bool
	Handler            func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error)
}

// Tools is the canonical inventory exposed over MCP. Order is the same as
// cli.Commands() to keep the parity test readable; the MCP client rebroadcasts
// whatever order we send.
var Tools = []Tool{
	{
		Name:               "canary_status",
		Title:              "Canary Status",
		Description:        "Daemon + gateway health snapshot: connection state, account, server version, members-list source, last-error, background tasks, per-subsystem health for storage/quote/watchlist/scanner/history/chain/gamma/breadth/proposals/opportunities, unhealthy IBKR data farms, and high-level `data_quality` warnings for degraded gamma or stale regime clusters. Run this first when troubleshooting connectivity or tool-specific slowness (\"why is data missing / stale / wrong-account?\", \"will scanner or gamma be busy?\", \"are downstream risk reads stale?\", \"why is the protection or opportunities panel stale?\"). `subsystems[].status` can be ready/computing/unavailable/degraded/disabled and is more specific than the top-level gateway connection — quote/scanner degrade when required market-data readiness has not been observed, history degrades when historical-data readiness has not been observed, and chain degrades when market-data readiness is missing or a security-definition farm explicitly reports trouble, even if the socket is connected; `storage` is the daemon's own database rather than a broker feed, and an unavailable one means the daemon has latched fail-closed — served data can be stale and order writes are blocked, with the matching blocker on `canary_trading_status`; the `proposals` and `opportunities` entries turn degraded with blocker codes and the served snapshot's as_of when refreshes keep failing while an older snapshot is still served; `opportunities.message` also carries active opportunity policy id/version/status/fingerprint when available; `data_farms[]` is omitted when farms are healthy and only lists farms currently broken/disconnected; `data_quality[]` means the daemon can still serve data, but say so when a decision rests on it. NOT for portfolio state — use `canary_account` for cash/margin or `canary_positions` for what you own, and NOT for full risk evidence — use `canary_regime` or `canary_stress`.",
		MonitorDescription: "Connectivity and data-health check for the scheduled monitor. Use only when `canary_stress` reports degraded/failed inputs or the gateway may be disconnected. Read-only.",
		JSONSchema:         schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.HealthResult
			if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_trading_status",
		Title:       "Canary Trading Status",
		Description: "Local trading status: whether order entry is disabled, paper-ready, live-ready, or blocked; includes connected-gateway readiness, pinned endpoint/account/client-ID evidence, preview requirement, explicit `can_preview` and `can_write` booleans, MCP write mode, live override status, open-order count, and concrete blockers. Use before any order preview/place/modify/cancel request or when the user asks whether Canary can trade; `can_preview`/`can_write` are false while TWS/IB Gateway is disconnected or still handshaking. This tool does NOT place, modify, or cancel orders; it only reports readiness. For portfolio state use `canary_positions`; for account cash/margin use `canary_account`; for market context use `canary_quote`, `canary_chain`, or `canary_regime`.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.TradingStatus
			if err := conn.Call(ctx, rpc.MethodTradingStatus, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_settings",
		Title:       "Canary Platform Settings",
		Description: "Read Canary's platform settings and observed state: runtime user preferences such as purge/restore, stock-protection, and rulebook enablement, the per-symbol earnings-date overrides `canary_rules` reports as the `override` source, the regime and stress journal toggles, read-only trading mode/account/build capability, trading safety limits with access/source metadata, and compact observed market-data quality. Use when the user asks what Canary features are enabled, whether purge/restore or stock trailing-stop protection is available, why the rulebook returns nothing or where an earnings date came from, why a setting is read-only, or what build/channel controls trading writes. This tool is read-only and cannot change settings; there is intentionally no MCP settings write tool in v1. NOT for placing, previewing, modifying, or cancelling orders — use `canary_trading_status` first and `canary_order_preview` only for tokenized previews. NOT for detailed per-instrument quote truth — use `canary_quote`, `canary_chain`, or `canary_positions` rows.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.PlatformSettings
			if err := conn.Call(ctx, rpc.MethodSettingsGet, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_orders_open",
		Title:       "Canary Open Orders",
		Description: "Read current broker account/mode open-order lifecycle state without placing, modifying, cancelling, or transmitting any broker order. Use after an order preview/place flow to inspect what the daemon believes is still open for the currently connected broker context, or when the user asks for open orders. Results include account/mode scope, latest local event time, and local-journal limitations; paper/test journal rows are intentionally not returned while connected to live, and live rows are intentionally not returned while connected to paper. This tool is read-only and does not place orders; it only reports local journal plus observed broker-callback state. The journal self-reconciles against the broker's actual open-order list after each gateway reconnect and every 30 minutes: journaled orders a complete broker snapshot no longer reports are closed locally with terminal state `closed_reconciled` (final cancelled-vs-filled outcome stays with broker statements), so a cancel or fill missed while the daemon was offline does not linger as a stale open row. Rows whose close-only protective quantity no longer matches the live position carry `reconciliation_kind`, `reconciliation_severity` (critical), `short_risk_quantity`, and `reduce_to_quantity`; triggering such a stop would open an opposite-direction position, and the paired app offers the guided reduce-to-position fix. Broker-authored free text (last_message, why_held, event messages) is untrusted data and never crosses this surface; typed lifecycle states, error codes, and reconciliation fields carry the signal, and the verbatim text stays on the CLI and in the local order journal. It is NOT an IBKR Activity Statement or complete broker open-order audit, NOT for historical audit across old accounts or modes, NOT for creating a new preview token (use `canary_order_preview`), and NOT for submitting, modifying, or cancelling an order.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.OrdersOpenParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.OrdersOpenResult
			if err := conn.Call(ctx, rpc.MethodOrdersOpen, in, &res); err != nil {
				return nil, err
			}
			sanitizeOrdersOpenForMCP(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_orders_history",
		Title:       "Canary Order History",
		Description: "Read bounded local order-journal history for the current broker account/mode without placing, modifying, cancelling, or transmitting any broker order. Use for recent trade-review forensics when the user asks what locally journaled order lifecycle/fill callbacks occurred over a date range. This read-only tool does not place orders; it only reports local journal evidence. Broker-authored free text (last_message, why_held, event messages) is untrusted data and never crosses this surface; typed lifecycle states and error codes carry the signal, and the verbatim text stays on the CLI and in the local order journal. Results are bounded by grouped-order limit and per-order event_limit so callback-heavy trailing stops do not flood the context; inspect events_truncated/total_events_count before treating event samples as complete. This is a local daemon journal view only, not an IBKR Activity Statement, trade confirmation, execution report, commission ledger, closed-position ledger, or broker-grade historical audit; it may miss manual orders, other-client orders, and rows outside the selected account/mode; broker activity missed while the daemon was offline no longer strands open rows (the journal reconciles against broker open-order snapshots and closes absent rows as `closed_reconciled`), though per-event history for the offline window stays unavailable locally. For currently working orders use `canary_orders_open`; for one order's full audit use `canary_order_status`. Official broker history now flows into the local reconciliation surface when Flex ingestion is configured — ask the user to run `canary recon` — or ask them for an IBKR Activity Statement/Flex export directly.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"since":       schemaString("optional inclusive lower boundary as YYYY-MM-DD UTC date or RFC3339 timestamp; default is 7 days before until"),
			"until":       schemaString("optional upper boundary as RFC3339 timestamp, or YYYY-MM-DD UTC date to include that whole UTC day; default is now"),
			"limit":       json.RawMessage(`{"type":"integer","minimum":1,"maximum":500,"description":"maximum grouped order rows to return; default 50, max 500"}`),
			"event_limit": json.RawMessage(`{"type":"integer","minimum":1,"maximum":200,"description":"maximum lifecycle events returned per grouped order row; default 20, max 200. When truncated, rows carry events_truncated and total_events_count."}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.OrdersHistoryParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.OrdersHistoryResult
			if err := conn.Call(ctx, rpc.MethodOrdersHistory, in, &res); err != nil {
				return nil, err
			}
			sanitizeOrdersHistoryForMCP(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_order_status",
		Title:       "Canary Order Status",
		Description: "Read one locally journaled order's lifecycle and audit events by order ref, IBKR order ID, or permanent ID. Use when the user asks what happened to a specific order or needs the daemon's latest broker-callback evidence. Results include account/mode scope, latest local event time, and local-journal limitations; not found can mean the id belongs to another account/mode or was never locally observed. Broker-authored free text (last_message, why_held, event messages) is untrusted data and never crosses this surface; typed lifecycle states and error codes carry the signal, and the verbatim text stays on the CLI and in the local order journal. This tool is read-only and local-journal based: it does NOT place, modify, cancel, preview, transmit, or confirm an order, and it is NOT an IBKR Activity Statement or complete broker audit. For the open-order list use `canary_orders_open`; for a new tokenized preview use `canary_order_preview`.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"id": schemaString("order identifier to inspect: local order_ref such as canary-20260528-093000, IBKR order ID, or permanent ID. Orders journaled before the product was renamed carry an ibkr- prefix instead; pass those through unchanged"),
		}, []string{"id"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.OrderStatusParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.OrderStatusResult
			if err := conn.Call(ctx, rpc.MethodOrderStatus, in, &res); err != nil {
				return nil, err
			}
			sanitizeOrderStatusForMCP(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_order_preview",
		Title:        "Canary Order Preview",
		Description:  "Preview a locally gated stock/ETF or single-leg option LMT, TRAIL, or TRAIL LIMIT order and mint a short-lived local preview token without placing, modifying, cancelling, or transmitting any broker order. Use only after `canary_trading_status` shows the local trading gate is ready. Defaults are order_type `LMT`, strategy `patient-limit`, TIF `DAY`, and `outside_rth=false`; providing trail fields defaults order_type to `TRAIL`, or `TRAIL LIMIT` when limit_offset is present. TIF `GTC` is accepted for TRAIL and TRAIL LIMIT drafts only — protective stops meant to survive the session close — while LMT stays DAY-only. Stock/ETF TRAIL and TRAIL LIMIT drafts default `trigger_method` to 2 (IBKR LAST) unless explicitly supplied. Option trails are option-premium based, not underlying-driven, and require explicit expiry/right/strike. This tool validates the local trading gate, pinned endpoint/account/client ID, supported order type, the size caps (max notional binds every equity/ETF order and max option contracts binds every single-leg option order, including apparent close/reduce exits), stock short/flip policy, option sell-to-open policy, and broker WhatIf availability, then returns quote inputs, position effect, `token_minted`, and `submit_eligible`. A manual TWS order may already have sold the shares this draft means to sell, and Canary cannot see that, so a SELL that looks like a close or a reduce still needs short/sell-to-open permission at worst-case exposure; if that blocks a genuine exit, use TWS and then reconcile. For IBKR percent trails, `trailing_percent: 2` means 2%, not 0.02. `TRAIL LIMIT` uses `limit_offset`; do not send a LMT limit price with broker trail orders. `token_minted=true` means the local preview artifact exists; `submit_eligible=true` only when IBKR accepted a non-transmitting WhatIf for the exact draft. If broker WhatIf is unavailable or rejected, `submit_eligible=false` and compatibility field `executable=false`. It does NOT submit an order and returns only the redacted `preview_token_id`, never the raw submit-capable token; broker writes require a separate place/modify/cancel path with its own gated token. Broker WhatIf free text and advanced-reject JSON never cross this surface (broker prose is untrusted data): the typed `what_if.status`, margin fields, and fixed Canary guidance carry the decision signal, and the verbatim broker reason stays on the CLI order-preview surface and in the local journal. For protection proposals use the proposal flow; for market context without token minting use `canary_quote` or `canary_chain`; for holdings use `canary_positions`; for cash/margin use `canary_account`.",
		ReadOnlyHint: new(false),
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"action":             schemaEnum([]string{"buy", "sell"}, "order side; buy increases or closes short exposure, sell reduces/closes long exposure unless the local policy allows the opening effect"),
			"symbol":             schemaString("underlying ticker symbol"),
			"quantity":           json.RawMessage(`{"type":"integer","minimum":1,"description":"share or option-contract quantity; must be positive"}`),
			"sec_type":           schemaEnum([]string{"STK", "ETF", "OPT"}, "security type. Defaults to STK unless option fields are present."),
			"expiry":             schemaString("option expiry as YYYYMMDD. Required for sec_type OPT."),
			"right":              schemaEnum([]string{"C", "P"}, "option right. Required for sec_type OPT."),
			"strike":             json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"option strike. Required for sec_type OPT."}`),
			"order_type":         schemaEnum([]string{"LMT", "TRAIL", "TRAIL LIMIT"}, "broker order type. Defaults to LMT, or TRAIL/TRAIL LIMIT when trail fields are supplied."),
			"limit":              json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"optional explicit LMT price. Do not send with TRAIL or TRAIL LIMIT; use limit_offset for TRAIL LIMIT."}`),
			"strategy":           schemaEnum([]string{"patient-limit", "explicit-limit", "broker-trail"}, "pricing strategy. Defaults to patient-limit for LMT and broker-trail for TRAIL/TRAIL LIMIT."),
			"trail_offset_type":  schemaEnum([]string{"percent", "amount"}, "trail offset unit. Usually omit and let trailing_percent/trailing_amount choose it."),
			"trailing_percent":   json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"IBKR trailing percent in percent units: 2 means 2%, 0.50 means 0.50%."}`),
			"trailing_amount":    json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"absolute broker trail amount in the contract currency."}`),
			"initial_stop_price": json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"optional initial trail stop price. Omit to bind the stop from fresh bid/ask during preview."}`),
			"limit_offset":       json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"TRAIL LIMIT offset from the dynamic stop. Required for TRAIL LIMIT and rejected for plain TRAIL."}`),
			"trigger_method":     json.RawMessage(`{"type":"integer","enum":[1,2,3,4,7,8],"description":"IBKR stop trigger method for TRAIL/TRAIL LIMIT drafts; omit for the daemon default. Stock/ETF protective trails default to 2 (LAST). Useful values include 1 double bid/ask, 2 last, 3 double last, 4 bid/ask, 7 last or bid/ask, 8 midpoint."}`),
			"tif":                schemaEnum([]string{"DAY", "GTC"}, "time in force. DAY (default) expires at the session close; GTC persists until filled or cancelled and is accepted for TRAIL and TRAIL LIMIT orders only."),
			"outside_rth":        json.RawMessage(`{"type":"boolean","description":"whether the draft allows outside regular trading hours. Default false; option protection previews should keep this false."}`),
			"timeout_ms":         json.RawMessage(`{"type":"integer","minimum":100,"description":"quote snapshot timeout; default 5000 ms"}`),
			"market":             json.RawMessage(`{"type":"string","enum":["us","de"],"description":"optional stock routing shortcut; omit or use \"us\" for SMART/USD, use \"de\" for German/Xetra EUR equities via SMART with primary_exchange=IBIS"}`),
			"exchange":           schemaString("optional IBKR exchange or venue override, such as SMART or IBIS"),
			"primary_exchange":   schemaString("optional IBKR primary-exchange hint when routing through SMART, such as IBIS for German/Xetra listings"),
			"currency":           schemaString("optional contract currency override, such as USD or EUR"),
			"replace_id":         schemaString("optional existing open order ref, order ID, or permanent ID to preview a replacement draft"),
		}, []string{"action", "symbol", "quantity"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Action           string   `json:"action"`
				Symbol           string   `json:"symbol"`
				Quantity         int      `json:"quantity"`
				SecType          string   `json:"sec_type"`
				Expiry           string   `json:"expiry"`
				Right            string   `json:"right"`
				Strike           float64  `json:"strike"`
				OrderType        string   `json:"order_type"`
				Limit            *float64 `json:"limit"`
				Strategy         string   `json:"strategy"`
				TrailOffsetType  string   `json:"trail_offset_type"`
				TrailingPercent  *float64 `json:"trailing_percent"`
				TrailingAmount   *float64 `json:"trailing_amount"`
				InitialStopPrice float64  `json:"initial_stop_price"`
				LimitOffset      *float64 `json:"limit_offset"`
				TriggerMethod    int      `json:"trigger_method"`
				TIF              string   `json:"tif"`
				OutsideRTH       bool     `json:"outside_rth"`
				TimeoutMs        int      `json:"timeout_ms"`
				Market           string   `json:"market"`
				Exchange         string   `json:"exchange"`
				PrimaryExchange  string   `json:"primary_exchange"`
				Currency         string   `json:"currency"`
				ReplaceID        string   `json:"replace_id"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			secType := strings.ToUpper(strings.TrimSpace(in.SecType))
			if secType == "" {
				secType = "STK"
				if strings.TrimSpace(in.Expiry) != "" || strings.TrimSpace(in.Right) != "" || in.Strike > 0 {
					secType = "OPT"
				}
			}
			orderType, err := normalizeMCPPreviewOrderType(in.OrderType, in.TrailingPercent != nil || in.TrailingAmount != nil || in.InitialStopPrice > 0, in.LimitOffset != nil)
			if err != nil {
				return nil, err
			}
			multiplier := 0
			if secType == "OPT" {
				multiplier = 100
			}
			var trail *rpc.OrderTrailSpec
			if orderType == rpc.OrderTypeTRAIL || orderType == rpc.OrderTypeTRAILLIMIT {
				trail = &rpc.OrderTrailSpec{
					Basis:            rpc.OrderTrailBasisInstrumentPrice,
					OffsetType:       strings.ToLower(strings.TrimSpace(in.TrailOffsetType)),
					TrailingPercent:  in.TrailingPercent,
					TrailingAmount:   in.TrailingAmount,
					InitialStopPrice: in.InitialStopPrice,
					LimitOffset:      in.LimitOffset,
				}
			}
			var res rpc.OrderPreviewResult
			params := rpc.OrderPreviewParams{
				Action: strings.ToUpper(strings.TrimSpace(in.Action)),
				Contract: rpc.ContractParams{
					Symbol:      strings.ToUpper(strings.TrimSpace(in.Symbol)),
					SecType:     secType,
					Market:      strings.TrimSpace(in.Market),
					Exchange:    strings.ToUpper(strings.TrimSpace(in.Exchange)),
					PrimaryExch: strings.ToUpper(strings.TrimSpace(in.PrimaryExchange)),
					Currency:    strings.ToUpper(strings.TrimSpace(in.Currency)),
					Expiry:      strings.TrimSpace(in.Expiry),
					Right:       strings.ToUpper(strings.TrimSpace(in.Right)),
					Strike:      in.Strike,
					Multiplier:  multiplier,
				},
				Quantity:      in.Quantity,
				OrderType:     orderType,
				LimitPrice:    in.Limit,
				Trail:         trail,
				TriggerMethod: in.TriggerMethod,
				Strategy:      strings.TrimSpace(in.Strategy),
				TIF:           strings.ToUpper(strings.TrimSpace(in.TIF)),
				OutsideRTH:    in.OutsideRTH,
				ReplaceID:     strings.TrimSpace(in.ReplaceID),
				TimeoutMs:     in.TimeoutMs,
			}
			if params.Contract.Currency == "" && params.Contract.Market == "" && params.Contract.Exchange == "" && params.Contract.PrimaryExch == "" {
				params.Contract.Currency = "USD"
			}
			if params.Contract.Currency == "" && strings.EqualFold(params.Contract.Market, "de") {
				params.Contract.Currency = "EUR"
			}
			if err := conn.Call(ctx, rpc.MethodOrderPreview, params, &res); err != nil {
				return nil, err
			}
			// Raw submit-capable tokens never cross MCP (matching the
			// proposal surface's sanitizeProposalPreview): the token ID is
			// enough to correlate with CLI and journal evidence, and an
			// agent must mint its own token through the gated CLI path
			// before any place/modify write.
			res.PreviewToken = ""
			sanitizeOrderPreviewWhatIfForMCP(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_account",
		Title:       "Canary Account",
		Description: "Account-level financials: net liquidation, buying power, cash, margin — all in base currency. Use when the question is about *the account as a whole* (\"how much cash?\", \"how much margin am I using?\", \"what's today's P&L?\"). Includes daily_pnl (start-of-trading-day to now); when the gateway provides them, pnl_unrealized_total and pnl_realized_total carry the account's TOTAL unrealized/realized P&L from the same reqPnL stream — inception-to-now, NOT a breakdown of daily_pnl (they do not sum to it) — and are distinct from the session-running unrealized_pnl/realized_pnl on the account-updates feed. For multi-currency accounts, also returns currency_exposure: one row per non-base currency holding with net liquidation in that currency, gateway-reported exchange rate, and the base-currency conversion. Useful for attributing P&L between underlying moves and FX moves. NOT for per-position detail — use `canary_positions` to see what you actually own.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.AccountResult
			if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_positions",
		Title:       "Canary Positions",
		Description: "Open positions: stocks and options separated, plus normalized per-underlying exposure. Use when the question is about *what you own* (\"show me my positions\", \"what's my exposure to AAPL?\", \"how much delta do I have?\") or when building a held-portfolio dashboard outside market hours. Stock rows separate the IBKR account valuation mark (`mark`/`valuation_mark`) from market context: `regular_close` is the latest completed regular-session close, `prior_regular_close` is the close before that, and `quote_price` is the live/pre/post/overnight indication when IBKR supplies one. `day_change`/`day_change_pct` compare the account mark to `regular_close`; do not treat `quote_price` as the position valuation. Quote context includes data_type, feed_type, quote_price_source, quote_quality, indicative, spread_pct, day/52-week ranges, volume/avg_volume, volume_phase, quote_price_at/quote_price_as_of, warning_details, stale flags, and session_context from the trading calendar. Position money fields are explicit: `market_value_ccy`, `unrealized_pnl_ccy`, `realized_pnl_ccy`, and `daily_pnl_ccy` are in the contract currency; `market_value_base`, `unrealized_pnl_base`, `realized_pnl_base`, and `daily_pnl_base` are present when the account base currency and FX rate are known. Non-base rows carry `fx_rate` as BASE per CCY. `daily_pnl_ccy` is null when the daemon hasn't yet pre-warmed that contract's reqPnLSingle subscription or the account isn't entitled; never zero-substituted. Option legs include option_bid/option_ask, option_prev_close, and per-leg Greeks (delta/gamma/theta/vega) when IBKR delivers the model-computation tick within budget; outside U.S. option regular hours, `options_closed` warning_details mean those option quote/model fields are closed-session context, not executable quotes. `daily_theta_base` converts portfolio theta bleed to account base when every theta-bearing leg has an FX path; `mark_outside_bid_ask` and warning_details flag option marks away from bid/ask. The `by_underlying` rows include `group_market_value_base`, `group_market_value_pct_nlv`, `group_dollar_delta_base`, `group_unrealized_pnl_base`, and `group_daily_pnl_base` so agents do not need to re-aggregate currencies. The `portfolio.exposure_base` table is sorted by absolute base-currency market value and is the preferred portfolio map for multi-currency accounts. `protection_coverage` is the read-only stock/ETF stop-coverage ledger from positions plus locally observed open orders: states are covered, partial, unprotected, orphaned_order, reconcile_required, not_protectable, and unknown; `orphaned_order`/`reconcile_required` rows are stale protective orders and are not counted as coverage; `not_protectable` rows are defunct or unquoted holdings with no mark to stop out against (a cancelled equity carries no protectable exposure), so they are reported as position truth but excluded from unprotected counts and notional, and mismatch rows carry `reconciliation_kind`/`short_risk_quantity`/`reduce_to_quantity` (triggering an oversized close-only stop would open an opposite-direction position; the paired app offers the reduce-to-position fix). Use `view:\"risk\"` for compact top exposures, option health, and coverage summaries; use `canary_proposals` for candidate new protective stops; use `canary_orders_open`/order status for raw open-order lifecycle. Top-level `effective_delta` is a cross-symbol share-equivalent diagnostic; use per-underlying effective/dollar delta for coherent exposure. NOT for cash/margin totals (use `canary_account`), NOT for live quotes on symbols you don't hold (use `canary_quote`), and NOT for option-chain selection or replacement structures (use `canary_chain`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": schemaString("filter to a single underlying symbol (case-insensitive)"),
			"type":   schemaEnum([]string{"stk", "opt"}, "filter to stock or option positions"),
			"view":   schemaEnum([]string{rpc.ViewFull, rpc.ViewRisk}, "response shape: full returns existing stocks/options/by_underlying detail plus protection_coverage; risk returns compact portfolio aggregates, top exposures, option-health counts, protection coverage, and flagged option legs"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol string `json:"symbol"`
				Type   string `json:"type"`
				View   string `json:"view"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.View == "" {
				in.View = rpc.ViewFull
			}
			if in.View != rpc.ViewFull && in.View != rpc.ViewRisk {
				return nil, fmt.Errorf("view must be %q or %q (got %q)", rpc.ViewFull, rpc.ViewRisk, in.View)
			}
			var res rpc.PositionsResult
			params := rpc.PositionsListParams{Symbol: in.Symbol, Type: in.Type}
			if err := conn.Call(ctx, rpc.MethodPositionsList, params, &res); err != nil {
				return nil, err
			}
			if in.View == rpc.ViewRisk {
				return json.Marshal(rpc.CompactPositionsRisk(&res, 5))
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_quote",
		Title:       "Canary Quote",
		Description: "Snapshot quotes for one or more equity / ETF symbols. Returns bid/ask/last, mark, sizes, `regular_close` (latest completed regular-session close), `prior_regular_close`, regular close change, `quote_price` (current live/pre/post/overnight indication), quote-vs-close change, volume, avg_volume, avg_volume_20d, avg_dollar_volume_20d, liquidity_status/source, effective data_type for the legacy selected `price`, feed_type when the gateway subscription state differs, quote_quality (firm/indicative/wide/prev_close/stale/missing), indicative, spread_pct, volume_phase, warning_details, and `session_context` when the official market calendar explains stale/frozen/missing data. Use for *current quote and close context* questions on stocks/ETFs (\"what's SPY trading at?\", \"what was IBM's close and what is overnight quoting?\") and quick liquidity gates; for multi-symbol trend/RS screens use `canary_technical` because it batches daily-history calculations. Off-hours, prefer `regular_close` for the official close that matters and treat `quote_price` as an indication; gate decisions on quote_quality/spread_pct/data_type/quote_price_at rather than assuming `live` means regular-session executable. `price`/`price_source` are retained for compatibility and mirror `quote_price` when an indication exists, otherwise `regular_close`. Stock/ETF IV tick 106 arrives only when the gateway happens to send it and is often null; for a real IV read use `canary_chain` expiry IV or an expiry strike grid. US symbols default to SMART/USD. For German/Xetra equities whose ticker collides with the US default route (for example MBG), set `market: \"de\"` or explicit `exchange`/`currency`. NOT for options (use `canary_chain` with an `expiry` argument), NOT for historical bars (use `canary_history`), NOT for full technical screens (use `canary_technical`), NOT for the position valuation of something you already hold (`canary_positions` carries account marks plus quote context).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols":          json.RawMessage(`{"type":"array","items":{"type":"string"},"minItems":1,"description":"ticker symbols, e.g. [\"AAPL\",\"MSFT\"] or [\"MBG\"] with market=\"de\""}`),
			"market":           json.RawMessage(`{"type":"string","enum":["us","de"],"description":"optional stock routing shortcut; omit or use \"us\" for SMART/USD, use \"de\" for German/Xetra EUR equities via SMART with primary_exchange=IBIS"}`),
			"exchange":         schemaString("optional IBKR exchange/venue override for stocks, e.g. SMART or IBIS; omit unless the default market route fails"),
			"primary_exchange": schemaString("optional IBKR primary-exchange hint when routing a stock through SMART, e.g. NASDAQ or IBIS"),
			"currency":         schemaString("optional ISO currency override for stocks, e.g. USD or EUR"),
		}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbols         []string `json:"symbols"`
				Market          string   `json:"market"`
				Exchange        string   `json:"exchange"`
				PrimaryExchange string   `json:"primary_exchange"`
				Currency        string   `json:"currency"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if len(in.Symbols) == 0 {
				return nil, fmt.Errorf("symbols is required and must be non-empty")
			}
			quotes := make([]rpc.Quote, 0, len(in.Symbols))
			for _, sym := range in.Symbols {
				params := rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{
					Symbol:      strings.ToUpper(strings.TrimSpace(sym)),
					SecType:     "STK",
					Market:      strings.TrimSpace(in.Market),
					Exchange:    strings.ToUpper(strings.TrimSpace(in.Exchange)),
					PrimaryExch: strings.ToUpper(strings.TrimSpace(in.PrimaryExchange)),
					Currency:    strings.ToUpper(strings.TrimSpace(in.Currency)),
				}, IncludeLiquidity: true}
				var q rpc.Quote
				if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
					return nil, fmt.Errorf("quote %s: %w", sym, err)
				}
				quotes = append(quotes, q)
			}
			if len(quotes) == 1 {
				return json.Marshal(quotes[0])
			}
			return json.Marshal(quotes)
		},
	},
	{
		Name:        "canary_watch",
		Title:       "Canary Watchlist",
		Description: "Read the user's local Canary watchlist: symbols they explicitly saved with the CLI via `canary watch SYMBOL --add`. Defaults to a decision-making monitor with close-vs-indication context: `regular_close` and regular close change, `quote_price` and quote-vs-close change, currency, day range, 52-week range, volume, average volume, 20-day average volume/dollar volume from daily bars, effective data freshness, quote_quality/spread_pct/volume_phase, warning_details, session context, and optional held-stock context. In pre/post/overnight sessions, `regular_close` is the close that matters; `quote_price` is an outside-hours indication. Set `include_quotes: false` only when the user explicitly wants the saved symbol list without market data (still requires a reachable daemon). This MCP tool is read-only: it does NOT add, remove, clear, create IBKR/TWS watchlists, or place trades. For ad-hoc symbols that are not saved in the watchlist, use `canary_quote` instead; for trend/RS ranking use `canary_technical`.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"include_quotes":    json.RawMessage(`{"type":"boolean","description":"return enriched quote rows for saved symbols; default true. Set false only for the daemon-reachable list-only symbol inventory"}`),
			"include_positions": json.RawMessage(`{"type":"boolean","description":"when include_quotes is true, attach compact held-stock context where available; default true"}`),
			"timeout_ms":        json.RawMessage(`{"type":"integer","minimum":100,"description":"per-symbol quote timeout when include_quotes is true; default 5000 ms"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				IncludeQuotes    *bool `json:"include_quotes"`
				IncludePositions *bool `json:"include_positions"`
				TimeoutMs        int   `json:"timeout_ms"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			path, err := watchlist.DefaultPath()
			if err != nil {
				return nil, err
			}
			snap, err := watchlist.New(path).Snapshot()
			if err != nil {
				return nil, err
			}
			includeQuotes := true
			if in.IncludeQuotes != nil {
				includeQuotes = *in.IncludeQuotes
			}
			if !includeQuotes {
				if err := ensureDaemonReachable(ctx, conn); err != nil {
					return nil, err
				}
				return json.Marshal(snap)
			}
			if conn == nil {
				return nil, fmt.Errorf("include_quotes requires a daemon connection")
			}
			includePositions := true
			if in.IncludePositions != nil {
				includePositions = *in.IncludePositions
			}
			res, err := buildWatchlistQuoteResult(ctx, conn, snap, in.TimeoutMs, includePositions)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_calendar",
		Title:       "Canary Market Calendar",
		Description: "Official market-session calendar for supported first-release markets: U.S. cash equities (`market: \"us\"` / `\"us-equity\"`), U.S. listed options regular sessions (`\"us-options\"`), and German Xetra cash equities (`\"de\"` / `\"de-xetra\"`). Use for questions like \"is the market open?\", \"when is the next session?\", \"is today a holiday or early close?\", \"why is this quote frozen at 1am ET?\", or risk-manager context before a long market holiday weekend. NOT for prices (use `canary_quote`), NOT for broad futures/FX/bonds/Eurex/crypto calendars, and NOT for per-contract SPX/VIX global-hours nuance — v1 is official exchange calendars only and returns `unknown` outside embedded coverage rather than guessing from weekdays.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"market": json.RawMessage(`{"type":"string","enum":["us","us-equity","us-options","de","de-xetra"],"description":"which official calendar to query: us/us-equity for U.S. stocks and ETFs, us-options for U.S. listed options regular sessions, de/de-xetra for Xetra cash equities"}`),
			"date":   schemaString("optional local market date YYYY-MM-DD; omit to use now"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"maximum":400,"description":"number of calendar days to include in sessions (default 14, capped at 400)"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.MarketCalendarParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.MarketCalendarResult
			if err := conn.Call(ctx, rpc.MethodMarketCalendar, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_chain",
		Title:       "Canary Option Chain",
		Description: "Option chain — use whenever the user asks about option selection, strike grids, expiry IV, implied moves, or trade-structure liquidity (\"AAPL puts\", \"this Friday's chain\", \"call wall on SPY\"). Two shapes: **omit `expiry`** to get the expiry list (each row carries ATM IV, `iv_source`, `iv_quality`, DTE, and the 1-σ implied move `spot × IV × √(DTE/365)` — the desk-standard expected dollar move by expiration, used for earnings sizing and strike selection; daemon caches IV results, second call within ~60 s during RTH is instant; `warning_details[].code=expiry_iv_unavailable` means IV/move is unusable); **provide `expiry`** (YYYY-MM-DD) for the ATM±`width` strike grid. For SPX exact-expiry grids, the daemon uses IBKR's classed sec-def strikes and returns `trading_class`; pass `trading_class:\"SPX\"` or `\"SPXW\"` only when you need the AM/monthly or PM/weekly class explicitly, otherwise leave it empty for auto-selection. For 3-6 month screening, prefer expiry-list filters such as `min_dte:90,max_dte:180` or `target_dte:120` instead of `all_expiries:true`; filters are applied before IV fan-out. Set `require_live_iv:true` only for live-option-IV preflight/readiness checks: outside U.S. option RTH it returns a fast warning instead of spending the IV fan-out budget, and should not be used to value held option positions. The strike grid leads with `tradable_summary` and `liquidity_summary`: live bid/ask leg counts, stale/model-only/subscribe-error/no-quote counts, OI coverage, `options_tradable`, `feed_gap`, `liquidity_grade`, ATM spread, nearest live call/put, tightest live spread, and `recommended_structure_hint` (`stock_only`, `shares_or_spreads`, `calls_ok`, `untradable_chain`). Treat `options_tradable:false` as a hard gate for option structures. The grid also has top-level data_type/session_state/feed_type plus warning_details; outside regular option hours data_type is `closed` even if the underlying stock feed is live. For SPX/VIX, this does not prove the product cannot trade in GTH/ETH/curb; it means this API response did not deliver a complete quote/OI/IV surface, so frozen bid/ask/last are reference context only, never executable liquidity. Per-leg fields include bid/ask/last, prev_close, IV, delta, OI, as_of, data_status (`quoted`, `prev_close`, `model_only`, `no_quote`, `subscribe_error`), iv_status, and oi_status. `call_prev_close` / `put_prev_close` are the option contract's own prior close and are stale context, not executable quotes. `call_oi` / `put_oi` are option open interest from IBKR ticks 27/28 and stay null when the gateway did not push OI within budget; never treat missing OI as zero. Off-hours, `prev_close`, frozen quote fields, and `model_only` legs can be useful context but are not executable quotes. `no_iv` returns the fast skeleton for the expiry list (DTE only). `all_expiries` lifts the default 12-expiry cap (nearest 12 normally — back-half LEAPS rarely on the decision path). NOT for stock-level quotes (use `canary_quote`), NOT for historical bars (use `canary_history`), and NOT for held option valuation or portfolio exposure (use `canary_positions`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol":          schemaString("underlying ticker"),
			"expiry":          schemaString("expiry date YYYY-MM-DD; omit to list available expiries"),
			"width":           json.RawMessage(`{"type":"integer","minimum":0,"description":"strikes ATM ± this count (default 5; 0 returns ATM only)"}`),
			"side":            schemaEnum([]string{"calls", "puts", "both"}, "filter strike legs (default both)"),
			"trading_class":   schemaEnum([]string{"SPX", "SPXW"}, "exact-expiry SPX class selector; omit for auto-selection from IBKR classed sec-def data"),
			"no_iv":           json.RawMessage(`{"type":"boolean","description":"when listing expiries, skip ATM IV (faster)"}`),
			"all_expiries":    json.RawMessage(`{"type":"boolean","description":"when listing expiries, return every listed date (default: nearest 12 with IV)"}`),
			"require_live_iv": json.RawMessage(`{"type":"boolean","description":"expiry-list preflight guard; when true, skip slow IV fan-out outside U.S. option regular hours and return warning_details code live_option_iv_unavailable"}`),
			"min_dte":         json.RawMessage(`{"type":"integer","minimum":0,"description":"expiry-list filter: minimum calendar days to expiration, applied before IV fan-out; useful for 3-6 month option screening"}`),
			"max_dte":         json.RawMessage(`{"type":"integer","minimum":0,"description":"expiry-list filter: maximum calendar days to expiration, applied before IV fan-out"}`),
			"target_dte":      json.RawMessage(`{"type":"integer","minimum":0,"description":"expiry-list filter: return the listed expiry closest to this calendar DTE, after min/max DTE filters when present"}`),
		}, []string{"symbol"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol        string `json:"symbol"`
				Expiry        string `json:"expiry"`
				Width         *int   `json:"width"`
				Side          string `json:"side"`
				TradingClass  string `json:"trading_class"`
				NoIV          bool   `json:"no_iv"`
				AllExpiries   bool   `json:"all_expiries"`
				RequireLiveIV bool   `json:"require_live_iv"`
				MinDTE        int    `json:"min_dte"`
				MaxDTE        int    `json:"max_dte"`
				TargetDTE     int    `json:"target_dte"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Symbol == "" {
				return nil, fmt.Errorf("symbol is required")
			}
			if in.Expiry == "" {
				var res rpc.ChainExpiriesResult
				params := rpc.ChainExpiriesParams{
					Symbol:        strings.ToUpper(in.Symbol),
					WithIV:        !in.NoIV,
					AllExpiries:   in.AllExpiries,
					RequireLiveIV: in.RequireLiveIV,
					MinDTE:        in.MinDTE,
					MaxDTE:        in.MaxDTE,
					TargetDTE:     in.TargetDTE,
				}
				if err := conn.Call(ctx, rpc.MethodChainExpiries, params, &res); err != nil {
					return nil, err
				}
				return json.Marshal(res)
			}
			width := 5
			if in.Width != nil {
				width = *in.Width
			}
			if in.Side == "" {
				in.Side = "both"
			}
			var res rpc.ChainResult
			params := rpc.ChainFetchParams{Symbol: strings.ToUpper(in.Symbol), Expiry: in.Expiry, Width: width, Side: in.Side, TradingClass: strings.ToUpper(in.TradingClass)}
			if err := conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_history",
		Title:       "Canary History",
		Description: "Daily OHLCV bars for an equity / ETF symbol. Use for trend / moving-average / lookback questions (\"is AAPL above its 50-DMA?\", \"what's the 90-day range?\"). Non-trading days are skipped, so the row count is typically smaller than `days`. NOT for intraday bars (not exposed today), NOT for options (use `canary_chain`), NOT for the live current price (use `canary_quote`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": schemaString("equity / ETF ticker, case-insensitive (e.g. \"AAPL\", \"spy\")"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"description":"calendar-day lookback (default 90); the returned row count is smaller because non-trading days are skipped"}`),
		}, []string{"symbol"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.HistoryDailyParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Symbol == "" {
				return nil, fmt.Errorf("symbol is required")
			}
			in.Symbol = strings.ToUpper(in.Symbol)
			var res rpc.HistoryDailyResult
			if err := conn.Call(ctx, rpc.MethodHistoryDaily, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_technical",
		Title:       "Canary Technical Screen",
		Description: "One-call technical and relative-strength screen for equity / ETF symbols. Use for weekly stock screening questions such as \"is IREN above its 50/200-DMA?\", \"rank these names vs SPY\", \"is this extended from the 200-DMA?\", or \"does this pass liquidity?\" Returns price, SMA50/SMA200, percent distance from those moving averages, 21/63/126-trading-bar returns, 63/126-bar RS versus the benchmark (symbol return minus benchmark return), ATR14/ATR%, avg_volume_20d, avg_dollar_volume_20d, trend_state, data_quality, and missing_reasons. Values ending in `_pct` and `return_*` are decimal fractions (0.10 = 10%). For German/Xetra equities set `market:\"de\"`; for ETF resolver gaps use `primary_exchange:\"ARCA\"` or an explicit exchange/currency. Large batches can return partial rows with warning_details, so agents should cap screening batches and drop rows with data_quality != ok. Uses daily bars only; not for live quotes (use `canary_quote`) and not for options (use `canary_chain`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols":          json.RawMessage(`{"type":"array","items":{"type":"string"},"minItems":1,"description":"ticker symbols, e.g. [\"AAPL\",\"MSFT\",\"NVDA\"]"}`),
			"benchmark":        schemaString("relative-strength benchmark, default SPY"),
			"lookback_days":    json.RawMessage(`{"type":"integer","minimum":30,"maximum":800,"description":"calendar-day history lookback; default 420, enough for 200-DMA and 126 trading-bar returns"}`),
			"market":           json.RawMessage(`{"type":"string","enum":["us","de"],"description":"optional route for symbols, not the benchmark; omit/use us for SMART/USD, use de for Xetra/IBIS EUR equities"}`),
			"exchange":         schemaString("optional IBKR exchange override for symbols, e.g. SMART or IBIS"),
			"primary_exchange": schemaString("optional primary-exchange hint for symbols, e.g. ARCA for ETFs or IBIS for Xetra"),
			"currency":         schemaString("optional ISO currency override for symbols, e.g. USD or EUR"),
		}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.TechnicalParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if len(in.Symbols) == 0 {
				return nil, fmt.Errorf("symbols is required and must be non-empty")
			}
			var res rpc.TechnicalResult
			if err := conn.Call(ctx, rpc.MethodTechnical, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_market_events",
		Title:       "Canary Market Events",
		Description: "Read observed market-event flags for held or requested stock/ETF symbols: borrow inventory tightness from IBKR shortable-share data when available, extreme annualized borrow fee from IBKR short-stock availability when observed, Reg SHO threshold-list membership from official Nasdaq files, and active/recent LULD or regulatory/news halts from Nasdaq's trade-halt feed. Use when the user asks whether a held name has market-structure, borrow, threshold, LULD, or halt context that should annotate protection proposals or underlyings. Returns typed flags with `status`, `severity`, `role`, source/as-of metadata, source_health, warning_details, and a semantic fingerprint. Each `source_health[]` row can carry durable `refresh_state`, `next_attempt`, and a redacted typed `last_failure` (`code`, `stage`, `failed_at`, `retryable`); respect backoff/not-due state and do not parse `notes` for failure classification. Unknown or unavailable data stays unknown/null and must not be treated as inactive; absence of Nasdaq Reg SHO flags is not proof for non-Nasdaq threshold feeds. This tool is read-only and does NOT place, preview, submit, modify, cancel, size, or recommend opening exposure. NOT for current prices (use `canary_quote`), NOT for held position sizing/P&L (use `canary_positions`), and NOT for broad-market regime (use `canary_regime`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols": json.RawMessage(`{"type":"array","items":{"type":"string"},"description":"optional stock/ETF symbols to evaluate, e.g. [\"AAPL\",\"GME\"]; omit to use held underlyings from the daemon positions snapshot"}`),
			"symbol":  schemaString("optional single symbol or comma-separated symbols; equivalent to symbols for simple calls"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.MarketEventsParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.MarketEventsResult
			if err := conn.Call(ctx, rpc.MethodMarketEventsSnapshot, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_scan",
		Title:       "Canary Market Scanner",
		Description: "Run a market scanner. Three call shapes: (1) preset by name — `{preset: \"top-movers\"}` — for the configured shortcuts; (2) ad-hoc — `{type: \"HIGH_LAST_VS_EMA50\", exchange: \"STK.US.MAJOR\", instrument: \"STK\"}` for US stock breakouts, `{type: \"MOST_ACTIVE_USD\", exchange: \"STK.US.MAJOR\", instrument: \"STK\"}` for broad liquid activity, or `{type: \"HIGH_VS_52W_HL\", exchange: \"STK.EU.IBIS\", instrument: \"STOCK.EU\"}` for German/Xetra stocks; (3) empty `{}` — enumerates the configured presets so the agent can pick one. For common known scan codes such as `HIGH_LAST_VS_EMA50`, `HIGH_LAST_VS_EMA200`, `HIGH_VS_52W_HL`, `MOST_ACTIVE_USD`, and `HOT_BY_VOLUME`, call this tool directly and reserve `canary_scan_params` for unfamiliar codes, regional uncertainty, or unsupported-code/location errors. Each row is enriched with last/prev_close/change/change_pct/volume/iv/week_52_high/week_52_low plus instrument_tags and data_type/feed_type/price_at/price_as_of/volume_phase/warning_details via per-row market-data subscriptions the daemon issues automatically (IBKR's scanner protocol returns only rank+symbol). `instrument_tags` flags known ETFs/leveraged ETPs that IBKR may still return from stock scans; when the user asks for non-ETF single-name ideas, drop rows tagged `etf` or `leveraged_etp`. Missing tags mean unknown, not confirmed common stock. Scanner enrichment can be the slowest step because it briefly subscribes to each returned row; use small `limit` values and stop after one weak/noisy scan instead of stacking rescue scans. Use `min_price`, `min_volume`, `min_dollar_volume`, `exclude_penny`, and `require_live` to suppress micro-cap/off-hours noise before the result reaches the agent. Nil fields mean the gateway didn't deliver the corresponding tick within the enrichment window — common off-hours, and IV is nil for symbols without actively-traded options. Ad-hoc rows are capped at 50.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"preset":            schemaString("preset name from `canary_scan` with no args (e.g. \"top-movers\"); omit for ad-hoc or list mode"),
			"type":              schemaString("ad-hoc scanCode (e.g. \"HIGH_LAST_VS_EMA50\", \"HIGH_VS_52W_HL\", \"MOST_ACTIVE_USD\") — required with `exchange` when no `preset` is given"),
			"exchange":          schemaString("ad-hoc locationCode (e.g. \"STK.US.MAJOR\" or \"STK.EU.IBIS\") — required with `type` when no `preset` is given"),
			"instrument":        schemaString("IBKR scanner instrument for ad-hoc scans; defaults to STK for US stocks, use STOCK.EU for European stock locations such as STK.EU.IBIS"),
			"limit":             json.RawMessage(`{"type":"integer","minimum":1,"description":"max rows; preset default when omitted; ad-hoc capped at 50"}`),
			"min_price":         json.RawMessage(`{"type":"number","minimum":0,"description":"drop enriched rows whose last price is below this value; rows without last price fail this filter"}`),
			"min_volume":        json.RawMessage(`{"type":"integer","minimum":0,"description":"drop enriched rows whose current share volume is below this value; rows without volume fail this filter"}`),
			"min_dollar_volume": json.RawMessage(`{"type":"number","minimum":0,"description":"drop enriched rows whose last×volume dollar-volume is below this value; rows without last or volume fail this filter"}`),
			"require_live":      json.RawMessage(`{"type":"boolean","description":"drop rows whose quote context is off-hours, stale, previous-close-only, or otherwise not a usable live quote"}`),
			"exclude_penny":     json.RawMessage(`{"type":"boolean","description":"drop enriched rows below $5, equivalent to min_price at least 5"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.ScanRunParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Preset == "" && in.Type == "" && in.Exchange == "" {
				var res rpc.ScanListResult
				if err := conn.Call(ctx, rpc.MethodScanList, nil, &res); err != nil {
					return nil, err
				}
				return json.Marshal(res)
			}
			var res rpc.ScanResult
			if err := conn.Call(ctx, rpc.MethodScanRun, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_scan_params",
		Title:       "Canary Scanner Parameters",
		Description: "Discover the scanner catalog this IBKR gateway supports: every scanCode (the `type` for ad-hoc `canary_scan`) and every locationCode (`exchange`), plus the instrument types each scanCode applies to. Use this before composing an ad-hoc scan — the catalog varies by gateway version, market-data permissions, and region. Pass `instrument: \"STK\"` to narrow scan_types to US stocks or `instrument: \"STOCK.EU\"` for European stocks; pass `include_raw_xml: true` only when you need a field not surfaced in the parsed result (the XML payload is ~200 KB). NOT for actually running a scan (use `canary_scan` — including `{}` to enumerate configured presets), and NOT for quotes or technicals on known symbols (use `canary_quote` or `canary_technical`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"instrument":      schemaString("filter scan_types to those valid for this instrument (e.g. \"STK\", \"STOCK.EU\", \"OPT\", \"ETF\"); empty returns all"),
			"include_raw_xml": json.RawMessage(`{"type":"boolean","description":"include the gateway's raw XML payload (~200 KB); default false"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.ScanParamsParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.ScanParamsResult
			if err := conn.Call(ctx, rpc.MethodScanParams, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_breadth",
		Title:       "Canary S&P 500 Breadth",
		Description: "S&P 500 market-breadth readings for the risk-regime dashboard. Use for questions about the *market's internals* — \"how many S&P names are above their 50-DMA?\", \"is this a narrow rally?\", \"what's the new-high/new-low spread?\". S&P 500 constituents only — NDX, RUT, sector-specific, or single-stock breadth are NOT supported. Returns three readings every call: `pct_above_50dma` (the percentage of S&P 500 constituents trading above their 50-day SMA — the tactical signal), `pct_above_200dma` (the slower companion that catches cyclical tops cleanly), and `new_highs_today`/`new_lows_today` (constituent counts of names making fresh 52-week highs/lows), plus the derived `net_new_highs_pct`. The classic narrow-rally pattern — SPX near highs with `net_new_highs_pct` near zero or negative — fires when a few mega-caps carry the index while the median name is rolling over. IBKR does not redistribute S&P DJI's S5FI or the equivalent breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (method: `constituent-fanout-50/200dma+nh-v2`). A once-daily refresh post-close (16:35 ET) slides each name's 200-bar window forward and updates a 252-bar rolling max/min; readers see a cached snapshot. **Cold start (no cache yet) takes ~74 min** — IBKR's historical-data pacing limit caps the fan-out at ~6 names/min sustained, so the response carries `state: \"computing\"` until the cache is built. Pulling 200 bars per constituent instead of 50 doesn't cost more requests; the pacing limit is per-request, not per-bar, so the cold-start budget is unchanged. After cold-start the cache persists across daemon restarts and every subsequent call is instant. Canary does not band these readings green/yellow/red; the caller picks the cutoffs. Suggested bands: 50-DMA — `>55` green / `40-55` yellow / `<40` with SPX at highs red. 200-DMA — `>60` green / `40-60` yellow / `<40` red (calibrated to the post-Mag-7 era; StockCharts' 70/30 default fires red far too often).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"history_days": json.RawMessage(`{"type":"integer","minimum":1,"maximum":90,"description":"trailing daily-series length (default 30)"}`),
			"timeout_ms":   json.RawMessage(`{"type":"integer","minimum":100,"description":"per-snapshot wait budget when the engine has a fresh value but the wire envelope is still being assembled (default 5000 ms); does not affect the multi-minute cold-start fan-out"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.BreadthSPXParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.BreadthSPXResult
			if err := conn.Call(ctx, rpc.MethodBreadthSPX, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_gamma",
		Title:       "Canary Dealer Gamma",
		Description: "Dealer-gamma market-structure snapshot for SPX/SPXW, with SPY as corroborating ETF context when usable. Use for questions like \"where is dealer gamma?\", \"did the signed profile find a zero-gamma crossing?\", \"is the modeled book long-gamma or short-gamma?\", or \"where are the largest gamma concentrations?\" NOT for portfolio Greeks (use `canary_positions`) and NOT for options chains/quotes (use `canary_chain` / `canary_quote`). SPX is the stable production signal for S&P 500 dealer gamma; a fresh, rankable SPX result remains the main market-structure read when SPY is throttled or unavailable. SPY-only is a labeled proxy, not the canonical S&P dealer-gamma signal. The ready result leads with `quality.rankability`: `rankable` means fresh and covered enough to treat as a market-structure signal; `context_only` means show for awareness but do not treat as the active gamma read; `blocked` or `unavailable` means gamma is not a usable signal in this snapshot. `quality` also carries session key, current session, age/max age, coverage percentages, horizon/skew/derived-IV/concentration gates, blockers, and context notes. Missing 0DTE is disclosed in horizon coverage and warnings but does not by itself make a healthy SPX result context-only when 1-7DTE and term buckets are present; after the expiring SPXW series closes, 0DTE can be absent while the broader SPX surface remains usable. `summary` then gives `primary_statement`, `zero_gamma_status` (`crossing`, `none_in_window`, `mixed`, `mixed_degraded`, `unavailable`), `regime`, `confidence`, `not_advice`, and per-index summaries. In combined scope there is no top-level combined zero-gamma price because SPY and SPX use different price scales; read `summary.per_index.SPY` and `summary.per_index.SPX` for per-underlying spot, zero, swept range, regime, and GEX leg counts. If SPY cannot produce usable option OI/IV/GEX, the daemon may return a canonical SPX result with `warning_details[].code` starting `spy_unavailable:`; that warning is context, not a blocker for rankable SPX. If SPX is unavailable, the daemon may return a degraded SPY proxy with `spx_unavailable:`; stale SPX fallback is marked with `spx_cache_fallback`, and failed cache refreshes with `refresh_failed:`. `gamma_total_abs` and `top_strikes` are sign-agnostic concentration/magnitude diagnostics. `leg_count` means legs with non-zero OI-weighted GEX; `priced_leg_count` means legs that priced/fit IV but may not have usable OI. Missing OI is unknown, never zero: SPY OI can be absent outside regular option hours, while SPX OI should normally be session-stable and missing SPX OI is data-quality evidence in any session. Non-fatal data-quality issues are in `warning_details` with `{code, scope, severity, message, impact, action}`; raw warning tokens are not part of the JSON contract. By default profile arrays are stripped to keep MCP responses compact; set `include_profiles: true` only when charting the sweep. First call of a NY session may return `status: \"computing\"` with progress/ETA; set `wait_ms` to wait. The signed zero-gamma convention is a regime hint, not advice or a trade level.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"wait_ms":          json.RawMessage(`{"type":"integer","minimum":0,"description":"block up to this many ms for the result; 0 (default) returns the current status immediately"}`),
			"force":            json.RawMessage(`{"type":"boolean","description":"diagnostics-only: start a fresh refresh; when a good cached value is already serving, keep serving it and promote the forced run only on success; default false"}`),
			"scope":            json.RawMessage(`{"type":"string","enum":["spy","spx","spy+spx"],"description":"which underlying(s) to compute: 'spy+spx' (default request; SPX/SPXW is canonical and SPY is added when fresh/rankable, so SPY throttling does not block a rankable SPX result); 'spx' (canonical SPX-only production signal); 'spy' (SPY-only proxy/context, not the canonical S&P dealer-gamma signal). Omit for the default view. Mirrors the CLI's --only flag."}`),
			"include_profiles": json.RawMessage(`{"type":"boolean","description":"include full sweep profile arrays for charting; default false keeps the response compact for agents and tooling"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.GammaZeroSPXParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			// Normalise/validate scope at the MCP edge so a bad value
			// surfaces as a tool error rather than the daemon's wire
			// error envelope — clients distinguish the two.
			switch strings.ToLower(strings.TrimSpace(in.Scope)) {
			case "", rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeCombined:
				in.Scope = strings.ToLower(strings.TrimSpace(in.Scope))
			default:
				return nil, fmt.Errorf("scope must be one of 'spy', 'spx', 'spy+spx' (got %q)", in.Scope)
			}
			var res rpc.GammaZeroSPXResult
			if err := conn.Call(ctx, rpc.MethodGammaZeroSPX, in, &res); err != nil {
				return nil, err
			}
			if !in.IncludeProfiles {
				rpc.StripGammaProfiles(&res)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:  "canary_regime",
		Title: "Canary Risk Regime",
		Description: strings.Join([]string{
			"Broad-market stress-lifecycle snapshot — single-call, non-advisory answer for \"how does the market regime look today?\", \"is this a risk-on or risk-off tape?\", \"are we close to stress thresholds?\", or \"give me the daily-check dashboard.\"",
			"Use this when the user wants the market's current evidence balance across equity vol (VIX/VIX3M + VVIX), credit (HYG/SPY + official HY/IG OAS), funding stress (CP/T-bill spread), FX carry proxy, dealer gamma, and S&P 500 breadth.",
			"NOT for account/portfolio action, trade selection, hedging, sizing, execution, or a probability forecast — use `canary_stress` when the user needs market weather combined with held portfolio shape, and use `canary_positions` / `canary_account` for held-risk inspection.",
			"The compact MCP response leads with `fingerprint` (semantic identity for the classified regime state), `posture` (canonical display policy: `label`, `tone`, `stage`, `severity`, `readiness`, `confidence`, and `evidence`), market-scoped `lifecycle` (`scope: \"market\"`, `stage`, `severity`, `readiness`, `timing`, `confidence`, `evidence[]`, `confirmed_by[]`, `unconfirmed[]`, `governors[]`, lifecycle fingerprint, and `not_execution`), `source_health[]` (per cluster `as_of`, `max_age_seconds` staleness policy, stale/degraded/partial status, confidence, semantic-bucket fingerprint stability, durable `refresh_state`/`next_attempt`, and redacted typed `last_failure` code/stage), `summary`, `data_quality`, and `composite` raw + cluster counts including `cluster_eligible_red_count` / `cluster_provisional_red_count`, then the eight indicator rows.",
			"Lifecycle stages are `quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`, or `data_quality`.",
			"A red row CONFIRMS stress only when its `eligibility.eligible` is true (depth + persistence + cadence-freshness gates); provisional reds (`eligibility.reasons` names the failed gate: depth_below_min, streak_N_of_M, data_not_due, data_overdue) stay visible and never rescue another cluster or reach `confirmed_by`. A depth- or persistence-provisional red may drive `early_warning` only while every required input is usable. Missing, unavailable, broken, or cadence-overdue evidence instead yields `stage:\"data_quality\"`, `readiness:\"blocked\"`, low confidence, and the canonical label `Market state undefined — data incomplete`; it is not a market warning. Exact typed `not_due` cadence — VIX3M outside its dissemination window, gamma, breadth, or USD/JPY while IDEALPRO is shut for the weekend or its daily changeover — remains context rather than a source defect. Off-window the served VIX3M is Cboe's published dated close rather than the broker quote, and `vix3m_cross_check` reports what comparing the two established: `agree`, `official_only`, and `pending_publication` keep the row `not_due`, while `disagree` (the broker leg is not the close it claims) and `unverified` (no usable official close) drop it to overdue. Read that field before treating an off-window volatility row as healthy.",
			"`lifecycle.governors[]` discloses severity downgrades applied after stage selection: while threshold sets are `pending_backtest`, heuristic evidence without a fresh tape co-sign (SPY ≤ −1.5%, VIX +10%, or a same-session term inversion) reads one severity rung down (`confirmed_stress` → watch; 3-red panic → act); a confirming cluster with impaired data quality also caps severity. Check `governors[]` before concluding the engine is ignoring red rows — severity watch beside two red rows is governed policy, not a bug.",
			"`posture.tone` is the canonical display tone and follows governed severity, not just stage: `confirmed_stress` with `severity:\"watch\"` is watch/amber, while act-grade stress remains stress/red and full risk-off remains risk_off.",
			"Per-row fields include raw measurements, `status`, `band`, `band_reason`, `thresholds` (heuristic + pending_backtest, plus `trip`: the short form of the red band, meant to be shown next to the reading), `eligibility` (`eligible`, `latched`, `reasons[]`), `freshness` (`class` fresh/not_due/overdue + served `max_age_seconds`), `as_of` (`label`, freshness/source/time/date), `streak` (consecutive NY trading sessions in band), and per-scalar `*_quality` provenance.",
			"`warning_details` gives scoped prose for unavailable/stale/computing/context-only rows with `{code, scope, severity, message, impact, action}`; do not parse opaque error strings when this field is present.",
			"MOVE/rates-vol is intentionally absent until a verified IBKR contract/source exists; do not infer it from ETFs or futures.",
			"Methodology prose is omitted from MCP for compactness; use `spec_doc` or CLI `canary regime --explain` for full threshold notes.",
			"Gamma embeds the compact `canary_gamma` envelope with profiles stripped: `envelope.result.quality.rankability` says whether gamma is fresh and covered enough to be the active market-structure read.",
			"Only `rankable` gamma contributes a band, cluster count, lifecycle evidence, or `confirmed_by`; `context_only`, `blocked`, and `unavailable` gamma are awareness/data-quality evidence only.",
			"The latest completed-options-session gamma compute may serve before the next options open as typed `status: stale`, `freshness.class: not_due` context: its band stays visible but cannot confirm. No last-good, a missed completed session, or a prior-session result after the options session opens is overdue data-quality evidence, not an `early_warning`.",
			"In combined scope use `envelope.result.summary`, `per_index.SPY`, `per_index.SPX`, `gamma_total_abs`, and `top_strikes`; the signed γ-zero is a regime hint, not a precise level.",
			"Expect gamma/breadth to be `computing` on cold starts and optional `fields_missing` values when a secondary scalar missed the fetch budget or an official daily file is temporarily unavailable.",
		}, " "),
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"view": schemaEnum([]string{rpc.ViewDetail, rpc.ViewMonitor}, "response shape: detail returns the existing compact regime snapshot (default); monitor returns posture, lifecycle, summary, source health, warnings, and compact indicator rows only. Monitor rows carry the reading, its `cluster`, and the row's `thresholds` including `trip`, so a dashboard can print the trigger without the full snapshot"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				View string `json:"view"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.View == "" {
				in.View = rpc.ViewDetail
			}
			if in.View != rpc.ViewDetail && in.View != rpc.ViewMonitor {
				return nil, fmt.Errorf("view must be %q or %q (got %q)", rpc.ViewDetail, rpc.ViewMonitor, in.View)
			}
			var res rpc.RegimeSnapshotResult
			if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
				return nil, err
			}
			if in.View == rpc.ViewMonitor {
				return json.Marshal(rpc.CompactRegimeMonitor(&res))
			}
			rpc.CompactRegimeSnapshot(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:  "canary_stress",
		Title: "Canary Portfolio Stress",
		Description: strings.Join([]string{
			"Live stateless portfolio stress read for scheduled checks every few minutes: it combines broad-market weather from `canary_regime` with the user's current portfolio shape.",
			"Use when the user asks how current market weather interacts with the held portfolio, whether to watch/stage/defend/rebalance/deploy, or when orchestration needs a stable alert fingerprint for this snapshot.",
			"NOT for account-only questions such as cash, buying power, or daily P&L in isolation — use `canary_account` for those. Stress evidence may include margin/P&L facts, but the headline stress action is gated by market confirmation plus portfolio fit. Margin cushion is the one account-only danger this tool answers on its own; see the margin note below.",
			"Returns `action` (`stand_down`, `watch`, `defend`, `rebalance`, `deploy`, `confirm_inputs`), `market_confirmation` (`none`, `partial`, `confirmed`, `blocked`), `portfolio_fit` (`low`, `medium`, `high`, `unknown`), and `input_health` (`ok`, `warming`, `degraded`, `failed`) so agents can explain whether a risk recommendation is market-confirmed or only contextual.",
			"Also returns `direction`, `severity`, `planner_mode_hint` (`none`, `stage`, `defend`, `rebalance`, `deploy`, `confirm_data`), and `planner_readiness` (`none`, `watch`, `prestage`, `ready`, `blocked`) so monitor workflows can explain whether the snapshot is actionable, staged, or data-blocked without parsing prose.",
			"`portfolio.held_stress[]` is a bounded positions-only held-underlying stress surface for material names: held-name daily P&L shock, near-expiry held-option delta concentration, and held-name quote/option bid-ask degradation. It is emitted only when an existing held underlying is material and a stress condition is present.",
			"`protection_coverage` in the alert view and `portfolio.protection_coverage` in the full view summarize stock/ETF stop coverage, unprotected base notional, largest unprotected exposures, and stale `orphaned_order`/`reconcile_required` protective orders; those stale orders are not counted as protection.",
			"`signals[]` carries stable IDs, direction (`defensive`, `constructive`, `rebalance`, `mixed`, `data_quality`), per-signal posture, severity (`observe`, `watch`, `act`, `urgent`), observed values, thresholds, targets, confidence impact, and blocking degraded or stale inputs. Signals are supporting evidence; do not infer a DEFEND action from account-only or portfolio-only signals when top-level `action` says otherwise.",
			"Margin cushion is the exception to that rule. `margin_cushion_low` and `lookahead_cushion_low` stand on their own authority: the broker can force liquidation in a calm market, so Canary raises them as their own urgent alert with no market confirmation, and their severity comes straight from the risk policy. Report a low cushion as urgent even when `action` reads `stand_down`. `portfolio.cushion_trip_pct` is the policy's cushion watch floor, so the reading can be stated against the level that trips it.",
			"`market_indicators[]` lists each regime indicator with `status` (`green`, `amber`, `red`, `context`, `n/a`), `as_of`, `reading`, `trip` (the level that would turn the row red, or for dealer gamma the measured spot and γ-zero pair), and a short decision comment; context-only gamma appears here as `context` evidence rather than degraded input health. A row without a `trip` has no served trigger — do not invent one.",
			"`market.regime_posture` is the canonical market-regime display/policy read from `canary_regime`; render its `label` and `tone` instead of deriving risk-off from raw cluster counts.",
			"`fingerprint` is the semantic alert identity for monitor dedupe/recovery; `source_fingerprints.account`, `.positions`, `.regime`, and `.market_events` record the classified input buckets consumed by this stress run; `source_health[]` records each source's `as_of`, freshness/degraded/stale/partial status, confidence, max-age cadence, semantic-bucket fingerprint stability, durable `refresh_state`/`next_attempt`, and redacted typed `last_failure` (`code`, `stage`, `failed_at`, `retryable`). Respect retry/backoff/not-due state and do not infer failure classes from `notes`.",
			"High-precision policy: market tape is confirmed only by market evidence (SPY/VIX or independent regime clusters), not by margin pressure; DEFEND requires confirmed market pressure, vulnerable portfolio fit, and clean enough input health. Medium/low input health caps the headline at WATCH or CONFIRM_INPUTS.",
			"Standalone portfolio limit breaches and held-underlying stress become rebalance/watch context rather than market-stress alerts; stale account or positions snapshots block dependent margin, P&L, exposure, concentration, held-stress, and option signals with explicit `blocked_by` sources.",
			"Context-only gamma is context/unranked evidence, not degraded input health; blocked, unavailable, degraded, or computing gamma/breadth becomes explicit input-health evidence, not a false safe/false red signal. Stale/degraded/partial confirming clusters cannot upgrade `market_confirmation` to confirmed until refreshed.",
			"Market confirmation and act-grade decisions key on `market.eligible_red_clusters` — reds that passed the regime confirmation gates (depth + persistence + cadence-freshness) — never on raw red counts; `market.unconfirmed_red_cluster_names` lists the visible-but-provisional reds, which hold the stress read at watch.",
			"Works pre-market and after hours by relying on account, positions, regime, and daemon market-event freshness/status metadata; it does not call option chains, scanners, short-interest feeds, paid borrow vendors, or external flow sources, and it refuses to escalate solely on incomplete computed surfaces.",
			"This tool is read-only and does NOT place, preview, submit, modify, cancel, draft, size, or select orders.",
			"NOT for detailed diagnostics — use `canary_regime`, `canary_positions`, or `canary_account` when you need underlying evidence; use `canary_positions` for held-option warnings such as `mark_outside_bid_ask`, `options_closed`, per-leg greeks, quote freshness, and the full by-underlying ledger.",
		}, " "),
		MonitorDescription: "One-call scheduled portfolio risk monitor. Use first for market-regime × held-portfolio state; do not call account/positions/regime separately unless this returns degraded inputs or an action requiring diagnostics. Read-only.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"view": schemaEnum([]string{rpc.ViewFull, rpc.ViewAlert}, "response shape: full returns the existing stress evidence payload (default); alert returns compact monitor headline, source health, portfolio/market summaries including held_stress, protection_coverage, option health, hedge offset, warnings, and non-observe flags"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				View string `json:"view"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.View == "" {
				in.View = rpc.ViewFull
			}
			if in.View != rpc.ViewFull && in.View != rpc.ViewAlert {
				return nil, fmt.Errorf("view must be %q or %q (got %q)", rpc.ViewFull, rpc.ViewAlert, in.View)
			}
			res, positions, err := stress.FetchStressSnapshot(ctx, conn)
			if err != nil {
				return nil, err
			}
			if in.View == rpc.ViewAlert {
				return json.Marshal(rpc.CompactStressAlert(&res, &positions))
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_rules",
		Title:       "Canary Trading Rulebook",
		Description: "Advisory 14-rule daily trading checklist evaluated daemon-side against the live book: per-name exposure cap (with disclosed lower-bound breaches under Greeks gaps), single option-line premium cap (hedge lines get their own higher tier), negative-cash sell-only posture, portfolio extrinsic (theta-rent) budget excluding classified hedge legs, expiry runway on long options, catalyst coverage vs earnings, short options spanning earnings (short calls act; short puts watch, act on assignment notional), pre-earnings size freeze, red-on-green-tape relative weakness, winner-trim into strength, green-day execution nudge, hedge-band integrity (act past twice the band top), exit discipline on long-option loss fences, and non-base-currency FX exposure (watch-only). Rules 3/4/12 thresholds are regime-conditional (calm/early_warning/confirmed sets from the latched regime lifecycle stage; a carried stage evaluates worse-of carried/calm, a never-seen stage uses calm thresholds — both disclosed in row notes). Use when the user asks \"what should I fix today?\", \"which rules am I breaking?\", whether the book breaches the compiled discipline model, or wants a daily discipline review. The compiled model is not itself proof that every threshold has operator approval. Rows are ranked hardest-first (`ranked` indexes: act > watch > unknown, then base-currency impact); statuses are pass/info/watch/act/unknown/not_evaluated where `unknown` means an input was missing (positions pending, earnings unknown, Greeks gaps, FX report absent) — never treat unknown as pass; `observed_is_lower_bound` marks a breach proven from partial data (\"at least X%\"). `earnings[]` shows each name's next earnings date with source (`fetched`, `override`, `verified_terminal`, or `unknown`, with estimated and stale flags); exact-contract `terminal_non_reporting` is explicit not-evaluated/exempt, never pass or a symbol-wide exemption, and conflicting or expired terminal evidence stays unknown. If the optional Wall Street Horizon earnings feed is unavailable because the account lacks its WSH research subscription, Nasdaq remains active; names without a usable date stay unknown, never pass. `input_health[]` is the result-level gate (includes the regime_stage row). Advisory only: verdicts never block orders; order previews may carry matching advisory `rule_*` warnings. NOT for the market-regime × portfolio alert verdict (use `canary_stress`), NOT for executable protective-stop candidates (use `canary_proposals`), and NOT a data source for positions themselves (use `canary_positions`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": json.RawMessage(`{"type":"string","description":"optional underlying symbol (case-insensitive) to narrow per-rule offender lists; portfolio verdicts are unaffected"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol string `json:"symbol"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.RulesResult
			if err := conn.Call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{Symbol: in.Symbol}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_proposals",
		Title:       "Canary Protection Proposals",
		Description: "Read daemon-owned protection proposals for existing positions. Use when the user asks what protective actions Canary currently recommends — broker-side trailing stops (TRAIL/TRAIL LIMIT for stocks/ETFs and, when policy opts in, single-leg option premium trails; each row carries the trail spec, computed initial stop, trail_sizing explanation including dynamic ATR/spread sizing or explicit policy fallback, execution_semantics, stop_risk, and stop_ladder), theta hygiene (close/reduce short-dated options), or single-name risk reduction — or asks why a proposal is blocked (per-row blockers include codes like stock_protection_disabled with remediation text). `execution_semantics` discloses reference side, trigger method, TRAIL market-order conversion, and TRAIL LIMIT non-fill tradeoff; `stop_risk` includes estimated stop loss, % NLV when computable, and a fixed 5% gap/slippage scenario. Estimates are advisory diagnostics, not fill guarantees. This tool can return the latest snapshot or request a refresh, but it is read-only: it does NOT preview, submit, place, modify, cancel, transmit, or expose raw preview tokens. For coverage of existing open stop orders use `canary_positions` with `view:\"risk\"`; for broad risk evidence use `canary_stress` or `canary_regime`; for local order-entry readiness use `canary_trading_status`.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"refresh": json.RawMessage(`{"type":"boolean","description":"when true, ask the daemon to recompute proposals before returning; otherwise returns the latest daemon snapshot"}`),
			"show":    json.RawMessage(`{"type":"boolean","description":"when true, records a shown audit event for returned proposal rows"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Refresh bool `json:"refresh"`
				Show    bool `json:"show"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.TradeProposalSnapshot
			if in.Refresh {
				if err := conn.Call(ctx, rpc.MethodTradeProposalsRefresh, rpc.TradeProposalRefreshParams{Show: in.Show}, &res); err != nil {
					return nil, err
				}
			} else if err := conn.Call(ctx, rpc.MethodTradeProposalsSnapshot, rpc.TradeProposalSnapshotParams{Show: in.Show}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_opportunities",
		Title:        "Canary Opportunities",
		Description:  "Read daemon-owned opportunities for existing positions. Use when the user asks whether Canary sees mechanical portfolio opportunities, especially long option exercise candidates where exercise may beat selling the option bid or reduce an illiquid risk position. Option-exercise rows include `post_exercise_risk`: before/after underlying share exposure, whether exercise opens/increases/flips/reduces/closes risk, current protection coverage state when available, and whether a protection review is needed after exercise. That context is advisory and does not authorize exercise. This tool can return the latest snapshot or request a refresh, but it is read-only: it does NOT preview exercise, submit exercise, place, modify, cancel, transmit, or expose submit-capable tokens. For holdings and current protection coverage use `canary_positions`; for candidate protective stops use `canary_proposals`; for local broker-write readiness use `canary_trading_status`.",
		ReadOnlyHint: new(true),
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"refresh": json.RawMessage(`{"type":"boolean","description":"when true, ask the daemon to recompute opportunities before returning; otherwise returns the latest daemon snapshot"}`),
			"show":    json.RawMessage(`{"type":"boolean","description":"when true, records a shown audit event for returned opportunity rows"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Refresh bool `json:"refresh"`
				Show    bool `json:"show"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.OpportunitySnapshot
			if in.Refresh {
				if err := conn.Call(ctx, rpc.MethodOpportunitiesRefresh, rpc.OpportunityRefreshParams{Show: in.Show}, &res); err != nil {
					return nil, err
				}
			} else if err := conn.Call(ctx, rpc.MethodOpportunitiesSnapshot, rpc.OpportunitySnapshotParams{Show: in.Show}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_size",
		Title:       "Canary Position Size",
		Description: "Fixed-fractional position sizing pegged to live NLV. Pure math against the account snapshot — never proposes or executes an order. Pass an optional target to also get the R-multiple (reward:risk) and breakeven win rate. NOT for drafting an actual order ticket (use `canary_order_preview` after `canary_trading_status` shows readiness), NOT for protective stops on existing positions (use `canary_proposals`), and NOT for account cash/margin context on its own (use `canary_account`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol":   schemaString("ticker the trade plan applies to (for reporting only)"),
			"entry":    json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"planned entry price per share, quote currency"}`),
			"stop":     json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"planned stop price per share, quote currency"}`),
			"target":   json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"optional take-profit price; when set, response includes r (reward:risk multiple) and breakeven_win_rate"}`),
			"risk_pct": json.RawMessage(`{"type":"number","exclusiveMinimum":0,"maximum":100,"description":"percent of NLV to risk (default 1.0)"}`),
			"side":     schemaEnum([]string{"long", "short"}, "trade direction (default long)"),
			"lot":      json.RawMessage(`{"type":"integer","minimum":1,"description":"round shares down to this multiple (default 1; use 100 for one option contract's worth of stock)"}`),
			"fx":       json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"quote-currency units per 1 base-currency unit (default 1.0 for same-currency trades)"}`),
		}, []string{"symbol", "entry", "stop"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol  string  `json:"symbol"`
				Side    string  `json:"side"`
				Entry   float64 `json:"entry"`
				Stop    float64 `json:"stop"`
				Target  float64 `json:"target"`
				RiskPct float64 `json:"risk_pct"`
				Lot     int     `json:"lot"`
				FX      float64 `json:"fx"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Side == "" {
				in.Side = "long"
			}
			if in.RiskPct == 0 {
				in.RiskPct = 1.0
			}
			if in.Lot == 0 {
				in.Lot = 1
			}
			if in.FX == 0 {
				in.FX = 1.0
			}
			var acct rpc.AccountResult
			if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
				return nil, err
			}
			res, err := risk.ComputeSize(risk.SizeInput{
				Symbol:      in.Symbol,
				Side:        in.Side,
				Entry:       in.Entry,
				Stop:        in.Stop,
				Target:      in.Target,
				RiskPct:     in.RiskPct,
				Lot:         in.Lot,
				FX:          in.FX,
				NLV:         acct.NetLiquidation,
				BuyingPower: acct.BuyingPower,
				Currency:    acct.BaseCurrency,
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
}

func buildWatchlistQuoteResult(ctx context.Context, conn *dial.Conn, snap *watchlist.Snapshot, timeoutMs int, includePositions bool) (*rpc.WatchlistResult, error) {
	if timeoutMs <= 0 {
		timeoutMs = int((5 * time.Second).Milliseconds())
	}
	res := &rpc.WatchlistResult{
		Name:    snap.Name,
		Symbols: append([]string(nil), snap.Symbols...),
		Rows:    make([]rpc.WatchlistRow, 0, len(snap.Symbols)),
		AsOf:    time.Now(),
	}
	holdings := map[string]*rpc.WatchlistHolding{}
	if includePositions {
		var pos rpc.PositionsResult
		if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "stk"}, &pos); err == nil {
			for _, p := range pos.Stocks {
				// The non-option slice carries every secType that is not OPT.
				// Same filter as the CLI join: a bond sharing an equity's
				// ticker must not masquerade as that stock holding nor steer
				// the quote contract's currency and exchange.
				if !rpc.PositionQuotesAsStock(p) {
					continue
				}
				holdings[strings.ToUpper(p.Symbol)] = &rpc.WatchlistHolding{
					Quantity:      p.Quantity,
					AvgCost:       p.AvgCost,
					Mark:          p.Mark,
					MarketValue:   p.MarketValue,
					UnrealizedPnL: p.UnrealizedPnL,
					DailyPnL:      p.DailyPnL,
					Exchange:      p.Exchange,
					Currency:      p.Currency,
				}
			}
		}
	}
	for _, sym := range snap.Symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:         watchlistQuoteContract(sym, holdings[strings.ToUpper(sym)]),
			TimeoutMs:        timeoutMs,
			IncludeLiquidity: true,
		}
		row := rpc.WatchlistRow{}
		if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			row.Quote = rpc.Quote{Symbol: sym}
			row.Error = err.Error()
		} else {
			row.Quote = q
		}
		if h, ok := holdings[strings.ToUpper(sym)]; ok {
			row.Holding = h
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func ensureDaemonReachable(ctx context.Context, conn *dial.Conn) error {
	if conn == nil {
		return fmt.Errorf("daemon connection required")
	}
	var health rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &health); err != nil {
		return fmt.Errorf("daemon reachability check failed: %w", err)
	}
	return nil
}

func watchlistQuoteContract(sym string, h *rpc.WatchlistHolding) rpc.ContractParams {
	c := rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"}
	if h == nil {
		return c
	}
	if h.Currency != "" {
		c.Currency = h.Currency
	}
	if h.Exchange != "" {
		if strings.EqualFold(h.Exchange, "IBIS") && strings.EqualFold(c.Currency, "EUR") {
			c.Market = "de"
		} else {
			c.Exchange = h.Exchange
		}
	}
	return c
}

func normalizeMCPPreviewOrderType(raw string, hasTrail, hasLimitOffset bool) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, "_", " ")
	normalized = strings.ReplaceAll(normalized, "-", " ")
	normalized = strings.Join(strings.Fields(normalized), " ")
	switch normalized {
	case rpc.OrderTypeLMT:
		if hasTrail || hasLimitOffset {
			return "", fmt.Errorf("LMT order_type cannot include trail fields")
		}
		return normalized, nil
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return normalized, nil
	case "":
		if hasLimitOffset {
			return rpc.OrderTypeTRAILLIMIT, nil
		}
		if hasTrail {
			return rpc.OrderTypeTRAIL, nil
		}
	}
	if normalized == "" {
		return rpc.OrderTypeLMT, nil
	}
	return "", fmt.Errorf("order_type must be LMT, TRAIL, or TRAIL LIMIT")
}

// sanitizeOrderPreviewWhatIfForMCP replaces broker-authored WhatIf prose with
// fixed Canary copy before the result crosses the MCP boundary. Broker free
// text is untrusted data and must not reach an agent surface where it could
// smuggle instructions or authorization claims; the typed status, margin
// numbers, and eligibility fields carry the decision signal, and the verbatim
// broker text stays on the CLI surface and in the local journal for the human.
func sanitizeOrderPreviewWhatIfForMCP(res *rpc.OrderPreviewResult) {
	res.WhatIf.AdvancedRejectJSON = ""
	switch res.WhatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		res.WhatIf.Message = "Broker WhatIf accepted the draft; the margin impact fields below are the broker's typed numbers."
	case rpc.OrderWhatIfStatusRejected:
		res.WhatIf.Message = "Broker WhatIf rejected this draft. The verbatim broker reason is untrusted free text and is withheld from this surface; read it with the CLI order preview or in the local order journal."
	default:
		res.WhatIf.Message = "Broker WhatIf did not return an accepted preview; local detail is on the CLI order-preview surface."
	}
	if res.WhatIf.Margin != nil && res.WhatIf.Margin.WarningText != "" {
		res.WhatIf.Margin.WarningText = "Broker margin warning present; the verbatim text is untrusted and withheld from this surface — read it with the CLI order preview."
	}
}

// orderJournalProseWithheld replaces non-empty free-text detail on order
// journal views before they cross the MCP boundary.
const orderJournalProseWithheld = "Free-text detail is withheld from agent surfaces; the typed lifecycle, error-code, and reconciliation fields carry the decision signal. Read the verbatim text with the CLI order status or in the local order journal."

// sanitizeOrderViewForMCP and sanitizeOrderEventsForMCP blank free-text fields
// on journal-reduced order state before it crosses the MCP boundary. The wire
// contract does not track message provenance — broker-error text (which can
// carry concatenated advanced-reject JSON) and daemon-composed display notes
// share the same fields — so the boundary fails closed and withholds all of
// it. Typed states, error codes, and reconciliation fields remain.
func sanitizeOrderViewForMCP(v *rpc.OrderView) {
	v.WhyHeld = ""
	if v.LastMessage != "" {
		v.LastMessage = orderJournalProseWithheld
	}
}

func sanitizeOrderEventsForMCP(events []rpc.OrderEvent) {
	for i := range events {
		events[i].WhyHeld = ""
		events[i].Message = ""
	}
}

func sanitizeOrdersOpenForMCP(res *rpc.OrdersOpenResult) {
	for i := range res.Orders {
		sanitizeOrderViewForMCP(&res.Orders[i])
	}
}

func sanitizeOrdersHistoryForMCP(res *rpc.OrdersHistoryResult) {
	for i := range res.Orders {
		sanitizeOrderViewForMCP(&res.Orders[i].Order)
		sanitizeOrderEventsForMCP(res.Orders[i].Events)
	}
}

func sanitizeOrderStatusForMCP(res *rpc.OrderStatusResult) {
	sanitizeOrderViewForMCP(&res.Order)
	sanitizeOrderEventsForMCP(res.Events)
}

// ExcludedCLI is the set of cli.Commands() names that intentionally have no
// MCP tool counterpart. The parity test consults this so adding a new CLI
// command without an MCP tool fails the gate unless the exclusion is recorded.
//
// `quote` is intentionally absent from this map — it has both a snapshot
// MCP tool (canary_quote) and, for the `--watch` mode, the MCP resource
// template canary://quote/{symbol} gated by TestStreamingParity in
// resources_test.go.
var ExcludedCLI = map[string]string{
	"version":  "info-only CLI verb; not useful as a tool call",
	"mcp":      "transport server mode; the MCP host starts this process, no LLM should call it as a tool",
	"daemon":   "local background service mode; autospawned by CLI/MCP clients and not an agent operation",
	"app":      "local mobile/PWA service mode with browser pairing and Web Push state; not a broker-data MCP tool",
	"setup":    "local configuration verb (writes claude_desktop_config.json); not a daemon RPC, no LLM should ever call it",
	"update":   "binary-management verb (replaces the canary binary from GitHub releases); not a daemon RPC, must stay user-triggered for trust-boundary reasons",
	"restart":  "local process-management verb (signals daemon processes); useful for humans and scripts, but not a broker-data MCP tool",
	"stop":     "local process-management verb (stops the daemon and app the caller is talking through); a tool call that ends order tracking and phone alerts belongs to the human at the terminal",
	"backtest": "offline research harness over local JSONL fixtures; not a live broker/MCP operation",
	"purge":    "emergency local-terminal purge-book workflow; deliberately withheld from MCP until execution/review safety is proven",
	"policy":   "risk-constitution surface deferred from MCP in phase 1 (internal-docs/design/risk-policy.md): its writes are human-only governance acts the daemon rejects from agents, and the read view ships CLI-first; revisit after the phase-2 manual cadence",
	"recon":    "post-trade reconciliation surface deferred from MCP in phase 3a (internal-docs/design/post-trade-truth.md): dismiss/sign-off are human-only governance acts and the read view ships CLI-first, same posture as `policy`; revisit together with it",
	"brief":    "daily brief MCP exposure is explicitly deferred until after phase 2; agent CLI renders are read-only and never stamp",
	"alerts":   "alert-source coverage diagnostic for the operator watching push-delivery readiness; CLI-first like `brief` — an agent inspecting alert internals should read the issue #19 surfaces deliberately, not via a routine tool call",
}

func schemaObject(props map[string]json.RawMessage, required []string) json.RawMessage {
	// Minimal hand-built schema — avoids pulling a JSON Schema library and
	// keeps the wire payload exactly what MCP clients expect (a JSON object
	// with type:"object" and a properties map).
	buf := &strings.Builder{}
	buf.WriteString(`{"type":"object","properties":{`)
	first := true
	// Sorted iteration so the JSONSchema bytes are stable across builds —
	// MCP clients hash these for caching; non-deterministic property order
	// would invalidate caches unnecessarily.
	keys := sortedKeys(props)
	for _, k := range keys {
		if !first {
			buf.WriteString(",")
		}
		fmt.Fprintf(buf, "%q:%s", k, string(props[k]))
		first = false
	}
	buf.WriteString(`}`)
	if len(required) > 0 {
		b, _ := json.Marshal(required)
		fmt.Fprintf(buf, `,"required":%s`, string(b))
	}
	buf.WriteString(`}`)
	return json.RawMessage(buf.String())
}

func schemaString(description string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}{Type: "string", Description: description})
	return json.RawMessage(b)
}

func schemaEnum(values []string, description string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type        string   `json:"type"`
		Enum        []string `json:"enum"`
		Description string   `json:"description,omitempty"`
	}{Type: "string", Enum: values, Description: description})
	return json.RawMessage(b)
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func unmarshalArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
