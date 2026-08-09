# Market data and entitlements

Broker market data — quotes, option chains, Greeks, historical bars — arrives
through your own TWS or IB Gateway session. Nothing is resold or proxied. If a
symbol is real-time in TWS it is real-time here, and if TWS shows it delayed,
so does `canary`. Market data is real-time wherever your IBKR market-data
subscriptions cover it, delayed where they don't.

That has a useful consequence: no configuration in this project can turn
delayed data into real-time data. The only place to change what you receive is
IBKR's account management. What the project can do is tell you, per row, which
kind of data you are looking at, and refuse to compute things that the data
cannot support.

A short list of regime inputs is the exception: they come from the institution
that publishes them, not from your broker session.

## Series the daemon reads from the publisher

These are official daily series fetched over HTTPS, straight from the source
that computes them:

| Series | Publisher | Feeds |
| --- | --- | --- |
| VVIX daily close | Cboe | The vol-of-vol row. |
| VIX3M daily close | Cboe | The off-window VIX3M leg of the VIX term-structure row, and the cross-check against the broker's own reading. |
| ICE BofA high-yield and investment-grade OAS | FRED / St. Louis Fed | The cash-credit row. |
| 90-day AA financial commercial paper, 13-week T-bill | Federal Reserve, US Treasury | The funding-spread row. |

Three things follow. No IBKR entitlement is involved, so these rows keep
working on an account with no market-data subscription at all. They fail
independently of the gateway: a broker outage leaves them current, and a
publisher outage leaves them stale while your quotes stay live. And they are
daily closes, so their age is counted in sessions rather than seconds —
[Sensors](sensors.md) has that vocabulary.

VIX3M is the one row where both kinds of data meet. In session the broker's
live tick is the source, because Cboe publishes closes and not intraday values.
Once the publication window shuts, the served value is Cboe's dated close and
the broker's own reading is compared against it.

That comparison is there for a specific failure. A gateway that keeps answering
with a stale value looks exactly like a quiet market: in frozen mode it
re-sends its last known price on request, so the tick arrives new however old
the value is, and an index carries no trade timestamp to date it with. A lapsed
market-data entitlement fails this way rather than going silent. The published
close is what settles it, and
[Regime dashboard](../internals/regime-dashboard.md#two-independent-vix3m-sources)
has the verdicts and what each one does to the row.

## What the daemon asks for

On every successful connect the daemon sets market-data type 2, frozen-aware.
The gateway then returns live ticks for symbols your subscriptions cover and
the last-known price when it cannot. Type 1, pure live, can leave snapshot
requests hanging when the market is closed, so it is not used.

Dealer gamma has one narrow regular-hours exception. If the gateway rejects an
SPY or SPX spot request with IBKR 354, Canary retries that underlying once under
type 3. An entitled request still comes back live; an unentitled one may come
back 15–20 minutes delayed. The retry is bounded to the failed gamma phase and
the connection returns to type 2 afterwards. Nothing learned about the account
is stored as an entitlement.

Entitlements are never stored. The daemon reports the data type it observed on
each request instead, which is why a `DATA` column appears beside a quote and
why the same symbol can report differently in two sessions.

## What each type is good enough for

| `DATA` | Decisions it supports |
| --- | --- |
| `live` | Anything price-sensitive: a limit price, a spread you are about to cross, an option Greek. |
| `frozen` | Position accounting and after-hours review. It is the last recorded quote with no further updates, and it arrives as a single snapshot, never a stream. |
| `delayed` | Direction and rough level. Not a limit price: the number is 15 to 20 minutes old, which is several lifetimes for a short-dated option. Dealer gamma can use it as a coarse daily regime input only when both spot and every option model tick are delayed together. |
| `delayed-frozen` | Orientation only. It is yesterday's close. |

Two more values appear on some surfaces. `prev_close` marks a price taken from
a prior close rather than the current session, and `closed` replaces the data
type on option chains outside option regular trading hours, with the real feed
state moved to a `feed_type` field beside it. An empty data type means the
gateway has not sent its notice yet, which happens for a few hundred
milliseconds after a fresh subscription; it is treated as live.

Some computations refuse delayed input rather than producing a plausible wrong
answer. Dealer gamma accepts `live`, `frozen`, and clock-aligned `delayed`
inputs. It never mixes a delayed spot with live, frozen, or untyped option
prices: a delayed run must receive IBKR's delayed model-computation tick for
every priced leg. `delayed-frozen` is still refused.

## Why one symbol says delayed

There are two distinct causes and they need different fixes.

**No subscription for that instrument class.** IBKR answers with error 354,
"Requested market data is not subscribed". That rejection is terminal for the
subscription. Ordinary quote paths do not retry it in delayed mode: the key is
suppressed for 30 minutes so a poller stops hammering a dead name, and a
gateway reconnect re-arms it immediately. The regular-hours dealer-gamma phase
is the narrow exception described above; it rearms only its rejected
underlying for one clock-aligned delayed attempt. Options are a separate entitlement
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
| Position marks and quote sensors | Streaming or snapshot market data for that instrument. |
| Option and exercise sensors | Option market data for the class, typically OPRA. Contract details alone need less. |
| Gamma engine | Option market data for SPX and SPY. After an RTH IBKR 354 it can retry once on delayed spot plus delayed option-model ticks; mixed clocks and delayed-frozen data are refused. |
| Breadth engine and `canary technical` | Historical daily bars. No streaming-quote entitlement is involved. |
| Calendar engine | Nothing. It makes no broker call at all. |

Live quotes also consume subscription slots, which retail accounts cap at
around a hundred concurrent. That is why a wide chain serializes rather than
firing every request at once.

## What works with no market-data subscription at all

More than you would expect. Account and position data is broker account state,
not market data, so balances, holdings, cost basis, and order history are
unaffected. A position whose quote is missing stays account truth; the daemon
marks the missing quote as the expected state rather than a data-quality
defect.

The published series above need no broker session at all, so the vol-of-vol,
cash-credit, and funding rows are unaffected. Daily bars are a separate
entitlement path from streaming quotes, so `canary technical` and the S&P 500
breadth engine keep working.
Breadth is computed from constituent daily bars precisely because the index
itself is not redistributed on retail subscriptions. Its first build is slow
for pacing reasons covered in
[Troubleshooting](../start/troubleshooting.md).

Market calendars are embedded in the binary and read no broker data, so the
brief and app retain session context even with the gateway down.

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
