# Working with agents

Updated: 2026-08-09

`canary mcp` is a deliberately small, read-only adapter for local AI clients.
It reads the same typed daemon authority as the CLI and paired app, but it has
no streaming resources and cannot preview, place, modify, cancel, exercise, or
transmit a broker action.

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

Drill into narrower evidence only when needed:

- `canary_account` and `canary_positions` provide account-scoped detail.
- `canary_rules` explains policy adherence and unknown inputs.
- `canary_technical` analyzes explicitly named stock or ETF symbols.
- `canary_proposals` reads close/reduce-only protection candidates.
- `canary_opportunities` reads option-exercise candidates.
- `canary_orders_open`, `canary_orders_history`, and `canary_order_status`
  inspect the local order lifecycle without changing it.
- `canary_trading_status`, `canary_settings`, and `canary_status` explain
  capability, configuration, and degraded data.

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

## Reference

- [MCP tools reference](../reference/mcp-tools.md)
- [Daily desk](daily-desk.md)
- [Orders and trading safety](orders.md)
- [Updating](../start/updating.md)
- [Model Context Protocol](https://modelcontextprotocol.io/)
