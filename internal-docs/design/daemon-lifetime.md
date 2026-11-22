# Daemon lifetime

Status: built for macOS on 2026-11-22 (owner instruction 09:13 CET: build
it, starting at login is fine). Code: `internal/dial/agent_darwin.go`.
"What was built" below says where the build differs from the design; the
rest of this note is the design as written at 08:05 CET the same day.

## What was built

- `canary restart` installs the agent
  (`~/Library/LaunchAgents/com.osauer.canary-daemon.plist`) for the binary
  it restarts and loads it; loading starts the daemon. `canary update` and
  `make restart-daemon` run `canary restart`, so an update installs it.
  Nothing else installs it: a first start on a new machine is a direct
  start until the first restart or update.
- Every other start uses the agent only when it is installed for this
  binary and this scope. An own agent that is installed and not loaded, as
  a failed restart leaves it, is loaded again by that start. The scope is the set of variables
  that decide which config, state, log and socket a daemon uses (the four
  `XDG_*` roots, `CANARY_CONFIG`, `CANARY_LOG`,
  `CANARY_SPX_MEMBERS_AUTO_REFRESH`); a variable set to its own default
  counts as unset. Otherwise it is a direct start, as before. A development
  build, a test with its own HOME or socket, and a login without a GUI
  session never touch launchd.
- One agent per user, for the scope that installed it. A restart in another
  scope leaves it alone, starts its daemon directly and says so.
- Proxy variables, `TZ` and `IBKR_PACKET_LOG_TEMPLATE` are copied into the
  agent from the shell that runs the restart: launchd gives a job a bare
  environment.
- The daemon ends every exit it chooses with status 0 under the agent and
  checks the operator's stop intent before it starts.
- The startup reservation: a daemon launchd started does not wait for it,
  because the starter that asked for it holds it while it waits for the
  socket. The agent runs one process and the instance lock admits one
  daemon. A restart closes its own gap: after it stopped the daemon and
  before its start budget begins, it unloads the agent on this socket,
  whatever scope installed it, and waits until launchd has let go of it
  (`QuiesceDaemonAgent`), so a daemon launchd restarted after a kill or a
  hung shutdown is gone. An earlier draft passed the reservation on a token
  file; review found two races in it and it was removed.
- When the agent cannot be installed or loaded, the start says so on the
  terminal and starts the daemon directly.
- The installed agent is read through `plutil`, so a file rewritten by a
  macOS tool, to other escaping or to the binary format, is still
  recognised.
- `canary daemon --remove-agent` unloads the agent and deletes the file. It
  refuses while the daemon runs: stopping a daemon is `canary stop`, which
  guards a working order.
- The job runs as `ProcessType Interactive`. As a `Background` job the
  daemon took 2m15s to start serving where it takes half a minute.

Measured on the owner's Mac that day, with the live daemon:

- started through `canary restart`, the daemon's coalition differs from the
  shell that ran the restart inside the desktop app;
- after SIGKILL launchd started a new daemon within a second, and it served
  again after its normal half-minute start;
- after SIGTERM the daemon exited with status 0 and launchd left it down
  for the 14 seconds watched;
- right after launchd spawns the job, its process cannot be signalled for a
  moment (EPERM), which the liveness probe read as "exited during startup";
  the start now waits until the pid answers.

Reviewed twice by an independent reader the same day. The first review
found the token races, the asynchronous `bootout` and a comparison on the
raw environment; the second found that a failed restart left the agent
unloaded for good, that an agent of another scope on the same socket was
not unloaded, that the unload ate the start budget, and that recognising
the agent depended on the file's spelling. All are fixed, each with a test.

One more defect came from running the tests, not from a review: the first
version bound the agent to the default socket by the environment alone, so a
unit test that restarted a daemon on a temporary socket unloaded the live
agent and stopped the live daemon for half a minute. The agent is now bound
to the socket a caller actually starts, the restart's unload is an injected
dependency that tests leave empty, and a test pins both.

Not built, not proven:

- No test covers the daemon's own two rules under the agent: status 0 on a
  refused start, and the early return on a stop intent. Both were read by
  the second reviewer and the clean stop was observed live.

- **A client that embeds an older Canary starts the daemon directly.** It
  compiles the start code of the version it pins, which knows no agent. Such
  a daemon is in that client's coalition and is not restarted after a kill.
  Seen the same day: after the clean stop, an embedding app's next poll
  started the daemon directly, before any `canary` command ran.
- A deterministic panic at start is restarted by launchd for as long as it
  lasts. One such site is known (`internal/daemon/cash_policy_settings.go`).
- The restart hint "run: kill" for a stuck daemon is unchanged.
- Linux: unchanged, `setsid` only.
- The end-to-end proof is still pending: the daemon alive after the next
  self-update of the app that started the restart.

---


## What happened

On 2026-11-22 at 05:54:03 CET the daemon that had run since the afternoon
before stopped without a line in its own log. One second earlier the desktop
app whose terminal had started it quit to update itself. A second app's next
request started a new daemon a second later, so nothing was missed that
morning. With a working stop order and no client asking, the daemon would have
stayed down until someone ran a command, and fills, cancels and the order
journal would have gone unwatched for that time. The daemon already refuses
to idle-exit while an order works, for the same reason.

## Why it died

Verified in the macOS unified log for that second (loginwindow, subsystem
`com.apple.launchservices:quitsupport`):

- "has non-application coalition pids [ … canary … ]"
- "is not allowed by BTM so terminating its subordinates"
- a force quit addressed to the app's coalition.

macOS groups every process an app starts into the app's coalition. When the
app quits and is not allowed to leave background processes behind, the system
ends every process in that coalition. The daemon logs a line on SIGTERM and
logged none, so the signal was SIGKILL; that last step is inferred, the log
does not print the kill.

Measured on this machine with throwaway `sleep` children:

| Started with | Parent | Coalition |
|---|---|---|
| a shell in the app | the shell | the app's |
| `setsid` (what Canary does today) | launchd | the app's |
| double fork, exec after reparenting | launchd | the app's |
| `posix_spawn` with the private disclaim attribute | launchd | the app's |
| a submitted launchd job (`launchctl submit`) | launchd | its own |
| an agent installed in `~/Library/LaunchAgents`, started with `launchctl kickstart` from the app's shell | launchd | its own |

So a new session, a double fork and the disclaim attribute all leave the
daemon in the starter's coalition. Only launchd starts a process outside it.

A coalition is the set of processes macOS accounts to one app. "BTM" in
the log is Background Task Management, the system service behind the
"Allow in the Background" list in System Settings.

Seen once, for one app. The log ties the kill to that app not being allowed
background processes, so an app with that permission may leave the daemon
alone. Canary cannot know which kind started it, and the same exposure
exists for any app that starts Canary directly, through a terminal or
through an MCP child. Whether Terminal.app behaves the same is not measured.

## Decision

On macOS the daemon runs as a per-user launchd agent that Canary installs.
launchd starts it when it is loaded (at install and at login, see point 3)
and Canary starts it again on demand after it has exited. Every path that starts a daemon today (the CLI's autospawn,
`canary mcp`, the app host and `canary restart`) ends in one function,
`spawnDaemonFromExecutable` in `internal/dial/autospawn.go`; that function
changes, the paths above it do not.

1. **Install once, quietly.** The first start writes
   `~/Library/LaunchAgents/dev.osauer.canary.daemon.plist` and bootstraps it
   into the user's GUI domain.
2. **Start on demand after an exit.** A start is `launchctl kickstart`. launchd is the
   parent and the job has its own coalition, whoever asked.
3. **Come back after a kill.** `KeepAlive` with `SuccessfulExit = false`:
   launchd restarts the job when it ended by a signal or a non-zero status,
   ten seconds later, without waiting for a client. An exit with status 0
   stays down (from the launchd documentation, not measured here). Measured
   on this machine with a `sleep` agent: restarted ten seconds after
   SIGKILL. The narrower `Crashed = true` does not restart
   after SIGKILL (measured, none within 15 seconds), so it is no use here.
   Two consequences of the setting, both to be built in:
   - Every exit the daemon chooses must be status 0 under the agent: the
     idle exit, a requested stop, and a refusal to start on a config error.
     A refusal that exits non-zero would, by the same documentation, be
     restarted every ten seconds for as long as the config stays broken. The reason goes to the log and to the starter, not to the status.
   - launchd also starts such a job when it is loaded, at install and at
     every login, whatever `RunAtLoad` says (measured). So a daemon runs
     after login without anyone asking and idles out if nobody does. That
     is a product change, the owner's to accept: the alternative is no
     restart after a kill, which is the second half of this fix.
4. **The daemon takes its own lock.** Today the starter reserves the startup
   lock and hands the descriptor to its child. A descriptor cannot cross
   launchd, so the daemon takes the lock itself and the starter waits for the
   socket as it already does for a daemon that another process started.
5. **Only the default daemon.** A daemon given its own socket or config (the
   gateway smoke test, the integration tests, a second broker account on its
   own config) keeps today's direct start. They are short-lived and started on purpose.
6. **Linux.** No coalition exists, and `setsid` survives a closed terminal.
   The exposure is a starter inside a systemd unit that kills its control
   group. Where a user systemd runs, start with `systemd-run --user`; where
   it does not, keep `setsid`. Not tested yet; see open points.

## Cases the build must handle

- **`canary restart` and a forced stop.** Under the agent a SIGKILL makes
  launchd start a daemon while `canary restart` starts its own. When the
  daemon is the agent's, restart goes through launchd (`launchctl kickstart
  -k`), and the "run: kill" hint printed for a stuck daemon names that
  command instead.
- **Who decides agent or direct start.** The starter does, before it starts
  anything: it resolves its own config, state, cache and socket paths
  (`XDG_*` and the `CANARY_SOCKET`, `CANARY_CONFIG`, `CANARY_LOG` and
  `CANARY_APP_STATE_DIR` overrides) and compares them with the ones recorded
  in the plist. Equal: kickstart the agent. Different: direct start, as
  today. The first install records the paths of the starter that made it; a
  first start from an unusual environment is therefore refused as the
  agent's and takes the direct start, and the install waits for a starter on
  the default paths.
- **The binary moves or is updated.** The plist names the installed path.
  `canary update` and the installer rewrite it and reload the job
  (`bootout`, then `bootstrap`). A development build started from a checkout
  never installs or replaces the agent; it takes the direct start.
- **Two users on one Mac, and SSH.** The agent is per user, in that user's
  GUI domain, with that user's paths. A login without a GUI session (SSH)
  has no such domain: direct start.
- **The starter can no longer see a failed start.** Today it watches its
  child's pid and reports an early death at once. Under launchd it reads the
  job's last exit status from `launchctl print` while it waits for the
  socket.
- **Uninstall.** No uninstall command exists today. One is needed with
  this: boot the job out and delete the plist.

## What this costs

- A file in `~/Library/LaunchAgents` and a launchd job that outlive an
  uninstall unless `canary` removes them. `canary update` and the installer
  must rewrite the plist when the binary path changes; an uninstall step
  must boot the job out and delete the file.
- **The environment is the plist's, not the starter's.** launchd starts the
  job with the environment written into the plist. Canary reads
  `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME` and `XDG_RUNTIME_DIR`
  to find its config, store and socket. The install writes the starter's
  values into the plist. A later starter with different values would be
  talking about a different daemon: it must be given a direct start, as in
  point 5, never the agent. v3.35.0 showed what a daemon on the wrong config
  root does: it ran on default policy files in place of the owner's. The
  comparison under "Cases the build must handle" is the guard against
  repeating it.
- The lock handover changes on a path every start takes. That is a change to
  process lifetime and recovery, so it ships behind the release tier, with a
  live smoke.

## Considered and rejected

- **The private disclaim attribute.** Needs cgo or a helper binary, is not a
  public API, and does not change the coalition.
- **Double fork.** Does not change the coalition.
- **`launchctl submit`.** Its own coalition (measured), but in the incident
  the system log shows a submitted job and two agents whose plists sat
  outside `~/Library/LaunchAgents` unloaded with the app, and one agent
  inside that folder kept. Why it kept that one is inferred; see open
  points.
- **A watchdog that restarts a dead daemon.** It would live in the same
  coalition and die with it.
- **Leave it and rely on the next request.** That is today. It is wrong
  exactly when it matters, with an order working and nobody at the desk.

## Proof

- A test starts the daemon through the new path from inside an app's
  terminal and asserts two facts: the daemon's coalition id differs from the
  starter's, and `launchctl print` shows the installed agent. Both can be
  read without quitting anything.
- A test kills the daemon with SIGKILL and asserts launchd brings it back
  and that the next status shows the restart; a clean stop stays down, and
  a start refused on a broken config is not restarted.
- The end-to-end proof is the next self-update of an app that started
  Canary. Report "proof pending" until that log shows the daemon alive after
  the app is gone.

## Open points

- Which plist property made the system keep one agent and unload two is
  inferred from one incident (the location under `~/Library/LaunchAgents`).
  If it is something else the agent is unloaded with the app and the daemon
  dies as today. Check the system log line "Setting ignorable launch jobs"
  on the first app update after the change.
- Linux is reasoned, not tested.

## First step, before the agent

One small change makes the problem visible and needs none of the above: at
start, the daemon checks whether the previous one left a clean-stop marker,
and when it did not, logs a warning and shows "the previous daemon ended
without a shutdown" on `canary status`. Today a kill leaves no trace
anywhere. Build this first; it also measures how often it happens.
