# Canary Edge

Canary Edge is a retrospective broker-truth review. It answers two different questions without blending them:

- What happened to the account after confirmed external cash and position flows?
- How did each stock or ETF position change compare with leaving the pre-trade position unchanged?

It does not grade the trader, recommend a trade, or claim that an outcome was caused by a decision. The result is deterministic: the daemon calculates and ranks it; an AI client may explain the typed result but cannot replace its arithmetic.

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

The headline selects the action with the most clean observations at the chosen horizon; ties use the fixed order open, add, trim, then exit. This favors a repeated behavioral pattern over one large position. The detail list is capped at three changes and ranks by absolute Decision price impact as a percentage of disclosed execution notional, then absolute base-currency impact, then opaque change ID. Execution notional is:

```text
absolute changed quantity x multiplier x execution VWAP x horizon FX
```

The percentage, notional, execution VWAP, multiplier, horizon close, FX, direct costs, and base-currency result travel in the typed calculation trail. There is no hidden score.

## Options: actual only

Listed options use broker-reported realized and open P/L. Several legs are called one strategy only when they share exact IBKR order linkage; otherwise Edge shows contract-level results. Open-position rows have no order linkage, so open option P/L remains contract-level.

Edge never manufactures an expired-option price series. IBKR does not provide historical data for expired options through this API, so an unavailable counterfactual remains unavailable. See [IBKR historical-data limitations](https://interactivebrokers.github.io/tws-api/historical_limitations.html).

## Coverage is part of the result

Every snapshot says how many changes were found, how many were eligible, how many were scored at each horizon, which query sections were present, and why other values were unavailable. An absent Trades section is `unproved`, not evidence that the account had no trades. A present but empty Trades section is broker evidence of zero reported executions for that report. Edge preserves that distinction and keeps a valid account-P/L result visible even when no decision review can be proved. Common exclusion reasons include:

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
- `insufficient_evidence`: account evidence may be usable, but the returned reports do not prove the Trades section, so Edge does not imply that a decision review exists.
- `unavailable`: no safely scoped result can be served.

Use `canary edge` for a concise terminal review, `canary edge --json` for the complete typed contract, the Edge tab in the paired app, or `canary_edge` after `canary_brief` in a full MCP profile. `canary_reporting` gives an AI the same redacted setup and evidence blockers as `canary reporting status`; it cannot receive credentials, validate a candidate, refresh evidence, or change setup. The app loads Edge only when its tab opens. These reads never start Flex or historical-data work and expose no trading control.

The exact shared field profile is generated from the parser manifest in the [Canary reporting Flex query reference](../reference/edge-flex.md). Follow [Set up broker reporting](../start/reporting.md) for the Client Portal ceremony.
