# Canary Portfolio Analysis — MCP Workflow

Last updated: 2026-05-29 06:36 CEST

Use Canary's MCP tools to review the user's live Interactive Brokers / TWS context. The goal is not generic personal-finance advice; it is a concrete desk workflow for traders who care about exposure, market regime, option risk, data freshness, and what to review next. Produce analysis and plans only. Do not invoke order-preview, placement, modification, cancellation, or broker-submission tools in this workflow.

Optimize for decision quality, data provenance, and a compact final answer. Use tools deliberately, parallelize independent calls when the client supports it, and stop when the portfolio picture is clear enough to produce a useful review. Do not narrate progress while running; emit only the final report or a readiness/partial-stop report.

## Defaults

- Analysis horizon: today through the next 1-8 weeks for risk review; 3-6 months only when discussing thesis durability or option expiries.
- Target budget after preflight: **8-14 tool calls**. Hard cap: **20 total tool calls** unless the user explicitly asks for a deep-dive.
- Default markets: U.S. equities/ETFs/options plus German/Xetra equities when the account or watchlist contains EUR/Xetra names.
- Do not run broad scanners in the default portfolio review. Use `canary_scan` only when the user asks for replacement ideas, hedges, or fresh candidates after the portfolio diagnosis.
- Do not fetch option chains for every option holding. Prioritize the 1-3 underlyings that drive the most delta/theta/vega, near-term expiry risk, or P&L.
- Confidence language: use **high / medium / low** confidence and name the data reason. Do not assign precise probabilities unless the user asks for a forward-looking trade plan.
- Treat missing daily P&L, missing Greeks, missing open interest, stale quotes, and closed-market option quotes as data-quality facts, not zeros.

## Workflow

### 1. Readiness Gate

Run this first. If a hard gate fails, stop with a readiness report that names the blocker, what can still be analyzed, and the exact next action for the user.

1. `canary_status`: require a connected gateway and account discovery. Read `subsystems` before deciding which follow-up tools are reliable.
2. `canary_account`: capture net liquidation value, buying power, cash, margin, base currency, daily P&L, and currency exposure.
3. `canary_positions`: capture stocks, options, `portfolio.exposure_base`, per-underlying grouping, portfolio-level Greeks, daily P&L fields, quote freshness, and FX/base-currency fields.
4. `canary_rules`: capture the current rulebook policy fingerprint, rule verdicts, missing inputs, and any existing breach. If it is unavailable or conflicts with the user's stated policy, mark the review `insufficient_policy`; do not supply a model-invented threshold.
5. `canary_stress`: use the typed top-level action, market confirmation, portfolio fit, and input health as the governed combined posture. Do not reconstruct a stronger action from lower-level signals.

Hard stop when the gateway is disconnected, the wrong account is clearly selected, or `canary_positions` cannot return holdings. If market-data subsystems are unavailable but positions are present, continue with a partial portfolio review and label quote-dependent conclusions as blocked.

`daily_pnl_ccy` / `daily_pnl_base` are populated from per-contract `reqPnLSingle` subscriptions that the first positions call may only pre-warm. If the daemon was just started and daily P&L is important to the review, rerun `canary_positions` once after the readiness gate before treating missing daily P&L as unavailable.

### 2. Market And Session Context

Only after the readiness gate passes:

1. `canary_calendar` for `market:"us"` and `market:"us-options"`.
2. `canary_calendar` for `market:"de"` only when EUR/Xetra positions or watchlist names are present.
3. `canary_regime`: use the embedded gamma and breadth context for the market backdrop. Do not separately call `canary_gamma` or `canary_breadth` unless the user specifically asks or the regime result says one of those rows is unavailable and the extra call would change the review.

Use this context to classify the review environment: regular session, pre/post-market, holiday/closed market, option market open/closed, and risk regime. If option markets are closed, option marks and `prev_close` can support context but should not be treated as executable quotes.

### 3. Portfolio Map

Build the portfolio map from `canary_account` and `canary_positions` before adding any outside context.

Deliver these diagnostics:

- **Capital base:** NLV, cash, buying power, margin usage, daily P&L, base currency.
- **Exposure map:** use `portfolio.exposure_base` first; otherwise use `by_underlying` base fields. Rank top underlyings by `market_value_base`, `% NLV`, `dollar_delta_base`, per-underlying share-equivalent effective delta, unrealized P&L base, and daily P&L base where available. Treat top-level `portfolio.effective_delta` as a coverage/debug field, not as a coherent cross-symbol account exposure.
- **Options map:** underlyings with option legs, net delta/gamma/theta/vega, near-term expiries, and whether Greeks are missing or partial.
- **Currency map:** non-base-currency exposure, FX conversion source, and where P&L attribution may mix security movement and FX. Row money fields ending in `_ccy` are contract-currency values; fields ending in `_base` are account-base values.
- **Data map:** stale, delayed, frozen, previous-close-only, wide, or missing quote rows; positions with null daily P&L; `mark_outside_bid_ask` option rows.

Do not infer sector, factor, beta, correlation, tax status, or user intent unless the data or the user's instructions provide it. If those dimensions matter, put them in "Open Questions."

### 4. Focused Enrichment

Add only the enrichment that changes the portfolio diagnosis.

- `canary_quote`: use for at most 10 non-held watchlist names or held underlyings needing refreshed stock/ETF context outside `canary_positions`.
- `canary_watch`: use when the user asks for watchlist overlap, monitoring candidates, or "what should I watch next?" Set `include_positions:true` unless the user asks only for the saved symbol list.
- `canary_technical`: batch up to 10 held or watchlist symbols when trend, relative strength, ATR, or liquidity would change the action list. Drop rows with `data_quality!="ok"` and label partial rows.
- `canary_history`: use sparingly for one-symbol deep dives when a chart-level question cannot be answered by `canary_technical`.
- `canary_chain`: use for 1-3 important option underlyings when you need option selection, expiry IV, implied moves, or a live strike grid. Do not use chain IV as the held-option valuation source; `canary_positions` already carries held option marks, option previous close, captured bid/ask context, IV, Greeks, and row warnings. First omit `expiry` with `min_dte` / `max_dte` or `target_dte` to get IV and implied move when live option readiness matters; then fetch a strike grid only when live option context is needed. Treat `options_tradable:false`, stale/model-only legs, missing bid/ask, missing IV, `live_option_iv_unavailable`, and closed option sessions as hard limits for executable option conclusions.
- `canary_gamma`: use when SPY/SPX dealer positioning directly affects the portfolio, hedging, or index-option exposure and `canary_regime` did not already provide enough detail.
- `canary_size`: use only for explicit what-if plans with entry, stop, and optional target. It is sizing math against NLV, not a recommendation or order ticket.

### 5. Review Logic

Convert the data into a ranked review, not a dump.

Prioritize:

1. Concentration and margin risks that could dominate portfolio outcomes.
2. Option Greek exposures: outsized delta, short gamma, theta bleed, vega concentration, near-expiry risks, and missing Greek coverage.
3. Liquidity and data-quality risks: stale quotes, wide spreads, option chains that are not tradable, thin names, or frozen markets.
4. Regime fit: whether the current risk regime, breadth, and gamma backdrop support or challenge the portfolio's dominant exposure.
5. Currency exposure: non-base holdings, FX sensitivity, and where sizing or P&L interpretation needs currency care.
6. Monitoring priorities: which 3-7 symbols, expiries, or risk metrics should be watched next.

Be explicit about what the portfolio is already doing. For example: "The account is primarily a long U.S. equity beta book with a short-volatility overlay" or "The book is cash-heavy but has concentrated single-name gamma risk." If the data does not support a clean characterization, say so.

### 6. Output Format

Use compact Markdown. Lead with the answer.

1. **Executive Snapshot**
   - One paragraph: account state, dominant exposure, today's P&L driver if visible, data quality, and market-regime context.

2. **Risk Dashboard**

   | Area | Status | Evidence | What to watch |
   | --- | --- | --- | --- |
   | Concentration | green/yellow/red | top exposures, % NLV, and per-underlying share-equivalent delta where available | trigger or review point |
   | Options | green/yellow/red | Greeks, expiries, IV/chain caveats | next expiry/Greek risk |
   | Liquidity/Data | green/yellow/red | quote quality, spreads, stale fields | what needs fresh data |
   | Regime | green/yellow/red | `canary_regime` punch line, breadth/gamma caveats | what would change the call |
   | Rulebook | green/yellow/red/unknown | policy fingerprint, breaches, and missing inputs | user decision or remediation |
   | FX/Margin | green/yellow/red | cash, buying power, currency exposure | threshold to monitor |

3. **Top Findings**
   - 3-7 ranked findings. Each finding must include evidence, implication, confidence, and the specific data limitation if any.

4. **Position Review**
   - Compact table by underlying: base-currency market value, % NLV, per-underlying share-equivalent effective delta, dollar delta base, P&L base, option Greeks when present, quote quality, and note.

5. **Actionable Next Reviews**
   - Concrete review tasks, not orders: "refresh option chain during U.S. option RTH," "stress-test SPY -2% / -5% exposure," "ask for a sizing check if reducing AAPL to X% NLV," "monitor XYZ expiry risk before Friday."

6. **Tool/Data Notes**
   - Tools used, stale or unavailable fields, markets closed, chain/gamma/breadth computing states, and anything that should be rerun later.

## Guardrails

- Do not place, preview, modify, or cancel orders in this workflow. Order preview is a separate non-submitting surface and is not part of portfolio review.
- Do not convert analysis into a trade instruction unless the user explicitly asks for a plan.
- Do not invent risk limits when the rulebook or user-approved policy is missing; use `insufficient_policy` and keep sizing watch-only.
- Do not treat `quote_price` as the official account valuation mark; positions carry account marks and quote context separately.
- Do not zero-fill null daily P&L, missing Greeks, missing OI, missing IV, or missing FX rates.
- Do not call stale/model-only option legs tradable.
- Do not hide uncertainty. A useful portfolio review says exactly which conclusions are blocked by market hours, entitlements, stale data, or missing context.
- Keep legal language short: "This is analytical context, not financial advice or an order recommendation."
