# Gated orders and the trading build

Updated: 2026-10-31 08:33 CET

The standard `canary` binary is read-only and compiles in no broker-write path.
It does preview: once `[trading].mode` is paper or live, `canary order preview`
runs the broker WhatIf and mints a token, as do the proposal and opportunity
previews, and the standard build refuses every place, modify, cancel, exercise
or submit before anything is sent. The separate opt-in trading binary exposes
these actions:

- place or modify a single-leg stock/ETF or option order through the
  tokenized draft path (`canary order preview` → `place`/`modify`);
- place a buy or sale of one government or investment-grade bill or bond
  named by ISIN or CUSIP ([Bond orders](#bond-orders));
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

## Deactivated broker orders

An explicit broker `deactivate` flag appears as `deactivated` in the order
read model. Such an order is retained at IBKR and can be activated externally.
It remains visible, blocks competing orders, and does not count as active
protection. Canary does not activate it or offer modification through this
state. A later explicit active open-order callback resumes the same identity.
Ordinary `Inactive` callbacks remain distinct, and final fills, cancellations
and rejections remain terminal. Older callbacks without the flag cannot prove
deactivation.

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

## Trading permissions at IBKR

IBKR does not tell an application which products the account may trade, so
Canary learns it from the broker's answer to an order preview: an accepted
opening preview shows the class enabled, one it refuses for want of
permission shows it not enabled. For futures, the class of the index hedge,
the daemon checks itself at each start and then daily, with a preview of
buying one Micro E-mini S&P 500 contract at half its price, or selling one at
twice its price when a short is held so the preview still opens exposure.
IBKR evaluates it and no order is sent (owner decision 2026-10-29 07:14 CET). Options,
stocks and bonds are learned from your own previews. `canary trading status`
shows a row per class, and `canary status` a `Broker access` line, in yellow
while a class is not enabled. An order that opens or adds to a position in
such a class retains `broker_permission_not_enabled` until a fresh broker
preview confirms permission; a reduction or a close stays open. After enabling
stocks, options or bonds at IBKR, request another opening order preview in
Canary, even after a restart. An accepted opening WhatIf updates the
remembered permission; an accepted reduction or close says nothing about
opening and leaves it as it was, and a rejected, unavailable or failed
preview keeps the refusal. Futures keep their next-start and daily checks,
including the index hedge's opening gate: a check IBKR refuses for a reason
other than permission no longer confirms a remembered refusal, so futures
read unknown and the hedge's own preview asks IBKR again. All other risk and
submission gates still apply.

An accepted opening preview is recorded as permission evidence only if its account,
connection and held-position evidence stay unchanged while the preview is evaluated. An older request cannot
replace a newer result. A permission refusal learned before transmission also
stops an opening or increasing order that was previewed earlier; reductions
and closes retain their own broker and risk checks.

## Protective put purchases

A put purchase can use the protective-put exemption only for shares still
available to back it. Canary counts held long puts and working put purchases,
and subtracts working share sales, including stops and trailing stops. A
pending stock purchase or put sale creates no capacity until its fill appears
in the positions. An uncertain send still reserves capacity.

The daemon checks these commitments at preview, admission and the final send.
Before a preview or submission, it also requests a complete broker positions
receipt and compares all option quantities and the covering shares with its
measured book. A completed order from another client cannot release capacity
merely by disappearing from the open-order list before positions reflect it.
Missing or conflicting order and position evidence grants no exemption. A
purchase beyond available cover must satisfy the ordinary addition gates;
the exemption never changes the order caps or confirmation requirements.

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
complete open-order list and carry no origin. They are `cancel_eligible` and
`modify_eligible` only while [TWS order control](#orders-placed-in-tws) is
available for the current mode (`untracked_control`); otherwise cancel or
change them in TWS. An empty `untracked` list means none only when
`untracked_status` is `current`.

## Orders placed in TWS

An order entered by hand in TWS belongs to TWS's client id 0, and the API
lets only a connection with that id bind and change it. Canary's own
connection is pinned to another id and stays as it is. When the owner turns
`trading.tws_order_control.enabled` on, an owner-confirmed cancel or change
of such an order runs this way (owner decision "TWS B", 2026-10-11):

1. The ordinary broker-write gates run: trading mode, account, the freeze (a
   cancel passes it, a change does not), the journal.
2. The order is looked up by its permanent id in the broker's open-order
   list, and its terms must equal what was reviewed (symbol, side,
   quantity, filled quantity, order type, limit price, time in force).
3. A second, short-lived connection opens as client id 0 and asks TWS to
   list its orders, which binds the manual orders. The order is found again
   by permanent id and checked against the same terms.
4. The cancel is sent, or the replacement under the bound order id with the
   new quantity and limit price. Canary waits a bounded time for TWS's
   answer, then disconnects.

Side effect, by TWS's design: once bound, the order is an API order for the
rest of the TWS session. TWS shows it as such.

Only a DAY limit order on a stock or ETF is changed, to a quantity at most
the reviewed one and above the filled quantity, at a new limit price. Any
working order with a permanent id can be cancelled.

```sh
canary settings set trading.tws_order_control.enabled=true   # terminal only
canary orders open                                           # the perm id
canary order tws-cancel PERM_ID
canary order tws-modify PERM_ID --quantity 60 --limit 149.50
```

Without `--terms`, the command reads the order's current terms from
`canary orders open`, prints them and sends those. A caller that reviewed the
terms earlier passes them as `--terms JSON` (`symbol`, `action`, `quantity`,
`filled`, `order_type`, `limit_price`, `tif`), and `--request-id` makes a
repeat safe: the same id with the same intent returns the first outcome and
sends nothing; the same id with another intent is refused.

The outcome is one of four, in the result and in the journal:

| Outcome | Meaning |
| --- | --- |
| `confirmed` | TWS confirmed the cancel, or echoed the replacement's terms. |
| `pending` | TWS acknowledged the request (`PendingCancel`, `PreSubmitted`) without a final status before the connection closed; `canary orders open` settles it. |
| `refused` | Nothing changed. The control is off, the inventory could not be read, the order is not there, it differs from the reviewed terms, client id 0 is already connected (`tws_client0_busy`), the bind did not finish, or TWS refused with its own code. |
| `uncertain` | The request may have reached TWS and nothing proves either way. Read `canary orders open` again before deciding anything; Canary never retries it. |

Every request is journaled before any socket opens, under the origin that
asked and `bound_from_tws`, and its outcome after; a request without an
outcome is reported uncertain.

### Paper proof

The live account stays off until the path has worked once on the paper
account. The daemon records that proof itself, as
`trading.tws_order_control.paper_proven_at`, when a paper request ends
confirmed or pending; a patch cannot set it. The owner runs the proof, on a
paper TWS login, with Canary connected to the paper account:

```sh
canary status                      # paper, connected
canary settings set trading.tws_order_control.enabled=true
# In TWS, enter a small DAY limit order by hand, far from the market, e.g.
# BUY 1 AAPL LMT at half the price, and let it sit as Submitted.
canary orders open                 # the order is listed under "not placed by
                                   # Canary" with "tws-cancel or tws-modify"
canary order tws-modify PERM_ID --quantity 1 --limit <a new price>
                                   # TWS now shows it as an API order
canary order tws-cancel PERM_ID
canary settings show               # "TWS order control ... paper proven <time>"
```

A refused outcome names what to fix: `tws_client0_busy` means another API
client (another tool, or a stale session) holds id 0 in TWS; close it and
run again. Once `paper_proven_at` is set, the live account is eligible with
the same switch; the first proof is the one kept.

## Protection and exercise context

Protection proposals carry typed market-event source health and candidate
blockers. Active regulatory/news halts and LULD pauses block action. Borrow
inventory and fee flags can strengthen cover context but cannot create a new
long sell or buy-add idea. Reg SHO membership is context unless an existing
reduce/cover candidate supplies the action authority.

Option exercise is limited to daemon-owned candidates for held options. The
confirmation surface must disclose the resulting underlying exposure and block
an exercise that opens, increases, or flips risk.

### Exercise reconciliation

Owner decision recorded: 2026-10-23 11:43 WEST. Reconcile against IBKR evidence
with human confirmation of the exact request; keep uncertain requests blocked.

Sending an exercise instruction does not prove that IBKR has completed it.
Canary keeps the affected option quantity and underlying capacity reserved
while the outcome is unresolved, including after a timeout or restart. It
blocks another disposal of the same option and orders that would use the same
underlying capacity, including a leg of a strategy order. Unrelated contracts
and actions keep their existing gates.

Read the pending requests and available broker evidence:

```sh
canary opportunities exercises --json
```

Each pending request has a reference and revision. The result lists available
evidence IDs and explains any missing evidence. After checking that the exact
request has completed in full, with no quantity still pending, confirm it from
an interactive human terminal:

```text
canary opportunities reconcile-exercise REQUEST_REF REVISION --evidence EVIDENCE_ID --confirm-complete --json
```

Canary checks the imported IBKR Flex exercise record and its linked trade:
the account, option, underlying and whole quantity must match, the evidence
must be dated after the request, and the record must not already have been
used. The current option and underlying holdings in the same account must also
reflect the exercise. `--confirm-complete` is your attestation that this exact request
is fully resolved; the broker records alone do not prove which request caused
the position change. Canary saves that distinction with the resolution.

Successful reconciliation releases the reservation; it sends no order,
exercise or cancellation to IBKR. Missing, partial or ambiguous evidence keeps
the request blocked. A cancellation claim alone cannot clear it. The command
cannot override these checks, and agents and MCP cannot perform this
confirmation.

Older pending exercise records may lack the exact contract and position
metadata needed for evidence reconciliation. They stay visible with an
explanation and cannot yet use this recovery command. Canary blocks sales of
the affected option and trades in its known symbol; provably unrelated,
identified contracts keep their existing gates. If the historical record
cannot identify the affected symbol, or the proposed trade cannot be
identified, Canary cannot establish that it is unrelated and keeps it blocked.

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
