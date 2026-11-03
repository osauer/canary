# Canary

[![ci](https://github.com/osauer/canary/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/osauer/canary/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/osauer/canary?display_name=tag&sort=semver)](https://github.com/osauer/canary/releases/latest)
[![license](https://img.shields.io/github/license/osauer/canary)](LICENSE)

**A local risk desk for your Interactive Brokers account.**

Canary reads one local IB Gateway or TWS session. From it you get a daily
brief, the current portfolio with the market evidence behind it, and a
broker-confirmed review of what your past position changes delivered. It is for
an IBKR Pro user who runs the Gateway or TWS locally and wants to see when
evidence is stale or missing rather than have it hidden. The standard binary
can preview an order but never place, change, or cancel one, and no MCP tool
can even preview one in any build.

**[Documentation](https://osauer.dev/canary/docs/)** · [Install](docs/docs/start/install.md) · [First session](docs/docs/start/first-session.md) · [Canary Edge](docs/docs/understand/edge.md) · [MCP tools](docs/docs/reference/mcp-tools.md) · [Safety](SECURITY.md) · [Privacy](PRIVACY.md)

![Canary Edge showing a synthetic decision review, its coverage, and price outcomes after decisions](docs/social/canary-app-edge.png)

_Canary Edge in the paired app, rendered from synthetic data. Edge reports what
happened after past decisions; it is not a forecast or a claim of cause._

## Start

You need IB Gateway 10.37+ or TWS with API socket access enabled, an IBKR Pro
account, and macOS or Linux on arm64 or amd64. WSL works; native Windows does not.

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary status    # show which Gateway and account Canary reached
canary brief     # review what changed and what needs attention
```

The installer verifies the signed release checksum and installs to
`~/.local/bin`. The [install guide](docs/docs/start/install.md) shows how to
inspect the script first and covers every other installation path.

If you use Claude Desktop, download the MCP Bundle,
[`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb),
open it with Claude Desktop, then quit Claude completely and relaunch it. The
[walkthrough](https://osauer.dev/canary/claude-desktop-interactive-brokers/)
covers the first connection. Ask:

> What needs attention today, and which inputs are degraded?

## Choose how you use it

| You want to… | Start with | What you get |
| --- | --- | --- |
| Ask an agent about the account | The MCP Bundle, or `canary mcp` from any local framework that can launch a stdio MCP server | Read-only tools; nothing that changes settings or orders |
| Work in a terminal or script | `canary brief`, `canary positions --by underlying`, and `--json` | Every reading as text or JSON, from the same source as the app and the MCP tools |
| Check the desk from a phone | `canary app`, then `canary app pair` | A paired web app on your phone; local by default, remote relay optional |
| Read Canary from your own Go program | `github.com/osauer/canary/v2`, the daemon client, with `canarytest` for tests | The same read-only tools over the daemon socket, without an MCP process |
| Build directly on the TWS protocol | `github.com/osauer/canary/v2/pkg/ibkr` | A low-level transport; policy, authorization, and journaling are yours |

## MCP and agent frameworks

For a host other than Claude Desktop, install the standard binary and point the
host at the binary's absolute path; `which canary` prints it. Hosts that use
the common `mcpServers` shape accept:

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

Ask plain questions like the one above rather than naming tools. The
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
missing or degraded inputs. `canary --help` and the
[CLI reference](docs/docs/reference/cli.md) carry the complete command and flag
list.

For the paired app, run `canary app` on the machine that owns the Gateway or
TWS session, then run `canary app pair` and scan the QR code. The
[app guide](docs/docs/operate/app.md) covers pairing, remote relay, and push
notifications.

## Go wire-protocol library

`pkg/ibkr` is a clean-room Go library for the TWS wire protocol. It transports
broker requests only; the policy, authorization, and journaling that Canary
adds are your application's job. The Go module is on `/v2`. Canary itself ships
as signed v3 binaries, so `go install github.com/osauer/canary/v2/cmd/canary@latest`
installs the v2 CLI, not the current release. Start with the
[package documentation](pkg/ibkr/doc.go) and the
[wire protocol page](docs/docs/internals/protocol.md).

## Safety and privacy

The installer, updater, and MCP Bundle install the standard binary, which has
no broker-write path compiled in. Trading needs a separate, opt-in binary.
Every action in it is pinned to one account, client ID, and mode, reviewed
again just before it runs, blocked by any runtime freeze, and authorized by a
person for that one transaction. [Gated orders](docs/docs/operate/orders.md)
describes this boundary.

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
