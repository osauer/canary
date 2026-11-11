# Canary with an Interactive Brokers Hong Kong or Singapore account

Updated: 2026-11-07 18:42 CET

Canary reads one Interactive Brokers account through your own IB Gateway or
TWS session. Which IBKR entity holds the account, Interactive Brokers Hong
Kong, Singapore, Japan, Australia, India, LLC, Ireland or the UK, changes
nothing in that connection: the API, the ports and the client ID are the same
everywhere, so the install steps are the ones in [Install and first
run](install.md). The daemon cannot tell the entity from the gateway and does
not need to. This page covers what is different when the account's base
currency is Hong Kong dollars, Singapore dollars or yen and the shares sit on
SEHK, SGX, the Tokyo Stock Exchange or the ASX.

## Gateway steps: identical

- Install IB Gateway 10.37 or newer, or TWS, log in to the account and enable
  API socket access under Global Configuration → API → Settings. Paper and
  live both work. The login region of the gateway does not matter.
- Install Canary with one of the paths in [Pick an install
  path](install.md#pick-an-install-path) and run `canary status`. Discovery
  probes ports 4001, 4002, 7496 and 7497 on the local machine, as everywhere.
- An IBKR Pro account is needed; IBKR Lite has no API access. Market data for
  Hong Kong, Singapore and Tokyo listings is live only where your account has
  that exchange's subscription; otherwise Canary shows the broker's delayed
  data and labels it, as [Market data](../understand/market-data.md) explains.

## Base currency: shown as the broker reports it

Every account figure is rendered in the account's base currency, with the
currency IBKR names, and every position row carries its own currency. The
figures below are synthetic.

```text
Account  U1234567 · base=HKD · live
────────────────────────────────────────────
  Net liquidation         HKD 1,842,310.55
  Daily P&L               HKD -4,120.00
```

```text
  LEG                           QTY         AVG        MARK  CHANGE / GREEKS             MKT VALUE     UNREAL P&L
  700
  Stock                      100 sh  HKD 352.60  HKD 401.80  +HKD 380.00 (+0.95%)     HKD 40,180.00   HKD 4,920.00
  D05
  Stock                     1000 sh   SGD 41.10   SGD 43.75  -SGD 250.00 (-0.57%)    SGD 43,750.00   SGD 2,650.00
  7203
  Stock                      300 sh     ¥ 2,810     ¥ 2,955  -¥ 9,000 (-1.01%)           ¥ 886,500       ¥ 43,500
```

IBKR's symbols drop the leading zeros of a Hong Kong stock code, so 00700
(0700.HK) appears as `700`.

Hong Kong and Singapore dollars are written with their ISO code, yen with its
sign and never with minor units: a yen-base brief reads `session P&L ok
¥ 15,000,000 · day -¥ 82,500`. The paired app formats money with your device's
locale, so the same HKD figure may read `HK$1,842,310.55` there. Values in
another currency than the base are converted at the exchange rates the
broker's ledger reports, in `market_value_base` and the account's currency
exposure rows.

## Looking up a Hong Kong or Singapore share

Name the market when you ask about a share. Canary treats a bare symbol as a US
stock unless your account already holds that share. If you don't hold it,
`canary technical 700` asks for a US stock called 700, which Interactive Brokers
does not have, and a code that is also a US ticker returns the US company.

```sh
canary technical 700,5,1299,9988 --market hk
canary technical D05,Z74,ES3 --market sg
canary market --symbol 700 --exchange SEHK --currency HKD --range 1M
canary market --symbol D05 --exchange SGX --currency SGD --range 1M
```

On `canary technical`, `--market hk` is the same as `--exchange SEHK --currency
HKD`, and `--market sg` the same as `--exchange SGX --currency SGD`. `canary
market` takes the long form.

SMART, Interactive Brokers' automatic router, refused every SEHK and SGX listing
we tried, so `--exchange SMART` fails with the error `no security definition`.
Canary reads these shares on SEHK or SGX directly. `--exchange SEHK --currency
USD` fails the same way, because 700 is not listed in US dollars. Tokyo, Sydney and European listings can be read
through SMART.

Relative strength, how a share has moved against a benchmark, compares the
share's return in its own currency with SPY's in US dollars unless you set
`--benchmark`, so exchange-rate moves are part of the figure.

## Orders

An order for an SEHK or SGX share goes to its own exchange, because Interactive
Brokers does not route these listings through SMART. The preview names the exchange
and is judged on that market's hours, lunch break included. Interactive Brokers'
Adaptive algorithm runs only on SMART, so these orders go as plain limits: a
reduction says so in its terms, and asking for Adaptive by hand is refused with the
reason. Interactive Brokers decides the rest, such as board lots and price steps, in
its answer to the preview. Read that answer before you confirm.

Canary checks a cross-currency order against your per-order limit with a live quote
from IDEALPRO, Interactive Brokers' currency market. HKD has a pair with EUR, GBP,
AUD, USD, CAD, JPY and SGD, and SGD with EUR, GBP, AUD, USD, JPY and HKD. Interactive
Brokers lists none for HKD or SGD against NZD or CHF, or for SGD against CAD, so an
order that needs one of those is refused and the preview names the reason. Canary
reads these pairs to check an order. It does not convert HKD or SGD for you: the
currency conversions in [Cash](../operate/cash.md) cover EUR, GBP, AUD, NZD, USD, CAD,
CHF and JPY.

## Exchange calendars and the brief's session

`canary calendar` carries the official session calendars Canary embeds, each
with its lunch break, half days and coverage window:

| `--market` | Exchange | Local session | Coverage |
| --- | --- | --- | --- |
| `hk` | Hong Kong cash equities | 09:30–12:00, 13:00–16:00 HKT | 2026–2027 |
| `sg` | Singapore securities | 09:00–12:00, 13:00–17:00 SGT | 2026–2027 |
| `jp` | Tokyo cash equities | 09:00–11:30, 12:30–15:30 JST | 2026–2027 |
| `au` | ASX cash equities | 10:00–16:00 Sydney | 2026–2027 |
| `cn` | Mainland China A-share sessions | 09:30–11:30, 13:00–15:00 CST | 2026 |

```sh
canary calendar --market hk --days 5
canary calendar --market sg --date 2026-02-16
```

Hong Kong no longer closes for typhoons. Since 23 September 2024 HKEX has traded
through Typhoon Signal No. 8 or above, Black Rainstorm warnings and extreme
conditions, so the calendar shows those days as regular sessions.

A date outside the coverage window reads `unknown`, never a guess from the
weekday. [Market calendars](../understand/concepts.md#market-calendars) lists
every calendar Canary embeds, with the exact date each one's coverage ends.
A-shares reach an IBKR account northbound through Stock Connect,
which trades only on days both Hong Kong and the mainland are open; check the
HKEX Stock Connect calendar before relying on a date near a holiday.

The brief's session row follows your book. The daemon reads the session of
the exchange holding the largest share of your stock and option value, so an
account of SEHK and SGX shares reads `session ok hk_hkex · regular` at a Hong
Kong morning, and the words `since the last close` and `off-session marks`
refer to that exchange. A book mostly on US venues keeps the US session, as
before.

## The brief at an Asian morning

Run the brief before your exchange opens, as [The daily
desk](../operate/daily-desk.md) does before the US open:

| Exchange | Open (local) | Run the brief by |
| --- | --- | --- |
| SGX | 09:00 SGT | 08:45 SGT |
| Hong Kong | 09:30 HKT | 09:15 HKT |
| Tokyo | 09:00 JST | 08:45 JST |
| ASX | 10:00 AEDT or AEST | 09:45 Sydney |

At that hour the US session closed a few hours earlier, so the rows that
describe the US stay US rows: the regime read (credit spreads, volatility and
rates from US sources), S&P 500 breadth and SPX dealer gamma are context about
the market that just closed, not the state of your exchange. Rules that need
an earnings date read `unknown` for a listing the earnings providers do not
cover, never `pass`. Portfolio stress, concentration, protection coverage and
the rulebook's size and exposure rules work on any listing, in your base
currency.

The gateway's overnight diagnostics follow the same calendars. By default a
lost gateway connection counts as a warning during the US, Xetra, London,
Tokyo and Hong Kong sessions; a position on SGX, the ASX or Stock Connect
widens those hours on its own, and `[daemon].log_markets` in the
[configuration reference](../reference/config.md) declares them outright:

```toml
[daemon]
log_markets = ["hk_hkex", "sg_sgx"]
```

## Statements and reconciliation

The Activity Flex Query in [Set up broker reporting](reporting.md) is the
same for every IBKR entity; reconciliation, statement-derived equity and
Canary Edge read it in your base currency. The query's sections and fields do
not depend on the account's region.

## Using it with an AI host

`canary mcp` works the same on these accounts, with any MCP host on your machine: see [Connect your account to the LLM you already use](your-llm.md) and [Agentic trading](agentic-trading.md).

Claude may not be offered where you are: in November 2026 Anthropic's [list of supported regions](https://www.anthropic.com/supported-countries) did not include Hong Kong, Macau or mainland China. The CLI and paired app work without it, and other MCP hosts can use the same tools with a model that is available where you are.

## Next

[Your first session](first-session.md) walks through the reads; [The daily
desk](../operate/daily-desk.md) orders them across a trading day. When a word
or figure reads wrong for your market, open an issue on GitHub and name the
exchange and the base currency; that is how the calendars and the renderers
get better.
