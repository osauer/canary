# Bounded connection diagnostics

The daemon owns a single in-memory connection incident. Failed discovery,
handshake attempts and watchdog observations open it. The first observation
warns, subsequent details are debug-only, and continuing failures produce a
reminder every fifteen minutes. A completed connection closes it with a warning
bookend, so recovery remains visible at the default log level. Current typed
status is updated on every failure, independently of this diagnostic cadence.

History refreshes classified by the typed IBKR-unavailable error and proposal or
opportunity refreshes blocked solely by `account_unavailable` may join an
already-open connection incident. Gamma refreshes with no usable connector join
the same incident instead of warning each minute. They cannot create that
incident. Independent history defects and other blockers retain their warnings. The counter measures
observations, including dependent failures, not distinct outages or retries.

Managed broker connections send recoverable handshake-attempt detail to debug;
standalone library clients retain their existing severity. Protocol errors,
identity conflicts, read-loop failures and backend-link notices are not covered
by that switch. Neither retries nor trading gates change.

P&L silence has a separate bounded incident. Rebuild attempts continue at the
existing cadence. Only a frame for the current account subscription closes the
silence incident; an old subscription frame, reconnect, or attempted rebuild
cannot claim recovery. Frame quality remains separately visible in account
health.

## Crash output

The daemon's supervisor redirects its stderr into the daemon log. At startup
the daemon moves file descriptor 2 to `<log name>.crash.log` beside the log
(`cmd/canary/crash_output.go`, `dial.CrashLogPath`), so the runtime's own
fatal output — a SIGQUIT goroutine dump, an unrecovered panic, a fatal runtime
error — has a file of its own and cannot bury the slog stream (the 2026-10-03
SIGQUIT put 11,817 trace lines into `ibkr-daemon.log`). The slog writer keeps
its own descriptor. The crash log rotates once at open past 8 MiB and is
otherwise written only when something is fatally wrong; the log monitor reports
new content there as one ERROR signal naming the first line and the line
count. Lines written before the redirect (argument or config rejections) still
reach the inherited stderr, as before.

## Authority storage incident

The daemon's authority store (`corestore.Store`) latches fail-closed on a
critical SQLite failure or an unproven head watermark; every dependent write
then fails with `corestore: health is blocked`. The store reports the
Ready-to-blocked transition once, outside its locks, through
`corestore.Options.HealthObserver`, and the daemon's single authority incident
announces it at WARN with the health code, the error the failing operation
returned, whether the latch is recovery-eligible (a transient proof retries
every five seconds) or needs a restart, and a pointer to debug logs. Health
and status RPC surfaces keep reading the store's health directly.

While the incident is open, a warning whose argument wraps
`corestore.ErrBlocked` joins it at debug through the logger hook instead of
repeating the symptom; those joins are counted. A dependency cannot open the
incident: with no latch announced, the warning stays a warning, as do
independent defects during the latch. A successful recovery proof reports a
Ready health through the same observer, and the incident closes with one WARN
bookend carrying the blocked duration and the dependent failure count. The
recovery loop no longer writes its own bookend.

Two latch classes are recovery-eligible and share one proof (intact content,
exact authority epoch, head no older than the latch's floor, successful
synchronous watermark persistence): a post-commit head read that hit its
bounded deadline, whose floor is the last proven head, and a watermark
persistence failure after the committed head was read, whose floor is that
committed head. Every other critical class stays latched until a restart.

## Resource budget

Each incident uses fixed in-memory timestamps, a counter and a short mutex.
There are no per-symbol maps, retained error strings, timers, goroutines,
database queries, network requests or calendar lookups in this mechanism.
Disabled debug messages are tested to avoid formatting. Benchmarks cover both
the incident primitive and the complete suppressed daemon diagnostic path.

## Configured gateway duty windows

Startup TOML `[daemon]` owns diagnostic scheduling; it is not a runtime platform
preference and never changes broker-write gates, retry cadence, or data health.

- `log_calendar_mode = "conservative"` (default) retains first/ongoing incident
  and recovery warnings. Cached calendar windows decide when repeated backend
  losses deserve per-event warnings. No portfolio polling is added in this mode.
- `log_calendar_mode = "scheduled"` explicitly declares that the configured
  markets and padding cover the operator's intended duties, including manual
  orders that Canary cannot enumerate. An entire known off-duty incident may
  use INFO. Unknown scope always retains WARN.
- `log_markets` defaults to `us_equity`, `us_options`, `de_xetra`, `uk_lse`,
  `jp_tse`, and `hk_hkex`. A configured list replaces that baseline; automatic
  US analytics still add US equities/options. `["always"]` disables quieting.
- `log_before_open_minutes` defaults to 360; `log_after_close_minutes` defaults
  to 240. Both accept 0..720, including explicit zero. These are diagnostic
  duty envelopes, not claims about exchange product hours. Lunch stays relevant.

Example opt-in (restart required):

```toml
[daemon]
log_calendar_mode = "scheduled"
log_markets = ["us_equity", "us_options", "de_xetra", "uk_lse", "jp_tse", "hk_hkex"]
log_before_open_minutes = 360
log_after_close_minutes = 240
```

There is no authoritative watchlist store in the current daemon. Scheduled mode
samples passive, exact-session portfolio evidence once per minute and reuses
already collected API-order snapshots; neither path sends broker requests.
Recognized stock venues add markets. Options, futures, cash lines, SMART-only
routes and outside-RTH orders neither widen nor veto the declaration: the
configured markets are the duty statement, and inventory is evidence that can
only extend it. (Until 2026-10-03 such inventory forced the unknown scope, so
an option book kept every off-duty outage at WARN — the opt-in never quieted.)
Inferred markets are retained until daemon restart; closing a position does
not automatically narrow the logging scope. API snapshots do not cover every
manual TWS order; the configured duty declaration is essential, not inferred
from an empty portfolio.

Quieting needs no inventory evidence: the configured declaration decides on its
own, and inventory can only widen it. (Until 2026-10-04 quieting also required a
completed portfolio projection no older than 24 hours from the current
connector and session. A broker outage, or a daemon restart during one, can
never supply that, so the weekends and overnight windows the mode exists for
stayed at WARN — the Sunday 2026-10-04 outage still paged at WARN with the
opt-in in place.) Only a completed, same-account, uncontested projection widens
the declaration; a partial or conflicting one is ignored. Obsolete workers
cannot publish over a successor connector. A minute-cadence refresh may take up
to one minute to observe new portfolio/order scope; it does not participate in
trading decisions.

## Wire notice severities

Broker notices that only echo the connector's own actions, or that a requester
classifies itself, do not warn per notice. Code 300 "Can't find EId" answers a
cancel for a ticker the gateway no longer holds and is always debug; those
drawn during a broken backend link are counted and reported once on the
restore bookend. Code 162 "query cancelled" acknowledges the requester's own
timeout cancel and is debug; 162 "no data" is a verdict the requester records
per contract and logs at INFO; other 162 texts (pacing) keep their warning.
Code 200 for a symbol-only stock lookup (no conID on the request) is a
discovery miss at INFO — screen candidates, warrants and units — while a 200
for a request that carried a conID stays a warning, because a known identity
that stops resolving is the stale-cache case worth an operator's attention.

A parser misalignment — the gateway reading a request frame shifted by one
byte or field — is recognised only by its echo: a parse-fault code (320–323)
or an explicit parse-exception text that quotes a venue name without its first
byte as a token of its own (`MART`, `BOE`, `ASDAQ`), or a NumberFormatException
naming a non-numeric value. That echo logs at ERROR with the suspicious
outbound frame as context. Any other notice that merely mentions SMART or CBOE
keeps its ordinary severity (`pkg/ibkr/notice_misalignment.go`).

Data-farm notices (2103/2105/2157 breaks, 2104/2106/2158 recoveries) log the
transition at INFO. A farm break warns once when it has lasted five minutes
(checked on the next farm notice of any farm), and the recovery of a break
that warned, or that lasted at least five minutes, warns once with the
duration. Short flaps during IBKR's maintenance windows therefore leave no
WARN. Status keeps the live per-farm state regardless.

Embedded calendars are compiled at most hourly unless the inferred market scope
changes. One atomic pointer publishes merged intervals; log events perform only
bounded timestamp comparisons, with no database, network, timezone, or calendar
query. Expired views, clock rollback, and missing calendar coverage warn. The
declaration is published when the daemon starts and stays published across
connection attempts: a missing view reads as on duty, and clearing it on every
dial kept each off-duty failure at WARN (the night of 2026-10-05). A
quiet incident promotes on the next connection failure when duty begins; an
ongoing backend outage promotes within the worker's minute cadence even without
a new broker notice. Recovery considers the entire outage interval.

## P&L recovery integrity

Hermetic reproduction found that an asynchronous 1101 recovery from a retired
session could rebuild successor P&L subscriptions. This is a demonstrated code
defect, not a proven explanation for the September 21 feed incident. The ordinary
1101 replay and silent-after-1102 repair paths work in the synthetic wire tests.

P&L recovery now carries the originating connection and epoch through account
updates, cancellation and replacement requests. The existing guarded transport
checks the epoch again after pacing, immediately before writing. Short cache
commits exclude session retirement; conditional cleanup cannot erase successor
request identities. Receipts enter under publication, inbound and evidence
leases in that order, and both session and request identity must match.

Rebuilds serialize independently of receipt handling. A stale monitor decision
cannot replace a newer repair, and backend-down state defers pointless repair.
Failed request slots remain retryable; a bounded per-rebuild failure summary
contains counts, not account or contract identifiers. Position demand remains
responsible for retrying an individual failed position subscription. Restored
backend messages now identify the restoration code. These diagnostics distinguish
transport/subscription activity from valid P&L health; only a newer valid frame
clears the separate health failure.

Tests cover normal 1101/1102 behavior, rejected old frames, queued requests
crossing socket generations, failed sends, concurrent display/publication reads,
and cache publication against epoch retirement. No test places a broker order.
