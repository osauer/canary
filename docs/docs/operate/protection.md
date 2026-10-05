# Protection and risk reduction

Updated: 2026-09-30 21:23 CEST

Proposals are advisory by default. The standard binary cannot place an order.
In a trading build, manual submission requires the exact proposal and its
fresh preview; optional pre-authorised buckets let the daemon schedule
close/reduce actions, and the cash sweep's bill orders, under an owner-edited
policy. Installing Canary does not
enable that automation.

A blocked proposal row is normally the system refusing to act on evidence it
cannot trust, not a fault to work around. Canary exposes only constrained
close/reduce actions here, plus the cash sweep's bill buys; use TWS for an
unmodeled emergency exit.

## What each command does

| Command | Reaches the broker | Use it for |
|---|---|---|
| `canary proposals list` | no | the current proposal set, blockers included |
| `canary proposals refresh` | no | rebuild the set against fresh positions and quotes |
| `canary proposals preview KEY REVISION` | no | mint a preview token and read the broker WhatIf verdict |
| `canary proposals submit KEY REVISION` | yes | place that one protective order |
| `canary proposals reduce SYMBOL --percent N` | only with `--submit` | a discretionary partial close |
| `canary proposals veto KEY` | no | hold the current automatic proposal revision before submission |
| `canary proposals request-stop SYMBOL` | no | stage a trailing-stop proposal for one uncovered stock/ETF holding now |

`canary proposals` with no subcommand runs `list`. In a standard build the submit
path fails closed anyway: the daemon's write handler is compiled out behind the
`trading` build tag and returns `ErrTradingDisabled`. See
[Constrained orders and the trading build](orders.md).

## Proposals close or reduce only

The daemon owns generation. It rebuilds the set from current positions, the
protection policy, and market-event context, and every row it emits closes or
reduces, except a [cash sweep](#cash-sweep) buy of a same-currency bill, a
typed exception described there. The submit path checks that twice, once against the proposal's own
position effect and once against the preview's, and blocks on either. A
protection proposal cannot open, increase, or flip exposure.
`authority.auto_submit` must be false; the policy file fails validation
otherwise.

Six buckets generate protection rows, enabled through the protection policy
(a seventh, the [cash sweep](#cash-sweep), puts idle cash into bills):

- **Trailing stop** places a broker-side trail against a stock or ETF. Its
  time-in-force is a policy decision, DAY by default, and a DAY
  stop expires at the session close without covering the overnight gap. Set
  `tif = "GTC"` under `[buckets.trailing_stop]` to persist it. The proposal
  spells out which lifetime you are getting.
- **Option loss exit** appears only for an exact standalone long contract that
  has a current exact `directional_intents` declaration or qualifies under the
  approved `default_long_calls_directional` setting. At the
  Rulebook-owned 60% loss of premium paid,
  measured on a fresh live bid against multiplier-adjusted cost, it proposes a
  full DAY patient-midpoint-limit close. It may remain unfilled while the loss
  worsens. It is event-driven: Canary does not install a resting loss stop or
  claim the loss is capped.
- **Option profit trail** shares the trailing-stop bucket but is a separate
  decision contract. It arms after a 50% premium gain and proposes a
  full-quantity DAY `TRAIL LIMIT` with a native percentage premium trail,
  normally 30%. Spread, minimum-premium-distance and tick floors can widen the
  effective percentage within the approved 20-50% range. The initial
  rounded stop must retain at least 5% over cost after spread and tick floors.
  Possible hedges, multi-leg strategies, stale/delayed quotes and wide spreads
  remain blocked. The profit trail needs at least 14 DTE; the loss exit keeps
  working to expiry, and an in-the-money long option is proposed for a DAY
  limit close from the Rulebook's expiry act level (7 DTE) as
  `option_expiry_close`, so nothing is exercised by accident (owner decision,
  2026-09-28). A hedge-listed index put needs both
  exact operator intent, a current directional Rulebook role, and exact-ConID
  Greeks evidence; symbol, option shape, and shared-cache Greeks never prove
  intent. Missing or invalid exact-ConID evidence keeps the role unclassified.

The option-exit policy uses the explicitly approved absolute `0.05`
quote-currency `TRAIL LIMIT` offset. An inherited or omitted offset still fails
activation. Standing defaults apply only to standard ungrouped long contracts.
`default_long_calls_directional` classifies long calls and non-hedge-listed
long puts as directional unless they cover a holding: a call over a short stock
of its own underlying, a put over a long stock of its own underlying, or an
index option over such a stock anywhere in the book, which makes the option
protection. `default_index_puts_protection` excludes hedge-listed puts from
directional exits. Exact declarations, including expired declarations, take
precedence. A contract no standing default covers, such as a short option, a
non-standard multiplier or a strategy leg, stays an owner review; Canary
requests no quote for it and reports no quote blocker. Without a qualifying
standing default or exact intent, an exit stays blocked.

- **Theta hygiene** proposes closing an option whose remaining value is mostly
  time value bleeding toward expiry. When the underlying spot or the option mark
  is missing or stale, the row still appears, blocked with
  `extrinsic_uncomputable`, because the daemon then cannot separate intrinsic
  from time value and cannot assert the close is non-destructive.
- **Risk reduction** trims an issuer whose worst-case loss reaches the
  Rulebook's act level (rule 1, `single_name_act_pct`, 40% by default) back to
  its watch level (`single_name_watch_pct`, 30%; 20/30 for an illiquid
  issuer), on the same measure: the most the issuer can lose at any price,
  every leg netted. It reduces the leg that loses most at that price, capped
  by `max_order_notional`, and becomes a full close only when the computed
  quantity equals the whole position. When that leg alone cannot reach the
  watch level the proposal says so; ranking rolls, collars and trims across
  legs by cost is a later step. The levels live in `rulebook-policy.toml`: the
  retired `single_name_target_pct_nlv` key is ignored with a note if a file
  still carries it.
- **Budget reduction** brings long option premium back inside a declared share
  of the risk constitution's declared risk capital while the constitution's
  drawdown brake is engaged. Its caps are your numbers: the file Canary
  writes shows the table only as a commented placeholder, and the governor
  stays off until you write it; the section below says how.

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
every other bucket keeps working. The numbers above are the 2026-09-21 mandate
the product manager set for the desk owner to confirm by writing them; they
are not a recommendation from the software.

It generates rows only when all of the following hold, and the snapshot's
`budget_reduction` status names the first one that does not:

| State | Meaning |
|---|---|
| `needs_your_number` | the table is enabled but a cap or `max_order_notional` is not written; the reason names the keys |
| `constitution_unapproved` | no risk constitution is active, or a material key is unapproved |
| `enforcement_shadow` | `drawdown.block_enforcement` is `shadow`; the bucket acts only under `advisory` or stronger |
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
`Budget:` line in the text. `max_order_notional` bounds one order exactly as
`risk_reduction.max_order_notional` does; the next cycle measures what is
left. A stale mark blocks the row with `fresh_option_quote_required`; a leg of
a multi-leg unit is measured but routes to the strategy workflow. Rows are
close or reduce only, like every proposal.

**The whole fix in one place.** The `budget_reduction` status lists up to
three `candidates`, the ranked lines with their contract, contracts held,
unit value, the rules they relieve (`relief`), `time_value_pct`, unrealised
P&L and a one-line `why` ("offends 2 open rules; 50% time value; unrealised
−500"), and the `plan`: every order the measurement needs, each with its
`rank`, `contract`, `contracts`, `raises_base` and `cycle` (1 for this
refresh, 2 and later for what `max_order_notional` holds back). `canary
proposals list` prints both under the Budget header. Every governor row adds
three detail lines: its place in the plan, the other open rules the sale
relieves, and the next two candidates with their `why`. Ignoring a row takes
its line out of the plan: the next refresh moves to the next candidate.

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
the snapshot with `shadow: true`, and listed by `canary proposals list` under
a *Shadow (budget reduction)* heading of their own, so you can read what the
rule would have done beside what you did. `preview` and `submit` refuse them
with `shadow_mode`, and the pre-authorisation scheduler skips them: a row is
eligible for automatic placement only when `AutomaticEligible()` holds, which
a shadow row never does. `counts.budget_reduction` and
`counts.budget_reduction_shadow` report the rows; shadow rows are never
`actionable`.

**Activating.** Set `mode = "active"` and bump `policy_version`. The rows
become ordinary proposals under every existing gate (preview, WhatIf, the
double check of position effect, duplicate orders, account, mode, freeze,
origin and the explicit submit). One thing does not change with the mode:
every row carries `never_skip_veto`, so a scheduler that shortens or skips the
veto window under a latched brake must still wait the full window for these.
A reduction to budget is a discretionary-scale action, not a stop.

`canary proposals reduce` is separate: a discretionary partial close you size
yourself. It previews unless you pass `--submit`. Under `--portfolio` the
percentage is the share of net delta-adjusted portfolio risk to remove rather
than a flat per-position cut, and hedges are never selected, so
`--include-hedges` is a hard error there instead of a silent no-op.

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
Canary's own work and asks nothing of the owner; the standing rule applies
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

Two rules settle the cases where the largest would be wrong (owner decisions,
2026-09-28):

- An immediate sale goes before a trailing stop, whatever the sizes, unless
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
quote raises `fresh_option_quote_required`. A revision older than the current
snapshot raises `stale_revision`. A time-in-force that drifted between the
proposal and its preview raises `tif_drift`, and a quantity beyond the position
raises `quantity_outside_position`. The row stays visible with its reason
attached rather than quietly disappearing.

A directional option whose exact quote, cost, role, or session evidence is
unavailable appears as a blocked **Option exit review** row. It is not
silently dropped and it cannot be previewed as an order.

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

For a sell stop Canary placed on a stock or ETF, the protective stop guard
makes that fix itself (next section). An order placed by hand in TWS is yours:
Canary reports it and leaves it alone.

## Protective stop exemption and guard

A broker-side stop that protects a whole long stock position is usually larger
than `[trading].max_notional`, and every apparent stock sell exit is otherwise
read as opening a short, which needs `allow_stock_short`. A protective stock
exit is exempt from both:

- a stock or ETF `SELL` stop (`TRAIL`, `TRAIL LIMIT`, `STP`, `STP LMT`);
- the position is long and the stop sells no more than it holds, so it closes
  or reduces and never opens or flips;
- the broker's complete, current open-order list, hand orders in TWS included,
  shows no other working sell for the same contract that together with this
  stop would sell more than is held.

If the open-order list cannot be read, the exemption does not apply and
today's refusal stands. A modify that only lowers the quantity of a working
stop is exempt on the same terms without the third condition, because it can
only shrink what is already working. Option orders and every other stock order
keep both gates and `max_option_contracts` unchanged.

A trailing-stop row that the gates would refuse no longer reads ready. It
carries `protective_exit_competing_sell` when another working sell already
covers the shares, `protective_exit_inventory_unavailable` (waiting for the
broker) when the open orders cannot be read, or
`protective_exit_exceeds_position` when the stop is larger than the position.

A resting GTC stop outlives a later hand sale, so the exemption comes with a
guard. On the proposal cadence (30 seconds by default) the daemon compares each
working sell stop it placed on a stock with the current position: at zero or
short it cancels the stop; below the stop's remaining quantity it shrinks the
stop to the whole-share position, keeping the trail amount or percent, the
current stop price, the limit offset, trigger method, time in force and
outside-hours flag. It acts only on a current, complete portfolio download for
the connected account and a complete open-order list, and only when the same
correction is planned on two passes at least 20 seconds apart, so a partial
download or an out-of-order fill never moves an order. Each change goes through
the ordinary modify or cancel path with the origin `daemon-protective-guard`,
is journaled on the order, and is written to the decision log
(`protective_stop_guard.shrink` or `.cancel`). The order's
`position_mismatch` notice stays open until the change lands.

Every write gate still decides. With trading disabled, the broker link down or
daemon storage unhealthy the guard does nothing and logs the blocker once.
`trading.freeze` blocks every modify, so a shrink waits while frozen; a cancel
still goes through, because a freeze never strands an order that needs
cancelling.

The residual gap: while the daemon is down, a hand sale is not followed, so a
stop can stay larger than the position until Canary is back and the guard has
run.

## Pre-authorised buckets

The protection policy's `[authority].pre_authorised` list is empty by default.
Only an explicit owner policy edit and version bump enable the named buckets:
`trailing_stop`, `option_loss_exit`, `option_profit_trail`, `budget_reduction`,
or `cash_sweep` (the sweep's bill buys and redemptions).
`auto_submit` remains false; it is not the switch for this scoped scheduler.
Build capability, account/mode pins, freeze, fresh evidence, preview, journal,
and broker eligibility gates still apply to every submission.

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

**After the open.** An order that prices off the regular session (a patient
limit, or a trail without an initial stop) does not go out in the session's
first minutes: the pre-authorisation scheduler and a queued authorisation send
no earlier than the open plus 5 minutes for stocks and 15 for options. At a
stress open, while the latched regime stage reads confirmed stress (a stale
confirmed stage still counts; a stage never observed reads calm), options rows
that scale the book at your discretion (budget, theta and issuer-trim
reductions) wait 30 minutes; loss exits, expiry closes and trailing stops keep
15, because a stop is a stop. The cash sweep's bills are not options: they
keep the 5-minute offset after their own session opens (see *Cash sweep*). A row's `readiness` dates
that time in `default_send_at`; at a stress open it adds `stress_open: true` to
a row that waits 30 minutes and `stress_open_exempt: true` to one that keeps
15, and its message says which applies, with the time.

A latched drawdown brake lets eligible non-budget protection records bypass
both the notice prerequisite and the waiting window. Budget reductions and
cash sweep rows always retain the notice prerequisite and full veto window;
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
pending record, and a veto still applies while it waits. Any other refusal
fails the record for that revision, as before.

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
submission is due, the submission waits. With no bucket pre-authorised, nothing
is ever held.

While a record waits, deferred or held, its Protection notice stays open and
says what it waits for: deferred because trading is frozen, held behind a hand
order, or held because the broker's open orders cannot be read. It never reads
as recovered while it waits; it recovers only when the record is submitted,
vetoed, or superseded.

The daemon persists submission intent before the broker call and reconciles
it against its journal after restart. Installing or updating the binary does
not edit policy, enable buckets, or clear freeze.

## Cash sweep

The cash sweep puts idle cash to work in bills of the same currency and never
converts one currency into another. It is the only bucket whose rows buy;
`authority.close_reduce_only` stays `true`, and the sweep passes the
close-or-reduce check only as a typed exception: a `cash_sweep` buy of the
row's own resolved bill, a vocabulary instrument in the row's own currency,
for no more face value than the free cash it was planned against, costing no
more than that free cash at the preview's limit, and within the sweep's order
cap in force. Apart from the trading-cap exemption described under
[Reserve and order sizing](#reserve-and-order-sizing), nothing else is relaxed.

It is off until you write the table. In `active` mode a row is an ordinary
proposal: `canary proposals preview` previews its bill as a limit order of
the bill's own security type (BILL for a US Treasury bill, BILL or BOND for
the others; DAY, regular hours) through every gate any proposal meets (trading freeze,
authority, the bill's session, a live two-sided quote read during the preview,
`[trading].max_notional` unless the bill exemption applies, broker WhatIf), and it is sent only when you approve
it, or by the daemon after the full veto window when you list `cash_sweep`
under `[authority].pre_authorised`. Your approval of each order is the last
step.

```toml
[buckets.cash_sweep]
enabled = true
mode = "active"              # shadow (default) or active
reserve_floor_base = 10000.0 # reserve kept as cash: the largest of this,
reserve_pct_nlv = 10.0       #   this percent of NLV, and planned needs
min_order_notional = 20000.0 # smallest buy, in base currency
max_order_notional = 50000.0 # largest order: the larger of this
max_order_pct_nlv = 10.0     #   and this percent of NLV
keep_cash = 5000.0           # settlement float in each currency's own unit
bills_exempt_from_trading_max_notional = true
# tax_reviewed_at = 2026-01-01   # advisory: until written, rows say the tax treatment is not yet confirmed
```

Every sizing number is read from the file only. Canary's own values exist
solely for `canary policy ensure` to write them; a missing number holds the
sweep at `needs_your_number`, naming the key, and never falls back to a
compiled value.

Per currency, in that currency: **cash** is the lower of trade-date cash and
the broker's observed per-currency settled cash
(`settled_cash_source: broker`). The account-wide `SettledCash` figure does
not establish a currency's settled cash. Without a per-currency observation,
the sweep holds at `settlement_unknown`; waiting after a daemon restart does
not clear it. The order journal lacks verified settlement dates, holiday
calendars and complete account-wide fill coverage, so its estimate cannot
authorise a sweep. An optional authenticated read-only Web API connection can
provide the missing observation; see [Settlement evidence](cash-ledger.md).
**Committed** is working buy orders with a fixed finite limit plus armed, held
or sending queued buys at their finite worst price. An unknown principal or
commission bound or a nonfinite total holds the sweep. Outstanding buy records
currently lack fee envelopes, so new sweeps wait while such buys remain.
A prepared, unarmed queue entry or an
unapproved proposal never counts. **Kept** is the currency's `keep_cash` and,
in the base currency, the reserve (below): the larger of the two. **Free** is
cash − committed − kept − any reserve shortfall this currency keeps for the base
currency. When free exceeds the smallest order (`min_order_notional` at the
ledger rate) the sweep buys one order, held to the order cap in force at the
ledger rate; a cap that holds the order below the smallest order holds the
currency. Exact buy previews also require principal plus the broker's maximum
same-currency commission to fit free cash. The cap is per order; no aggregate
percentage or daily budget exists. When cash less commitments falls below
kept it sells the nearest maturity (or the declared ETF) to cover the gap,
held to the cap like a buy (the next cycle sells the rest), unless a held bill
pays out before a sale today would settle; unsettled proceeds of such a sale
count toward kept so it is not sold twice. A sale restores cash, so it is
never held to, or raised to, the smallest order: it sells what the gap needs on
the bill's size grid. Otherwise nothing happens.

A currency without its own table follows Canary's default declaration: USD
`us_tbill`; EUR `de_bubill` and `fr_btf` with an `etf` fallback; GBP
`uk_tbill`; CAD `ca_tbill`; every other currency `none`, which keeps its cash
as cash. The ladder defaults to four rungs from `min_maturity_days = 28` to
`max_maturity_days = 91` (EUR 182, at most 397). `keep_cash` has no compiled
default: a currency table's own value wins, else the bucket's. `min_tranche`
is retired in favour of `min_order_notional`; an old file that still writes it
keeps reading, and the value then raises that currency's smallest buy. The
fallback ETF acts only after a completed contract search finds no bill line;
until you write its `etf_symbol` and `etf_exchange` the status names them under
`needs_your_number`, and the bills still plan.

### Reserve and order sizing

Owner decisions of 2026-10-05 18:35 CEST. All figures are in the account's
base currency at the ledger rate.

- **Reserve kept as cash** is the largest of `reserve_floor_base`,
  `reserve_pct_nlv` percent of net liquidation value, and the cash planned
  exercises or withdrawals need. Canary records no planned draw today: an
  approved exercise goes to the broker at once, and a withdrawal is recorded
  only after it happens. That term is therefore 0, and the status says so in
  `planned_needs_reason`. Working and armed buys are not added, because each
  currency's cash already has them deducted as committed.
- **Where it is held.** The reserve is held in the base currency first: the
  base currency keeps the larger of its `keep_cash` and the reserve. The part
  base cash cannot hold (`reserve_shortfall_base`) is kept in the other
  currencies before they invest, the largest free cash first. If base cash is
  unknown, the other currencies do not invest. A shortfall never makes another
  currency sell; the base currency redeems its own bills to restore the
  reserve. Every other currency keeps its own `keep_cash`.
- **Order bounds.** A buy is at least `min_order_notional` and at most the
  larger of `max_order_notional` and `max_order_pct_nlv` percent of NLV. A
  sale is never held to the minimum.
- **Fail closed.** When a percentage is above 0 and net liquidation value
  cannot be read, every currency holds with nothing bought or sold, and no row
  carries a trading-cap exemption.
- **Trading-cap exemption.** With `bills_exempt_from_trading_max_notional =
  true`, a sweep bill order may pass `[trading].max_notional` up to the
  sweep's order cap in force, never beyond it. The order must be a BILL or BOND
  buy that opens or increases, or a sale that reduces or closes, of a vocabulary
  bill in the order's own currency, with no conversion. Stocks, ETFs, the
  sweep's fallback ETF, conversions and anything over the sweep's cap keep the
  trading cap. The row's order terms carry the limit as
  `trading_cap_exempt_up_to_base`, and both preview and submit check it.
  Without the key, the exemption is off.

Worked check. NLV 233,000 EUR, EUR cash 70,500, USD cash 6,800, `keep_cash`
5,000: the reserve is 23,300 EUR (10% of NLV), held in EUR, and EUR invests
one order of about 47,000 (70,500 − 23,300, under the 50,000 cap). USD free
cash is 1,800 USD, below 20,000 EUR, so USD stays cash. At NLV 1,200,000 with
1,000,000 cash: reserve 120,000, and orders up to 120,000 each.

The status's `sizing` block and every row's `cash_sweep.sizing` carry the
figures, with stable field names: `base_currency`, `net_liquidation_base`,
`reserve_base`, `reserve_bound` (`reserve_floor_base`, `reserve_pct_nlv` or
`planned_needs`), `reserve_floor_base`, `reserve_pct_nlv`,
`reserve_pct_nlv_base`, `planned_needs_base`, `planned_needs_known`,
`planned_needs_reason`, `reserve_shortfall_base`, `min_order_base`,
`max_order_base`, `max_order_bound` (`max_order_notional` or
`max_order_pct_nlv`), `max_order_notional_base`, `max_order_pct_nlv` and
`trading_max_notional_exempt`. Each currency's status and row carry
`reserve_held`, the part of the reserve kept in that currency in its own unit.
`max_order_notional_base` on the status and the row is the cap in force. A row
detail says it in words, for example "kept as cash: 23300 EUR (10% of NLV
233000 EUR), held in EUR; orders from 20000 EUR to 50000 EUR
(max_order_notional)".

`canary policy ensure --dry-run` lists each missing sizing key it would add to
an existing `[buckets.cash_sweep]` section. Applying the reviewed plan backs
the file up, writes only the missing keys at the values above, keeps every
value you wrote, and raises `policy_version` by one. A file without the table
is left alone, and the sweep stays off.

```toml
[buckets.cash_sweep.currency.EUR]
instruments = ["de_bubill", "fr_btf"]
fallback = "etf"
etf_symbol = "AAA"       # your fallback ETF
etf_exchange = "IBIS"
keep_cash = 8000
```

A key you leave out of a currency table takes that currency's default. An
instrument outside the vocabulary, a bill of another currency, or any
conversion fails validation.

Which bill a buy names: for USD, the outstanding bill from TreasuryDirect's
public list (read once a day and kept in daemon state) that matures nearest
the rung's target inside `min_maturity_days`–`max_maturity_days`; for EUR,
GBP and CAD, the nearest of the bills you list by ISIN:

```toml
[buckets.cash_sweep.currency.EUR]
isins = ["DE000BU0ZZ19", "FR0128ZZZZ13"]   # your bills; each of a declared instrument
```

Canary names a bill only after the broker resolves it to one line that
carries its size rules and minimum tick, and quotes it. IBKR lists US
Treasury bills as security type BILL, so Canary asks for a US bill as BILL
and for a German, French, UK or Canadian bill as BILL first and BOND second
(an assumption checked per instrument after install); the row records which
type resolved. A currency with no
list to choose from reads `universe_unavailable` (TreasuryDirect unreachable
for two days, or no `isins` written); one whose candidates do not resolve,
carry no size rules or no price reads `instrument_unresolved` with the
evidence per candidate. `canary market --symbol <ISIN|CUSIP> --type BILL`
(or `--type BOND`, which for a bill's identifier asks BILL as well and says
so) runs the same resolution and quote as a read-only check, naming every
attempt with IBKR's code and text when none finds a line, and `canary positions`
lists held bills and bonds in their own section with class, maturity and
currency.

An order counts the bill's own unit: Canary assumes a US Treasury bill is
bought in bonds of 1,000 USD face (`face_1000`) and a German, French, UK or
Canadian bill in single units of face (`face_1`), priced per 100 of face.
The buy is the planned cash at the higher of par and the quoted price, in
whole units rounded down to the bill's minimum size and size step, so neither
its face value nor its cost passes the free cash; a tranche too small for the
bill's minimum holds the currency and says so. A redemption rounds its sale up
to the held bill's size step, or down when up would pass the position or
the order cap. The row's `cash_sweep`
block carries `quantity_unit`, `face_value`, `estimated_cost` and the
`session` its order fills in: the bill's liquid hours from its contract
details, else assumed weekday hours (US bills 08:00–17:00 New York, Bubills
and BTFs 09:00–17:30 Frankfurt and Paris, UK bills 08:00–16:30 London,
Canadian bills 08:00–17:00 Toronto; holidays are then not modelled, and the
live-quote requirement refuses instead). The units, the price convention and
the hours are assumptions the post-install proof checks. The preview prices a
patient limit on the bill's minimum tick (a buy at the mid rounded down, never
below the bid), and Canary refuses to build any bond order off the bill's size
or price grid.

A row carries a blocker only when its order cannot be priced or sized:
`fresh_bill_quote_required` (the bill's quote, or a held bill's mark, is not
live), `bill_contract_rules_unavailable` or `below_minimum_increment` (a
redemption the held bill's size grid cannot fit), and `bill_unit_mismatch`:
a buy's preview divides the broker's WhatIf initial-margin change (the figure
IBKR returns for a bond) by the order's value at the assumed unit, and when
the ratio falls outside 0.005 to 1.2 (a bill margined at one percent reads
about 0.01, a cash account about 1.0, a unit 1,000 times off near 10 or near
0.00001), the preview is refused and that bill instrument's buys stay
blocked, for every submit, until a preview checks clean. A stale quote's
readiness is `quote_unusable`. Outside the bill's session the row's readiness
is `market_closed` with the session's next open, and the preview refuses with
`market_closed` before any quote. An
order already working for the same bill and side, or sent and not yet
acknowledged, holds a new preview until it fills or is cancelled; working
bond buys count as committed cash, and a bill sold inside the settlement
window counts toward `keep_cash` until it settles.

The snapshot's `cash_sweep` status lists every currency the account ledger
reports, with its figures and a state:

| State | Meaning |
|---|---|
| `invest` / `redeem` | the band asks for a buy or a sale; the row follows |
| `hold` | inside the band, or the reason says why no order follows |
| `no_instrument` | the currency declares `none` |
| `cash_unavailable` | no current ledger cash for the currency (never read as zero) |
| `settlement_unknown` | the broker supplied no per-currency settled cash, or working/armed queued buy commitments have no fixed finite bound; a journal estimate does not clear this state |
| `equivalents_unclassified` | a bond or bill holding whose contract details cannot be read, or a declared-ETF holding |
| `needs_your_number` | a sizing number (`max_order_notional`, `max_order_pct_nlv`, `min_order_notional`, `reserve_floor_base`, `reserve_pct_nlv`, or the currency's `keep_cash`), or the symbol of an ETF-only declaration, is not written; the reason names the key |
| `universe_unavailable` | no list of bills to choose from (see above) |
| `instrument_unresolved` | no candidate bill was confirmed by contract details and a quote; `evidence` says why |

`canary proposals list` shows the sweep under its own *Cash sweep* heading
with one band line per currency; JSON carries a `cash_sweep` block on each
row, and `counts.cash_sweep` and `counts.cash_sweep_shadow` count the rows.
An invest row's key names its bill, so a preview or submit buys the bill you
saw, never another one a later cycle names. Every row carries
`never_skip_veto`. `mode = "active"` makes the rows ordinary proposals under
every gate, freeze included; shadow rows carry `shadow_mode` and preview and
submit refuse them. Without `tax_reviewed_at` each
row carries the line "tax treatment not yet confirmed" and the status
`tax_reviewed: false`; it blocks nothing. Each currency's status carries
`cash_like`, cash plus cash equivalents, when both are known. While the sweep
is enabled, `canary brief` adds a `cash` row (cash, cash equivalents and
their sum per currency) and rule 14's
evidence gains the same figures; the rule's own figure is unchanged, because
the sweep never converts. Set `enabled = false`, or remove the table, to stop
it: rows leave on the next refresh and held bills mature to cash.
