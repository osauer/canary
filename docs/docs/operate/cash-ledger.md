# Settlement evidence for the cash sweep

Read-only proposal history summarizes funding observations and omits repeated
obligation lists and calibration studies. Each historical row carries
`detail_availability.state=retained_in_canary_audit` and original list counts;
a missing count means that list was unavailable, not empty. Current funding
evidence and studies stay complete. Full historical detail remains unchanged
in Canary's SQLite audit and retained current state.

Canary explicitly requests `SettledCash` and `$LEDGER:ALL` from TWS. An
account-wide settled total cannot certify a currency balance, and TWS sends a
margin account none. Per-currency settled cash comes from TWS's
`SettledCashByDate` schedule on the account-value stream, below. Canary can also
read an optional historical Flex cash baseline through the existing reporting query.
It remains a **held estimate**, not permission to sweep. A current native
settled-cash observation or the optional authenticated Web API ledger can
separately certify live balances.

## TWS settlement schedule

TWS reports settled cash per native currency as the account value
`SettledCashByDate`, with a securities-segment twin `SettledCashByDate-S`, on
the full account-value feed Canary already subscribes to (reqAccountUpdates;
reqAccountUpdatesMulti only with `ledgerAndNLV=false`). Neither
reqAccountSummary nor the lightweight ledger request carries it. The value
lists settlement dates with the currency's settled balance on each, such as
`20261002:1234.56;20261005:2345.67`: the first point is today's settled cash
and the last is the balance once every pending trade has settled, which
equals trade-date cash. A rising schedule means proceeds are still to settle;
a falling one means a payment is.

`canary account --json` shows each currency's schedule as
`settled_cash_schedule` with `points`, `segment_points`, the local
`received_at` and a `status`. The daemon admits a schedule only when the
account stream is completely downloaded for the selected account on the
current socket, the value parses strictly (exact ascending dates, finite
amounts, IBKR's unset sentinel refused) and its final balance is within one
currency unit of that refresh's trade-date cash. TWS rounds stream cash to
whole units; a schedule that predates a fill or was cut short fails the
check. An admitted schedule's `low`, its lowest balance including the segment
twin, is the most the currency can spend without a settled debit on any
listed date. The sweep reads it as settled cash (`settled_source_kind:
native_tws_settlement_schedule`). Otherwise `status` is `held` with a
`reason`, and the currency stays `settlement_unknown`.

TWS resends a schedule only when it changes, so `received_at` can be hours
old and still current; reconciliation with fresh trade-date cash is the
freshness test. Proposal generation also rechecks the account snapshot
against the latest confirmed local fill and holds when the fill journal
cannot be read. The `BASE` row converts every currency into the base
currency and is never one currency's settled cash.

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

`account_stream_observation` separately surveys the already-running
`reqAccountUpdates` subscription. Reading it sends no new subscription,
connection or login. Its source is exact selected-account/current-socket
receipt metadata: allowlisted cash key names and currencies, broker ledger
prefix classification, invalid/unset/observed value flags, first/last callback
clocks and the existing initial-download health. No amounts or account IDs are
included. `AccountReady` can be ready, not ready, invalid or unknown;
`TradingType-S` distinguishes a recognized cash or margin regime, while absent
or unrecognized values remain unknown. `AccountType=INDIVIDUAL` describes
account ownership and cannot classify cash versus margin.

All stream clocks are **local receive/read clocks**, not broker valuation
times. `initial_complete` records an initial account download; it does not
prove that an individual cash field is fresh, that every subsequent cash event
was observed, or that cash can be swept. Unknown readiness stays explicit even
after initial completion. Bare `SettledCash` remains an account total. The
passive diagnostic never contributes a balance to cash authority. A scope or
socket change retires the receipt, and a subscription reset clears its rows.
Settlement schedule rows carry `currency_settlement_schedule` and read
`observed` only when they parse strictly.

- [IBKR account-value keys](https://www.interactivebrokers.com/docs/tws-api/doc/account-portfolio-data/account-updates/account-value-keys)

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
later confirmed baseline. Commitments and the currency's `keep_cash` reduce
estimated free cash. Purchases are conservatively counted from the start of the report day in
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
url = "https://localhost:5050/v1/api"
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

IBKR distinguishes outer Web sessions from brokerage sessions. Only one
brokerage session per username can be active across TWS, Client Portal and
other IBKR services. Do not assume that a Client Portal Gateway browser login
creates only an outer session: its actual authentication path must be verified
before use alongside the owner's TWS session. A competing login can displace
TWS. IBKR documents a second username for concurrent products; its permissions
and actual coexistence require owner commissioning. This reader neither starts
nor maintains a brokerage session and never calls `/iserver` initialization.
Gateway authentication and API calls must be on the same machine. IBKR requires
daily browser reauthentication and does not support automated Gateway login.
The example port is illustrative; no service is launched or trusted by this
configuration example.

- [IBKR Ledger fields](https://www.interactivebrokers.com/docs/web-api/v1/endpoints/portfolio/portfolio-ledger)
- [IBKR session boundaries](https://www.interactivebrokers.com/docs/web-api/authentication/sessions)
- [Gateway limitations](https://www.interactivebrokers.com/docs/web-api/authentication/cpgw/limitations-of-the-client-portal-gateway)
- [IBKR multiple sessions](https://www.interactivebrokers.com/docs/web-api/authentication/multiple-sessions)
- [Gateway authentication FAQ](https://www.interactivebrokers.com/docs/web-api/authentication/cpgw/client-portal-gateway-faq)

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

The policy keeps a reserve as cash in the base currency and a per-currency
`keep_cash`, subtracts commitments, requires `min_order_notional` for a buy and
caps each order's principal at the larger of `max_order_notional` and
`max_order_pct_nlv` of net liquidation value, in the account's base currency
([Reserve and order sizing](protection.md#reserve-and-order-sizing)). It has
no aggregate, daily or cycle budget.
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
hours, a settlement route and a supported payment calendar. USD and EUR bills
follow Canary's maintained route: SMART, USD T+1, EUR T+2 and T+1 for trades
from 11 October 2027. Canary releases keep it current with the payment
calendars, so a policy file needs no settlement lines and no date. A currency
table may override `settlement_exchange` and `settlement_days`;
`settlement_valid_through` only ends that currency's route on a date. A bill
order on another exchange, a passed end date, a currency without a payment
calendar or a date beyond the calendars' published coverage holds the route; a
weekday guess never authorizes an order. Each sweep row names its route and
`settlement_source`.

`min_order_notional` bounds a purchase's gross principal in account base
currency; a liquidity sale is never held to it. The order cap (the larger of
`max_order_notional` and `max_order_pct_nlv` of net liquidation value) bounds
principal on both sides. Broker lots and the reviewed
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


## Settled-cash request comparison

`canary account --settlement-probe --json` compares the normal account-summary
batch with one separate request whose tags are exactly `SettledCash`. Its raw
RPC response contains callback source/currency labels, mathematical finiteness,
receipt times and transport states. It contains no balances, account identifiers
or trading authority. Normal `canary account` reads retain their existing shape.

This is an explicit one-shot diagnostic on the existing connection, not a
scheduled fetch or a new broker session. The isolated request skips busy
ordinary subscriptions and yields when a new account, risk or exit read arrives.
End freezes its receipt; a guarded cancellation follows on the same socket.
`completed_empty`, `timeout`, `broker_error`, scope/session changes and failed
cancellation remain distinct. Late diagnostic callbacks cannot seed ordinary
account caches. An uncertain cancel prevents another probe and reserves one
broker summary slot; a competing second ordinary subscription receives a named
hold instead of waiting indefinitely. Session reset retires that reservation.

The isolated send/response has a two-second budget, then cancellation has at
most one second. A normal-batch comparison needs enough of the existing account
RPC budget to leave that room. Successful cancellation means the request was
written in order; TWS provides no cancellation acknowledgement. The experiment
can test whether batching suppresses a callback in the current session. A
returned account-wide or ambiguous total still cannot authorize native-currency
cash sweeping.


## Currency-only settlement experiment

`canary account --currency-settlement-probe --json` requests one lightweight
`reqAccountUpdatesMulti` subscription on the existing ready connector: the
exact managed account, an empty model and `ledgerAndNLV=true`. Ordinary reads
never start it. The request does not replace the existing legacy account stream
or require another connection, client ID, Gateway or login.

The official currency-only request contract differs from legacy account-value
callbacks. Exact concrete-currency callbacks retain `currency_only_multi`
origin, even when their key has no broker prefix. `BASE`, empty and malformed
currencies remain separate. Allowlisted wire keys, real-zero/finite/unset/invalid
value states, callback counts and local receipt clocks are returned without
amounts or account identifiers. No result is admitted to a financial cache or
used to authorize a sweep. Initial End alone does not prove field freshness,
settled-cash completeness or suitability for execution.

The same diagnostic scheduling lane gives ordinary account/risk reads priority.
Send/response is bounded at two seconds, followed by at most one second for
guarded cancellation on the original socket. End freezes the receipt; retired
IDs and late callbacks remain quarantined. An uncertain Multi cancel prevents
all further diagnostics until a socket reset, while preserving both ordinary
account-summary slots because Multi is a separate broker service. No cancel
acknowledgement is claimed. Active raw wire/packet capture prevents this
metadata-only experiment; existing capture behavior stays intact. Unimplemented
account protobuf protocols are refused.

The [official API1050 stable source](https://interactivebrokers.github.io/downloads/twsapi_macunix.1050.02.zip)
verifies legacy request76/version1, cancel77/version1 and callbacks73/74.
Account/position protobuf begins at protocol207; Canary currently advertises203.
See the [official model-account request contract](https://www.interactivebrokers.com/docs/tws-api/doc/account-portfolio-data/account-update-by-model/requesting-account-update-by-model).

`ledgerAndNLV=true` returns the lightweight ledger set only. Account values
such as `SettledCashByDate` arrive only with `false`, so an empty result here
says nothing about settled cash; the settlement schedule above is how TWS
supplies it.

If concrete-currency SettledCash is returned, financially admitting it is a
separate change requiring verified account/model/original-epoch provenance,
complete batch and duplicate handling, clock/freshness rules, cancellation and
readiness checks. If omitted, the result establishes omission only for this
session and request, rather than claiming that TWS never has that information.
