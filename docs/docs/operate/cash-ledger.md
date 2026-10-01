# Settlement evidence for the cash sweep

The TWS cash ledger currently omits per-currency settled cash. The optional
`[cash_ledger]` connection reads it from an existing authenticated IBKR Web API
session. It supplements TWS; it never logs in, changes mode, starts a brokerage
session, keeps a session alive, or sends an order.

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
distinguishes broker hours from assumed fallback hours. For an issued US bill
only, an exact fresh TreasuryDirect CUSIP may supply maturity when an explicit
broker BILL line omits it; contradictory dates are refused and the review names
the public maturity source. No date is invented for another issuer or identifier.
This fallback applies to new USD bill candidates. A held recognized bill with
missing or invalid broker maturity holds its currency's cash-equivalent and
sweep calculation; a provenance-bound public maturity for held positions and
redemptions remains a commissioning follow-up.

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

A buy's accepted exact WhatIf must supply a consistent finite same-currency
maximum commission. Limit principal plus that upper envelope must fit free
cash. Missing or contradictory bounds hold at `cash_sweep_fees_unknown`; the
order cap remains principal-only. A buy that unexpectedly closes or reduces a
short holding is refused rather than bypassing the planned-bill checks.
Outstanding working/armed buys currently
carry no fee upper bound or currency, so fee-inclusive commitments remain
unknown and new sweeps wait in all currencies until those buys resolve.
Redemption proceeds and pending-sale estimates are still gross of commissions;
net redemption top-ups, forecast outflows and net-yield floors need follow-up.
