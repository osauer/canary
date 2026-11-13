# Troubleshooting

Updated: 2026-11-13 04:30 CET

Each section below starts from a symptom, names the cause and gives the fix. Start with `canary status`: it prints one of four verdicts (`READY`, `STARTING`, `ATTENTION`, `OFFLINE`), waits up to 25 seconds for the API handshake to land, and exits 1 while the gateway is not connected. For missing or degraded inputs, run `canary data health`.

## The `Gateway setup` row says a setting differs

A few seconds after each connect, the daemon reads the application's API
settings once, on a separate short-lived connection (the same read as
`--gateway-diagnostics` below), and `canary status` prints the result as the
`Gateway setup` row. `as expected` means every setting Canary depends on is as
it needs it. Otherwise the row carries one short sentence per difference, each
naming the setting as TWS or IB Gateway labels it and what it costs you:

```text
Gateway setup  Master API client ID is not 15: orders and fills from TWS or
               other apps stay unseen. · read 08:36
               Open orders are not downloaded at connect: orders placed
               elsewhere stay unseen.
```

The settings it checks, in Configure → Settings → API → Settings unless noted:
`Read-Only API` (must be off while `[trading].mode` is paper or live), `Master
API client ID` (Canary's client id), `Download open orders on connection` (on),
`Reject messages above maximum allowed message rate` (off: on, three rejections
cut the session), the trusted-IP list when the daemon dials a loopback address
(`Allow connections from localhost only` on, or `127.0.0.1` trusted; otherwise
every reconnect waits for a click), the API log level (`Detail` grows fast) and
`Include market data in API log file` (off), and under Lock and Exit the daily
`Auto restart` (a log-off leaves the Gateway signed out). A setting the broker
did not report is not a difference. `not read yet` means the read has not landed
or could not be made; the reason follows it. Canary never changes a setting:
fix it in the application, and the row updates at the next connect or
`canary status --gateway-diagnostics`.

IB Gateway writes its logs and settings file encrypted (`*.ibgzenc`), and IBKR
documents no setting that turns that off. Plain-text copies come from the
Gateway's File menu (`API Logs` or `Gateway Logs` → `Export Today Logs…`).
The `Automatic Encryption` menu item is not about logs: it concerns the FIX
CTCI connection and does nothing in API mode.

## Check API settings after changing TWS or Gateway

Run `canary status --gateway-diagnostics` (add `--json` for all fields).
The normal connection keeps its tested protocol ceiling of 203. The diagnostic
opens one separate connection to the same endpoint, negotiates up to 219,
reads API settings, and closes within five seconds. It does not restart TWS,
change ports, update settings, bind orders, or submit broker instructions.

`observed` means the broker returned settings. `unsupported` means that session
could not negotiate the required config-read protocol; check settings in TWS
or Gateway manually. `unavailable` or `rejected` carries a reason. An omitted
setting means **not reported**, not off. The negotiated protocol is not the
installed application's version number.

The output includes read-only API mode, automatic open-order download,
reconnect order retention, US-stock volume-in-lots mode, rate-limit
rejection, the master API client id, localhost-only mode with whether a
loopback address is trusted (the trusted-IP list itself is never kept), the API
message log with its level, the order-precaution bypasses that are on, and the
Lock and Exit schedule as the application reports it (its own clock; the
broker sends no time zone). These are observations, not changes or permission
to trade. Volume values preserve the broker's decimal quantity; Canary does not
guess shares versus lots from the value's size. Retained broker orders are
reconciled by identity; an uncertain send is never automatically replayed.

When switching applications, compare the Session endpoint with the socket port
in the selected application. An explicit `[gateway].port` pin remains binding:
TWS commonly uses 7496/7497 and Gateway 4001/4002. Keep the working endpoint
until the intended application's login, API socket and settings are ready.
The config diagnostic does not establish a restart timezone or alter the
operator-declared restart schedule.

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

`canary status` separates the two cases. `STARTING` means the daemon has not published a connect error yet, so the handshake is still in flight; wait a few seconds and re-run. `OFFLINE` means it gave up and published a reason, which the `Last attempt` row prints as written.

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

## `daemon socket did not appear within …` or `daemon pid N exited during startup`

When the socket is missing, the CLI starts `canary daemon` itself and waits for the socket to appear. The wait grows with the size of the daemon's state, because the daemon checks all of it, and may upgrade it, before it accepts connections. It is 5 seconds plus 6 seconds for every 40 MiB of `daemon.db`, capped at four hours, and the message prints the wait it used, for example `daemon socket did not appear within 2m41s` for a 1 GiB database. This message means the daemon was still running at the end of that wait without opening the socket, so it is wedged during startup.

A daemon that exits during startup is reported at once, without waiting out the budget: `daemon pid N exited during startup without opening PATH`.

The error carries what the CLI could find out. When a lock holder is alive it adds `daemon PID N holds PATH but never opened the socket` and `if it's stuck, run: kill N`. When the last daemon log line is readable it appends `last daemon log: ...`. Read the full log at `~/.local/state/ibkr/ibkr-daemon.log` before killing anything.

If the message says the daemon exited and the `last daemon log` line starts
with `start:` and names `daemon authority`, `daemon.db` or the preview key, a
startup integrity check failed and the daemon stays stopped on purpose; follow
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

## `daemon authoritative storage reported lost_write; broker writes are blocked`

`canary trading status` and order commands report `daemon_storage_unavailable`
with this message; the code in it is `lost_write` or `outside_opener`. `canary
status` shows `Subsystem issue: storage unavailable: authoritative persistence
is latched fail-closed`. The daemon found that its database no longer matched
what it had just saved, or that another program had opened the database, and it
stopped sending orders rather than act on a state it cannot trust. Reading still
works.

- `lost_write`: the daemon saved a change and then read back an older state.
- `outside_opener`: another program got past the daemon's lock on `daemon.db`,
  which is rare.

Either way, the stop holds until a restart:

1. Orders already at the broker stay there. Change or cancel them in TWS or
   another IBKR app meanwhile.
2. Close any other program that has `daemon.db` open, such as a database browser
   or a script.
3. The daemon copies its database files to a folder under `damaged/` in its
   state folder (`~/.local/state/ibkr/` by default). Wait until the daemon log,
   `ibkr-daemon.log` in the same state folder, names that folder: a restart
   before then loses the copy.
4. Run `canary restart`, then `canary status`.

The copy contains your account data, so delete it when you are done with it.
[Storage](../internals/storage.md) explains both checks.

## `database is locked` when another program opens `daemon.db`

This is expected. The daemon holds `daemon.db` exclusively while it runs, so
`sqlite3` and similar tools are refused and the daemon carries on. Backup and
sync tools that only copy the files do not disturb it. To read the database
yourself, use `canary` commands with `--json`, or stop the daemon first with
`canary stop --daemon`.

## `bad_request` with `decode params: json: unknown field`

The daemon refuses a request that carries a field its method does not declare;
the message names the field. The usual causes are a `canary` command or MCP
plugin from a different version than the daemon, or a misspelt field in a script
of your own that calls the daemon. Update the command, the plugin and the daemon
together, run `canary restart`, and correct the field in your script.

## `CLI version X does not match daemon version Y`

The full warning:

```
Canary: warning: CLI version X does not match daemon version Y — run `canary restart` to pick up the new binary.
```

You installed a new binary and the old daemon is still resident. Run `canary restart`.

The check is skipped for `canary status`, and it stays quiet when either side stamps `dev`, so a working tree does not warn against itself every run.

## Quotes say `delayed`, `frozen`, or `delayed-frozen`

Usually correct behaviour, not a fault. Market data is real-time wherever your IBKR market-data subscriptions cover it and delayed where they don't.

| `DATA` value | What it means |
| --- | --- |
| `live` | Real-time ticks; your subscription covers the symbol |
| `delayed` | `15-20 min delayed quotes (entitlement-limited)` |
| `frozen` | `markets closed; only the last-recorded quote is available` |
| `delayed-frozen` | `markets closed; showing yesterday's close` |

The daemon asks the gateway for market-data type 2 (frozen-aware) on every connect, deliberately: type 1 (pure live) can leave snapshot requests hanging when the market is closed, while type 2 returns the last-known price instead. If you expected `live` on a symbol and got `delayed`, the fix is a market-data subscription in IBKR's account management, not a change here.

The gateway sends one snapshot in frozen mode and never streams. To see a
quote's context, inspect the relevant position or app row and its session.

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

Computing for a long time is expected on a fresh daemon. IBKR's historical-data pacing caps the 503-name fan-out at about 6 names a minute, so the first build takes about 74 minutes. A pass that lands below 80% constituent coverage publishes nothing. The engine keeps the names it did fetch and tries again every 12 minutes until a pass reaches 80%; there is no limit on the number of tries. While the broker connection or its historical-data farm is unusable, it checks every 60 seconds instead. Once a snapshot is published, the normal once-daily refresh takes over, 35 minutes after the official session close.

## Gamma says `no data yet (cold cache)` or `computing`

Both are states rather than failures, and both exit 0. Only the `error` status exits 1.

Cold means no usable result exists and nothing is computing. The daemon prewarms gamma after gateway startup and refreshes behind the served value after a 15-minute soft TTL during regular US option hours. Outside those hours automatic refresh is not due at all, so an off-hours cold cache stays cold on purpose rather than running a heavy option-chain fan against a closed market.

`canary status --json` and the app show the computing state, its progress, and
the reason for a failure.

During regular US option hours, an IBKR 354 on SPY or SPX triggers one delayed
retry for that underlying. A successful result says `15m delayed` in text and
carries `data_type: "delayed"` in JSON. Canary accepts it only when the spot and
the option model ticks share that delayed clock; it does not turn a delayed
quote into real-time data. If the delayed option model ticks also fail, gamma
stays unavailable and the error still names IBKR 354.

## Report a bug with `canary feedback`

```bash
canary feedback --surface cli --error "the message you saw"
```

It prints a short diagnostic report and a link. The link opens the bug report
form on GitHub with the report filled in; the "Where did this happen?"
dropdown is the one field you pick yourself, because GitHub does not fill a
dropdown from a link. Nothing is sent until you read the form and submit.
`--surface` names where it happened (`cli`, `mcp`, `app`, `install`) and is
the option to pick; `--error` is the text to include. The report carries the Canary
version and build, your OS and architecture, whether the daemon answered,
the gateway kind and API server version, the data health summary as source
coverage, and your error text after redaction: account ids, balances,
holdings, order references, host names, user names and home paths are
replaced with bracketed words. It works without a daemon running and starts
none; the report then says so. An error message that ends with
``run `canary feedback` `` is a Canary defect, not a configuration problem,
and this is the step it asks for. An AI assistant connected over MCP has the
same report as the `canary_feedback` tool. Context that is private or
broker-sensitive goes [by email](https://osauer.dev/canary/feedback/#private)
instead of a public issue.

## Claude Desktop does not list the tools

Quit Claude Desktop completely and reopen it. A closed window is not a quit, and the MCP server is only respawned on a real relaunch. This is also the step people miss after reinstalling the bundle.

`canary setup claude-desktop` writes an `mcpServers.canary` entry pointing at the validated absolute launcher path of the running binary, into `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS. It backs up any existing config first. It refuses to write over a file that is not valid JSON, and prints the path so you can fix or delete it. [Connect an MCP host](hosts.md) covers the wiring for each host in full.

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
