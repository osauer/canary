# Canary Edge

Canary Edge is a retrospective broker-truth review. It answers two different questions without blending them:

- What happened to the account after confirmed external cash and position flows?
- How did each stock or ETF position change compare with leaving the pre-trade position unchanged?

It does not grade the trader, recommend a trade, or claim that an outcome was caused by a decision. The result is deterministic: the daemon calculates and ranks it; an AI client may explain the typed result but cannot replace its arithmetic.

The normal experience is automatic. Open the Edge tab, run `canary edge`, or call `canary_edge` with no arguments. Canary reviews the retained 365-day broker history, chooses the longest adequately covered horizon, and returns no more than three concrete findings. There is no analysis form to configure and no trade-intent journal to maintain. The shorter window and explicit horizon flags remain optional CLI/MCP inspection lenses, not prerequisites for getting the result.

## Account P/L

For the selected 90- or 365-day window, account P/L is:

```text
ending equity - starting equity - statement-confirmed external flows
```

The equity and flow evidence comes from IBKR Flex. When the requested boundary date has no equity row, Edge shows the actual later start date and actual end date it used. A missing amount or conversion remains missing; it is never filled with zero.

This is the account result. It includes everything reflected in equity after external flows, including distributions, financing, borrow, option results, and market movement.

## Decision price impact

For a stock or ETF entry, add, trim, or exit, Edge compares the execution with one fixed counterfactual: leave the exact-contract pre-trade position unchanged.

```text
signed quantity change x multiplier x
(horizon close - execution VWAP) x horizon FX
- direct trade costs in base currency
```

The quantity change is signed. The same formula therefore works for long and short decisions and for buys and sells. A flip is split deterministically into an exit and a new entry; the allocated pieces add back to the original decision.

The horizons are the first, fifth, and twentieth available IBKR daily closing bars after the execution session. Horizon FX is the latest broker conversion at or before that close and must be no more than seven calendar days old. Edge suppresses a horizon if another trade, transfer, exercise, assignment, expiration, or quantity-changing corporate action touches that contract before the horizon. This avoids adding overlapping counterfactuals as if they were independent decisions.

Decision price impact is not generic P/L. It excludes distributions, financing and borrow, market impact, and anything else outside that fixed price path. Rollups report only observed totals, medians, counts, and coverage. They are not a claim of causality, predictive edge, or statistical skill.

The automatic review checks 20, 5, then 1 session and selects the longest horizon where at least three clean observations exist, one action appears at least three times, and at least 25% of eligible changes were scored. If no horizon clears those gates, Edge shows the best-covered lens but does not label it a strength or drag. An explicit `--horizon` flag keeps that selected lens explicit; it does not bypass the evidence gates.

Within the selected horizon, the headline first discards actions that fail the evidence or account-materiality gates, then selects the remaining action with the most clean observations. Ties use the fixed order open, add, trim, then exit. It calls the observed pattern a strength when total and median are both positive, a drag when both are negative, and mixed otherwise — but only after the action has at least three observations, absolute total impact reaches 0.10% of starting equity, and absolute median impact reaches 0.02% of starting equity. These labels describe historical results, not durable or predictive alpha.

Ranked findings also have an account-relative materiality gate: decision notional must reach 0.25% of starting equity and absolute Decision price impact must reach 0.02% of starting equity. Only then does Edge rank by absolute Decision price impact as a percentage of disclosed execution notional, then absolute base-currency impact, then opaque change ID. The detail list remains capped at three changes. Execution notional is:

```text
absolute changed quantity x multiplier x execution VWAP x horizon FX
```

The percentage, notional, execution VWAP, multiplier, horizon close, FX, direct costs, and base-currency result travel in the typed calculation trail. There is no hidden score.

## Market context, without inferred intent

For every scored interval, Edge also shows what happened in four fixed benchmarks:

- S&P 500 proxy (`SPY`)
- Nasdaq-100 proxy (`QQQ`)
- Dow proxy (`DIA`)
- CBOE VIX (`VIX`)

Each move runs from the last daily close before the execution session to the benchmark close on the decision's horizon day. VIX also reports the change in index points. QQQ and DIA are explicitly ETF proxies; Edge does not relabel them as the cash Nasdaq or Dow indices.

This context is informational. It never changes Decision price impact, the selected action, or the sign of a headline. It helps the user see whether a repeated result accompanied a broad rise, selloff, technology-led move, Dow-led move, or volatility shift without asking them to tag trade intent manually. Edge does not infer why the user traded. If a benchmark has no matching daily interval, the public result names it as unavailable instead of silently omitting it.

## Options: actual only

Listed options use broker-reported realized and open P/L. Several legs are called one strategy only when they share exact IBKR order linkage; otherwise Edge shows contract-level results. Open-position rows have no order linkage, so open option P/L remains contract-level. Results are ranked by absolute broker-actual P/L. The typed result reports the total option-result count and whether the public list was truncated at 20; the app says “20 of N” instead of presenting the cap as the total.

Edge never manufactures an expired-option price series. IBKR does not provide historical data for expired options through this API, so an unavailable counterfactual remains unavailable. See [IBKR historical-data limitations](https://interactivebrokers.github.io/tws-api/historical_limitations.html).

## Coverage is part of the result

Every snapshot says how many changes were found, how many stock/ETF changes were eligible by asset class, how many were scored at each horizon, which query sections were present, and why other values were unavailable. Eligibility does not disappear merely because an intervening trade suppressed every horizon; this keeps high-turnover behavior in the coverage denominator instead of comparing only isolated decisions. Reporting labels a Trades container that was not returned as `absent`; it is not evidence that the account had no trades. A present but empty Trades section is broker evidence of zero reported executions for that report. Edge preserves that distinction and keeps a valid account-P/L result visible even when no decision review can be proved. Common exclusion reasons include:

- `missing_horizon`
- `intervening_change`
- `unsupported_asset`
- `corporate_action`
- `missing_fx`
- `market_data_unavailable`
- `query_field_missing`
- `position_path_unbalanced`

If replayed quantities do not reconcile between Flex open-position anchors, Edge suppresses that contract instead of presenting a plausible-looking number.

## Lifecycle and surfaces

An Edge snapshot is in one of six states:

- `action_required`: Flex is not configured or its canonical query profile is incomplete.
- `backfilling`: the initial year or an exact-contract price series is still loading.
- `current`: the published snapshot matches the retained evidence.
- `degraded`: a last-good snapshot exists, but evidence changed or part of a refresh failed.
- `insufficient_evidence`: account evidence may be usable, but a completed one-year report did not return a Trades section, so Edge does not imply that a decision review exists. This is a terminal evidence diagnosis, not a backfill promise. If the account traded during the period, verify that Trades is selected at execution detail in the saved Activity Flex Query; if it did not, there are no decisions to score.
- `unavailable`: no safely scoped result can be served.

Use `canary edge` for the automatic concise review, `canary edge --json` for its complete typed contract, the Edge tab in the paired app, or `canary_edge` with no arguments after `canary_brief` in a full MCP profile. In the app, tap any finding to expand its position before/after, execution VWAP, costs, horizon bars, FX, market context, and typed results or exclusion reasons. The interaction is explanatory and read-only; it cannot preview or submit a trade.

For a narrower investigation, the terminal and MCP surfaces accept optional 90-day, 1-session, or 5-session overrides. They select another view of the same daemon-published evidence and do not start a refresh. `canary_reporting` gives an AI the same redacted setup and evidence blockers as `canary reporting status`; it cannot receive credentials, validate a candidate, refresh evidence, or change setup. The app loads Edge only when its tab opens. All of these reads expose no trading control.

The exact shared field profile is generated from the parser manifest in the [Canary reporting Flex query reference](../reference/edge-flex.md). Follow [Set up broker reporting](../start/reporting.md) for the Client Portal ceremony.
