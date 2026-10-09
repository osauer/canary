# Read-only portfolio planning

Implementation record: 2026-10-09 06:41 CEST. The owner requested an expedited
build with focused tests during development and the full gate after integration.
This feature introduces no live policy values or broker execution authority.

`portfolio.plan`, `canary portfolio plan [--json]` and
`canary_portfolio_plan` return the same daemon-owned result. The first delivery
selects one next action across the existing book and accepted watchlist. Later
candidates remain desired additions until a fresh plan observes the first
action's actual outcome. This avoids independent Max calculations spending the
same cash or claiming that independent broker margin estimates prove a batch.

## Mandate

An optional `[portfolio_plan]` section in the risk constitution declares
`contract = "target-band-plan-v1"`, `valid_until`, and exact-stock `targets`.
Each target supplies `symbol`, positive `con_id`, `currency`, a distinct positive
`priority`, `lower_pct_nlv`, `target_pct_nlv`, `upper_pct_nlv`, `limit_price`,
`entry_regimes` and a `reason`. All target numbers are explicit. Installation
adds commented placeholders only. A missing or expired mandate yields a held
plan. It does not prevent existing independent reduction reviews.

Bands measure the exact long stock's market value divided by current NLV.
Below the lower band, a permitted current regime admits consideration of an
addition toward the target. Inside the band, hold. Above the upper band,
withhold new exposure pending an existing supported reduction review. The
planner does not construct a new generic sale or liquidate a hedge to meet a
stock allocation target. Whole shares are rounded down using the greater of
the current stock mark and the owner's purchase price ceiling. Lower positive
priority is considered first. A watchlist entry without an exact identity is
unresolved, never a proven empty holding.

The mandate grants advisory selection only. ADD still requires the separately
approved `stock-entry-v1` admission contract, allocation and order limits,
cash reserves and settled cash, canonical Rulebook checks and exact broker fee
and margin evidence. Existing option exposure remains in those checks; option,
bond and short construction are outside this first entry contract.

## Sources and selection

The daemon captures a complete account and portfolio in one concrete broker
session, the accepted watchlist revision, active policy fingerprints and the
current regime. The reduction engine publishes a precise financial book hash
and its original authority envelope with each proposal generation. Bucketed
display fingerprints cannot prove the same financial book. An old persisted
snapshot, an expired proposal generation, or any changed book holds the plan.
The configured proposal cadence bounds source age; a read does not renew it.
Read-time diagnostic clocks are excluded from the financial identity while
original broker receipt times remain bound. The producer also binds the exact
decision policies and latched regime before and after generation; a policy or
regime change cannot leave an older reduction review eligible for a new plan.

The passive proposal read reuses scope checks and decorations, but cannot
wake a proposal refresh or the automatic executor. Existing automatic and
queued instructions take precedence, including cash-sweep purchases that could
compete for cash. Reduction and protection rows retain their exact keys,
revisions, instrument or strategy-unit scope, quantities, reasons and coverage.
Covered alternatives are not added together. A stop is identified as protection
and contributes no assumed sale proceeds.

With no unresolved reduction, the first eligible stock target calls the shared
ADD planner. Its maximum must be proven; a supported quantity without a proven
maximum is disclosed as a hold. If the target needs less than the maximum, that
smaller exact order receives a separate fee and margin check. A pending purchase
of that stock must resolve before filling the target gap, so partial fills
cannot make the planner buy the outstanding quantity twice. Pending purchases
of other stocks consume their existing canonical commitments.

After sizing, broker session, position and order generations, account values,
policy, watchlist, regime and the served proposal/automatic/queue state are
checked again. A mismatch removes the selected action. The response's expiry
is the mandate expiry, not a promise that market evidence will remain current.
Every later order preview still repeats its own checks.

## Ownership and limits

The result is transient evidence. It reserves no cash, creates no preview or
confirmation token, creates no durable plan or work queue, and submits nothing.
Unassigned stocks remain visible. It does not change the existing SELL AUTO
scheduler, widen its pre-authorised buckets, or turn Desk AUTO into trading
permission. No runtime restart or installation is part of this source build.

The isolated tree builds on the revised ADD work and completed Go-analysis
repairs committed to local main as `0283ce7c` by the Add chat. That chat's
checkout remains unchanged by this work. Both build variants are scanned,
analyzer failures remain fatal, and source changes cannot reuse a dependency-only
vulnerability stamp. Local main's shared exercise outcome recorder and its
regression test are retained without alteration.

The next product integration renders the typed result in Desk and connects the
selected exact action to the existing review path. Observation history belongs
in Canary's decision evidence or Torok's durable work, with source identity
retained. Unattended portfolio execution, repeated-order budgets, re-entry
rules and recovery require a separately specified owner mandate and witnesses.

## Focused witnesses

Tests exercise competing candidates, held/watchlist overlap, missing or
ambiguous identities, target bands and regime gaps, exact smaller-order costs,
pending purchases, reduction/protection precedence and unchanged scope, expired
or changed source evidence, executor state changes without row revision changes,
and a read that cannot wake the automatic executor. CLI/MCP parity uses a
synthetic Unix-socket daemon; human output has golden fixtures. These tests do
not prove live broker timing, installation or autonomous trading.
