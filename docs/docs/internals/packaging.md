# Packaging and distribution

Updated: 2026-11-07 07:48 CET

Canary ships as a Claude Code plugin, a Claude Desktop `.mcpb` bundle, and a
separately installed binary. This page covers what each package contains and
what you can check before trusting a download.

Canary is a local interface to Interactive Brokers account, portfolio, market,
and risk context for individual traders who already run IB Gateway or TWS.
Available data and freshness depend on the current session and the account's
market-data subscriptions. Regime, stress, and rulebook checks stay advisory,
and risk policy plus statement reconciliation stay operator-owned in the
terminal.

The Claude Desktop bundle's manifest describes it as:

> Canary's local read-only Interactive Brokers MCP server for account, market, reporting, and retrospective decision review.

## Requirements and boundaries

- Requires IB Gateway 10.37+ or TWS running locally.
- Requires an IBKR Pro account; IBKR Lite does not include TWS API access.
- The bundled path is a macOS or Linux binary.
- The plugin/skill does not ship the binary; users install `canary` separately.
- The MCP server exposes only the canonical read-only tool registry, with no resources, previews,
  settings writes, or broker actions. Standard binaries compile out broker
  writes. The opt-in trading binary retains only constrained order actions,
  all bound by the daemon's safety gates: CLI/app actions with fresh
  confirmation or preflight, protection orders the owner pre-authorised or
  queued, and the protective stop guard, which shrinks or cancels Canary's own
  stock stops to match the position.
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
- `hooks/hooks.json` allows order-status and proposal/opportunity preview reads, lets broker-write Bash calls through only when `canary trading status` shows the session ready to write, and starts the install/version warning hook.
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

- Platform: the bundle declares macOS and Linux (`compatibility.platforms` is `darwin` and `linux`) and carries no Windows binary, so it does not serve Claude Desktop on Windows.
- Signing: unsigned MCPBs are installable in permissive Claude Desktop configurations, but enterprise admins can require signatures. A trusted code-signing certificate or compatible signing API is needed before the bundle can be advertised as signed.

## Directory metadata

These files identify the project across MCP directories:

- `server.json` for the official MCP Registry release path.
- `.claude-plugin/plugin.json` for Claude Code plugin metadata.
- `glama.json` to identify the authorized Glama maintainer.

## Trading build

The trading-capable build is a separate artifact, not a hidden variant of the
normal binary. Every release carries it beside the standard build:

- Its tarballs are named `canary-trading-vX.Y.Z-<os>-<arch>.tar.gz` and carry a `TRADING-WARNING.md` that links to the security policy and the order guide at that tag.
- `canary settings` reports its build as `experimental-trading`; the standard build reports `stable`.
- `canary update` matches the standard tarball name exactly, so it never installs a trading build and refuses to replace one.
- The trading configuration template ships inert as `examples/config.toml.trading`. It takes effect only once it is moved to `~/.config/ibkr/config.toml`, its pinned values are checked, and the daemon is restarted.
- The MCP Bundles are built from the standard binaries, and the MCP server has no broker-write tools in either build.
