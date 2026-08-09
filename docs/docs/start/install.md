# Install and first run

Updated: 2026-07-25 11:38 CEST

`canary` is one Go binary. It carries the terminal CLI, a local stdio MCP server, and the background daemon that holds the connection to your own IB Gateway or TWS session. Nothing is hosted, and no Python or Java runtime is involved.

## Before you start

- **A gateway, running and logged in.** IB Gateway 10.37 or newer, or TWS. Paper or live both work.
- **API socket access enabled** in that app, under Global Configuration → API → Settings. Without it the port accepts TCP and never completes the handshake.
- **An IBKR Pro account.** IBKR Lite does not include TWS API access.
- **macOS or Linux, on arm64 or amd64.** Releases build for those four combinations.
- **The same machine as the gateway**, unless you pin `[gateway].host`. Discovery only probes loopback.

There is no Windows build. The daemon uses `setsid`, `flock`, and AF_UNIX sockets, so the code does not compile for Windows at all, and the installer stops with a message when it sees an MSYS, MinGW, or Cygwin shell. WSL is the Windows path: inside WSL, `uname` reports Linux and the ordinary Linux install applies.

## Pick an install path

| Path | How | Use it when |
| --- | --- | --- |
| Claude Desktop bundle | Open [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) with Claude Desktop | Claude Desktop is your only host. The bundle carries its own macOS and Linux binaries. |
| Shell installer | `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh \| sh` | You want one shared binary for the terminal and for every MCP host. |
| `go install` | `go install github.com/osauer/canary/v2/cmd/canary@latest` | You already have Go 1.26 or newer and prefer building from source. |
| Release tarball | Download `canary-vX.Y.Z-<os>-<arch>.tar.gz`, verify it against `SHA256SUMS` | You verify downloads by hand, or you install to a managed location. |
| Clone and build | `make install` | You are working on the code. |

The [Claude Desktop install page](../../claude-desktop-interactive-brokers/index.html) walks through the bundle step by step. [Packaging and distribution](../internals/packaging.md) describes what each artifact contains.

## What the shell installer does

1. Detects your OS and architecture, and refuses anything other than Darwin or Linux on arm64 or amd64.
2. Resolves the latest release by following the `/releases/latest` redirect, so it makes no API call and cannot hit a rate limit.
3. Downloads the tarball, `SHA256SUMS`, and `SHA256SUMS.asc`. From v1.0.0 onward a missing signature aborts the install rather than falling back to a checksum-only path.
4. Imports the release-signing key from the tagged source tree into a throwaway keyring, checks its fingerprint against `D98426D48FED85EFA33904694D922A4F922B7D7D`, and verifies the signature. A fingerprint or signature mismatch aborts. `gpg` is required for v1.0.0 and later.
5. Verifies the SHA-256 checksum.
6. Lists the archive before extracting it. Anything beyond `canary`, `LICENSE`, and `README.md` is rejected, and a symlinked `canary` is rejected too.
7. Installs the binary mode 0755 into `~/.local/bin`, or into `CANARY_INSTALL_DIR` if you set one.
8. Clears the macOS `com.apple.quarantine` attribute so Gatekeeper does not block the first run.
9. Adds `~/.local/bin` to your shell rc when it is missing from `PATH`, picking the file from `$SHELL`: `.zshrc`, `.bashrc`, `config.fish`, or `.profile`. Open a new terminal afterwards, or source that file, before the shell finds `canary`. Setting `CANARY_INSTALL_DIR` suppresses the edit entirely, and you manage `PATH` yourself.
10. Runs `canary version` and warns if the installed binary reports something other than the version it just fetched.

Prefer to read it first: `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh -o install.sh`, then `less install.sh`, then `sh install.sh`.

## Check it worked: `canary status`

`canary status` is the one command worth memorizing. It answers whether the daemon is up, whether the gateway handshake completed, which endpoint and account it landed on, and which subsystem is still warming.

```sh
canary status
```

A healthy install prints something close to this. The values below are synthetic:

```text
IBKR Gateway  READY

Session        DU0000000 via 127.0.0.1:4001 (tls=false, discovered), client 15
Market data    Live
Daemon         v1.0.0, up 30m42s
TWS            API server 178
SPX members    cache:2026-05-22, 503 names
```

The badge on the first line is `READY`, `STARTING`, `ATTENTION`, or `OFFLINE`. `discovered` on the Session row means the port came from the probe; `pinned` means it came from your config file. Rows for data farms, market access, background work, subsystems, trading, and data quality appear only when they have something to say, and a `Next concern` row appears only when there is one.

The `Market access` row is the one place a missing IBKR market-data subscription shows up by name, instead of surfacing separately in each feature that needed the data:

```text
Market access  SPX not subscribed (IBKR 354, seen 14:03, retry 14:33)
```

It lists what the gateway refused, when, and when Canary will ask again — IBKR code 354 means the account is not subscribed to that data. Nothing is switched off because of it: a listed symbol only means a fresh fetch would be refused right now, and features holding a cached result keep serving it. The row is an observation with a 30-minute window, not a record of your entitlements, so a symbol nothing asked for during the window never appears, and a farm outage can put a symbol there that your account does hold. Subscriptions are managed in IBKR Client Portal, not in Canary.

When the daemon has no verdict from the gateway yet, `status` polls every 500 ms for up to 25 seconds before giving up, then prints the API checklist and the daemon log path. When the gateway is not connected, the command exits 1 and the `Next concern` row carries the underlying error, which makes it usable in a script.

No config file is needed for this. The daemon probes `4001` (Gateway live), `4002` (Gateway paper), `7496` (TWS live), and `7497` (TWS paper) in parallel with a 200 ms timeout each, takes the first responder in that order, lists any others as alternates, auto-detects the account from `managedAccounts`, and uses client ID `15`. Create `~/.config/ibkr/config.toml` only when you want to pin something; every key in the [configuration reference](../reference/config.md) is optional.

## Two things that surprise people

**The daemon starts itself.** Any daemon-backed command dials the Unix socket, and starts `canary daemon` in its own session when the socket is missing, waiting up to 5 seconds for it to appear. You never start it by hand. One daemon serves the CLI and every MCP host, so they share a single gateway connection and client ID. It exits once 15 minutes pass with no client connection and no background work in flight, which `[daemon].idle_timeout` controls.

**The first breadth build takes about 74 minutes.** S&P 500 breadth is computed locally from constituent daily bars, because IBKR does not redistribute S&P DJI's breadth indices on retail subscriptions. The shared historical-data limiter allows an initial 60 requests and then refills at 0.1 per second, so the remaining 443 names pace out to roughly 74 minutes on a cold cache. Nothing is broken while that runs: `canary status` shows `breadth:computing` and a `Background` row reading `refreshing rolling SPX breadth`. The snapshot is published only once coverage reaches 80% of the member count; below that the engine keeps the per-name windows it did fetch, retries every 12 minutes, and gives up after 15 tries until the next post-close refresh. The idle timer will not cut the build short: breadth counts as background work, so the daemon stays up until the refresh finishes or the retry budget runs out. Later refreshes are quick, and the cache survives restarts.

## Where things land

| Path | Contents |
| --- | --- |
| `~/.local/bin/canary` | The binary, unless you set `CANARY_INSTALL_DIR` |
| `~/.config/ibkr/config.toml` | Optional pinned config. Absent by default |
| `~/.config/ibkr/policies/risk-policy.toml` | Your risk policy, if you write one |
| `~/.local/state/ibkr/daemon.db` | Daemon-owned state: settings, journals, membership, evidence |
| `~/.local/state/ibkr/ibkr-daemon.log` | Daemon log. First place to look when something will not start |
| `~/.cache/ibkr/ibkr.sock` | The daemon socket, under `$XDG_RUNTIME_DIR/ibkr/` when that is set |
| `~/.cache/ibkr/` | Caches and update scratch space |

`$XDG_CONFIG_HOME`, `$XDG_STATE_HOME`, `$XDG_CACHE_HOME`, and `$XDG_RUNTIME_DIR` take precedence where they are set. `CANARY_CONFIG`, `CANARY_SOCKET`, and `CANARY_LOG` override the config, socket, and log paths individually.

## Next

Wire an agent to the server in [Connect an MCP host](hosts.md). Keep the binary current with [Updating](updating.md). Read [Order previews and the trading build](../operate/orders.md) before assuming anything about what this can and cannot send to the broker.
