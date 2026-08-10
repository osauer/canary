# Protection and risk reduction

Updated: 2026-08-10 08:25 CEST

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

Three buckets generate rows, each enabled separately in the protection policy:

- **Trailing stop** places a broker-side trail against a stock, ETF, or option
  premium. Its time-in-force is a policy decision, DAY by default, and a DAY
  stop expires at the session close without covering the overnight gap. Set
  `tif = "GTC"` under `[buckets.trailing_stop]` to persist it. The proposal
  spells out which lifetime you are getting.
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

## A blocked row is the system working

Every blocker carries a code, a message, and an action line. Under stress the
action is the part to read, because it names the next command.

An active regulatory or news halt blocks the row. So does an active LULD pause.
That is deliberate: a protective order priced against a symbol that is not
trading is a guess about the reopening print. Recent flags that are no longer
active stay visible as context and do not block.

Other rows block because the evidence is not good enough. A stale option quote
raises `stale_quote`. A revision older than the current snapshot raises
`stale_revision`. A time-in-force that drifted between the proposal and its
preview raises `tif_drift`, and a quantity beyond the position raises
`quantity_outside_position`. The row stays visible with its reason attached
rather than quietly disappearing.

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
