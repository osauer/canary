# Risk Policy Contract Template

Copy this into a task design or use it as a checklist before changing a risk
definition, threshold, pre-trade gate, post-trade report, exception path, or
process-adherence rule. This records a human policy decision; it does not create
one. Leave an item `unapproved` when the user has not decided it, and do not
replace that gap with a model default.

## Decision

- Goal and protected asset or behavior:
- Policy owner:
- Human decision or approval reference:
- Current authoritative document/code/fingerprint:
- Status: proposed / approved / implemented / retired:

## Meaning

- Capital or exposure base:
- Aggregation unit (account / thesis / underlying / strategy / currency):
- Risk-increasing, risk-reducing, and unknown classifications:
- Measurement horizon and market-session assumptions:
- Threshold or rule, including units:
- Enforcement class: advisory / pre-trade hard gate / post-trade exception:
- Unknown, stale, partial, and unavailable-data posture:

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
|  |  |  |  |  |

For post-trade reporting, distinguish broker truth (Activity/Flex/trade
confirmations, fills, commissions, assignments, FX, and corrections) from the
local intent/lifecycle journal. Define reconciliation keys, partial fills, late
corrections, unmatched-state escalation, and when a report becomes final.

## Exceptions And Change Control

- Who may grant an exception:
- Required reason, scope, expiry, and audit record:
- Whether exceptions can increase risk:
- Rollback trigger and operator action:
- Policy version/fingerprint transition:

## Operating Cadence

- Pre-open or daily review artifact:
- Pre-trade artifact and acknowledgement:
- Post-trade/EOD reconciliation artifact:
- Weekly review and breach follow-up:
- Missed-run or stale-report escalation:
- Shadow/manual observation period before hard enforcement:

## Verification

- Historical or fixture scenarios, including breaches and near misses:
- Property/invariant tests:
- Adversarial free-text and prompt-injection cases:
- Cross-surface parity checks:
- Redacted before/after artifact:
- Residual risk accepted by the user:
