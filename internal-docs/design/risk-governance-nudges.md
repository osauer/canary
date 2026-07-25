# Risk-Governance Nudges and Monthly Pulse (L2)

Updated: 2026-07-18
Status: implementation approved by the operator on 2026-07-18; no code or live
policy change yet. Code may land behind the current v3 policy. Live v4
activation remains blocked on the automatic same-day reconciliation proof.

This design refines the open L2 work in
[`operator-ergonomics.md`](operator-ergonomics.md). That document remains the
policy approval record. This document owns the implementation shape: typed
contracts, trigger identities, delivery evidence, failure behavior, cluster
boundaries, and acceptance gates. It does not duplicate capital limits or
authorize a policy-file edit.

## Outcome

The paired phone receives a lock-screen-safe advisory nudge when the risk
constitution needs attention. The clean case stays quiet. The monthly pulse is
the only standing review touchpoint; every other nudge is caused by a due
clock, exception, new broker-confirmed flow, shadow would-block occurrence,
drawdown-latch episode, or sibling policy drift.

Success is tangible only when all of the following are true:

1. The daemon derives nudge eligibility from authoritative typed state without
   gateway connectivity and without changing that state on a read.
2. The app sends only daemon-authored safe copy and journals every transport
   attempt per active subscription/device, including no subscription and
   push-service failure.
3. One stable occurrence produces at most one push-service acceptance per
   active subscription/device, while failures retry independently and a
   materially new occurrence can notify again.
4. The actual paired phone receives a notification, shows no private account
   data on the lock screen, and opens the paired PWA when tapped.
5. Notification failure never changes policy, risk state, reconciliation,
   trading eligibility, freeze, or any broker-write path.

## Activation Gate: Prove Risk-Policy v3 Before Enabling L2

Live v4 activation starts only after the first automatic clean-report extension
is observed on a same-day statement/runtime equity pair. Code may be developed,
tested, and committed behind the current v3 policy before that proof. The
2026-07-18 read-only snapshot was healthy and exception-free, but its statement
and runtime observations were from different days, so the automatic path had
not yet proved itself.

The proof is read-only and redacted:

- `ibkr recon show --json` reports an active report, statement source `ok`,
  zero unresolved exceptions, and a same-day equity pair.
- `ibkr policy show --json` reports policy v3, statement-authoritative flows,
  and `last_reconcile_source=automatic` for that report.
- The automatic event occurs once for the report and advances the reconcile
  evidence time without changing the policy fingerprint, peak, tier, or latch.
- If no same-day pair arrives, the result is "not proved yet". No manual
  sign-off or data fabrication substitutes for the missing evidence.

This is an operational activation gate, not a dependency for implementing and
reviewing inactive v4 support.

## Policy Contract

### Goal and protected behavior

- Goal: make the advisory risk constitution exception-driven in daily use.
- Protected behavior: reconciliation freshness, visibility of reconciliation
  exceptions and external cash flows, drawdown-latch awareness, and sibling
  policy identity review.
- Policy owner: the operator.
- Enforcement class: advisory notification only.
- Current authority: `risk-policy.toml` plus the approved decisions in
  `operator-ergonomics.md`.

### Later decisions supersede the old artefact-nudge wording

The early L2 list says to nudge when a morning/EOD artefact is incomplete. The
later consolidated decision retires routine daily and weekly sign-off duties
and keeps one monthly pulse. Preserving daily overdue alerts would recreate the
ritual the later decision removed.

This design therefore applies the later decision:

- morning and EOD render stamps remain passive adherence evidence;
- they do not create notification duties;
- weekly folds into the monthly pulse;
- the monthly pulse is the only artefact-like due nudge.

Changing that interpretation is an operator policy decision, not an
implementation detail.

### Proposed risk-policy v4 shape

Policy v4 carries the new cadence semantics. `schema_version` remains 1. The
operator must write every value; code supplies no defaults.

```toml
policy_version = 4

[cadence.nudges]
timezone = "Europe/Berlin"
reconcile_warning_days = 2

[cadence.monthly]
class = "advisory"
day_of_month = 1
nudge_at_local = "09:00"
```

These values were approved on 2026-07-18 for the next operator-authored policy
revision. This document and the implementation request do not authorize a live
policy edit before the activation gate passes.

The v4 fingerprint projection includes the new cadence fields. The v1-v3
projections must remain byte-for-byte stable so a new binary cannot make an
unchanged older policy drift. V4-only keys on an older `policy_version` are
rejected. Missing v4 material values are reported through `UnapprovedKeys` and
disable only the dependent timed nudges; they are never filled from host time,
environment, or code constants.

Morning/EOD records may remain in the schema for backward-compatible passive
measurement. A v4 policy does not interpret them as notification duties.

## Approved Implementation Decisions

The operator approved this pack on 2026-07-18:

| Decision | Recommended choice | Status |
|---|---|---|
| Nudge timezone | `Europe/Berlin`; never implicit host local time | `approved` |
| Monthly schedule | Day 1 at local `09:00` | `approved` |
| Monthly completion | An authenticated paired-device foreground render records completion only when all sibling pins are readable and match; it is not proof that a person reviewed the content | `approved` |
| Persistent-condition repeats | Once per stable occurrence per active subscription/device; no periodic reminders | `approved` |
| Existing Canary alert mode | `none` disables all push delivery; `act_only` vs `watch_and_act` filters Canary only, not governance nudges | `approved` |
| Subscription target set | Every valid subscription belonging to a current non-revoked paired device; success/failure tracked independently | `approved` |
| Per-kind severity | `watch`: reconcile due soon, confirmed flow, monthly; `act`: overdue recon, recon exception, shadow would-block, open latch, policy drift | `approved` |
| Shadow-event/latch aggregation | Keep two trigger types; notify on the first qualifying risk-increasing preview in a latch episode and count later previews in the visible record | `approved` |
| Confirmed-flow cutover | Record a broker-report-anchored coverage watermark and require one explicit review of pre-cutover coverage; never silently call old rows reviewed | `approved` |
| Confirmed-flow catch-up and expiry | No unseen event expires | `approved` |
| Delivery audit retention | Retain resolved detail for 90 days; active occurrences and retry-suppressing receipts never evict | `approved` |
| Source/policy failure notification | Render persistent delivery/source health; do not emit a separate source-failure push in the first implementation | `approved` |
| Policy revision during completed month | A new policy fingerprint reopens the current monthly pulse | `approved` |

The confirmed-flow nudge is approved by the operator's agreement with the L2
assessment on 2026-07-18. It is still advisory and carries no amount or
direction on the lock screen.

This table authorizes implementation behavior, not live v4 policy activation.

## Authority Split

| Concept | Authority | Typed surface | Consumer | Failure posture |
|---|---|---|---|---|
| Nudge schedule and two-day horizon | operator-authored v4 `risk-policy.toml` | `risk.Constitution` | pure nudge evaluator | missing key = `unapproved`; no timed candidate |
| Reconcile deadline | `risk.EvaluateUnreconciledClock` | existing deadline/days fields | nudge evaluator | no evidence = no fabricated deadline |
| Recon exceptions and confirmed flows | daemon recon engine over Flex truth | `rpc.ReconResult` rows/report identity | nudge evaluator | stale/unavailable report never produces reassurance |
| Drawdown tier/latch | daemon risk-capital state | `rpc.CapitalStateReport` | nudge evaluator | unavailable stays unavailable |
| Sibling drift | policy manager comparison | `rpc.PolicyPinStatus` | nudge evaluator | `unavailable` is not silently treated as match |
| Monthly completion | authenticated paired-device foreground render; not human attestation | paired-device-render `brief.ack`, fingerprint-pinned | daemon journal | CLI/agent origin rejected; paired browser automation cannot be distinguished |
| Candidate eligibility and safe copy | daemon | new read-only `nudges.snapshot` RPC | app live service | snapshot read has no writes |
| Push subscriptions and transport | app | existing app state plus bounded per-subscription attempt history | Web Push service | failure journaled; risk/trading state unchanged |
| Tap-through destination | app enum-to-route map | `monitor` or `alerts` enum only | service worker | same-origin route only; relay outage may break reachability but does not prove push failure |
| Phone receipt | physical paired device | manual acceptance evidence | operator | push-service 2xx is not called phone delivery |

The daemon decides; the app transports. The app must never infer due state,
signability, tier, drift, or policy meaning from brief strings.

The paired-device render origin is an adherence label, not a security or human
presence proof. The authenticated app route supplies it, but a paired automated
browser can exercise that route and a local RPC caller can forge an origin
string. That limitation is acceptable only because the pulse is advisory and
changes no policy, risk, reconciliation, or trading state. UI copy says
"foreground render recorded", never "reviewed" or "approved". A stronger
user-gesture attestation would create a new recurring ritual and is not part of
this design.

There is deliberately no app-to-daemon "delivered" acknowledgement. The local
daemon socket has no app-specific authentication, so a local caller could forge
that write and suppress a control. The daemon owns candidate facts; the app
owns push-service attempt evidence and delivery deduplication. Neither store
claims physical phone receipt.

## Typed Candidate Contract

Add a read-only, gateway-independent `nudges.snapshot` RPC. Its result contains
current eligible candidates and source health. It neither creates a journal
entry nor marks anything delivered.

`NudgeSourceHealth` is daemon-authored and separate from the app poll status.
It exposes allowlisted per-input state for policy, reconciliation, capital,
sibling pins, cadence, and confirmed-flow coverage. Each input contains only
`ok | unapproved | stale | unavailable | error`, an enum reason code, and
`as_of`; the aggregate says `ready | suppressed | degraded`. It contains no raw
errors or source prose. A successful RPC with zero candidates is a clean empty
case only when the relevant inputs are `ready`; otherwise the typed health says
why eligibility is suppressed.

Each `GovernanceNudgeCandidate` contains only:

- opaque semantic fingerprint;
- allowlisted kind and state enums;
- allowlisted severity;
- daemon-authored safe title and body;
- `occurred_at`, optional `due_at`, and optional `expires_at`;
- a safe app destination (`monitor` or `alerts`), not an arbitrary URL.

It must not contain balances, currency amounts, account IDs, order references,
report IDs, statement line IDs, symbols, holdings, free-text notes, broker
descriptions, policy paths, tokens, or raw fingerprints from other surfaces.
Private identities may contribute to an internal hash, but never appear in the
candidate copy or Web Push payload.

The daemon composes title/body from enum-controlled templates. It never copies
broker, journal, policy-note, or error text into a candidate.

## Trigger Matrix

| Kind | Eligibility | Stable occurrence identity | Safe lock-screen meaning | Resolution |
|---|---|---|---|---|
| `reconcile_due` | current approved clock enters `reconcile_warning_days`; an expired clock is a distinct state | deadline + approved warning horizon + `due_soon`/`overdue` | "Reconciliation is due soon/overdue. Open IBKR for the current report." | new automatic or human reconcile evidence changes the deadline |
| `reconcile_exception` | active report has one or more unresolved exceptions | hash of normalized unresolved-exception identities and material fields | "Reconciliation needs review. Open IBKR for the exceptions." | report becomes resolved or is superseded |
| `shadow_would_block` | the daemon evaluates a risk-increasing, non-exempt preview that would be blocked under promoted enforcement and journals the shadow outcome without changing eligibility | policy fingerprint + latch episode; later previews increment an internal episode count | "A risk-increasing preview met the shadow block condition." | latch reset or a new policy fingerprint rearms |
| `drawdown_latched` | block latch is open | `latched_at` episode identity | tier and approved consumed percentage; no money values | human reset creates a new episode boundary |
| `policy_drift` | at least one sibling pin has status `drift` | hash of sorted pinned/live identity tuples | "Approved policy identities changed. Review the drift table." | every sibling returns to its approved pin, through a sibling fix/rollback or an approved policy revision |
| `confirmed_flow` | a v3+ report contains a newly incorporated `confirmed` external flow and current broker truth still contains that material row | hash of statement line identity + content identity | "A broker-confirmed external cash flow was recorded. Open IBKR to review." | accepted one-shot event; removal/supersession marks it obsolete, while a material restatement has a new identity |
| `monthly_pulse` | v4 monthly time passed and current month is not completed | local `YYYY-MM` plus the approved within-month policy-revision rule | "Monthly risk pulse is ready. Review the brief and policy pins." | valid paired-device foreground-render acknowledgement |

`shadow_would_block` and `drawdown_latched` remain separate because the
approval record names both. The existing `drawdown_block_latched` journal fact
is the open-latch transition, not proof that an order preview occurred. A
shadow-would-block event is created only on the daemon's typed, exempt-aware
risk-increasing preview path; it records no order reference and never changes
`submit_eligible`. The first qualifying preview in a latch episode notifies;
later qualifying previews increment internal episode state without widening
the candidate or push payload and without another push. A future visible
aggregate needs its own typed contract. Latch reset or a new policy fingerprint
rearms the event. The evaluator may not silently drop either trigger type.

Confirmed flows need a recoverable event rather than a current-state-only
projection: a flow is still important if the app was stopped during ingest.
V4 activation records a cutover watermark anchored internally to the active
broker-backed report content identity, activation time, and redacted row count.
The typed surface discloses `coverage_from` and whether pre-cutover flows remain
unreviewed; it never exposes the report identity or amounts. Existing rows are
not silently baselined as reviewed. A one-time paired-device cutover review is
required before live v4 activation.

Later newly incorporated `confirmed` rows append a redacted event containing
only an opaque content hash and occurrence time. `nudges.snapshot` serves the
unseen event until accepted under the approved per-subscription rule. Unseen
flow events do not expire or disappear during compaction.

Before serving an unseen flow event, the daemon revalidates its private
identity against the current active broker-backed report. Report
unavailability suppresses delivery and marks source health unavailable; it
does not resolve the event. If a later Flex restatement removes or supersedes
the row, the local event is retained as redacted audit evidence but marked
`superseded` and is no longer eligible for push. A materially restated row gets
a new identity and occurrence. Local first-seen history never outranks current
broker truth.

`reconcile_exception` identity hashes only the normalized unresolved-exception
identities and material exception fields. Unrelated confirmed-flow rows,
equity observations, report ordering, or display prose do not re-arm an
unchanged exception.

The two-day trigger preserves the existing rolling clock. `due_soon` begins
when `deadline - now <= reconcile_warning_days * 24h` while `now <= deadline`;
`overdue` begins only when `now.After(deadline)`, matching
`EvaluateUnreconciledClock`. The ceiling-rounded `days_remaining` display is
not itself the eligibility test.

No candidate says "all clear". Missing, stale, partial, or unapproved inputs
may suppress a positive claim but may not create a reassuring notification.

## Monthly Pulse

The monthly pulse is a review, not a new policy-approval mechanism:

1. The brief shows the current risk/process rows and the sibling-pin table.
2. When the monthly period is due, `stamp_target=monthly` takes priority over
   passive daily stamp targets.
3. An authenticated paired-device foreground render may call `brief.ack` with
   `kind=monthly`, the dedicated paired-device-render origin, and the rendered
   brief fingerprint.
4. The daemon accepts it only for that app-origin contract, only for the
   current due month, and only when every sibling pin is readable and matches.
   It rejects CLI/agent origins but does not claim to distinguish a person from
   paired-browser automation.
5. Drift or unavailable pin evidence leaves the pulse incomplete. Restoring a
   sibling to its approved pin or an approved policy revision—not the pulse
   acknowledgement—resolves drift.
6. Agent CLI renders remain side-effect-free and can never complete the pulse.

The existing one-stamp-per-foreground-look rule remains binding. No new
confirmation button or recurring attestation ritual is introduced.

## Delivery and Evidence Contract

Web Push travels directly from `internal/app/push` to the browser push-service
endpoint. The Cloudflare relay is not in that delivery path; it supplies remote
HTTP/SSE access and the URL opened after a tap. Therefore:

- journal push-service acceptance/failure, not "phone delivered";
- journal `no_subscription`, `missing_keys`, `sender_unavailable`, transport
  error, and non-2xx response explicitly;
- treat relay unavailability as tap-through degradation, not a push failure;
- prove receipt only on the physical paired phone.

App state gains three separate bounded records:

- governance occurrences containing daemon-authored safe copy and state;
- per-subscription delivery attempts containing allowlisted transport truth;
- a durable receipt index keyed internally by occurrence fingerprint plus
  active subscription/device identity.

Every active subscription is attempted. A 2xx response marks only that
subscription's receipt as `push_service_accepted`; it does not suppress retry
for another subscription that failed. The occurrence may summarize that one or
more push services accepted it, but neither the app nor daemon calls that
physical-phone delivery. Device removal and subscription revocation prune the
corresponding retry obligation through the existing device lifecycle.

Clearing visible governance history never clears receipts or dedupe state.
Governance records never inherit Canary account/mode fingerprint staleness.

Daemon state contains no push endpoint, subscription, device attempt, or
delivery status. It persists only the redacted event facts required to recover
one-shot candidates such as a newly confirmed flow.

The recommended retry shape is a code-owned bounded backoff per active
subscription while the candidate remains eligible. Acceptance stops retrying
only for that subscription; a resolved or expired candidate stops all retry;
every attempt is recorded. The final backoff numbers are engineering
constants, not risk-policy thresholds, and belong in tests.

The governance path does not reuse Canary's history-cap lookup as its durable
dedupe authority: dropping an old UI row must not make an unchanged latch or
drift episode notify again. Canary alert behavior stays unchanged.

The Web Push payload is an allowlist of safe title, safe body, destination enum,
kind, severity, and an app-minted non-sensitive display ID. Semantic dedupe
keys stay local and never enter the payload. Retries of one occurrence reuse
its persisted display ID; a materially new occurrence gets a new ID even when
the kind is unchanged. The service worker uses that display ID as its tag so
retries coalesce without collapsing two distinct same-kind occurrences.

The payload never contains a URL. The service worker maps `monitor` and
`alerts` to fixed same-origin routes, focuses and navigates an existing PWA
window where possible, and otherwise opens a same-origin window. Unknown
destinations fail closed to `monitor`. Tests inject hostile absolute URLs,
schemes, and origins and prove none can reach `openWindow` or `navigate`.

### Lifecycle, retention, and overflow

The daemon owns governance meaning and redacted one-shot event facts. The app
owns subscription-specific transport evidence. Flex rows remain broker truth;
neither local journal replaces them.

| Record | Owner | Creation and active life | Resolution/expiry | Retention and overflow |
|---|---|---|---|---|
| Persistent candidate (`reconcile_due`, exception, latch, drift, monthly) | daemon typed state | derived from current authoritative state; no write on snapshot | resolves only when its authoritative condition resolves | active occurrence is never evicted; no arbitrary TTL |
| One-shot event (`shadow_would_block`, confirmed flow) | daemon redacted event journal | appended at the authoritative event/cutover boundary; confirmed flows revalidate against current broker truth before serving | unseen event has no arbitrary expiry; a removed/superseded broker flow becomes obsolete but remains audit evidence | append-only active facts never overwrite; overflow sets source health to error and preserves existing facts |
| Governance occurrence | app | first observation of an opaque daemon occurrence | follows daemon resolution/expiry | visible history may be hidden separately; active record is never evicted |
| Delivery attempt | app | one record per occurrence, subscription, and attempt | terminal attempt class is immutable | resolved detail compacts after 90 days; aggregate failure/success evidence remains |
| Push-service receipt | app | 2xx for one occurrence/subscription pair | active until occurrence resolves or subscription/device is revoked | never evicted while it can suppress a retry; resolved detail compacts after 90 days |
| Delivery-health episode | app | no subscription, missing keys/sender, store failure, or unresolved transport failure | clears only after a later healthy evaluation/attempt proves recovery | current episode and last acceptance time are never hidden by history caps |

No bounded collection may silently discard an active occurrence, unseen
one-shot event, unresolved failure, or receipt that still suppresses retry. If
the configured/storage cap cannot preserve them, the app/daemon reports a
typed overflow error and stops unsafe compaction. The operator owns unresolved
delivery-health exceptions. Resolved detail retains 90 days; unseen confirmed
flows have no expiry.

## Paired-App Surface

- The Alerts tab labels governance history as `Risk & process`, distinct from
  Canary signal history.
- Existing Canary severity filtering remains conceptually separate from
  governance delivery; the exact global `none` behavior is decided before
  implementation.
- The brief process section shows monthly status as `not due`, `due`,
  `completed this month`, or `blocked by policy evidence`.
- The current-eligibility area shows `unavailable` with the last successful
  snapshot time and allowlisted error class when `nudges.snapshot` is stale or
  failing. When the RPC is current but daemon inputs are unapproved, stale, or
  unavailable, it shows the daemon-authored suppression reason. Persisted
  history remains visible; the UI never substitutes "no alerts" or "all
  clear".
- A notification tap opens the paired app on the enum-mapped same-origin
  destination. No notification action performs a governance or broker write.
- Browser automation remains read-only and renders only prebuilt completion
  fixtures. Daemon/app tests exercise acknowledgement in an isolated throwaway
  stack; the real completion comes from the user's phone.

### SPA authority matrix

These are planned, stable JSON paths. The SPA renders them directly and never
derives governance state from prose, Canary rows, or `stamp_target`.

| Visible concept | Authority | App/SPA path | Fixture states | Stale/error behavior | Rendered gate |
|---|---|---|---|---|---|
| Current eligible nudges | daemon `nudges.snapshot` | `bootstrap.snapshot.nudges.candidates[]`; live `snapshot.nudges.candidates[]` | empty, one per kind, multiple kinds | use `bootstrap.snapshot.sources.nudges`; unavailable is explicit and never means empty/clear | candidate grouping and safe-copy fixture assertions |
| Nudge RPC freshness | app live poll metadata | `bootstrap.snapshot.sources.nudges` | current, stale, allowlisted transport error | retain last successful RPC time and show unavailable | RPC-stale/unavailable browser assertion |
| Evaluator input health | daemon `NudgeSourceHealth` | `bootstrap.snapshot.nudges.source_health`; live `snapshot.nudges.source_health` | ready, suppressed by unapproved/stale/unavailable input, degraded | render allowlisted per-input state/reason/as-of; current RPC never turns suppressed authority into empty/clear | current-RPC-with-suppressed-input fixture assertion |
| Governance occurrence history | daemon safe candidate plus app occurrence record | `bootstrap.governance.occurrences[]`; same object from `GET /api/governance` | active, resolved, expired, cleared-from-view | persisted rows remain source-labelled; no Canary staleness rules | `Risk & process` grouping assertion |
| Delivery attempt evidence | app transport store | `bootstrap.governance.attempts[]`; same object from `GET /api/governance` | accepted, no subscription, missing keys/sender, timeout, non-2xx | label exact allowlisted class; never "delivered" | failed/partial/multi-device fixtures |
| Delivery health | app transport store | `bootstrap.governance.delivery_health`; same object from `GET /api/governance` | healthy, suppressed, degraded, unavailable, overflow | show current state, last service acceptance, unresolved allowlisted failure, and suppression reason | persistent failure/recovery assertions |
| Monthly pulse | daemon typed brief row | `bootstrap.snapshot.brief.process.monthly_pulse` | `not_due`, `due`, `completed`, `blocked` | unavailable or unreadable pins produce `blocked`, never client inference | all four mobile fixtures |

`bootstrap.governance` is a typed app DTO defined and tested by Cluster 2A
before Cluster 2B consumes it. Clearing the visible occurrence list is a
separate operation from receipt/dedupe retention.

## Failure Matrix

| Failure | Required behavior |
|---|---|
| Policy absent/error/drift | disclose source health; do not invent candidates or schedule values |
| V4 schedule incomplete | report keys unapproved; disable only candidates whose due calculation depends on those keys; never backfill defaults |
| Gateway disconnected | daemon state-only nudge snapshot still works from persisted policy/recon/capital state |
| Daemon disconnected | app keeps persisted history and renders current eligibility/source as unavailable |
| Flex/report stale or unavailable | no clean-report claim; reconciliation clock continues under existing policy |
| App process stopped | no false delivery record; current eligible candidate can be attempted after restart |
| No push subscription | record missed attempt; do not create a push-service receipt |
| Push service rejects/times out | record exact transport class; retry only under bounded policy |
| Relay unavailable | push may still be accepted; disclose tap-through degradation separately |
| App state write fails | do not claim dedupe or acceptance; expose one persistent failure episode |
| Candidate/journal contains hostile free text | typed evaluator ignores it; payload remains enum-template copy only |
| Notification duplicated by uncertain transport result | persisted occurrence display ID coalesces only that occurrence; attempt history remains truthful |

## Implementation Clusters

Implementation decomposes into independent clusters: the foundation lands
first; later clusters start from that integrated base and own disjoint files.

### Cluster 0: shared policy and RPC foundation

Ownership:

- `internal/risk/constitution.go`
- `internal/risk/constitution_test.go`
- `internal/risk/constitution_explain.go`
- new `internal/risk/nudges.go` and tests
- new `internal/rpc/nudges.go`
- `internal/rpc/brief.go`
- `internal/rpc/risk_policy.go`

Deliverables:

- version-aware v4 cadence schema and unapproved-key behavior;
- byte-stable v1-v3 fingerprint projections;
- pure period/due evaluation;
- `BriefKindMonthly`;
- safe nudge enums, candidate/result contract, and method name.
- daemon-authored per-input source-health enums distinct from app poll
  freshness.

Accept gate: `go test ./internal/risk ./internal/rpc` and `make check`.

### Cluster 1: daemon eligibility and monthly acknowledgement

Starts after Cluster 0. Ownership:

- new `internal/daemon/nudges.go` and tests
- `internal/daemon/server.go` and dispatch tests
- `internal/daemon/brief.go` and tests
- `internal/daemon/risk_capital_state.go` and tests
- `internal/daemon/risk_policy_handlers.go` and focused preview-warning tests
- recon accessors/tests needed by the new evaluator
- `internal/cli/brief.go` and focused tests

Deliverables:

- state-only `nudges.snapshot` handler;
- trigger assembly and stable opaque identities;
- no-write snapshot behavior;
- shadow-would-block journaling on the existing typed preview path without any
  submit-eligibility change;
- confirmed-flow cutover watermark, coverage disclosure, and the approved
  treatment of pre-cutover rows without a historical flood;
- v4 monthly target/ack while preserving v3 behavior;
- redaction and never-false-pass tests.

Accept gate: `go test -race ./internal/daemon ./internal/cli` and `make check`.

### Cluster 2A: app delivery and durable attempt evidence

Starts after Cluster 0 and can run parallel to Cluster 1. Ownership:

- `internal/app/app.go` and tests
- `internal/app/daemonclient/client.go`
- `internal/app/live/service.go` and tests
- `internal/app/alerts/alerts.go` and tests
- `internal/app/state/store.go` and tests
- `internal/app/push/push.go` and tests
- `internal/app/http/routes.go` and tests for the typed governance DTO and the
  authenticated safe notification diagnostic
- app HTTP test fakes required by the client-interface addition

Deliverables:

- one-minute candidate polling without policy re-evaluation;
- governance delivery separate from Canary alert semantics;
- retention-controlled attempt history, durable receipt index, and fail-loud
  overflow behavior;
- explicit missed-attempt records and truthful retry behavior;
- typed persistent delivery health, last service acceptance, suppression reason,
  recovery, and overflow behavior;
- fixed-copy safe test notification path through the real paired subscription,
  isolated from governance occurrences and completion state;
- exact payload allowlist and hostile-sentinel leak tests.

Accept gate: `go test -race ./internal/app/...` and `make check`.

### Cluster 2B: paired-PWA rendering and service worker

Starts after Cluster 2A publishes and tests the app-facing governance DTO. It
may overlap the remainder of Cluster 2A only after that contract is integrated.
Ownership:

- `web/app/alerts.js`
- `web/app/brief.js`
- `web/app/service-worker.js`
- `web/app/index.html` and CSS only if the reviewed design needs new elements
- relevant `web/app/*_test.go` contract tests
- browser smoke assertions for governance history/monthly status

Deliverables:

- source-aware governance history;
- monthly process status;
- safe notification click-through;
- visible `Send safe test notification` control with fixed diagnostic copy;
- executable service-worker payload, tag, focus/navigation, same-origin, and
  hostile-destination assertions (the general SPA helper currently skips the
  service worker, so these are a dedicated harness);
- mobile rendered fixtures for due, drift-blocked, completed, and failed push.

Accept gate: `make app-check` and `make check`.

### Cluster 3: docs, operator policy, and integration

This stays in the orchestrating session after the operator supplies and
authorizes the v4 values. It updates the operator template/design/reference
surfaces without touching the live policy until that exact edit is authorized.

Deliverables:

- record selected v4 cadence values and fingerprint;
- mark L2/monthly implementation status in the authority docs;
- retire remaining daily/weekly duty wording;
- add generated reference and browser-smoke assertions where needed;
- preserve unrelated working-tree changes.

## Verification Plan

### Pure and package tests

- old-policy fingerprint regression for v1, v2, and v3;
- v4 material-key, timezone, day, and time validation;
- month boundaries and DST transitions in the selected timezone;
- every trigger, resolution, and stable-identity transition;
- confirmed-flow events revalidate against current Flex truth; unavailable
  truth suppresses, removal marks obsolete, and material restatement rearms;
- repeated snapshot reads create no files or journal entries;
- stale/missing/unapproved inputs never create positive claims;
- a current empty RPC distinguishes authoritative `ready` from input-suppressed
  empty, separately from app poll stale/unavailable;
- candidate JSON and push JSON reject sentinel balances, account/order/report
  IDs, symbols, notes, and token-like strings;
- transport attempts and per-subscription receipts survive app-store reopen;
- one subscription's acceptance never suppresses another subscription's
  failure/retry, and no attempt is labelled physical delivery;
- retries reuse one display ID while distinct same-kind occurrences do not
  coalesce;
- service-worker destination mapping rejects hostile schemes, origins, and
  absolute URLs;
- current nudge source failure renders unavailable without an all-clear claim;
- monthly origin rejection, fingerprint pinning, idempotency, and rollover;
- v3 daily stamp behavior stays byte/behavior compatible.

### Primary-tree gates after integration

```sh
make test
make restart-daemon
ibkr status --json
make smoke
make app-refresh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
make app-lifecycle-smoke APP_SMOKE_BROWSER=webkit
```

Install, restart, and smoke targets run once, post-integration, on the
primary tree.

### Redacted runtime evidence

- `nudges.snapshot`: exit status, schema/version, candidate kind, safe state,
  opaque fingerprint presence, and source-health summary;
- app state: push-service status class and candidate kind, with subscription
  and candidate identifiers redacted;
- policy/recon: version, flow source, report health, unresolved count,
  same-day flag, and automatic-source assertion only;
- no balances, holdings, account IDs, report/line IDs, endpoints, keys, or raw
  logs in the completion.

### Physical-phone gate

Physical verification has three separate proofs; subscriptions are never
copied into throwaway state and live risk policy/broker state is never mutated
to manufacture a condition:

1. A hermetic daemon-to-app-to-service-worker stack proves candidate, payload,
   storage, retry, and rendering contracts with fake subscriptions. This does
   not prove phone transport.
2. A visible, authenticated `Send safe test notification` control on the real
   paired origin sends fixed non-governance copy through the live subscription.
   It is a one-off acceptance diagnostic, not a candidate, completion, or
   receipt. On the actual phone, confirm the lock screen, then tap and confirm
   same-origin focus/navigation to Alerts. This adds no relay deployment.
3. The next natural genuine governance occurrence proves the full production
   path. Until it occurs, report the path as implemented but awaiting natural
   end-to-end evidence; do not fabricate or replay live policy/recon state.

The isolated monthly fixture proves daemon validation and rendered behavior.
Actual paired-device monthly completion is verified in its natural due period;
the evidence is a paired foreground-render record, not a claim that a person
reviewed it. CLI/agent origins and drifted/unavailable pins remain rejected.

Desktop Browser or Playwright evidence can prove rendering only after embedded
assets are rebuilt and the app host is refreshed. It cannot replace the
physical-device lock-screen/tap gate.

## Rollback

- Retire v4 timing/monthly semantics only through an explicit operator-authored
  higher policy revision (v5 or later). A downgrade to v3 is not a valid
  rollback and must be rejected as policy drift.
- Before v4 activation, code can be reverted normally. After activation, never
  deploy a binary that cannot load the active v4 policy; land a forward policy
  revision and compatible code first.
- The app delivery path may be disabled or reverted while the daemon/RPC stays
  wire-compatible; app attempt history then becomes inert local evidence and
  delivery health must disclose suppression.
- Monthly acknowledgement records remain audit evidence but have no effect
  once v4 is retired.
- No rollback step touches broker orders, trading pins, freeze, preview tokens,
  reconciliation truth, capital events, or the drawdown latch.

## Explicitly Out of Scope

- hard drawdown enforcement or any broker-write change;
- freeze, limit, pin, preview-token, WhatIf, or submit semantics;
- relay deployment or Cloudflare changes;
- MCP exposure of policy/recon/brief;
- R5 dual-compute cleanup before its clean-week evidence gate;
- daily morning/EOD or weekly notification duties;
- strategy attribution, execution-quality reporting, or capital allocation;
- editing the live operator policy without an explicit later instruction.
