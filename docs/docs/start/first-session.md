# Your first session

Updated: 2026-08-09

This walkthrough uses only read-only commands. It assumes Canary is installed
and a local IB Gateway or TWS session is available. [Install and first
run](install.md) covers setup.

## 1. Prove the authority

```sh
canary status
```

Start with the connected account mode and gateway endpoint, then read storage,
subsystem, data-farm, and background-task health. An idle daemon is neutral; a
normal command starts it on demand. Missing or stale source evidence remains a
warning or unavailable state rather than becoming a clean result.

## 2. Read the account and book

```sh
canary account
canary positions --by underlying
```

Both reads identify one selected account and mode. A missing value stays
missing, and an unresolved multi-account login is refused rather than combined.
Check positions freshness and Greeks coverage before relying on aggregate
exposure.

## 3. Read the desk brief

```sh
canary brief
```

Review covers what changed since the last regular close. Ready covers the next
session: market-risk posture, portfolio fit, policy state, current protection
work, and any source that could not be read. Quotes, calendars, breadth, gamma,
regime, stress, earnings, borrow, and halt inputs are daemon-owned sensors in
v3; they feed this assembled surface rather than returning as separate public
CLI commands.

## 4. Inspect the rulebook

```sh
canary rules
canary policy show
canary recon show
```

`rules` reports the hardest advisory finding and every unknown input. `policy
show` identifies the approved risk constitution. `recon show` compares retained
broker statements with the declared capital ledger. The read commands do not
acknowledge, override, dismiss, or change any state.

## 5. Inspect current work

```sh
canary proposals list
canary opportunities list
canary orders open
```

Proposals are close/reduce-only protection candidates. Opportunities are
option-exercise candidates. Order reads inspect the local lifecycle journal.
These records are evidence, not broker-write authority; any action requires the
separate trading build, a fresh exact review contract, daemon revalidation, and
an explicit instruction for that transaction.

## 6. Analyze an explicitly named symbol

```sh
canary technical SPY,QQQ
```

The technical read batches daily trend, relative strength, ATR, and liquidity
evidence for named stocks or ETFs. It reports degraded history rather than
silently ranking an incomplete row.

## 7. Ask through an MCP host

The bundled MCP server has 13 read-only tools. Ask the question rather than
naming a tool:

| Ask | Typical tool |
| --- | --- |
| “What needs attention today?” | `canary_brief` |
| “How does my account look?” | `canary_account`, then `canary_positions` |
| “Which rulebook inputs are unknown?” | `canary_rules` |
| “Are there protection or exercise candidates?” | `canary_proposals` or `canary_opportunities` |
| “Why is a local order still open?” | `canary_orders_open`, then `canary_order_status` |

MCP has no resource subscriptions, preview tools, settings writes, governance
writes, or broker-write tools. [Working with agents](../operate/agents.md) has
the host setup and evidence rules.

## Where to go next

- [The daily desk](../operate/daily-desk.md) for the recurring workflow.
- [Orders and trading safety](../operate/orders.md) for the constrained review
  boundary.
- [Sensors](../understand/sensors.md) for data type, freshness, source health,
  last-good, and warning semantics.
- [Updating](updating.md) for stable-major update behavior.
