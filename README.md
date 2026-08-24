# Canary — local IBKR risk desk

[![ci](https://github.com/osauer/canary/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/osauer/canary/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/osauer/canary?display_name=tag&sort=semver)](https://github.com/osauer/canary/releases/latest)
[![go.mod](https://img.shields.io/github/go-mod/go-version/osauer/canary)](go.mod)
[![Go reference](https://pkg.go.dev/badge/github.com/osauer/canary/v2.svg)](https://pkg.go.dev/github.com/osauer/canary/v2)
[![license](https://img.shields.io/github/license/osauer/canary)](LICENSE)

**[Documentation](https://osauer.dev/canary/docs/)** · [Install](docs/docs/start/install.md) · [First session](docs/docs/start/first-session.md) · [MCP tools](docs/docs/reference/mcp-tools.md) · [Configuration](docs/docs/reference/config.md) · [Safety](SECURITY.md) · [Privacy](PRIVACY.md)

Canary turns one local IB Gateway or TWS session into a daily risk brief for the
terminal, an MCP host, and a paired phone. It keeps one daemon responsible for
broker connectivity, market evidence, policy state, and the local order
journal, so every surface reads the same account authority.

Start with two commands:

```sh
canary status    # prove which gateway and account Canary reached
canary brief     # review what changed and what needs attention
```

The standard binary is read-only by construction. Its 13 MCP tools can inspect
the account, risk desk, proposals, opportunities, and local order state, but
cannot preview or transmit a broker action. Trading-capable builds are separate,
experimental artifacts with additional human and daemon gates.

Canary is for an IBKR Pro user who runs Gateway or TWS locally and wants risk
evidence to stay explicit when a source is stale, held, or unavailable. It is
not a hosted brokerage service or a complete replacement for the TWS API. If
you only need a Go wire-protocol client, use [`pkg/ibkr`](#go-library).

**Contents** — [Install](#install) · [What you get](#what-you-get) · [Pick your path](#pick-your-path) · [How it works](#how-it-works) · [Configure](#configure) · [Safety](#safety) · [Other install paths](#other-install-paths) · [Troubleshooting](#troubleshooting)

## Install

You need:

- IB Gateway 10.37+ or TWS, running and logged in with API socket access enabled
- an IBKR Pro account; IBKR Lite does not include TWS API access
- macOS or Linux on arm64 or amd64; WSL works, native Windows does not

### Shell installer

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary status
```

The installer downloads the matching release, verifies its signed checksum,
and installs `canary` in `~/.local/bin`. To inspect it before running it, follow
the [install guide](docs/docs/start/install.md#what-the-shell-installer-does).

### Claude Desktop

Download [canary.mcpb](https://github.com/osauer/canary/releases/latest/download/canary.mcpb),
open it with Claude Desktop, then quit Claude completely and relaunch it. The
bundle carries its own macOS and Linux binaries, so it does not need the shell
installer.

The [Claude Desktop walkthrough](https://osauer.dev/canary/claude-desktop-interactive-brokers/)
covers installation and the first connection.

## What you get

| Surface | What it answers |
| --- | --- |
| **Daily brief and Action Queue** | What changed since the close, what the next session needs, and which alerts, protection candidates, exercise candidates, or process exceptions deserve attention. |
| **Account and positions** | Which selected account is authoritative, how the book is exposed, and whether values or Greeks are missing. Multi-account ambiguity is refused rather than blended. |
| **Rulebook, policy, and reconciliation** | Which daily-desk rule is hardest, which inputs are unknown, what risk constitution is approved, and whether broker statements agree with the capital ledger. |
| **Market and technical evidence** | How named stocks or ETFs trend, plus the calendar, breadth, gamma, regime, stress, earnings, borrow, and halt evidence used by the desk. Each source keeps its own freshness and health. |
| **Current work** | Which close/reduce-only proposals, exercise opportunities, and local orders exist, why they are blocked or ready for review, and how they changed. |

Every data command supports `--json`. The embedded paired app presents the same
daemon-owned evidence through Monitor, Brief, Alerts, Orders, and Settings.

## Pick your path

### Claude Desktop, Cursor, Continue, Zed

The MCP Bundle is the shortest path for Claude Desktop. For another local stdio
host, install the shell binary and point the host at its absolute path:

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

Use `which canary` to find the path. After an upgrade, fully relaunch the host
so it respawns the MCP process. A browser-only assistant cannot reach this
local stdio server.

Ask questions in desk language:

> What needs attention today, and which inputs are degraded?
>
> How is my portfolio exposed by underlying?
>
> Are any protection or exercise candidates ready for human review?

See [Connect an MCP host](docs/docs/start/hosts.md) for Claude Code, host logs,
and connection checks. The generated [MCP reference](docs/docs/reference/mcp-tools.md)
lists all 13 tools and their schemas.

### Claude Code

The plugin adds the skill, MCP configuration, and safety hooks; it does not ship
the Canary binary:

```text
/plugin marketplace add osauer/canary
/plugin install canary@canary
```

Install the binary separately, and update the plugin and binary on their own
cadences. The [agent guide](docs/docs/operate/agents.md) explains the authority
boundary.

### The shell

```sh
canary account
canary positions --by underlying
canary brief
canary rules
canary technical SPY,QQQ
canary proposals list
canary opportunities list
canary orders open
```

Add `--json` for machine-readable output. Run `canary status` first when
anything looks wrong; `canary --help` and the [CLI reference](docs/docs/reference/cli.md)
carry the full command and flag inventory.

### Mobile app

Run `canary app` on the machine that owns the Gateway or TWS session, then
`canary app pair` and scan the QR code. Local pairing is the default; the
optional remote relay and Web Push paths are documented in the
[app operator guide](web/app/README.md) and [privacy policy](PRIVACY.md).

### Go library

`pkg/ibkr` is a clean-room Go client for the TWS wire protocol:

```go
import (
    "context"
    "time"

    "github.com/osauer/canary/v2/pkg/ibkr"
)

func accountSummary(ctx context.Context) (*ibkr.RawAccountSummary, error) {
    cfg := ibkr.DefaultConfig()
    cfg.Port = 4002 // Gateway paper

    connector := ibkr.NewConnector(&ibkr.ConnectorConfig{
        PreferredClientID: 15,
        BaseConfig:        cfg,
    })
    if err := connector.Start(ctx); err != nil {
        return nil, err
    }
    defer connector.Stop()

    return connector.RequestAccountSummary(ctx, 5*time.Second)
}
```

Protocol coverage is purpose-driven, not exhaustive. The package transports
broker requests but does not provide Canary's complete application-level
authority boundary; direct users must supply their own policy, authorization,
journaling, and reconciliation controls. Start with the
[package documentation](pkg/ibkr/doc.go).

## How it works

```text
shell, MCP host, or paired app
              ↓ local Unix socket
         canary daemon
              ↓ local TCP by default
       IB Gateway or TWS
```

The daemon starts on demand, keeps the gateway session warm, and normally exits
after 15 idle minutes. One daemon serves every local client, so they share the
same selected account, client ID, cached evidence, and durable state.

Use `canary restart` after upgrading or changing daemon-loaded configuration.
Use `canary stop` for an orderly shutdown. Restarting the daemon does not
restart an MCP subprocess owned by another application; relaunch that host when
you need it to load a new binary. The [architecture](docs/docs/internals/architecture.md)
and [storage](docs/docs/internals/storage.md) pages describe the ownership
boundaries in detail.

## Configure

Normal read-only use needs no config file. Canary probes the standard Gateway
and TWS ports on loopback, selects one account from `managedAccounts`, and uses
client ID 15. Create `~/.config/ibkr/config.toml` only when you want a binding
override, for example:

```toml
[gateway]
port = 7496 # TWS live
```

Unknown keys fail at startup. Runtime preferences and durable operational state
remain under the existing `ibkr` XDG paths so an upgrade cannot create a second
authority. See the generated [configuration reference](docs/docs/reference/config.md)
for every TOML key and environment variable, and [Trading policy](docs/docs/understand/policy.md)
for the risk constitution and advisory boundaries.

## Safety

- **Read-only is the normal product.** The normal installer, updater, and MCP
  bundles select the standard binary, whose broker-write handlers are not
  compiled in.
- **MCP remains read-only in every build.** Its registry contains no preview,
  place, modify, cancel, or exercise tool.
- **Trading is a separate decision.** Trading artifacts are explicitly named,
  experimental, and require pinned connection authority, an eligible fresh
  review contract, healthy journaling, daemon revalidation, an open runtime
  freeze, and a transaction-specific human instruction.
- **Missing evidence stays missing.** Source health, freshness, and retained
  last-good state remain visible rather than becoming a clean or zero result.
- **The Go package is lower-level.** Default builds disable unrestricted order
  and exercise methods, but narrow paper-gated transport methods remain for
  daemon use. A library build is not a substitute for application authority.

Read [SECURITY.md](SECURITY.md) and [Gated orders and the trading build](docs/docs/operate/orders.md)
before using a trading-capable artifact.

Canary has no telemetry and does not send account IDs, balances, quantities, or
P&L to the maintainer. Some public-source refreshes make outbound requests; an
earnings lookup can disclose the normalized ticker being evaluated. An MCP host,
remote app relay, or push service receives only the data sent through the path
you enable. [PRIVACY.md](PRIVACY.md) is the authoritative data map.

## Other install paths

- **Release tarball:** download the asset for your platform and verify it
  against signed `SHA256SUMS`; the [release guide](docs/docs/reference/releases.md)
  has the exact commands and signing-key fingerprint.
- **Public Go module / source-built v2 CLI:** `go install
  github.com/osauer/canary/v2/cmd/canary@latest` requires Go 1.27+. This remains
  on the maintained v2 module line and does not install product v3.
- **Local development build:** clone the repository and run `make install`.
- **Self-update:** `canary update` selects the newest stable release on the
  installed product major, verifies its signature and checksum, and replaces
  the binary atomically. It does not silently move a v2 installation to v3.

See [Install and first run](docs/docs/start/install.md) for custom install paths,
manual verification, native-platform limits, and the product/module version
split.

## Testing

```sh
make check  # formatting, analysis, vulnerability, documentation, and parity gates
make test   # make check plus unit and hermetic lifecycle tests
```

Live Gateway checks are separate and read-only: `make smoke-fast` is the quick
gate and `make smoke` exercises the full CLI/daemon wire path. See
[CONTRIBUTING.md](CONTRIBUTING.md) before sending a change.

## Troubleshooting

| Symptom | First check |
| --- | --- |
| `no IBKR listener found` | Confirm Gateway or TWS is logged in and API socket access is enabled. Discovery probes only loopback and the four standard ports. |
| `gateway not responding to TWS handshake` | In TWS settings, enable ActiveX and Socket Clients. Avoid running several IBKR desktop applications against the same login. |
| `daemon socket did not appear` | Read `~/.local/state/ibkr/ibkr-daemon.log`; startup failures name the port, client ID, or config problem. |
| MCP tools are absent | Confirm the host uses an absolute binary path, then fully quit and relaunch it. |
| CLI and daemon versions differ | Run `canary restart`; relaunch MCP hosts separately. |

The [troubleshooting guide](docs/docs/start/troubleshooting.md) covers endpoint
discovery, subscriptions, permissions, logs, and safe diagnostic capture.

## Disclaimer & trademarks

Canary is an independent third-party client for Interactive Brokers' publicly
documented TWS API. It is not built, endorsed, sponsored, or supported by
Interactive Brokers Group, Inc. or its affiliates.

`pkg/ibkr` is a clean-room implementation and redistributes no Interactive
Brokers code, libraries, jars, or market data. “Interactive Brokers”, “IBKR”,
“TWS”, and “IB Gateway” are used only to identify the brokerage and API.

Nothing here is investment advice. Use it at your own risk; the MIT license's
AS IS clause applies in full.

## License

MIT. See [LICENSE](LICENSE).
