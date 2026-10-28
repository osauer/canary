# Mandate risk budget (ex-ante volatility budget and the additions gate)

Updated: 2026-10-23 19:30 CEST (options at the gate, the override and the
exposure view, "Options, the override and the exposure view" below)
Status: increment 1 implemented 2026-10-14 (measurement, verdict, additions
gate); increment 2 implemented 2026-10-14 (forced reduction, below);
increment 3 implemented 2026-10-18 (allocations, below); increment 4
implemented 2026-10-19 (the owner sleeve's drawdown stop, "Risk allocations"
and "The forced reduction" below; its release band added 2026-10-18 22:00
WEST, "Engaged, held and released"). The feature is inactive until the owner
writes `[mandate]` into `risk-policy.toml`; the forced reduction places
nothing by itself until the owner lists `mandate_reduction` under
`[authority].pre_authorised`; the owner sleeve's stop runs only once
`[mandate.sleeves] owner_stop_pct` is written.

This record follows `.agents/docs/risk-policy-contract.md` and the
daemon/CLI trading contract. It records human policy decisions; it creates
none. Every number below that acts was written by the owner or is a
proposal the owner confirms by writing it into the policy file.

## Decision

- Goal and protected asset or behavior: keep the book's loss measure (the
  premium every bought option can lose plus the cash loss to the stop of
  every stopped share line) inside the loss room derived from the owner's
  maximum tolerated loss, and refuse any order that adds risk while the
  book is outside that room or while it cannot be measured. Until
  2026-10-27 the headline was the book's ex-ante volatility against a
  volatility budget derived from the same loss; the volatility stays
  computed as the sizing input of the sleeve allocations ("The loss measure
  as the verdict's headline" below). Protected asset: the risk base
  (account equity after the tax reserve). Protected behaviour: no
  risk-adding order while the book is over budget or unmeasured.
- Policy owner: the desk owner (single-trader desk).
- Human decisions, recorded verbatim from the chat of 2026-10-14 06:26 to
  06:52 CEST:
  - Maximum loss D = 30%, measured on account equity minus a tax reserve
    ("Tax comes from this account"; the reserve is an owner number,
    currently estimated EUR 70,000), counted fresh from 2026-10-14 with a
    cash-flow-adjusted high-water mark ("Start fresh today").
  - Volatility target = D/2 = 15% a year of the risk base. Since
    2026-10-27 19:50 CET it sizes the sleeve allocations and the machine
    strategies; the verdict compares the loss measure with the loss room
    (spec TM-04.12, "The loss measure as the verdict's headline" below).
  - Protection that acts: on a budget breach, a reduce-only batch back to
    budget, largest risk contributor first, pre-authorised with the
    30-minute veto; release when risk is back in budget, not when P&L
    recovers. Built in increment 2 ("Forced reduction" below) on the
    measurement and the additions gate of increment 1. The order of the
    cuts was replaced on 2026-10-18 08:06 CEST (allocations, below): the sleeve
    furthest over its allocation first, within it the largest contributor;
    and again on 2026-10-27 19:50 CET (spec TM-04.12): the largest premium
    at risk per name first, until the book fits the maximum loss.
  - Sleeve volatility shares of the 15%: owner 7, core 6, overlay 4. Recorded
    here as `[mandate.sleeves]`; enforcement per sleeve comes later. Since
    2026-10-18 the machine's `[mandate.sleeves]` numbers size its allocations
    ("Risk allocations" below); the owner's allocation is the rest of the
    budget, and `owner_vol_pct` is recorded and not read.
- Departure from risk-policy.md decision 3 ("No auto-increase: deposits,
  profits, and live events never raise declared risk capital"): the
  mandate's budget follows the high-water mark of the cash-flow-adjusted
  risk base, so it rises with gains and falls with flows. This is
  deliberate and owner-decided ("Start fresh today", a high-water mark). The
  declared risk capital, the protected floor and the drawdown brake are
  untouched; the owner switches those numbers separately. The two controls
  coexist: the brake refuses on realised drawdown of declared capital, the
  mandate refuses on ex-ante risk against a budget that moves with the book.
- Current authoritative document/code/fingerprint: `risk-policy.toml`
  `[mandate]` (part of `risk-constitution-fp-v1` once written; absent it
  stays out of every fingerprint, like `drawdown.release`); code
  `internal/risk/mandate.go` (schema), `internal/mandate` (the pure
  evaluator), `internal/daemon/mandate_*.go` (capital state, history store,
  verdict, gate).
- Status: implemented (increments 1, 2 and 3, allocations). Active only with
  `[mandate]` written; the forced reduction is automatic only with
  `mandate_reduction` pre-authorised.

## Meaning

- Capital or exposure base: the **risk base** = account equity (NLV in the
  base currency) − `mandate.tax_reserve_base`. The **budget base** is the
  high-water mark of the cash-flow-adjusted risk base since
  `mandate.started_at`, translated into today's capital terms: `budget_base =
  HWM(equity − flows_since_start) − tax_reserve + flows_since_start`. It rises
  with every new high, once a second reading at least five minutes later
  confirms it (at the lower of the two), and moves one-for-one with declared
  or statement flows after the start, so a withdrawal lowers the budget and a
  deposit raises it, while neither creates a drawdown or a false high. The tax
  reserve shifts both the high-water mark and the current risk base by the
  same amount, so changing it never changes the drawdown, only the base.
  Changing `started_at` starts the series again (a fresh high-water mark
  seeded from the next observation).
- Drawdown and room: `drawdown = max(0, HWM − adjusted risk base now)`,
  `drawdown_pct = drawdown / HWM`, `room = clamp(1 − drawdown_pct / (D /
  100), 0, 1)`. Room is 1 at a new high and 0 once the loss reaches D.
- Budget: `budget_annual_sd = vol_target_pct / 100 × budget_base × room`.
  With D = 30 and the target 15, the budget is half the remaining loss room
  per year of volatility; at room 0 the budget is 0 and any measured risk
  is over it.
- Aggregation unit: the whole account, per position (exact contract) and
  per underlying for contributions; per underlying for gap stresses.
- Measurement: historical simulation over the last 252 daily log-returns on
  dates every needed series shares, with full revaluation of every leg:
  - stock and ETF: linear in the underlying's scenario return;
  - option: Black-Scholes (European, rate 0, dividend yield 0) at the leg's
    current implied volatility as IBKR's model reports it on the position
    row, the underlying moved by the scenario return and one calendar day
    of time decay; a leg whose IV is missing makes the verdict unknown (no
    guess);
  - a leg in another currency: its scenario value is converted to base
    with that currency's scenario FX return, read jointly from the same
    date grid (the EUR.USD series for USD legs; `EUR.<CCY>` for any other
    held currency);
  - bills, bonds and currency conversions carry no equity or volatility
    risk here and are listed as not modelled; any other security type
    (future, index, CFD, fund, warrant) makes the verdict unknown.
  Outputs: 1-day P&L sd and annualised sd (×√252) in base currency; 99%
  expected shortfall (the mean loss of the worst ⌈1%⌉ scenarios); Euler
  risk contributions per position and per underlying
  (cov(leg, total) / var(total), which sum to 100% of the total by
  construction); the FX share of the risk (the same decomposition of each
  leg's base P&L into its local-currency part and its currency part);
  named stresses: each underlying alone −`gap_shock_pct`, and
  +`gap_shock_pct` where a short-call or short-stock leg loses on the
  upside; the index −10% applied through each name's historical beta to SPY
  over the same 252 returns (SPY itself at beta 1).
- Gap cap per underlying: `room × D / 100 × budget_base × gap_room_pct /
  100`; a stress loss above it is a breach.
- Broad index gap (owner decision D4, 2026-10-14, recorded in
  `core-sleeve.md`: "12% for broad index funds"): an underlying whose
  every leg is on the compiled list `mandate.BroadIndexInstruments` (SPY,
  SPX, SPXW, XSP, QQQ, NDX, IWM, RUT, DIA in USD; IBCF, SXR8, CSPX, VUSA,
  VUAA, EUNL, IWDA, SWDA, VWCE, VWRL, VGWL by ISIN-named listing; matched on
  symbol and currency together) is stressed by `index_gap_shock_pct`
  instead of `gap_shock_pct`; single names keep 25. The key is optional:
  absent, every underlying takes `gap_shock_pct`, and a file without it
  keeps its fingerprint. The forced reduction's gap cut and the additions
  gate read the same per-underlying gap.
- Returns proxy (core sleeve design review S5, 2026-10-14): IBCF in EUR,
  the S&P 500 hedged to EUR at fund level, moves in the simulation with
  SPY's local-currency daily returns, with no currency leg in a EUR book.
  Its own Xetra close (17:30 CET) is four and a half hours before the US
  close every other US line is measured at, and asynchronous closes
  understate its correlation with them. The book therefore reads no IBCF
  series, so a missing or stale one can never make the verdict unknown;
  an IBCF order the book does not hold is priced at its limit, and one
  without a limit is refused. The list is compiled
  (`mandate.ReturnsProxies`).
- Threshold or rule, including units: `max_loss_pct` (D, percent of the
  budget base), `vol_target_pct` (percent a year of the budget base),
  `tax_reserve_base` (base currency), `started_at` (date, YYYY-MM-DD),
  `gap_shock_pct` (percent; the proposal is 25), `gap_room_pct` (percent of
  the remaining loss room one name may lose in a gap; the proposal is
  33.3), `[mandate.sleeves]` `owner_vol_pct`, `core_vol_pct`,
  `overlay_vol_pct` (percent a year; the machine's two size its allocations, the
owner's is recorded and not read: "Risk allocations" below). The
  proposals are written into the file by the owner; the code carries no
  default that acts. A `[mandate]` table missing a material key is active
  and unknown: the verdict names the unapproved keys and additions are
  refused until the owner writes them (data absence never renders ok).
- Verdict: `within` (the loss measure ≤ the loss room and no gap breach),
  `reduce` (the loss measure > the loss room, or any gap-cap breach),
  `unknown` (anything the measurement lacks). Until 2026-10-27 the verdict
  compared the volatility with its budget and read `over` between the
  budget and 1.25 × budget; `over` is no longer produced. The result carries
  `status_since`, when the status last changed as the daemon has seen it
  across evaluations and restarts (a `mandate_status` state document; each
  change journaled as `mandate_status_changed` with how long the old status
  held), because a machine sleeve cannot trade while the verdict is unknown
  and the owner asked how often and how long it is (2026-10-18);
  `canary mandate` prints it as "Since".
- Enforcement class: pre-trade hard gate for additions (`mandate_budget`
  blocker), at the same four enforcement points as the drawdown brake (order
  preview, strategy preview, admission, the final wire guard), with the
  brake's risk-adding classification: a close or reduce passes, a BUY of a put
  on a Rulebook hedge symbol passes, cancels never reach it. One exemption is
  the mandate's alone: a BUY of a bill (security type `BILL`, as IBKR names
  the line) passes whatever the verdict reads, as a held bill is measured as
  carrying no equity or volatility risk (2026-10-17 06:47 CEST, owner: "If
  cash like investments do not count against it, I'm fine with 15/30"). A bond
  (`BOND`, including a bill IBKR lists as a bond) stays refused, because its
  interest-rate risk is not measured and a long bond could carry real risk
  counted as zero; a bond fund (`STK`) is measured as before; the drawdown
  brake keeps refusing a bill purchase while engaged. The exemption is not
  widened further. Allocations (2026-10-18, "Risk allocations" below): an
  addition passes only when its own sleeve is inside its allocation and stays
  there with the order added, on top of every whole-book check here. Release
  is automatic: the moment a fresh verdict reads `within`, additions pass;
  there is no latch and no reset. The incremental pre-trade check re-evaluates
  the book with the proposed leg added and refuses an addition that would take
  the annualised risk over the budget even while the standing verdict reads
  `within`, and one that would create a gap-cap breach or deepen one
  (2026-10-14, the core sleeve's design review B1: an addition the gap caps
  refuse would otherwise be admitted and then sold back by the forced
  reduction). An option order the evaluator cannot price (no limit, a limit
  below intrinsic value, an expiry reached, an implied volatility that does
  not solve) fails closed (2026-10-14, the same review): a sale is refused, as
  a short option's loss has no bound without a volatility; a buy is judged by
  a volatility-free bound, its premium at the limit added to the underlying's
  gap loss, and √252 × units × (the largest rate × the root mean square of the
  underlying's daily moves × spot, plus the call's spot or the put's strike ×
  the rate's moves) added to the book's risk (a vanilla option's price moves
  at most as much as its underlying, and the decay shifts every scenario
  alike), and refused when either does not fit or without a limit. A held
  contract with its IV is priced as before. An addition on a name or in a
  currency the book does not hold is measured on the series the history store
  keeps for it (spot from its last close when the book holds none); while none
  is fresh, the order is refused (`mandate_budget`: "no daily history for X
  yet; the desk is fetching it", or the base currency's exchange rate against
  the order's) and the series is requested on the background lane under the
  same budget, so a retry after the fetch is measured. A book that cannot be
  measured with the order added refuses it too; nothing passes unmeasured
  (2026-10-14 16:57 CEST, from the core-sleeve design review).
- Unknown, stale, partial, and unavailable-data posture: the verdict is
  `unknown`, and additions are refused, when the equity observation is
  missing or older than `capital.max_equity_age_minutes` (or that horizon
  is unapproved), the base currency is unproven or differs from
  `capital.base_currency`, the positions read is not current for the
  selected account, any held equity or option leg lacks a decision-grade
  series or its series is stale, fewer than 253 shared dates exist, an
  option leg lacks its IV or spot, a leg lacks an FX rate, the cached
  verdict is older than five minutes, the mandate section is incomplete, or
  an equity move the book's P&L does not explain suggests an undeclared
  cash flow (see Forced reduction).
  Every reason is named on the verdict. Nil is unavailable, never zero.
  The verdict is judged at a clock read once the capital state is in hand,
  so an equity observation that lands while the evaluation starts (its own
  account read, or another the store was persisting) is current, never one
  from the future (2026-10-16: scheduled verdicts read unknown while
  reads measured).

## Forced reduction

Owner decisions, recorded verbatim:

- 2026-10-14 06:26 CEST, decision 3 "Protection that acts": "on a budget
  breach, one reduce-only batch sized back to budget, largest risk
  contributor first, pre-authorised with the 30-minute veto. The brake
  releases when risk is back in budget, not when P&L recovers." Owner: "yes
  to all".
- 2026-10-14 06:26 CEST: "The system should run by itself within risk
  boundaries."
- 2026-10-18 08:06 CEST, replacing "largest risk contributor first" in
  decision 3: "Accept all three: give the machine its own seat (replacing D1
  and largest-contributor-first), put s1 into its six-month shadow test, and
  go for the core venue fix with the core fund out of protection's automatic
  sales." The recommendation accepted (mandate budget review, 2026-10-17,
  decision 1): "When the book is over budget, the sleeve furthest over its
  seat is cut first." The 30-minute veto and every other rule here are
  unchanged ("Risk allocations" below).
- The bucket ships absent from the embedded default and inactive; the owner
  arms it by listing it under `[authority].pre_authorised`.

Revised 2026-10-14 10:17 CEST after the independent safety review of the
first build (coverage, a batch that never shrank, the veto clock, the
prepared path, netting and the debounce), and again 2026-10-14 16:48 CEST
after the second review (a veto clock that let a row grow unboundedly,
covered calls, protective puts at exercise, undeclared cash flows, the
gap-breach trigger, a DAY order that expired unfilled). Decision relayed
with the second review: the batch does not buy back short calls and does
not cut the shares that cover them; a covered position needs the owner's
hand (the owner's policy forbids selling options to open, so short calls
come only from hand trades, and a two-leg unwind needs a combination order
Canary does not route automatically).

What the daemon does (`internal/daemon/mandate_reduction.go`, bucket
`mandate_reduction`):

- **Trigger.** While `[mandate]` is written, the proposal engine reads the
  kept verdict for the policy in force on every refresh. The verdict cache
  counts a streak of scheduled verdicts (the background cadence, every two
  minutes, which a read's own evaluation never moves) reading over or
  reduce, each counted reading at least five minutes after the last one
  and at most fifteen; a read's evaluation never counts, and any reading,
  scheduled or not, that is neither over nor reduce resets it, as does a new
  policy. A batch starts on two counted readings, whether over, reduce or a
  gap-cap breach, so polling cannot build it in seconds and one bad reading
  cannot start it; only a running episode carries a breach on without them.
  One reading reports `watching` and proposes nothing. `unknown` never
  sizes anything.
- **Episode.** The first batch opens a breach episode (`episode` on the
  status and on every row); it lasts until a verdict reads `within`.
- **Target and hysteresis.** The batch is sized to the loss measure ≤ 0.95 ×
  the loss room with every gap cap met, so the next verdict reads `within`
  rather than `reduce` by a hair; release is at `within` (the loss measure
  ≤ the loss room, no gap breach). Until 2026-10-27 the target was 0.95 ×
  the volatility budget. The 5%
  band is compiled (`mandateReductionTargetFactor`), not a policy key.
- **Sizing, afresh at every verdict.** Greedy, with the daemon's own evaluator
  on the book the verdict measured; a standing batch is never carried forward
  as it was. Cut by cut, the book re-evaluated after each: a name over its gap
  cap has its largest gap loser cut until the name's gap loss is under the cap
  (a gap cut never leaves the book riskier); with no breach left, the name
  with the most premium at risk is cut first and, within it, its largest leg
  (2026-10-27, spec TM-04.12, "The loss measure as the verdict's headline"
  below; from 2026-10-18 to 2026-10-27 the sleeve furthest over its
  allocation first, "Risk allocations" below), by the smallest whole step
  that reaches the target or, when no step on that line does, by the step
  that takes the most loss off; the next cut is chosen afresh on the book
  re-evaluated, and follows only when needed. Steps are contracts for options and share lots for stocks (1
  below 100 shares, 10 to 999, 100 from 1,000). Damping: a line whose position
  has not moved keeps the last generation's cut when the fresh cut is smaller
  by at most one step, so a steady risk does not re-size a row every verdict;
  a larger ease shrinks it, a rise grows it.
- **Covered and protected shares.** The batch never buys a short option
  back and never sells the shares that cover short options of their own
  underlying (short calls over long shares, short puts over a short sale):
  those shares and those options are left out and named, the batch cuts
  other contributors instead, and where the target cannot be met without
  them the status says "a covered position needs your hand" and names
  them. Shares above the cover are cut normally. Nor are shares ever cut
  below what the long options on them would deliver or take at exercise
  (long puts on a long line, long calls on a short sale), so exercising
  them never flips the line; those options are protection and never sold
  (the budget governor's test, `budgetLegIsProtection`, which also keeps
  hedge-listed puts and legs the option policy derives as protection).
  Every order the batch may send alone lowers the book's risk.
- **Expiry.** An in-the-money long option within three US option sessions
  of expiry is cut ahead of the batch only where its exercise, as delivered
  stock, would by itself push the planned book over budget (and add risk),
  by the fewest contracts that keep the exercised book inside the target;
  the batch is then sized again around that cut. An option whose exercise
  changes little is left to the variance.
- **Working orders.** A line a working stop or trail holds leaves the plan
  (netting would hold its row as long as the stop works), the next
  contributor is cut instead, and the line is named under `left_out`. A line
  with an immediate order in flight (a sale partly filled, the rest
  working) stays in the plan, since that cut is on its way; its row waits
  behind the working order (netting) until it fills or is cancelled.
- **Every row** is reduce-only (a sale of a long line, a buy-back of a short
  one, never past zero), a DAY limit at the mark like the budget governor's
  rows, `never_skip_veto`, and carries the verdict it was sized on
  (`mandate.verdict_status`, `verdict_at`, `verdict_fingerprint`), its
  episode, rank, phase (`expiry`, `gap`, `risk`), the line's share of the
  risk, the risk the cut takes off and the risk after the batch.
- **Order caps.** One order is held to the lower of
  `[buckets.risk_reduction].max_order_notional` and the `[order_limits]` cap
  in force, at the row's mark; `mandate.units` keeps the whole cut and the
  status `plan` both numbers. After the fill the next verdict measures the
  book as it then is and sizes the remainder. As in the governor, an order
  is never held below one unit: one contract or share above a cap is the
  order gate's to judge, and it passes only as a delta-reducing exit.
- **Veto clock within an episode.** A row re-sized within its episode
  (its revision changes) keeps the veto clock of the record it replaces,
  which never reached the broker, only while its size stays within one lot
  above the size that clock, and its notice, was started for: shrinking,
  or growing by up to a lot, keeps it, so a batch re-sized at every verdict
  is never starved. Growing further starts a fresh clock: a new notice (a
  new alert episode, keyed by the line, the episode, the clock and its
  size; the push copy itself carries no figures by the presentation
  contract, the Protection row shows the order) and a full window, once
  per such growth, never per verdict, so "sell 100" can never become
  "close 3,000" on the old clock. A record whose clock started before the
  regular session it would be sent in, as after an overnight hold, never
  goes out on it: when that session opens it gets a fresh notice and a full
  window. The owner's veto holds the line for the rest of the episode,
  whatever its size; a sent, failed or otherwise ended record passes no
  clock on. Each order stays held to the caps.
- **One session's order.** A row's revision includes the trading date
  (New York) of the verdict it was sized on, so a DAY order that expired
  unfilled (its record terminal for that revision) is proposed afresh, with
  a fresh window, the next session instead of stranding the batch.
- **Hold.** A verdict that is `unknown`, older than five minutes, or
  measured on another book than the one held (a fill, a hand trade:
  `book_changed`) sizes nothing. Standing rows are served held
  (`mandate.held`, `hold_reason`): preview, submit, the device-confirmed
  prepared submit and the owner's order queue refuse them
  (`mandate_sizing_held`), the scheduler holds a due record on its original
  window (`automaticHoldMandateVerdict`) and the notice reads "Reduction
  held" (`protection_auto_held_mandate`). A held row whose own line moved is
  dropped.
- **Release.** A verdict reading `within` ends the episode and withdraws
  every standing row; a waiting automatic record is superseded and never
  sent. Nothing is cancelled at the broker: an order already working stays.
- **One row per contract and side.** Where another bucket (the budget
  governor, the issuer trim, theta hygiene) also wants to sell the line, the
  same-contract merge keeps one row sized by the largest need, never the sum
  (precedence on a tie: after the governor, before theta hygiene). A held
  mandate row takes no part in the merge. While the drawdown brake is
  latched, a pre-authorised stock trailing stop is placed at once and a
  mandate sale waits its full window, so the sale never covers the stop
  then: the stop goes first, and netting holds the sale behind it.
- **Submission.** Only the existing scheduler and `submitAutomatic`: one
  durable record per key and revision, the notice, the full veto window even
  under a latched drawdown brake, one grant under `brokerWriteMu`, origin
  `daemon-preauthorised`, every write gate unchanged. Records are dispatched
  in rank order within a cycle; one row's refusal never holds the others.
  Not pre-authorised, the rows are ordinary proposals that wait for the
  owner's hand. They are not queueable for the open. At a stress open they
  wait 30 minutes after the options open, like the governor's rows.
- **The maximum loss.** At a drawdown of D the room, the budget and every
  gap cap are zero, so every name is in breach and the batch acts on one
  reading: it reduces the book to what it may not cut (hedge-listed puts,
  protection, multi-leg units, ignored rows). At the maximum loss the
  mandate reduces the book to its hedges (owner, 2026-10-14 ~16:10 CEST, in
  chat: "Yes, flatten to hedges"; the 30-minute veto still applies). The
  verdict's ratio to a zero budget is unbounded and encodes as
  `ratio_to_budget: null` (read back as unbounded), so the verdict keeps
  serving instead of failing to encode.
- **A glitchy equity reading.** A new high-water mark counts once a second
  reading at least five minutes later confirms it, at the lower of the two
  (`mandate_capital.go`): one spike never raises the mark, which would read
  as a drawdown from then on. A single low print that collapses the room
  breaches every gap cap but is one reading: it only watches.
- **Undeclared cash flows.** The owner withdraws regularly, and a flow not
  yet declared moves equity: a withdrawal would read as a drawdown (less
  room, a smaller budget and gap cap, a batch), a deposit would raise the
  high-water mark and read as a drawdown once declared. With every equity
  observation the capital state keeps the broker's daily P&L of the same
  read and judges the unexplained part of the move since the last
  observation: the move net of declared and statement flows, less the
  P&L over the same span: the difference of the two daily figures within
  one broker day, the new day's own figure across the broker's daily reset
  (the old day's last reading taken as its close); zero where the broker
  reports none, so without P&L the whole move counts. The reset is read
  from the figures themselves and from the session date, never from the
  calendar alone: IBKR starts its day at its own hour, before the New York
  midnight the session date turns at, so a reading whose daily figure sits
  within a quarter of the flow threshold of zero while the equity moved by
  no more than that figure is the first of a new broker day, and so is a
  reading in a new session date (2026-10-18 after the 2026-10-16 incident:
  a reading at 23:59:59 New York still carried the trading day's session
  while the broker's figure was already the new day's zero, and the
  difference charged a whole down day to a flow; every machine sleeve was
  refused from Friday night on, and Saturday's statement did not clear it,
  below). A flow moves the equity: only a reading whose equity move, net of
  declared flows, is itself beyond the threshold can raise a suspicion
  (2026-10-18 21:30 CEST: two readings with the equity unchanged to the euro
  and the broker's figure a day's P&L apart, one at a daemon reconnect on a
  Sunday, read as flows and held the room for days; a record raised before
  this rule clears on the next reading when the equity has not moved beyond
  the threshold since its base, `legacy_flat_equity` in the journal). More
  than max(1% of equity, 1,000 in base currency) unexplained, on such a
  move, is a suspected flow (`flow_suspect`, journaled
  `mandate_flow_suspected` with `equity_move_base`). A cash flow moves cash,
  a transfer in kind moves quantities, and a mark moves neither, so since
  2026-10-19 13:30 CEST the reading that would raise a suspicion also reads
  the broker's total cash (the account read's `total_cash`, kept on each
  observation as `cash_base`) and a fingerprint of the book's quantities
  from the portfolio cache, taken only while that cache is current and in
  scope (`positions_key`: contract and quantity of every non-flat row,
  sorted and hashed; marks and valuations left out): when both readings
  carry both, the cash moved, net of declared and statement flows, by no
  more than the threshold, and the quantities are the same, the equity
  move is the marks', and the reading is journaled `mandate_mark_move`,
  never a hold, with two residues no reading can tell apart: a quantity
  change booked in the seconds between the broker's position push and the
  account read is not yet in the cache (a transfer in kind landing in that
  window passes until the statement incorporates it), and a cash posting
  from an accrual the equity already carried (a dividend or interest paid
  out) can net a withdrawal of its size in the same two minutes. The incident: on 2026-10-19 at 08:01
  UTC, when IBKR's pre-market opened at 04:00 New York, the book was
  re-marked (3,965 up) while the daily figure still read the new day's
  zero; the rule held every machine sleeve, the standing reductions and the
  owner's additions until the next statement, and would have done so on
  every such morning. What stays held as before: a deposit spent on a
  purchase in the same reading, a transfer in kind, an assignment or an
  expiry (the quantities change); a morning where cash itself carries the
  move, as a futures variation margin, the revaluation of foreign-currency
  cash or a weekend assignment can (the cash moves); and any reading
  without the cash figure or a current positions cache. Cash and quantities
  are read only at the raising reading, never to clear a hold: over hours
  trades move cash into positions and back, so an unchanged figure says
  nothing then. Taken by the integrating session on the owner's standing
  instruction that the machine runs by itself (refuted twice, 2026-10-19
  13:05 and 13:30 CEST: the first version read cash alone and would have
  passed a transfer in kind); the owner may reverse it. The price of the
  rule: a
  flow that lands in the same reading as an opposite market move of about the
  same size, netting under the threshold, is not suspected until the next
  statement incorporates it; readings run every two minutes in the session, so
  the exposure is a gap, overnight or a weekend, and one statement long. The
  clearing rule for records from before it was dropped in v3.23.2, once the
  one such record had cleared on the v3.23.1 install. The alternative, to
  hold the room at every reading a reset cannot be told from an offsetting
  flow until a statement reconciles it (proposed as Canary issues #109 to
  #111 on 2026-10-18 evening), was declined by the integrating session at
  22:15 CEST under the owner's decision of that evening ("yes, patch" to the
  fix for the false weekend and restart suspicions; the owner's standing
  priority that the machine runs by itself): it would hold every machine
  addition for a trading day after every daemon restart. A flat-equity jump
  of the figure is journaled as `mandate_pnl_basis_jump` for the record; the
  floor stands for the accruals a broker posts to cash that its daily P&L
  may not carry (interest, a dividend, a fee), and a flow that small moves
  the budget by under 150 a year at the 15% target. While suspected the
  high-water mark does not move and the capital reads unknown, so the
  verdict is unknown: additions stay refused, no new mandate rows,
  standing rows held. The unexplained part is then measured cumulatively
  from the observation before the suspicion, the P&L summed step by step,
  so a later P&L move never offsets it (a 3% withdrawal stays suspect
  through a +1% day; an 8% withdrawal on a −4% day is flagged). It clears
  (`mandate_flow_cleared`) only when that cumulative part is back under
  the threshold, because a declared or statement flow accounts for it or
  the reading that raised it is undone (a glitch), or when a statement
  covering its day has been reconciled: its day is the New York date of
  the reading that raised it, the date IBKR's statements run by, so a
  reading after UTC midnight but before the New York date turns belongs
  to the trading day still open and that day's statement covers it (the
  first build compared UTC dates, and Friday's statement, fetched Saturday
  morning, never covered a suspicion raised at 23:59 New York Friday).
  Owner instruction recorded 2026-10-19 06:35 UTC: close #109 as the
  owner's documented decision; retain the net-equity admission filter and
  do not reinstate the residual-only rule declined on 2026-10-18 22:15 CEST. Fix #111 only as the two-deposit clearing
  hole: once a suspicion exists, accumulate P&L using the same-session
  difference or the new session's own figure, without treating a near-zero
  figure as evidence of a reset. Otherwise a second deposit offset by a
  loss can disappear from the residual and declaring only the first clears
  both. The net-equity admission filter and its numeric reset heuristic
  remain in force for readings without an existing suspicion. A held
  reading compatible with both a reset and continuous P&L persists its
  ambiguity immediately: neither numerical recovery nor even a full flow
  declaration proves the basis. Only a statement covering the latest such
  reading clears it. Otherwise a real reset from negative P&L to zero can
  itself look like a gain that erases a deposit. This retains an existing
  hold; it does not create the declined new hold.
  How the owner clears it: declare
  the flow (`canary policy capital-event deposit|withdrawal --amount …
  --effective-at …`), or let the next statement reconciliation cover the
  day. A suspicion nothing explains stays in the safe direction, the room
  held, until one of the two (revised 2026-10-14 17:27 CEST after the
  third review: the first version let a later P&L move offset a flow and
  did not judge a move without P&L).
- **Surfaces.** `canary proposals` (and `--details`, JSON, MCP
  `canary_proposals`) lists the rows with bucket, rank, share of the risk
  and the verdict in plain words under a Mandate header; `canary mandate`
  (and `canary_mandate`) shows the batch, the rows pending and the veto end.
  The pre-authorised notice is `protection_auto_mandate_reduction` (no "now"
  variant). The scorecard counts the fills as `rule`
  (`proposal:mandate_reduction`), never the owner's.
- **Residual risk accepted.** The sizing inherits the measurement's limits
  (one year of history, today's IV, no rates or dividends). A covered
  position, and shares long options would deliver, are never reduced by
  the batch: where they carry the breach, the book stays over budget until
  the owner acts. A line a stop holds is not reduced while the stop works.
  Within an episode a row may grow by one lot on its clock: the owner sees
  the current size on the Protection row, not in the push. The flow check
  reads the broker's daily P&L: a P&L feed that lags a fast market, or
  is missing, can suspect a flow (the safe direction: the room is held)
  until it catches up, the flow is declared or a statement covers the
  day. Two more holds are accepted the same way: a reading taken long
  before the broker's day closed leaves that day's later P&L untracked, so
  the next day's first reading charges it to a flow (the broker's
  previous-day equity is not read; the statement clears it); and a flow
  landing in the very reading that carries the broker's reset is caught
  but measured against the within-day step, so a declaration alone does
  not clear it before the statement does. One unit of an order may exceed
  a cap, judged by the order gate as for the governor. After any fill the
  batch waits for the next verdict (at most two minutes).
- **Verification of the owner sleeve's stop** (synthetic books,
  `internal/daemon/mandate_sleeve_stop_test.go`, `internal/mandate/sleevestop_test.go`,
  CLI golden `mandate_owner_stop`): no key, nothing changes (no stop on the
  verdict, no state, no streak, additions open); the measure follows the
  book's adjusted risk base less the machine's marked P&L, seeded from the
  book's mark, a spike never raising it and a confirmed high doing so; at the
  stop the gate refuses the owner's additions and passes the core's and a
  reduction, the within verdict counts toward the streak, the target is
  pinned through a deeper fall, a stale capital state holds the stop as it
  was and resets the streak, a drawdown back under the stop but inside the
  release band holds the stop and its pin with the gate naming the release,
  and one under the release lets go; with the sleeve cut to its target, a
  drawdown oscillating around the stop on five-minute readings keeps the
  first pin (engaged throughout, the target never moves, nothing over, no
  streak) and only a reading under the release clears it, the next fall past
  the stop pinning half the use of its own verdict, the journal reading
  engage, one hold, release with the pinned figures, engage; inside the
  budget the batch cuts the owner's sleeve alone to half its use by the
  smallest lots, nothing of the core's fund, every row reduce-only and
  lowering the risk, the same book without the stop proposing nothing; over
  budget and stopped the batch meets both targets; at the target the rows are
  withdrawn and the episode kept, released it ends, unknown holds the rows;
  the key is optional, out of the fingerprint projection while absent,
  adopted and shown when written, refused at or above `max_loss_pct`.
- **Verification** (synthetic books;
  `internal/daemon/mandate_reduction_test.go`,
  `mandate_reduction_review_test.go`, `mandate_reduction_automatic_test.go`,
  `mandate_reduction_trading_test.go`, `scorecard_authority_test.go`,
  `internal/mandate/mandate_test.go`,
  `internal/app/alerts/presentation_test.go`, CLI goldens
  `proposals_list_mandate*`, `mandate_reduce`): the dominant name first and
  sized to the target by its smallest lot, the second name only when needed;
  the expiry cut only by its own need; one reading watches (gap breach
  included), two act, reads never count; a standing batch shrinks, grows and
  damps one lot; a covered position left to the owner and named; a protective
  put never sold and its deliverable shares kept (exercise never flips the
  line); the book's risk falls after every sendable step; a stop-held line out
  of the plan, a partly filled sale in it; the maximum loss reduces to the
  hedges and encodes; a glitch never raises the mark; an unexplained equity
  move holds the room until declared or a statement covers it, a P&L-explained
  one does not; the broker's daily reset before the New York date turns is
  no flow, a flow in that reading is, and the statement for the New York
  date covers a suspicion raised after UTC midnight
  (`TestMandateFlowCheckSurvivesTheBrokerDailyPnLReset`, `mandate_gate_test.go`);
  the merge under the latch and with automatic channels; the
  veto clock kept within one lot, renewed on a larger growth and in a new
  session or episode, a veto holding the line; the next session's row a new
  revision; `unknown`, stale and changed-book verdicts hold a standing batch
  with its revision; `within` withdraws, a sent record stays; a latched brake
  keeps the veto; not pre-authorised waits for the hand; pre-authorised places
  one order after the veto with origin `daemon-preauthorised`; the prepared
  submit refuses a held row; rank order and no hold-up after a refusal;
  reduce-only on long and short lines; order-cap carry-over; a gap breach cut
  to its cap; absent `[mandate]` changes nothing.

## Risk allocations

Owner decision, verbatim, 2026-10-18 08:06 CEST: "Accept all three: give
the machine its own seat (replacing D1 and largest-contributor-first), put
s1 into its six-month shadow test, and go for the core venue fix with the
core fund out of protection's automatic sales." The first clause accepts
decision 1 of the mandate budget review of 2026-10-17: "seats inside the
whole-book limits. The machine's seat is only as big as what it actually
runs (each strategy's stage times its share; nothing today), and you have
the rest. When the book is over budget, the sleeve furthest over its seat is
cut first. Each position keeps the seat of whoever opened it. The 30%
maximum loss, the budget and the gates still cover everything. This replaces
D1 and "largest risk contributor first"."

It replaces two records: D1 "No, whole book only" (`core-sleeve.md` §7),
under which the machine had only the room the owner's book left and could
never count on any; and "largest risk contributor first" in decision 3 of
2026-10-14 06:26 CEST (Forced reduction, above). No strategy runs in the
overlay yet.

Terms (owner, 2026-10-18 08:50 CEST: "on terms, always use the ones that
professionals use, risk budget or risk allocation, not seats or share."):
the review and the decision say "seat"; this record says **risk budget** for
the whole book's volatility budget, **sleeve** for each trader's part of the
book and **risk allocation** for a sleeve's part of the risk budget, as the
owner's strategy-engine map does (Desk `docs/strategy-engine.md`).

### Sleeves

- **Owner**: the owner's own trading, by hand, in TWS or the console, and
  anything the owner's own Rulebook rules open (the owner wrote the rule).
- **Core**: the core rule (`core-ibcf-v1`, the lab's s0), `core-sleeve.md`.
- **Overlay**: the risk allocation for a lab strategy on index options once
  one passes its gates; none runs in it yet.

A machine sleeve is a lab strategy Canary runs at a live stage. Rule
buckets that only reduce (protection, hygiene, the budget governor, the
forced reduction, expiry closes) are no sleeve: they place no units, they
take them away.

### How an allocation is measured

- **Use**: a sleeve's use is the sum of its legs' Euler contributions,
  `contribution_pct / 100 × sd_annual_base`, in base currency a year, the
  budget's own unit. The contributions add up to the book's risk exactly, so
  the uses do too; a hedge's negative contribution lowers its sleeve's use,
  and an addition that diversifies the book can have a negative use.
- **One contract, two sleeves**: the Euler contribution of a contract is
  linear in its units (q × ∂σ/∂q), so a contract held by two sleeves splits
  its contribution exactly in proportion to the units each holds.
- **Allocation**: a machine sleeve's allocation is reserved only as far as it
  runs: its stage times its allocation, in percent a year of the budget base,
  scaled by the room exactly as the budget is: `allocation_base = stage_pct /
  100 × target_pct / 100 × budget_base × room`. The core's stage is
  `[mandate.core] scale_pct` and its allocation `[mandate.sleeves]
  core_vol_pct`; a lab strategy's stage is the lab's ladder (R3 25, R4 50,
  established 100) and the overlay's allocation `overlay_vol_pct`. A sleeve
  that sends no orders runs nothing: its stage is 0 whatever the policy says.
  In this build the core sends none (phase 1 is read-only; its rows are phase
  2) and no strategy runs in the overlay, so both stages are 0 and the
  machine's allocation is zero, as the review said: "nothing today".
- **The owner's allocation is the rest**: `budget − Σ machine allocations`.
  The allocations add up to the budget, so the book is inside the budget
  exactly when the uses do not exceed the allocations in sum, and a book over
  budget always has a sleeve over its allocation. Today the owner's allocation
  is the whole budget. The constitution refuses machine allocations
  (`core_vol_pct + overlay_vol_pct`) above `vol_target_pct`, so the owner's
  allocation is never negative; the evaluator also floors it at zero.
- **The owner's 7 is no longer read**: the numbers of 2026-10-14 (owner 7,
  core 6, overlay 4) were standalone figures whose sum (17) exceeded the
  target because the sleeves are not perfectly correlated. Allocations count
  use, which adds up, so the owner's allocation is the rest: 15 − 6 − 4 = 5
  points once both machine sleeves run at full stage, not 7. `owner_vol_pct`
  is recorded and not read.
- **Use and standalone risk**: a sleeve's use is its standalone risk times its
  correlation with the book, so it never exceeds its standalone risk; others'
  holdings can raise it at most to that. A sleeve whose standalone risk fits
  its allocation is therefore inside its allocation whatever the others hold.
  The core sizes its volatility to its allocation on its own closes, the mandate
  measures it on the index's returns, and every core buy is cut to its
  allocation on the mandate's measure (`coreHeadroomFits`); between decisions
  a smaller room shrinks the allocation and the core's standalone risk does
  not follow, so correlated additions elsewhere can lift the core's use past
  its allocation (residual risk, below). The verdict serves each sleeve's
  standalone risk beside its use.
- **A flat sleeve keeps its allocation**: the allocation is a reservation by
  stage, not a holding. The core out of the market (trend off) keeps its
  allocation empty. The owner's room shrinks the day a machine stage rises:
  the core at 25 takes 1.5 of the 15 points, a tenth of the budget.

### The owner sleeve's allocation and stop

Owner delegation, 2026-10-18 21:41 CEST, to the question how large the
owner's own allocation should be and where its stop sits: "Need your
judgement, be on the aggressive side". The agent set, and the owner's
strategy-engine map (Desk `docs/strategy-engine.md`, "The owner's answers,
2026-10-18 21:41 CEST") records: the owner sleeve's allocation is the whole
remainder of the budget after the machine's reservations, no haircut (what
the register computes above); its drawdown stop is 25% of the owner sleeve's
own high-water mark, acting reduce-only to half the sleeve's risk, with the
whole book's 30% maximum loss unchanged as the backstop; reviewed when the
scorecard holds 100 entry weeks. Built 2026-10-19 (increment 4).

- **The key**: `[mandate.sleeves] owner_stop_pct` (percent of the sleeve's
  high-water mark; the owner writes 25). Optional: absent, nothing changes and
  a file without it keeps both fingerprints; written, it must be below
  `max_loss_pct`, the backstop. No number acts by default.
- **The sleeve's measure** (`mandate_sleeve_stop.go`): the owner's sleeve has
  no account of its own, so its equity-like measure is derived from what the
  capital state already keeps: the book's cash-flow-adjusted equity (the
  series behind the whole-book high-water mark, observed with every account
  read) less the machine sleeves' marked P&L since the start, read from the
  verdict's legs: for every leg placed in a machine sleeve, the units held at
  the last reading times the move of its unit value in base currency since
  that reading, at the evaluator's own prices (spot and FX for a stock,
  Black-Scholes at the position's IV for an option). The tax reserve is
  subtracted at read time, as the book's is, so a changed reserve never
  creates a drawdown. With the machine holding nothing, as today, the measure
  is the book's risk base and its high-water mark the book's: the 25% stop
  sits inside the 30% maximum loss exactly as the owner's words place it. The
  series counts from `mandate.started_at` and seeds from the book's
  high-water mark, so a key written after the start inherits the history the
  book has; the machine's P&L is tracked from the first reading with the key
  written. A new high counts once a second reading at least five minutes
  later confirms it, at the lower of the two (the book's glitch rule). The
  statement and the broker's daily P&L were not chosen: the statement arrives
  a day late and carries no sleeve, the broker's figure is whole-account, and
  both would need the same split by placement the verdict already gives from
  the legs it prices anyway. The state lives in the `risk_capital` document
  beside the book's mark (`mandate_owner_sleeve`), journaled as
  `mandate_owner_stop_started`, `mandate_owner_hwm_advanced`,
  `mandate_owner_stop_engaged`, `mandate_owner_stop_held` and
  `mandate_owner_stop_released`, the last three with the pinned figures.
- **Engaged, held and released**: the stop engages on the reading at which
  the sleeve's drawdown from its high-water mark reaches `owner_stop_pct`,
  and pins the sleeve's use on that verdict and half of it as the target. It
  releases with hysteresis: the pin holds while the drawdown is back under
  the stop but within the release band, five points of the high-water mark
  (`mandate.SleeveStopReleaseBand`; a 25% stop releases under 20%, carried as
  `release_pct`), and only a reading under the release clears it; the next
  reading past the stop is a new engagement and pins half the use of its own
  verdict. Five points is a fifth of the stop the owner delegated and the
  same width as the room the stop leaves under the book's 30% maximum loss;
  at the 15% volatility budget it is about five daily standard deviations,
  so a crossing back is a recovery, not one reading's noise. A stop under
  ten points keeps half of itself as the band, so the release stays above
  zero. Added 2026-10-18 22:00 WEST after the quality loop found, and a
  second pass confirmed, that the first build ratcheted: it released on the
  first reading under the stop and cleared the pin, so a re-engagement on
  the next reading pinned half of the already halved use, and a drawdown
  oscillating around 25% cut the sleeve to a quarter, an eighth and on, each
  notch sizing a new reduction batch (two scheduled readings five minutes
  apart sufficed). While engaged, held included, the owner's sleeve is
  reduce-only: a risk-adding order in it is refused (`mandate_budget`: "your
  sleeve is 27% below its high-water mark 240,000 EUR, past its 25% stop; the
  sleeve is reduce-only until its drawdown is back under 20%, 5 points under
  the stop"; held: "your sleeve is 23% below its high-water mark 240,000 EUR,
  under its 25% stop but not yet under the 20% that releases it"), inside the
  budget or over it; reductions, closes, cancels and hedges pass, and the
  machine sleeves are untouched.
- **While the measure cannot be read** (the capital state unknown or stale, a
  suspected flow, the book unmeasured) the series holds, the stop keeps its
  last state (`known: false` with the reason) and sizes nothing new; the
  verdict is unknown then anyway, so standing rows are held as before.
- **Surfaces**: the verdict carries `verdict.owner_stop` (the measure, the
  high-water mark, the drawdown, the machine's P&L set aside, engaged since,
  the use at the stop and the target, the release, the use now and `over`);
  `canary mandate` prints the "Owner sleeve" line (drawdown against the
  stop, STOP ENGAGED with the target and the release, or the hold) and the
  "Additions" line names the stop and its release; the forced reduction's rows and status say "owner sleeve stop"
  (`phase: stop`); the policy view echoes `owner_stop_pct`.

### Placement

Each position stays in the sleeve of whoever opened it, fixed for its life.

- **The core's contract is reserved to the core**: every unit, whoever bought
  it (`core-sleeve.md`, "Adopting an existing IBCF holding"; the build brief
  of 2026-10-18: "The core fund stays reserved to the core"). The core's
  contract is the line Canary resolves for the ISIN and the one the core last
  used (`mandateCoreContracts`, the venue fix of 2026-10-18), and a written
  `[mandate.core] con_id`. An order on it, the owner's hand included, is the
  core's addition, judged against the core's allocation. While the core runs
  nothing that allocation is zero, so a hand buy of the fund is refused once
  the core's use would be above zero; a sale passes. While no contract is
  resolved nothing is reserved: the core then runs nothing, so it has no
  allocation to keep, and a holding is adopted once its contract is known.
- **Orders are placed by contract id**, which the order path resolves before
  every gate point (`resolvePreviewOrderContract`); a second listing of the
  fund under another contract id is the owner's, and the core's target holds
  while one is held.
- **Everything else is the owner's**, until a machine sleeve's own order
  path places units by its fills. In this build no path places units in a
  machine sleeve, so placement is total: the core's contract or the owner.
- **Split by fill lineage** (the rule the first machine order path that can
  trade a contract the owner may also hold must journal; nothing here is
  built before that path):
  1. An opening fill (it raises the position's size in its direction)
     places its units in the sleeve of the order's origin. The core's
     reserved contract is the core's whatever the origin.
  2. A closing fill of a sleeve's own order takes that sleeve's units. A
     forced-reduction fill takes units of the sleeve its row was sized for
     (the row names it, `mandate.sleeve`), and the row's cut is capped at
     that sleeve's units on the contract. A fill of a row the same-contract
     merge built from several buckets takes units in the order of the needs
     merged, the mandate's first. A fill acting on the whole line
     (protection, hygiene, an expiry, an exercise or assignment, another
     broker lifecycle event) takes every sleeve's units in proportion. The
     owner's hand sale takes the owner's units first, then the machine's in
     proportion.
  3. A position that crosses zero starts over: the new side belongs to the
     origin of the fill that crossed.
  4. The placed units of a contract always sum to the broker's position. A
     machine sleeve placed more units than the line holds, or a fill the
     journal cannot match while a machine order on the contract works or is
     of unknown outcome, makes that contract's placement unknown, until the
     order journal's reconciliation resolves the order or a statement
     covering the day accounts for the units.
  5. Placement is never edited: no tag and no re-placement. A position that
     must move does so by a recorded owner instruction.
  Before any machine order path exists, two gate gaps close: a combination
  (BAG) order passes the mandate gate unmeasured while the verdict reads
  within (`mandateUnpricedOrderGate`, an unmodelled security type), and a
  BUY of a put on a Rulebook hedge symbol counts as a hedge whoever sends
  it; a machine sleeve's order gets neither pass.
- **As built for the register's other entries (2026-10-19, with the first
  order path that sends a non-core entry's rows; `strategy_placement.go`,
  after three rounds of an adversarial review).** Each entry trades one
  line. Rule 1: its units are what its own rows filled (per order, the
  larger of its journaled executions and its last reported filled
  quantity, read from the proposal and order journals by order reference),
  never more than the broker holds; every other unit of the line is the
  owner's, so no holding is ever adopted. Rule 2: a sale of the entry's
  units, its own or by hand, lowers them; the owner's units go first. While
  an entry is live a hand buy of its line is refused at the gate ("this
  fund line is the … strategy's while it is live"; sales stay open; to trade
  it by hand the owner writes the entry suspended), so through Canary the
  line is held by the entry alone and is placed whole in its sleeve. The
  owner's units can stand beside the entry's only from before its first
  fill or from a trade outside Canary; the line is then split into the
  owner's leg and the entry's own (`sleeve:<sleeve>:con:<id>`), the placed
  units summing to the broker's position (rule 4), and the entry holds,
  naming them, until they are sold. The units are read afresh at every
  verdict, never carried as state: an execution journaled before its
  position updates, or the reverse, reads right on the next verdict, and an
  order journal that cannot be read holds the entry. Rule 5: nothing is
  re-placed. Orders are placed by origin, as fills are: the entry's own row
  carries its id in the signed draft (`OrderDraft.RegisterEntry`,
  daemon-set) and is judged in its sleeve, any other order in the owner's.
  The two gate gaps are closed for a machine sleeve: an order naming an
  entry that is not a stock order of that entry's line is refused before
  the hedge pass and the unmeasured pass are reached. Two strategies on one
  fund line refuse the file. The forced reduction sizes a split line as one
  line on the broker's position, ranked in the owner's sleeve; its fill
  takes the owner's units first. Residual: a hand buy in TWS outside
  Canary's client after a hand sale of the entry's units is read as the
  entry's (its fills still exceed the holding), so its trend-off sale would
  sell it; trade the line through Canary, or suspend the entry first. A
  forced or protective sale of the entry's units is not an own fill: it
  lowers the entry's units but not its own-fill kill measure.
- **Unknown placement never guesses**: while any contract's placement is
  unknown the allocations are unknown and the allocation check refuses every
  risk-adding order (the uses add up, so one unknown contract leaves every
  sleeve's use unknown). The forced reduction does not hold for it: it sizes
  in the whole book's order (largest contributor first) and says so, because a
  protection that acts must not wait on bookkeeping. With placement total in
  this build, the allocations are unknown only when the verdict is.

### The gate

The whole-book rules bind at the same four points, with one change decided
on 2026-10-18 15:31 CEST (owner, to "Should a sleeve inside its own
allocation keep adding while another sleeve is over its own?": "yes, it
should. unless you advise differently"): a book over budget no longer stops
every sleeve. While the standing verdict reads `within`, the book with the
order must stay inside the budget. While it reads `over` or `reduce`, a
risk-adding order passes when its own sleeve is inside its allocation and
stays inside it with the order; the forced reduction keeps cutting the sleeve
furthest over its allocation meanwhile. Three holds remain whatever the
sleeve: an `unknown` or stale verdict refuses every addition ("the mandate
risk cannot be measured"), the order must create or deepen no gap-cap breach,
and at the maximum loss (no room) no sleeve adds, since the book goes to its
hedges. The bill and hedge exemptions stand. Then the allocation: the order is
placed (the core's contract the core's, anything else the owner's), and a
risk-adding order is refused (`mandate_budget`) while its sleeve is over its
allocation, or would be with the order added. A sleeve is over its allocation
when its use exceeds it, or when its standalone risk does ("on its own"):
use is standalone risk times the sleeve's correlation with the book, so a
book far over budget dampens a small sleeve's use to nothing whatever it
holds, and the standalone bound is what keeps "inside its allocation" a
limit. The owner's room is the owner's allocation less the owner's use: the
budget, less the machine's allocations, less what the owner's book uses.

The owner approved the change with that guard on 2026-10-18 17:24 CEST, in
this session's words: "Yes: finish the mandate gate change with the own-risk
guard while over budget, record it in the Canary docs, run make check and make
test, land, push and reinstall the daemon." The guard as built (6f182ab6,
Canary v3.22.1) binds in every state, wider than the approval's "while over
budget": inside the budget it changes nothing while the machine's
allocations are zero, and once a machine sleeve trades it narrows the
owner's room to what the owner's sleeve holds on its own risk. Whether to
limit it to a book over budget is the owner's call.

- The check judges the order's own sleeve. By the bound above, an order can
  raise another sleeve's use at most to that sleeve's standalone risk.
- A sleeve over its allocation adds nothing, even an order that would lower its
  use, as the book over budget adds nothing in that sleeve.
- An option buy the evaluator cannot price is judged by its volatility-free
  bound (Enforcement class, above): its sleeve's standalone risk plus that
  bound must fit the allocation (use ≤ standalone, and the standalone risk grows
  by at most the bound).
- The gate view (`gate.refuses_additions`) reads the owner's additions: it
  refuses while the verdict is unknown or stale, while the room is gone, and
  while the owner's allocation is over; over budget with the owner's sleeve
  inside its allocation it reads "an addition that keeps it there passes".
- `canary core` reads "no buy: the core sends no orders yet, so it has no
  allocation" while the core runs nothing; once it runs, its buys are cut to its
  allocation as to the budget and the gap caps.

The decision step (review, 2026-10-19). A strategy row's buy, the core's
included, is judged on the standing verdict by the same reading as an order
at the gate (`mandateVerdictHolds`, one function for both, so the two never
disagree): within the budget it is sized to the headroom as before; over it
(`over`, `reduce`) it is sized while its own sleeve is inside its allocation,
by use and on its own risk, and the whole-book margin then does not bind it,
while the buy must still create or deepen no gap-cap breach, keep its own gap
at most at 95% of its cap and keep its sleeve inside its allocation with the
buy added; an unknown verdict holds the entry; at the maximum loss nothing
is sized. Until 2026-10-19 the step refused the buy on any verdict but
`within` ("the mandate verdict reads reduce"), which held the core's October
2026 row against this rule, and the row builder served a buy blocked while
the forced reduction's breach episode was open; both are gone. A
`mandate_reduction` row of the entry's own line still blocks its buy. The
shadow record's paper decision reads the same way.

### The forced reduction

Superseded 2026-10-27 19:50 CET (spec TM-04.12): the premium phase cuts the
largest premium at risk per name whatever its sleeve ("The loss measure as
the verdict's headline" below). The allocations' order below held from
2026-10-18 to 2026-10-27 and is kept as the record of that decision.

The trigger, the episode, the target (0.95 × budget), the gap phase, the
expiry gate, covered and protected lines, the veto clock and every other
rule are unchanged. Only the risk phase changes:

- On the book re-evaluated after each cut, the sleeve furthest over its
  allocation (use less allocation, in base currency; ties in the order owner,
  core, overlay) is cut, and within it its largest Euler contributor. A sleeve
  with nothing left to reduce passes the turn to the next one.
- The cut is the smallest whole step that takes the book to the target or,
  while another sleeve is over its own allocation, that takes this sleeve
  inside its allocation, whichever comes first; when neither is reachable on
  that line, the step that takes the most risk off. It is never more than the
  old rule would cut on that line, so every sendable order still lowers the
  book's risk. Once no other sleeve is over its allocation, the sleeve chosen
  carries the rest down to the target, the margin included.
- So the owner's book is cut for the owner's excess and the machine's for
  the machine's: while the machine's sleeves are inside their allocations, the
  batch cuts only the owner's lines; a machine sleeve over its allocation is cut
  before the owner's lines once it is the furthest over. With the machine
  holding nothing, as today, the batch is the old one.
- The allocations' order never stops short where the old order reaches the
  target: if it does, the batch is sized largest contributor first and says
  so ("the allocations' order stops short of the target").
- Each row names its sleeve (`mandate.sleeve`) and why it was chosen ("the
  largest risk contributor left in your sleeve, the sleeve furthest over its
  allocation").
- A core holding while the core runs nothing (allocation zero) is over its
  allocation by its whole use and is cut once it is the furthest over. The
  core's fund leaves protection's automatic sales (the owner's third clause of
  2026-10-18 08:06 CEST, built separately); the mandate's forced reduction
  still covers it, as the review recommended.
- **The owner sleeve's stop** (owner delegation 2026-10-18 21:41 CEST, "be on
  the aggressive side"; the agent's setting above, built 2026-10-19): the stop
  acts through this same batch, episode, veto window and pre-authorisation.
  A verdict whose owner stop is engaged with the sleeve's use above its target
  (`owner_stop.over`) is a breach reading whatever the book reads: two
  scheduled readings start the batch, inside the budget or over it. The
  sizing runs as above until the book is at its target in the allocations'
  order (the sleeve furthest over its allocation first); then, with the book
  at or under the target, only the owner's sleeve is cut, its largest Euler
  contributor by the smallest whole step that takes the sleeve's use to the
  target pinned at the stop (half its use on the engaging verdict), rows in
  phase `stop`. Nothing of the machine sleeves is cut for the stop; the
  owner's hand sales count, since the use is measured afresh at every
  verdict, and a sleeve cut to half is not cut again, because the target is
  pinned for the engagement and the engagement outlasts a drawdown back
  under the stop until it is under the release (five points under the stop,
  "Engaged, held and released"); without that band an oscillation around the
  stop re-engaged at half of the halved use and cut again (found and fixed
  2026-10-18 22:00 WEST). The target is reached only when the book's
  target and the stop's are both met. A verdict within the budget with the
  stop engaged and the use at or under its target withdraws the rows and
  keeps the episode open (the sleeve stays reduce-only); a rise in the
  sleeve's risk is then sized again inside the episode, with its veto clocks.
  The episode ends when the book reads within and the stop has released,
  under the release band. The
  whole-book line is unchanged: at the maximum loss the batch still reduces
  to the hedges.

### Policy keys

`[mandate.sleeves] owner_stop_pct` is the owner sleeve's drawdown stop
(above); optional, no default acts.

The stage lives in the strategy register (owner decision 2026-10-18 09:01
CEST; [strategy-register.md](strategy-register.md)): `[mandate.core]` is its
first entry and every later machine strategy a `[mandate.strategies.<id>]`
table naming its compiled rule, the lab spec hash it implements, its sleeve,
its risk allocation, its stage (shadow, live or suspended) and the scale it
sizes at. A machine sleeve reserves its allocation times its scale only while
its entry is live and its rule sends orders; shadow, suspended and an entry
that holds reserve nothing, except that a live entry on hold keeps its
reservation while its sleeve holds positions. The core's allocation stays
`[mandate.sleeves] core_vol_pct` and the overlay's `overlay_vol_pct`; the
owner's allocation is the rest. No key acts by default.

### Surfaces

`canary mandate` (and `--json`, MCP `canary_mandate`) lists each allocation:
its use against its allocation, its standalone risk, and its stage and
allocation or why it runs nothing; the verdict carries them as
`verdict.allocations`. Desk shows each allocation's use against its allocation
on Risk › Mandate. An allocation refusal names the sleeve, its use, its
allocation and, for the owner, the machine's allocations.

### Safety review (2026-10-18) and how it is folded in

An independent review of this design and its first build found no blocker
and four serious items.

| # | Finding | Resolution |
|---|---|---|
| S1 | The core's reservation is not in the owner's quoted words; with the core's allocation at zero a hand buy of the fund is refused and a held fund line is cut first in a breach | Kept: the build brief carries it ("The core fund stays reserved to the core"), and `core-sleeve.md` has reserved the instrument since 2026-10-14. Stated plainly in Placement and The gate as a consequence the owner sees; the core holds no fund today |
| S2 | Allocations key on `con_id` and the compiled symbol; the venue fix makes `con_id` optional and resolves the line by ISIN | Placement by contract id only, through one function (`mandateCoreContracts`) that takes the venue fix's resolved line when both land; the symbol rule is dropped, since the order path resolves a contract id before every gate point |
| S3 | "Allocations unknown ⇒ the batch holds" would stop a pre-authorised protection on bookkeeping | Unknown allocations refuse additions only; the batch sizes in the whole book's order and says so; rule 4 gets its recovery (journal reconciliation or a statement) |
| S4 | "A sleeve inside its allocation stays there" holds only while its standalone risk on the mandate's measure fits its allocation | Restated as use ≤ standalone; every core buy cut to its allocation on the mandate's measure; the drift between decisions recorded as residual risk |
| M1-M8 | the overlay without a strategy; the owner's 7 no longer read; the gate's treatment of a sleeve already over; a guard against stopping short; overfull machine allocations; lineage and gate gaps for later; stale lines; `canary core` copy | Each folded in above, with a test where it acts (Verification) |

### Residual risk accepted

A sleeve's use moves with the rest of the book by correlation, bounded by its
standalone risk; the core's allocation shrinks with the room between monthly
decisions while its holding does not, so a correlated addition elsewhere can
take the core over its allocation, and the next breach then cuts the core
first. Allocations inherit every limit of the measurement. A machine
allocation that is empty stays reserved, so the owner's room is smaller than
the budget while a machine stage is live. The split by lineage is specified,
not built: it lands with the first machine order path that can share a
contract with the owner.

### Verification

Synthetic books only. `internal/mandate` (`allocations_test.go`): uses add up
to the book's risk and allocations to the budget, the core's allocation is
reserved only as far as it runs: its stage times its allocation and the
owner's the rest, a contract split by units splits its contribution in
proportion, every allocation is zero at room 0, an overfull machine leaves the
owner a zero allocation, a sleeve whose standalone risk fits its allocation
stays inside it whatever is added elsewhere. `internal/daemon`
(`mandate_allocations_test.go`): the allocation plan gives the core no stage
while it sends no orders and places its contract with it; with a live core
allocation the gate refuses an owner addition past the owner's allocation that
the whole budget would take, and passes one inside; with the core running
nothing the same order passes; a hand buy of the core's contract is the core's
and refused while its allocation is zero, a sale passes; an unpriced option
buy is judged by its sleeve's standalone risk plus the bound; the forced
reduction cuts the sleeve furthest over first, leaves a sleeve inside its
allocation alone, stops a sleeve's cut at its allocation while another is
over, never stops short where the old order reaches the target, every row
alone lowers the risk, and with the machine empty the batch is the old one;
the constitution refuses machine allocations above the target. Core: the
headroom-cut buy passes the gate with the allocation
(`TestCoreTargetBuyCutToTheGapCapMargin`). CLI golden `mandate_allocations`.

## Decision-grade history

The chart-history store (`market_history_store.go`) is display-only: it
serves what it has, labels gaps and never refuses. The mandate keeps its
own series (`mandate_history.go`, state documents of kind
`mandate_history`, one per contract identity, scope `mandate/history/<id>`)
with the source time of every read (`fetched_at`, the broker's `source`
label, the session that answered), and refuses to judge on a series that is
missing or stale. A series covers every held stock and ETF, the underlying
of every held option, SPY (the index proxy) and `EUR.<CCY>` for every held
non-base currency, and, for a day, any name or currency a refused order
asked for. It holds the last 320 daily closes (at least 253 are
needed) and is refreshed on the background pacing lane within the existing
sixty-per-ten-minutes historical budget: at most one refresh per name per
broker session, only when the series lacks the last completed session, and
never more than two reads in flight. A read asks the broker for 400
calendar days so a mixed US and European book still shares 253 dates after
alignment. A series is fresh while its last close is no more than one
completed US session behind; a failed refresh keeps the last good series
and its clock, so the verdict says stale rather than inventing a bar. The
fetch helper `fetchMarketHistoryDays` is reused; the display store's
documents are neither read nor written.

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
| D, vol target, tax reserve, start date, gap numbers, sleeves | `risk-policy.toml` `[mandate]` | `risk.ConstitutionMandate` | manager reread every 30 s; a change is a revision (fingerprinted) | table absent ⇒ feature inactive, no behaviour change; incomplete ⇒ verdict unknown, additions refused |
| Equity, base currency | account summary success path | `AccountResult.NetLiquidation/BaseCurrency/AsOf` | `capital.max_equity_age_minutes` | missing or stale ⇒ unknown |
| High-water mark, flows at start, daily risk-base samples | daemon runtime state (the `risk_capital` document, `state.mandate`) | `mandateCapitalStateV1` | persisted with the adjusted peak, CAS in SQLite | storage failure ⇒ the last committed state, disclosed |
| Flows since start | the capital store's effective flows (statement when the constitution has statement reconciliation, else declared) | `effectiveFlowsLocked` | per observation | a flow value-dated before the start but incorporated after it shifts the budget; the owner resets `started_at` |
| Positions, IV, spot, FX | one current `positions.list` read of the selected account | `PositionView.Quantity/Mark/IV/Underlying/FXRate/Delta` | current portfolio authority | not current ⇒ unknown |
| Daily closes | `mandate_history` state documents | `mandateHistorySeries` | last close within one completed session | missing or stale ⇒ unknown |
| Verdict | daemon-owned evaluation (`mandate.risk`) | `rpc.MandateRiskResult` | refreshed every two minutes and on every read; the gate reads a verdict no older than five minutes | older ⇒ unknown ⇒ additions refused |
| Blocker | `mandateBudgetGate` | `TradingBlocker{Code: "mandate_budget"}` | per order, at four points | — |
| Allocations and placement | the verdict (`verdict.allocations`), from `[mandate.sleeves]`, `[mandate.core] scale_pct` and the core's contract, and this build's order paths | `mandate.AllocationReport` | with the verdict | unknown ⇒ additions refused; the batch sizes in the whole book's order |
| Owner sleeve's stop | `[mandate.sleeves] owner_stop_pct`; the sleeve's series in the `risk_capital` document (`mandate_owner_sleeve`), from the capital state's adjusted equity and the verdict's machine legs | `mandate.SleeveStop` (`verdict.owner_stop`) | updated with every measured verdict; holds while the capital state or the book is unknown | key absent ⇒ no stop; unknown ⇒ the stop keeps its last state, the verdict is unknown and the batch holds |

The exercise path binds its own position authority and never binds risk
(`exerciseBound`); an exercise is reduce-only by the opportunity policy and
cannot add risk, so the wire guard's risk block (which runs only when
`binding.riskBound` is set) covers every path that can add risk: order
place and order modify both bind through `bindPreviewOrderRiskAuthority`.
This is stated here because the gate's placement inside `if
binding.riskBound` was reviewed as the riskiest integration point.

## Options, the override and the exposure view

Owner decisions, verbatim in substance, 2026-10-23 18:22 CEST (the live book
was at 940% of the volatility budget, every addition refused, while the
option book's loss was bounded by its premium):

1. **Options are gated by premium at risk, not by yearly volatility.** Per
   name, the premium at risk on an underlying (every long option outside
   protection on it, each leg the higher of price paid and today's value,
   rule 2's figure) may be at most `option_name_act_pct` of NLV, a new
   Rulebook key written into the owner's file at 15 by the rulebook
   migration (`# Added by Canary vX at its default`, the struct comment
   carrying the date). In total the premium budget's act level of the regime
   set in force (rule 3, 35% in every set by default) applies as it does
   today. An option buy that would breach either is refused with the figure,
   the limit and the name (`mandate_premium`: "with this order the premium at
   risk in SYNA would be 46,800 EUR, 15.6% of net liquidation 300,000 EUR,
   over the 15.0% limit for one name (now 0.0%)"; "the book's premium at risk
   would be 107,200 EUR, 35.7% of net liquidation 300,000 EUR, over the 35.0%
   premium budget of the calm set (now 33.3%)"), and the action names the
   room left. The order's premium is contracts × multiplier × limit in base
   currency; without a limit, a rate or a current NLV it is refused, never
   passed (nothing passes unmeasured). The gate reads the premium from the
   exposure view the verdict was measured with (one measurement for the
   page and the gate: `risk.PremiumAtRisk` over the rulebook's classified
   names, `mandate_exposure.go`), so a verdict too old for the gate holds
   option buys as it holds stock buys.
2. **The volatility budget keeps gating stock and ETF additions.** For an
   option buy its verdict, the 1-day 99% expected shortfall and the
   beta-adjusted exposure are computed and shown as before, and the whole
   volatility judgement (the standing verdict, the book re-evaluated with the
   order, the gap caps, the sleeve allocation, a name without history) is
   carried on the preview as a warning (`mandate_budget`, severity warning),
   never a block (`mandateBudgetJudgement`). Two holds bound a loss rather
   than a volatility and stay for every addition: at the maximum loss (no
   room) nothing adds, and the owner sleeve's drawdown stop keeps its sleeve
   reduce-only while engaged (`mandateHardHolds`). A short option opened to
   sell is no option addition: its loss has no premium to bound it, so it
   stays on the volatility gate as before. The gate view reads both:
   `gate.refuses_additions` for stock and ETF additions, `gate.options` for
   option buys (refused while the book's premium is at its cap or cannot be
   measured; `warning` the volatility sentence a buy passes with).
3. **A human-only override lifts the additions gate.** `canary policy
   override --control mandate.vol_target_pct --reason TEXT --hours N` (the
   existing mechanism: journaled, expiring, capped by
   `override.max_duration_hours`) lifts the volatility budget's gate for the
   owner's own additions until it expires. The `[mandate]` keys are
   constitution limits once the table is written (`risk.mandateLimits`), so
   the control is a known key; of them only `mandate.vol_target_pct` takes
   an override (`risk.MandateAdditionsOverrideControl`), the others change
   by a revision. What still bounds risk while it is active: the drawdown
   brake and the no-room hold at the 30% maximum loss, the owner sleeve's
   stop, every order cap, the premium limits above, the device confirmation
   of every order, and the override's expiry. A machine sleeve's order never
   reads it. `canary mandate` prints the override with its reason, grant and
   expiry; `gate.override` carries it and Desk shows it on Risk › Mandate.
   Desk never grants one: the owner does, in a terminal.
4. **Exposure through the greeks, to watch, never to gate.** The verdict
   carries `exposure` (`rpc.MandateExposureView`): per underlying and for the
   book, net delta in money (shares plus delta × contracts × multiplier, at
   the spot, in base currency), the same through the name's beta to SPY from
   the verdict, gamma as the money change of that delta for a 1% move, theta
   a day and vega a point as IBKR's model reports them per contract, the
   name's share of the risk and beta, and the premium at risk against its cap
   (the cap and the room in money, `premium_over` at the cap). A leg without
   a greek, a spot or a rate, or with a stale quote, leaves that figure of
   its name absent with the reason, and the book's total absent naming the
   name; a verdict that prices no name leaves its share and beta absent. The
   NLV the caps are shares of is the order limits' current account reading,
   named when absent. Nothing here is zero for want of data.

Verification (`internal/daemon/mandate_options_gate_test.go`; the earlier
assertions that an option buy was refused by the gap cap and the budget,
`TestMandateGateFailsClosedOnUnpricedOptions` and
`TestMandateGateBoundsAnUnpricedOptionBuyByItsAllocation`, now assert the
warning and the premium refusal; CLI golden `mandate_exposure`): on a book
far over budget a stock buy is refused and a call buy inside the premium
limits passes with the verdict as its warning, a put sold to open stays
refused; thirteen contracts past the 15% name limit are refused naming the
figures, twelve pass; the book's premium is capped at the set's 35% with the
hedge put left out and named; the greeks sum per name and for the book, a
missing vega or a stale quote leaves the figure absent with its reason; the
override lifts the stock gate with its warning, the gate view names it, the
other mandate keys take none, the no-room hold stands, and an expired
override reads as none; the rulebook migration writes
`option_name_act_pct = 15` with its provenance and keeps the policy in
force.

## The loss measure as the verdict's headline

Owner decision 2026-10-27 19:50–19:52 CET, on the trading machine spec's
callout TM-04.12 (Desk, `docs/design/trading-machine/trading-machine.md`):
the mandate's verdict is the loss measure against the maximum loss, not the
book's yearly volatility against a volatility budget; and protection's
premium counts in the premium budget ("Count it").

- **The loss measure** (`mandate.LossMeasure`, `verdict.loss`): every
  bought option at its premium at risk, the higher of the price paid and
  today's value in base currency, as rule 2 measures a line
  (`risk.PremiumAtRiskLeg`), protection included; plus, for every share
  line a working stop or trail covers at the broker (STP, STP LMT, TRAIL,
  TRAIL LIMIT on the reducing side, read from the open-order inventory,
  `mandateArmedStops`), the cash loss from the mark to the stop over the
  covered shares (a stop above a long line's mark loses nothing; a line
  covered in part counts the covered shares and is named for the rest). A
  share line without a stop counts nothing and is named as `unstopped`; the
  measure says what it did not count rather than guessing. An option leg
  whose premium cannot be measured (no price paid and no value, or no rate
  to base) makes the verdict unknown, as a missing series does. A short
  option carries no premium and counts nothing here; its risk stays in the
  volatility and the gap stresses.
- **The loss room** (`mandate.LossRoomBase`, `verdict.loss.room_base`):
  `max_loss_pct / 100 × budget_base × room`, the maximum loss of the
  budget base less the drawdown already taken (room runs from 1 at a new
  high to 0 at the maximum loss). With D = 30 the room is 30% of the budget
  base at a new high; at room 0 it is 0 and any measured loss is over it.
- **Verdict.** `within` while the loss measure ≤ the loss room and no gap
  cap is breached; `reduce` over the room or on a gap breach; `unknown`
  when anything is missing. `over` is no longer produced (its 1.25 × band
  belonged to the volatility). The additions gate reads the same: a stock or
  ETF order is refused while the verdict reads reduce or unknown, while it
  would create or deepen a gap breach, or while its sleeve is over its
  allocation; a stock order that would take the loss measure over the room
  is refused too (`mandate_budget`). An option buy keeps the premium limits
  as its gate (2026-10-23 above) and carries the loss-room judgement as its
  warning where the volatility was before.
- **Volatility stays, as sizing.** The historical simulation, the Euler
  contributions, the betas, the gap stresses and the owner sleeve's stop are
  unchanged and still shown: the volatility budget
  (`vol_target_pct × budget_base × room`) sizes the sleeve allocations and
  the machine strategies, and `canary mandate` prints the book's volatility
  as the 1-day swing beside the loss measure. The volatility is no longer
  the headline and no longer the reduction's sort key.
- **Forced reduction.** The trigger, the episode, the gap phase, the expiry
  gate, covered and protected lines, the owner sleeve's stop phase, the
  veto clock and the pre-authorisation are unchanged. The target is 0.95 ×
  the loss room. After the gap breaches, the premium phase (phase `premium`)
  cuts the name with the most premium at risk among its reducible legs,
  within it the largest leg, by the smallest whole step that takes the loss
  measure to the target or, when no step on that line does, by the step
  that takes the most loss off, re-evaluated after every cut; whatever the
  name's sleeve (the allocations' order of 2026-10-18 is superseded; a
  sleeve over its allocation still refuses additions). The expiry gate is
  still judged on volatility: shares an option delivers at exercise carry
  no premium, so the loss measure cannot see an exercise. Each row carries
  `loss_base` (the leg's share of the measure) and `loss_cut_base` (what
  the cut takes off); the status carries `loss_base`, `loss_room_base`,
  `loss_target_base` and `projected_loss_base`.
- **Protection in the premium budget.** Rule 3 counts every bought option
  at its premium at risk, protection included; the note names the
  protection share ("of which protection premium N% of NLV, counted in this
  budget since 2026-10-27"). The per-name cap `option_name_act_pct` reads
  the same figure, so protection counts there too (one measure), while a
  hedge buy keeps bypassing the premium gate as a hedge. Rule 4 (time
  value) and rule 12's hedge band are unchanged: rule 2's protection tier
  is tied to rule 12's act tier, not to rule 3.
- **What Desk reads.** New fields on `rpc.MandateVerdict.Loss`
  (`total_base`, `premium_base`, `stop_loss_base`, `room_base`,
  `ratio_to_room`, `names`, `unstopped`), on each leg (`loss_base`), on the
  reduction status (`loss_base`, `loss_room_base`, `loss_target_base`,
  `projected_loss_base`) and on each reduction order (`loss_base`,
  `loss_cut_base`); `TradeProposalMandate` carries the same for a row. Desk
  compiles the verdict's handlers at its Canary pin and re-pins to read
  them.

Verification (`internal/mandate/loss_test.go`,
`internal/daemon/mandate_gate_test.go`, `mandate_options_gate_test.go`,
`mandate_reduction_test.go`, `mandate_allocations_test.go`,
`internal/risk/rulebook_sell_only_test.go`; CLI goldens `mandate*`,
`proposals_list_mandate*`): a book whose premium at risk is over the loss
room reads reduce and refuses a stock buy naming the measure, the room and
the unstopped names; an option buy that would take the measure over the
room passes the premium limits with the loss-room warning; the reduction
cuts the largest premium at risk first whatever its sleeve and lowers the
measure row by row to 95% of the room; a stopped share line counts its loss
to the stop and an unstopped one counts nothing and is named; the premium
budget counts protection with its share in the note.

## Fallback volatility

Owner instruction 2026-10-28 08:51 CET ("Build it"), after one option
without an implied volatility before the US open left the whole verdict
unknown, every stress and plan figure with it. An option position row
carries the volatility of the session's last model tick; before the first
one, after a daemon restart and outside the option's market hours it has
none. The design question and the owner's framing: "a stale IV has most
likely relatively low impact on the whole book and we can accept that
risk", provided the impact is measured.

- **Fallback.** The daemon prices such a leg at the volatility its own mark
  implies (`mandateFallbackIV`, the Black-Scholes inversion the order gate
  already uses for unheld legs) and tags it `iv_source = "mark"` with the
  mark's time when supplied by the position data. No clock is invented when
  the portfolio stream supplies none. A mark at or below intrinsic implies none; the leg stays
  unknown.
- **Approval.** The evaluator counts the tag only under
  `[mandate.iv_fallback]`; absent, or with a key missing, such a leg leaves
  the verdict unknown as before, and every fingerprint is kept.
- **Band.** Each fallback volatility is moved down and up by its band:
  `short_band_pct` under 8 days to expiry, `month_band_pct` from 8 to 35
  days, `long_band_pct` beyond. The book is simulated twice more, every
  fallback leg at the edge that loses more in the S&P fall and at the
  other. The verdict's `iv_fallback` carries the legs, the S&P-fall range
  and half its width (`uncertainty_base`) against the loss room.
- **Tiers.** Half the width within `measured_pct` of the loss room: the
  verdict stands. Within `range_pct`: it stands only when the verdict (the
  loss measure and the gap caps) is the same at both edges. Beyond: unknown
  with the reason, no stresses, as any unknown.
- **Numbers.** The proposed values come from Canary's recorded option
  chains, 19 to 27 October 2026, four complete sessions, three held names,
  778 contract pairs: the 99th percentile of the one-session IV move per
  class was 63% under 8 days, 13% from 8 to 35 days and 5% beyond, rounded
  up to 65, 15 and 8. One week, three names and a rising-volatility week:
  the bands should be re-measured once the recorder has a few months.
  The tiers 5 and 20 follow common desk practice for data-quality
  exceptions against a limit.
- **Not built.** A last recorded volatility as a further fallback (the
  recorder keeps no contract id and failed three of seven nights), a wider
  band around known events such as earnings, and an option with no mark at
  all, which still leaves the verdict unknown.

## Exceptions And Change Control

- Who may grant an exception: the owner, by revising `[mandate]` (raise D or
  the target, lower the reserve, move the start) with a higher
  `policy_version`, or by the one-shot override of `mandate.vol_target_pct`
  (above, 2026-10-23), which lifts the additions gate alone and expires.
- Required reason, scope, expiry, and audit record: the file edit and its
  fingerprint; the high-water mark advances and starts are journaled
  (`mandate_hwm_advanced`, `mandate_started`) with the fingerprint.
- Whether exceptions can increase risk: a revision can raise the budget; no
  exception can admit an order the standing verdict refuses.
- Rollback trigger and operator action: remove `[mandate]` and raise
  `policy_version`; the gate is then inactive and the state document keeps
  its high-water mark for a later restart.
- Policy version/fingerprint transition: a file written before this table
  existed keeps both fingerprints byte for byte (pointer table, omitted
  when nil). An older daemon refuses a file that carries `[mandate]`
  (strict unknown-key rejection keeps the last good policy); `canary policy
  ensure` never writes the table and its migration leaves it as written.
  The table needs schema 2, as every feature added since the schema split.

## Operating Cadence

- Pre-open or daily review artifact: `canary mandate` (budget, risk,
  room, contributions, stresses, evidence clocks).
- Pre-trade artifact and acknowledgement: the order preview's
  `mandate_budget` blocker, naming the verdict and its reasons.
- Post-trade/EOD reconciliation artifact: none new; the verdict refreshes
  from the positions and the equity as they then are.
- Weekly review and breach follow-up: `canary policy check` reports the
  realised drawdown of the risk base against D and the realised 20- and
  60-day volatility against the target (`mandate_realised_drawdown`,
  `mandate_realised_volatility`), and names inconsistent mandate keys
  (`mandate_keys_inconsistent`).
- Missed-run or stale-report escalation: stale equity, positions or history
  reads unknown and refuses additions; nothing passes silently.
- Shadow/manual observation period before hard enforcement: none. The owner
  activates the gate by writing the table.

## Verification

- Fixture scenarios (synthetic books only): `internal/mandate` tests pin
  the evaluator (contributions sum to 100%, put-call parity, deep in the
  money ≈ intrinsic, gap caps as specified, beta of SPY on itself is 1,
  FX conversion, missing IV ⇒ unknown); `internal/daemon` tests pin the
  gate at preview, admission and the wire guard (over budget ⇒
  `mandate_budget`; a reduction passes; stale history ⇒ unknown ⇒ refused;
  back within ⇒ additions pass without a reset; absent `[mandate]` ⇒ no
  behaviour change), the capital state (high-water mark, flows, room, a
  reserve change leaves the drawdown unchanged), the history store
  (freshness, one refresh per session), the constitution (fingerprint
  unchanged while absent, unknown mandate keys refused, migration leaves
  the table unchanged) and the policy check rules.
- Property/invariant tests: contributions sum to 100%; room in [0, 1];
  budget 0 at room 0; the gate never refuses a close or reduce.
- Cross-surface parity checks: `canary mandate` golden render (room 0:
  `mandate_no_room`); the MCP tool `canary_mandate`;
  `TestOnlyTheDaemonEvaluates` lists `mandate.Evaluate`. A room-0 verdict
  with risk crosses the daemon's `mandate.risk` writer with the ratio null
  (`TestMandateRoomZeroVerdictCrossesTheWire`, added 2026-10-15 07:41 CEST).
- Redacted before/after artifact: none against a live gateway (fixtures
  only, by instruction).
- Residual risk accepted: the measurement is historical (one year of
  returns, no regime weighting), options are revalued at today's IV (no
  volatility scenario), rates and dividends are zero in the model, and an
  option addition whose IV cannot be derived from its limit price, on an
  underlying with fresh history, is not judged incrementally (the standing
  verdict still gates it). All stated on
  the verdict and here.

## Files

```
internal/risk/mandate.go               [mandate] schema, validation, unapproved keys
internal/mandate/                      pure evaluator: Black-Scholes, historical simulation, verdict
internal/rpc/mandate.go                mandate.risk method, result types, FetchMandateRisk
internal/daemon/mandate_capital.go     high-water mark, room, daily samples (inside the risk_capital document)
internal/daemon/mandate_history.go     decision-grade daily closes, background refresh
internal/daemon/mandate_verdict.go     evaluation cache, RPC handler, mandate_budget gate
internal/daemon/mandate_options_gate.go the premium gate for option buys, the hard holds, the override (2026-10-23)
internal/daemon/mandate_exposure.go    the exposure view: greeks and premium at risk per name and for the book
internal/risk/premium_at_risk.go       premium at risk per leg, name and book (rule 3's figure)
internal/risk/mandate_limits.go        the [mandate] keys as constitution limits; the override control
internal/daemon/mandate_status.go      when the verdict's status last changed (status_since)
internal/daemon/mandate_reduction.go   forced reduction: sizing, hold, release, rows
internal/daemon/mandate_allocations.go       allocation plan and placement, the allocation check
internal/daemon/mandate_sleeve_stop.go  the owner sleeve's measure, high-water mark and drawdown stop
internal/mandate/sleevestop.go         the stop on the evaluation: the sleeve's use against its target
internal/cli/mandate.go                canary mandate [--json]
internal/mcp/tools.go                  canary_mandate
```
