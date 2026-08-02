# Regime input currency: one model for how inputs report whether they are current

**Status:** Implemented 2026-07-31; policy marker `regime-currency-v1`
**Created:** 2026-07-31 18:44 CEST
**Owner:** osauer
**Related:** `internal-docs/design/regime-calibration.md` (Part 3 is the model this
replaces), `docs/docs/internals/regime-dashboard.md`,
`docs/docs/understand/sensors.md`, `internal/rpc/lifecycle.go`,
`internal/rpc/regime_policy.go`, `internal/daemon/regime_indicators.go`,
`internal/daemon/gamma_session.go`, `internal/daemon/gamma_quality.go`,
`internal/daemon/status_quality.go`, `.agents/docs/risk-policy-contract.md`

## Why this exists

Four defects flip the market state to `data_quality` the same way. They are not
four bugs; they are one model gap seen four times.

1. One impaired cluster blanks the whole market state
   (`regimeLifecycleHasWeakSourceRows`, `internal/rpc/lifecycle.go:1021`).
2. The dashboard reports degraded inputs every trading morning, through two
   independent paths — gamma cadence (`gammaOperationalCadence`,
   `internal/daemon/gamma_session.go:58`) and gamma rankability
   (`gammaQualityActiveSessionFreshnessGate`, `internal/daemon/gamma_quality.go:140`
   → `DegradedClusters` at `internal/daemon/status_quality.go:413` →
   `regimeLifecycleHasDegradedInputs`, `internal/rpc/lifecycle.go:858`).
3. A missed VIX3M poll during market hours still blanks the vol cluster
   (`fetchRegimeVIXTerm`, `internal/daemon/regime.go:799`).
4. A thin-index timeout downgrades VIX-led stress from act to watch
   (`regimeLifecycleVIXTapeCurrent`, `internal/rpc/lifecycle.go:1012`).

Both axes are present in all four:

- **Granularity.** Currency is judged per cluster and consumed as fatal for the
  whole read, while the real dependency is finer. Symptom 4 needs one leg;
  symptom 1 needs "is this cluster part of what is being claimed right now".
- **Gradation.** The current/not-current judgement is binary, so "a refresh for
  the current period is genuinely in flight" (symptom 2) and "this value cannot
  have moved enough to matter" (symptom 3) collapse into the same fatal state as
  a dead feed.

## The model

One typed **currency class** per **evidence unit**, and one **consumption
policy** that says what each class may do. Consumers ask about the unit they
actually read. Cluster currency stops being a primitive and becomes a derived
roll-up.

### Layer 1 — evidence units (granularity)

A unit is a measurement a consumer reads, not a row and not a cluster. Wire
tokens are stable; they key journal fields and test fixtures.

| Unit | Cluster | Legs | Read by |
|---|---|---|---|
| `vol.vix_term` | vol | VIX, VIX3M | ratio band; tape co-sign arm 3 |
| `vol.vix_tape` | vol | VIX | VIX day-change: tape co-sign arm 2, panic/independent-stress arm |
| `vol.vvix` | vol | VVIX | VVIX band |
| `credit.hyg_spy` | credit | HYG, SPY | HYG/SPY band; SPY day-change tape arms |
| `credit.hy_oas` | credit | FRED HY/IG | OAS band |
| `funding.cp_tbill` | funding | CP, T-bill | funding band |
| `fx.usdjpy` | fx | USD/JPY | FX band |
| `gamma.zero` | gamma | SPX (+SPY context) | gamma band |
| `breadth.pct50` | breadth | constituent closes | breadth band |

Derived cluster predicates, all expressed over units:

- `current` — every unit `fresh`.
- `usable` — every unit context-or-better (`fresh`, `not_due`, `pending`, `stale`).
- `defective` — any unit `overdue`, or any unit whose evidence is missing or
  untyped.

### Layer 2 — currency classes (gradation)

Five classes, aligned with `docs/docs/understand/sensors.md`. `pending` is the
one new word; it is not a relabel of `not_due` and not a relabel of `stale`.

| Class | Meaning | Confirms | Fast path | Bands / visible | Grade |
|---|---|---|---|---|---|
| `fresh` | current observation under the unit's own cadence | yes | yes | yes | — |
| `not_due` | the unit's publication window is closed, so no newer observation can exist | no | no | yes | none |
| `pending` | the current period's refresh is in flight, evidenced by a typed marker, and now is inside a bounded window anchored to the period start | no | no | yes | none |
| `stale` | a known value older than its window, or a due refresh that failed, inside an explicit tolerance | no | no | yes | degrade |
| `overdue` | a newer observation should exist and no bounded excuse applies; or the evidence is missing, untyped, or past its served max age | no | no | policy-specific | fatal for that unit |

Two bounds apply to every class computation, not as special cases:

- `not_due` and `pending` both require age ≤ the cluster's served
  `RegimeSourceMaxAgeSeconds`. Beyond it the class is `overdue`. This closes the
  off-hours hole where `RegimeClusterExpectedNotDue` never applied
  `MaxAgeSeconds` and a dead subscription serving its last value read healthy
  indefinitely.
- `pending` requires a **typed** in-flight marker plus a **bounded window**.
  Absent either, the class is `overdue`. A bare timer would let a compute that
  never completes read as pending.

### Layer 3 — consumption policy (one place, allowlist-shaped)

```
MayConfirm(class)  == class == fresh          // explicit allowlist
MayFastPath(class) == class == fresh
MayContext(class)  == fresh | not_due | pending | stale
Grade(class)       == fatal(overdue) | degrade(stale) | none(fresh|not_due|pending)
```

`MayConfirm` is an allowlist on purpose. Today `EvaluateRegimeEligibility`
(`internal/rpc/regime_policy.go:169`) rejects `not_due` and `overdue` by name and
lets anything else through, so a class added later would silently become
confirmation-eligible. Inverting it is the guard that makes the vocabulary
extensible without re-auditing authority.

The fatal test moves from "any cluster is not current" to a scoped question:

```
data_quality when
    usable clusters      <  RegimeVerdictFloor (3)          // as today
 or current clusters     <  CURRENCY_FLOOR                  // decision B
 or the stage's own evidence is not fresh

otherwise
    defective/impaired clusters  =>  readiness degraded, confidence capped,
                                     clusters named in governors[] / data_quality
```

"The stage's own evidence is fresh" generalizes
`regimeLifecycleHasIndependentCurrentStress` (`lifecycle.go:312`) from
confirmed_stress/panic to every stage:

| Stage | Its own evidence |
|---|---|
| `panic` / `confirmed_stress` | the confirming clusters, plus the tape arm that fired — today's function, unchanged in substance |
| `early_warning` | the cluster carrying the visible red. A provisional red is by definition non-confirming, but an **overdue** red is not a market warning at all: it stays `data_quality`, as `lifecycle.go:1066-1070` already states |
| `quiet` / `opportunity` / `stabilization` | the absence claim over the current clusters — this is what CURRENCY_FLOOR sizes |

## What each symptom becomes

**1 — one impaired cluster blanks everything.** `regimeLifecycleHasWeakSourceRows`
is replaced by the scoped test above. A dead FX feed with five healthy clusters
gives `readiness: degraded`, `confidence` capped, `fx` named as the defect, and
the stage those five clusters support — including a confirming stress read the
graded escape hatch already allowed, and now including quiet and early_warning.

**2 — degraded inputs every trading morning.** Both paths, one mechanism.

- *Cadence path.* `gammaCadenceClass` gains a `pending` branch: the served result
  is the immediately-prior completed options session, `Envelope.Refreshing` is
  true, and now is inside `[options open, options open + GAMMA_GRACE]`
  (decision A). Outside that window, or without the typed marker, it stays
  `overdue`, so a hung compute still surfaces.
- *Rankability path.* `regimeLifecycleHasDegradedInputs`'s `DegradedClusters`
  branch gets the same shape its `StaleClusters` branch already has: a degraded
  cluster is not fatal when its currency is context-or-better **and** its
  blockers are cadence-only. Cadence-only is decided typed, not by reason text:
  every blocker carries the `freshness:` gate prefix and
  `GammaSignalQuality.Freshness == "session_mismatch"` (descending into
  `ByUnderlying["SPX"]` when the combined node's only blocker is
  `spx_coverage:`). A gamma blocked on OI, model source, or coverage is still
  fatal, exactly as today.

Gamma stays non-confirming for the whole grace window, so the risk of a longer
window is delayed detection of a broken compute, never a false confirmation.

**3 — missed VIX3M poll in market hours.** `carryVIXTermFromLastGood` extends
into the publication window under the operator's move-based gate (decision C):
carry the previous VIX3M print only when it is from the current session's
window, live VIX has moved less than the agreed amount since that observation,
and wall-clock age is inside the ceiling. The row is `status: stale` and the unit
is `stale` — a failed due refresh, named as what it is. It bands, it warns, it
never confirms, it never fast-paths, and it never banks a streak session (the
streak freeze already keys on `class != fresh`). The vol cluster is impaired, not
blank. Fail the gate and there is no carry: the row is `error`/`overdue` and the
cluster is defective, as today.

**4 — thin-index timeout demotes VIX-led stress.** `regimeLifecycleVIXTapeCurrent`
splits along the unit boundary:

- `vol.vix_tape` (VIX leg only) gates `lifecycle.go:331` and the arm-2 co-sign at
  `:449`, both of which read only `VIXChangePct`.
- `vol.vix_term` (both legs) gates the arm-3 co-sign at `:452`, which reads the
  ratio.

Note this differs from the brief's framing, which grouped arms 2 and 3. Arm 3
reads `VIXTermStructure.Ratio`, so it genuinely depends on VIX3M; gating it on
the term unit keeps it exactly as strict as today (a carried VIX3M makes the row
stale, which never satisfies `fresh`) while arm 2 and the panic arm stop being
hostage to the thin index. This is the conservative reading and is consistent
with the standing decision that an in-session carried VIX3M is context-only.

## Invariants and how the model keeps them

| Invariant | Mechanism |
|---|---|
| Nothing non-fresh confirms | `MayConfirm` is an allowlist on `fresh`; `not_due`, `pending`, `stale`, `overdue` all fail it |
| Nothing non-fresh reaches the day-one fast path | `MayFastPath` is the same allowlist; evaluated before depth/streak gates |
| A dead feed still reaches `overdue` | `pending` requires a typed in-flight marker; `not_due`/`pending` both require age ≤ served max age; missing or untyped evidence is `overdue` by default |
| The streak freeze holds | banking already requires `class == fresh`; `pending` and `stale` are not `fresh` |
| A failed refresh is `stale`, never `not_due` | the in-session VIX3M carry is classed `stale` and degrades readiness; `not_due` remains reserved for a closed publication window |
| No invented thresholds | every number in the model is either already decided (`RegimeSourceMaxAgeSeconds`, `RegimeVerdictFloor`) or listed under "Open decisions" |

## Decided policy (operator, 2026-07-31)

| # | Decision | Value | Where it lives |
|---|---|---|---|
| A | Gamma morning grace, anchored to the options open | 30 minutes | `gammaPublicationWindow` |
| B | Clusters tolerated before the read blanks | blank at 2 | `RegimeCurrencyBlankFloor` |
| C | In-session VIX3M carry gate | VIX moved < 1% since the print, ceiling 15 min | `vix3mCarryMaxVIXMovePct`, `vix3mCarryMaxAge` |
| D | Readiness while gamma is `pending` | `ready`, with the pending unit disclosed | `BuildRegimeLifecycle` |

Two notes on how B was applied. The tolerance was decided for *defects*
(overdue or missing evidence); it is applied to the sum of defects and
impairments, because two clusters that cannot supply current evidence leave the
same hole whichever grade got them there — the stricter reading, and closer to
the behaviour it replaces. And the pre-existing floor still applies on top: a
read with fewer than `RegimeVerdictFloor` ranked clusters was, and remains,
`data_quality`.

## Deviations from the brief, and why

**Arm 3 of the tape co-sign keeps cluster scope.** The brief grouped tape arms 2
and 3 as readers of the VIX day-change leg. Arm 3 reads
`VIXTermStructure.Ratio`, so it genuinely depends on VIX3M; scoping it to the
leg would widen a severity co-signature. It keeps the cluster-scoped test, which
is exactly as strict as before and consistent with the standing decision that an
in-session carried VIX3M is context-only. The SPY tape arms keep cluster scope
for the same reason: the model permits per-unit scoping, but narrowing is only
applied where an observed defect asks for it.

**Breadth's publication window moved from `not_due` to `pending`.** It is the
same state gamma reaches at the open — the session's own observation is due and
being computed — and leaving two words for one state would re-create the
fragmentation this work removes. Authority is unchanged: context, never
confirmation, bounded by the same 90-minute window. Wire effect: the breadth row
reads `pending` and its source `refresh_state` reads `pending` during that
window.

**The severity governor keeps the strict cluster test.** `regimeImpairedConfirming
Clusters` still caps severity when a confirming cluster is anything but `fresh`.
Relaxing it for the scheduled classes would have been consistent with the model
but would raise severity in cases nothing complained about; a cap is the safe
direction, so it stays.

## Change list

Contract (`internal/rpc`)

- `regime_currency.go` (new): class constants, `MayConfirm`/`MayContext`/`Grade`,
  unit tokens, `RegimeUnitCurrency(r, unit)`.
- `regime_policy.go`: `EvaluateRegimeEligibility` inverts to a `fresh` allowlist.
- `lifecycle.go`: `regimeLifecycleHasWeakSourceRows` → scoped stage test;
  `regimeLifecycleHasDegradedInputs` degraded-branch exemption;
  `regimeLifecycleVIXTapeCurrent` split; `RegimeClusterExpectedNotDue` folds in
  the max-age bound.
- `rpc.go`: `GammaZeroSPXResult.Refreshing` (mirrors
  `BreadthSPXResult.Refreshing`, `rpc.go:661`).

Daemon (`internal/daemon`)

- `gamma_zero_cache.go` / `gamma_handler.go`: populate `Refreshing` from the
  scope's refresh slot (`slot.refresh != nil && !slot.refresh.isDone()`), the way
  `handlers.go:4152` does for breadth.
- `gamma_session.go`: bounded morning-grace predicate beside
  `gammaOperationalCadence`, mirroring `spx.PublicationPending`.
- `regime_indicators.go`: per-unit classifiers emit the five-class verdict;
  `gammaCadenceClass` gains `pending`; breadth's publication-window case moves
  from `not_due` to `pending` so both slow inputs use one word.
- `regime.go`: in-session VIX3M carry under decision C, classed `stale`.
- `status_quality.go` / `gamma_quality.go`: typed cadence-only blocker helper.
- `regime_decisions.go`: policy-version marker (below).

Docs

- `docs/docs/understand/sensors.md`: add `pending` to the vocabulary table.
- `docs/docs/internals/regime-dashboard.md`: currency classes, per-unit gates,
  what degrades versus what blanks.
- `internal-docs/design/regime-calibration.md`: Part 3 points here.
- `CHANGELOG.md`.

## Policy-version marker

Behaviour changes here alter the daily fingerprint sequence in the decisions
journal, which is the calibration corpus for every threshold still carrying
`pending_backtest`. A backtest must not blend pre- and post-change days.

- `internal/rpc/regime_policy.go` gains `RegimeCurrencyPolicyVersion =
  "regime-currency-v1"`.
- `regimeDecisionLine` gains `currency_policy` and bumps `v` 1 → 2. Events
  without the field are pre-change by construction.
- `docs/docs/internals/regime-dashboard.md` records the marker and its cutover
  date beside the `pending_backtest` promotion criteria.

## Release note (for the next version stub)

The four fixes shipped before this one did not carry changelog entries either;
this text is ready to lift into the stub when the next version is cut.

> **The dashboard stops blanking the market state over one cold input.** A
> single impaired cluster now degrades the read and names itself instead of
> discarding the five healthy ones — including when those five are confirming
> stress. Two impaired clusters, or a defect in the evidence the current read
> rests on, still produce "Market state undefined". Dealer gamma reads
> "recomputing" for up to 30 minutes after the options open while the session's
> first compute is genuinely in flight, instead of reporting degraded inputs
> every trading morning; a compute that hangs or never starts still goes
> overdue. A VIX3M poll missed while the index is publishing carries the
> previous print for up to 15 minutes, and only while VIX has moved under 1%,
> instead of blanking the whole volatility cluster — carried evidence bands and
> stays visible but never confirms. Alert dedupe re-keys once on upgrade, as the
> lifecycle now discloses the input-currency downgrade it applied.

## Verification

Regression tests, one per symptom plus the invariants:

1. five healthy clusters + one overdue cluster → stage preserved, readiness
   degraded, defect named; and the same fixture at quiet and at early_warning.
2. options open + prior-session gamma + `Refreshing` → `pending`, no
   `data_quality`, gamma non-confirming; past the grace deadline → `overdue`;
   `Refreshing` false → `overdue`; gamma blocked on OI → still fatal.
3. in-window VIX3M timeout inside the gate → row `stale`, vol cluster usable,
   band visible, not confirmable, streak frozen; outside the gate → no carry.
4. VIX3M timeout with a live VIX → arm 2 and the panic arm still fire; arm 3 does
   not.
5. invariants: no non-`fresh` class confirms or fast-paths (table-driven over all
   five classes); an unknown/empty class fails closed; a `not_due` value past its
   served max age reads `overdue`; the streak freeze holds.

A decision line written before this cutover carries no `currency_policy` marker
and legitimately differs from a recompute under the current policy. Boot-time
reconciliation demands byte equality only within one policy version;
`TestUpgradeBootReachesReadyOnPriorVersionStores` boots the daemon over such a
line and requires it to reach ready. Bumping `RegimeCurrencyPolicyVersion` adds
a fixture there — see `internal-docs/design/daemon-sqlite-authority.md`.

Gates: `make test` (binding for Go behaviour, includes `check`), then `make check`
before commit; `docs-check` / `docs-html-check` for the `docs/` edits.
`make smoke` applies because the RPC contract gained fields
(`GammaZeroSPXResult.Refreshing`, `RegimeVIXTerm.VIX3MAnchorVIX`) and the daemon
serves two new freshness classes.
