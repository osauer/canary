# Index hedge

Created 2026-10-28 13:55 CET. Owner decisions 2026-10-28 13:17 CET (Desk
session): the hedge instrument is the Micro E-mini S&P 500 future (MES); its
size is "to neutral", meaning hedge until a 10% S&P fall changes the book by
about zero. "Flatten" in Desk means to neutral by cuts (net mode at 100%);
closing everything stays a separate emergency action in Desk.

## Built: the plan (`proposals reduce --portfolio --mode hedge --plan`)

- **Size.** The hedge is sized on the mandate's index stress of the whole
  book (`stresses.index_loss_base`). Every line counts, locked hedges too:
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
- **Loss measure (TM-04.12).** A held hedge counts its loss in the 10%
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
   does not increase, and no gap cap is breached. Otherwise it remains a
   risk addition and the normal mandate and drawdown gates apply. Counting
   the hedge's rise loss does not authorize weakening this criterion.
5. **Plan words for Desk.** The hedge plan and preview carry `roll_due`
   from ten days before a held contract's expiry and `hedge_not_needed`
   while the book without its held hedges reads within the mandate, each
   with one plain message (`docs/docs/operate/protection.md`).

## Held by the owner

Nothing above places a hedge until the owner does these things:

- **The table.** Daemon start writes `[mandate.index_hedge] max_contracts =
  25` and `[mandate.iv_fallback]` once beside a written `[mandate]`, after a
  backup and with a higher `policy_version` (owner decision 2026-10-29
  07:06 CET; `mandate_defaults_policy_file.go`). A table the owner wrote is
  kept; one deleted afterwards stays deleted.
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
