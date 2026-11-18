# Regime calibration: confirmation eligibility, staleness honesty, and severity governance

**Status:** Implemented; persistence contract updated for `daemon.db`
**Created:** 2026-06-12 13:09 CEST
**Last update:** 2026-11-18 14:08 CET

> **v3 citation note (2026-08-10):** file references below predate the v3
> reduction. `internal/rpc/regime_policy.go` and `internal/rpc/fingerprint.go`
> were consolidated into `internal/rpc/brief.go` and
> `internal/rpc/history_index.go`; `internal/cli/regime.go`,
> `internal/cli/backtest*.go`, and `internal/cli/canary.go` were retired with
> their CLI surfaces. The semantics documented here still bind; resolve cited
> symbols by name in the consolidated files.
**Owner:** osauer
**Related:** `docs/docs/internals/regime-dashboard.md`,
`internal/rpc/lifecycle.go`, `internal/rpc/regime_policy.go`, `internal/daemon/regime*.go`,
`internal/stress/stress.go`, `internal-docs/design/platform-settings.md`,
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
confirm). A stress witness's provisional red caused only by depth or
persistence may warn when the required input set is otherwise usable; a
co-sign's provisional red is context and opens nothing (Part 8).
`data_overdue`, missing health, or an unavailable producer is instead a
data-quality defect.

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
A stress witness's depth- or persistence-provisional red may trigger
`early_warning` only while all required inputs are usable. A broken or overdue
input keeps the provisional measurement visible for diagnosis but changes the
top-level state to `data_quality`; a typed `not_due` schedule remains context,
not a source failure. Provisional reds never:

- count toward the red tally used by `confirmed_stress`,
- rescue another cluster: only a standing stress witness makes a co-sign red
  count (Part 8),
- appear in `confirmed_by`,
- carry `timing: contemporaneous` or `severity: act` in evidence rows.

**A co-sign red counts only beside a standing stress witness.** The HYG proxy,
breadth, the yen and dealer gamma strengthen a warning that equity volatility,
funding or cash credit has started; alone or together they read yellow in the
cluster count and confirm nothing. The rule, its evidence and the two
incidents behind it (2026-10-05 and 2026-09-10) are in Part 8.

Known residual: the HYG 50DMA is built from unadjusted
`TRADES` closes, so each monthly distribution (≈0.5% of price) steps the price
below a lagging average. In a flat-spread tape the proxy therefore sits a few
tenths below its 50DMA by construction, above the 0.25% eligibility floor.
A dividend-adjusted series (`ADJUSTED_LAST`) or a HYG/IEI duration-matched
ratio would remove the bias; it changes the measured series and the streak
history, so it is an operator decision.

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
| VVIX | ≥ 120 (`vvix_daily_v3`, 2026-11-18; was ≥ 110, see Part 8) | ≥ 110 (unchanged; every red is deeper, so it no longer binds) | ≥ 120 (every red reaches it on its first session) | 2 | latest official daily close (newest possible observation) | leave red < 115 (was < 105) |
| HYG/SPY credit proxy | HYG < 50DMA and SPY ≥ 97% of 52w high | HYG ≥ 0.25% below 50DMA | HYG ≥ 1.0% below 50DMA | 2 | RTH tick, or latest official close outside RTH (never a thin pre/post-market tick) | leave red only after HYG closes back above 50DMA |
| Credit spreads (HY OAS) | ≥ 5.5 or 20-obs widening ≥ 1.00 pp | levels are already deep — no extra depth | n/a (official daily series) | 1 | series ≤ 7d old (as today) | leave red < 5.25 and widening < 0.85 pp |
| Funding (CP−T-bill) | ≥ 75 bp | ≥ 75 bp | n/a | 1 | series ≤ 7d old (as today) | leave red < 65 bp |
| USD/JPY | yen +≥2%/week | ≥ 2% (speed *is* the depth; Aug-2024-calibrated) | ≥ 2% in 3 days (existing spec prose, now enforced) | 1 | same-day tick or same-day close | leave red < 1.5% |
| Dealer gamma | spot >2% below γ-zero / wholly short profile | see gamma paths below | see gamma paths below | 1 | current-options-session compute for confirmation; latest completed-session result is typed `not_due` context before the next options session | leave red when the depth gap > +0.5% |
| SPX breadth | < 40% above 50DMA (the value alone; the index level is not read) | ≤ 38% | ≤ 30% | 2 | last completed session's compute (the newest possible observation) | leave red > 45% |

**Gamma eligibility, all three red paths** (red has three producers —
`classifyGammaBand` gap path, no-crossing wholly-short profile
(`regime_streaks.go:390-398`), and the combined-scope weighted vote
(`combineGammaComputedBands`, `regime_streaks.go:415-458`)):

- (a) gap path: min depth = gap ≤ −0.5% below γ-zero; fast path gap ≤ −4.5%
  (the 2026-08-05 saturation re-derivation; it read −2.0% when this table was
  written). Neither gate binds on this path: `classifyGammaBand` already needs
  gap ≤ −2.0% for red, so a single-index red clears the −0.5% floor by
  construction and `MinSessions` is 1, so it confirms same-day whether or not
  the fast path fires. The −0.5% floor is the depth scale's noise floor, and it
  decides only through path (c).
- (b) wholly-short profile with no crossing: an extreme state — depth 100,
  eligible day 1. It has no gap because there is no line left to measure a
  distance from, not because the reading is mild.
- (c) combined-scope vote: eligibility evaluated on the per-index |GEX|-weighted
  mean of the per-index *depths*, same −0.5%/−2.0% levels, over the same legs
  and weights the vote read. Implemented 2026-08-05: `rpc.RegimeGammaDepth` had
  averaged the SPY and SPX gaps *unweighted* while `combineGammaComputedBands`
  voted on |GEX| weight, so a mixed book banded red on the dominant index and
  was then refused `depth_below_min` because the other index's positive gap
  cancelled it out of the mean — SPX −2.5% at 100bn against SPY +2.6% at 10bn
  banded red at depth −0.05, now +2.04 and eligible. Both sides read
  `rpc.GammaIndexWeight` now, which is the single copy of the weight rule.

  Completed 2026-08-06 (issue #28), after the first pass left the combination
  reading gaps rather than depths: a leg on path (b) has no gap, so it was
  skipped *before* its weight was considered and contributed nothing, and the
  mean collapsed onto the crossing leg. SPX wholly short at 100bn against SPY
  +2.6% at 10bn banded red and reported −2.60, refused `depth_below_min`, while
  the strictly worse book — both legs wholly short — was eligible, because a nil
  depth passes the gate vacuously. That ordering is not something the thresholds
  can express, so the fix is structural: `gammaIndexDepth` is one leg's depth on
  either path, and `gammaCombinedDepth` weighs those. Human policy decision
  2026-08-06: yes, extend path (b) into path (c) at full weight — strictly more
  red gamma readings become eligible to confirm; thresholds unmoved.

  Asymmetric by decision, same date: a leg wholly *long* with no crossing has no
  depth to contribute and is left out of the mean rather than counted as extreme
  calm. The scale defines an extreme for the amplifying side only. So an
  equal-weight book with one index wholly short and one wholly long confirms on
  the short leg instead of cancelling to zero.

  `rpc.GammaCombinedGapPct` is deliberately not the same function: it reports a
  measured distance and is what the streak store and the renderers read, so it
  keeps skipping a leg that has no crossing to measure.

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

`BuildRegimeLifecycle` (`internal/rpc/lifecycle.go`) tests the stages in this
order on the confirmed cluster bands of Part 8, where a co-sign red counts
only beside a standing stress witness. `eligibleRed` is therefore at least 1
only when a witness stands.

- `panic` — a fall on the tape: `(SPY ≤ −4% && eligibleRed ≥ 1) || SPY ≤ −7%`.
  Reds alone, however many, are `confirmed_stress`. With an input impaired,
  panic stands only on a current SPY print; without one it is
  `confirmed_stress` when that stands on current evidence (Part 8).
- `confirmed_stress` — `eligibleRed ≥ 2 || (eligibleRed ≥ 1 && SPY ≤ −2.5%)
  || (SPY ≤ −4% && yellow-or-worse ≥ 2) || (eligibleRed ≥ 1 && VIX +20%)`. A
  standing witness plus one co-sign red confirms; two co-signs never do.
- `early_warning` — a confirmed eligible red; or a witness cluster that is raw
  red but not eligible yet (a day-one term inversion, a witness red on a row
  that is not current); or a current cash credit row whose HY OAS is 0.50 pp or more
  wider over 20 observations, the row's own yellow step; or three clusters at
  yellow or worse of which at least one is a witness at yellow; or a stress
  witness row nobody can read beside a co-sign row at yellow or red
  (`witness_unavailable`, Part 8); or `SPY ≤ −1.5%`; or `VIX +10%`. A red or
  yellow in co-sign clusters alone opens nothing while every witness can be
  read. All of it only when the required input set is usable.
- `opportunity` — no red, at most one cluster at yellow, at least three
  ranked clusters, `SPY ≥ +1.5%` and `VIX ≤ −10%`. Every cluster counts,
  co-signs included; a co-sign red with no witness standing is one yellow.
- `stabilization` — no red, one or two stress witnesses at yellow, and
  `SPY ≥ +1%` or `VIX ≤ −10%`. Co-sign yellows are not counted here: a day
  whose only yellows are co-signs has nothing to stabilize from and is
  `quiet`.
- `quiet` — none of the above.

Every provisional red is named in `unconfirmed`. Outside `data_quality` a
co-sign red with no witness standing is context and is repeated in
`suppressed`. A co-sign red beside a standing witness that has not passed its
own gate is not context: it keeps its cluster red and waits to confirm, as a
provisional witness does, and is in `unconfirmed` only. In `data_quality` the
lifecycle repeats every unconfirmed red in `suppressed`, witnesses included;
that branch predates the roles, and no surface words the list.

`rpc.RegimeWarningOpeners` names the arms that open a warning on a snapshot;
the stage vote and the required-input gate read the same list.

After stage selection, the required-input gate runs fail-closed. A blank or
non-OK status, explicit `freshness: overdue`, missing source-health row,
unknown refresh state, or source age beyond its served maximum changes the
result to `data_quality / watch / blocked / low`. The only schedule exceptions
are exact typed `not_due` contracts for VIX3M (with current VVIX and a VIX leg
that satisfies its own schedule) and gamma. There is no generic “unranked stale
is fine” exemption. Confirmed stress or panic survives an unrelated defect only
when every witness that independently establishes that stage is current; stale
SPY or VIX tape cannot preserve it. A witness is judged by its own row: an
eligible witness red on a current reading counts although its cluster-mate is
defective. When the defect is local (one cluster, attributable) and a stress
witness row is dark beside a co-sign at yellow or red, the gate does not
blank the stage: it is `early_warning` (Part 8, owner ruling 2026-11-18 14:47
CET). Two or more defective clusters, or an unattributable defect, still
produce `data_quality`.

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
equity-vol corroboration, which stayed session-blind by the decision of
2026-07-19, was removed on 2026-11-18 (Part 8, "Witness health and standing"):
an eligible witness red stands. The rulebook's
regime-stage latch skips closed-date snapshots so the last trading-date stage
ages into the carried worse-of(carried, calm) path instead of a weekend stage
re-latching fresh.

For the 2026-06-12 regression fixture as recorded — HYG red provisional,
gamma cadence-overdue, and volatility explicitly stale without a valid
`not_due` contract — this now yields `data_quality / watch / blocked / low`
and **“Market state undefined — data incomplete.”** The red measurements stay
visible in `unconfirmed`, but they do not define a market warning. The same
marginal HYG break evaluated with a complete, current required-input set reads
`quiet`: the proxy is a co-sign, and with no stress witness red it is context
on the panel, named in `unconfirmed` and `suppressed`.

### Severity governor (ordered, applied after stage selection)

1. **Provenance gate** (task Q4 — the explicit policy):

   > While a confirming cluster's threshold set carries
   > `pending_backtest: true`, its evidence is *heuristic evidence*. The
   > gate lifts when either a fresh tape co-signature is present in the same
   > snapshot (SPY day change ≤ −1.5%, or VIX day change ≥ +10%, or a
   > same-session VIX-term inversion at ratio ≥ 1.00 on a fresh tick) or
   > every confirming set has been promoted. Without a lift, heuristic
   > evidence is capped one severity rung down: stage `confirmed_stress`
   > reads **watch** instead of act, however many eligible reds it has.
   > Panic is entered only on the tape (SPY ≤ −7%; SPY ≤ −4% with an eligible
   > red, which is itself act-grade tape). While that SPY print is current it
   > carries its co-signature by construction and reads **urgent**. With an
   > input defect and no current SPY print a panic does not survive: it is
   > `confirmed_stress`, which passes through both caps like any heuristic
   > confirmation (2026-11-18, Part 8).
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
| act | stage `confirmed_stress` (any branch) with promotion or tape co-sign and quality-clean confirmers; a panic without a current SPY print is `confirmed_stress` since 2026-11-18 |
| urgent | panic on a current tape: SPY ≤ −7%, or ≤ −4% with an eligible red — the tape is act-grade by construction |

Heuristic eligible reds without a tape co-sign read watch, two or five alike;
a tape co-sign or promotion lifts them to act. "Act" is reachable with a
single eligible red **when the tape co-signs** (SPY ≤ −2.5% / VIX +20%
branches) — intended; the tape is the second witness. A same-session term
inversion is itself a co-sign, so a confirmation that stands on the VIX term
structure reads act. Stage and severity stay separable throughout.

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
| stage is quiet, opportunity or stabilization | Normal regime | normal; watch for stabilization, or while a stress witness is yellow |
| a red cluster (a witness red, eligible or not, or a co-sign red beside a standing witness) | Stress signal present | watch |
| one stress witness warning without confirming (a held-back witness red, cash credit widening, or a witness nobody can read beside a weak co-sign) | Watch: one unconfirmed stress signal | watch, also while readiness is degraded by that witness |
| more than one | Watch: unconfirmed stress signals | watch |
| yellow ≥ 3 | Elevated stress watch | watch |
| otherwise (an early warning the tape alone opened) | Normal regime | watch |

A co-sign's red or yellow changes neither the headline nor the tone. Where a
surface needs a sentence for it, the sentence is "Context, not a stress
reading on its own: <names>." (`rpc.RegimeContextNote`).

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

> Superseded in part by `internal-docs/design/regime-input-currency.md`
> (2026-07-31). The per-indicator cadence table below still holds. What changed
> is the vocabulary and how it is consumed: five classes instead of two
> (`fresh` / `not_due` / `pending` / `stale` / `overdue`), confirmation as an
> allowlist on `fresh`, currency judged per evidence unit rather than per
> cluster, and a defect degrading the read rather than blanking it until two
> clusters are affected. Decision events carry `currency_policy` so the
> calibration corpus stays partitionable across that cutover.

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
- The former manual regime JSONL logger was retired in v3; typed daemon decision
  events are the calibration authority.
- Historical calibration is driven by the typed decision-event corpus below;
  the retired general-purpose backtest CLI is not a product surface.

The 2026-06-12 incident still cannot be reconstructed completely from data
that did not exist then, so promotion remains forward-data-driven. The SQLite
cutover deliberately starts current regime/streak state and the
regime/rules/stress/proposal/opportunity decision histories clean. Imported
historic market and gamma measurements are immutable observations stamped
`decision_eligible=false`, never current state or retrospective decisions.

Regime and Stress event history remains internal evidence rather than a product
surface. There is no derived `history.db`, JSONL backfill/tail ingest,
raw-month rotation, archive query, file fallback, or dual write.

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

The pre-v3 regime and stress MCP adapters documented eligible-vs-provisional
reds, governor behavior, and cadence freshness. V3 retired those standalone
tools; the same typed authority now reaches MCP through the brief and rulebook.

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
- `docs/docs/internals/regime-dashboard.md`: the decision-event corpus is the
  forward-collection corpus and the promotion criteria are documented there.
- The public contract publishes as generated HTML in the Pages artifact → run
  `make pages-build` after editing its Markdown source.
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
  wire-path change), `make restart-daemon`, then redacted `canary status`,
  `canary brief`, and `canary rules` evidence in the completion message.

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

## Part 7 — What the shadow scoring experiment measured (2026-08-06)

A shadow regime model ran beside the shipped stage machine, journalling a
second scoring of every decision without serving it. It is being removed. This
section is the durable record of what its readings did and did not establish,
written because the code and its journal rows both go away.

**The decision it existed to support.** Whether the band-plus-gates
architecture should be replaced by a continuous strength score — specifically
whether the six mechanisms bolted on after 2026-06-12 (depth floor, fast path,
eligibility latch, red-exit hysteresis, persistence, currency) can be subsumed
by ramping each indicator's strength through its own band boundaries, so that a
7bp HYG break and a 3% break become different numbers rather than the same
band. An earlier static count put the reduction at 194 decision points to 86.

**Verdict: the readings cannot support that decision, and are not close.**
Not primarily because of sample size. The window contains no variance in the
quantity under test.

| what was recorded | value |
|---|---|
| window | 2026-08-05 19:54Z → 2026-08-06 06:24Z, ~10.5h |
| rows / distinct snapshot fingerprints | 144 / **12** (~91% are republication duplicates) |
| shadow confirming arm, observed range | 0.000 – 0.429 against a 1.0 cutoff |
| shadow warning arm | 0.000 – 1.232 against a 0.25 cutoff |
| tape term | 0.000 throughout |
| clusters ever carrying weight | credit, fx, vol only — breadth, funding, gamma zeroed by staleness for the whole window |
| red-band stress events | none |

The window runs from just after a US close through one overnight session, so
the daily-cadence indicators are stale by design and the currency gate zeroes
half the board. The confirming arm never came within a factor of two of its
cutoff. Nothing in the window exercised the discrimination the model exists to
perform.

Two further reasons the corpus is not homogeneous, both worth knowing before
anyone cites these numbers: `RegimeGammaDepth` changed mid-window (f379ea6), so
gamma strengths before and after are on different inputs; and the model id
moved v3→v4 during development, which by the model's own versioning rule means
readings under the two definitions must not be pooled.

**What would actually be needed.** The unit is regime *episodes*, not days or
rows — the previous 14-day corpus had six of eight indicators never leaving
green, so calendar time buys nothing on its own. To decide this you would need
at least two independent stress episodes, each carrying a cluster across its red
boundary with real depth, so that both models are observed near their decision
boundaries and one episode cannot be a fluke; plus a calm baseline long enough
to measure the false-positive rate the gates exist to suppress. On this desk's
observed episode rate that is months, not weeks. Anything less measures
agreement in the region where neither model is deciding anything.

### Finding worth keeping: the shipped model has two routes to `data_quality`

Five of the twelve states read `data_quality` from the shipped model while the
shadow returned `early_warning`. The obvious hypothesis — that the shadow
ignores the ranked-cluster floor — is wrong. The shadow applies
`RegimeVerdictFloor` correctly, and on those states the ranked count was 5 or 6,
comfortably above it.

The real cause is that `data_quality` is reachable two different ways:

1. **too few ranked clusters** — the `RegimeVerdictFloor` check, and
2. **an input-health defect** — a degraded or stale source lane forcing the
   whole verdict down regardless of how many clusters ranked.

On these states it was route 2: gamma blocked on a missed session, with
funding, gamma and breadth all stale. The shadow models only route 1, so it
scored a verdict from the clusters that were still current.

This is recorded as a finding about the **shipped** model because it raises a
question the shadow's removal does not settle: overnight, with five or six
clusters ranked and current, the shipped model refuses to state a verdict at
all because other lanes are stale. Whether "some inputs are stale" should
suppress a verdict the surviving inputs could support — rather than qualify it
— is a live design question, and it is the one thing this experiment surfaced
that outlives it.

### Finding worth keeping: the disagreement was cosmetic in operational terms

On all four fingerprints where the shipped model read `confirmed_stress` and
the shadow read `early_warning`, the served severity was `watch` — the
provenance governor had already capped it, because every threshold remains
`pending_backtest`. Across all twelve states, every severity was `watch`.

So on this window the two models produced the same operational posture
throughout, and the stage-name disagreement had no consequence a desk would
act on differently. That is a caution about reading stage-agreement percentages
as if they measured impact: measure severity and the action it implies, not
the stage label.

## Part 8 — Stress witnesses and co-signs (2026-11-18 11:14 CET)

Owner instruction, 2026-11-18 08:29 CET, as recorded in the regime-fix handoff
of that morning: a Regime warning on every trading day is wrong and needs a
solid fix, tested against history and tuned for proper sensitivity.

### What was wrong

The diagnosis of 2026-11-18 found no trading day reading `quiet` in the
daemon's retained decisions from 2026-10-06 on. One row carried it: the HYG
proxy is red whenever HYG closes below its 50-day average while SPY is within
3% of its 52-week high, and any red, eligible or not, opened `early_warning`.
A second red made `confirmed_stress`, and three made `panic` with no fall: in
the replay the proxy, the yen and breadth read `panic` on 2026-09-10 with SPY
2.6% below its highest close of the year and VIX/VIX3M at 0.90. On 2026-10-05
the proxy and breadth had confirmed each other with VIX at 15.5 and HY OAS at
3.10%.

### The two roles

- **Stress witnesses** measure stress directly: the equity-volatility cluster
  (VIX term structure, VVIX), funding, and the cash credit row
  (`credit_spreads`) inside the credit cluster. Only a witness starts a
  warning.
- **Co-signs** are the HYG proxy row, breadth, USD/JPY and dealer gamma. A
  co-sign strengthens a warning a witness has started. It never starts one,
  and no co-sign cluster confirms one, alone or together with other co-signs.
  The tape can confirm stress while every red is a co-sign (`SPY ≤ −4%` with
  two clusters at yellow or worse); `confirmed_by` then names `tape` and the
  co-signs stay context.

A witness **stands** when its red is eligible, with no further condition. The
roles live in one list, `regimeRowRoles` in `internal/rpc`, which the
combination, the lifecycle, the stress read, the regime summary and the row
notes read.

In the confirmed cluster bands (`rpc.BuildRegimeClusterBands`) a co-sign
cluster that is raw red stays red only while a witness stands; otherwise it is
yellow. In the credit cluster a red cash row is a witness red; a red from the
proxy alone is a co-sign red, and the proxy's eligibility never turns a cash
red that may not confirm into one that does. This replaces the cash-credit
veto of the proxy, the isolated-FX downgrade and the divergence-cluster rule
with one rule.

Stage triggers are in Part 2. Panic is a fall on the tape; three eligible
reds without one are `confirmed_stress`, which the provenance gate holds at
watch unless the tape co-signs.

### Evidence

History replay, `TestRegimeHistoryReplay` in `internal/daemon`: the daemon's
band rules, streak and eligibility pass, cluster combination and stage vote,
run once per session over a file of daily closes from 2021-11-18 to
2026-11-17 (market data, kept outside Git). 1,003 sessions are replayed from
2022-11-17. The outcome is SPY's lowest close in the next 20 sessions against
that day's close. Gamma has no history and is absent; breadth covers 204
sessions and cash credit 752. Inputs with no reading are left out of the vote.

| Condition | Share of sessions | Low 5% or more below | Low 8% or more below |
|---|---|---|---|
| All sessions | 100% | 12% | 3% |
| VIX/VIX3M at or above 1.00 | 5% | 33% | 14% |
| HY OAS wider by 0.5 pp or more over 20 observations | 5% of 752 | 44% | 35% |
| VVIX at or above 110 | 12% | 19% | 5% |
| VVIX at or above 120 | 5% | 21% | 5% |
| HYG proxy row red | 30% | 9% | 0% |
| USD/JPY row red | 13% | 14% | 1% |
| Breadth row red | 23% of 204 | 20% | 0% |

The first four rows are witness conditions read straight from the file: the
term inversion is that row's red; VVIX at 110 was the VVIX row's red until
2026-11-18 and 120 is its red since; a 0.5 pp widening is the cash row's
yellow (its red needs 1.0 pp or a level of 5.5). The last three
are served row bands from the replay, exit hysteresis included. A term inversion and a widening of cash spreads precede falls at
about three to twelve times the base rate, VVIX at about one and a half
times. The proxy precedes fewer falls than an average session and the yen
does no better than average. Breadth turned red 30 sessions into the 2026
fall, 13 before its low, and again from 2026-09-09, when no 8% fall followed.
Dealer gamma could not be measured; the product describes it as a conditional
amplifier, which is a co-sign's job.

Before the change `early_warning` was followed by a 5% low after 12% of
sessions, the base rate. Of the four falls of 8% or more that the replay
covers (highs of 2023-07-31, 2024-07-16, 2025-02-19 and 2026-01-27),
`early_warning` now comes 16 sessions before the high, 2 after, 16 before and
6 before; `confirmed_stress` comes before the low in three of the four. The
stage shares are under Result below.

### What this rests on, and what still bounds risk

Band boundaries, depth and streak gates and hysteresis are unchanged, with
one exception the owner decided on the same day: the VVIX red line and its
exit ("VVIX red line at 120" below). All of them stay `pending_backtest`.
This change is to the vote, and it rests on a history replay with the limits
above, not on the six months of forward decisions the promotion criteria in
Part 4 require; those criteria still bind every threshold set.

A session that now reads `quiet` moves the trading rulebook's regime-stage
latch from its early-warning bucket to calm on the days only a co-sign is
red. `stabilization` stays in the early-warning bucket and needs a stress
witness at yellow.

What still bounds risk on such a day: the rulebook's calm thresholds still
apply; every witness still warns from its first red session; the SPY and VIX
tape arms open `early_warning` on a 1.5% fall or a 10% VIX rise the same day;
and the stress read keeps its own rows. The worst case is a fall that only
co-signs foresaw. Among the sessions with 20 more after them, none of the 278
with the proxy red and none of the 46 with breadth red was followed by a low
8% or more below; one of the 126 with the yen red was.

### Cash credit widening opens a warning

The strongest condition in the table is a yellow: a 0.5 pp widening of HY OAS
is the cash row's yellow step. From that step the cash row opens
`early_warning` as a stress witness that warns without a red, provided the
row is current. It counts with the provisional witnesses in the headline and
has its own opener, `cash_widening`: the credit cluster can hold the proxy's
red at the same time, and that red stays a co-sign shown as context. A yellow
from the level alone (HY OAS between 4.0 and 5.5 without widening) opens
nothing. The number has one copy, `rpc.RegimeCreditWideningYellowPP`, which
the row classifiers also band on.

The evidence is thin and is stated as such: 36 sessions in five runs
(2024-08-05, 2025-03-11 to 03-20, 2025-03-28 to 04-24, 2026-10-01 and
2026-10-14 to 10-22), followed by a 5% low on 44% and an 8% low on 35%,
against 12% and 3% for all sessions. Five runs are three market episodes.
Four of those sessions, 2025-03-17 to 03-20, read `quiet` or `stabilization`
before this opener existed, three weeks before the April 2025 low.

### Every surface follows the vote

- **Headline and tone** (`rpc.RegimeHeadline`, `BuildRegimePosture`). A calm
  stage reads "Normal regime". The Watch headlines count stress witnesses that
  warn without confirming, and `composite.cluster_provisional_red_count` is
  that count. A co-sign's yellow or red does not colour a calm read; a stress
  witness at yellow still keeps it on watch.
- **Regime summary** (`summary.punch_line`, `summary.dominant_risks`). A
  co-sign red with no witness beside it is named in the context sentence, not
  as stressed and not as a dominant risk. A cash row whose widening opens a
  warning reads "cash credit spreads is widening", not as one more mixed
  reading.
- **Stress read** (`internal/stress`). `market.warning_openers` carries
  `rpc.RegimeWarningOpeners` for the regime read behind the summary, and the
  market rows follow it: market pressure is partial only when a cluster arm
  opens a warning or the tape does; three yellow clusters are a deteriorating
  tape only with a witness among them; the fast carry unwind reads the
  confirmed FX band; and "an early warning is flashing" names what warns
  without confirming: a witness that is red and, as "cash credit spreads
  widening", the cash widening. A co-sign indicator that is red with no
  witness standing is amber and carries the context sentence.
- **Names and roles, served once.** `market.unconfirmed_signals` lists what a
  surface may name beside the confirmed reds: each unconfirmed red cluster
  and the cash widening, with the reader's name of the row behind it and its
  role (`witness`, `widening`, `waiting`, `context`; under a dark-witness
  warning also `unavailable` and `weak`). The names come from one
  list, `rpc.RegimeRowName`, which the regime summary reads too; in the credit
  cluster the name is the cash row's whenever the cash row is red. The stress
  evidence and the app word the context sentence from these names, and the
  app keeps no name list and no role rule of its own for it. A provisional
  red keeps the app's input label ("volatility feed", "HYG 50-DMA"), except a
  credit witness and the widening, which take the served name.
- **One test for context.** A co-sign red is context when its cluster is not
  red in the shared combination, which is when no witness stands. The regime
  summary, the stress rows, the indicator rows and the served roles apply
  that test; `lifecycle.suppressed` applies it outside `data_quality`
  (Part 2).

Over the replay every session that reads `quiet` reads "Normal regime". 223
of the 781 quiet sessions keep the watch tone because a stress witness is
yellow; 191 did before any of this. 63 `early_warning` sessions read "Normal
regime" on a watch tone: the tape alone opened them.

### Result

| Stage | Before | After | After: low 5% or more below | After: low 8% or more below |
|---|---|---|---|---|
| `quiet` | 47% | 78% | 10% | 2% |
| `early_warning` | 40% | 13% | 19% | 11% |
| `confirmed_stress` | 6% | 5% | 17% | 2% |
| `panic` | 3% | 0.4% | three sessions | |
| `stabilization` | 4% | 3% | 25% of 28 | 0% |
| `opportunity` | 1% | 1% | 0% of 8 | 0% |

"After" includes the VVIX red line at 120 and the witness rules (next two
sections); with the line at 110 `quiet` was 74% and `early_warning` 17%.

314 sessions that warned before no longer do; none was followed by a low 8%
or more below and 10% by a low 5% or more below, under the base rate. Four
sessions warn that did not: the March 2025 widening. The share of sessions in
the rulebook's early-warning bucket or worse falls from 53% to 21%.

### VVIX red line at 120

Owner decision, 2026-11-18 12:59 CET. Asked "Which early-warning sensitivity
should I land?" with the replay table below in front of them, the owner
answered "Tighter: 13% of days", the option described to them as: VVIX red
line from 110 to 120.

What changed: the VVIX row turns red at 120 (it was 110) and leaves red below
115 (it was 105). The threshold set is `vvix_daily_v3` and its published
texts read "VVIX >= 120" and "trips >=120". The yellow rule, the eligibility
gate (minimum depth 110, day-one depth 120, minimum streak 2) and the
isolated-VVIX rule (120) were left as they were; the isolated-VVIX rule was
removed later the same day (next section). The band stays `pending_backtest`:
this rests on the history replay, not on the forward decisions Part 4 asks
for.

In plain words: a VVIX red is deep enough on the session it turns red to be
eligible at once, because the red line and the gate's day-one depth are the
same number, and an eligible witness red stands. A red that exit hysteresis
holds between 115 and 120 keeps its eligibility and keeps standing until it
is released.

What that leaves of the older numbers: the gate's minimum depth of 110 no
longer binds, and its minimum streak binds only for a held red whose first
session was not banked.

Evidence, the replay with the line at 110 against the line at 120 (vote
only, 1,003 sessions):

| | Line at 110 | Line at 120 |
|---|---|---|
| `quiet`, whole file | 74.3% | 78.0% |
| `early_warning`, whole file | 17.4% | 13.3% |
| `confirmed_stress`, whole file | 4.8% | 4.6% |
| `stabilization` / `opportunity`, whole file | 2.3% / 0.8% | 2.9% / 0.9% |
| `quiet` / `early_warning` / `confirmed_stress`, 2026 | 63.8% / 24.9% / 9.0% | 69.7% / 19.5% / 8.1% |
| After `early_warning`: sessions, low 5% or more below, 8% or more below | 169, 18.3%, 8.3% | 129, 19.4%, 10.9% |
| VVIX row red, share of sessions | 14.4% | 6.0% |
| Rulebook early-warning bucket or worse | 24.9% | 21.1% |

The first warning before each of the four falls is on the same session
either way. `confirmed_stress` before the 2026 low comes on 2026-03-06, 16
sessions before the low, where it came on 2026-03-03. 44 sessions that
warned with the line at 110 no longer warn; of the 42 with 20 sessions after
them, seven were followed by a low 5% or more below and none by 8%. Two of
the 44 are 2026-11-02 and 2026-11-03, the sessions before the proxy-red
rebound of 2026-11-04 and the 4.4% fall of 2026-11-05. VVIX between 110 and
120 now reads yellow while it is rising, which adds sessions to the
three-yellow opener (23 to 31) and to `stabilization`. `opportunity` gains
one session, 2024-08-13, where a VVIX of 114 is released from red.

At the install: the streak store keeps the row's last band across a restart.
A VVIX red entered under the old line is therefore held while VVIX stays at
or above 115, even though it never reached 120; it releases below 115 like
any other. The replay starts from an empty store and does not show this.

### Witness health and standing

Review of 2026-11-18 (coordinator's instruction of that day, after an
independent review of this change). Five rules, each with a test that failed
before it.

- **A dark stress witness beside a weak co-sign is an early warning.** Owner
  ruling, 2026-11-18 14:47 CET. Asked "When a real stress input is dark and
  weak readings are yellow or red, what should Regime say?" with the two
  options "Early warning" and "Data incomplete", the owner answered: "Early
  warning". This keeps the operator decision of 2026-08-11 ("unknown cash
  evidence never softens a live warning": when the state is unknown, assume
  the worst), which the roles change had reversed without a record and the
  first review pass had turned into "data incomplete", a read that holds the
  rulebook's stage latch, calm included.

  The rule: a witness row (VIX term, VVIX, cash credit, funding) is dark when
  its own row is failed, unavailable, stale or overdue, cannot fill a field
  it needs to band, or is older than its cluster's age bound (VVIX on its
  dated not-due schedule: past its next due close) (`regimeWitnessRowDark` in
  `internal/rpc`). A valid typed `not_due` or `pending` schedule inside the
  bound is not dark, and each row is judged by itself, the cash row as much
  as the term row. These are the row-level parts of the input verdict; a
  defect that only the served data-quality items or source health carry is
  not a dark witness, because the summary reads the openers before those are
  stamped. When at least one witness row is dark and at least one co-sign row is
  served at yellow or red, the opener `witness_unavailable` is in
  `rpc.RegimeWarningOpeners`, so a stage that would be `quiet`,
  `opportunity` or `stabilization` is `early_warning` at watch and the
  rulebook moves to its early-warning bucket. A tape-made `opportunity` or
  `stabilization` gives way like a quiet day. An `early_warning` is never
  blanked to `data_quality` because the witness that is dark sits in a
  defective cluster: that witness is the reason for the warning. Stages that
  a readable witness or a current tape arm establish (`early_warning`,
  `confirmed_stress`, `panic`) keep their rules: a `confirmed_stress` or
  `panic` that fails its survival test is `data_quality` as before, and the
  rulebook's latch holds. The headline is "Watch: one unconfirmed
  stress signal" (`composite.cluster_provisional_red_count` counts each
  cluster with a dark witness while the condition holds), the tone is watch
  although readiness is degraded, the regime summary words the co-signs as
  weak ("ETF credit proxy is weak; … volatility term structure is
  unavailable."), and the stress read names the cause: "An early market
  warning is flashing (volatility term structure unavailable; ETF credit
  proxy and breadth weak, not confirmed yet)", from
  `market.unconfirmed_signals` rows with the roles `unavailable` and `weak`.

  The existing floor still applies: with two or more clusters defective or
  impaired, or a defect that is not attributable to a cluster, the read is
  `data_quality` and the rulebook's latch holds. Such a read is
  "Market state undefined — data incomplete", not calm, but the latch it
  holds can be calm.

  What still bounds risk: a calm stage only rises; a stress stage keeps its
  rules; the early-warning bucket is stricter than calm. The worst case is a
  loosening: where an early warning used to be blanked to `data_quality`,
  which held the rulebook's latch, it now writes the early-warning bucket.
  When a confirmed stress rests on a term inversion and the term row goes
  overdue (from 09:31 until the first live VIX3M tick, or off the window
  when Cboe's close does not vouch), the latched red drops, the vol red is a
  provisional witness, and beside red co-signs the read is `early_warning`:
  the rulebook moves from its confirmed thresholds to its early-warning ones
  until the row returns, minutes in the usual case. At 994c9be6 the read was
  `data_quality` and the latch held confirmed.
  `TestConfirmedStressHoldsThroughTheTermRowsNotDueWindow` pins this. The
  other cost is false warnings while inputs arrive late (below).
  `TestRegimeDarkWitnessBesideAWeakCoSignIsAnEarlyWarning`,
  `TestRegimeTwoDarkClustersStillBlankTheRead`,
  `TestDarkWitnessWarningMovesTheRulebookBucket`,
  `TestStressReadsTheDarkWitnessWarning`.
- **A witness is judged by its own row.** A current witness red is not muted
  by its cluster-mate's outage: the cash red stands with the proxy row
  unavailable, VVIX at 125 stands with the term row in error. The
  required-input gate accepts an early warning whose witness red row can be
  read, and the survival test counts a witness whose eligible red row is
  current although its cluster is not. The quality governor still caps the
  severity at watch while a confirming cluster is impaired, and the stress
  read agrees ("Confirmed stress held at watch").
  `TestRegimeWitnessIsJudgedByItsOwnRow`,
  `TestStressAgreesWithTheVoteWhenTheCashRowsMateIsDark`.
- **Panic needs a current tape.** With an input impaired, panic survives only
  on a current confirmable SPY print. Three current eligible reds without one
  are `confirmed_stress` at act, confirmed by those clusters, and the
  lifecycle discloses the downgrade as a governor record (`stage_downgraded`,
  panic to confirmed_stress, `tape_not_current`).
  `TestRegimePanicNeedsACurrentTape`. The SPY print is judged by the credit
  cluster (`regimeLifecycleSPYTapeCurrent`), not by the proxy row it rides
  on, so an outage of the cash row alone also makes it not current; that is
  unchanged here.
- **A witness keeps standing through its own not-due window.** A witness red
  that was eligible in its window stays eligible while its own row is typed
  `not_due` and still red (exit hysteresis included); the eligibility latch
  of the red streak carries it. Overdue, stale, pending or failed evidence
  still drops it, and a co-sign's prior-date reading stays context. The hold
  keeps a confirmed stress only while the vol cluster's own not-due contract
  holds (VVIX current, or on its dated not-due schedule): with VVIX dark at
  the same time the latched term red is not a current reading, the stress
  does not survive the gate, and the dark VVIX beside the co-signs makes the
  read `early_warning`. A confirmed stress resting on a term inversion
  therefore no longer drops to `early_warning` at the close and returns at
  09:31: it holds the same evening, the next morning before 09:31 and over
  the weekend. Its severity still falls from act to watch at the close: the
  provenance gate's term-inversion co-sign needs a same-session row.
  `TestConfirmedStressHoldsThroughTheTermRowsNotDueWindow`.
- **An eligible witness red stands.** The isolated equity-vol corroboration
  is removed. With the VVIX line at 120 it decided only a red held between
  115 and 120, where its "SPY down 1%" and "VIX up 20%" terms turned every
  co-sign from context into confirmation on one tick, with no hysteresis.
  `TestRegimeEligibleWitnessStandsWhateverTheTape`.
- **The widening is judged by the cash row.** It opens while the cash row is
  current, whatever the proxy row beside it can serve.
  `TestRegimeCashWideningIsJudgedByTheCashRow`.
- **A confirmed stress does not rest on co-signs beside a broken witness.**
  A co-sign's red survives a broken input only beside a clean standing
  witness. With the term row in error and its red held, the proxy and
  breadth red read `early_warning` by the dark-witness rule, not confirmed
  stress. `TestRegimeConfirmedStressDoesNotRestOnCoSignsWhenItsWitnessIsBroken`.

Each row's standing is decided once, in `rpc.BuildRegimeClusterBands`, row by
row: a red `counts`, is `waiting` (a witness red that is not eligible yet, or
a co-sign red beside a standing witness that has not passed its own gate), or
is `context` (a co-sign red with no witness standing). The daemon stamps it
on every served snapshot as `row_standings` (indicator, reader's name, role,
red, and the context sentence for a red that is context). The field is
response-only, like `authority_health`: it is never persisted, so a retained
snapshot keeps its shape and a rollback still loads it. The monitor view
passes role, red and note through per indicator, the CLI prints a context
co-sign as amber with the served sentence, and the stress read serves
`market.context_note` with its context signals, so the app keeps no copy of
the sentence. In a credit cluster whose cash row is red but may not confirm
and whose proxy is red with no witness standing, the cash row warns and the
proxy row is context; `lifecycle.suppressed`, which is keyed by cluster,
does not list that cluster. The regime summary words a usable row by its
served band, so a red held by exit hysteresis reads stressed there too.

In the replay this moves four sessions of 1,003: three held VVIX reds beside
co-signs read `confirmed_stress` where they read `early_warning`
(2025-11-21, 2026-03-16, 2026-03-19), and 2026-09-18 reads `data_quality`
where it read `quiet`, because commercial paper had gone a week without a
print and the proxy and breadth were red. `confirmed_stress` is 4.9% of
sessions against 4.6%.

After the owner's ruling of 14:47 CET one session of 1,003 moves again:
2026-09-18 reads `early_warning` (opener `witness_unavailable`, "Watch: one
unconfirmed stress signal") where it read `data_quality`, and no session of
the vote-only replay reads `data_quality`. `early_warning` is 13.1% of
sessions (13.0% before), 19.0% in 2026 (18.6%); after it a low 5% or more
below follows on 18.9% and 8% or more on 11.0% (19.0% and 11.1%). The first
warnings and confirmations before the four falls are unchanged.

What the replay cannot see: it evaluates once a session at noon New York and
leaves inputs with no reading out of the vote, so it shows neither a row
going dark inside a session nor the close of a row's window. The tests named
above cover those cases.

**When a witness reads dark on a normal week.** From the cadence rules in
`internal/daemon/regime_indicators.go` (`vixTermCadenceClass`,
`volOfVolCadenceClass`, `volOfVolNextDue`), not changed here and pinned at
named instants by `TestWitnessDarkHoursOnANormalWeek`:

- VVIX is current while Cboe's latest dated close is at most 4 days old,
  measured from midnight UTC of its date. On Tuesday to Friday the previous
  session's close always is. On a Monday, Friday's close passes 4 days at
  19:00 New York (20:00 in summer time), the session has closed, and the row
  is overdue until Canary reads Cboe's Monday close. After a holiday the
  last session's close is on its typed `not_due` schedule during the next
  session, and the row is overdue from that session's close until its own
  close is read. Any delay at Cboe beyond 4 days is overdue too.
- The VIX/VIX3M row is current inside VIX3M's window (09:31 to 15 minutes
  after the close) only with both legs live. At 09:31 the VIX3M leg is the
  previous session's print, which no carry rule accepts, so the row is
  overdue until the first live VIX3M tick. A missed tick inside the window is
  stale for up to 15 minutes while VIX moves less than 1%, then overdue.
  Outside the window the row is `not_due` while Cboe's dated close vouches
  for the gateway leg (agree, official only, or publication pending) and
  overdue when it does not (disagree, or no Cboe close within one session).
  A lost VIX3M subscription (row status error) is overdue at all hours.

In those hours only the vol cluster is defective, so the blank floor of two
clusters is not reached. This is inferred from the fetchers and
`regimeLifecycleInputVerdict`, not tested at those instants: off-hours the
HYG row bands on the official close and stays ok, the cash and funding rows
are ok while FRED answers, USD/JPY trades on Monday evening, and gamma and
breadth are on their own typed schedules. A second outage at the same time
(for example the HYG row stale) reaches the floor and the read is
`data_quality`. Proof pending: the next Monday evening and the next 09:31. With a co-sign at yellow or red the
read is `early_warning` and the rulebook moves to its early-warning bucket,
then back to calm when the row returns: on a day the HYG proxy is below its
average this can happen at 09:31 for the minutes before VIX3M's first tick,
and on Monday evenings until Cboe's close is read. The early-warning bucket is
stricter than calm, so this flip tightens the rulebook and never loosens it.
An estimate from the readings the daemon retained since 2026-10-06 puts a
witness dark beside a co-sign red at about 20% of the time (the stage then
read `data_quality` 12% of the time); it is an estimate from retained
decisions, not a measurement of this rule, and yellow co-signs would add to
it.

### Lifecycle fingerprint v3, and what happens at the install

The lifecycle fingerprint is `lifecycle-fp-v3`. Its fields are unchanged; what
`confirmed` and `suppressed` mean changed.

- Retained decisions are split at the install of this change by date and
  commit, as at the change of 2026-10-05. The vote change has no marker of
  its own on the decision line: the line carries the regime fingerprint,
  which hashes the lifecycle's fields and no lifecycle version. The VVIX
  row's `thresholds_label` in each decision moves from `vvix_daily_v2` to
  `vvix_daily_v3` at the same install, which marks the decisions made under
  the new red line.
- On the first start the alert producer refuses the retained regime snapshot
  once as alert evidence (`evidence_fingerprint_invalid`), until the first
  new publication replaces it. Until then the previous stage and headline can
  be served, for up to about four minutes. In that window the stress read
  applies the new rules to the retained rows while the stored posture is
  served, so "No market-context stress action" can stand beside a stored
  "Confirmed stress regime".
- The streak store keeps each row's last band across a restart. A VVIX red
  entered under the old line of 110 is held while VVIX stays at or above 115,
  though it never reached 120, and releases below 115 like any other.
- The page-hold after a restart is taken from the stored lifecycle even when
  its evidence is refused. This is known and is not changed here.
- Opportunity keeps its rule: no red, and at most one cluster at yellow of
  any role. What counts as red did change: a co-sign red with no witness
  standing is one yellow. In the replay opportunity is 9 sessions against 7
  before. One added session is 2026-11-04, the proxy red on a strong rebound,
  the session before SPY fell 4.4%; the other is 2024-08-13, where VVIX at
  114 is no longer red.

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
