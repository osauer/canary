# Market read

Design record: 2026-11-24. `market.read` (`canary market read --json`,
`canary_market_read`) answers one question during the US cash session: what
is today doing against yesterday? It reads the S&P 500 index against the prior
session's range, SPY volume against the same time of day, VIX against its
line, and the largest companies against their opening gaps, and names a
condition with a tag only once the condition holds. Every level comes from
data; nothing in it names a date. It is descriptive and feeds no decision.

## Inputs

All inputs come through paths the daemon already has. The read adds no
persistent state and opens no standing market-data line.

| Input | Path | Notes |
| --- | --- | --- |
| SPX prior session high, low, close | `market.history` SPX `6M`, daily regular-session bars | Selected by the calendar's prior-session date, never "the last bar". The intraday series includes extended hours on stocks and is not used for this. |
| SPX five-minute bars | `market.history` SPX `1D` | Followed, so the refresh worker keeps it current. |
| SPY five-minute volume | `market.history` SPY `1D`, then the retained series | The index carries no volume; SPY is the named proxy. The selection keeps only the current day, so the baseline reads the retained series, which holds the prior sessions the daemon recorded (up to twenty venue dates). |
| VIX five-minute bars | `market.history` VIX `1D` | |
| SPX, VIX and leader quotes | a brief look through the broker seam (`BriefQuote`), as the regime takes its quotes | Transient: four at a time, two seconds each, each line released after its read. The look returns the last trade or mark, the prior close and the session open (tick 14). |
| Leader prior close and low | the market tape's leader daily series (`6M`) | Chart cache first; through the bounded history request only when the cache has no bar of the prior session. |
| Leader open | the look's session open | Else the first regular bar of an intraday series the chart cache already holds. No history request is made per name for an open. |

Contracts are spelled as the daemon's other readers spell them (SPX, VIX and
SPY as `market.snapshot`'s references; the leaders as the market tape's), so
the read shares their chart series instead of starting new ones.

## Cost

A read is answered from one in-memory result for 30 seconds, and a read in
flight is joined, not repeated, so a console polling it costs the broker one
read per window. In steady state a read makes no historical request: the
intraday and SPX daily series are followed and refreshed by the worker, and
the leaders' daily series are read from the chart cache. A cold or evicted
cache costs at most one daily read per leader and one per index series, at
interactive priority through the two-slot, coalescing request path, which
the historical pacing (60 requests per ten minutes) still governs. Quotes are
twelve brief looks.

## Session

From the official calendar: today's session, `pre` before its open, `open`
during it, `closed` after it; on a day without a session, the last that
traded, `closed`. The prior session is the calendar session before it.

## Bars and holds

A bar counts once it had completed when its series was read (its end at or
before the series' fetch clock); a forming bar is never measured. During the
session, bars whose last fetch is older than 15 minutes are unavailable and
draw no tags.

A level holds after two consecutive completed closes beyond it, so one print
across never flips a tag. Each bar-based tag is evaluated at every completed
bar. Consecutive bars with the same code are one event in `tag_events`
(index, volume and vix, oldest first, `inside_prior_range` included): `from`
is the close of its first bar, `to` the close of the bar that ended it, absent
while the run still stands at the latest bar. A standing tag's `since` is its
event's `from`. The volume tags hold only when their condition was met at the
last two completed bars. The leader tag reads leader states, not bars, so it
has no `since` and says why.

## Volume baseline

A bucket is its offset from its own session's open. Its baseline is the mean
of that bucket over the prior sessions recorded in the retained series; a
bucket needs at least three such sessions. `relative` is today's completed
volume over the sum of the same buckets' baselines, so it means the same at
any time of day; it is absent, with a reason, unless every completed bucket
has a baseline. `baseline_sessions` is the fewest sessions any of today's
buckets rests on. An early close only lacks its late buckets. The baseline is
as deep as the daemon's recording: a series followed for two days has a
two-day baseline and is reported as such.

## Tags

Drawn while the session is open, as of its latest completed bar; once it has
closed, the read keeps the tags and events as they stood at its last bar.
Before the open there are none. All levels are named constants,
returned in `thresholds` with `heuristic: true`.

| Scope | Code | Tone | Condition |
| --- | --- | --- | --- |
| index | `continuation` | positive | holds above the prior high and relative volume ≥ 1.2 |
| index | `breakout_thin_volume` | caution | holds above the prior high and relative volume < 0.8 |
| index | `above_prior_high` | neutral | holds above the prior high, volume between or unavailable |
| index | `bounce_failed` | negative | holds below the prior low |
| index | `inside_prior_range` | neutral | otherwise |
| volume | `rally_fading` | caution | SPX above its close 30 minutes ago, the last 30 minutes of SPY volume < 0.7 × their baseline, and relative volume < 1.0 |
| volume | `selling_on_volume` | negative | SPX below its close 30 minutes ago and the last 30 minutes of SPY volume ≥ 1.3 × their baseline |
| vix | `stress_rising` | negative | holds above 25 and up ≥ 5% on the day |
| vix | `stress_elevated` | caution | holds above 25 otherwise |
| vix | `fear_easing` | positive | holds below 25, down on the day, and either the prior close was at or above 25 or the fall is at least 5%; a calm day drifting lower is not it |
| leaders | `leaders_hold` | positive | at least 3 leaders gapped up and ≥ 70% of them hold |
| leaders | `leaders_fading` | negative | at least 3 gapped up and ≤ 40% hold |

## Leader states

One share line per company of the market tape's fixed basket (GOOGL, not
GOOG). `gap_pct` is the open against the prior close. Under the prior
session's low is `below_prior_low`, ahead of every other state, `no_gap`
included. A gap under 0.25% is `no_gap`. A gap up holds while the price is at
least half the gap above the prior close and is filled at or under the prior
close; between the two a single read, which keeps no earlier state, still
reads it as held. Gaps down mirror this (`holds_gap_down`,
`gap_down_reclaimed`). States are drawn while the session is open from the
current price, and once it has closed from the session's daily bar (its open
and close; a later print is never used), unavailable until that bar is
recorded. Before the open there are none: the opening tick still names the
previous session. `gapped_up`
counts the companies with a state whose gap is up by at least the minimum,
whatever their state; `holding_gap_up` those that hold it.

## Missing values

A value that could not be measured is absent and a `*_unavailable` field says
why; nothing is zero-filled or carried from another session. Under OpenD the
read is not served: OpenD supplies neither index.

## Tests

`internal/daemon/market_read_test.go` drives the pure assembly with synthetic
bars and quotes: the hold rule and the forming bar, each index, volume, VIX
and leader tag, every leader state, a baseline under three sessions, a leader
without an open, the pre-market and weekend states, a closed session keeping
its tags and its leaders' closing states, the tag timeline, `fear_easing`
firing from fear and staying silent on a calm day, stale bars, absent values
with reasons, and the shared 30-second result.
