# Market data and entitlements

Every price on every surface arrives through your own TWS or IB Gateway
session. Nothing is resold, proxied, or fetched from a third-party feed. If a
symbol is real-time in TWS it is real-time here, and if TWS shows it delayed,
so does `canary`. Market data is real-time wherever your IBKR market-data
subscriptions cover it, delayed where they don't.

That has a useful consequence: no configuration in this project can turn
delayed data into real-time data. The only place to change what you receive is
IBKR's account management. What the project can do is tell you, per row, which
kind of data you are looking at, and refuse to compute things that the data
cannot support.

## What the daemon asks for

On every successful connect the daemon sets market-data type 2, frozen-aware.
The gateway then returns live ticks for symbols your subscriptions cover and
the last-known price when it cannot. Type 1, pure live, can leave snapshot
requests hanging when the market is closed, so it is not used.

Entitlements are never stored. The daemon reports the data type it observed on
each request instead, which is why a `DATA` column appears beside a quote and
why the same symbol can report differently in two sessions.

## What each type is good enough for

| `DATA` | Decisions it supports |
| --- | --- |
| `live` | Anything price-sensitive: a limit price, a spread you are about to cross, an option Greek. |
| `frozen` | Position accounting and after-hours review. It is the last recorded quote with no further updates, and it arrives as a single snapshot, never a stream. |
| `delayed` | Direction and rough level. Not a limit price: the number is 15 to 20 minutes old, which is several lifetimes for a short-dated option. |
| `delayed-frozen` | Orientation only. It is yesterday's close. |

Two more values appear on some surfaces. `prev_close` marks a price taken from
a prior close rather than the current session, and `closed` replaces the data
type on option chains outside option regular trading hours, with the real feed
state moved to a `feed_type` field beside it. An empty data type means the
gateway has not sent its notice yet, which happens for a few hundred
milliseconds after a fresh subscription; it is treated as live.

Some computations refuse delayed input rather than producing a plausible wrong
answer. Dealer gamma accepts `live` and `frozen` and rejects both delayed
types, because a 15-minute-old spot biases every gamma in the chain.

## Why one symbol says delayed

There are two distinct causes and they need different fixes.

**No subscription for that instrument class.** IBKR answers with error 354,
"Requested market data is not subscribed". That rejection is terminal for the
subscription: there is no automatic retry in delayed mode. The key is
suppressed for 30 minutes so a poller stops hammering a dead name, and a
gateway reconnect re-arms it immediately. Options are a separate entitlement
from the underlying stock, which is the usual surprise: a stock quote can be
live while its chain returns nothing.

**A competing live session.** If the same login is already streaming live data
somewhere else, in TWS or a mobile app, the gateway answers with error 10197
and the connection is forced into delayed mode as a whole. Closing the other
session and reconnecting restores live data.

[Troubleshooting](../start/troubleshooting.md) has the symptom-side version of
this, including what each badge string says verbatim.

## Which surfaces need which entitlement

| Surface | What it needs |
| --- | --- |
| `canary quote`, position marks | Streaming or snapshot market data for that instrument. |
| `canary chain` | Option market data for the class, typically OPRA. The expiry list alone is contract details and needs less. |
| `canary gamma` | Option market data for SPX and SPY, and it rejects delayed input outright. |
| `canary scan` | Scanner access for the ranking, plus market data for the per-row enrichment. |
| `canary breadth`, `canary history`, `canary technical` | Historical daily bars. No streaming-quote entitlement is involved. |
| `canary calendar` | Nothing. It makes no broker call at all. |

Live quotes also consume subscription slots, which retail accounts cap at
around a hundred concurrent. That is why a wide chain or a large scan
serializes rather than firing every request at once.

## What works with no market-data subscription at all

More than you would expect. Account and position data is broker account state,
not market data, so balances, holdings, cost basis, and order history are
unaffected. A position whose quote is missing stays account truth; the daemon
marks the missing quote as the expected state rather than a data-quality
defect.

Daily bars are a separate entitlement path from streaming quotes, so
`canary history`, `canary technical`, and the S&P 500 breadth engine keep working.
Breadth is computed from constituent daily bars precisely because the index
itself is not redistributed on retail subscriptions. Its first build is slow
for pacing reasons covered in
[Troubleshooting](../start/troubleshooting.md).

Market calendars are embedded in the binary and read no broker data, so
`canary calendar` answers whether a market is open even with the gateway down.

## When a quote looks wrong, read the session

A quote attaches a `session_context` block when the calendar state is needed to
interpret it, and omits it on an ordinary live in-session row. The block names
the market, its state (`regular`, `closed`, `holiday`, `early_close`, or
`unknown`), and the next open and close.

That is usually the answer when a price looks stale. A `frozen` quote at 20:00
ET on a Tuesday is the system working correctly; the same quote at 11:00 ET is
worth investigating. [Concepts](concepts.md#market-calendars) explains why the
embedded exchange calendar, rather than the gateway's quote state, decides
whether a market is open.
