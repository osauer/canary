# Protection and risk reduction

Updated: 2026-09-25 21:20 CEST

Proposals are advisory by default. The standard binary cannot place an order.
In a trading build, manual submission requires the exact proposal and its
fresh preview; optional pre-authorised buckets let the daemon schedule
close/reduce actions under an owner-edited policy. Installing Canary does not
enable that automation.

A blocked proposal row is normally the system refusing to act on evidence it
cannot trust, not a fault to work around. Canary exposes only constrained
close/reduce actions here; use TWS for an unmodeled emergency exit.

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
reduces. The submit path checks that twice, once against the proposal's own
position effect and once against the preview's, and blocks on either. A
protection proposal cannot open, increase, or flip exposure.
`authority.auto_submit` must be false; the policy file fails validation
otherwise.

Six buckets generate rows, enabled through the protection policy:

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
  Possible hedges, multi-leg strategies, stale/delayed quotes, wide spreads and
  contracts under 14 DTE remain blocked. A hedge-listed index put needs both
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
- **Risk reduction** proposes trimming a single-name group that exceeds
  `single_name_target_pct_nlv`. It becomes a full close only when the computed
  quantity equals the whole position.
- **Budget reduction** brings long option premium back inside a declared share
  of the risk constitution's declared risk capital while the constitution's
  drawdown brake is engaged. It is absent from the embedded default and
  disabled until you write it; the section below says how.

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
value you did not write is a value the bucket does not have, and the file
fails validation if `enabled = true` without both. The numbers above are the
2026-09-21 mandate the product manager set for the desk owner to confirm by
writing them; they are not a recommendation from the software.

It generates rows only when all of the following hold, and the snapshot's
`budget_reduction` status names the first one that does not:

| State | Meaning |
|---|---|
| `constitution_unapproved` | no risk constitution is active, or a material key is unapproved |
| `enforcement_shadow` | `drawdown.block_enforcement` is `shadow`; the bucket acts only under `advisory` or stronger |
| `not_latched` | the drawdown block tier is neither latched nor breached |
| `currency_mismatch` | the constitution's base currency is not the account's |
| `unmeasurable` | no long option leg has a base market value |
| `within_budget` / `over_budget` | measured; the status carries the total, the share of risk capital, and the leg counts |

Selection runs per line first: a line above the per-line cap is cut to the
cap in whole contracts (`keep = floor(cap ÷ value per contract)`), and a cap
that leaves no whole contract is a full close. Then the total: while the
projected sum still exceeds the total cap, lines are cut in order of largest
unrealised loss first, then largest market value, whole contracts, until the
sum is within the cap. Every row names the cap that selected it, the measured
line and total, the excess, and its place in the order, under `budget` in JSON
and on a `Budget:` line in the text. `max_order_notional` bounds one order
exactly as `risk_reduction.max_order_notional` does; the next cycle measures
what is left. A stale mark blocks the row with `fresh_option_quote_required`;
a leg of a multi-leg unit is measured but routes to the strategy workflow.
Rows are close or reduce only, like every proposal.

**Measured against the Rulebook instead.** `basis = "rulebook"` replaces the
two caps with limits you already keep in the Rulebook policy, as shares of NLV:
a line is cut to `option_line_act_pct` (its premium at risk being the higher of
price paid and value), and when broker-reported available funds sit below
`cash_reserve_min_pct`, lines are sold in the same loss-first order until
their value covers the shortfall. This basis needs the account's NLV and
available funds, not a risk constitution, and it waits for no drawdown brake:
the rows appear whenever those limits are breached. Leave out the two
percentages; the file fails validation with both a basis of `rulebook` and
declared-capital caps.

```toml
[buckets.budget_reduction]
enabled = true
mode = "shadow"
basis = "rulebook"
max_order_notional = 10000
```

The state is `account_unavailable` when NLV or available funds are missing;
the status then carries `per_line_pct_of_nlv`, `cash_reserve_min_pct`, the
account values and `cash_shortfall_base`, and each row names the limit that
selected it.

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

## Pre-authorised buckets

The protection policy's `[authority].pre_authorised` list is empty by default.
Only an explicit owner policy edit and version bump enable the named buckets:
`trailing_stop`, `option_loss_exit`, `option_profit_trail`, or `budget_reduction`.
`auto_submit` remains false; it is not the switch for this scoped scheduler.
Build capability, account/mode pins, freeze, fresh evidence, preview, journal,
and broker eligibility gates still apply to every submission.

`veto_window` defaults to `"30m"` and cannot be less than five minutes. The
window starts after the daemon records the Protection alert in its registry.
The paired app or another consuming application owns notification delivery;
registry acceptance does not prove that a phone received or displayed it.
Do not treat the window as a guaranteed opportunity to receive a push.

A latched drawdown brake lets eligible non-budget protection records bypass
both the notice prerequisite and the waiting window. Budget reductions always
retain the notice prerequisite and full veto window; shadow rows never schedule.
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

The daemon persists submission intent before the broker call and reconciles
it against its journal after restart. Installing or updating the binary does
not edit policy, enable buckets, or clear freeze.
