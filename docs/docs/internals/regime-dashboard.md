# Regime dashboard contract

Updated: 2026-11-22 09:20 CET

The daemon's Regime engine classifies the broad-market stress lifecycle as `quiet`,
`early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`,
or `data_quality`. It is an evidence-balance read, not a prediction, trading
system, portfolio planner, or investment recommendation.

Use it to answer one question: are several independent market-risk indicators
confirming each other, or is the market still broadly calm?

The daemon's Stress engine may consume this output, but Stress owns account and
portfolio action. A portfolio concentration problem can be real even while the
broad market regime is calm. `canary regime [--explain] [--view full|monitor]
[--json]` and `canary stress [--details] [--json]` give the detailed reads,
alongside MCP `canary_regime` and `canary_stress`; `--view monitor` returns the
compact monitor projection. Brief keeps the summary; the app keeps
its existing detailed projections. Regime JSON omits large gamma profile arrays
unless `--profiles` (MCP `include_profiles=true`) is requested. There is no
regime or stress history command and no forced refresh.

## Output shape

Each row carries:

- current value;
- band: `green`, `yellow`, `red`, or unranked;
- status: `ok`, `stale`, `computing`, `unavailable`, or `error`;
- source and as-of;
- a short band reason;
- the threshold set used.

The compact app monitor also carries `gamma_insights[]`, preserving each
underlying's explanation and quality. The daily Brief exposes canonical SPX
under `ready.gamma` (or a degraded, explicitly named SPY proxy). CLI and
`canary_brief` MCP render this daemon-authored contract; app snapshot/bootstrap
and SSE retain the same evidence. A retained app authority labels these as
last-known observations rather than promoting their historical rankability.

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

### Stress witnesses and co-signs

Rows play one of two roles in the count.

A **stress witness** measures stress directly: the equity-volatility cluster,
funding, and the cash credit row (HY OAS). Only a witness starts a warning. A
witness stands when its red is eligible to confirm (see
[Eligibility gates](#eligibility-gates)). A witness is judged by its own row:
an outage of the row beside it in its cluster does not mute it.

When a witness row cannot be read (failed, unavailable or overdue, or a
schedule past its age bound) and a co-sign is yellow or red, the read is
`early_warning`, never a calm stage and not `data_quality`: what cannot be
read is assumed to be the worst. The owner ruled this on 2026-11-18 at 14:47
CET, answering "Early warning" to "When a real stress input is dark and weak
readings are yellow or red, what should Regime say?"; it keeps the operator
decision of 2026-08-11 that unknown cash evidence never softens a live
warning. The warning opener is `witness_unavailable`, the headline is "Watch:
one unconfirmed stress signal", and the stress read names the cause, for
example "volatility term structure unavailable; ETF credit proxy and breadth
weak". In that read every surface names a red co-sign "weak" ("Weak beside a
stress reading that is unavailable"), not context. The trading rulebook moves
to its early-warning thresholds. A witness on a valid typed not-due schedule
is not dark, and a band held on an overdue co-sign row is not a weak reading.
While the warning is open, an outage of a co-sign row does not count toward
the two-cluster floor; two dark witness clusters, or a defect not
attributable to a cluster, still read `data_quality`.

A reading that is not published yet is not dark. VVIX holds its last close
until a newer one is due, at 09:30 New York the morning after the next
session (an assumption that Cboe's file carries a close by then): Monday
evening holds Friday's close, and so does the evening after a holiday. The
VIX/VIX3M row holds the prior VIX3M close for five minutes from 09:31, until
the first VIX3M tick. A VIX3M tick carried for up to 15 minutes while VIX
stays within 1% is still a reading: it degrades readiness and opens no
warning (owner, 2026-11-19 06:19 CET). The warning still shows on real gaps: VVIX when a due
close was not read, VIX/VIX3M from 09:36 without a VIX3M tick, after a
carried tick older than 15 minutes, off the window when Cboe's dated close
does not vouch for the gateway leg, and the cash or funding row when FRED
does not answer. An earlier estimate of about 20% of the time applied to the
cadence rules before these fixes and has not been re-measured.

What still bounds risk: a calm read only rises, and the early-warning
thresholds are stricter than calm. Evidence on its valid schedule counts as
current: a latched term inversion after the close still stands, and a SPY
print dated today is today's tape, live or frozen after the close. A panic
that cannot stand falls to confirmed stress when that stands; no impaired
input moves a stress stage below the confirmed bucket, and a stress stage
that proves nothing reads `data_quality` with the rulebook's last bucket
held. A panic on a current SPY fall of 7% or more is never blanked. The VVIX
publication time is an unverified assumption; if Cboe publishes later, the
warning shows each morning from 09:30 until the file is read. The one
loosening: when a confirmed stress rests on the VIX/VIX3M row and that row
goes dark beside red co-signs (off the window without Cboe's vouching, or
from 09:36 without a VIX3M tick), the read is `early_warning` and the
rulebook uses its early-warning thresholds instead of its confirmed ones
until the row returns.

A **co-sign** strengthens a warning a witness has started: the HYG/SPY proxy,
USD/JPY, dealer gamma and breadth. A co-sign row still turns red by itself and
stays visible, but its cluster counts as yellow until a witness stands, and
co-signs never confirm each other. Each served row says which it is: `role`
(`witness` or `co_sign`) and, when the row is red, `red` (`counts`, `waiting`
or `context`), in `row_standings` on the snapshot and per indicator in the
monitor view. A co-sign alone changes neither the stage,
nor the headline, nor the tone; where a sentence is needed the read says
"Context, not a stress reading on its own" and names it. In a replay of 1,003 sessions to
2026-11-17 the HYG/SPY row was red on 30% of them and SPY's next 20 sessions
were no worse after it than after any other session; a term-structure
inversion was followed by a 5% fall almost three times as often as an average
session.

### Equity volatility

This cluster watches option-market fear. VIX is Cboe's 30-day
implied-volatility index for the S&P 500 and VIX3M is the same measure over
roughly three months, so the ratio asks whether near-term fear is priced above
longer-term fear. VVIX, how volatile VIX itself is expected to be, asks whether
traders are paying up for large volatility moves. When both worsen, equity
stress is usually becoming more urgent.

VIX/VIX3M backwardation is stress-level evidence by itself, and so is a VVIX
red: the row turns red at 120, which is deep enough to be eligible on its
first session, and an eligible witness red stands. A VVIX red held between
115 and 120 after the index has eased keeps standing until it is released
below 115. A witness that was eligible in its window also keeps standing
after the window closes, while no newer observation can exist.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| VIX/VIX3M | < 0.92 | 0.92 to under 1.00 | >= 1.00 |
| VVIX | < 90, or at level but not rising | >= 90 and +3% over 5 sessions | >= 120 |

VVIX's red line is 120 since 2026-11-18 (threshold set `vvix_daily_v3`; it was
110), and a red is held until a close below 115. In a replay of 1,003
sessions the row was red on 6% of them at 120 against 14% at 110, and the
market read `early_warning` on 13% of sessions against 17%. The band is still
a heuristic awaiting forward evidence.

VVIX's yellow is a transition, not a zone (it came with vvix_daily_v2). The index has a
structural floor near 85-90, so the earlier static 90-110 band was amber on
47% of calm days in a ten-year replay; requiring the level plus a 3% rise
over five sessions cut that to 17% while every labeled 2015-2024 stress
onset still lit ahead of its date. That change left the red line where it
was, at 110 then and at 120 since 2026-11-18, and a missing five-session
change holds yellow at level rather than softening the warning.

### Credit

The question here is whether corporate credit is weakening before or alongside
stocks. HYG holds high-yield corporate bonds, meaning lower-rated company debt
that behaves more like risk assets than Treasuries; SPY is the stock-market
side of the comparison.

HYG/SPY is the faster market proxy. The Credit spreads row is the slower
official cash-credit read. Its band reads the high-yield option-adjusted spread
(OAS) only: the extra yield investors demand over Treasuries after adjusting
for bond options. Credit stress matters because equity rallies are
less sturdy when lenders are already demanding more compensation for risk.

The two rows play different roles. The cash row is a stress witness: a red
HY OAS keeps the cluster red and can start a warning, and so does a current
cash row whose HY OAS has widened 0.50 pp or more over 20 observations, the
row's own yellow step. In the replay that widening occurred on 36 sessions in
five runs and was followed by a 5% fall on 44% of them, against 12% of all
sessions; five runs are little evidence, and the rule is to be read with
that. A yellow from the level alone opens nothing. HYG/SPY is a co-sign: it
can still show a red row by itself, but for the cluster count a red from the
proxy alone is a yellow watch until a stress witness stands, whatever the cash
row beside it reads short of red. The proxy can sit below its average for
months while cash spreads stay calm: in the replay it was red on two sessions
in three of 2026 and the cash row was never red. One cause is known: the
50-day average is built from unadjusted closes, so each monthly distribution
steps the price below it. The row stays visible either way; it does not get
to call stress alone.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| HYG/SPY | HYG healthy | HYG below 50-DMA | HYG weak while SPY is near highs |
| HY OAS | < 4.0 and widening < 0.50 pp | 4.0 to under 5.5, or widening 0.50 to under 1.00 pp | >= 5.5, or widening >= 1.00 pp |

The HYG 50-DMA and SPY historical annual-high fallback must reach the last
completed equity session. A live quote cannot freshen an outdated historical
baseline. Missing/stale history is disclosed before confirmation eligibility;
the SPY annual fallback requires at least 252 observations.

### Funding

Funding tracks stress in short-term money markets. Commercial paper is
short-term company borrowing; T-bills are short-term U.S. Treasury borrowing.
The spread between 90-day AA financial commercial paper and 3-month T-bills is
a simple check on whether financial firms are paying noticeably more than the
government to borrow over a similar short horizon.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| CP 90-day AA financial minus 3-month T-bill | < 25 bp, or at level but not widening | >= 25 bp and +10 bp over 5 publications | >= 75 bp |

Funding's yellow is a transition, not a zone (funding_cp_tbill_v2). The
spread's calm level ranged 6-31 bp across rate regimes, so the earlier static
25 bp gate was amber on 26.6% of calm days in an eleven-year replay; requiring
the level plus a 10 bp rise over the last five commercial-paper publications
cut that to 5.3%. No definition, the static band included, showed real onset
lead for the 2020-2024 stress events — funding confirms late by nature — so
the quieter definition costs no warning. Red at 75 bp is unchanged (ten days
in eleven years, eight of them March 2020), and a missing five-publication
change holds yellow at level rather than softening the warning.

The current funding spread pairs the latest commercial-paper publication with
the latest T-bill observation on or before that date, at most three calendar
days earlier. The five-publication change uses the same date join, so a newer
Treasury print cannot silently shift only one endpoint.

The previous and current Treasury months fetch concurrently and merge only
when both succeed. Month selection starts from day one so short months cannot
cause the same month to be fetched twice. Treasury gets a 25-second HTTP timeout
within a 30-second series budget: valid monthly XML responses can take about
20 seconds. CP and cash-credit series retain their 10-second HTTP and 12-second
series budgets.
Parent cancellation and the 45-second overall refresh deadline remain binding;
the allowance changes neither publication freshness nor funding thresholds.

### FX carry

USD/JPY stands in for global carry-trade pressure. It is quoted as yen per U.S.
dollar, so a falling rate means the yen is strengthening. When the yen
strengthens quickly, yen-funded carry trades and other leveraged risk positions
can unwind at the same time. That does not predict every selloff, but it is
useful confirmation when other clusters are also deteriorating.

USD/JPY is a co-sign. It can still show a red row by itself, but for the
cluster count an FX red is a yellow watch until a stress witness stands.
The stress read acts on a fast carry unwind when the FX cluster is red in
that count, which takes a standing stress witness, and direct SPY/VIX
tape or breadth confirms the move. On official non-trading dates (weekend or
holiday) frozen last-session SPY/VIX prints cannot supply that tape
confirmation. Inside the stress read only the breadth arm can, until live prints
return at the next open, and its direct tape-shock row demotes to observe with
confirm-at-next-open guidance.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| USD/JPY weekly change | yen move < 1% | yen strengthens 1-2% | yen strengthens > 2% |

### Dealer gamma

This row models whether hedging would dampen or amplify index moves under the
calls-positive/puts-negative OI assumption. The sign is measured at actual spot;
above/below a crossing is only a geometric distance and can imply the wrong
sign on a non-monotone profile. Treat this as conditional market structure,
not a directional forecast or observed dealer inventory. The row is a co-sign:
a red counts in the cluster tally only while a stress witness stands.

SPX/SPXW index options are the canonical production signal for S&P 500 dealer
gamma. SPY's option book trades separately and is used as corroborating context
when fresh and high quality. Missing or throttled SPY does not downgrade an
otherwise fresh, rankable SPX gamma result. SPY-only gamma is a proxy, not the
canonical S&P dealer-gamma row.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| SPX zero-gamma | positive modeled GEX beyond transition distance | within +/-2% of nearest crossing, or balanced signed GEX | negative modeled GEX beyond transition distance |

The v4 profile includes exact spot and narrow expiry kernels, refines sign
changes, and reports all detected crossings, selecting the nearest. Same-date
SPX and SPXW fits stay separate. Per-index `insight` carries the 0DTE, 1–7 DTE
and term readings, partial coverage, feed, observation time and rankability.
`option_skews` compares bracketed same-expiry 25-delta put and call IV;
`insight.selected_skew` selects a covered 7–60 day expiry nearest 30 days.
Positive values mean richer downside pricing, not bearish positioning. The
[Gamma concepts](../understand/concepts.md#gamma) document assumptions and sources.

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

Successful computes retain immutable typed gamma payload observations in
`$XDG_STATE_HOME/ibkr/daemon.db`, including fit and coverage measurements.
Serving annotates current rankability afterward. The separate ranked skew
calibration stream is inactive under production storage; a populated, ranked
calibration corpus is not established. Existing quality bars remain heuristic.

### Breadth

Breadth counts how many S&P 500 stocks are participating. A rally led by many
stocks is healthier than a rally carried by a few mega-caps. Weak breadth near
index highs warns that the headline index may be hiding fragility. The band
reads the breadth value alone, so the row is also red in a broad sell-off. It
is a co-sign: a red counts in the cluster tally only while a stress witness
stands.

No live IBKR symbol carries this row: the retail feed does not provide the
official S&P breadth series directly. The daemon computes it from S&P 500
member-stock daily bars and caches the post-close result; reads should not
trigger a 500-name fanout.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| S&P 500 breadth | > 55% above 50-DMA | 40-55% | < 40% |

Breadth v3 retains the latest plus 252 preceding closes and fetches enough
calendar history to populate them. `coverage_50`, `coverage_200` and
`coverage_highs_lows` disclose separate denominators against `member_count`,
including in history. Uncovered secondary values are null/absent; measured
zero remains zero. The existing 80% 50-DMA publication requirement is unchanged.

## How inputs report currency

Every regime input reports one typed currency class, and one policy says what
each class may do. The class is per evidence unit — the measurement a consumer
actually reads — because consumers differ: the tape arms read the VIX
day-change leg, while the term-structure ratio needs the VIX3M leg beside it.
Cluster currency is the worst of its rows, never the primitive.

| Class | Meaning | May confirm | Visible / bands | Cost to the read |
| --- | --- | --- | --- | --- |
| `fresh` | current under the row's own cadence | yes | yes | none |
| `not_due` | the publication window is closed, so no newer observation can exist | keeps a confirmation earned while `fresh`; cannot gain one | yes | none |
| `pending` | the current period's refresh is in flight, typed and inside a bounded window anchored to the period start | no | yes | none |
| `stale` | a known value older than its window, or a due refresh that failed, inside an explicit tolerance | no | yes | degrades readiness |
| `overdue` | a newer observation should exist and no bounded excuse applies; also the class for missing or untyped evidence | no | policy per row | data-quality defect |

A red earns confirmation only while `fresh`, and that check is an allowlist,
so a class added later cannot inherit authority. A row that had confirmed
keeps its confirmation while it is `not_due`, for a witness and a co-sign
alike; a co-sign still counts only beside a standing witness. Both scheduled
classes are bounded by the cluster's served `max_age_seconds`, so a dead
subscription still serving its last value reaches `overdue` rather than
reading healthy off-hours. The bounds are four days for volatility, USD/JPY
and breadth, seven for credit and funding, and three for gamma. A row older
than its cluster's bound is not a current rating either, whatever its
schedule says: `canary regime` shows it as recorded, and `current` is false on
its row in `canary regime --json --view monitor`.

Two inputs may read `pending`. Dealer gamma is `pending` for 30
minutes from the options open while the session's first compute is in flight
(measured compute ≈ 9 minutes; the window covers a slow open including a
retry), and `overdue` the moment the typed in-flight marker or the window goes
away — so a hung compute still surfaces. S&P 500 breadth is `pending` inside
its 90-minute post-close publication window. One input may carry a print
forward as `stale`: a VIX3M poll missed while the index is publishing keeps the
previous print, only while live VIX has moved under 1% since that print and for
at most 15 minutes. VIX3M is the slower leg, so a 1% VIX move bounds the
printed-ratio distortion near 0.01, keeping a true 1.02 away from the 1.05 that
makes a red eligible on its first day.

A defect no longer discards the whole read. One cluster that is defective or
impaired degrades readiness, caps confidence, and is named in
`lifecycle.governors[]` as `readiness_degraded / input_currency`. The read goes
to `data_quality` when two clusters are affected, when a defect cannot be
attributed to one cluster (authority health, a surface-wide degradation), or
when the defect is in the evidence the current stage itself rests on — a
confirming cluster, or the only stress witness carrying an `early_warning`.

## Confirmation eligibility and severity governance

### Eligibility gates

A red row may CONFIRM stress only when its evidence is deep, persistent, and
cadence-fresh, and a co-sign's red only while a stress witness stands.
Otherwise it is PROVISIONAL: visible on the row and listed in
`lifecycle.unconfirmed`. A witness's provisional red drives `early_warning`; a
co-sign's opens nothing. With no witness standing it is context and is
repeated in `lifecycle.suppressed`; beside a standing witness it waits on its
own gate and is in `lifecycle.unconfirmed` only. In `data_quality` the
lifecycle repeats every unconfirmed red in `lifecycle.suppressed`, witnesses
included. A provisional red never counts toward `confirmed_stress`, never
corroborates another cluster, and never reaches `confirmed_by`. This policy exists
because of the 2026-06-12 false positive, where a 7 bps HYG break (one session
old, thin pre-open tick) and a prior-evening gamma cache mutually confirmed
"Broad stress regime / act" against a green tape
(internal-docs/design/regime-calibration.md).

Gates per indicator (heuristic noise floors, pending_backtest like the band
thresholds; values live in `internal/rpc/history_index.go`):

| Indicator | Min depth for eligible red | Fast path (eligible day 1) | Min streak (NY trading sessions) | Cadence freshness | Exit hysteresis (leave red) |
| --- | --- | --- | --- | --- | --- |
| VIX/VIX3M | ratio >= 1.00 | ratio >= 1.05 | 2 | same-session tick (off-window the row is `not_due` at best, and only while Cboe's dated close vouches for the VIX3M leg) | ratio < 0.98 |
| VVIX | >= 110 (every red is at 120 or more when it turns red) | >= 120 (every red reaches it on its first session) | 2 | latest official daily close (<= 4d) | < 115 |
| HYG/SPY | HYG >= 0.25% below 50DMA | >= 1.0% below | 2 | RTH tick or latest official close (off-hours banding input is the close, never a thin pre/post print; a missing spot tick falls back to the close and marks the row stale) | HYG closes back above 50DMA |
| HY OAS | band is the gate | n/a | 1 | series <= 7d | < 5.25 and widening < 0.85 pp |
| Funding | band is the gate | n/a | 1 | series <= 7d | < 65 bp |
| USD/JPY | band is the gate (speed is depth) | n/a | 1 | live tick while IDEALPRO trades (Sunday 17:15 to Friday 17:00 ET); the weekend and the daily 17:00-17:15 ET changeover are `not_due`, not overdue | yen move < 1.5% |
| Dealer gamma | gamma-weighted SPY/SPX signed model depth >= 0.5% | short-gamma depth >= 4.5%, or a wholly-short profile | 1 | compute within current NY trading date (before the next open a prior-date result is `not_due` and keeps a confirmation it had earned; from the open until the session's first compute it does not confirm) | signed depth < -0.5% |
| Breadth | <= 38% | <= 30% | 2 | last completed session's compute | > 45% |

Dealer gamma's depth averages the two indexes by each one's gross gamma
exposure — the same weighting the combined row's band vote uses, so the index
that decides the band is the index that decides whether the red is deep enough
to count. Crossing distance takes its sign from local GEX: negative modeled
GEX gives positive stress depth, and positive GEX gives negative depth.
What is averaged is each index's depth, not its geometric gap: an index whose
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
every age bound short of the cluster's served maximum. The served row carries `vix3m_source`, `vix3m_official`,
`vix3m_official_date`, and — on a disagreement — the broker's own
`vix3m_gateway_last`, so the discrepancy is inspectable rather than asserted. A
disagreement also raises `vix3m_source_disagreement`. An `unverified` verdict
raises `vix3m_official_close_pending` when Cboe's file was read but ends more
than one session back, naming the last official close and when the leg became
unverified; that is Cboe's publication delay and needs no local action. It
raises `vix3m_official_close_unavailable` only when no usable file was read.
VVIX raises `vvix_official_close_pending` the same way once a newer close was
due (09:30 New York the morning after the next session) and was not read,
instead of staying silent until the row turns stale after seven days.

The comparison tolerance is heuristic and operator-owned, like the band
thresholds. In session the gateway remains the source and the check does not
run: Cboe publishes closes, not intraday values.

Eligibility latches for the life of the red streak (a depth wobble back inside
the floor does not flip it); freshness is never latched: a currency of
`pending`, `stale` or `overdue` drops eligibility immediately, disclosed as
`data_refresh_pending`, `data_stale`, or `data_overdue`, with the latch still
shown. A row that is `not_due` keeps the eligibility it had earned, because no
newer reading can exist until its next publication; a red that had not
confirmed stays unconfirmed and is disclosed as `data_not_due`. A red held from memory while its input is unavailable carries
`input_unavailable` and the latch it had. When an outage hides a red that had
confirmed, and counting it would read confirmed stress, the stage reads
`data_quality` (governor `stage_blanked`, reason `outage_hides_confirmation`)
rather than a calmer stage, so the Rulebook keeps its last bucket. Streaks
count NY trading days; a weekend or holiday poll keys to the most recent
trading day.

### Severity governance

Applied after stage selection and disclosed in `lifecycle.governors[]`:

1. While a confirming cluster's threshold set carries `pending_backtest`,
   heuristic evidence without a fresh tape co-sign (SPY <= -1.5%, VIX +10%, or
   a same-session term inversion) reads one severity rung down:
   `confirmed_stress` -> watch, however many eligible reds it has. Panic is
   entered only on the tape (SPY <= -4%/-7%); while that SPY print is current
   it is exempt from both caps and reads urgent.
2. If a confirming cluster's source health is stale/partial/degraded, severity
   caps at watch (evidence-keyed: an unrelated dead feed does not mute a fresh
   confirmation).

Display tone follows governed severity, not just stage: `confirmed_stress`
with `severity: watch` remains an amber/watch headline, preserving red for
act-grade stress and `risk_off` for full risk-off conditions. The condition
label still stays "Confirmed stress regime" so the evidence balance is not
watered down. On a calm stage a co-sign's yellow or red does not colour the
read; a stress witness at yellow keeps it on watch.

Three stress witnesses page. When equity volatility, funding and cash credit
are all eligible red and standing, current or on their valid schedules, a
confirmed stress reads act and pages your phone, also on a flat market and
without a tape co-sign. The owner ruled this on 2026-11-18 at 16:56 CET,
answering "Yes, keep the page" to "Three real stress readings red with a
flat market: should that page your phone?"; it restores the page the
witness and co-sign roles had removed. Co-signs do not count toward the
three, and with fewer than three the caps apply as before.

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
string. The first matching row wins. Below three ranked clusters the read
claims no verdict.

| Cluster state, in order | Regime label |
| --- | --- |
| stage `data_quality` | Market state undefined — data incomplete |
| no ranked cluster | No usable signal yet |
| fewer than 3 ranked clusters | Insufficient signal — too few inputs ready |
| every cluster ranked and eligible red | Full risk-off conditions |
| 3+ eligible red | Broad stress regime |
| stage `confirmed_stress` or `panic` | Confirmed stress regime |
| stage `quiet`, `opportunity` or `stabilization` | Normal regime |
| at least one red cluster, eligible or not | Stress signal present |
| one stress witness warning without confirming | Watch: one unconfirmed stress signal |
| more than one | Watch: unconfirmed stress signals |
| 3+ yellow | Elevated stress watch |
| anything else | Normal regime |

Raw indicator counts may also appear. Cluster counts are the primary signal
because related rows, such as VIX and VVIX, are not fully independent votes;
`cluster_eligible_red_count` counts the red clusters that may confirm.
`cluster_provisional_red_count` counts the stress witnesses that warn without
confirming: a witness red held back for depth, persistence or currency,
cash credit widening, or a witness nobody can read beside a co-sign at yellow
or red. A co-sign red with no witness beside it is in
neither; `lifecycle.unconfirmed` names it and `lifecycle.suppressed` marks it
as context. The stress read serves the same split for a reader in
`market.unconfirmed_signals`: each unconfirmed red cluster and the cash
credit widening, with the reader's name of the row behind it and its role
(`witness`, `widening`, `waiting`, `context`). Under a dark-witness warning
it also lists the witness nobody can read (`unavailable`) and every co-sign
at yellow or red (`weak`).

Lifecycle is a second layer over the row and cluster evidence. The stages are
tested in the order below and the first match wins. "Tape" means the day's SPY
and VIX moves, read only on a session that is not a closed date.

| Lifecycle stage | Trigger |
| --- | --- |
| `data_quality` | Fewer than three clusters are ranked, or the input-currency rules in [How inputs report currency](#how-inputs-report-currency) send the read here. |
| `panic` | SPY down 4% or more with at least one eligible red; or SPY down 7% or more. Red clusters without a fall, however many, are `confirmed_stress`. The SPY print is judged by its own date: a print dated today is current, live or frozen after the close; a prior session's print is not. While an input is impaired a panic that cannot stand falls to `confirmed_stress` when that stands. |
| `confirmed_stress` | Two or more eligible red clusters, which takes a standing stress witness plus a co-sign or a second witness; or one eligible red with SPY down 2.5% or more, or with VIX up 20% or more; or SPY down 4% or more with at least two clusters yellow or worse. |
| `early_warning` | An eligible red cluster; a stress witness that is red but provisional or not standing (a first-day term inversion, a witness red on a row that is not current); a current cash credit row with HY OAS 0.50 pp or more wider over 20 observations; three or more clusters yellow or worse of which at least one is a stress witness at yellow; a stress witness nobody can read beside a co-sign at yellow or red; or SPY down 1.5% or more, or VIX up 10% or more. A red or yellow in co-sign clusters alone opens nothing while every witness can be read. |
| `opportunity` | No red, at most one cluster at yellow, at least three ranked clusters, SPY up 1.5% or more and VIX down 10% or more. Every cluster counts, co-signs included, and a co-sign red with no stress witness standing is one yellow. This is broad-market context only, not a trade instruction. |
| `stabilization` | No red, one or two stress witnesses at yellow, and SPY up 1% or more or VIX down 10% or more. Stress is easing, but this is not yet an opportunity. A day whose only yellows are co-signs is `quiet`. |
| `quiet` | None of the above. |

`readiness` should be `blocked` or degraded when critical source health is
stale, partial, computing, or degraded; the severity governor additionally caps
the demanded response when the CONFIRMING clusters themselves are impaired.

An eligible red cluster always includes a standing stress witness, because a
co-sign red counts only beside one. Two co-signs cannot confirm each other.
The tape can confirm stress while every red is a co-sign (SPY down 4% or
more with two clusters yellow or worse); `confirmed_by` then names `tape`.

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
