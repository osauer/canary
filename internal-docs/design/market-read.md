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
| SPY five-minute volume | `market.history` SPY `1D`, then the retained series | The index carries no volume; SPY is the named proxy. The selection keeps only the current day, so the baseline reads the retained series, which holds the prior sessions the daemon recorded (up to twenty venue dates). Each 1D read also keeps the earlier days the broker sends with it (the read pads six days to find its session), so one read seeds about five prior sessions. |
| VIX five-minute bars | `market.history` VIX `1D` | |
| SPX, VIX and leader quotes | a brief look through the broker seam (`BriefQuote`), as the regime takes its quotes | Transient: four at a time, two seconds each, each line released after its read. The look returns the last trade or mark, the prior close and the session open (tick 14). |
| Leader prior close and low | the market tape's leader daily series (`6M`) | Chart cache only; a missing series is read in the background. Without the bar, the prior close falls back to the quote's previous close (`prior_close_source: quote_previous_close`, before and during the session only); the prior low has no stand-in. |
| ES and NQ front futures (state `pre` only) | resolved and quoted as `market.snapshot` does: the broker's front-contract resolution, then one transient quote snapshot of the dated contract, its line released when read | Each look runs beside the others inside the budget; a future that does not answer is `*_unavailable` in `indicated` and the rest still answers. |
| NDX, else QQQ (state `open` only) | two more brief looks through the broker seam, in the same pool | QQQ serves only when the index does not quote, and `source` says so. No historical series is read for either. |
| Leader open | the look's session open | Else the first regular bar of an intraday series the chart cache already holds. No history request is made per name for an open. |
| Held underlyings | the daemon's positions read, picked as `market.snapshot` picks them (stock and option underlyings, at most twelve) | Holdings must be available and current, else `held.rows_unavailable`. |
| Held prior session, today's bars | the name's daily series (`6M`, the same chart series as `market.snapshot`'s `1Y`) and its `1D` series, spelled as `market.snapshot` spells the contract | Chart cache only. A missing daily series is read in the background like a leader's; the `1D` series is never requested by the read: `market.snapshot` follows it for every priced held row. |
| Held price, day high and low | the line the daemon already holds for the name (the shared subscription manager, usually the display feed's) | Else one contract-exact transient quote snapshot, as `market.snapshot` takes it, in a pool of its own. No seam field was added. |

Contracts are spelled as the daemon's other readers spell them (SPX, VIX and
SPY as `market.snapshot`'s references; the leaders as the market tape's), so
the read shares their chart series instead of starting new ones.

## Cost and budget

A read answers within a five-second budget. Its history comes from the chart
cache only and never waits on the broker: a series the cache does not hold,
or holds without a bar the read needs, is marked unavailable for this read and
read in the background through the bounded, coalescing history request on the
background pacing lane, at most once per series per ten minutes, to serve a
later read. The intraday series (SPX, SPY, VIX `1D`) and SPX's daily series
are followed, so the refresh worker keeps them current. Quotes are twelve brief
looks, six at a time, 1.5 seconds each; a look that has not answered by the
budget is left out of that read.

The positions read runs beside the other acquisitions, inside the budget:
it can probe the broker while it prewarms quotes, so a slow one delays only
the held block, which then says `positions not read within the read's
budget` (`TestMarketReadHeldPositionsThatNeverAnswerStayInsideTheBudget`).
The held names never take a slot from those looks. A held name whose line
the daemon already holds (a held stock, which the display feed streams) is
priced from that line with no look. Any other (typically the underlying of
an option-only position) takes one quote snapshot of 1.5 seconds in a
separate pool, four at a time. A held leader takes nothing of its own: its
row is built from the leader's inputs. With eight held underlyings that
means no extra look when all eight lines are held, and at most eight looks
in two rounds, three seconds, when none is. At the cap of twelve it is
three rounds, 4.5 seconds. A look still out at the budget is left out, its
row says why, and the read answers at five seconds as before
(`TestMarketReadHeldQuotesThatNeverAnswerStayInsideTheBudget`: eight held
names whose quotes never answer). The held daily series add at most twelve
background reads per ten minutes while they stay missing, so at most 24
with the leaders and SPX, against the broker's 60.

One result answers every caller for 30 seconds, and a read in flight is
joined, not repeated. While a read is in flight, a last good read up to five
minutes old answers at once, its `as_of` saying when. A result answers only
in the account scope it was read in.

Steady state, with the series retained: no historical request per read; the
followed intraday series cost what the refresh worker already spends on them.
When series are missing (the chart cache drops the least recently written,
and the leaders' daily series are written once each evening), at most eleven
background daily reads per ten minutes while they stay missing, against the
broker's 60 per ten minutes, plus one full read of SPY's intraday window per
session when its record holds fewer prior sessions than the baseline needs (a
routine refresh reads only the recent tail).

Before 2026-11-24 the read fetched a missing daily series inline at
interactive priority; with the leaders' series evicted it queued eleven reads
behind the two shared history slots and the pacing, and timed out at 30
seconds. Those two slots also serve charts, the tape and the refresh worker,
which waited behind it.

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
as deep as the daemon's recording and the days each read brings with it: a
series followed for two days has a
two-day baseline and is reported as such.

## Tags

Drawn while the session is open, as of its latest completed bar; once it has
closed, the read keeps the tags and events as they stood at its last bar.
Before the open there are none. All levels are named constants,
returned in `thresholds` with `heuristic: true`.

| Scope | Code | Tone | Condition |
| --- | --- | --- | --- |
| index | `gap_holds` | neutral | opened above the prior high, holds above it, and has not advanced half the opening gap (open against prior close) past the open: the move came with the open |
| index | `gap_down_holds` | neutral | the mirror below the prior low |
| index | `continuation` | positive | holds above the prior high and relative volume ≥ 1.2 |
| index | `breakout_thin_volume` | caution | holds above the prior high and relative volume < 0.8 |
| index | `above_prior_high` | neutral | holds above the prior high, volume between or unavailable |
| index | `breakdown_on_volume` | negative | holds below the prior low and relative volume ≥ 1.2 |
| index | `bounce_failed` | negative | holds below the prior low, volume under 1.2 or unavailable |
| index | `testing_prior_high` | neutral | the latest completed close is above the prior high, not yet held (a gap above the range before it holds included) |
| index | `testing_prior_low` | neutral | the mirror below the prior low |
| index | `inside_prior_range` | neutral | the latest completed close is inside the prior range |
| volume | `rally_fading` | caution | SPX above its close 30 minutes ago, the last 30 minutes of SPY volume < 0.7 × their baseline, and relative volume < 1.0 |
| volume | `selling_on_volume` | negative | SPX below its close 30 minutes ago and the last 30 minutes of SPY volume ≥ 1.3 × their baseline |
| vix | `stress_rising` | negative | holds above 25 and up ≥ 5% on the day |
| vix | `stress_easing` | neutral | holds above 25 and down ≥ 5% on the day |
| vix | `stress_elevated` | caution | holds above 25 otherwise |
| vix | `fear_easing` | positive | holds below 25, down on the day, and either the prior close was at or above 25 or the fall is at least 5%; a calm day drifting lower is not it |
| leaders | `leaders_breaking_down` | negative | at least 3 leaders have a state and ≥ 70% of them are under their prior low, whatever their gaps; ahead of the two below |
| leaders | `leaders_hold` | positive | at least 3 leaders gapped up and ≥ 70% of them hold |
| leaders | `leaders_fading` | negative | at least 3 gapped up and ≤ 40% hold |
| leaders | `leaders_gap_down_hold` | negative | at least 3 leaders gapped down and ≥ 70% of them hold the gap down (`holds_gap_down`, or under the prior low: the gap has only gone further) |
| leaders | `leaders_reclaiming` | positive | at least 3 gapped down and ≥ 70% of them have reclaimed the prior close |
| relative | `nasdaq_lagging` | caution | the Nasdaq 100's day change is at least 0.5 points (`relative_gap_pct`) under the S&P 500's |
| relative | `nasdaq_leading` | neutral | at least 0.5 points over it |

Among the leader tags, `leaders_breaking_down` comes first; then the side
with more gapped names is read (the gap-up side on a tie), and the other side
only when the first names nothing. The leader and relative tags read current
quotes, not bars: they have no hold and no `since`, and say why.

## Nasdaq beside the S&P 500

While the session is open, `relative` sets the Nasdaq 100's day change (the
NDX index's last against its previous close, or QQQ's when the index does not
quote, named in `source`) beside the S&P 500's (SPX's last against its prior
session close) and their difference, `spread_pct`. It uses no second chart
and no historical series. Before the open the Nasdaq is read in `indicated`;
after the close `relative` is absent with a reason, because a quote's previous
close can already name the day just ended.

## Pre-market indication

Before the open (state `pre` only) the read says what the open is indicated
to do, in `indicated` and `indications`, kept apart from the tags. Nothing
here has held: pre-market trade is thin, a price before the open is an
indication and not a level the session has reached, and the read keeps no
earlier pre-market reading.

- `indicated.index`: the ES front future (the dated contract named in
  `source`) against its previous close; `implied_level` applies that change to
  SPX's prior session close, and `implied_position` places the level against
  SPX's prior range (`above_prior_high`, `inside`, `below_prior_low`).
- `indicated.nasdaq`: the NQ front future the same way.
- `indicated.leaders`: the leaders with a pre-market price, counted `up`,
  `down` or `flat` by `indicated_gap_pct` (the pre-market price against the
  prior close, on each row) at the 0.25% gap minimum, and `near_prior_low`
  (within 1%, `near_low_pct`, above the prior low) or `below_prior_low`. A
  price that is still the prior close, or a trade from before the session's
  day, is no indication and the row says so.

Before the open `index.position` is absent, whatever the index's last price,
with `position_unavailable: "the session has not opened"`; the position the
futures imply is `indicated.index.implied_position`. The first live
pre-market read, 2026-11-24, drew `inside` from the index's last print of the
day before, which differed from the official close and so passed as a trade.

| Scope | Code | Tone | Condition |
| --- | --- | --- | --- |
| index | `indicated_gap_up` / `indicated_gap_down` | positive / negative | ES change at or beyond the 0.25% gap minimum |
| index | `indicated_above_prior_high` / `indicated_below_prior_low` | positive / negative | the implied level beyond SPX's prior range |
| leaders | `leaders_indicated_up` / `leaders_indicated_down` | positive / negative | at least 3 leaders quoted and ≥ 70% of them indicated that way |
| leaders | `leaders_near_prior_low` | caution | at least 3 quoted and at least half (`near_low_share`) near or below their prior low |
| nasdaq | `nasdaq_indicated_weaker` / `nasdaq_indicated_stronger` | caution / neutral | NQ's change at least 0.5 points under / over ES's |

Each carries its numbers in `evidence`. Without the quotes, or with too few
leaders quoted, the indications are silent and each missing part says why.

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
whatever their state; `holding_gap_up` those that hold it; `below_prior_low`
the companies with a state under their prior session's low.

## Held underlyings

`held` makes the read account-scoped: it names the underlyings of the
current stock and option positions, never a quantity, value or account id.
Each row reads the name as a desk reads a position on price: its prior
regular session (`prior`: date, high, low, close, from the daily series as
the index's), today's regular range so far (`day_high`, `day_low` and
`day_mid`, their midpoint) and `last` against the prior range (`position`:
`above_prior_high`, `inside`, `below_prior_low`). `open`, `last` and `state`
follow the leader rules, and a held leader carries its leader row's values.

- Open: today's range comes from the name's completed regular five-minute
  bars when its `1D` series is retained and current (`day_source: bars`),
  else from the quote's day high and low (`quote`). It is widened to `last`,
  which can lie past the last completed bar.
- Before the open: `prior` and `last` (with the pre-market rule above) only;
  `day_*`, `position` and `state` are absent, because a quote's high, low and
  open still name the prior session.
- After the close: the session's daily bar gives the range (`daily_bar`, else
  its bars), and `last` is its close (`last_source: daily_bar`), as the
  leaders read it.
- A holding that takes no market data, or a non-US listing (the read follows
  the US session), gets a row with its reasons and no look. With nothing
  held, `rows` is empty and `rows_unavailable` says so; `truncated` says the
  cap of twelve cut the list.

VIX holds a side of its line on two closes beyond it; once a side holds,
crossing back needs two closes beyond the line by 0.5 (`vix_band`).

## Replay, 2026-11-24

`TestMarketReadReplay` (run with `MARKET_READ_REPLAY_DATA`; no data is
committed) steps each recorded session five minutes at a time, giving the
read only the bars completed by then and a baseline of earlier sessions only.
Five-minute bars were recorded for 2026-11-23 alone; 2026-11-17 to 11-20 were
replayed coarsely from 30-minute bars (a hold is then an hour, and the
30-minute volume tags cannot fire). Leader states were replayed at the close
only, from daily bars. What it showed, and what changed:

- 11-23 opened 4.6% above the prior close and above the prior high, dipped
  back inside the range for two bars, then held above it all day and closed
  near its open. The read said `above_prior_high` from 09:55 to the close; with
  a volume baseline it would have said `continuation`, though the session
  added nothing after the open. Hence `gap_holds`.
- 11-23 VIX opened near 29 from a 35.89 close and stayed between 26 and 29:
  `stress_elevated` (caution) all day named the level and missed a 25% fall.
  Hence `stress_easing`.
- 11-18 VIX traded between 23.6 and 25.2 against a prior close of 25.46: the
  tags swapped `fear_easing` and `stress_elevated` three times, one run lasting
  30 minutes. Hence the band; with it the day reads `fear_easing` throughout.
- 11-17 VIX broke its `stress_rising` run for an hour at the line; with the
  band it stands from 11:35.
- 11-23's volume baseline rested on one prior session, so relative volume was
  unavailable all day although the session traded about 1.5 times its usual
  volume. The chart cache had kept only 11-20 of SPY's five-minute history,
  having dropped and rewritten the series. Hence keeping the padded days of
  each 1D read.

A later round added `breakdown_on_volume` and `leaders_breaking_down`: the
replay of 11-20, a heavy selloff on about twice the usual volume with every
leader closing under its prior low, read only `bounce_failed` and no leader
tag, because the leader tags looked only at gap-ups.

Rendering the replay showed 11-20 at 10:30 reading `inside_prior_range`
while the latest close was already under the prior low and `index.position`
said `below_prior_low`: an unheld break fell through to "inside". It now
reads `testing_prior_low`.

Bar close times (`last_at` when no quote, `since`, event bounds) are a bar's
start plus five minutes. Live reads serve five-minute series only (the read
refuses any other interval), so that holds; the replay harness, which can
feed coarse bars, moves those times to the coarse bar's end itself.

`last_at` is the broker's trade time when it sends one (`last_source:
trade`). Before the open a price equal to the previous close is that close,
stamped with the prior session's close time (`prior_close`); a line opened
pre-market delivers the old print with a fresh receipt time, which was shown
as current until 2026-11-24. Before the open a trade timed before the
session's New York day is also the prior session's (`prior_close`), keeping
its real time, whatever it differs from the official close by; a trade timed
that day, such as VIX's pre-market trade, stays `trade`.

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
`market_read_indicated_test.go` covers every indication code, their silence
when quotes are missing or too few, the pre-market position rule, the
gap-down leader tags and their precedence, the relative tags and the QQQ
stand-in, and that unanswered futures and Nasdaq looks keep a read inside its
budget. `market_read_held_test.go` covers the held rows: the range from bars,
from the quote and from the daily bar after the close, the range widened to
the price, each position and state, the pre-market and closed rules, every
missing value's reason, a held leader matching its leader row, nothing held,
the pick and cap of holdings, the shared chart series, which held line
serves a name, and eight unanswered held quotes inside the budget.
