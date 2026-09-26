# Trading Rulebook

Updated: 2026-09-26 10:45 CEST
Status: implemented, advisory, and active as compiled baseline `rulebook-v4` with an owner policy file (amendments 11 and 12, 2026-09-23; reported-limit amendment 13, expiry-runway amendment 14, issuer-concentration amendment 15 and net-exposure amendment 16, 2026-09-26). The
initial 12-rule surface shipped in v1.15.0; the 14-rule contract (15 with amendment 11) folds
in the July 2026 live-market, implementation-review, SQLite-authority, multi-provider
earnings, terminal-evidence, canonical-refresh, and alert-production
amendments described below.
Updated: 2026-07-23 CEST
Status: senior-reviewed 2026-07-07 (verdict: build with amendments — all
amendments folded in below); shipped in v1.15.0. Rule 1's hedge-exemption
semantics were amended post-ship after the first live-market run
(2026-07-07); see the rule-1 semantics note. Rulebook v2 (2026-07-08, dual
senior review against a live confirmed-stress tape): two never-false-pass
fixes (rules 6/8 silent skips), regime-conditional thresholds for rules
3/4/12, rule 2 hedge-premium tier + rule 12 act tier (atomic pair), rule 7
short-put coverage, rule 1 provable lower bounds, new rules 13
(exit_discipline) and 14 (fx_exposure), stock-leg underlying join.

A daily, mechanical 15-rule checklist evaluated daemon-side against the live
book. It is surfaced through CLI, MCP, the Canary SPA, daily-brief deltas,
history, source-neutral alerts, and non-blocking order-preview causes. The
initial heuristic set came from a discretionary-trader review on 2026-07-06;
that provenance and subsequent engineering reviews do not by themselves prove
operator approval of every compiled threshold. The behavioral target is
"hardest trade first", not "most comfortable trade first".

## Why this layer exists

The Rulebook is the desk's **mechanical discipline layer**. It is deliberately
separate from adjacent surfaces:

- Regime and the stress read answer whether market stress exists and whether
  it matters to the held portfolio.
- The risk constitution answers whether capital, drawdown, evidence, and
  reconciliation remain inside the operator-approved policy.
- Protection proposals answer which current positions have executable
  reduce-only candidates.
- The Rulebook answers which repeatable operating mistakes are present now:
  concentration, oversized option premium, cash and carry pressure, catalyst
  mismatch, tape-relative weakness, hedge sizing, exit discipline, and
  structural FX exposure.

That distinction is the reason to keep it. A Rulebook row that merely
duplicates another surface, cannot name its current evidence, or has no
repeatable operator response should be removed or redesigned rather than kept
as another warning. Rulebook verdicts remain advisory evidence; they never
authorize or block a broker write.

## Scope

- Goal: encode rules 1–14 (below) as a versioned model evaluated by the
  daemon; advisory-only. The hardest-first ranking is a property of the
  output, not a rule row. Still out of scope after v2: rule 12(b)
  (regime-aware wing-sell preview cause), carried-forward greeks for
  off-session rule-12 evaluability (deferred with a documented morning
  consequence: hedge classification — rule 2's tier, rule 1/13 exemptions —
  needs a live delta, so those tiers only engage once greeks land intraday;
  pre-market the affected legs conservatively take the stricter non-hedge
  path), an fx_exposure act tier + preview cause (watch-only until the TOML
  policy loader ships), and the TOML loader itself.
- User-facing surfaces: `canary rules [--json]`, `canary rules history`, MCP
  `canary_rules`, the SPA Rules card, daily-brief Rulebook deltas,
  source-neutral alert episodes/inbox delivery, and advisory `rule_*` warnings
  on `canary order preview`.
- Owner layers: the compiled `rulebook-v3` baseline (`internal/risk`) under the
  owner's `rulebook-policy.toml` (amendment 11), earnings and regime
  state (`daemon.db`), manual earnings overrides + feature toggle (runtime
  platform settings), canonical evaluation and alert lifecycle (daemon), and
  rendering (CLI/MCP/app/SPA adapters, no policy duplication).
- Existing behavior: stress signals already cover margin cushion, gross/net
  exposure, and single-name exposure; proposals already run theta_hygiene
  and risk_reduction buckets. The rulebook does not replace these; it
  presents a fixed 15-rule daily contract on top of the same aggregation.

## Which verdict wins when

Three surfaces measure overlapping metrics with different bars, by design:
the **stress read** is regime×portfolio alerting (compiled thresholds, push
alerts), **proposals** are executable protection orders (protection-policy
TOML), and the **rulebook** is an advisory discipline model
(compiled baseline `rulebook-v4` under the owner's `rulebook-policy.toml`). Same
measurements, different questions. Containment so this never drifts into
contradiction:

- One definition of concentration (amendment 15): the stress read's
  concentration row and signals read rule 1's issuer verdict and bands and
  rule 16's delta-swing watch from the Rulebook result, quoting the Rulebook
  watch level in their trim text, and the protection risk-reduction bucket
  trims at rule 1's act level back to its watch level through the same issuer
  netting (`risk.PlanIssuerTrim`). Neither keeps a concentration threshold of
  its own; the retired stress single-name watches (35/35, target 25) and the
  protection `single_name_target_pct_nlv` (25) are gone. Without a Rulebook
  result the stress concentration row is a data-quality watch, never a pass.
- One definition of net exposure (amendment 16): the stress read's exposure
  row, its `net_delta_high` signal and its `net_delta_pct_nlv` figure read
  rule 15's measure and bands from the Rulebook result. The retired stress
  net-delta levels (watch 125, stress act 80, stress urgent 125) are gone;
  confirmed stress moves rule 15's reading one band up instead. Without a
  rule 15 measurement the exposure row is a data-quality watch, never a pass.
- One aggregation: rule evaluation consumes the same
  `PositionsPortfolio`/`PositionGroup`/`UnderlyingExposure` values the stress
  read consumes. Bars may differ; observations may not. (An earlier revision
  claimed a Go test asserting identical observations; none existed, and
  concentration now has a single reader instead.)
- The Rulebook policy in force (baseline `rulebook-v4` version 4, or the owner's
  file by its `policy_id`/`policy_version`) is what the risk-constitution sibling
  pin compares. The sibling pin currently compares ID/version, not the
  Rulebook fingerprint; it detects version drift but is not threshold-level
  approval provenance.
- Every rendered breach shows observed value next to threshold, so two
  surfaces disagreeing on severity still visibly agree on the number.

## Recorded implementation decisions (not threshold approval)

1. Earnings dates: free web fetch (Nasdaq per-symbol endpoint) + manual
   override; unknown renders as `unknown`, never a false pass.
2. Enforcement: advisory + preview causes. No hard blocks in v1.
3. SPA: compact card on the overview + drill-in. No new tab.
4. Hook: read-only `canary orders` allowlisted explicitly (see Agent hook
   boundary).
5. Amendment (2026-07-21): earnings resolution combines Nasdaq with the
   subscription-gated IBKR Wall Street Horizon feed. Provider outcomes remain
   independent; conflicting published dates are `unknown`, and an override is
   still the only operator-authored authority. A non-retryable WSH
   `not_entitled` result at the metadata or event stage is rendered once as an
   account subscription notice: Nasdaq remains active and any name without a
   usable date stays `unknown`, never a pass. No other provider failure is
   treated as an entitlement result.
6. Amendment (2026-07-21): a reviewed exact broker contract may be classified
   `terminal_non_reporting` from typed daemon.db evidence. This is an explicit
   not-applicable/exempt result for rules 6-8, never a pass and never a
   ticker-wide ignore. A published date, manual override, identity mismatch,
   expired review, or malformed authority fails closed as `unknown`.
7. Amendment (2026-07-23): an exact current broker identity observation may
   independently prove that a stock-only holding is a nonissuer security. Its
   typed `not_applicable` result joins reviewed terminal evidence as a trusted
   negative for rules 6-8. It does not exempt option-bearing groups, and an
   ordinary issuer without a usable date remains `unknown` regardless of option
   or size relevance.
8. Amendment (2026-08-03 11:24 CEST, operator decisions): (a) a held security
   type with no issuer earnings by nature — index, future, fund, bond, bill,
   cash, commodity, from the closed canonical vocabulary — is classified
   before any provider poll and yields the `nonissuer_security` exemption for
   rules 6-8; equities and unrecognized types never qualify, and option-only
   groups carry no such row and stay on ordinary evidence. (b) An unentitled
   WSH subscription is nonexistent for every decision: the primary vendor's
   definitive no-date or unsupported verdict stands as a disclosed per-name
   note without degrading the earnings source, while the affected name's rows
   stay `unknown` — the row, never the source health, carries the missing-date
   truth. (c) A freshly fetched elapsed date is `no_date_published`, not a
   format change (Nasdaq parser contract v5). Alert-side, rulebook coverage
   became per-rule the same day; see alert-regime-production.md.
9. Amendment (2026-08-10, operator decisions): each rule has a closed mode:
   `off`, `track`, or `alert`. Track rows remain visible but cannot create an
   alert episode; off rows are not evaluated. The default modes are part of
   the compiled policy until a dedicated versioned Rulebook policy loader and
   editor ship. Rule 3 now measures broker-reported available funds against a
   75% NLV reserve. Rule 6 is an earnings-timing fact in track mode. Rule 8 is
   a tracked size proxy until quantified earnings-event loss is implemented.
   Rules 9-11 default off and rule 14 defaults track.
10. Amendment (2026-08-10): a structurally eligible index put receives
    protection treatment only when its short delta can plausibly protect the
    current gross-long book. With no long book, or above the over-hedge
    multiple (default twice, amendment 12) of the widest configured
    protection band, it is directional short exposure. Directional
    exposure follows ordinary concentration, premium, time-value, expiry, and
    loss rules; rule 12 does not size it as protection.

11. Amendment (2026-09-23 08:37 CEST, operator decision): the owner adopted the compiled
    values as the starting limits and asked for limits relative to cash that
    they can change. The Rulebook policy loader ships: `rulebook-policy.toml`
    (`[rulebook].policy_file`) overrides any subset of the compiled baseline,
    now `rulebook-v3`; edits are version-gated, an unreadable or invalid file
    keeps the policy in force, and every result carries `policy_status` and the
    effective `policy`. `canary rules policy set|reset` writes only the keys
    that differ from the baseline and is human-only in agent sessions. Rule 15
    `net_exposure` (the book's signed stock-equivalent exposure with hedges,
    watch 100%, act above 150% of NLV) defaults to track. Rule 2 now counts a
    losing line at the price paid. The budget governor gains `basis =
    "rulebook"`: rule 2's act level per line and rule 3's cash reserve, with no
    brake gate.

12. Amendment (2026-09-23 22:07 CEST, operator decision): the last compiled
    limit becomes a key. `overhedge_multiple` (default 2, between 1 and 10)
    sets both uses of the former fixed 2x: rule 12 acts above that multiple of
    the current regime's band top, and index puts above that multiple of the
    widest band top classify as directional rather than protection. One key,
    not two, keeps the two boundaries moving together. The per-regime
    `cash_sell_only_pct`, which no rule read, is retired: `set` refuses it, a
    file that still carries it loads with the key ignored and a
    `policy_status` note, and any edit removes it. Refusing the whole file
    instead would void every limit the owner set beside a key that changes
    nothing. The baseline stays `rulebook-v3` because its behaviour is
    unchanged; the fingerprint projection moves to `rulebook-fp-v5`.

13. Amendment (2026-09-26, operator decisions): one reported limit per
    verdict. Every two-band rule carries both bands on its row
    (`watch_threshold`, `act_threshold`) from the policy set that produced
    the verdict: the regime set for rules 4 and 12, and the protection tier
    when it drives rule 2. `threshold` is chosen in one place after
    evaluation, never by a rule: an act row reports the act band, and every
    other status (watch, pass, unknown) reports the watch band. Single-limit
    rules (3, 8, 9, 10, 14) keep their one limit in `threshold`; rules 6, 7
    and 11 report none. Every evidence line quotes the row's `threshold`
    exactly as configured (7.5 stays 7.5); unknown rows name the missing
    input and never quote a different band. A contract test drives every
    reachable status of every rule and checks both. This fixed rule 13 at
    act, which reported the 40% watch band while its evidence quoted 60%,
    and the same mismatch on rules 1, 2, 4, 5, 12 and 15.
    Rising two-band rules (1, 2 including the protection tier, 4, 13, 15)
    share one comparison: watch when the reading is at or above the watch
    band, act when it is at or above the act band. Rules 1, 2, 4 and 15
    used `> act` before, so a reading exactly at the act band moves from
    watch to act; nothing else changes. Two rules keep their comparisons
    pending an owner decision, because at-or-above does not name them: rule
    5 counts down (watch inside 14 days, act inside 7, both strict), and
    rule 12 is a range whose edges belong to the range, with act above the
    over-hedge multiple that also bounds protection classification
    (amendment 12). Rule 12's `watch_threshold` is the edge on the observed
    ratio's side: the top when above the range, otherwise the bottom. Rows
    that stop at an input gate before a comparison (positions or account
    unavailable, rule 4's uncomputable time value, rule 12's unmeasured
    exposure) carry no limit, and an off rule clears all three fields. The
    thresholds themselves are unchanged, so the baseline stays `rulebook-v3`
    and the fingerprint projection stays `rulebook-fp-v5`.

14. Amendment (2026-09-26, operator decision): rule 5 triggers when it
    reaches its limits. The runway counts down, so "at or above" from
    amendment 13 becomes "at or within": a long option watches at 14 days
    to expiry or fewer (was fewer than 14) and acts at 7 days or fewer (was
    fewer than 7). Evidence reads "14 days or fewer" and "7 days or fewer",
    quoting the row's threshold, which stays as amendment 13 set it: 7 on an
    act row, 14 otherwise. An option at exactly 14 or exactly 7 days moves
    up one band; nothing else changes. Rule 12 stays as it is: its upper
    edge is also the protection-versus-directional classification boundary
    (amendment 12). The thresholds are unchanged, so the baseline stays
    `rulebook-v3` and the fingerprint projection stays `rulebook-fp-v5`.

15. Amendment (2026-09-26, operator decisions): one definition of
    concentration, one home. Single-name concentration was defined four
    ways in three policies over two measures (rule 1 delta exposure 30/40;
    stress market value and delta watches 35/35 with a 25 target; protection
    `single_name_target_pct_nlv` 25 on group market value). Rule 1 becomes
    the per-issuer concentration cap and the only definition:
    - Issuer = the underlying, joined with the share classes and ADR or
      ordinary lines the owner lists under `issuer_groups`; an ungrouped
      symbol is its own issuer. Per-leg limits are not concentration and
      stay where they are (rule 2's premium cap, rule 13's loss reviews).
    - Measure: the worst-case loss on the issuer across all prices, as % of
      NLV, netting every leg from current marks. Legs are valued on
      intrinsic payoffs at a price grid (zero, every strike, spot, spot ×
      (1 + `takeover_gap_pct`)); payoffs are linear between grid points, so
      the grid finds the true worst price. Long stock loses its value, a
      long option its premium, a short put its strike notional less its
      current liability, a covered call keeps only its premium as credit,
      and a protective put stops the loss at its strike. A book that keeps
      losing as the price rises (short stock, uncovered short calls) is
      sized at the takeover gap (default 100%) and the responsible legs are
      flagged unbounded, with the uncovered share when part is covered. An
      early assignment realizes exactly a short leg's intrinsic value, so the
      netting holds under early assignment. Index or other-underlying options
      sit on their own issuer and give no credit; a long index put's worst
      case is its premium, so rule 1 no longer needs a hedge exemption.
    - Hedge credit: a long option pays off in the grid only when it expires
      after the issuer's next earnings and at least `hedge_min_days` (14)
      out. With the date unknown only the day test applies and the issuer
      notes it. A non-qualifying option contributes its full premium as loss
      and no protective payoff. Offenders label protective long options
      credited or uncredited with the reason.
    - Bands 30/40 through `bandStatus` (at or above). Days to exit = the
      line's shares (share-equivalents |delta| × contracts × multiplier for
      an option-only line, the full contract when delta is missing) over
      `exit_participation_pct` (20%) of the 20-day average volume, the
      longest line of the issuer; above `illiquid_days_to_exit` (3) the
      issuer uses `illiquid_watch_pct`/`illiquid_act_pct` (20/30). Volume
      comes from the daemon's caches (the quote path's 20-day liquidity entry
      or its daily bars without a session still in progress), never a
      synchronous broker read; a miss starts one paced background read and
      keeps the normal bands with a note. Partial data may indict, never
      acquit: a missing price, share count or FX rate leaves the issuer
      unknown, and an unbounded option-only line without a price is a
      disclosed lower bound that may only indict.
    - Rule 8's size test is rule 1's verdict on the name's issuer.
    Three watches follow the Rulebook architecture as numbered rules, so
    every surface, the alert authority and history treat them as rows: rule
    16 `delta_swing` (one issuer's dollar delta ≥ 30% of NLV; evidence says
    what a 10% move costs and names gamma when it moves that by at least
    `greeks_gap_floor_pct_nlv`; this carries rule 1's former delta measure,
    its lower bound and the protection exemption for index puts), rule 17
    `cluster_stress` (owner-declared `clusters` fall `cluster_drop_pct` (30)
    together, netted with rule 1's leg rules; watch at `cluster_watch_pct`
    (15) of NLV; `not_evaluated/no_clusters` until a cluster is declared)
    and rule 18 `loss_budget` (an issuer's worst-case loss ≥
    `budget_watch_pct` (100) of the constitution's effective risk capital;
    unknown naming the missing number, never pass, without it). They never
    act, so they stay out of act counts and the trim and proposal paths.
    Default modes: 16 and 17 track, 18 alert (owner decision the same day).
    The baseline becomes `rulebook-v4` (Version 4) and the fingerprint
    projection `rulebook-fp-v6`. The stress read's concentration row and
    signals read rule 1 and rule 16 (its three single-name thresholds are
    deleted and the stress fingerprint projection becomes
    `stress-policy-fp-v2`), and the protection risk-reduction bucket trims at
    rule 1's act level back to the watch level on this measure; protection's
    `single_name_target_pct_nlv` is retired and a file that still carries it
    loads with the key ignored and named.

16. Amendment (2026-09-26, operator decision): one definition of net
    exposure. The stress read defined it a second time, as the positions
    aggregate's absolute net dollar delta against its own levels (watch 125%
    in any market, act 80% and urgent 125% under confirmed stress), beside
    rule 15's watch 100% and act 150%. Rule 15 becomes the only definition:
    - The stress read keeps no net measure or level. Its `net_delta_pct_nlv`
      figure is rule 15's observed magnitude (a proven lower bound when
      `net_exposure.is_lower_bound` is set, absent when rule 15 did not
      measure the book), and `portfolio.net_exposure` carries rule 15's
      status, bands, side and reason.
    - Regime conditioning keeps its shape on rule 15's two bands: in a calm
      market only rule 15's act band is a stress watch (rebalance); under
      confirmed stress rule 15's watch band acts and its act band is urgent
      (defensive). The calm trigger is the act band because a fully
      invested, unlevered book already sits at rule 15's watch, and rule 15
      tracks by default. What moves: calm watch 125 → 150, stress act 80 →
      100, stress urgent 125 → 150. The separate, lower stress act level is
      dropped rather than kept as a new key; restoring one would be a
      regime band on rule 15 in `rulebook-policy.toml`, an owner decision.
    - Rule 15 unknown or unavailable raises no `net_delta_high` and turns
      the exposure row into a data-quality watch; rule 15 turned off reads
      "not assessed". Gross exposure and gross delta keep their stress
      levels, which no Rulebook rule defines.
    The stress levels never lived in a file, so there is nothing to migrate.
    The stress fingerprint projection becomes `stress-policy-fp-v3`; the
    positions fingerprint keeps the retired edges as hashing constants. The
    Rulebook itself is unchanged: baseline `rulebook-v4`, projection
    `rulebook-fp-v6`.

These decisions govern evidence handling, advisory enforcement, and surface
placement. They do not establish that the operator approved every numerical
threshold in the compiled model; a value in the owner's file is approved by
being written there.

## The 18 rules

Inputs available today unless marked otherwise. "Exposure" for a name =
stock shares×spot + Σ(option delta×100×contracts×spot), from
`UnderlyingExposure`/`PositionGroup` aggregation (base currency); rules 10,
15 and 16 read it. Rule 1 reads the worst-case loss per issuer instead
(amendment 15), from each line's summed stock rows and each option leg's mark,
strike and FX rate.
Rules 4/12 thresholds are regime-conditional: calm / early_warning /
confirmed sets selected by the latched regime lifecycle stage (see the
regime-conditionality notes).

| # | Rule id | Check | Default threshold | Default mode |
|---|---|---|---|---|
| 1 | `single_name_exposure` | worst-case loss per issuer / NLV, every leg netted from current marks (amendment 15) | watch ≥ 30%; act ≥ 40%; illiquid 20% / 30% | alert |
| 2 | `option_line_premium` | each long option position's market value / NLV; protection positions use the protection tier | watch ≥ 5%; act ≥ 10%; protection watch ≥ 15%, act ≥ 25% | track |
| 3 | `cash_sell_only` | broker AvailableFunds / NLV; the stable id is retained for history compatibility | watch < 75% | alert |
| 4 | `extrinsic_budget` | Σ long-option time value / NLV, excluding protection-classified legs | watch ≥ 10 / 7.5 / 5%; act ≥ 15 / 12 / 10% by regime | alert |
| 5 | `expiry_runway` | long option DTE ≤ 14 unless ≥70-delta ITM or protection-classified | watch ≤ 14 DTE; act ≤ 7 DTE | alert |
| 6 | `catalyst_coverage` | OTM long option expiring before the next earnings announcement | expiry < earnings | track |
| 7 | `overwrite_earnings` | short option spanning earnings; short-put assignment notional ≥10% NLV line or ≥20% name escalates | see ET semantics below | alert |
| 8 | `earnings_size_freeze` | name ≤3 US sessions from earnings while its issuer is at or above rule 1's watch level | ≤3 sessions | track |
| 9 | `red_on_green` | stock day change ≤−1.5% while SPY ≥+0.5% | intraday only | off |
| 10 | `winner_trim` | stock day change ≥+4% with exposure ≥15% NLV | intraday only | off |
| 11 | `green_day_action` | account daily P&L >0 while an act-level rule is open | informational | off |
| 12 | `hedge_integrity` | protection-classified short delta / gross long delta | 25–35 / 30–50 / 40–70% by regime (edges inside); act > 2× the top | alert |
| 13 | `exit_discipline` | each long option position's unrealized loss / premium paid; protection-classified legs exempt | watch ≥40%; act ≥60% | alert |
| 14 | `fx_exposure` | Σ non-base-currency NLV / NLV | track ≥60% | track |
| 15 | `net_exposure` | signed Σ exposure of every name, hedges included / NLV; missing delta may indict (lower bound), never acquit | watch ≥ 100%; act ≥ 150% | track |
| 16 | `delta_swing` | one issuer's dollar delta / NLV; protection-classified index short delta exempt; never acts | watch ≥ 30% | track |
| 17 | `cluster_stress` | loss when every issuer of a declared cluster falls 30% together / NLV; never acts | watch ≥ 15% | track |
| 18 | `loss_budget` | one issuer's worst-case loss / effective risk capital; never acts | watch ≥ 100% | alert |

Row status enum: `pass | info | watch | act | unknown | not_evaluated`.
`info` renders neutral; it exists so rule 11 never inflates severity. The
five non-pass states are load-bearing: **no input condition may ever
produce `pass` by absence of data.**

Rules 1–8, 12–13 and 15–18 are portfolio-discipline checks in this advisory
model; 16–18 watch and never act.
Rules 9–10 are optional tape heuristics, rule 11 is an optional behavioral
nudge, and rule 14 is structural tracking. None is an enforced risk-policy
limit.

Semantics notes:

- Ranking (hardest-first; the number 13 was reassigned to exit_discipline
  in v2): estimated exposure impact descending where the rule has a natural
  impact (1, 2, 4, 5, 6, 7, 8, 10, 12, 13 = offending exposure, premium, or
  salvageable premium in base currency); rules 3, 9, 11, 14 rank by
  severity then rule number. Impact definition lives beside each rule in
  the policy file.
- Index-put roles (rules 1, 2, 4, 5, 12, 13): eligible long puts use the
  policy-owned index list (`SPY, SPX, SPXW, QQQ, IWM`). They are protection
  only when the current book gives them plausible gross-long exposure to
  protect. With no gross-long book, or when short delta exceeds the
  over-hedge multiple (`overhedge_multiple`, default 2) of the widest
  configured protection band, they are directional short exposure.
  Directional positions receive no protection exemptions. Missing delta,
  underlying price, or stock-leg mark provenance leaves the role unclassified.
- Rule 16 exempts only the portion of net-short index delta carried by
  protection-classified legs, capped at the name's net-short exposure and
  disclosed in `Exempt`. Any residual, unclassified, or directional short is
  an ordinary delta swing. Rule 1 needs no exemption since amendment 15: a
  long index put can lose only its premium.
- Rules 9/10 evaluate only during the US equity session
  (`marketcal.SessionAt`) and only from existing stock-leg quote enrichment
  (`DayChangePct`) plus one dedicated best-effort SPY snapshot quote per
  evaluation (`spyDayChangePct`, 2.5s budget; correction 2026-07-08 — the
  implementation never read the regime snapshot's SPY tape). **No standing
  market-data subscriptions** (100-slot budget). Option-only names:
  `not_evaluated` with reason `no_stock_leg_tape`. Off-session:
  `not_evaluated`.
- Daily P&L is part of account-source health for rule 11. During the US equity
  regular session, missing, malformed, or stale Daily P&L degrades account
  health and fails portfolio-dependent rows closed. Outside that session,
  absence is typed `not_due`; the other rules continue on complete
  account/position evidence and rule 11 alone is
  `not_evaluated/pnl_unavailable`.
- Rules 6/7/8 report `unknown` when the earnings date is unknown or stale
  (staleness threshold in policy). Provider-flagged estimated dates
  evaluate normally but evidence discloses `estimated`. Manual overrides win
- Rules 6/7/8 report `unknown` when an ordinary issuer's earnings date is
  unknown or stale. That check happens before stock-only, option-side, or size
  relevance, so absence of an option or a small holding cannot manufacture a
  pass. A known-date name that is irrelevant to the individual rule may still
  skip normally. Provider-flagged estimated dates evaluate normally but
  evidence discloses `estimated`. Manual overrides win
  over ordinary provider resolution, but neither an override nor a published
  provider date may silently displace exact-contract terminal authority: that
  disagreement is `conflicting_sources`, with neither a usable date nor a
  terminal exemption. Comparisons are computed in ET with `time_of_day`: AMC
  earnings on an option's expiry day do NOT breach rule 7 (the option dies
  before the gap); BMO earnings count the prior session as last runway day;
  unknown `time_of_day` is conservative (flag, with ambiguity disclosed).
  Rule 8 counts US sessions via `marketcal`, not calendar days.
- A `terminal_non_reporting` input is available only when the held stock's
  positive IBKR ConID, symbol, and `STK` type all match one reviewed authority
  record. The affected name is listed in each relevant row's `Exempt` array;
  when every relevant name is terminal, the row is `not_evaluated` with reason
  `terminal_non_reporting`, never `pass`. Ticker reuse or a different listing
  receives ordinary provider resolution. The typed earnings projection carries
  the SQLite revision, a normalized-record SHA-256 fingerprint, effective and
  verification timestamps, the mandatory review deadline, and a deterministic
  binding of the public symbol to the exact ConID, revision, fingerprint,
  times, and classification. Allowlisted primary-source references remain
  supporting detail, not the alert proof. A stock-only terminal holding is an
  explicit Exempt/not-applicable row. If the same symbol group also contains options,
  the stock record cannot exempt them because the portfolio projection does
  not carry each option's exact underlying ConID; those legs use ordinary
  provider resolution or remain unknown. Other held names in the same snapshot
  remain assessed normally. An expired,
  stale, identity-conflicted, or date-conflicted exact terminal record is
  likewise recognized before option-side or size relevance in rules 6-8, but
  fails closed as `unknown` with no exemption, including for a stock-only name.
- A broker `not_applicable` input follows the same stock-only boundary. It is
  accepted only from an exact current positive ConID/`STK` identity whose
  closed broker classification is `ETF`; arbitrary contract text is never
  interpreted. The append-only typed identity observation and matching state
  revision/fingerprint remain usable while that held identity still matches.
  A temporary exact-contract lookup failure retains the proof and schedules a
  five-minute retry; a definitive `COMMON`, unknown, or identity-mismatch
  result supersedes it. Proof age alone does not invent an expiry. Mixed
  stock-and-option groups use ordinary provider resolution because option
  underlying ConIDs are not present on the portfolio projection.
- Greeks gaps (rules 1, 4, 12): a leg with material notional (≥ policy
  floor, default 1% NLV) missing delta makes that name's rule-1/12 row
  `unknown` naming the leg; rule 4 goes `unknown` when uncomputable
  extrinsic exceeds the same floor. Mirrors the proposal engine's
  `extrinsic_uncomputable` rigor — never a silent skip.
- Every rule row carries: id, title, status, observed, threshold (the limit
  for its status; amendment 13), watch_threshold and act_threshold on
  two-band rules, evidence quoting the threshold, per-name offenders (worst
  first), exempted/unknown legs where relevant, and data-quality notes.
  Offenders of rules 1, 2 and 13 carry their own `status` (act or watch via
  bandStatus against the offender's own bands: an illiquid issuer's, a
  protection line's tier), offenders of the watch-only rules 16-18 read
  watch, and an unmeasured offender reads unknown (2026-09-26).

Rulebook v2 implementation-review findings (2026-07-08, trading-semantics
and Go-implementation lenses; engineering review, not operator policy
approval):

- **Partial data may indict, never acquit.** Rule 16 (rule 1 before
  amendment 15) computes a provable
  per-name minimum when material legs miss delta: known legs are already in
  `ExposureBase`; each delta-less leg contributes a signed interval (long
  call: intrinsic…notional, since delta·S ≥ C ≥ intrinsic; long put:
  −notional…0 — put intrinsic is NOT a bound on |delta·S| and is never
  used; shorts mirrored; missing underlying or FX ⇒ unbounded). A breach is
  asserted only when the interval minimum alone crosses the bar, rendered
  "≥ X%" with `observed_is_lower_bound`; anything short of provable stays
  `unknown`.
- **Regime conditionality (rules 3/4/12).** The daemon latches the regime
  lifecycle stage on every regime snapshot, buckets it (quiet/opportunity →
  calm; early_warning/stabilization → early_warning; confirmed_stress/
  panic → confirmed; data_quality holds the previous latch;
  unrecognized future stages take the MIDDLE bucket, never silently calm)
  and persists it as a versioned daemon.db state document so a restart
  mid-stress cannot reset thresholds to calm. Stage older than
  `regime_stage_max_age_minutes` (default 240) serves as *carried*: the
  rule evaluates under BOTH the carried set and the calm set and reports
  the worse verdict — stale regime data can hold or tighten, never relax,
  in either band direction (a stale "confirmed" hedge band is wider than
  calm and would otherwise acquit). A cold or stale latch kicks one async
  regime refresh (single-flight, 10-minute cooldown, never from the
  preview path). Row notes always disclose which set applied and why.
- **Rule 2 hedge tier + rule 12 act tier are an atomic pair.** Hedge-
  classified legs (`rule12HedgeLeg`) measure against 15/25% of NLV; the
  oversized-hedge act moves to rule 12 at >2× the band top. Shipping one
  without the other leaves an oversized hedge with no act anywhere. The
  rule-12 act evidence states explicitly: the flag is sizing honesty ("a
  directional short wearing a hedge's clothing"), not a directive to get
  long during stress. Unclassifiable legs (no live delta) take the normal
  tier — no relief without classification.
- **Rule 13 exit_discipline** fences long-option losses at −40/−60% of
  premium paid (cost basis = multiplier-inclusive AvgCost × |contracts| ×
  FX — never re-multiplied). Hedge legs are exempt: a decayed hedge's
  problem is lost protection, which rule 12's under-band arm catches as
  deltas shrink, not lost premium. Averaging down resets the basis and
  clears the fence — documented deliberately; the order-preview cause on
  adding to a flagged line is the guard.
  The desk operator explicitly approved these 40% watch / 60% act bands for
  directional-option exit discipline on 2026-08-12. The protection proposal
  engine consumes the 60% act line as an event-driven full-close candidate;
  rule 13 remains the reporting authority for the 40% watch state. Hedge-
  classified legs remain exempt from both the Rulebook loss act and the option
  exit proposal.
- **Rule 14 fx_exposure is watch-only in v2.** On a structurally high
  non-base-currency book, a permanent act and
  an every-USD-order preview cause would be pure alarm fatigue — a warning
  with a 100% base rate trains the operator to ignore `rule_*` causes.
  Never-false-pass corroboration: an empty `CurrencyExposure` report only
  passes as "base-only" when the positions snapshot shows no non-base leg
  and no FX sensitivity; otherwise the row is `unknown` (`fx_unavailable`).
- **Rule 15 net_exposure (amendment 11).** Rule 1 bounds one name and
  premium rules bound what can be lost; neither says how far the book moves
  with the market. The net is the signed sum of every name's exposure,
  index protection included, so a large put offsets long single-name delta.
  A fully invested, unlevered stock book sits exactly at the 100% watch
  level; act starts above 150%. Names with missing delta contribute the
  `nameExposureInterval` bounds rule 1 uses; a provable excess reports a
  lower bound and an interval that straddles the levels is `unknown`. Track
  by default, so no user receives a new alert from the release.
- **Rule 2 at the price paid (amendment 11).** A losing long line counts at
  the higher of its cost basis and its value: a fall in value must not free
  room under the per-position limit to buy more of it. A gaining line counts
  at its value, which is what it can still lose.
- **Stock-leg underlying join.** An option leg whose greeks tick carried no
  underlying spot borrows the same-name stock leg's account mark
  (`UnderlyingSource: stock_leg_mark`; quality-gated `Mark > 0 && !Stale`),
  making rules 4/6 evaluable pre-market for stock-backed names. Derived
  spots support OTM-ness and extrinsic; they never classify hedge legs —
  pairing a greeks-tick delta with a different-source spot is exactly the
  apples-and-oranges sizing the join exists to avoid.

## Input health (result-level gate)

`RulesResult.InputHealth` mirrors the stress read's source-health pattern: one
entry per source (account, positions, regime_stage, earnings, tape) with
status/as_of/reason. When positions or account are pending, stale, or absent
— boot races included — every portfolio-dependent row is `unknown` with the
source reason. A cold daemon renders a column of `unknown`, never 14 green
rows. Positions health is bound to the completed portfolio-stream receipt for
the current broker account, not the age of a locally assembled response: an
unprimed, wrong-account, future-dated, or more-than-five-minute-silent stream is
pending/unavailable/stale and cannot clear a Rulebook alert episode. A download
whose end marker arrived before its rows accounted for the stream's gross
position value is still unprimed (see the protocol doc's portfolio receipt). This is
the acceptance criterion for the property test below. Alert recovery also
requires the exact 14 rule IDs with their canonical numbers and exactly those
five health sources. Missing, extra, duplicate, or unknown rows stay
uncovered. On rules 6-8, `not_evaluated` is a trusted negative only when every
exempt symbol has matching current typed authority in the same result:
reviewed terminal evidence, exact broker nonissuer identity, or a disclosed
per-symbol mixture of both. A row reason alone, missing/mismatched authority,
or stale/future/malformed proof retains the prior episode. Off-session tape on
rules 9-10 and no long book on rule 12 remain the other accepted reasons.

## Architecture

```
internal/risk/rulebook.go         rule ids, typed inputs, Evaluate() (pure)
internal/risk/concentration.go    rule 1 issuer netting, trim plan (pure)
internal/risk/concentration_watches.go
                                  rules 16-18 (pure)
internal/daemon/rulebook_concentration.go
                                  stock lines, 20-day volume, risk capital
internal/risk/rulebook_policy.go  RulebookPolicy, regime sets, fingerprint
internal/risk/option_math.go      intrinsic/extrinsic/spread helpers hoisted
                                  from proposal_engine (shared, one copy)
internal/daemon/rulebook.go       input assembly, rules.snapshot handler,
                                  cached-eval provider for preview causes
internal/daemon/rulebook_refresh_scheduler.go
                                  daemon-owned one-minute canonical refresh
internal/daemon/rulebook_regime_stage.go
                                  regime stage bucket/latch/persist/kick
internal/daemon/earnings_cache.go async provider + broker-identity authority
internal/daemon/earnings_wsh.go   typed IBKR WSH adapter + strict event parser
internal/daemon/earnings_terminal.go
                                  exact-contract terminal evidence authority
pkg/ibkr/wsh.go                   serialized read-only WSH wire protocol
internal/rpc/brief.go             MethodRulesSnapshot, RulesResult, RuleRow (consolidated there in v3)
internal/cli/rules.go             `canary rules` renderer
internal/mcp/tools.go             canary_rules tool
internal/app/live/service.go      snapshot.rules sibling section (SSE)
web/app/*                         rules card + drill-in
```

- **Placement:** Stress and Rulebook share pure evaluators but remain separate
  typed results. The daemon now runs the canonical Stress and Rulebook
  cadences; CLI/MCP/app readers reuse those daemon-owned inputs and contracts.
  Rules do not attach to `StressResult`: `rules.snapshot` remains a sibling
  section of the app live snapshot (`Rules *rpc.RulesResult` beside
  `Stress *rpc.StressResult`), riding the existing SSE event.
- The pure Rulebook evaluator is stateless and lives in `internal/risk` with
  table tests. Production evaluation is daemon-owned: a complete canonical
  evaluation runs every minute independently of the app, and interactive
  readers may publish an earlier result through the same single-flight path.
- **Earnings fetches are strictly off the snapshot path.** `rules.snapshot`
  only observes cache state and kicks an async refresher: bounded concurrency
  (≤4), an 8s provider budget, and durable per-provider outcome/backoff state.
  Transport failure alone may retain a last-good date as stale; an explicit
  no-date, unsupported-security, schema change, or provider conflict cannot
  hide behind LKG. The shared `internal/publichttp` request policy deliberately
  suppresses User-Agent for `api.nasdaq.com`, preserving the later working
  compatibility observation. Accept and Accept-Language remain source-specific;
  the fetcher sends no Origin or Referer. The earlier browser-UA experiment was
  superseded by this empty-UA policy. Both provider parsers are strict; any ambiguity becomes a typed
  unknown plus source degradation, never a guessed date. Nasdaq symbol mapping
  is an explicit tested function (IBKR `BRK B` → Nasdaq `BRK.B`). IBKR WSH is
  requested through serialized metadata/event reads and requires the account's
  WSH research entitlement. Matching dates form consensus; differing dates or
  incompatible published session halves remain `conflicting_sources`.
  Nasdaq accepts a date and a semantic no-date only from the same observed
  HTTP 200 envelope: a `data` object with no `data.status`, a top-level numeric
  `status.rCode=200`, and the exact symbol-bound announcement prefix followed by
  exactly one ASCII space. A no-date ends there; a date follows with `Jan 2, 2006`
  or `Jan 02, 2006` — the endpoint zero-pads a scheduled day and leaves a Zacks
  estimate unpadded — round-tripping its own layout exactly and not yet elapsed.
  A present `data.status` is a conflicting authority, not an alternative
  envelope. Semantic unsupported requires explicit `data:null` plus top-level
  numeric `rCode=400`.
  Missing/null/empty announcements, `rCode=404`, and bare non-200 responses are
  typed format or protocol failures.
  Parser contract v4 (2026-08-01) replaced v3's unserved nested-`data.status`
  date envelope, which had made every published date a format change.
- Persistence: daemon.db v4 current state plus immutable v3 provider-outcome and
  exact-contract identity observations. Each symbol stores aggregate resolution,
  per-provider latest attempt/next retry/typed redacted failure/last-good value,
  and the current broker identity attempt with any retained matching nonissuer
  proof. One SQLite transaction appends the provider and identity observations,
  binds the returned identity receipt and next state revision into the candidate
  state, and compare-and-swap commits that state; builder, observation, or CAS
  failure rolls the whole transaction back before memory publication. Accepted
  identity proof projects the state revision/fingerprint, opaque observation
  link, proof time/outcome, and a deterministic symbol-to-proof binding without
  exposing the raw receipt ID or ConID. The provider fresh window is 24h and the
  retained-date TTL is 45d.
  Manual override `features.rulebook.earnings_overrides` (map sym →
  YYYY-MM-DD or YYYY-MM-DDTamc/bmo, `null` clears) wins over fetch;
  platform-settings contract (access/source/reason) applies.
- Terminal evidence is a separate daemon.db v1 state document under the
  earnings authority. `[rulebook].terminal_evidence_file` optionally names a
  private operator JSON file used only as a startup import/update; snapshots
  never read the file and SQLite remains the sole served authority. The file
  must be a regular `0600` file, use the closed schema below, bind positive
  ConID + symbol + `STK`, and contain at least two independent allowlisted
  primary authorities. CIK is optional; when supplied it must be ten digits
  and not all zero, and an SEC filing reference requires a matching CIK. No
  SEC reference is otherwise mandatory. Unknown fields, unsafe URLs, future
  verification, duplicate ConIDs, an older catalog `reviewed_at`, changed catalog content at
  the same review time, or a changed/older record without a newer
  `verified_at` fail daemon startup. `reviewed_at` and `verified_at` may not be
  later than the daemon's validation clock; there is no future-clock grace.
  Each removed ConID creates a daemon-owned tombstone at that import's
  `reviewed_at`. Operator files cannot author or erase tombstones. Reactivating
  that exact ConID requires record evidence whose `verified_at` is strictly
  later than the retained revocation watermark, so neither the exact old file
  nor its old record under a newly bumped wrapper can resurrect authority.
  Tombstones remain in SQLite after legitimate reactivation. Omitting the
  config path retains the committed SQLite revision; importing an empty
  `contracts` array with a newer `reviewed_at` explicitly revokes all active
  records.
  The maximum 366-day interval from `verified_at` to
  `revalidate_after` is evidence-expiry safety, not an alert or trading-policy
  threshold. At the deadline the record remains visible but rules degrade to
  unknown until a newer review advances the SQLite revision.

  ```json
  {
    "version": 1,
    "reviewed_at": "2026-07-21T12:00:00Z",
    "contracts": [{
      "contract": {"con_id": 1001, "symbol": "EXAMPLEQ", "sec_type": "STK"},
      "issuer": "Example Issuer, Inc.",
      "cik": "0000001001",
      "classification": "equity_interests_cancelled",
      "effective_date": "2026-06-01",
      "verified_at": "2026-07-21T12:00:00Z",
      "revalidate_after": "2027-07-21T12:00:00Z",
      "evidence": [
        {"kind": "finra_uniform_practice_advisory", "url": "https://www.finra.org/sites/default/files/example.pdf"},
        {"kind": "sec_filing", "url": "https://www.sec.gov/Archives/edgar/data/1001/example.htm"}
      ]
    }]
  }
  ```
- Every initialize/import/update/revoke revision atomically couples the current
  state document to one immutable typed observation. Its payload records the
  old/new revision, normalized catalog fingerprints and review times, plus
  sorted per-ConID `added`, `updated`, `revoked`, or `reactivated`
  dispositions with record fingerprints and any revocation watermark. It
  contains no issuer, symbol, evidence URL, or operator prose. This authority
  history reconstructs the revision/fingerprint and contract-disposition chain;
  rule-transition events separately identify evaluation changes.
- **Canonical cache and preview causes:** daemon, CLI, app, and preview readers
  reuse a broker-scope-, connector-, and connector-generation-bound result for
  up to 75 seconds. An expired preview read performs a bounded canonical
  evaluation; contention or interruption returns an explicit
  `rulebook_unavailable` advisory instead of silently dropping warnings. When
  a drafted order would worsen a currently breached rule (increase the
  breached metric; reduce/close never warns), it appends
  `DataWarning{Code: "rule_<id>", Severity: <the rule's own watch|act>,
  Scope: "rulebook"}`. The Rulebook as-of time is disclosed in the warning's
  `Impact` prose; `DataWarning` has no separate `rules_as_of` field. No ninth
  severity word; `submit_eligible` is never affected.
- Broker applicability reads independently append closed typed identity-outcome
  observations. A successful exact nonissuer classification links its matching
  state proof to that observation; a retryable exact-contract read may retain
  the prior matching proof, while an issuer result or identity mismatch removes
  it. The local immutable observation payload deliberately contains the exact
  ConID/`STK` binding needed to verify that proof, but never raw broker
  classification or upstream prose. The RPC identity projection, transition
  evidence, and logs expose only the closed outcome plus opaque receipt linkage
  and binding—not the raw receipt ID, ConID, broker classification, or prose.
- **Preview causes:** the preview handler consults a short-TTL cached last
  evaluation (45s, with `rules_as_of` echoed in the warning detail) — never
  a fresh assembly per preview. When the drafted order would worsen a
  currently breached rule (increase the breached metric; reduce/close never
  warns), it appends `DataWarning{Code: "rule_<id>", Severity: <the rule's
  own watch|act>, Scope: "rulebook"}`. No ninth severity word;
  `submit_eligible` is never affected.
- Policy: compiled baseline `rulebook-v3` (Version 3) or the owner's file (every threshold —
  including the three regime sets — is part of `FingerprintKey`, so a
  threshold outside the fingerprint is impossible without failing the
  fingerprint test). The optional operator TOML override
  (`~/.config/ibkr/policies/rulebook-policy.toml`, protection-policy
  manager semantics, 30s reload) is **planned, not shipped** — v1 doc
  described it aspirationally; corrected 2026-07-08. Until it lands,
  threshold changes are code changes.
- Evidence: rule-status transitions append as typed analytical events to the
  daemon's sole live database, `~/.local/state/ibkr/daemon.db`, so threshold
  calibration can use the observations that landed. Transition history is
  best-effort observability, not policy-critical continuity: an append failure
  is logged and can leave a gap without suppressing the current canonical
  result. It therefore cannot prove post-trade adherence or broker causality.
  Every transition payload also carries a sorted,
  deduplicated `terminal_authorities` list for the exact terminal evidence the
  evaluation accepted: contract ConID, authority revision and fingerprint,
  review/verification/revalidation times, and classification only. The field
  is an explicit empty list when none was accepted, and never contains issuer
  or symbol text, CIK, URLs, evidence prose, expired evidence, or conflicting
  authority. The latched regime bucket is a versioned state document in the
  same database. Retired JSON/JSONL paths exist only as one-time cutover
  inputs and isolated test seams.
- Alerts: the daemon maps complete, unfiltered Rulebook snapshots into the
  source-neutral alert authority. Current watch/act rows open or escalate
  episodes; only a current, complete, account/positions-bound negative can
  recover them. The app owns inbox, unread, delivery attempts, receipts, and
  fixed presentation copy; neither side gains broker-write authority. Inside
  the authenticated app, each Rulebook alert may repeat the matching current
  `RuleRow.Evidence`, and a tap visibly marks that exact row. Private evidence
  never enters fixed Web Push copy.
- Evidence: rule-status transitions append as typed events to the daemon's
  sole live authority, `~/.local/state/ibkr/daemon.db`, so threshold
  calibration has data. Every transition payload carries sorted, deduplicated
  authority lists for the applicability evidence the evaluation accepted:
  `terminal_authorities` records terminal contract authority revision and
  fingerprint, symbol-to-contract binding, review/verification/revalidation
  times, and classification;
  `identity_authorities` records broker proof revision and fingerprint, opaque
  observation linkage and symbol-to-proof binding, proof time, and closed proof
  outcome. Both fields are explicit empty lists when nothing of that class was
  accepted. Neither list contains issuer or symbol text, CIK, source URLs,
  evidence prose, raw broker classification, expired evidence, or conflicting
  authority. The latched
  regime bucket is a versioned state document in the same database. Retired
  JSON/JSONL paths exist only as one-time cutover inputs and isolated test seams.
- Settings: `features.rulebook.enabled` (default true, runtime) gates
  canonical evaluation, alert production, the SPA card, and preview causes;
  disabled leaves `canary rules` readable with `status: disabled`
  (stock_protection pattern). This is a product feature toggle, not a
  rule-scoped policy exception, threshold approval, or broker-write control.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback |
|---|---|---|---|---|
| Rule thresholds | Rulebook policy in force (baseline or owner file) × latched regime stage for rules 3/4/12 | `RulesResult.PolicyFingerprint` | all | baseline `rulebook-v3` or owner file (`policy_status`); sibling ID/version pin is not fingerprint-level approval; stage carried/never-seen ⇒ worse-of/calm with disclosure |
| Rule verdicts | daemon canonical evaluation + `rules.snapshot` | `RulesResult.Rules []RuleRow` | CLI/MCP/SPA, brief delta, history | per-row `unknown`/`not_evaluated`, result-level InputHealth |
| Earnings dates/applicability | daemon multi-provider earnings resolution ∪ authoritative override ∪ exact-contract SQLite terminal evidence ∪ exact broker identity observations | `RulesResult.Earnings[]` with provider outcomes and typed applicability authority | same | typed `unknown`; conflicts, expired, or mismatched evidence have no usable date or exemption; stale LKG flagged |
| Preview causes | daemon preview handler (scope-bound canonical result ≤75s) | `Warnings[].Code = rule_*`, `Scope = rulebook`; as-of in `Impact` | order preview surfaces | explicit unavailable advisory when canonical read cannot complete |
| Alert episodes | daemon source-neutral alert authority | complete `RulesResult` + typed episode/occurrence contracts | Alerts inbox and Web Push | stale/incomplete evidence cannot clear an episode |
| Feature toggle + overrides | platform settings (runtime) | `features.rulebook.*` | settings surfaces + SPA | defaults on |

## SPA (authority-matrix row)

| UI concept | Label | Source | Snapshot path | Fixture/test | Stale/error | QA gate |
|---|---|---|---|---|---|---|
| Rules card | "Rules" | live snapshot sibling section | `snapshot.rules` | browser_script_ids_test + app-browser-smoke | worst 2–3 breaches as tone pills; `unknown` neutral; InputHealth degradation reachable behind the "Data notes" info affordance; card hidden when disabled | `make app-check` + `make app-refresh-smoke` |

Card: `#canaryRulesCard` beside the stress hero (worst 2–3 breaches as
severity pills, ranked hardest-first) + `#canaryRulesToggle` expanding
`#canaryRulesDetailPanel` with the full 14-row `.detail-grid` (tone classes
`risk|warn|ok|neutral`; `info` and `unknown` render neutral). Each breach
card shows observed against the served limits: watch and act levels on a
two-band rule, the reference threshold otherwise. Money strings arrive daemon-rendered with
real currency (the compat test bans a `"USD"` literal in app.js). Read-only.

The earnings/applicability/entitlement/InputHealth notices are five
independent strings. Concatenating them into one paragraph made them
unreadable, so they render one block each inside `#canaryRulesNotesDialog`,
opened by `#canaryRulesNotesToggle` ("Data notes · N", attention tone while an
unknown-rule note is present). The trigger stays visible whenever any notice
exists — degradation is collapsed, never suppressed.

The `canary*` element ids and `canary-*` CSS class names in this section are
deliberately unchanged by the stress rename: they are DOM and stylesheet
contracts pinned by `browser_script_ids_test` and the compat test, not sensor
naming.

## Safety invariants (unchanged)

- Advisory only: no rulebook state may alter `submit_eligible`, blockers,
  freeze, pins, tokens, or any gated broker-write path.
- Nil means unavailable; unknown earnings ≠ pass; off-session tape rules are
  `not_evaluated`; absent inputs are `unknown`. Never false pass.
- `as_of`, InputHealth, and policy fingerprint ride every result.
- MCP description states when to invoke (daily review, "what should I fix
  today") and when not (`canary_brief` for the full desk summary,
  `canary_proposals` for executable protection candidates).

## Agent hook boundary

The broker hook explicitly allowlists Rulebook and other read-only
investigations while keeping broker writes, settings writes, and destructive
maintenance on their separate gated paths. The table-driven
`hooks/canary-pre-tool-use_test.sh` is wired into `make check` and covers both
false-block and false-allow directions. Hook deployment/version history is not
part of the Rulebook semantic contract.

## Verification

- **Never-false-pass property test** (acceptance for the safety invariant):
  for every rule, nil each input dimension — positions empty/pending,
  account absent, greeks stripped, earnings unknown, off-session, per-leg
  underlying stripped with healthy positions (the rule-6 live false pass of
  2026-07-08), FX report absent, cost bases missing — and assert
  `unknown`/`not_evaluated`, never `pass`.
- v2 unit coverage: regime-set selection incl. carried worse-of in both
  band directions and never-seen disclosure; rule 2 tier split incl.
  unclassifiable fallback; rule 7 short-put notional tiers incl. unknown-FX
  no-quiet-escalation; rule 8 gap propagation (unknown/near/provably-out);
  rule 1 lower-bound direction-awareness (long-put blocks the bound);
  exit-discipline fences + hedge exemption; fingerprint mutation coverage
  for every new policy field.
- Unit: `internal/risk` table tests per rule (pass/info/watch/act/unknown/
  not_evaluated, hedge classification incl. suppression, rule-1 hedge
  exemption incl. residual and no-sizeable-legs cases, ranking, greeks
  floors); hoisted option-math helpers keep proposal-engine tests green.
- Doc drift: `internal/risk` tests pin the rule-1 hedge-exemption wording,
  canonical one-minute/75-second freshness contract, compiled-policy
  authority, rule-14 boundary, and top-level discoverability links. A semantic
  or ownership change therefore fails the hermetic suite until the design and
  navigation move with it.
- Earnings: recorded-fixture parse tests (normal, estimated, malformed,
  missing-date, human-format dates); symbol-normalization tests (BRK.B,
  EUR names → unsupported); ET/DST table tests (BMO Monday after Friday
  expiry, AMC on expiry day, DST boundary week); cache
  load/save/TTL/override-precedence; refresher backoff + failure memory.
- Earnings applicability authority: terminal strict JSON/URL/permission and
  no-future-time validation;
  exact ConID identity including ticker-reuse rejection; SQLite restart
  continuity and revision/fingerprint projection; per-ConID tombstone
  anti-rollback covering exact-old and bumped-wrapper resurrection attempts;
  legitimate newly verified reactivation; atomic immutable authority-change
  observations; provider/date and override conflict; expiry degradation; broker
  identity outcome observations and exact current proof projection; retained
  proof only for the typed retryable exact-contract five-minute retry contract;
  atomic identity observation/state rollback on builder, append, or CAS failure;
  issuer/identity-mismatch invalidation;
  terminal symbol/ConID/revision/fingerprint/time/classification binding;
  pure rules (including stock-only terminal and broker-proven nonissuer names) assert
  exempt or `not_evaluated`, never pass. Alert recovery rejects reason-only,
  wrong-symbol, stale, future, malformed, cross-bound, duplicate-receipt, or
  duplicate-terminal-ConID applicability authority and accepts only valid
  terminal, broker, or per-symbol mixed evidence.
- Aggregation-consistency drift test: stress concentration and rule 1 read
  identical exposure values (review finding 6a).
- Portfolio receipt gate: current completed empty snapshot is a trustworthy
  negative; stale silence and account changes stay uncovered and retain active
  Rulebook episodes; a later current scoped negative may recover them.
- Preview causes: worsen logic incl. reduce/close exemption; TTL staleness;
  severity vocabulary unchanged (no new words).
- Contract: CLI JSON preservation test; generic CLI↔MCP command parity;
  Rulebook MCP provenance-description test; SQLite Rulebook-history
  round-trip; `make docs-regen` for generated MCP and public HTML references.
- SPA: browser_script_ids_test additions; app-browser-smoke exercises card,
  drill-in, and InputHealth-degraded rendering.
- Live gate: full `make smoke` + before/after artifacts per the
  daemon-cli-trading-contract template: `canary status --json`, `canary rules
  --json` against the live book, `canary order preview … --json` showing a
  `rule_*` warning, SPA rendered-flow screenshot.

## Rollback

- Revert files above; runtime state added: daemon.db earnings/settings/latch
  documents (including terminal evidence and retained per-ConID revocation
  tombstones), immutable earnings/provider/identity and terminal-authority
  change observations, and rule-transition events with `terminal_authorities`
  and `identity_authorities`. A rollback may ignore the rulebook
  records, but must not delete or replace daemon.db. To revoke terminal evidence
  before a code rollback, import a reviewed empty v1 document and verify its
  advanced SQLite revision; removing the import path alone deliberately retains
  authority.
- User-visible on rollback: the rules card, current `canary rules` result,
  advisory `rule_*` preview warnings, new Rulebook alert production, and new
  brief state deltas disappear. Retained history/evidence is not deleted; no
  trading-path change occurs either way.
