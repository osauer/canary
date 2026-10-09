# Position additions

Updated: 2026-10-08 21:40 CEST

## Scope and ownership

The first increment opens or increases a long stock position. It includes a
selected watchlist instrument with no holding. Canary proves the existing
quantity from a complete broker portfolio; no caller can supply a holding,
cash balance, risk budget or a claim that an absent position means zero.

Position contract correction, 2026-10-09 08:36 CEST: held equities use the
canonical `STOCK` response type, while broker requests use `STK`. Add and
portfolio target evidence use the existing position classification helper;
known holdings retain their measured quantity and currency conversion. Unknown
and unsupported types cannot become stock evidence through this conversion.

Owner revision approved 2026-10-09 15:52 CEST in the Desk interview
`01a11d08-684a-70c3-bed6-ab84135953f8`: existing policy supplies Add limits.
The optional allocation table is no longer required. Existing explicit extra
caps remain enforced when present; installation writes no private policy.
Automatic maximum sizing respects applicable warning boundaries. Exact manual
additions may acknowledge advisory warnings; missing evidence and hard funding,
order, margin and execution constraints remain binding. Warning acceptance is
bound to an exact preview and recorded before broker transmission.

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

The existing owner-reviewed risk, Rulebook and cash policies must be current.
`position_add` is an optional extra allocation restriction, not an activation
prerequisite. Its stock-only caps, when provided, have different semantics from
issuer loss and issuer delta; no default percentages are introduced.

Issuer loss, dollar delta and risk-capital checks are scoped to the affected
issuer, including configured share-class groups. Cluster checks retain every
member of each affected configured cluster. Cash, margin, regime exposure,
protection coverage and sell-only guidance remain portfolio-wide. Rule display
modes and existing thresholds are respected. FX and earnings findings retain
advisory semantics and source evidence is shown before and after the purchase.

Capacity is the smallest whole-share allowance from any explicitly configured stock allocation limits,
the current per-order notional limit, and spendable cash in the
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

After sizing funding and order allowances, Canary evaluates the hypothetical
portfolio through existing rules 1, 3, 8, 12, 14, 15, 16, 17, 18 and 19.
A missing required measurement cannot create capacity. Max is conservative;
manual exact quantity may carry brief warnings. `order place` requires explicit
`--accept-add-warnings` when the signed Add preview contains warnings. The
confirmation journal records acceptance; changed warnings require a new review.
Desk's device confirmation binds the same preview and accepts its displayed
warnings through the existing execution adapter. No AUTO strategy or arming UI
is added to the Canary CLI.

Capital evidence must be current and reconciled. AUTO sizing requires normal
capital state; manual entry retains the constitution's existing enforcement
and preview warnings. Existing option legs remain in the risk calculation.
The exact candidate's broker
before/after maintenance-margin and equity-with-loan change determines its
headroom debit. Commission is charged additionally; any simulated margin
release is ignored. Current and look-ahead headroom both retain the approved
floor. Pending purchases still consume their full fee-inclusive debit because
they have no combined broker simulation here. Broker buying power is never a
cash budget. Missing, invalid or mismatched margin currency holds the order;
an absent margin currency is unavailable evidence, not an inferred
account-base denomination. All margin values must be finite. Thresholds remain in their existing
policy files; the code supplies no new investment numbers.

A legacy explicitly configured `stock-entry-v1` table still routes ordinary
stock increases through the same funding/evidence review. It no longer silently
promotes advisory warnings to unoverrideable refusals. Reductions and short
covers retain their existing controls; flips from short to long are unsupported.

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
