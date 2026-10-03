# Canary Edge

Canary Edge is a retrospective broker-truth review. It answers three different questions without blending them:

- What happened to the account after confirmed external cash and position flows?
- How did each stock or ETF position change compare with leaving the pre-trade position unchanged?
- What realized option episodes, lifecycle activity, and dated open option P/L did the broker actually report?

It describes the price consequences of covered decisions. It does not grade the trader, recommend a trade, or claim that an outcome was caused by a decision. The result is deterministic: the daemon calculates and ranks it; an AI client may explain the typed result but cannot replace its arithmetic.

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

The automatic review checks 20, 5, then 1 session and selects the longest horizon where at least three clean observations exist, one action appears at least three times, and at least 25% of eligible changes were scored. If no horizon clears those gates, Edge shows the best-covered lens but does not publish a repeated-outcome headline. An explicit `--horizon` flag keeps that selected lens explicit; it does not bypass the evidence gates.

Within the selected horizon, the headline separates long and short changes and
selects the action/direction group with the most observations among groups that
clear the existing evidence and account-materiality gates. It states the impact,
median and scored/eligible count. It does not label a group a strength or drag.
An outcome still requires at least three observations, absolute total impact of
0.10% of starting equity and absolute median impact of 0.02% of starting equity.
These are display filters, not tests of trading skill or changes to risk policy.

## How much supports the observation?

The review puts coverage next to the headline. Each action/direction group shows
how many changes were scored, which exclusions prevented scoring and how much
execution notional was covered. That size denominator uses execution-date FX,
so the same denominator applies at every horizon. A missing amount remains
unknown: known subtotals are retained, but a percentage is withheld unless every
eligible execution notional is available.

Monthly counts, totals and medians show whether the result occurs across time.
Concentration shows the largest execution-date and exact-contract shares of
**absolute** price impact, plus the signed total after removing the largest
individual impact. A cluster of trades during one market move cannot therefore
hide behind a large observation count. These are descriptive checks; calendar
months, dates and contracts do not prove statistically independent opportunities.

## Compare the same decisions

The review compares 1 versus 5 and 1 versus 20 sessions using only decisions
scored at both endpoints. It discloses the common sample, each endpoint total,
total difference and median paired difference. Direct costs are already present
in each endpoint. A difference also includes any change in the disclosed horizon
FX; it is not a pure local-currency price return.

The original matrix remains visible, with an explicit different-samples label.
Its columns cannot tell the user to hold longer or exit earlier because their
populations change as intervening activity excludes longer horizons. The matched
comparisons answer the narrower question of how later closes changed the fixed
counterfactual for exactly the same observed decisions.

Ranked findings also have an account-relative materiality gate: decision notional must reach 0.25% of starting equity and absolute Decision price impact must reach 0.02% of starting equity. Only then does Edge rank by absolute Decision price impact as a percentage of disclosed execution notional, then absolute base-currency impact, then opaque change ID. The detail list remains capped at three changes. Ranking uses horizon-converted decision notional, separate from the execution-date
notional used for coverage:

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

## Stock lending income

Under account P/L, expand **Lending income earned** to read reported customer net
fees, latest first. Expand a fee for its earning date, loan quantity, net rate,
collateral and statement conversion. **Show more fees** reads another bounded
page; the total remains for the full period, independent of the page or security
filter. Historical fees remain available after a position closes.

Positions show a dated loan badge only when the exact stock contract and the
statement's owned quantity agree with the current holding. The expanded position
opens that contract's fee history. An outdated observation says **stale report**;
a missing badge does not prove non-enrollment or that no shares were lent.

The fee period uses the account result's actual equity boundaries: the opening
date is excluded and the closing date included. Signed corrections replace prior
reported fees. Native amounts remain visible when FX is missing; incomplete
coverage is labelled partial and never becomes a complete total. Collateral has
a repayment obligation and is neither income nor available sweep funding.

The first version reports earned fees. Payment status and reconciliation to
equity/accruals remain **unproved**, so Canary does not label fees as paid, unpaid,
or included in account P/L. Do not add them to the P/L already shown. This feature
does not change lending participation, place orders or report short-borrow costs.
Account privacy masks fee totals and expanded monetary details.

```sh
canary lending fees --json
canary lending fees --con-id 900901 --limit 25 --json
```

The contract in the second example is fictional. CLI, `canary_lending_fees` and
authenticated `GET /api/financing/fees` use the same retained daemon evidence.
Reads do not trigger Flex downloads. Cursors and optional fingerprints reject
changed account, query, period, filter or statement evidence. Add the optional
sections through the [existing reporting setup](../start/reporting.md#optional-stock-lending).
The XML path and display are fixture-tested; a populated IBKR report is still
required to commission the account-specific schema and P/L/payment linkage.

## Options: broker actual, in separate scopes

Options are a first-class review surface, but they cannot credibly use the stock/ETF counterfactual. Edge therefore keeps three broker-evidence scopes separate:

- **Activity coverage** counts exact execution episodes as opening, closing, mixed, or unknown lifecycle, plus unmatched exercise, assignment, and expiration events. An opening-only execution with broker-reported zero realized P/L remains visible in coverage and is not promoted into the realized ranking.
- **Realized episodes** use only broker-reported realized P/L inside the selected review window. Executions share one episode only when they carry the same IBKR order identity on the same day. A row without order linkage remains its own execution episode. An `OptionEAE` row is a separate lifecycle event unless its broker trade identity matches an already retained trade.
- **Latest open snapshot** uses only non-zero option positions in the latest dated Flex Open Positions snapshot. A present snapshot retains its date even when it confirms zero open options, while absent snapshot evidence remains undated. Its unrealized P/L is not added to historical realized P/L and is never presented as one all-time option total.

The compact public lists seat at least one gain and one loss when both exist, then rank by absolute broker P/L. Realized and open lists each cap at 20 rows and disclose the full count, known P/L subtotal, sign counts, completeness counts, and truncation. Rows without numeric P/L stay in those counts but are not magnitude-ranked. Missing realized P/L, open P/L, FX conversion, or contract metadata remains typed missing evidence; none becomes zero.

Tap an option row in the app, pass `--option option_…` to the CLI, or pass `option_id` to `canary_edge` to read the evidence behind that opaque result. A realized detail carries exact contracts, opening/closing indicator, side, quantity, execution price, multiplier, broker realized P/L, and direct costs where reported. An open detail carries the dated contract, side, quantity, mark, cost basis, and broker open P/L. This is evidence expansion, not a trading control.

Exact shared-order grouping does not assert a strategy name. A mixed opening-and-closing order is reported as `mixed`, not called a roll. Edge does not infer trade intent, holding period, return on risk, max risk, Greeks, IV, theta, or a cross-order strategy identity. It also never manufactures an expired-option price series; an unavailable historical option counterfactual remains unavailable. See [IBKR historical-data limitations](https://interactivebrokers.github.io/tws-api/historical_limitations.html).

## Completed option positions

Alongside order-level episodes, Edge reconstructs exact-contract positions that
started flat, opened, and returned to flat inside the selected window. Adds and
partial closes stay together; reopening after flat starts another position.
Raw execution chronology, open/close indicators and dated quantity anchors must
agree. Long and short positions remain distinct.

Closing-only history or an add to an already-held position cannot prove the
opening. Unknown quantities, ambiguous simultaneous opposing executions,
cross-zero fills, inconsistent anchors and lifecycle/transfer/corporate-action
activity keep the affected contract unproved. This first version conservatively
excludes contracts touched by those lifecycle events rather than allocating
underlying deliveries or inventing expired-option values. Positions opened
before the window are counted as boundary exclusions. Nonzero ending quantity
is still open, not completed.

Each completed row includes its dates, execution count, broker realized P/L and
missing-evidence status. The bounded list discloses truncation. These positions
are a **subset of realized activity**, not another amount to add to it. They are
not reconstructed multi-leg strategies, and their counts are not a strategy win
rate. No holding-period or return-on-risk recommendation is inferred.

## Local protection context

Where a Flex execution exactly matches a scoped local execution record and a
uniquely consistent prior submitted Canary protection proposal, Edge can name
that protection category. It requires agreement on execution, contract, account,
mode, order and preview identities. Ticker similarity, nearby timestamps, free
text and current portfolio stress cannot supply the link.

Every execution in a grouped change must agree before the whole change receives
a linked purpose. Partial links remain partial; missing or conflicting evidence
leaves purpose unknown. Local context is separately dated and checked against
its retained evidence. When that evidence changes or cannot be read completely,
prior links are withheld while the broker price review remains available. This
also applies when the optional local-history read reaches its completeness cap. The
next normal rebuild incorporates the new context.

A protection link proves local provenance, not broker-confirmed intent, policy
compliance or protection effectiveness. An exit followed by a rebound can have
negative price impact while still having been sensible risk management.

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

Use `canary edge` for the automatic concise review, `canary edge --json` for its complete typed contract, the Edge tab in the paired app, or `canary_edge` with no arguments after `canary_brief` in a full MCP profile. In the app, tap a stock/ETF finding to expand its position before/after, execution VWAP, costs, horizon bars, FX, market context, and typed results or exclusion reasons. Tap an option row to expand its exact broker facts and missing-evidence state. Both interactions are explanatory and read-only; neither can preview or submit a trade.

For a narrower investigation, the terminal and MCP surfaces accept optional 90-day, 1-session, or 5-session overrides. They select another view of the same daemon-published evidence and do not start a refresh. `canary_reporting` gives an AI the same redacted setup and evidence blockers as `canary reporting status`; it cannot receive credentials, validate a candidate, refresh evidence, or change setup. The app loads Edge only when its tab opens. All of these reads expose no trading control.

The exact shared field profile is generated from the parser manifest in the [Canary reporting Flex query reference](../reference/edge-flex.md). Follow [Set up broker reporting](../start/reporting.md) for the Client Portal ceremony.

### Lending research and borrowing rates

`canary lending rates --symbols AAA,BBB --json` and MCP
`canary_lending_rates` (`{"symbols":["AAA","BBB"]}`) read the same
`market_events.snapshot` evidence for 1–100 US stock symbols. The symbols here
are fictional. The existing source cache and acquisition cadence apply.

`borrow_fee_coverage` preserves nullable annualized borrower fees, source and
receipt dates, coverage status, scale status and source health. Only observed,
current, percent-annualized evidence is usable for comparison. Missing evidence
is not a zero rate. Symbol-level bulk rows do not establish exact contract
identity, SYEP eligibility or allocation. Historical FEE_RATE fallbacks with
unverified numeric scale remain unsuitable for rate comparisons.

A high borrower fee can identify lending research candidates but is not a
quoted lender yield or a reason to buy. Any income scenario must separately
state its assumed lender rate, revenue share, collateral basis and time actually
on loan. Rates and allocation can change. Use `canary lending fees` for actual
reported customer income; never add a hypothetical estimate to broker P/L.

### Broader US fee discovery

Use `canary lending screen --min-rate 50 --limit 25 --exclude AAA,BBB --json`
or `canary_lending_screen` with `{"min_rate":50,"limit":25,"exclude":["AAA","BBB"]}`
to discover names beyond an existing list. The example symbols are fictional.
CLI/MCP default to 50% and 25 rows; limits are 1–100, exclusions at most 100.
The shared `lending.screen` RPC defaults to 50 rows and accepts a nonnegative
minimum annualized borrower percentage. These are research filters, not risk policy.

The daemon scans its existing IBKR US short-stock bulk feed, reusing acquisition
cadence, durable cache, calendar and failure backoff. There is no per-symbol
broker fan-out, independent download or unverified historical-rate fallback.
Only usable, published, finite, nonnegative USD rates qualify; results sort by
fee descending, then symbol. Names come from the provider and do not establish
exact broker identity, exchange listing, liquidity or lending eligibility.

`total` counts parsed USD records, `usable` counts valid published USD rates,
`matching` counts those meeting the filter after exclusions, and `truncated`
means more matches exist than returned rows. `skipped_rows` reports malformed
feed rows omitted by the source parser. `status: unavailable` is distinct from
an observed zero-match screen. Both source and receipt clocks, plus source health,
remain visible. Stale/failed feeds never rank last-good quotes as current.
This covers the US feed, not every market; distressed and OTC names may appear.
No enrollment, watchlist mutation, policy change or order is authorized.
