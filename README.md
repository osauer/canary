# Canary

[![ci](https://github.com/osauer/canary/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/osauer/canary/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/osauer/canary?display_name=tag&sort=semver)](https://github.com/osauer/canary/releases/latest)
[![license](https://img.shields.io/github/license/osauer/canary)](LICENSE)

**Your Interactive Brokers account, read by your terminal, your phone, and your
AI agent. The binary you install has no code that writes to the broker.**

Canary is one Go binary, a risk desk that runs beside your IB Gateway or TWS;
nothing is hosted. It tells you what needs attention today, which of twenty
discipline checks your positions breach against limits you set, your exposure
by underlying, and what the price did over the next 1, 5, and 20 sessions after
each past stock or ETF trade, measured against your broker statements. The same
readings come as text, as JSON, in a paired phone app, and as MCP tools for
Claude Desktop, Claude Code, Cursor, or Zed.

The standard binary does not place, change, or cancel an order, and MCP has no
order or preview tool in either build. When you want trades, install the
separate trading binary. It places one explicitly confirmed transaction at a
time, for you or for an agent, through the CLI.

**[Documentation](https://osauer.dev/canary/docs/)** · [Install](docs/docs/start/install.md) · [First session](docs/docs/start/first-session.md) · [Canary Edge](docs/docs/understand/edge.md) · [MCP tools](docs/docs/reference/mcp-tools.md) · [Safety](SECURITY.md) · [Privacy](PRIVACY.md)

## Start

You need IB Gateway 10.37+ or TWS with API socket access enabled, an IBKR Pro
account, and macOS or Linux on arm64 or amd64. Paper and live both work. WSL
works; native Windows does not.

**Terminal, and every MCP host.** One signed binary in `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary status    # show which Gateway and account Canary reached
canary brief     # review what changed and what needs attention
```

The [install guide](docs/docs/start/install.md) shows how to inspect the script
first and covers every other installation path.

**Claude Desktop.** Download the MCP Bundle,
[`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb),
open it with Claude Desktop, then quit and relaunch Claude. The
[walkthrough](https://osauer.dev/canary/claude-desktop-interactive-brokers/)
covers the first connection.

**Any other MCP host.** Install the binary as above, then point the host at its
absolute path; `which canary` prints it.

```json
{
  "mcpServers": {
    "canary": {
      "command": "/ABSOLUTE/PATH/TO/canary",
      "args": ["mcp"]
    }
  }
}
```

Then ask:

> What needs attention today, and which data is missing or stale?

![Canary Edge, rendered from synthetic data: a decision review, its coverage, and price outcomes](docs/social/canary-app-edge.png)

_Canary Edge in the paired app, rendered from synthetic data: what the price did
after past decisions, and how much evidence supports each row._

## Ways in

| You want to… | Start with | What you get |
| --- | --- | --- |
| Ask an agent about the account | The MCP Bundle, or `canary mcp` from any local framework that can launch a stdio MCP server | Read-only tools; nothing that changes settings or orders |
| Work in a terminal or script | `canary brief`, `canary positions --by underlying`, and `--json` | Every reading as text or JSON, from the same source as the app and the MCP tools |
| Check the desk from a phone | `canary app`, then `canary app pair` | A paired web app on your phone; local by default, remote access optional |
| Read Canary from your own Go program | `github.com/osauer/canary/v2`, the daemon client | The same read-only tools over the local daemon socket, without an MCP process |
| Build directly on the TWS protocol | `github.com/osauer/canary/v2/pkg/ibkr` | A low-level transport; policy, authorization, and journaling stay in your application |

## MCP hosts

Ask plain questions rather than naming tools. The
[host guide](docs/docs/start/hosts.md) covers Claude Code, Cursor, Zed,
continuously running agents, logs, and connection checks. The generated
[MCP reference](docs/docs/reference/mcp-tools.md) lists every tool and its
schema.

## Terminal and paired app

Every data command supports `--json`:

```sh
canary account
canary positions --by underlying
canary edge
canary rules
canary regime --explain
canary stress --details
```

Run `canary status` first for connection problems and `canary data health` for
missing or stale data. `canary --help` and the
[CLI reference](docs/docs/reference/cli.md) list every command and flag.

For the paired app, run `canary app` on the machine that owns the Gateway or
TWS session, then run `canary app pair` and scan the QR code. The
[app guide](docs/docs/operate/app.md) covers pairing, remote access, and push
notifications.

## Go wire-protocol library

`pkg/ibkr` is a clean-room Go library for the TWS wire protocol. It transports
broker requests only. `go install github.com/osauer/canary/v2/cmd/canary@latest`
installs the v2 CLI, not the current v3 release; use the installer for the
binary. Start with the [package documentation](pkg/ibkr/doc.go) and the
[wire protocol page](docs/docs/internals/protocol.md).

## Safety and privacy

The standard binary has no broker-write path compiled in; the installer,
updater, and MCP Bundle never install the trading binary. In the trading binary
each trade, yours or an agent's, is pinned to one account, client ID, and mode,
checked again just before it runs, and blocked by any runtime freeze.
[Gated orders](docs/docs/operate/orders.md) describes this boundary.

Canary has no telemetry and does not send account IDs, balances, quantities, or
P/L to the maintainer. [PRIVACY.md](PRIVACY.md) is the data map.
[SECURITY.md](SECURITY.md) covers the threat model and signed releases.

## Documentation and legal

- [Daily desk workflow](docs/docs/operate/daily-desk.md)
- [Troubleshooting](docs/docs/start/troubleshooting.md)
- [Configuration](docs/docs/reference/config.md)
- [Release and update policy](docs/docs/reference/releases.md)
- [Contributing](CONTRIBUTING.md)

Canary is an independent third-party client for Interactive Brokers' publicly
documented TWS API. It is not built, endorsed, sponsored, or supported by
Interactive Brokers Group, Inc. or its affiliates. `pkg/ibkr` redistributes no
Interactive Brokers code, libraries, jars, or market data. Nothing here is
investment advice.

Canary is a private project of Oliver Sauer.

MIT. See [LICENSE](LICENSE).
