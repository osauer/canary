package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
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
	{
		Name:               "canary_status",
		RPCMethods:         []string{rpc.MethodStatusHealth},
		Title:              "Canary Status",
		Description:        "Read daemon, gateway, storage, and source health. Use for connectivity or degraded-input diagnosis; use canary_brief for the desk decision surface.",
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
		Description: "Read local broker-write readiness, pinned context, freeze state, and blockers. It only reports readiness and does not place, modify, or cancel orders.",
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
		Description: "Read-only current-context local order lifecycle view. It does not place, modify, cancel, or transmit orders and is not a broker statement.",
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
		Description: "Read account financials. The `authority` block identifies one concrete account and mode with availability, freshness, typed reason, and field presence; missing is never zero.",
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
		RPCMethods:  []string{rpc.MethodPositionsList},
		Title:       "Canary Positions",
		Description: "Read held positions and exposure. The `authority` block identifies one concrete account and mode with availability, freshness, and typed reason; stale or unavailable empty rows do not prove an empty book.",
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
		Name:        "canary_strategies",
		RPCMethods:  []string{rpc.MethodPositionsList},
		Title:       "Canary Option Strategies",
		Description: "Read how Canary groups currently held option legs into proportional strategies and which legs still need review. Use canary_positions for the full book. This tool cannot preview, submit, place, modify, cancel, or transmit an order.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.PositionsResult
			if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "opt"}, &res); err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Strategies []rpc.PositionStrategy      `json:"strategies"`
				Issues     []rpc.StrategyGroupingIssue `json:"issues,omitempty"`
				AsOf       time.Time                   `json:"as_of"`
			}{res.Strategies, res.StrategyIssues, res.AsOf})
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
		Name:         "canary_brief",
		Title:        "Canary Daily Brief",
		Description:  "Read-only current daily brief composed by the daemon. It never acknowledges the brief or writes to the journal. Drill into canary_positions or canary_account only when the brief points there.",
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
		Description:  "Read shared IBKR statement-reporting setup, broker reachability, backfill state, proven missing fields, and unproved empty sections for Recon and Edge. Read-only; returns no Query ID or token and cannot validate candidates, refresh reports, or change setup.",
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
		Name:         "canary_edge",
		Title:        "Canary Edge Decision Review",
		Description:  "Call with no arguments for Canary's automatic one-year review of where past decisions historically helped or hurt. Optional parameters are only for drill-down after canary_brief. Do not use for current risk, positions, order decisions, forecasting, or causal claims. It is read-only and cannot refresh data.",
		ReadOnlyHint: new(true),
		RPCMethods:   []string{rpc.MethodEdgeSnapshot},
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"window":           schemaEnum([]string{"90d", "365d"}, "optional review override; default 365d"),
			"horizon_sessions": json.RawMessage(`{"type":"integer","enum":[1,5,20],"description":"highlighted decision-price-impact horizon; default 20"}`),
			"limit":            json.RawMessage(`{"type":"integer","minimum":1,"maximum":3,"description":"maximum findings; default 3"}`),
			"change_id":        schemaString("optional opaque change ID returned by a prior canary_edge call"),
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
		Name:        "canary_rules",
		RPCMethods:  []string{rpc.MethodRulesSnapshot},
		Title:       "Canary Trading Rulebook",
		Description: "Read the daemon-evaluated desk rulebook, ranked findings, policy identity, and explicit unknown inputs. Advisory evidence never authorizes an order.",
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
		RPCMethods:  []string{rpc.MethodTradeProposalsSnapshot, rpc.MethodTradeProposalsRefresh},
		Title:       "Canary Protection Proposals",
		Description: "Read-only protection candidates for existing positions. It can refresh discovery but cannot preview, submit, place, modify, cancel, or transmit an order.",
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
var ExcludedCLI = map[string]string{
	"version": "info-only CLI verb; not useful as a tool call",
	"mcp":     "transport server mode; the MCP host starts this process, no LLM should call it as a tool",
	"daemon":  "local background service mode; autospawned by CLI/MCP clients and not an agent operation",
	"app":     "local mobile/PWA service mode with browser pairing and Web Push state; not a broker-data MCP tool",
	"setup":   "interactive local integration and credential configuration; not an LLM operation",
	"update":  "binary-management verb (replaces the canary binary from GitHub releases); not a daemon RPC, must stay user-triggered for trust-boundary reasons",
	"restart": "local process-management verb (signals daemon processes); useful for humans and scripts, but not a broker-data MCP tool",
	"stop":    "local process-management verb (stops the daemon and app the caller is talking through); a tool call that ends order tracking and phone alerts belongs to the human at the terminal",
	"policy":  "risk-constitution surface deferred from MCP in phase 1 (internal-docs/design/risk-policy.md): its writes are human-only governance acts the daemon rejects from agents, and the read view ships CLI-first; revisit after the phase-2 manual cadence",
	"recon":   "post-trade reconciliation surface deferred from MCP in phase 3a (internal-docs/design/post-trade-truth.md): dismiss/sign-off are human-only governance acts and the read view ships CLI-first, same posture as `policy`; revisit together with it",
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
