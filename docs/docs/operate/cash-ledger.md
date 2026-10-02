# Settlement evidence for the cash sweep

Canary explicitly requests `SettledCash` and `$LEDGER:ALL` from TWS. An
account-wide settled total cannot certify a currency balance. Canary can also
read an optional historical Flex cash baseline through the existing reporting query.
It remains a **held estimate**, not permission to sweep. A current native
settled-cash observation or the optional authenticated Web API ledger can
separately certify live balances.

## Native TWS observation

`canary account --json` includes `settlement_observation` only for a completed,
current-session account-summary request. It reports the callback count and
currency, source label and finite-value flag for each settled-cash callback;
it contains no amounts or account identifiers. `finite` means mathematical
finiteness only, not availability: IBKR's finite unset sentinel is refused by
the native settled-cash parser. Real zero remains an observed value. `account_total` is the concrete
account's bare field, `aggregate_ambiguous` lacks usable currency scope, and
`broker_ledger_label` carries TWS's explicit `$LEDGER-` prefix. A completed
request with no callbacks is distinct from an unavailable receipt. The request
end marker freezes both rows and diagnostic counts; continuing subscription
updates cannot rewrite that observation cutoff.

The diagnostic never supplies a balance or authorizes a sweep. Bare
`SettledCash`, including an `Account=All` callback from a single-account login,
remains excluded from the native currency ledger. Aggregate ledger callbacks
also require one known managed account matching the selected account. Raw
callbacks cannot claim the reserved internal `$LEDGER:` storage namespace.
Request cancellation,
timeout, conflicting account scope, cached fallback and changed socket/account
sessions cannot publish a current receipt. Existing requests are cancelled by
their own request IDs; an old socket epoch cannot cancel a successor's request.

- [IBKR account-summary tags](https://www.interactivebrokers.com/docs/tws-api/doc/account-portfolio-data/account-summary/account-summary-tags)
- [IBKR per-currency prefix](https://www.interactivebrokers.com/docs/tws-api/doc/tws-settings/per-currency-account-value-prefix)

## Historical Flex baseline

In the existing Activity Flex Query, enable **Cash Report** and select
`accountId`, `currency`, `fromDate`, `toDate`, `endingCash` and
`endingSettledCash`. Fetch a new statement. This adds no gateway login and does
not change the required Recon/Edge reporting profile.

Native cash exports can omit `reportDate`: Canary binds it to the row's `toDate`
only when that date exactly matches the parent statement's period end. If
`reportDate` is supplied, it must also match; empty or invalid values are refused.

Canary reads only accepted, query-scoped statement bytes bound to the current
account/mode. The selected report must cover the latest completed New York
reporting day. Cash rows must match its account and dates; aggregates, duplicate
currencies, invalid amounts and conflicting same-generation baselines hold.
Unknown values never become zero.

The diagnostic estimate subtracts observed purchase principal from baseline settled
cash, also bounded by current TWS cash after excluding observed sale proceeds.
Exact fees remain unknown. Sale credits stay excluded until a
later confirmed baseline. Commitments and the reserve reduce estimated free
cash. Purchases are conservatively counted from the start of the report day in
New York: neither its date nor its timezone-less generation label proves an
exact intraday cutoff.

**Coverage remains incomplete.** Before an estimate can authorise a sweep,
Canary must verify unsettled debit obligations already outstanding at the
baseline, complete account-wide manual/offline executions, withdrawals,
transfers, FX/corporate-action cash legs, fees and uninterrupted activity.
Equal ending cash and settled cash, an empty local journal or improving TWS
cash cannot clear these gaps. Current code exposes `settlement_projection` in
proposal JSON and labels CLI estimates unverified; it never uses them as
settled-cash authority. Missing Cash Report fields show the owner action above.

## Optional live Web API ledger

The `[cash_ledger]` connection supplements TWS using an existing authenticated
IBKR Web API session. It never logs in, changes mode, starts a brokerage session,
keeps a session alive or sends an order.

```toml
[cash_ledger]
url = "https://localhost:5001/v1/api"
ca_cert_file = "/absolute/private/path/client-portal-ca.pem"
```

Use the port and trusted certificate of the local Client Portal Gateway you
actually run. The example has no default or automatic discovery. A local
Gateway normally needs no bearer file. For an already supported direct Web API
session, `bearer_token_file` can name a private regular file containing the
final SSO bearer token; this does not register an OAuth application or make
retail OAuth access available. Keep credentials out of TOML and Git.

Canary makes only `GET /portfolio/accounts`, followed by
`GET /portfolio/{selected-account}/ledger`. Redirects and system proxies are
disabled; TLS certificate and hostname checks remain enabled. A successful
response must name the exact TWS account and explicit currency, cash balance,
settled balance and original broker timestamp. `BASE` aggregates, duplicate or
case-aliased fields, missing/null values, nonfinite amounts, future dates and
timestamps older than one minute cannot certify cash. Non-ASCII object keys are
also refused, including Unicode aliases that a JSON decoder could fold into a
cash field.

The daemon shares reads for at most 15 seconds within one exact TWS connector
session, account and mode. It rechecks that binding and the original source
time before use. Confirmed local fills invalidate pre-fill TWS and Web balances;
an unreadable/uncertain local journal cannot release that reservation. The
journal supplies a latest-known fill frontier, rechecked when proposal generation
consumes the cash after its position and working-order reads, never settled cash or proof of
complete external/offline activity. Failed reads never revive a last-good cash
balance. Usable cash is the lower of TWS trade-date cash, Web cash balance and
Web settled cash; native per-currency settled cash, when present, also bounds it.
TWS account-summary authority, source time and FX rates remain unchanged.

Restart Canary after configuring the connection. Check `canary account --json`:
`cash_ledger` reports source health, and each admitted currency carries
`web_cash` with its scope and original timestamp. `canary proposals list --json`
reports remaining sweep holds. Source readiness does not authorise an order.

IBKR documents that an outer read-only Web session can coexist with TWS; a
second brokerage session can replace it. Authenticate locally without taking
over the existing trading session. Client Portal Gateway authentication and API
calls must be on the same machine; Canary never calls `/iserver` session
initialisation as a fallback.

- [IBKR Ledger fields](https://www.interactivebrokers.com/docs/web-api/v1/endpoints/portfolio/portfolio-ledger)
- [IBKR session boundaries](https://www.interactivebrokers.com/docs/web-api/authentication/sessions)
- [Gateway limitations](https://www.interactivebrokers.com/docs/web-api/authentication/cpgw/limitations-of-the-client-portal-gateway)

## Bill commissioning and allocation limits

`canary market --symbol <ISIN-or-CUSIP> --type BILL --json` reads contract,
quote and session evidence without previewing an order. Session provenance
names verified broker hours; missing or malformed hours stay unknown. For an issued US bill
only, an exact fresh TreasuryDirect CUSIP may supply maturity when an explicit
broker BILL line omits it; contradictory dates are refused and the review names
the public maturity source. No date is invented for another issuer or identifier.
Held USD bills use the same exact fresh TreasuryDirect CUSIP binding, with
public provenance shown separately. Invalid dates or conflicting frames hold
classification and orders. German Bubills may use the exact official Finance
Agency factsheet for their maturity. When the broker reports only an IBCID,
issuer ISIN and currency are explicitly **request-bound**, never invented broker
fields. Held German bills repeat the owner-allowlisted ISIN lookups and the
ConID lookup on one captured account/session; exactly one matching mapping is
required. Missing, ambiguous or changed mappings hold the currency. Every
preview repeats exact identity and maturity checks on its own broker session;
changes to semantic maturity or identity invalidate the approval revision.
Order units remain assumptions until an exact broker WhatIf verifies them.

An actual eligible sweep row is still needed for the gated WhatIf preview.
It verifies units, access and commissions; preview tokens do not authorise
submission. Missing settlement evidence, an empty owner ISIN list, unavailable
quotes or session evidence remain holds.

The policy keeps a per-currency `keep_cash`, subtracts commitments, requires
`min_tranche` and caps each order's principal with `max_order_notional` in the
account's base currency. It has no overall percentage, daily or cycle budget.
Repeated separately authorised orders may therefore deploy almost all eligible
free cash down to the reserve and sizing residual. Those additional limits are
owner decisions, not values Canary chooses.

A bill review chooses a sensible whole-order quantity, then requests one broker
WhatIf for those exact terms. It shows price, yield and fees; fees do not trigger
repeated previews or automatic resizing. Behind the scenes, an exact finite
same-currency maximum commission and limit principal must fit free cash. The
ordinary broker estimate is informational; it cannot certify that reservation.
A missing or contradictory exact bound holds at `cash_sweep_fees_unknown`; the order cap remains
principal-only. A buy that unexpectedly closes or reduces a
short holding is refused rather than bypassing the planned-bill checks.
Canary persists that exact-term maximum-fee reservation with its place or modify attempt.
A matching current DAY working buy reserves remaining principal plus the full
reserved fee, including after restart or partial fills. Unknown, stale or changed
bounds hold new sweeps; external and unbounded queued buys retain that hold.
Liquidity sales estimate net proceeds after the exact reserved broker fee.
They may restore only part of a shortfall without fee-driven resizing or a
shortfall-only hold; the remaining gap stays visible until settlement proves
cash has returned.
Pending-sale estimates remain diagnostic, and forecast outflows and the broader
portfolio stress reserve still need follow-up.

## Sweep order checks

Verified settled cash is one input. Bill orders also require broker trading
hours, a reviewed route-specific settlement lag and a supported payment calendar.
Missing evidence holds the route; a weekday guess does not authorize an order.

`min_order_notional` bounds the complete order in account base currency, on both
sides, using gross principal for both purchases and liquidity sales.
`max_order_notional` also bounds principal. Broker lots and the reviewed
price still need to fit those bounds. A failed cash or whole-order check holds
the reviewed quantity; a different quantity needs a new review.

Purchase-benefit advice compares holding the bill to maturity with leaving cash
at the broker, using entry spread, reserved purchase fees and a dated
owner-reviewed cash-interest upper bound. Current complete inputs give a known
estimate; missing inputs give unknown advice, and expired interest assumptions
give stale advice. A missing rate is never assumed to be zero.

`min_net_gain` stays readable in existing policy files as an advisory benchmark.
Below-benchmark or negative estimates remain warnings; unknown or stale benefit
does not block or resize an order. Fees still matter to actual cash accounting.
Liquidity sales show fee-adjusted expected proceeds and any remaining cash gap. See
[the configuration reference](../reference/config.md) for the policy fields.
Existing private policy values stay unchanged until explicit owner migration;
route commissioning remains required for orders. Dated cash-interest assumptions
are needed only for current purchase-benefit advice.
