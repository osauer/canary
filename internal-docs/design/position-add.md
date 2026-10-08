# Position additions

Updated: 2026-10-08 19:39 CEST

## Scope and ownership

The first increment opens or increases a long stock position. It includes a
selected watchlist instrument with no holding. Canary proves the existing
quantity from a complete broker portfolio; no caller can supply a holding,
cash balance, risk budget or a claim that an absent position means zero.

The owner requested this staged build on 2026-10-08. The two new stock
allocation percentages remain **unapproved** until chosen by the owner. No
installer, migration, CLI, model tool or daemon start writes their values.
The feature is opt-in through `[position_add]` in the risk constitution.
Installing the code alone changes no trading policy.

`internal/risk/stock_add.go` owns sizing. The daemon assembles broker and policy
evidence. `internal/rpc/stock_add.go` is the CLI/MCP contract. Both surfaces
render the same result. Desk integration, bond sizing and option construction
are later increments.

## Operator and model surfaces

- `canary add plan SYMBOL --currency CCY --limit PRICE [--quantity N] [--con-id ID] [--json]`
  reads current capacity. Omitted/zero quantity means the maximum permitted
  addition in one order. A positive quantity requests exactly that addition.
- `canary add preview` accepts the same arguments and prepares a normal signed
  stock BUY, LMT, DAY, regular-session order review. The existing separate
  confirmation and submission path remains necessary.
- MCP `canary_add` calls only `add.plan`. It cannot mint a preview token,
  reserve funds, confirm, submit, modify, cancel or schedule an order.
- Planning can request a broker WhatIf for the exact candidate's commission
  upper bound. This is an untransmitted simulation, never a broker order.

Auto means calculate the maximum **for this invocation**, subject to the
per-order cap. There is no loop, recurring replenishment or automatic
submission. A watchlist membership or research signal grants no authority.
An optional `--exchange` selects the resolving venue; ambiguous stock identity
holds. The price ceiling and currency must be explicit. No FX trade is created.

## Approved inputs and arithmetic

The optional risk-constitution table has two required percentages in `(0,100]`:

| Key | Meaning |
| --- | --- |
| `position_add.max_stock_pct_nlv` | Gross market value of all stocks, including pending stock buys, as a percentage of current measured account net liquidation |
| `position_add.max_underlying_stock_pct_nlv` | Market value of stock in the selected underlying, including its pending buys, as a percentage of that same net liquidation |

These limits are distinct from issuer loss. The existing Rulebook issuer groups
combine stock and options, including share classes that the owner groups.

Capacity is the smallest whole-share allowance from stock allocation, stock in
the underlying, the current per-order notional limit, and spendable cash in the
stock's currency. Cash is the lower of observed trade-date and native settled
cash, less outstanding purchases and their full fee bounds, the existing
currency cash float, and the existing account reserve. The reserve is the
larger of its approved base amount and its approved percentage of NLV, converted
into the purchase currency. The full account reserve is retained in that
currency as a conservative first-stage constraint; currencies are never pooled.
The existing `no_buy_while_borrowed` cash policy also applies. Missing reserve or
borrowing policy values hold the feature.

The exact candidate needs an accepted WhatIf with a finite maximum commission
in its own cash currency. That fee reduces spendable cash. Up to three candidate
simulations seek a stable affordable quantity; failure to stabilize holds.
A requested quantity above the allowance is refused, never silently resized.
The maximum is conditional on the current snapshot, selected limit and returned
fee bound; it is not an investment recommendation or a fill guarantee.

After sizing the simple limits, Canary evaluates the hypothetical portfolio
through the existing issuer-concentration, premium-budget/sell-only, whole-book
net exposure, issuer loss-budget and margin-headroom rules (1, 3, 15, 18, 19).
Entry must remain inside their pass bands. Rule display modes cannot disable
these admission measurements. The existing capital state must be current,
reconciled and in its normal tier. A current regime is required. Existing
option legs use the Rulebook's earnings-aware hedge credit and valuation.
Missing risk evidence cannot create capacity. The entire stock purchase and
pending purchase commitments are conservatively deducted from excess liquidity;
broker buying power is never a cash budget. Thresholds remain in their existing
policy files; the code supplies no new investment numbers.

This is a new hard admission contract **only when the owner supplies the
optional table**. It does not globally promote other advisory rules or change
capital-state enforcement for other instrument types. An opted-in table also
applies to ordinary stock BUY previews/sends that open or increase a long, so
choosing the older order command cannot bypass it. Reductions and short covers
remain on their existing controls. A flip from short to long is unsupported.

## Pending activity and support limits

Known pending stock purchases consume allocation and issuer risk without being
reported as filled holdings. Every currency's purchase commitments must have
known fees. An unacknowledged local buy, uncertain broker inventory, armed queued
instruction, pending non-stock order or immediate sale holds an Add. Existing
covered stock stop orders may remain working; they confer no sizing credit and
are not resized by an Add. The normal protection process can subsequently
review the changed holding.

Held government bonds can be excluded from stock allocation only with exact
broker identity and independent government-issuer evidence. Held corporate debt
and unresolved non-stock instruments hold this first increment because combined
issuer mapping is not yet supported. An ambiguous stock identity also holds.
Options may be held and contribute risk; this increment creates no option
orders. Short additions, bond orders, multi-leg orders, modifications of an Add,
and scheduled or unattended additions are outside this increment.

## Review, admission and first-byte checks

The signed draft contains the exact stock, price, quantity, observed current
holding and policy fingerprints. The daemon recomputes capacity at preview and
again at submission. Changed holdings or policy require another review. A
smaller remaining allowance refuses the reviewed quantity; a larger allowance
cannot enlarge it.

The existing broker-write mutex serializes local sends. Existing journal
commitments prevent a second Add from spending an unacknowledged purchase's
cash. Before the first broker frame, the normal session, account, portfolio,
freeze, order limits and authorization checks remain binding. Add also compares
policy identities, the working-order generation, and an in-memory fingerprint
of account values, covering cash/margin-only changes without persisting account
data. There is no new broker access path. All owner confirmation, origin and
credential gates still apply.

Plans create no persistent state. Preview tokens use the existing signer and
expiry. Execution evidence stays in the existing order journal. Reverting the
feature code requires withdrawing unused Add previews; no migration of account
state exists. An owner policy revision is required to change or remove an
active `[position_add]` table.

## Verification contract

Synthetic tests cover zero and existing holdings, every capacity, native FX,
fees, exact quantity refusal, nonlinear option exposure, issuer groups, missing
inputs, pending purchases, strict request schemas, policy fingerprints, CLI
rendering, MCP read-only dispatch, submission revalidation and final wire holds.
The maximum is also checked against exhaustive quantities in small fixtures.
`make test` includes static/privacy/docs gates, render checks, race tests and
the historical regression spine. Broker-free tests establish these contracts;
actual broker commissioning is separate and requires the owner's policy values.
