# Gated orders and the trading build

Updated: 2026-10-06 21:34 CEST

The standard `canary` binary is read-only and compiles in no broker-write path.
The separate opt-in trading binary exposes these actions:

- preview, place, or modify a single-leg stock/ETF or option order through the
  tokenized draft path (`canary order preview` → `place`/`modify`);
- preview and place a buy or sale of one government or investment-grade bill
  or bond named by ISIN or CUSIP ([Bond orders](#bond-orders));
- submit or reduce a daemon-owned close/reduce protection proposal;
- submit a cash sweep proposal, which buys a same-currency bill with idle cash
  or sells one ([Cash sweep](cash.md#cash-sweep));
- submit a currency leveling conversion, which repays a borrowed currency
  ([Currency leveling](cash.md#currency-leveling));
- exercise an eligible held option when the action reduces or closes risk;
- close or reduce a grouped option strategy as one combo; and
- cancel a Canary-owned order.

`order preview` mints a signed draft token and runs the broker WhatIf; it never
transmits, and a minted token is not submit authority. `place` and `modify`
consume a submit-eligible token for that exact draft and pass the same daemon
admission gates as every other write. A proposal or opportunity preview is
candidate-specific evidence and never submit authority. The MCP server has no
preview or execution tools.

## Required authority

Trading configuration must pin `[gateway].account`,
`[gateway].client_id`, and `[trading].mode` to `paper` or `live`. A missing or
disabled mode means no order entry. The broker-confirmed account, client ID and mode must match those pins in
paper and live sessions. An explicitly pinned endpoint must also match.

The per-order limits are the risk policy's `[order_limits]` in
`~/.config/ibkr/policies/risk-policy.toml`: a notional cap that scales with
net liquidation value, an option contract cap, the stock-short and option
sell-to-open permissions, and the longest maturity a bond or bill buy may have
([Order limits](../understand/policy.md#order-limits)).
While a key is missing, every order preview is refused with `order_risk_limit`;
a missing `max_bond_maturity_years` refuses bond and bill buys only.
A risk policy written before the table existed gains it at the next daemon
start, after a backup. The `[trading]` keys `max_notional`,
`max_option_contracts`, `allow_stock_short` and `allow_option_sell_to_open`
are retired: they still load but are never read.

Every broker action additionally keeps its exact candidate revision, fresh
confirmation or preflight contract, quantity and exposure limits, journal
health, daemon authorization, origin policy, and `trading.freeze` gate. An
alert, plan, proposal, preview, prior instruction, or write-ready status is
evidence, not authority for a new transaction.

Keep an inactive example at `~/.config/ibkr/config.toml.trading`; the daemon does
not load it until the `.trading` suffix is removed. Before activating it, verify
the pins and start with a paper session. `canary trading status` reports the
current boundary and the order cap in force but cannot authorise a trade. Its
`freeze` field mirrors `trading.freeze` in every mode, and
`trading_control_generation` advances with every change to the freeze, so two
readings show a freeze that was set and lifted in between.

## Bond orders

```text
canary order preview buy|sell ISIN|CUSIP FACE --type BOND|BILL --currency CCY
```

`FACE` is the nominal amount in the bond's currency, for example `25000`
for 25,000 USD of a Treasury note. Canary finds the bond at IBKR, works out
the order quantity (IBKR counts US bonds in units of 1,000 and EUR, GBP and
CAD bonds in units of 1), and prices a limit order for the day between the
live bid and ask, inside the bond's own trading hours. A limit price, trail,
outside-hours flag or change to a working bond order is not offered; cancel
the order and preview again instead.

A buy is accepted only when an outside source confirms what the bond is:

| Bond | Confirmed by |
|---|---|
| US Treasury bill, note or bond | TreasuryDirect |
| Any bond on the ECB's daily list of eligible assets: euro-area government bonds and investment-grade corporate, bank and agency bonds | European Central Bank |
| UK gilt or Treasury bill, Government of Canada bond or bill | OpenFIGI, and the maturity and coupon must match IBKR's own description |

The ECB list only admits bonds rated BBB- or better, so a bond on it counts
as investment grade. Canary refuses inflation-linked bonds, floating-rate
notes and asset-backed securities, a bond maturing within seven days, and a
bond maturing later than `[order_limits] max_bond_maturity_years` allows. A
US-dollar corporate bond that is not on the ECB list is refused.

The value checked against the order cap is the face times the price per 100,
plus up to one year of coupon for the accrued interest a buyer pays. The
preview shows the bond, the source that confirmed it, its maturity and
coupon, and both parts of the value.

A sale needs no confirmation: it may sell only bonds you hold, never more
than the held face less any sale already working, and it never opens a short
position.

A buy priced at or above what the bond still pays (100 plus its coupon for
the years left) is refused: it would yield nothing. TWS's own negative-yield
confirmation is switched off for API orders, so Canary makes this check
itself. A buy preview also states the bond's yield, its duration and what a
one-point rise in yields would cost.

## Bond risk

`canary positions` shows, for each bond you hold, its issuer and whether it
is a government or investment-grade issuer, its yield, its duration, how much
a 0.01-point move in yields changes its value (DV01), and what a one-point
rise in yields would cost. Below the table it sums the book: bonds and that
loss as a share of net liquidation value, the non-government share, and the
largest issuer. A bond Canary cannot measure, such as an inflation-linked one,
is named with the reason. `canary stress` adds a "Rates and credit" row with
the same sums. None of this warns or blocks yet: the limits are still to be
set.

## Who placed an order

Each row of `canary orders open --json`, `orders history` and `order status`
carries the `origin` journaled with the request that placed it: `agent`,
`human-tty`, `human-paired-device`, `daemon-preauthorised` or
`daemon-owner-queued` (a queued authorisation the owner signed). Modify and
cancel requests keep their own origin on their events; the daemon's protective
stop guard journals `daemon-protective-guard` when it shrinks or cancels one of
Canary's own stops. A row without an origin
was not placed through Canary. `canary orders open` also lists, under
`untracked`, the orders working at the broker that Canary's journal does not
track, such as orders entered by hand in TWS. They come from the broker's
complete open-order list, carry no origin and cannot be modified or cancelled
through Canary. An empty `untracked` list means none only when
`untracked_status` is `current`.

## Protection and exercise context

Protection proposals carry typed market-event source health and candidate
blockers. Active regulatory/news halts and LULD pauses block action. Borrow
inventory and fee flags can strengthen cover context but cannot create a new
long sell or buy-add idea. Reg SHO membership is context unless an existing
reduce/cover candidate supplies the action authority.

Option exercise is limited to daemon-owned candidates for held options. The
confirmation surface must disclose the resulting underlying exposure and block
an exercise that opens, increases, or flips risk.

## Auto connections

Auto endpoint discovery can select Gateway or TWS without a port pin. The
broker must confirm the configured account; that account, not its port number,
determines live/paper mode. Account and client ID pins, trading mode, freeze,
risk checks and per-order authorization still apply. A switch requires a new
order preview and current broker evidence. Pinned ports remain fixed. After an
upstream loss lasting 30 seconds, Auto tries another discovered listener first;
brief losses and outages with no alternative retain the existing connection.

IBKR grants one login per user. When both IB Gateway and TWS run, the app that
signed in last holds the session and the other keeps listening at its login
screen. The daemon warns once, at WARN level, when two local API listeners
answer the probe, names the port it tries first, and logs every failover at
WARN. A listener that accepted the connection but never completed the handshake
is tried last on the next rediscovery, so the signed-in app is reached without
waiting out the handshake budget; a pinned port or a sole listener is never
reordered. A listener that closes or resets the probe's connection before
reading a byte is ordered behind every listener that holds it at discovery
time already, and is reported as its own state (`port_rejecting`) when it is
the only one. Quit the app you are not using.

## Release boundary

Each release publishes standard and `canary-trading-*` artifacts side by side.
The installer and updater select the standard artifact unless the user
deliberately installs the trading tarball. Product v3 release publication is
hermetic: it performs no broker preview or write. Gateway behavior is verified
separately by the authorized read-only smoke target.

MCP remains read-only in every build. Adding an MCP broker action would require
a separate authority, nonce, audit, confirmation, and adversarial review; no
current configuration turns one on.
