# Operator Ergonomics and Exception-Driven Governance (Phase 2.1)

Updated: 2026-07-23 08:15 CEST
Status: design approved by the operator (interviews 2026-07-16, consolidated
greenlight 2026-07-18). Implemented same day: the backfill backtest (decision
4/5 mechanics), risk-policy v3 (auto-extend, R3/R4, divergence gate), and the
L1 brief surface (decisions 1+3 — daemon brief, `ibkr brief`, PWA card,
render-stamps, one-tap sign-off; see the implementation records at the end).
L2 push nudges, cadence re-declaration, and the monthly pulse were approved for
implementation later on 2026-07-18; live v4 activation remains gated on the
first automatic same-day reconciliation proof. R5 cleanup remains open. This document is the approval record and the
implementation authority for the ergonomics build, the accelerated R3/R4
cutover, and the risk-policy v3 revision scope. It amends
docs/design/risk-policy.md (deferred list) and docs/design/post-trade-truth.md
(Rückbau table) on implementation; it does not duplicate their numbers.

## Diagnosis

The constitution needs about four human signatures; everything else was
paperwork and memory. The daemon knows every clock, latch, and due state but
pushes nothing — phase 1 deliberately deferred push alerts, the SPA card, and
automated reports, and Phase 2 surfaced that deferral as the dominant
operating cost immediately: artefacts stopped after day 1 (morning 2026-07-16
was the only completion as of 2026-07-18), while a real shadow-block latch ran
from 2026-07-15. A control that is correct in code but too heavy to operate is
not effective risk management (harness guide, step 7).

Principles (approved):

1. Automate evidence assembly and memory, never signatures.
2. Push, not pull: anything with a clock or a due state notifies the operator.
3. Attestation-by-review: the stamp is the act of looking, not a second act.
4. The machine owns the empty case; the human owns the exceptional case.

## Approved decisions (2026-07-16 → 2026-07-18)

1. **Brief surface.** The daemon composes a typed morning/EOD brief from
   existing results (policy tier + latch, recon clock, rules deltas since the
   last brief, canary state, Phase-2 day counter, artefacts due). Rendered by
   `ibkr brief` and a card in the paired PWA. Rendering on a human-origin
   surface journals the artefact completion; agent-origin renders never stamp.
2. **Push nudges** over the existing relay/PWA pairing: artefact not done by
   the operator's chosen time, recon clock inside 2 days, recon exception,
   shadow would-block event journaled, latch open, sibling-policy drift.
   Advisory only. Payloads carry states, tiers, and percentages — never
   balances, ids, or tokens (lockscreen-safe). Nudge times: `unapproved`
   (operator sets them in the v3 TOML).
3. **One-tap reconcile.** The sign-off verb defaults to the latest clean
   report; the brief presents it as a single confirm. Semantics unchanged.
4. **Accelerated R3/R4 via backfill backtest.** The "two future fetch cycles
   with all declarations auto-matched" trigger is retired: flows are rare in
   this account, so the gate could stall for months while months of broker
   history sit unfetched, and empty windows satisfy it vacuously. Replacement
   gate: one backfill backtest review (below).
5. **Backtest before reset.** The drawdown latch (engaged 2026-07-15, 51.9%
   of declared risk consumed as of 2026-07-18) stays until the equity replay
   has validated the runtime-observed peak and crossing dates against
   statement truth. The reset itself remains the operator's journaled act.
6. **Signatures only on drift.** Standing sign-off duties are retired; every
   human act is triggered by an exception, a breach, drift, or the operator's
   own intent — plus exactly one standing touchpoint, a **monthly** pulse
   (read the brief, glance at the drift table, confirm the pins). Approved as
   monthly; revisitable.
7. **Policy v3 bundle** (one fingerprinted revision, operator-written):
   statement-authoritative flows (R3), clean-report auto-extend of the
   reconcile clock, cadence re-declaration (stamp duties retired, weekly
   folded into exception-driven recon + the monthly pulse), nudge times, and
   an equity-divergence bound key. No value defaults in code; every number is
   `unapproved` until the operator writes it into the TOML.
8. **Build during Phase 2**, journaled as a Phase-2 event: the window
   measures the operator's process, and its first finding was that the
   process is too heavy to keep.
9. **Equity-check metric fix.** The recon equity comparison becomes same-day
   (statement EOD vs the runtime observation for that day); the current
   latest-vs-latest form conflates market movement with sampler error and
   reads worst exactly when trust matters most.

## The backfill backtest (R3/R4 gate)

**Mechanics.** The daily pipeline reaches back days; a Flex query period
reaches back up to a year. The operator adjusts the saved query's period at
IBKR (or creates a second backfill query id) — a one-time human act, same
standing as the token; agents never touch it. One fetch flows through the
existing immutable-raw-XML → `internal/flexstmt` → recon path.

**What it validates:**

- *Parser classification at scale*: months of real dividends, withholding,
  interest, fees, FX, corporate actions. Unknown lines land in
  `uncategorized` as exceptions now, in a review, not live later — retiring
  most of 3a's accepted parser-gap risk.
- *Equity replay* (the prize): the daily `EquitySummaryInBase` series replays
  the constitution against broker-true equity — does it reproduce the
  recorded high-water mark, the 2026-07-14 warn crossing, the 2026-07-15
  block crossing? This lands before the reset decision (decision 5).
- *Flow classification* on the real flows in the window.

**What it cannot validate, honestly:** statement↔ledger matching, because the
declared ledger is days old and empty of flows. Post-flip the ledger stops
being load-bearing, so this is acceptable; the meaningful check is the
operator reading the backtest's complete flow list against memory.

**Operator prior (recorded 2026-07-18):** roughly one, at most two flow
events expected in the backfill window — one known withdrawal in the last
several weeks. A backtest that finds materially more or fewer flows than the
operator remembers is itself an exception to resolve, not a pass.

**Gate:** the flip happens when the backtest report is clean or every
exception is explained, and the operator has signed the flow-list review —
the last clean-case signature of the old regime. R4 (statement value dates
replace the late-deposit heuristic) rides in the same flip. For the first
weeks the declared-flow number is still computed and displayed as a shadow
comparison, then removed (R5).

*Amended 2026-07-18 (implementation):* the originally approved anchor
("flows before the signed 2026-07-14 baseline are baked in, not
re-litigated") is **not** implemented as an automatic exclusion — an
auto-anchor would also swallow a *restated* flow inside an already-attested
window, a never-false-pass violation the existing reconcile-gate tests
expose. Instead, historical flows surface carrying a `pre_genesis`
disclosure label (value-dated before the runtime state's genesis).

*Amended again 2026-07-18 (operator decision, second interview):* per-line
dismissal of pre-genesis flows is retired before it ever ran. For a flow
dated before the runtime state existed there is exactly one valid
treatment — it is embedded in the seeded baseline; declaring it would
double-count — and a decision with one valid answer is ritual, not
judgment. Pre-genesis statement flows therefore auto-classify as
**baseline**: listed with amounts, counted, and folded into the report id
(a newly restated backdated line still changes the report id and is seen
at the next sign-off), but never an exception and never signature-gated.
The partition happens only when the runtime state is seeded (an unseeded
install baselines nothing), and a *ledger* event dated pre-genesis still
surfaces loudly as `ledger_only`. Human signatures on flows now exist only
where a real decision exists: post-genesis flows until the R3 authority
flip, anomalies always.

## Signature inventory (before → after)

| Human act | Trigger today | Trigger after |
|---|---|---|
| Reconcile sign-off | Weekly, even when clean | Exception only; clean + fresh statements + divergence in bound ⇒ clock auto-extends |
| Flow declarations | Every deposit/withdrawal | Retired (optional same-day bridge; superseded by statements) |
| Artefact stamps | Twice daily + weekly | Retired as duties; brief render stamps passively; adherence measured from behavior (3b) |
| Drawdown reset | Only when latched | Unchanged — intrinsically judgment |
| Policy revisions, overrides, freeze | Self-initiated | Unchanged |
| Earnings overrides | Whenever the feed degrades | Fallback source; ask only when no source knows and the position is oversized |
| Sibling-policy drift re-approval | Passive display in `policy show` | Push + explicit re-approval act (new ask, deliberately) |
| Monthly pulse | — | The one standing touchpoint |

**Anti-complacency backstop:** auto-extend fires only on positive fresh
evidence. No statement ⇒ no clean report ⇒ no extend ⇒ the clock expires ⇒
tier degrades ⇒ push. Silence is structurally self-limiting at
`capital.max_unreconciled_days`; never-false-pass is preserved.

## Authority

| Concept | Authoritative source | Typed contract | Renderer/tool | Fallback / unavailable |
|---|---|---|---|---|
| Brief content | daemon composition of existing typed results | new `rpc` brief result | `ibkr brief`, PWA card, push summary | degraded inputs disclosed per row, never omitted silently |
| Artefact completion | human-origin brief render | journal entry with origin + brief fingerprint | `policy show`, 3b replay | agent-origin render ⇒ no stamp |
| Nudge schedule | v3 TOML cadence keys (times `unapproved`) | `risk.Constitution` | daemon scheduler → relay push | relay down ⇒ missed nudge journaled, never blocks |
| Backfill statements | one-off long-period Flex query (operator-configured at IBKR) | existing `flexstmt` records | backtest report | fetch failure ⇒ no backtest, no flip |
| Backtest verdict | recon engine over full window + equity replay | new `rpc` backtest report (id, window, flow list, replay result) | `ibkr recon` (backtest view) | exceptions block the flip |
| cumFlows after flip | statement-confirmed flows | flows/equity store | `policy show` | statements stale ⇒ existing staleness posture, clocks unchanged |
| Auto-extend evidence | clean fresh report | auto-entry in reconcile journal carrying report id | `ibkr recon`, `policy show` | any exception or staleness ⇒ no extend, clock runs, push |

## Safety invariants

- No enforcement change anywhere in this phase: the block stays shadow, no
  submit path, blocker, pin, token, or freeze semantics are touched.
- All policy write verbs, recon resolutions, and the reset stay
  human-origin-only; origin gating is **extended** to attestation stamps.
- Auto-extend requires positive fresh evidence; data absence still degrades
  loudly (never-false-pass). A dead fetcher cannot extend anything.
- Push payloads and brief pushes never carry balances, account ids, order
  references, or tokens.
- The Flex token rules are unchanged; the backfill uses the same token and
  stays unreadable to agents. Statement content remains untrusted typed data;
  unknown lines become exceptions, never actions.
- Briefs and nudges are advisory; a dead relay or missed nudge never blocks
  anything and is journaled.

## Sequencing

1. **Operator (blocking):** adjust the Flex query period at IBKR (or second
   query id) for the backfill.
2. **Backtest:** backfill ingest + backtest report; operator reviews the flow
   list against memory and the equity replay against the recorded crossings;
   then makes the reset decision on broker-true numbers.
3. **v3 revision (operator-written TOML)** → authority flip (R3/R4),
   auto-extend live, cadence re-declared. In parallel: **ergonomics build**
   (brief + render-stamps, pushes, one-tap sign-off, earnings fallback,
   same-day metric, Phase-2 day counter) — briefs do not depend on v3.
4. **R5 cleanup:** remove the dual-compute display and attestation-era prose;
   amend post-trade-truth.md R3/R4 rows and the risk-policy.md deferred list.

## Verification (at implementation)

Backfill fixtures (long-window anonymized XML incl. a real-shaped withdrawal,
restatement, unknown line); equity-replay test against known crossing dates;
auto-extend refusal cases (exception present, stale statements, no report);
stamp-origin tests (agent render produces no journal entry); push-payload
redaction test (no money fields in any marshaled payload); `make check` +
`make test` binding; daemon/CLI wire changes take full `make smoke`; redacted
before/after artifacts per docs/templates/daemon-cli-trading-contract.md.

## Rollback

Each phase reverts independently. Auto-extend and the authority flip retire by
policy revision (the declared ledger stays intact through the dual-compute
window, so flipping back is a revert, not a reconstruction). Briefs, pushes,
and stamps remove without any trading-path change.

## Out of scope

3b measurement content (still gated on Phase 2 data), promotion of any control
to hard, MCP exposure of `policy`/`recon` (queued for after Phase 2), relay
deployment changes (separate go/no-go), and every threshold value — numbers
exist only in the operator's TOML.

## Implementation record — risk-policy v3 (2026-07-18)

Shipped and active 2026-07-18 evening, all gates green
(worktree race tests, primary `make test`, daemon restart, full `make smoke`).

- **Scope chosen by the operator (interview, same day):** v3 = clean-report
  auto-extend + R3/R4 only. Cadence re-declaration and nudge-time keys are
  deferred to a later revision — they depend on the L1 brief surface, and
  dead keys violate the no-speculative-knobs precedent.
- **Auto-extend (strict form, operator choice):** requires policy status
  active at v3+, a report with status active and statement health ok, zero
  unresolved exceptions, statement freshness within
  `recon.max_report_age_days`, and a same-day equity pair no older than the
  same window with |divergence| ≤ `recon.max_equity_divergence_pct`.
  Unmeasurable divergence refuses (positive-evidence rule). At most one
  automatic event per report id; events are ordinary `reconcile` journal
  entries with origin `daemon-auto` carrying the report id. Evaluation runs
  only at daemon startup and after a successful Flex ingest — never from any
  RPC read.
- **R3/R4:** statement-confirmed post-genesis flows plus not-yet-covered
  declared bridge entries are the authoritative cumFlows input under v3;
  matched declarations are superseded (statement value wins), within-coverage
  unmatched declarations stay `ledger_only` exceptions, and a statement flow
  with no declaration is the new non-exception category `confirmed` (listed,
  amount-disclosed, report-id-pinned, never signature-gated; under v2 it
  remains the `missing_from_ledger` exception). Peak correction keys off
  statement value dates, exactly once per line id, persisted across
  restarts; v3 declarations never correct the peak. Activation is a baseline:
  flipping versions recomputes nothing retroactively (verified live:
  fingerprint changed, peak/tier/latch byte-stable).
- **Dual-compute disclosure** (declared vs statement cumFlows plus
  `flow_source`) is live in `policy show`/`recon` and on the wire; removal is
  R5, not before a few clean weeks.
- **Defect fixed in passing:** the documented outage valve — a one-shot
  override on `capital.max_unreconciled_days` — was recorded, journaled, and
  displayed but never consumed by evaluation. It now extends the
  unreconciled clock; only that one control reaches evaluation.
- **Fingerprint stability:** the pre-v3 fingerprint projection is preserved
  byte-for-byte (regression-tested), so existing v2 files do not drift under
  the new binary. The v2 report-id projection is likewise unchanged; v3
  report ids pin full row content (a restated confirmed flow cannot reuse an
  id).
- **Process note (one-shot, dated):** the v3 numbers are the operator's
  (divergence bound 1.0% chosen in-session 2026-07-18); the mechanical file
  save was delegated to the agent by explicit one-shot operator instruction
  the same day, recorded in the session transcript and the file header. The
  standing operator-authored rule is unchanged for all future revisions, and
  all daemon-side human-origin gates on policy write verbs remain binding.
- **Known edge (accepted, documented):** if a system with v2-era declared
  deposits flipped to v3 *before ever ingesting a statement*, statements
  arriving later could re-apply a peak correction the v2 heuristic already
  made (conservative direction: peak too low). Unreachable on this
  installation — statements predate the flip, so activation baselined the
  full history.
- **Candidate L2 push trigger (recorded for the ergonomics build):** "new
  `confirmed` statement flow." Post-flip flows are deliberately not
  exceptions; a push should still announce money movement so an unexpected
  disbursement is seen before the monthly pulse.
- **Flex query window (operator decision, 2026-07-18):** reverted from the
  365-day backfill not to daily but to a **14-calendar-day rolling window** —
  deliberately wider so a daemon outage of up to two weeks loses no history
  and late restatements are re-read. The retained backfill file keeps the
  full-year coverage in the merged report either way; the next fetch
  confirms the new window.
- **Expected first automatic extension:** the first fetch that brings a
  same-day equity pair (statement day paired with a runtime daily sample —
  ~2026-07-19), comfortably before the 2026-07-25 clock expiry.

## Implementation record — L1 brief surface (2026-07-18)

Both slices shipped 2026-07-18 evening.
Daemon/RPC/CLI: commit 73bc12d — worktree `make check` + race tests, primary
`make test`, `make restart-daemon`, full `make smoke` PASS with zero skips.
PWA card: integrated the same evening — worktree `make app-check` +
`make check` + unfiltered app/relay tests, primary `make test`, rendered QA
against a fully isolated throwaway stack (own socket/state/HOME/policy
fixture), then `make app-refresh` on the real host.

- **Content scope grew by operator decision (interview, same day, two
  rounds):** the approved six-row list became a five-section desk-style
  brief — A Market (regime, breadth, dealer gamma, canary), B Calendar
  (session, held-name event flags), C Portfolio (equity + daily P&L, top-3
  movers, premium-at-risk, best-effort hedge cost, working orders), D Risk &
  limits (tier, latch + latch age, active overrides, sibling-pin drift),
  E Process (reconcile clock, auto-extend, one-tap, rules deltas, artefacts
  due) — realizing the machine-compiled part of the 2026-07-09 morning-page
  template as one combined morning+EOD brief. Pattern tags, process grade,
  and pair correlations stay out (human inputs / deferred P3).
- **Phase-2 day counter retired before it existed (operator decision,
  clean-slate):** no phase metadata is stored anywhere — no TOML key, no
  journal event. The observation orientation is the derived latch-age line
  from `latched_at`, shown while latched. Decision 1's row list is amended
  accordingly.
- **Attestation-by-render mechanics:** `brief.snapshot` is side-effect-free
  for every origin (regression-tested by walking the XDG state tree across
  repeated compositions — this forced read-only seams: account summary
  without the capital observation, breadth without the refresh trigger,
  rules evaluation without earnings kicks or transition journaling, regime
  from a snapshot cache, overrides without expiry maintenance). `brief.ack`
  is the stamp: human-origin-only through the same gate as all policy
  writes (agent/empty origin refused with nothing journaled), idempotent
  per artefact kind per daemon-local day, recording `origin` and
  `brief_fingerprint` as new omitempty fields on `ArtefactRecord` and the
  `artefact_completed` journal entry (legacy entries and the manual
  `ibkr policy artefact` verb unchanged).
- **Stamp kind = first-incomplete rule (operator choice):** morning, then
  eod, then nothing (disclosed); `--kind` overrides; weekly is never
  render-stamped until the v4 cadence re-declaration. Accepted quirk: two
  early-day renders can stamp both kinds before close, honestly visible in
  journal timestamps.
- **One-tap reconcile (decision 3):** `policy.capital_event` type
  `reconcile` with an empty report id resolves the current report
  daemon-side and passes the *unchanged* gate. Signability lives once in
  `reconcileReportAssessment`: the write gate returns its first blocker,
  the brief's one-tap row exposes the ordered list. Explicit `--report`
  behavior is untouched.
- **[Rulebook state deltas](trading-rulebook.md)** diff the current Rulebook
  snapshot against the row set
  persisted at the last *stamped* brief (a runtime-owned daemon.db state
  document written only by `brief.ack`); first run discloses "no delta
  baseline yet". This is a status-snapshot comparison, not post-trade adherence
  or broker evidence: marks, Regime, earnings, and source health can move it
  without a trade. The shared unreconciled-clock arithmetic moved to
  `risk.EvaluateUnreconciledClock` (override can only extend; zero
  last-reconcile fabricates no deadline) and feeds both evaluation and the
  brief's typed `deadline`/`days_remaining`.
- **PWA card (rendered surface):** first card on the monitor tab (operator
  placement choice), rendered verbatim from the snapshot's typed brief —
  the SPA computes no signability, deltas, or risk numbers. The
  render-stamp fires via `POST /api/brief/seen` only when authenticated, on
  the monitor tab, card rendered, and `document.visibilityState` visible —
  once per brief fingerprint per page session; the app server-assigns
  origin `human-paired-device` and the poller never acks. One-tap is
  `POST /api/recon/signoff` carrying the pinned report id the operator saw
  (no dialog — the labeled tap is the confirm; daemon refusals surface
  verbatim). Both routes are `requireAuth`-gated, relay-forwardable, and
  deliberately outside the broker-write confirmation wrapper (governance
  write, not a broker write). A static contract test pins the card
  placement, the section renderers, and forbids `confirm_account`/
  `window.confirm` in the module.
- **Live verification (2026-07-18, Saturday):** agent-origin CLI render
  left the real artefact journal byte-identical; gateway-down rows
  disclosed per-row while risk/process rows rendered (latch age `3
  day(s)`, reconcile `due 2026-07-25 (7 day(s))`, one-tap signable with
  the pinned report id). Rendered QA ran in the isolated stack: the SPA
  correctly refused to stamp while the page reported itself hidden (the
  preview pane never becomes visible — agent browsing structurally cannot
  stamp), and with visibility simulated in isolation the full chain
  produced `artefact_completed · morning · origin human-paired-device ·
  brief fingerprint` in the throwaway journal plus the on-card receipt.
  The real journal gained no entries at any point; the operator's first
  real phone render will be the first honest stamp.
- **Known cosmetic debt (deferred deliberately):** empty regime
  stage/verdict renders a stray `·` in the CLI; the one-tap blocker for an
  unbuildable daemon-resolved report still reads as the bare-attestation
  refusal; the card's artefact rows print raw `declared true/completed
  false` booleans. Queued for the next tidy round, none behavioral.

*Amended same evening — pre-release polish (operator interview after live
use):* the first real phone render at 19:33 stamped morning and, on the
next poll's fingerprint, eod 39 seconds later — the accepted first-
incomplete quirk observed in practice. Operator decision: **the stamp rule
is tightened to one stamp per look** — a successful render-stamp disarms
further stamping until the app has been backgrounded and foregrounded
again (client-side look counter with race protection; daemon `brief.ack`
unchanged). Shipped in the same polish batch: one-decimal percentages and
local ISO date/short-time formatting on the card (no locale-dependent
timestamps), human artefact labels, the artefact group header loses its
placeholder value, held-name event rows collapse to one disclosed row when
the positions source is down, the rules-delta fingerprint note renders
only on change, the CLI renders `—` for empty joined values, the
unbuildable-resolved-report blocker is reworded ("current reconcile report
is unavailable to sign off"), quote tiles show `Closed` instead of `Feed
issue` when the calendar affirms the session is closed, and the snapshot
banner claims "showing last good snapshot" only when retained data is
actually displayed (cold portfolio failure reads "Account and positions
unavailable."). The cosmetic-debt list above is thereby cleared. Gates:
worktree `make app-check`+`make check`+race tests, primary `make test`,
`make app-refresh`, full `make smoke` PASS zero skips; rendered
verification on both an isolated paper-connected stack and the
real-daemon preview.

## Implementation approval — L2 nudges and monthly pulse (2026-07-18)

The operator approved the implementation contract in
[`risk-governance-nudges.md`](risk-governance-nudges.md). Code may land behind
the active v3 policy; the live policy stays v3 until the automatic same-day
clean-report extension is observed.

The approved v4 cadence is `Europe/Berlin`, monthly day 1 at 09:00, with the
already-approved two-day rolling reconcile warning horizon. Monthly completion
means an authenticated paired-device foreground render with readable matching
sibling pins; it is adherence evidence, not proof of human attention. Push is
once per stable occurrence per active subscription/device; `none` disables all
push; resolved delivery detail retains 90 days while active, unseen, and
retry-suppressing records never evict. The first qualifying shadow would-block
preview in a latch episode notifies and later previews increment internal
episode state without widening the notification payload. Confirmed-flow
activation records a broker-backed coverage watermark,
requires one cutover review, never expires unseen events, and revalidates them
against current statement truth. Source failures stay persistently visible but
do not emit a separate push in the first implementation. A policy fingerprint
change reopens that month's pulse.

## Brief re-conception — two process movements (2026-07-20)

Updated: 2026-07-20 15:13 CEST. Operator decision, final: the daily brief is
re-conceived from five data-domain sections (A Market, B Calendar, C Portfolio,
D Risk & limits, E Process) into two **process movements** in one morning
edition. The daemon still composes the whole brief and every surface renders it
verbatim — that split is unchanged.

- **Review** — post-trade of the last completed session, statement-authoritative
  where the overnight Flex statement is available. Rows: Session P&L (equity +
  daily P&L headline); attribution by underlying (the existing movers basis plus
  the disclosed residual); process coherence (the Rulebook state delta with
  act transitions, proposals offered-vs-acted from the trade-proposal-outcomes
  journal, and overrides used); capital events (latch engagement and
  adjusted-peak provenance); reconcile / auto-extend / one-tap sign-off (the tap
  closes the movement); working-orders end state.
- **Ready** — pre-trade for today. Rows: overnight & market (regime posture +
  canary verdict + breadth/gamma tape with their existing provenance stamps);
  calendar (session phase/next open, held-name earnings with the rule-unknown
  cross-link, event flags); risk capacity (capital tier, drawdown latch, premium
  at risk, hedge carry); desk readiness (policy-pin drift, cadence artefacts, the
  monthly pulse).

This is a **regrouping** of facts the daemon already had: row severities, the
attention semantics, and the worst-child section rollup are unchanged in kind.
The five domain composers remain as internal intermediates; the two movements
reassemble their rows. The one new derivation is **proposals offered vs acted**,
read read-only from typed daemon.db proposal-outcome events (only the counts and
the covered day reach the wire — no proposal keys, symbols, order refs, or
tokens).
VaR and any risk-unit measure are explicitly out of scope (reserved to the
operator). The one-tap reconcile sign-off keeps its exact endpoint, evidence
class, and semantics. Where a named facet is not a fact the brief already held,
the row renders honestly unavailable rather than inventing a value.

The wire contract changes shape accordingly: `BriefResult` now carries `review`
and `ready` movements (the `market`/`calendar`/`portfolio`/`risk_limits`/
`process` top-level sections are retired), the content fingerprint hashes the
two movements, and the monthly-pulse row moves under `ready`. The brief also
moves to its own bottom-tab slot ("Brief", slot 2 of Monitor · Brief · Alerts ·
Orders · Settings); Monitor stays the default landing and the universal
fallback. Phase heads render a sunrise (Ready) or history-clock (Review) glyph
next to a sentence-case title. The `brief` SSE event name is unchanged.
