# Scheduled log monitor

`go run ./scripts/log-monitor` emits bounded, redacted JSON and commits private
cursors after successful scans. It never calls the daemon or starts services.
On macOS the default app input is the launchd stderr log created by
`canary setup app`: `~/Library/Logs/ibkr/app.err.log`. Other platforms retain the
state-directory default. Use `-app-log` for a different service arrangement.
The report names both input paths and their modification times.

For investigations, always use `-commit=false` and separate `-daemon-offset`
and `-app-offset` paths. A dry run never creates or updates cursor state.

Version 2 cursors record file identity, a line checkpoint and content digests.
Replacement and copy/truncate rotation replay the new file and consume the
unread tail from `.1` when it can be matched. Missing rotation history is a
coverage warning. Legacy numeric cursors replay both retained files once
because their file identity cannot be established. Unterminated final records
remain unread until completed. Cursor files are private, atomically replaced,
and contain counts and digests rather than raw log text.

Missing logs require attention. `-stale-after=24h` also flags inactive logs as
**health unverified**, not as proof of a service outage; set a different interval
or zero to disable this check for deliberately quiet deployments. A clean scan
is not proof that every service or data source is healthy.

When the daemon runs on another machine, point `-daemon-log` at a local mirror
of its log and `-daemon-mirror-status` at the mirror's sync record: JSON with
`version: 1` and `last_success` (RFC 3339, `null` before the first completed
sync). A missing, unreadable or never-completed record, or one older than
`-mirror-stale-after` (default 20 minutes), is a coverage warning: an unchanged
copy is no evidence while the mirror is behind. The report's `daemon.mirror`
states which. The mirror must append in place and rotate its copy by rename, as
the daemon does, so cursor identity and rotated-tail recovery still apply.

A "no security definition" (code 200) notice is about one contract, so its
signal names the contract the connector tagged it with, `(OKE STK)`, or
`(untagged)`; each contract counts separately. Reports never name a current
holding: the monitor reads the account-data gate's holdings list (the latest
Flex position report in the daemon store, opened read-only, else the gate's
private cache at `~/.cache/ibkr/holdings-denylist`) and writes `[holding]` in
its place, in contract labels and free text alike. The report's `holdings`
field says which source was read; with neither it is `unavailable` and
contract names are withheld as `[contract]`. The list is this machine's: a
report on a mirrored log from another machine masks this machine's holdings.

Broker definition, entitlement and cancellation notices remain visible even
below the old noise threshold. They require outcome assessment; the monitor
does not infer recovery or an outage from a code alone. Only the indicative-data
disclaimer remains in that benign family set. Its volume threshold applies to
actual UTC calendar days, across scans. Closed-family counts retain 28 days of
recurrence evidence; short restart windows also span scans. Report size remains
bounded by ranked signals plus suppressed-family summaries.
