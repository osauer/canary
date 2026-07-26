**The data flows out. Orders don't go in.**

Canary is a read-only interface to your Interactive Brokers account, reachable from a Go library, a shell CLI, a stdio MCP server (Claude Desktop, Cursor, Continue, Zed), and a Claude Code plugin. Hand it to an assistant, a cron job, a notebook, or your own service.

## What's new in __VERSION__

__HIGHLIGHTS__

## Claude Desktop MCPB

Download the canonical `canary.mcpb` asset from the Assets section below or from:

<https://github.com/osauer/canary/releases/latest/download/canary.mcpb>

Open the `.mcpb` file with Claude Desktop, drag it into Claude Desktop, or use Settings -> Extensions -> Advanced settings -> Install Extension. The bundle carries the read-only local server for macOS and Linux. Windows is not supported outside WSL because the daemon uses Unix-only primitives.

## Shell and generic MCP install

~~~sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary setup claude-desktop
~~~

The first command picks the right binary for your platform, verifies its signed SHA-256 inventory, installs it to `~/.local/bin/canary`, adds the directory to your `PATH` if needed, and clears the Gatekeeper quarantine flag on macOS. The second command writes the MCP server entry into Claude Desktop's config; fully quit Claude Desktop and reopen.

If you only want the shell tool, stop after the first command and try:

~~~sh
canary account
canary quote AAPL
canary positions --by underlying
~~~

**Prerequisite**: a running [IB Gateway 10.37+](https://www.interactivebrokers.com/en/trading/ib-gateway.php) or TWS (paper or live) on the same machine. The daemon auto-discovers it across the four standard ports.

See the [README](https://github.com/osauer/canary#readme) for the full feature menu and the troubleshooting matrix. Read-only by construction; the [Safety](https://github.com/osauer/canary#safety) section walks through the four guards.

## ⚠️ Broker-write capable build (`canary-trading-*` tarballs)

Everything above — the installer, the MCPB bundle, and the plain `canary-__VERSION__-*` tarballs — is **read-only by construction**: order transmission is not compiled in.

The `canary-trading-__VERSION__-*` tarballs are different: **that binary can place, modify, and cancel orders with your broker** once you configure the trading gates (`[trading]` mode plus a pinned gateway endpoint and account, cross-checked against the connected session; every write still needs a submit-eligible preview token). Only download it if you intend to trade through Canary. Before enabling anything, read [SECURITY.md](https://github.com/osauer/canary/blob/__VERSION__/SECURITY.md) and the [trading preview guide](https://github.com/osauer/canary/blob/__VERSION__/docs/docs/operate/orders.md), start against a paper account, and verify with `canary trading status`. Each release's order pipeline is exercised by an automated paper-trading round-trip before tagging.

---

### Paranoid? Inspect the installer before running it

~~~sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh -o install.sh
less install.sh
sh install.sh
~~~

### Doing something custom?

- **`go install`**: `go install github.com/osauer/canary/v2/cmd/canary@__VERSION__` (or `@latest`).
- **Different install dir**: `CANARY_INSTALL_DIR=/usr/local/bin sh install.sh`. The installer won't touch your shell rc when you override; manage PATH yourself.
- **Manual download**: pick a tarball or `.mcpb` from the Assets section below. Verify against `SHA256SUMS`.
- **Cursor / Continue / Zed / other local MCP clients**: see [Pick your path](https://github.com/osauer/canary#claude-desktop-cursor-continue-zed) in the README for the JSON snippet (config file path differs per client).
- **Claude Code**: `/plugin marketplace add osauer/canary` then `/plugin install canary@canary` inside any session.

Windows isn't supported. The daemon uses Unix-only primitives (`setsid`, `flock`, AF_UNIX sockets). WSL works.

---
