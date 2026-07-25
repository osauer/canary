# Packaging and distribution

Updated: 2026-07-25 20:23 CEST

`ibkr` ships as a Claude Code plugin, a Claude Desktop `.mcpb` bundle, and a
separately installed binary. This page covers what each package contains and
what you can check before trusting a download.

`ibkr` is a local platform for running an agentic trading desk on Interactive
Brokers, aimed at individual traders who already run IB Gateway or TWS. Agents
get the full market-data surface (real-time per your IBKR market-data
subscriptions) and portfolio context. Regime, stress, and rulebook checks stay
advisory, and risk policy plus statement reconciliation stay operator-owned in
the terminal.

The description used in listings:

> An agentic trading desk for Interactive Brokers: account, positions, real-time and streaming quotes (per your IBKR market-data subscriptions), official market calendars, option chains, history, technical/relative-strength screens, scans, fixed-fractional sizing, S&P 500 breadth, SPY+SPX dealer gamma, a broad-market stress-lifecycle regime dashboard, a 14-rule advisory trading rulebook, and a stateless portfolio stress read through a local CLI/MCP server connected to IB Gateway or TWS.

## Requirements and boundaries

- Requires IB Gateway 10.37+ or TWS running locally.
- Requires an IBKR Pro account; IBKR Lite does not include TWS API access.
- The bundled path is a macOS or Linux binary.
- The plugin/skill does not ship the binary; users install `ibkr` separately.
- Current bundled CLI and MCP releases expose analysis, sizing, and preview-only stock/ETF order drafts. The MCP surface has no order-entry tools at all. The CLI keeps the `ibkr order place|modify|cancel` verbs in every build, and in a standard build they fail closed: the daemon's write handlers are compiled out behind the `trading` build tag and return `ErrTradingDisabled` before anything reaches the broker.
- Trading builds ship on every release as a separately named artifact. They are experimental, as-is, and outside the stable read-only channel. Do not promote them through MCP marketplaces until the execution, approval, and safety metadata are reviewed for that channel.
- Data returned by MCP tools can include account-sensitive balances, positions, and P&L.

[Privacy](../../../PRIVACY.md) covers data locality, local files, and the
third-party host caveat. [Security](../../../SECURITY.md) covers the read-only
threat model, release integrity, and diagnostic data sensitivity. Security
reports go through GitHub Private Vulnerability Reporting, everything else
through GitHub issues.

## Claude Code plugin

The marketplace source is `claude-plugin/`, which bundles the canonical skill,
hooks, and plugin-local `.mcp.json` for `ibkr mcp`.

- `.claude-plugin/plugin.json` describes the plugin.
- `claude-plugin/.mcp.json` declares the Claude Code plugin MCP server.
- `.claude-plugin/marketplace.json` exposes the self-hosted marketplace.
- `skills/ibkr/SKILL.md` teaches Claude the current analysis, sizing, and preview-only workflow.
- `hooks/hooks.json` allows preview/status order reads, blocks broker-write Bash calls, and starts the install/version warning hook.
- `settings/ibkr.settings.json` is the optional global allow/deny template.

## Claude Desktop MCPB

Anthropic's public distribution surface for `.mcpb` files is the Claude
Connectors Directory, not the open MCP Registry. The open registry at
`registry.modelcontextprotocol.io` is useful metadata distribution, but
Anthropic's docs say registry publication does not surface a connector in Claude
products; directory submission is a separate review process.

Current package:

- GitHub release asset: `ibkr-vX.Y.Z.mcpb`, plus stable `ibkr.mcpb`.
- MCP Registry metadata: generated to `dist/server.json` with `registryType: "mcpb"` and `fileSha256`.
- Release integrity: both MCPB assets are listed in signed `SHA256SUMS`; the bundle itself is not code-signed unless `mcpb verify dist/ibkr-vX.Y.Z.mcpb` succeeds.

Two caveats before installing:

- Platform: Claude Desktop's documented primary platforms are macOS and Windows. If native Windows support remains absent, the MCPB is described as macOS-only for Claude Desktop, and Linux stays a generic MCPB/client capability only if the target directory accepts it.
- Signing: unsigned MCPBs are installable in permissive Claude Desktop configurations, but enterprise admins can require signatures. A trusted code-signing certificate or compatible signing API is needed before the bundle can be advertised as signed.

## Directory metadata

These files identify the project across MCP directories:

- `server.json` for the official MCP Registry release path.
- `.claude-plugin/plugin.json` for Claude Code plugin metadata.
- `glama.json` to identify the authorized Glama maintainer.

## Experimental trading preview

The stable marketplace story stays read-only. A trading-capable build is a
separate preview channel, not a hidden variant of the normal binary.

Minimum product rules before publishing a trading preview:

- Name the channel and assets distinctly, for example `ibkr-trading-preview`, so users do not confuse it with stable `ibkr`.
- Mark the build experimental and provided as-is in README, release notes, download pages, and any CLI status surface that can report the channel.
- Keep trading config inactive as `config.toml.trading`. Activation requires removing the `.trading` suffix, verifying pinned account and endpoint fields, and restarting the daemon.
- Publish paper-trading capability before live trading. Live trading needs its own approval, paper-smoke, audit, and rollback story.
- Keep MCP broker writes out of the preview until there is a separate human-confirmation and nonce model for MCP.
- Tighten updater asset matching before attaching experimental tarballs to any release that stable `ibkr update` can see.
- Run a trading-tag test and smoke matrix separate from the stable read-only release smoke.
