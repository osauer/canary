# Fill quotes and the execution cost report

Updated: 2026-11-04 08:40 CET (backfill on a new broker session)

Canary kept no quote at any order or fill, so the spread the desk pays was
unmeasured and the scorecard reported `slippage_reason: no_fill_quote` for
every decision. This note is the contract for the quote the daemon now
retains at every order event and fill, the poll that finds executions Canary
did not place, the `reporting.costs` report, and what none of it can know.
Code: `internal/daemon/fill_quotes.go`, `pkg/ibkr/executions.go`,
`internal/rpc/reporting_costs.go`, `internal/cli/reporting_costs.go`.

## What is recorded, when, where

| Event | Quote | Where it lives |
| --- | --- | --- |
| Order previewed | The preview's exact-contract quote (bid, ask, mid, last, displayed sizes, data type, the quote's own time) and the gap between that time and the preview. | The `previewed` journal event, field `quote` (`rpc.FillQuote`). |
| Order sent | The same preview quote the order was priced on, with its gap to the send. No broker read happens inside the send path. | The `send-attempted` journal event, field `quote`. |
| Fill of an order Canary placed | A fresh exact-contract read (bid, ask, mid, last, displayed sizes, data type, the quote's own time) made right after the fill's `execDetails` callback was journaled, with the gap between quote time and execution time, plus the FX rate to base at that moment. | The fill-quote store: daemon.db `observations`, scope `fill-quotes`, kind `fill_quote`, source `canary`, one row per `exec_id`. |
| Execution Canary did not place | The same read, made when the periodic poll (or, for a master client, the callback) first sees the execution, with the lag from the broker's execution time. | The same store, source `broker`. |

The first poll of each broker session (a new socket session, or a daemon
start) also sends one reqExecutions with `lastNDays` set to seven, the most
TWS answered on 2026-11-04 (server version 203; eight and above returned an
empty, complete answer with no notice). Executions it finds that are not yet
retained are stored with `seen: backfill` and no quote read: `status:
unavailable`, `reason: backfilled`, because a quote taken days after a fill
measures nothing. They count in the record and in the visibility proof, never
in a cost. The current-day request is unchanged and its outcome remains the
poll's; a backfill that did not complete is owed again next poll, and an
empty complete answer (a server that returns nothing for the field) settles
the session. Fills the daemon missed while down therefore reach the record
at the next connect instead of the next statement.

The journal events gained one optional field; nothing existing changed, and
the order path's gates, tokens and journaling run exactly as before. The
fill quote is read off the callback path: the lifecycle handler only offers
the journaled fill to a bounded queue (256), and a worker reads the quote
within 4 s on the background pacing lane. A quote that cannot be read in time
is recorded as `unavailable` with the reason (`timeout`, `broker_unavailable`,
`quote_not_two_sided`, `quote_crossed`, `contract_unknown`,
`quote_read_failed`); the fill is retained either way, exactly once per
`exec_id`. A full queue releases the fill to the next poll.

FX is base per contract currency at capture: a cached rate under fifteen
minutes old, else a live read (3 s budget), else the last known rate within
the cache lifetime (`cache_stale`); the base currency comes from the proven
account summary. A missing rate is recorded as `fx_unavailable` and the
report shows that fill's cost in the contract currency only.

Execution time is the broker's (`broker_exec_time`) when it parses and does
not sit more than a minute after the local receipt; otherwise the receipt.

## Fills Canary did not place

The owner trades mostly by hand in TWS, so those fills matter most. What the
TWS API documents (legacy reference, `executions_commissions.html` and
`initial_setup.html`; the current IBKR Campus text is behind a 403 for
non-browser fetches and was not re-read):

- `execDetails` is delivered "when an order is filled either fully or
  partially", for the orders the client is entitled to see.
- `reqExecutions` "receives a ExecutionFilter object as parameter to obtain
  only those executions matching the given criteria"; "an empty
  ExecutionFilter object can be passed to obtain all previous executions";
  "by default, only those executions occurring since midnight for that
  particular account will be delivered". `ExecutionFilter.ClientId` is "the
  API client which placed the order".
- "To receive commissions reports for all clients it is necessary to connect
  as the Master Client ID"; the Master Client ID "should automatically
  receive updates about all open orders, as well as commission reports from
  orders placed from all clients". Manual TWS orders bind to client 0 or the
  master client.

The documentation does not say in one sentence whether `reqExecutions` from
a non-master client returns other clients' executions. Canary therefore
designs for both cases and lets the daemon prove which one holds:

- Every 60 s while the broker session is up, the daemon sends
  `reqExecutions` (version 3, account filter only, client id 0, request
  type general, background priority) and waits up to 15 s for the matching
  `execDetailsEnd` in either wire encoding. Answers carry the request id and
  are routed to the asking flight, never to the order lifecycle handlers, so
  a requested inventory cannot mint phantom journal rows or re-journal a
  fill. Every execution not yet retained is captured as above.
- An execution whose client id is Canary's (or, without a client id, one
  that carries an order reference) is source `canary`; anything else is
  source `broker`.
- Visibility is proven, not assumed: `observation.visibility` is
  `all_clients` once any execution from another client id has been seen in
  this account, broker mode and API client scope, otherwise `own_orders_unproven`, and the report
  then names the setting that widens it: TWS Global Configuration → API →
  Settings → Master API client ID = Canary's client id. The owner decides
  that setting; Canary never changes TWS configuration.
- The same verdict is where a user looks first (2026-10-18): `canary status`
  carries an `Executions` row (`this client's own orders only · set TWS
  Master API client ID to N to see hand and other-client fills`, or `all
  clients (proven HH:MM)`), `--json` and `canary_status` carry it under
  `executions` (`visibility`, `proven_at`, `client_id`, `setting`, `action`),
  and `observation.proven_at` in the costs report names the poll that first
  saw another client's execution. One helper (`executionVisibility`) builds
  both from the retained poll state; the proof and its time are sticky for
  the same account, broker mode and client id and start over for any other.

Consequence to decide before flipping the master client setting: a live
`execDetails` for an order Canary did not place would then reach the
lifecycle handler, which (by its existing, documented pass-through) journals
unmatched executions as orphan `status-updated` rows. The fill-quote capture
records such a fill as source `broker`; the journal pass-through itself is
unchanged by this work and is called out in the final report.

## What the report shows

`canary reporting costs [--month YYYY-MM] [--json]` (RPC `reporting.costs`,
MCP `canary_reporting_costs`, read-only). Per fill: time, side, quantity,
contract, fill price and currency, source; the quote (bid, ask, mid, how
long after the fill it was read); the half-spread paid

    (fill − mid) × side sign × quantity × multiplier

in the contract currency and in base at the fill's FX (BUY sign +1, SELL −1;
positive is cost, negative is price improvement). Multiplier is 1 for stock
and currency pairs, the contract multiplier for options and futures, 0.01
for bonds (IBKR prices per 100 of face); an option without a reported
multiplier is unknown (`multiplier_unknown`). Totals by month (UTC), by
source (`canary` vs `broker`) and by instrument class sum the measured fills
that have an FX rate and count the ones that do not (`unconverted`).
Coverage is fills with a usable two-sided quote over all fills. Fills
without a quote are listed as unknown with the reason. The observation
section states the poll cadence, the last poll's outcome and the visibility
verdict. Nothing names the account.

## Lookup for the scorecard

`(*Server).FillQuoteByExecID(execID)` and
`(*Server).FillQuoteNear(conID, at, tolerance, exclude)` return the retained
`rpc.FillQuoteRecord` from the in-memory mirror of the store; `exclude`
names execution ids another match already claimed. Since 2026-10-19 the
scorecard reads them on every pass (`scorecardFillCosts`,
decision-scorecard.md "Slippage"): every execution id of every ledger row
first, then for the rows without one the unclaimed retained fill on the same
contract within five minutes of the statement's execution time, which the
parser places in the statement's zone (US Eastern) so the two instants meet;
each decision carries the half-spread paid and each stream its sum, and
`no_fill_quote` is kept only for fills before 2026-10-18.

## What it cannot know

- Fills before this change have no quote and never will; the store starts
  empty and `reqExecutions` returns only the current day.
- A quote read seconds after a fill is not the quote at the fill. The gap is
  recorded on every row and the report prints it; nothing is back-dated.
- Whether TWS withholds other clients' executions from a non-master client
  is proven only by seeing one; until then the verdict is `unproven`.
- The quote is the displayed NBBO-style top of book from IBKR's feed, not
  the venue book the fill hit; half-spread against mid is a measure of
  spread cost, not of total implementation shortfall.
- Commission reports ride along with every execution; the daemon ignores
  them (message 59 is treated as benign), so commissions are not in this
  report.
- The reqExecutions request frame is sent in the legacy encoding, as
  `reqAllOpenOrders` is; its acceptance by the live TWS build and the
  encoding of the answer (legacy or protobuf, both parsed) are to be
  witnessed on the live daemon after install.

Account scope comes from the concrete configured or broker-confirmed account.
Paper/live is the broker account identity, independent of order-entry control.
Legacy fill rows are projected using their stored account identity, preserving
the original observations. Disabling trading never removes historical fills.
