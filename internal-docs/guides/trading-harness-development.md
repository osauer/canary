# Trading Harness Development With Codex

Updated: 2026-07-10 08:50 CEST

## Purpose

The target is a personal trading harness that protects assets and makes risk
decisions explicit before and after a trade. Canary is one input and one user
surface; it is not the policy authority. The harness should help the user follow
an approved process, not turn model judgment into an undocumented trading rule.

## Four Different Things

Keep these separate in every design and prompt:

1. **Policy:** the user's approved risk definition, limits, cadence, and
   exceptions.
2. **Measurement:** typed account, position, market, event, and broker evidence,
   including freshness and unavailable states.
3. **Enforcement:** advisory, shadow, pre-trade hard gate, or post-trade
   exception handling.
4. **Reporting:** what happened, which policy applied, which evidence was final,
   and what remains unreconciled.

A model may propose policy options and challenge them. Only the user approves a
policy. Code implements a versioned decision; it must not silently create one.

## Development Sequence

### 1. Define the policy decision

Use `.agents/docs/risk-policy-contract.md`. Decide units, aggregation,
risk-increasing versus risk-reducing behavior, stale-data posture, exception
authority, and operating cadence. Mark unknown choices `unapproved`.

### 2. Map evidence and authority

Use `.agents/docs/daemon-cli-trading-contract.md`. Name the authoritative
source and typed field for each decision input. Nil, stale, partial, delayed,
and unreconciled must stay distinct from zero, current, complete, live, and
final.

### 3. Implement pure semantics first

Put reusable evaluation and threshold meaning in `internal/risk`; put shared
types in `internal/rpc`; let the daemon assemble observed state and own runtime
policy. Adapters render the typed result without inventing a second verdict.

### 4. Run in shadow or advisory mode

Before a new rule blocks a trade, show what it *would* have warned or blocked,
journal the policy fingerprint and evidence quality, and observe normal and
stress cases. Resolve false positives, missing inputs, and operator workflow
friction before promotion.

### 5. Promote approved pre-trade controls

Promotion requires a human-approved policy version, explicit fail-open or
fail-closed behavior for each unavailable input, tested reduction exemptions,
an exception path with owner/reason/expiry, and redacted evidence that the gate
works across CLI, daemon, app, and MCP descriptions.

### 6. Reconcile post-trade truth

The local order journal records intent and lifecycle evidence; it is not a
broker statement. A final post-trade report must define how Activity/Flex data,
trade confirmations, fills, commissions, assignments, FX, and broker
corrections reconcile with local events. Unmatched and provisional states stay
visible until resolved.

### 7. Review the operating process

Measure whether the daily, pre-trade, post-trade/EOD, and weekly artifacts were
actually completed. A control that is correct in code but routinely bypassed,
ignored, stale, or unreconciled is not effective risk management.

## How To Use Codex At Each Phase

Ask for one kind of work at a time:

- **Map:** `Read-only. Map the current <risk> path from evidence to UI and name
  policy decisions that are missing. Do not propose code yet.`
- **Interview:** `Interview me to complete the risk-policy contract. Offer
  options and tradeoffs, but leave every threshold unapproved until I choose.`
- **Challenge:** `Use the risk-governance reviewer. Attack this policy for
  ambiguous units, hidden aggregation, stale-data behavior, exceptions, and
  ways it could increase risk.`
- **Design:** `Turn the approved policy into a typed authority matrix and test
  scenarios. Separate policy, measurement, enforcement, and reporting.`
- **Implement:** `Implement policy version <fingerprint>. Do not change policy.
  Preserve <invariants>. Done means <tests, redacted artifact, smoke tier>.`
- **Live investigate:** read the
  [Rulebook authority](../design/trading-rulebook.md), then ask:
  `Use $ibkr-harness read-only to compare the current rulebook, canary,
  positions, and data quality. Return insufficient data rather than guessing.`
- **Render/QA:** `Refresh the embedded Canary app and verify the displayed
  contract in the in-app Browser. Browser QA is read-only.`

For broker writes, the current message must identify the transaction and ask
for submission explicitly. Analysis, monitoring, policy work, previews, and
protection goals do not carry submit authority.

## Evidence To Expect Back

For a completed harness change, Codex should return:

- the policy/design authority used and any decision still unapproved;
- the typed contract and owning layer changed;
- the tests that exercise pass, breach, unknown, stale, and reduction cases;
- a redacted live artifact with schema or policy fingerprint and data quality;
- the required static/runtime/smoke results and any skip or first failure;
- residual risk, rollback, and the next human decision.

## Recommended Task Boundaries

Keep policy interviews, adversarial review, implementation, live validation,
and release as explicit phase boundaries. A single coherent change can move
through explore → implement → verify in one task, but a topic pivot or major
policy decision deserves a fresh task or compact handoff. This keeps the main
thread focused on requirements and decisions while read-only agents absorb
repo-wide exploration and test/log noise.

## First Harness Milestone

Before adding more automation, produce two approved design artifacts:

1. a risk constitution covering capital base, aggregation, sizing/loss limits,
   drawdown response, data-quality posture, exceptions, and review cadence;
2. a post-trade authority contract defining broker truth, local intent evidence,
   reconciliation, corrections, and report finality.

Only then choose the first narrow control to run manually and in shadow mode.
