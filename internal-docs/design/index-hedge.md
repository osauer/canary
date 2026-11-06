# Index hedge

Created 2026-10-28 13:55 CET. Owner decisions 2026-10-28 13:17 CET (Desk
session): the hedge instrument is the Micro E-mini S&P 500 future (MES); its
size is "to neutral", meaning hedge until a 10% S&P fall changes the book by
about zero. "Flatten" in Desk means to neutral by cuts (net mode at 100%);
closing everything stays a separate emergency action in Desk.

## Built: the plan (`proposals reduce --portfolio --mode hedge --plan`)

- **Size.** The hedge is sized on the mandate's index stress of the whole
  book (`stresses.index_loss_base`), with its working sales counted as sold
  (see "A cut still working" below). Every line counts, locked hedges too:
  neutral is about the book's own change in the fall, and a hedge the owner
  keeps pays in it. This differs from net mode, which leaves locked hedges out
  of its direction because it chooses what to trade.
  - One short contract offsets `10% × price × 5 × USD rate` in the fall.
  - The count is the floor of the fall's loss over that offset, so the book
    never ends short of the index.
  - Nothing is sold when the fall already leaves the book even or ahead
    (`nothing_to_hedge`), or when the loss is less than one contract's offset
    (`below_one_contract`).
- **Contract.** The front MES quarterly on CME, read from IBKR's contract
  details (`FrontFuture`). A contract within 7 days of its expiry is skipped,
  so the hedge is sold in the next quarterly before the front one's expiry
  week.
- **Price.** The quote's mark, else its selected price, else the mid, else
  the last trade.
- **Scores.** The same fields as a sweep plan: index fall and rise, loss
  measure against the room, and half-spread trading cost. The leg carries
  `contract`, with month and multiplier.
- **Mandate model.** The future is a linear line under the key `MES FUT`:
  - a stock-kind leg at its multiplier, priced at the future's price;
  - moved by SPY's returns, so its beta is 1;
  - stressed by the index gap.

  The key is not the bare symbol: ES is also a listed company, and the
  compiled lists match on symbol and currency.
- **Loss measure.** A held hedge counts its loss in the 10%
  index rise (`index_hedge_base`), so it spends loss room like every other
  line (owner decision 2026-10-28 20:07 CET; built 2026-10-28). A stop on
  it would count its distance at contract value per point instead. Its
  rally loss also shows in the plan's `index_rise_*` figures.

## Gated hedge placement (implemented 2026-10-28)

Each item landed on local main with its own gate; the commits name them.

1. **Held futures in the mandate.** A held MES is the same `MES FUT` leg the
   plan uses, at its multiplier and mark. Net mode reads its dollar delta at
   the multiplier. A stop on it counts at contract value per point.
2. **Policy table.** `[mandate.index_hedge] max_contracts`, read by the
   risk-policy loader. Absent, the hedge still prices, and placing it is
   refused (`index_hedge_unplaceable`); `canary policy check` reports it
   (`index_hedge_unplaceable`, info). No `allow_future_short`.
3. **The order path.** A future reaches the broker in two shapes only, both
   through the one device-confirmed path (preview, WhatIf, signed token,
   admission, wire guard; index_hedge_order.go):
   - the hedge sale, previewed by the daemon from the plan (`--mode hedge`
     without `--plan`): a limit at the bid, good for the day, in US regular
     hours (`outside_regular_hours` otherwise, at preview and at submit),
     with no stop order. The leg carries one signed token, submitted as a
     basket's tokens are. The order gate holds the contract's position
     after the sale within `max_contracts`; the submit includes other
     months, all-client pending sales and durable uncertain commitments; over it is refused, never
     resized (`index_hedge_over_max`). A hedge within the cap is exempt
     from the order cap, whose face-value notional one contract can exceed.
   - its close: a percent reduce of the held line with `--include-hedges`,
     priced as every reduce is. The portfolio sweep never selects a hedge.
4. **Gate classification.** A SELL is a reduction only when the modeled
   index-fall loss decreases without crossing neutral, the loss measure
   does not increase, and it adds or deepens no gap-cap breach. Otherwise
   it remains a risk addition and the normal mandate and drawdown gates
   apply. **Replaced 2026-11-06, see "At the maximum loss" below.** The plan
   offers the largest sale up to neutral that placement admits and names what
   holds it lower (`hedgeSaleRefusal`, 2026-10-29 12:45 CET).
6. **The index gap nets the hedge** (owner decision "1. A", 2026-10-30
   09:41 CET). One index gap scenario moves every broad-index line
   together, the hedge included: SPY, the other funds and indices on
   `mandate.BroadIndexInstruments` and the Micro S&P future, each by the
   full `index_gap_shock_pct`, down and up, with no beta scaling (owner
   decision 2026-10-30 09:58 CET: a fund closing before the US close
   measures a beta under 1 on daily closes without moving less in a real
   gap, and scaling up by beta would tighten lines beyond today's stress). The gap cap
   applies to the group's net loss (`verdict.stresses.index_gap`); each
   broad-index row carries those net figures, and a breach names the
   members that lose in the breaching direction. Single names and any
   other underlying are stressed one by one as before. So a short hedge
   lowers the index gap loss, can cure an index breach, and breaches on
   its own only when it hedges past neutral. Placement
   (`indexHedgeProjectedRiskRefusal`), the gate classification
   (`indexHedgeReduces`), the plan (`hedgeSaleRefusal`) and the additions
   gate (`mandateGapBreachAdded`) refuse a sale only when it adds or
   deepens a breach, judging a broad-index line by its group. Every other
   limit is unchanged: the 10% fall and rise, the per-name gap caps, the
   order caps, the neutral cap and device confirmation. Not modelled: the
   basis between SPY and the future (both move on SPY's returns), and a
   fund's own tracking against the index.
5. **Plan words for Desk.** The hedge plan and preview carry `roll_due`
   from ten days before a held contract's expiry and `hedge_not_needed`
   while the book without its held hedges reads within the mandate, each
   with one plain message (`docs/docs/operate/protection.md`).

## At the maximum loss (owner, 2026-11-06 14:23 CET)

The owner's instruction, "Build the change", answered this finding: at zero loss
room every sale was refused, because the hedge's own rise loss and its index gap
exceed a room and a cap of zero, so the one hedge Canary trades closed when the
book needed it most. A book of single-name calls has nothing in the index group
to net against, so the gap test refused a sale at any room. This replaces the
loss-measure and gap tests of items 4 and 6 for the sale, and leaves the measure
itself unchanged (the owner decision of 2026-10-28 20:07 CET still counts the
rise loss, so a hedged book reads `reduce` and `canary mandate` shows the measure
higher by it, Fully armed staying refused meanwhile).

- **The sale.** One rule at the plan, the classification and placement
  (`indexHedgeSaleRefusal`): the 10% fall's loss is lower and not below zero, and
  the 10% rise does not start to lose, or lose more than it already did. Sizing
  to neutral makes the hedge's 10% rise loss at most the fall's loss it removes,
  itself at most what the bought options are worth.
- **The close.** The reduction buys back what has passed neutral on the book as
  held (`mandateHedgeCloses`): a contract goes while the book without it is still
  even or ahead in the fall, or while the hedge makes the rise lose more than the
  book would with none of it. It is a `mandate_reduction` row of phase `hedge`,
  rank 0, so it keeps its own veto clock, neither waiting behind the cuts nor
  holding them. It stays out of the relief ranking, where a future's small value
  would make it the cheapest relief and unwind the hedge before the first call
  sold. It is priced at the ask, the mirror of the sale at the bid. Within the
  room, with no owner sleeve stop to act on, the batch withdraws and nothing
  buys it back: the measure already counts its rise loss.
- **A hedge never makes the reduction sell.** The cuts are sized on the book
  without its short hedges, and a book inside its room without them sells
  nothing. Counting the rise loss in the sizing made a neutral hedge sell
  extra calls to offset its own figure, which then left it past neutral (found
  by review, 2026-11-06; a 75-contract hedge took a batch from 36 contracts to
  306 at the same room). The rise loss still shows in the measure and the
  batch's projection, and a hedge with nothing to cut reads `nothing_reducible`.
- **A cut still working** (2026-11-06 21:15 CET; found by review of Desk
  70929f97, which sends the sale with one device confirmation). The plan and
  placement sized on the held book and the hedge's own working sales, so a sale
  of calls working at the broker was still counted as held, and the short stood
  past neutral from its fill until the buy-back of a breach. Both now measure the
  book as it reads once its working sales fill (`hedgeBookWithWorkingSales`), on
  the same all-client order and journal evidence as the hedge's own sales and
  bound through the wire the same way.
  - **What counts.** A working sale of a line the book holds long, stocks and
    options, its remaining units and at most what the line holds; the units left
    keep the line's own valuation. A partly filled order counts what remains,
    because its fills are already in the held quantity, and placement refuses
    until they are. A combination's selling leg counts by its own direction and
    ratio.
  - **What does not.** A stop or trail sells only when the market reaches it,
    and every protected line carries one, so counting it would leave such a book
    unhedged. A sale that would raise the fall, such as a bought put's: the fall
    adds up line by line, so leaving those out gives the lowest fall any set of
    the fills could reach, and the size is safe whichever of them fill. A sale
    opening or adding to a short, a buy, and a cut not yet sent.
  - **Unread.** A working sale that may touch a long line and has no direction,
    order type, remaining quantity or contract is left out of the size. The plan
    says so, and the preview and placement refuse it, as for an unread hedge sale.
    An unread sale of a line the book does not hold changes nothing.
  - **What it costs.** While the cut works the hedge covers less of the fall than
    the held book loses, by what the cut will take away. A cut that lapses leaves
    the hedge short of neutral, never past it, and the next plan sells the rest.
  - **Left as it is.** The gate classification (`indexHedgeReduces`) reads the
    held book: it can only admit more than placement, which binds. The neutral
    cap counts the hedge's own contracts alone. The sale's rule and the buy-back
    are unchanged.
  - **Proof pending:** the next live hedge sized beside a working cut.
- **What still bounds it.** `max_contracts`, neutral sizing, the rise rule, a
  limit at the bid, DAY in regular hours, the WhatIf and the device for every
  sale, and the standing authority and veto for every automatic close.
- **Worst case.** The index rises and the names do not follow, which the model's
  betas do not show: the short's face value times the rise. One contract costs
  about 3,300 EUR in a 10% rise at 2026-11-06 prices, and 25 about 83,000 EUR. As
  calls sell after the hedge was sized, the part past neutral stays short until
  the next verdict and one veto window after it, and to the next open plus a
  window if that runs past the close. The sales go in tranches, so that part is
  what the tranches sent since the sizing freed; a sale working at the sizing no
  longer adds to it. Nothing protects it while the machine sleeps.

## Held by the owner

Nothing above places a hedge until the owner does these things:

- **The table.** Daemon start writes `[mandate.index_hedge] max_contracts =
  25` and `[mandate.iv_fallback]` once beside a written `[mandate]`, after a
  backup and with a higher `policy_version` (owner decision 2026-10-29
  07:06 CET; `mandate_defaults_policy_file.go`). A table the owner wrote is
  kept; one deleted afterwards stays deleted. The daemon records in its
  state (`mandate_defaults`) that a file had its defaults, so this holds
  for tables the owner wrote and after the closing note is tidied away; a
  table the owner commented out with other values is left as it is.
- **Install the build** carrying these commits, from a clean worktree at
  local main.
- **Grant the futures permission at IBKR.** IBKR offers no read of it;
  the daemon learns it from a daily preview of one contract and refuses the
  hedge while IBKR shows it not enabled (`broker_permissions.go`, owner
  decision 2026-10-29 07:14 CET). The owner confirms it under Settings → Trading Permissions → Futures (United
  States, CME) in IBKR's account settings. Market data for CME index
  futures works (ESZ6 quoted live 2026-10-28).
- **Prove the path live.** A WhatIf of the hedge sale on the live session,
  on the owner's instruction, before the first real hedge; a refused
  permission shows there.
