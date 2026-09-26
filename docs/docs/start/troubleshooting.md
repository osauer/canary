# Troubleshooting

Updated: 2026-08-27

Symptom, cause, fix. Start with `canary status`: it prints one of four verdicts (`READY`, `STARTING`, `ATTENTION`, `OFFLINE`), waits up to 25 seconds for the API handshake to land, and exits 1 while the gateway is not connected.

## Gateway is ready, but Recon or Edge has no statement evidence

Gateway API access and broker reporting are separate. A new installation can
connect successfully while every statement-backed feature remains empty.
Complete [Set up broker reporting](reporting.md): create the eight-section XML
Activity Flex Query, enable the Flex Web Service, and configure its Query ID
and token file. Start diagnosis with `canary reporting status`; it separates
local files, broker reachability, retained evidence, absent sections,
present-empty sections, and proven missing fields.

Use the stable reason to choose the next step:

| Reason or state | What to do |
| --- | --- |
| `backfilling`, `report_not_ready`, `service_busy`, `response_invalid` | Leave the query unchanged and let Canary retry. IBKR is preparing the report or returned a temporary response Canary could not use. |
| `token_missing`, `token_invalid`, `token_expired` | Replace the protected token file; do not put the token in TOML or a shell argument. |
| `query_missing`, `query_invalid` | Check the configured Query ID and that the exact same account selection still exposes the query in Client Portal. |
| `service_inactive`, `ip_restricted` | Re-enable Flex Web Service or correct its IP restriction in Client Portal. |
| `absent_sections_unproved` | Open each named section in the saved query, choose the reported detail level and `Select All`, then save. Canary validates the next report automatically. If there was no matching activity, IBKR may omit an enabled empty section. |
| `action_required`, `flex_query_incomplete` | A real row proved fields missing. Edit the named section, choose `Select All`, then save. Canary validates the next report automatically; a new Query ID is optional. |
| broker code `1025` | IBKR does not document this code. Canary requests only ranges that end on a US session day, because a Saturday-ended range was observed to return it. If it appears on a session-day check, review Flex Web Service/query configuration; contact IBKR if it persists. Do not rotate a working query on a guessed meaning. |

An absent section was not returned; an empty section was returned with zero
rows. Neither proves which fields were selected. Never place a trade or move
cash merely to populate a report section.

## Every command prints a hint pointing at `canary status`

A command failed and the CLI appended this:

```
  hint: run `canary status` to see whether the daemon is still
        connecting (retry in a few seconds) or the gateway is
        down (start IB Gateway; check ~/.local/state/ibkr/ibkr-daemon.log).
```

The CLI adds those three lines whenever the daemon's error contains `gateway_unavailable`. It means the daemon is up and answering, and the broker connection behind it is not.

`canary status` separates the two cases. `STARTING` means the daemon has not published a connect error yet, so the handshake is still in flight; wait a few seconds and re-run. `OFFLINE` means it gave up and published a reason, which is printed in the status output.

## `no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497]`

Nothing answered a TCP probe on any of the four standard ports (4001 Gateway live, 4002 Gateway paper, 7496 TWS live, 7497 TWS paper) within the 200 ms per-port budget. The rest of the message tells you which case you are in.

If an IBKR app is running, the error names it and its PID. For TWS and IBKR Desktop it lists the three causes it cannot tell apart from outside the process: `'Enable ActiveX and Socket Clients'` unchecked under Global Configuration → API → Settings, a login that has not finished (2FA or a day-end dialog), or a non-default Socket port. IB Gateway has no such checkbox — its API is always on — so for the Gateway the message names the two that remain: it is still starting (the port opens once its login has completed), or it listens on a non-default Socket port. Pin the port in `~/.config/ibkr/config.toml` under `[gateway]` for that case.

If no app is running, the message says so and ends with `start one and Canary will reconnect automatically`. It will.

Discovery only probes the host in your config, which defaults to `127.0.0.1`. A gateway on another machine needs `host` set explicitly; it will never be found by probing.

## `accepts connections on 127.0.0.1:4001 and resets them before the API handshake`

The port is open, but whatever owns it ends every connection before a byte of the API handshake is exchanged. The probe's connection is closed (FIN) or reset (RST) before Canary has sent anything, and the daemon's own connect attempts fail at the dial (`connection reset by peer`), at the first socket option (`failed to set TCP_NODELAY: ... invalid argument`) or on the version-descriptor write (`broken pipe`), depending on which step the reset lands on. Canary reports all of these as one state: `canary status --json` shows `port_rejecting` as `gateway_phase` with the evidence under `port_rejection`, the text `TWS` row states the fact, and the daemon log carries the verdict once per change instead of a different socket error every cycle.

The message names the app that owns the port and what to verify in it. For IB Gateway, which has no API on/off switch: login and initialisation finished (2FA, the daily auto-restart, an "existing session" dialog), no incoming-connection prompt waiting in the Gateway window, and under Configure → Settings → API → Settings and Precautions that Trusted IPs covers the daemon's address with its action not set to reject and that the Socket port is the one probed. An OS application firewall in front of the Gateway can reset accepted connections the same way, before the Gateway ever sees them (macOS: System Settings → Network → Firewall). For TWS the `'Enable ActiveX and Socket Clients'` checkbox is listed first.

A rejecting port stays a candidate. When another listener holds its connection — a ready TWS beside a Gateway that drops — the daemon tries that one first.

## `not responding to TWS handshake within 12s`

The full text is `gateway HOST:PORT not responding to TWS handshake within 12s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on`. TCP connected and the API handshake got no reply, which almost always means the API socket is disabled rather than absent.

`canary status` prints the same checklist while a handshake is in flight:

```
    Configure → Settings → API → Settings → 'Enable ActiveX and Socket Clients'
    Trusted IPs include 127.0.0.1 (or empty)
    Login fully completed (not paused at 2FA)
```

A related message, `none of N discovered endpoint(s) completed TWS handshake (tried ...)`, means several apps accepted TCP and none completed the handshake. It lists every endpoint tried, which is usually a stale Gateway window alongside a freshly logged-in TWS. Quit the one you do not want.

## `daemon socket did not appear within 5s`

The CLI autospawns `canary daemon` when the socket is missing and gives it a 5 second budget to reach its accept loop. This message means it did not, so the daemon crashed or wedged during startup.

The error carries what the CLI could find out. When a lock holder is alive it adds `daemon PID N holds PATH but never opened the socket` and `if it's stuck, run: kill N`. When the last daemon log line is readable it appends `last daemon log: ...`. Read the full log at `~/.local/state/ibkr/ibkr-daemon.log` before killing anything.

If the daemon log ends with a `start:` line about `daemon authority`,
`daemon.db` or the preview key, a startup integrity check failed and the
daemon stays stopped on purpose; follow
[Recover from a failed startup check](../internals/storage.md#recover-from-a-failed-startup-check).

## The daemon stops at start with a config error

The daemon log or the `canary daemon` output reads `config ...: ... The
[gateway] pins and [trading].mode decide which broker account Canary acts on`.
Canary could not read the settings that pick the broker account: the file
itself, a `[gateway]` pin (`host`, `port`, `account`, `client_id`, `tls`) or
`[trading].mode`. It also stops when `[gateway]` or `[trading]` carries a key it
does not know, since that may be a misspelled pin, or when a pin name sits in
another section. The message names the key and the line. Fix it in
`~/.config/ibkr/config.toml` and run `canary restart`.

Any other part of the file that cannot be read never stops the daemon. It runs
on Canary's default for that part and says so: `canary status` shows
`config:degraded` with the keys, the brief carries a `config` row, and the
alert inbox a "Config file needs attention" notice. While `[trading]`,
`[auto_trade]` or `[rulebook]` runs on defaults, pre-authorised protection
submission pauses; manual exits, trims and reads continue. Fix the file and
run `canary restart` to clear it.

## A second daemon will not start

It is not supposed to. The daemon takes a non-blocking exclusive `flock` on `ibkr.lock` beside the socket before it touches the gateway. A second one finds the lock contended, logs `Another daemon is already running for socket PATH; exiting cleanly`, and exits 0.

A socket file left behind by a crashed daemon is not a problem either. The new daemon dial-probes it: no answer means stale, and it is removed automatically. A live answer while the lock is held is the impossible case, and it fails loudly with `socket PATH already serving despite holding lock; refusing to evict`.

To replace a running daemon deliberately, use `canary restart`. It stops the old process, starts a new one from the current binary, and reports gateway health.

## `CLI version X does not match daemon version Y`

The full warning:

```
Canary: warning: CLI version X does not match daemon version Y — run `canary restart` to pick up the new binary.
```

You installed a new binary and the old daemon is still resident. Run `canary restart`.

The check is skipped for `canary status`, and it stays quiet when either side stamps `dev`, so a working tree does not warn against itself every run.

## Quotes say `delayed`, `frozen`, or `delayed-frozen`

Usually correct behavior, not a fault. Market data is real-time wherever your IBKR market-data subscriptions cover it and delayed where they don't.

| `DATA` value | What it means |
| --- | --- |
| `live` | Real-time ticks; your subscription covers the symbol |
| `delayed` | `15-20 min delayed quotes (entitlement-limited)` |
| `frozen` | `markets closed; only the last-recorded quote is available` |
| `delayed-frozen` | `markets closed; showing yesterday's close` |

The daemon asks the gateway for market-data type 2 (frozen-aware) on every connect, deliberately: type 1 (pure live) can leave snapshot requests hanging when the market is closed, while type 2 returns the last-known price instead. If you expected `live` on a symbol and got `delayed`, the fix is a market-data subscription in IBKR's account management, not a change here.

The gateway sends one snapshot in frozen mode and never streams. In v3, inspect
the relevant position or app row and its session context; the standalone quote
watch command has been retired.

## A held name stopped updating after an overnight gateway reset

IBKR's nightly reset can make the gateway answer "no security definition" for everything for a while, and names asked for during that window get marked inactive. The mark is a cache rather than a verdict: it lives in memory only, expires after 12 hours, and is rebuilt from scratch on every reconnect.

The daemon also remembers that answer so it does not ask again on every read. The connector holds off re-resolving the name for one minute, doubling to 30 minutes, until a resolution succeeds. A quote's closed-market context keeps the refusal for 30 minutes. The allocation tables keep it for the rest of the broker session, and the breadth sweep leaves the name out until the next completed session.

So the fix is `canary restart`. It restarts the daemon and clears all of these at once; a gateway reconnect on its own clears only the inactive mark and the allocation memo. A separate 30 minute retry window covers entitlement rejections, which are a different failure with the same symptom.

## Breadth shows `0.0 %`, or stays `computing`

A cold or still-computing engine has no reading yet. Use `canary status --json`
and the daily brief to distinguish `cold`, `computing`, `ready`, and `degraded`.

A `ready` reading is one specific session's close. The brief retains that date
and marks it stale when it is no longer the latest completed session. The daemon
keeps serving last-good rather than publishing a partial replacement; `canary
status` carries the subsystem cause and retry state.

Computing for a long time is expected on a fresh daemon. IBKR's historical-data pacing caps the 503-name fan-out at about 6 names a minute, so the first build takes about 74 minutes. A pass that lands below 80% constituent coverage publishes nothing and retries every 12 minutes, up to 15 times, which can stretch the wait considerably. After that the state falls back to `cold` and the normal once-daily refresh takes over, 35 minutes after the official session close.

## Gamma says `no data yet (cold cache)` or `computing`

Both are states rather than failures, and both exit 0. Only the `error` status exits 1.

Cold means no usable result exists and nothing is computing. The daemon prewarms gamma after gateway startup and refreshes behind the served value after a 15-minute soft TTL during regular US option hours. Outside those hours automatic refresh is not due at all, so an off-hours cold cache stays cold on purpose rather than running a heavy option-chain fan against a closed market.

`canary status --json` and the app expose the computing state, progress, and
typed failure without the retired force/no-wait command controls.

During regular US option hours, an IBKR 354 on SPY or SPX triggers one delayed
retry for that underlying. A successful result says `15m delayed` in text and
carries `data_type: "delayed"` in JSON. Canary accepts it only when the spot and
the option model ticks share that delayed clock; it does not turn a delayed
quote into real-time data. If the delayed option model ticks also fail, gamma
stays unavailable and the error still names IBKR 354.

## Claude Desktop does not list the tools

Quit Claude Desktop completely and reopen it. A closed window is not a quit, and the MCP server is only respawned on a real relaunch. This is also the step people miss after reinstalling the bundle.

`canary setup claude-desktop` writes an `mcpServers.canary` entry pointing at the resolved absolute path of the running binary, into `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS or `%APPDATA%\Claude\claude_desktop_config.json` on Windows. It backs up any existing config first. It refuses to write over a file that is not valid JSON, and prints the path so you can fix or delete it. [Connect an MCP host](hosts.md) covers the wiring for each host in full.

If the tools still do not appear, read Claude Desktop's own log at `~/Library/Logs/Claude/mcp-server-canary.log`.

An MCPB install is different: it carries its own embedded binary, and `canary update` does not touch it. [Updating](updating.md) covers reinstalling the bundle.

## Where the logs and local files are

| Path | What it holds |
| --- | --- |
| `~/.local/state/ibkr/ibkr-daemon.log` | Daemon diagnostics: discovery, handshakes, reconnects, subsystem errors. Override with `CANARY_LOG` |
| `~/Library/Logs/Claude/mcp-server-canary.log` | Claude Desktop's view of the MCP server it launched |
| `$XDG_RUNTIME_DIR/ibkr/ibkr.sock`, else `~/.cache/ibkr/ibkr.sock` | Daemon IPC socket. Override with `CANARY_SOCKET` |
| `ibkr.lock`, beside the socket | Single-instance lock, holding the daemon's PID |
| `~/.config/ibkr/config.toml` | Optional persistent config, including the `[gateway]` pins |

The daemon log is a diagnostic stream, not an audit trail. It is rolled aside to `.1` at boot once it passes 64 MiB, and one generation is kept. Captured frames and logs can carry account-sensitive data; read [SECURITY.md](https://github.com/osauer/canary/blob/main/SECURITY.md) before sharing one.
