# Directional option exit policy

Updated: 2026-09-17
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
  not grant broker-write authority. When enabled, every nonzero held option is
  evaluated for review coverage, including absent or expired intent and
  unsupported short positions. An eligible position below both action
  thresholds has no action proposal; unresolved positions have blocked review
  rows rather than silently disappearing. Existing explicit ignore decisions
  remain effective.

## Meaning

- **Capital base:** IBKR multiplier-inclusive average cost divided by the exact
  contract multiplier, producing per-share option premium cost. The executable
  comparison price for a long exit is the fresh live bid.
- **Aggregation unit:** one exact broker option contract (`con_id`). V1 accepts
  only positive whole-contract long positions. Confirmed or unresolved
  multi-leg strategy membership blocks a single-leg exit. The sole exception
  is a current inferred two-leg group whose two exact long contracts each have
  a current `independent_exit = true` declaration; this affects exit management,
  not combined portfolio exposure.
  Broker position types `OPT` and `OPTION` identify the same option security;
  reconstruction accepts both and emits canonical `OPT` contracts. Exact IDs,
  whole quantities and ambiguity checks remain required.
- **Directional intent:** a current time-bounded `directional_intents` record
  with reason, approval time and expiry takes precedence. Without an exact
  declaration, approved `default_long_calls_directional` can classify standard
  ungrouped long calls when no short-book conflict exists.
  `default_index_puts_protection` keeps hedge-listed long puts in protection.
  Expired declarations and strategy conflicts remain exceptions. A
  hedge-listed index put must additionally be
  classified `directional` by the current Rulebook economic-role classifier.
  A `protection`, conflicting, or unclassified role blocks the proposal.
  Shared-cache Greeks cannot prove exact option class. The daemon collects
  fresh positive-ConID model receipts for the complete book; absent or invalid
  exact evidence keeps the role unclassified.
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
| Economic hedge role | `internal/risk` Rulebook classifier plus exact-contract Greeks gate | `LegInput.IndexPutRole`; `option_exit.economic_evidence` binds scope and receipts | current complete positions, delta, underlying, explicit FX, whole-book exposure and exact option class | protection, partial/shared-cache book or unknown blocks |
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
- An explicit independent-exit declaration may resolve only the inferred
  pair described above. It cannot override Canary strategy lineage, a
  guaranteed combo, unknown/review-required grouping, conflicting membership,
  or a grouping issue. Freshness, full-quantity, duplicate-order, preview,
  account/mode, freeze, journal, and current-turn broker-write authority gates
  remain binding.
- Any parameter or intent-record change requires a higher protection
  `policy_version`; the daemon fingerprints the resulting semantic policy.
- Roll back by disabling `[buckets.trailing_stop.options]` with a version bump.
  Existing broker-working orders are broker state and are not cancelled by a
  policy rollback.

## Operating cadence

- The daemon evaluates the policy during its ordinary proposal cadence and on
  explicit refresh; routine threshold checking is automated. Missing, future
  or expired intent produces `directional_intent_required` and
  `option_exit.intent = "unconfirmed"`. Such rows do not request new broker
  quotes or reuse shared-cache quotes to imply an executable exit.
- Each option-policy blocker includes a typed `action` describing the next
  step and whether the unresolved work concerns owner intent, broker evidence,
  a session condition or the unsupported short/strategy workflow. A blocked
  review never implies a 0% return when eligibility prevented measurement.
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
refuses activation when the field is inherited or omitted. Without an exact
intent record, only a qualifying approved standing-purpose default can resolve
intent. Missing intent, expired exact declarations and economic conflicts keep
review rows blocked. Fresh exact-contract price and risk evidence remain
required before an actionable directional exit can qualify.


## Incremental exact-contract evidence extension

The exact-contract extension is implemented locally with synthetic proof.
Regular-session broker commissioning remains separate. It introduces no new
model calls or risk thresholds:

1. Bind a fresh, non-sharing option subscription to a positive ConID, complete
   contract identity and the current broker session. Capture model-computation
   delta and underlying price from that exact request ID, with their actual
   receipt time and live/delayed classification. The existing symbol-based
   Greeks cache cannot supply this receipt; zero delta must remain distinct
   from missing delta. A new quote price tick does not freshen older Greeks.
2. Use the same exact evidence for every option contributing to the economic
   exposure calculation, together with current stock positions and explicit
   FX-to-base evidence. Require one complete broker-position scope and prove
   it did not change during collection. Missing rows, stale evidence,
   unsupported exposures, session changes or non-finite numbers keep the
   result unclassified. Do not treat an incomplete positive-exposure sum as
   "no long book" or use it as a small denominator to declare a put directional.
3. Feed those checked inputs into the existing pure Rulebook economic-role
   calculation. Preserve its protection/directional semantics and configured
   bands. Carry the exact evidence and position-scope identity into the
   proposal revision; recheck role and scope at the existing preview/submit
   boundary so a formerly directional put cannot be sold after it becomes
   portfolio protection.
4. Keep strategy grouping independent from economic role. Directional intent
   alone cannot turn two inferred legs into independent exits. The explicit
   `independent_exit` contract below may resolve that inferred pair; confirmed
   strategy lineage and unresolved grouping still block. Economic-role proof
   remains required regardless of the owner's exit-management declaration.

Synthetic acceptance witnesses must reject same-symbol/different-class or
ConID swaps, reconnects, stale/delayed computations, partial Greek components,
missing FX, an incomplete long book, and a position change between review and
execution. A complete exact book classified directional should qualify only
when the existing intent, strategy, DTE, quote, spread and order gates also
pass. Real regular-session broker evidence is a separate commissioning check;
hermetic fixtures cannot prove entitlement or live option liquidity.

### Receipt and execution contract

`pkg/ibkr.OptionRiskMeasurement` is an atomic model-computation receipt from
one non-sharing subscription. It carries the full requested contract, request
ID, physical-session epoch, request/receipt times, data type and nullable delta
and underlying. Only model ticks populate it. Partial computations replace
missing components with nil, zero delta stays observed, and price ticks cannot
freshen it. Unknown, delayed and frozen data cannot qualify.

The collector compares every nonzero position with the broker's complete
account-scoped structural projection. Unsupported instruments, missing rows,
duplicate identities, account changes and reconnects fail closed. Every option
needs exact model evidence; stocks need fresh live prices; foreign currencies
need an exact live FX quote. Existing current exact cancelled/dissolved stock
authority may account for a terminal row excluded by `analysisPositions`.
Missing prices never grant that exemption. Terminal fingerprints and validity
are rechecked through the broker wire guard.

The existing grouping and exposure aggregation feed
`risk.ClassifyCompleteIndexPutRoles`. This entry point uses the unchanged
Rulebook bands. It normalizes the put numerator to account base currency in a
private classifier copy and restores native spot pointers afterward; original
spot and FX inputs and the general advisory Rulebook path are unchanged.

Collection has a 20-second infrastructure evidence lifetime. The proof's
`as_of` is the collection request boundary; every accepted model, stock-price
and non-identity FX receipt must be at or after that boundary. Completion does
not renew the lifetime. Scope (physical session, account, structural portfolio
generation, base currency and terminal authority) and resulting role enter the
proposal revision. The exact receipt fingerprint is carried in
`option_exit.economic_evidence`; ordinary new receipts do not falsely stale an
otherwise unchanged proposal.

Both preview and submit refresh proposals, then collect/reclassify again after
the ordinary exact-order preview/WhatIf. Scope drift, protection, missing
evidence, changed intent/grouping or contract/class mismatch blocks. The newer
proof is signed into the existing preview token with expiry bounded from the
original collection boundary, then checked at admission and before broker
send. Existing all-client duplicate-order, full-quantity, freeze, mode,
account, journal and transaction-specific broker-write gates still apply.

### Waiting and actionable blockers

`option_exit.readiness` is daemon-authored and additive:

- `ready`: an action passed the row's advisory checks; it is never execution
  permission. Snapshot, account, trading and preview blockers take precedence.
- `waiting`: a `kind = "review"` row whose live collection was intentionally
  deferred after a complete current structural/account check and an official
  known closed session. It is an exit watch, not an executable order.
- `blocked`: actionable, unsupported, unknown or failed evidence, including
  mixed waiting/actionable causes and measured protection.

Waiting permits only `option_rth_closed`, `live_option_quote_required`,
`fresh_option_quote_required`, `two_sided_option_quote_required`,
`directional_role_not_confirmed` and `option_exit_measurement_unavailable`.
The last code is the consequence of the same deferred measurement, not an
additional diagnosis. All blockers remain in the payload. Reference price and
return stay unavailable. The generic role blocker alone never establishes
waiting: the collector must positively report the intentional deferral.
Previously measured protection and known model/FX/stock-data failures in the
same scope remain blocked after close. A successful live collection clears a
known data failure; closing the exchange does not.

Fixed, redacted blocker messages distinguish exact model data, stock pricing,
currency data, complete-book/scope failures, unknown calendars, intentional
deferral and measured protection. No private broker text creates authority.

### Local proof limits

Synthetic connector, risk, generation, preview-contract, token-age and wire
guard tests cover the evidence path. Regular-session entitlement, callback
availability, full-book collection latency and liquidity still require a
read-only commissioning check. The bounded sequential collector may time out
on a large/slow book; that remains unclassified and is reported as a blocker.
This change does not install or restart services, edit private policy, request
a live order preview, or submit an order. Integration/install and Desk rendering
validation belong to the parent task.

## Remaining owner choices

- Record the owner's purpose and exit-management choice for related option
  legs; do not infer either from a contract name. The owner approved the
  independent-exit capability on 2026-09-12. Actual held-contract declarations
  remain in the private policy, not this source document.
- The existing approved loss line is **60% premium loss** (40% is a Rulebook
  watch line). It creates a **DAY patient-limit close proposal**, not a resting
  loss stop. The profit trail arms at **50% premium gain**, normally trails
  **30%**, remains within **20–50%**, preserves at least **5% over cost**, and
  uses a **0.05 quote-currency TRAIL LIMIT offset**. These settings are reused,
  not newly calibrated or silently relaxed by this coverage change.
- Pre-authorised submission of option orders is not activated here. The owner
  still needs to approve the precise standing execution mandate, including
  the residual risk that a triggered limit trail can remain unfilled. Broker
  writes continue to require the current transaction-specific authority path.
- The long-option policy requires **14 DTE** and spread at most **25% of mid**.
  Shorts, near-expiry options and grouped strategies remain visible exceptions
  for their own workflows; this work does not extend the long-option policy
  to them or claim automatic protection for every option position.


## Desk execution handoff boundary

The current proposal API is suitable for read-only mechanical review. Its
preview returns a sanitized draft and token ID, while proposal submission
creates a fresh internal order preview. Desk cannot yet submit the exact
previously displayed preview through this contract. A later execution slice
must explicitly bind the reviewed native order terms, current proposal
revision and execution authority; it must not reconstruct an option or
stock/ETF trailing stop as a generic order to work around this boundary.
This coverage change neither widens that API nor activates automatic orders.


## Explicit independent exits for an inferred pair

The optional `independent_exit` boolean belongs to each existing
`[[buckets.trailing_stop.options.directional_intents]]` record. Its default is
false. Setting it requires the operator's exact-contract decision and shares
the record's reason, `approved_at` and `expires_at`; it is neither inferred from
prose nor renewed automatically. Both exact legs must have current true
values. One-sided, missing, future or expired declarations leave both legs
blocked by their inferred grouping.

Only a current `source = "inferred"` group of two distinct positive whole long
option legs can be resolved this way. Confirmed or unknown sources, guaranteed
combos, review-required groups, short/fractional legs, conflicting memberships
and `strategy_issues` still block. The original position/strategy/exposure
snapshot is retained unchanged: management can be independent while exposure
is viewed together.

The proposal records the effective choice in
`option_exit.exit_management`: `standalone`, `independent`, or
`grouped_or_unresolved`. This is separate from the current purpose declaration
(`option_exit.intent`) and the measured role (`option_exit.economic_role`). An
independent directional declaration cannot clear missing exact-contract
Greeks, a possible-protection classification, closed sessions, quote quality,
order conflicts or broker-write gates.

The boolean participates in the existing semantic policy fingerprint. A
changed declaration needs a higher `policy_version`; a same-version reload is
rejected as drift and cannot expand the active authority. To activate for an
owner-approved pair, add `independent_exit = true` to both existing private
exact-contract records and increment the current policy version, retaining the
other approved fields. This implementation does not edit the live policy.
