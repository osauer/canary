---
name: canary-harness
description: Use the repo-local Canary MCP/CLI for Interactive Brokers account, market, rulebook, stress, proposal, opportunity, and order investigations while developing the trading harness. Read/preview first; explicit current-turn broker writes use only the gated CLI.
---

Updated: 2026-07-24 08:40 CEST

## Contract

Use the typed `canary` MCP tools for read-only account, market, rulebook, stress,
proposal, opportunity, settings, reconciliation, and order-preview/status
work. Use `canary ... --json` only when MCP is unavailable, a CLI-only diagnostic
is required, or the project needs an exact reproducible CLI artifact.

The MCP surface cannot place, modify, cancel, exercise, liquidate, or
transmit. An explicit current-turn broker-write request uses only the
agent-origin gated CLI; trading readiness, route/account/client pins, preview
tokens, broker WhatIf/eligibility, freeze state, journaling, and daemon
authorization remain binding. A minted preview token is not submit authority.
Settings/freeze changes, policy/reconciliation governance writes, and
destructive daemon maintenance remain human-only.

Preserve typed evidence exactly: nil is unavailable rather than zero; report
live/delayed/frozen data, freshness, stale reasons, session context, source
health, and warning details when material. Local order history is intent and
lifecycle evidence, not an IBKR statement or complete broker audit. Offline
opportunity research is diagnostic only, never alpha proof or trade authority.

## Canonical References

This is only the repo-development safety overlay. Detailed command selection,
schemas, and domain authority stay in:

- [command catalog](../../../skills/canary/SKILL.md)
- [response schemas](../../../skills/canary/schemas.md)
- [Rulebook design and authority](../../../internal-docs/design/trading-rulebook.md)

## Project Workflow

Read the root AGENTS.md before editing. For daemon/CLI/MCP/trading semantic
changes, use `.agents/docs/daemon-cli-trading-contract.md`. For Canary SPA
changes, use `.agents/docs/spa-authority-matrix.md`.

After daemon or CLI edits, run the required tests and smoke tier, refresh the
installed daemon, and capture redacted `canary status --json` plus one command
that exercises the change. Report every skip and first failure explicitly.
