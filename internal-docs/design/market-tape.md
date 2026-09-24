# Market tape and rally quality

Status: stages 1 and 2 implemented locally, 2026-09-24; new participation coverage
awaits scheduled collection. Owner: Canary daemon; CLI, MCP, SPA and Desk adapt
one result. This is market observation, with no order,
portfolio, risk-policy, freeze or alert authority.

## Question and decision

Can deteriorating participation and volume pressure distinguish a durable rally
from a bounce likely to retrace over the next 1–3 sessions, soon enough to help?
The strongest practical baseline is a price-only model using recent returns,
trend, drawdown and realized volatility, alongside the unconditional event rate.
An attractive chart or this week's retrospective explanation is not evidence of
forecasting skill.

Measure chronological out-of-sample calibration/Brier score, precision at the
same warning frequency, warning lead time, false alarms, missed upside, and
portfolio outcomes after turnover, slippage and hedge costs. Keep an untouched
test period, use block uncertainty estimates, purge overlapping outcome windows,
and ablate each added input. Retain forecasts only if improvement is stable
across regimes and justifies data cost, maintenance and operator attention.
Otherwise keep the descriptive tape and retire the forecast.

## Stages

1. **Observed tape (current scope).** Serve 5–60 completed US equity sessions
   (default 20), aligned by the official exchange calendar. Show SPX and QQQ
   price changes, QQQ relative volume, and retained S&P 500 breadth (50/200-day
   participation and 252-session closing highs/lows). Use existing acquisition,
   caches and breadth history, preserving gaps and producer clocks. Add
   `canary market tape`, `canary_market_tape`, and a lazy read-only SPA panel.
2. **Participation and pressure.** Extend constituent collection with dated
   volume, advance/decline/unchanged counts, 20-day participation and advancing
   versus declining volume, all with separate coverage. A volume field attached
   to an index or an ETF is not total-market or signed trade flow. Preserve
   membership snapshots and collection timestamps; no retrospective
   backfill may masquerade as information known at the time. Add session/VWAP
   persistence only after intraday acquisition and same-clock comparisons are
   proven. Explain price and participation relationships without a predictive score.
3. **Forecast evaluation.** Freeze event definitions and candidate thresholds
   before evaluation. Test next-session and three-session retracement/drawdown
   targets against the baseline. Add separated equity/index/ETF put/call and
   short-expiry flow only after a source/access/cost review and incremental-value
   test. Volatility/skew is pricing context; open interest is not dealer intent.
   No calibrated probability, alert or execution coupling ships in stage 1.

## Stage 1 authority and timing contract

| Concept | Owner / input | Output and missing behavior |
| --- | --- | --- |
| Calendar | `internal/marketcal` | Completed regular/early-close sessions only; unknown calendar fails explicitly |
| Prices and ETF volume | Existing daemon `market.history`, IBKR regular-session TRADES | Exact SPX/QQQ identities, original acquisition/cache metadata; missing sessions stay null |
| Breadth | Existing daemon `breadth.spx` | Measurement-specific denominators; zero coverage is unavailable, not zero participation |
| Derived changes | Daemon composition | One-session changes require consecutive official sessions; relative volume needs the previous 20 complete observations |
| Chart | Typed `market.tape` via CLI/MCP/GET `/api/market-tape` | Adapters format/plot measurements; never infer posture from gaps or calculate a forecast |

The snapshot is a retrospective reconstruction. `as_of` is composition time;
source timestamps remain acquisition times, not per-session publication times.
Historical first-availability is unknown in this first version. Reads do not
create backdated decision records. Daily observations are selected only when
both the session has closed (plus the existing 15-minute bar settle window)
and acquisition occurred after its close. Breadth may arrive later than prices.
No carry-forward, zero fill, interpolation across gaps, or multi-session return
labelled as daily is permitted. Index volume is inapplicable and discarded.

The first panel uses existing infrastructure with no new provider, dependency,
scheduler, policy thresholds or durable authority. Existing bounded history
interest/refresh handles acquisition. A missing component must not hide another
component or present the whole tape as complete. The UI names the last completed
session, missing observations, and QQQ's ETF-volume scope.

## Acceptance and rollout

- Focused composition tests: holiday/weekend and open-session boundaries,
  stale acquisition, a missing middle session, nil versus zero volume,
  20-session volume baseline, invalid/future evidence and independent coverage.
- Adapter tests: read-only CLI/MCP/HTTP contract; no raw broker error disclosure.
- SPA: aligned axes, disconnected paths at gaps, unavailable cells, source
  detail, safe text rendering and late-response protection. Inspect the embedded
  isolated loopback preview at desktop and narrow widths.
- Run `make docs-regen` for the CLI/MCP catalogue; run logged `make test` once
  for the full Go/runtime gate, including repository and rendered checks.
- Refresh the daemon through `make restart-daemon`; capture redacted status and
  `canary market tape --json`. No shared phone-host restart or publication is
  part of this change. Live order-path smoke is irrelevant to this read-only
  composition; existing broker adapters remain unchanged.

Rollback removes the new composition and adapters. New breadth fields are
optional additions to v3 storage; older readers ignore them without a version
wipe. Existing closes and risk policy are retained.

## Local verification receipt, 2026-09-24

- Focused daemon, CLI, MCP and HTTP tests pass. The first CLI run exposed flag
  hoisting before the subcommand; routing now uses the existing positional
  parser. The first UI test exposed a timezone-dependent test expectation;
  the assertion now respects the display formatter while checking distinct
  acquisition and composition clocks.
- `make docs-regen` and logged `make test` exit 0. The full gate includes
  normal/trading daemon builds, race-enabled hermetic integration, production
  browser rendering and all eight retained regression witnesses. The standard
  vulnerability gate reused its same-day scan because dependencies/toolchain
  were unchanged. No live order-path smoke was needed or run.
- Installation through `make restart-daemon` succeeded. Its start step then
  exited nonzero because another local client had already started the new
  installed daemon after the graceful stop. Read-only follow-up confirmed the
  installed executable, connected/ready gateway, identical account/connection
  pins and trading mode/override fields, and successful `market-tape-v1` output.
  `CANARY_SOCKET` was pinned to the existing socket so shared app management
  stayed disabled; the phone host retained its original process.
- `canary market tape --sessions 20 --json` exits 0, preserves 20 expected
  sessions and reports seven missing early breadth sessions. The 10-session
  view is complete at verification time. This is acquisition coverage, not
  evidence of forecasting skill or timely historical availability.
- Embedded `market-tape.js` bytes match the working source. The isolated
  `127.0.0.1:8766` read-only app was inspected at desktop and 360-pixel widths:
  three aligned charts, visible gaps, range/session selection, correct selected
  values, source clocks and no horizontal overflow. This does not verify the
  phone's installed app or publish a release.

## Participation collection implemented on 2026-09-24

`internal/breadth/spx` collection and persistence now retain dated constituent
volume and observation times while preserving existing close history.
Old observations keep unknown volume and historical availability. Record the
membership used for each snapshot; never assume today's members were the old
universe. No extra paid data provider is required for this first collection trial.

Advancing/declining/unchanged counts use only constituents with both
consecutive session closes. Advancing/declining volume uses the separately
disclosed subset with valid volume. Supply coverage and denominator counts for
every measure; an empty or zero denominator remains unavailable. 20-day
participation can be computed immediately from adequate retained closes. Preserve these facts before trying
a composite label, and measure collection load and missingness before expanding
to intraday sampling or options feeds.

Stage 3 then fixes the event/target definition and evaluation windows, compares
price-only and participation-augmented models, and reports false warnings and
net decision costs as well as prediction scores. A negative incremental-value
result stops the forecasting work while retaining the useful observed tape.


## Interpretation UX and ownership

Each session carries one daemon-generated `reading`: a short observed-relationship
headline, plain-English explanation, measurement/value/meaning rows, next-session
checks and explicit limitations. CLI text prints it before the aligned tape;
JSON carries identical content. Text uses the shared terminal wrapping and
aligned numeric tables; `--explain` reveals measurement meanings, limitations
and source clocks. `canary market tape --help` works without a daemon and names
the default, bounds and examples. Canary presents a date selector, the reading,
four aligned plots, measure-specific denominators and collapsed meaning/source
sections. Desk's Market view renders the same reading on demand, preserves the
selected session across snapshots/navigation, and labels a retained read after a
failed refresh. Opening it makes no model call and does not change risk posture.

Examples include "Price rose; trend participation narrowed" and "Price fell;
trend participation weakened". Nearly unchanged means zero after rounding to
the displayed two decimals, not a fitted threshold. Trend participation is not
daily advancer share. Daily share-volume groups stocks by their close direction;
it is neither signed trades nor dollar turnover. Missing daily participation
never becomes a bearish signal. Unchanged names/volume are excluded from the two
directional shares; zero denominators stay unavailable.

A non-rising session can show the fraction of the most recent up day's point
gain given back, looking back at most five sessions. The reference date is always
named. Missing sessions break the comparison. A value over 100% means the whole
gain was lost; a negative number means the current close remains higher. The
comparison is independent of the selected chart window and is retrospective.

The collector adds dated close/volume pairs and actual acquisition timestamps to
existing windows without invalidating older closes. Unchanged rereads preserve
acquisition time; corrected bars get the new acquisition time. Each revision
records the sorted membership used and its hash, computation time, and latest
paired-input acquisition. These clocks do not establish original historical
publication time. Legacy rows retain absent participation; no forced 500-name
backfill or added provider subscription is introduced. New membership records
are the universe used at collection, not reconstructed historical constituents.
50-day changes require equal member/coverage counts and equal membership hashes
when both exist; equal counts still cannot prove an identical covered subset.

Desk temporarily uses a fixed, bounded `canary market tape --sessions N --json`
command because its published Go client predates the new tool catalogue. The
40-second read discards raw stderr, caps output at 2 MiB, validates the descriptive
envelope, and sits behind the existing authenticated GET console boundary. This
bridge can be replaced with the typed client after normal Canary publication;
no local module replacement or premature release is needed.

## Likely build and observation sequence

These are planned acceptance stages, not scheduled agent jobs or promises about
market direction. Official calendar checks on 2026-09-24 show regular US equity
sessions on September 24, 25, 28 and 29. For these dates the close is 22:00 Berlin.
The existing constituent collector starts at close +35 minutes and has a
90-minute publication window; 22:35 is the earliest start, not a guaranteed
availability time. Price rows may appear earlier after their 15-minute settle.

| Trading day / stage | Work and usable outcome | Continue / stop test |
| --- | --- | --- |
| Thu Sep 24, after close | Run the tested collection candidate; retain the first dated count/volume row. CLI and Canary already explain legacy price/trend breadth. Desk source and isolated rendering are checked. | New fields must have honest coverage, official-session pairs and acquisition clocks; absent coverage stays pending. Preserve the existing gateway pacing and no extra fanout. |
| Fri Sep 25 / next morning | Compare two completed rows; inspect a rise, fall, flat day and missing-data case. Confirm the same selected-day values/text in CLI, Canary and Desk. | Coverage, latency and saved history must survive restart. Pause expansion if the collector misses its ordinary publication window or harms interactive reads. |
| Mon Sep 28 and Tue Sep 29 | Use the tape prospectively in preparation and compare next-session outcomes with the saved earlier reading. Write down disagreements and false warnings without changing labels after the outcome. | Establish workflow usefulness and timing; four observations cannot establish forecasting skill. Finalize event/target definitions, source availability and an untouched evaluation split. |
| Following 1–2 weeks | Build reproducible offline replay and the price-only baseline. Review same-clock volatility/credit inputs and separate equity/index/ETF put/call access, definitions and cost. Add only inputs whose timing and coverage can be defended. | Chronological held-out improvement, useful lead time and net decision outcomes must justify added complexity. Historical data with unknown availability cannot prove live warning lead time. |
| Longer forward sample | Run any promising frozen candidate in shadow mode, with a complete failure/false-alarm log. Only then consider calibrated probability or alerts. | If improvement does not survive untouched periods and uncertainty/cost checks, keep the descriptive tape and stop forecasting work. |

The next trading days validate acquisition, explanations and usability. They do
not supply enough independent reversal events for probability calibration. A
same-day pre-close warning is a separate later feature: official intraday clocks,
VWAP/session persistence and a frozen decision cutoff must be captured and tested
before claiming it would have helped before the close. No broker action, policy
change or recurring manual sign-off is part of this research.

Historical forecasting needs a separate dataset decision. The current retained
breadth history covers at most 60 sessions and older rows lack daily counts,
volume and original availability. It cannot support a credible multi-regime
forecast evaluation by itself. Prefer a sufficiently long dataset with dated
membership, delisted names where relevant, and defensible availability clocks;
otherwise restrict conclusions to the forward sample and postpone calibration.

## Stage 2 local verification receipt

- New tests cover Friday-to-Tuesday holiday pairs, missing prior closes, legacy
  history preservation, zero versus absent volume, independent coverage,
  membership identity, unchanged reread clocks, corrected revisions, denominator
  changes, and uncapped up-day retracement without bridging missing sessions.
- `make test` passes, including race-enabled daemon/default/trading tests,
  hermetic lifecycle integration, rendered app checks and regression witnesses.
  After the final view-only change, `make app-check app-render-check` passes.
  Documentation is regenerated; whole-tree account and identity scans pass.
- Desk's targeted Go tests, `make modernize`, current stable toolchain check
  (Go 1.27.1), `make build` and synthetic browser witness pass. The sandboxed
  briefing gate first failed because its synthetic child process did not
  dispatch; the unchanged test passes outside the sandbox. A 4-pixel spacing
  grid violation was corrected before the passing frontend gate.
- Desk source is integrated into its current working checkout, preserving
  concurrent briefing changes; the combined build and browser witness pass.
  Its managed live service has not been cut over. The browser proof uses
  synthetic data and verifies on-demand GET, shared prose, retained selection,
  failed-refresh labeling and narrow-screen layout; it is not forecasting proof.
- Canary's daemon was refreshed on the existing socket and reports connected /
  ready with unchanged authority fields. The shared phone app retained its
  existing process. Live JSON includes readings for every requested session;
  20-day participation is available from retained closes, while daily counts
  and constituent volume remain explicitly uncollected pending the scheduled
  sweep. No live fanout or broker/order-path smoke was forced: the wire decoding
  and pacing are unchanged, and the additive volume adapter is covered locally.
- No publication, risk-policy change, broker order, paid feed or model run is
  part of this implementation.

The final CLI review preserves raw JSON precision while rounding text to its
displayed precision, including neutral zero instead of `-0.00%`. Plain output
has no ANSI escapes; explicit colour mode uses the existing terminal palette.
The required stable supplier check found HyperServe v2.2.0 published after the
initial implementation, so the final candidate pins that release and checksums.
Canary's strict JSON and flush-error adapters remain in place.
