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

Broker definition, entitlement and cancellation notices remain visible even
below the old noise threshold. They require outcome assessment; the monitor
does not infer recovery or an outage from a code alone. Only the indicative-data
disclaimer remains in that benign family set. Its volume threshold applies to
actual UTC calendar days, across scans. Closed-family counts retain 28 days of
recurrence evidence; short restart windows also span scans. Report size remains
bounded by ranked signals plus suppressed-family summaries.
