# Your first session

Updated: 2026-07-25 20:23 CEST

Seven steps, roughly half an hour, nothing that can reach a broker write. This assumes a working install; [Install and first run](install.md) covers getting there.

The terminal output below comes from the repository's fixture renderer (`go run cmd/_preview/main.go`), which draws the real CLI screens from synthetic data on the placeholder account `DU0000000`. Your figures will differ. The shape of each screen will not.

## 1. Prove the gateway is reachable

```sh
canary status
```

```
IBKR Gateway  READY

Session        DU0000000 via 127.0.0.1:4001 (tls=false, discovered), client 17
Market data    Live
Daemon         v1.0.0, up 30m42s
TWS            API server 178
SPX members    cache:2026-05-22, 503 names
```

The word beside `IBKR Gateway` is a verdict, and there are four: `READY`, `STARTING` (the API handshake is still in flight), `ATTENTION` (connected, but something below wants a look), and `OFFLINE`.

Read the `Session` row next. The port names which IBKR app you actually reached: 4001 is IB Gateway live, 4002 Gateway paper, 7496 TWS live, 7497 TWS paper. `discovered` means a TCP probe found it, `pinned` means your config chose it. On a machine running both Gateway and TWS this row is the difference between paper and live money.

Skip `SPX members` on a first run. It reports the constituent list behind the breadth indicator and says nothing about your connection.

`canary status` exits 1 when the gateway is not connected, so it works as a guard in a script. Anything other than `READY` is covered in [Troubleshooting](troubleshooting.md).

## 2. Read the account

```sh
canary account
```

You get net liquidation and daily P&L on top, then balances, session P&L, margin, look-ahead margin, and the total P&L that IBKR's `reqPnL` stream reports. A multi-currency account gets a currency-exposure table underneath with the FX rate used for each leg.

One figure carries into everything else: net liquidation. `canary size` reads it from this same account snapshot and expresses risk as a percentage of it, so an NLV that looks wrong makes every later sizing wrong in the same direction.

Leave the look-ahead margin block alone for now. It answers what happens at the next margin cycle, which matters when you are near a limit and not before.

## 3. Read the book as risk

```sh
canary positions --by underlying
```

`--by underlying` puts each stock together with the options written on it, so a covered call sits under its shares instead of in a separate table. The block under the groups is the part worth your time:

```
Summary
  Effective delta               +1,847.0  share-equivalents
  Dollar delta                326,584.50  USD
  Daily theta                     -42.18  USD / day
  Gamma                         +12.4000
  Vega                         +1,205.00  / 1 vol pt
  Greeks coverage                  5 / 5  legs  ✓
  FX sensitivity                 -854.32  EUR per +1% FX
```

Effective delta is signed share-equivalent exposure: a stock contributes its quantity, an option contributes delta times contracts times multiplier. Daily theta is the P&L that time decay produces over a day if nothing else moves.

Check `Greeks coverage` before you trust the Greek rollups above it. It counts the option legs whose Greeks arrived over the total, and anything short of full coverage means those sums were computed from part of the book.

## 4. Quote a symbol

```sh
canary quote SPY
```

The fixture renders three symbols at once so the freshness cases sit side by side:

```
  SYMBOL            BID  BID_SZ         ASK  ASK_SZ       PRICE  PREV CLOSE       CHG      CHG%  VOLUME      ADV$20       IV  DATA
──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  AAPL           207.82  240         207.88  510         207.85      206.40     +1.45    +0.70%  38M              —    24.7%  live
  NVDA                —  —                —  —           128.54           —         —         —  —                —        —  live
  SPY            461.04  —           461.20  —           461.12      463.50     -2.38    -0.51%  8.1M             —        —  delayed
```

The `DATA` column is the one to read. Market data is real-time wherever your IBKR market-data subscriptions cover it and delayed where they don't, and this column tells you which one you got for that symbol. `delayed` is normal for an unsubscribed name and means the prices are 15 to 20 minutes old. Out of hours you will see `frozen` or `delayed-frozen` instead, which is the last recorded quote with no further updates coming.

A dash in the NVDA row marks a field that never arrived. The daemon leaves an absent tick absent rather than substituting a zero for it.

## 5. Let one command assemble the rest

```sh
canary brief
```

`canary brief` is the assembled read. `Review` covers the last completed session: session P&L, per-underlying attribution, rules delta, proposals offered and acted on, overrides used, capital events, reconcile state, and working orders. `Ready` covers today: regime stage, breadth, dealer gamma, stress action, session state, market-event counts, and capital tier.

Run from a terminal it also stamps the day's brief artefact when one is due, and prints the stamp line. `canary brief --json` renders without stamping, and an agent-origin invocation prints `agent-origin render — not stamped`. There is no MCP tool for `brief`; it is a CLI surface.

## 6. Read the market context

```sh
canary regime
canary stress
```

`canary regime` returns the eight-row stress dashboard plus a lifecycle stage. `canary stress` asks the narrower question of whether today's market weather matters for the portfolio you are actually holding, and answers with an action and the evidence behind it.

Two of the regime rows are heavy computes and will not be ready on a cold daemon. Gamma reports `status: "computing"` with an ETA and progress, and the first-ever breadth build is paced by IBKR's historical-data limits into about 74 minutes of wall clock. Neither is broken; both fill in.

What the output means is owned elsewhere, and those pages go deeper than this one should. [Concepts → Regime](../understand/concepts.md#regime) and [Concepts → Stress](../understand/concepts.md#stress) give the mental model. [Sensors](../understand/sensors.md) covers authority, freshness, and the safe check for each one.

## 7. Ask the same questions through an agent

With an MCP host wired up, the same reads happen as tool calls. Ask the question rather than naming the tool:

| Ask | Tool the host calls |
| --- | --- |
| "How does my account look right now?" | `canary_account`, then `canary_positions` |
| "Is SPY quoting live or delayed?" | `canary_quote` |
| "How does the market regime look?" | `canary_regime` |

The answers are the same data with the interpretation written out, which is most of the value on the regime and stress surfaces. [Working with agents](../operate/agents.md) has the setup commands and a longer list of worked examples.

Two boundaries are worth knowing before you start asking. The bundled MCP surface has no order-entry tools, so no host can place, modify, or cancel an order through it. And several CLI surfaces have no MCP tool at all, `brief` among them, along with the advisory `canary policy` and `canary recon`.

## Where to go next

- [Order previews and the trading build](../operate/orders.md) for what a preview does, and what the read-only default means.
- [Troubleshooting](troubleshooting.md) when a command stops answering.
- [Updating](updating.md) for the binary, the Desktop bundle, and the S&P 500 list.
