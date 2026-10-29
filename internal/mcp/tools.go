package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/scorecard"
	"github.com/osauer/canary/v2/internal/stress"
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
	// RPCMethods declares every daemon method this handler may invoke. Timing
	// and parity tests use it to keep adapter deadlines above daemon deadlines.
	RPCMethods []string
	Handler    func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error)
}

// Tools is the canonical inventory exposed over MCP. Order is the same as
// cli.Commands() to keep the parity test readable; the MCP client rebroadcasts
// whatever order we send.
var Tools = []Tool{
	stockAddTool(),
	portfolioPlanTool(),
	accountIntradayTool(),
	shortInterestTool(),
	{Name: "canary_data_health", Title: "Canary Data Health", RPCMethods: []string{rpc.MethodDataHealth}, ReadOnlyHint: new(true), Description: "Read Canary's authoritative provider/service health, receiving data modes, causes and producer clocks. Sources are services and data products, never individual instruments; quote availability and chart coverage stay in their dataset tools. A passive read performs no broker or provider acquisition. Follow next_offset with the returned revision for a consistent complete report. Unknown means unverified, never healthy. Use canary_data_check to request a bounded check, canary_status for service connectivity, and dataset tools for values.", JSONSchema: schemaObject(map[string]json.RawMessage{"offset": json.RawMessage(`{"type":"integer","minimum":0,"description":"Zero-based page offset; default 0. Use the preceding next_offset."}`), "limit": json.RawMessage(`{"type":"integer","minimum":1,"maximum":64,"description":"Maximum sources per bounded page; default 24. Byte limits may return fewer."}`), "revision": schemaString("Report revision from the first page; omit to begin a current report. Expired revisions require restarting pagination.")}, nil), Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
		var p rpc.DataHealthParams
		if err := unmarshalArgs(args, &p); err != nil {
			return nil, err
		}
		// The producer owns the bounded page. Preserve additive evidence even
		// when this SDK predates its optional fields.
		var r json.RawMessage
		if err := conn.Call(ctx, rpc.MethodDataHealth, p, &r); err != nil {
			return nil, err
		}
		return r, nil
	}},
	{Name: "canary_data_check", Title: "Canary Required Data Check", RPCMethods: []string{rpc.MethodDataCheck}, ReadOnlyHint: new(true), Description: "Request or inspect a coalesced bounded check of required data sources using reference quote probes, not a holdings inventory. Counts describe sources, not instruments. Reuses Canary subscriptions and existing retry windows; history, options and public sources keep their producer schedules. Returns queued/running or completed coverage; it cannot purchase subscriptions or repair access. Use canary_data_health for the passive canonical report. Does not place orders, change settings or restart the gateway.", JSONSchema: schemaObject(nil, nil), Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
		var r rpc.DataCheckResult
		if err := conn.Call(ctx, rpc.MethodDataCheck, nil, &r); err != nil {
			return nil, err
		}
		return json.Marshal(r)
	}},
	{Name: "canary_portfolio", Title: "Canary Portfolio Composition", RPCMethods: []string{rpc.MethodPortfolioSnapshot}, Description: "Read current portfolio composition in account base currency: signed market value by asset class and by GICS sector as a share of net liquidation. Options count at premium value under their underlying; the embedded index funds (SPY, QQQ, IWM) spread over published sector weights with their as-of dates; other funds pool without look-through. Holdings the broker no longer quotes or marks at zero are excluded and counted. Cost basis uses broker average cost including the option multiplier. This is allocation, not directional risk, a return percentage, or statement performance.", JSONSchema: schemaObject(nil, nil), Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
		var res rpc.PortfolioSnapshotResult
		if err := conn.Call(ctx, rpc.MethodPortfolioSnapshot, nil, &res); err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}},
	{Name: "canary_recon_status", Title: "Canary Reconciliation Status", RPCMethods: []string{rpc.MethodReconStatus}, Description: "Read redacted automatic statement acquisition and capital-flow/equity reconciliation status. Acquisition health is separate from evaluation. This does not prove complete broker order or fill visibility and performs no refresh, reconciliation acknowledgement, or broker action.", JSONSchema: schemaObject(nil, nil), Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
		var res rpc.ReconStatusResult
		if err := conn.Call(ctx, rpc.MethodReconStatus, rpc.ReconStatusParams{}, &res); err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}},

	{
		Name: "canary_market", Title: "Canary Market Quotes", RPCMethods: []string{rpc.MethodMarketSnapshot, rpc.MethodMarketBond},
		Description: "Read benchmark quotes and independent quotes for held underlyings, including option-only holdings. Preserves contract identity, current versus close prices, feed quality, source timestamps and partial coverage. Benchmarks name their actual instruments, including ETFs. With `bond_identifier` (an ISIN or CUSIP) it instead checks one bond or bill read-only: whether the broker resolves it to exactly one line, asked as BOND and, for a government bill's identifier, also as BILL (maturity, coupon, class bill or bond, min size, tick, assumed quantity unit) and one quote (bid/ask/last per 100 of face, yields in percent, `fresh` only for a live bid or ask); a gap is returned as `resolved` or `quoted` false with a `reason`, never as zero. Use canary_positions for valuation and Greeks; this is not a chart or order tool.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"bond_identifier": schemaString("optional ISIN (12 characters) or CUSIP (9) of one bond or bill to resolve and quote read-only; omit for the market snapshot"),
			"bond_currency":   schemaString("optional three-letter currency for bond_identifier; default: USD for a CUSIP, else the ISIN's issuer country (required for XS ISINs)"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				BondIdentifier string `json:"bond_identifier"`
				BondCurrency   string `json:"bond_currency"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.BondIdentifier) != "" {
				var res rpc.MarketBondResult
				if err := conn.Call(ctx, rpc.MethodMarketBond, rpc.MarketBondParams{Identifier: in.BondIdentifier, Currency: in.BondCurrency}, &res); err != nil {
					return nil, err
				}
				return json.Marshal(res)
			}
			var res rpc.MarketSnapshotResult
			if err := conn.Call(ctx, rpc.MethodMarketSnapshot, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_market_history", Title: "Canary Price History", RPCMethods: []string{rpc.MethodMarketHistory},
		Description: "Read bounded observed underlying price bars for a chart. Intraday ranges include extended hours; longer ranges use daily regular-session bars. Missing or rejected history is unavailable, never an empty healthy chart. Use canary_market for current quotes and canary_technical for analysis. No option history, continuous futures roll series, or trading authority. Futures history belongs to the exact dated contract.",
		JSONSchema:  schemaObject(map[string]json.RawMessage{"contract": json.RawMessage(`{"type":"object","description":"Exact underlying identity; specify symbol, sec_type (STK, IND, CASH, FUT), exchange and currency; preserve con_id when known.","properties":{"symbol":{"type":"string"},"sec_type":{"type":"string"},"exchange":{"type":"string"},"currency":{"type":"string"},"con_id":{"type":"integer"},"primary_exchange":{"type":"string"},"local_symbol":{"type":"string"},"trading_class":{"type":"string"},"multiplier":{"type":"integer"},"expiry":{"type":"string"}},"required":["symbol","sec_type","exchange","currency"]}`), "range": schemaEnum([]string{"1D", "5D", "1M", "6M", "YTD", "1Y", "5Y"}, "Requested date range; 1D uses 5-minute bars, 5D 30-minute bars, longer ranges daily bars.")}, []string{"contract", "range"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var p rpc.MarketHistoryParams
			if err := unmarshalArgs(args, &p); err != nil {
				return nil, err
			}
			var res rpc.MarketHistoryResult
			if err := conn.Call(ctx, rpc.MethodMarketHistory, p, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_market_tape", Title: "Canary Market Tape", RPCMethods: []string{rpc.MethodMarketTape}, ReadOnlyHint: new(true),
		Description: "Compare rallies and reversals with daily SPX/SPY/QQQ prices, a fixed 11-share-line/10-company leader basket, company participation, relative volume and VIX. Daily open/high/low, when available, show range recovery but not the time of a turn. Set history=true to read permanent local records and one/three/five-session closing-price follow-ups without a broker request; original and latest versions retain capture times and reconstructed-history labels. Follow-ups are price comparisons, not achievable trading returns or validated forecasts. Normal mode uses completed sessions and also archives the measurements. Missing data stays explicit. ETF volume is not total-market volume or signed flow; put/call flow is absent. Use canary_market_history for a named instrument chart, canary_regime for current regime and canary_edge for past portfolio decisions. Read-only; no risk-policy or trading authority.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"sessions": json.RawMessage(`{"type":"integer","minimum":5,"maximum":60,"description":"Number of completed US equity sessions to align; default 20, maximum 60 per page. Missing observations remain null."}`),
			"history":  json.RawMessage(`{"type":"boolean","description":"Default false. Read the durable archive with first/latest captures and one/three/five-session price changes. No score or trading rule."}`),
			"before":   json.RawMessage(`{"type":"string","description":"Only with history=true: exclusive YYYY-MM-DD session date. Use history.next_before to read earlier pages beyond the rolling 60-day view."}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var p rpc.MarketTapeParams
			if err := unmarshalArgs(args, &p); err != nil {
				return nil, err
			}
			p, err := rpc.NormalizeMarketTapeParams(p)
			if err != nil {
				return nil, err
			}
			var res rpc.MarketTapeResult
			if err := conn.Call(ctx, rpc.MethodMarketTape, p, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:               "canary_status",
		RPCMethods:         []string{rpc.MethodStatusHealth, rpc.MethodSettingsGet},
		Title:              "Canary Status",
		Description:        "Read daemon, gateway, storage, and source health, plus execution visibility and MCP client activation guidance. Execution visibility remains unproven until a fill from TWS or another API client is observed; executions.action gives configuration guidance. After an MCPB upgrade and host relaunch, check mcp_client.activation_required; run its restart_command yourself in a terminal only when provided, then recheck. Different or unverified daemon build capabilities require the matching installation. This tool never restarts anything. Each register strategy appears under `strategies` with its stage and its month's row as the proposal engine last served it, or why none stands. Use for connectivity or degraded-input diagnosis; use canary_brief for the desk decision surface.",
		MonitorDescription: "Use only to diagnose why canary_brief is unavailable or degraded. Read-only.",
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
		RPCMethods:  []string{rpc.MethodTradingStatus},
		Title:       "Canary Trading Status",
		Description: "Read local broker-write readiness, pinned context, freeze state, the order cap in force (`order_limits`), and blockers. `broker_permissions` reports the account's trading permission per product class as IBKR's own previews showed it: futures always, unknown with the reason until a check could run (the daemon checks them at each start and then daily, once a preview is possible, with a preview of one Micro E-mini S&P 500 contract that sends no order), options, stocks and bonds once a preview has shown them; each row's `state` is enabled, not_enabled or unknown, with `checked_at`, `source` and the broker's `detail`. An opening or increasing order in a not_enabled class is refused with `broker_permission_not_enabled`; closing orders are not. Use it for 'can Canary trade', 'why was my order refused for permission' or 'is the index hedge allowed'; use canary_status for daemon and gateway health. It only reports readiness and does not place, modify, or cancel orders.",
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
		RPCMethods:  []string{rpc.MethodSettingsGet},
		Title:       "Canary Platform Settings",
		Description: "Read platform settings and observed data quality. This tool cannot change settings or authorize a broker action.",
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
		RPCMethods:  []string{rpc.MethodOrdersOpen},
		Title:       "Canary Open Orders",
		Description: "Read-only current-context order lifecycle view: journaled open orders, each with the origin that placed it (agent, human-tty, human-paired-device, daemon-preauthorised or daemon-owner-queued), and under `untracked` the orders working at the broker that Canary never placed, such as hand orders in TWS, which carry no origin. Trust an empty `untracked` only when `untracked_status` is current. Use canary_orders_history for past orders. It does not place, modify, cancel, or transmit orders and is not a broker statement.",
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
		RPCMethods:  []string{rpc.MethodOrdersHistory},
		Title:       "Canary Order History",
		Description: "Read-only bounded local order-journal history for the current account and mode. It does not place orders and is not an IBKR Activity Statement.",
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
		RPCMethods:  []string{rpc.MethodOrderStatus},
		Title:       "Canary Order Status",
		Description: "Read-only lifecycle and typed callback evidence for one locally journaled order. Broker free text is withheld; this tool cannot preview or change the order.",
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
		Name:        "canary_account",
		RPCMethods:  []string{rpc.MethodAccountSummary},
		Title:       "Canary Account",
		Description: "Read account financials. The `authority` block identifies one concrete account and mode with availability, freshness, typed reason, and field presence; missing is never zero. Each `currency_exposure` row and the `base_currency_ledger` carry `interest`: the broker's own credit and debit rate on that currency's cash from the Flex statement's Interest Details (Tiers) section (`source` flex_interest_tiers, `read_at`), each side `available` with the annual decimal `rate` on the tier the current balance's top unit falls in, that `tier`, every `tiers` row, the broker's `value_date` and `generated_at`, or `unavailable` with the `reason` and `last_value_date` (no row for the side, no statement with the section, or a value date older than two business days); never zero for unknown.",
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
		RPCMethods:  []string{rpc.MethodPositionsList, rpc.MethodPositionsRisk},
		Title:       "Canary Positions",
		Description: "Read held positions and exposure. The `authority` block identifies one concrete account and mode with availability, freshness, and typed reason; stale or unavailable empty rows do not prove an empty book. Held bonds and bills stay in `stocks` with their valuation (sec_type BILL or BOND); the `bonds` section classifies each by `con_id` as `bill`, `bond` or `unresolved` (with a `reason`) and adds maturity, days to maturity, coupon, ISIN/CUSIP and currency, and where it can be measured the issuer and issuer class, yield, modified duration and, in the account base, value, DV01 and `rate_shock_loss_base` (the loss if yields rise one point), else `risk_unmeasured` saying why. `bond_risk` sums them: bonds and the one-point loss as % of NLV, by currency and issuer, the non-government share, and the lines not in the sums. Each classified line's `facts`, in its own currency and per 100 of face, state the price it trades at (`price` with `price_source` bid or mark; bid, ask, mark, the prior `close` and the quote clock), its yield on a named `yield_basis` (simple actual/360 for EUR bills, actual/365 for USD, GBP and CAD bills, yield to maturity for coupon bonds; the broker's yield ticks are not used), the change since the prior close in basis points with that close's date, the value change for +25 and -25 bp and `dv01` as a model estimate, `face_value` and `pays_at_maturity`, the `purchase` (date and price from Canary's order journal, else the broker's average cost without a date, `source` saying which) and `pnl`: the clean gain since purchase with `carry`, `rates` and `spread` that sum to `total`; the spread reads a BTF against the nearest Bubill `benchmark` and is `spread_unavailable` with the reason until a benchmark yield is on record. Every missing fact is absent with a `*_unavailable` reason, never zero. Measurement only: no bond limit judges these yet.",
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
			params := rpc.PositionsListParams{Symbol: in.Symbol, Type: in.Type}
			if in.View == rpc.ViewRisk {
				var risk rpc.PositionsRiskResult
				if err := conn.Call(ctx, rpc.MethodPositionsRisk, params, &risk); err != nil {
					return nil, err
				}
				return json.Marshal(risk)
			}
			var res rpc.PositionsResult
			if err := conn.Call(ctx, rpc.MethodPositionsList, params, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "canary_strategies",
		RPCMethods:  []string{rpc.MethodPositionsList},
		Title:       "Canary Option Strategies",
		Description: "Read how Canary groups currently held option legs into proportional strategies and which legs still need review. The `authority` block is the positions read's: one concrete account and mode with availability, freshness, and typed reason. When availability is unavailable, an empty `strategies` list does not mean no strategies are held; report the reason, and treat strategies a stale read still lists as context, not current holdings. Use canary_positions for the full book. This tool cannot preview, submit, place, modify, cancel, or transmit an order.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.PositionsResult
			if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "opt"}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(rpc.NewPositionStrategiesView(&res))
		},
	},
	{
		Name:        "canary_technical",
		RPCMethods:  []string{rpc.MethodTechnical},
		Title:       "Canary Technical Screen",
		Description: "Analyze explicitly named stock or ETF symbols using daily trend, relative strength, ATR, and liquidity evidence. This is analysis, not order entry.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols":          json.RawMessage(`{"type":"array","items":{"type":"string"},"minItems":1,"description":"ticker symbols, e.g. [\"AAPL\",\"MSFT\",\"NVDA\"]"}`),
			"benchmark":        schemaString("relative-strength benchmark, default SPY"),
			"lookback_days":    json.RawMessage(`{"type":"integer","minimum":30,"maximum":800,"description":"calendar-day history lookback; default 420, enough for 200-DMA and 126 trading-bar returns"}`),
			"market":           json.RawMessage(`{"type":"string","enum":["us","de","uk"],"description":"optional route for symbols, not the benchmark; omit/use us for SMART/USD, use de for Xetra/IBIS EUR equities, or uk for London equities (SMART, LSE, GBP by default)"}`),
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
		Name:        "canary_calendar",
		Title:       "Canary Market Calendar",
		Description: "Read official exchange sessions to plan work around market opens, closes, holidays, and early closes. Supports US cash equities, US listed options, Xetra, London SETS, Tokyo, and Hong Kong cash equities. Session windows exclude scheduled lunch breaks; outer open/close alone cannot establish tradability. Preserve timezone, source, coverage bounds, and session.state: unknown is not closed and cannot supply a trading schedule. This is an exchange-session calendar, not an economic-release or earnings calendar; use canary_brief for current held-name event context. Read-only; no scheduling, refresh, or broker actions.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"market": schemaEnum([]string{"us", "us-options", "de", "uk", "jp", "hk"}, "exchange-session market; default us. US listed options models the regular 16:15 session; per-class exceptions and global/curb hours are not modeled"),
			"date":   schemaString("optional YYYY-MM-DD in the market timezone; evaluates that date at local noon. Omit for now; at takes precedence when both are supplied"),
			"at":     schemaString("optional RFC3339 instant including a timezone offset; evaluates the market at that instant and takes precedence over date"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"maximum":400,"description":"number of forward calendar dates including the selected date, not trading sessions; default 14, maximum 400"}`),
		}, nil),
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodMarketCalendar},
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
		Name: "canary_macro", Title: "Canary Economic Calendar and Official News",
		Description: "Read cached economic releases, central-bank meetings and recent official publications for the trading day. Includes BLS, BEA, Federal Reserve, New York Fed and ECB sources with per-feed availability, original times and a bounded window. Missing or stale feeds do not mean nothing is scheduled. Preserve source_label/date-only precision, per-source window bounds, failure streaks and list-specific events_truncated/publications_truncated flags; legacy truncated is their union. New York Fed supplies independent key-release backup; its success does not establish full BLS coverage. Use window_start/window_end to filter source-local calendar dates before response limits. This is official-source coverage, not a licensed general-news or consensus feed. Use canary_calendar for exchange hours and canary_brief for the current book and risk. Read-only; does not fetch, acknowledge, schedule or trade.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"window_start": schemaString("inclusive source-local YYYY-MM-DD; supply with window_end, at most 31 days; omitted window uses yesterday through next week"),
			"window_end":   schemaString("inclusive source-local YYYY-MM-DD; supply with window_start"),
		}, nil), ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodMacroSnapshot},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.MacroSnapshotParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var out rpc.MacroSnapshotResult
			if err := conn.Call(ctx, rpc.MethodMacroSnapshot, in, &out); err != nil {
				return nil, err
			}
			return json.Marshal(out)
		},
	},
	{
		Name: "canary_regime", Title: "Canary Market Regime",
		Description:  "Read the detailed broad-market regime: all eight indicators, independent clusters, confirmation eligibility, gamma horizons and skew, source health, and stale or unavailable evidence. Choose view=monitor for a compact dashboard projection or view=full (default) for detailed evidence. Use after canary_brief or for an explicit market-regime question; use canary_stress for how that market state affects the held portfolio. Gamma is a conditional amplification/damping model; open interest does not identify dealer inventory or market direction. Read-only; no history or refresh controls.",
		JSONSchema:   schemaObject(map[string]json.RawMessage{"view": json.RawMessage(`{"type":"string","enum":["full","monitor"],"description":"full (default) retains detailed evidence; monitor uses the shared compact dashboard projection. monitor cannot include gamma profile arrays."}`), "include_profiles": json.RawMessage(`{"type":"boolean","description":"Include large gamma profile arrays; false by default. Scalar measurements, thresholds, warnings and freshness are always retained."}`)}, nil),
		ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodRegimeSnapshot},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				IncludeProfiles bool   `json:"include_profiles"`
				View            string `json:"view"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.View != "" && in.View != "full" && in.View != "monitor" {
				return nil, fmt.Errorf("view must be full or monitor")
			}
			if in.View == "monitor" && in.IncludeProfiles {
				return nil, fmt.Errorf("monitor view cannot include profiles")
			}
			var res rpc.RegimeSnapshotResult
			if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
				return nil, err
			}
			if in.View == "monitor" {
				return json.Marshal(rpc.CompactRegimeMonitor(&res))
			}
			if !in.IncludeProfiles {
				rpc.StripRegimeGammaProfiles(&res)
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_stress", Title: "Canary Portfolio Stress",
		Description: "Read the full current portfolio-stress assessment: margin, P&L and tape shocks, exposure, concentration, protection coverage, held-name and options risk, market indicators, and source health. Use after canary_brief or for an explicit portfolio-risk question; use canary_regime for the detailed broad-market dashboard. Preserves the same shared assessment used by the app. Missing inputs cannot become healthy zero values. Advisory and read-only; cannot preview or submit orders or change limits.",
		JSONSchema:  schemaObject(nil, nil), ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodStressSnapshot},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct{}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := stress.FetchStress(ctx, conn)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_mandate", Title: "Canary Mandate Risk Budget",
		Description: "Read the daemon's mandate verdict: the loss measure (every bought option at its premium at risk, the higher of the price paid and today's value, protection included, plus the cash loss to the stop of every share line a working stop or trail covers) against the mandate's loss room, which is `max_loss_pct` of the high-water mark of the risk base (equity after the tax reserve, cash-flow adjusted) times the room left under the maximum loss (owner decision 2026-10-27, spec TM-04.12). `verdict.loss` carries the measure (`total_base`, `premium_base`, `stop_loss_base`, `room_base`, `ratio_to_room`, the names with `unstopped` share lines, which count nothing). The book's ex-ante volatility (historical simulation with full revaluation of every leg, currency moves included) stays computed against the volatility budget (`vol_target_pct` times the budget base times room) as the sizing input of the sleeve allocations and the 1-day swing, no longer the headline. `verdict.status` is within, reduce or unknown (reduce when the loss measure is over the loss room or a gap cap is breached; `over` is no longer produced); `verdict.reasons` names every missing or stale input (the verdict is unknown, never within, without them); `verdict.underlyings` and `verdict.legs` carry Euler risk contributions that sum to 100%, betas to SPY and each name's gap loss against its gap cap; `verdict.stresses` the index fall, equal-sized rise and gap breaches; a held index future contributes `verdict.loss.index_hedge_base`, its loss in the adverse index shock when no effective full stop bounds it. Optional `[mandate.iv_fallback]` policy permits mark-implied volatility with uncertainty bands: `verdict.iv_fallback` reports its tier, legs and range; `verdict.iv_fallback.legs` reports `source` and, when supplied by the position data, `as_of`; an absent clock is not a current-quote claim. Missing policy or excessive uncertainty keeps the verdict unknown; `evidence` the clocks of the equity, positions and every daily series; `gate.refuses_additions` whether risk-adding stock and ETF orders are refused right now (`mandate_budget` on an order preview: the loss measure over the loss room, a gap breach, a sleeve over its allocation, or the verdict unknown); `gate.options` the same for option buys, which the premium limits gate instead (owner decision 2026-10-23: `mandate_premium` refuses a buy that would take one name's premium at risk past `option_name_act_pct` of NLV or the book's past the premium budget's act level, protection counted since 2026-10-27; `gate.options.warning` carries the loss-room or volatility judgement an option buy passes with); `gate.override` the owner's active one-shot override of `mandate.vol_target_pct` with its reason and expiry, while one lifts the volatility gate and the premium limits for the owner's own orders (owner 2026-10-29); a put bought to protect shares held is a hedge entry the gates never refuse; `exposure` the book through the greeks for the operator to watch, per underlying and in total: net delta in money, the same through each name's beta (`delta_beta_base`), gamma per 1% move, theta a day, vega a point, each name's share of the risk and beta, and the premium at risk against its cap (`premium_cap_base`, `premium_room_base`, `premium_over`), a figure that cannot be measured absent with its reason in `unknown`, never zero; `reduction` the forced reduction back under the loss room, largest premium at risk per name first (phase `premium`; `loss_base`, `loss_room_base`, `loss_target_base`, `projected_loss_base`, each order's `loss_base` and `loss_cut_base`; its `state`: watching until a second scheduled verdict at least five minutes later confirms the breach, reducing, held while no fresh verdict of the book as held confirms the sizing, within once the standing rows were withdrawn), whether `mandate_reduction` is `pre_authorised`, how many rows are `pending` and the `veto_until` time, and its `orders` in dispatch order. `register` lists each machine strategy of the strategy register (the core first): its rule and whether the policy cites its lab spec, its stage as written and as this build runs it (shadow, live, suspended or hold), the part of its risk allocation it reserves, why it holds and the data its rule collects. `verdict.owner_stop`, present when `[mandate.sleeves] owner_stop_pct` is written, is the owner sleeve's own drawdown stop: the sleeve's fall from its high-water mark, whether the stop is `engaged` (the sleeve then adds nothing and the reduction cuts it to `target_use_base`, half its risk at the stop, pinned until the drawdown is back under `release_pct`, five points under the stop) and whether risk is still `over` that target. `active` is false while the constitution carries no [mandate] table. Use it for 'can I add risk', 'what is driving the risk' or 'why was my order refused with mandate_budget'; use canary_stress for the broader stress findings and canary_policy_check for the mandate keys and the realised figures. Read-only; it places no order and changes no limit; refresh=true reads the account and positions again first.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"refresh": json.RawMessage(`{"type":"boolean","description":"when true, the daemon reads the account and positions again before judging; false (the default) serves a verdict younger than thirty seconds as it is"}`),
		}, nil), ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodMandateRisk},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Refresh bool `json:"refresh"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := rpc.FetchMandateRisk(ctx, conn, in.Refresh)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_core", Title: "Canary Core Sleeve Target",
		Description: "Read the daemon's core sleeve target: the mandate's rule-based share in one broad index fund (rule `core-ibcf-v1`, the fund IE00B3ZW0K18 in EUR; `instrument` is the contract IBKR lists under that ISIN, its primary exchange and the calendar it is judged on), a beta allocation the owner chose with a drawdown filter, not an edge claim. `decision` is the month's reading at the last session of the month on that calendar: the close against its 10-month average (`trend_on`), the EWMA, 252-day and floor volatilities and which one binds, frozen at its first evaluation (`frozen_at`; `drift` when the same window's closes later read differently). `sizing` is the target in shares and money, frozen at the decision and lowered to today's capital, and `holdable` the largest holding the mandate's headroom admits (the book at most at 95% of the budget and the core's gap at most at 95% of its cap). `holding` is that contract's confirmed shares; `action` is what the rule would do at the next window (buy, sell, sell_all, wait, none or hold) with its reason, the band test and the minimum order. `stage` is the core's entry in the strategy register: its stage as written and as this build runs it, and the part of its allocation it reserves; while the core runs in shadow, `shadow` is its paper position on the production code and its newest records (each month's decision, the intent for the next session with the order terms its row would carry, and its simulated fill at that session's close), research evidence that is never sent. While the core is live, `row` is its pending core_rebalance proposal (key, action, quantity, the session it is for, its state, blockers and readiness, and the terms: a DAY limit at the live mid for the first five sessions after the decision that the US also trades) and `last_decision` the last month decided live with what became of its row (served, sent with the order reference, or the latest refusal). The owner can confirm each row through the prepared-proposal path, or an authorized controller can submit it under the device-confirmed standing mandate; buys require Fully armed scope and all daemon gates are re-checked. Installing this release does not arm the controller. Reading this tool never submits an order. `status` is ready, hold (with `reasons`: an unresolved or ambiguous contract, a missing, stale or refused series read, unwritten keys, an unknown mandate verdict) or inactive while the constitution carries no [mandate.core]. Use it for 'what is the core doing', 'why did the core not buy', 'is a core row pending' or 'what will the core trade at month-end'; use canary_mandate for the whole book's risk budget and canary_proposals for orders waiting for approval. Reading changes nothing: `read_only` is true while the core is not live; refresh=true measures the mandate again first.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"refresh": json.RawMessage(`{"type":"boolean","description":"when true, the daemon reads the account and positions and measures the mandate again before deciding; false (the default) uses a mandate verdict younger than thirty seconds"}`),
		}, nil), ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodCoreTarget},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Refresh bool `json:"refresh"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := rpc.FetchCoreTarget(ctx, conn, in.Refresh)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name: "canary_strategy", Title: "Canary Registered Strategy Target",
		Description: "Read the daemon's target for one machine strategy of the strategy register by its id (`id`: \"core\" for the core sleeve, else the `[mandate.strategies.<id>]` table's key; `canary_mandate` lists them). It is the same verdict shape as canary_core for any entry: `rule` (the compiled rule, `timed-holding-v1` for an entry the owner parameterises), `signal` (the trend filter from the library, `trend-sma10m`), `spec_hash` (the lab spec the entry cites), `sleeve`, `instrument` (the fund by ISIN and currency as IBKR lists it), `decision` (with `signal_from` and `traded_close` while the month is measured on the fund's older share class because the traded line's own closes do not fill the window; `evidence.signal` shows that series and when the line's own closes take over), `sizing` against the sleeve's risk allocation, `holding` (the units its own fills left on its line; your units of the line hold it, named, and a hand buy of a live entry's line is refused), `action`, `stage` and `kill` (the kill rule's drawdown in units of the sleeve's capital at its stage (`scale_pct`: its allocation over the 15% floor, times the scale), on its own fills while live (`source`: own_fills) or on the shadow record, against `kill_drawdown_k`; past it the strategy holds and the owner suspends it; a live entry without a written bound holds). While the entry is live (`read_only=false`), `row` is its pending month's row (a `strategy_rebalance` proposal, or `core_rebalance` for the core: key, action, quantity, the session it is for, its state, blockers and readiness, and the terms: a DAY limit at the live mid for the first five sessions after the decision that the US also trades, due fifteen minutes after the US cash open) and `last_decision` the last month decided live with what became of its row. The row is sent only on the owner's device confirmation through the gated proposal path or by an authorized controller under the device-confirmed standing mandate (buys require Fully armed scope and the register the mandate was armed under); Canary's gates re-check it at every point it passes. Installing this release does not arm the controller. `status` is ready, hold (with `reasons`: an unwritten key, a filter the library lacks, a currency other than the account's base, an unresolved contract, a missing or stale series, an unknown mandate verdict, units of its line it did not buy, the kill bound) or inactive. Not the option combos of canary_strategies. Reading this tool never submits an order. refresh=true measures the mandate again first.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"id":      json.RawMessage(`{"type":"string","description":"the strategy's id in the register: core, or the [mandate.strategies.<id>] table's key"}`),
			"refresh": json.RawMessage(`{"type":"boolean","description":"when true, the daemon reads the account and positions and measures the mandate again before deciding; false (the default) uses a mandate verdict younger than thirty seconds"}`),
		}, []string{"id"}), ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodStrategyTarget},
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				ID      string `json:"id"`
				Refresh bool   `json:"refresh"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := rpc.FetchStrategyTarget(ctx, conn, in.ID, in.Refresh)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_brief",
		Title:        "Canary Daily Brief",
		Description:  "Start here for the daemon's current desk summary: regime stage and verdict, portfolio-stress summary, breadth coverage, modeled gamma response and expiry horizons, and observed 25-delta option skew. Preserve freshness, rankability, source warnings and unavailable values. Take severity order from Canary: `attention_order` lists what needs attention (drawdown tier, latch, rules at act, then other attention rows) and `ready.ranked` orders every Ready row; never re-sort them. Gamma models potential amplification or damping; open interest does not prove bullish/bearish intent or dealer inventory, and put-call skew is relative option pricing, not a forecast. Use canary_regime for all market indicators and confirmation details, canary_stress for full portfolio-risk evidence, canary_status to diagnose degraded sources and canary_edge for retrospective decision outcomes; drill into canary_positions or canary_account only when the brief points there. Read-only; never acknowledges the brief or writes to the journal.",
		JSONSchema:   schemaObject(nil, nil),
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodBriefSnapshot},
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.BriefResult
			if err := conn.Call(ctx, rpc.MethodBriefSnapshot, rpc.BriefSnapshotParams{}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_reporting",
		Title:        "Canary Broker Reporting Status",
		Description:  "Read shared IBKR statement-reporting setup, broker reachability, backfill state, absent sections, present-empty sections, and proven missing fields for Recon and Edge. Read-only; returns no Query ID or token and cannot validate candidates, refresh reports, or change setup.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodReportingStatus},
		JSONSchema:   schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.ReportingStatusResult
			if err := conn.Call(ctx, rpc.MethodReportingStatus, struct{}{}, &res); err != nil {
				return nil, err
			}
			if err := rpc.ValidateReportingStatusResult(res); err != nil {
				return nil, fmt.Errorf("invalid reporting status: %w", err)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_reporting_fx",
		Title:        "Canary FX Contribution",
		Description:  "Read completed daily FX valuation contribution for investments, currency cash and native accrued interest, in the portfolio base currency. Use for the effect of exchange rates over Day, week-to-date, month-to-date and YTD. Returns ascending daily contributions, reconciled period totals, explicit missing dates and acquisition progress. Null/absent money is unavailable evidence, never zero; no_exposure means no gross foreign exposure throughout a complete period. Uses closing native values, including price/income interaction and actual conversion principal correction. Does not report asset performance, realised currency tax lots or live intraday P&L. Use canary_reporting_performance for portfolio performance facts and canary_account for live P&L. Read-only cached evidence: cannot start backfill, alter configuration or transmit orders.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodFX},
		JSONSchema:   schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.FXResult
			if err := conn.Call(ctx, rpc.MethodFX, struct{}{}, &res); err != nil {
				return nil, err
			}
			if err := rpc.ValidateFXResult(res); err != nil {
				return nil, fmt.Errorf("invalid FX result: %w", err)
			}
			return json.Marshal(res)
		},
	},

	{
		Name:         "canary_reporting_performance",
		Title:        "Canary Statement Performance Series",
		Description:  "Read the retained IBKR statement equity series in the account base currency (one close per report date), dated external capital flows (deposits, withdrawals, position transfers in and out), statement coverage, and year-to-date sums of FIFO realised trading P&L, commissions, dividends, interest, withholding tax and fees, so that a consumer can compute cash-flow-adjusted (time-weighted) performance, drawdowns and a benchmark comparison itself. A missing report date is a gap, never interpolated; nothing is annualised or compared here. Use canary_account for live net liquidation and today's P&L, canary_reporting for statement setup and broker reachability, and canary_recon_status for capital-flow reconciliation verdicts. Read-only; returns no account identity, computes no return figure, and cannot refresh statements.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodReportingPerformance},
		JSONSchema:   schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.ReportingPerformanceResult
			if err := conn.Call(ctx, rpc.MethodReportingPerformance, struct{}{}, &res); err != nil {
				return nil, err
			}
			if err := rpc.ValidateReportingPerformanceResult(res); err != nil {
				return nil, fmt.Errorf("invalid reporting performance: %w", err)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_reporting_costs",
		Title:        "Canary Execution Costs",
		Description:  "Read the half-spread each fill paid against the exact-contract quote Canary retained at the fill: per fill the side, quantity, price, bid, ask and mid, the quote's age against the fill, and the cost in the contract currency and in the base currency at the fill's own FX rate; totals by month, by source (orders Canary placed versus executions observed from the account) and by instrument class; coverage as fills with a usable two-sided quote over all fills; fills without a live quote listed as unknown with the reason. Each fill names its retained base currency; totals keep different base currencies separate. Delayed, frozen and unclassified feeds remain visible but are not measured costs. Requires one concrete account scope. Use for execution quality and spread cost. Not for realised P&L (canary_reporting_performance), statement setup (canary_reporting) or order state (canary_orders_history). Executions entered in TWS or by other API clients appear only when TWS reports them to this client; the observation section says whether that is proven and names the TWS Master API client ID setting, which the owner decides. Read-only: names no account, computes nothing live and cannot place or change orders.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodReportingCosts},
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"month": schemaString("optional calendar month YYYY-MM in UTC; omitted returns every retained fill"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var params rpc.ReportingCostsParams
			if len(args) > 0 {
				if err := json.Unmarshal(args, &params); err != nil {
					return nil, fmt.Errorf("invalid reporting costs arguments: %w", err)
				}
			}
			if params.Month != "" && !rpc.ValidCostsMonth(params.Month) {
				return nil, fmt.Errorf("month must be YYYY-MM")
			}
			var res rpc.ReportingCostsResult
			if err := conn.Call(ctx, rpc.MethodReportingCosts, params, &res); err != nil {
				return nil, err
			}
			if err := rpc.ValidateReportingCostsResult(res); err != nil {
				return nil, fmt.Errorf("invalid reporting costs: %w", err)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_edge",
		Title:        "Canary Edge Decision Review",
		Description:  "Call with no arguments after canary_brief for an automatic one-year review of historical decision price outcomes: action/direction coverage, comparisons of the same decisions at different horizons, monthly consistency, concentration, and exact local protection provenance when available. SPY, QQQ, DIA, and VIX supply informational context. Options separately report option lifecycle coverage, proven flat-to-flat exact-contract positions, broker-reported realized option episodes, and the latest dated open option snapshot; these scopes overlap and must not be added. Use a returned change_id or returned option_id for broker-fact details; completed position cycles are already expanded in options.cycles, each with closed_by (trade, expiry, exercise or assignment; after an exercise or assignment the P/L is the option leg's only). The option lists return 20 rows by default; their counts and known totals always cover every row, and option_offset with option_limit pages them, following next_offset until it is absent. Do not use for current risk, order decisions, forecasting, or causal claims. Do not infer skill, strategy win rates, trade intent, or risk effectiveness. Use canary_brief for current portfolio risk and canary_positions for current holdings. Read-only; parameters select retained evidence and cannot refresh data.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodEdgeSnapshot},
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"window":           schemaEnum([]string{"90d", "365d"}, "optional review override; default 365d"),
			"horizon_sessions": json.RawMessage(`{"type":"integer","enum":[1,5,20],"description":"optional explicit decision-price-impact horizon; omitted selects the longest adequately covered horizon automatically"}`),
			"limit":            json.RawMessage(`{"type":"integer","minimum":1,"maximum":3,"description":"maximum findings; default 3"}`),
			"change_id":        schemaString("optional opaque change ID returned by a prior canary_edge call"),
			"option_id":        schemaString("optional opaque option ID returned by a prior canary_edge call"),
			"option_offset":    json.RawMessage(`{"type":"integer","minimum":0,"description":"first row of each option list (options.cycles, options.realized, options.open) to return; default 0. Pass a list's next_offset to read its next page."}`),
			"option_limit":     json.RawMessage(`{"type":"integer","minimum":1,"maximum":500,"description":"rows per option list; default 20. Counts and known totals cover every row whatever the page."}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.EdgeSnapshotParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			in, err := rpc.NormalizeEdgeSnapshotParams(in)
			if err != nil {
				return nil, err
			}
			var res rpc.EdgeResult
			if err := conn.Call(ctx, rpc.MethodEdgeSnapshot, in, &res); err != nil {
				return nil, err
			}
			if err := rpc.ValidateEdgeResult(res); err != nil {
				return nil, fmt.Errorf("invalid Edge result: %w", err)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:         "canary_scorecard",
		Title:        "Canary Decision Scorecard",
		Description:  "Read the daemon's decision scorecard: one ledger row per past position change (open, add, trim, exit) per sleeve, each scored at 5, 10 and 21 sessions after entry as the underlying's move net of the market and its sector fund in units of its own pre-decision noise (z), plus, for options, the realised-vs-implied move and the option's P&L against the delta-equivalent stock position, and the half-spread paid at the fill from the quote retained there (slippage; fills before 2026-10-18 have none). Per sleeve and stream it carries the sequential evidence on the directional score (the mean z per name-week unit; not an information coefficient): entries (open, add) are gated (established, futile or accruing) with every component disclosed, the effective sample after overlapping horizons, the e-value, the posterior P(score > 0, 0.05, 0.10), the futility e-value and the sign-shuffle placebo; exits (trim, exit) report the same statistics and are never gated; decisions on a broad index underlying (SPY, SPX, SPXW, XSP, QQQ, NDX, IWM, RUT, DIA, or a pre-entry R² on SPY of at least 0.9) are market timing and hedges, scored as the index's own move over its total pre-entry volatility in index_entries and index_exits, reported and never gated, with the realised-vs-implied median of bought puts; one name in one entry week counts once; fills Canary's own rules or the broker's lifecycle produced are the rule sleeve and count for no owner stream. Use for the question whether a strategy's decisions carry information, how far the evidence is from a decision, and what the fills cost in spread; quote figures with their sample and never call a sleeve skilled before its gate reads established. Not for price consequences in money (use canary_edge), the monthly cost report (canary_reporting_costs), current risk (canary_stress) or holdings (canary_positions). Unknown scores stay unknown with a reason. Read-only; the daemon evaluates, the read cannot refresh history.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodScorecardRead},
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"sleeve": schemaString("optional sleeve name, lower case, for example owner; omitted returns every sleeve"),
			"limit":  json.RawMessage(`{"type":"integer","minimum":1,"maximum":2000,"description":"decisions to return, newest first; default 50. The sleeve evidence always covers every decision."}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.ScorecardReadParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := scorecard.Fetch(ctx, conn, in)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},

	{
		Name: "canary_lending_market", Title: "Canary Lending Stock Context", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodLendingMarket},
		Description: "Compare prices and liquidity for 1-100 named USD stocks with exact identities from the US borrowing feed or previously resolved displayed research rows. Returns cached market context and queues bounded background acquisition, one stock at a time: last completed close or recent trade with feed/date, day change, session shares, 20-completed-session average dollar turnover and YTD completed-close price return excluding dividends. Exact contract identity is required; named reads do not initiate missing-feed resolution. Missing history stays absent; pending rows fill on later reads. Five-minute context receipts; no synchronous per-symbol scan. Use canary_lending_screen to discover names, canary_lending_rates for fees, and canary_market_history for charts. Filters over these values are research, not liquidity guarantees, orders or lending enrollment. Read-only.",
		JSONSchema:  schemaObject(map[string]json.RawMessage{"symbols": json.RawMessage(`{"type":"array","minItems":1,"maxItems":100,"items":{"type":"string"},"description":"Explicit US borrowing-feed stock symbols, normalized and deduplicated"}`)}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var p rpc.LendingMarketParams
			if err := unmarshalArgs(args, &p); err != nil {
				return nil, err
			}
			symbols, err := rpc.NormalizeLendingRateSymbols(p.Symbols)
			if err != nil {
				return nil, err
			}
			p.Symbols = symbols
			var result rpc.LendingMarketResult
			if err := conn.Call(ctx, rpc.MethodLendingMarket, p, &result); err != nil {
				return nil, err
			}
			if err := rpc.ValidateLendingMarketResult(result, symbols); err != nil {
				return nil, err
			}
			return json.Marshal(result)
		},
	},

	{
		Name: "canary_lending_screen", Title: "Canary US Borrow-Fee Discovery", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodLendingScreen},
		Description: "Discover unusually expensive borrowing outside known holdings or a watchlist, ranked by annualized borrower fee in IBKR's US short-stock bulk feed. Applies price, turnover, distinct source-date filters and sorting across fee/exclusion candidates before limiting output. One fair bounded worker prioritizes at most 100 returned rows in display order, taking a dated completed-close price read before yearly enrichment, while preserving background progress; reads do not fan out broker requests. Returns market evidence and covered/pending/unavailable counts across the candidate universe. Incomplete coverage is not zero matches. High dates are distinct provider dates within seven days, reset by later lower rates; repeated polls never add dates. Returns bounded rows, company names, source clocks, source health, total/usable/matching counts and truncation. Unavailable source is distinct from no matches. This is not all markets, lender yield, verified listing or liquidity, allocation, earned income or a buy recommendation. Use canary_lending_rates for named symbols and canary_lending_fees for earned income. Read-only; no orders, enrollment or risk-policy changes.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"min_price":                 json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum known USD price; default 0 disables. Applied across candidates before limit; missing evidence excluded."}`),
			"min_avg_dollar_volume_20d": json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum known average USD turnover across 20 complete sessions; default 0 disables."}`),
			"min_high_dates":            json.RawMessage(`{"type":"integer","minimum":0,"maximum":7,"description":"Minimum distinct provider dates at min_rate within seven days, reset by a later below-threshold observation; default 0 disables."}`),
			"sort_by":                   json.RawMessage(`{"type":"string","enum":["symbol","price","day_change_pct","volume","avg_dollar_volume_20d","ytd_change_pct","fee_rate","high_dates"],"description":"Field ranked before limiting results; default fee_rate. Missing values stay last."}`),
			"sort_dir":                  json.RawMessage(`{"type":"string","enum":["asc","desc"],"description":"Sort direction, default desc; symbol breaks numeric ties."}`),
			"min_rate":                  json.RawMessage(`{"type":"number","minimum":0,"description":"Minimum annualized borrower percentage, default 50; research filter only"}`),
			"limit":                     json.RawMessage(`{"type":"integer","minimum":1,"maximum":100,"description":"Maximum ranked results, default 25; matching count reports omitted results"}`),
			"exclude":                   json.RawMessage(`{"type":"array","maxItems":100,"items":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9.]{0,31}$"},"description":"Known symbols to omit, normalized case-insensitively; optional"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			in := rpc.LendingScreenParams{MinRate: 50, Limit: 25}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			p, err := rpc.NormalizeLendingScreenParams(in)
			if err != nil {
				return nil, err
			}
			var result rpc.LendingScreenResult
			if err := conn.Call(ctx, rpc.MethodLendingScreen, p, &result); err != nil {
				return nil, err
			}
			if err := rpc.ValidateLendingScreenResult(result, p); err != nil {
				return nil, err
			}
			return json.Marshal(result)
		},
	},
	{
		Name: "canary_lending_rates", Title: "Canary Indicative Borrowing Rates", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodMarketEventsSnapshot},
		Description: "Read dated indicative annualized borrowing fees for 1-100 explicit US stock symbols using Canary's existing market-event source and cache cadence. Returns symbol-level borrow_fee_coverage, nullable fee_rate, source dates, scale_status, policy_eligible and source_health. Missing, stale and unverified-scale rows are not zero or usable quotes. These are borrower rates, not the owner's lending yield, guaranteed allocation, exact contract identity, earned income or buy recommendations. Use canary_lending_fees for actual reported income. Read-only; no enrollment, policy changes or orders.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols": json.RawMessage(`{"type":"array","minItems":1,"maxItems":100,"items":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9.]{0,31}$"},"description":"Explicit US stock symbols; case-insensitive, deduplicated and sorted"}`),
		}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbols []string `json:"symbols"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			symbols, err := rpc.NormalizeLendingRateSymbols(in.Symbols)
			if err != nil {
				return nil, err
			}
			var result rpc.MarketEventsResult
			if err := conn.Call(ctx, rpc.MethodMarketEventsSnapshot, rpc.MarketEventsParams{Symbols: symbols}, &result); err != nil {
				return nil, err
			}
			if err := rpc.ValidateLendingRateScope(result, symbols); err != nil {
				return nil, err
			}
			return json.Marshal(result)
		},
	},
	{
		Name: "canary_lending_fees", Title: "Canary Stock Lending Fees", ReadOnlyHint: new(true), RPCMethods: []string{rpc.MethodFinancingFees},
		Description: "Read earned customer net stock-lending fees, native/base currency sums, report coverage and a bounded latest-first history. Use after canary_edge to expand lending income or to read historical fees after a holding closes. Missing sections, net amounts or conversion stay unavailable; fees are not extra P/L to add to broker equity and collateral is not income. Cash-payment and P/L reconciliation are unproved until linked statement evidence exists. Use canary_positions for dated loan annotations and current holdings, canary_edge for decision review, not this tool for short-borrow availability or enrollment. Read-only retained evidence; cannot fetch Flex, enroll or place orders.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"window":      schemaEnum([]string{"90d", "365d"}, "Edge period; default 365d unless paired explicit equity dates are supplied"),
			"from":        schemaString("optional exclusive opening YYYY-MM-DD equity date; requires to"),
			"to":          schemaString("optional inclusive closing YYYY-MM-DD equity date; requires from; at most 400 days"),
			"con_id":      json.RawMessage(`{"type":"integer","minimum":0,"description":"optional exact stock contract ID; omitted or zero includes all contracts"}`),
			"limit":       json.RawMessage(`{"type":"integer","minimum":1,"maximum":100,"description":"maximum fee rows per page; default 25; totals always cover the full period"}`),
			"cursor":      schemaString("opaque next_cursor returned by the prior page; bound to period, scope and contract filter"),
			"fingerprint": schemaString("optional summary fingerprint from Edge or a prior page; rejects changed evidence"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var params rpc.FinancingFeesParams
			if err := unmarshalArgs(args, &params); err != nil {
				return nil, err
			}
			params, err := rpc.NormalizeFinancingFeesParams(params)
			if err != nil {
				return nil, err
			}
			var result rpc.FinancingFeesResult
			if err := conn.Call(ctx, rpc.MethodFinancingFees, params, &result); err != nil {
				return nil, err
			}
			if err := rpc.ValidateFinancingFeesResponse(result, params); err != nil {
				return nil, fmt.Errorf("invalid lending fees: %w", err)
			}
			return json.Marshal(result)
		},
	},
	{
		Name:        "canary_rules",
		RPCMethods:  []string{rpc.MethodRulesSnapshot},
		Title:       "Canary Trading Rulebook",
		Description: "Read the desk Rulebook: each rule's verdict against the owner's limits (worst-case loss on one issuer with every leg netted, premium per option position, the premium budget, time value, expiry, earnings, loss per option, currency and net market exposure, three concentration watches that never act, and margin headroom as the worse of current and look-ahead excess liquidity), ranked hardest first, with explicit unknown inputs. `sell_only` is active while the premium budget or net exposure is at watch or act and names those rules: the premium budget then holds option buys, net exposure holds opening or adding orders that push the side it flags, and closes and reductions are never held. `policy_status` says where the limits came from (Canary's defaults, flagged `review: unreviewed` until the owner reviews the file, or the owner's rulebook-policy.toml) and `policy` carries every threshold, so this answers 'am I within my limits' and 'how much room is left'. Each row's `threshold` is the limit for its status (the act band at act, otherwise the watch band) and its evidence quotes that number; two-band rules also carry `watch_threshold` and `act_threshold`, and each offender its own `status`. A rule 1 offender carries `issuer`: its lines, legs, worst-case loss, credited and uncredited hedges and unbounded legs. Use canary_proposals for protection orders and canary_brief for the capital and drawdown ladder. Read-only: the owner changes limits with `canary rules policy set`. Advisory evidence never authorizes an order.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": json.RawMessage(`{"type":"string","description":"optional underlying symbol (case-insensitive) to narrow per-rule offender lists; an issuer group holding the symbol is kept; portfolio verdicts are unaffected"}`),
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
		Name:         "canary_policy_check",
		RPCMethods:   []string{rpc.MethodRiskPolicySnapshot},
		Title:        "Canary Policy Plausibility Check",
		ReadOnlyHint: new(true),
		Description:  "Read the plausibility check of the owner's config.toml and policy files: values read against each other, across files and against the live book. Use it for 'do my limits make sense', 'why is a ready proposal refused at the trading gate' or before suggesting a policy edit. Each finding carries `severity` (error: an order path that can never work or a contradiction, such as a bucket's max_order_notional above the order cap in force of the risk constitution's [order_limits], a missing [order_limits] key, or a watch level above its act level; warn: implausible against the book or the economics, such as a per-order cap under 2% or over 50% of NLV, a cash reserve under 2% of NLV, or a sweep minimum whose interest does not cover the commission; info: unreviewed defaults and dates about to end), `keys` (file, key, value), a one-sentence `message` and a `suggestion` with its reasoning. `skipped` names the checks that did not run (no live account, no positions); `assumptions` names every number assumed because no key carries it. Use canary_rules for the verdicts against those limits. Read-only: it never changes a limit; the owner edits the files.",
		JSONSchema:   schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.RiskPolicyResult
			if err := conn.Call(ctx, rpc.MethodRiskPolicySnapshot, struct{}{}, &res); err != nil {
				return nil, err
			}
			if res.Plausibility == nil {
				return nil, fmt.Errorf("the daemon does not report a policy check; restart it after upgrading Canary")
			}
			return json.Marshal(res.Plausibility)
		},
	},
	{
		Name:        "canary_proposals",
		RPCMethods:  []string{rpc.MethodTradeProposalsSnapshot, rpc.MethodTradeProposalsRefresh},
		Title:       "Canary Protection Proposals",
		Description: "Read-only proposals: protection candidates for existing positions and, when enabled, cash sweep and currency leveling rows. Proposal index comparisons report both fall and rise; absent measurements retain their reasons. For CLI portfolio planning, use `canary proposals reduce --portfolio --plan` to compare both directions, loss measure against room and quoted trading costs. The index-hedge plan uses `--mode hedge`; a plan is not submission authority. Protection candidates are stops, option loss exits and profit trails, closes of in-the-money long options near expiry (`option_expiry_close`), and premium-budget reductions; while the risk constitution carries [mandate], `mandate_reduction` rows cut the book back under the mandate's loss room (two consecutive scheduled reduce verdicts), largest premium at risk per name first, each with a `mandate` block (`rank`, `batch_rows`, `phase` expiry/gap/premium/stop, `contribution_pct`, `loss_base` and `loss_cut_base`, `units` of the whole cut, the verdict it was sized on, its breach `episode`, `held` with `hold_reason` while no fresh verdict confirms it) and the snapshot's `mandate_reduction` status, whose `plan` lists every cut with this cycle's `quantity` and, where the order caps hold it lower, why it `waits`. The batch is sized afresh at every verdict; it never buys back a short option, never sells the shares that cover one (a covered position needs the owner's hand, and the status names it), never cuts shares below what the long options on them deliver at exercise, and never sells protection. Pre-authorised rows carry `automatic`, `shadow` and `never_skip_veto`; governor rows carry a `budget` block whose `basis` is declared_risk_capital or rulebook (the Rulebook's premium budget and per-position limit). The `budget_reduction` status lists up to three `candidates` (ranked by open Rulebook rules relieved, then time value, then unrealised loss, each with a one-line `why`) and the whole `plan` (every order, with the `cycle` it can go in when `max_order_notional` holds it back); under basis rulebook, `rulebook_review` (reviewed, unreviewed, no_file, drift or error) names the review state of the Rulebook limits; it is advisory, and the configured mode alone decides shadow. A blocked option exit names why: `option_quote_broker_unavailable` or `option_quote_request_failed` when no quote could be read, otherwise the unmet quote, session or evidence requirement. When several buckets would sell the same exact contract, one row carries every reason under `covers`; the rows it stands for are blocked `covered_by_proposal` and name it in `covered_by`, while a pre-authorised row Canary still places stays live and is listed with `automatic: true`. A row carrying `queued` is covered by the owner's queued authorisation for the open (Canary sends it itself inside its window): it is neither a candidate nor ready to act, and `counts.queued` counts such rows. It can refresh discovery but cannot preview, submit, place, modify, cancel, or transmit an order. Not for option exercise: use canary_opportunities. When the owner enabled the cash sweep, `cash_sweep` reports per currency the cash (the lower of trade-date cash and broker-observed per-currency settled cash, named in `settled_cash_source`; without that observation it holds at settlement_unknown, and the order journal's estimate cannot authorise a sweep), commitments, free cash above `keep_cash`, cash equivalents, `cash_like` (cash plus equivalents when both are known) and a `state` (invest, redeem, hold, no_instrument, an unknown posture such as settlement_unknown or needs_your_number, or universe_unavailable / instrument_unresolved when no bill could be named, with `evidence`); `tax_reviewed` false is advisory only. Each invest row's `cash_interest` is the broker's credit rate its benefit estimate forgoes, read from the Flex statement's Interest Details (Tiers) section (`available` with the annual decimal `rate`, `tier` and `value_date`, or `unavailable` with the `reason`, when the estimate reads unavailable); it is never an entered or assumed number. Its rows (bucket `cash_sweep`, each with a `cash_sweep` block) buy or sell same-currency bills as limit DAY orders of the bill's own security type (BILL for a US bill, BILL or BOND for the others) and never convert: an invest row names its resolved `bill` (ISIN or CUSIP, maturity, quoted price per 100 of face, `min_size`, `size_increment`, `min_tick`), its `quantity` in the bill's order unit (`quantity_unit` face_1000 or face_1), `face_value` and `estimated_cost`; a redemption sells a held bill in the position's unit; each row names the `session` its order fills in. A row is blocked only by what stops the order: `fresh_bill_quote_required` (stale quote or mark), `bill_contract_rules_unavailable` or `below_minimum_increment` (a redemption the bill's size grid cannot fit), `bill_unit_mismatch` (a preview's broker WhatIf initial-margin change divided by the order's value at the assumed unit fell outside 0.005 to 1.2; buys of that bill instrument wait until a preview checks clean); a shadow row carries `shadow_mode`. Active rows are ordinary proposals the owner previews and approves (or pre-authorises with `cash_sweep`), always after the full veto window. When the owner enabled currency leveling, `currency_leveling` reports per currency the trade-date cash, its value in base, its `role` (loan or payer), the `loan_rate` or `cash_rate` measured from the broker's daily statements, and a `state` (convert, pays, in_band, deliberate_carry, hold with a reason, or cash_unavailable), with `trigger_base`, `cushion_base`, `max_slippage_bp`, `payback_days`, the order cap in force, `rates_through` and its `bundles` (one per loan: the row `keys` in send order, a `revision`, `saving_base` and `cost_base`); its rows (bucket `currency_leveling`, each with a `currency_leveling` block naming the borrowed `currency`, the paying `funding_currency`, the IDEALPRO `pair`, `cash`, `target`, `allotment`, `received`, `spent`, both rates, `bundle_id`, `leg` and `legs`) repay a currency whose cash is negative beyond the band, back between zero and the cushion and never more, from currencies whose cash earns less than the loan costs, as CASH limit DAY orders on the pairs' exact contracts that the owner approves per repayment; leveling is never pre-authorised, and its preview refuses `conversion_beyond_target`, `conversion_funding_short`, `conversion_terms_drift`, `conversion_already_working`, `conversion_bundle_needs_one_approval` or `fx_spread_beyond_bound`.",
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
		RPCMethods:   []string{rpc.MethodOpportunitiesSnapshot, rpc.MethodOpportunitiesRefresh},
		Title:        "Canary Opportunities",
		Description:  "Read-only option-exercise candidates for existing positions. It can refresh discovery but cannot preview, exercise, submit, or expose an execution token.",
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
	for i := range res.Untracked {
		sanitizeOrderViewForMCP(&res.Untracked[i])
	}
}

func sanitizeOrdersHistoryForMCP(res *rpc.OrdersHistoryResult) {
	for i := range res.Orders {
		sanitizeOrderViewForMCP(&res.Orders[i].Order)
		sanitizeOrderEventsForMCP(res.Orders[i].Events)
	}
}

func sanitizeOrderStatusForMCP(res *rpc.OrderStatusResult) {
	if res.AttachedStop != nil {
		sanitizeOrderViewForMCP(res.AttachedStop)
	}
	sanitizeOrderViewForMCP(&res.Order)
	sanitizeOrderEventsForMCP(res.Events)
}

// ExcludedCLI is the set of cli.Commands() names that intentionally have no
// MCP tool counterpart. The parity test consults this so adding a new CLI
// command without an MCP tool fails the gate unless the exclusion is recorded.
var ExcludedCLI = map[string]string{
	"watchlist": "owner-managed local preferences; typed read-only client and owner CLI, no model mutation tool",
	"locks":     "owner keep list for bulk and automatic reductions; owner CLI and console only, no model tool",
	"setups":    "owner-configured entry observations ship through CLI and the typed host client first; no model tool or automatic playbook activation",
	"chains":    "research panel of recorded option chains: bulk CSV export and run coverage for the owner, not a decision input; read-only CLI and RPC only",
	"version":   "info-only CLI verb; not useful as a tool call",
	"mcp":       "transport server mode; the MCP host starts this process, no LLM should call it as a tool",
	"daemon":    "local background service mode; autospawned by CLI/MCP clients and not an agent operation",
	"app":       "local mobile/PWA service mode with browser pairing and Web Push state; not a broker-data MCP tool",
	"setup":     "interactive local integration and credential configuration; not an LLM operation",
	"update":    "binary-management verb (replaces the canary binary from GitHub releases); not a daemon RPC, must stay user-triggered for trust-boundary reasons",
	"restart":   "local process-management verb (signals daemon processes); useful for humans and scripts, but not a broker-data MCP tool",
	"stop":      "local process-management verb (stops the daemon and app the caller is talking through); a tool call that ends order tracking and phone alerts belongs to the human at the terminal",
	"policy":    "risk-constitution surface deferred from MCP in phase 1 (internal-docs/design/risk-policy.md): its writes are human-only governance acts the daemon rejects from agents, and the read view ships CLI-first; revisit after the phase-2 manual cadence. The read-only plausibility check (policy check) is exposed as canary_policy_check",
	"recon":     "post-trade reconciliation surface deferred from MCP in phase 3a (internal-docs/design/post-trade-truth.md): dismiss/sign-off are human-only governance acts and the read view ships CLI-first, same posture as `policy`; revisit together with it",
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
