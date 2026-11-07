# Updating

Updated: 2026-11-07 07:52 CET

Four things can go out of date: the **binary** (`canary` itself), the **Claude Desktop MCPB** when installed through Desktop Extensions, the **S&P 500 constituent list** the breadth indicator uses, and the embedded **official market calendars**. They update independently because they have different sources and cadences.

## Install and activate a new binary

A copied binary does not change an already-running process. Choose the row that
matches how the binary arrived:

| Installation path | Automatic restart | Activation |
| --- | --- | --- |
| `canary update` | Interactive use asks whether to restart. `--restart` refreshes only the app and daemon processes that were already running; `--no-restart` refreshes neither. | Use `canary update --restart` for running processes. Run `canary restart` afterward when you also want to start an absent daemon. |
| `make install GO_TAGS=""` | No. It builds and atomically installs the standard read-only checkout. | Prefer `make restart-daemon GO_TAGS=""` to build, install, and activate it in one target. Plain `make install` and `make restart-daemon` use the experimental trading build default. After a separate install, run the installed `canary restart`. |
| Manual download or binary replacement | No. Existing app, daemon, and MCP host processes keep executing their old image. | Run the newly installed `canary restart`. Fully relaunch the MCP host as well when it owns a `canary mcp` process. |

After activation, `canary version` reports the CLI build and `canary status`
reports the daemon build. A version mismatch warning means the file was replaced
but the long-running process has not moved to it yet.

### Updating from v3.26.2 or earlier

If your app uses a path containing spaces, or you run separate HOME/XDG profiles, use `canary update --no-restart` first. Then invoke the newly installed `canary restart` from the app's original profile. The older updater performs activation with its own old restart code even after replacing the file, so its `--restart` cannot apply these fixes. Trading-build users should install the new trading artifact through their existing installation procedure before restarting.

App lifecycle commands verify the app's effective HOME, socket, config and state paths before stopping it. A different or unreadable profile is refused without stopping a process; use the app's original environment. Relative profile paths and relative app `--state-dir`/`--log` values need absolute paths. Custom launchd jobs with their own environment or working directory require explicit supervisor management; automatic lifecycle commands leave them untouched. An older saved supervisor recovery record has no profile identity: recover it with `canary setup app` from its original profile instead of automatically resuming it under the current environment.

## Updating the binary: `canary update`

Updating the binary is one command:

```sh
canary update            # fetch latest, prompt to restart the local app/daemon stack
```

The CLI then:

1. Lists stable GitHub releases, selects the newest release on the installed major line, and matches your OS/arch against its tarballs. A v2 install therefore follows maintained v2 releases and never silently crosses to v3; a development build follows the newest stable line.
2. Requires and verifies the compact **Ed25519 signature on `SHA256SUMS`** against the public key embedded in your current `canary` binary.
3. SHA-verifies the tarball.
4. Atomically replaces `~/.local/bin/canary`. Prior bytes exist only in hidden transaction staging and are deleted after publication; no runnable rollback binary is retained because daemon state migrations are forward-only.

Custom shell installations must keep `CANARY_INSTALL_DIR` set to their install
directory when updating. Without it, Canary refuses a destination that differs
from the executable you invoked, so an update cannot silently leave that binary
behind. MCPB installations use their host's extension update path below.

Trading builds refuse the standard updater's install action because that channel
contains read-only binaries. To retain trading capability, download and verify
the matching `canary-trading-vX.Y.Z-<os>-<arch>.tar.gz` from the release, then use
the manual replacement/activation path above, or build the chosen tag with
`make install` and restart. A deliberate switch to read-only uses the standard
installer. `canary update --check` can still check the standard channel.

At the end, the updater can restart the local processes that were already
running. It uses the same app-first stack path as `canary restart`: a running app
is restarted and verified on the installed binary before the daemon is touched.
Processes that were stopped before the update remain stopped.

The paired app performs the same signed, same-major check after bootstrap and
caches it for six hours. When a newer stable release is provable, the footer
names the exact version and offers one update action. The action runs the same
updater and reconnects only after the app host serves the requested version.

Ahead-of-tag `git describe` builds (for example `v3.0.1-12-gabcdef0`), dirty
builds, `dev`, and commit-only versions are deliberately incomparable with a
stable release. Neither the CLI nor the app offers an automatic replacement for
them: semver would otherwise rank an ahead build as a prerelease below its base
tag and could silently downgrade it. `canary update --force` remains the
explicit recovery path when replacing such a build is intentional.

### Release integrity

Each release ships two signatures over the same `SHA256SUMS` bytes. `SHA256SUMS.asc` keeps `install.sh` and older Canary binaries working. Current binaries require `SHA256SUMS.ed25519`, verified by a narrow standard-library implementation and a dedicated public key embedded in Canary. A missing or invalid compact signature fails closed; the binary carries no PGP fallback.

Releases that publish an MCP Bundle include both `canary-vX.Y.Z.mcpb` and the stable latest-download asset `canary.mcpb` in `SHA256SUMS`. The MCP Registry publish artifact also records the versioned MCPB file's SHA-256 in `server.json` as `fileSha256`.

The MCPB container itself is not code-signed. Treat MCPB release integrity as signed-checksum and registry-hash based unless `mcpb verify canary-vX.Y.Z.mcpb` succeeds.

`canary update` **refuses** any release missing the compact signature, and any release whose signature does not verify against its embedded key. There is no `--insecure` flag. If you ever need to debug a verification failure, the underlying error is printed verbatim and the manual verification steps are in [SECURITY.md → Release integrity](https://github.com/osauer/canary/blob/main/SECURITY.md#release-integrity-v100).

### Headless / scripted use

In non-interactive contexts (cron, systemd timers, CI, stdin-redirected shells) the `[Y/n]` prompt would block. Pass an explicit restart decision:

```sh
canary update --restart        # restart a running app first, then a running daemon
canary update --no-restart     # leave both processes as-is; print a restart-pending hint
```

Closing an interactive restart prompt without a response cancels the restart.

Running `canary update` from a non-TTY *without* either flag exits non-zero with `ambiguous in non-interactive mode` and does not install. This is deliberate: a silent default of "no" would leave a systemd timer that expects a restart running the old binary without saying so.

`canary update --restart` does not wake an app or daemon that was absent. If a
restart stage fails, the ordered, non-atomic stage rules described under
`canary restart` apply and the command points back to explicit `canary restart`
recovery.

### Dry run and forced reinstall

```sh
canary update --check          # dry-run: print "would install vX.Y.Z", exit 0
canary update --force          # re-install latest even if same version (corrupt-binary recovery)
```

`--check` exits 0 whether or not an update is available; only fetch failures exit non-zero. So `canary update --check && canary update` is the idiomatic confirm-then-install pattern.

### Older `ibkr`-named installations

Do not ask the old executable to update itself across the product rename. Those
binaries cannot safely select and extract the current Canary archive. Run the
current shell installer once instead; it verifies the signed release assets
before installing them:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary restart
```

The installer publishes `~/.local/bin/canary` and retires a co-located
pre-rename executable. If the old installer used a custom `IBKR_INSTALL_DIR`,
reuse that same directory under its new name so the old binary is retired in
the same transaction:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh |
  CANARY_INSTALL_DIR=/your/previous/bin sh
canary restart
```

When an `ibkr` executable is still on `PATH` somewhere else, the installer
refuses a split installation and prints the directory to use. It does not
remove the existing config or state directories. `canary restart` then starts
the new daemon, which performs any required state migration before it connects
to the broker.

An installation that still keeps file-backed state, or an older alert ledger
or registry, from before the SQLite database first installs and starts the
latest stable 2.x release once, which converts it; version 3 refuses that
state rather than guessing at it. The
[changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md) describes
each earlier upgrade step.

## Restarting local processes: `canary restart`

Use `canary restart` when you changed daemon-loaded config, installed a new binary outside `canary update`, or want to clear stale gateway connection state:

```sh
canary restart
```

On the default socket the command runs two ordered stages:

1. **App stage.** Refreshes an already-running `canary app` host and verifies the
   replacement. It preserves unsupervised app flags such as `--addr`,
   `--public-url`, and `--remote`; a launchd-owned app is restarted from its
   loaded plist only after the plist executable is verified as the current
   installed binary. If no app host is running, it leaves the app stopped.
2. **Daemon stage.** Only after the app stage succeeds, the command verifies the
   daemon pidfile holder, stops it, starts a fresh daemon, and reports the new
   PID and gateway health. Plain `canary restart` starts the daemon when none was
   running.

The two stages are ordered but not atomic. An app-stage failure leaves
the daemon untouched. A later daemon-stage failure does not roll the
already-restarted app back; fix the reported stage and rerun `canary restart`.

`--force` is an explicit fallback for an app or daemon that ignores SIGTERM:

```sh
canary restart --force          # escalate to SIGKILL only after graceful timeout
canary restart --timeout 30s    # wait longer before failing or forcing
canary restart --json           # scriptable result: daemon health plus any refreshed app process
canary restart --app            # app-only restart/start for the HyperServe app process
canary app restart              # same app-only restart path, grouped under app commands
```

JSON mode is for automation and CI. It avoids text parsing and includes the post-start `status.health` payload so a script can distinguish "process restarted but gateway offline" from "restart failed." When an app host was running, JSON also includes an `app` object with its old/new PID and preserved args. If that app stage succeeds but the daemon stage fails, the command exits non-zero and still emits the partial structured result with the successful app stage and `started: false` for the daemon.

When `CANARY_SOCKET` selects a non-default daemon scope, app process discovery
cannot prove which scope a found app belongs to. The decision is therefore made
before any process mutation: the app is left untouched and only the explicitly
selected daemon is restarted. The result marks the app as
`reason: "socket_overridden"`. If that stack also needs an app restart, run
`canary restart --app` as a separate, explicit, non-atomic operation.

`canary restart` restarts the shared daemon that CLI commands and MCP tools dial, plus any currently running local or remote app host. It does not restart the `canary mcp` stdio process itself; that process is owned by Claude Desktop, Cursor, Continue, or whichever MCP host launched it. Relaunch the host when you need it to respawn MCP from a new binary or MCPB bundle.

`canary restart --app` targets only the long-running `canary app` HyperServe process. It finds a local `canary app` server process, sends SIGTERM so HyperServe can shut down gently, preserves its old flags, and then starts the app again. A same-argv respawn is accepted only when it uses the current executable. When launchd owns the app, restart uses the loaded job's executable and plist arguments; change those arguments by rewriting and reloading the plist, not with restart flag overrides. If no app or supervisor is present, this explicit app-only form starts `canary app` with default/env configuration.

In remote mode the hosted Cloudflare Worker is not restarted or redeployed by
this command. The local app process restarts its outbound relay connector and
reuses the persisted relay route while that route remains inside the relay TTL,
so paired phones and Home Screen installs can keep opening the same relay
origin across ordinary app restarts.

## Updating Claude Desktop MCPB installs

Claude Desktop MCPB installs carry their own embedded `canary` binary. `canary update` updates shell-managed installs such as `~/.local/bin/canary`; it does not replace a binary embedded inside Claude Desktop's extension store.

To update a Claude Desktop MCPB install, download the latest bundle and reinstall it in Claude Desktop:

<https://github.com/osauer/canary/releases/latest/download/canary.mcpb>

After reinstalling, fully quit Claude Desktop and reopen it so it respawns the MCP server from the updated bundle. Then call `canary_status`: compare `mcp_client.version` with `daemon_version`. A surviving shared daemon keeps its old version until explicitly restarted.

If `mcp_client.activation_required` is true, follow its guidance. When supplied,
copy `mcp_client.restart_command` into your terminal. It uses the absolute
embedded executable path and preserves the MCP process's custom configuration
and socket paths, so a shell-installed `canary` is not required. It restarts the
shared daemon and any app already running in that scope. Call `canary_status`
again and confirm the versions agree.

If the shared daemon is a trading build while the bundle is standard read-only,
update and restart it from the matching trading installation instead. Status
withholds the bundle restart command when capabilities differ or cannot be
verified. The MCP host never performs an automatic restart.

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

The same page carries each constituent's GICS sector, and the fetch keeps that
column too. Sectors classify held S&P 500 names for the
[allocation tables](../understand/concepts.md#portfolio-allocation); they are
held in memory only, with an embedded baseline that `make refresh-spx-members`
regenerates beside the member list on every release. A restart therefore uses
the baseline sectors until the next successful refresh. A page whose sector
column has moved leaves the sectors untouched: the member list is validated on
its own, and a sector map smaller than the membership sanity floor is ignored.

### Pinning the list (regulated traders, reproducibility audits, air-gapped)

Two override layers, with symmetric semantics:

**Persistent (TOML config):**

```toml
[spx]
members_auto_refresh = false
```

**Ad-hoc (env var, overrides TOML):**

```sh
CANARY_SPX_MEMBERS_AUTO_REFRESH=0 canary daemon   # force off
CANARY_SPX_MEMBERS_AUTO_REFRESH=1 canary daemon   # force on (even if TOML says off)
```

When pinned, `canary status` shows the reason, `refresh disabled (env)` vs `refresh disabled (config)`, so you can tell which setting to change.

### Status row

`canary status` always carries a one-line summary of the members source and refresh health:

```
SPX members  cache:2026-05-22, 503 names                            # healthy
SPX members  embedded:2026-05-22, 503 names, refresh parse_failed   # Wikipedia changed its page layout
SPX members  embedded:2026-05-22, 503 names, refresh network_failed  # offline / DNS down
```

The `cache:DATE` vs `embedded:DATE` source token tells you whether the in-process list is from the auto-refresh path or the binary's compiled-in fallback. The trailing `refresh <state>` suffix appears only when something needs attention.

## Updating market calendars: binary release

Market calendars are embedded official exchange schedules. [Market calendars](../understand/concepts.md#market-calendars) lists the supported markets and where each one's coverage ends. They do not cold-start, hit a network cache, or apply an IBKR-specific overlay at runtime; the official exchange calendar is the binding source for open/closed/holiday/early-close context.

Each response includes `coverage_start` and `coverage_end`. Queries outside embedded coverage return `state: "unknown"` rather than guessing from weekdays. Calendar lookahead is capped at 400 calendar days, which covers the practical risk-manager lookahead for next-session, long-weekend, year-end, and next-year holiday checks while keeping responses bounded.

Calendar updates arrive with normal `canary` binary updates. If a supported exchange publishes an unscheduled closure or changes a future holiday after your installed binary was built, update the binary once a release carrying the refreshed calendar is available.

## Where state lives

- `~/.local/bin/canary`: the sole installed executable. No prior-version binary is retained.
- Claude Desktop extension storage: MCPB-managed installs carry a separate embedded binary; update by reinstalling `canary.mcpb`.
- `~/.local/state/ibkr/daemon.db`: runtime-refreshed S&P membership state and observations, along with other daemon-owned business state.
- `~/.cache/ibkr/update/`: install-time scratch space (downloaded tarball, lock file).
- `~/.config/ibkr/config.toml`: optional persistent config; the [configuration reference](../reference/config.md) documents every TOML field, product-owned `CANARY_*` variable, and broker-specific `IBKR_*` diagnostic variable.

All under `$XDG_STATE_HOME` / `$XDG_CACHE_HOME` / `$XDG_CONFIG_HOME` when set;
the paths above are the fallback.
