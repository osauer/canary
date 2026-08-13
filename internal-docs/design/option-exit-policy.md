# Directional option exit policy

Updated: 2026-08-13
Status: implemented locally; execution parameters approved

## Decision

- **Goal and protected behavior:** stop an approved standalone directional long
  option from drifting into an unmanaged large loss, while preserving gains
  with a broker-managed trail only after the position has moved far enough in
  the trader's favor.
- **Policy owner:** desk operator.
- **Human approval:** the operator approved the four-part design on 2026-08-12:
  separate loss and profit mechanisms; conservative hedge classification;
  actionable proposals with no automatic submission; and all numeric and
  execution parameters below. On 2026-08-13 the operator approved IBKR's native
  percentage trail with an explicit `0.05` quote-currency `TRAIL LIMIT` offset.
- **Authority:** Rulebook owns the 40/60 loss-discipline lines;
  `[buckets.trailing_stop.options]` owns exact intent and profit/order
  construction. `internal/risk.EvaluateOptionExit` consumes both and the typed
  trade-proposal RPC binds both policy fingerprints.
- **Status:** advisory proposal policy. It is not protection coverage and does
  not grant broker-write authority.

## Meaning

- **Capital base:** IBKR multiplier-inclusive average cost divided by the exact
  contract multiplier, producing per-share option premium cost. The executable
  comparison price for a long exit is the fresh live bid.
- **Aggregation unit:** one exact broker option contract (`con_id`). V1 accepts
  only positive whole-contract long positions that are not part of, or
  ambiguously associated with, a reconstructed multi-leg strategy.
- **Directional intent:** the exact contract must have a time-bounded
  `directional_intents` record with reason, approval time and expiry. A
  hedge-listed index put must additionally be
  classified `directional` by the current Rulebook economic-role classifier.
  A `protection`, conflicting, or unclassified role blocks the proposal.
  Current shared-cache Greeks cannot prove exact option class, so hedge-listed
  puts remain unclassified in V1 until positive-ConID Greeks authority ships.
- **Session and quality:** proposals require the regular listed-options session,
  live fresh timestamped two-sided bid/ask, positive cost basis, at least 14
  calendar DTE, and spread no wider than 25% of mid. Missing or stale evidence
  is unknown and blocked, never treated as zero or a pass.
- **Loss discipline:** Rulebook watch remains at a 40% premium loss. At a 60%
  premium loss, the proposal engine stages a full exact-contract DAY patient
  limit close. It does not place a resting loss stop and does not promise an
  exit through a gap or illiquid market.
- **Profit discipline:** at a 50% favorable premium move, stage a full-quantity
  `TRAIL LIMIT` with `DAY` lifetime and broker-default option trigger. The
  initial native percentage premium trail is normally 30%, bounded to 20-50%,
  at least $0.10 at activation, and at
  least twice the current spread. After spread and exact tick floors, the
  rounded initial stop must still retain at least 5% over cost or the proposal
  is blocked.
- **Regime:** regime and volatility remain decision context. V1 has no VIX or
  regime multiplier because that would add calibration without replay evidence.
- **Enforcement class:** advisory generation plus pre-trade hard validation.
  Proposal, preview, and preview token are evidence only; no automatic submit.

## Authority and evidence

| Concept | Authoritative source | Typed field or contract | Freshness or finality | Fallback or blocker |
|---|---|---|---|---|
| Exact directional intent | versioned protection policy | `directional_intents[]` (`con_id`, reason, approved/expiry times) | current loaded policy fingerprint and unexpired declaration | absent, future or expired declaration blocks |
| Economic hedge role | `internal/risk` Rulebook classifier plus exact-contract Greeks gate | `LegInput.IndexPutRole`; option-exit V1 requires positive-ConID Greeks evidence | current complete positions, delta, underlying, whole-book exposure and exact option class | protection, partial/shared-cache book or unknown blocks; hedge-listed puts are therefore review-only in V1 |
| Cost and position | daemon positions snapshot from broker account state | `PositionView.AvgCost`, `Multiplier`, `Quantity`, `ConID` | current proposal refresh | missing, non-positive or fractional blocks |
| Executable option price | daemon exact-contract quote authority | positive-ConID non-sharing subscription, `OrderQuoteSnapshot` | new broker price-tick receipt after the request boundary, live and during RTH | stale, delayed, missing or wide blocks |
| Threshold decision | pure risk evaluator | `risk.OptionExitDecision` | recomputed on refresh and against the preview's newer exact quote | no threshold means no proposal; unavailable evidence emits a blocked review row |
| Broker order shape | daemon proposal engine | `TradeProposal.OptionExit`, `OrderTrailSpec`, `TIF` | revision and policy fingerprint bound | drift or non-full quantity blocks |
| Working-order conflict | current broker all-client API open-order snapshot | complete same-session `reqAllOpenOrders` receipt and positive `con_id` | rechecked before preview and submit | missing, incomplete, changed or conflicting inventory blocks |
| Broker acceptance | existing gated preview and place pipeline | WhatIf, preview token, submit eligibility, journal | transaction-specific and short-lived | every existing account, mode, origin, freeze and write gate remains binding |

## Exceptions and change control

- Only the desk operator may classify an exact contract as directional or
  approve a threshold, order-shape, or guardrail change.
- There is no symbol-wide intent fallback and no automatic `SPY put = hedge`
  or `SPY put = directional` rule. Borderline evidence remains a hedge.
- Exceptions cannot bypass strategy grouping, freshness, full-quantity,
  duplicate-order, preview, account/mode, freeze, journal, or current-turn
  broker-write authority gates.
- Any parameter or intent-record change requires a higher protection
  `policy_version`; the daemon fingerprints the resulting semantic policy.
- Roll back by disabling `[buckets.trailing_stop.options]` with a version bump.
  Existing broker-working orders are broker state and are not cancelled by a
  policy rollback.

## Operating cadence

- The daemon evaluates the policy during its ordinary proposal cadence and on
  explicit refresh; routine threshold checking is automated.
- A fresh proposal is the pre-trade artifact. The trader must still review the
  exact contract, quantity, order shape, WhatIf result, and write confirmation.
- Broker order status and broker statements remain final for fills, partial
  fills, commissions, assignments, cancellations, and corrections.
- Contract intent is recorded with an explicit expiry when the trader opens or
  reclassifies a position. Canary does not auto-renew it and does not add a
  separate periodic re-attestation ritual.

## Verification

- Pure risk fixtures cover the approved loss, arm, trail and locked-gain lines,
  plus missing intent, strategy membership, hedge conflict, quantity, DTE,
  market-data and session blockers.
- Daemon fixtures cover full-quantity DAY patient-limit loss proposals,
  `TRAIL LIMIT` profit proposals, time-bounded exact intent validation and
  DAY-only option trails. Preview validation re-runs thresholds and rejects a
  reduced or no-longer-full exact-contract close.
- SPA fixtures cover distinct option loss/profit labels, explanation and action
  copy without calling either mechanism protection coverage.
- Cross-surface proof is the typed proposal payload plus CLI/SPA rendering;
  Browser QA remains read-only and never submits a broker order.
- **Residual risk accepted by the operator:** a DAY loss proposal can remain
  unfilled and cannot cap an
  overnight move; a `TRAIL LIMIT` can trigger and remain unfilled; premium gaps,
  spread expansion and option liquidity can make realized loss worse than the
  policy line. Those limits are disclosed rather than hidden by a false stop
  guarantee.

## Activation

The approved runtime policy sets `[buckets.trailing_stop.options].enabled =
true` and explicitly supplies `limit_offset_abs = 0.05`. The loader still
refuses activation when the field is inherited or omitted. An empty
`directional_intents` list produces no option-exit candidates; each exact
contract requires its own current time-bounded intent record before evaluation.
