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
- **Loss measure (TM-04.12).** The later owner decision below supersedes the initial unstopped-share
  treatment: a held index future spends loss room through its upward-shock
  loss, or its effective full stop distance at the multiplier. The rally
  comparison also remains in `index_rise_*`.

## Placement work and retained owner decisions

Each item below must land before a hedge can be sent.

1. **Held futures in the mandate (implemented 2026-10-28).** Previously a held future read "carries
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
4. **Owner numbers.** Decided 2026-10-28 20:07 CET (Desk session, on the
   recommendation); placing a hedge is refused while the table is absent:
   - `[mandate.index_hedge] max_contracts = 25`, the most contracts one
     hedge may hold: a hard stop against a wrong size, never the target,
     set where it covers the long side of the book. The owner's policy
     file carries the table as a comment with the decision, since the
     policy loader refuses a key the daemon does not read; the work that
     reads it asks the owner before activating it (uncomment and bump
     `policy_version`).
   - No second switch: the table's presence is the permission. No
     `allow_future_short`.
   - The loss measure (TM-04.12) counts a held hedge's loss in the 10%
     index rise, so a hedge spends loss room like every other line. This
     replaces "a future without a stop adds nothing" above for the hedge.
   - Order shape: sell at the bid, good for the day, US regular hours;
     no stop order on the hedge. Desk says "roll due" from ten days before
     the contract's expiry and "hedge no longer needed" when the book
     without it is inside the limit; the owner rolls and unwinds by hand.
5. **Futures trading permission at IBKR.** This cannot be read through
   Canary: the order path has no futures, and a WhatIf is an order-path
   request. Market data for CME index futures works (ESZ6 quoted live
   2026-10-28). The owner confirms the permission under Settings → Trading
   Permissions → Futures (United States, CME) in IBKR's account settings.
