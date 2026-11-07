# Canary with moomoo and Futu: experimental support

Updated: 2026-11-07 09:14 CET

Canary is a local tool that reads a brokerage account and reports on its positions
and risk. It normally works with Interactive Brokers. It can also read a moomoo or
Futu account through OpenD, the gateway program moomoo and Futu publish.

On OpenD it only reads. It cannot place, change or cancel an order, and it refuses
if asked. Canary has never read holdings from a real account, so check what it shows against
your broker.

## Set it up

1. Install OpenD yourself, log in, and complete its questionnaire and agreement.
   Canary doesn't include OpenD and never handles your password.
2. Add this to `~/.config/ibkr/config.toml`:

   ```toml
   [broker]
   kind = "opend"
   ```

3. Run `canary restart`, then `canary status`. With one real account on your login,
   Canary uses it. With several, it picks none and lists them, and you set `account`
   under `[opend]` to its id. A simulated account is only used if you set `account`
   to its id.

Canary's background process, its daemon, serves one broker. To use OpenD beside
Interactive Brokers, start a second daemon with its own `XDG_CONFIG_HOME`, `XDG_STATE_HOME`,
`XDG_CACHE_HOME`, `XDG_RUNTIME_DIR` and `CANARY_SOCKET`.

## What you get

- Account balances and stock and ETF positions, and Hong Kong and US quotes where
  your login holds the market-data permission, which OpenD calls a quote right.
- `canary status` lists your quote rights by market. A refused quote names the
  missing right, and a holding without a quote shows the price OpenD reports with
  the holding, not a market quote.
- Options, futures and anything else appear in a separate list at the value OpenD
  reports, not converted to the base currency. Canary's exposure figures and checks
  against your limits leave them out, so those figures don't cover the whole account.
- Holdings outside the account's base currency are converted at a rate worked out
  from OpenD's own account totals, not a quoted rate. It may differ from the market
  rate and has not been compared with a real one. Where Canary can't work out a rate,
  they count as not measured, and the checks against your limits say they can't
  judge them.

## What you don't get

Placing orders, daily P&L, the market regime read, portfolio stress, Edge (the review
of past decisions), statement reporting and reconciliation, the market overview, the
market tape and technical analysis, among others. `canary status` lists most of them.

## How far it has been tested

"Simulator" means a stand-in for OpenD written for Canary's tests.

| Area | Tested against |
| --- | --- |
| Login, finding the account with none set, quote rights | A real login |
| Hong Kong and US quotes | A real login |
| A refused quote naming the missing right | A real login |
| Quotes from any other market | Not tested with a quote right |
| Reading balances, holdings, order and fill history | Simulator only |
| Currency conversion | Simulator only; never compared with a real rate |
| Futu accounts, and moomoo accounts in Singapore and Hong Kong | Simulator only |

Canary isn't affiliated with or endorsed by moomoo or Futu.
