# Regime and canary backtest runbook

Updated: 2026-07-25 09:45 CEST

This is the single umbrella for proving and tuning `ibkr regime` and
`ibkr canary`. Keep the work here. Do not add another experiment plan, tuning
plan, or backtest framework unless this runbook says to.

The goal: prove that regime and canary produce useful stress detection without
overfitting to named events.

## Plain definitions

- A panel is a JSONL table: one row per date, using only information that would
  have been known on that date.
- A target label is for scoring only. It must not feed the signal.
- `regime` answers: what is the broad market state?
- `canary` answers: given account, positions, and regime, should the monitor
  stay quiet, watch, act, rebalance, flag opportunity, or block on data quality?
- Portfolio-only stress, including positions-only held-underlying stress,
  belongs to canary. Regime can keep those rows for context, but they are
  out-of-scope for market-regime precision and recall.

## Artifact map

All backtest artifacts live in one of these places:

| Path | Purpose |
| --- | --- |
| `docs/docs/internals/regime-backtest.md` | This runbook: sequencing, gates, stop rules, and current backlog. |
| `docs/docs/internals/regime-dashboard.md` | Product contract for live `ibkr regime`; not a tuning backlog. |
| `scripts/backtest/` | Reproducible data-build and comparison scripts only. |
| `build/backtest/backtest_sources*.jsonl` | Source ledgers: URLs, checksums, gaps, and retrieval status. Built, not checked in. |
| `internal/cli/testdata/regime_pit_panel_sample.jsonl` | Tiny point-in-time sample consumed by `build-regime`. Checked in; a unit test reads it. |
| `internal/cli/testdata/regime_backtest_sample.jsonl` | Tiny compact replay sample consumed by `backtest regime`. Checked in. |
| `internal/cli/testdata/canary_backtest_sample.jsonl` | Tiny canary replay sample with account and position overlays. Checked in. |
| `build/backtest/*.jsonl` | Captured evidence corpora. Built on demand, git-ignored, never checked in. |

Do not hand-edit generated compact regime rows when the point-in-time panel is
the source of truth. Rebuild them.

## Command sequence

Small smoke fixtures:

```bash
ibkr backtest regime --input internal/cli/testdata/regime_backtest_sample.jsonl
ibkr backtest canary --input internal/cli/testdata/canary_backtest_sample.jsonl
```

Capture a fresh tuning and holdout split before tuning anything. The checked-in
sourced corpora were deleted in July 2026: they recorded verdicts from regime
code that has been corrected several times since, so they measured the old bugs
rather than the current engine.

```bash
ibkr backtest capture-opportunity --preset top-movers --include-regime --split tuning \
  --append /tmp/regime_tuning.jsonl
ibkr backtest capture-opportunity --preset top-movers --include-regime --split holdout \
  --holdout-plan <plan-id> --append /tmp/regime_holdout.jsonl
ibkr backtest regime --input /tmp/regime_tuning.jsonl
ibkr backtest canary --input /tmp/regime_tuning.jsonl
```

Keep captures outside the repository. A corpus checked in beside the unit-test
fixtures reads as a fixture and stops being regenerated.

Point-in-time regime builder:

```bash
ibkr backtest build-regime --input internal/cli/testdata/regime_pit_panel_sample.jsonl \
  > /tmp/regime_backtest_rows.jsonl
ibkr backtest regime --input /tmp/regime_backtest_rows.jsonl
```

That is two passes only when starting from raw point-in-time market rows. If the
input is already compact regime JSONL, run `ibkr backtest regime` directly.

Tier 1 expanded panel. Build it, then work from the build output:

```bash
python3 scripts/backtest/build-tier1-regime-panel.py
ibkr backtest build-regime --input build/backtest/regime_pit_panel_tier1.jsonl \
  > build/backtest/regime_backtest_tier1.jsonl
ibkr backtest regime --input build/backtest/regime_backtest_tier1.jsonl
python3 scripts/backtest/compare-tier1-vol-rules.py
```

## Data tiers

### Tier 0: smoke fixtures

- Purpose: keep CLI contracts stable.
- Gate: tiny samples continue to run and render.

### Tier 1: expanded volatility/calm/event panel

- Sources: Cboe VIX/VIX3M/VVIX, Nasdaq ETF OHLC, FRED funding/FX/credit where
  available.
- Build outputs: `build/backtest/regime_pit_panel_tier1.jsonl` and its source
  ledger `build/backtest/backtest_sources_tier1.jsonl`.
- Primary label: 5-session market stress.
- Secondary feature: 20-session drawdown for early-warning analysis.
- Known gap: gamma and breadth are explicitly unavailable.

### Tier 2: confirmation proxy panel

- Purpose: test whether noisy isolated red volatility can be confirmed or
  downgraded without losing major stress events.
- Allowed sources: reproducible public or IBKR/Nasdaq/FRED daily data.
- Candidate proxies: `RSP/SPY`, `IWM/SPY`, `QQQE/QQQ` or `QQQ/SPY`,
  `HYG/LQD`, `HYG/IEF`, `LQD/TLT`, `TLT/IEF`, `SHY/IEF`, and FRED rates/curve
  series.
- Label these as proxies. They are not official S&P 500 breadth, official MOVE,
  or reconstructed gamma.
- `LQD/TLT` is context-only for now because it mixes credit spread, duration,
  and rates effects. Do not use it as an active confirmation input.

### Tier 2 input classification

The Tier 2 inputs are not all product indicators. This is the final
classification for the current pass:

| Series | Classification | Runtime decision |
| --- | --- | --- |
| `RSP/SPY` | Historical proxy for missing breadth/participation. | Do not promote. Live `ibkr regime` already has first-class constituent breadth. |
| `QQQE/QQQ` | Historical proxy for Nasdaq mega-cap concentration/participation. | Do not promote into broad regime. Keep as backtest context unless a separate concentration surface is designed. |
| `IWM/SPY` | Context-only risk appetite/size rotation. | Do not promote; it is not S&P breadth and can fire during benign leadership shifts. |
| `HYG/LQD`, `HYG/IEF` | Historical credit-confirmation proxies. | Do not promote now. Runtime already exposes HYG/SPY and official HY OAS; add another live credit ETF row only if HY OAS proves persistently unavailable. |
| `LQD/TLT` | Context-only. | Excluded from active confirmation because it mixes credit, duration, and rates effects. |
| `TLT/IEF`, `SHY/IEF` | Context-only rates/duration stress proxies. | Do not promote; they are not MOVE/rates vol and produced calm/rally false alarms. |
| FRED rates/curve series | Context or source-data support. | Do not promote as new regime indicators without a separate product definition and point-in-time source gate. |

No Tier 2 ETF proxy is promoted into live `ibkr regime` in this pass. The
production change is narrower: isolated red VVIX is treated as an
unconfirmed equity-volatility warning unless severity or already-visible tape
confirms it. The red row remains visible in `ibkr regime`; only the cluster vote
is downgraded from stress to watch.

## Current findings

### From the sourced corpora

- Curated sourced regime holdout materially improves confirmed-stress precision
  while preserving watch-level visibility: before 67% precision / 100% recall /
  13% false alarms, after 100% precision / 100% recall / 0% false alarms.
- Curated sourced regime tuning improves precision from 68% to 75% and false
  alarms from 32% to 21%; recall falls from 93% to 86%, which is a monitoring
  item but not a collapse.
- Curated canary holdout catches labelled stress at watch level: watch
  precision 73%, recall 100%; act precision 86%, recall 75%, false alarms 6%.
- Canary adds portfolio-specific lift beyond regime alone: sourced holdout
  portfolio-stress recall moves from 75% regime-only to 100% canary, with one
  additional portfolio true positive.
- Canary action/readiness metrics are now first-class: watch recall of later
  confirmed stress is 100% on the sourced holdout, median lead is 1.0 day,
  and action buckets are tracked separately from the stateless canary JSON
  surface.
- Category reporting is split for market-driven, portfolio-driven,
  concentration-driven, margin-driven, options-driven, and data-quality cases;
  held-underlying P&L shock maps to concentration-driven, near-expiry held-option
  delta maps to options-driven, and held-liquidity degradation maps to
  data-quality. Current sourced holdout has no options-driven rows, so that
  slice is instrumented but not yet proven.

### Tier 1

- Tier 1 exposes the broader problem: current "any red cluster" stress signals
  catch stress rows but fire too often in non-stress volatility regimes.
- A pure confirmation rule cuts false alarms but gives up too much recall.
- Therefore the next tuning target is narrow: isolated red volatility.

### Tier 2 proxy pass

- Tier 2 source access is usable for a bounded proxy pass. The current build
  fetched all required Nasdaq ETF histories and recorded checksums. The first
  14 rows have unavailable proxy windows because the 20-session lookback is not
  mature yet.
- Tier 2 stress-label scoring moves the current holdout baseline out of the
  10.8% Tier 1 forward-label noise zone: current "any red cluster" is 34.2%
  precision and 69.1% recall on the 2024+ observable-stress target.
- The best tested Tier 2 confirmation rule improves holdout stress precision to
  45.1% and cuts false alarms from 17.2% to 9.2%, but recall falls to 58.2%.
- This remains a diagnostic proxy result, not a production rule, because it
  relies on ETF proxy groups that are not live regime indicators.

### Promoted severity split

- The promoted runtime-visible severity split improves Tier 2 holdout stress
  precision from 34.2% to 43.9%, keeps recall at 65.5% versus the 69.1%
  current baseline, and cuts false alarms from 17.2% to 10.8%.
- Tier 1 primary holdout, which is intentionally noisy forward-label scoring,
  improves from 10.8% precision / 66.7% recall / 21.5% false alarms to 14.6% /
  66.7% / 15.2%.

## Tuning pass sequence

Run this sequence and stop at the first failed gate:

1. Validate Tier 2 proxy sources and record them in the source ledger.
2. Build a Tier 2 point-in-time panel by extending Tier 1 with confirmation
   proxy features.
3. Split labels into:
   - `watch`: early warning / elevated risk.
   - `stress`: observable market damage or strongly confirmed broad stress.
4. Compare exactly three stress-signal rules:
   - current: any red cluster is stress.
   - confirmation-only: isolated red volatility is not stress.
   - severity split: isolated red volatility is watch unless severity or
     independent confirmation is strong enough.
5. Tune only the severity split, and only on the tuning split.
6. Score holdout once the tuning behavior is stable.

## Promoted rule

The isolated-VVIX downgrade is live policy; the
[regime dashboard contract](regime-dashboard.md#equity-volatility) owns its
wording and thresholds, along with which rows stay visible under it. One
constraint belongs to this runbook rather than to a dashboard row:

- Do not use Tier 2 ETF proxy groups in runtime behavior until they are
  promoted into the live contract.

## Confirmation gates in replay

The live engine applies confirmation eligibility (depth + persistence +
cadence-freshness, see the dashboard contract) before any red may confirm.
Point-in-time panels are independent daily observations, so the replay
evaluates DAY-1 gates: depth and freshness bind, persistence is sessions=1,
and streak-gated indicators confirm only through their fast-path depths.
Bands-only panel rows (no raw values) therefore cannot satisfy depth-gated
fast paths, a known fidelity limit. Expected consequence in the curated
sample: 2023-03-13 (SVB Monday) reads `early_warning` day 0 and confirms with
persistence, while crash days (volmageddon, COVID, the 2024 carry unwind)
still confirm same-day via fast paths and tape co-signs.

Sequence-aware streak replay is the follow-up: typed regime decision events in
the daemon's sole live authority (`$XDG_STATE_HOME/ibkr/daemon.db`, see the
dashboard contract) forward-collect raw values, depths, streaks, eligibility,
and governor decisions per snapshot. That event corpus is the calibration
source for promoting threshold sets out of `pending_backtest`, per versioned
label, with measured false-alarm and recall rates.

## Data gates

Tier 2 data is green only if all are true:

- Every proxy has a reproducible source, retrieval status, and checksum.
- Missing data stays unavailable; no fabricated green/yellow/red values.
- The source ledger names every source gap plainly.
- Gamma remains excluded unless the row carries a trusted point-in-time gamma
  snapshot stamped with method, source, coverage, and timestamp.
- Official S&P 500 breadth and MOVE are excluded unless a clean licensed or
  public source is proven.
- The point-in-time panel can rebuild the compact replay file deterministically.

If these fail, do not tune. Fix data or stop.

## Tuning gates

A tuning change is allowed only if all are true:

- Watch recall remains high on major broad-market stress events.
- Stress precision materially beats the Tier 1 holdout baseline of 10.8%.
- Stress recall does not collapse on holdout.
- Major events are not hidden: Volmageddon, COVID, 2022 bear-market stress,
  yen carry unwind, and tariff shock remain visible at least at watch level.
- Calm/rally controls get quieter or the remaining false alarms are explainable.
- Portfolio-only stress is evaluated by canary, not counted as regime failure.
- Data-quality warnings are separate from stress false positives.

If Tier 2 cannot materially improve precision without destroying recall, stop
tuning and revisit the product definition. Do not add more indicators just to
force convergence.

## Verification gates

Before calling a pass done:

```bash
make check
make smoke
```

After CLI or daemon changes, also install and smoke the actual binary:

```bash
make restart-daemon
ibkr status
ibkr backtest build-regime --input internal/cli/testdata/regime_pit_panel_sample.jsonl \
  > /tmp/ibkr-build-regime-smoke.jsonl
ibkr backtest regime --input /tmp/ibkr-build-regime-smoke.jsonl
```

## Not doing

- No forecast probabilities.
- No learned cluster weights.
- No automated experiment store.
- No combined `backtest loop` command.
- No gamma reconstruction from current or later data.
- No official S&P 500 breadth without point-in-time constituent coverage.
- No MOVE/rates-vol input without a clean source.
