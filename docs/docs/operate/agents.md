# Working with agents

Updated: 2026-11-07 07:52 CET

`canary mcp` is a read-only adapter for local AI clients. It reads the same
daemon results as the CLI and the paired app. It has no streaming resources,
and it cannot preview, place, modify, cancel or exercise an order.

For exact parameters, use the generated [MCP tools reference](../reference/mcp-tools.md).

## Setup

The Claude Code plugin carries the Canary skill, safety hooks, and MCP server
configuration. Install the `canary` binary separately, then verify the host:

```sh
claude plugin details canary@canary
claude mcp list
canary status
```

Fully quit and relaunch the host after updating Canary. MCP hosts keep their
stdio child alive across chats; `canary restart` restarts the shared daemon,
not that child process.

## Start with the brief

For “what matters today?” or “what changed since the close?”, use
`canary_brief`. It is the same combined post-trade and pre-trade report used by
the CLI and paired app. It includes current market-risk posture, portfolio and
PnL context, rulebook adherence, actions, and explicit missing or stale inputs.
The MCP read never stamps or acknowledges the report.

Canary owns the severity order, so an agent or app never sorts the brief itself.
The brief's pre-trade rows (`ready.*`) say whether the book is ready for a new
trade: `ready.capital` is the drawdown tier of the risk policy, and
`ready.latch` whether the drawdown brake has latched. `attention_order` lists,
most urgent first, what needs attention across those rows and the rulebook: the
drawdown tier, then the latch, then each alert-mode rule at act (`rules.<id>`,
in the rulebook's ranking), then any other pre-trade row at attention.
`ready.ranked` orders every pre-trade row: attention first, then degraded,
unavailable, and ok.

Drill into narrower evidence only when needed. The generated
[MCP tools reference](../reference/mcp-tools.md) lists all 45 tools; by
question:

- **The book.** `canary_account` and `canary_positions` give account-scoped
  detail, `canary_account_intraday` how net liquidation moved through the
  session, `canary_portfolio` where the money sits, and `canary_strategies`
  how held option legs group into strategies.
- **Risk.** `canary_stress` returns the full portfolio-risk evidence, including
  margin, exposure, concentration, options and protection. `canary_mandate`
  measures the book's loss against the loss room of the mandate in the risk
  policy and says whether additions are refused right now. `canary_rules`
  explains the rulebook (below), and `canary_policy_check` whether the policy
  files make sense against each other and the book.
- **The market.** `canary_regime` returns the eight market indicators, cluster
  confirmation and source health. `canary_market`, `canary_market_history` and
  `canary_market_tape` read quotes, price bars and the day's tape;
  `canary_technical` analyses explicitly named stock or ETF symbols;
  `canary_calendar` reads exchange sessions and `canary_macro` economic
  releases and central-bank publications.
- **Strategies and additions.** `canary_core`, `canary_strategy` and
  `canary_strategy_one` read the targets of the rule-based strategies;
  `canary_portfolio_plan` the next portfolio action under your target bands;
  `canary_add` the permitted addition to one stock.
- **Proposals and orders.** `canary_proposals` reads close/reduce-only
  protection candidates and, when you enabled them, the cash sweep's bill buys
  and redemptions and currency levelling rows. `canary_opportunities` reads
  option-exercise candidates. `canary_orders_open`, `canary_orders_history` and
  `canary_order_status` inspect the local order lifecycle without changing it.
- **Review and reporting.** `canary_edge` reviews past decisions against the
  broker's statements, and `canary_scorecard` scores each past position change
  against the market. `canary_reporting`, `canary_reporting_performance`,
  `canary_reporting_fx`, `canary_reporting_costs` and `canary_recon_status`
  read statement setup, equity, currency effects, fill costs and
  reconciliation.
- **Research.** `canary_lending_market`, `canary_lending_screen`,
  `canary_lending_rates`, `canary_lending_fees` and
  `canary_short_interest_screen` read borrowing fees, lending income and
  FINRA short interest.
- **Health.** `canary_trading_status`, `canary_settings` and `canary_status`
  explain capability, configuration and degraded data; `canary_data_health`
  and `canary_data_check` report and check each data source;
  `canary_feedback` composes a redacted bug report link.

Reading `canary_rules`: rule 1 is the worst-case loss on one issuer with every
leg netted. Each offender carries its own `status`, so a second issuer between
the levels reads `watch` while the row reads `act`, and `issuer` lists the
legs, the hedges credited or not, and the legs that keep losing as the price
rises. Rules 16 to 18 watch and never act; never count them as an act. Rule 20
acts when a held name carries long option premium within three trading
sessions of its earnings, and stays `unknown`, never `pass`, while such a name
has no current earnings date. When `policy_status.review` is `unreviewed`, the
limits are Canary's defaults that the owner has not yet reviewed, not approved
numbers.

## Monitor profile

```sh
canary mcp --profile monitor
```

The low-token monitor profile exposes only `canary_brief` and `canary_status`.
Use the brief for the desk decision surface and status only to diagnose
connectivity or degraded inputs.

## Safety and evidence

MCP output is evidence, never broker-write authority. Missing or stale data is
not a clean result, and a cached or held market-risk value is display context
only. Multi-account responses must identify one current account and mode;
otherwise the daemon refuses account-scoped authority.

Protection and exercise candidates are discovery records. Actual submission
requires the separate gated CLI or paired-app flow, a fresh exact preview or
preflight, and an explicit transaction-specific instruction from the user.

Policy files are the owner's. An agent reads limits and the policy-file list
(`canary policy show --json` includes each file's review state and what waits
for the owner's number) but never edits a policy file or runs
`canary rules policy set` on the owner's behalf.

## Reference

- [MCP tools reference](../reference/mcp-tools.md)
- [Daily desk](daily-desk.md)
- [Orders and trading safety](orders.md)
- [Updating](../start/updating.md)
- [Model Context Protocol](https://modelcontextprotocol.io/)
