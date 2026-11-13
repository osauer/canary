# Protection and risk reduction

Updated: 2026-11-07 08:25 CET

Proposals are advisory by default. The standard binary cannot place an order.
In a trading build, manual submission requires the exact proposal and its
fresh preview. Optional pre-authorised buckets let the daemon place close and
reduce orders itself under a policy you edit, once you have armed a standing
authority on a paired device ([Pre-authorised buckets](#pre-authorised-buckets)).
Installing Canary does not enable that automation. The cash sweep and currency leveling are cash
management, not protection: they have their own section of the policy and
their own authority ([Cash management](cash.md)).

A blocked proposal row is normally the system refusing to act on evidence it
cannot trust, not a fault to work around. Canary exposes only constrained
close/reduce actions here; use TWS for an unmodelled emergency exit.

## What each command does

| Command | Reaches the broker | Use it for |
|---|---|---|
| `canary proposals status` | no | monitoring and trading status: the pre-authorised buckets, pending counts, and buckets waiting for an arming |
| `canary proposals list` | no | the current proposal set, blockers included |
| `canary proposals refresh` | no | rebuild the set against fresh positions and quotes |
| `canary proposals preview KEY REVISION` | no | mint a preview token and read the broker WhatIf verdict |
| `canary proposals submit KEY REVISION` | yes | place that one protective order |
| `canary proposals reduce SYMBOL --percent N` | only with `--submit` | a discretionary partial close of N percent (1 to 100; the paired app offers 25, 50, 75 and 100); the preview returns a token with the exact terms, `--submit --preview-token` places them |
| `canary proposals request-stop SYMBOL` | no | stage a trailing-stop proposal for one uncovered stock/ETF holding now |
| `canary proposals ignore KEY [REVISION]` | no | record that a proposal should be ignored |
| `canary proposals veto KEY` | no | stop a pending pre-authorised submission of the current revision; human only |
| `canary proposals queue ACTION` (`prepare`, `arm`, `cancel`, `list`, `status`) | the daemon sends an armed record inside its window | carry an order you signed to the next regular session ([Queued authorisations](#queued-authorisations)) |
| `canary proposals prepare`, `prepared-status` | no | hand one proposal to a reviewing console under a private reference, and read its receipt |
| `canary proposals prepare-bundle`, `submit-bundle`, `bundle-status` | `submit-bundle` only | prepare, send and read a currency leveling repayment approved once for all its conversions |

`canary proposals` with no subcommand runs `list`. In a standard build every
submit fails closed: the build carries no broker-write handler, and the daemon
refuses with `order_writes_unavailable` before anything is sent. See
[Gated orders and the trading build](orders.md).

## Proposals close or reduce only

The daemon owns generation. It rebuilds the set from current positions, the
protection policy, and market-event context, and every protection row it
emits closes or reduces, without exception. The cash sweep's bill buys and
currency leveling's conversions are not protection rows: the `[cash]` section
admits them by their own typed bounds ([Cash management](cash.md)). The
monthly rows of a live strategy in the strategy register (`core_rebalance`,
`strategy_rebalance`) may buy; they are not protection either. They are never
pre-authorised, so the daemon's own scheduler never sends them. Each goes on
your device confirmation, or through Canary Desk's controller while a standing
authority you armed stands; a buy needs Fully armed, and a row served for your
review goes only on your confirmation. The
submit path checks a protection row twice, once against the proposal's own
position effect and once against the preview's, and blocks on either. A
protection proposal cannot open, increase, or flip exposure.
`authority.auto_submit` must be false; the policy file fails validation
otherwise.

These buckets generate protection rows. Each is enabled through the
protection policy, except the forced reduction, which runs whenever the risk
policy carries a `[mandate]` table:

- **Trailing stop** (`trailing_stop`) places a broker-side trail against a stock or ETF. Its
  time-in-force is a policy decision, DAY by default, and a DAY
  stop expires at the session close without covering the overnight gap. Set
  `tif = "GTC"` under `[buckets.trailing_stop]` to persist it. The proposal
  spells out which lifetime you are getting. A stop you queue as a
  whole-position stop ([Queued authorisations](#queued-authorisations)) then
  follows the position as it grows or shrinks, without resetting or loosening
  the broker trail.
- **Option loss exit** (`option_loss_exit`) appears only for an exact standalone long contract that
  has a current exact `directional_intents` declaration or qualifies under the
  `default_long_calls_directional` setting. At the
  Rulebook-owned 60% loss of premium paid,
  measured on a fresh live bid against multiplier-adjusted cost, it proposes a
  full DAY patient-midpoint-limit close. It may remain unfilled while the loss
  worsens. It is event-driven: Canary does not install a resting loss stop or
  claim the loss is capped.
- **Option profit trail** (`option_profit_trail` in the pre-authorised list)
  shares the trailing-stop bucket but is a separate
  decision contract. It arms after a 50% premium gain and proposes a
  full-quantity DAY `TRAIL LIMIT` with a native percentage premium trail,
  normally 30%. Spread, minimum-premium-distance and tick floors can widen the
  effective percentage within the 20-50% range. The initial
  rounded stop must retain at least 5% over cost after spread and tick floors.
  Possible hedges, multi-leg strategies, stale/delayed quotes and wide spreads
  remain blocked. The profit trail needs at least 14 DTE; the loss exit keeps
  working to expiry. A hedge-listed index put needs all three: your
  exact intent, a current directional Rulebook role, and exact-ConID
  Greeks evidence; symbol, option shape, and shared-cache Greeks never prove
  intent. Missing or invalid exact-ConID evidence keeps the role unclassified.
- **Option expiry close** (`option_expiry_close`) proposes a DAY limit close of
  an in-the-money standalone long option from the Rulebook's expiry act level
  (7 DTE), so nothing is exercised by accident. It is never pre-authorised.
- **Option exit review** (`option_exit_review`) is the blocked row a
  directional option gets when its exact quote, cost, role or session
  evidence is unavailable ([A blocked row is the system working](#a-blocked-row-is-the-system-working)).
- **Theta hygiene** (`theta_hygiene`) proposes closing an option whose remaining value is mostly
  time value bleeding toward expiry. When the underlying spot or the option mark
  is missing or stale, the row still appears, blocked with
  `extrinsic_uncomputable`, because the daemon then cannot separate intrinsic
  from time value and cannot assert the close is non-destructive.
- **Risk reduction** (`risk_reduction`) trims an issuer whose worst-case loss reaches the
  Rulebook's act level (rule 1, `single_name_act_pct`, 40% by default) back to
  its watch level (`single_name_watch_pct`, 30%; 20/30 for an illiquid
  issuer), on the same measure: the most the issuer can lose at any price,
  every leg netted. It reduces the leg that loses most at that price, capped
  by `max_order_notional`, and becomes a full close only when the computed
  quantity equals the whole position. When that leg alone cannot reach the
  watch level the proposal says so. The levels live in `rulebook-policy.toml`: the
  retired `single_name_target_pct_nlv` key is ignored with a note if a file
  still carries it.
- **Budget reduction** (`budget_reduction`) brings long option premium back inside a declared share
  of the risk constitution's declared risk capital while the constitution's
  drawdown brake is engaged. Its caps are your numbers: the file Canary
  writes shows the table only as a commented placeholder, and the governor
  stays off until you write it; [Budget reduction](#budget-reduction) says how.
- **Forced reduction** (`mandate_reduction`) cuts the book back under the
  mandate's loss room once a breach is confirmed
  ([Forced reduction](#forced-reduction)).
- **Unit exit** (`strategy_exit`) covers every multi-leg unit: each current
  strategy, whatever its source, and every underlying whose legs have no single
  decomposition. Canary never asks which leg is which. It values the unit as
  one position: net premium paid per unit against the net close value at fresh
  leg quotes (long legs at bid, short legs at ask), applies the same
  Rulebook loss line to that net figure, and manages a profit trail itself from
  the unit's high-water close value, since a broker trail cannot follow a combo.
  Two long calls or two long puts of one underlying are not a unit; each is a
  standalone position under the standing defaults. A unit row names its route:
  `canary strategies close ID REVISION` for a recorded strategy, or a combo
  order at the broker when Canary has no strategy record. `canary proposals
  preview` and `submit` refuse a unit with `strategy_workflow_required`; nothing
  about a unit row places an order.

The option-exit policy uses an absolute `0.05` quote-currency `TRAIL LIMIT`
offset. An inherited or omitted offset fails activation. Standing defaults apply
only to standard ungrouped long contracts.
`default_long_calls_directional` classifies long calls and non-hedge-listed
long puts as directional unless they cover a holding: a call over a short stock
of its own underlying, a put over a long stock of its own underlying, or an
index option over such a stock anywhere in the book, which makes the option
protection. `default_index_puts_protection` excludes hedge-listed puts from
directional exits. Exact declarations, including expired declarations, take
precedence. A contract no standing default covers, such as a short option, a
non-standard multiplier or a strategy leg, stays yours to review; Canary
requests no quote for it and reports no quote blocker. Without a qualifying
standing default or exact intent, an exit stays blocked.

### Budget reduction

The bucket measures the market value of every long option leg Canary does not
hold as protection — a hedge-listed index put (`SPY`, `SPX`, `SPXW`, `QQQ`,
`IWM`), a put over a long stock of its own underlying, or a call over a short
one is never counted and never selected — against two caps you write as a
percent of `capital.declared_risk_capital` from `risk-policy.toml`:

```toml
[buckets.budget_reduction]
enabled = true
mode = "shadow"                            # shadow (default) or active
premium_at_risk_pct_of_risk_capital = 40   # total, (0, 100]
per_line_pct_of_risk_capital = 15          # one line, at most the total
max_order_notional = 10000                 # one order, as risk_reduction
```

Bump `policy_version` when you add it. Neither cap has a default in code: a
value you did not write is a value the bucket does not have. An enabled table
missing a cap or `max_order_notional` stays valid, and the governor switches
itself off with the state `needs_your_number`, naming the keys it waits for;
every other bucket keeps working. The numbers above are an example; they
are not a recommendation from the software.

It generates rows only when all of the following hold, and the snapshot's
`budget_reduction` status names the first one that does not:

| State | Meaning |
|---|---|
| `needs_your_number` | the table is enabled but a cap or `max_order_notional` is not written; the reason names the keys |
| `constitution_unapproved` | no risk constitution is active, or a material key is unapproved |
| `enforcement_shadow` | `drawdown.block_enforcement` is not `hard`; a risk policy that passes validation never shows it, because only `hard`, or the key left out, is accepted |
| `not_latched` | the drawdown block tier is neither latched nor breached |
| `currency_mismatch` | the constitution's base currency is not the account's |
| `unmeasurable` | no long option leg has a base market value |
| `within_budget` / `over_budget` | measured; the status carries the total, the share of risk capital, and the leg counts |

Selection runs per line first: a line above the per-line cap is cut to the
cap in whole contracts (`keep = floor(cap ÷ value per contract)`), and a cap
that leaves no whole contract is a full close. Then the total: while the
projected sum still exceeds the total cap, lines are cut in whole contracts,
ranked by what the sale fixes, until the sum is within the cap:

1. the number of open Rulebook rules the line relieves: rows at watch or act
   among rules 1, 2, 4, 5, 13, 16 and 18 on which the line, or its issuer, is
   an offender, read from the Rulebook result Canary already holds;
2. the share of the line's value that is time value, highest first (unknown
   last);
3. the unrealised loss, largest first; then the line's value; then the
   contract id.

Without a current Rulebook result no line relieves anything, the ranking
starts at time value, and the status says `ranking_without_rulebook`. Every
row names the cap that selected it, the measured line and total, the excess,
its place in the order and the order used, under `budget` in JSON and on a
`Budget:` line in `canary proposals list --details`. `max_order_notional`
bounds one order exactly as `risk_reduction.max_order_notional` does; the next
cycle measures what is left. A stale mark blocks the row with
`fresh_option_quote_required`; a leg of a multi-leg unit is measured but
routes to the strategy workflow. Rows are close or reduce only, like every
proposal.

**The whole fix in one place.** The `budget_reduction` status lists up to
three `candidates`, the ranked lines with their contract, contracts held,
unit value, the rules they relieve (`relief`), `time_value_pct`, unrealised
P&L and a one-line `why` ("offends 2 open rules; 50% time value; unrealised
−500"), and the `plan`: every order the measurement needs, each with its
`rank`, `contract`, `contracts`, `raises_base` and `cycle` (1 for this
refresh, 2 and later for what `max_order_notional` holds back). `canary
proposals list --details` prints both under the Budget header, and every
governor row there adds three detail lines: its place in the plan, the other
open rules the sale relieves, and the next two candidates with their `why`.
Ignoring a row takes its line out of the plan: the next refresh moves to the
next candidate.

Index-put classification is shared with the Rulebook for budget reduction
plans. A measured directional put can enter that plan even when its symbol is
on the hedge list; measured protection stays excluded. The forced reduction
never sells a hedge-listed long put, whatever it measures as.
Missing classification holds the line and is reported separately from
protection. Planning may use the same closed-session observations as the
warning. Before preview or prepared submission admits a reduction, a fresh,
complete broker measurement must still confirm that the put is directional;
the existing scope-bound evidence expires with the order token. The policy
limits, order caps, veto window and approval requirements still apply.

**Measured against the Rulebook instead.** `basis = "rulebook"` replaces the
two caps with limits you already keep in the Rulebook policy, as shares of NLV:
a line is cut to `option_line_act_pct` (its premium at risk being the higher of
price paid and value), and when the book's premium at risk reaches the premium
budget's act level of the regime set in force (Rulebook rule 3,
`premium_budget_act_pct`), lines are sold in the same ranked order,
each contract counted at its premium at risk, until the total is back at the
budget's watch level (`premium_budget_watch_pct`). This basis needs the
account's NLV, not a risk constitution or available funds, and it waits for no
drawdown brake: the rows appear whenever those limits are breached. Leave out
the two percentages; the file fails validation with both a basis of `rulebook`
and declared-capital caps.

```toml
[buckets.budget_reduction]
enabled = true
mode = "shadow"
basis = "rulebook"
max_order_notional = 10000
```

The state is `account_unavailable` when NLV is missing; the status then
carries `per_line_pct_of_nlv`, `premium_budget_watch_pct`,
`premium_budget_act_pct` and `premium_budget_set`, the account values,
`premium_pct_of_nlv` and, once the act level is reached,
`premium_excess_base` (the premium at risk above the watch level), and each
row names the limit that selected it.

This basis sells against the Rulebook's numbers, which may still be Canary's
defaults. Your `mode` applies whatever state the Rulebook policy file is in:
you approve every order, and that approval is the gate. The status names the
state in `rulebook_review`, and while it is anything but `reviewed` every row
carries one detail line saying so:

| `rulebook_review` | Detail line on every row |
|---|---|
| `reviewed` | none: the file in force is yours, read cleanly |
| `unreviewed` | "Rulebook limits: Canary's defaults, not yet reviewed" (the file still opens with `# Canary defaults, not yet reviewed.`) |
| `no_file` | "Rulebook limits: no Rulebook policy file; compiled defaults" (`canary policy ensure` writes one) |
| `drift` | "Rulebook limits: the file on disk is not the one in force" (edited without a higher `policy_version`, or removed) |
| `error` | "Rulebook limits: the file could not be read; the last good file applies" (before any good file, Canary's compiled defaults apply) |

`canary proposals list` names the state on the Budget header. The
declared-risk-capital basis measures your own caps and reads no review state.

**Shadow first.** In `mode = "shadow"` the rows are generated, journaled in
the snapshot with `shadow: true`, marked shadow in `canary proposals list` and
listed by `--details` under a *Shadow (budget reduction)* heading of their
own, so you can read what the rule would have done beside what you did.
`preview` and `submit` refuse them with `shadow_mode`, and the
pre-authorisation scheduler skips them: a shadow row is never eligible for
automatic placement. `counts.budget_reduction` and `counts.budget_reduction_shadow` report
the rows; shadow rows are never `actionable`.

**Activating.** Set `mode = "active"` and bump `policy_version`. The rows
become ordinary proposals under every existing gate (preview, WhatIf, the
double check of position effect, duplicate orders, account, mode, freeze,
origin and the explicit submit). One thing does not change with the mode:
every row carries `never_skip_veto`, so a scheduler that shortens or skips the
veto window under a latched brake must still wait the full window for these.
A reduction to budget is a discretionary-scale action, not a stop.

### Forced reduction

When the risk policy carries a `[mandate]` table, the daemon compares the
book's loss measure with the loss room the mandate still allows
(`canary mandate`). The loss measure is today's value of every bought option,
what it can still lose; a hedge-listed long put is protection, named beside
the measure and left out of it, so a hedge never forces the sale of what it
hedges (the Rulebook's premium budget still counts every bought option at the
higher of the price paid and its value, protection included),
plus the cash loss to the stop of every share line a working stop or trail
covers, plus a held index-hedge future's loss in a 10% index move against it
(or to its stop, when a working stop covers the whole line). A share line
without a stop counts nothing and is named. The loss room is `max_loss_pct` of
the budget base, less the drawdown already taken; the budget base is the
high-water mark of the risk base, moved by deposits and withdrawals since the
start ([Concepts](../understand/concepts.md#mandate)). The forced reduction is
the batch of `mandate_reduction` rows that cuts the book back when that
measure passes the room.

**Trigger.** The daemon evaluates the mandate in the background every two
minutes. Two consecutive background verdicts reading `reduce`, at least five
and at most fifteen minutes apart, start a breach episode. Only a verdict at
least five minutes after the last one counted joins the streak: the verdicts
in between neither count nor reset it, and one more than fifteen minutes later
starts the streak again. A verdict you request with `canary mandate` never
counts, so polling cannot start a batch.
Until the second verdict lands, the `mandate_reduction` status reads
`watching`.

**Size.** Each verdict sizes the batch afresh, on the book that verdict
measured. Canary cuts step by step and measures the book again after each
cut: first a losing line of a name over its gap cap, then the premium at risk
that is left. Each time it picks the cheapest relief: the option whose premium
at risk (the higher of the price paid and its value) is largest against what
it is worth today, so a deep out-of-the-money option goes before a dated one,
and both go before shares, whose loss to the stop is a fraction of their value.
Each cut is the smallest whole step (contracts, or share lots) that takes the
loss measure to 95% of the loss room with every gap cap met; the 5% margin
keeps the next verdict from reading over by a hair. An in-the-money long
option within three sessions of expiry is cut first only when its exercise
alone would push the planned book over the room. Every row is held to the
money caps in force (`[order_limits]` and `[buckets.risk_reduction]
max_order_notional`). An option row is held to
`[order_limits].max_option_contracts` as well unless the order gate would pass
it as an [order that reduces delta](../understand/policy.md#order-limits),
which a sale of a bought call normally is; what a cap holds back goes in the
next cycle.

**What it never does.** It never buys back a short option, and never sells
the shares that cover one: a covered position needs your hand, and the status
names it. It never cuts shares below what the long puts on them would deliver
at exercise. It never sells protection, a hedge-listed long put included,
whatever that put measures as; the put's premium still counts in the measure,
so the reduction alone may not bring the book back inside the room. It never
sells a holding on your keep list (`canary locks`, below). A short index hedge never
makes it sell: the cuts are sized on the book without its short hedges. When
the episode ends, orders already at the broker are never cancelled.

**Send order and price.** Expiry cuts go first; the rest go nearest expiry
first, options before shares, and the cheaper relief first within one expiry.
A row whose veto window has closed waits, for at most one more window, for a
better-ranked row still inside its own. A pre-authorised cut after the open is
a limit halfway from the mid toward the bid, as long as the spread is within the line's cap in the protection policy (25%
of the mid for an option and 2% for shares when the policy sets none) and the
price stays within 25% of the row's mark; otherwise the cut goes at the mid.
An option cut whose spread is past its cap goes at the bid when the give, the
mid minus the limit over the whole order, is at most
`[mandate.reduction] cheap_leg_give_pct_of_loss` (2% as Canary writes it) of
the loss measure the cut removes; otherwise it goes at the mid.

**End and hold.** A verdict reading within the room ends the episode and
withdraws the standing rows. An unknown verdict, or one measured on a
different book from the one held (after a fill or a hand trade), holds the
rows unsent and says why. The daemon places rows itself only when
`mandate_reduction` is in `[authority].pre_authorised` and Protection or Fully
armed stands ([Pre-authorised buckets](#pre-authorised-buckets)), and every
row waits its full veto window; otherwise the rows wait for you.
`canary mandate` shows the status, the loss, the room, the 95% target, the
projected loss and every order in dispatch order.

`canary proposals reduce` is separate: a discretionary partial close you size
yourself. It previews unless you pass `--submit`. Under `--portfolio` the
mode says what is cut. `--mode net`, the default, removes that share of the
book's net delta-adjusted risk, only on lines in the net direction. `--mode
longs` sells the same share of every long-delta line and keeps every short
and long put. `--mode all` cuts that share of every line, buying back shorts.
`--sector` (repeatable, GICS names) and `--min-beta` narrow a longs or all
sweep. `--plan` sizes the legs without broker previews and scores them; a
plan cannot be submitted.

A plan is scored on both sides against the loss limit, each figure on the
book now and with every planned leg filled, in the base currency:

- The index fall, from the mandate's index stress: the index falls 10% and
  every name follows with its beta, options repriced
  (`index_shock_pct`, `index_loss_before_base`, `index_loss_after_base`).
- The index rise, the same shock upward (`index_rise_pct`,
  `index_rise_loss_before_base`, `index_rise_loss_after_base`). A negative
  loss is a gain, on either side.
- The mandate's loss measure, as [Forced reduction](#forced-reduction)
  defines it, against the loss room the mandate still allows
  (`loss_measure_before_base`, `loss_measure_after_base`,
  `loss_room_base`). Locked lines count in it and are never traded.
- The trading cost: half the quoted bid-ask spread on each leg, times its
  units and multiplier (`trade_cost_base`). Commissions are not in it. A
  share line is quoted off its position row; an option row carries only a
  mark, so an option leg is quoted by its exact contract, within two
  seconds a leg, as the hedge's future is.

`--mode hedge --plan` prices an index hedge instead of a cut. It sells
Micro E-mini S&P 500 futures, the front quarterly contract on CME (a
contract in its expiry week is skipped for the next), until a 10% S&P fall
leaves the whole book about even. The size is in whole contracts, rounded
toward less hedge, so its modelled payout in that fall does not exceed the
book's modelled loss. Every line
counts toward the size, locked ones too, and hedge sales already working
count as sold: the plan offers the rest to neutral and says how many are
working. A working sale of a line you hold long counts as sold too, for what
remains of it, when selling that line lowers the loss in the fall, so a cut
still working cannot leave the hedge past neutral when it fills. A stop or trail
does not count until the market reaches it. If that sale is cancelled, the hedge
covers less of the fall than the book would lose, and the next plan sells the
rest. A working sale Canary cannot read is left out of the size, the plan says
so, and the sale is refused until it can be read. The plan carries one leg with its
`contract` (month and multiplier) and the same scores as a cut. When the
fall already leaves the book even or ahead, or its loss is less than one
contract offsets, the plan sells nothing and says why. A held future counts
in the mandate like every other line: its gain or loss in the 10% fall and
rise, and in the loss measure its loss in the adverse index shock
(`index_hedge_base`), so a hedge spends loss room. Effective full stops use their contract-value distance instead.

In the mandate's gap stress the future moves with SPY and every other broad
index fund or index on the list: one index gap moves them all together,
each by the full gap. The
gap cap applies to their net loss, so a hedge lowers the index gap loss and
can bring a breached index line back under its cap. With nothing else in the
group, as with a book of single-name calls, the hedge reads as a breach of its
own, and the book reads `reduce` until it is bought back; that breach does
not refuse the sale. `canary mandate` shows the group under
Stresses as `Index gap ±12% across` with its lines and their net figure.
The stress does not model the difference between SPY and the future.

Without `--plan`, `--mode hedge` previews the sale and the leg carries a
signed preview token. You place the sale yourself, in one of two ways: from a
terminal in the trading build with `--portfolio --submit --preview-token
TOKEN`, or through the device-confirmed basket in Canary Desk. Either places
exactly those terms; Canary never sells a hedge on its own. The sale is a
limit at the bid, good for the day, in US regular hours only, with no stop
order. Placing needs `[mandate.index_hedge] max_contracts` in the risk
constitution: without it the leg says `index_hedge_unplaceable`, and a sale
that would leave the book more contracts short than that says
`index_hedge_over_max`; it is never resized to fit. While IBKR has shown
futures not enabled for the account, the sale says
`broker_permission_not_enabled` ([Trading permissions at
IBKR](orders.md#trading-permissions-at-ibkr)). At submit the hours,
the cap and neutral sizing with all pending sales across contracts and clients and the bid are read again. A
held hedge is bought back as a percent reduce of its own line,
`canary proposals reduce --con-id ID --percent N --include-hedges`; no
portfolio sweep selects it.

A sale is admitted when it lowers the loss in the 10% fall without passing
neutral and does not turn the 10% rise into a loss or deepen one. The loss room
and the gap cap do not refuse it, so the hedge stays open at the maximum loss.
The drawdown brake and the mandate gate read the
same rule. A sale past neutral, or one that would make the rise lose, is refused
(`index_hedge_past_neutral`, `index_hedge_rise_loss`), and the plan offers the
largest sale that passes and says what holds it lower.

While an option has no volatility of its own and is priced on its mark, neutral
is read at the end of its band that loses less, so a sale cannot pass neutral at
either end, and the plan says so. A hedge already held is left as it is. What
this leaves unhedged, the next plan sells once the option has its own volatility.

During a mandate breach the reduction buys back what has passed neutral, in the
`mandate_reduction` bucket with its veto window, as a limit at the ask in US
regular hours (phase `hedge`). As the calls are sold, or a fall leaves them less
to lose, the short would otherwise end a bare short of the index. A contract is
bought back while the book without it is still even or ahead in the fall, or
while the hedge makes the rise lose more than the book would without it. A
locked hedge, an ignored row and a buy-back already working are left as they are
and named under what was left out. This buy-back is the only hedge order Canary
places on its own, and only as a `mandate_reduction` row under the arming
([Pre-authorised buckets](#pre-authorised-buckets)); otherwise the row waits for
you. A hedge is sold only when you place it. Within the loss room, with no stop
on your own sleeve to act on, nothing buys it back, because the measure already counts its rise
loss; `hedge_not_needed` says so. The close waits out its veto window from the
verdict that first sees the excess, and moves to the next open if that runs past
the close, so a short past neutral can stay overnight.

A hedge never makes the reduction sell. The cuts are sized on the book without
its short hedges, and a book inside its room without them sells nothing. The
hedge's rise loss still shows in the measure and the batch's projection.

While a hedge is held, the hedge plan and preview carry two flags for you.
You roll the hedge by hand; the flags themselves trigger nothing:

| Field | Set when | Message field |
| --- | --- | --- |
| `roll_due` | from ten days before a held Micro S&P contract's expiry | `roll_due_message`: the contract, its expiry and the date the roll became due |
| `hedge_not_needed` | the book without its held short hedges reads `within` on the mandate | `hedge_not_needed_message`: the loss measure without the hedge against the loss room |


The written contract cap bounds position size; it does not cap the loss of a
short future if the index rises. The rule above reads the model's betas, so it
assumes the names held follow the index: if the index rises and they do not, the
short loses its face value times the rise, about the fall's loss it was sized to
offset at 10% and more beyond, until it is bought back. The modelled upward shock
is a comparison, not a maximum possible loss.

A figure the plan cannot give is absent, and `index_loss_unknown`,
`index_rise_loss_unknown`, `loss_measure_unknown` or `trade_cost_unknown`
says why. The cost is unknown when any leg's line has no two-sided quote:
a partial cost would read as the plan's. The figures are served, never
stored. A preview carries the same fields.

Canary's own proposals carry the same figure. `canary proposals list`, and
the proposal snapshot it reads, give each row an `index_loss` object with
the index fields the plan carries at its top level: `index_shock_pct`,
`index_loss_before_base`, `index_loss_after_base`, and `index_loss_unknown`
when there is no figure, and the rise's `index_rise_*` beside them. The before figure is the book now; the after figure
is the book with that row filled. The snapshot's own `index_loss` fills every
actionable row together, and its `base_currency` names the currency. A
blocked or shadow row is priced on its own but left out of the total. A stop
or trail reads unknown, because it fills only when the market reaches it. A
cash sweep or currency row carries no figure. The figures read the mandate's
last evaluation under the current policy and are never stored; without one,
every row says the figure is unknown and why. Reading the list never runs an
evaluation.

To price a chosen set of proposals, name them: `canary proposals reduce
--portfolio --plan --proposal KEY` (repeatable) returns a plan in mode
`proposals`, one leg per named row at its own size, with the same scores
as a sweep plan. The after figures are the book with every named row
filled; a row selling held units takes them off the line at the line's own
premium or distance to the stop. The cost reads each row's held line; a row
for a contract the book does not hold has no quote, so the cost is unknown.
A row that is not current, or that cannot be priced (a stop, a trail, a
cash or currency row, an option without the terms to price it), is a
blocked leg, and then every figure is unknown with the count of rows not
priced: a partial figure would read as the whole set's. `--percent`,
`--mode`, `--sector` and `--min-beta` do not apply, and a plan cannot be
submitted.

A reduction reserves the quantities already committed by Canary's working
orders, including individual legs of combo orders. A known fill does not
make those units available for another sale while positions are catching up:
Canary checks a completed broker position snapshot against the position used
for the reduction and refuses while they disagree or cannot be confirmed.
An order-state change before transmission also requires another review.
These checks, and the reviewed worst price, apply when a reduction token is
submitted through `order place` as well as through `proposals reduce`.

No portfolio sweep, in any mode, selects a locked holding. `canary locks
lock SYMBOL --role hedge|core` puts a name on the keep list: a hedge is a
hedge because you say so, not because of its delta sign that day. A lock
names the underlying and covers every contract of it the book holds when a
sweep runs: the shares, the calls written on them, the puts, and the
contract a roll moves into. `--con-id` is optional and records the contract
the lock was placed from, which the lock also covers. Net mode sizes and
directs its cut on the book without its locked hedges: a locked core name
counts, though it is never traded, so the puts that protect it are not read
as a bare short and sold. A locked hedge never sets the direction. The
mandate's automatic reduction and the
premium budget sweep leave locked names out too, and `positions.risk`
carries the keep list resolved against its book under `holding_locks`.

A lock on a name the book no longer holds keeps nothing. It is not pruned,
since removing a lock is yours: `canary locks` and `holding_locks.list`
report it as not held (`held: false`, no `contracts`), and it holds the name
again once a line of it is held. A list that names one contract per lock
reads too: the entries of one name fold into one lock on that name, and one
name saved under both roles reads as a hedge. The next save that changes the
list writes it in the folded form. If the keep
list cannot be read, automatic reductions wait; an unreadable list never
means no locks. A
lock save waits for any in-flight broker transaction before acknowledging the
new list. It does not cancel orders already sent. A reviewed basket refuses,
and sends nothing for, a leg locked after the review. A single-holding
`proposals reduce SYMBOL` still works on a locked line. Under `--portfolio`,
`--include-hedges` is refused with an error.

A net portfolio preview also discloses signed net dollar delta before and the
estimated delta after eligible orders fill, using current prices and Greeks;
blocked orders do not count toward that estimate. Its `untouched_positions`
list names every nonzero holding left unchanged, with exact contract identity,
held quantity and the reason: locked by you, opposite-direction delta,
unsupported position, whole-unit rounding, or a blocked preview. An empty list
means no untouched holdings; null means the review is unavailable, as for a
scoped sweep, which lists what it keeps under `kept`. These fields only explain
the existing sizing and never change which orders are selected.

A reduce preview returns, for each order it would place, a signed preview
token and the exact terms that token binds (`terms`): the contract, side,
quantity, order type, limit, time in force and algorithm the broker will
receive, the token's expiry, and the worst price the submit will hold the
market to. `canary proposals reduce --submit --preview-token TOKEN` places
exactly those terms, nothing re-sized and nothing re-priced; under
`--portfolio` pass one `--preview-token` per leg, and each leg is placed on
its own, so a refused leg never holds another. The daemon refuses, and sends
nothing, when the token is stale or expired, when the position no longer
holds the reviewed quantity on the reviewed side (a sale of at most the
confirmed long is admitted; a smaller position refuses as `position_changed`
rather than shrinking the order), when the market is already beyond the
reviewed worst price (`beyond_worst_price`: the bid below a sale's limit, or
the ask above a purchase's; a market that moved the other way is no reason
to refuse; stated in every preview as `price_bound`), when the token was already submitted (`token_consumed`), or
when any order gate refuses, Canary's own stop on the line included (a reduce
sale joins the stop's one-cancels-all group or waits for the stop to shrink,
like any other sale). `--request-ref REF` names a submission: a retry with
the same reference replays the recorded result and never sends twice.

Two reductions of one line never together sell more than is held: a reduction sizes its percentage from
what Canary's own working orders on the line leave unsold (stops aside,
which their one-cancels-all group and the stop guard keep within the line)
and states that quantity as `committed_quantity`; the send refuses
(`reduce_already_working`) when another of Canary's sales went out in
between. Hand orders in TWS do not count. A send that may have reached the
broker is `submit_unclear`, never `submit_failed`. Under `--portfolio`, a
stock the broker reports as having no market and values at zero counts as
no exposure and is listed as left out (`defunct_holding`) instead of
blocking the book.

A reduction is a limit priced as a worst price, DAY: a sale's limit sits `[order_limits].reduce_allowance_pct` below the
bid (`reduce_option_allowance_pct`, and at least one tick, for an option), a
purchase's that far above the ask, rounded toward the touch. The order can
fill at once in a fast market, and the broker fills a marketable limit at
the best price available. On a US stock or ETF routed SMART it carries IBKR's
Adaptive algorithm at `Normal` priority unless `--adaptive-priority` asks
for `Patient`, `Urgent` or `none` (a plain limit). On an option, a non-USD
line or a directed route the order is a plain limit and the terms say
"Adaptive not supported for this contract"; the broker layer sends Adaptive
only for that one stock shape.

`--submit` without a token is the hand path: it previews the holding at the
live book and places that in one call, with the terms it sent in the result.
Anything that reviews terms with a person first, Desk and the paired app
included, submits the token.

`canary proposals request-stop` answers the coverage ledger directly: name an
uncovered stock/ETF holding (symbol, or `--con-id` when ambiguous) and the
daemon rebuilds the proposal set and returns that position's trailing-stop
proposal with the key and revision to preview. It generates only — placing the
stop still goes through `preview` and `submit` with every gate intact. An
earlier `ignore` for that stop is cleared by the explicit request, and the
result says so. The paired app offers the same action from the Protection
panel's uncovered-positions list.

Option exits do not enter the protection coverage ledger. That ledger remains
stock/ETF stop coverage; calling a directional option exit portfolio protection
would overstate what the broker is actually covering. Option proposals still
use the same preview, WhatIf, full position-effect, duplicate-order, account,
mode, freeze, origin and explicit submit gates as every other broker-adjacent
proposal.

### Option hedges are listed, not proposed

A held long option the engine holds as protection produces no exit row, but it
is not silent either. The snapshot lists it under `option_hedges` with what it
covers (`book` for a hedge-listed index option, otherwise the underlying whose
stock it covers), the Rulebook's role for a hedge-listed put (`protection` or
`unclassified`) and how that role was established: `measured` by Canary's check
of the whole book, `structural` because it covers a holding of its own
underlying, `unmeasured` when that check has not completed (the detail says
why), or `closed_market` when it waits for the options session. The check is
Canary's own work and asks nothing of you; the standing rule applies
until it completes, and the detail sentence says so in plain words. A
hedge record carries its days to expiry, cost basis per contract unit, mark and
market value, and no threshold, premium return or order terms. A hedge the
classifier measures as directional leaves the list and appears as an exit
review among the proposals. `counts.option_hedges` reports the list size
separately from the proposal counts.

### One row per contract

Two buckets can ask to sell the same contract. A loss exit and theta hygiene
both close a near-expiry call deep in loss, and the budget governor and the
issuer trim can both cut one line. Canary shows one row for each exact
contract and side, and that row carries every reason under `covers`. Its size
is the largest single requirement, never the sum: each sale asks to sell at
least its own quantity now, so the largest meets them all. When sizes tie,
the Rulebook's exits come first (loss exit, expiry close, profit take), then
the issuer trim, the budget governor and theta hygiene; the orders are then
identical. The other rows stay listed, blocked with `covered_by_proposal`,
and name the covering row in `covered_by`.

Three rules settle the cases where the largest would be wrong:

- An unapproved stock sale does not suppress stock protection. Both proposals remain available until an actual order
  or confirmed intent invokes the existing duplicate-order checks.
- Otherwise, an immediate sale goes before a trailing stop, whatever the sizes, unless
  the stop is pre-authorised (below). The stop is conditional and would hide
  the sale. It is covered until the sale fills or is cancelled, then
  re-proposes for what is left.
- A pre-authorised row that Canary will still place is never covered by a row
  that needs your approval. Unless it already meets the larger requirement,
  Canary places it after its veto window, and the row you approve lists it
  under `covers` with `automatic: true`. Once its automatic submission has
  ended (vetoed, failed, superseded, or placed and no longer working), the
  row is left out of the merge, so the rows you can approve stay available.

Once an order works at the broker for the contract and side, the exits,
trims, budget and theta rows for it block until that order fills or is
cancelled, and they then recompute from the new position:

- Theta hygiene, issuer trims and budget reductions block with
  `existing_reduction_order` on any same-side order that the broker's complete
  open-order inventory shows working (not cancelled, inactive or rejected) for
  the exact contract in the account. It does not matter which client placed
  the order or what type it is. A trailing stop counts too: selling beside a
  stop would leave the stop larger than the position.
- They block with `reduction_order_evidence_unavailable` when that inventory
  is missing or stale, and with `reduction_order_identity_unknown` when a
  working order may match but carries no contract id.
- Option exits keep `existing_option_exit_order`.
- A stock trailing stop also waits, with `existing_reduction_order`, while a
  sale Canary proposed works for its position.

Preview and submit repeat the check against a fresh broker read. The brief
counts covered rows apart from blocked ones.

## A blocked row is the system working

Every blocker carries a code, a message, and an action line. Under stress the
action is the part to read, because it names the next command.

An active regulatory or news halt blocks the row. So does an active LULD pause.
That is deliberate: a protective order priced against a symbol that is not
trading is a guess about the reopening print. Recent flags that are no longer
active stay visible as context and do not block.

Other rows block because the evidence is not good enough. A stale option-exit
quote raises `fresh_option_quote_required`. A revision older than that row's
current one raises `stale_revision`; each row carries its own revision, so a
neighbour that moved (a cash sweep re-sized on net liquidation value, say)
leaves a reviewed stop current. A time-in-force that drifted between the
proposal and its preview raises `tif_drift`, and a quantity beyond the position
raises `quantity_outside_position`. The row stays visible with its reason
attached rather than quietly disappearing.

A directional option whose exact quote, cost, role, or session evidence is
unavailable appears as a blocked **Option exit review** row. It is not
silently dropped and it cannot be previewed as an order.

## When a stop no longer matches its position

If a position shrinks or goes flat while a close-only protective order is still
working, the daemon marks that order `position_mismatch` and grades it critical.
Triggering it would open an opposite-direction position rather than close
anything.

| Kind | What it means | Fix |
|---|---|---|
| `short_entry_full` | no coverage left | cancel the order |
| `short_entry_excess` | partial coverage | reduce to `reduce_to_quantity` |

`reduce_to_quantity` is the position magnitude available in the order's closing
direction: the long share count for a SELL, the short magnitude for a BUY. It is
the exact quantity a reduce-modify has to target, and it appears with
`short_risk_quantity` in `canary orders open --json`. The same holdings show up in
`canary positions` as `reconcile_required` under protection coverage, where a
stale protective order is deliberately not counted as protection.

For a sell stop or trail Canary placed on a stock, ETF or option, the
protective stop guard makes that fix itself (next section). An order placed by
hand in TWS is yours: Canary reports it and leaves it alone.

## Protective stop exemption and guard

A broker-side stop that protects a whole long stock position is usually larger
than the order cap in force (the risk constitution's `[order_limits]`, scaled
with the book; [Order limits](../understand/policy.md#order-limits)), and every
apparent stock sell exit is otherwise read as opening a short, which needs
`[order_limits].allow_stock_short`. A protective stock
exit is exempt from both:

- a stock or ETF `SELL` stop (`TRAIL`, `TRAIL LIMIT`, `STP`, `STP LMT`);
- the position is long and the stop sells no more than it holds, so it closes
  or reduces and never opens or flips;
- the broker's complete, current open-order list, hand orders in TWS included,
  shows no other working sell for the same contract that together with this
  stop would sell more than is held.

If the open-order list cannot be read, the exemption does not apply, and the
order cap and the short check refuse the stop. A modify that only lowers the quantity of a working
stop is exempt on the same terms without the third condition, because it can
only shrink what is already working. Every other order keeps the short
re-read. The notional cap and `[order_limits].max_option_contracts` also pass
any close or reduction that lowers the absolute delta of its underlying and of
the book without its protection puts, a stock sale, an option close or a
strategy close alike
([Order limits](../understand/policy.md#order-limits)); an order
whose delta cannot be measured keeps both caps.

A trailing-stop row that the gates would refuse does not read ready. It
carries `protective_exit_competing_sell` when another working sell already
covers the shares, `protective_exit_inventory_unavailable` (waiting for the
broker) when the open orders cannot be read, or
`protective_exit_exceeds_position` when the stop is larger than the position.

A resting GTC stop outlives a later hand sale, so the exemption comes with a
guard. On the proposal cadence (30 seconds by default) the daemon compares each
working sell stop or trail it placed on a stock, ETF or option with the current
position, less any sale of Canary's own that is working beside it without being
linked to it (next paragraph): at zero or short it cancels the stop; below the
stop's remaining quantity it shrinks the stop to the whole-unit position,
keeping the trail amount or percent, the current stop price, the limit offset,
trigger method, time in force and outside-hours flag. It acts only on a
current, complete portfolio download for the connected account and a complete
open-order list, and only when the same correction is planned on two passes at
least 20 seconds apart, so a partial download or an out-of-order fill never
moves an order. Each change goes through the ordinary modify or cancel path
with the origin `daemon-protective-guard`, is journaled on the order, and is
written to the decision log (`protective_stop_guard.shrink` or `.cancel`). The
order's `position_mismatch` notice stays open until the change lands.

Two orders Canary placed never sell the same shares twice. Every stop or trail
Canary places is born in its own broker one-cancels-all group, named after its
order reference (`oca_group` in `canary orders open --json`). When you sell
the line through Canary while that stop works and the two together would sell
more than you hold, the sale joins the stop's group: the broker reduces the
other order by whatever fills first, a partial fill included, and routes one
of them at a time, so the stop cannot sell what the sale already sold, nor the
other way round, without waiting for the guard (decision log
`protective_oca.link`). An older stop placed without a group cannot be moved
into one while it works, so Canary first shrinks that stop to
what the sale leaves, or cancels it when nothing is left, waits for the
broker's answer, and only then sends the sale (`protective_oca.shrink`,
`.cancel`). If that change is refused, unconfirmed within a few seconds, or
its send is uncertain, the sale is refused with the reason and the stop stays
as it is (`protective_oca.refuse`); a sale is never sent unlinked beside
Canary's own stop. A stop you placed by hand in TWS is never linked or
changed: the sale goes out as it is, and the guard reports the hand stop.

Every write gate still decides. With trading disabled, the broker link down or
daemon storage unhealthy the guard does nothing and logs the blocker once.
`trading.freeze` blocks every modify, so a shrink waits while frozen; a cancel
still goes through, because a freeze never strands an order that needs
cancelling.

The residual gap: while the daemon is down, a hand sale is not followed, so a
stop can stay larger than the position until Canary is back and the guard has
run; and a hand sale beside Canary's stop, or beside a Canary sale, can still
sell the line twice until the guard's next passes.

## Pre-authorised buckets

The protection policy's `[authority].pre_authorised` list is empty by default.
Only your own policy edit, with a higher `policy_version`, enables the buckets
it names:
`trailing_stop`, `option_loss_exit`, `option_profit_trail`,
`budget_reduction` or `mandate_reduction` (the
[forced reduction](#forced-reduction)).
Budget and mandate reductions always wait the full window, even with the
drawdown brake latched. The cash sweep is pre-authorised in its own section,
`[cash] pre_authorised`, under the same veto window
([Cash management](cash.md)); currency leveling never is.
`auto_submit` remains false; it is not the switch for this scoped scheduler.

The list says which buckets may go; the arming lets Canary act on it. The
daemon places nothing itself until Protection or Fully armed stands: a
standing authority you sign once on a paired device, which the daemon keeps in
its own store. It survives restarts and needs nothing else running but the
daemon itself: while the machine sleeps, nothing is checked or sent. Both
levels let the daemon place the pre-authorised rows. Fully armed also lets
Canary Desk's controller send buys that add risk, such as a strategy row's
buy; under Protection it sends only protection and reductions. While nothing
is armed no record is made, the rows wait for you, `canary proposals status`
names the buckets as waiting, and `auto_trade.pre_authorised_disarmed` lists
them. Arming
starts a full veto window for every row; disarming ends every window that is
running, and arming again starts them afresh. During a breach of the mandate's
loss room, Fully armed is held at Protection until you confirm full again on a
device once the book is back within the room. The only signer is Canary Desk,
a private project: its device-confirmed arming signs the authority. In an
open-source install nothing is armed, so every pre-authorised row and every
automatic sweep order waits for you. An order already at the broker stays
there when you disarm.

`veto_window` defaults to `"30m"` and cannot be less than five minutes. The
window starts after the daemon records the Protection alert in its registry.
The paired app or another consuming application owns notification delivery;
registry acceptance does not prove that a phone received or displayed it, and
neither does push-service acceptance. A push is **witnessed** only when a
paired device reports it back: the app's service worker sends a `displayed`
receipt when it shows the notification and an `opened` receipt when you tap
it, and the app host relays the latest receipts to the daemon. `canary status`
shows the last push sent, the last receipt with its device, and how long alert
pushes have been silent; `canary app push-test` proves the channel with a
diagnostic push that never counts as an alert
([the paired app](app.md#prove-the-phone-receives-pushes)). Do not treat the
window as a guaranteed opportunity to receive a push, and do not list a bucket
until `canary status` shows a push witnessed on the phone.

Build capability, account/mode pins, freeze, fresh evidence, preview, journal,
and broker eligibility gates still apply to every submission.

### Queued authorisations

A queued authorisation carries an order you signed to the next regular
session, so it can go out at the open without you at the screen.
`canary proposals queue prepare KEY REVISION --json` fixes the terms from a
proposal row: one exact contract and side, a maximum quantity, a limit halfway
from the mid toward the bid with a worst price 25% from the mark, and a one-hour send window that opens 15 minutes after the
options open, or 5 minutes after the stock open. It returns the terms with
their digest and a private reference, and sends nothing. The record must be
armed within ten minutes with your signature over that digest from a paired
device (`queue arm --stdin`); Canary Desk supplies that signature. A queue can also
carry an exact stock or single-leg option order you wrote, including a stock
trailing stop that follows the whole position: once the broker accepts it, it
grows and shrinks with the position and ends when the position is closed.

Inside the window the daemon sends the order itself, under the origin
`daemon-owner-queued`, after every gate a manual submit passes; otherwise it
holds, cancels or expires the record. If the daemon or the broker link is
down for the window, nothing trades. `queue list` and `queue status QUEUE_ID`
read the records, and `queue cancel QUEUE_ID` (or `--all`) cancels them. A
proposal row a queue covers carries `queued`; it is neither a candidate nor
ready to act.

**After the open.** An order that prices off the regular session (a patient
limit, or a trail without an initial stop) does not go out in the session's
first minutes: the pre-authorisation scheduler and a queued authorisation send
no earlier than the open plus 5 minutes for stocks and 15 for options. At a
stress open, while the latched regime stage reads confirmed stress (a stale
confirmed stage still counts; a stage never observed reads calm), options rows
that scale the book at your discretion (budget, theta and issuer-trim
reductions) wait 30 minutes; loss exits, expiry closes and trailing stops keep
15, because a stop is a stop. The cash sweep's bills are not options: they
keep the 5-minute offset after their own session opens (see
[Cash sweep](cash.md#cash-sweep)). A row's `readiness` dates
that time in `default_send_at`; at a stress open it adds `stress_open: true` to
a row that waits 30 minutes and `stress_open_exempt: true` to one that keeps
15, and its message says which applies, with the time.

A latched drawdown brake lets eligible non-budget protection records bypass
both the notice prerequisite and the waiting window. Budget reductions, the
forced reduction and cash sweep rows always retain the notice prerequisite and
full veto window;
shadow rows never schedule. A sweep row also waits for its bill's session: a
record created outside it is due at the session's open plus five minutes.
`canary proposals status` reports authorised buckets and pending counts. A human
can veto from `canary proposals veto KEY` or the app before submission. Agent
origins cannot veto. A veto applies to that proposal revision; changed evidence
can create a new revision and window.

A submission that `trading.freeze` refuses is deferred, not failed. The record
reads `deferred` with a resubmit time and is not tried again while the freeze
stays set. Once you lift the freeze, the daemon resubmits it once, provided the
proposal revision is still current; a changed revision supersedes it like a
pending record, and a veto still applies while it waits. If standing authority
changes before transmission, a refusal proven to have sent nothing ends the
old record. The current proposal can receive a fresh notice and veto window
under the new authority, with the drawdown-brake exception above unchanged.
An unknown or possibly transmitted outcome is never retried.

A due submission also does not fire while a hand order that is new to its
proposal revision is still working at the broker. A hand order is one entered
by hand in TWS or by another API client, or placed from a terminal or the
paired app; orders the daemon placed itself and agent-origin gated orders never
count. It is new when it was placed, or last modified, after the automatic
record for the revision was created. When the record is created, the daemon
notes the hand orders already working, so a standing order that stays
unmodified, such as a stop you placed last week, never holds it, even while its
trailing trigger moves. A new order holds it, and so does a standing order you
modify, in TWS or through Canary. Any instrument counts, not only the
proposal's. The record stays pending, says why it is held, and fires as soon as
that order is filled or cancelled, on its original window: waiting never
restarts the veto window. The hold applies under a latched brake as well. If
the broker's open-order list cannot be read when the record is created, every
working hand order counts as new for that record; if it cannot be read when the
submission is due, the submission waits. With no bucket pre-authorised, or
nothing armed, nothing is ever held.

While a record waits, deferred or held, its Protection notice stays open and
says what it waits for: deferred because trading is frozen, held behind a hand
order, or held because the broker's open orders cannot be read. It never reads
as recovered while it waits; it recovers only when the record is submitted,
vetoed, or superseded.

The daemon persists submission intent before the broker call and reconciles
it against its journal after restart. Installing or updating the binary
enables no bucket and clears no freeze; the daemon's next start migrates the
policy file after a backup.

## Cash management

The cash sweep and currency leveling buy bills and convert currencies. They
are not protection, so they live in their own `[cash]` section of the
protection policy file with their own authority; see
[Cash management](cash.md).
