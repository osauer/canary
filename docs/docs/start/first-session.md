# Your first session

Updated: 2026-11-07 08:23 CET

This walkthrough uses only read-only commands. It assumes Canary is installed
and a local IB Gateway or TWS session is available. [Install and first
run](install.md) covers setup. The samples under each command are short
excerpts of real output with synthetic values; your rows will differ.

## 1. Check the connection

```sh
canary status
```

The first line is Canary's overall verdict:

```text
Canary  READY

Session        DU0000000 (PAPER) via 127.0.0.1:4002 (tls=false, discovered),
               client 15
Executions     other-client fills not yet observed · required TWS Master API
               client ID: 15
```

The `Session` row names the account, whether it is paper or live, and the port
Canary found. A daemon, the background process that holds the broker
connection, starts on its own the first time a command needs it. `OFFLINE`
with a `Reason` row means the broker connection is not up; [Troubleshooting](troubleshooting.md)
explains each message. The `Executions` row says whether fills you enter in
TWS reach Canary: they do only once TWS's Master API client ID is Canary's
client ID, as [Install and first
run](install.md#check-it-worked-canary-status) explains.

A working Gateway connection does not give Canary your broker statements.
Four features read them: reconciliation, which compares the statements with
the deposits and withdrawals you declared; equity taken from the statements;
Canary Edge, a review of your past trading decisions; and FX contribution.
[Set up broker reporting](reporting.md) creates the one Activity Flex Query
they share. Then check it:

```sh
canary reporting status
```

```text
Broker reporting — action_required (flex_query_incomplete)
  local: enabled=true  query_configured=true  token_present=true  token_private=true
  broker: current  reachability=reachable  next_attempt=2026-11-06 15:37 CET
  missing: open_positions.markPrice, open_positions.costBasisMoney
  action: Add the missing fields to the Activity Flex Query, then run canary setup reporting.
```

`local` covers the query ID and token file on this machine, `broker` whether
IBKR answered, and `missing` the fields a real statement row proved absent.
The `action` line says what to change. Sections IBKR did not return, or
returned empty, are listed separately, because they cannot yet prove which
fields you selected.

## 2. Read the account and book

```sh
canary account
canary positions --by underlying
```

```text
Account  DU0000000 · base=EUR · paper · type=INDIVIDUAL
────────────────────────────────────────────
  Net liquidation         € 250,000.00
  Daily P&L                   € 500.00
```

```text
Positions by underlying

  LEG                           QTY         AVG        MARK  CHANGE / GREEKS             MKT VALUE     UNREAL P&L
  700
  Stock                      500 sh  HKD 412.40  HKD 438.60  +HKD 26.20 (+6.4%)   HKD 219,300.00  HKD 13,100.00
```

Both reads name one account and say whether it is paper or live. Each
position shows its money figures in its own currency. A value IBKR did not
send stays missing rather than reading as zero, and a login that carries
several accounts is refused rather than combined. Check the `as of` time at
the foot of the positions table and the option Greeks before relying on
aggregate exposure.

## 3. Read the daily brief

```sh
canary brief
```

```text
Daily brief · 6 Nov 14:00 CET
  Markets read constructive with one unconfirmed stress signal; the book needs
  one review before the open.

Needs review
  SYNA concentration is 42.0% of net liquidation, above rule 1's act level
    of 40%; review the position size before adding exposure.
```

The brief opens with one sentence on markets and the book. Then come the items
that need review, dated context, and coverage gaps, which are inputs Canary
could not read. A finding carried over from an earlier read keeps its own
time; it does not become current because the brief was generated now.

`canary brief --details` adds the full evidence and the state of each source.
The app offers the same overview with expandable details. `canary regime
--explain` opens all eight market indicators, their observation times,
thresholds, and source failures. `canary stress --details` opens the
portfolio-risk evidence. JSON and MCP carry the complete evidence; the short
human view changes no risk decision.

## 4. Check the rules and your limits

```sh
canary rules
canary policy show
canary recon show
```

```text
Trading rulebook · 6 Nov 14:00 CET · policy rulebook-synthetic v3 (your file)
ACT    1  Worst-case loss on one issuer  single_name_exposure
WATCH  3  Premium budget  cash_sell_only
1 act, 2 watch, 1 unknown, 0 info, 1 pass, 2 not evaluated
```

`rules` runs the trading rulebook against the book and lists the most serious
findings first: `ACT`, then `WATCH`, then `?` for a rule whose input could not
be read, which never counts as a pass.

```text
Risk constitution — 2026-11-06 14:00 CET  status active
  policy risk-constitution v1  sha256:3f1c9a0b7e2d…  (/home/you/.config/ibkr/policies/risk-policy.toml)

Capital: NOT ARMED — the policy file is missing decisions (listed below)
```

`policy show` reads your risk policy, which Canary calls the risk constitution:
the capital and loss limits you write in `risk-policy.toml`. On a new install
its capital numbers are placeholders, so it reads `NOT ARMED` and lists the
decisions still to write. `recon show` compares the broker statements Canary
has kept with the capital ledger you declared:

```text
Reconciliation — 2026-11-06 14:00 CET  status active
  equity check: statement 2026-11-05 250000.00  vs runtime at statement closing prices and FX +0.00%
```

None of these commands acknowledges, overrides, dismisses, or changes
anything.

## 5. Look at pending work

```sh
canary proposals list
canary opportunities list
canary orders open
```

```text
Protection proposals
0 actionable / 0 total · 0 queued for the open
Full risk evidence, sizing and funding: canary proposals list --details
```

Proposals are orders Canary suggests to close or reduce positions for
protection, plus the cash sweep's bill buys when you enable it. Opportunities
are options worth exercising; with none, the list reads `Canary Opportunities ·
0 actionable / 0 total` and `No opportunities in this snapshot.`

```text
Canary Open Orders  1
Scope          DU0000000 paper
  canary-20261106-113700  SELL 100 SYNA LMT 96.0000 DAY
    working  Submitted  updated 2026-11-06T11:37:00+01:00  origin canary
```

Order reads show Canary's own order record and the orders the broker reports.
None of these lists can send anything. An order needs the separate trading
build, a fresh preview of that exact order and the daemon checking it again
before it sends. It then goes on your instruction for that order, or, for a
pre-authorised row, after its veto window once you have armed a standing
authority.

## 6. Analyse a named symbol

```sh
canary technical SYNA
```

```text
Technical screen · SPY benchmark · 420-day lookback

SYNA · uptrend
Trend          50-day avg 95.00 · 200-day avg 90.00
vs benchmark   +5.0 pp / 63 days · +10.0 pp / 126 days
```

The technical read gives the trend, strength relative to SPY, the daily range
and trading volume for each stock or ETF you name. A name with incomplete
history reads `partial` or `error` rather than being ranked on what is missing.

## 7. Ask through an MCP host

The bundled MCP server is read-only. Ask the question rather than
naming a tool:

| Ask | Typical tool |
| --- | --- |
| “What needs attention today?” | `canary_brief` |
| “How does my account look?” | `canary_account`, then `canary_positions` |
| “What is my exposure per underlying?” | `canary_positions` |
| “Is broker reporting ready for Recon and Edge?” | `canary_reporting` |
| “What did my past stock, ETF, and option activity actually deliver?” | `canary_edge` |
| “Which of my past entries, adds, trims, or exits had the largest 20-session price impact?” | `canary_edge` |
| “Which rulebook inputs are unknown?” | `canary_rules` |
| “Are there protection or exercise candidates?” | `canary_proposals` or `canary_opportunities` |
| “Why is a local order still open?” | `canary_orders_open`, then `canary_order_status` |

MCP has no resource subscriptions, preview tools, settings writes, governance
writes, or broker-write tools. [Working with agents](../operate/agents.md) has
the host setup and evidence rules.

## Where to go next

- [Set up broker reporting](reporting.md) if statement-backed rows are missing
  or still need their first report.
- [The daily desk](../operate/daily-desk.md) for the recurring workflow.
- [Gated orders and the trading build](../operate/orders.md) for what an order needs
  before it can be sent.
- [Sensors](../understand/sensors.md) for data type, freshness, source health,
  last-good values, and what each warning means.
- [Updating](updating.md) for how updates stay on your installed major version.
