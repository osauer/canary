# Position additions

Updated: 2026-10-08 21:40 CEST

## Scope and ownership

The first increment opens or increases a long stock position. It includes a
selected watchlist instrument with no holding. Canary proves the existing
quantity from a complete broker portfolio; no caller can supply a holding,
cash balance, risk budget or a claim that an absent position means zero.

The owner requested this staged build on 2026-10-08. The two new stock
allocation percentages remain **unapproved** until chosen by the owner. No
installer, migration, CLI, model tool or daemon start writes their values.
The feature is opt-in through `[position_add]` in the risk constitution.
Installing the code alone changes no trading policy. The owner's "I agree,
proceed" on 2026-10-08 authorised the design-review corrections: explicit
sizing intent and admission activation, exact fee accounting, shared reserve
funding, broker margin evidence and a complete position-plan explanation.
No numerical limits or live policy activation were requested. They remain
unapproved. Source revision recorded at 2026-10-08 21:40 CEST.

`internal/risk/stock_add.go` owns sizing. The daemon assembles broker and policy
evidence. `internal/rpc/stock_add.go` is the CLI/MCP contract. Both surfaces
render the same result. Desk integration, bond sizing and option construction
are later increments.

## Operator and model surfaces

- `canary add plan SYMBOL --currency CCY --limit PRICE (--quantity N | --max) [--con-id ID] [--json]`
  reads current capacity. `--max` explicitly requests the maximum for one
  order; a positive `--quantity N` requests exactly that addition. Neither or
  both intents are rejected, including zero quantity without Max.
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

The optional risk-constitution table requires two percentages in `(0,100]`
and explicit approval of the named admission contract. Setting allocation
percentages alone does not activate the new generic stock-order admission
checks. An Add request always requires all three values. Once activated,
ordinary stock orders cannot bypass the same checks.

`admission_contract = "stock-entry-v1"` approves the full contract below:
Rulebook rules 1, 3, 15, 18 and 19 must stay inside their **pass** bands for
long-stock increases, including ordinary stock BUYs, irrespective of display
modes. These previously advisory bands become hard admission checks for that
scope. The selected bands and thresholds remain in the existing Rulebook
policy. No new numerical threshold or exception is supplied by code.

| Key | Meaning |
| --- | --- |
| `position_add.admission_contract` | Explicit approval of `stock-entry-v1`; distinct from choosing allocation percentages |
| `position_add.max_stock_pct_nlv` | Gross market value of all stocks, including pending stock buys, as a percentage of current measured account net liquidation |
| `position_add.max_underlying_stock_pct_nlv` | Market value of stock in the selected underlying, including its pending buys, as a percentage of that same net liquidation |

These limits are distinct from issuer loss. The existing Rulebook issuer groups
combine stock and options, including share classes that the owner groups.

Capacity is the smallest whole-share allowance from stock allocation, stock in
the underlying, the current per-order notional limit, and spendable cash in the
stock's currency. Cash is the lower of observed trade-date and native settled
cash, less outstanding purchases and their full fee bounds, the existing
currency cash float, and the existing account reserve. The reserve is the
larger of its approved base amount and its approved percentage of NLV.
Every observed currency first deducts its own settlement float and known
fee-inclusive commitments. The purchase currency retains only the account
reserve shortfall after that other cash is counted in measured base value.
Negative balances reduce funding; missing settlement, float or conversion
holds instead of supplying reserve credit. The purchase itself still fits
settled native cash: reserve funding never creates an FX trade or borrowing.
The existing `no_buy_while_borrowed` cash policy also applies. Missing reserve or
borrowing policy values hold the feature.

The exact candidate needs an accepted WhatIf with a finite maximum commission
in its own cash currency and complete before/after margin evidence. Each
quantity is checked against **its own** fee and margin; neither is reused to
claim support for another quantity. A manual request reports its exact check,
not an extrapolated maximum. An oversized request is refused, never resized.

Max starts from a pure arithmetic/risk upper bound before broker costs and
simulated margin. It finds an affordable candidate and checks every larger
quantity up to that bound before claiming `maximum_known=true`. This does not
assume monotone fees or broker margin. A technical bound of 24 simulations and
the existing RPC deadline bound work; they are not investment limits. If work
ends before maximality is established, `add_search_incomplete` carries the
supported quantity and allowances, without a selected order or review. An
explicit quantity can then be checked afresh. A missing/rejected WhatIf remains
missing broker evidence, not proof that every smaller order is unaffordable.
The maximum is conditional on current evidence, not a recommendation or fill
guarantee. The normal preview obtains its own exact simulation again.

The plan distinguishes allocation room (the stock/type ceilings alone), the
next-order upper bound (cash, non-margin risk and the order cap before exact
broker costs), and the exact checked order. It exposes each monetary allowance
in base currency, native cash deductions, before/after allocations, the selected
rule evidence and stop-instruction coverage. Blockers distinguish policy,
capacity, unavailable evidence and unsupported instruments. Partial evidence is
retained; an unknown maximum never becomes a zero-capacity claim.

After sizing the simple limits, Canary evaluates the hypothetical portfolio
through the existing issuer-concentration, premium-budget/sell-only, whole-book
net exposure, issuer loss-budget and margin-headroom rules (1, 3, 15, 18, 19).
Entry must remain inside their pass bands. Rule display modes cannot disable
these admission measurements. The existing capital state must be current,
reconciled and in its normal tier. A current regime is required. Existing
option legs use the Rulebook's earnings-aware hedge credit and valuation.
Missing risk evidence cannot create capacity. The exact candidate's broker
before/after maintenance-margin and equity-with-loan change determines its
headroom debit. Commission is charged additionally; any simulated margin
release is ignored. Current and look-ahead headroom both retain the approved
floor. Pending purchases still consume their full fee-inclusive debit because
they have no combined broker simulation here. Broker buying power is never a
cash budget. Missing, invalid or mismatched margin currency holds the order;
an absent margin currency is unavailable evidence, not an inferred
account-base denomination. All margin values must be finite. Thresholds remain in their existing
policy files; the code supplies no new investment numbers.

This is a new hard admission contract **only when the owner approves the named
admission contract in the optional table**. It does not globally promote other advisory rules or change
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
are not resized by an Add. The plan shows shares under working stop
instructions and shares without them before and after the proposed addition;
pending buys are disclosed separately. Stops guarantee neither execution nor a
loss cap. A protection change requires its own separate owner review.

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
The maximum is also checked against exhaustive quantities in small fixtures,
including discontinuous and quantity-dependent fees. Permanent regressions cover
the former fee oscillation and the manual-order fee extrapolation defect.
Account-wide reserve tests prove native cash is never pooled; exact-margin tests
prove no headroom credit is borrowed from another quantity, look-ahead is retained
and missing evidence holds. CLI golden reports cover successful and held plans.
`make test` includes static/privacy/docs gates, render checks, race tests and
the historical regression spine. Broker-free tests establish these contracts;
actual broker commissioning is separate and requires the owner's policy values.
