# Scheduled log monitor

`go run ./scripts/log-monitor` reads the daemon and app logs incrementally and
reports **incidents**, not line shapes: one bounded, redacted JSON object
(report `version` 4). It commits private cursors and open items after a
successful scan, never calls the daemon and never starts services.
On macOS the default app input is the launchd stderr log created by
`canary setup app`: `~/Library/Logs/ibkr/app.err.log`. Other platforms retain the
state-directory default. Use `-app-log` for a different service arrangement.
The report names both input paths and their modification times.

For investigations, always use `-commit=false` and separate `-daemon-offset`,
`-app-offset`, `-daemon-crash-offset` and `-open-items` paths. A dry run never
creates or updates cursor state or open items.

## What needs the owner

`needs_attention` is true exactly when the top-level `attention` array is
non-empty. Each entry names the log, the item and one reason:

| Reason | When |
| --- | --- |
| `error` | any ERROR/FATAL line, panic, crash-log content, or an ERROR connectivity line |
| `new_family` | a family absent from the 28-day recurrence history and not declared |
| `long_incident` | an incident at least `-incident-attention` long (default 15m) |
| `open_incident` | an incident still open at the end of the window |
| `grew` | an item carried from the previous run whose count at least doubled (and rose by 3 or more) |
| `coverage` | a coverage gap (missing, stale, rotated or unmirrored log) seen for the first time |

Everything else is informational: a carried family at its usual rate, a
standing condition, a short closed incident, a daemon restart, a declared
item. A scheduled reader leads with `attention` and reports nothing else as
needing the owner.

## Incidents

An anchor is a connectivity event: a backend loss (1100, 2110) or restore
(1101/1102), the backend link episode summary, a farm break or recovery
(2103/2105/2157 and 2104/2106/2158 are one farm-transition family; 2107/2108
"inactive, available on demand" lines are counted but anchor nothing), the
Gateway session going away (`Gateway unavailable`, EOF, heartbeat timeout,
disconnection) or recovering. Anchors chain into one incident while each
starts within `-incident-gap` (default 30m) of the previous one's end. A line
that says how long its condition lasted ("recovered … after 15m16s",
"disconnected for 19m21s", "29 losses over 6h3m") back-dates its anchor, so an
episode summary joins the morning's farm break to the afternoon's flap.

Each client logs its own copy of a transition; copies of one event (same
subject, role and farm within two seconds) count once, and `clients` says how
many clients reported. `anchors` counts events by type, never lines × clients.

An incident is **open** while any subject it broke has not recovered. An open
incident owns every later line and stays open across runs: it is written to
the open-items file and continued by the next run until a closing anchor (a
restore, a Gateway reconnect, or for backend and farm state a daemon start)
arrives. The run that closes it reports it closed, with `status: carried`.

Every WARN/ERROR line from `-incident-before` (60s) before an incident's first
anchor to `-incident-after` (2m) after its last one is a **dependent**:
counted under the incident by family (`dependents`, at most eight rows plus
`other`) instead of standing as a signal. Lines within `-restart-edge` (2m)
after a daemon start belong to that `daemon_restart` incident, which also names
the build and whether it changed. A daemon stop with no start after it is an
open `daemon_stop`. Repeated failures of one subsystem outside any incident
(at least three distinct minutes, chained by `-incident-gap`) form one
`failure_run` with a redacted sample.

## Families and signals

A family is a short lowercase key: a closed broker notice family
(`broker_code_200_no_definition`, `broker_code_354`, …), `broker_code_<n>` for
other system notices, or the message's leading lowercase words (`gamma` for
`gamma.prewarm`/`gamma.abort`/`gamma.combine`, `market_history`,
`regime_history`). Family keys never contain names: an all-capitals token ends
the key. Lines no incident, restart or run explains are **standalone** and
grouped by family: `signals` holds the `-max-signals` (default 10) highest
ranked — attention first, then severity, then count — applied after incident
grouping. Each signal gives its most frequent redacted sample, up to three
`other_samples`, the number of distinct samples, first and last times, and a
`status`: `new` (never in history), `returning` (in history, not open at the
previous run) or `carried` (open at the previous run, with `first_seen`).
Families the cap cuts fold into one `suppressed` row each.

Samples normalize what varies per occurrence: elapsed times, expiries, clocks,
rates, pids, `key=N` counters, client numbers (`[cid=N]`, `Client N`) and
counts or ordinals in prose ("loss N this session", "N cancel echoes", "N
losses over …"). Codes stay: `code 2103`, `code=200`, `IBKR 200`, `1101/1102`,
statuses, and numbers glued to letters (`1D`, `365d`).

## Carry-forward and standing conditions

Open items persist in `-open-items` (default `<daemon-offset>.open.json`,
mode 0600): family and failure-run keys with first-seen, last-seen, count and
days, the last daemon build, and any open incident's start and open subjects.
It holds no log text. An item open at the previous run and absent now is
reported once under `resolved`. An unreadable file restarts carry state
(`carry_state: reset`).

The daemon cursor keeps 28 UTC days of per-family counts. A family seen on at
least `-standing-days` (default 7) prior days whose count now is at most
double its median daily count is **standing**: one line under `standing`,
never attention. A failure run made only of standing families is
`status: standing`.

## Owner-declared states

`-states` (default `~/.config/ibkr/log-monitor-states.json`) lists conditions
the owner already knows about. The monitor only reads it; a missing file
declares nothing and is not an error; an unreadable one reports
`declarations.state: invalid`. Declared items are counted under `declared`
with the owner's reason and never raise attention. Panics and crash output
cannot be declared.

```json
{
  "version": 1,
  "declarations": [
    {"match": "usbond", "reason": "US bond market holiday", "dates": ["2026-11-11"],
     "declared_at": "2026-11-10T20:00:00+01:00", "declared_by": "owner"},
    {"match": "app:log_coverage_stale", "reason": "app stopped while travelling",
     "until": "2026-11-30", "declared_at": "2026-11-01T09:00:00+01:00", "declared_by": "owner"},
    {"match": "broker_code_354", "reason": "no market data subscription for bills",
     "from": "2026-11-01", "until": "2026-12-31", "declared_by": "owner"}
  ]
}
```

A one-word `match` (letters, digits, `_ . : -`) is compared with an item's
keys only: its family or kind, an incident's subject (`backend_link`,
`gateway`, `farm`, a farm name such as `usbond`), a failure run's subsystem
(`gamma`, `run:gamma`), each also prefixed with the log (`app:…`,
`daemon:…`). A phrase (anything with a space) matches the redacted sample
text instead, so `gateway` never declares a family whose message merely
mentions the Gateway. A declaration needs `until` (inclusive, optionally with
`from`) or `dates`; nothing is declared forever. A connectivity incident is
declared only when every subject in it is declared on its start or end date.

## Cursors, rotation and crash output

Version 2 cursors record file identity (the inode: macOS renumbers volumes at
boot), a line checkpoint and content digests.
Replacement and copy/truncate rotation replay the new file and consume the
unread tail from `.1` when it can be matched. Missing rotation history is a
coverage family. Legacy numeric cursors replay both retained files once
because their file identity cannot be established. Unterminated final records
remain unread until completed. Cursor files are private, atomically replaced,
and contain counts and digests rather than raw log text.

The daemon writes its process-level fatal output (the runtime's SIGQUIT
goroutine dump, an unrecovered panic) to `<daemon-log-name>.crash.log` beside
its log. The monitor scans that file with its own cursor (`-daemon-crash-log`,
`-daemon-crash-offset`; both derive from the daemon paths by default) and
reports unexplained content as one ERROR `crash_output` signal naming the
first line and the line count, never the trace itself. The known macOS
diagnostic saying malloc stack logging could not be turned off because it was
not enabled is counted separately in `crash_scan.allocator_diagnostic_lines`;
it is not crash evidence. A panic or other output in the same scan remains an
ERROR. `crash_scan` exposes first-read/reset status, newly scanned lines and
the file modification time. These undated lines have no known event times: an
initial read includes retained history, and line counts never represent
numbers of crashes. A missing crash log is normal.

## Coverage

A missing log, `-stale-after=24h` inactivity (**health unverified**, not proof
of an outage) and lost rotation history are coverage families
(`log_coverage_missing`, `log_coverage_stale`, `log_coverage_rotation`,
`log_coverage_mirror`). They need the owner the first time; the same gap the
next day is carried, and a declaration (`app:log_coverage_stale`) records a
service stopped on purpose. A clean scan is not proof that every service or
data source is healthy.

When the daemon runs on another machine, point `-daemon-log` at a local mirror
of its log and `-daemon-mirror-status` at the mirror's sync record: JSON with
`version: 1` and `last_success` (RFC 3339, `null` before the first completed
sync). A missing, unreadable or never-completed record, or one older than
`-mirror-stale-after` (default 20 minutes), is a coverage gap: an unchanged
copy is no evidence while the mirror is behind. The report's `daemon.mirror`
states which. The mirror must append in place and rotate its copy by rename, as
the daemon does, so cursor identity and rotated-tail recovery still apply.

## Privacy

A "no security definition" (code 200) or "market data is not subscribed"
(code 354) notice is about one contract, so the family's samples name the contract the connector tagged it with, `(OKE STK)`,
or `(untagged)`. Reports never name a current holding: the monitor reads the
account-data gate's holdings list (the latest Flex position report, which
the daemon writes to `~/.cache/ibkr/holdings-denylist`) and writes
`[holding]` in its place, in contract labels and free text alike. It never
opens `daemon.db`: an outside SQLite opener resets a live daemon's shared
memory if the daemon's lock on it is lost. The report's `holdings` field is
`cache` when the list was read; without the file it is `unavailable` and
contract names are withheld as `[contract]`. The list is this machine's: a report on a mirrored
log from another machine masks this machine's holdings.

Broker definition, entitlement and cancellation notices remain visible; the
monitor does not infer recovery from a code alone. Only the indicative-data
disclaimer (2129) is benign; above 150 occurrences on one UTC day it becomes a
`noise_loop` signal. Short restart windows span scans.

Messages never carry account IDs or broker order references. Besides
`key=value` fields, free-text references such as `order <n>`, `OrderId <n>`,
`orderId=<n>` and `permId <n>` keep their words and read `[ref]` in place of
the number.
