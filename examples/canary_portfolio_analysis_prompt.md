# Canary Portfolio Review — MCP Workflow

Last updated: 2026-08-09

Use Canary's read-only MCP tools to review the user's current Interactive
Brokers context. Produce analysis and review tasks only. The MCP server has 13
tools and no resource subscriptions, settings writes, order previews, or broker
execution tools.

## Workflow

1. Call `canary_status`. Stop if the gateway is disconnected or account scope
   is unresolved. A stale or unavailable source is not green because it still
   returned a value.
2. Call `canary_brief`. Treat its source-health, freshness, warning codes, and
   session context as part of the answer, not footnotes.
3. Call `canary_account`, `canary_positions`, and `canary_rules` when the brief
   points to portfolio or rulebook work. Require matching account authority and
   field-availability flags; missing is not zero.
4. Use `canary_technical` only for explicitly named stocks or ETFs where daily
   trend, relative strength, ATR, or liquidity evidence would change the review.
5. Use `canary_proposals` for close/reduce-only protection candidates and
   `canary_opportunities` for option-exercise candidates. They may refresh
   daemon analysis but cannot preview or execute an action.
6. Use `canary_trading_status`, `canary_orders_open`,
   `canary_orders_history`, or `canary_order_status` only to explain current
   readiness or lifecycle state. These tools do not authorize a broker action.

The daemon also evaluates calendar, quote, event, breadth, gamma, regime,
stress, and reconciliation inputs. Consume those through the brief, rules,
proposals, and app when present; do not invent retired MCP tool names for the
underlying sensors.

## Output

Lead with a one-paragraph executive snapshot, then provide:

- a compact risk dashboard for concentration, options, liquidity/data quality,
  market context, rulebook, FX, and margin;
- three to seven ranked findings with evidence, implication, confidence, and
  limitations;
- a per-underlying position summary when the available data supports one;
- concrete next reviews, never orders; and
- tools used plus every stale, degraded, held, partial, or unavailable source.

Do not infer sector, factor, beta, correlation, tax status, or user intent from
symbols alone. Do not zero-fill missing P&L, Greeks, IV, OI, FX, or quote fields.
Closed-market and entitlement limits are environmental facts, not proof of a
product defect and not proof that the data is current.

Do not place, preview, modify, cancel, exercise, or submit anything. End with:
"Analytical context only; not financial advice or an order recommendation."
