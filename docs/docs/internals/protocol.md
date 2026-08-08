# TWS wire protocol

Updated: 2026-07-25 20:23 CEST

`pkg/ibkr` is a clean-room Go implementation of the TWS wire protocol. It is
not a full replacement for every TWS API method; it covers the broker reads and
narrow order-wire operations used by the `canary` binary and daemon.

## Order and exercise gates

Unrestricted `SubmitOrder`/`CancelOrder`, `PlaceOrder`/`CancelOrder`, and
`ExerciseOptions` methods are present in every build, but their
position-changing wire paths are enabled only in trading-capable builds;
default builds return `pkg/ibkr.ErrTradingDisabled` before writing such a
frame.

The narrower `SubmitPaperOrder`, `PlacePaperOrder`, and `CancelPaperOrder`
methods are present in both builds and validate a concrete paper account plus
matching connection coordinates. That library-level evidence is not submit
authority. Broker WhatIf previews are also available in both builds, but they
do not create working orders and never grant submit authority.

The daemon adds trading status, pinned account/mode, preview-token, freeze,
journal, broker, and origin checks; the MCP server remains preview/read-oriented
and exposes no place/modify/cancel tools.

Order caps bind every equity/ETF or single-leg option order, including an
apparent close or reduction. `reqAllOpenOrders` covers API-created orders across
clients but cannot prove that a manual TWS order has not already consumed exit
capacity, so sell-side apparent exits also pass the short/sell-to-open gates
under worst-case exposure. If that blocks a genuine exit, use TWS and then
refresh and reconcile the daemon.

The account-base unit used by those caps is exact-session authority. A
completed one-shot account-summary response must identify it through the
explicit currency tag or one consistent suffix from the closed allowlist of
ordinary aggregate value tags; `$LEDGER:ALL` rows and `ExchangeRate=1` are
ineligible. Preview and token redemption bind the value plus provenance to the
same concrete broker session. A reconnect or redemption-time change rejects
the token. The first-byte guard treats the redeemed value as immutable for that
session and never issues a new broker request or reparses an unstamped cache.

Option-exercise preview likewise returns the typed
`exercise_submission_unavailable` blocker and mints no token because exact
option-to-underlying risk policy and durable one-shot authority are not yet
approved. These disabled daemon capabilities do not become available merely
because the lower-level wire method or a platform workflow setting exists.

## Semantic fingerprints

Monitor-facing decision surfaces expose a semantic `fingerprint` object:

```json
{"version": "stress-fp-v2", "key": "sha256:..."}
```

The key is a SHA-256 hash of classified state rather than of the full JSON
response. It deliberately excludes timestamps, raw prices, exact observed
values inside the same threshold bucket, methodology prose, row guidance text,
and rendering order. Monitors should use the key for dedupe, recovery, and
repeat-alert TTL policy instead of recomputing their own hash.

`regime.snapshot` emits `regime-fp-v2` from indicator bands/statuses,
composite counts, lifecycle stage/severity/readiness buckets, warning
codes/scopes/severities, high-level data quality, source-health buckets, and
gamma/breadth semantic state. V2 adds the allowlisted failure code/stage when a
source carries typed failure evidence.

`market_events.snapshot` emits `market-events-fp-v3` from requested symbols,
semantic flag categories/statuses/severities/roles/sources, source-health
buckets, and the typed identity/status of borrow-fee coverage. It excludes
timestamps, exact rates/share counts, retry timing, source prose, and rendering
order. V3 retains the V2 typed failure code/stage projection and adds coverage
scope, exact-contract identity, source/data type, entitlement, scale status,
policy eligibility, and closed reason/status buckets.

`stress` emits `stress-fp-v2` from policy, action, market confirmation,
portfolio fit, input health, direction, severity, planner mode/readiness,
primary drivers, signal semantics including held-underlying stress buckets,
classified market state, source-health buckets, row titles/states, and
`source_fingerprints.account`, `source_fingerprints.positions`,
`source_fingerprints.regime`, and `source_fingerprints.market_events`.
V2 includes typed source failure
code/stage. The separate `established_alert_projection` intentionally retains
its frozen `stress-fp-v1` compatibility identity for delivery continuity.

[`rules.snapshot`](../../../internal-docs/design/trading-rulebook.md) emits
`rulebook-fp-v3`, a policy-identity fingerprint rather than a classified-state
hash. The key is a SHA-256 of a JSON projection of the active Rulebook model
(rule set, thresholds, regime-conditional sets), so it changes when the model
changes, not when verdicts move. The rules-decisions journal records the same
`policy_fingerprint` with every transition, naming the model identity that
produced each journaled verdict.

Regime also exposes a nested `lifecycle.fingerprint` for consumers that dedupe
by broad-market lifecycle transition rather than by the full regime snapshot.
Stress is stateless and exposes no lifecycle fingerprint of its own. Source
fingerprints and source-health entries use semantic buckets only; timestamps,
tiny raw-value movement inside a bucket, and prose changes must not churn the
hash.

## Market events

`market_events.snapshot` returns `MarketEventsResult` for explicit symbols or,
when symbols are omitted, held stock/ETF underlyings from the positions snapshot.
The result contains `flags[]`, `by_symbol`, `source_health[]`,
`borrow_fee_coverage[]`, `warning_details[]`, `fingerprint`, and
`not_execution`.

`MarketEventFlag.id` is one of `borrow_inventory_tight`,
`borrow_fee_extreme`, `reg_sho_threshold`, `luld_pause`, or
`halt_regulatory_or_news`. Event `status` uses event terms such as `active` and
`recent`; source freshness uses `source_health[].status` values such as `ok`,
`stale`, `unknown`, or `degraded`.

`source_health[]` may also carry durable `refresh_state`, `next_attempt`, and
redacted typed `last_failure` fields (`code`, `stage`, `failed_at`,
`retryable`). Consumers must respect backoff/not-due state and use these typed
fields instead of parsing provider or transport prose from `notes`.

### Flag sources

Borrow inventory comes from IBKR shortable-share market data. The global FTP
short-stock availability file remains the primary borrow-fee source and is the
only source currently eligible to emit `borrow_fee_extreme` at or above 50%
annualized. A stale FTP last-good never emits or clears that flag.

When a regular-session FTP refresh is due but fails, is backed off, or returns
unusable evidence, the daemon may request historical `FEE_RATE` only for exact
positive-ConID stock contracts that are currently held short in a complete,
fresh, same-account portfolio stream. It never resolves a symbol, invents a
route, or queries a long/non-held name. `borrow_fee_coverage[]` discloses
`coverage_scope` (`global` or `portfolio_only`), optional
`contract_con_id`/`contract_fingerprint`, `source`, `data_type`, nullable
`fee_rate`, `as_of`, `observed_at`, `status`, `reason`, `entitlement`,
`scale_status`, `policy_eligible`, and a redacted typed `last_failure`.

Historical `FEE_RATE` is portfolio-only context with
`scale_status=unverified` and `policy_eligible=false` until a controlled broker
fixture commissions its numeric scale. It therefore cannot emit or clear the
50% flag; missing, unentitled, malformed, paced, or unavailable evidence stays
nullable and explicit rather than becoming zero. Outside the FTP publication
window, `refresh_state=not_due` suppresses the fallback. Reg SHO V1 uses
Nasdaq's threshold list, so non-Nasdaq listing-exchange absence remains outside
coverage. LULD and halt context comes from Nasdaq trade-halt evidence.
Runtime entitlement, failure, and backoff are not restored after a restart;
only the exact identical-wire 15-second retry boundary is durable.

Flags are context and safety gates, not execution authority. Active halt/LULD
can block proposal preview/submit; borrow flags modify existing short
buy-to-cover reductions only; `reg_sho_threshold` is regulatory context unless
paired with an existing reduce/cover proposal.

## Capability coverage

| Capability | Wire opcodes | Library entry point | Status |
|---|---|---|---|
| Account summary | `reqAccountSummary` (62), `accountSummary` (63), `acctValue` (6) | `Connector.RequestAccountSummary`, `GetAccountSummary` | ready |
| Positions + portfolio | `reqPositions` (61), `position` (61), `portfolioValue` (7), `$LEDGER:ALL` | `Connector.CachedPositions` | ready |
| Snapshot quote | `reqMktData` (1) snapshot=true, `tickPrice` (1), `tickSnapshotEnd` (57) | `Connector.FetchMarketSnapshot` | ready |
| Streaming quote | `reqMktData` (1) snapshot=false, `tickPrice` / `tickSize` / `tickGeneric` / `tickString` | `Connector.SubscribeMarketData`, `MarketDataSnapshot` | ready |
| Generic-tick set | gen-ticks `100,101,104,106,165,221,233,236` (option volume, open interest, historical and implied vol, misc stats, mark price, RTVolume, shortable) | populated into `MarketData` automatically | ready |
| Contract resolution | `reqContractData` (9), `contractData` (10) | `Connector.FetchContractDetails` | ready |
| Option chains | `reqSecDefOptParams` (78), `tickOptionComputation` (21) | `Connector.FetchOptionExpiries`, `FetchOptionExpiryStrikes`, `OptionGreeks`, `OptionIV` | ready |
| Daily historical bars | `reqHistoricalData` (20), `historicalData` (17), `historicalDataEnd` (17) | `Connector.FetchHistoricalDailyBars`, exact-contract `Connector.FetchHistoricalDailyFeeRates` | ready; `FEE_RATE` is a held-short daemon fallback and remains policy-ineligible until its scale fixture is commissioned |
| Market-data type switch | `reqMarketDataType` (59), `marketDataType` (58) | `Connector.SetMarketDataType` | ready |
| Unrestricted order placement / cancel | `placeOrder` (3), `cancelOrder` (4) | `Connector.SubmitOrder`, `CancelOrder`; `Connection.PlaceOrder`, `CancelOrder` | disabled by default (`ErrTradingDisabled`); `-tags trading` only |
| Paper-gated order placement / cancel | `placeOrder` (3), `cancelOrder` (4) | `Connector.SubmitPaperOrder`, `CancelPaperOrder`; `Connection.PlacePaperOrder`, `CancelPaperOrder` | all builds; validates paper account and connection coordinates, but does not replace application authorization |
| Broker WhatIf preview | `placeOrder` (3) with WhatIf | `Connector.PreviewOrderWhatIf`, `Connection.PreviewOrderWhatIf` | all builds; broker evaluation that does not create a working order, never submit authority |
| Option exercise / lapse | `exerciseOptions` (21) | `Connector.ExerciseOptions`, `Connection.ExerciseOptions` | low-level wire disabled by default (`ErrTradingDisabled`) and `-tags trading` only; daemon preview/submission remains typed-disabled with no token |
| API open-order snapshot | `reqAllOpenOrders` (5) | `Connector.SnapshotOpenOrders`, `Connection.RequestAllOpenOrders` | all builds; covers API-created orders across clients, not manual TWS orders by itself |
| Real-time bars | `reqRealTimeBars` (50) | - | not implemented |
| Market depth (L2) | `reqMktDepth` (10), `reqMktDepthL2` (13) | - | not implemented |
| Fundamental data | `reqFundamentalData` (52) | - | not implemented |
| Wall Street Horizon earnings dates | `reqWshMetaData` (100), `reqWshEventData` (102) | `Connector.FetchWSHEarnings` | ready; read-only, subscription-gated |
| News bulletins | `reqNewsBulletins` (12) | - | not implemented |
| Financial Advisor (FA) | `reqFA` (18) | - | not implemented |
| IV / option-price calculators | `reqCalcImpliedVolatility` (54), `reqCalcOptionPrice` (55) | - | not implemented |

Tests exercise handshake and parser behavior across IB Gateway server versions
100 through 203. Runtime connections reject versions below 124 and negotiate
the highest supported protocol version with newer gateways.
