# Updating

Updated: 2026-07-21 01:15 CEST

Four things can affect data freshness: the **binary** (`ibkr` itself), the **Claude Desktop MCPB** when installed through Desktop Extensions, the **S&P 500 constituent list** the breadth indicator uses, and the embedded **official market calendars**. They update independently because they have different sources and cadences.

## Updating the binary: `ibkr update`

Once you're on v1.0.0 or later, the next upgrade is one command:

```sh
ibkr update            # fetch latest, prompt to restart the local app/daemon stack
```

The CLI checks the [GitHub `/releases/latest`](https://api.github.com/repos/osauer/ibkr/releases/latest) endpoint, matches your OS/arch against the published tarballs, verifies the **PGP signature on `SHA256SUMS`** against the maintainer's public key embedded in your current `ibkr` binary, then SHA-verifies the tarball and atomically replaces `~/.local/bin/ibkr`. The prior binary is stashed as `~/.local/bin/ibkr.bak` for one-step rollback (`mv ~/.local/bin/ibkr.bak ~/.local/bin/ibkr`).

At the end, the updater can restart the local processes that were already
running. It uses the same app-first stack path as `ibkr restart`: a running app
is restarted and verified on the installed binary before the daemon is touched.
Processes that were stopped before the update remain stopped.

## Restarting local processes: `ibkr restart`

Use `ibkr restart` when you changed daemon-loaded config, installed a new binary outside `ibkr update`, or want to clear stale gateway connection state:

```sh
ibkr restart
```

On the default socket, the command first refreshes an already-running `ibkr
app` host and verifies the replacement. It preserves unsupervised app flags
such as `--remote`; a launchd-owned app is restarted from its loaded plist only
after the plist executable is verified as the current installed binary. Only
after that app stage succeeds does the command verify the daemon pidfile
holder, stop it, start a fresh daemon, and report the new PID and gateway
health. If no app host is running, it leaves the app stopped. Plain `ibkr
restart` starts the daemon when none was running.

The two process stages are ordered but not transactional. An app-stage failure
leaves the daemon untouched. A later daemon-stage failure does not roll the
already-restarted app back; fix the reported stage and rerun `ibkr restart`.

`--force` is an explicit fallback for an app or daemon that ignores SIGTERM:

```sh
ibkr restart --force          # escalate to SIGKILL only after graceful timeout
ibkr restart --timeout 30s    # wait longer before failing or forcing
ibkr restart --json           # scriptable result: daemon health plus any refreshed app process
ibkr restart --app            # app-only restart/start for the HyperServe app process
ibkr app restart              # same app-only restart path, grouped under app commands
```

JSON mode is for automation and CI. It avoids text parsing and includes the post-start `status.health` payload so a script can distinguish "process restarted but gateway offline" from "restart failed." When an app host was running, JSON also includes an `app` object with its old/new PID and preserved args. If that app stage succeeds but the daemon stage fails, the command exits non-zero and still emits the partial structured result with the successful app stage and `started: false` for the daemon.

When `IBKR_SOCKET` selects a non-default daemon scope, app process discovery
cannot prove which scope a found app belongs to. The decision is therefore made
before any process mutation: the app is left untouched and only the explicitly
selected daemon is restarted. The result marks the app as
`reason: "socket_overridden"`. If that stack also needs an app restart, run
`ibkr restart --app` as a separate, explicit, non-atomic operation.

`ibkr restart` restarts the shared daemon that CLI commands and MCP tools dial, plus any currently running local or remote app host. It does not restart the `ibkr mcp` stdio process itself; that process is owned by Claude Desktop, Cursor, Continue, or whichever MCP host launched it. Relaunch the host when you need it to respawn MCP from a new binary or MCPB bundle.

`ibkr restart --app` targets only the long-running `ibkr app` HyperServe process. It finds a local `ibkr app` server process, sends SIGTERM so HyperServe can shut down gently, preserves the old app flags such as `--addr`, `--public-url`, or `--remote`, and then starts the app again. A same-argv respawn is accepted only when it uses the current executable. When launchd owns the app, restart uses the loaded job's executable and plist arguments; change those arguments by rewriting and reloading the plist, not with restart flag overrides. If no app or supervisor is present, this explicit app-only form starts `ibkr app` with default/env configuration.

In remote mode the hosted Cloudflare Worker is not restarted or redeployed by
this command. The local app process restarts its outbound relay connector and
reuses the persisted relay route while that route remains inside the relay TTL,
so paired phones and Home Screen installs can keep opening the same relay
origin across ordinary app restarts.

### Release integrity

From v1.0.0 onward, every release ships `SHA256SUMS.asc`: a PGP detached signature over `SHA256SUMS`, produced by the maintainer's Ed25519 key (fingerprint `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D`). The public key is embedded in every ibkr binary, so `ibkr update` verifies the next release using a key your already-installed binary carries, with no network bootstrap an attacker could swap.

Releases that publish an MCP Bundle include both `ibkr-vX.Y.Z.mcpb` and the stable latest-download asset `ibkr.mcpb` in `SHA256SUMS`. The MCP Registry publish artifact also records the versioned MCPB file's SHA-256 in `server.json` as `fileSha256`.

The MCPB container itself is not yet code-signed. Treat MCPB release integrity as signed-checksum and registry-hash based unless `mcpb verify ibkr-vX.Y.Z.mcpb` succeeds for a future release.

`ibkr update` **refuses** any release missing the signature, and any release whose signature does not verify against the embedded key. There is no `--insecure` flag. If you ever need to debug a verification failure, the underlying error is printed verbatim and the manual verification steps are in [SECURITY.md → Release integrity](../../../SECURITY.md#release-integrity-v100).

### Headless / scripted use

In non-interactive contexts (cron, systemd timers, CI, stdin-redirected shells) the `[Y/n]` prompt would block. Pass an explicit restart decision:

```sh
ibkr update --restart        # restart a running app first, then a running daemon
ibkr update --no-restart     # leave both processes as-is; print a restart-pending hint
```

Running `ibkr update` from a non-TTY *without* either flag exits non-zero with `ambiguous in non-interactive mode` and does not install. This is deliberate: silent default-to-N would be a footgun for systemd timers expecting auto-restart.

`ibkr update --restart` preserves liveness: it does not wake an app or daemon
that was absent. If both were stopped, the binary is updated and the desk stays
stopped. If a restart stage fails, the same ordered, non-atomic rules above
apply and the command points back to explicit `ibkr restart` recovery.

### Other flags

```sh
ibkr update --check          # dry-run: print "would install vX.Y.Z", exit 0
ibkr update --force          # re-install latest even if same version (corrupt-binary recovery)
```

`--check` exits 0 whether or not an update is available; only fetch failures exit non-zero. So `ibkr update --check && ibkr update` is the idiomatic confirm-then-install pattern.

### Pre-v1.0.0 binaries

`ibkr update` only exists from v1.0.0 onward. Earlier installs upgrade once manually (download the tarball from [releases](https://github.com/osauer/ibkr/releases), extract, run `make install`), then carry forward with `ibkr update`.

## Updating Claude Desktop MCPB installs

Claude Desktop MCPB installs carry their own embedded `ibkr` binary. `ibkr update` updates shell-managed installs such as `~/.local/bin/ibkr`; it does not replace a binary embedded inside Claude Desktop's extension store.

To update a Claude Desktop MCPB install, download the latest bundle and reinstall it in Claude Desktop:

<https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb>

After reinstalling, fully quit Claude Desktop and reopen it so it respawns the MCP server from the updated bundle.

## Updating the S&P 500 list: automatic

The daemon refreshes the constituent list from [Wikipedia's "List of S&P 500 companies"](https://en.wikipedia.org/wiki/List_of_S%26P_500_companies) on three triggers, all converging on one shared fetch:

- **Daily at 02:30 ET**: between midnight NY-session-key roll and 04:00 ET pre-market open. Catches reconstitution effective dates that Wikipedia editors typically have ready by the morning of the change.
- **On daemon startup** if the persisted membership snapshot is from a NY trading date earlier than today (covers laptop-closed-at-02:30).
- **On the first breadth call after midnight ET rollover** if neither of the above fired (network outage during the ticker, etc.).

On success the new list is written as current state plus an immutable
observation in `~/.local/state/ibkr/daemon.db` and pushed into the breadth
engine. On any failure (network, parse error, count outside the 450–520 sanity
band), the daemon keeps using the last valid SQLite snapshot, then the embedded
list as the cold-start fallback, so breadth never goes silent.

### Pinning the list (regulated traders, reproducibility audits, air-gapped)

Some users need a frozen membership list. Two override layers, with symmetric semantics:

**Persistent (TOML config):**

```toml
[spx]
members_auto_refresh = false
```

**Ad-hoc (env var, overrides TOML):**

```sh
IBKR_SPX_MEMBERS_AUTO_REFRESH=0 ibkr daemon   # force off
IBKR_SPX_MEMBERS_AUTO_REFRESH=1 ibkr daemon   # force on (even if TOML says off)
```

When pinned, `ibkr status` shows the reason, `refresh:disabled (env)` vs `refresh:disabled (config)`, so a confused user knows which knob to flip.

### Status row

`ibkr status` always carries a one-line summary of the members source and refresh health:

```
Members  cache:2026-05-22  count:503                            # healthy
Members  embedded:2026-05-22  count:503  refresh:parse_failed   # silent rot (Wikipedia changed HTML)
Members  embedded:2026-05-22  count:503  refresh:network_failed # offline / DNS down
```

The `cache:DATE` vs `embedded:DATE` source token tells you whether the in-process list is from the auto-refresh path or the binary's compiled-in fallback. The bracketed `refresh:<state>` suffix appears only when something needs attention.

## Updating market calendars: binary release

Market calendars are embedded official exchange schedules in this first release. The supported calendars are US cash equities, US listed options regular sessions, and German Xetra cash equities. They do not cold-start, hit a network cache, or apply an IBKR-specific overlay at runtime; the official exchange calendar is the binding source for open/closed/holiday/early-close context.

Each response includes `coverage_start` and `coverage_end`. Queries outside embedded coverage return `state: "unknown"` rather than guessing from weekdays. The CLI/MCP `days` horizon is capped at 400 calendar days, which covers the practical risk-manager lookahead for next-session, long-weekend, year-end, and next-year holiday checks while keeping responses bounded.

Calendar updates arrive with normal `ibkr` binary updates. If a supported exchange publishes an unscheduled closure or changes a future holiday after your installed binary was built, update the binary once a release carrying the refreshed calendar is available.

## Where state lives

- `~/.local/bin/ibkr`: installed binary; `.bak` carries the immediately-prior version.
- Claude Desktop extension storage: MCPB-managed installs carry a separate embedded binary; update by reinstalling `ibkr.mcpb`.
- `~/.local/state/ibkr/daemon.db`: runtime-refreshed S&P membership state and observations, along with other daemon-owned business state.
- `~/.cache/ibkr/update/`: install-time scratch space (downloaded tarball, lock file).
- `~/.config/ibkr/config.toml`: optional persistent config (see [config reference](../reference/config.md)).

All under `$XDG_STATE_HOME` / `$XDG_CACHE_HOME` / `$XDG_CONFIG_HOME` when set;
the paths above are the fallback.

## Reference

- [Configuration reference](../reference/config.md): every TOML field and `IBKR_*` env var.
- The updater keeps one `.bak` binary beside `~/.local/bin/ibkr`; live SPX
  membership state lives in daemon.db under the XDG state root.
