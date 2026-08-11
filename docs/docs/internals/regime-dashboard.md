# Regime dashboard contract

Updated: 2026-08-09

The daemon's Regime engine classifies the broad-market stress lifecycle as `quiet`,
`early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`,
or `data_quality`. It is an evidence-balance read, not a prediction, trading
system, portfolio planner, or investment recommendation.

Use it to answer one question: are several independent market-risk indicators
confirming each other, or is the market still broadly calm?

The daemon's Stress engine may consume this output, but Stress owns account and
portfolio action. A portfolio concentration problem can be real even while the
broad market regime is calm. In v3 these are internal sensors surfaced through
the daily brief, rules, and app rather than standalone CLI or MCP commands.

## Output shape

Each row carries:

- current value;
- band: `green`, `yellow`, `red`, or unranked;
- status: `ok`, `stale`, `computing`, `unavailable`, or `error`;
- source and as-of;
- a short band reason;
- the threshold set used.

The top-level envelope also carries:

- `lifecycle`: scope (`market` for regime), stage, severity, readiness, timing, confidence, evidence,
  confirmed sources, unconfirmed sources, a semantic lifecycle fingerprint, and
  an explicit no-execution statement;
- `source_health`: per-cluster `as_of`, status, age/freshness, confidence, and
  fingerprint-stability semantics;
- `fingerprint`: semantic identity for the classified broad-market state.

Missing, stale, computing, and degraded data must stay visible. A quiet reading
with missing critical inputs is not the same thing as a confirmed calm regime.

## Indicator sources

Historical replays may substitute point-in-time equivalents for these live
sources; the row meaning should stay the same.

| Row | Actual symbols or series | Live source |
| --- | --- | --- |
| VIX/VIX3M | `VIX` and `VIX3M`, Cboe equity-volatility indexes | IBKR index market data in session; outside Cboe's VIX3M publication window the served VIX3M is Cboe's official dated daily close, which also cross-checks the broker leg. Backtests use Cboe official historical CSVs. |
| VVIX | `VVIX`, Cboe's VIX-of-VIX index | Cboe official daily VVIX time series. |
| HYG/SPY | `HYG`, a high-yield corporate bond ETF, and `SPY`, an S&P 500 ETF | IBKR quotes plus HMDS daily bars; SPY 52-week high uses IBKR Misc Stats tick 165 when available, daily-bar fallback otherwise. Backtests use Nasdaq public ETF history. |
| HY OAS | FRED `BAMLH0A0HYM2` for high-yield OAS and `BAMLC0A0CM` for investment-grade corporate OAS | FRED/St. Louis Fed CSVs for ICE BofA option-adjusted spread series. |
| CP 90-day AA financial minus 13-week T-bill | Federal Reserve `RIFSPPFAAD90_N.B` and U.S. Treasury `ROUND_B1_CLOSE_13WK_2`; cached under legacy series keys `RIFSPPFAAD90NB` / `DTB3` for wire compatibility | Federal Reserve Commercial Paper Data Download Program plus U.S. Treasury Daily Treasury Bill Rates. |
| USD/JPY weekly change | `USD.JPY`, routed as IBKR `CASH` on `IDEALPRO` with currency `JPY` | IBKR FX tick plus HMDS midpoint history for the seven-trading-day comparison; Tier 1 historical replay uses FRED `DEXJPUS`. |
| SPX-canonical dealer gamma | `SPX`/`SPXW` index options with `SPY` ETF options as context | IBKR option chains, open interest, option quotes/model-computation ticks, and the daemon's gamma cache. |
| S&P 500 breadth | Current S&P 500 constituent stock tickers; there is no single breadth symbol used live | Local daemon compute from IBKR HMDS constituent daily bars and the generated S&P 500 membership list. |

## Clusters

A cluster is a group of related indicators. The composite regime counts
clusters, not raw rows, so one market theme cannot vote twice.

Within each cluster, the worst ranked row wins: red beats yellow, yellow beats
green. Unavailable, computing, and error rows are unranked.

### Equity volatility

This cluster watches option-market fear. VIX is Cboe's 30-day
implied-volatility index for the S&P 500 and VIX3M is the same measure over
roughly three months, so the ratio asks whether near-term fear is priced above
longer-term fear. VVIX, how volatile VIX itself is expected to be, asks whether
traders are paying up for large volatility moves. When both worsen, equity
stress is usually becoming more urgent.

VIX/VIX3M backwardation is stress-level evidence by itself. An isolated VVIX
red between 110 and 120 is noisier: the VVIX row remains red and visible, but
the equity-volatility cluster counts as yellow unless VVIX is at least 120, VIX
is up at least 20% on the day, SPY is down at least 1% on the day, or another
independent cluster is red.

Isolated red equity-volatility clusters are the main source of repeated false
alarms in the expanded Tier 1 backtest. They are not dropped, because major
stress often starts in volatility before credit, funding, or FX confirms. The
downgrade keeps volatility warnings visible without letting a standalone
vol-of-vol pop dominate the broad-market read.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| VIX/VIX3M | < 0.92 | 0.92-1.00 | > 1.00 |
| VVIX | < 90 | 90-110 | > 110 |

### Credit

The question here is whether corporate credit is weakening before or alongside
stocks. HYG holds high-yield corporate bonds, meaning lower-rated company debt
that behaves more like risk assets than Treasuries; SPY is the stock-market
side of the comparison.

HYG/SPY is the faster market proxy. The Credit spreads row is the slower
official cash-credit read, comparing high-yield and investment-grade corporate bond
spreads, where OAS means the extra yield investors demand over Treasuries after
adjusting for bond options. Credit stress matters because equity rallies are
less sturdy when lenders are already demanding more compensation for risk.

HYG/SPY can still show a red row by itself. For the cluster count, that
single proxy red is treated as a yellow watch only when the official cash
gauge affirmatively disagrees: a recent OAS read (within 5 calendar days)
that is neither red nor widening 0.50 pp or more over 20 observations. Cash
that is red, cash that is widening at that pace, an absent or stale official
read, or another independent red cluster all leave the proxy red standing —
unknown cash evidence never softens a live warning. The row stays visible
either way; it just does not get to call broad stress alone.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| HYG/SPY | HYG healthy | HYG below 50-DMA | HYG weak while SPY is near highs |
| HY OAS | < 4.0 and not widening | 4.0-5.5 or widening > 0.50 pp | > 5.5 or widening > 1.00 pp |

### Funding

Funding tracks stress in short-term money markets. Commercial paper is
short-term company borrowing; T-bills are short-term U.S. Treasury borrowing.
The spread between 90-day AA financial commercial paper and 3-month T-bills is
a simple check on whether financial firms are paying noticeably more than the
government to borrow over a similar short horizon.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| CP 90-day AA financial minus 3-month T-bill | < 25 bp | 25-75 bp | > 75 bp |

### FX carry

USD/JPY stands in for global carry-trade pressure. It is quoted as yen per U.S.
dollar, so a falling rate means the yen is strengthening. When the yen
strengthens quickly, yen-funded carry trades and other leveraged risk positions
can unwind at the same time. That does not predict every selloff, but it is
useful confirmation when other clusters are also deteriorating.

USD/JPY can still show a red row by itself. For the cluster count, an isolated
FX red is treated as a yellow watch until another independent cluster confirms
stress. The stress read may still act on a fast carry unwind when direct SPY/VIX
tape or breadth confirms the move. On official non-trading dates (weekend or
holiday) frozen last-session SPY/VIX prints cannot supply that tape
confirmation. Inside the stress read only the breadth arm can, until live prints
return at the next open, and its direct tape-shock row demotes to observe with
confirm-at-next-open guidance.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| USD/JPY weekly change | yen move < 1% | yen strengthens 1-2% | yen strengthens > 2% |

### Dealer gamma

This row asks whether dealer hedging is more likely to dampen or amplify index
moves. Above zero-gamma, hedging flows are usually more stabilizing.
Below zero-gamma, hedging can chase the market lower or higher and make moves
sharper. Treat this as a regime hint, not a precise tradable level.

SPX/SPXW index options are the canonical production signal for S&P 500 dealer
gamma. SPY's option book trades separately and is used as corroborating context
when fresh and high quality. Missing or throttled SPY does not downgrade an
otherwise fresh, rankable SPX gamma result. SPY-only gamma is a proxy, not the
canonical S&P dealer-gamma row.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| SPX zero-gamma | spot > 2% above zero-gamma | within +/-2% | spot > 2% below zero-gamma |

Gamma is ranked only when `gamma_zero.envelope.result.quality.rankability` is
`rankable`. Non-rankable gamma remains visible in the row/envelope, but it does
not become the active gamma market-structure read:

| Rankability | Meaning |
| --- | --- |
| `rankable` | Fresh and covered enough to treat as the active market-structure signal. Rankable SPX is stable and production-ready even when SPY is unavailable and disclosed as context. |
| `context_only` | Awareness-only market-structure context. |
| `blocked` | Payload exists but a freshness, coverage, OI, model, cache, farm, entitlement, pacing, or partial-chain gate blocks ranking. |
| `unavailable` | No usable OI-weighted gamma payload exists. |

Missing 0DTE is disclosed in the horizon coverage and warning details, but it
does not by itself make an otherwise healthy SPX read context-only when the
1-7DTE and term buckets are present. After the expiring SPXW series closes, the
0DTE bucket can be absent while the broader SPX surface remains usable.

Model-quality gates judge each underlying on its own slice, never pooled:
derived-IV share, top-strike concentration, and median per-expiry skew-fit R².
The skew bars are preferred ≥ 0.75 SPX, ≥ 0.70 SPY, with a hard block below
0.50. A median between the block and preferred bars still ranks, with the
gate's reason disclosing the sub-preferred fit: median R² is
amplitude-relative and tracks intraday smile noise rather than coverage health,
so it is disclosure-worthy but not rank-blocking on its own.

The combined node carries no pooled model gates: its pooled derived-IV share is
leg-count weighted across both chains and its cross-book concentration ratio
matches no per-slice calibration, so gating there would let a
present-but-degraded SPY downgrade a rankable SPX. Pooled numbers stay visible
in `quality.coverage` as diagnostics, and the SPX slice's own verdict reaches
the combined node through the `spx_coverage` gate. One consequence: a SPY slice
ranking inside the disclosed skew window votes in the combined band weighting.

Every successful compute appends an immutable typed gamma-skew observation to
`$XDG_STATE_HOME/ibkr/daemon.db`: per-expiry R² and residual RMS, coverage,
rankability. These retained observations are offline calibration input for the
heuristic bars. Live decisions do not read the corpus, and it is not a
delete-safe cache.

### Breadth

Breadth counts how many S&P 500 stocks are participating. A rally led by many
stocks is healthier than a rally carried by a few mega-caps. Weak breadth near
index highs warns that the headline index may be hiding fragility.

No live IBKR symbol carries this row: the retail feed does not provide the
official S&P breadth series directly. The daemon computes it from S&P 500
member-stock daily bars and caches the post-close result; reads should not
trigger a 500-name fanout.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| S&P 500 breadth | > 55% above 50-DMA | 40-55%, or weakening near highs | < 40%, especially while SPX is near highs |

## How inputs report currency

Every regime input reports one typed currency class, and one policy says what
each class may do. The class is per evidence unit — the measurement a consumer
actually reads — because consumers differ: the tape arms read the VIX
day-change leg, while the term-structure ratio needs the VIX3M leg beside it.
Cluster currency is the worst of its rows, never the primitive.

| Class | Meaning | May confirm | Visible / bands | Cost to the read |
| --- | --- | --- | --- | --- |
| `fresh` | current under the row's own cadence | yes | yes | none |
| `not_due` | the publication window is closed, so no newer observation can exist | no | yes | none |
| `pending` | the current period's refresh is in flight, typed and inside a bounded window anchored to the period start | no | yes | none |
| `stale` | a known value older than its window, or a due refresh that failed, inside an explicit tolerance | no | yes | degrades readiness |
| `overdue` | a newer observation should exist and no bounded excuse applies; also the class for missing or untyped evidence | no | policy per row | data-quality defect |

Confirmation is an allowlist on `fresh`, so a class added later cannot inherit
authority. Both scheduled classes are bounded by the cluster's served
`max_age_seconds`, so a dead subscription still serving its last value reaches
`overdue` rather than reading healthy off-hours.

The two bounded states each have one owner. Dealer gamma is `pending` for 30
minutes from the options open while the session's first compute is in flight
(measured compute ≈ 9 minutes; the window covers a slow open including a
retry), and `overdue` the moment the typed in-flight marker or the window goes
away — so a hung compute still surfaces. S&P 500 breadth is `pending` inside
its 90-minute post-close publication window. A VIX3M poll missed while the
index is publishing carries the previous print as `stale`, only while live VIX
has moved under 1% since that print and for at most 15 minutes: VIX3M is the
slower leg, so a 1% VIX move bounds the printed-ratio distortion near 0.01,
keeping a true 1.02 away from the 1.05 that would read as backwardation.

A defect no longer discards the whole read. One cluster that is defective or
impaired degrades readiness, caps confidence, and is named in
`lifecycle.governors[]` as `readiness_degraded / input_currency`. The read goes
to `data_quality` when two clusters are affected, when a defect cannot be
attributed to one cluster (authority health, a surface-wide degradation), or
when the defect is in the evidence the current stage itself rests on — a
confirming cluster, or the only red carrying an `early_warning`.

## Confirmation eligibility and severity governance

### Eligibility gates

A red row may CONFIRM stress only when its evidence is deep, persistent, and
cadence-fresh. Otherwise it is PROVISIONAL: visible on the row, listed in
`lifecycle.unconfirmed`, able to drive `early_warning`, but it never counts
toward `confirmed_stress`/`panic`, never rescues another cluster from its
isolated-red downgrade, and never reaches `confirmed_by`. This policy exists
because of the 2026-06-12 false positive, where a 7 bps HYG break (one session
old, thin pre-open tick) and a prior-evening gamma cache mutually confirmed
"Broad stress regime / act" against a green tape
(internal-docs/design/regime-calibration.md).

Gates per indicator (heuristic noise floors, pending_backtest like the band
thresholds; values live in `internal/rpc/regime_policy.go`):

| Indicator | Min depth for eligible red | Fast path (eligible day 1) | Min streak (NY trading sessions) | Cadence freshness | Exit hysteresis (leave red) |
| --- | --- | --- | --- | --- | --- |
| VIX/VIX3M | ratio >= 1.00 | ratio >= 1.05 | 2 | same-session tick (off-window the row is `not_due` at best, and only while Cboe's dated close vouches for the VIX3M leg) | ratio < 0.98 |
| VVIX | >= 110 | >= 120 | 2 | latest official daily close (<= 4d) | < 105 |
| HYG/SPY | HYG >= 0.25% below 50DMA | >= 1.0% below | 2 | RTH tick or latest official close (off-hours banding input is the close, never a thin pre/post print; a missing spot tick falls back to the close and marks the row stale) | HYG closes back above 50DMA |
| HY OAS | band is the gate | n/a | 1 | series <= 7d | < 5.25 and widening < 0.85 pp |
| Funding | band is the gate | n/a | 1 | series <= 7d | < 65 bp |
| USD/JPY | band is the gate (speed is depth) | n/a | 1 | live tick while IDEALPRO trades (Sunday 17:15 to Friday 17:00 ET); the weekend and the daily 17:00-17:15 ET changeover are `not_due`, not overdue | yen move < 1.5% |
| Dealer gamma | gamma-weighted SPY/SPX depth >= 0.5% below gamma-zero | depth >= 4.5% below, or a wholly-short profile | 1 | compute within current NY trading date (prior-date cache = `stale`, warns only) | weighted gap > +0.5% |
| Breadth | <= 38% | <= 30% | 2 | last completed session's compute | > 45% |

Dealer gamma's depth averages the two indexes by each one's gross gamma
exposure — the same weighting the combined row's band vote uses, so the index
that decides the band is the index that decides whether the red is deep enough
to count. What is averaged is each index's depth, not its gap: an index whose
dealers are short gamma across the whole modelled range has no crossing, and so
no gap, but that is the most amplifying reading gamma has and it enters the
average at its full weight. An index with no crossing on the long side has no
depth to contribute and is left out.

A session is banked only from evidence that is cadence-fresh under the row's own
schedule. A mixed-vintage pre-open VIX/VIX3M ratio or a closed-venue FX tick
still displays its band and its hysteresis hold, but the persistence counter
freezes rather than spending one of the sessions the gate requires.

### Two independent VIX3M sources

In frozen mode the broker re-sends its last known value on request, and an index
carries no trade timestamp, so off-window arrival time says nothing about a
value's age. A gateway that keeps answering with a stale VIX3M — a lapsed
market-data entitlement, a contract id that no longer resolves — is therefore
indistinguishable from a quiet market on the broker leg alone.

Cboe's published VIX3M daily close is read independently of the broker and
carries a real session date. Outside the publication window it is the served
VIX3M whenever it covers the last completed session, and `vix3m_cross_check`
records what the two sources established:

| Verdict | Meaning | Off-window cadence |
| --- | --- | --- |
| `agree` | Both described the last completed window and matched. | `not_due` |
| `official_only` | The broker produced no VIX3M; the official close is the leg. | `not_due` |
| `pending_publication` | Cboe has not published the last completed window yet (it lands after the session); the broker leg stands in, bounded to one session. | `not_due` |
| `disagree` | Both described the same window and differed beyond the tolerance: the broker leg is not the close it claims to be. | `overdue` |
| `unverified` | No usable official close within one session of the last completed window. | `overdue` |

Only a vouched leg may read `not_due`, because `not_due` exempts a row from
every age bound. The served row carries `vix3m_source`, `vix3m_official`,
`vix3m_official_date`, and — on a disagreement — the broker's own
`vix3m_gateway_last`, so the discrepancy is inspectable rather than asserted. A
disagreement also raises `vix3m_source_disagreement`.

The comparison tolerance is heuristic and operator-owned, like the band
thresholds. In session the gateway remains the source and the check does not
run: Cboe publishes closes, not intraday values.

Eligibility latches for the life of the red streak (a depth wobble back inside
the floor does not flip it); freshness is never latched: any currency other
than `fresh` drops eligibility immediately, disclosed as `data_not_due`,
`data_refresh_pending`, `data_stale`, or `data_overdue`. Streaks count NY trading days; a weekend or holiday
poll keys to the most recent trading day.

### Severity governance

Applied after stage selection and disclosed in `lifecycle.governors[]`:

1. While a confirming cluster's threshold set carries `pending_backtest`,
   heuristic evidence without a fresh tape co-sign (SPY <= -1.5%, VIX +10%, or
   a same-session term inversion) reads one severity rung down:
   `confirmed_stress` -> watch, 3-red `panic` -> act. Pure-tape panic
   (SPY <= -4%/-7%) always reaches urgent.
2. If a confirming cluster's source health is stale/partial/degraded, severity
   caps at watch (evidence-keyed: an unrelated dead feed does not mute a fresh
   confirmation).

Display tone follows governed severity, not just stage: `confirmed_stress`
with `severity: watch` remains an amber/watch headline, preserving red for
act-grade stress and `risk_off` for full risk-off conditions. The condition
label still stays "Confirmed stress regime" so the evidence balance is not
watered down.

### Closed-date tape gating

Every lifecycle term that reads the direct SPY/VIX day-change prints requires
an official trading date (2026-07-19). The daemon stamps `tape_session_state`
(embedded NYSE calendar) on each regime snapshot and journals it as
`tape_session`; the backtest replay stamps the same classification from the
observation clock. On a closed date (weekend or holiday), frozen last-session
prints:

- cannot enter or hold `panic`, `confirmed_stress`, `early_warning`,
  `opportunity`, or `stabilization`;
- cannot co-sign heuristic confirmation (the term-inversion co-sign keeps its
  own status gate);
- cannot claim the pure-tape panic severity exemption.

The tape evidence rows keep the frozen print's magnitude but read
forward-warning / observe / unconfirmed. Cluster-driven terms are untouched, so
real cluster reds still warn and confirm on any date. Weekday
pre/post/overnight prints keep full effect because they are live, and dates
outside embedded calendar coverage leave the state empty so tape terms fail
open. The trading rulebook's regime-stage latch skips closed-date snapshots:
the last trading-date stage governs weekend rule thresholds through the
existing carried worse-of path instead of a frozen-print or cluster-only
weekend stage re-latching fresh.

The day-change numbers themselves are pinned on those same dates. The gateway's
last print and its tick-9 previous-close anchor can each reset independently
while the market is closed. The live Sunday exhibit read SPY +0.00% beside VIX
+12.19% while Friday truly closed SPY −0.99% / VIX +12.19%, a pair no market
ever printed. So on official non-trading dates the daemon computes
`spy_change` / `spy_change_pct` / `vix_change_pct` from the official daily
closes of the last two completed sessions and names the span in
`spy_change_basis` / `vix_change_basis` ("official closes 2026-07-16 →
2026-07-17 (weekend)"). Bars are matched by exact official session date; when
the closes cannot be resolved the change fields are withheld
(`fields_missing: spy_day_change` / `vix_day_change`) rather than backfilled
from drifted snapshots. Price ticks, the VIX/VIX3M ratio, and banding inputs
are unchanged. Only the day-change fields pin.

## Composite logic

The headline label is a single wording table shared by `composite.verdict` and
`posture.label` (`rpc.RegimeHeadline`); CLI, MCP, and SPA render the served
string:

| Cluster state | Regime label |
| --- | --- |
| 0 red and 0-2 yellow | Normal regime |
| 0 red and 3+ yellow | Elevated stress watch |
| provisional (unconfirmed) red only | Watch: one unconfirmed stress signal |
| any eligible red below confirmation | Stress signal present |
| stage `confirmed_stress`/`panic` (2 eligible reds, or 1 + tape) | Confirmed stress regime |
| 3+ eligible red | Broad stress regime |
| all ranked clusters eligible red | Full risk-off conditions |

Raw indicator counts may also appear. Cluster counts are the primary signal
because related rows, such as VIX and VVIX, are not fully independent votes;
`cluster_eligible_red_count` and `cluster_provisional_red_count` split the reds
by confirmation eligibility.

Lifecycle is a second layer over the row and cluster evidence:

| Lifecycle stage | Broad-market meaning |
| --- | --- |
| `quiet` | Enough data is ranked and no material stress or recovery/opportunity evidence is present. |
| `early_warning` | Weak, isolated, provisional, or forward-looking evidence is visible, but eligible independent confirmation is not yet present. |
| `confirmed_stress` | At least two ELIGIBLE stress clusters, or one eligible cluster plus confirming SPY/VIX tape, are active. |
| `panic` | Three or more eligible stress clusters, or tape severe enough (SPY <= -4%/-7%) that the regime should be treated as acute. |
| `stabilization` | Stress evidence is easing, but this is not yet a deployable opportunity by itself. |
| `opportunity` | Constructive tape and low stress evidence are present; this is broad-market context only, not a trade instruction. |
| `data_quality` | Missing, stale, computing, or degraded inputs prevent a confident lifecycle read. |

`readiness` should be `blocked` or degraded when critical source health is
stale, partial, computing, or degraded; the severity governor additionally caps
the demanded response when the CONFIRMING clusters themselves are impaired.

The independence rescue that waives an isolated-red downgrade counts ELIGIBLE
reds only, so two marginal reds can no longer confirm each other.

## Method notes

The live gamma sweep uses the nearest 80 listed strikes per expiry inside the
+/-10% candidate window to keep the IBKR fan-out bounded, especially for
SPX/SPXW. Historical verification must preserve the same bounded selection and
source-quality rules.

Open interest is a required input for OI-weighted dealer GEX, but missing OI is
unknown, never zero. Priced legs without observed OI may still fit the IV/skew
surface, but they must be omitted from OI-weighted GEX and surfaced through
`warning_details` / `data_quality`. SPY option OI can be absent outside regular
U.S. option hours. SPX option OI should normally be stable across session
phases; missing SPX OI is unexpected data-quality evidence even pre-market,
after-hours, overnight, or on closed-session cache reads.

MOVE/rates-vol is outside the live surface until a verified IBKR contract or
licensed official connector exists. Do not proxy it with ETFs or futures.

## Decision events

Every decision-relevant regime snapshot appends one typed event to the daemon's
sole live authority, `$XDG_STATE_HOME/ibkr/daemon.db`: raw values, bands, depth
metrics, streaks, freshness, eligibility, cluster tallies, lifecycle decision,
and governor records. Events dedupe on the snapshot's semantic fingerprint with
an hourly heartbeat. The append-only event corpus powers typed history queries
and makes the `pending_backtest` thresholds calibratable; it is not a separate
file or a delete-safe cache. Each event also carries `currency_policy`, the input-currency policy version it
was decided under (`regime-currency-v1` from 2026-07-31, when the model
replaced four per-symptom freshness rules). Behaviour changes there move the
daily fingerprint sequence, so a backtest partitions on this marker instead of
blending days from either side of a cutover. A threshold set drops
`pending_backtest` only with months of coverage, measured false-alarm/recall
rates against labeled episodes, and a version-label bump documented here. Disable collection via
`canary settings set regime.journal.enabled=false`.

## Calibration evidence

The event history above is the replay input for future calibration. A policy
change may replace `pending_backtest` only when its versioned evidence records
coverage, false-alarm and recall rates, data-quality exclusions, and ordinary
regression tests for the resulting thresholds. Canary no longer exposes a
general-purpose user-facing backtest command.
