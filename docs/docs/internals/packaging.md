# Marketplace readiness

Last reviewed: 2026-07-18 21:37 CEST

This page is the maintainer checklist for presenting `ibkr` in AI tool marketplaces and app directories. It is intentionally about packaging, trust, and user expectations; feature details live in the README and reference docs.

## Positioning

`ibkr` is a local platform for running an agentic trading desk on Interactive Brokers, aimed at individual traders who already run IB Gateway or TWS: agents get the full market-data surface (real-time per the user's IBKR market-data subscriptions) and portfolio context, while regime, canary, and rulebook checks stay advisory and risk policy plus statement reconciliation stay operator-owned in the terminal. The clearest marketplace description is:

> An agentic trading desk for Interactive Brokers: account, positions, real-time and streaming quotes (per your IBKR market-data subscriptions), official market calendars, option chains, history, technical/relative-strength screens, scans, fixed-fractional sizing, S&P 500 breadth, SPY+SPX dealer gamma, a broad-market stress-lifecycle regime dashboard, a 14-rule advisory trading rulebook, and a stateless portfolio canary through a local CLI/MCP server connected to IB Gateway or TWS.

Always include these qualifiers:

- Requires IB Gateway or TWS running locally.
- Requires an IBKR Pro account; IBKR Lite does not include TWS API access.
- The plugin/skill does not ship the binary; users install `ibkr` separately.
- The Claude Code marketplace source is `claude-plugin/`, which bundles the
  canonical skill, hooks, and plugin-local `.mcp.json` for `ibkr mcp`.
- Current bundled CLI and MCP releases expose analysis, sizing, and preview-only stock/ETF order drafts, but no place/modify/cancel broker-write interface.
- Trading builds, if published, are experimental, as-is, and separate from the stable read-only release channel. Do not promote them through MCP marketplaces until the execution, approval, and safety metadata are reviewed for that channel.
- Data returned by MCP tools can include account-sensitive balances, positions, and P&L.

## Anthropic / Claude Code

Current package:

- `.claude-plugin/plugin.json` describes the plugin.
- `claude-plugin/.mcp.json` declares the Claude Code plugin MCP server.
- `.claude-plugin/marketplace.json` exposes the self-hosted marketplace.
- `skills/ibkr/SKILL.md` teaches Claude the current analysis, sizing, and preview-only workflow.
- `hooks/hooks.json` allows preview/status order reads, blocks broker-write Bash calls, and starts the install/version warning hook.
- `settings/ibkr.settings.json` is the optional global allow/deny template.

Pre-submit checks:

```sh
claude plugin validate .
make check
```

Anthropic's plugin submission guidance says plugins can bundle skills, MCP connectors, hooks, commands, and agents, and recommends clear setup guidance for MCP configuration. It also asks submitters to run plugin validation before submission. The public submission path is documented at <https://claude.com/docs/plugins/submit>.

## OpenAI / ChatGPT Apps

OpenAI's Apps SDK is MCP-based, but ChatGPT app distribution is not the same artifact as a Claude Code plugin. Treat this repo's current Claude plugin as reusable product content, not as an OpenAI-ready package.

Before submitting to OpenAI, add or decide:

- a ChatGPT app or connector packaging plan using OpenAI's current Apps SDK documentation
- how a local-only IB Gateway dependency will be reached from ChatGPT, for example a supported secure tunnel or a clear "local/developer mode only" installation story
- a privacy policy URL pointing at [PRIVACY.md](../../../PRIVACY.md)
- a safety statement that no order placement, cancellation, or modification exists
- setup text covering IB Gateway/TWS prerequisites and the local binary install
- screenshots or a short demo focused on the regime lifecycle, stateless canary, and portfolio workflows

OpenAI's Apps SDK help says app submissions are open, apps are built on MCP, and developers should prepare for safety, privacy, and functionality review, including a clear privacy policy. The current help article is at <https://help.openai.com/en/articles/12515353-build-with-the-apps-sdk>.

## Documentation That Must Stay In Sync

- README: first-run install, update path, core feature list, MCP setup, safety.
- [Agentic use](../operate/agents.md): natural-language workflows and limits.
- [MCP tools reference](../reference/mcp-tools.md): generated from `internal/mcp/tools.go`.
- [MCP resources reference](../reference/mcp-resources.md): quote resource read/subscribe behavior.
- [Configuration reference](../reference/config.md): generated from config structs and `docgen:env` comments.
- [Privacy](../../../PRIVACY.md): data locality, local files, and third-party host caveat.
- [Security](../../../SECURITY.md): read-only threat model, release integrity, diagnostic data sensitivity.

## Pre-Promotion Smoke List

Run these before marketplace announcements:

```sh
ibkr --help
ibkr mcp --help
ibkr setup --help
ibkr update --help
ibkr restart --help
ibkr calendar --help
ibkr regime --help
make docs-regen
make check
```

With a gateway available, also run:

```sh
ibkr status
ibkr account --json
ibkr positions --json
ibkr quote SPY --json
ibkr regime --json
```

For a Claude Code release, reinstall from the marketplace path and confirm the SessionStart hook is quiet when binary and plugin major.minor match.

## Experimental Trading Preview

The stable marketplace story stays read-only. A trading-capable build should be treated as a separate preview channel, not as a hidden variant of the normal binary.

Minimum product rules before publishing a trading preview:

- Name the channel and assets distinctly, for example `ibkr-trading-preview`, so users do not confuse it with stable `ibkr`.
- Mark the build experimental and provided as-is in README, release notes, download pages, and any CLI status surface that can report the channel.
- Keep trading config inactive as `config.toml.trading`. Activation requires removing the `.trading` suffix, verifying pinned account and endpoint fields, and restarting the daemon.
- Publish paper-trading capability before live trading. Live trading needs its own approval, paper-smoke, audit, and rollback story.
- Keep MCP broker writes out of the preview until there is a separate human-confirmation and nonce model for MCP.
- Tighten updater asset matching before attaching experimental tarballs to any release that stable `ibkr update` can see.
- Run a trading-tag test and smoke matrix separate from the stable read-only release smoke.

## Anthropic / Claude Desktop MCPB

Anthropic's public distribution surface for `.mcpb` files is the Claude Connectors Directory, not the open MCP Registry. The open registry at `registry.modelcontextprotocol.io` is useful metadata distribution, but Anthropic's docs say registry publication does not surface a connector in Claude products; directory submission is a separate review process.

Current package:

- GitHub release asset: `ibkr-vX.Y.Z.mcpb`, plus stable `ibkr.mcpb`.
- MCP Registry metadata: generated to `dist/server.json` with `registryType: "mcpb"` and `fileSha256`.
- Release integrity: both MCPB assets are listed in signed `SHA256SUMS`; the bundle itself is not code-signed unless `mcpb verify dist/ibkr-vX.Y.Z.mcpb` succeeds.

Local gates before submitting:

```sh
npx -y @anthropic-ai/mcpb@2.1.2 validate dist/mcpb/ibkr/manifest.json
npx -y @anthropic-ai/mcpb@2.1.2 info dist/ibkr-vX.Y.Z.mcpb
npx -y @anthropic-ai/mcpb@2.1.2 verify dist/ibkr-vX.Y.Z.mcpb
bin/mcp-publisher validate dist/server.json
make check
make smoke
```

Submission blockers to clear:

- Tool annotations: every MCP tool must expose a human-readable `title` and `readOnlyHint: true` in `tools/list`. This is required by Anthropic's review checklist and should be verified from the built binary, not just source.
- Claude Desktop custom-install smoke: install the `.mcpb` through Settings -> Extensions -> Advanced settings -> Install Extension, restart Claude Desktop, then call every tool at least once with valid parameters against a paper or live IB Gateway.
- Reviewer test path: Anthropic asks for test credentials and setup instructions. For `ibkr`, this means either a reviewer-ready IBKR paper setup with TWS API access or a documented reviewer path that Anthropic accepts for local finance tools.
- Branding: provide a store-ready logo/icon and a concise detail-card description. The MCPB manifest should include an `icon` before broad promotion.
- Platform claim: Claude Desktop's documented primary platforms are macOS and Windows. If native Windows support remains absent, describe the MCPB as macOS-only for Claude Desktop review, and keep Linux as a generic MCPB/client capability only if the target directory accepts it.
- Signing posture: unsigned MCPBs are installable in permissive Claude Desktop configurations, but enterprise admins can require signatures. A trusted code-signing certificate or compatible signing API is needed before advertising the bundle as signed.

Submission materials:

- Server name: `ibkr`.
- Tagline: "Agentic Interactive Brokers portfolio analysis and trading research for Claude Desktop."
- Description: emphasize local-only operation, IB Gateway/TWS prerequisite, IBKR Pro requirement, portfolio review, options diagnostics, market-regime context, current no-order-entry boundary, and account-sensitive outputs.
- Documentation: README install section, [agentic use](../operate/agents.md), [configuration reference](../reference/config.md), and [privacy policy](../../../PRIVACY.md).
- Support: GitHub issues; security through GitHub Private Vulnerability Reporting.
- Capabilities: account, positions, quotes, watchlists, calendars, option chains, history, technical screens, scanners, portfolio review workflows, sizing math, S&P 500 breadth, dealer gamma, broad-market regime lifecycle, stateless portfolio canary; no built-in prompts; quote resource read/subscribe.

## External MCP directories

Some directory surfaces are crawler-driven and some still require a maintainer account or manual form. Keep the repo metadata current first, then use the submission pack below for account-gated channels.

Prepared assets:

- `server.json` for the official MCP Registry release path.
- `.claude-plugin/plugin.json` for Claude Code plugin metadata.
- `glama.json` to identify the authorized Glama maintainer.
- GitHub README, docs, privacy policy, and releases as the product-owned public surfaces.

Directory notes:

- PulseMCP: prioritize official MCP Registry publication and repo metadata.
- Glama: keep `glama.json` present, then claim or refresh the GitHub-backed server entry from a maintainer account.
- Smithery: hosted URL publishing is built around remote transports; for this local stdio project, use the official registry metadata and manual listing text unless a hosted bridge is added.
- MCP.so, MCPMarket, MCPpedia, and similar directories: use the submission pack below; most need a maintainer login, GitHub URL, public docs URL, and concise safety language.

Submission pack:

- Name: `ibkr`.
- Title: `IBKR MCP Server`.
- Tagline: "Agentic Interactive Brokers portfolio analysis and trading research through local TWS or IB Gateway."
- Short description: "Connect Claude Desktop, Claude Code, Cursor, Zed, or another MCP host to a local Interactive Brokers session for portfolio review, exposure analysis, options diagnostics, quotes, scanners, sizing, breadth, dealer gamma, broad-market regime lifecycle, and a stateless portfolio canary."
- Requirements: IB Gateway 10.37+ or TWS running locally; IBKR Pro account with TWS API access; macOS or Linux binary for the bundled path.
- Current boundary: current public CLI and MCP releases expose analysis, sizing, and preview-only stock/ETF order drafts, but no order placement, modification, cancellation, or broker-transmission interface.
- Target users: semi-professional retail traders using IBKR as capital-markets infrastructure who want agent-assisted review and research on broker-native data.
- Links: <https://github.com/osauer/ibkr>, <https://github.com/osauer/ibkr/blob/main/docs/reference/mcp-tools.md>, <https://github.com/osauer/ibkr/blob/main/docs/guides/agentic-use.md>, <https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb>.

Publish sequence:

1. Release any MCPB-readiness changes through `make release RELEASE_VERSION=vX.Y.Z`; do not tag or create releases manually.
2. After the GitHub Release is published, the `registry-publish` Actions workflow publishes the open MCP Registry metadata via OIDC. `make release` polls the public registry for the exact released version for about four minutes and normally completes without local login. If OIDC does not deliver in that window, it falls back to the existing `make registry-publish RELEASE_VERSION=vX.Y.Z MCP_PUBLISHER=bin/mcp-publisher` device-code flow. Run `make registry-publish` directly only as a manual heal; be at a browser only if the fallback is needed.
3. Submit to Anthropic's Desktop extension submission form from the Claude docs, attaching the public GitHub release asset URL and the submission materials above.
4. Wait for Anthropic review. Directory approval, listing slug, and in-product availability are controlled by Anthropic; a successful MCP Registry publish is not a Claude Directory publish.
