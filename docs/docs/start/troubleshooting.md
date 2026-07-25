# Troubleshooting

Updated: 2026-07-25 11:33 CEST

Symptom, cause, fix. Start with `ibkr status`: it prints one of four verdicts (`READY`, `STARTING`, `ATTENTION`, `OFFLINE`), waits up to 25 seconds for the API handshake to land, and exits 1 while the gateway is not connected.

## Every command prints a hint pointing at `ibkr status`

A command failed and the CLI appended this:

```
  hint: run `ibkr status` to see whether the daemon is still
        connecting (retry in a few seconds) or the gateway is
        down (start IB Gateway; check ~/.local/state/ibkr/ibkr-daemon.log).
```

The CLI adds those three lines whenever the daemon's error contains `gateway_unavailable`. It means the daemon is up and answering, and the broker connection behind it is not.

`ibkr status` separates the two cases. `STARTING` means the daemon has not published a connect error yet, so the handshake is still in flight; wait a few seconds and re-run. `OFFLINE` means it gave up and published a reason, which is printed in the status output.

## `no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497]`

Nothing answered a TCP probe on any of the four standard ports (4001 Gateway live, 4002 Gateway paper, 7496 TWS live, 7497 TWS paper) within the 200 ms per-port budget. The rest of the message tells you which case you are in.

If an IBKR app is running, the error names it and its PID, then lists the three causes it cannot tell apart from outside the process: `'Enable ActiveX and Socket Clients'` unchecked under Global Configuration → API → Settings, a login that has not finished (2FA or a day-end dialog), or a non-default Socket port. Pin the port in `~/.config/ibkr/config.toml` under `[gateway]` for the third case.

If no app is running, the message says so and ends with `start one and ibkr will reconnect automatically`. It will.

Discovery only probes the host in your config, which defaults to `127.0.0.1`. A gateway on another machine needs `host` set explicitly; it will never be found by probing.

## `not responding to TWS handshake within 12s`

The full text is `gateway HOST:PORT not responding to TWS handshake within 12s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on`. TCP connected and the API handshake got no reply, which almost always means the API socket is disabled rather than absent.

`ibkr status` prints the same checklist while a handshake is in flight:

```
    Configure → Settings → API → Settings → 'Enable ActiveX and Socket Clients'
    Trusted IPs include 127.0.0.1 (or empty)
    Login fully completed (not paused at 2FA)
```

A related message, `none of N discovered endpoint(s) completed TWS handshake (tried ...)`, means several apps accepted TCP and none completed the handshake. It lists every endpoint tried, which is usually a stale Gateway window alongside a freshly logged-in TWS. Quit the one you do not want.

## `daemon socket did not appear within 5s`

The CLI autospawns `ibkr daemon` when the socket is missing and gives it a 5 second budget to reach its accept loop. This message means it did not, so the daemon crashed or wedged during startup.

The error carries what the CLI could find out. When a lock holder is alive it adds `daemon PID N holds PATH but never opened the socket` and `if it's stuck, run: kill N`. When the last daemon log line is readable it appends `last daemon log: ...`. Read the full log at `~/.local/state/ibkr/ibkr-daemon.log` before killing anything.

## A second daemon will not start

It is not supposed to. The daemon takes a non-blocking exclusive `flock` on `ibkr.lock` beside the socket before it touches the gateway. A second one finds the lock contended, logs `Another daemon is already running for socket PATH; exiting cleanly`, and exits 0.

A socket file left behind by a crashed daemon is not a problem either. The new daemon dial-probes it: no answer means stale, and it is removed automatically. A live answer while the lock is held is the impossible case, and it fails loudly with `socket PATH already serving despite holding lock; refusing to evict`.

To replace a running daemon deliberately, use `ibkr restart`. It stops the old process, starts a new one from the current binary, and reports gateway health.

## `CLI version X does not match daemon version Y`

The full warning:

```
ibkr: warning: CLI version X does not match daemon version Y — run `ibkr restart` to pick up the new binary.
```

You installed a new binary and the old daemon is still resident. Run `ibkr restart`.

The check is skipped for `ibkr status`, and it stays quiet when either side stamps `dev`, so a working tree does not warn against itself every run.

## Quotes say `delayed`, `frozen`, or `delayed-frozen`

Usually correct behavior, not a fault. Market data is real-time wherever your IBKR market-data subscriptions cover it and delayed where they don't.

| `DATA` value | What it means |
| --- | --- |
| `live` | Real-time ticks; your subscription covers the symbol |
| `delayed` | `15-20 min delayed quotes (entitlement-limited)` |
| `frozen` | `markets closed; only the last-recorded quote is available` |
| `delayed-frozen` | `markets closed; showing yesterday's close` |

The daemon asks the gateway for market-data type 2 (frozen-aware) on every connect, deliberately: type 1 (pure live) can leave snapshot requests hanging when the market is closed, while type 2 returns the last-known price instead. If you expected `live` on a symbol and got `delayed`, the fix is a market-data subscription in IBKR's account management, not a change here.

`ibkr quote SYM --watch` exits by itself on frozen data:

```
  stream ended — frozen data is snapshot-only. Use `ibkr quote SYM` for one-shots.
```

The gateway sends one snapshot in frozen mode and never streams, so there is nothing to wait for.

## A held name stopped updating after an overnight gateway reset

IBKR's nightly reset can make the gateway answer "no security definition" for everything for a while, and names asked for during that window get marked inactive. The mark is a cache rather than a verdict: it lives in memory only, expires after 12 hours, and is rebuilt from scratch on every reconnect.

So the fix is a reconnect. `ibkr restart` gets you one immediately instead of waiting out the TTL. A separate 30 minute retry window covers entitlement rejections, which are a different failure with the same symptom.

## Breadth shows `0.0 %`, or stays `computing`

`ibkr breadth` renders the snapshot fields without checking the state first, so a cold or still-computing engine prints `0.0 %` for both moving-average rows rather than saying it has no data yet. That is an unset field, not a market reading.

Use `ibkr breadth --json` and check `state`, which is one of `cold`, `computing`, `ready`, or `degraded`. `ibkr status` also carries a breadth subsystem row that reports `S&P 500 breadth refresh is running or waiting to retry` while a refresh is live.

Computing for a long time is expected on a fresh daemon. IBKR's historical-data pacing caps the 503-name fan-out at about 6 names a minute, so the first build takes about 74 minutes. A pass that lands below 80% constituent coverage publishes nothing and retries every 12 minutes, up to 15 times, which can stretch the wait considerably. After that the state falls back to `cold` and the normal once-daily refresh takes over, 35 minutes after the official session close.

## Gamma says `no data yet (cold cache)` or `computing`

Both are states rather than failures, and both exit 0. Only the `error` status exits 1.

Cold means no usable result exists and nothing is computing. The daemon prewarms gamma after gateway startup and refreshes behind the served value after a 15-minute soft TTL during regular US option hours. Outside those hours automatic refresh is not due at all, so an off-hours cold cache stays cold on purpose rather than running a heavy option-chain fan against a closed market. `ibkr gamma --force` starts one anyway, which is mostly a troubleshooting move.

Computing prints the start time, an ETA, and a progress percentage. Re-running `ibkr gamma` blocks on the result again; `ibkr gamma --no-wait` returns the current status immediately instead.

## Claude Desktop does not list the tools

Quit Claude Desktop completely and reopen it. A closed window is not a quit, and the MCP server is only respawned on a real relaunch. This is also the step people miss after reinstalling the bundle.

`ibkr setup claude-desktop` writes an `mcpServers.ibkr` entry pointing at the resolved absolute path of the running binary, into `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS or `%APPDATA%\Claude\claude_desktop_config.json` on Windows. It backs up any existing config first. It refuses to write over a file that is not valid JSON, and prints the path so you can fix or delete it. [Connect an MCP host](hosts.md) covers the wiring for each host in full.

If the tools still do not appear, read Claude Desktop's own log at `~/Library/Logs/Claude/mcp-server-ibkr.log`.

An MCPB install is different: it carries its own embedded binary, and `ibkr update` does not touch it. [Updating](updating.md) covers reinstalling the bundle.

## Where the logs and local files are

| Path | What it holds |
| --- | --- |
| `~/.local/state/ibkr/ibkr-daemon.log` | Daemon diagnostics: discovery, handshakes, reconnects, subsystem errors. Override with `IBKR_LOG` |
| `~/Library/Logs/Claude/mcp-server-ibkr.log` | Claude Desktop's view of the MCP server it launched |
| `$XDG_RUNTIME_DIR/ibkr/ibkr.sock`, else `~/.cache/ibkr/ibkr.sock` | Daemon IPC socket. Override with `IBKR_SOCKET` |
| `ibkr.lock`, beside the socket | Single-instance lock, holding the daemon's PID |
| `~/.config/ibkr/config.toml` | Optional persistent config, including the `[gateway]` pins |

The daemon log is a diagnostic stream, not an audit trail. It is rolled aside to `.1` at boot once it passes 64 MiB, and one generation is kept. Captured frames and logs can carry account-sensitive data; read [SECURITY.md](https://github.com/osauer/ibkr/blob/main/SECURITY.md) before sharing one.
