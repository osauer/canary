# Regime calibration: confirmation eligibility, staleness honesty, and severity governance

**Status:** Implemented; persistence contract updated for `daemon.db`
**Created:** 2026-06-12 13:09 CEST
**Last update:** 2026-07-21
**Owner:** osauer
**Related:** `docs/docs/internals/regime-dashboard.md`, `docs/docs/internals/regime-backtest.md`,
`internal/rpc/lifecycle.go`, `internal/rpc/regime_policy.go`, `internal/daemon/regime*.go`,
`internal/stress/stress.go`, `internal/cli/backtest.go`, `internal-docs/design/platform-settings.md`,
`.agents/docs/daemon-cli-trading-contract.md`, `.agents/docs/spa-authority-matrix.md`

## Why this exists — the 2026-06-12 false positive

Pre-open on 2026-06-12 the engine headlined **"Broad Stress Regime /
confirmed_stress / severity act"** while every live tape input was green: SPY
+0.3% near its highs, VIX 18.84 vs VIX3M 21.42 in normal contango. The
maintainer judged the market calm; the engine's own tape evidence agreed.

The escalation was mechanically correct under current policy and wrong in
substance. Every citation below names the pre-fix tree as it stood on
2026-06-12; Part 5 records where each piece went. Two moves since then matter
most for navigation: the duplicated band and rescue logic collapsed onto
`internal/rpc/regime_policy.go` (`BuildRegimeClusterBands`), and the stress
engine moved out of `internal/cli` into `internal/stress`.

The exact chain, verified in code:

1. **A 7 bps credit break counted as a full red.** HYG 79.95 sat 0.07% below
   its 50DMA (80.008) with SPY above the 97%-of-52-week-high line.
   `bandForHYGSPY` (`internal/daemon/regime_composite.go:509`) has no minimum
   depth: one tick below the DMA is the same red as a 3% break. The red streak
   was 1 session old, on a thin pre-open tick — and the indicator's own notes
   say "single-day moves are noise."
2. **A prior-evening gamma cache counted as a contemporaneous red.** The gamma
   compute ran 22:19 ET the prior evening; outside RTH the cache never
   refreshes (`softTTLClosed = math.MaxInt64`,
   `internal/daemon/gamma_zero_cache.go:259`). The regime row has no `stale`
   status path for gamma (only ok/computing/unavailable/error,
   `internal/daemon/regime.go:938-974`), and the lifecycle vote checks only
   rankability, never age (`rankableLifecycleGammaBand`,
   `internal/rpc/lifecycle.go:458`). Confirmed red evidence is stamped
   `timing: contemporaneous` unconditionally (`lifecycle.go:477-479`). Note
   the prior-evening profile still contained the day's *expired* 0DTE
   exposure — it was not merely old, it was partly about options that no
   longer existed.
3. **The two marginal reds rescued each other.** Credit's isolated-red
   downgrade is waived whenever any *raw* red exists elsewhere
   (`lifecycle.go:446`, `regime_composite.go:615`), and gamma has no
   isolated-red downgrade rule at all. So stale gamma red rescued the 7 bps
   credit red; `ClusterRedCount = 2` → `confirmed_stress` → `severity act`
   (`lifecycle.go:355-360`, `123-126`).
4. **Degraded readiness capped confidence, not severity.** The readiness check
   runs *after* stage/severity assignment and only calls
   `capLifecycleConfidence` (`lifecycle.go:152-155`). The stress tone
   short-circuits before the degraded-readiness check
   (`lifecycle.go:218-227`). So the snapshot simultaneously said
   `readiness: degraded`, `confidence: medium` — and `severity: act` with a
   red headline.
5. **Headline wording is computed in four places and two of them disagree at
   exactly this tally.** `verdictFor` (`regime_composite.go:478`) requires 3+
   reds for "Broad stress regime"; `regimePostureLabel` (`lifecycle.go:197`)
   says it at 2+. The other two copies live CLI-side:
   `(regimeComposite).verdict()` (`internal/cli/regime.go:824`, what the CLI
   actually renders) and `backtestRegimeVerdict`
   (`internal/cli/backtest.go:1131`). The SPA and MCP posture showed "Broad
   stress regime" while `composite.verdict` said "Stress signal present" for
   the same snapshot.
6. **The stress surface independently reproduces the same false positive.**
   `summarizeCanaryMarket` (`internal/cli/canary.go:539-611`) recomputes
   cluster bands through a *third copy* of the band/rescue logic
   (`rawRegimeClusterBands` / `confirmedRegimeClusterBands` /
   `hasIndependentRegimeRedCluster`, `internal/cli/backtest.go:1076-1116`)
   and `canaryConfirmedMarketStress` (`canary.go:1063`) confirms on raw
   `RedClusters >= 2`. The stress read feeds the SPA regime panel
   (`renderRegimePanel` reads the stress snapshot key, `web/app/app.js:3149-3164`)
   and the alert pipeline (`BuildCanaryFingerprint`,
   `internal/rpc/fingerprint.go:106`, deduped by
   `internal/app/alerts/alerts.go:32-44`) — the surfaces the user actually
   watches. Fixing the daemon lifecycle alone would not have fixed the
   incident as experienced.
7. **Every threshold involved is flagged `heuristic` + `pending_backtest`**
   (`heuristicThresholds`, `internal/daemon/regime_metadata.go:78-87`) — and
   no policy code reads either flag. The provenance disclosure is decorative.
8. **Streaks are persisted but purely presentational.** The streak store
   (`internal/daemon/regime_streaks.go`) tracks per-indicator band streaks
   across restarts, but lifecycle confirmation never consults them.

Each of these is an independent defect; together they let two pieces of weak
evidence escalate to the strongest non-panic posture against a green tape.

## Design principles

- **Confirmation must be deep, persistent, fresh, and independent.** Marginal
  but current evidence may warn; mutually-dependent evidence may not confirm.
  Missing, broken, contradictory, or cadence-overdue evidence does not describe
  the market at all: it puts the snapshot in `data_quality` with the headline
  **“Market state undefined — data incomplete.”**
- **Display and policy separate — with one precise caveat.** Row bands keep
  their current semantics (a red row stays visible the moment a threshold
  crosses). What changes is which reds may *escalate*: the cluster-level
  rollup and lifecycle. Note this means a cluster can render yellow while its
  row renders red (the incident's credit cluster, post-fix) — that is
  intended and must be explained in row `band_reason` / cluster notes, not
  smoothed over.
- **Data quality is a separate state, not a weak market signal.** A required
  source defect yields `data_quality / watch / blocked / low`; it is never
  relabelled `early_warning`. Independently current confirmed stress or panic
  may survive an unrelated source defect, but readiness becomes degraded and
  every confirming witness must prove current on both its row and source-health
  contracts.
- **One copy of policy.** Band classification, rescue/confirmation, and
  headline wording each exist in one shared place; daemon, CLI, stress, and
  backtest consume served values or the shared functions. (The third-reader
  threshold promised in `regime_composite.go:27-30` has been crossed.)
- **Raw measurements stay on the wire; banding stays disclosed.** New policy
  outputs are additive fields.
- **Every governor action is observable.** When policy downgrades or caps
  something, the payload says what was capped, from what, and why.
- **Calibration is forward-data-driven.** Persist typed decision events in
  `daemon.db`, backtest against them later, and only then promote
  thresholds out of `pending_backtest`. No threshold value in this document is
  claimed to be calibrated; they are defensible noise floors. The fast-path
  depths are the most invented numbers here and are flagged as such.

## Part 1 — Confirmation eligibility (the core mechanism)

Introduce one concept that all four task questions hang off: a red cluster
band is either **eligible** (may confirm stress) or **provisional** (may not
confirm). A provisional red caused only by depth or persistence may warn when
the required input set is otherwise usable. `data_overdue`, missing health,
or an unavailable producer is instead a data-quality defect.

A red is *eligible* when all three hold:

1. **Depth** — the measurement clears the threshold by at least the
   indicator's minimum depth (noise floor), OR clears a deeper "fast-path"
   level that overrides persistence (so genuine day-one crashes still
   escalate same-day).
2. **Persistence** — the indicator's red streak (persisted as current state in
   `daemon.db`) has reached the indicator's minimum sessions, OR
   the fast-path depth is met. A missing streak entry (fresh install, deleted
   store) counts as sessions = 1.
3. **Freshness** — the data is *cadence-fresh*: no newer observation should
   exist under the indicator's native cadence (Part 3). Overdue data can
   never confirm.

**Eligibility latches for the life of the red streak.** Once a red becomes
eligible, it stays eligible until the band exits red (per exit hysteresis),
even if the measurement wobbles back inside the minimum depth. This prevents
eligible/provisional churn at the depth boundary without a second hysteresis
band. (Freshness is not latched — if the feed goes stale mid-streak,
eligibility drops with it; see the severity governor.)

Provisional reds remain fully visible: the row renders red, the evidence list
carries them with `confirmed: false`, and they appear in `unconfirmed`.
Depth- or persistence-provisional reds may trigger `early_warning` only while
all required inputs are usable. A broken or overdue input keeps the provisional
measurement visible for diagnosis but changes the top-level state to
`data_quality`; a typed `not_due` schedule remains context, not a source
failure. Provisional reds no longer:

- count toward the red tally used by `confirmed_stress` / `panic`,
- rescue another cluster from its isolated-red downgrade
  (`hasIndependentLifecycleRed` / `hasIndependentRedCluster` count *eligible*
  reds only — this kills marginal mutual confirmation),
- appear in `confirmed_by`,
- carry `timing: contemporaneous` or `severity: act` in evidence rows.

### Per-indicator calibration table

Band boundaries (green/yellow/red) are unchanged in this design — they are
the `pending_backtest` quantities the forward data will calibrate. This table
adds the eligibility gates and band-exit hysteresis. "Streak" means
consecutive NY *trading* sessions in red (see the weekend fix below); a
session held red only by exit hysteresis still counts toward the streak —
persistence measures how long the market has been in the state, and the state
includes hysteresis.

| Indicator | Red band (unchanged) | Min depth for eligible red | Fast path (eligible day 1) | Min streak | Cadence-freshness for eligibility | Exit hysteresis (band re-arm) |
|---|---|---|---|---|---|---|
| VIX term (VIX/VIX3M) | ratio ≥ 1.00 | ratio ≥ 1.00 (inversion is already discrete) | ratio ≥ 1.05 | 2 | both legs live during their shared 09:31–16:15 ET window for confirmation; VIX3M prior-close carry is typed `not_due` outside that window only when VIX satisfies its own 03:15–09:25 and 09:31–16:15 ET schedule | leave red only when ratio < 0.98; re-enter at ≥ 1.00 |
| VVIX | ≥ 110 | ≥ 110 | ≥ 120 (existing isolated rule, kept) | 2 | latest official daily close (newest possible observation) | leave red < 105 |
| HYG/SPY credit proxy | HYG < 50DMA and SPY ≥ 97% of 52w high | HYG ≥ 0.25% below 50DMA | HYG ≥ 1.0% below 50DMA | 2 | RTH tick, or latest official close outside RTH (never a thin pre/post-market tick) | leave red only after HYG closes back above 50DMA |
| Credit spreads (HY OAS) | ≥ 5.5 or 20-obs widening ≥ 1.00 pp | levels are already deep — no extra depth | n/a (official daily series) | 1 | series ≤ 7d old (as today) | leave red < 5.25 and widening < 0.85 pp |
| Funding (CP−T-bill) | ≥ 75 bp | ≥ 75 bp | n/a | 1 | series ≤ 7d old (as today) | leave red < 65 bp |
| USD/JPY | yen +≥2%/week | ≥ 2% (speed *is* the depth; Aug-2024-calibrated) | ≥ 2% in 3 days (existing spec prose, now enforced) | 1 | same-day tick or same-day close | leave red < 1.5% |
| Dealer gamma | spot below γ-zero / wholly short profile | see gamma paths below | see gamma paths below | 1 | current-options-session compute for confirmation; latest completed-session result is typed `not_due` context before the next options session | leave red when gap > +0.5% |
| SPX breadth | < 40% above 50DMA with SPX near highs | ≤ 38% | ≤ 30% | 2 | last completed session's compute (the newest possible observation) | leave red > 45% |

**Gamma eligibility, all three red paths** (red has three producers —
`classifyGammaBand` gap path, no-crossing wholly-short profile
(`regime_streaks.go:390-398`), and the combined-scope weighted vote
(`combineGammaComputedBands`, `regime_streaks.go:415-458`)):

- (a) gap path: min depth = gap ≤ −0.5% below γ-zero (within ±0.5% is
  transition noise); fast path gap ≤ −2.0%.
- (b) wholly-short profile with no crossing: an extreme state — treated as
  fast-path (eligible day 1, depth gate vacuous).
- (c) combined-scope vote: eligibility evaluated on the per-index weighted
  gap that produced the vote, same −0.5%/−2.0% levels.

Rationale highlights:

- **HYG depth 0.25%** ≈ the noise floor of HYG's typical daily range; the
  incident's 0.07% fails it. The 1.0% fast path keeps a genuine credit gap
  eligible on day one. Depth is measured against the DMA:
  `(dma − price) / dma`.
- **Streak = 2 for vol/credit-proxy/breadth** enforces what the indicator
  notes already claim in prose ("sustained inversion over 2-3 sessions",
  "single-day moves are noise") but never enforced. Fast-moving indicators
  (USD/JPY, gamma, funding, cash OAS) keep streak 1 because their red bands
  are either speed-defined or already deep.
- **Exit hysteresis** prevents band flapping at the boundary (red → green →
  red consuming the streak reset each time). It is applied **once, in the
  daemon fetch/annotate path**, so the served `Band` is post-hysteresis;
  every downstream reader (CLI, stress, SPA, MCP) consumes served bands and
  needs no store access. The previous band comes from the daemon-owned streak
  state (`StreakStore.Get`), which is backed by `daemon.db` where banding
  happens.
- **Panic is untouched.** SPY −4%/−7% tape triggers stay as they are; tape is
  never depth/streak-gated. Real crashes escalate immediately through the
  panic path and through fast-path depths regardless of streaks. (The
  2026-07-19 closed-date pass adds a session gate, not a depth/streak gate:
  frozen closed-date prints cannot fire the tape branches, live trading-date
  prints keep immediate escalation — see Part 2.)
- A missing current streak document starts counters from session 1 and demotes
  an ongoing confirmation to provisional until fast-path depth or fresh
  sessions re-establish it. Cutover seals legacy `regime-streaks.json` without
  importing it into current state; only allowlisted historical market/gamma
  measurements become immutable `decision_eligible=false` observations.

### Trading-day streak fix

`StreakStore.Tick` increments on any new NY *calendar* date
(`regime_streaks.go:181`), so a Saturday poll inflates streaks. Sessions must
be derived from NY trading days — `spx.CompletedSessionKey`
(`internal/breadth/spx/scheduler.go:118`) is the holiday-aware precedent.
Small correctness fix, bundled because eligibility makes streaks load-bearing.

## Part 2 — Lifecycle and severity policy

### Stage triggers (revised)

`BuildRegimeLifecycle` (`internal/rpc/lifecycle.go:89`) keeps its shape; the
red tallies change meaning. All existing branches are kept — including the
`SPY ≤ −4% && yellow ≥ 2` branch (`lifecycle.go:358`) the tape can satisfy
without any red:

- `panic` — `eligibleRed ≥ 3 || (SPY ≤ −4% && eligibleRed ≥ 1) || SPY ≤ −7%`.
- `confirmed_stress` — `eligibleRed ≥ 2 || (eligibleRed ≥ 1 && SPY ≤ −2.5%)
  || (SPY ≤ −4% && yellow ≥ 2) || (eligibleRed ≥ 1 && VIX +20%)`.
- `early_warning` — the home of current but not-yet-confirmed stress:
  `rawRed ≥ 1 || yellow ≥ 3 || provisional present || SPY ≤ −1.5% || VIX +10%`,
  but only when the required input set is usable.
- `opportunity` / `stabilization` / `quiet` — unchanged.

After stage selection, the required-input gate runs fail-closed. A blank or
non-OK status, explicit `freshness: overdue`, missing source-health row,
unknown refresh state, or source age beyond its served maximum changes the
result to `data_quality / watch / blocked / low`. The only schedule exceptions
are exact typed `not_due` contracts for VIX3M (with current VVIX and a VIX leg
that satisfies its own schedule) and gamma. There is no generic “unranked stale
is fine” exemption. Confirmed stress or panic survives an unrelated defect only
when every witness that independently establishes that stage is current; stale
SPY or VIX tape cannot preserve it.

**Closed-date tape gating (added 2026-07-19).** Every tape term above reads
the direct SPY/VIX day-change prints, which freeze at last-session values on
official non-trading dates and can even reset independently while closed (the
2026-07-18/19 weekend evidence held `early_warning`/watch on a half-reset
Saturday print: SPY change collapsed to 0.00 while VIX kept Friday's +12%).
The stage arms, the governor's SPY/VIX co-sign arms, and the pure-tape panic
exemption therefore require a confirmable session — `tape_session_state !=
closed_date`, classified by `rpc.TapeSessionFor` (marketcal US-equity date
state; the shared copy the stress tape row also keys on), stamped by the
daemon at snapshot time and by the backtest replay from the observation
clock, empty-and-fail-open outside embedded calendar coverage. Cluster-driven
terms and the status-gated term-inversion co-sign are untouched. The isolated
equity-vol corroboration inside cluster combination deliberately stays
session-blind (decision 2026-07-19: bounded residual — it can only preserve a
red that live-session banding produced, and its worst weekend effect is
cluster-grade early warning; revisit with decision-event evidence). The rulebook's
regime-stage latch skips closed-date snapshots so the last trading-date stage
ages into the carried worse-of(carried, calm) path instead of a weekend stage
re-latching fresh.

For the 2026-06-12 regression fixture as recorded — HYG red provisional,
gamma cadence-overdue, and volatility explicitly stale without a valid
`not_due` contract — this now yields `data_quality / watch / blocked / low`
and **“Market state undefined — data incomplete.”** The red measurements stay
visible in `unconfirmed`, but they do not define a market warning. If the same
marginal HYG break is later evaluated with a complete, current required-input
set, `early_warning` is appropriate: that warning then describes market
evidence rather than an ingestion defect.

### Severity governor (ordered, applied after stage selection)

1. **Provenance gate** (task Q4 — the explicit policy):

   > While a confirming cluster's threshold set carries
   > `pending_backtest: true`, its evidence is *heuristic evidence*. The
   > gate lifts when either a fresh tape co-signature is present in the same
   > snapshot (SPY day change ≤ −1.5%, or VIX day change ≥ +10%, or a
   > same-session VIX-term inversion at ratio ≥ 1.00 on a fresh tick) or
   > every confirming set has been promoted. Without a lift, heuristic
   > evidence is capped one severity rung down: stage `confirmed_stress`
   > reads **watch** instead of act, and the `eligibleRed ≥ 3` panic branch
   > reads **act** instead of urgent (three deep, fresh, persistent,
   > independent reds have earned a strong response — but never the maximum
   > on unvalidated thresholds alone). The pure-tape panic branches
   > (SPY ≤ −7%; SPY ≤ −4%, which is itself act-grade tape) carry their
   > co-signature by construction and always reach **urgent**.
   > A threshold set is promoted out of `pending_backtest` per versioned
   > label (e.g. `hyg_spy_credit_proxy_v1` → `_v2`) only through the backtest
   > plan, with documented precision/recall on the forward-collected
   > decision-event corpus (Part 4).

   The `PendingBacktest` flag on `RegimeThresholds` finally becomes
   load-bearing: the governor reads it from the snapshot's own metadata, so
   per-set promotion automatically relaxes the gate without policy edits.
   Note the tape-corroborated `confirmed_stress` branches (SPY ≤ −2.5%, VIX
   +20%, SPY ≤ −4%) satisfy the co-sign by construction — the gate bites only
   on the pure `eligibleRed ≥ 2` path, which is exactly the incident shape.

2. **Required-input gate, then evidence-keyed survival.** Any required source
   defect normally changes the state to `data_quality`. It may not overwrite
   an independently established `confirmed_stress` or `panic` whose complete
   witness set is current: for example, one unavailable funding feed does not
   erase fresh, deep, multi-cluster confirmation elsewhere. In that exception
   readiness is `degraded`; an impaired confirming cluster never qualifies as
   an independent witness. Pure tape also has to pass its typed SPY/VIX
   freshness and source-health checks.
3. **Disclosure.** Any cap or downgrade emits a governor record (new
   `lifecycle.governors[]`, Part 5): `{action: "severity_capped", from:
   "act", to: "watch", reason: "pending_backtest_no_tape_cosign", clusters:
   ["credit","gamma"]}`. Nothing is silently weakened.

Severity reachability after this design:

| Severity | Reachable when |
|---|---|
| observe | default / stabilization |
| watch | any warning state; the ceiling for heuristic-only `confirmed_stress` without co-sign, and for quality-impaired confirmation |
| act | stage `confirmed_stress` (any branch) with promotion or tape co-sign and quality-clean confirmers; OR heuristic-only `eligibleRed ≥ 3` panic without co-sign (governor record: "urgent withheld") |
| urgent | pure-tape panic (SPY ≤ −7%, or ≤ −4% with an eligible red — the tape is act-grade by construction); or `eligibleRed ≥ 3` panic with co-sign or full promotion |

The ladder is monotone in evidence: 2 heuristic eligible reds → watch, 3 →
act, tape co-sign or promotion lifts each one rung. "Act" is reachable with a
single eligible red **when the tape co-signs** (SPY ≤ −2.5% / VIX +20%
branches) — intended; the tape is the second witness. Stage and severity stay
separable throughout: a governed panic reads stage `panic`, severity `act`,
with the governor record explaining the withheld rung.

### Tone follows governed severity — stated explicitly

When the governor caps severity, the *stage* is not rewritten: two deep, fresh,
persistent eligible reds with no tape co-sign still produce stage
`confirmed_stress` and label "Confirmed stress regime". The display tone does
follow the governed severity: `severity: watch` renders tone `watch` (amber),
with a governor record explaining why act/red was withheld. This preserves
headroom for act-grade stress and full risk-off conditions while keeping the
evidence label honest. The recorded incident case is weaker still because its
required inputs were overdue: stage `data_quality`, tone `data_quality`, with
the provisional measurements retained only as diagnostic context.

### Timing honesty

Evidence rows derive `timing` from cadence-freshness: only cadence-fresh data
may be `contemporaneous`. An overdue measurement remains visible with
`confirmed: false`, but the top-level state is `data_quality`, not a forward
market warning. A 22:19-yesterday gamma read can never again be presented as
contemporaneous confirmation.

### Headline unification

One function, one wording table, in `internal/rpc`; all four current copies
(`regimePostureLabel`, daemon `verdictFor`, CLI `(regimeComposite).verdict`,
CLI `backtestRegimeVerdict`) collapse onto it. The CLI renders the *served*
`composite.verdict`; the backtest builder calls the shared function. First
match wins:

| Condition | Label | Tone |
|---|---|---|
| stage == data_quality (including too few or broken required inputs) | Market state undefined — data incomplete | data_quality |
| all ranked clusters eligible-red, none unranked | Full risk-off conditions | risk_off |
| eligible red ≥ 3 | Broad stress regime | stress |
| stage == confirmed_stress and severity == watch | Confirmed stress regime | watch |
| stage == confirmed_stress and severity >= act | Confirmed stress regime | stress |
| raw red ≥ 1 (provisional or single eligible without tape) | Stress signal present | watch |
| yellow ≥ 3 | Elevated stress watch | watch |
| otherwise | Normal regime | normal |

This fixes the red==2 drift (incident headline), makes "Broad" mean broad
again, and removes the label/stage mismatch when a tape-corroborated single
red confirms. The Tone column is the display contract after severity governors:
the label can confirm stress while the tone remains watch if policy withheld the
act rung. A pure-tape panic with few reds still renders red because the panic
stage reaches act/urgent severity.

### Deferred: stage dwell

The first draft proposed a persisted stage-dwell (hold `confirmed_stress` one
session before de-escalating). Review killed it for v1: no flap incident has
been observed, eligibility latching + exit hysteresis already dampen the
plausible flap sources, dwell-on-stale-evidence contradicts staleness honesty
(a confirming feed going stale would *hold* the stress headline on evidence
the snapshot itself reports stale), and the decision-event corpus is precisely
the instrument that will measure whether stage flap exists. Revisit with
event data; if added later it must re-verify the held stage's confirming
evidence is still eligible.

## Part 3 — Staleness model: cadence-relative freshness, served on the wire

### The rule

An observation is **cadence-fresh** when no newer observation *should* exist
under the indicator's native cadence; it is **overdue** otherwise. Overdue
data cannot make a red eligible. This replaces the first draft's
"prior-session data can never confirm" absolute, which was both too strong
(breadth's post-close compute and HYG's settled close are legitimately the
newest possible observations for the next session) and too weak (a
"post-close window" clause would have re-admitted the incident's
prior-evening gamma).

Per-indicator cadence policy:

| Indicator | Native cadence | Fresh means | Overdue example |
|---|---|---|---|
| VIX term | VIX approximately 03:15–09:25 and 09:31–16:15 ET; VIX3M approximately 09:31–16:15 ET on the official options calendar | both legs live during the shared RTH window for a fresh, confirmable term read; frozen VIX3M is exact `not_due` outside it only when VIX is live during its own window or frozen during the 09:25–09:31 pause/closed period | missing VIX3M, frozen VIX during its due window, impossible pre-open live VIX3M, or frozen VIX3M during its RTH window |
| VVIX | one official close per day | latest published daily close | close > 1 publication day behind |
| HYG/SPY | intraday RTH ticks; settled closes outside RTH | RTH tick during RTH; latest official close otherwise (thin pre/post-market ticks are *never* the banding input — this also removes the incident's thin-tick wobble) | missing yesterday's close |
| Credit OAS / funding | official daily series with publication lag | ≤ 7 days (unchanged) | > 7 days |
| USD/JPY | 24/5 FX ticks | same-day tick or same-day close | only a prior-day close |
| Dealer gamma | intraday-capable compute during official option RTH | current-session compute during RTH; latest completed-session result before the next open is exact `not_due` context and cannot confirm | no last-good, result older than the latest completed option session, or prior-session result after the current options session opens |
| Breadth | once per session, post-close | last completed session's compute — inherently the newest possible | compute older than the last completed session |

The volatility windows come from Cboe's current
[VIX methodology](https://cdn.cboe.com/resources/indices/Volatility_Index_Methodology_Cboe_Volatility_Index.pdf)
and [selected SPX term-index methodology](https://cdn.cboe.com/api/global/us_indices/governance/Volatility_Index_Methodology_Selected_SPX_Target_Expected_Volatility_Term_Indices.pdf),
with shortened sessions taken from the official options calendar rather than a
hardcoded weekday clock.

The gamma/breadth asymmetry is principled, not ad-hoc: breadth's inputs
(daily closes) cannot exist intraday, so its post-close compute *is* current;
gamma's inputs (live option chains) refresh intraday and roll at the open, so
the latest completed-session compute is valid only as pre-open/closed-session
`not_due` context and becomes overdue when the next options session opens.

Gamma additionally has an explicit context path. Before the next options
session, the latest completed-session result is served as `status: stale`,
`freshness: not_due`: its band stays visible but cannot confirm. Once the
options session opens it becomes overdue unless replaced. `no_last_good` and
`missed_session` are defects, not quiet periods and not market warnings.

### Served policy, no hardcoded twins, no churn

Following the protection-panel pattern (`renderProtectionTimestamp` /
`goDurationMinutes` deriving from served `proposal_cadence`):

- `SourceHealth.MaxAgeSeconds` — exists in the contract, never populated for
  regime (`BuildRegimeSourceHealth`, `lifecycle.go:300-312`) — gets filled
  from the cadence policy, per cluster. The SPA's hardcoded
  `staleMinutes: 60` in `renderRegimePanel` (`web/app/app.js:3159-3161`) is
  replaced by a value derived from served max ages.
- Per-row, `RegimeIndicatorMeta` gains `freshness {class, max_age_seconds}`
  where class ∈ {fresh, overdue}. **No wall-clock `age_seconds` on served
  rows**: the app's change detection JSON-hashes the monitor result
  (`internal/app/live/service.go:863-876`), and a ticking age field would
  emit an SSE "regime" event every poll. Clients derive age from the existing
  `as_of` — the same reason `compactSourceHealth` already strips
  `AgeSeconds` (`internal/rpc/compact.go`).
- No separate top-level `policy` block (first draft had one; review cut it):
  per-row gates land on `eligibility`/`freshness` meta, max ages on
  `SourceHealth`, and the provenance gate discloses itself through
  `governors[]` when it bites.

### Fingerprint policy

Fingerprints feed alert dedupe, so new fields need an explicit stance:

- `Evidence.Confirmed` is already projected into `lifecycleFingerprint`
  (`lifecycle.go:320-347`), so eligibility flips re-key the fingerprint —
  intended: eligible→provisional is a semantically different state and
  *should* re-alert.
- `governors[]` enter the projection as `{action, reason}` only — never ages,
  depths, or other continuous values.
- `freshness.class` (binary) may be projected; `max_age_seconds` and all raw
  measurements stay out.
- The projection version bumps `lifecycle-fp-v1` → `lifecycle-fp-v2`. One-time
  alert re-fire on upgrade: accepted and called out in the changelog.

## Part 4 — Persistence and the calibration data gap (Q2)

### What exists today

- Post-cutover `daemon.db` current documents: active band/streak state and
  current regime/gamma/breadth material needed by runtime decisions.
- `daemon.db` immutable observations: HMDS/official series, breadth windows,
  gamma results/OI/expiry grids, and skew diagnostics with source,
  method/version, as-of, quality, and original payload.
- `daemon.db` append-only decision events: the forward regime and stress
  calibration corpora, deduped by semantic fingerprint with an hourly
  heartbeat.
- `ibkr regime --log <path>`: manual, opt-in JSONL of full snapshots.
- `docs/docs/internals/regime-backtest.md`: PIT-panel methodology, with gamma and
  breadth explicitly *unavailable* in its historical tiers.

The 2026-06-12 incident still cannot be reconstructed completely from data
that did not exist then, so promotion remains forward-data-driven. The SQLite
cutover deliberately starts current regime/streak state and the
regime/rules/stress/proposal/opportunity decision histories clean. Imported
historic market and gamma measurements are immutable observations stamped
`decision_eligible=false`, never current state or retrospective decisions.

`ibkr regime history` and `ibkr stress history` query the post-cutover event
corpus directly through typed daemon RPC. There is no derived `history.db`,
JSONL backfill/tail ingest, raw-month rotation, archive query, file fallback,
or dual write. See `internal-docs/design/history-index.md`.

### Decision event corpus

`daemon.db` records one typed event per *decision-relevant* snapshot:

- **When:** every assembled snapshot whose semantic fingerprint differs from
  the last persisted event, plus one heartbeat event per hour while snapshots
  are being served (the app polls regime at 1-minute cadence
  (`internal/app/live/service.go:86`); fingerprint dedupe keeps the corpus
  small and the heartbeat keeps a baseline for time-in-state statistics).
- **Schema (versioned `v: 1`):** `ts`, `session_key`; per indicator: raw
  value(s), band, status, freshness class, age at evaluation, streak
  sessions, depth metric (e.g. `hyg_dma_gap_pct`, `vix_ratio`,
  `gamma_gap_pct`), thresholds label; cluster bands raw/confirmed/eligible;
  composite tallies; lifecycle `{stage, severity, readiness, confidence}`;
  posture label; governor records; data-quality statuses.
- **Store contract:** append-only event and typed projection rows; never parsed
  back into current decision authority. The database is not delete-safe and
  has no rotation setting.
- **Consumers:** the backtest plan's forward passes (false-alarm and recall
  measurement against labeled episodes), threshold promotion evidence, and
  incident forensics through typed RPC or read-only database analysis.

### Promotion criteria (binding for leaving `pending_backtest`)

A threshold set may drop `pending_backtest` only with: ≥ 6 months of
decision-event coverage including at least one labeled stress episode or
documented near-miss set; measured false-alarm rate and recall against the
backtest plan's labels; and a written delta in the spec doc bumping the set's
version label. The event corpus exists precisely so this stops being aspirational.

## Part 5 — Front-to-back change list (Q3)

Implementation goes through `.agents/docs/daemon-cli-trading-contract.md`
first (daemon/CLI semantic change), and the SPA rendering changes go through
`.agents/docs/spa-authority-matrix.md`. The authority table below seeds the
contract template.

| Concept | Authoritative source | Typed contract | Renderers |
|---|---|---|---|
| Band thresholds + provenance | daemon code + spec doc (versioned labels) | `RegimeThresholds` (exists) | CLI `--explain`, MCP, SPA tooltip |
| Eligibility gates (depth/streak/freshness) | daemon policy, computed once daemon-side | new `RegimeIndicatorMeta.eligibility` | CLI row suffix, SPA detail line |
| Cadence/staleness policy (max ages) | daemon policy, served | `SourceHealth.MaxAgeSeconds` + row `freshness` | SPA badges, CLI as-of column |
| Lifecycle stage/severity + governors | shared rpc lifecycle builder | `LifecycleState` + new `governors[]` | CLI summary, MCP, SPA headline |
| Headline wording + tone | single shared function in `internal/rpc` | `composite.verdict` == `posture.label` | all (CLI renders served verdict) |
| Stress market confirmation | served eligible tallies / lifecycle | existing stress contract fields | stress CLI, SPA, alerts |
| Decision history | append-only events in `daemon.db` | typed history RPC | CLI, backtest tooling, read-only analysis |

### Daemon (`internal/daemon`)

- `regime_streaks.go` — trading-day session keys; exit-hysteresis-aware
  classification (consult previous band via the store, daemon-side only);
  expose depth metrics.
- `regime_composite.go` — eligibility computation per cluster (with latch);
  independence rescue counts eligible reds only; `verdictFor` delegates to
  the shared rpc wording function; new tallies (`ClusterEligibleRedCount`).
- `regime.go` — HYG/SPY banding input switches to latest official close
  outside RTH; gamma row gains `stale` status for prior-trading-date `AsOf`;
  cadence-freshness computation per row.
- `regime_metadata.go` — populate eligibility + freshness in row meta; serve
  `MaxAgeSeconds`.
- New: `regime_decisions.go` (typed event writer + fingerprint dedupe + hourly
  heartbeat).

### Contract (`internal/rpc`)

Additive only; raw measurements untouched:

- `RegimeIndicatorMeta` += `freshness {class, max_age_seconds}`,
  `eligibility {eligible, latched, reasons[]}` (reasons name the failed gate:
  `depth_below_min`, `streak_1_of_2`, `data_not_due`, `data_overdue`).
- `LifecycleState` += `governors []GovernorAction`.
- `RegimeComposite` += eligible-red tallies.
- Lifecycle builder: revised triggers (all branches kept), governor ordering,
  timing honesty; shared headline/tone function; fingerprint projection v2.
- `CompactRegimeSnapshot` keeps the new fields; `CompactRegimeMonitor`'s
  flattened indicator rows (`compact.go:171-205`) gain explicit
  `eligible`/`freshness_class` fields — they are agent-relevant. No ticking
  values in either view (SSE-hash stability).

### CLI (`internal/cli`)

- `regime.go` — row rendering: provisional marker on non-eligible reds, e.g.
  `stress (provisional — day 1 of 2, depth 0.07% < 0.25%)`; summary governor
  line ("act withheld: thresholds pending backtest, no tape co-sign");
  `--explain` gains eligibility gates and cadence policy; the local
  `(regimeComposite).verdict()` copy is deleted in favor of the served
  verdict.
- `backtest.go` — `rawRegimeClusterBands` / `confirmedRegimeClusterBands` /
  `hasIndependentRegimeRedCluster` / `backtestRegimeVerdict` collapse onto
  the shared rpc functions (the promised lift-on-third-reader).
- `stress.go` (the engine has since moved to `internal/stress`) — **blocker
  fix:** `summarizeStressMarket` and `confirmedMarketStress` consume served
  eligible tallies / the shared confirmation function instead of recomputing
  from raw bands; the stress read's market severity inherits the governor.
  Stress row wording gains the same provisional language.

### MCP (`internal/mcp/tools.go`)

`ibkr_regime` (and the regime-relevant text in `ibkr_stress`) updated to
documentation grade: explain eligible-vs-provisional reds, the governor (why
severity may read watch while two rows show red), cadence freshness, and that
`governors[]` is the place to look before concluding the engine is "ignoring"
red rows. Then `make docs-regen`; `make check` enforces no drift.

### SPA (`web/app/app.js`)

- Headline tone keeps reading `posture.tone` — it inherits the unified
  wording and tone table automatically.
- `renderRegimePanel` stale badge derives from served max ages (drop
  hardcoded `staleMinutes: 60`), reusing the `goDurationMinutes` pattern.
- Status line distinguishes current provisional evidence (for example,
  "2 stress signals pending confirmation") from broken evidence, which uses
  the canonical undefined-data headline instead of a warning euphemism.
- Goes through `.agents/docs/spa-authority-matrix.md`.

### Spec prose, notes, and doc gates

- `docs/docs/internals/regime-dashboard.md`: new "Confirmation eligibility and
  severity governance" section; per-indicator tables gain
  depth/streak/freshness columns; promotion criteria section.
- `docs/docs/internals/regime-backtest.md`: decision-event corpus becomes the
  forward-collection corpus; promotion criteria cross-referenced.
- Both specs have generated `.html` derivatives → run
  `make docs-html-regen` after editing their Markdown sources.
- Indicator notes constants in `regime.go`: each gains one sentence naming
  its eligibility gates (the notes are consumer-visible documentation).

### Tests

- **Incident regression fixture (regime):** synthetic snapshot reproducing
  2026-06-12 inputs (HYG 79.95/50DMA 80.008, SPY +0.3% near highs,
  prior-evening gamma red, VIX 18.84/VIX3M 21.42, vol cluster stale)
  asserting `data_quality / watch / blocked / low`, headline “Market state
  undefined — data incomplete,” and both red measurements retained in
  `unconfirmed` for diagnosis.
- **Incident regression fixture (stress):** same inputs through
  `summarizeStressMarket` asserting no "Confirmed market stress" row and no
  act-severity stress alert.
- Eligibility unit tables per indicator (depth/streak/fast-path/freshness/
  latch), incl. gamma's three red paths and nil-streak fresh-install.
- Required-input fail-closed tables: missing source health, blank status,
  explicit overdue freshness, stale unranked rows, exact typed `not_due`, and
  stale SPY/VIX tape attempting to preserve an otherwise confirmed stage.
- VIX/VIX3M cadence table: before 03:15 ET, VIX GTH, the 09:25–09:31 pause,
  the 09:31 boundary, regular/early close plus 15 minutes, weekends/holidays, missing
  legs, future ticks, and unknown calendar coverage.
- Hysteresis transition tables (enter/exit, no flap; hysteresis-held sessions
  count toward streak).
- Crash-sensitivity fixtures: Aug-2024-style carry unwind (fast paths + tape
  co-sign ⇒ confirmed_stress day 1), gap-crash (panic unaffected), slow-bleed
  2007-style (OAS+funding eligible, act gated on co-sign or promotion —
  asserting the documented trade-off, not hiding it).
- Decision events: fingerprint dedupe, heartbeat, valid typed payload and
  projection.
- Contract: compact + monitor views keep new fields; **monitor-hash
  stability** (two consecutive snapshots with identical semantics hash
  identically — no ticking fields); verdict == posture.label property test
  across all four former call sites; fingerprint v2 projection test (age-only
  change does not re-key).
- Verification per repo rules: `make check`; **full `make smoke`** (daemon +
  wire-path change), `make restart-daemon`, then `ibkr regime` /
  `ibkr regime --json --explain` and an `ibkr stress` output pasted in the
  completion message.

## Part 6 — Settings knobs (deliberately almost none)

Review position, adopted: the eligibility gates (min depth, streaks, co-sign)
are the *same class* of pending-backtest confirmation policy as the band
values themselves — code/spec-owned until promotion. User-tunable gates would
also fork the decision corpus's comparability, undermining Part 4. So:

- **Runtime collection setting:** `regime.journal.enabled` (default true) keeps
  its public compatibility name but controls typed regime decision events in
  `daemon.db`; the preference itself is a versioned document in that database.
- **Stays a code constant until promotion:** every gate value in Part 1's
  table, the co-sign thresholds, the cadence max ages. They are named
  constants in one place with the threshold-set version labels, so a future
  settings task can expose any of them *after* the event corpus gives promotion
  evidence — but the recommendation to that task is: don't, until then.

## Non-goals

- No backtest execution or threshold re-derivation here — there was no data
  at design time; the decision-event corpus supplies it going forward.
- No settings mechanism build (Part 6 names the one knob).
- No stage dwell / lifecycle persistence (deferred; decision events will measure).
- No changes to gamma compute methodology, breadth fan-out, data sources, or
  MOVE/rates-vol addition.
- No change to row-band boundaries or to the panic tape triggers.
- No SPA layout redesign — wording/badge/detail changes only.

## Risks and trade-offs

- **Missed-detection risk** is the cost of every gate. Mitigations are
  structural: current tape triggers remain available; fast-path depths make
  day-one eligibility possible for every gated indicator; and current
  depth/persistence-provisional reds still escalate to `early_warning`
  immediately. Broken inputs do not: they produce `data_quality`, while an
  independently current stress stage may survive an unrelated outage.
  **Stated loudly:** a slow, deep, multi-cluster credit bleed (2007-style) that moves
  no tape will sit at stage `confirmed_stress` / severity `watch` until a
  tape co-sign day or threshold promotion — "act" arrives later than today.
  That is the deliberate price of the provenance gate, and the event corpus will
  measure whether it was ever paid in practice.
- **Tone follows governed severity by design:** deep fresh confirmed evidence
  keeps the "Confirmed stress regime" label, but renders amber/watch when the
  governor holds severity at watch. Red is reserved for act-grade stress, and
  the incident case cannot reach the confirmed label at all (its reds were
  provisional).
- **Degraded-cap scope:** capping on global readiness was rejected (one dead
  feed would mute everything); evidence-keyed capping can in principle miss a
  quality problem outside the confirming set — accepted, since non-confirming
  quality issues only inflate *toward* caution.
- **Fingerprint v2** re-fires active alerts once on upgrade — accepted,
  changelog-noted.
- **Rollback:** policy sits behind typed additive fields and a handful of
  shared functions; reverting the lifecycle builder and composite to raw-red
  tallies restores today's behavior without contract breakage. Persisted
  decision events and current streak state remain authoritative database data;
  rollback does not delete or replace them.

## Review log

- **2026-06-12 — adversarial review (subagent, full code verification):
  needs-rework.** Blockers: (1) canary surface recomputes confirmation from
  raw bands and would have reproduced the incident on the SPA/alert path —
  added as finding #6 and Part 5 canary/backtest consolidation; (2)
  freshness-class definitions readmitted the prior-evening gamma via the
  post-close-window clause — replaced with cadence-relative freshness
  (Part 3). Should-fixes adopted: kept the `SPY ≤ −4% && yellow ≥ 2` branch;
  reconciled the severity table with single-red+tape branches; stated
  tone-vs-severity explicitly; moved hysteresis application daemon-side and
  defined streak-under-hysteresis; added eligibility latch for depth-boundary
  churn; specified fingerprint policy + v2 bump; de-ticked monitor fields for
  SSE-hash stability; enumerated all four headline copies; defined gamma's
  three red paths. Cuts adopted: stage dwell, per-gate settings knobs (one
  event-collection knob remains), top-level policy block, the then-proposed
  journal rotation. Factual
  fixes: SPA regime stale badge is 60m (not 15) at `app.js:3159`; app polls
  regime at 1-minute cadence.
- **2026-06-12 — re-review (fresh subagent, citation spot-checks):
  ready-to-gate, one condition.** Both blockers verified closed; all
  should-fixes/cuts/nits landed; three-table cross-check passed on five
  scenarios. Condition applied: reconciled the urgent row with the panic
  triggers — heuristic-only `eligibleRed ≥ 3` panic now reads severity act
  (not urgent) without co-sign/promotion, making the severity ladder monotone
  and the provenance rationale truthful (pure-tape branches carry their
  co-sign by construction). Cosmetic: `BuildCanaryFingerprint` cite corrected
  to `internal/rpc/fingerprint.go:106`; `CompletedSessionKey` path corrected
  to `internal/breadth/spx/scheduler.go:118`; tone-precedence sentence added
  under the headline table.

## Open questions for the maintainer

1. HYG min depth 0.25% / fast path 1.0% — acceptable starting floors, or
   prefer a vol-scaled depth (e.g. 0.5 × 20d realized vol) at the cost of
   explainability?
2. Label wording at 2 eligible reds: "Confirmed stress regime" (proposed) vs
   keeping "Broad stress regime" and accepting 2-cluster breadth.
3. **Resolved:** regime and stress lifecycle decisions use sibling typed event
   kinds in the same `daemon.db`; neither uses a decision file.
