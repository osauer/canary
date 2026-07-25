# Alerts and Regime production contract

**Status:** Active source-neutral inbox and Web Push delivery authority

**Decision date:** 2026-07-22

**Owner:** osauer

## Decision

Alerts now have one durable path from a daemon-owned fact to the paired app.
The daemon decides whether a condition exists, how serious it is, and whether
it opened, escalated, or recovered. The app records that decision, decides
whether the selected notification level permits a push, and owns every
delivery attempt and receipt.

This cutover replaces the earlier record-only commissioning topology. Canary,
governance, and order-integrity alerts no longer have separate send owners.
They enter the same source-neutral candidate snapshot, inbox, unread cursor,
and dispatcher as every other alert source.

The change does not grant trading authority. An alert cannot place, modify, or
cancel an order; change a freeze or limit; approve a risk policy; or turn a
degraded observation into a decision.

## Authority boundaries

- `daemon.db` is the sole live authority for source evaluations, Regime
  publications, alert episodes, occurrence identity, and lifecycle decisions.
- The app's private `state.json` is the sole authority for the inbox, unread
  cursor, notification mode, delivery attempts, per-target receipts, and
  delivery health. The app never opens `daemon.db`.
- The daemon sends classified, redacted candidates. It does not send display
  prose, device identities, or transport instructions.
- The app does not recompute risk, dwell, severity, escalation, recovery, or
  rearm. It accepts only the versioned candidate contract and maps its closed
  presentation code to fixed app-owned copy.
- Account and broker mode are bound into opaque episode scope. Raw account,
  order, symbol, target, and subscription identities do not appear in public
  inbox or delivery-health output.
- A push-service acceptance is not proof that a physical device displayed or
  was read from the notification. The ledger reports that boundary plainly.

SQLite failure, an invalid lifecycle transition, an older or equivocal
snapshot, corrupt app state, or capacity exhaustion fails closed. Neither side
falls back to a file or legacy delivery path that could produce a second
authority.

## End-to-end path

1. The daemon evaluates the fixed source universe and publishes one typed
   snapshot with per-source coverage and freshness.
2. The app validates and atomically records that snapshot. One replacement of
   `state.json` advances the inbox, lifecycle view, unread cursor, and delivery
   eligibility together.
3. The app reconstructs due work from the durable ledger. It reserves one
   occurrence-and-target attempt before transport is allowed.
4. Immediately before sending, the dispatcher rechecks the current occurrence,
   notification mode, source evidence, target, and prior receipt under the
   store lock.
5. The dispatcher builds the payload from fixed app copy, calls Web Push, and
   commits the accepted, retryable, rejected, or uncertain outcome.

The dispatcher serializes this observe, reserve, confirm, send, and complete
sequence. Restart recovery uses the same ledger; it does not reconstruct send
authority from log lines or an old candidate list.

## Cutover baseline: no backlog page

The first complete, current snapshot in each opaque account-and-mode scope
establishes a delivery baseline. Conditions already active in that snapshot
remain visible in the inbox as `cutover_existing`, but they are never pushed.
Only a new occurrence or qualifying escalation created after the baseline can
become transport-eligible.

Changing scope archives active rows as bounded previous context. A scope
change is not evidence of recovery and does not create unread or delivery
work. Returning to the prior scope resumes its private lifecycle. Older
pre-scope ledger bytes are retained only as migration evidence; the app never
fabricates a current account scope for them.

## The delivery gate

A durable occurrence is sent only when every applicable check passes:

- its scope has a complete, current cutover baseline;
- its state is open or a daemon-qualified escalation;
- its severity is allowed by the app's current notification mode;
- the candidate evidence is current;
- the candidate's exact source row is covered, current, and still inside its
  producer-authored freshness deadline;
- the occurrence is still present in the current daemon snapshot;
- the target is active and has no accepted receipt for this occurrence;
- the app ledger is healthy enough to reserve and confirm the attempt; and
- an active subscription, signing keys, and sender are available.

An unrelated source may be unavailable without suppressing an independently
current candidate. The gate is deliberately per source. By contrast, an empty
candidate list means clear only when the whole expected source set is complete
and current; otherwise the snapshot is unknown.

A source is covered only after its current typed evaluation can express both
an active fact and a trustworthy negative. A positive-only seam or an
unimplemented producer is not complete source evidence and cannot clear or
authorize delivery.

Stale, partial, unavailable, invalid, or contradictory evidence may preserve
an existing condition as context, but it cannot authorize a new push or a
recovery. Exact producer-authored `not_due` windows remain typed context rather
than outages.

## Notification modes

The paired app exposes one global mode for that app host and all paired
devices:

| Operator choice | Stored value | New occurrences that may push |
|---|---|---|
| Off | `none` | None. Inbox history and unread state remain available. |
| Action required | `act_only` | `act` and `urgent`. |
| Watch + action | `watch_and_act` | `watch`, `act`, and `urgent`. |

`observe` severity is always inbox-only. Eligibility is sampled when an
occurrence is first recorded. Turning notifications up later does not arm an
occurrence that was created while its severity was suppressed; the daemon
must produce a new occurrence or qualifying escalation.

Notification mode changes delivery only. They do not change producer policy,
source freshness, risk limits, or whether an occurrence exists.

## Durable dedupe, retries, and health

Delivery identity is the private tuple of authority scope, occurrence, and
target. The app reserves before sending and records at most one accepted
receipt for that tuple. Replayed snapshots, repeated polls, and restarts
therefore do not create duplicate pushes.

Retryable transport failures wait 1, 5, and 15 minutes after the first three
attempts. A fourth retryable failure is terminal. A definite rejection is not
retried. A dead subscription is retired so it cannot keep degrading active
targets. A reservation abandoned before transport becomes a definite no-send
retry; a crash after the final confirmation but before outcome persistence is
reported as `interrupted_uncertain` because the app cannot prove whether the
push service saw it.

Delivery health is derived from the durable attempts for every active target,
not from the most recent callback. One successful target cannot hide another
target's retry, rejection, or uncertain outcome.

| Health | Meaning |
|---|---|
| `healthy` | No active delivery defect is recorded. |
| `degraded` | A retry, rejection, exhausted retry sequence, or uncertain interrupted send needs attention. |
| `unavailable` | State persistence or a required delivery prerequisite is unavailable. |
| `overflow` | The bounded ledger reached capacity and stopped accepting authority transitions. |

Prerequisite detail distinguishes no active subscription, unavailable signing
keys, and an unavailable sender. `last_push_service_acceptance_at` records only
transport acceptance; it does not claim device display or human attention.

## Fixed notification copy

The daemon candidate carries a closed presentation code such as market stress,
protection reconciliation, reconciliation exception, or data quality. The app
maps that code and lifecycle state to a fixed title and body. The same mapping
supplies resolved wording for ended inbox history; recovered rows are not sent.

Producer text is never interpolated into a notification. The payload contains
only the fixed title and body plus allowlisted severity, kind, destination,
display ID, and app URL. This keeps broker fields, symbols, journal text, and
source errors out of the transport surface.

## Inbox and unread state

Every new occurrence receives one monotonic attention sequence. The public
inbox exposes a redacted display ID and allowlisted presentation; producer
keys, targets, attempts, and receipts stay private.

The unread count is the exact contiguous range above one durable read-through
cursor shared by all paired devices. The app acknowledges only a complete set
that it actually rendered. A missing row, duplicate sequence, stale response,
or failed write leaves the cursor unchanged. The service worker mirrors the
server count onto the app badge; it does not invent a second unread count.

Unread occurrences are never removed by compaction. Read, ended history may be
removed after the retention window. An authority-scope change creates generic
previous context without marking the old condition recovered or read.

## Producer contract

The fixed producer universe is Canary, Regime, Rulebook, risk policy,
Protection, order integrity, reconciliation, governance, and Data Health.
Delivery health is a downstream result, not a producer in that universe.

- **Canary** consumes current account, positions, Regime, and applicable
  market-event evidence. A required source failure makes its result unknown;
  it does not become a reassuring clear or a market warning.
- **Regime** opens from producer-qualified stress states only. `data_quality`,
  stale or unavailable authority, overdue required evidence, and invalid
  `not_due` claims cannot open or recover an episode.
- **[Rulebook](trading-rulebook.md)** uses the complete unfiltered rules
  snapshot. Watch and act rows open episodes; only current
  account-and-positions-backed evidence may clear them.
- **Protection and order integrity** operate only inside their declared
  journal and complete open-order/portfolio coverage. Manual TWS orders and
  unmatched non-journaled API orders are detected as outside authority, not
  silently adopted.
- **Risk policy, reconciliation, and governance** consume the normalized
  daemon evaluation. This transport cutover does not activate unrelated
  risk-policy v4 reminder policy that remains shadow or otherwise inactive;
  independent drift, drawdown, and reconciliation-exception facts retain
  their own current producer rules.
- **Data Health** opens one condition per failing allowlisted root. Startup,
  expected `not_due`, normal computing, and intentionally disabled services do
  not manufacture outages.

Daemon-owned heartbeats keep Regime, Protection, and Data Health observations
moving without an open app or CLI. Client reads may produce an earlier
observation, but they do not create a second lifecycle authority.

## Operating and rollback rules

The operator can trust an inbox row as a durable record of what the daemon
classified and can use delivery health to see whether Web Push was accepted,
retrying, rejected, unavailable, or uncertain. Physical-device receipt still
requires a check on that device.

Disable notifications immediately if the system sends from stale or uncovered
evidence, aliases account scopes, leaks private identity, duplicates an
accepted occurrence-and-target receipt, loses lifecycle or unread state, or
hides a failed target behind a successful one. Turning the app mode to Off
stops new Web Push eligibility without deleting `daemon.db`, app evidence, or
inbox history. It does not change a broker order, freeze, limit, or risk-policy
guardrail.

## Related authority

- [Architecture](../architecture.md)
- [Sensors](../sensors.md)
- [Daemon SQLite authority](daemon-sqlite-authority.md)
- [Regime calibration](regime-calibration.md)
- [Risk governance nudges](risk-governance-nudges.md)
- [Trading harness development](../guides/trading-harness-development.md)
