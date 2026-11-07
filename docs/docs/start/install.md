# Install and first run

Updated: 2026-11-07 07:48 CET

`canary` is one Go binary. It carries the terminal CLI, a local stdio MCP server, and the background daemon that holds the connection to your own IB Gateway or TWS session. Nothing is hosted, and no Python or Java runtime is involved.

## Before you start

- **A gateway, running and logged in.** IB Gateway 10.37 or newer, or TWS. Paper or live both work.
- **API socket access enabled** in that app, under Global Configuration → API → Settings. Without it the port accepts TCP and never completes the handshake.
- **An IBKR Pro account.** IBKR Lite does not include TWS API access.
- **macOS or Linux, on arm64 or amd64.** Releases build for those four combinations.
- **The same machine as the gateway**, unless you pin `[gateway].host`. Discovery only probes loopback.
- **Any IBKR entity.** An account held with Interactive Brokers Hong Kong, Singapore, Japan, Australia, Ireland, the UK or the LLC connects the same way; [Hong Kong and Singapore accounts](ibkr-hong-kong-singapore.md) covers base currency and exchange calendars.

There is no Windows build. The daemon uses `setsid`, `flock`, and AF_UNIX sockets, so the code does not compile for Windows at all, and the installer stops with a message when it sees an MSYS, MinGW, or Cygwin shell. WSL is the Windows path: inside WSL, `uname` reports Linux and the ordinary Linux install applies.

On Linux, `canary stop`, `canary restart` and the restart after `canary update` read process identity from `/proc`, so they need no `ps` and work the same on BusyBox systems such as Alpine. Before Canary signals a process, it checks that the PID in the daemon's lock file runs `canary daemon`, and it refuses an unrelated or unreadable process without sending a signal. An amd64 binary run on an arm64 host through qemu-user emulation is recognised by its interpreter. Other emulators, such as Rosetta for Linux, FEX or box64, are not: `stop` names the command it saw and leaves the daemon running, so stop it yourself with `kill`.

Upgrading an existing install is covered in [Updating](updating.md).

## Pick an install path

| Path | How | Use it when |
| --- | --- | --- |
| Claude Desktop bundle | Open [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) with Claude Desktop | Claude Desktop is your only host. The bundle carries its own macOS and Linux binaries. |
| Shell installer | `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh \| sh` | You want one shared binary for the terminal and for every MCP host. |
| Public Go module / v2 CLI | `go install github.com/osauer/canary/v2/cmd/canary@latest` | You intentionally want the newest v2 release. This does not install v3. |
| Release tarball | Download `canary-vX.Y.Z-<os>-<arch>.tar.gz`, verify it against `SHA256SUMS` | You verify downloads by hand, or you install to a managed location. |
| Clone and build | `make install GO_TAGS=""` | You are working on the code. |

Plain `make install` and `make restart-daemon` use this repository's experimental trading build default. Pass `GO_TAGS=""` to retain the standard read-only capability.

The [Claude Desktop install page](../../claude-desktop-interactive-brokers/index.html) walks through the bundle step by step. [Packaging and distribution](../internals/packaging.md) describes what each artifact contains.

Version 3 installs through the signed installer, MCPB, or release tarballs. The
public Go module remains `github.com/osauer/canary/v2`: Go ignores root v3 tags
for that module path, so `/v2/...@latest` resolves the newest v2 tag. Existing
library consumers and source-built v2 installations stay on v2 instead of
crossing to v3 without asking.

## Current source and signed releases

The documentation and MCP discovery catalogue follow `main`, so a command or
tool can appear here before a signed release carries it. Check your installed
`canary --help` and the
[latest release](https://github.com/osauer/canary/releases/latest).

To try current source with a read-only build:

```sh
git clone https://github.com/osauer/canary.git
cd canary
make build GO_TAGS=""
./bin/canary --help
```

This builds a separate executable; it does not replace an installed Canary.
Go, Make and the versions declared in the repository are required.

## What the shell installer does

1. Detects your OS and architecture, and refuses anything other than Darwin or Linux on arm64 or amd64.
2. Resolves the latest release by following the `/releases/latest` redirect, so it makes no API call and cannot hit a rate limit.
3. Downloads the tarball, `SHA256SUMS`, and `SHA256SUMS.asc`. A missing signature aborts the install rather than falling back to a checksum-only path.
4. Imports the release-signing key from the tagged source tree into a throwaway keyring, checks its fingerprint against `D98426D48FED85EFA33904694D922A4F922B7D7D`, and verifies the signature. A fingerprint or signature mismatch aborts. `gpg` is required.
5. Verifies the SHA-256 checksum.
6. Lists the archive before extracting it. Anything beyond `canary`, `LICENSE`, and `README.md` is rejected, and a symlinked `canary` is rejected too.
7. Installs the binary mode 0755 into `~/.local/bin`, or into `CANARY_INSTALL_DIR` if you set one.
8. Clears the macOS `com.apple.quarantine` attribute so Gatekeeper does not block the first run.
9. Adds `~/.local/bin` when it is missing from `PATH`. For Bash it updates `.bashrc` and the first existing login file (`.bash_profile`, `.bash_login`, or `.profile`), creating `.bash_profile` if none exists. For Zsh it updates `${ZDOTDIR:-$HOME}/.zshrc`; fish uses `${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish`; other shells use `.profile`. Open a new terminal afterwards. A custom `CANARY_INSTALL_DIR` leaves shell configuration to you. If a startup file is not writable, installation continues and prints the line to add manually. The installer warns when an older executable takes precedence on PATH and shows an absolute command for the installed version.
10. Runs `canary version` and warns if the installed binary reports something other than the version it just fetched.

Prefer to read it first: `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh -o install.sh`, then `less install.sh`, then `sh install.sh`.

## Check it worked: `canary status`

`canary status` is the one command worth memorising. It answers whether the daemon is up, whether the gateway handshake completed, which endpoint and account it landed on, and which subsystem is still warming.

```sh
canary status
```

A healthy install prints something close to this. The values below are synthetic:

```text
Canary  READY

Session        DU0000000 (PAPER) via 127.0.0.1:4002 (tls=false, discovered),
               client 15
Market data    Live
Daemon         v3.34.1, up 30m42s
TWS            API server 178
Gateway setup  as expected · read 09:02
Executions     other-client fills not yet observed · required TWS Master API
               client ID: 15
SPX members    cache:2026-05-22, 503 names
Colors         Amber = row needs attention; headline = overall Canary status.
```

The badge on the first line is `READY`, `STARTING`, `ATTENTION`, or `OFFLINE`. When it is not `READY`, a `Reason` row directly below it names the cause. `discovered` on the Session row means the port came from the probe; `pinned` means it came from your config file. Rows for data farms, market access, background work, subsystems, trading, and data quality appear only when they have something to say.

The `Executions` row reports evidence of fills from other clients. Before one is observed, visibility is unproven even if TWS is already configured correctly. TWS reports fills entered by hand or by other API clients only to its master client, so the execution cost report and the decision scorecard see them only after you set **Global Configuration → API → Settings → Master API client ID** in TWS to Canary's client ID; the row reads `all clients (proven HH:MM)` once the first such fill has been seen, and `canary status --json` carries the same facts under `executions`.

The `Market access` row is the one place a missing IBKR market-data subscription shows up by name, instead of surfacing separately in each feature that needed the data:

```text
Market access  SPX not subscribed (IBKR 354, seen 14:03, retry 14:33)
```

It lists what the gateway refused, when, and when Canary will ask again — IBKR code 354 means the account is not subscribed to that data. Nothing is switched off because of it: a listed symbol only means a fresh fetch would be refused right now, and features holding a cached result keep serving it. The row is an observation with a 30-minute window, not a record of your entitlements, so a symbol nothing asked for during the window never appears, and a farm outage can put a symbol there that your account does hold. Subscriptions are managed in IBKR Client Portal, not in Canary.

On a first run with no IBKR app open, the badge is `OFFLINE` and the `Last attempt` row says what the daemon found and what to do next. The values below are synthetic:

```text
Canary  OFFLINE
Reason         Broker data connection unavailable

Session        auto-detect via 127.0.0.1 (tls=false), client 15
Market data    unavailable until connected
Daemon         v3.34.1, just started
TWS            not connected
Last attempt   no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497]
               (probe timeout 200ms); no TWS / IB Gateway / IBKR Desktop process
               found — start one and Canary will reconnect automatically

  Daemon log: ~/.local/state/ibkr/ibkr-daemon.log
Colors         Amber = row needs attention; headline = overall Canary status.
```

The `Last attempt` row is the daemon's own verdict on its last connect attempt, printed as written: no IBKR app running, a port that refuses the connection while an app runs (with the setting to check, or the `[gateway]` port to pin in `config.toml`), a handshake that did not finish, or a port that accepts connections and drops them. [Troubleshooting](troubleshooting.md) explains each message. The `Reason` row keeps the daemon's overall verdict.

When the daemon has no verdict from the gateway yet, `status` polls every 500 ms for up to 25 seconds before giving up, then prints the daemon log path, and the API checklist when the daemon has not yet said why the handshake stalls. When the gateway is not connected, the command exits 1; `canary status --json` carries the same verdict under `verdict` and the connect error under `last_error`, which makes it usable in a script.

No config file is needed for this. The daemon probes `4001` (Gateway live), `4002` (Gateway paper), `7496` (TWS live), and `7497` (TWS paper) in parallel with a 200 ms timeout each, takes the first responder in that order, lists any others as alternates, auto-detects the account from `managedAccounts`, and uses client ID `15`. Create `~/.config/ibkr/config.toml` only when you want to pin something; every key in the [configuration reference](../reference/config.md) is optional.

## Two things that surprise people

**The daemon starts itself.** Any daemon-backed command dials the Unix socket, and starts `canary daemon` in its own session when the socket is missing. It waits for the socket for 5 seconds plus 6 seconds per 40 MiB of `daemon.db`, up to four hours, because the daemon checks its whole database, and may upgrade it, before it accepts connections. A daemon that exits during start-up is reported at once. You never start it by hand. One daemon serves the CLI and every MCP host, so they share a single gateway connection and client ID. It exits once 15 minutes pass with no client connection and no background work in flight, which `[daemon].idle_timeout` controls.

**The first breadth build takes about 74 minutes.** S&P 500 breadth is computed locally from constituent daily bars, because IBKR does not redistribute S&P DJI's breadth indices on retail subscriptions. The shared historical-data limiter allows an initial 60 requests and then refills at 0.1 per second, so the remaining 443 names pace out to roughly 74 minutes on a cold cache. Nothing is broken while that runs: `canary status` shows `breadth:computing` and a `Background` row reading `refreshing rolling SPX breadth`. The snapshot is published only once coverage reaches 80% of the member count; below that the engine keeps the per-name windows it did fetch and tries again every 12 minutes until a pass reaches 80%, with no limit on the number of tries. The idle timer will not cut the build short: breadth counts as background work while a pass runs and while a retry is pending, so the daemon stays up until the snapshot is published. Later refreshes are quick, and the cache survives restarts.

## Where things land

| Path | Contents |
| --- | --- |
| `~/.local/bin/canary` | The binary, unless you set `CANARY_INSTALL_DIR` |
| `~/.config/ibkr/config.toml` | Optional pinned config. Absent by default |
| `~/.config/ibkr/policies/` | Policy files the installer writes from Canary's defaults, each headed `Canary defaults, not yet reviewed`: `rulebook-policy.toml`, `protection-policy.toml`, `opportunity-policy.toml`, and `risk-policy.toml`, whose capital numbers are placeholders for you to write and whose `[order_limits]` carries the per-order limits. Never replaced: the daemon migrates an existing file in place at its next start, after a backup, and keeps every setting you wrote; see [Trading policy](../understand/policy.md#policy-files-canary-writes-for-you) |
| `~/.local/state/ibkr/daemon.db` | Daemon-owned state: settings, journals, membership, evidence |
| `~/.local/state/ibkr/ibkr-daemon.log` | Daemon log. First place to look when something will not start |
| `~/.cache/ibkr/ibkr.sock` | The daemon socket, under `$XDG_RUNTIME_DIR/ibkr/` when that is set |
| `~/.cache/ibkr/` | Caches and update scratch space |

`XDG_CONFIG_HOME` relocates `config.toml`; `XDG_STATE_HOME` relocates daemon state and logs, and `XDG_CACHE_HOME` relocates caches and update scratch space. The socket uses `XDG_RUNTIME_DIR` when set, otherwise `HOME/.cache/ibkr`, independently of `XDG_CACHE_HOME`. `CANARY_CONFIG`, `CANARY_SOCKET`, and `CANARY_LOG` override the corresponding file individually.

Policy defaults remain under `HOME/.config/ibkr/policies`, even with `XDG_CONFIG_HOME` or `CANARY_CONFIG`. Rulebook, protection and opportunity policies have explicit `policy_file` overrides in `[rulebook]`, `[auto_trade]` and `[opportunities]`; the risk constitution uses its fixed default path. Changing the config path does not relocate policies.

## Next

Set up the shared IBKR statement source in [Set up broker
reporting](reporting.md) before relying on reconciliation, statement-derived
equity, or Canary Edge. Then wire an agent to the server in [Connect an MCP
host](hosts.md). An account in Hong Kong dollars, Singapore dollars or yen
reads [Hong Kong and Singapore accounts](ibkr-hong-kong-singapore.md) for the
calendars and the brief's timing. Keep the binary current with
[Updating](updating.md). Read
[Constrained orders and the trading build](../operate/orders.md) before
assuming anything about what this can and cannot send to the broker.
