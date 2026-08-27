# Packaging and distribution

Updated: 2026-08-10 08:25 CEST

Canary ships as a Claude Code plugin, a Claude Desktop `.mcpb` bundle, and a
separately installed binary. This page covers what each package contains and
what you can check before trusting a download.

Canary is a local interface to Interactive Brokers account, portfolio, market,
and risk context for individual traders who already run IB Gateway or TWS.
Available data and freshness depend on the current session and the account's
market-data subscriptions. Regime, stress, and rulebook checks stay advisory,
and risk policy plus statement reconciliation stay operator-owned in the
terminal.

The description used in listings:

> A local CLI and MCP server for Interactive Brokers account, portfolio, market, and risk context through IB Gateway or TWS. Available data and freshness depend on the current session and the account's market-data subscriptions.

## Requirements and boundaries

- Requires IB Gateway 10.37+ or TWS running locally.
- Requires an IBKR Pro account; IBKR Lite does not include TWS API access.
- The bundled path is a macOS or Linux binary.
- The plugin/skill does not ship the binary; users install `canary` separately.
- The MCP server exposes only the canonical read-only tool registry, with no resources, previews,
  settings writes, or broker actions. Standard binaries compile out broker
  writes. The opt-in trading binary retains only constrained CLI/app order
  actions, still bound by fresh confirmation/preflight and daemon safety gates.
- Trading builds ship on every release as a separately named artifact. They are experimental, as-is, and outside the stable read-only channel. Do not promote them through MCP marketplaces until the execution, approval, and safety metadata are reviewed for that channel.
- Data returned by MCP tools can include account-sensitive balances, positions, and P&L.

[Privacy](../../../PRIVACY.md) covers data locality, local files, and the
third-party host caveat. [Security](../../../SECURITY.md) covers the read-only
threat model, release integrity, and diagnostic data sensitivity. Security
reports go through GitHub Private Vulnerability Reporting, everything else
through GitHub issues.

## Claude Code plugin

The marketplace source is `claude-plugin/`, which bundles the canonical skill,
hooks, and plugin-local `.mcp.json` for `canary mcp`.

- `.claude-plugin/plugin.json` describes the plugin.
- `claude-plugin/.mcp.json` declares the Claude Code plugin MCP server.
- `.claude-plugin/marketplace.json` exposes the self-hosted marketplace.
- `skills/canary/SKILL.md` teaches Claude the retained desk-first workflow.
- `hooks/hooks.json` allows order-status and proposal/opportunity preview reads, blocks broker-write Bash calls, and starts the install/version warning hook.
- `settings/canary.settings.json` is the optional global allow/deny template.

## Claude Desktop MCPB

Anthropic's public distribution surface for `.mcpb` files is the Claude
Connectors Directory, not the open MCP Registry. The open registry at
`registry.modelcontextprotocol.io` is useful metadata distribution, but
Anthropic's docs say registry publication does not surface a connector in Claude
products; directory submission is a separate review process.

Current package:

- GitHub release asset: `canary-vX.Y.Z.mcpb`, plus stable `canary.mcpb`.
- MCP Registry metadata: generated to `dist/server.json` with `registryType: "mcpb"` and `fileSha256`.
- Release integrity: both MCPB assets are listed in signed `SHA256SUMS`; the bundle itself is not code-signed unless `mcpb verify dist/canary-vX.Y.Z.mcpb` succeeds.

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

- Name the channel and assets distinctly, for example `canary-trading`, so users do not confuse it with the stable read-only Canary build.
- Mark the build experimental and provided as-is in README, release notes, download pages, and any CLI status surface that can report the channel.
- Keep trading config inactive as `config.toml.trading`. Activation requires removing the `.trading` suffix, verifying pinned account and endpoint fields, and restarting the daemon.
- Publish paper-trading capability before live trading. Live trading needs its own explicit approval, audit, and rollback story.
- Keep MCP broker writes out of the preview until there is a separate human-confirmation and nonce model for MCP.
- Tighten updater asset matching before attaching experimental tarballs to any release that stable `canary update` can see.
- Run a trading-tag test and smoke matrix separate from the stable read-only release smoke.
