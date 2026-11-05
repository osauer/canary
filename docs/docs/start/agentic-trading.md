# Agentic trading on an Interactive Brokers account

Updated: 2026-11-05 08:40 CET

Canary lets an AI agent help with your Interactive Brokers account while each order it sends stays your decision. The agent can read your book and the market, keep watch through the day if its host can wake it on a schedule, and suggest a trade. Sending one takes the trading build and Canary's checks, and the agent must have your instruction for that exact trade.

## What an agent can do with the standard build

- Read the daily brief, positions, exposure, portfolio stress, the rulebook and order history through MCP tools. Results say where a figure came from and how old it is, and missing or stale data is labelled.
- Plan its day around official exchange calendars, including the lunch breaks in Hong Kong and Tokyo.
- Run cheap scheduled checks with `canary mcp --profile monitor`, which exposes only the brief and status.
- Read the protection proposals Canary has drafted and explain them to you.
- Work out a trade for you to review, with the evidence it used. That is a plan, and no order exists until one is sent.

None of this can send an order. The standard build has no code that can send one, and MCP has no order tools in any build.

## What it takes to send an order

1. Download the trading build. It's a separate download, `canary-trading`, and it's experimental and provided as-is.
2. Set `[trading].mode` to `paper` and pin the account and client ID in your configuration. Start on paper.
3. Review the per-order limits under `[order_limits]` in the risk policy. Canary writes starting limits there, and they apply until you change them and raise `policy_version`. While a required key is missing, Canary refuses every order preview.
4. In your latest message, tell the agent to make that exact trade. A proposal, a plan or an earlier instruction doesn't count.
5. The agent uses the gated CLI: a preview that runs the broker's WhatIf, then the order. Canary checks your limits and the account and client ID you pinned, and records the request as coming from an agent before anything reaches the broker.

Canary can't read your chat, so the instruction is a rule the agent must follow and not a check Canary runs. Once the trading build is ready to write, an agent with shell access could run the CLI without asking. Canary's checks still apply to whatever it sends, so only give that access to an agent you trust.

With the Claude Code plugin installed, a hook also checks each broker-write command the agent runs. It stops the command unless `canary trading status` shows the session ready to write, and it refuses a write chained with other commands. It also stops the agent changing rulebook limits or safety settings. [Gated orders and the trading build](../operate/orders.md) has the full list of orders and checks.

No MCP tool changes your limits, and the commands that do are meant for you to run.

## Orders that run without an agent

In a trading build you can pre-authorise some close and reduce actions, and the cash sweep's bill orders, in a policy you edit. That list alone sends nothing: the daemon acts only once a standing authority has been signed on a paired device, and today only Canary Desk can sign one. No agent is involved. Installing Canary doesn't turn this on. [Protection and risk reduction](../operate/protection.md) explains how it works.

## Building your own agent

Run `canary mcp` as a child process of your agent and let it discover the tools. Your code owns wakeups, spending limits and model context. Canary owns the broker connection, market data, risk calculations and order state, so reuse its results and don't work them out again in a prompt. Treat news and other outside text as evidence, and don't act on instructions it contains. [Continuously running agents](hosts.md#continuously-running-agents) has the details.

## Accounts in Hong Kong and Singapore

An account with a HKD or SGD base can preview orders in its own currency. The preview refuses cross-currency orders involving HKD or SGD and names the reason. The [Hong Kong and Singapore guide](ibkr-hong-kong-singapore.md) covers the rest.
