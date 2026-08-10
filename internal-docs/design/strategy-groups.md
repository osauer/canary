# Strategy Groups and Group Exits

Status: approved design for the first implementation.

## Purpose

Canary receives positions from TWS as individual contracts. A multi-leg option
position still needs one durable identity so its loss, alerts, reductions, and
exit can be handled as one strategy.

The first implementation covers strategy reconstruction, full close, and
proportional reduction. Rolling and free-form leg adjustment remain out of
scope.

## Authority

The daemon owns the strategy registry in `daemon.db`. RPC carries typed group
and operation contracts. CLI and app surfaces render that authority and never
construct a group or combo order themselves.

Each strategy revision binds:

- broker account and trading mode;
- an opaque strategy id and monotonically increasing revision;
- exact positive ConIDs;
- signed integer leg ratios and allocated quantities;
- the grouping source and confidence;
- the live position fingerprint used for the most recent reconciliation.

Every held contract quantity may be allocated at most once. Any remainder is a
standalone position. A strategy closes when its allocated legs disappear.
Assignment, exercise, expiry, an external trade, or a manual TWS adjustment
creates a new live-position fingerprint and requires reconciliation before a
later group operation.

## Grouping sources

Grouping uses this order of authority:

1. Canary order lineage.
2. Broker combo lineage when the TWS contract and order callbacks carry it.
3. An unambiguous reconstruction from current exact contracts and quantities.
4. One operator confirmation stored in the registry.
5. Conservative standalone positions when no safe grouping exists.

Automatic reconstruction is allowed only when one exact decomposition exists.
Several plausible decompositions are ambiguous even when each is a familiar
option structure. Canary must not choose one because it produces the lowest
risk number.

## Operations

`close` reverses every remaining leg in the strategy's ratio.

`reduce` accepts a positive number of whole strategy units. If a strategy has
three 1x2 units, reducing one unit closes one contract of the first leg and two
contracts of the second. A percentage shortcut is valid only when it resolves
to a whole number of units. An operation that would distort a leg ratio is
rejected.

Both operations become one IBKR `BAG` limit order. The preview names the price
as either "Pay up to" or "Receive at least" and includes:

- every leg and its before/after quantity;
- strategy id, revision, operation, and units;
- the net debit or credit limit;
- broker WhatIf and margin evidence;
- the resulting strategy and portfolio risk;
- the route's guaranteed-combo classification.

The first implementation submits only combinations covered by Canary's closed,
documented guaranteed-route policy. Unknown and non-guaranteed combinations
may be inspected but are not submit-eligible. Canary never falls back to a
sequence of single-leg orders.

## Preview and submit binding

A group preview token binds the complete draft plus the account, mode, client,
gateway session, trading-control generation, strategy id and revision, every
leg, every ratio, every current allocated quantity, the portfolio receipt, the
live-position fingerprint, price, order type, and time in force.

Submit repeats the normal broker-write authorization and rejects a stale token
when any bound value changed. A preview token is evidence, not broker-write
authority.

## Fills and reconciliation

A guaranteed combo may fill fewer strategy units than requested. That is a
valid partial reduction when the filled legs retain their ratio. Canary reports
the filled and remaining strategy units and evaluates risk after each fill.
The unfilled combo remains one working order until cancellation or expiry.
Repricing requires a fresh preview.

Execution details are reconciled per leg as well as at combo level. A leg
imbalance marks the operation `reconcile_required`, shows the remaining
exposure, and blocks another group operation until current broker positions
are reconciled. Cancellation targets the combo order. A partially filled
cancel leaves the completed strategy-unit reduction in place.

## Excluded workflow

Canary does not add a rolling workflow in this phase. A roll performed in TWS
is observed as a position change. The old grouping is retired or revised, and
Canary asks for confirmation only when the new grouping cannot be reconstructed
safely.

## Verification

Required coverage includes:

- exact and ambiguous reconstruction;
- no double allocation of a contract quantity;
- close and whole-unit reduction math for 1x1, 1x2, and four-leg groups;
- stale strategy revision and live-position fingerprint rejection;
- guaranteed-route admission and non-guaranteed rejection;
- protobuf `BAG` encoding with all exact ConIDs, actions, and ratios;
- accepted WhatIf binding before token minting;
- proportional partial fills and broken-ratio reconciliation;
- CLI JSON preservation and human debit/credit wording;
- full daemon/CLI tests and live smoke without transmitting an order.
