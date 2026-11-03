# Strategy evaluator (one evaluator for every register entry)

Written: 2026-10-18 18:10 CEST. Status: built on Canary branch
`strategy-evaluator`, 2026-10-18 (section 8). This record follows
`.agents/docs/risk-policy-contract.md`: it proposes the keys the owner may
write and creates no policy. It carries out plan item 4 of the review of
2026-10-18 (Desk `output/secret-sauce-20261018/review.md`, finding 2: "a
strategy is a project, not a register entry") against `strategy-register.md`
and `core-sleeve.md`. Revised 2026-10-19 09:30 CEST: every live entry serves
its rows (build 2; §5), the kill rule is measured on a live entry's own fills
(§3), and §7 records the owner's decisions on the second entry as taken
(owner instruction 2026-10-19 07:07 CEST, "task build 2 and then 6 as well").

Sources read: Canary main `73577985` (`internal/core`, `internal/register`,
`internal/risk`, the daemon's core files), the lab `0870f3b` (`core_rule.py`,
`core_fixture.py`, `s0_core_sleeve.yaml`), Desk `9f0d0ff`.

## Decision

- Goal: one evaluator in Canary runs any entry of the strategy register. An
  entry names its instrument (ISIN and currency), a signal from a small
  library, the owner's numbers (band, minimum, value cap) and its kill rule;
  the sizing is against its sleeve's risk allocation at the register's stage
  and scale. The evaluator serves the verdict shape `core.target` serves, so
  the gate, the forced reduction, the shadow record and Desk's controller
  read one number per strategy.
- Protected behaviour: the core's verdict is unchanged (its goldens and the
  parity fixture prove it); no entry adds risk, reserves allocation, creates
  a proposal or places an order: every entry rule sent nothing, so every
  entry ran in shadow at most in this first build. That was the build's reach, not a step the owner wants: on 2026-10-18 21:05 CEST the owner decided "we NEVER want shadow, it's useless", so an entry goes live on the owner's word. Since 2026-10-19 (build 2) every live entry serves its month's row through the one order path (`strategy-register.md`, "Rows for every live entry").
- Policy owner: the desk owner, who writes every entry, promotion and number;
  this build proposes keys only (§2). Status: built; the second entry (the
  trend fund) waits on the owner's decisions (§7).

## 1. What the core's code became

`internal/core` (pure) was already instrument-free: `Measure`, `SizeTarget`,
`Guard`, `Headroom` and `Decide` take a calendar, closes, a decision day and
the owner's numbers. It is now the evaluator's arithmetic:

| Before | After |
|---|---|
| `Measure` with the 10-month average compiled in | `MeasureWith(trend, label, …)`: the filter is a library entry, the digest label the strategy's; `Measure` is the core's call, byte for byte |
| `Sizing.CoreVolPct` | `Sizing.AllocationPct` (JSON `allocation_pct`): the sleeve's risk allocation, whichever sleeve |
| `Digest(w)` names `core-ibcf-v1|<spec>` | `DigestFor(label, w)`; the core's label is unchanged, so no frozen month drifts |
| `Guard` corroborates on SPY | `GuardWith(…, proxy)`: the instrument's returns proxy, or none (section 5) |
| `Decide` names IBCF and "the core" | `DecideInput.Symbol` and `.Name`; the core passes its own, so its copy is unchanged |

The compiled rule catalogue (`internal/register`) carries two rules:

| Rule | Form | Instrument | Signal | Numbers | Spec hash |
|---|---|---|---|---|---|
| `core-ibcf-v1` | `[mandate.core]` | compiled: IE00B3ZW0K18 EUR | compiled: `trend-sma10m` | the core's keys, as today | compiled and pinned; a cited hash must match |
| `timed-holding-v1` | `[mandate.strategies.<id>]` | the entry's `isin`, `currency` (`con_id`, `exchange` optional) | the entry's `signal` | the entry's `band_pct`, `min_order_base`, `max_value_pct` | the entry's citation (section 4) |

Both run through one daemon evaluator (`strategy_target.go`): the core is
its first instance, built from `[mandate.core]` with the compiled
constants, and `evaluateCoreTarget` wraps it. One contract resolver per
instrument (pins keyed by ISIN, currency and pinned id), one shadow recorder
and one frozen-month store serve every entry; the core's keys, scopes and
wording are kept exactly. `register.Resolve` gives each entry what the
evaluator runs: `Instrument`, `Trend`, `Terms`, `SpecHash`, `Data` and the
kill bound; for `timed-holding-v1` an unwritten key or a signal the library
lacks holds the entry and names it. The register's refusals are unchanged.

## 2. The entry (proposed; the owner writes it)

```toml
[mandate.strategies.trend-fund]     # id: the owner's, lower-case
rule                = "timed-holding-v1"
spec_hash           = "…"           # the lab registry's hash of the spec it implements
sleeve              = "overlay"     # its allocation is [mandate.sleeves] overlay_vol_pct
stage               = "shadow"
scale_pct           = 25
isin                = "…"           # the fund; IBKR's record names the line, venue and calendar
currency            = "EUR"         # the account's base currency, in this build (section 5)
signal              = "trend-sma10m"
band_pct            = 10
min_order_base      = 2500
max_value_pct       = 40
# con_id, exchange: optional pins, as in [mandate.core]
# kill_drawdown_k   = …             # section 3, in units of the sleeve invested at its stage; unwritten, a live entry holds (2026-10-19)
```

`risk_allocation_pct` stays as the register defines it. `[mandate.core]`
keeps its form and takes `kill_drawdown_k` as an optional key (fingerprint kept).

## 3. The signal library and the kill rule

A signal is a trend filter on month-end closes, `core.Trend{ID, Months}`,
read on the fixed window of `WindowCloses` sessions to the decision day (the
last session of the month on the listing's calendar). `trend-sma10m` is the
only entry: on when the decision close is strictly above the mean of the
last ten month-end closes, the decision's included; not live before ten
month-ends. A second filter is a new library entry with its own lab fixture
(section 4); the window stays the evaluator's, and a test refuses a filter
that needs more month-ends than the window holds. The volatility estimates
(EWMA span 63, the 252-day sample, the 15% floor) and the sizing
(allocation over σ, capped at `max_value_pct`, scaled by `scale_pct`, whole
shares) are the evaluator's, not the signal's: `core-sleeve.md` §1.

The kill rule is the lab spec's; the owner's lever is `stage = "suspended"`.
What Canary measures is the spec's drawdown test (a): the sleeve's
peak-to-trough loss in units of K, marked at each decision. K is the
sleeve's capital at its stage (review, 2026-10-19): S / 0.15 × `scale_pct` /
100, the notional of the sleeve fully invested at that stage, so the lab's
"0.35 K is a 35% fall of a fully invested sleeve" holds at every scale.
Until 2026-10-19 K was S / 0.15 whatever the stage, so at scale 25 a 0.35 K
bound was a loss of 140% of the invested amount and could not fire in time
(`stageCapital`; `canary strategy` prints the bound "of the sleeve at scale
25%"). The entry may write `kill_drawdown_k`, the bound the lab's design run
writes into its registry; unwritten, the kill rule cannot fire: a shadow
entry says so and runs, since it sends nothing, and a live entry holds,
named ("kill_drawdown_k is unwritten in [mandate.strategies.<id>], so the
kill rule cannot fire: buys stop until the bound is written"), as any
unwritten key holds (2026-10-19). A live entry's written bound of 1 K or
more is the whole invested sleeve and refuses the file (`register.Refusals`,
`KillBoundMax`). Written, the evaluator marks the paper sleeve (cash and the
paper position) at each decision close on the shadow record and at the
current reading, and past the bound holds the strategy before the month can
be frozen: buys stop, a sale the rule wants goes to the owner, the month is
recorded as a hold naming it, and the owner suspends.

Live (built 2026-10-19, `strategy_kill_live.go`): a live entry keeps no
shadow record, so the drawdown is measured on its own fills. At each frozen
decision the background refresh keeps one mark (scope
`strategy/<id>/<account>|<mode>|<conid>`, never read by an order path): the
decision close, the units and the cash
its own fills (the executions of its rows' orders) left, and S at that
decision; the sleeve's equity is cash plus units times the close, its peak
starts at zero as the paper position's does, and the drawdown in K is
measured from the running peak, this reading included; each mark keeps the
stage's scale it was sized at (`scale_pct`; a mark kept before 2026-10-19
reads the entry's current scale). A hand trade of the line is not an own
fill, so it moves the measure nothing. The spec's other tests are the
scorecard's and the cost report's, not this. The live core's sleeve is its
line's holding (refuter, 2026-10-19): every unit of the line is the core's
whoever bought it (placement), its shadow record is not written while it is
live, and its rows' own fills would miss a hand-built holding. So its marks
carry the holding, each change of holding since the previous mark taken at
that mark's close, the decision close the rule acted on (the paper
convention: the row's units carry the month that follows, units the rule
sold carry none of it), and the first mark adopting the holding at its own
close (`coreSleeveFills`): between decisions the sleeve's equity moves by
the holding times the move of the close, s0's test (a) on the sleeve, and
`canary core` reads the drawdown "on its holding". A hand trade between
decisions is measured as if done at the previous decision close. Marks that
cannot be read hold the entry ("kill rule: the marks of … cannot be read"),
the core and every live entry alike.

A mark belongs to one account, its paper or live mode, the strategy and the
line its own fills trade (the contract id), the scope its month is frozen
under, and the kill test reads only the marks of the scope it measures (the
v3.25.4 review's finding 1, 2026-10-20). Until v3.26.1 marks were kept by
strategy and month alone in a state database that outlives an account
change, so account A's mark of a month marked account B's month and B's own
drawdown past its bound read 0.00 K, not breached. Another account's, the
other mode's or another line's mark therefore never marks a month; the
month is measured on the current scope's own fills, and its mark is kept
once under that scope (a second write of the month keeps the first). A mark
whose payload names another scope than the one it is kept under holds the
entry as an unreadable mark does. While the account or its mode is not
resolved (no single concrete account, or an unknown mode), nothing is
measured and nothing is kept: a live entry holds ("kill rule: the account
and its paper or live mode are not resolved"), never "measured, not
breached". Marks kept by v3.25.4 and earlier (`strategy.mark.v1` under
scope `strategy/<id>`) name no account, mode or line; they stay in the event
log as written, for audit, and never count for any scope: their evidence is
not guessed into the active account, and the first decision after the
upgrade starts the scope's measure afresh from its own fills. The core's bound is the owner's to
write (`[mandate.core] kill_drawdown_k`); until it is written the live core
holds.

## 4. Parity: how the lab's fixture tests any entry

The lab's fixture (`core_ibcf_v1_parity.json`) names no instrument and no
account figure: synthetic closes on a synthetic calendar, and the rule's
reading per decision at three scales for a fixed synthetic allocation. It
therefore proves the evaluator, not the core: the trend filter and the
sizing. Every entry that declares `trend-sma10m` is tested by it, so no
fixture is written per entry; `TestParityWithTheLab` runs it through the
library entry, `TestTrendLibraryCarriesTheCoreFilter` pins `Measure` to
`MeasureWith(TrendSMA10)`, and `TestParityFixturePinnedToTheSpec` pins
fixture, rule and spec together as before.

What `spec_hash` means changes for a parameterised rule. For `core-ibcf-v1`
the hash is compiled and a cited hash must match (as today). For
`timed-holding-v1` Canary carries no hash to match: the hash is the owner's
citation of the lab spec the entry implements, checked for shape, shown and
recorded on every shadow record as cited. The parity guarantee moves to the
signal: the lab spec names its signal and numbers in `signal.params` (as
`s0_core_sleeve` does), the fixture pinned to that signal proves Canary
reads it as the lab does, and the owner cites the spec whose params they
copied into the entry (`strategy-register.md`, invariant 1). A second filter
needs the lab to extend `lab core-fixture` with its months and to commit one
fixture per filter; Canary pins each.

## 5. Where the evaluator stops, and why

- Currency: an entry trades in the account's base currency only; another
  holds it. Sizing a base-money target against a price in another currency
  needs an FX rate at the decision and funding by conversion, both outside
  the core's invariants; they come with the first entry that needs them.
- Data guard: a move beyond 8% is corroborated by the instrument's returns
  proxy where the mandate has one (`mandate.ReturnsProxies`: SPY for IBCF);
  an instrument without one holds the month on such a move, for review.
- Series: the core's stays in the mandate's history pass. Every other
  entry's is read after the verdict, one per entry per refresh, under
  `strategy:<id>:con:<contract id>` (register review S2), so an entry never
  ages the book's verdict; a test pins that no such key enters its evidence.
- Holding and placement: every unit of the core's contract is the core's.
  An entry's holding is what the placement puts in its sleeve: the units
  its own fills left, never more than the broker holds (by lineage,
  `mandate-risk-budget.md`, "Placement", as built 2026-10-19); while the
  owner holds units of its line it holds, naming them; in shadow the paper
  position. In the hypothetical book an entry's paper
  position is a leg of its own (`sleeve:<sleeve>:con:<id>`), placed in its
  sleeve beside the owner's real units of the same fund, and the fund is
  measured on the entry's own closes where the book keeps none; the core's
  real units are replaced, as today.
- Protection's automatic sales leave out the core's contract (D5) and, since
  2026-10-19, every line the verdict places whole in another entry's sleeve
  (the entry holds it alone; carried out under the owner's instruction of
  07:07 CEST, the brief naming the exclusion "as the core's");
  `mandateReservedIDs`. A line the owner holds units of keeps its automatic
  protection. The forced reduction still covers them all.
- Rows (built 2026-10-19): `timed-holding-v1` sends, so a live entry serves
  its month's `strategy_rebalance` row with the core's terms and gates, its
  units placed by lineage (`strategy-register.md`, "Rows for every live
  entry"); shadow is the technical state of an entry not yet written live,
  never a required step (owner, 2026-10-18 21:05 CEST).

## 6. Surfaces

`strategy.target {id, refresh}` serves any entry's verdict in the
`core.target` shape (`rpc.StrategyTargetResult`; `CoreTargetResult` is its
alias) with `strategy`, `sleeve`, `signal` and `kill` added; `core.target`
is unchanged. `canary strategy ID [--refresh] [--json]` and
`canary_strategy` render it as `canary core` does, headed by the id;
`canary core` is byte for byte as before (goldens). `canary mandate`'s
register lines name each entry's fund and signal. Shadow records of an
entry carry its id, rule, cited hash and stage under `shadow/<id>`.

## 7. The second entry: the decisions as taken (2026-10-19)

The owner's instruction of 2026-10-19 07:07 CEST, "task build 2 and then 6
as well", with every authorisation given, delegated these to the
integrating session; each is recorded with its reason.

1. **The fund and its line.** iMGP DBi Managed Futures, in its class hedged
   to EUR (R EUR HP, ISIN LU3359622902), on Xetra (IBKR primary IBIS2,
   symbol MFEH), in EUR. The rule chose it: a EUR line on a venue Canary
   serves comes before the London line DBMG (the USD class quoted in GBP),
   which would have needed FX sizing and funding; the Paris EUR class has
   no calendar Canary embeds. §5's currency hold therefore still stands for
   any other currency.
2. **The signal series** (integrating session on the owner's delegated
   judgement, 2026-10-19 07:31 CEST, verbatim in substance: "The trend
   signal is the fund's, not the share class's, so read the ten-month
   month-end series from the fund's oldest share class with enough history
   ... A seven-month data hold on a live entry is a shadow by another name,
   and the owner ruled shadow out"). The EUR class listed on 18 May 2026, so
   its own closes fill the 253-session window only about mid-May 2027. The
   entry names a signal series separately from the traded instrument
   (`signal_isin`, `signal_currency`, `signal_in`): the fund's R USD class
   (LU2951555585) on its London line (DBMG, GBP, closes since April 2025),
   measured in USD, converted at each session's GBP.USD IDEALPRO close,
   since the class's own currency is the dollar and a GBP quote of it would
   mix sterling into the fund's trend. Until the EUR class's own closes
   fill the window the month is measured on that series, on London's
   calendar; the target is sized, the row's limit checked and the kill
   rule marked on MFEH's own close; the mandate measures MFEH on the
   series' returns, as it measures the core's fund on SPY's. From the first
   decision whose window MFEH's closes fill, they take over. A signal line
   that cannot be resolved, a series or a rate missing or stale, holds the
   entry, named; nothing falls back. "Same fund" is the owner's citation in
   the policy and the lab spec, checked for shape (another ISIN than the
   traded line's), not by Canary.
3. **The lab spec**: `s6_trend_fund`, registered (no edge claim; the same
   ten-month filter on month-end closes; the kill rule a drawdown bound of
   0.35 K, set a priori at about twice the replicated CTA index's recent
   worst drawdown, wide on the owner's "aggressive side"). Its hash is the
   entry's `spec_hash`.
4. **The entry**: `[mandate.strategies.s6-trend]`, live, its own sleeve
   `trend` with `risk_allocation_pct` at the budget's room left beside the
   core's and the overlay's allocations (the file refuses more), scale 25
   (the ladder's first live step, as the core's), band 10%, minimum 2,500,
   cap 40%, `kill_drawdown_k = 0.35`; risk policy v16, written after the
   daemon that reads it was installed.
5. **Protection's automatic sales** leave its line out while the entry
   holds it alone, as the core's (D5, extended); a hand buy of its line is
   refused while it is live.

## 8. Build and verification

Built 2026-10-18 on branch `strategy-evaluator`, synthetic data only.
Tests: the fixture unchanged and pinned, `Measure` pinned to
`MeasureWith(TrendSMA10)`; the two rules and the parameterised resolve
(each unwritten key and an unknown signal hold; a complete entry resolves
with its instrument, filter, terms and series); the keys' shapes, old files
keeping both fingerprints; the core's daemon tests unchanged; a second entry
on a synthetic ISIN on a London line in shadow decides its month, writes
decision, intent and fill once beside the core's, keeps its paper leg in its
own sleeve beside the owner's real units, never reaches the kept verdict,
its key never in the verdict's evidence; an unwritten key, another
currency, an unknown signal and a missing listing hold; the kill bound holds
past the drawdown and cannot fire unwritten; CLI goldens (`core*`
unchanged, `strategy_shadow`, `strategy_hold`); the order-path scan and
`TestOnlyTheDaemonEvaluates` widened. Files: `internal/daemon/strategy_target.go`
(was `core_target.go`), `core_resolve.go`, `shadow_record.go`,
`internal/rpc/core.go`, `internal/cli/core.go`, `internal/mcp/tools.go`.

## 9. The replay: did we build the right thing? (2026-10-19)

The parity fixture (§4) proves the arithmetic on synthetic closes. It says
nothing about the daemon's data, its calendar or the rule as the register
runs it on real month-ends. The replay does, as the owner asked on
2026-10-19 (07:58 CEST): the daemon runs the rule over the month-ends of an
entry's instrument on its own history and prints the lab's decision table;
the lab prints the same table from its backtest data; a lab script holds the
two side by side and names every disagreement with the date, both sides'
numbers and its cause.

- `strategy.replay {id, from, to}` and `canary strategy ID --replay FROM TO
  [--json]`: the daemon resolves the entry's contract as the evaluator does,
  reads five years of daily closes through the charts' broker helper (the
  read that fills the entry's series, `strategyReplayRange`), builds the
  calendar as the monthly evaluation does (`coreCalendar`: the official
  sessions where the embedded calendar covers them, the series' own dates
  before) and runs `core.Replay` from FROM to TO, TO bounded at the last
  decided month-end. `core.Replay` is `MeasureWith`, `SizeTarget` and
  `Decide` at every month-end, the paper holding carried from zero under the
  entry's band and minimum at one fixed, printed sizing (`core.ReplaySizing`:
  6% of 250,000 at full room, cap 40%, scale 100; the lab's
  `REPLAY_SIZING`). Read-only: nothing is frozen, recorded or sent, and the
  evaluator stays the only place the rule runs (`TestOnlyTheDaemonEvaluates`
  lists `Replay`). The series-file harness
  (`TestStrategyReplayFromSeriesFile`, env-gated) runs the same function
  over a `canary market --symbol SYM --range 5Y --exchange X --currency C
  --json` dump, for a daemon that predates the method or a gateway that is
  down. A CLI flag, not a test, is the home: the owner runs it monthly,
  and the daemon owns the data it is judged on.
- Lab: `lab replay-table SPEC --write` (the table on the spec's
  `data_requirements.calendar` instrument from the lab's data, committed
  under `tests/fixtures/replay/`; `--symbol` names another line) and `lab
  replay-compare SPEC CANARY.json [--series]`, which reports every
  disagreement and its cause: calendar, data source, precision, terms,
  rule, history, carried, rounding (`lab/replay.py`).

Monthly, after the first session of a new month and before the row is sent:

```sh
canary strategy core --replay 2021-10-01 $(date +%Y-%m-%d) --json > ~/core-replay.json
cd ~/dev/lab && uv run lab replay-table s0_core_sleeve --write --refresh
uv run lab replay-compare s0_core_sleeve ~/core-replay.json                      # the spec's line (Xetra)
uv run lab replay-table s0_core_sleeve --symbol IUSE.L --out /tmp/iuse.json && \
  uv run lab replay-compare s0_core_sleeve ~/core-replay.json --lab /tmp/iuse.json  # the line the daemon trades
```

The exit code is 1 on any disagreement. The replay JSON and the dump are
runtime data and stay outside Git; the lab's table is public market data.

First run, 2026-10-19 08:25 CEST, the core over 2021-10-19 to 2026-09-30
(IBKR's five years for the fund's London line, IUSE on LSEETF in EUR, the
listing IBKR gives for the ISIN in EUR; the lab on Yahoo). Rule: identical.
On the 48 month-ends both sides decide (2022-10 to 2026-09; the twelve
before are `history`, the daemon's window not yet filled), the trend reads
the same every month, the paper path buys and sells every share in the same
months, and the volatility and the target differ only where the closes do.
Three data findings, no defect:

1. Venue: the spec (`s0_core_sleeve`) names Xetra (IBCF.DE); the daemon
   trades the London line (owner decision 2026-10-18 08:06 CEST: the venue
   comes from IBKR's record). The two calendars differ in 34 sessions over
   the span (UK bank holidays; Xetra closed on 24 and 31 December and 1
   May), four month-ends fall on different sessions (December 2021, 2024
   and 2025; August 2026), and the target differs by up to 4% (2025-05-30:
   540 shares on Xetra closes, 519 on London's). The committed table is the
   spec's line; the London line is compared with `--symbol IUSE.L`. Whether
   the spec should name the London line or the policy pin `exchange =
   "IBIS"` is the owner's.
2. Thin prints: on the same London line, Yahoo and IBKR disagree on 66 of
   1,260 closes beyond 1e-5, up to 2.4% (2025-05-01, a day Xetra was closed:
   IBKR 115.24, the day's low on 7,630 units; Yahoo 118.12) and 1.2%
   (2026-08-28: IBKR 157.52, the low, on 8,100 units; Yahoo 159.35; Xetra
   158.475). IBKR's TRADES close is the last print of a thin line; the
   daemon's volatility, and so its target, carries that noise (target
   523 vs 519 on 2025-05-30). The 8% jump guard does not see a 1 to 2%
   stale print.
3. A gap: IBKR has no bar for 2025-09-03 on the London line (Yahoo does).
   Before 2026 the embedded calendar does not cover the venue, so the
   evaluator reads the series' own dates as the calendar and cannot name a
   missing session; from 2026 it holds the month ("no close on …"). No live
   decision falls before 2026.

Terms: the spec's params carry the 10% band and no minimum; the policy
writes `min_order_base = 2500`. The compare says so under `terms` every run
until the spec names it; the owner's number, not a defect.


## 10. The regime gate (2026-11-03)

Written 2026-11-03 13:42 CET. Desk's trading-machine spec TM-04.08, on the
owner's decisions of 2026-11-03 13:29 CET (the decision list's D1 to D6,
D3 at two closes). Code: `internal/core/regime_gate.go` (the arithmetic),
`internal/daemon/regime_gate.go` (inputs, cadence, serving).

- **Readings.** On the latest settled US close (spx.CompletedSessionKey):
  trend, SPY's close above the mean of itself and the nine month-end closes
  before it, which at a month-end is the library's trend-sma10m exactly (a
  test holds the two equal); breadth, the breadth engine's share of S&P 500
  constituents above their 50-day average, above `breadth_min_pct`; credit,
  HYG's close above the mean of its last 50 closes. SPY and HYG come from
  the regime engine's history cache (the same entries); a copy fetched
  before the session's close plus 20 minutes is fetched again, and its bar
  for that session is never read as the close.
- **States.** Three on: on, scale 100. Two: half, `half_scale_pct`. One or
  none: off, scale 0, no new entries and no adds; open positions are left
  to their stops.
- **Confirmation and staleness.** A reading flips only when
  `confirm_closes` consecutive observed closes agree, walked over the last
  30 sessions, so the state is a function of the history and no restart
  loses it. A reading whose latest close is more than
  `stale_after_sessions` sessions behind the latest session, or that has
  none, counts off with its reason.
- **Policy.** `[mandate.regime_gate]` `breadth_min_pct = 50`,
  `half_scale_pct = 50`, `stale_after_sessions = 1`, `confirm_closes = 2`,
  written once at daemon start as the second defaults batch (the first is
  2026-10-29's), recorded under its own key so a first-batch table the
  owner deleted stays deleted. Absent or half-written, the gate reads off.
  The ten months and the 50 days are the spec row's, compiled.
- **Scope.** The gate is measured every 15 minutes beside the mandate's
  scheduled verdict and served on `mandate.risk` as `regime_gate`. It
  scales only rules that declare it: strategy one's rule (TM-03.01) when it
  lands. The core and s6-trend do not read it; the owner sleeve is never
  scaled.

## 11. Strategy one's screen (2026-11-03)

Written 2026-11-03 14:04 CET. Desk's spec TM-03.01, on the owner's
decisions of 2026-11-03 13:29 CET (the decision list's A2 and B1 to B10).
Code: `internal/core/strong_names.go` (the rule, `strong-names-v1`),
`internal/daemon/strategy_one.go` (inputs, served as `strategy.one`).

- **A rule of its own, not a library filter.** The evaluator above holds
  one fund on a trend filter; strategy one screens many names. Its numbers
  (B1 to B9) are compiled with the rule like the core's, so a change is a
  new rule and a new lab spec version (`s7_strong_names`), never a policy
  key. How much budget it may use is the register's risk allocation.
- **Universe and history.** The S&P 500 constituents the breadth engine
  keeps: 253 closes per name, dated on the US calendar back from the
  window's last bar and checked against its 60 dated bars, which carry the
  volumes; a window that disagrees is left out and counted. The 50-session
  watch-list walk fits inside those closes with every 200-session average
  defined. SPY is the regime engine's cached series under the settled-bar
  rule of section 10; highs and lows for the average true range are read
  only for the names a plan needs.
- **Stateless.** The watch list is walked from the history, so restarts and
  rebuilds never change who is on it.
- **Zero allocation (A2).** No register entry names the rule, and the
  loader refuses an allocation of zero, so the strategy stays out of the
  register until the TM-01.01 bar is met or the owner writes a number. The
  screen sizes each plan by the rule and buys nothing; it proposes and
  sends nothing. Adds and exits (B7, B8) are computed for open names, and
  there are none.
- **Not yet built.** The 2.5-ATR trailing stop under the highest close
  since entry needs the entry record a live sleeve keeps; the five-session
  judgement of an add is the scorecard's. Both arrive with a register
  entry. Watch-list names outside the S&P 500 are not screened yet.
