# Protection and risk reduction

Updated: 2026-08-13

Nothing here submits an order for you. The daemon can propose a close or a
reduce and can price one against the broker. Placing it stays an explicit
instruction from you, for that exact order, in that moment.

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
| `canary proposals request-stop SYMBOL` | no | stage a trailing-stop proposal for one uncovered stock/ETF holding now |

`canary proposals` with no subcommand runs `list`. In a standard build the submit
path fails closed anyway: the daemon's write handler is compiled out behind the
`trading` build tag and returns `ErrTradingDisabled`. See
[Constrained orders and the trading build](orders.md).

## Proposals are advisory, and close or reduce only

The daemon owns generation. It rebuilds the set from current positions, the
protection policy, and market-event context, and every row it emits closes or
reduces. The submit path checks that twice, once against the proposal's own
position effect and once against the preview's, and blocks on either. A
protection proposal cannot open, increase, or flip exposure.
`authority.auto_submit` must be false; the policy file fails validation
otherwise.

Four buckets generate rows, each enabled separately in the protection policy:

- **Trailing stop** places a broker-side trail against a stock or ETF. Its
  time-in-force is a policy decision, DAY by default, and a DAY
  stop expires at the session close without covering the overnight gap. Set
  `tif = "GTC"` under `[buckets.trailing_stop]` to persist it. The proposal
  spells out which lifetime you are getting.
- **Option loss exit** appears only for an exact standalone long contract that
  has a current time-bounded `directional_intents` declaration. At the
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
  intent. Until exact-ConID Greeks ship, hedge-listed puts remain blocked as
  unclassified rather than risking the sale of a hedge.

The option-exit policy uses the explicitly approved absolute `0.05`
quote-currency `TRAIL LIMIT` offset. An inherited or omitted offset still fails
activation, and an empty exact-contract intent list produces no candidates.
- **Theta hygiene** proposes closing an option whose remaining value is mostly
  time value bleeding toward expiry. When the underlying spot or the option mark
  is missing or stale, the row still appears, blocked with
  `extrinsic_uncomputable`, because the daemon then cannot separate intrinsic
  from time value and cannot assert the close is non-destructive.
- **Risk reduction** proposes trimming a single-name group that exceeds
  `single_name_target_pct_nlv`. It becomes a full close only when the computed
  quantity equals the whole position.

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

An explicitly declared option whose exact quote, cost, role, or session
evidence is unavailable appears as a blocked **Option exit review** row. It is
not silently dropped and it cannot be previewed as an order.

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
