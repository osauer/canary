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
- **Loss measure (TM-04.12).** A future without a stop is counted as an
  unstopped share line is today: listed under `unstopped`, nothing added. A
  stop on it would count at its distance, as a stopped share line does. No new
  number. Its rally loss shows in the plan's `index_rise_*` figures.

## Not built: placing a hedge

Each item below must land before a hedge can be sent.

1. **Held futures in the mandate.** Today a held future reads "carries
   security type FUTURE, which the mandate does not model", which makes the
   whole verdict unknown and refuses every addition.
   - A held MES must become the same `MES FUT` leg the plan uses.
   - Net mode must read its dollar delta at the multiplier:
     `positionDollarDelta` reads a non-option at quantity × mark.
   - A stop order on it must count at contract value per point.
2. **The order path.** Preview, WhatIf, placement, journal and fill
   coherence accept STK/ETF/OPT/BOND/BILL/CASH only ("order preview supports
   STK/ETF/OPT contracts only"). A future through them is a broker-path change:
   release tier, with a live WhatIf proof under the owner's instruction. Desk's
   device-confirmed bundle submits one token per leg; a hedge leg is one token,
   with no second path.
3. **Gate classification** (`riskAddingOrder`, shared by the mandate gate and
   the drawdown brake). A SELL of an index future is a reduction, passing
   `refuses_additions`, only when all of these hold, re-evaluated on the kept
   book with the order:
   - the 10% fall's loss is lower and still not below zero (to neutral, never
     past it);
   - the loss measure does not rise;
   - no gap cap is breached.

   Otherwise it adds risk and the normal gate applies. A BUY that closes or
   reduces a held hedge is already a close or reduce.
4. **Owner numbers.** These go in an optional table, and placing a hedge is
   refused while they are absent:
   - the order cap reads notional, and one MES carries about five times the
     index level in USD, so a hedge needs its own cap: proposed
     `[mandate.index_hedge] max_contracts`, the most contracts one hedge may
     hold;
   - the order limits have switches for a stock short and an option sale to
     open, but none for a future sold to open: proposed
     `allow_future_short` under `[order_limits]`, or the table above standing
     as the switch.
5. **Futures trading permission at IBKR.** This cannot be read through
   Canary: the order path has no futures, and a WhatIf is an order-path
   request. Market data for CME index futures works (ESZ6 quoted live
   2026-10-28). The owner confirms the permission under Settings → Trading
   Permissions → Futures (United States, CME) in IBKR's account settings.
